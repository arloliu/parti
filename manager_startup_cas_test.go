package parti

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

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

// TestCasToStableFromWaitingAssignment_EmitsStateChangeHook pins that the
// lock-free CAS path fires the SAME OnStateChanged hook a normal transition
// would — the reason casToStableFromWaitingAssignment exists rather than a bare
// CAS. It is the net for emitTransitionEffects on the CAS caller's side
// (transitionState's net lives in TestHookGoroutinesTrackedByWaitGroup). In this
// test only casToStableFromWaitingAssignment produces a WaitingAssignment→Stable
// transition, so the flag uniquely attributes the fired hook to the CAS path.
func TestCasToStableFromWaitingAssignment_EmitsStateChangeHook(t *testing.T) {
	t.Parallel()

	var sawWaitingToStable atomic.Bool
	hookSet := &types.Hooks{
		OnStateChanged: func(_ context.Context, from, to types.State) error {
			if from == StateWaitingAssignment && to == StateStable {
				sawWaitingToStable.Store(true)
			}
			return nil
		},
	}
	mgr := newHookTestManager(hookSet)
	defer mgr.cancel()

	require.True(t, mgr.transitionState(StateClaimingID))
	require.True(t, mgr.transitionState(StateElection))
	require.True(t, mgr.transitionState(StateWaitingAssignment))

	mgr.casToStableFromWaitingAssignment()
	mgr.wg.Wait()

	require.Equal(t, StateStable, mgr.State())
	require.True(t, sawWaitingToStable.Load(),
		"the CAS path must fire OnStateChanged(WaitingAssignment, Stable) like a normal transition")
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

// TestStart_WatchdogFiresAfterStartupTimeout pins the soft watchdog
// wiring: once StartupTimeout elapses with state still
// WaitingAssignment, the watchdog enters Degraded for probe-driven pod
// rotation.
//
// This is a unit-level test driving startStartupTimeoutWatchdog
// directly, rather than the live-NATS form sketched in
// docs/plans/manager-start-async/2026-05-24-manager-start-async.md
// Task 9. See tmp/impl-deviations.md for the reason — StartupTimeout
// also bounds the synchronous sanity ctx in prepareStart, so setting
// it to an aggressively short value (1ms) kills bucket creation
// before Start can return, defeating the test.
func TestStart_WatchdogFiresAfterStartupTimeout(t *testing.T) {
	mgr, _, _, _ := newTestManager(t)
	mgr.cfg.StartupTimeout = 50 * time.Millisecond
	mgr.cfg.DegradedAlert.AlertInterval = time.Hour // suppress alert monitor in tests
	mgr.startedAt = time.Now()

	require.True(t, mgr.transitionState(StateClaimingID))
	require.True(t, mgr.transitionState(StateElection))
	require.True(t, mgr.transitionState(StateWaitingAssignment))

	mgr.startStartupTimeoutWatchdog()

	require.Eventually(t, func() bool {
		return mgr.State() == StateDegraded
	}, 2*time.Second, 10*time.Millisecond,
		"watchdog must fire enterDegraded after StartupTimeout elapses")
}

// TestStart_WatchdogNoFireAfterStateAdvanced asserts the watchdog is a
// no-op when the runner has already advanced state past
// WaitingAssignment by the time the deadline lands.
func TestStart_WatchdogNoFireAfterStateAdvanced(t *testing.T) {
	mgr, _, _, _ := newTestManager(t)
	mgr.cfg.StartupTimeout = 50 * time.Millisecond
	mgr.cfg.DegradedAlert.AlertInterval = time.Hour // suppress alert monitor in tests
	mgr.startedAt = time.Now()

	require.True(t, mgr.transitionState(StateClaimingID))
	require.True(t, mgr.transitionState(StateElection))
	require.True(t, mgr.transitionState(StateWaitingAssignment))
	require.True(t, mgr.transitionState(StateStable))

	mgr.startStartupTimeoutWatchdog()

	// Wait past the deadline; state must remain Stable.
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, StateStable, mgr.State())
}

var errTestApplyFailed = errors.New("test: apply failed")

// fullBootstrapAssignment is the pre-advanced full set a worker's
// waitForAssignment stores into the snapshot before any claim is written.
func fullBootstrapAssignment() Assignment {
	return Assignment{
		Version:        1,
		LeaderRevision: 5,
		Partitions:     []Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}},
	}
}

// TestScheduleApplyRetry_BeforeBootstrap_UsesEmptyPrev is the verify-first
// reproducer for the startup empty-diff retry self-exit (root cause RC3 / F-D3).
//
// Sequence reproduced: waitForAssignment pre-advances the in-memory snapshot to
// the full partition set BEFORE any claim is written (manager_election.go's
// m.assignment.Store with no apply hook); the initial apply fails under a
// KV-write fault and schedules a retry. On the parent the retry — and every
// other apply path — reads prev = m.CurrentAssignment() = the pre-advanced full
// set, so the handoff coordinator's prepare diff is EMPTY (preparePhase only
// writes partitions in next absent from prev): the apply "succeeds" writing ZERO
// claims and the retry self-exits. Claims never land until a process restart.
//
// The fix: while no claims have ever been committed, applyAssignmentWithPrevCore
// overrides prev to empty so the FULL claim set is (re)written. This test pins
// that the retry path benefits from the override by capturing the prev the
// coordinator receives. It references only the pre-existing API, so it compiles
// and FAILS on the parent base (the coordinator gets the full pre-advanced prev).
func TestScheduleApplyRetry_BeforeBootstrap_UsesEmptyPrev(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)

	// Simulate waitForAssignment: pre-advance the snapshot to the full set with
	// NO claims written (no apply hook ran — a direct, non-apply store).
	full := fullBootstrapAssignment()
	m.assignment.Store(full)

	// Schedule a retry for the same full set, exactly as the failed initial
	// apply does on its apply-failure arm.
	m.scheduleApplyRetry(full)

	// Wait past the ~1s+jitter retry backoff for the retry's Apply to fire.
	require.Eventually(t, func() bool { return rh.applyCount.Load() >= 1 },
		5*time.Second, 50*time.Millisecond, "retry's Apply must fire within 5s")

	rh.mu.Lock()
	defer rh.mu.Unlock()
	require.NotEmpty(t, rh.applyPrevs, "retry must have called Apply at least once")
	prev := rh.applyPrevs[len(rh.applyPrevs)-1]
	require.Empty(t, prev.Partitions,
		"before the first claim commit the coordinator MUST receive an empty prev so the "+
			"prepare diff is the FULL partition set; the pre-advanced snapshot yields an "+
			"empty diff and a zero-claim self-exit (the startup empty-diff bug)")
}

// TestApplyCore_BootstrapOverridesPrevAndRecordsCommitted pins both halves of the
// fix on the shared apply pipeline: while nothing has been committed, core uses an
// empty prev (committedAssignmentOrEmpty) so the coordinator writes the full claim
// set, and a successful write records the applied assignment as committed.
func TestApplyCore_BootstrapOverridesPrevAndRecordsCommitted(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)

	full := fullBootstrapAssignment()
	m.assignment.Store(full) // waitForAssignment pre-advance
	require.Nil(t, m.committedAssignment.Load(), "precondition: nothing committed yet")

	// A same-version apply with the pre-advanced full prev (what the
	// commit/assignment watcher computes after the pre-advance). Core must use the
	// empty committed prev so claims are actually written.
	require.NoError(t, m.applyAssignmentWithPrevCore(full, full))

	require.True(t, m.currentAssignmentApplied(full),
		"a full-set bootstrap write must record full as the committed assignment")

	rh.mu.Lock()
	defer rh.mu.Unlock()
	require.Len(t, rh.applyPrevs, 1)
	require.Empty(t, rh.applyPrevs[0].Partitions,
		"during bootstrap core MUST hand the coordinator an empty prev so the full claim "+
			"set is written, not an empty diff against the pre-advanced snapshot")
}

// TestApplyCore_BootstrapVersionAdvanceOverridesPrev pins the exact scenario
// that REQUIRES the override to live in the shared apply pipeline rather than
// only in scheduleApplyRetry: a higher-version apply (V2) arriving over the
// pre-advanced V1 snapshot while no claims are committed (a startup-window
// rebalance). Without the core override, that V2 apply reads prev =
// CurrentAssignment() = V1, computes an empty prepare diff, and writes zero
// claims — and it stale-gate-drops the V1 retry. The core override forces an
// empty prev so the full set is written. A retry-only fix leaves THIS path
// (commit-/assignment-watcher applies) empty-diffing, so this test goes RED
// under retry-only placement (verified by mutation).
func TestApplyCore_BootstrapVersionAdvanceOverridesPrev(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)

	v1 := fullBootstrapAssignment()
	v2 := Assignment{Version: 2, LeaderRevision: 6, Partitions: v1.Partitions}
	m.assignment.Store(v1) // waitForAssignment pre-advanced to V1, no claims written
	require.Nil(t, m.committedAssignment.Load())

	// Apply the NEWER version against the pre-advanced V1 snapshot.
	require.NoError(t, m.applyAssignmentWithPrevCore(v1, v2))

	require.True(t, m.currentAssignmentApplied(v2), "the V2 bootstrap write must record V2 as committed")

	rh.mu.Lock()
	defer rh.mu.Unlock()
	require.Len(t, rh.applyPrevs, 1)
	require.Empty(t, rh.applyPrevs[0].Partitions,
		"a higher-version apply over the pre-advanced snapshot during bootstrap MUST receive an "+
			"empty prev (full claim write); retry-only placement leaves this watcher path empty-diffing")
}

// TestApplyCore_AfterBootstrapKeepsPrev is the other direction of the boundary
// (feedback_test_both_directions_of_boundary): once claims ARE committed, core
// must NOT override prev, so steady-state applies stay incremental and do not
// re-issue a full-set write over already-committed claims.
func TestApplyCore_AfterBootstrapKeepsPrev(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)

	full := fullBootstrapAssignment()
	next := Assignment{
		Version:        2,
		LeaderRevision: 6,
		Partitions:     append(append([]Partition(nil), full.Partitions...), Partition{Keys: []string{"p3"}}),
	}
	m.assignment.Store(full)
	committed := full
	m.committedAssignment.Store(&committed) // full already applied+acked

	require.NoError(t, m.applyAssignmentWithPrevCore(full, next))

	rh.mu.Lock()
	defer rh.mu.Unlock()
	require.Len(t, rh.applyPrevs, 1)
	require.Equal(t, full.Partitions, rh.applyPrevs[0].Partitions,
		"once an assignment is committed core MUST diff against the committed prev "+
			"(incremental semantics), not a forced empty prev")
}

// TestApplyCore_PrevIsCommittedNotSnapshot pins the heart of the fix: the apply
// prev is the last COMMITTED assignment, not the current snapshot. After V1 is
// committed, a refresh advances the snapshot to V2 (no apply); a subsequent V2
// apply must still diff against committed V1 — otherwise prev==snapshot==V2
// yields an empty diff and writes zero claims (the latched-worker defect).
func TestApplyCore_PrevIsCommittedNotSnapshot(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)

	v1 := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	v2 := Assignment{Version: 2, LeaderRevision: 8, Partitions: []Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}}}

	// Commit V1 (bootstrap: empty prev → full set → committed = V1).
	m.assignment.Store(v1)
	require.NoError(t, m.applyAssignmentWithPrevCore(v1, v1))

	// Refresh advances the snapshot to V2 WITHOUT an apply (the monotonicStore
	// the recovery refresh performs). committed stays V1.
	m.assignment.Store(v2)

	// Apply V2: prev MUST be committed V1, NOT the V2 snapshot.
	require.NoError(t, m.applyAssignmentWithPrevCore(v2, v2))

	rh.mu.Lock()
	defer rh.mu.Unlock()
	require.Len(t, rh.applyPrevs, 2)
	require.Empty(t, rh.applyPrevs[0].Partitions, "bootstrap V1 apply uses empty prev")
	require.Equal(t, v1.Partitions, rh.applyPrevs[1].Partitions,
		"the V2 apply MUST diff against committed V1, not the pre-advanced V2 snapshot "+
			"(otherwise an empty diff writes zero claims)")
}

// TestApplyCore_CommittedUpdatesEveryApply pins that committedAssignment tracks
// EVERY successful apply (not a one-way latch) and that a FAILED apply does not
// advance it.
func TestApplyCore_CommittedUpdatesEveryApply(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)

	v1 := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	v2 := Assignment{Version: 2, LeaderRevision: 6, Partitions: []Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}}}
	v3 := Assignment{Version: 3, LeaderRevision: 7, Partitions: []Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}, {Keys: []string{"p2"}}}}

	m.assignment.Store(v1)
	require.NoError(t, m.applyAssignmentWithPrevCore(v1, v1))
	require.True(t, m.currentAssignmentApplied(v1), "committed tracks V1")

	m.assignment.Store(v2)
	require.NoError(t, m.applyAssignmentWithPrevCore(v2, v2))
	require.True(t, m.currentAssignmentApplied(v2), "committed advances to V2 (not one-way)")
	require.False(t, m.currentAssignmentApplied(v1), "committed no longer matches V1")

	// A FAILED apply must not advance committed.
	rh.errOnce.Store(&errBox{err: errTestApplyFailed})
	m.assignment.Store(v3)
	require.Error(t, m.applyAssignmentWithPrevCore(v3, v3))
	require.True(t, m.currentAssignmentApplied(v2), "a failed apply must NOT advance committed past V2")
	require.False(t, m.currentAssignmentApplied(v3), "committed must not record the failed V3")
}

// TestApplyCore_ConcurrentBootstrapApplies_LatchOnceNoRace pins the
// concurrent-apply safety the override relies on (AGENTS.md concurrency
// discipline). The retry goroutine and the commit/assignment watchers can all
// issue an apply for the same bootstrap version at once. applyStoreMu serializes
// them and committedAssignment is read+written under that lock, so EXACTLY ONE
// apply takes the empty-prev bootstrap path; the rest observe the recorded
// committed assignment and use it as prev. Run under -race by the suite.
func TestApplyCore_ConcurrentBootstrapApplies_LatchOnceNoRace(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)

	full := fullBootstrapAssignment()
	m.assignment.Store(full) // pre-advance

	const racers = 8
	var wg sync.WaitGroup
	wg.Add(racers)
	for range racers {
		go func() {
			defer wg.Done()
			_ = m.applyAssignmentWithPrevCore(full, full)
		}()
	}
	wg.Wait()

	require.True(t, m.currentAssignmentApplied(full), "the bootstrap write must record full as committed")

	rh.mu.Lock()
	defer rh.mu.Unlock()
	require.Len(t, rh.applyPrevs, racers, "all serialized applies ran")
	empties := 0
	for _, p := range rh.applyPrevs {
		if len(p.Partitions) == 0 {
			empties++
		}
	}
	require.Equal(t, 1, empties,
		"exactly one apply may take the empty-prev bootstrap path; once it records committed "+
			"the rest must use the committed prev (no redundant full-set re-write)")
}
