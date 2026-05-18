package assignment

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestEmergencyDetector_Hysteresis verifies grace period prevents false positives.
func TestEmergencyDetector_Hysteresis(t *testing.T) {
	t.Parallel()
	gracePeriod := 200 * time.Millisecond
	detector := NewEmergencyDetector(gracePeriod)

	prev := map[string]bool{"worker-1": true, "worker-2": true, "worker-3": true}

	// Worker-2 disappears
	curr := map[string]bool{"worker-1": true, "worker-3": true}

	// Immediate check - should NOT be emergency (grace period not expired)
	emergency, workers, _ := detector.CheckEmergency(prev, curr)
	require.False(t, emergency)
	require.Empty(t, workers)

	// Wait half grace period
	time.Sleep(100 * time.Millisecond)
	emergency, workers, _ = detector.CheckEmergency(prev, curr)
	require.False(t, emergency)
	require.Empty(t, workers)

	// Wait full grace period
	time.Sleep(110 * time.Millisecond) // Total: 210ms
	emergency, workers, _ = detector.CheckEmergency(prev, curr)
	require.True(t, emergency)
	require.Len(t, workers, 1)
	require.Contains(t, workers, "worker-2")
}

// TestEmergencyDetector_WorkerReappears verifies tracking cleared when worker returns.
func TestEmergencyDetector_WorkerReappears(t *testing.T) {
	t.Parallel()
	// Reduce grace period from 2s to 200ms for faster test
	gracePeriod := 200 * time.Millisecond
	detector := NewEmergencyDetector(gracePeriod)

	prev := map[string]bool{"worker-1": true, "worker-2": true}

	// Worker-2 disappears
	curr := map[string]bool{"worker-1": true}
	emergency, _, _ := detector.CheckEmergency(prev, curr)
	require.False(t, emergency)

	// Wait 100ms
	time.Sleep(100 * time.Millisecond)

	// Worker-2 reappears
	currReappeared := map[string]bool{"worker-1": true, "worker-2": true}
	emergency, workers, _ := detector.CheckEmergency(prev, currReappeared)
	require.False(t, emergency)
	require.Empty(t, workers)

	// Wait another 200ms - should still not be emergency (tracking cleared)
	time.Sleep(200 * time.Millisecond)
	emergency, workers, _ = detector.CheckEmergency(prev, currReappeared)
	require.False(t, emergency)
	require.Empty(t, workers)
}

// TestEmergencyDetector_MultipleWorkers verifies tracking multiple disappeared workers.
func TestEmergencyDetector_MultipleWorkers(t *testing.T) {
	t.Parallel()
	gracePeriod := 200 * time.Millisecond
	detector := NewEmergencyDetector(gracePeriod)

	prev := map[string]bool{
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
		"worker-4": true,
	}

	// Two workers disappear
	curr := map[string]bool{"worker-1": true, "worker-2": true}

	// Immediate check - no emergency yet
	emergency, workers, _ := detector.CheckEmergency(prev, curr)
	require.False(t, emergency)
	require.Empty(t, workers)

	// Wait past grace period
	time.Sleep(210 * time.Millisecond)

	emergency, workers, _ = detector.CheckEmergency(prev, curr)
	require.True(t, emergency)
	require.Len(t, workers, 2)
	require.Contains(t, workers, "worker-3")
	require.Contains(t, workers, "worker-4")
}

// TestEmergencyDetector_PartialReappearance verifies partial worker recovery.
func TestEmergencyDetector_PartialReappearance(t *testing.T) {
	t.Parallel()
	gracePeriod := 200 * time.Millisecond
	detector := NewEmergencyDetector(gracePeriod)

	prev := map[string]bool{
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
	}

	// Two workers disappear
	curr := map[string]bool{"worker-1": true}

	// Start tracking
	emergency, _, _ := detector.CheckEmergency(prev, curr)
	require.False(t, emergency)

	// Wait 100ms
	time.Sleep(100 * time.Millisecond)

	// Worker-2 reappears, worker-3 still missing
	currPartial := map[string]bool{"worker-1": true, "worker-2": true}
	emergency, workers, _ := detector.CheckEmergency(prev, currPartial)
	require.False(t, emergency) // worker-3 not past grace period yet
	require.Empty(t, workers)

	// Wait another 120ms (total 220ms for worker-3)
	time.Sleep(120 * time.Millisecond)

	emergency, workers, _ = detector.CheckEmergency(prev, currPartial)
	require.True(t, emergency)
	require.Len(t, workers, 1)
	require.Contains(t, workers, "worker-3")
	require.NotContains(t, workers, "worker-2") // worker-2 tracking was cleared
}

// TestEmergencyDetector_ZeroGracePeriod verifies immediate emergency with zero grace period.
func TestEmergencyDetector_ZeroGracePeriod(t *testing.T) {
	t.Parallel()
	detector := NewEmergencyDetector(0 * time.Second)

	prev := map[string]bool{"worker-1": true, "worker-2": true}
	curr := map[string]bool{"worker-1": true}

	// With zero grace period, should be immediate emergency
	emergency, workers, _ := detector.CheckEmergency(prev, curr)
	require.True(t, emergency)
	require.Len(t, workers, 1)
	require.Contains(t, workers, "worker-2")
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
