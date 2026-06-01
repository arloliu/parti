package parti

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/stretchr/testify/require"
)

// TestNP5_BlockedApplyStartupTimeout_RecoversToStableAfterUnblock is the
// unit-level proof of the "...unless the runner recovers" clause of the
// startup-timeout taxonomy.
//
// Scenario:
//  1. A handoff Apply blocks during startup while the manager sits in
//     StateWaitingAssignment. The startup watchdog fires after StartupTimeout,
//     driving enterDegraded("startup-timeout").
//  2. When the blocked Apply finally completes (releaseFirst), the runner's
//     own self-exit path (casToStableFromWaitingAssignment, invoked via
//     markStartupAssignmentApplied) CANNOT promote to Stable: its CAS only
//     succeeds from WaitingAssignment, but the state is now Degraded. So the
//     successful apply does NOT silently undo the degraded entry.
//  3. attemptRecoveryFromDegraded heals the SAME process: it refreshes the
//     assignment from KV (the planted V=1 snapshot, identical (V, LR, digest)
//     to what the apply committed), sees currentAssignmentApplied == true, and
//     calls exitDegraded — transitioning Degraded -> Stable and clearing
//     degradedSince.
//
// The asserted invariants hinge on real state transitions and the committed
// assignment, never on a bare timeout. Predicted verdict: PASS.
func TestNP5_BlockedApplyStartupTimeout_RecoversToStableAfterUnblock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-NATS test in short mode")
	}
	t.Parallel()

	m, rh, _, _ := newTestManager(t)

	// Wire a real KV bucket so refreshAssignmentFromNATS (called by recovery)
	// can read the worker's planted assignment.
	_, nc := partitest.StartEmbeddedNATS(t)
	m.assignmentKV = partitest.CreateJetStreamKV(t, nc, "np5-asgn")

	// The assignment the blocked Apply commits. plantAssignment writes the SAME
	// (V, LR, digest) to the KV key so the recovery refresh produces a snapshot
	// that currentAssignmentApplied matches against committedAssignment. NOTE:
	// plantAssignment writes KV ONLY — it leaves m.assignment at the zero
	// Assignment{} that newTestManager stored, so the V=1 apply candidate passes
	// the (V, LR) stale gate and actually reaches the barrier inside Apply.
	np5Asgn := Assignment{
		Version:        1,
		LeaderRevision: 5,
		Partitions:     []Partition{{Keys: []string{"p0"}}},
	}
	plantAssignment(t, m, np5Asgn)

	// Configure the watchdog: a 100ms StartupTimeout fires quickly while the
	// Apply is parked on the barrier. Suppress the degraded alert monitor's
	// ticker (a zero AlertInterval panics; time.Hour never fires in-test).
	m.cfg.StartupTimeout = 100 * time.Millisecond
	m.cfg.DegradedAlert.AlertInterval = time.Hour
	// startStartupTimeoutWatchdog computes its deadline from m.startedAt.
	m.startedAt = time.Now()

	// Record OnDegraded reasons (hook fires from a goroutine; guard via
	// atomic.Value-backed []string copy, per the jitter-startup sibling).
	var np5DegradedReasons atomic.Value
	np5DegradedReasons.Store([]string{})
	m.hooks.OnDegraded = func(_ context.Context, reason string) error {
		prev, _ := np5DegradedReasons.Load().([]string)
		updated := append(append([]string{}, prev...), reason)
		np5DegradedReasons.Store(updated)

		return nil
	}

	// Arm the barrier: the FIRST Apply call signals firstApplyReady then blocks
	// on releaseFirst.
	rh.blockFirstApply.Store(true)

	// Drive state to WaitingAssignment (mirrors prepareStart's transitions).
	require.True(t, m.transitionState(StateClaimingID))
	require.True(t, m.transitionState(StateElection))
	require.True(t, m.transitionState(StateWaitingAssignment))

	// Launch the apply goroutine. applyAssignment routes through the core,
	// which acquires applyStoreMu and blocks inside the coordinator Apply
	// barrier — holding the manager in WaitingAssignment for the watchdog.
	// applyDone closes after Apply unblocks AND the full commit path (snapshot
	// Store, committedAssignment.Store, applyStoreMu.Unlock) has run, so the
	// recovery refresh below never contends for applyStoreMu.
	np5ApplyDone := make(chan struct{})
	go func() {
		defer close(np5ApplyDone)
		_ = m.applyAssignment(np5Asgn)
	}()

	// Gate: ensure the Apply actually parked on the barrier before starting the
	// watchdog. A spurious pass (Apply never blocked) would fatal here.
	select {
	case <-rh.firstApplyReady:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Apply did not enter the barrier within 500ms — test cannot prove the blocked-apply scenario")
	}

	// Start the watchdog. It fires Degraded("startup-timeout") at ~100ms while
	// the Apply is still parked.
	m.startStartupTimeoutWatchdog()

	// ASSERTION 1 — WATCHDOG FIRES: state goes Degraded with reason
	// "startup-timeout".
	require.Eventually(t, func() bool {
		if m.State() != StateDegraded {
			return false
		}
		reasons, _ := np5DegradedReasons.Load().([]string)

		return slices.Contains(reasons, "startup-timeout")
	}, 2*time.Second, 10*time.Millisecond,
		"watchdog must drive Degraded('startup-timeout') while the startup Apply is blocked")

	// Release the barrier. The Apply now completes: it Stores the snapshot,
	// records committedAssignment (V=1), releases applyStoreMu, and calls
	// markStartupAssignmentApplied — whose casToStableFromWaitingAssignment CAS
	// FAILS because the state is Degraded, not WaitingAssignment.
	close(rh.releaseFirst)

	// Wait for the apply goroutine to fully finish so the recovery refresh below
	// cannot contend for applyStoreMu (and so committedAssignment is visible).
	select {
	case <-np5ApplyDone:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Apply did not complete after release")
	}

	// ASSERTION 2 — APPLY COMMITTED: committedAssignment advanced to V=1
	// (the apply's full success path ran past the unblock).
	require.Eventually(t, func() bool {
		return m.committedAssignmentOrEmpty().Version == 1
	}, 2*time.Second, 10*time.Millisecond,
		"the unblocked Apply must commit V=1")

	// ASSERTION 3 — RUNNER DID NOT SELF-EXIT: immediately before recovery, the
	// state is STILL Degraded. The successful apply's
	// casToStableFromWaitingAssignment CAS could not fire from Degraded, so the
	// watchdog's degraded entry is intact.
	require.Equal(t, StateDegraded, m.State(),
		"a successful apply must NOT self-promote out of Degraded — casToStableFromWaitingAssignment CAS fails from Degraded")

	// ASSERTION 4 — RECOVERY HEALS THE SAME PROCESS: attemptRecoveryFromDegraded
	// refreshes the (identical) planted assignment, finds the current snapshot
	// applied, and exits to Stable. Called strictly AFTER the apply completed so
	// the refresh's monotonicStore does not deadlock on applyStoreMu.
	m.attemptRecoveryFromDegraded()

	require.Equal(t, StateStable, m.State(),
		"recovery must heal the blocked-apply worker from Degraded to Stable")
	require.Zero(t, m.degradedSince.Load(),
		"exitDegraded must clear degradedSince on recovery")

	// ASSERTION 5 — NO FLAP / SINGLE ENTRY: exactly one "startup-timeout" reason
	// was recorded (the watchdog fired once), and no apply retry was stashed
	// (the apply succeeded, so the failure-path scheduleApplyRetry never ran and
	// the applied-current recovery guard did not re-arm).
	reasons, _ := np5DegradedReasons.Load().([]string)
	np5Count := 0
	for _, r := range reasons {
		if r == "startup-timeout" {
			np5Count++
		}
	}
	require.Equal(t, 1, np5Count, "watchdog must enter startup-timeout Degraded exactly once (no flap)")
	require.Nil(t, m.stashedApplyRetry.Load(),
		"an applied worker must not re-arm an apply retry during recovery")
}
