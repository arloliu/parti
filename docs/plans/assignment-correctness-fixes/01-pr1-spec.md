# PR-1 Implementation Spec — Partition-Triggered Rebalance Lifecycle Bundle

Implements **ISSUE-002 + ISSUE-003 + ISSUE-005** from
[`00-fix-plan.md`](./00-fix-plan.md), per the test designs in
`tmp/assignment_review/07-verification-plan.md` §7.2, §7.3, §7.5.

**Revision history:**
- v1 (initial draft)
- v2 — address Codex review `tmp/01-pr1-spec_pr1-impl-spec_review.md`:
  - P0-A: deferred-update queue + drain ticker for recovery grace skip.
  - P0-B: explicit commit-point fence in `assignment_publisher.go`.
  - P1-A: CAS-blocking KV wrapper is **mandatory**; strategy-block fallback removed.
  - P1-B: stop-triggered cancellation reclassified to `errShuttingDown`.
  - P2: 50ms `Wait` in Test 7.5 replaced with event-driven observer.
- v4 (this revision) — address v3 review `tmp/01-pr1-spec_pr1-impl-spec_v3_review.md`:
  - PR1-V3-001 (P1, immediate watch path lost-update on grace re-entry):
    `triggerPartitionRebalance` now returns its error. **Both** the immediate
    watch arm and the drain-tick arm restore `pendingPartitionUpdate=true`
    on `errShuttingDown` when `stopCh` is not closed. Restore logic centralised.
  - PR1-V3-002 (P2, metric): remove `IncrementCommitAborts()` from the
    shutdown gate. The `"shutdown"` batch-abort label is added to the
    public `IncrementBatchAborted` reason list in `types/metrics_collector.go`.
  - PR1-V3-003 (P2, wrapper select fairness): Test 7.3 must NOT call
    `Release()` until either `Stop` has returned OR `CommitReturnedChan`
    fires (signal emitted by the wrapper when the blocked goroutine has
    returned, whether via ctx cancellation or forwarded result). Wrapper contract additionally mandates that on a
    simultaneous-readiness `select`, after either arm wins, the wrapper
    re-checks `ctx.Err()` before forwarding — eliminates Go-select-fairness flakiness.
  - PR1-V3-004 (nit): define package-private lifecycle constants
    `lifecyclePartitionUpdate` / `lifecyclePartitionUpdateDeferred` and
    reference them everywhere (the trigger sites and `isPartitionLifecycle`).
- v3 — address v2 review `tmp/01-pr1-spec_pr1-impl-spec_v2_review.md`:
  - P0-A (grace-reentry race): drain decision re-checks grace inside `rebalanceMu` via a new
    partition-lifecycle short-circuit inside `rebalance` itself. Closes the
    "grace flaps true→false→true between drain-tick check and rebalance entry" window.
  - P0-B (residual CAS window vs §7.3 strict wording): reframed. The strict §7.3
    assertion holds in test because `blockingAssignmentKV` is **ctx-aware**:
    when held at the CAS site and ctx cancels, it returns `ctx.Err()` WITHOUT
    forwarding. Layer 1 (stop-aware ctx) is therefore sufficient for §7.3.
    The Publisher gate (Layer 2) is retained as production defense-in-depth.
    The narrow production residual ("bytes already on the wire") is explicitly
    ISSUE-001 (PR-4) territory.
  - P2 (metric label): use the EXISTING `"shutdown"` label (`assignment_publisher.go`
    already labels at line 379 with `"commit_cas_failed"`; we add a sibling
    label). If the metric layer requires registration, register `"shutdown"`
    where `"commit_cas_failed"` is registered.
  - P2 (Config knob commitment): `RebalanceGraceDrainInterval` IS an exported
    field on `Config`. Default computed in `NewCalculator` if zero. Not
    test-internal.

---

## 1. Anchors (verified 2026-05-17 against HEAD)

| Anchor | File:line | Status |
|---|---|---|
| `monitorPartitions` goroutine | `internal/assignment/calculator.go:587-626` | unchanged |
| Detached 30s timeout (ISSUE-002 surface) | `calculator.go:619` | unchanged |
| `rebalance` entry (ISSUE-003 surface) | `calculator.go:929-930` | unchanged |
| Existing pre-publish state check | `calculator.go:1038-1044` | reused; not the fence |
| `publisher.Publish` call inside rebalance | `calculator.go:1054` | unchanged |
| CAS site inside Publish (commit point) | `assignment_publisher.go:366-389` | **new fence inserted here** |
| `PublisherConfig` struct | `assignment_publisher.go:136-145` | **adds one optional field** |
| `IsInRecoveryGrace` skip pattern to mirror | `calculator.go:700` | reused |
| `pollForChanges` worker-only path | `calculator.go:559-584` | confirms grace-skip needs explicit drain |
| `checkForChanges` worker-only path | `calculator.go:645-672` | confirms grace-skip needs explicit drain |
| `errShuttingDown` sentinel | `calculator.go:15-18` | reused (package-private; Publisher is same package) |
| `handleRebalance` error suppression | `calculator.go:888-898` | suppresses ONLY `errShuttingDown`; informs P1-B fix |
| State-machine error-level logging | `state_machine.go:254-260, 301-308` | informs P1-B fix |
| `Calculator.stopCh` field | `calculator.go:61` | reused |
| Existing `mockWatchableSource` test double | `calculator_test.go:604-639` | reused |
| Existing `mockStateProvider` (has `SetGrace`) | `testing_helpers.go:11-30` | reused |

`Strategy.Assign` — `strategy/round_robin.go:53` — takes no ctx. Confirms
the verification plan's caveat: strategy is not a viable blocking point;
the test fixture must block at the KV CAS site (§4 below).

---

## 2. Helper — `ctxFromStopCh`

### Signature

```go
// ctxFromStopCh returns a derived context that is cancelled when EITHER
// the parent context is cancelled OR stopCh is closed. The caller MUST
// invoke the returned CancelFunc to release the watcher goroutine.
//
// timeout == 0 → only parent + stopCh cancellation.
func ctxFromStopCh(parent context.Context, stopCh <-chan struct{}, timeout time.Duration) (context.Context, context.CancelFunc)
```

### Placement

`internal/assignment/calculator.go`, file-private (lowercase), immediately
above `monitorPartitions`.

### Implementation sketch

```go
func ctxFromStopCh(parent context.Context, stopCh <-chan struct{}, timeout time.Duration) (context.Context, context.CancelFunc) {
    var ctx context.Context
    var cancel context.CancelFunc
    if timeout > 0 {
        ctx, cancel = context.WithTimeout(parent, timeout)
    } else {
        ctx, cancel = context.WithCancel(parent)
    }
    // Cheap, leak-free fanout via context.AfterFunc (Go 1.21+; Parti's go.mod
    // targets >=1.21). The registered hook runs on stopCh receive OR ctx end.
    stop := context.AfterFunc(ctx, func() {})
    go func() {
        select {
        case <-stopCh:
            cancel()
        case <-ctx.Done():
        }
        stop() // best-effort; harmless if hook already ran
    }()
    return ctx, cancel
}
```

If `context.AfterFunc` is unavailable in our target Go version, the
goroutine alone is sufficient (it exits on `ctx.Done()` regardless).
The simpler form (drop `stop`/`AfterFunc`) is acceptable.

---

## 3. Wiring

### 3.1 ISSUE-002 — `monitorPartitions` ctx detach (`calculator.go:619`)

Replace
```go
reqCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
```
with
```go
reqCtx, cancel := ctxFromStopCh(context.Background(), c.stopCh, 30*time.Second)
```

### 3.2 ISSUE-003 — Commit-point fence + stop-aware ctx in `rebalance`

**Two-layer fix.** Layer 1 (stop-aware ctx) is the load-bearing fence;
Layer 2 (Publisher gate) is production defense-in-depth.

**Test-invariant note (P0-B):** the strict §7.3 assertion
("`kv.Get(_commit)` shows the pre-rebalance commit") holds in test because
`blockingAssignmentKV` is **ctx-aware** — when held at the CAS site and
the wrapped ctx fires, it returns `ctx.Err()` WITHOUT forwarding to the
underlying KV. Layer 1 alone is therefore sufficient for the §7.3 bar.
Layer 2 hardens production against the case where a real
`jetstream.KeyValue` impl has already flushed bytes to the wire before
observing ctx cancellation. The narrow production residual ("bytes on
the wire, server commits, leader has stepped down") is ISSUE-001 (PR-4)
territory.

**Layer 1 (early-abort): stop-aware ctx through `rebalance`.**
At the top of `rebalance`, after acquiring `rebalanceMu`:
```go
ctx, cancel := ctxFromStopCh(ctx, c.stopCh, 0)
defer cancel()
```
This causes `getActiveWorkersFiltered`, `snapshotSource`, and the early
phases of `publisher.Publish` to short-circuit on `Stop`. All callers
(`monitorPartitions`, state-machine `executeRebalance`, audit loop,
emergency) benefit without signature changes.

**Layer 2 (commit-point fence): publisher checks shutdown immediately
before the CAS.** Add to `PublisherConfig`:
```go
type PublisherConfig struct {
    // ...existing fields...
    // IsShuttingDown, when non-nil, is consulted immediately before the
    // commit CAS. If it returns true, Publish aborts with errShuttingDown
    // and no _commit write is attempted. nil → no gate (test default).
    IsShuttingDown func() bool
}
```
Calculator wires it during publisher construction:
```go
cfg.IsShuttingDown = func() bool {
    select { case <-c.stopCh: return true; default: return false }
}
```
In `assignment_publisher.go` immediately before line 369 (`Create`/`Update`):
```go
if p.isShuttingDown != nil && p.isShuttingDown() {
    p.metrics.IncrementBatchAborted("shutdown")
    return errShuttingDown
}
```
**Note (PR1-V3-002):** `IncrementCommitAborts()` is intentionally NOT
called here — the existing contract reserves that counter for actual
CAS attempt failures (`types/metrics_collector.go:259-263`). The new
`"shutdown"` reason MUST be added to the documented label list at
`types/metrics_collector.go:239-246`.
This closes the wider race window. A residual narrow window remains
(stopCh closes between the gate check and the CAS landing on the server);
this matches the `tmp/assignment_review/07-verification-plan.md` §7.3
"either-or" invariant ("aborted with sentinel OR CAS attempt skipped").

**Production residual window:** between the gate check and the CAS bytes
landing on the server, the leader could step down. The combined effect of
layers 1+2 reduces this from "tens of seconds of detached-ctx work"
(current) to "single RPC round-trip" (post-fix). ISSUE-001 (CAS-loss
recovery, PR-4) is the orthogonal fence that handles the case where a
leader's commit lands after step-down.

**Metric label:** the new shutdown abort path uses
```go
p.metrics.IncrementBatchAborted("shutdown")
```
Sibling of the existing `"commit_cas_failed"` label
(`assignment_publisher.go:379`). If the metric layer registers labels
statically, register `"shutdown"` next to `"commit_cas_failed"` (same
file's metric definitions).

### 3.3 ISSUE-005 — Recovery-grace skip with deferred-update queue

The naïve `continue` drops the partition-update event. `pollForChanges`
(`calculator.go:559-584`) and `checkForChanges` (`calculator.go:645-672`)
only act on worker-set changes; the audit path explicitly delegates
source-revision drift to `monitorPartitions` (`calculator_audit.go:46-51`).
Therefore the missed event would persist until either (a) the next worker
membership change OR (b) another partition watch event arrives —
indefinite in principle.

**Fix:** queue + drain ticker contained inside `monitorPartitions`.

Add to `Calculator`:
```go
pendingPartitionUpdate atomic.Bool
```

Rewrite the `monitorPartitions` loop to add a periodic tick that drains
the pending flag once grace lifts:
```go
func (c *Calculator) monitorPartitions(ctx context.Context, source types.WatchablePartitionSource) {
    ch := source.Watch(ctx)
    drainTick := time.NewTicker(c.RebalanceGraceDrainInterval) // see config note
    defer drainTick.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-c.stopCh:
            return
        case <-drainTick.C:
            if !c.pendingPartitionUpdate.CompareAndSwap(true, false) {
                continue
            }
            if c.inRecoveryGrace() {
                c.pendingPartitionUpdate.Store(true) // restore; retry next tick
                continue
            }
            err := c.triggerPartitionRebalance(lifecyclePartitionUpdateDeferred)
            c.restorePendingOnGraceBail(err)
        case _, ok := <-ch:
            if !ok { return }
            select { case <-c.stopCh: return; default: }
            if c.inRecoveryGrace() {
                c.pendingPartitionUpdate.Store(true)
                c.Logger.Info("deferring partition rebalance: leader is in recovery grace period")
                continue
            }
            c.pendingPartitionUpdate.Store(false) // immediate trigger supersedes any pending
            err := c.triggerPartitionRebalance(lifecyclePartitionUpdate)
            c.restorePendingOnGraceBail(err)
        }
    }
}

func (c *Calculator) inRecoveryGrace() bool {
    return c.stateProvider != nil && c.stateProvider.IsInRecoveryGrace()
}

// triggerPartitionRebalance runs a partition-lifecycle rebalance and
// returns the rebalance result so the caller can act on errShuttingDown
// (grace bail OR shutdown). Logs only non-errShuttingDown failures.
func (c *Calculator) triggerPartitionRebalance(lifecycle string) error {
    reqCtx, cancel := ctxFromStopCh(context.Background(), c.stopCh, 30*time.Second)
    defer cancel()
    err := c.rebalance(reqCtx, lifecycle)
    if err != nil && !errors.Is(err, errShuttingDown) {
        c.Logger.Error("failed to rebalance after partition update", "error", err, "lifecycle", lifecycle)
    }
    return err
}

// restorePendingOnGraceBail restores pendingPartitionUpdate=true if the
// returned err is errShuttingDown AND stopCh is not closed (i.e. the bail
// was caused by recovery-grace re-entry inside rebalance, not by Stop).
// Called by both the immediate watch arm and the drain-tick arm.
func (c *Calculator) restorePendingOnGraceBail(err error) {
    if !errors.Is(err, errShuttingDown) {
        return
    }
    select {
    case <-c.stopCh:
        // Real shutdown; nothing to restore.
    default:
        c.pendingPartitionUpdate.Store(true)
    }
}
```

**`RebalanceGraceDrainInterval` config:** exported field on `Config`
(needed by Test 7.5 Phase B to keep wall time tight). Default computed
in `NewCalculator` when zero, as `min(Cooldown, 30*time.Second)` capped
below at 1s. Drain is not time-critical — grace itself is on the order
of seconds to a minute — but should be short enough that the deferred
rebalance fires soon after grace lifts.

**P0-A grace-reentry guard (inside `rebalance`):**
After acquiring `rebalanceMu`, before proceeding for partition-lifecycle
calls, re-check grace atomically:
```go
// Inside rebalance(), immediately after rebalanceMu.Lock() / defer Unlock():
if isPartitionLifecycle(lifecycle) && c.inRecoveryGrace() {
    return errShuttingDown
}
```
where
```go
// Package-private lifecycle name constants (PR1-V3-004).
const (
    lifecyclePartitionUpdate         = "partition_update"
    lifecyclePartitionUpdateDeferred = "partition_update_deferred"
)

func isPartitionLifecycle(lc string) bool {
    return lc == lifecyclePartitionUpdate || lc == lifecyclePartitionUpdateDeferred
}
```
This closes the "grace flaps to true between drain-tick check and rebalance
entry" race: the drain tick sees grace=false, decides to trigger, but by
the time `rebalance` acquires `rebalanceMu`, grace has flipped back to
true → the rebalance bails atomically (returns `errShuttingDown`).

**Restoration is centralised in `restorePendingOnGraceBail` (defined in
the monitor block above) and called by BOTH the immediate watch arm AND
the drain-tick arm.** That closes PR1-V3-001 — the immediate watch path
also restores the pending flag if grace re-enters between the watch
arm's grace check and `rebalance` acquiring `rebalanceMu`.

The canonical drain-tick pseudocode lives in the `monitorPartitions`
sketch above (§3.3); this subsection only declares the constants and
the inside-`rebalance` short-circuit.

Watch event arriving during a deferred drain (rebalance in flight):
because `rebalance` holds `rebalanceMu`, a concurrent watch arm would
also wait on the mu. We do NOT want to queue a duplicate deferred
rebalance for the same logical update. The `monitorPartitions` watch arm
sets `pendingPartitionUpdate=true` only when in grace; if not in grace
(common case during a drain) it falls through to its own trigger path
which acquires `rebalanceMu` and serialises naturally. Last-write-wins
semantics are correct because every rebalance freshly snapshots the
source.

### 3.4 P1-B — Reclassify stop-triggered cancellation as `errShuttingDown`

State-machine callers (`state_machine.go:254-260, 301-308`) log
`handleRebalance` errors at error level unless the sentinel is
`errShuttingDown`. If `rebalance` returns `context.Canceled` during a
normal shutdown, we get spurious error logs.

**Fix:** at every error-return inside `rebalance`, wrap when stopCh is
closed:
```go
func (c *Calculator) wrapStopErr(err error) error {
    if err == nil { return nil }
    select {
    case <-c.stopCh:
        return errShuttingDown
    default:
        return err
    }
}
```
Replace `return fmt.Errorf(...)` returns in `rebalance` with
`return c.wrapStopErr(fmt.Errorf(...))`. The pre-existing
`errShuttingDown` return path (`calculator.go:1043`) is untouched.

---

## 4. Tests

All three new tests in `internal/assignment/calculator_test.go`. The
mandatory CAS-blocking fixture (`blockingAssignmentKV`) is added near the
existing test doubles.

### 4.1 Mandatory fixture — `blockingAssignmentKV`

`AssignmentPublisher` consumes `jetstream.KeyValue` via
`PublisherConfig.AssignmentKV` (`assignment_publisher.go:137`). The
fixture wraps a real embedded-NATS `jetstream.KeyValue` and exposes:
- `BlockOnCommitCAS()` — every `Create`/`Update` on the `_commit` key
  blocks on an internal channel; reports a "blocked" signal so the test
  can synchronise on the CAS arrival event-driven (no sleep).
- `Release()` — unblocks all currently-blocked calls (those that haven't
  already returned via ctx cancellation).
- `CommitsForwarded() int` — count of CAS calls that were actually
  forwarded to the underlying real KV. Used by Test 7.3 to assert "no
  CAS landed after Stop."
- `CommitAttemptChan() <-chan struct{}` — fires once per blocked CAS;
  test uses it for event-driven synchronisation.
- `CommitReturnedChan() <-chan struct{}` — fires once when a blocked
  CAS goroutine returns (either via ctx cancellation or after release
  + forwarding). Used by Test 7.3 to deterministically order
  `Release()` after the ctx-cancelled return.

**Ctx-awareness contract (required for §7.3 strict assertion;
PR1-V3-003):** while blocked at the `_commit` CAS, the wrapper selects
on `ctx.Done()` and the internal release channel. If ctx fires first,
it returns `ctx.Err()` directly WITHOUT incrementing `CommitsForwarded`
and WITHOUT forwarding to the underlying KV.

**Select-fairness clause:** if the `select` happens to pick the release
case despite ctx being concurrently cancelled, the wrapper MUST re-check
`ctx.Err()` AFTER the select and BEFORE forwarding. If `ctx.Err() != nil`,
return it directly without forwarding. This eliminates Go select random
fairness as a source of test flakiness.

This mirrors well-behaved real `jetstream.KeyValue` semantics for
ctx-cancelled-before-send.

Only the `_commit` key path blocks; all other reads/writes pass through
unchanged so publisher prerequisite steps (payload writes, alias writes)
complete normally. ~80 LOC of test helper, narrow surface — only wraps
the methods Publisher actually calls.

**No fallback to strategy-block** — Codex P1-A made the KV fixture
mandatory; strategy-block does not exercise the CAS path.

### 4.2 Test 7.2 — `TestCalculator_Stop_BoundedByPartitionRebalance`

```
1. Calc with mockWatchableSource + blockingAssignmentKV(BlockOnCommitCAS).
2. calc.Start(ctx)
3. source.Update(...)
4. wait CommitAttemptObserved channel (event-driven)
5. start := time.Now(); err := calc.Stop(stopCtx5s)
6. assert.NoError(err) && time.Since(start) < 2*time.Second
7. blockingAssignmentKV.Release() // unblock the (now-cancelled) CAS attempt
```

**Pre-fix:** Stop blocks ~30s on the detached timeout.
**Post-fix:** Stop returns within ~1s; the blocked CAS returns errShuttingDown via the gate.

### 4.3 Test 7.3 — `TestCalculator_Stop_PreventsStaleCommitAfterStop`

```
1. Same setup. Capture pre-test _commit revision (revBefore; may be 0/absent).
2. source.Update(...) → wait CommitAttemptChan (blocked at CAS).
3. calc.Stop(stopCtx5s)  // runs concurrently. Stop closes stopCh,
   which cancels the stop-aware ctx inside rebalance, which propagates
   to the wrapper's ctx.Done() select.
4. The wrapper returns ctx.Err() WITHOUT forwarding (ctx-awareness contract).
   Test waits on the wrapper's "returned" signal (a second event channel
   distinct from CommitAttemptChan) OR on Stop's return — whichever fires
   first proves the goroutine has exited.
5. blockingAssignmentKV.Release()  // strictly AFTER step 4; defensive only.
6. Assert: blockingAssignmentKV.CommitsForwarded() == 0.
7. Assert: real KV.Get("_commit") matches revBefore.
8. Assert: calc.publisher.CurrentVersion() did NOT advance.
```

**Pre-fix:** ctx is NOT stop-aware (detached `Background()` for partition
path) → wrapper blocks until Release → forwards to real KV → CAS lands.
**Post-fix:** Layer 1 stop-aware ctx fires when Stop closes stopCh →
wrapper observes ctx.Done() first → returns ctx.Err() → no forwarding.
**Production hardening:** Layer 2 Publisher gate provides defense-in-depth
for the case where a real KV impl has already flushed bytes; verified
by code review only (cannot be exercised in test because the wrapper
contract returns ctx.Err() before the gate check sees stopCh closed
in some interleavings — equivalent outcome either way).

### 4.4 Test 7.5 — `TestCalculator_PartitionUpdate_HonoursRecoveryGrace`

Two-phase test (covers both skip and deferred-drain):

```
Phase A (during grace — must NOT publish):
1. Calc with mockWatchableSource + blockingAssignmentKV(observe-only mode)
   + mockStateProvider.SetGrace(true).
2. Short RebalanceGraceDrainInterval (e.g., 25ms) via Config knob.
3. calc.Start(ctx).
4. source.Update(...)
5. Use require.Never (event-driven) to assert NO CommitAttemptObserved
   for 100ms while grace stays true.
6. Assert calc.pendingPartitionUpdate.Load() == true (via test-only accessor
   OR via metrics counter for deferred updates — TBD during implementation;
   if no clean accessor exists, add a package-private getter behind a build tag
   or use the deferred-rebalance log line as the observation).

Phase B (after grace lifts — drain MUST publish):
7. stateProvider.SetGrace(false)
8. Assert CommitAttemptObserved fires within 200ms (event-driven).
9. Assert calc.publisher.CurrentVersion() advanced.
```

No `time.Sleep` — both assertions use channel/eventually patterns per
`.agents/rules/300-testing.md:19-23`.

### 4.5 Helper unit test — `TestCtxFromStopCh_Lifecycle`

Small unit test for the helper itself (per Codex Additional Tests):
1. Created with already-closed stopCh → ctx returns cancelled immediately.
2. Created live; close stopCh → ctx cancels within 10ms (eventually).
3. Created live; call returned cancel → ctx cancels; subsequent stopCh
   close does not panic; helper goroutine exits.
4. Created with parent ctx that cancels first → ctx cancels; helper
   goroutine exits.

---

## 5. Out of scope for PR-1

- ISSUE-001 (CAS-loss recovery, PR-4): the orthogonal fence for "what
  happens after a commit lands despite step-down". The residual
  single-RPC window of ISSUE-003 is exactly this scenario.
- ISSUE-007 (audit clock-skew, PR-2).
- ISSUE-006/008/004: PR-3 housekeeping.
- IOPS work (CC-IOPS-1..6).

---

## 6. Risk audit (revised)

| Risk | Mitigation |
|---|---|
| `ctxFromStopCh` goroutine leak | `defer cancel()` at every call site; goroutine exits on ctx end. Helper unit test §4.5 asserts. |
| Wrapping ctx inside `rebalance` regresses caller error class | §3.4 `wrapStopErr` reclassifies stop-triggered cancellation to `errShuttingDown`; existing `handleRebalance` suppression (`calculator.go:888-898`) and state-machine logging (`state_machine.go:254-260`) keep working. |
| Drain ticker fires concurrently with watch event | Both go through `pendingPartitionUpdate.CompareAndSwap`/`Store(false)`; only one of them wins the trigger. `rebalanceMu` serialises the actual rebalance. Worst case: one extra rebalance attempt — benign. |
| Drain ticker overhead | Default interval is `min(Cooldown, 30s)` ≥ 1s. One non-blocking check per tick is negligible. Per CC-IOPS-3 evidence, even per-tick W Gets are within noise floor; a bool load is far below that. |
| Residual ISSUE-003 race (gate-then-CAS window) | Test §7.3 strict assertion holds via ctx-aware wrapper (Layer 1 + wrapper.ctx-awareness). Production residual ("bytes on the wire") is ISSUE-001 (PR-4) territory. Layer 2 Publisher gate is defense-in-depth. |
| Grace re-entry race (drain tick → rebalance call) | New atomic re-check inside `rebalance` under `rebalanceMu` (§3.3); drain restores `pendingPartitionUpdate=true` on `errShuttingDown` so next tick retries. |
| `pendingPartitionUpdate` flag observable for tests | Phase-A assertion uses log capture OR an exported test-only accessor; spec leaves implementer to choose. Phase B's positive assertion (CommitAttemptChan fires after grace lift) does NOT depend on flag visibility — that alone proves the deferred-drain semantics. |
| `RebalanceGraceDrainInterval` Config addition | Exported field on `Config`; default-computed when zero. Documented in this spec and in the GoDoc of the field. |
| Publisher gate requires plumbing | `IsShuttingDown func() bool` is a new optional field on PublisherConfig — single insertion, defaults nil (no gate) for existing publisher tests. |
| `RebalanceGraceDrainInterval` Config addition expands scope | Trade-off documented in §3.3. Single optional field with sane default; if a future PR removes the queue we remove the knob. |

---

## 7. LOC budget (revised)

| File | Estimated LOC |
|---|---|
| `calculator.go` (helper, monitorPartitions rewrite, wrapStopErr, drain plumbing) | +50 / −15 |
| `assignment_publisher.go` (gate + 1 PublisherConfig field) | +10 |
| `config.go` (RebalanceGraceDrainInterval optional field + default) | +6 |
| `calculator_test.go` (3 tests + blockingAssignmentKV + helper test) | +280 |
| Total | ~70 LOC production + ~280 LOC tests |

Up from initial ~30/200; the increase is the commit-point fence and the
deferred-drain queue — both required by the P0 findings. Still well
under PR-2 (~20) + PR-3 (~25) for context.
