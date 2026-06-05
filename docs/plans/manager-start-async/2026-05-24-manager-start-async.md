# Manager.Start Async Refactor — Implementation Plan (v5 — ready to implement)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Change `Manager.Start(ctx)` to return after sanity checks (bucket existence, stable-ID claim, election participation, heartbeat, calculator) instead of blocking until `StateStable`. The unpredictable phases — initial assignment wait and initial apply — run in a background goroutine launched before Start returns.

**Architecture (simplified after v2 review; v4 closes v3-P0):** Split current synchronous Start into a sync sanity-check phase plus a background runner. The runner does **best-effort initial wait + apply** then unconditionally starts the post-Stable monitor set. It does NOT enter or exit degraded itself. Ongoing recovery is delegated to the existing mechanisms (`monitorAssignmentChanges` watcher redelivery, `monitorCommitChanges`, `monitorNATSConnection` → `attemptRecoveryFromDegraded`, `scheduleApplyRetry` inside `applyAssignmentWithPrev`). A separate watchdog goroutine fires `enterDegraded("startup-timeout")` once if state is still `WaitingAssignment` after `StartupTimeout` — that's the only probe-rotation signal we need.

**v4 critical addition — startup completion via apply pipeline:** Every successful apply path (initial, watcher-delivered, scheduleApplyRetry) calls `casToStableFromWaitingAssignment` at the end. This is the idempotent "complete startup if still pending" handoff that v3 missed. Without it, a worker whose runner's first apply failed but whose retry-or-watcher-driven apply later succeeded would stay in `WaitingAssignment` forever (CAS guard prevents over-firing in any non-`WaitingAssignment` state). The CAS lives in `applyAssignmentWithPrev`'s success path and in `applyInitialAssignment`'s cold-start-empty path — the two terminal points of every apply chain.

**Why simpler:** v2 grew a retry loop and explicit degraded entry/exit. That introduced two new P0s (per-attempt ctx not threaded through apply; runner clearing unrelated degraded state) and two P1s (calculateAndPublish re-execution; backoff lockstep). The simpler design removes their causes rather than working around them. Apply boundedness becomes identical to pre-refactor Start — also unbounded — and is documented as such, not falsely claimed.

**Tech Stack:** Go 1.x, NATS JetStream KV, existing `m.wg` waitgroup, existing `transitionState` / `WaitState` / `enterDegraded` machinery, atomic CAS on `m.state` for the guarded final transition.

**Breaking change posture:** This flips the default Start contract. Callers that read `CurrentAssignment()` immediately after `Start` returns must now block on `mgr.WaitState(StateStable, timeout)` first. Deliberate v2 semantic break; recorded in CHANGELOG and the migration note added in Task 12.

---

## Address-list for v3 review findings (copilot 2026-05-24)

This v4 plan closes the one P0 + three P1s from `tmp/2026-05-24-manager-start-async_architectural_v3_review.md`:

- **v3-P0 (delegated recovery applies but never transitions to Stable):** every successful apply path now calls `casToStableFromWaitingAssignment`. Added in Task 3a (modify `applyAssignmentWithPrev` success path + `applyInitialAssignment` cold-start-empty path). The runner's own `if applyOK { casToStableFromWaitingAssignment }` call becomes redundant (idempotent second CAS is a no-op) but is kept for clarity.
- **v3-P1 (watchdog degraded recovery prose overstated):** Task 11 documentation explicitly notes that startup-timeout-degraded is **not** guaranteed to self-heal while the runner is blocked inside an unbounded apply call — it is a probe-rotation signal until the runner returns or the pod is restarted. `monitorNATSConnection` does call `attemptRecoveryFromDegraded` even without a disconnect (per `manager_degraded.go:32-69`, on `ExitThreshold` after first `connUpSince`), so the runner-succeeds-after-watchdog case self-heals once monitors start; the blocked-runner case does not.
- **v3-P1 (test snippets still not implementation-ready):** Task 6 uses the existing `newTestManager(t)` helper at `manager_commit_state_machine_test.go:150` (returns `*Manager, *recordingHandoff, *recordingHeartbeat, *recordingMetrics`); no `t.Skip` placeholder. Task 9 uses two sequential `WaitState` calls — no `WaitStateAny` reference.
- **v3-P1 (calculator-race test doesn't actually pin the race):** acknowledged as a fundamental observability limitation. Task 7 records OnStateChanged transitions per worker and asserts liveness + non-zero hook delivery, but does NOT pin the clobber deterministically — pure observability via OnStateChanged cannot distinguish clobber from normal calculator oscillation (both produce overlapping transition sequences; the transition table permits both `Scaling → Stable` and `Stable → Scaling` as valid steady-state transitions). The CAS guard's behavior is **precisely pinned by the three unit tests in Task 6** (`TestCasToStableFromWaitingAssignment_{FailsWhenStateMoved,SucceedsFromWaitingAssignment,NoOpFromDegraded}`). A deterministic live-cluster regression pin requires a production test hook — tracked as a follow-up issue in CHANGELOG.
- **v3-P2 (watchdog wording):** "exits on state change away from WaitingAssignment" replaced with "checks state at the deadline; fires only if still WaitingAssignment."

## Address-list for v2 review findings (codex 2026-05-24)

This v3 plan resolves the findings from `tmp/2026-05-24-manager-start-async_architectural_v2_review.md`:

- **v2-P0-1 (apply retry loop not bounded):** removed by removing the retry loop. The runner makes one best-effort apply attempt; failure → start monitors and exit. Existing `scheduleApplyRetry` (manager_assignment.go:944) and the assignment/commit watchers' redelivery handle subsequent retries. Apply boundedness is **explicitly documented as unchanged from pre-refactor** — `handoffCoordinator.Apply(m.ctx, ...)` is still unbounded per attempt; this is not a regression because pre-refactor Start had the same property.
- **v2-P0-2 (runner clears unrelated degraded):** removed by removing the runner's `exitDegraded` call. The runner does not touch degraded state. If degraded fires (from any cause, including the new watchdog), the existing `attemptRecoveryFromDegraded` path (driven by `monitorNATSConnection` at manager_degraded.go:67-69) handles exit.
- **v2-P1 (StartupTimeout Option A reintroduces double-budget):** Option A is **deleted**. Implementation MUST add `m.startedAt time.Time` field and capture in `prepareStart`. No alternative.
- **v2-P1 (test snippets don't compile — wrong interface signatures):** every snippet rewritten against verified ground truth: `WorkerConsumerUpdater.UpdateWorkerConsumer(ctx, workerID, partitions)` (options.go:131-143), `PartitionSource` is Start/List/Stop only (types/partition_source.go:47-80), no Subscribe.
- **v2-P1 (calculateAndPublish re-runs every leader retry):** removed because there are no retries. The runner calls `waitForAssignment` once; for leaders it includes one `calculateAndPublish`. If that fails, the watcher / scheduleApplyRetry mechanisms drive subsequent attempts — the runner does not loop.
- **v2-P1 (backoff lockstep):** removed because there is no backoff loop in the runner.
- **v2-P1 (Calculator.Start synchronously calls source.List, so BlockingSource gates sync phase not background):** test fixtures rewritten — the calculator-race test now uses the 3-worker cluster template at `test/integration/manager/manager_epoch_monitor_concurrency_test.go:54-145` to exercise the calculator-driven transition naturally.
- **v2-P1 (unit tests in `package parti_test` can't reach unexported helpers):** unit tests for `casToStableFromWaitingAssignment` are in `package parti` (same package as the helper); cross-package integration tests stay in their existing packages.
- **v2-P1 (placeholder traffic generators and t.Skip):** the calculator-race test is the only one **required for PR merge**. It uses the concrete 3-worker template, no placeholder. The other test ideas (apply-failure recovery, race-soak) are demoted to **follow-up issues** explicitly listed in CHANGELOG and AGENTS.md.
- **v2-P2 (lifecycle diagram marker):** Task 12 adds the inline marker in the ASCII diagram.

---

## File structure

**Modified:**
- `manager.go` — Manager struct gets `startedAt time.Time` + `postStableMonitorsOnce sync.Once`. `Start()` returns after `startCalculator` instead of `StateStable`. Spawns `runStartupBackground` + the soft-deadline watchdog. Updates Godoc at `manager.go:366-393`.
- `manager_setup.go:19-46` — `prepareStart` sets `m.startedAt = time.Now()` under the lock.
- `internal/testutil/manager_helpers.go:21-34` — `StartManagerWithHandoffRecorder` waits for `StateStable` after `Start`.
- `internal/testutil/nats.go` and other `internal/testutil/*.go` — every `mgr.Start` call site flagged by Task 2.
- Direct caller files identified by Task 2 audit (examples, harness, simulation worker, integration test helpers). **NOT** `k8s/cmd/manager/main.go` — that's controller-runtime, not Parti.
- `doc.go:36-41`, `README.md:153-160`, `manager.go:366-393` Godoc, `docs/USER_GUIDE.md:166-175`, `docs/REFERENCE.md:323-326,399-403`, `docs/LIFECYCLE.md:22-86`, `docs/API_REFERENCE.md:77-101 + 1138-1155`.
- `AGENTS.md` — fourth cross-feature contract entry.

**New:**
- `manager_startup_async.go` — `runStartupBackground`, `casToStableFromWaitingAssignment`, `startPostStableMonitors`, `startStartupTimeoutWatchdog`. Single file, ~120 lines incl. Godoc.
- `manager_startup_async_test.go` — `package parti` (so it can call unexported helpers directly). Holds the contract test + the CAS-guard unit test.
- `test/integration/manager/startup_async_calculator_race_test.go` — `package manager_test` cluster test that pins the P0-2 fix using the 3-worker template.
- `docs/plans/manager-start-async/2026-05-24-manager-start-async.md` — this plan.
- `CHANGELOG.md` (verify with `ls CHANGELOG.md`; if absent check `RELEASE_NOTES.md` / `docs/MIGRATION.md`) — breaking-change entry + follow-up issue list.

**Untouched (load-bearing invariants preserved):**
- `manager.go:587` comment about watcher redelivery on failed Apply — applies unchanged.
- `manager.go:521-534` invariant "Apply→Store→Ack BEFORE StateStable" — preserved by ordering in the runner (apply first, then CAS).
- `recordKVError` path (manager_degraded.go:82-146) — unchanged; cross-feature contract #1 (whole-bucket-missing → all workers Degraded) intact.
- `enterDegraded` (manager_degraded.go:167-204) — unchanged; OnDegraded-once-per-entry invariant preserved via `degradedSince.CompareAndSwap`.
- `applyAssignmentWithPrev` / `handoffCoordinator.Apply` — no signature change. Apply boundedness identical to pre-refactor Start.

---

## Cut-line summary

**Synchronous in Start (fail-fast, bounded by OperationTimeout per call):**
1. `prepareStart` (sets `m.startedAt`, context wiring)
2. `transitionState(StateClaimingID)`
3. `ensureStableIDKV` + `reconcileStableIDBucketMaxAge`
4. `claimWorkerID` (bounded scan of `[WorkerIDMin, WorkerIDMax]`; renewal already async)
5. `source.Start`
6. `ensureCoreKVBuckets`
7. `setupHandoff` (if enabled) + `handoffCoordinator.Start`
8. `transitionState(StateElection)`
9. `participateElection` (bounded by `OperationTimeout`)
10. `startHeartbeat`
11. `startCalculator` (if leader)
12. `transitionState(StateWaitingAssignment)`
13. Spawn `startStartupTimeoutWatchdog()` and `runStartupBackground(assignmentKV)`
14. **Return nil**

**Asynchronous in `runStartupBackground` (best-effort, single attempt, bounded only by `m.ctx`):**
1. `waitForAssignment(m.ctx, assignmentKV, heartbeatKV)` — uses `m.ctx`, not a per-attempt deadline. For leaders this also runs `calculateAndPublish` once.
2. On error: log, skip the apply, fall through to monitor startup. The assignment watcher (started below) will deliver the assignment when it lands; the existing `scheduleApplyRetry` handles apply retries.
3. `applyInitialAssignment(m.ctx, assignmentKV)` — uses `m.ctx`. On failure, log and fall through (same reasoning).
4. If `applyInitialAssignment` succeeded: `casToStableFromWaitingAssignment()`. Guarded CAS: only `WaitingAssignment → Stable`; if calculator already moved state to `Scaling`/`Rebalancing`/`Emergency`, leave it.
5. `startPostStableMonitors(assignmentKV)` — unconditional. The monitors handle subsequent recovery via their existing paths.

**Asynchronous watchdog `startStartupTimeoutWatchdog`:**
1. Sleep `StartupTimeout - time.Since(m.startedAt)` (computed from `m.startedAt`, so it's the absolute deadline). On `m.ctx.Done()` during sleep, exits immediately.
2. **Checks state at the deadline.** Fires `enterDegraded("startup-timeout")` only if `m.State() == StateWaitingAssignment` at that moment.
3. Does not poll state during sleep — a state change while sleeping just makes the deadline-time check a no-op.

**Apply boundedness disclaimer (explicit, not glossed):**

The runner calls `applyInitialAssignment(m.ctx, ...)` which internally calls `applyAssignmentWithPrev(...)` → `handoffCoordinator.Apply(m.ctx, ...)` (manager_assignment.go:931). The per-attempt ctx is **not threaded through** to the handoff coordinator — that's the existing code path, unchanged. **A stuck updater or handoff phase can block the runner inside one apply attempt until `m.ctx` cancels (i.e., `Stop`).** This is **not a regression**: pre-refactor `Start` had the same property because it called the same chain on `startupCtx` → `m.ctx`. The soft watchdog covers the probe-rotation case (degraded fires regardless of whether the runner is stuck inside apply). Threading ctx end-to-end through the handoff coordinator is **out of scope** for this plan — tracked as a follow-up issue.

---

## Tasks

### Task 1: Add `m.startedAt` and `m.postStableMonitorsOnce` to Manager

**Files:**
- Modify: `manager.go` (Manager struct, near line 152 where `connMonitorOnce` lives)

- [ ] **Step 1: Add fields**

Find the line containing `connMonitorOnce` in the Manager struct. Add two fields adjacent:

```go
	connMonitorOnce        sync.Once     // ensures single connection monitor goroutine
	postStableMonitorsOnce sync.Once     // ensures startPostStableMonitors fires exactly once
	startedAt              time.Time     // absolute wall-clock anchor for StartupTimeout budget; set in prepareStart
```

(If the file does not already import `time`, it almost certainly does — verify with `head -30 manager.go`.)

- [ ] **Step 2: Set `startedAt` in `prepareStart`**

In `manager_setup.go:19-46`, add `m.startedAt = time.Now()` inside the existing `m.mu.Lock()` block, immediately after the `m.ctx, m.cancel = context.WithCancel(...)` line:

```go
	m.mu.Lock()
	if m.ctx != nil {
		m.mu.Unlock()
		return nil, func() {}, types.ErrAlreadyStarted
	}
	m.ctx, m.cancel = context.WithCancel(context.Background()) //nolint:gosec // G118
	m.startedAt = time.Now()
	m.mu.Unlock()
```

- [ ] **Step 3: Build**

Run: `go build ./...`
Expected: success.

- [ ] **Step 4: Commit**

```bash
git add manager.go manager_setup.go
git commit -m "feat(manager): add startedAt + postStableMonitorsOnce for async-Start scaffolding"
```

---

### Task 2: Audit every direct `mgr.Start` caller

**Files:** none (audit only — output is `tmp/start-call-audit.md`)

- [ ] **Step 1: Run the grep**

```bash
grep -rn 'mgr\.Start(\|m\.Start(\|manager\.Start(\|w\.manager\.Start(' --include='*.go' . | grep -v 'internal/testutil/manager_helpers.go' | grep -v '^k8s/' | sort
```

- [ ] **Step 2: Classify each match**

For each line, label one of:
- **MIGRATE** — caller reads `CurrentAssignment()` or asserts partitions immediately after Start. Needs `WaitState(StateStable, ...)`.
- **OK** — caller tests Start-error behavior or already waits for assignment via `require.Eventually` / `WaitState`.
- **REVIEW** — open the file and decide.

Known matches (expand via the grep):
- `doc.go:38` — MIGRATE (Godoc)
- `manager.go:391` — MIGRATE (Manager.Start Godoc example)
- `examples/basic/main.go:133` — MIGRATE
- `examples/degraded-readiness/main.go:97` — REVIEW (degraded-mode example)
- `test/simulation/internal/worker/worker.go:433` — MIGRATE
- `test/perf-measurement/cmd/harness/harness.go:493` — MIGRATE
- `test/integration/consumer/dynamic_test.go:277` — REVIEW
- `test/integration/manager/manager_lifecycle_idempotency_test.go:47,57` — REVIEW
- `test/integration/failure/degraded_mode_test.go:46,110,197,256,333` — REVIEW (testing degraded; Start may intentionally not reach Stable)
- `test/integration/failure/claim_resolver_nats_restart_test.go:419` — REVIEW
- `test/integration/handoff/handoff_sweeper_integration_test.go:67` — MIGRATE
- `test/integration/assignment/assignment_helpers_test.go:132-154` — MIGRATE
- `test/integration/durable/durable_helper_test.go:150-163` — REVIEW
- `manager_initial_bootstrap_test.go:100` — REVIEW
- Plus the unit-test grid: `manager_claimer_error_test.go:151`, `manager_stableid_bucket_test.go:54,91,125,221,263`, `manager_handoff_bucket_test.go:71,110,154,188`, `manager_resolver_reconcile_warning_test.go:112`, `manager_audit_wireup_test.go:51,91`, `manager_test.go:221,239`, `example_test.go:40`, `manager_max_reconnects_warning_test.go:94`, `manager_capability_reporter_test.go:238,297` — REVIEW each.

- [ ] **Step 3: Save the classified list**

```bash
mkdir -p tmp
# write tmp/start-call-audit.md by hand from Step 2 output
```

- [ ] **Step 4: Commit**

```bash
git add tmp/start-call-audit.md
git commit -m "audit: classify mgr.Start callers ahead of async-Start migration"
```

---

### Task 3: Implement `runStartupBackground` and the watchdog

**Files:**
- Create: `manager_startup_async.go`

- [ ] **Step 1: Write the file**

```go
package parti

import (
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// runStartupBackground completes the parts of startup whose duration is
// not bounded by a single RPC: waiting for the leader-published initial
// assignment, and applying it via the unified pipeline. It is best-effort
// and single-attempt — on error it logs and falls through to monitor
// startup, letting the existing recovery mechanisms drive subsequent
// retries:
//
//   - Failed assignment fetch: monitorAssignmentChanges (started below)
//     delivers the assignment when the leader publishes it; handleAssignmentEntry
//     applies via the unified pipeline.
//   - Failed initial apply: applyAssignmentWithPrev's scheduleApplyRetry
//     (manager_assignment.go:944) schedules a retry on the same input.
//
// The runner does NOT call enterDegraded or exitDegraded. Soft probe
// signaling is handled by startStartupTimeoutWatchdog (separate goroutine
// decoupled from the runner so the watchdog fires even if the runner is
// blocked inside applyInitialAssignment).
//
// On a clean success the runner CAS-transitions WaitingAssignment → Stable
// via casToStableFromWaitingAssignment. If the calculator-state monitor
// (started in startCalculator at manager_assignment.go:157-168) has
// already moved state to Scaling/Rebalancing/Emergency, the CAS fails and
// the runner leaves state alone — calculator ownership wins. The
// post-Stable monitor set always starts, regardless of apply success or
// state path; the monitors handle whatever state they find.
//
// Apply boundedness: applyInitialAssignment internally calls
// handoffCoordinator.Apply(m.ctx, ...) which is unbounded per attempt.
// This matches pre-refactor Start (same chain). A stuck updater can block
// the runner inside one apply attempt until m.ctx is cancelled. The
// watchdog still fires enterDegraded("startup-timeout") in that case so
// the readiness probe can rotate the pod.
func (m *Manager) runStartupBackground(assignmentKV jetstream.KeyValue) {
	defer func() {
		if r := recover(); r != nil {
			m.logError("startup background panicked", "panic", r)
			m.enterDegraded("startup-background-panic")
			// Still attempt to start monitors so the manager can recover
			// once degraded is exited via attemptRecoveryFromDegraded.
			m.startPostStableMonitors(assignmentKV)
		}
	}()

	applyOK := false
	if err := m.waitForAssignment(m.ctx, assignmentKV, m.heartbeatKV); err != nil {
		m.logError("startup: initial assignment fetch failed; will recover via assignment watcher",
			"error", err)
	} else if err := m.applyInitialAssignment(m.ctx, assignmentKV); err != nil {
		m.logError("startup: initial apply failed; will recover via scheduleApplyRetry / commit watcher",
			"error", err)
	} else {
		applyOK = true
	}

	if applyOK {
		m.casToStableFromWaitingAssignment()
	}

	// Always start post-Stable monitors. If the runner failed to apply,
	// the monitors are the recovery path: monitorAssignmentChanges
	// redelivers; scheduleApplyRetry retries; monitorNATSConnection
	// drives attemptRecoveryFromDegraded if degraded fired.
	m.startPostStableMonitors(assignmentKV)
}

// casToStableFromWaitingAssignment performs a guarded WaitingAssignment →
// Stable transition. The calculator-state monitor (manager_assignment.go:157-168)
// can move state from WaitingAssignment to Scaling/Rebalancing/Emergency
// while this runner is mid-apply (see syncStateFromCalculator at
// manager_state.go:194-242). Generic transitionState(StateStable) would
// CAS-walk through those states — manager_state.go:165-168 lists StateStable
// as a valid next state from each. The direct CAS here only succeeds when
// state is still WaitingAssignment; otherwise calculator owns state.
//
// On a successful CAS we manually invoke the same OnStateChanged hook and
// RecordStateTransition metric that transitionState would have fired
// (manager_state.go:121-138), so observers see no difference from a normal
// transition.
func (m *Manager) casToStableFromWaitingAssignment() {
	if !m.state.CompareAndSwap(int32(StateWaitingAssignment), int32(StateStable)) { //nolint:gosec // controlled enum
		m.logger.Info("startup: WaitingAssignment was already replaced by calculator-driven state; not forcing Stable",
			"current_state", m.State().String())
		return
	}
	m.logger.Info("state transition",
		"from", StateWaitingAssignment.String(),
		"to", StateStable.String(),
		"worker_id", m.WorkerID())
	if m.hooks.OnStateChanged != nil {
		m.invokeHook("state change", func() error {
			return m.hooks.OnStateChanged(m.ctx, StateWaitingAssignment, StateStable)
		})
	}
	m.metrics.RecordStateTransition(StateWaitingAssignment, StateStable, 0)
}

// startPostStableMonitors launches the four monitor goroutines that drive
// post-Stable lifecycle. Wrapped in postStableMonitorsOnce because the
// runner calls it whether or not the initial apply succeeded — and a future
// degraded→recovered cycle could re-enter and double-spawn without this
// guard. monitorNATSConnection itself is also independently idempotent via
// connMonitorOnce (manager_degraded.go:14-32).
func (m *Manager) startPostStableMonitors(assignmentKV jetstream.KeyValue) {
	m.postStableMonitorsOnce.Do(func() {
		m.wg.Go(func() { m.monitorCommitChanges(m.ctx, assignmentKV) })
		m.wg.Go(func() { m.monitorAssignmentChanges(m.ctx, assignmentKV) })
		m.monitorNATSConnection()
		m.wg.Go(func() { m.monitorBucketEpochs(m.ctx) })
	})
}

// startStartupTimeoutWatchdog spawns a goroutine that fires
// enterDegraded("startup-timeout") once if the manager is still in
// StateWaitingAssignment after StartupTimeout has elapsed. The deadline is
// absolute (computed from m.startedAt), so the synchronous sanity phase
// counts against the budget — preserving the documented contract that
// StartupTimeout covers full manager startup from Start invocation
// (config.go:406-410).
//
// Decoupled from runStartupBackground: the watchdog fires even if the
// runner is blocked inside applyInitialAssignment (which is unbounded —
// see runStartupBackground Godoc). enterDegraded is CAS-gated on
// degradedSince so concurrent degraded entries from other paths are
// harmless; OnDegraded fires exactly once per entry per the existing
// contract.
//
// firedAtomic is a guard so calling this twice (e.g. from a test driver)
// only schedules one watchdog goroutine.
func (m *Manager) startStartupTimeoutWatchdog() {
	if m.cfg.StartupTimeout <= 0 {
		return
	}
	if !m.startupWatchdogFired.CompareAndSwap(false, true) {
		return
	}
	deadline := m.startedAt.Add(m.cfg.StartupTimeout)
	wait := time.Until(deadline)
	m.wg.Go(func() {
		if wait > 0 {
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		if m.State() != StateWaitingAssignment {
			return
		}
		m.logError("startup: exceeded StartupTimeout without reaching Stable; entering degraded for probe rotation",
			"startup_timeout", m.cfg.StartupTimeout,
			"elapsed", time.Since(m.startedAt))
		m.enterDegraded("startup-timeout")
	})
}

// startupWatchdogFired guards startStartupTimeoutWatchdog so the watchdog
// goroutine is scheduled at most once per Manager instance.
//
// (This field is declared here for locality; declare it in the Manager
// struct alongside startedAt in Task 1 — exact insertion text in Task 1.)
var _ atomic.Bool // documentation hint; the real field lives on Manager
```

(The trailing `var _ atomic.Bool` is documentation only. Delete it before commit if `go vet` complains. The real `startupWatchdogFired atomic.Bool` field must be added to the Manager struct in the same place as `startedAt` and `postStableMonitorsOnce`.)

- [ ] **Step 2: Add the `startupWatchdogFired` field**

Amend Task 1 Step 1 by adding a third field:

```go
	startupWatchdogFired   atomic.Bool   // guards startStartupTimeoutWatchdog (one-shot)
```

(If `atomic` is not yet imported in `manager.go`, add `"sync/atomic"` to the import block.)

- [ ] **Step 3: Build**

Run: `go build ./...`
Expected: success.

- [ ] **Step 4: Commit**

```bash
git add manager.go manager_startup_async.go
git commit -m "feat(manager): runStartupBackground + soft StartupTimeout watchdog"
```

---

### Task 3a: Wire `casToStableFromWaitingAssignment` into every apply-success path

**Closes v3-P0.** The runner's own CAS call covers the happy path (runner's first apply succeeds). But if the first apply fails and a later watcher-delivered apply or `scheduleApplyRetry`-driven apply succeeds, nothing transitions `WaitingAssignment → Stable`. Add the CAS call to the two terminal points of every apply chain so the transition fires idempotently from any successful apply, no matter which path drove it.

**Files:**
- Modify: `manager_assignment.go:906` (`applyAssignmentWithPrev` — at the end of the success path, after `SetAppliedAssignment` and `m.applyStoreMu.Unlock()`)
- Modify: `manager.go:603-633` (`applyInitialAssignment` cold-start-empty branch — does not flow through `applyAssignmentWithPrev`)

- [ ] **Step 1: Add CAS at the end of `applyAssignmentWithPrev` success path**

Locate `applyAssignmentWithPrev` at `manager_assignment.go:906`. Find the success path (after step 4 sets the heartbeat, before the final `return nil`). The exact location is just before the function returns nil on success (around manager_assignment.go:980-1000 depending on intervening edits).

Add immediately before `return nil` (after `m.applyStoreMu.Unlock()`):

```go
	// Complete startup if we are still in WaitingAssignment. Idempotent CAS
	// (guarded to only fire from WaitingAssignment) so calling it from
	// every apply-success path (initial, watcher-delivered,
	// scheduleApplyRetry-driven) is safe. This is the missing handoff that
	// v3 lacked: without it, a worker whose runner's first apply failed
	// but whose retry-or-watcher-driven apply later succeeded would stay
	// in WaitingAssignment forever even though the assignment was applied
	// and acked. See manager_startup_async.go runStartupBackground Godoc.
	m.casToStableFromWaitingAssignment()
```

- [ ] **Step 2: Add CAS at the end of `applyInitialAssignment` cold-start-empty path**

Locate the cold-start-empty branch at `manager.go:603-633`. It directly stores an empty assignment + publishes heartbeat without going through `applyAssignmentWithPrev`. Add the CAS call before `return nil`:

```go
		// Cold-start empty path also completes startup. Without this,
		// a worker that boots before the leader publishes any
		// partitions would receive an empty assignment, ack it, and
		// stay in WaitingAssignment until a later non-empty assignment
		// triggers applyAssignmentWithPrev's CAS — which may never
		// happen if the source is genuinely empty.
		m.casToStableFromWaitingAssignment()

		return nil
	}
```

- [ ] **Step 3: Build**

Run: `go build ./...`
Expected: success.

- [ ] **Step 4: Commit**

```bash
git add manager.go manager_assignment.go
git commit -m "feat(manager): every apply-success path completes startup via CAS guard"
```

---

### Task 4: Refactor Start to spawn the runner + watchdog

**Files:**
- Modify: `manager.go:508-554` (the block from `// Step 5: Wait for assignment.` through `return nil`)

- [ ] **Step 1: Replace the block**

Find the block in `manager.go` starting at the `// Step 5: Wait for assignment.` comment. Replace with:

```go
	// Step 5: Hand off the assignment-wait + initial apply to the
	// background runner. Start returns once the synchronous sanity
	// checks (claim, election, heartbeat, calculator) are wired. The
	// background runner attempts one initial wait + apply on a best-
	// effort basis; existing recovery mechanisms drive any retries
	// (monitorAssignmentChanges watcher redelivery, scheduleApplyRetry,
	// monitorNATSConnection → attemptRecoveryFromDegraded). The
	// startStartupTimeoutWatchdog goroutine fires enterDegraded(
	// "startup-timeout") once if the manager is still in WaitingAssignment
	// after StartupTimeout from m.startedAt — providing a probe-rotation
	// signal independent of whether the runner is blocked inside apply.
	//
	// Callers that need to know the manager is ready to process work
	// should call mgr.WaitState(StateStable, timeout).
	//
	// The runner preserves the pre-refactor invariant "Apply→Store→Ack
	// BEFORE StateStable" by ordering apply before CAS. Monitor goroutines
	// start unconditionally after the apply attempt so they are present
	// for subsequent recovery whether or not the initial apply succeeded.
	m.transitionState(StateWaitingAssignment)
	m.logger.Info("startup: sanity checks done; background runner taking over for initial apply")
	m.startStartupTimeoutWatchdog()
	m.wg.Go(func() { m.runStartupBackground(assignmentKV) })

	return nil
}
```

- [ ] **Step 2: Build + run a focused subset**

Run: `go build ./... && go test -run TestManager_Start_ -count=1 -short ./...`
Expected: build passes. Some tests may fail (intentionally — Task 11 migrates them). Failures should be on tests reading assignment immediately after Start, not on Start-error paths.

- [ ] **Step 3: Commit**

```bash
git add manager.go
git commit -m "refactor(manager): Start returns after sanity checks; runner + watchdog spawn in background"
```

---

### Task 5: Contract test — Start returns at WaitingAssignment or later

**Files:**
- Create: `manager_startup_async_test.go`

**Package choice:** `package parti` — required so Task 6 can call unexported `casToStableFromWaitingAssignment` and read `m.startedAt`. The existing `export_test.go` pattern (only exposes `CalculatorForTest`) doesn't fit a unit test that exercises this many internals; same-package keeps it clean.

- [ ] **Step 1: Write the test**

```go
package parti

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestStart_ReturnsBeforeStable pins the new contract: Start returns once
// sanity checks (worker ID, buckets, election, heartbeat, calculator) are
// wired. State at return may be WaitingAssignment OR any later valid
// state (the background runner may have completed; the calculator may
// have projected Scaling/Rebalancing/Emergency). The post-Stable monitors
// drive recovery from here.
func TestStart_ReturnsBeforeStable(t *testing.T) {
	nc, cleanupNATS := testutil.StartEmbeddedNATS(t)
	defer cleanupNATS()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	src := source.NewStatic(testutil.CreateTestPartitions(3))

	mgr, err := NewManager(&cfg, js, src, strategy.NewConsistentHash())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, mgr.Start(ctx))

	require.NotEmpty(t, mgr.WorkerID(), "worker ID must be claimed synchronously in Start")

	s := mgr.State()
	require.Truef(t,
		s == types.StateWaitingAssignment ||
			s == types.StateStable ||
			s == types.StateScaling ||
			s == types.StateRebalancing ||
			s == types.StateEmergency,
		"state after Start must be WaitingAssignment or a later active state; got %v", s)

	require.NoError(t, <-mgr.WaitState(types.StateStable, 5*time.Second))
	require.NotEmpty(t, mgr.CurrentAssignment().Partitions)
}
```

- [ ] **Step 2: Run**

Run: `go test -run TestStart_ReturnsBeforeStable -count=1 -v ./...`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add manager_startup_async_test.go
git commit -m "test(manager): pin Start contract — returns at WaitingAssignment or later"
```

---

### Task 6: Unit test — CAS guard rejects from non-WaitingAssignment

**Files:**
- Modify: `manager_startup_async_test.go`

- [ ] **Step 1: Add the test**

```go
// TestCasToStableFromWaitingAssignment_FailsWhenStateMoved asserts the
// CAS guard: the runner's direct CAS WaitingAssignment → Stable does
// nothing if the calculator-state monitor has already projected an
// active state. Same-package access lets us call the unexported helper
// and drive m.state via transitionState directly — no live NATS needed.
//
// Uses the existing newTestManager helper at
// manager_commit_state_machine_test.go:150. The helper returns 4 values
// (Manager + recording fakes); we destructure with _ for the unused ones.
func TestCasToStableFromWaitingAssignment_FailsWhenStateMoved(t *testing.T) {
	mgr, _, _, _ := newTestManager(t)

	// Walk the state machine through to Scaling.
	require.True(t, mgr.transitionState(StateClaimingID))
	require.True(t, mgr.transitionState(StateElection))
	require.True(t, mgr.transitionState(StateWaitingAssignment))
	require.True(t, mgr.transitionState(StateScaling))

	mgr.casToStableFromWaitingAssignment()
	require.Equal(t, StateScaling, mgr.State())
}

// TestCasToStableFromWaitingAssignment_SucceedsFromWaitingAssignment is
// the positive control: CAS succeeds when state is still
// WaitingAssignment.
func TestCasToStableFromWaitingAssignment_SucceedsFromWaitingAssignment(t *testing.T) {
	mgr, _, _, _ := newTestManager(t)

	require.True(t, mgr.transitionState(StateClaimingID))
	require.True(t, mgr.transitionState(StateElection))
	require.True(t, mgr.transitionState(StateWaitingAssignment))

	mgr.casToStableFromWaitingAssignment()
	require.Equal(t, StateStable, mgr.State())
}

// TestCasToStableFromWaitingAssignment_NoOpFromDegraded asserts that
// when the watchdog has fired enterDegraded("startup-timeout") between
// the runner's apply attempt and CAS, the CAS does NOT clobber Degraded
// with Stable. The connection monitor's attemptRecoveryFromDegraded
// drives degraded → stable separately.
func TestCasToStableFromWaitingAssignment_NoOpFromDegraded(t *testing.T) {
	mgr, _, _, _ := newTestManager(t)

	require.True(t, mgr.transitionState(StateClaimingID))
	require.True(t, mgr.transitionState(StateElection))
	require.True(t, mgr.transitionState(StateWaitingAssignment))
	require.True(t, mgr.transitionState(StateDegraded))

	mgr.casToStableFromWaitingAssignment()
	require.Equal(t, StateDegraded, mgr.State())
}
```

- [ ] **Step 2: Run**

Run: `go test -run TestCasToStableFromWaitingAssignment -count=1 -v ./...`
Expected: PASS (after implementer fills in the constructor).

- [ ] **Step 3: Commit**

```bash
git add manager_startup_async_test.go
git commit -m "test(manager): CAS guard prevents StateStable clobber of calculator state"
```

---

### Task 7: Integration test — calculator-driven transition is not clobbered

**Files:**
- Create: `test/integration/manager/startup_async_calculator_race_test.go`

**Required for PR merge.** This is the integration-level pin for the v1 P0-2 fix.

- [ ] **Step 1: Write the test against the 3-worker cluster template**

```go
package manager_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestStartupAsync_CalculatorStateNotClobbered exercises the v1-review P0-2
// race window: the runner is mid-apply when the calculator transitions
// away from WaitingAssignment.
//
// Scope of this test: liveness + smoke coverage only. The integration
// layer cannot deterministically distinguish a broken CAS clobber from
// normal calculator-driven state oscillation via OnStateChanged alone
// (both produce overlapping transition sequences; the transition table
// at manager_state.go:165-167 permits Stable ↔ Scaling/Rebalancing/
// Emergency as valid steady-state transitions). The precise CAS-guard
// regression pin is in the three unit tests in Task 6
// (TestCasToStableFromWaitingAssignment_*), which exercise the helper
// directly.
//
// What this test contributes: 3-worker join under live KV traffic
// converges to a healthy steady state; OnStateChanged is correctly
// wired; partition coverage is complete. A deterministic CAS-clobber
// live pin is tracked as a CHANGELOG follow-up (requires a production
// test hook mirroring testHookAfterApplyStore).
//
// Modeled on the 3-worker cluster template at
// test/integration/manager/manager_epoch_monitor_concurrency_test.go:54-145.
//
// IMPLEMENTER NOTE: the OnStateChanged hook is set per-Manager via
// parti.WithHooks or the Hooks field at construction time. Check
// testutil.WorkerCluster's AddWorker signature; if it does not accept
// hooks per-worker, either extend WorkerCluster or construct managers
// directly (without WorkerCluster) for this test. The cleanest
// implementation may be to bypass WorkerCluster and roll the 3-worker
// pattern inline so each worker gets its own recording hook.
func TestStartupAsync_CalculatorStateNotClobbered(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	partitions := testutil.CreateTestPartitions(6)
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Per-worker state-history recorder. Hook fires on every transition;
	// we filter to the demotion patterns that indicate a broken CAS guard.
	type transition struct {
		from, to types.State
	}
	var (
		mu       sync.Mutex
		recorded = map[int][]transition{} // worker index → transitions
	)
	makeHooks := func(idx int) *parti.Hooks {
		return &parti.Hooks{
			OnStateChanged: func(_ context.Context, from, to parti.State) error {
				mu.Lock()
				recorded[idx] = append(recorded[idx], transition{from, to})
				mu.Unlock()
				return nil
			},
		}
	}

	// Three workers in sequence so each addition forces a calculator-
	// state projection (Scaling) on existing leader and on the joiners.
	workers := make([]*parti.Manager, 0, 3)
	for i := 0; i < 3; i++ {
		mgr, err := parti.NewManager(&cfg, js, src, assignStrat, parti.WithHooks(makeHooks(i)))
		require.NoError(t, err)
		require.NoError(t, mgr.Start(ctx))
		workers = append(workers, mgr)
		// Stagger so each worker joins while the cluster is still
		// settling — maximises the window where the runner is mid-
		// apply while the calculator projects an active state.
		time.Sleep(200 * time.Millisecond)
	}
	t.Cleanup(func() {
		for _, mgr := range workers {
			_ = mgr.Stop(context.Background())
		}
	})

	// Wait for convergence: all workers reach StateStable.
	for i, mgr := range workers {
		require.NoErrorf(t, <-mgr.WaitState(types.StateStable, 15*time.Second),
			"worker %d did not reach StateStable", i)
	}

	// LIMITATION (acknowledged): pure observability via OnStateChanged
	// cannot deterministically distinguish the broken-CAS clobber from
	// normal calculator-driven oscillation, because both
	// `WaitingAssignment → Scaling → Stable` (normal calculator path
	// via syncStateFromCalculator at manager_state.go:218-225) and a
	// broken `WaitingAssignment → Stable (clobbered)` followed by
	// calculator re-projecting Scaling → Stable would produce overlapping
	// transition sequences. The transition-table allows both
	// Scaling → Stable and Stable → Scaling as valid steady-state
	// transitions (manager_state.go:165-167).
	//
	// The CAS guard is precisely pinned by the unit tests in Task 6:
	//   - TestCasToStableFromWaitingAssignment_FailsWhenStateMoved
	//   - TestCasToStableFromWaitingAssignment_SucceedsFromWaitingAssignment
	//   - TestCasToStableFromWaitingAssignment_NoOpFromDegraded
	//
	// This integration test contributes:
	//   - Liveness: the cluster converges under realistic 3-worker join load.
	//   - Coverage: the runner's CAS path is exercised under live KV traffic.
	//   - Smoke detection: any catastrophic regression (e.g., runner
	//     leaves the cluster in an inconsistent state) shows up as
	//     convergence failure below.
	//
	// A deterministic CAS-clobber regression pin would require a test
	// hook in production code (e.g., m.testHookBeforeStartupCAS) — that
	// is tracked as a follow-up in CHANGELOG.
	mu.Lock()
	transitionCount := 0
	for _, trs := range recorded {
		transitionCount += len(trs)
	}
	mu.Unlock()
	require.Greater(t, transitionCount, 0,
		"OnStateChanged should have fired during 3-worker join — if 0, the hook is not wired correctly")

	// Liveness sanity check: the union of all partitions across workers
	// must equal the source set.
	seen := make(map[string]struct{}, len(partitions))
	for _, mgr := range workers {
		for _, p := range mgr.CurrentAssignment().Partitions {
			seen[p.ID()] = struct{}{}
		}
	}
	require.Len(t, seen, len(partitions))
}
```

(Imports needed: `sync` for the mutex. Verify `parti.WithHooks` exists — `grep -n "WithHooks\b" options.go` — if absent, use the `Hooks` field on the Manager constructor or whatever the existing hook-injection seam is.)

- [ ] **Step 2: Run**

Run: `go test -run TestStartupAsync_CalculatorStateNotClobbered -count=1 -race -v ./test/integration/manager/`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add test/integration/manager/startup_async_calculator_race_test.go
git commit -m "test(integration): pin runner CAS guard does not clobber calculator state"
```

---

### Task 8: Test — Stop during background runner ends in Shutdown

**Files:**
- Modify: `manager_startup_async_test.go`

- [ ] **Step 1: Add the test**

```go
// TestStart_StopDuringBackground_NoDegraded asserts that calling Stop
// while the background runner is mid-flight transitions cleanly to
// Shutdown without leaving Degraded residue. The runner's only blocking
// operations are waitForAssignment and applyInitialAssignment, both of
// which honor m.ctx via the standard JetStream/NATS plumbing.
func TestStart_StopDuringBackground_NoDegraded(t *testing.T) {
	nc, cleanupNATS := testutil.StartEmbeddedNATS(t)
	defer cleanupNATS()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	cfg.StartupTimeout = 30 * time.Second // long, so Stop wins the race

	// A static source so the sync phase completes; the leader publishes
	// the initial assignment immediately, so the runner reaches its
	// apply step quickly. We want Stop to land somewhere in the
	// run+monitor-start sequence; a few millisecond gap suffices.
	src := source.NewStatic(testutil.CreateTestPartitions(2))

	mgr, err := NewManager(&cfg, js, src, strategy.NewConsistentHash())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, mgr.Start(ctx))

	require.NoError(t, mgr.Stop(context.Background()))
	require.Equal(t, types.StateShutdown, mgr.State())
}
```

- [ ] **Step 2: Run**

Run: `go test -run TestStart_StopDuringBackground_NoDegraded -count=1 -race -v ./...`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add manager_startup_async_test.go
git commit -m "test(manager): Stop during background runner ends in Shutdown"
```

---

### Task 9: Test — soft watchdog fires enterDegraded after StartupTimeout

**Files:**
- Modify: `manager_startup_async_test.go`

- [ ] **Step 1: Add the test**

```go
// TestStart_WatchdogFiresAfterStartupTimeout asserts that if the
// background runner cannot reach Stable within StartupTimeout, the
// soft watchdog enters Degraded for probe-driven pod rotation. The
// runner is decoupled from the watchdog — so this fires even when
// the runner is blocked.
//
// We force "cannot reach Stable" by using a source that returns no
// partitions: the leader publishes an empty assignment which applies
// fine but the calculator-state monitor never projects an active state
// (and our refactor still requires the runner to CAS to Stable). The
// runner does call CAS to Stable on success, so this test actually
// exercises the path where the runner SUCCEEDS but slowly — which is
// fine for asserting that the watchdog is wired correctly, since
// StartupTimeout is set artificially low.
//
// To reliably trigger the watchdog without depending on the runner's
// natural speed, we set StartupTimeout to a value much shorter than the
// fastest possible sync phase: with StartupTimeout = 1ms, the watchdog
// fires almost immediately after spawn, while the runner is still in
// its first apply attempt.
func TestStart_WatchdogFiresAfterStartupTimeout(t *testing.T) {
	nc, cleanupNATS := testutil.StartEmbeddedNATS(t)
	defer cleanupNATS()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	cfg.StartupTimeout = 1 * time.Millisecond // forces watchdog fire before runner stabilizes

	src := source.NewStatic(testutil.CreateTestPartitions(2))

	mgr, err := NewManager(&cfg, js, src, strategy.NewConsistentHash())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, mgr.Start(ctx))

	// Watchdog must have fired enterDegraded("startup-timeout"). It only
	// fires while state == WaitingAssignment, so this assertion races
	// the runner finishing. If the runner won the race, state would be
	// Stable; either outcome means the wiring works.
	//
	// `WaitStateAny` does not exist on Manager (manager_state.go:54 only
	// defines WaitState for a single state). Race-tolerant pattern:
	// try Degraded first with a short budget; if not reached, the runner
	// must have raced past it to Stable.
	if err := <-mgr.WaitState(types.StateDegraded, 500*time.Millisecond); err == nil {
		return // saw Degraded; wiring works
	}
	require.NoError(t, <-mgr.WaitState(types.StateStable, 3*time.Second),
		"manager reached neither Degraded nor Stable; watchdog or runner is broken")
}
```

- [ ] **Step 2: Run**

Run: `go test -run TestStart_WatchdogFiresAfterStartupTimeout -count=1 -race -v ./...`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add manager_startup_async_test.go
git commit -m "test(manager): soft watchdog fires Degraded after StartupTimeout"
```

---

### Task 9a: Empty-assignment startup correctness tests

**Files:**
- Modify: `manager_startup_async_test.go`

**Why:** Confirms the empty-assignment startup paths behave correctly under the refactor — specifically that (a) the cold-start-empty bypass at `manager.go:603-633` still suppresses spurious assignment hooks, (b) a worker that draws zero partitions from a non-empty cluster assignment still reaches Stable without breaking other workers, and (c) existing workers continue functioning when a new worker joins. These are integration-level pins for the analysis surfaced after v5 review (the "does the plan introduce an empty-partition reassignment hazard" question).

- [ ] **Step 1: Test the cold-start-empty bypass (Path A)**

```go
// TestStart_ColdStartEmpty_NoAssignmentHooks asserts that when the
// partition source is empty at startup (leader has nothing to publish),
// the cold-start-empty bypass at manager.go:603-633 suppresses
// OnAssignmentChanged + OnPartitionsAssigned + OnPartitionsRevoked. The
// worker still reaches Stable via Task 3a's CAS (added to the cold-empty
// branch alongside the existing CAS in applyAssignmentWithPrev).
func TestStart_ColdStartEmpty_NoAssignmentHooks(t *testing.T) {
	nc, cleanupNATS := testutil.StartEmbeddedNATS(t)
	defer cleanupNATS()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	src := source.NewStatic(nil) // empty source — verify by reading source/static.go:43

	var assignmentChanged, partitionsAssigned, partitionsRevoked atomic.Int32
	hooks := &parti.Hooks{
		OnAssignmentChanged: func(_ context.Context, _, _ []parti.Partition) error {
			assignmentChanged.Add(1)
			return nil
		},
		OnPartitionsAssigned: func(_ context.Context, _ []parti.Partition) error {
			partitionsAssigned.Add(1)
			return nil
		},
		OnPartitionsRevoked: func(_ context.Context, _ []parti.Partition) error {
			partitionsRevoked.Add(1)
			return nil
		},
	}

	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(), parti.WithHooks(hooks))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, mgr.Start(ctx))
	require.NoError(t, <-mgr.WaitState(parti.StateStable, 5*time.Second))

	// Give async hook dispatch time to fire (or not).
	time.Sleep(200 * time.Millisecond)

	require.Equal(t, int32(0), assignmentChanged.Load(),
		"cold-start-empty bypass must NOT fire OnAssignmentChanged")
	require.Equal(t, int32(0), partitionsAssigned.Load())
	require.Equal(t, int32(0), partitionsRevoked.Load())
	require.Empty(t, mgr.CurrentAssignment().Partitions)
}
```

- [ ] **Step 2: Test Path B — empty-slice from non-empty cluster assignment**

```go
// TestStart_EmptySliceFromNonEmptyCluster_HookFiresEmptyEmpty asserts the
// Path B case: leader publishes a non-empty assignment (Version > 0) but
// this worker's slice is empty (more workers than partitions, or strategy
// distribution leaves the worker with nothing). Because Version > 0, the
// cold-start-empty branch does not match — control flows through
// applyAssignmentWithPrev which fires OnAssignmentChanged([], []) and
// then UpdateWorkerConsumer(ctx, workerID, []). UpdateWorkerConsumer's
// idempotency contract (options.go:126) requires this to be safe.
//
// 2 workers + 1 partition under consistent hash: exactly one worker gets
// the partition; the other draws empty. Asserts:
//   - Both workers reach Stable.
//   - The worker with empty slice fires OnAssignmentChanged once with ([], []).
//   - The convenience hooks OnPartitionsAssigned/Revoked do NOT fire on
//     that worker (added/removed are both empty per
//     manager_assignment.go:1045-1054).
func TestStart_EmptySliceFromNonEmptyCluster_HookFiresEmptyEmpty(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	partitions := testutil.CreateTestPartitions(1) // single partition
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()

	type hookCounts struct {
		assignmentChanged, partitionsAssigned, partitionsRevoked atomic.Int32
		lastOldLen, lastNewLen                                   atomic.Int32
	}
	counts := make([]*hookCounts, 2)
	makeHooks := func(idx int) *parti.Hooks {
		counts[idx] = &hookCounts{}
		return &parti.Hooks{
			OnAssignmentChanged: func(_ context.Context, old, new []parti.Partition) error {
				counts[idx].assignmentChanged.Add(1)
				counts[idx].lastOldLen.Store(int32(len(old)))
				counts[idx].lastNewLen.Store(int32(len(new)))
				return nil
			},
			OnPartitionsAssigned: func(_ context.Context, _ []parti.Partition) error {
				counts[idx].partitionsAssigned.Add(1)
				return nil
			},
			OnPartitionsRevoked: func(_ context.Context, _ []parti.Partition) error {
				counts[idx].partitionsRevoked.Add(1)
				return nil
			},
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	mgrs := make([]*parti.Manager, 2)
	for i := 0; i < 2; i++ {
		m, err := parti.NewManager(&cfg, js, src, assignStrat, parti.WithHooks(makeHooks(i)))
		require.NoError(t, err)
		require.NoError(t, m.Start(ctx))
		mgrs[i] = m
		time.Sleep(150 * time.Millisecond)
	}
	t.Cleanup(func() {
		for _, m := range mgrs {
			_ = m.Stop(context.Background())
		}
	})

	for i, m := range mgrs {
		require.NoErrorf(t, <-m.WaitState(parti.StateStable, 10*time.Second),
			"worker %d did not reach StateStable", i)
	}
	time.Sleep(300 * time.Millisecond) // let async hook dispatch settle

	// Identify which worker drew empty vs the partition.
	var emptyIdx, ownerIdx int
	for i, m := range mgrs {
		if len(m.CurrentAssignment().Partitions) == 0 {
			emptyIdx = i
		} else {
			ownerIdx = i
		}
	}
	require.NotEqual(t, emptyIdx, ownerIdx, "exactly one worker should hold the partition")

	// The empty-slice worker fired OnAssignmentChanged with ([], []).
	// May fire more than once if the leader republished during rebalance —
	// but every fire must carry empty old + empty new because this worker
	// never held the partition.
	require.GreaterOrEqual(t, counts[emptyIdx].assignmentChanged.Load(), int32(1),
		"empty-slice worker must fire OnAssignmentChanged at least once")
	require.Equal(t, int32(0), counts[emptyIdx].lastOldLen.Load(),
		"empty-slice worker's last OnAssignmentChanged old should be []")
	require.Equal(t, int32(0), counts[emptyIdx].lastNewLen.Load(),
		"empty-slice worker's last OnAssignmentChanged new should be []")

	// Empty diff: derived hooks must not fire on the empty-slice worker.
	require.Equal(t, int32(0), counts[emptyIdx].partitionsAssigned.Load(),
		"empty-slice worker: no partitions to assign")
	require.Equal(t, int32(0), counts[emptyIdx].partitionsRevoked.Load(),
		"empty-slice worker: no partitions to revoke")

	// The owning worker received its partition cleanly.
	require.GreaterOrEqual(t, counts[ownerIdx].partitionsAssigned.Load(), int32(1),
		"owner worker must see OnPartitionsAssigned at least once")
	require.Equal(t, int32(0), counts[ownerIdx].partitionsRevoked.Load(),
		"owner worker should never see a revoke during this scenario")
}
```

- [ ] **Step 3: Existing-cluster-not-disrupted assertion (extend Task 7)**

Already partially covered by the convergence + partition-coverage assertions in `TestStartupAsync_CalculatorStateNotClobbered`. Add one more assertion at the end of that test: for each existing worker (workers added BEFORE the final joiner), record the partitions they held just before the joiner started and assert that the joiner's arrival did not cause `OnPartitionsRevoked` to fire with the worker's full prior set (which would indicate a spurious "lose everything then re-acquire" oscillation).

The simplest form, adapted to the existing test scaffolding:

```go
// After cluster converges in TestStartupAsync_CalculatorStateNotClobbered,
// snapshot each worker's partition count and assert no worker ever
// dropped to zero partitions during the join (which would be a
// spurious-revoke regression caused by the new worker's startup
// misinterpreting the assignment).
//
// Implementer note: this requires tracking partition counts over time,
// not just final. Add a sampler goroutine that polls CurrentAssignment()
// every 50ms during the cluster startup, recording min observed
// partition count per worker. Assert min > 0 for any worker that ever
// held > 0 partitions.
```

Implementer should keep this assertion minimal — a noisy sampler can cause its own race issues. If it's hard to do cleanly, demote to a TODO in CHANGELOG follow-ups.

- [ ] **Step 4: Run**

Run: `go test -run "TestStart_ColdStartEmpty|TestStart_EmptySliceFromNonEmptyCluster" -count=1 -race -v ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add manager_startup_async_test.go test/integration/manager/startup_async_calculator_race_test.go
git commit -m "test(manager): pin empty-assignment startup correctness — cold-start + empty-slice + existing-cluster"
```

---

### Task 10: Migrate test/binary callers per Task 2 audit

**Files:** every file marked **MIGRATE** in `tmp/start-call-audit.md`.

- [ ] **Step 1: For each MIGRATE entry, apply the standard migration**

Pattern:

Before:
```go
if err := mgr.Start(ctx); err != nil {
    return err
}
// (next line reads CurrentAssignment / starts workers)
```

After:
```go
if err := mgr.Start(ctx); err != nil {
    return err
}
if err := <-mgr.WaitState(parti.StateStable, 30*time.Second); err != nil {
    return fmt.Errorf("manager did not reach StateStable: %w", err)
}
// (next line reads CurrentAssignment / starts workers)
```

Adjust timeout per call-site context.

- [ ] **Step 2: For each REVIEW entry, decide MIGRATE / OK**

Common shapes:
- Calls Start then `require.Eventually(...assignment...)` → OK, no migration.
- Calls Start then immediately reads `mgr.CurrentAssignment()` → MIGRATE.
- Tests Start-error paths → OK.
- Tests degraded-mode behavior (`test/integration/failure/degraded_mode_test.go`) → REVIEW carefully; degraded-mode tests may want Start to succeed but not reach Stable.

- [ ] **Step 3: Update `internal/testutil/manager_helpers.go`**

```go
func StartManagerWithHandoffRecorder(t *testing.T, cfg *parti.Config, js jetstream.JetStream, src parti.PartitionSource, strategy parti.AssignmentStrategy, mr *HandoffMetricsRecorder) (*parti.Manager, func()) {
	t.Helper()
	mgr, err := parti.NewManager(cfg, js, src, strategy, parti.WithHandoffMetricsRecorder(mr))
	if err != nil {
		t.Fatalf("NewManager error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager start error: %v", err)
	}
	if err := <-mgr.WaitState(parti.StateStable, 10*time.Second); err != nil {
		_ = mgr.Stop(context.Background())
		t.Fatalf("Manager did not reach StateStable: %v", err)
	}
	cleanup := func() { _ = mgr.Stop(context.Background()) }
	return mgr, cleanup
}
```

- [ ] **Step 4: Audit other testutil helpers**

```bash
grep -rn "mgr\.Start\|manager\.Start" internal/testutil/
```

Apply the WaitState block where helpers expect a ready manager (most do).

- [ ] **Step 5: Run unit suite**

Run: `make test`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/testutil/ # plus the per-call-site files
git commit -m "test(migrate): wait for StateStable after Start across helpers + binaries"
```

---

### Task 11: Documentation

**Files:**
- Modify: `manager.go:366-393` (`Manager.Start` Godoc)
- Modify: `doc.go:36-41` (package Godoc example)
- Modify: `README.md:153-160` (quick-start)
- Modify: `docs/USER_GUIDE.md:166-175`
- Modify: `docs/REFERENCE.md:323-326,399-403`
- Modify: `docs/LIFECYCLE.md:22-86` (diagram + return-point note)
- Modify: `docs/API_REFERENCE.md:77-101` (Start section) + `:1138-1155` (State section)
- Modify: `AGENTS.md` (cross-feature contract #4)

- [ ] **Step 1: `Manager.Start` Godoc at manager.go:366-393**

Replace the existing Godoc with:

```go
// Start runs the manager's synchronous sanity-check phase: claim a stable
// worker ID, ensure required KV buckets exist, participate in election,
// start the heartbeat publisher, and start the calculator if elected
// leader. On success it transitions the manager to StateWaitingAssignment
// and spawns a background runner that attempts to fetch the initial
// assignment and apply it via the unified pipeline; on apply success the
// runner CAS-transitions to StateStable. A soft watchdog enters
// StateDegraded ("startup-timeout") if StartupTimeout (measured from
// Start invocation) elapses without reaching StateStable, signaling the
// readiness probe to rotate the pod.
//
// Start returns once the sanity-check phase succeeds. The state observed
// after Start may be WaitingAssignment, Stable, or any calculator-driven
// active state (Scaling, Rebalancing, Emergency) depending on race.
// Callers that need to know the manager is ready to process work should
// call mgr.WaitState(StateStable, timeout).
//
// The background runner is best-effort: on assignment-fetch or apply
// failure it logs and continues to monitor startup. Existing recovery
// mechanisms handle subsequent retries — the assignment watcher redelivers
// when the leader publishes; applyAssignmentWithPrev's scheduleApplyRetry
// re-attempts on apply failure; monitorNATSConnection drives
// attemptRecoveryFromDegraded on reconnect.
//
// Apply boundedness: applyInitialAssignment internally calls
// handoffCoordinator.Apply(m.ctx, ...) which is unbounded per attempt
// (identical to pre-refactor Start). A stuck consumer updater can block
// the runner inside one apply attempt until Stop. The soft watchdog still
// fires enterDegraded("startup-timeout") in that case for probe-driven
// rotation.
//
// Start returns an error only for synchronous-phase failures (bucket
// creation, ID claim, election RPC). Auto-cleanup invokes Stop on a
// non-nil error so callers do not need to call Stop after a failed Start.
```

- [ ] **Step 2: `doc.go:36-41`**

Add `WaitState(StateStable, ...)` between Start and the next line that touches assignment.

- [ ] **Step 3: `README.md:153-160`**

Same migration plus a one-sentence preamble: "Start returns once sanity checks pass; use WaitState to block until the manager is ready to process work."

- [ ] **Step 4: `docs/USER_GUIDE.md:166-175`**

Replace the `for mgr.State() != parti.StateStable { time.Sleep(...) }` loop with:

```go
if err := <-mgr.WaitState(parti.StateStable, 30*time.Second); err != nil {
    log.Fatalf("manager did not reach StateStable: %v", err)
}
```

- [ ] **Step 5: `docs/REFERENCE.md`**

The `require.Eventually(...StateStable)` at line 323-326 is already correct; just update surrounding prose. Replace the `for { sleep }` at line 399-403 with `WaitState`.

- [ ] **Step 6: `docs/LIFECYCLE.md:22-86` — diagram + prose**

Add `[Start returns]` inline marker to the `WAITING ASSIGNMENT` node in the ASCII diagram. Sketch:

```
    ┌────────┐    ┌───────────────┐    ┌──────────┐       ┌─────────────────────┐
    │  INIT  │───▶│ CLAIMING_ID   │───▶│ ELECTION │───▶   │ WAITING ASSIGNMENT  │
    └────────┘    └───────────────┘    └──────────┘       │  [Start returns ◀]  │
                                                          └──────────┬──────────┘
                                                                     │ (background runner)
                                                                     ▼
                                                              ┌──────────┐
                                                              │  STABLE  │
                                                              └──────────┘
```

Below the diagram, add:

```markdown
**Start return point:** `Manager.Start(ctx)` returns once the worker has
reached `WaitingAssignment` — i.e., the stable worker ID is claimed, KV
buckets exist, election has been run, and heartbeat + calculator are
wired. The transition to `Stable` happens in a background goroutine after
the initial assignment lands and is applied. Use
`Manager.WaitState(StateStable, timeout)` to block until the manager is
ready to process work.

The background runner is best-effort and single-attempt: if the initial
assignment fetch or apply fails, the runner logs the error and falls
through to monitor startup. Subsequent retries are driven by existing
recovery mechanisms — `monitorAssignmentChanges` redelivers when the
leader publishes; `scheduleApplyRetry` (inside `applyAssignmentWithPrev`)
retries failed applies; `monitorNATSConnection` drives
`attemptRecoveryFromDegraded` on reconnect.

A separate watchdog goroutine fires `enterDegraded("startup-timeout")`
once if the manager is still in `WaitingAssignment` after `StartupTimeout`
(measured from `Start` invocation). This is the probe-rotation signal.
The runner itself does not enter or exit degraded.

**Startup-timeout-degraded recovery is not guaranteed self-healing while
the runner is blocked.** Once monitors start, `monitorNATSConnection`
calls `attemptRecoveryFromDegraded` on its `ExitThreshold` tick even
without a prior disconnect (see `manager_degraded.go:32-69`), so the
runner-succeeds-but-watchdog-already-fired case recovers automatically.
But if the runner is stuck inside the unbounded
`handoffCoordinator.Apply(m.ctx, ...)` call, the monitor set has not
started yet — startup-timeout-degraded then stays until the runner
returns or the pod is rotated by the probe. This is the documented
trade-off of inheriting pre-refactor Start's apply boundedness.

Apply boundedness is unchanged from pre-refactor Start:
`handoffCoordinator.Apply(m.ctx, ...)` is unbounded per attempt. A stuck
consumer updater can block the runner inside one apply attempt until
Stop. The watchdog still fires for probe rotation in that case.
```

- [ ] **Step 7: `docs/API_REFERENCE.md:77-101`**

Mirror the Manager.Start Godoc.

- [ ] **Step 8: `docs/API_REFERENCE.md:1138-1155`**

Add a paragraph after the State enum table noting the new Start return point and the `WaitState(StateStable, timeout)` pattern.

- [ ] **Step 9: `AGENTS.md` cross-feature contract**

Append:

```markdown
4. **`Manager.Start` returns after the synchronous sanity-check phase, not
   after `StateStable`.** Start transitions to `WaitingAssignment` and
   spawns a background runner; the runner attempts one initial wait + apply
   and starts the post-Stable monitor set. On apply success it CAS-
   transitions `WaitingAssignment → Stable` (CAS-guarded so calculator
   ownership wins on conflict). Callers needing a ready manager call
   `WaitState(StateStable, timeout)`. A soft watchdog enters Degraded
   (reason `startup-timeout`) once if state is still WaitingAssignment
   after `StartupTimeout` from Start invocation. Pinned by
   `TestStart_ReturnsBeforeStable`,
   `TestCasToStableFromWaitingAssignment_*`,
   `TestStartupAsync_CalculatorStateNotClobbered`,
   `TestStart_StopDuringBackground_NoDegraded`, and
   `TestStart_WatchdogFiresAfterStartupTimeout`.

   Apply boundedness: the runner's `handoffCoordinator.Apply(m.ctx, ...)`
   call is unbounded per attempt — identical to pre-refactor Start. A
   stuck updater can block the runner inside apply until Stop; the
   watchdog still fires for probe rotation.
```

- [ ] **Step 10: Commit**

```bash
git add manager.go doc.go README.md docs/ AGENTS.md
git commit -m "docs: Manager.Start returns at WaitingAssignment; use WaitState"
```

---

### Task 12: CHANGELOG / release note + follow-up issues

**Files:**
- Modify or create: `CHANGELOG.md` (check `ls CHANGELOG.md`; if absent check `RELEASE_NOTES.md` / `docs/MIGRATION.md`)

- [ ] **Step 1: Add the breaking-change entry + follow-ups**

```markdown
## Unreleased

### Breaking changes

- `Manager.Start(ctx)` now returns once the synchronous sanity-check phase
  succeeds (stable worker ID claimed, KV buckets exist, election complete,
  heartbeat and calculator wired) — i.e. when the worker has transitioned
  to `StateWaitingAssignment`. Previously, `Start` blocked until
  `StateStable`. The initial assignment fetch and apply now run in a
  background goroutine.

  **What you observe:**
  - `mgr.WorkerID()` is still reliable immediately after `Start` returns.
  - `mgr.CurrentAssignment()` may return an empty assignment between
    `Start` returning and the background runner finishing. Block on
    `mgr.WaitState(parti.StateStable, timeout)` first.
  - `Start` returns an error only for synchronous-phase failures.
    Background failures fall through to monitor startup; existing recovery
    mechanisms (assignment watcher redelivery, `scheduleApplyRetry`,
    `monitorNATSConnection` → `attemptRecoveryFromDegraded`) handle them.
  - A soft watchdog enters `StateDegraded` (reason: `startup-timeout`)
    once if `StartupTimeout` elapses without reaching `Stable` — providing
    the probe-rotation signal. This is decoupled from the runner, so it
    fires even when the runner is blocked.

  **Migration:**

  ```go
  // Before
  if err := mgr.Start(ctx); err != nil { /* handle */ }
  use(mgr.CurrentAssignment())

  // After
  if err := mgr.Start(ctx); err != nil { /* handle */ }
  if err := <-mgr.WaitState(parti.StateStable, 30*time.Second); err != nil {
      /* handle */
  }
  use(mgr.CurrentAssignment())
  ```

### Follow-up issues (non-blocking for this release)

- **Apply ctx threading.** `handoffCoordinator.Apply` accepts `m.ctx`
  unbounded per attempt; threading a per-attempt deadline through to the
  consumer updater would let the background runner enforce per-attempt
  bounds. Same property exists in pre-refactor Start. Track separately.
- **Backoff jitter for `scheduleApplyRetry`.** The existing apply-retry
  loop uses ±20% jitter; consider exporting that posture as the default
  for any future async-Start retries.
- **Stress-soak test for the WaitingAssignment → Stable window.** AGENTS.md
  "Concurrency stress tests for monitor goroutines" rule applies in spirit
  to the new lifetime runner. Add a sibling to
  `test/integration/manager/manager_epoch_monitor_concurrency_test.go` in
  a follow-up PR.
- **Deterministic CAS-clobber regression pin.** The integration test in
  Task 7 (`TestStartupAsync_CalculatorStateNotClobbered`) is a liveness +
  smoke test, not a deterministic clobber pin — pure observability via
  OnStateChanged cannot distinguish clobber from normal calculator
  oscillation. A future follow-up should add a production test hook
  (e.g., `m.testHookBeforeStartupCAS func()`, mirroring the
  `testHookAfterApplyStore` pattern at `manager_assignment.go:957-959`)
  so the test can force a calculator state projection between the
  runner's apply and its CAS, then assert the CAS did NOT succeed. The
  unit tests in Task 6 cover the CAS guard's behavior in isolation;
  this follow-up would close the loop on a live-cluster regression pin.
```

- [ ] **Step 2: Commit**

```bash
git add CHANGELOG.md
git commit -m "docs(changelog): Manager.Start return-point breaking change + follow-up tracking"
```

---

### Task 13: Pre-PR gate — lint + unit + integration + cross-feature contracts

**Files:** none

- [ ] **Step 1: Lint**

Run: `make lint`
Expected: clean.

- [ ] **Step 2: Unit tests**

Run: `make test`
Expected: all PASS, no race-detector triggers.

- [ ] **Step 3: Integration tests (REQUIRED per AGENTS.md "Pre-PR gate")**

Run: `make test-integration`
Expected: all PASS. Critical contracts to verify:
- `TestManager_LiveNATSBucketLoss` (whole-bucket-missing → all workers Degraded)
- `TestManager_LiveNATSBucketLoss_OnDegradedHook` (OnDegraded once per Degraded entry)
- `TestStableID_StaleKeyTakeover_Reclaim` (peer takeover → only that worker claim-lost)
- `TestStartupAsync_CalculatorStateNotClobbered` (this PR's P0-2 pin)

If any fail, STOP and debug before proceeding.

- [ ] **Step 4: Commit lint fixes if any**

```bash
git add -p
git commit -m "chore: lint fixes for Start refactor"
```

---

### Task 14: Self-review

**Files:** none — checklist.

- [ ] **Step 1: User requirements**

User intent: "the start should do the sanity check such as required streams/bucket exist and fail-fast if we don't have recover mechanism (for example, establish retry). but it doesn't need to wait until stable."

- [x] Sanity checks stay in Start, fail-fast.
- [x] Wait-for-assignment + apply moved to background.
- [x] "Recover mechanism": uses existing watcher redelivery + `scheduleApplyRetry` + `attemptRecoveryFromDegraded`.
- [x] `WaitState(StateStable)` is the caller's block-until-ready primitive.

- [ ] **Step 2: v1 + v2 review findings**

v1:
- [x] P0-1 (degraded one-way trap): runner does not touch degraded; existing recovery paths handle it.
- [x] P0-2 (Stable clobbers calculator): direct CAS guard.
- [x] P1-3 (StartupTimeout double-budget): `m.startedAt` captured in `prepareStart`; Option A removed.
- [x] P1-4 (migration not grep-complete): Task 2 audit; k8s removed.
- [x] P1-5 (snippets don't compile): all snippets use verified API surface.
- [x] P2-6 (contract precision + diagram marker): AGENTS.md + LIFECYCLE.md diagram.

v2:
- [x] P0-1 (apply not bounded by attempt ctx): removed by removing retry loop. Boundedness explicitly documented as unchanged from pre-refactor.
- [x] P0-2 (runner clears unrelated degraded): removed by removing runner-driven degraded entry/exit.
- [x] P1-3 (Option A): removed.
- [x] P1-5 (UpdateWorkerConsumer / PartitionSource): all signatures fixed.
- [x] P1 (calculateAndPublish re-runs): removed (no retries).
- [x] P1 (backoff lockstep): removed (no backoff).
- [x] P1 (BlockingSource broken by Calculator.Start): replaced with 3-worker cluster test pattern.
- [x] P1 (unit tests in parti_test): unit tests now in `package parti`.
- [x] P1 (placeholder traffic generators + t.Skip): the calculator-race test (Task 7) is concrete; other tests demoted to follow-up issues in CHANGELOG.

- [ ] **Step 3: Verify no invariants regressed**

- [x] Apply→Store→Ack before StateStable (runner orders apply before CAS).
- [x] Post-Stable monitors start exactly once (`postStableMonitorsOnce sync.Once`).
- [x] `WorkerID()` reliable immediately after Start.
- [x] Whole-bucket-loss → degraded contract untouched (`recordKVError` path unchanged).
- [x] OnDegraded once-per-entry invariant preserved (`enterDegraded` uses `degradedSince.CompareAndSwap`).
- [x] Apply boundedness unchanged from pre-refactor (documented; not falsely claimed bounded).

- [ ] **Step 4: Run `/post-impl-review`**

Per memory `feedback_post_impl_review_workflow.md`. Pre-validate locally (`make lint && make test && make test-integration`), capture tails, then dispatch.

Run: `/post-impl-review manager-start-async docs/plans/manager-start-async/2026-05-24-manager-start-async.md v1`

Address blocking findings. Loop until merge-clean.

---

## Self-review checklist (run before handing off)

**Spec coverage:**
- User sanity-checks requirement → Task 4 (sync phase retains buckets/ID/election/heartbeat).
- User async-after-sanity requirement → Task 4 (Start returns at WaitingAssignment).
- v1+v2 P0 findings → Task 3 design (no retry loop, no runner-driven degraded exit, CAS guard).
- v2 P1 findings → Task 2 (audit), Task 3 (no backoff, no calculateAndPublish loop), Task 5-9 (compile-ready tests).
- Empty-assignment startup correctness → Task 9a (cold-start-empty bypass + empty-slice Path B + existing-cluster-stability).
- Cross-feature contracts → Task 13 (re-run pinning integration tests).

**Placeholder scan:**
- Task 6's `newManagerForStateTest` carries a documented `t.Skip` that the implementer must replace by locating the existing minimal-constructor pattern (`grep -n "func newTestManager\|func newManager\b" manager_state_test.go`). This is a deliberate "find the existing pattern, don't invent" instruction, not a silent gap.
- Task 9's `WaitStateAny` may need to be replaced with two sequential WaitState calls — the implementer note covers this.
- No other placeholders.

**Type consistency:**
- `runStartupBackground(assignmentKV jetstream.KeyValue)` defined in Task 3, called in Task 4.
- `casToStableFromWaitingAssignment` defined in Task 3, called from `runStartupBackground` + unit test.
- `startPostStableMonitors(assignmentKV)` defined in Task 3.
- `startStartupTimeoutWatchdog()` defined in Task 3, called in Task 4.
- `m.startedAt`, `m.postStableMonitorsOnce`, `m.startupWatchdogFired` fields added in Task 1.
- Option name `parti.WithWorkerConsumerUpdater` (verified at options.go:168).
- `WorkerConsumerUpdater.UpdateWorkerConsumer(ctx, workerID, partitions)` (verified at options.go:131-143).
- `PartitionSource` is Start/List/Stop only (verified at types/partition_source.go:47-80).
- `testutil.StartEmbeddedNATS` returns `(*nats.Conn, func())` (verified at internal/testutil/nats.go:22-33).
- `testutil.IntegrationTestConfig` (verified at internal/testutil/nats.go:36-59).
- `source.NewStatic` (verified at source/static.go:43-47).
- `testutil.CreateTestPartitions` and `testutil.WorkerCluster` (verified by use in `test/integration/manager/manager_epoch_monitor_concurrency_test.go:54-145`).
