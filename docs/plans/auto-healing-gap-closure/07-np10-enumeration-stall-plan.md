# NP-10 Leader Enumeration-Stall Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close NP-10 — a leader whose heartbeat-bucket `Keys` scan times out (a non-connectivity `DeadlineExceeded`) while its own single-key heartbeat `Put` keeps succeeding stays `StateStable` and blind to worker topology, serving assignments from stale/empty membership with no degraded circuit (a silent false-healthy leader). Route a *sustained* enumeration stall into the manager's degraded circuit and gate the recovery exit on enumeration recovery.

**Architecture:** `Calculator.getActiveWorkers` (`internal/assignment/calculator.go:1194-1213`) degrades/caches only for `IsConnectivityError`; a `DeadlineExceeded` on the stream-wide `Keys` scan is returned bare and the worker-monitor poll loop only logs it (`internal/assignment/worker_monitor.go:282-284`), so it never reaches the manager. The fix adds a calculator-local consecutive-failure counter that, once a threshold of sustained non-connectivity enumeration failures is crossed, invokes a new manager-supplied `OnEnumerationError` callback; the manager degrades **directly** with a distinct reason and gates the recovery exit on enumeration recovery (a successful `Keys` scan / nil-error `GetActiveWorkers` after the degrade). The direct-degrade path (not the transient KV-error window) is mandatory — see the F-D1 constraint below.

**Tech Stack:** Go (go1.26), NATS JetStream KV, `internal/assignment` (`Calculator`/`WorkerMonitor`/`Config`), the manager degraded circuit (`manager_degraded.go`), and the embedded-NATS test helpers.

> **PRE-IMPLEMENTATION VERIFICATION (2026-06-02) — read before starting; 06 has landed.** The
> recovery-precondition linchpin was checked (read-only) against 06-complete HEAD:
> - **PRIMARY PATH IS VIABLE (the recovery half is NOT a permanent-Degraded trap).** `enterDegraded`
>   (`manager_degraded.go:307`) does NOT stop the calculator or release leadership, and the
>   worker-monitor poll loop (`worker_monitor.go:267-294`) runs on a `hbTTL/2` ticker bound to the
>   calculator lifetime (`m.ctx`/`stopCh`), independent of manager state. In the NP-10 scenario the
>   leader's connection is up and election renewal is a single-key op unaffected by a `Keys`-only
>   stall, so it KEEPS leadership → keeps polling `GetActiveWorkers` → `OnEnumerationSuccess` fires
>   when the stall clears → the reason-scoped exit gate opens. Confirmed the watch-item: the
>   calculator keeps polling Keys while Degraded.
> - **REQUIRED DESIGN ADDITION (not in the Stage-B plan below): a leadership-loss escape.**
>   `stopCalculator` is called on leadership loss (`manager_election.go:274`) and that path does NOT
>   clear `degradedSince`/the reason. So if a leader loses leadership *for any reason* while in
>   `enumeration-stall` Degraded, its poll loop stops and the reason-scoped exit gate can never see a
>   fresh `OnEnumerationSuccess` → **stuck Degraded** (the "gate on a capability that can't fire"
>   deadlock, [[feedback_static_review_blessed_fatal_design]]). It is narrow (a Keys-only stall does
>   not itself fail renewal, so it needs a *concurrent independent* leadership loss), but Stage B MUST
>   add an escape: on leadership loss clear an `enumeration-stall` degrade (or stamp recovery), OR make
>   the exit conjunct treat "not currently leader" as enumeration-N/A (don't block). Make the B.2 proof's
>   RECOVERY half load-bearing (disarm → Stable + HOLD), and add a leadership-loss-while-stalled case.
> - **The verify-first STOP gate still governs:** run the Task B.2 reproducer against the parent FIRST;
>   if the leader does not stay falsely Stable, NP-10 is not real — STOP, do not write Stage B.

---

## Source of truth, confidence, and the load-bearing constraint

- **Spec:** `05-deep-investigation.md` §5 (NP-10) and §8. NP-10 is **Medium confidence — code-derived, NOT harness-proven**. The empirical anchor is that `IsConnectivityError(context.DeadlineExceeded)` is false (bare and wrapped) and `IsDegradingJetStreamError(context.DeadlineExceeded)` is false (`internal/natsutil/errors.go`), so the `Keys` deadline is classified by neither and is logged-only.
- **Verify-first is mandatory — and the fail-first gate is the END-TO-END proof (Task B.2 Step 2), not the unit test.** Two distinct kinds of test here, do not conflate them:
  - The calculator-level Stage A.1 test is a **gap-characterization** test: it asserts the swallow (`enumErrCalls == 0`) and therefore **PASSES as written** on the parent. It documents the unit-level mechanism and seeds the fix-driver; it is NOT the fail-first reproducer.
  - The **fail-first reproducer is the integration proof in Task B.2**: run it against the PARENT (no Stage-B production changes) and confirm it FAILS — the leader stays `StateStable` despite the sustained `Keys` stall. **If that proof does NOT fail on the parent — if the leader already degrades or stops serving stale membership on its own — STOP: NP-10 is not real and no fix should land.** Do not commit Stage B's production changes until the B.2 proof has been seen to fail on the parent.
- **The F-D1 constraint (why the report's §5 one-liner does not work).** §5 suggested routing the stall "with the same `markKVUnavailable`/`recordKVOpError` semantics." That is **defeated by F-D1**: NP-10's defining asymmetry is that the single-key heartbeat `Put` keeps succeeding, and each success fires `recordKVHealthyOp`, which clears the transient (`ErrKVUnavailable`-class) entries from `kvErrorWindow` (`manager_degraded.go:266-288`). A scan deadline routed through `recordKVOpError` adds a transient entry that the next heartbeat success clears — so it never accumulates to `KVErrorThreshold`. **The fix MUST NOT route through the transient window.** Instead, the calculator thresholds locally (consecutive failures) and the manager degrades directly, the same shape the epoch fence uses (`checkBucketEpochs` → `enterDegraded` directly, bypassing the window).
- **Seam constraint (no import cycle).** `markKVUnavailable`/`recordKVOpError`/`enterDegraded` live on the manager side; `internal/assignment` must not import the manager package. The fix adds plain `func` callback fields to `assignment.Config` (matching the existing `LeaderRevision`/`LeaderCheck`/`Now` optional-callback pattern) that the manager sets in `startCalculator` (`manager_assignment.go:132-151`). assignment never imports manager → cycle-free.

## File Map

- Modify: `internal/assignment/config.go:131-153` — add `OnEnumerationError func(error)`, `OnEnumerationSuccess func()`, `EnumerationFailureThreshold int` (+ default in `SetDefaults`).
- Modify: `internal/assignment/calculator.go:67-161` — add an `enumFailures int` counter (guarded by `c.mu`).
- Modify: `internal/assignment/calculator.go:1194-1245` — record/reset enumeration failures in `getActiveWorkers`.
- Modify: `manager_degraded.go` — add `degradedReasonEnumerationStall` const, the manager-side handler + success stamp, the `lastEnumerationSuccessAt` field usage, and the reason-scoped exit conjunct.
- Modify: `manager.go:192-206` — add `lastEnumerationSuccessAt atomic.Int64`.
- Modify: `manager_assignment.go:132-151` — wire `OnEnumerationError`/`OnEnumerationSuccess`.
- Create: `internal/assignment/calculator_enumeration_stall_test.go` — the calculator-level reproducer + fix-driver.
- Create: `test/integration/failure/np10_enumeration_stall_test.go` — the end-to-end false-healthy-leader proof (gated).

## Invariant

A sustained non-connectivity enumeration failure (a `Keys`-scan `DeadlineExceeded` that single-key ops do not share) must become observable as `StateDegraded` on the affected leader, and the leader must NOT exit Degraded until enumeration succeeds again. A transient (single) enumeration failure must NOT degrade (no spurious flap), and the connectivity-error path must stay owned by the existing circuit (heartbeat `Put` `SetOnError` → `recordKVOpError`), unchanged.

---

## Stage A — Reproduce the gap (verify-first) and drive the seam at the calculator level

### Task A.1: Calculator-level reproducer — the enumeration deadline is swallowed today

**Files:**
- Create: `internal/assignment/calculator_enumeration_stall_test.go`

- [ ] **Step 1: Write the op-selective Keys-timeout fault KV + the swallow proof**

```go
package assignment

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// keysTimeoutKV wraps a real KeyValue and, when armed, makes Keys() block until
// the caller's context deadline (returning context.DeadlineExceeded) while every
// other op — notably Put/Get — passes through unchanged. This is NP-10's exact
// asymmetry: the stream-wide scan times out, single-key ops do not.
type keysTimeoutKV struct {
	jetstream.KeyValue
	armed atomic.Bool
}

func (k *keysTimeoutKV) Keys(ctx context.Context, opts ...jetstream.WatchOpt) ([]string, error) {
	if k.armed.Load() {
		<-ctx.Done()
		return nil, ctx.Err() // context.DeadlineExceeded under the monitor's bounded ctx
	}
	return k.KeyValue.Keys(ctx, opts...)
}

// TestGetActiveWorkers_EnumerationDeadline_IsSwallowed pins the NP-10 gap at the
// calculator level: a Keys()-only DeadlineExceeded is neither a connectivity nor
// a degrading-JetStream error, so getActiveWorkers returns it bare and nothing
// (no callback, no degrade) observes it. Predicted verdict (pre-fix): the error
// is returned but OnEnumerationError is never invoked.
func TestGetActiveWorkers_EnumerationDeadline_IsSwallowed(t *testing.T) {
	t.Parallel()

	// Build a Calculator over a real embedded-NATS heartbeat KV wrapped by
	// keysTimeoutKV. (Use the package's existing calculator test scaffolding to
	// construct a minimal valid Config; see calculator_test.go helpers.)
	fault := &keysTimeoutKV{KeyValue: newTestHeartbeatKV(t)}
	var enumErrCalls atomic.Int64
	c := newTestCalculator(t, func(cfg *Config) {
		cfg.HeartbeatKV = fault
		cfg.OnEnumerationError = func(error) { enumErrCalls.Add(1) } // wired but, pre-fix, never called
	})

	fault.armed.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	_, _, err := c.getActiveWorkers(ctx)
	require.Error(t, err, "a Keys-scan deadline must surface as an error")
	require.NotErrorIs(t, err, context.Canceled)

	// THE GAP (pre-fix): the deadline is classified by neither IsConnectivityError
	// nor IsDegradingJetStreamError, so getActiveWorkers returns it bare and the
	// enumeration-error callback is never invoked — the manager never learns.
	require.Zero(t, enumErrCalls.Load(),
		"pre-fix: a swallowed enumeration deadline must not reach any degrade signal (this is the gap)")
}
```

> The helpers `newTestHeartbeatKV` / `newTestCalculator` stand in for the package's existing calculator test scaffolding — if equivalents already exist (e.g. in `calculator_test.go`), reuse them; otherwise add minimal ones in this step. `OnEnumerationError` is referenced here to make the proof legible; Task A.2 Step 1 adds the field, so write this test and the field together if the package will not compile otherwise.

- [ ] **Step 2: Run — confirm the swallow**

```bash
go test ./internal/assignment -run TestGetActiveWorkers_EnumerationDeadline_IsSwallowed -count=1 -v
```

Expected: PASS as written (it asserts the *gap* — `enumErrCalls == 0`). This proof flips meaning after Task A.2: the fix makes a *sustained* run cross the threshold and call the callback, which a follow-up assertion (Task A.2 Step 4) checks.

### Task A.2: Add the seam and the calculator-local threshold

**Files:**
- Modify: `internal/assignment/config.go`
- Modify: `internal/assignment/calculator.go`
- Modify: `internal/assignment/calculator_enumeration_stall_test.go`

- [ ] **Step 1: Add the Config callbacks + threshold**

In `internal/assignment/config.go`, in the "Optional dependencies" block (after `LeaderCheck`, `:152`):

```go
	// OnEnumerationError, if set, is invoked when worker enumeration (the
	// heartbeat Keys scan in WorkerMonitor.GetActiveWorkers) has failed
	// EnumerationFailureThreshold times in a row with a non-connectivity error —
	// notably a context.DeadlineExceeded on the stream-wide scan while single-key
	// heartbeat Puts still succeed (NP-10). The manager wires this to a DIRECT
	// degrade (NOT the transient KV-error window, which a succeeding heartbeat Put
	// would clear). When nil, sustained enumeration failure remains logged-only
	// (back-compat / test default). Must be safe for concurrent use.
	OnEnumerationError func(err error)

	// OnEnumerationSuccess, if set, is invoked on every SUCCESSFUL worker
	// enumeration — i.e. whenever the heartbeat Keys scan (GetActiveWorkers)
	// returns a nil error — REGARDLESS of the F10-A worker-credibility decision
	// (it fires even when a sharply-shrunk result is treated as suspicious). The
	// manager stamps it as the "enumeration recovered" signal its recovery exit
	// gate keys on. Must be safe for concurrent use.
	OnEnumerationSuccess func()

	// EnumerationFailureThreshold is the number of consecutive non-connectivity
	// enumeration failures before OnEnumerationError fires. Default: 3 (also applied
	// to any value < 1, so a zero/negative config cannot fire on the first failure).
	// Set explicitly to 1 to fire on the first failure (no debounce).
	EnumerationFailureThreshold int
```

In `SetDefaults` (after the worker-shrink defaults, `:236`) — clamp `< 1`, not just `== 0`, so a negative value cannot fire immediately:

```go
	if c.EnumerationFailureThreshold < 1 {
		c.EnumerationFailureThreshold = 3
	}
```

- [ ] **Step 2: Add the counter field**

In `internal/assignment/calculator.go`, in the `Calculator` struct near `workerShrunkObservations` (`:161`):

```go
	// enumFailures is the running count of consecutive non-connectivity
	// enumeration (heartbeat Keys scan) failures. Crossed against
	// EnumerationFailureThreshold to fire OnEnumerationError; reset to zero on any
	// successful enumeration (nil-error GetActiveWorkers, before F10-A handling).
	// Guarded by c.mu (same as workerShrunkObservations).
	enumFailures int
```

- [ ] **Step 3: Record/reset around the enumeration result**

In `getActiveWorkers`, the swallowed non-connectivity error branch (`calculator.go:1213`):

```go
		// Sustained non-connectivity enumeration failure (NP-10): the poll loop
		// only logs this and no caller routes it to the degraded circuit. Threshold
		// locally and, once sustained, surface it via OnEnumerationError so the
		// manager can degrade directly (a transient-window route would be cleared by
		// the still-succeeding heartbeat Put — see 07-...-plan F-D1 constraint).
		c.recordEnumerationFailure(err)

		return nil, false, err
```

Reset/signal recovery **immediately after `c.monitor.GetActiveWorkers(ctx)` returns a nil error** (`calculator.go:1195`), BEFORE the F10-A worker-shrink credibility handling — NOT on the later "healthy fresh" path. The enumeration STALL has recovered the moment the `Keys` scan succeeds; whether F10-A then treats the result as suspicious (returning cached+`fresh=false`, `calculator.go:1227-1243`) is an orthogonal worker-credibility decision. Placing the reset on the fresh-only path would leave a recovered-but-suspicious scan stuck in the enumeration-stall degrade:

```go
	workers, err := c.monitor.GetActiveWorkers(ctx)
	if err != nil {
		if natsutil.IsConnectivityError(err) {
			// ... existing cached-fallback / ErrDegraded, unchanged ...
		}
		c.recordEnumerationFailure(err) // NP-10: increment + maybe fire (added above)
		return nil, false, err
	}
	// Enumeration succeeded: the Keys-scan stall (if any) has recovered. Reset the
	// NP-10 counter and signal recovery HERE — before F10-A suspicious-shrink
	// handling — so a recovered-but-suspicious scan still clears the stall degrade.
	c.resetEnumerationFailures()

	// ... existing F10-A recordWorkerObservation / suspicious-shrink handling ...
```

Add the two helpers:

```go
// recordEnumerationFailure increments the consecutive enumeration-failure
// counter and invokes OnEnumerationError once the threshold is reached/exceeded.
// Firing while already over the threshold is intentional and harmless: the
// manager's enterDegraded is idempotent (CAS short-circuits while degraded).
func (c *Calculator) recordEnumerationFailure(err error) {
	if c.OnEnumerationError == nil {
		return
	}
	c.mu.Lock()
	c.enumFailures++
	fire := c.enumFailures >= c.EnumerationFailureThreshold
	c.mu.Unlock()
	if fire {
		c.OnEnumerationError(err)
	}
}

// resetEnumerationFailures clears the consecutive-failure counter and signals
// enumeration recovery. Called on every SUCCESSFUL enumeration — right after a
// nil-error GetActiveWorkers, before F10-A suspicious-shrink handling — NOT only
// on the fresh==true path (a recovered-but-suspicious scan must still clear).
func (c *Calculator) resetEnumerationFailures() {
	c.mu.Lock()
	c.enumFailures = 0
	c.mu.Unlock()
	if c.OnEnumerationSuccess != nil {
		c.OnEnumerationSuccess()
	}
}
```

> Connectivity errors take the existing `IsConnectivityError` branch (cached fallback / `ErrDegraded`) and do NOT touch `enumFailures` — that fault class is already owned by the manager circuit via the heartbeat `Put` `SetOnError`. Only the swallowed non-connectivity branch increments.

- [ ] **Step 4: Extend the reproducer to prove the threshold fires (fix-driver)**

Add to `calculator_enumeration_stall_test.go`:

```go
// TestGetActiveWorkers_SustainedEnumerationDeadline_FiresCallback proves the fix:
// EnumerationFailureThreshold consecutive Keys deadlines invoke OnEnumerationError
// exactly once at the crossing, and a subsequent healthy scan invokes
// OnEnumerationSuccess and resets the counter.
func TestGetActiveWorkers_SustainedEnumerationDeadline_FiresCallback(t *testing.T) {
	t.Parallel()

	fault := &keysTimeoutKV{KeyValue: newTestHeartbeatKV(t)}
	var enumErr, enumOK atomic.Int64
	c := newTestCalculator(t, func(cfg *Config) {
		cfg.HeartbeatKV = fault
		cfg.EnumerationFailureThreshold = 3
		cfg.OnEnumerationError = func(error) { enumErr.Add(1) }
		cfg.OnEnumerationSuccess = func() { enumOK.Add(1) }
	})

	fault.armed.Store(true)
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		_, _, _ = c.getActiveWorkers(ctx)
		cancel()
	}
	require.GreaterOrEqual(t, enumErr.Load(), int64(1),
		"a sustained enumeration deadline must fire OnEnumerationError once the threshold is crossed")

	// Recovery: a healthy scan resets the counter and signals success.
	fault.armed.Store(false)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, _, err := c.getActiveWorkers(ctx)
	require.NoError(t, err)
	require.Positive(t, enumOK.Load(), "a healthy enumeration must signal recovery")
}
```

- [ ] **Step 5: Run + commit**

```bash
go test ./internal/assignment -run 'TestGetActiveWorkers_(EnumerationDeadline_IsSwallowed|SustainedEnumerationDeadline_FiresCallback)' -count=1 -v
make lint
git diff --check
git add internal/assignment/config.go internal/assignment/calculator.go internal/assignment/calculator_enumeration_stall_test.go
git commit -m "feat(assignment): surface sustained heartbeat-enumeration stalls via a callback

A heartbeat Keys-scan deadline is neither a connectivity nor a degrading-
JetStream error, so getActiveWorkers returned it bare and the poll loop only
logged it. Add a consecutive-failure threshold that invokes a new optional
OnEnumerationError callback once a non-connectivity enumeration failure is
sustained, plus an OnEnumerationSuccess recovery signal."
```

---

## Stage B — Degrade the leader and gate its recovery exit

### Task B.1: Manager-side direct degrade + reason-scoped recovery exit

**Files:**
- Modify: `manager.go:192-206`
- Modify: `manager_degraded.go`
- Modify: `manager_assignment.go:132-151`

- [ ] **Step 1: Add the enumeration-success stamp field**

In `manager.go`, in the "Degraded mode tracking" block:

```go
	// lastEnumerationSuccessAt is the UnixNano of the most recent healthy worker
	// enumeration (set via the calculator's OnEnumerationSuccess). The recovery
	// exit gate keys on it to confirm a heartbeat-enumeration-stall degrade
	// recovered before exiting. 0 = never.
	lastEnumerationSuccessAt atomic.Int64
```

- [ ] **Step 2: Add the reason const + handler + stamp**

In `manager_degraded.go`, near `degradedReasonKVUnavailable` (`:20`):

```go
// degradedReasonEnumerationStall is the distinct enterDegraded reason for a
// sustained leader-side worker-enumeration (heartbeat Keys scan) stall that the
// connectivity/degrading classifiers miss (NP-10). Kept separate so the recovery
// exit can require an enumeration success before exiting, and so the operator
// surface distinguishes it from a kv-unavailable op stall.
const degradedReasonEnumerationStall = "heartbeat-enumeration-stall"
```

Add the manager handler + success stamp (the calculator has already thresholded, so the handler degrades DIRECTLY — bypassing the transient `kvErrorWindow`, mirroring the epoch fence):

```go
// onEnumerationStall is wired to the calculator's OnEnumerationError. The
// calculator fires it only after EnumerationFailureThreshold consecutive
// non-connectivity enumeration failures, so the stall is already sustained:
// degrade directly. enterDegraded is idempotent (CAS), so repeated calls while
// degraded are no-ops.
func (m *Manager) onEnumerationStall(err error) {
	m.logger.Warn("sustained heartbeat-enumeration stall; entering degraded", "error", err)
	m.enterDegraded(degradedReasonEnumerationStall)
}

// recordEnumerationSuccess is wired to the calculator's OnEnumerationSuccess. It
// stamps the recovery signal the exit gate keys on.
func (m *Manager) recordEnumerationSuccess() {
	m.lastEnumerationSuccessAt.Store(time.Now().UnixNano())
}
```

- [ ] **Step 3: Add the reason-scoped exit conjunct**

In `attemptRecoveryFromDegraded`, add a third additive conjunct (after the Family A epoch re-probe conjunct from `06-deep-gap-fix-plan.md` Task 1.2, before `m.exitDegraded()`). REUSE the `reason` variable already read at the top of the exit block in `06` (do NOT re-read with `:=` — the empty-reason guard from `06` Task 1.1 Step 5 has already returned if `reason == ""`, so this conjunct only ever sees a fully-observable reason):

```go
	// NP-10 — reason-scoped enumeration-recovery gate. A heartbeat-enumeration
	// stall degrade must not exit on the healthy assignment read while the Keys
	// scan is still timing out (the leader would resume serving stale membership).
	// Require an enumeration success stamped AFTER we degraded. Reason-scoped so
	// other degrade reasons are unaffected. `reason` is the hoisted variable from
	// 06's exit block (already guarded for "").
	if reason == degradedReasonEnumerationStall {
		ensAt := m.lastEnumerationSuccessAt.Load()
		since := m.degradedSince.Load()
		if ensAt == 0 || ensAt <= since {
			m.logger.Debug("recovery: worker enumeration not recovered since stall degrade; staying Degraded",
				"last_enumeration_success_unixnano", ensAt, "degraded_since_unixnano", since)
			return
		}
	}

	// Success - exit degraded mode
	m.exitDegraded()
```

> **Depends on `06`.** This conjunct assumes `06`'s exit block already exists, including the hoisted `reason` read + the empty-reason guard, AND the `lastDegradedReason` ownership protocol (`06` Task 1.1 Step 4: the winning `enterDegraded` stores the reason AFTER its CAS; `exitDegraded` clears it before `degradedSince`). NP-10 introduces a NEW reason (`degradedReasonEnumerationStall`) that the manager's `onEnumerationStall` handler degrades with directly — so it relies on that ownership being atomic with the entry, exactly as `kv-unavailable` does. If `07` is implemented before `06` lands, port the full ownership protocol (not just the field) here first, or the new reason can be clobbered and skip this gate. State the dependency in the PR.

- [ ] **Step 4: Wire the callbacks in `startCalculator`**

In `manager_assignment.go`, in the `assignment.Config{...}` literal (`:132-151`), add:

```go
		OnEnumerationError:    m.onEnumerationStall,
		OnEnumerationSuccess:  m.recordEnumerationSuccess,
```

### Task B.2: End-to-end false-healthy-leader proof

**Files:**
- Create: `test/integration/failure/np10_enumeration_stall_test.go`

- [ ] **Step 1: Write the gated end-to-end proof**

Build a real fleet on an op-selective fault JetStream that times out `Keys()` on the heartbeat bucket only (the integration analog of `keysTimeoutKV`, applied at the JetStream layer so the manager's own heartbeat KV is affected). Assert the gap (pre-fix) and the fix (post-fix) in one gated test:

```go
//go:build integration
// (mirror the package's existing gating/build conventions; gate with
// PARTI_RUN_NP10_ENUM_STALL_PROOF and testing.Short() like the sibling proofs.)

// TestNP10_LeaderEnumerationStall_DegradesNotSilent proves a leader whose
// heartbeat Keys scan sustains DeadlineExceeded (while its single-key Put keeps
// succeeding) enters Degraded("heartbeat-enumeration-stall") instead of sitting
// StateStable blind to topology, and recovers once the scan succeeds again.
//
// Oracle:
//   1. Bring up a 3-manager fleet; identify the leader; reach all-Stable + coverage.
//   2. Arm a Keys-only timeout on the heartbeat bucket (Put/Get/election stay healthy).
//   3. PRE-FIX EXPECTATION (the gap): the leader stays StateStable (no degrade) —
//      this is what fails today; the assertion below requires it to DEGRADE.
//   4. POST-FIX: within ~EnumerationFailureThreshold poll intervals the leader
//      enters Degraded with reason "heartbeat-enumeration-stall", and the
//      connection stays CONNECTED throughout (this is not a connectivity loss).
//   5. Disarm the Keys timeout; assert the leader returns to Stable and HOLDS it.
```

Provide the full fault wrapper + fleet wiring + assertions when implementing (no placeholder): the wrapper embeds `jetstream.JetStream`, returns a `KeyValue` wrapper for the heartbeat bucket whose `Keys` blocks-until-deadline when armed, and passes through everything else. Record terminal reason via an `OnDegraded` hook and assert `reason == "heartbeat-enumeration-stall"`.

- [ ] **Step 2: Run the proof against the parent commit first (verify-first)**

Before Stage B's production changes are present, run the proof and confirm the leader does NOT degrade (the gap). If it already degrades or stops serving stale membership, STOP — NP-10 is not real; revert Stage B and record the finding.

```bash
git stash   # park Stage B production changes if interleaved
PARTI_RUN_NP10_ENUM_STALL_PROOF=1 go test ./test/integration/failure -run TestNP10_LeaderEnumerationStall_DegradesNotSilent -count=1 -v
git stash pop
```

Expected (parent): FAIL — leader stays Stable (the gap).

- [ ] **Step 3: Run with the fix + regressions (`-race`); commit**

```bash
PARTI_RUN_NP10_ENUM_STALL_PROOF=1 go test -race ./test/integration/failure -run TestNP10_LeaderEnumerationStall_DegradesNotSilent -count=1
go test -race . -run 'TestAttemptRecovery|TestNP5_BlockedApply' -count=1
go test -race ./internal/assignment -run 'TestGetActiveWorkers' -count=1
make lint
git diff --check
git add manager.go manager_degraded.go manager_assignment.go test/integration/failure/np10_enumeration_stall_test.go
git commit -m "fix(degraded): degrade a leader on a sustained heartbeat-enumeration stall

A leader whose heartbeat Keys scan sustains a non-connectivity deadline while its
single-key Put still succeeds used to stay Stable and serve stale membership.
Degrade directly with a distinct reason when the calculator reports a sustained
enumeration stall, and gate the recovery exit on a successful enumeration since the degrade."
```

---

## Final Verification

- [ ] **Step 1: Format + focused checks**

```bash
make fmt
go test ./internal/assignment -run 'TestGetActiveWorkers|TestCalculator' -count=1
go test . -run 'Test(AttemptRecovery|NP5_BlockedApply)' -count=1
```

- [ ] **Step 2: `-race` + lint + pre-PR**

```bash
go test -race ./internal/assignment . -count=1
make lint
git diff --check
make pre-pr
```

Expected: PASS. NP-10's gated end-to-end proof stays opt-in (`PARTI_RUN_NP10_ENUM_STALL_PROOF`) until verified stable, matching the sibling proof convention.
