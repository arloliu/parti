# Phase 4 Follow-up — Claim Resolver Watcher Restart + Periodic Reconcile

## Origin

User reported a production partition-reassignment failure. Investigation
(see conversation transcript for full trace) identified the root cause:

**`internal/durable/claim_resolver.go:ClaimBasedResolver` does not restart
its KV watcher when the watcher's `Updates()` channel closes.** When the
channel closes (NATS reconnect, server-side consumer GC, transient
errors), `processWatcher` returns silently and the in-memory cache
freezes forever at the pre-close state.

Observable symptom: both workers report
`pull suppressed reason="not_owner(owner=<theOtherWorker>)"` for their
newly-assigned partitions, even though the handoff coordinator wrote the
correct claim KV entries. The KV is correct; the workers' caches are
stale. Messages pile up in the NATS stream because no worker passes the
pull gate.

This is structurally identical to **R1 in
`docs/plans/cache-freeze-improvement/00-original-plan.md`** ("watcher channel closes
and watchLoop doesn't restart"). Pillar 2 §2.1–§2.5 fixed it for the
partition source watcher (`source/nats_kv.go`). The same fix was not
applied to the claim resolver, which is in a different package
(`internal/durable/`) but has the same wire pattern (NATS KV watcher
feeding a local cache that the hot path queries lock-free).

This phase delivers the **same fix shape** for `ClaimBasedResolver`:
two-value receive on `Updates()`, exponential-backoff restart on
channel close, and a periodic reconcile that re-warms the cache as a
safety net.

## Scope

Files in scope:

- `internal/durable/claim_resolver.go` — add watcher restart and
  reconcile loop.
- `internal/durable/claim_resolver_test.go` — extend with restart /
  reconcile tests (current file already covers cache semantics and
  basic watcher behaviour).
- `internal/durable/worker_consumer.go` — `ensureGateResolver` may need
  to pass a reconcile interval option through `NewClaimBasedResolver`.
  The default should be safe without an explicit option.

Files explicitly out of scope:

- `source/nats_kv.go` (already has this pattern; reference only).
- `internal/assignment/*` (Phase 3/4 already shipped commit watcher
  restart for `assignment._commit` — different layer).
- `internal/durable/processing_gate.go` (queries the resolver but does
  not own a watcher).
- `internal/durable/partition_consumer.go` (different watcher: the
  JetStream pull iterator; out of scope here).
- `manager_assignment.go` watcher loops (already covered by Phase 4).

## Pillar 2 reference (mirror this shape)

`source/nats_kv.go` is the canonical implementation. The new code must
match the same structural shape:

1. **Restart on channel close.** Two-value receive on `watcher.Updates()`.
   On `!ok`, log + close the dead watcher + back off (exponential with
   jitter) + relaunch via a helper that establishes a new watcher and
   spawns a fresh processing goroutine. Implementation pattern at
   `source/nats_kv.go:704-790` (`watchLoop` `!ok` branch +
   `restartWatcher`).

2. **Periodic reconcile.** A second goroutine ticks every
   `reconcileInterval` (default suggested below) and runs a full
   cache refresh:
   - Re-list the claims bucket keys.
   - For each key, `Get` + decode + compare against current cache.
   - Apply diffs (additions, updates, deletions) through the same
     code path the watcher uses (so canonicalization is identical).
   - No-op when in sync.
   Implementation pattern at `source/nats_kv.go:792-893`
   (`reconcileLoop` + `reconcileOnce`).

3. **`applyLocal` helper.** Both the watcher and the reconciler must
   funnel through one helper that does "compare, replace, fan out"
   atomically. The current `applyPendingBatch` (line 318) is close
   but is watcher-batched; the reconciler should reuse the same
   inner update path so the two converge identically.

## Detailed requirements

### R1. Watcher restart on channel close (closes the production bug)

Current code at `internal/durable/claim_resolver.go:281-285`:

```go
case upd, ok := <-watcher.Updates():
    if !ok {
        return  // ← bug: no restart
    }
```

Replace with:

```go
case upd, ok := <-watcher.Updates():
    if !ok {
        // Channel closed (NATS reconnect, server-side GC, etc.).
        // Apply any pending batch we already accumulated, then
        // surrender this watcher and request a restart.
        r.applyPendingBatch(pendingByPID, "watcher_close")
        return errWatcherClosed
    }
```

(Or equivalent — see "Concrete shape" below.)

`processWatcher` should signal the supervisor goroutine that restart
is needed. The supervisor pattern (mirroring `source/nats_kv.go`):

```go
func (r *ClaimBasedResolver) supervise(ctx context.Context) {
    backoff := watcherBaseBackoff
    for {
        err := r.runWatcher(ctx)
        if err == nil || ctx.Err() != nil {
            return
        }
        // Watcher died; log and back off before restarting.
        r.logger.Warn("claim resolver watcher closed, restarting",
            "error", err, "backoff", backoff)
        select {
        case <-ctx.Done():
            return
        case <-time.After(jittered(backoff)):
        }
        backoff = min(backoff*2, watcherMaxBackoff)
    }
}
```

`runWatcher` builds a new `kv.WatchAll`, runs the existing batch loop,
and returns either nil (clean shutdown) or an error (channel close,
watcher establishment failure).

Backoff constants:

- `watcherBaseBackoff = 100 * time.Millisecond`
- `watcherMaxBackoff = 30 * time.Second`
- Jitter ±20% (match `manager_assignment.go:295-308`).

### R2. Periodic reconcile loop

Add a second goroutine launched from `Start`:

```go
go r.reconcileLoop(ctx)
```

The loop:

```go
func (r *ClaimBasedResolver) reconcileLoop(ctx context.Context) {
    interval := r.reconcileInterval
    if interval <= 0 {
        return // disabled (test override)
    }
    t := time.NewTicker(interval)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
            r.reconcileOnce(ctx)
        }
    }
}
```

`reconcileOnce` walks the bucket (same logic as `warm` but reused
through the shared apply path), diffs against current cache, applies
all differences atomically (a single `r.cache.Store(&next)`).
**Critically**: the reconcile must NOT regress entries the watcher
has already applied at higher revision. Reuse the same
`existing.revision >= p.revision` short-circuit
(`applyPendingBatch:333-337`).

Default `reconcileInterval`: **30 seconds**, matching the source
reconciler. The handoff bucket is small (~partition count entries),
so a full re-list per 30s is cheap.

Tombstoned entries (entries deleted from KV) must be detectable from
`Keys()` returning a list that omits them; `reconcileOnce` should
tombstone any cache entry whose key is no longer in the bucket —
using a revision-aware approach to avoid racing with concurrent
watcher upserts. Match the watcher's tombstone semantics
(`applyPendingBatch:339-350`).

### R3. Configuration / wiring

Add an option to the constructor:

```go
func WithReconcileInterval(d time.Duration) ResolverOption
```

where `ResolverOption` is a new options type. The signature of
`NewClaimBasedResolver` should be extended variadically:

```go
func NewClaimBasedResolver(kv jetstream.KeyValue, prefix string,
    logger types.Logger, opts ...ResolverOption) *ClaimBasedResolver
```

Existing 3-arg call sites continue to compile (verified at
`internal/durable/worker_consumer.go:610`). Defaults apply silently:
30s reconcile interval, 100ms base backoff.

For tests, `WithReconcileInterval(0)` disables polling.
`WithReconcileInterval(50 * time.Millisecond)` drives deterministic
fast tests.

### R4. Lifecycle: Stop must drain both goroutines

Today `Stop` calls `r.watcher.Stop()` and returns. With supervision
and reconcile in place:

- `Stop` must signal both supervisor and reconcile to exit.
- Use the existing pattern: a `stopCh`, plus a `doneCh` (or
  `sync.WaitGroup`) so `Stop` waits for goroutine exit.
- The supervisor goroutine must observe `ctx.Done()` AND a local
  stop signal so an external context-cancel from `worker_consumer.go`
  also tears things down.

The current implementation stores `r.watcher` and stops it directly in
`Stop`. With restart, `r.watcher` is the *currently active* watcher;
the supervisor swaps it on each restart. Hold under `watcherMu` (new
mutex) or replace with an atomic pointer.

### R5. Metric for observability

Add (and emit) one new counter in `ResolverMetrics`:

- `IncWatcherRestart(reason string)` — incremented when the watcher
  is re-established. Reasons: `"channel_closed"`, `"establish_failed"`.

Also surface (via existing `IncUpdate`) a `"reconcile"` flush reason
so operators can see reconcile fired. This is additive to the
interface; provide a default-noop method on any nop implementation.

### R6. Test coverage

New tests in `internal/durable/claim_resolver_test.go`:

1. `TestClaimResolver_WatcherRestartOnChannelClose`
   - Start resolver, observe cache update via Watch.
   - Force the underlying watcher channel to close (use a wrapping
     fake, or stop+restart embedded NATS, or `kv.PurgeDeletes` to
     dislodge — find the cheapest reproducible trigger).
   - Within bounded time (<1s), verify a new claim written to KV is
     reflected in the cache.
   - Assert the `IncWatcherRestart("channel_closed")` metric fires.

2. `TestClaimResolver_ReconcileCatchesMissedEvent`
   - Configure `WithReconcileInterval(50ms)`.
   - Pause the watcher loop (e.g., wrap with a controllable proxy or
     monkey-patch the channel).
   - Write a claim directly to KV.
   - Within bounded time (~150ms), verify the cache reflects the
     write — proving reconcile rescued the missed event.

3. `TestClaimResolver_ReconcileNoSpuriousChanges`
   - Steady-state: watcher and KV agree. Run several reconcile ticks
     with no KV writes.
   - Assert no metric churn and no cache pointer reseats (snapshot
     via `r.cache.Load()` before/after pointer-equal).

4. `TestClaimResolver_ReconcileDoesNotRegressLaterWatcherUpdates`
   - Concurrently: a watcher batch sets entry P to revision 10; a
     reconcile pass observes revision 8.
   - Assert final cache revision is 10 (not 8). Use the revision
     short-circuit at the apply path.

5. `TestClaimResolver_StopBlocksUntilGoroutinesExit`
   - Start resolver, call Stop, assert it returns within a bounded
     window and that both goroutines have exited (race detector and
     a leak detector — `goleak` is already used in the repo per
     existing test patterns).

6. `TestClaimResolver_StopWithRestartingWatcher`
   - Force a watcher into restart-backoff state (NATS unreachable
     for a bounded period). Call Stop. Assert Stop returns promptly
     (does not block on backoff).

7. `TestClaimResolver_TombstoneSurvivesReconcile` (regression for the
   revision-aware tombstone path) — write claim, delete claim, run
   reconcile, assert cache shows deleted state (not resurrected
   from stale `Get`).

Existing tests must continue to pass unchanged. Run the full
package test suite to confirm no regressions:

```
go test ./internal/durable/... -race -count=1
```

## Non-goals (do not include)

- Do NOT fix the `twophase.go preparePhase` `cur.Owner == workerID`
  short-circuit. That's a real latent bug (analysis in the
  investigation transcript) but it does not produce the symptom in
  the user's report. Park it for a follow-up.
- Do NOT change the watcher / iterator semantics on the partition
  consumer side (different layer, different bug class).
- Do NOT add NATS connection-event hooks (`OnReconnect`, etc.) here.
  The watcher restart is sufficient and is connection-agnostic.
- Do NOT change `ClaimsRegistry` or `handoff/coordinator.go` —
  out of scope.

## Concrete shape (suggested layout)

```go
// internal/durable/claim_resolver.go

const (
    watcherBaseBackoff       = 100 * time.Millisecond
    watcherMaxBackoff        = 30 * time.Second
    watcherJitter            = 0.2
    defaultReconcileInterval = 30 * time.Second
)

type ResolverOption func(*ClaimBasedResolver)

func WithReconcileInterval(d time.Duration) ResolverOption {
    return func(r *ClaimBasedResolver) { r.reconcileInterval = d }
}

type ClaimBasedResolver struct {
    // ...existing fields...

    reconcileInterval time.Duration

    // Lifecycle
    stopCh   chan struct{}
    doneCh   chan struct{}  // signaled when supervise+reconcile both exit
    stopOnce sync.Once

    // Active watcher swap (replaces direct r.watcher reference; protected
    // by watcherMu).
    watcherMu sync.Mutex
    watcher   jetstream.KeyWatcher
}

func NewClaimBasedResolver(kv jetstream.KeyValue, prefix string,
    logger types.Logger, opts ...ResolverOption) *ClaimBasedResolver { ... }

func (r *ClaimBasedResolver) Start(ctx context.Context) error {
    if err := r.warm(ctx); err != nil { return err }
    r.stopCh = make(chan struct{})
    r.doneCh = make(chan struct{})
    // 2 supervised goroutines: supervise + reconcile
    var wg sync.WaitGroup
    wg.Add(2)
    go func() { defer wg.Done(); r.supervise(ctx) }()
    go func() { defer wg.Done(); r.reconcileLoop(ctx) }()
    go func() { wg.Wait(); close(r.doneCh) }()
    return nil
}

func (r *ClaimBasedResolver) Stop() {
    r.stopOnce.Do(func() {
        close(r.stopCh)
        r.watcherMu.Lock()
        w := r.watcher
        r.watcherMu.Unlock()
        if w != nil { _ = w.Stop() }
    })
    <-r.doneCh
}

func (r *ClaimBasedResolver) supervise(ctx context.Context) { ... }
func (r *ClaimBasedResolver) runWatcher(ctx context.Context) error { ... }
func (r *ClaimBasedResolver) reconcileLoop(ctx context.Context) { ... }
func (r *ClaimBasedResolver) reconcileOnce(ctx context.Context) { ... }
```

Adjust as needed — this is a sketch, not a binding signature.

## Validation gates before declaring done

1. `make lint` clean.
2. `go test ./internal/durable/... -race -count=1` green, including
   the new tests.
3. `go vet ./...` clean.
4. Manual smoke against an integration test that simulates a
   reconnect (if one exists; otherwise rely on the new unit tests).
5. Goroutine leak check: re-run any existing `goleak`-using tests
   in the package and confirm Stop drains cleanly.

## Spec alignment with the robustness plan

This work is a direct application of Pillar 2 §2.1–§2.5 to the claim
resolver. The plan's invariants:

- "Source read robustness: watch + periodic reconcile + delete fan-out
  + watcher restart" — applied here to the claims watcher.
- "Reconciliation path shares the watcher's compare → replace → fan
  out code … the only correctness invariant is that poll and watch
  converge on identical local state given identical KV state."

For the audit metrics, no new top-level metric namespaces are
introduced — the additions live in the existing `ResolverMetrics`
interface.

## Risk / rollback

- The resolver is on the hot pull path. A bug in the new code could
  cause cache thrash or, worse, regress the existing freeze-no-restart
  behaviour into a freeze-and-thrash one.
- All defaults are conservative; reconcile is opt-out via
  `WithReconcileInterval(0)`.
- Rollback: revert the single commit. The data plane (claims KV)
  is unchanged; this is purely cache plumbing.
