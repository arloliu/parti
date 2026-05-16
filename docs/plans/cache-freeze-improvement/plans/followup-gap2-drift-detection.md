# Phase 4 Follow-up — Gap 2: Silent watcher stall — observability + active recovery

## Origin

v3 review of the cache-freeze fix surfaced this gap: the nats.go KV
watcher does NOT close its `Updates()` channel on a NATS server
restart (verified against nats.go v1.50.0 source). The supervisor's
`!ok` branch — which emits `IncWatcherRestart("channel_closed")` —
therefore never fires in production for the most common silent
stall trigger. Recovery in those cases is delivered by the periodic
reconciler at its configured cadence (default 30s, now configurable
via gap-1). But operators have no metric that fires when this silent
recovery path actually engages, so a quietly broken watcher is
invisible until something else surfaces the symptom.

This gap-2 fix converts silent stall into an observable + actively
recovered event:

1. **Observability:** reconcileOnce emits a new
   `IncReconcileRescue()` counter whenever it finds drift between
   KV and the cache. Frequent non-zero values are operator-visible
   evidence that the watcher is unhealthy.
2. **Active recovery:** reconcileOnce, upon detecting drift, also
   asks the supervisor to restart the watcher (subject to a
   cooldown). The supervisor's existing restart path then runs and
   emits `IncWatcherRestart("drift_detected")` — a new restart
   reason that distinguishes reconciler-triggered restarts from
   cooperative channel-close restarts.

The result: a silently dead watcher produces a stream of observable
restart events instead of indefinite silent reconcile rescues, and
the worker recovers latency-bounded by one reconcile interval (the
same as today) — but with a clear failure signal that operators can
alert on.

## Scope

Files in scope:

- `internal/durable/claim_resolver.go` — new metric emission in
  `reconcileOnce`; new drift-triggered watcher restart path; cooldown
  state; `IncWatcherRestart("drift_detected")` reason.
- `internal/durable/resolver_metrics.go` and
  `consumer/metrics.go` — add `IncReconcileRescue()` to the
  `ResolverMetrics` interface.
- `test/simulation/internal/metrics/collector.go` — implement
  the new method on the simulation adapter.
- `internal/durable/claim_resolver_test.go` — extend the
  `metricsSpy` with the new method + helpers.
- New unit tests in `internal/durable/`.

Files explicitly out of scope:

- `internal/assignment/handoff/twophase.go` and any handoff/audit
  logic (gap-3 just landed there).
- Any of the assignment commit watcher / source watcher loops.
- Manager / consumer config plumbing (gap-1 already exposed the
  reconcile interval).

## Detailed requirements

### R1. New metric on the resolver-metrics interface

Add to both `internal/durable/resolver_metrics.go:ResolverMetrics`
and `consumer/metrics.go:ResolverMetrics`:

```go
// IncReconcileRescue increments when reconcileOnce applies any
// change to the cache — i.e., the reconciler observed drift between
// KV and the in-memory cache and rescued the cache. Persistent
// non-zero values are strong evidence the KV watcher is silently
// stalled (the nats.go KV watcher does not surface NATS server
// restarts as Updates() channel close, so the supervisor's
// channel_closed restart path will not fire for that trigger).
// Operators should alert on this metric being non-zero across
// multiple consecutive reconcile cycles.
IncReconcileRescue()
```

Implement on:

- `internal/durable/claim_resolver_test.go:metricsSpy` (with a
  counter snapshot helper similar to `watcherRestartCount`).
- `test/simulation/internal/metrics/collector.go:resolverMetricsAdapter`
  (define a new Prometheus counter, e.g.
  `simulation_resolver_reconcile_rescue_total`).
- Any other implementor surfaced by `grep -rn "IncWatcherRestart\b"`
  — the same set already covered by gap-1.

### R2. Emit `IncReconcileRescue` from `reconcileOnce`

At the bottom of `reconcileOnce` in
`internal/durable/claim_resolver.go`, change the trailing
"work to apply" block:

```go
if len(pendingByPID) == 0 {
    return
}
if r.metrics != nil {
    r.metrics.IncReconcileRescue()
}
r.applyPendingBatch(pendingByPID, "reconcile")

// Signal the supervisor that the watcher is likely silently stalled.
r.requestWatcherRestartFromReconcile()
```

`requestWatcherRestartFromReconcile` is a new private method (see R3).

The metric MUST fire BEFORE `applyPendingBatch` so that even if the
apply panics (it won't, but defensively) the rescue event is
observable.

### R3. Drift-triggered watcher restart with cooldown

Add to `ClaimBasedResolver`:

```go
// driftRestartPending signals to supervise that the most recent
// watcher close was triggered by the reconciler observing drift,
// so the restart should be classified as "drift_detected" rather
// than "channel_closed".
driftRestartPending atomic.Bool

// lastDriftRestart records the wall-clock time of the most recent
// reconciler-triggered watcher restart. Used to rate-limit drift
// restarts to no more than one per driftRestartCooldown.
lastDriftRestart atomic.Pointer[time.Time]
```

New method:

```go
// requestWatcherRestartFromReconcile is called by reconcileOnce when
// it has applied any change to the cache. The reconciler engaging
// means the watcher missed events — likely a silent stall (the
// nats.go KV watcher does not surface server restarts as Updates()
// channel close). The supervisor's restart path then re-establishes
// the watcher and re-delivers historical state via WatchAll's
// initial replay.
//
// Rate-limited to one drift-driven restart per driftRestartCooldown
// (default 2 × reconcileInterval, minimum 60s). The reconciler will
// still rescue subsequent drifts at its normal cadence; rate-
// limiting prevents restart storms when the watcher cannot
// successfully re-establish (e.g., persistent NATS unreachable).
func (r *ClaimBasedResolver) requestWatcherRestartFromReconcile() {
    cooldown := r.driftRestartCooldown
    if cooldown <= 0 {
        return // drift-driven restart disabled
    }

    if last := r.lastDriftRestart.Load(); last != nil {
        if time.Since(*last) < cooldown {
            return
        }
    }
    now := time.Now()
    r.lastDriftRestart.Store(&now)

    // Mark the next channel-close as drift-triggered for the
    // supervise reason emission.
    r.driftRestartPending.Store(true)

    // Stop the current watcher under watcherMu. This closes the
    // Updates() channel; processWatcher returns errWatcherClosed;
    // supervise re-establishes via runWatcher.
    r.watcherMu.Lock()
    w := r.currentWatcher
    r.watcherMu.Unlock()
    if w != nil {
        _ = w.Stop()
    }
}
```

In `supervise()`, when emitting the restart-success metric on the
channel-closed path, check the pending flag:

```go
reason := "channel_closed"
if r.driftRestartPending.CompareAndSwap(true, false) {
    reason = "drift_detected"
}
if r.metrics != nil {
    r.metrics.IncWatcherRestart(reason)
}
```

If `runWatcher` fails repeatedly (`IncWatcherRestart("establish_failed")`),
the pending flag stays set until the next successful restart, at
which point it correctly classifies as `drift_detected`. This is the
right behaviour: the original cause of the close was drift; the
intermediate failed-to-establish events are tracked separately by
the existing `establish_failed` reason.

### R4. Configuration

Add a constructor option:

```go
// WithDriftRestartCooldown sets the minimum interval between
// reconciler-triggered watcher restarts (R3). A value of 0 disables
// drift-driven restarts entirely; reconciler rescues still apply
// but the watcher is NOT torn down. Default: 2 × reconcileInterval,
// floored at 60s.
func WithDriftRestartCooldown(d time.Duration) ResolverOption
```

Default selection happens in `NewClaimBasedResolver` after
options are applied:

```go
if r.driftRestartCooldown == 0 && r.reconcileInterval > 0 {
    cooldown := 2 * r.reconcileInterval
    if cooldown < 60*time.Second {
        cooldown = 60 * time.Second
    }
    r.driftRestartCooldown = cooldown
}
```

If `reconcileInterval == 0` (reconciler disabled), drift restart is
inert (the trigger never fires).

This option is NOT exposed through `consumer.ResolverConfig` for
now — the default is safe and tunable from package-internal
callers if needed. Keep the user-facing surface minimal.

### R5. Tests

In `internal/durable/`:

1. **`TestClaimResolver_ReconcileRescueIncrementsMetric`** — set
   `WithReconcileInterval(50 * time.Millisecond)`, stop the watcher
   directly (cooperative), write a claim to KV, wait for reconcile
   to fire, assert `IncReconcileRescue()` was called.

2. **`TestClaimResolver_ReconcileNoRescueWhenNoDrift`** — steady
   state with watcher + reconcile both active and KV unchanged, run
   several reconcile ticks, assert `IncReconcileRescue` is NEVER
   called.

3. **`TestClaimResolver_DriftTriggersWatcherRestart`** — cooperative-
   stop the watcher (via `r.currentWatcher.Stop()`), write a claim
   so the reconciler observes drift, assert within a bounded window:
   - `IncReconcileRescue()` fires.
   - `IncWatcherRestart("drift_detected")` fires (NOT
     `channel_closed`).
   - The new watcher delivers a subsequent write (verifies the
     supervisor actually re-established).

4. **`TestClaimResolver_DriftRestartRespectsCooldown`** — set
   `WithDriftRestartCooldown(5 * time.Second)` and a short reconcile
   interval. Drive two drift events within 1s. Assert exactly ONE
   `drift_detected` watcher restart fires (rate limit honoured).

5. **`TestClaimResolver_DriftRestartDisabledByZeroCooldown`** —
   `WithDriftRestartCooldown(0)`. Drive drift. Assert
   `IncReconcileRescue()` fires but no watcher restart occurs.

6. **`TestClaimResolver_DriftRestartReasonClassifiedCorrectly`** —
   regression guard for the supervise reason CAS: trigger a drift
   restart and a cooperative-close (`r.watcher.Stop()`) in
   sequence. Assert the first restart classifies as
   `drift_detected` and the second as `channel_closed`.

All async waits use `require.Eventually` (per
`.agents/rules/300-testing.md`). No `time.Sleep` for synchronisation.

Carry through the existing tests in `claim_resolver_restart_test.go`
and `claim_resolver_watcher_freeze_test.go`; they must continue to
pass unchanged.

## Validation gates

```
make lint
go test ./internal/durable/... -race -count=3 -timeout 180s
go test ./... -race -count=1 -short -timeout 300s
go vet ./...
go build ./...
```

`TestManager_DegradedHook` is a documented flake; rerun once if it
fails.

## Verify the new test is sensitive

The most informative sensitivity verification: run
`TestClaimResolver_DriftTriggersWatcherRestart` on parent `0bbd124`
(before this fix) — it must fail because neither
`IncReconcileRescue` nor `drift_detected` exists yet. The compile
errors will demonstrate the test depends on the new API. That is
sufficient — there's no need to invent a separate "pre-existing
silent stall" scenario because the metric itself is new.

## Non-goals

- Do NOT change reconcileOnce's correctness semantics (revision
  checks, tombstone pre-snapshot logic).
- Do NOT change the supervisor's exponential backoff or jitter
  constants.
- Do NOT expose `WithDriftRestartCooldown` through `consumer.
  ResolverConfig`. The default is safe.
- Do NOT add a separate "watcher stale detected without drift"
  signal. The drift-based signal is the only one we can reliably
  detect.

## Risk / rollback

- New behaviour activates only when reconciler finds drift, which
  in healthy clusters should be rare.
- Cooldown prevents restart storms.
- Drift-driven restart can be disabled by callers via
  `WithDriftRestartCooldown(0)` if it proves disruptive.
- Rollback: revert the commit.

## Commit message template

```
feat(durable): observable + active recovery for silent watcher stalls

The nats.go KV watcher does NOT surface NATS server restarts (or
similar silent server-side events) as Updates() channel close;
verified against nats.go v1.50.0 source. The supervisor's
channel_closed restart path therefore never fires for the most
common production trigger, and a silently dead watcher is recovered
only by the periodic reconciler — invisibly.

This change converts that silent path into an observable event:

- IncReconcileRescue() fires whenever reconcileOnce applies any
  change to the cache. Persistent non-zero values are evidence the
  watcher is unhealthy.
- The reconciler asks the supervisor to tear down the current
  watcher when drift is observed. The supervisor's existing restart
  path then emits IncWatcherRestart("drift_detected"), a new
  restart-reason distinct from channel_closed and establish_failed.
- A cooldown (default 2 × reconcileInterval, floor 60s) rate-limits
  drift restarts to prevent storms under persistent NATS
  unreachable conditions. Configurable via WithDriftRestartCooldown,
  with 0 disabling drift-driven restarts entirely (rescue still
  fires; metric still observable).

Six new tests cover metric emission, no-rescue-in-steady-state,
restart triggering and reason classification, cooldown rate limit,
and the disable-by-zero option.
```

DO NOT add `Co-Authored-By` or any attribution trailers.
