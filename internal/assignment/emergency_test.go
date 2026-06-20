package assignment

import (
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// TestEmergencyDetector_GraceWindow folds the EmergencyDetector grace-window
// boundary tests (hysteresis, worker reappearance, multiple disappearances,
// partial reappearance, and zero grace period) into one table-driven test.
//
// Each row is a sequence of steps. A step optionally sleeps, then calls
// CheckEmergency(prev, curr) once and runs the step's assertion closure on the
// (emergency, workers) result. prev is fixed per row; curr can change per step
// (e.g. a reappearance observation).
func TestEmergencyDetector_GraceWindow(t *testing.T) {
	t.Parallel()

	type step struct {
		sleep  time.Duration // slept before the CheckEmergency call
		curr   map[string]bool
		assert func(t *testing.T, emergency bool, workers []string)
	}

	expectQuiet := func(t *testing.T, emergency bool, workers []string) {
		t.Helper()
		require.False(t, emergency)
		require.Empty(t, workers)
	}
	// expectFalseOnly asserts emergency is false without inspecting workers
	// (replicates originals that discarded the workers slice).
	expectFalseOnly := func(t *testing.T, emergency bool, _ []string) {
		t.Helper()
		require.False(t, emergency)
	}
	expectFired := func(want ...string) func(t *testing.T, emergency bool, workers []string) {
		return func(t *testing.T, emergency bool, workers []string) {
			t.Helper()
			require.True(t, emergency)
			require.Len(t, workers, len(want))
			for _, w := range want {
				require.Contains(t, workers, w)
			}
		}
	}

	tests := []struct {
		name        string
		gracePeriod time.Duration
		prev        map[string]bool
		steps       []step
	}{
		{
			// TestEmergencyDetector_Hysteresis
			name:        "hysteresis",
			gracePeriod: 200 * time.Millisecond,
			prev:        map[string]bool{"worker-1": true, "worker-2": true, "worker-3": true},
			steps: []step{
				// Worker-2 disappears. Immediate check - not emergency yet.
				{curr: map[string]bool{"worker-1": true, "worker-3": true}, assert: expectQuiet},
				// Wait half grace period.
				{sleep: 100 * time.Millisecond, curr: map[string]bool{"worker-1": true, "worker-3": true}, assert: expectQuiet},
				// Wait full grace period (total 210ms).
				{sleep: 110 * time.Millisecond, curr: map[string]bool{"worker-1": true, "worker-3": true}, assert: expectFired("worker-2")},
			},
		},
		{
			// TestEmergencyDetector_WorkerReappears
			name:        "worker reappears",
			gracePeriod: 200 * time.Millisecond,
			prev:        map[string]bool{"worker-1": true, "worker-2": true},
			steps: []step{
				// Worker-2 disappears.
				{curr: map[string]bool{"worker-1": true}, assert: expectFalseOnly},
				// Wait 100ms, worker-2 reappears.
				{sleep: 100 * time.Millisecond, curr: map[string]bool{"worker-1": true, "worker-2": true}, assert: expectQuiet},
				// Wait another 200ms - still no emergency (tracking cleared).
				{sleep: 200 * time.Millisecond, curr: map[string]bool{"worker-1": true, "worker-2": true}, assert: expectQuiet},
			},
		},
		{
			// TestEmergencyDetector_MultipleWorkers
			name:        "multiple workers",
			gracePeriod: 200 * time.Millisecond,
			prev: map[string]bool{
				"worker-1": true,
				"worker-2": true,
				"worker-3": true,
				"worker-4": true,
			},
			steps: []step{
				// Two workers disappear. Immediate check - no emergency yet.
				{curr: map[string]bool{"worker-1": true, "worker-2": true}, assert: expectQuiet},
				// Wait past grace period.
				{sleep: 210 * time.Millisecond, curr: map[string]bool{"worker-1": true, "worker-2": true}, assert: expectFired("worker-3", "worker-4")},
			},
		},
		{
			// TestEmergencyDetector_PartialReappearance
			name:        "partial reappearance",
			gracePeriod: 200 * time.Millisecond,
			prev: map[string]bool{
				"worker-1": true,
				"worker-2": true,
				"worker-3": true,
			},
			steps: []step{
				// Two workers disappear. Start tracking.
				{curr: map[string]bool{"worker-1": true}, assert: expectFalseOnly},
				// Wait 100ms, worker-2 reappears, worker-3 still missing.
				{sleep: 100 * time.Millisecond, curr: map[string]bool{"worker-1": true, "worker-2": true}, assert: expectQuiet},
				// Wait another 120ms (total 220ms for worker-3). Only worker-3 fires.
				{sleep: 120 * time.Millisecond, curr: map[string]bool{"worker-1": true, "worker-2": true}, assert: func(t *testing.T, emergency bool, workers []string) {
					t.Helper()
					require.True(t, emergency)
					require.Len(t, workers, 1)
					require.Contains(t, workers, "worker-3")
					require.NotContains(t, workers, "worker-2") // worker-2 tracking was cleared
				}},
			},
		},
		{
			// TestEmergencyDetector_ZeroGracePeriod
			name:        "zero grace period",
			gracePeriod: 0 * time.Second,
			prev:        map[string]bool{"worker-1": true, "worker-2": true},
			steps: []step{
				// With zero grace period, should be immediate emergency.
				{curr: map[string]bool{"worker-1": true}, assert: expectFired("worker-2")},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			detector := NewEmergencyDetector(tt.gracePeriod)
			for _, s := range tt.steps {
				if s.sleep > 0 {
					time.Sleep(s.sleep)
				}
				emergency, workers, _ := detector.CheckEmergency(tt.prev, s.curr)
				s.assert(t, emergency, workers)
			}
		})
	}
}

// TestCheckEmergency_RecoveryThenSecondDisappearance_HonorsGracePeriod is the FP-1
// reproducer (E1).
//
// FP-1 scenario: worker A disappears, becomes stranded in the detector's
// disappearedWorkers map (because lastWorkers got rebuilt without A via an
// out-of-band path — audit_repair, scaling-timer rebalance, etc.), then
// A's heartbeat reappears (observed by curr alone, not yet in prev), and
// finally A disappears again. The detector must honor the grace period for
// the second disappearance.
//
// On main this fails because:
//   - The old CheckEmergency only iterates `range prev` to clear tracking.
//   - During the recovery observation, A is in curr but not in prev, so
//     the stale firstSeen is never cleared.
//   - On the second disappearance, the stale firstSeen makes the second
//     disappearance look immediately confirmed — emergency fires without
//     waiting a fresh grace period.
//
// The v9 fix (Phase 1 unconditional clear by curr) closes this.
func TestCheckEmergency_RecoveryThenSecondDisappearance_HonorsGracePeriod(t *testing.T) {
	t.Parallel()
	gracePeriod := 200 * time.Millisecond
	detector := NewEmergencyDetector(gracePeriod)

	// Inject a deterministic clock.
	current := time.Unix(0, 0)
	detector.now = func() time.Time { return current }

	// Step 1: A disappears. Track A with firstSeen = current.
	emergency, confirmed, _ := detector.CheckEmergency(
		map[string]bool{"A": true, "B": true},
		map[string]bool{"B": true},
	)
	require.False(t, emergency)
	require.Empty(t, confirmed)

	// Step 2: lastWorkers gets rebuilt out-of-band (e.g., audit_repair did a
	// fresh KV scan that didn't include A). A is now stranded in the detector
	// but no longer in prev. Time advances past half the grace period.
	current = current.Add(100 * time.Millisecond)
	emergency, confirmed, _ = detector.CheckEmergency(
		map[string]bool{"B": true},
		map[string]bool{"B": true},
	)
	require.False(t, emergency)
	require.Empty(t, confirmed)

	// Step 3: A's heartbeat reappears. A is in curr but not yet in prev.
	// The fix must clear the stranded tracking here. Still inside the
	// original grace window.
	current = current.Add(90 * time.Millisecond)
	emergency, confirmed, _ = detector.CheckEmergency(
		map[string]bool{"B": true},
		map[string]bool{"A": true, "B": true},
	)
	require.False(t, emergency)
	require.Empty(t, confirmed)

	// Step 4: A disappears again 20ms later. Total elapsed from t=0 is 210ms
	// (> grace period of 200ms), but the SECOND disappearance is only 20ms
	// old — must NOT fire emergency.
	current = current.Add(20 * time.Millisecond)
	emergency, confirmed, _ = detector.CheckEmergency(
		map[string]bool{"A": true, "B": true},
		map[string]bool{"B": true},
	)
	require.False(t, emergency,
		"second disappearance must honor a fresh grace period, not inherit firstSeen from the first disappearance")
	require.Empty(t, confirmed)
}

// newDeterministicDetector returns a detector with a settable clock seam.
// All E-tests below use it so timing is deterministic.
func newDeterministicDetector(gracePeriod time.Duration) (*EmergencyDetector, *time.Time) {
	d := NewEmergencyDetector(gracePeriod)
	t0 := time.Unix(0, 0)
	now := t0
	d.now = func() time.Time { return now }
	return d, &now
}

// E2: Phase 1 (clear by curr) unconditionally clears stranded AND in-prev entries.
func TestCheckEmergency_ClearByCurr_ClearsStrandedAndInPrev(t *testing.T) {
	t.Parallel()
	d, clock := newDeterministicDetector(200 * time.Millisecond)

	// Track A (in-prev) and B (will become stranded).
	_, _, _ = d.CheckEmergency(map[string]bool{"A": true, "B": true}, map[string]bool{})
	// Strand B by removing it from prev.
	*clock = clock.Add(50 * time.Millisecond)
	_, _, _ = d.CheckEmergency(map[string]bool{"A": true}, map[string]bool{})

	require.Len(t, d.disappearedWorkers, 2)

	// Both A (in-prev) and B (stranded) appear alive in curr.
	*clock = clock.Add(50 * time.Millisecond)
	_, _, _ = d.CheckEmergency(map[string]bool{"A": true}, map[string]bool{"A": true, "B": true})

	require.Empty(t, d.disappearedWorkers, "Phase 1 must clear both stranded and in-prev entries")
}

// E3: Same-count replacement (A leaves, B arrives) tracks A and fires emergency for A after grace.
func TestCheckEmergency_SameCountReplacement_TracksAndFires(t *testing.T) {
	t.Parallel()
	d, clock := newDeterministicDetector(200 * time.Millisecond)

	// A is in prev, replacement B in curr (same count). A is missing.
	emergency, confirmed, pending := d.CheckEmergency(
		map[string]bool{"A": true, "B": true},
		map[string]bool{"B": true, "C": true},
	)
	require.False(t, emergency)
	require.Empty(t, confirmed)
	require.True(t, pending, "tracking A must report pending=true so scheduler suppresses planned_scale")

	// After grace, A's disappearance fires emergency.
	*clock = clock.Add(210 * time.Millisecond)
	emergency, confirmed, pending = d.CheckEmergency(
		map[string]bool{"A": true, "B": true},
		map[string]bool{"B": true, "C": true},
	)
	require.True(t, emergency)
	require.ElementsMatch(t, []string{"A"}, confirmed)
	require.True(t, pending)
}

// E4: Two co-pending workers tracked independently with separate firstSeen.
func TestCheckEmergency_CoPending_BothTrackedIndependently(t *testing.T) {
	t.Parallel()
	d, clock := newDeterministicDetector(200 * time.Millisecond)

	// A disappears at t=0; B and C still alive.
	_, _, _ = d.CheckEmergency(
		map[string]bool{"A": true, "B": true, "C": true},
		map[string]bool{"B": true, "C": true},
	)
	// B disappears 100ms later (A still missing).
	*clock = clock.Add(100 * time.Millisecond)
	_, _, _ = d.CheckEmergency(
		map[string]bool{"A": true, "B": true, "C": true},
		map[string]bool{"C": true},
	)
	// At t=210ms: A's grace expired (firstSeen=0, age=210ms >= 200ms);
	// B's grace not yet expired (firstSeen=100ms, age=110ms < 200ms).
	*clock = clock.Add(110 * time.Millisecond)
	emergency, confirmed, pending := d.CheckEmergency(
		map[string]bool{"A": true, "B": true, "C": true},
		map[string]bool{"C": true},
	)
	require.True(t, emergency)
	require.ElementsMatch(t, []string{"A"}, confirmed)
	require.True(t, pending, "B is still tracked")

	// At t=310ms: B's grace also expired (firstSeen=100ms, age=210ms >= 200ms).
	*clock = clock.Add(100 * time.Millisecond)
	emergency, confirmed, _ = d.CheckEmergency(
		map[string]bool{"A": true, "B": true, "C": true},
		map[string]bool{"C": true},
	)
	require.True(t, emergency)
	require.ElementsMatch(t, []string{"A", "B"}, confirmed)
}

// E5: Stranded entries (worker no longer in prev) are not confirmed by Phase 4.
func TestCheckEmergency_StrandedEntriesDoNotFire(t *testing.T) {
	t.Parallel()
	d, clock := newDeterministicDetector(100 * time.Millisecond)

	// Track A.
	_, _, _ = d.CheckEmergency(map[string]bool{"A": true, "B": true}, map[string]bool{"B": true})
	require.Len(t, d.disappearedWorkers, 1)

	// Drop A from prev (becomes stranded). Time exceeds grace.
	*clock = clock.Add(200 * time.Millisecond)
	emergency, confirmed, pending := d.CheckEmergency(map[string]bool{"B": true}, map[string]bool{"B": true})
	require.False(t, emergency, "stranded entry must not fire emergency")
	require.Empty(t, confirmed)
	require.False(t, pending, "pending is false: entry is not in prev")
	require.Contains(t, d.disappearedWorkers, "A", "stranded entry retained (safety valve hasn't fired)")
}

// E6: Idempotent — repeated tracking calls preserve firstSeen.
func TestCheckEmergency_TrackingIdempotent_FirstSeenPreserved(t *testing.T) {
	t.Parallel()
	d, clock := newDeterministicDetector(200 * time.Millisecond)
	prev := map[string]bool{"A": true, "B": true}
	curr := map[string]bool{"B": true}

	_, _, _ = d.CheckEmergency(prev, curr)
	originalFirstSeen := d.disappearedWorkers["A"]

	for range 5 {
		*clock = clock.Add(20 * time.Millisecond)
		_, _, _ = d.CheckEmergency(prev, curr)
	}

	require.Equal(t, originalFirstSeen, d.disappearedWorkers["A"], "firstSeen must be preserved across repeated observations")
}

// E7: A new disappearance after a recovery starts a fresh grace period.
func TestCheckEmergency_NewDisappearance_FreshGracePeriod(t *testing.T) {
	t.Parallel()
	d, clock := newDeterministicDetector(100 * time.Millisecond)

	// A disappears at t=0.
	_, _, _ = d.CheckEmergency(map[string]bool{"A": true, "B": true}, map[string]bool{"B": true})
	// A recovers at t=50ms (in both prev and curr).
	*clock = clock.Add(50 * time.Millisecond)
	_, _, _ = d.CheckEmergency(map[string]bool{"A": true, "B": true}, map[string]bool{"A": true, "B": true})
	require.Empty(t, d.disappearedWorkers, "recovery cleared tracking")
	// A disappears again at t=60ms.
	*clock = clock.Add(10 * time.Millisecond)
	_, _, _ = d.CheckEmergency(map[string]bool{"A": true, "B": true}, map[string]bool{"B": true})
	require.Equal(t, time.Unix(0, 0).Add(60*time.Millisecond), d.disappearedWorkers["A"], "new firstSeen at re-disappearance time")
}

// E8: Recovery clears pending state.
func TestCheckEmergency_RecoveryClearsPending(t *testing.T) {
	t.Parallel()
	d, clock := newDeterministicDetector(200 * time.Millisecond)

	_, _, pending := d.CheckEmergency(map[string]bool{"A": true, "B": true}, map[string]bool{"B": true})
	require.True(t, pending)

	*clock = clock.Add(50 * time.Millisecond)
	_, _, pending = d.CheckEmergency(map[string]bool{"A": true, "B": true}, map[string]bool{"A": true, "B": true})
	require.False(t, pending, "pending must drop after recovery")
}

// E9: Safety valve prunes stranded entries older than 10*gracePeriod.
func TestCheckEmergency_AgeBasedSafetyValve_PrunesStrandedOnly(t *testing.T) {
	t.Parallel()
	d, clock := newDeterministicDetector(100 * time.Millisecond) // 10*grace = 1000ms

	// Track A.
	_, _, _ = d.CheckEmergency(map[string]bool{"A": true, "B": true}, map[string]bool{"B": true})
	// Strand A.
	*clock = clock.Add(50 * time.Millisecond)
	_, _, _ = d.CheckEmergency(map[string]bool{"B": true}, map[string]bool{"B": true})
	require.Contains(t, d.disappearedWorkers, "A")

	// Advance past 10*grace.
	*clock = clock.Add(1100 * time.Millisecond)
	_, _, _ = d.CheckEmergency(map[string]bool{"B": true}, map[string]bool{"B": true})
	require.NotContains(t, d.disappearedWorkers, "A", "stranded entry older than 10*grace must be pruned")
}

// E10: Safety valve boundary — stranded entries at exactly 10*grace are NOT pruned (uses >, not >=).
func TestCheckEmergency_SafetyValveBoundary_StrandedOnly(t *testing.T) {
	t.Parallel()
	d, clock := newDeterministicDetector(100 * time.Millisecond) // 10*grace = 1000ms

	_, _, _ = d.CheckEmergency(map[string]bool{"A": true, "B": true}, map[string]bool{"B": true})
	// Strand A.
	*clock = clock.Add(10 * time.Millisecond)
	_, _, _ = d.CheckEmergency(map[string]bool{"B": true}, map[string]bool{"B": true})

	// Advance to exactly 10*grace from firstSeen.
	*clock = time.Unix(0, 0).Add(1000 * time.Millisecond)
	_, _, _ = d.CheckEmergency(map[string]bool{"B": true}, map[string]bool{"B": true})
	require.Contains(t, d.disappearedWorkers, "A", "at the 10*grace boundary the entry is preserved")

	// Now also assert in-prev entries are never pruned by the safety valve, even when old.
	d2, clock2 := newDeterministicDetector(100 * time.Millisecond)
	_, _, _ = d2.CheckEmergency(map[string]bool{"X": true}, map[string]bool{})
	*clock2 = clock2.Add(5 * time.Second) // way past 10*grace
	_, _, _ = d2.CheckEmergency(map[string]bool{"X": true}, map[string]bool{})
	require.Contains(t, d2.disappearedWorkers, "X", "in-prev entries are never pruned by the safety valve")
}

// E11: ObserveAlive clears tracked entries.
func TestObserveAlive_ClearsTrackedEntries(t *testing.T) {
	t.Parallel()
	d, _ := newDeterministicDetector(200 * time.Millisecond)

	_, _, _ = d.CheckEmergency(map[string]bool{"A": true, "B": true, "C": true}, map[string]bool{})
	require.Len(t, d.disappearedWorkers, 3)

	d.ObserveAlive([]string{"A", "B"})

	require.NotContains(t, d.disappearedWorkers, "A")
	require.NotContains(t, d.disappearedWorkers, "B")
	require.Contains(t, d.disappearedWorkers, "C", "unobserved entries are preserved")
}

// E12: ObserveAlive with an empty slice is a no-op.
func TestObserveAlive_EmptyCurr_NoOp(t *testing.T) {
	t.Parallel()
	d, _ := newDeterministicDetector(200 * time.Millisecond)

	_, _, _ = d.CheckEmergency(map[string]bool{"A": true, "B": true}, map[string]bool{})
	before := len(d.disappearedWorkers)

	d.ObserveAlive(nil)

	require.Equal(t, before, len(d.disappearedWorkers))
}

// TestEmergencyDetector_NoWorkerChange verifies no emergency when workers stable.
func TestEmergencyDetector_NoWorkerChange(t *testing.T) {
	t.Parallel()
	gracePeriod := 200 * time.Millisecond
	detector := NewEmergencyDetector(gracePeriod)

	workers := map[string]bool{"worker-1": true, "worker-2": true}

	// No changes
	emergency, disappeared, _ := detector.CheckEmergency(workers, workers)
	require.False(t, emergency)
	require.Empty(t, disappeared)

	// Wait past grace period - still no emergency (no changes)
	time.Sleep(210 * time.Millisecond)
	emergency, disappeared, _ = detector.CheckEmergency(workers, workers)
	require.False(t, emergency)
	require.Empty(t, disappeared)
}

// --- merged from emergency_edge_test.go ---

// TestEmergency_BasicScenarios tests basic emergency detection scenarios using table-driven approach.
// Uses shorter grace periods (100ms) and parallel execution for faster tests.
func TestEmergency_BasicScenarios(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		gracePeriod     time.Duration
		prevWorkers     map[string]bool
		currWorkers     map[string]bool
		waitAfterGrace  time.Duration
		expectEmergency bool
		expectedWorkers []string
		scenario        string
	}{
		{
			name:            "single worker disappears",
			gracePeriod:     100 * time.Millisecond,
			prevWorkers:     map[string]bool{"worker-1": true},
			currWorkers:     map[string]bool{},
			waitAfterGrace:  110 * time.Millisecond,
			expectEmergency: true,
			expectedWorkers: []string{"worker-1"},
			scenario:        "Single-node deployment crashes",
		},
		{
			name:            "single worker remains stable",
			gracePeriod:     100 * time.Millisecond,
			prevWorkers:     map[string]bool{"worker-1": true},
			currWorkers:     map[string]bool{"worker-1": true},
			waitAfterGrace:  110 * time.Millisecond,
			expectEmergency: false,
			expectedWorkers: []string{},
			scenario:        "Single-node deployment running normally",
		},
		{
			name:            "two workers one disappears",
			gracePeriod:     100 * time.Millisecond,
			prevWorkers:     map[string]bool{"worker-1": true, "worker-2": true},
			currWorkers:     map[string]bool{"worker-1": true},
			waitAfterGrace:  110 * time.Millisecond,
			expectEmergency: true,
			expectedWorkers: []string{"worker-2"},
			scenario:        "50% capacity loss in two-node cluster",
		},
		{
			name:            "two workers both disappear",
			gracePeriod:     100 * time.Millisecond,
			prevWorkers:     map[string]bool{"worker-1": true, "worker-2": true},
			currWorkers:     map[string]bool{},
			waitAfterGrace:  110 * time.Millisecond,
			expectEmergency: true,
			expectedWorkers: []string{"worker-1", "worker-2"},
			scenario:        "Total cluster failure",
		},
		{
			name:            "zero to one worker (cold start)",
			gracePeriod:     100 * time.Millisecond,
			prevWorkers:     map[string]bool{},
			currWorkers:     map[string]bool{"worker-1": true},
			waitAfterGrace:  110 * time.Millisecond,
			expectEmergency: false,
			expectedWorkers: []string{},
			scenario:        "First deployment, cluster starts",
		},
		{
			name:        "all workers disappear simultaneously",
			gracePeriod: 100 * time.Millisecond,
			prevWorkers: map[string]bool{
				"worker-1": true, "worker-2": true, "worker-3": true,
				"worker-4": true, "worker-5": true,
			},
			currWorkers:     map[string]bool{},
			waitAfterGrace:  110 * time.Millisecond,
			expectEmergency: true,
			expectedWorkers: []string{"worker-1", "worker-2", "worker-3", "worker-4", "worker-5"},
			scenario:        "Data center outage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			detector := NewEmergencyDetector(tt.gracePeriod)

			// Initial check - should not be emergency yet
			emergency, workers, _ := detector.CheckEmergency(tt.prevWorkers, tt.currWorkers)
			require.False(t, emergency, "Should not be emergency within grace period")
			require.Empty(t, workers)

			// Wait for grace period to expire
			time.Sleep(tt.waitAfterGrace)

			// Check again after grace period
			emergency, workers, _ = detector.CheckEmergency(tt.prevWorkers, tt.currWorkers)
			require.Equal(t, tt.expectEmergency, emergency, "Emergency detection mismatch for: %s", tt.scenario)

			if tt.expectEmergency {
				require.Len(t, workers, len(tt.expectedWorkers), "Wrong number of disappeared workers")
				for _, expectedWorker := range tt.expectedWorkers {
					require.Contains(t, workers, expectedWorker, "Expected worker not in disappeared list")
				}
			} else {
				require.Empty(t, workers, "No workers should be in disappeared list")
			}
		})
	}
}

// TestEmergency_SingleWorkerDisappears_ThresholdCalculation verifies that emergency
// detection works correctly for the edge case of a single worker disappearing.
//
// This test documents that there's no explicit threshold check - ANY worker disappearing
// beyond grace period is an emergency, regardless of percentage.
//
// Production consideration: In a 100-node cluster, losing 1 node (1%) still triggers
// emergency because it means some partitions need immediate reassignment.
func TestEmergency_SingleWorkerDisappears_ThresholdCalculation(t *testing.T) {
	t.Parallel()

	gracePeriod := 100 * time.Millisecond
	detector := NewEmergencyDetector(gracePeriod)

	// Large cluster: 10 workers
	prev := map[string]bool{
		"worker-1": true, "worker-2": true, "worker-3": true, "worker-4": true, "worker-5": true,
		"worker-6": true, "worker-7": true, "worker-8": true, "worker-9": true, "worker-10": true,
	}

	// Current state: only 1 worker disappeared (10%)
	curr := map[string]bool{
		"worker-1": true, "worker-2": true, "worker-3": true, "worker-4": true, "worker-5": true,
		"worker-6": true, "worker-7": true, "worker-8": true, "worker-9": true,
	}

	// Initial check
	emergency, _, _ := detector.CheckEmergency(prev, curr)
	require.False(t, emergency)

	// After grace period - should still be emergency even though only 10% lost
	time.Sleep(110 * time.Millisecond)
	emergency, workers, _ := detector.CheckEmergency(prev, curr)
	require.True(t, emergency, "Even 1 worker disappearing (10%) should be emergency")
	require.Len(t, workers, 1)
	require.Contains(t, workers, "worker-10")
}

// TestEmergency_CascadingFailures tests cascading worker failures with independent tracking.
//
// Production scenario: First one node fails, then others follow with different timing.
func TestEmergency_CascadingFailures(t *testing.T) {
	t.Parallel()

	gracePeriod := 200 * time.Millisecond
	detector := NewEmergencyDetector(gracePeriod)

	// Initial state: 4 workers
	prev := map[string]bool{
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
		"worker-4": true,
	}

	// First failure: worker-4 disappears
	curr1 := map[string]bool{
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
	}

	emergency, workers, _ := detector.CheckEmergency(prev, curr1)
	require.False(t, emergency, "Should be in grace period for worker-4")
	require.Empty(t, workers)

	// Wait 100ms (half grace period)
	time.Sleep(100 * time.Millisecond)

	// Second failure: worker-3 also disappears (cascading failure)
	curr2 := map[string]bool{
		"worker-1": true,
		"worker-2": true,
	}

	emergency, workers, _ = detector.CheckEmergency(prev, curr2)
	require.False(t, emergency, "Should still be in grace period for both")
	require.Empty(t, workers)

	// Wait another 120ms (total 220ms for worker-4, 120ms for worker-3)
	time.Sleep(120 * time.Millisecond)

	emergency, workers, _ = detector.CheckEmergency(prev, curr2)
	require.True(t, emergency, "worker-4 exceeded grace period")
	require.Len(t, workers, 1, "Only worker-4 should be past grace period")
	require.Contains(t, workers, "worker-4")
	require.NotContains(t, workers, "worker-3", "worker-3 still in grace period")

	// Wait another 100ms (total 320ms for worker-4, 220ms for worker-3)
	time.Sleep(100 * time.Millisecond)

	emergency, workers, _ = detector.CheckEmergency(prev, curr2)
	require.True(t, emergency, "Both workers exceeded grace period")
	require.Len(t, workers, 2, "Both workers should be past grace period now")
	require.Contains(t, workers, "worker-4")
	require.Contains(t, workers, "worker-3")
}

// TestEmergency_WorkerFlapping tests worker flapping scenarios with table-driven approach.
func TestEmergency_WorkerFlapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		scenario    string
		setupAndRun func(t *testing.T, detector *EmergencyDetector)
	}{
		{
			name:     "flapping within grace period",
			scenario: "Network hiccup causes brief heartbeat loss, worker reconnects quickly",
			setupAndRun: func(t *testing.T, detector *EmergencyDetector) {
				prev := map[string]bool{"worker-1": true, "worker-2": true}

				// Worker-2 disappears
				currDisappeared := map[string]bool{"worker-1": true}
				emergency, workers, _ := detector.CheckEmergency(prev, currDisappeared)
				require.False(t, emergency)
				require.Empty(t, workers)

				// Wait 50ms (half grace period)
				time.Sleep(50 * time.Millisecond)

				// Worker-2 reappears (flap)
				currReappeared := map[string]bool{"worker-1": true, "worker-2": true}
				emergency, workers, _ = detector.CheckEmergency(prev, currReappeared)
				require.False(t, emergency, "Worker reappearing within grace period should not be emergency")
				require.Empty(t, workers)

				// Wait another 70ms (total 120ms from initial disappearance)
				time.Sleep(70 * time.Millisecond)

				// Worker still present - should still not be emergency (tracking was cleared)
				emergency, workers, _ = detector.CheckEmergency(prev, currReappeared)
				require.False(t, emergency, "Tracking should be cleared, no emergency")
				require.Empty(t, workers)
			},
		},
		{
			name:     "flapping beyond grace period",
			scenario: "Worker has network issues, eventually crashes permanently",
			setupAndRun: func(t *testing.T, detector *EmergencyDetector) {
				prev := map[string]bool{"worker-1": true, "worker-2": true}

				// Worker-2 disappears
				currDisappeared := map[string]bool{"worker-1": true}
				emergency, _, _ := detector.CheckEmergency(prev, currDisappeared)
				require.False(t, emergency)

				// Wait 50ms
				time.Sleep(50 * time.Millisecond)

				// Worker-2 reappears briefly (flap)
				currReappeared := map[string]bool{"worker-1": true, "worker-2": true}
				emergency, _, _ = detector.CheckEmergency(prev, currReappeared)
				require.False(t, emergency)

				// Wait 20ms
				time.Sleep(20 * time.Millisecond)

				// Worker-2 disappears again (permanent failure)
				emergency, _, _ = detector.CheckEmergency(prev, currDisappeared)
				require.False(t, emergency, "Should restart grace period tracking")

				// Wait full grace period from this disappearance
				time.Sleep(110 * time.Millisecond)

				// Now should be emergency
				emergency, workers, _ := detector.CheckEmergency(prev, currDisappeared)
				require.True(t, emergency, "Worker disappeared beyond grace period after flapping")
				require.Len(t, workers, 1)
				require.Contains(t, workers, "worker-2")
			},
		},
		{
			name:     "worker rejoins before emergency detected",
			scenario: "Pod restart completes within grace period",
			setupAndRun: func(t *testing.T, detector *EmergencyDetector) {
				prev := map[string]bool{"worker-1": true, "worker-2": true, "worker-3": true}

				// Worker-2 disappears
				currDisappeared := map[string]bool{"worker-1": true, "worker-3": true}
				emergency, _, _ := detector.CheckEmergency(prev, currDisappeared)
				require.False(t, emergency)

				// Wait 80ms (almost at grace period)
				time.Sleep(80 * time.Millisecond)

				// Worker-2 rejoins just in time
				currRejoined := map[string]bool{"worker-1": true, "worker-2": true, "worker-3": true}
				emergency, workers, _ := detector.CheckEmergency(prev, currRejoined)
				require.False(t, emergency, "Worker rejoining before grace period should cancel emergency")
				require.Empty(t, workers)

				// Wait another 50ms (would have been past grace period)
				time.Sleep(50 * time.Millisecond)

				// Still should not be emergency
				emergency, workers, _ = detector.CheckEmergency(prev, currRejoined)
				require.False(t, emergency, "Rejoined worker should not trigger emergency later")
				require.Empty(t, workers)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			detector := NewEmergencyDetector(100 * time.Millisecond)
			tt.setupAndRun(t, detector)
		})
	}
}

// TestEmergency_K8sRollingUpdate simulates Kubernetes rolling update where pods restart
// in quick succession but each completes within grace period.
//
// Production scenario: Kubernetes rolling update with 3 pods, each pod restarts in sequence,
// each restart takes ~60ms but grace period is 200ms. Should NOT trigger emergency.
func TestEmergency_K8sRollingUpdate(t *testing.T) {
	t.Parallel()

	gracePeriod := 200 * time.Millisecond
	detector := NewEmergencyDetector(gracePeriod)

	// Initial state: 3 workers
	workers := map[string]bool{
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
	}

	// Pod 1 restarts (disappears briefly)
	workers1Restarting := map[string]bool{
		"worker-2": true,
		"worker-3": true,
	}
	emergency, disappeared, _ := detector.CheckEmergency(workers, workers1Restarting)
	require.False(t, emergency)
	require.Empty(t, disappeared)

	// Wait 50ms (quarter of grace period)
	time.Sleep(50 * time.Millisecond)

	// Pod 1 completes restart
	workersAllUp := map[string]bool{
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
	}
	emergency, disappeared, _ = detector.CheckEmergency(workers, workersAllUp)
	require.False(t, emergency, "Pod 1 restarted within grace period")
	require.Empty(t, disappeared)

	// Pod 2 starts restart
	workers2Restarting := map[string]bool{
		"worker-1": true,
		"worker-3": true,
	}
	emergency, disappeared, _ = detector.CheckEmergency(workers, workers2Restarting)
	require.False(t, emergency)
	require.Empty(t, disappeared)

	// Wait 50ms
	time.Sleep(50 * time.Millisecond)

	// Pod 2 completes restart
	emergency, disappeared, _ = detector.CheckEmergency(workers, workersAllUp)
	require.False(t, emergency, "Pod 2 restarted within grace period")
	require.Empty(t, disappeared)

	// Pod 3 starts restart
	workers3Restarting := map[string]bool{
		"worker-1": true,
		"worker-2": true,
	}
	emergency, disappeared, _ = detector.CheckEmergency(workers, workers3Restarting)
	require.False(t, emergency)
	require.Empty(t, disappeared)

	// Wait 50ms
	time.Sleep(50 * time.Millisecond)

	// Pod 3 completes restart
	emergency, disappeared, _ = detector.CheckEmergency(workers, workersAllUp)
	require.False(t, emergency, "All pods restarted successfully, no emergency")
	require.Empty(t, disappeared)
}

// TestEmergency_MultipleFlappingWorkers verifies that multiple workers flapping
// simultaneously are tracked independently.
//
// Production scenario: Network instability affecting multiple nodes differently.
func TestEmergency_MultipleFlappingWorkers(t *testing.T) {
	t.Parallel()

	gracePeriod := 200 * time.Millisecond
	detector := NewEmergencyDetector(gracePeriod)

	prev := map[string]bool{
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
		"worker-4": true,
	}

	// Workers 2 and 3 disappear
	curr1 := map[string]bool{
		"worker-1": true,
		"worker-4": true,
	}
	emergency, _, _ := detector.CheckEmergency(prev, curr1)
	require.False(t, emergency)

	// Wait 100ms
	time.Sleep(100 * time.Millisecond)

	// Worker-2 reappears, worker-3 still gone
	curr2 := map[string]bool{
		"worker-1": true,
		"worker-2": true,
		"worker-4": true,
	}
	emergency, workers, _ := detector.CheckEmergency(prev, curr2)
	require.False(t, emergency, "Worker-3 still within grace period")
	require.Empty(t, workers)

	// Wait another 120ms (total 220ms for worker-3, 120ms for worker-2's tracking cleared)
	time.Sleep(120 * time.Millisecond)

	// Now worker-3 should be emergency, but worker-2 is fine
	emergency, workers, _ = detector.CheckEmergency(prev, curr2)
	require.True(t, emergency, "Worker-3 exceeded grace period")
	require.Len(t, workers, 1)
	require.Contains(t, workers, "worker-3")
	require.NotContains(t, workers, "worker-2", "Worker-2 reappeared, should not be in emergency list")
}

// TestEmergency_GracePeriodRespected verifies that emergency detection never fires
// before the grace period expires, regardless of how many times we check.
//
// Production scenario: Aggressive monitoring that polls frequently should not cause
// premature emergency detection.
func TestEmergency_GracePeriodRespected(t *testing.T) {
	t.Parallel()

	gracePeriod := 200 * time.Millisecond
	detector := NewEmergencyDetector(gracePeriod)

	prev := map[string]bool{"worker-1": true, "worker-2": true}
	curr := map[string]bool{"worker-1": true}

	// Poll aggressively for 190ms (just under grace period)
	startTime := time.Now()
	for time.Since(startTime) < 190*time.Millisecond {
		emergency, workers, _ := detector.CheckEmergency(prev, curr)
		require.False(t, emergency, "Should never fire before grace period expires")
		require.Empty(t, workers)
		time.Sleep(20 * time.Millisecond) // Poll every 20ms
	}

	// Wait for grace period to definitely expire
	time.Sleep(30 * time.Millisecond) // Total: 220ms

	// Now should be emergency
	emergency, workers, _ := detector.CheckEmergency(prev, curr)
	require.True(t, emergency, "Should fire after grace period expires")
	require.Len(t, workers, 1)
	require.Contains(t, workers, "worker-2")
}

// --- merged from calculator_emergency_priority_test.go ---

// TestCalculator_EmergencyBypassesCooldown verifies that emergency rebalancing
// ignores the rate-limiting cooldown period.
func TestCalculator_EmergencyBypassesCooldown(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-emergency-priority-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-emergency-priority-heartbeat")

	// Setup: 3 workers initially
	workers := []string{"worker-1", "worker-2", "worker-3"}
	for _, w := range workers {
		_, err := heartbeatKV.Put(ctx, "worker-hb."+w, []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)
	}

	source := &mockSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
		},
	}
	strategy := &mockStrategy{}

	// Config with LONG cooldown but SHORT grace period
	cooldown := 5 * time.Second
	gracePeriod := 200 * time.Millisecond

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         200 * time.Millisecond,
		EmergencyGracePeriod: gracePeriod,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             cooldown, // Long cooldown!
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial assignment
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > 0
	}, 2*time.Second, 50*time.Millisecond, "initial assignment failed")

	initialVersion := calc.CurrentVersion()
	t.Logf("Initial version: %d", initialVersion)

	// Trigger Emergency: Delete worker-3
	// This should trigger emergency rebalance after grace period (200ms)
	// ignoring the 5s cooldown.
	err = heartbeatKV.Delete(ctx, "worker-hb.worker-3")
	require.NoError(t, err)

	// Wait for grace period + monitoring cycle
	// Should be much faster than cooldown (5s)
	start := time.Now()
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > initialVersion
	}, 2*time.Second, 50*time.Millisecond, "emergency rebalance failed to bypass cooldown")

	duration := time.Since(start)
	t.Logf("Emergency rebalance took %v (cooldown was %v)", duration, cooldown)
	require.Less(t, duration, cooldown, "rebalance should have happened faster than cooldown")
}

// TestCalculator_PlannedScaleRespectsCooldown verifies that normal scaling
// still respects the rate-limiting cooldown period.
func TestCalculator_PlannedScaleRespectsCooldown(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-planned-cooldown-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-planned-cooldown-heartbeat")

	// Setup: 1 worker initially
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{{Keys: []string{"p1"}}},
	}
	strategy := &mockStrategy{}

	clock := newFakeClock()
	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         2 * time.Second, // workers persist through the short test
		EmergencyGracePeriod: 100 * time.Millisecond,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             5 * time.Second, // fake-clock driven; never elapses in real time
		Now:                  clock.Now,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Cold-start rebalance(s) stamp lastRebalance at the (frozen) clock time.
	// Wait until cold start fully settles for a deterministic baseline.
	initialVersion := waitStableVersion(t, calc)
	t.Logf("initial version (after cold start): %d", initialVersion)

	// Planned scale: add worker-2. The cooldown gate (clock not advanced past
	// the 5s Cooldown) must block it. Because the gate reads the frozen fake
	// clock, real-time scheduling under load cannot let it slip through.
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)
	require.Never(t, func() bool { return calc.CurrentVersion() != initialVersion },
		750*time.Millisecond, 50*time.Millisecond, "planned scale must be blocked while in cooldown")

	// Advance past Cooldown and re-fire the watcher: the planned scale proceeds.
	clock.Advance(5*time.Second + 100*time.Millisecond)
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > initialVersion
	}, 3*time.Second, 50*time.Millisecond, "planned scale should proceed after cooldown")
}

// TestCalculator_EmergencyDuringScaling verifies that emergency rebalancing
// interrupts an active scaling window and doesn't cause duplicate rebalances.
func TestCalculator_EmergencyDuringScaling(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-emergency-scaling-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-emergency-scaling-heartbeat")

	// Setup: 2 workers initially
	workers := []string{"worker-1", "worker-2"}
	for _, w := range workers {
		_, err := heartbeatKV.Put(ctx, "worker-hb."+w, []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)
	}

	source := &mockSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
		},
	}
	strategy := &mockStrategy{}

	// Long scaling window so we can crash a worker during it
	scalingWindow := 2 * time.Second
	gracePeriod := 100 * time.Millisecond

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         200 * time.Millisecond,
		EmergencyGracePeriod: gracePeriod,
		ColdStartWindow:      scalingWindow, // Long window
		PlannedScaleWindow:   scalingWindow,
		Cooldown:             100 * time.Millisecond,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial assignment and scaling state
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > 0 && calc.GetState() == types.CalcStateScaling
	}, 1*time.Second, 50*time.Millisecond, "should be in scaling state")

	versionAfterImmediate := calc.CurrentVersion()
	t.Logf("Version after immediate assignment: %d, state: %s", versionAfterImmediate, calc.GetState())

	// Now crash a worker while in Scaling state
	err = heartbeatKV.Delete(ctx, "worker-hb.worker-2")
	require.NoError(t, err)

	// Wait for emergency rebalance (should happen within grace period + monitoring cycle)
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > versionAfterImmediate
	}, 1*time.Second, 50*time.Millisecond, "emergency should trigger during scaling")

	versionAfterEmergency := calc.CurrentVersion()
	t.Logf("Version after emergency: %d", versionAfterEmergency)

	// Wait a bit longer than the scaling window to ensure no extra rebalance
	// from the orphaned scaling timer
	time.Sleep(scalingWindow + 500*time.Millisecond)

	finalVersion := calc.CurrentVersion()
	t.Logf("Final version after scaling window expired: %d", finalVersion)

	// The version should NOT have increased again from the orphaned timer
	require.Equal(t, versionAfterEmergency, finalVersion,
		"orphaned scaling timer should NOT trigger extra rebalance")
}
