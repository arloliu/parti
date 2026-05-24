package parti

import (
	"testing"
	"time"

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
