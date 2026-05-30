# Degraded-Recovery Self-Heal — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop a worker that started during a sustained claim-write fault from reporting `StateStable` with zero claims written, and make it actively self-heal (no restart) once writes recover — including when a version advance lands during the Degraded window.

**Architecture:** A single guard in `Manager.attemptRecoveryFromDegraded` (`manager_degraded.go`). After the existing assignment refresh, if the worker holds an assignment it never committed claims for (`!initialClaimsCommitted && len(CurrentAssignment().Partitions) > 0`), stay degraded and re-arm a bootstrap apply for the **current** (post-refresh) assignment instead of exiting. The bootstrap override already in `applyAssignmentWithPrevCore` then writes the full claim set once the write fault clears; the next recovery tick latches the flag and exits to Stable normally.

**Tech Stack:** Go, NATS JetStream KV, `github.com/stretchr/testify/require`, embedded NATS via `partitest` / `internal/testutil`, the existing write-axis fault seam in `test/integration/failure/startup_writefault_test.go`.

**Spec:** `docs/plans/auto-healing-quorum-loss-fix/01-degraded-recovery-self-heal-plan.md` (approach B, review-clean). Read it before starting.

---

## Background the engineer must know (read once)

- **The latch.** `m.initialClaimsCommitted` is an `atomic.Bool` one-way latch (`manager.go`), set `true` only on a successful empty-prev → non-empty-next apply (`manager_assignment.go:1305`). A worker that ever committed claims has it `true` forever.
- **The bootstrap override** (`manager_assignment.go:1264`): while the latch is `false`, `applyAssignmentWithPrevCore` forces `oldAssignment = Assignment{}` so the prepare diff is the FULL partition set (writes every claim). This is the mechanism a re-armed apply rides.
- **`scheduleApplyRetry(a)`** (`manager_assignment.go:1440`): coalesces `a` into `m.stashedApplyRetry` (keeps the highest `.Version`) **synchronously** before returning, then a single goroutine applies it after a ≥0.8s backoff, re-reading `prev := m.CurrentAssignment()` at apply time.
- **The stale gate** (`isApplyResultStale`, `manager_assignment.go:1161`): `isApplyResultStale(cur, cur) == false` — re-arming with the *current* assignment passes the gate.
- **The defect site** (`manager_degraded.go:316-332`): `attemptRecoveryFromDegraded` refreshes (a READ via `refreshAssignmentFromNATS` → `monotonicStore`) then unconditionally `exitDegraded()`. Under a sustained claim-write fault with healthy reads, this reports Stable with no claims.
- **Why `cur` after the refresh:** the refresh is what advances the snapshot to a version published during the outage. Reading `cur` before the refresh re-arms the stale version that the stale gate would drop — that is the entire fix for edge (b).
- **`newTestManager(t)`** (`manager_commit_state_machine_test.go:152`) returns `(*Manager, *recordingHandoff, *recordingHeartbeat, *recordingMetrics)`; the `recordingHandoff` (`:89`) records each `Apply(ctx, workerID, prev, next)` call. Worker ID is `"worker-test"`, snapshot starts `Assignment{}`.

### Rebase note — base is `main @ 2b6bc16` ("fix manager startup stable readiness gate")

This plan was authored against `2e24875` and rebased onto `2b6bc16`. That commit reworked the **startup → Stable** readiness path. It does **not** touch `manager_degraded.go` (the fix site), but the engineer must keep two things straight:

- **Two distinct startup latches now exist** (`manager.go:154` / `:161`):
  - `initialClaimsCommitted` — set only on a successful **claims-committed** apply (`manager_assignment.go:1306`). **This is the latch the guard keys on.** It is false exactly when the worker holds an uncommitted non-empty assignment — the defect state.
  - `startupAssignmentApplied` — NEW; set once the startup assignment is **applied + acked** (true even for empty-source startup, which has no claims). **Do NOT key the guard on this one** — it would be true for a worker that acked an assignment but never wrote claims, defeating the fix.
- **Apply-pipeline success now marks startup readiness via `markStartupAssignmentApplied()`** (`manager_startup_async.go:114`, called from `applyAssignmentWithPrevCore` ~`:1360`). (The startup runner `runStartupBackground` still keeps its own idempotent direct `casToStableFromWaitingAssignment()` after the initial apply, `manager_startup_async.go:66` — so the readiness path is "pipeline marks via the helper; runner also CASes directly".) For this fix the helper is a **no-op on the heal path**: when the re-armed bootstrap apply succeeds, the worker is in `StateDegraded` (not `WaitingAssignment`), so `markStartupAssignmentApplied`'s inner WaitingAssignment-only CAS no-ops and the calculator branch (calculator-owned active states only) is skipped. The exit to Stable still happens via `exitDegraded()` on the **next recovery tick** (latch now true → guard skipped), exactly as Task 2 describes. No plan step changes.
- **Line drift is minor** (the claims latch moved `1305→1306`); every other cited line (`:1264` override, `:1440` `scheduleApplyRetry`, `:1503` `refreshAssignmentFromNATS`, `:316` `attemptRecoveryFromDegraded`) is unchanged. Treat cited line numbers as ±2 and confirm by symbol, not by number.

---

## File Structure

- **Modify:** `manager_degraded.go` — the 1 guard in `attemptRecoveryFromDegraded` (Task 2). ~10 lines.
- **Create:** `manager_degraded_recovery_selfheal_test.go` — unit tests for branch selection + edge (b) determinism + negative space (Tasks 1, 3).
- **Modify:** `test/integration/failure/resolver_readfault_test.go` — add a config-accepting variant of `rfBuildWorkerStack` so the existing builder is unchanged but a short-timeout worker can be built (Task 4).
- **Modify:** `test/integration/failure/startup_writefault_test.go` — add the edge-(a) sustained-fault integration test + the contract-3 OnDegraded-once assertion (Task 5).
- **Create:** `test/integration/failure/degraded_recovery_rearm_concurrency_test.go` — `-race` monitor-goroutine stress test, reusing the `wf` write-fault harness (Task 6).

---

## Task 1: Unit test — recovery branch selection (RED)

**Files:**
- Create: `manager_degraded_recovery_selfheal_test.go`

This test pins all four branches of the new guard. It is RED on the parent because the parent has no guard: the latch-false + non-empty case will `exitDegraded` to Stable instead of staying degraded.

- [ ] **Step 1: Write the failing test**

```go
package parti

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/stretchr/testify/require"
)

// plantAssignment writes an assignment to the worker's KV key so a subsequent
// refreshAssignmentFromNATS succeeds and advances the snapshot to it.
func plantAssignment(t *testing.T, m *Manager, a Assignment) {
	t.Helper()
	key := fmt.Sprintf("assignment.%s", m.WorkerID())
	b, err := json.Marshal(a)
	require.NoError(t, err)
	// Create-or-update: the key may already exist from a prior plant.
	if _, err := m.assignmentKV.Create(t.Context(), key, b); err != nil {
		_, err = m.assignmentKV.Put(t.Context(), key, b)
		require.NoError(t, err)
	}
}

// armDegraded puts the manager into Degraded with the given latch state and
// in-memory snapshot, then returns it ready for attemptRecoveryFromDegraded.
func armDegraded(t *testing.T, latched bool, snapshot Assignment) (*Manager, *recordingHandoff) {
	t.Helper()
	m, rh, _, _ := newTestManager(t)
	_, nc := partitest.StartEmbeddedNATS(t)
	m.assignmentKV = partitest.CreateJetStreamKV(t, nc, "selfheal-asgn")
	m.assignment.Store(snapshot)
	m.initialClaimsCommitted.Store(latched)
	m.state.Store(int32(StateDegraded))
	m.degradedSince.Store(time.Now().UnixNano())

	return m, rh
}

func TestAttemptRecovery_UnlatchedNonEmpty_StaysDegradedAndRearms(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, false, snap)
	// KV holds the SAME assignment, so refresh succeeds and snapshot stays V1.
	plantAssignment(t, m, snap)

	m.attemptRecoveryFromDegraded()

	require.NotZero(t, m.degradedSince.Load(),
		"unlatched worker holding an uncommitted assignment must STAY degraded, not exit")
	require.Equal(t, StateDegraded, m.State())
	stash := m.stashedApplyRetry.Load()
	require.NotNil(t, stash, "recovery must re-arm a bootstrap apply")
	require.Equal(t, int64(1), stash.Version, "re-arm targets the current assignment version")
}

func TestAttemptRecovery_Latched_ExitsToStable(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, true, snap) // claims already committed
	plantAssignment(t, m, snap)

	m.attemptRecoveryFromDegraded()
	m.wg.Wait()

	require.Zero(t, m.degradedSince.Load(), "a committed worker recovers normally")
	require.Equal(t, StateStable, m.State())
	require.Nil(t, m.stashedApplyRetry.Load(), "no bootstrap re-arm for a committed worker")
}

func TestAttemptRecovery_UnlatchedEmptyAssignment_ExitsToStable(t *testing.T) {
	t.Parallel()
	m, _ := armDegraded(t, false, Assignment{})
	// KV holds an empty-partition assignment at V1; refresh advances to it.
	plantAssignment(t, m, Assignment{Version: 1, LeaderRevision: 5})

	m.attemptRecoveryFromDegraded()
	m.wg.Wait()

	require.Zero(t, m.degradedSince.Load(),
		"a worker that owns no partitions has no claims to write — exit is correct")
	require.Equal(t, StateStable, m.State())
	require.Nil(t, m.stashedApplyRetry.Load())
}

func TestAttemptRecovery_RefreshFails_ReturnsBeforeGuard(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, false, snap)
	// Do NOT plant the key: refreshAssignmentFromNATS's Get fails → return
	// before the guard, no re-arm, stays degraded (whole-bucket-loss shape).

	m.attemptRecoveryFromDegraded()

	require.NotZero(t, m.degradedSince.Load(), "stays degraded when the refresh read fails")
	require.Nil(t, m.stashedApplyRetry.Load(),
		"a failed refresh must not re-arm a bootstrap apply (guard not reached)")
}
```

- [ ] **Step 2: Run the tests to verify they fail on the parent**

Run: `go test ./ -run 'TestAttemptRecovery_' -count=1 -v`
Expected: `TestAttemptRecovery_UnlatchedNonEmpty_StaysDegradedAndRearms` FAILS (parent exits to Stable → `degradedSince` is 0 and stash is nil). The other three may pass on the parent (they don't depend on the new guard) — that is fine; the discriminating RED is the unlatched-non-empty case. Confirm it fails for the right reason (Stable instead of Degraded).

- [ ] **Step 3: Commit the RED test**

```bash
git add manager_degraded_recovery_selfheal_test.go
git commit -m "test(manager): pin degraded-recovery branch selection (RED)"
```

---

## Task 2: Implement the guard (GREEN)

**Files:**
- Modify: `manager_degraded.go:316-332` (`attemptRecoveryFromDegraded`)

- [ ] **Step 1: Replace the body of `attemptRecoveryFromDegraded`**

Replace the existing function (currently lines 316-332) with:

```go
// attemptRecoveryFromDegraded checks if recovery conditions are met and exits degraded mode.
func (m *Manager) attemptRecoveryFromDegraded() {
	// Check if in degraded mode
	if m.degradedSince.Load() == 0 {
		return
	}

	// Try to refresh assignment from NATS
	if err := m.refreshAssignmentFromNATS(); err != nil {
		m.logger.Warn("failed to refresh assignment during recovery", "error", err)
		m.recordKVError(err)
		return
	}

	// The refresh succeeded (assignment reads are healthy), so the KV-error
	// window the read just exercised is stale. Record success on BOTH branches
	// below — it is not branch-dependent (harmless on the stay-degraded path
	// since recordKVError short-circuits while degraded).
	m.recordKVSuccess()

	// Startup self-heal guard (F-D3 follow-up): a worker that started during a
	// sustained claim-write fault holds a non-empty assignment (waitForAssignment
	// pre-advanced it) but never latched initialClaimsCommitted — no claims were
	// ever written. exitDegraded here would report StateStable with zero claims.
	// Instead stay degraded and re-arm a bootstrap apply for the CURRENT
	// (post-refresh) assignment so the worker actively self-heals once writes
	// recover. cur is read AFTER the refresh so it captures any version advance
	// that landed during the degraded window; re-arming with the current version
	// passes the stale gate (isApplyResultStale(cur, cur) == false) and, with the
	// latch false, the bootstrap override writes the FULL claim set. The next
	// recovery tick (after a successful apply latches the flag) exits normally.
	// This guard keys on STATE, not the degraded reason — exiting with an
	// uncommitted non-empty assignment is wrong regardless of why we degraded.
	cur := m.CurrentAssignment()
	if !m.initialClaimsCommitted.Load() && len(cur.Partitions) > 0 {
		m.scheduleApplyRetry(cur)
		return
	}

	// Success - exit degraded mode
	m.exitDegraded()
}
```

- [ ] **Step 2: Run the unit tests to verify GREEN**

Run: `go test ./ -run 'TestAttemptRecovery_' -count=1 -v`
Expected: all four PASS.

- [ ] **Step 3: Run the existing degraded-mode unit tests for no regression**

Run: `go test ./ -run 'TestManager_recordKVError|TestManager_exitDegraded|TestEnterDegraded' -count=1 -v`
Expected: all PASS (the `recordKVSuccess()` move is behavior-preserving on the exit path).

- [ ] **Step 4: Commit**

```bash
git add manager_degraded.go
git commit -m "fix(manager): self-heal instead of reporting Stable with uncommitted claims

attemptRecoveryFromDegraded refreshed the assignment (a read) and then
unconditionally exited degraded. A worker that started during a sustained
claim-write fault thus reached Stable owning partitions for which no claims
were ever written. Gate the exit on the bootstrap latch: while the worker
holds an uncommitted non-empty assignment, stay degraded and re-arm a
bootstrap apply for the current (post-refresh) assignment so it self-heals
once writes recover, including across a version advance during the window."
```

---

## Task 3: Unit test — edge (b) version advance during the window (RED-on-parent by determinism)

**Files:**
- Modify: `manager_degraded_recovery_selfheal_test.go`

This deterministically proves the `cur`-after-refresh capture: the snapshot is at V1, KV holds V2 (a version advance that landed during the Degraded window), and the re-arm must target **V2**, not the stale V1. Mirrors the codebase's existing choice to pin the version-advance path with a unit test rather than a timing-sensitive integration case (see the note at `startup_writefault_test.go:284`).

- [ ] **Step 1: Add the test**

```go
func TestAttemptRecovery_VersionAdvanceDuringWindow_RearmsAtNewVersion(t *testing.T) {
	t.Parallel()
	// Snapshot pinned at V1 (what the worker had when it degraded).
	v1 := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, false, v1)
	// A version advance landed in KV during the Degraded window: V2.
	v2 := Assignment{Version: 2, LeaderRevision: 8, Partitions: []Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}}}
	plantAssignment(t, m, v2)

	m.attemptRecoveryFromDegraded()

	require.NotZero(t, m.degradedSince.Load(), "must stay degraded")
	require.Equal(t, int64(2), m.CurrentAssignment().Version,
		"refresh must advance the snapshot to V2 before the re-arm reads cur")
	stash := m.stashedApplyRetry.Load()
	require.NotNil(t, stash, "must re-arm a bootstrap apply")
	require.Equal(t, int64(2), stash.Version,
		"re-arm MUST target V2 (cur read AFTER refresh) — not the stale V1 the gate would drop")
}
```

- [ ] **Step 2: Verify it passes with the fix and fails under a mutation**

Run: `go test ./ -run 'TestAttemptRecovery_VersionAdvanceDuringWindow' -count=1 -v`
Expected: PASS.

Mutation check (proves non-vacuity — do NOT commit the mutation): temporarily move `cur := m.CurrentAssignment()` to *before* `m.refreshAssignmentFromNATS()` in `attemptRecoveryFromDegraded`. Re-run; expected FAIL (`stash.Version == 1`, the stale version). Revert the mutation.

- [ ] **Step 3: Commit**

```bash
git add manager_degraded_recovery_selfheal_test.go
git commit -m "test(manager): pin re-arm targets the post-refresh version (edge b)"
```

---

## Task 4: Add a config-accepting integration worker-stack builder

**Files:**
- Modify: `test/integration/failure/resolver_readfault_test.go:182-245` (`rfBuildWorkerStack`)

The edge-(a) integration test needs a SHORT `StartupTimeout` + `ExitThreshold` (so the watchdog and degraded-recovery fire within the test) and an `OnDegraded` hook. The current builder hard-codes `StartupTimeout = 60s` and wires no hooks. Refactor so the existing builder is unchanged but a customizable variant exists.

- [ ] **Step 1: Extract a config/option-accepting variant and delegate**

Replace the `rfBuildWorkerStack` definition (keep everything inside identical except the two new params and where they are applied):

```go
// rfBuildWorkerStack builds a worker with the default (60s startup) config.
func rfBuildWorkerStack(
	t *testing.T,
	ctx context.Context,
	faultJS jetstream.JetStream,
	src parti.PartitionSource,
	index int,
) *rfWorkerStack {
	t.Helper()

	return rfBuildWorkerStackCfg(t, ctx, faultJS, src, index, nil, nil)
}

// rfBuildWorkerStackCfg is rfBuildWorkerStack with hooks for customizing the
// manager Config (cfgFn) and the manager Hooks (hooksFn). Either may be nil.
func rfBuildWorkerStackCfg(
	t *testing.T,
	ctx context.Context,
	faultJS jetstream.JetStream,
	src parti.PartitionSource,
	index int,
	cfgFn func(*parti.Config),
	hooksFn func(*parti.Hooks),
) *rfWorkerStack {
	t.Helper()

	s := &rfWorkerStack{}

	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		const prefix = "events."
		if subj := msg.Subject(); len(subj) > len(prefix) {
			s.consumed.Add(1)
		}

		return msg.Ack()
	})

	dyn, err := consumer.NewDynamic(
		faultJS,
		rfStreamName,
		fmt.Sprintf("w%d", index),
		rfSubjectTmpl,
		handler,
		consumer.WithPullGating(true),
		consumer.WithProcessingGate(&consumer.ProcessingGateConfig{
			Enabled:  true,
			NakDelay: 50 * time.Millisecond,
			AllowedStates: []types.HandoffState{
				types.HandoffStateStable,
				types.HandoffStateCommit,
			},
		}),
		consumer.WithResolver(consumer.ResolverConfig{
			HandoffBucketName:   rfHandoffBucket,
			HandoffClaimsPrefix: rfClaimsPrefix,
			ReconcileInterval:   1 * time.Second,
		}),
	)
	require.NoError(t, err)
	s.dyn = dyn
	t.Cleanup(func() { _ = dyn.Stop(context.Background()) })

	cfg := parti.TestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = rfHandoffBucket
	cfg.WorkerIDMax = 8
	cfg.StartupTimeout = 60 * time.Second
	if cfgFn != nil {
		cfgFn(&cfg)
	}

	opts := []parti.Option{parti.WithWorkerConsumerUpdater(dyn)}
	if hooksFn != nil {
		var h parti.Hooks
		hooksFn(&h)
		opts = append(opts, parti.WithHooks(&h)) // WithHooks takes *Hooks
	}

	mgr, err := parti.NewManager(&cfg, faultJS, src, strategy.NewConsistentHash(), opts...)
	require.NoError(t, err)
	s.mgr = mgr
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	require.NoError(t, mgr.Start(ctx))

	return s
}
```

- [ ] **Step 2: Verify the option API names (confirmed against source `2b6bc16`)**

Run: `grep -n "type Option\|func WithHooks\|func NewManager" /home/arlo/projects/parti/options.go /home/arlo/projects/parti/manager.go`
Expected: `type Option func(*managerOptions)` (`options.go:6`), `func WithHooks(hooks *Hooks) Option` (`options.go:57`), `func NewManager(..., opts ...Option)` (`manager.go:333`). The block above already uses `[]parti.Option` and `WithHooks(&h)` (pointer). If any name differs from this, match what grep returns — do NOT invent an API.

- [ ] **Step 3: Verify the refactor compiles and existing tests still pass**

Run: `go test ./test/integration/failure/ -run 'TestResolverReadFault_ConsumerSurvivesQuorumLossWindow|TestStartupWriteFault_SelfHealsWithoutRestart' -count=1`
Expected: PASS (behavior unchanged — the default builder delegates with nil hooks).

- [ ] **Step 4: Commit**

```bash
git add test/integration/failure/resolver_readfault_test.go
git commit -m "test(failure): add config/hooks-accepting worker-stack builder"
```

---

## Task 5: Integration test — edge (a) sustained fault → Degraded → no false-Stable + contract 3

**Files:**
- Modify: `test/integration/failure/startup_writefault_test.go`

Drives the worker into the unlatched-Degraded state under a sustained claim-write fault, holds it past several `ExitThreshold`-spaced recovery ticks, and asserts it does NOT report Stable while the latch is false. RED on parent: the parent exits to Stable via refresh-only recovery. Also asserts contract 3 (OnDegraded fires exactly once across the held window).

> **⚠️ IMPLEMENTATION CORRECTION (verified empirically during impl, commit `80020a8`).**
> The original premise below — "hold the claim-write fault past `StartupTimeout`, the
> **startup-timeout watchdog** enters Degraded" — is FALSE for the single-worker leader
> this test builds. `startStartupTimeoutWatchdog` only fires while state is still
> `WaitingAssignment` (`manager_startup_async.go:180`, `if m.State() != StateWaitingAssignment { return }`),
> but a single-worker leader's calculator drives state out of `WaitingAssignment` to
> Scaling/Rebalancing within ~1s (`ColdStartWindow`), so the watchdog always misses.
> The shipped test instead drives Degraded via a **dual write fault**: claims/* on the
> handoff bucket (keeps `initialClaimsCommitted` false) PLUS all writes on the heartbeat
> bucket, armed *after* `Start()` so the initial publish succeeds. Heartbeat-write errors
> route through the heartbeat publisher's `recordKVOpError` → KV-error-threshold circuit →
> `enterDegraded("kv-unavailable")`, which fires from any state. Config knobs:
> `KVErrorThreshold = 3`, `ExitThreshold = 1s`. The harness gained `ArmHeartbeat()`,
> `newWFFaultJetStreamDual`, and a `heartbeatMode` flag on `wfFaultKeyValue` (backward
> compatible — the existing `TestStartupWriteFault_SelfHealsWithoutRestart` still passes).
> **This change STRENGTHENS the design evidence:** it proves the guard fires for the
> `kv-unavailable` Degraded reason, not just startup-timeout — exactly the
> state-not-reason scoping argued in spec `01-...-plan.md` §2/§3.2. The RED-on-parent /
> GREEN-with-fix / contract-3 assertions are unchanged in intent; only the Degraded-entry
> seam changed. The code block below is the ORIGINAL (watchdog-premise) draft, retained as
> the historical record — the committed test is the source of truth.

- [ ] **Step 1: Add the test**

```go
// TestStartupWriteFault_DegradedRecoveryDoesNotReportStableUncommitted pins the
// F-D3 follow-up: under a sustained claim-write fault held PAST
// StartupTimeout+ExitThreshold, the startup-timeout watchdog enters Degraded and
// degraded-recovery runs. On the parent, recovery refreshes (a read) and exits to
// Stable with zero claims written (RED). With the fix the worker STAYS degraded
// (latch false, non-empty assignment) and self-heals once writes recover — no
// restart. Also asserts OnDegraded fires exactly once across the held window
// (cross-feature contract 3).
func TestStartupWriteFault_DegradedRecoveryDoesNotReportStableUncommitted(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	realJS, err := jetstream.New(nc)
	require.NoError(t, err)
	fc := &wfFaultController{}
	faultJS := newWFFaultJetStream(realJS, rfHandoffBucket, fc)

	rfBuildStream(t, ctx, realJS)

	pids := []string{"p0", "p1"}
	src := newWFMutableSource([]types.Partition{{Keys: []string{pids[0]}}, {Keys: []string{pids[1]}}})

	allStable := func() bool {
		for _, pid := range pids {
			if _, _, stable := rfReadClaimRevision(t, ctx, realJS, pid); !stable {
				return false
			}
		}

		return true
	}

	var degradedCalls atomic.Int64

	// Arm the write fault BEFORE start so the initial apply faults; short
	// StartupTimeout + ExitThreshold so the watchdog fires and recovery runs
	// well within the fault window.
	fc.ArmWrites()
	stack := rfBuildWorkerStackCfg(t, ctx, faultJS, src, 0,
		func(cfg *parti.Config) {
			cfg.StartupTimeout = 3 * time.Second
			cfg.DegradedBehavior.ExitThreshold = 1 * time.Second
		},
		func(h *parti.Hooks) {
			h.OnDegraded = func(_ context.Context, _ string) error {
				degradedCalls.Add(1)
				return nil
			}
		},
	)

	// The watchdog must drive the worker to Degraded under the held fault.
	require.NoError(t, <-stack.mgr.WaitState(parti.StateDegraded, 30*time.Second),
		"startup-timeout watchdog must enter Degraded under a sustained claim-write fault")

	// Hold the fault well past several ExitThreshold-spaced recovery ticks. On
	// the parent, recovery exits to Stable here (RED). With the fix the worker
	// stays Degraded because the latch is false and the assignment is non-empty.
	require.Never(t, func() bool { return stack.mgr.State() == parti.StateStable },
		8*time.Second, 250*time.Millisecond,
		"a worker with an uncommitted non-empty assignment must NOT report Stable")
	require.False(t, allStable(), "no claim may be Stable while writes are faulted")

	// --- Disarm: KV writes recover. NO restart. ---
	fc.DisarmWrites()

	require.Eventually(t, allStable, 40*time.Second, 100*time.Millisecond,
		"after write recovery the re-armed bootstrap apply must write the FULL claim set")
	require.NoError(t, <-stack.mgr.WaitState(parti.StateStable, 20*time.Second),
		"worker must reach Stable once claims are actually committed")

	// Contract 3: OnDegraded fired exactly once across the whole held window,
	// despite many recovery ticks (stay-degraded never re-enters).
	require.Equal(t, int64(1), degradedCalls.Load(),
		"OnDegraded must fire exactly once per Degraded entry (contract 3)")

	t.Logf("[%s] held Degraded under fault, self-healed to Stable after write recovery (no restart)", t.Name())
}
```

- [ ] **Step 2: Confirm the imports include `sync/atomic`**

Run: `grep -n '"sync/atomic"' /home/arlo/projects/parti/test/integration/failure/startup_writefault_test.go`
Expected: present. If absent, add it to the import block.

- [ ] **Step 3: Verify RED on the parent**

Stash the Task-2 fix and run the new test against the parent:

```bash
git stash push -- manager_degraded.go
go test ./test/integration/failure/ -run 'TestStartupWriteFault_DegradedRecoveryDoesNotReportStableUncommitted' -count=1 -v
git stash pop
```
Expected on parent: FAIL at the `require.Never(... StateStable ...)` assertion (parent exits to Stable via refresh-only recovery). After `git stash pop`, the fix is restored.

- [ ] **Step 4: Verify GREEN with the fix**

Run: `go test ./test/integration/failure/ -run 'TestStartupWriteFault_DegradedRecoveryDoesNotReportStableUncommitted' -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add test/integration/failure/startup_writefault_test.go
git commit -m "test(failure): degraded-recovery must not report Stable with uncommitted claims"
```

---

## Task 6: Concurrency `-race` stress test for the recovery re-arm

**Files:**
- Create: `test/integration/failure/degraded_recovery_rearm_concurrency_test.go`

`attemptRecoveryFromDegraded` runs on the connection-monitor goroutine; this fix adds an apply-issuing side effect (`scheduleApplyRetry`) to it. Per AGENTS.md "Concurrency stress tests for monitor goroutines", exercise the **real** recovery goroutine (driven by an armed write fault, NOT a test hook) concurrently with assignment-version churn on the same handoff bucket, and assert no race-detector trips. This reuses the `wf` write-fault harness in the same `failure_test` package (Tasks 4-5) rather than the `manager_test` `WorkerCluster` template — the `wf` harness gives a real worker stuck in unlatched-Degraded under a held fault, which is exactly the state whose recovery re-arm we must stress, and stays in one package so no cross-package exported test hooks are needed.

Design rationale (why NOT the `manager_test` template): driving the state via exported `…ForTest` accessors would require them to live in `package parti`, which is invisible to an external `manager_test` test binary. Using the real fault path keeps the test honest (the actual connection-monitor goroutine calls recovery) and needs zero production-surface test hooks.

- [ ] **Step 1: Write the stress test**

```go
package failure_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestDegradedRecovery_Rearm_NoRace stresses the degraded-recovery re-arm side
// effect (scheduleApplyRetry issued from the connection-monitor goroutine) added
// by the F-D3 follow-up, concurrently with assignment-version churn on the same
// handoff bucket. The worker is held in the real unlatched-Degraded state by a
// sustained claim-write fault (short StartupTimeout/ExitThreshold so the watchdog
// fires and recovery ticks run hot), while the source drives version advances
// that the recovery path re-arms against. Only a live worker under -race can find
// races between the real monitor goroutine and the production apply paths that
// share nats.go cached *stream state.
func TestDegradedRecovery_Rearm_NoRace(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	realJS, err := jetstream.New(nc)
	require.NoError(t, err)
	fc := &wfFaultController{}
	faultJS := newWFFaultJetStream(realJS, rfHandoffBucket, fc)

	rfBuildStream(t, ctx, realJS)

	src := newWFMutableSource([]types.Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}})

	// Hold the write fault for the whole run: the worker can never latch, so
	// every recovery tick takes the re-arm path under the race detector.
	fc.ArmWrites()
	stack := rfBuildWorkerStackCfg(t, ctx, faultJS, src, 0,
		func(cfg *parti.Config) {
			cfg.StartupTimeout = 2 * time.Second
			cfg.DegradedBehavior.ExitThreshold = 200 * time.Millisecond
		},
		nil,
	)

	require.NoError(t, <-stack.mgr.WaitState(parti.StateDegraded, 30*time.Second),
		"watchdog must drive the worker to Degraded so recovery ticks run")

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Driver: churn the source's partition set so the assignment version
	// advances repeatedly during the held fault. Each advance is what the
	// recovery re-arm reads as `cur` after the refresh — this races the
	// monitor goroutine's scheduleApplyRetry against the watcher/apply paths.
	wg.Add(1)
	go func() {
		defer wg.Done()
		toggle := true
		for {
			select {
			case <-stop:
				return
			default:
				if toggle {
					src.set([]types.Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}, {Keys: []string{"p2"}}})
				} else {
					src.set([]types.Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}})
				}
				toggle = !toggle
				time.Sleep(20 * time.Millisecond)
			}
		}
	}()

	time.Sleep(5 * time.Second)
	close(stop)
	wg.Wait()
	fc.DisarmWrites()

	// Pass condition: the run completes with no `WARNING: DATA RACE`. The race
	// detector marks the test failed at detection time; t.Failed() is the
	// in-body signal. No functional assertion is needed — the per-claim CAS
	// already arbitrates concurrent writers; this test only guards the new
	// monitor-goroutine side effect against data races.
	require.False(t, t.Failed(), "race detector tripped during the recovery re-arm stress run")
}
```

- [ ] **Step 2: Add a `set` mutator to `wfMutableSource`**

The stress test churns the source's partition set. `wfMutableSource` (defined in `startup_writefault_test.go:24`) currently has no mutator. Add one next to its constructor:

```go
// set replaces the source's partition set and signals the watcher so the
// manager re-lists and advances the assignment version.
func (s *wfMutableSource) set(parts []types.Partition) {
	s.mu.Lock()
	s.parts = append([]types.Partition(nil), parts...)
	s.mu.Unlock()
	select {
	case s.ch <- struct{}{}:
	default:
	}
}
```

- [ ] **Step 3: Run under the race detector**

Run: `go test ./test/integration/failure/ -run 'TestDegradedRecovery_Rearm_NoRace' -race -count=1 -v`
Expected: PASS, no `WARNING: DATA RACE`.

- [ ] **Step 4: Commit**

```bash
git add test/integration/failure/degraded_recovery_rearm_concurrency_test.go test/integration/failure/startup_writefault_test.go
git commit -m "test(failure): -race stress the degraded-recovery re-arm side effect"
```

---

## Task 7: Full gate — cross-feature contracts + pre-pr

**Files:** none (validation only).

- [ ] **Step 1: Run the 3 cross-feature contract regression tests**

Run:
```bash
go test ./test/integration/manager/ -run 'TestManager_LiveNATSBucketLoss|TestManager_LiveNATSBucketLoss_OnDegradedHook' -race -count=1 -v
go test ./test/integration/stableid/ -run 'TestStableID_StaleKeyTakeover_Reclaim' -race -count=1 -v
```
Expected: all PASS. These pin contracts 1, 3, and 2 respectively. If any fails, STOP — the guard regressed a shared-recovery contract; do not proceed.

- [ ] **Step 2: Run the full pre-PR gate**

Run: `make pre-pr`
Expected: lint clean, `make test` (`-race`) green, `make test-integration` (`-race`) green.

Known load-flakes (NOT regressions — see project memory): `TestLeaderElection_ColdStart` and `TestHandoffConflictStress` may flake under full-suite CPU load; re-run in isolation to confirm (`go test ./test/integration/manager/ -run 'TestLeaderElection_ColdStart' -count=3 -race`). If `make test` globs the gitignored `tmp/` scratch modules, move them aside for the run.

- [ ] **Step 3: Commit any lint fixes**

If lint required changes:
```bash
git add -A
git commit -m "chore: lint fixes for degraded-recovery self-heal"
```

---

## Task 8: Review loop → merge

- [ ] **Step 1: Simplify pass**

Invoke `/simplify` on the diff (quality-only cleanup; the change is small, expect little).

- [ ] **Step 2: External review gate**

Invoke `/codex:review` (fall back to `/post-impl-review` against the spec `01-degraded-recovery-self-heal-plan.md` for spec-compliance). Per `feedback_external_reviewer_no_revalidation`: the reviewer must NOT re-run `make test-integration`; pass it the tails of the gate output from Task 7.

- [ ] **Step 3: Fix-loop to merge-clean**

Address findings; re-review until the verdict is merge / no P0-P1. Apply the `feedback_global_grep_stale_text_in_review_loops` discipline if a finding is about stale text.

- [ ] **Step 4: Squash and open the PR**

```bash
git rebase -i main   # squash the task commits into one
```
PR title: `fix(manager): self-heal instead of reporting Stable with uncommitted claims`. Body summarizes the defect (degraded-recovery reported Stable with zero claims; version-advance left a restart-only tail), the guard, and the contract-safety argument. No plan/PR-jargon in the commit message (per `feedback_no_plan_jargon_in_commits`). Update the parent plan `00-fix-plan.md` §3 deferred-follow-up note and `01-...-plan.md` status to "implemented" in the same PR or a docs follow-up.

---

## Deferred follow-up surfaced by the final review (NOT fixed here — out of scope)

The final post-implementation code review (codex, xhigh) surfaced a **pre-existing,
out-of-scope P1** that this PR does not fix and does not regress:

**The same Stable-with-uncommitted-claims defect also exists for an *already-latched*
worker on a failed version-advance apply.** Scenario: a worker has committed V1 claims
(`initialClaimsCommitted == true`), then a V2 apply fails before its Store. During Degraded
recovery, `refreshAssignmentFromNATS` monotonic-stores V2 into the snapshot; because the
latch is already true, this PR's guard is skipped and recovery `exitDegraded`s to Stable —
while V2's newly-assigned claim is unwritten. The pending retry reads
`prev = CurrentAssignment() == V2 == next`, so the two-phase prepare diff
(`internal/assignment/handoff/twophase.go:216–230`) is empty and writes no claim.

**Why it's out of scope here and not a regression:**
- This PR's stated scope (spec `01-...-plan.md` §1/§2, invariant 4) is the **unlatched
  bootstrap** worker only; it explicitly commits to leaving committed-worker recovery
  "exactly as today." The latched path in `attemptRecoveryFromDegraded` is **byte-for-byte
  equivalent to `main`** (the guard adds only a side-effect-free read + boolean test before
  the same `exitDegraded`), so this PR cannot have regressed it.
- The edge-(b) repair (post-refresh `cur` + re-arm) only engages because the bootstrap
  override forces empty-prev *when `!latched`*. Once latched, that override never fires, so
  the retry genuinely empty-diffs — this is a **new sibling** of the §3 deferred item, framed
  around the latched worker, NOT something the existing §3 text covers ("immune" there meant
  "the guard won't touch them," not "they can't exhibit the defect for a new version").
- **This PR slightly increases the latched bug's reachability** (it makes unlatched workers
  self-heal and *reach* the latched state more often), so the follow-up deserves higher
  priority than "latent edge" — but it creates no new code path to the bug.

**Clean fix (its own PR):** track claim commitment **per assignment version** (not a one-way
"ever committed" latch); the recovery guard stays Degraded + re-arms whenever the current
assignment is non-empty AND its claims are not confirmed committed, regardless of prior
commits. This touches the shared recovery path + the latch/override semantics (contracts 1/3)
and warrants its own contract-regression pass — exactly why it is deferred, not bundled.
Do NOT add a latched-case regression test to THIS PR (it would be red or need a skip).

## Notes for the engineer

- **Verify-first is mandatory** (`feedback_verify_first_with_reproducer`): Task 1's unlatched-non-empty case and Task 5 must be confirmed RED on the parent before the fix (Tasks 1 Step 2, Task 5 Step 3). Task 3's edge-(b) determinism is proven by the mutation check (Task 3 Step 2).
- **Do not narrow the guard to a degraded-reason check.** The spec §2/§3.2 explains why the state guard (`!latch && non-empty`) is the correct predicate and reason-gating reintroduces the bug for NATS-down/kv-unavailable startup variants. A reviewer may suggest it; push back with the spec rationale.
- **API-name caution (Task 4 Step 2):** the option API is `parti.Option` + `parti.WithHooks(&h)` (pointer), confirmed against `options.go:6`/`:57` on base `2b6bc16`. If a future rebase changes these, match what grep returns — never invent an API.
- **Integration timing:** Task 5 uses `StartupTimeout=3s`, `ExitThreshold=1s` so the watchdog + several recovery ticks land inside the held window; `require.Never(... 8s ...)` spans many ticks. If the embedded-NATS box is slow, the disarm-then-heal `Eventually` bounds (40s) have ample headroom.
