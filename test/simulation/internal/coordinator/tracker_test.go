package coordinator

import (
	"errors"
	"testing"
	"time"
)

// TestMessageTrackerHoleHealing verifies that out-of-order arrival creates a hole
// which is later healed and reported via the returned durations slice.
func TestMessageTrackerHoleHealing(t *testing.T) {
	tracker := NewMessageTracker()

	// Establish initial contiguous sequence 1 (producer sequences start at 1)
	healed, err := tracker.RecordReceived(0, 1)
	if err != nil || len(healed) != 0 {
		t.Fatalf("initial RecordReceived unexpected err=%v healed=%v", err, healed)
	}

	// Jump ahead to sequence 3 creating hole for sequence 2
	healed, err = tracker.RecordReceived(0, 3)
	if err != nil {
		t.Fatalf("unexpected error recording out-of-order sequence 3: %v", err)
	}
	if len(healed) != 0 {
		t.Fatalf("expected no healed durations yet after out-of-order, got %d", len(healed))
	}

	// Small delay to produce measurable lifetime for hole 2
	time.Sleep(10 * time.Millisecond)

	// Now receive missing sequence 2 healing hole
	healed, err = tracker.RecordReceived(0, 2)
	if err != nil {
		t.Fatalf("unexpected error recording missing sequence 2: %v", err)
	}
	if len(healed) != 1 {
		t.Fatalf("expected 1 healed duration, got %d", len(healed))
	}
	if healed[0] <= 0 {
		t.Fatalf("expected positive healed duration, got %v", healed[0])
	}

	if tracker.GetHolesHealedCount() != 1 {
		t.Fatalf("expected healed holes count = 1, got %d", tracker.GetHolesHealedCount())
	}
}

// TestMessageTrackerPhysicalAndGapHealing validates physical received counting and gap healing after escalation.
func TestMessageTrackerPhysicalAndGapHealing(t *testing.T) {
	tracker := NewMessageTracker()

	// Receive initial sequence 1 (producer sequences start at 1)
	_, err := tracker.RecordReceived(1, 1)
	if err != nil {
		t.Fatalf("unexpected error receiving initial seq: %v", err)
	}

	// Jump ahead to 4 creating holes 2,3
	_, err = tracker.RecordReceived(1, 4)
	if err != nil {
		t.Fatalf("unexpected error receiving out-of-order seq 4: %v", err)
	}

	// At this point physicalReceived should be 2 (seq 1 and 4)
	if pr := tracker.GetPhysicalReceivedCount(); pr != 2 {
		t.Fatalf("expected physical received = 2, got %d", pr)
	}

	// Age out holes immediately by using cutoff=now
	escalations := tracker.AgeOut(time.Now())
	if len(escalations) != 2 { // holes 2 and 3 promoted
		t.Fatalf("expected 2 escalations (gaps), got %d", len(escalations))
	}
	// Contiguous window advanced virtually past the holes; physical count unchanged
	if pr := tracker.GetPhysicalReceivedCount(); pr != 2 {
		t.Fatalf("physical received should remain 2 after virtual advancement, got %d", pr)
	}
	// Healed holes count should still be 0 (no physical healing yet)
	if hh := tracker.GetHolesHealedCount(); hh != 0 {
		t.Fatalf("holes healed should be 0, got %d", hh)
	}
	// Gap healed count 0
	if gh := tracker.GetGapsHealedCount(); gh != 0 {
		t.Fatalf("gaps healed should be 0, got %d", gh)
	}

	// Late arrival of sequence 2 (previously escalated gap)
	_, err = tracker.RecordReceived(1, 2)
	if err != nil { // treat gap heal as non-error
		t.Fatalf("unexpected error on gap-healed arrival seq 2: %v", err)
	}
	if gh := tracker.GetGapsHealedCount(); gh != 1 {
		t.Fatalf("expected gaps healed count = 1, got %d", gh)
	}
	if pr := tracker.GetPhysicalReceivedCount(); pr != 3 {
		t.Fatalf("expected physical received = 3 after healing seq 2, got %d", pr)
	}

	// Late arrival of sequence 3
	_, err = tracker.RecordReceived(1, 3)
	if err != nil {
		t.Fatalf("unexpected error on gap-healed arrival seq 3: %v", err)
	}
	if gh := tracker.GetGapsHealedCount(); gh != 2 {
		t.Fatalf("expected gaps healed count = 2, got %d", gh)
	}
	if pr := tracker.GetPhysicalReceivedCount(); pr != 4 {
		t.Fatalf("expected physical received = 4 after healing seq 3, got %d", pr)
	}

	// Ensure holesHealed remains 0 (we did not physically heal holes before escalation)
	if hh := tracker.GetHolesHealedCount(); hh != 0 {
		t.Fatalf("holes healed should remain 0, got %d", hh)
	}
}

// TestMessageTrackerDuplicateDoesNotIncrementPhysical ensures duplicates are classified
// correctly and do not increase the physical received count.
func TestMessageTrackerDuplicateDoesNotIncrementPhysical(t *testing.T) {
	t.Parallel()
	tracker := NewMessageTracker()

	// First receipt increments physical count
	if _, err := tracker.RecordReceived(2, 1); err != nil {
		t.Fatalf("unexpected error on first receipt: %v", err)
	}
	if pr := tracker.GetPhysicalReceivedCount(); pr != 1 {
		t.Fatalf("expected physical received = 1 after first receipt, got %d", pr)
	}

	// Duplicate of seq 1 should return duplicate error and not change physical count
	if _, err := tracker.RecordReceived(2, 1); !errors.Is(err, ErrMessageDuplicate) {
		t.Fatalf("expected ErrMessageDuplicate, got %v", err)
	}
	if pr := tracker.GetPhysicalReceivedCount(); pr != 1 {
		t.Fatalf("duplicate should not increase physical received, got %d", pr)
	}
	if hh := tracker.GetHolesHealedCount(); hh != 0 {
		t.Fatalf("holes healed should remain 0, got %d", hh)
	}
}

// TestSuppressionMarksHeadOnlyAndIdempotent verifies that suppression marking only counts
// consecutive head-of-line aged holes and does not double count across ticks.
func TestSuppressionMarksHeadOnlyAndIdempotent(t *testing.T) {
	t.Parallel()
	tracker := NewMessageTracker()

	// Build a partition with three consecutive head-of-line holes plus one
	// gap-separated hole, by receiving seq=5 alone then seq=7. This yields
	// missing = {1,2,3,4,6}; head-of-line consecutive holes = {1,2,3,4}.
	if _, err := tracker.RecordReceived(3, 5); err != nil {
		t.Fatalf("unexpected error on seq 5: %v", err)
	}
	if _, err := tracker.RecordReceived(3, 7); err != nil {
		t.Fatalf("unexpected error on seq 7: %v", err)
	}

	// Age the holes a bit
	time.Sleep(5 * time.Millisecond)
	cutoff := time.Now()

	// Only the four consecutive head holes (1..4) should be marked; seq 6 is
	// gap-separated by the already-observed seq 5 and must be skipped — that
	// is the "head only" invariant this test guards.
	newly := tracker.MarkHeadOfLineAgedHolesSuppressed(cutoff)
	if newly != 4 {
		t.Fatalf("expected 4 newly suppressed head-of-line holes, got %d", newly)
	}
	// Idempotent on subsequent calls with same state
	if again := tracker.MarkHeadOfLineAgedHolesSuppressed(cutoff); again != 0 {
		t.Fatalf("expected 0 newly suppressed on second pass, got %d", again)
	}

	// Simulate cooldown accounting update
	tracker.IncrementSuppressedHoles(newly)
	if sc := tracker.GetSuppressedHolesCount(); sc != int64(newly) {
		t.Fatalf("expected suppressed holes count = %d, got %d", newly, sc)
	}
}

// TestAgeOutSkipsNotYetAged ensures AgeOut does not escalate holes when cutoff precedes firstSeen.
func TestAgeOutSkipsNotYetAged(t *testing.T) {
	t.Parallel()
	tracker := NewMessageTracker()

	// Create hole: receive 1, then jump to 3 (missing 2)
	if _, err := tracker.RecordReceived(4, 1); err != nil {
		t.Fatalf("unexpected error on seq 1: %v", err)
	}
	if _, err := tracker.RecordReceived(4, 3); err != nil {
		t.Fatalf("unexpected error on seq 3: %v", err)
	}

	// Cutoff far in the past: firstSeen will be after this cutoff, so not aged
	past := time.Now().Add(-time.Hour)
	if escalations := tracker.AgeOut(past); len(escalations) != 0 {
		t.Fatalf("expected 0 escalations for not-yet-aged holes, got %d", len(escalations))
	}
	if pr := tracker.GetPhysicalReceivedCount(); pr != 2 {
		t.Fatalf("physical received should be 2 (seq 1 and 3), got %d", pr)
	}

	// Now with current cutoff, the hole should escalate
	if escalations := tracker.AgeOut(time.Now()); len(escalations) != 1 {
		t.Fatalf("expected 1 escalation with current cutoff, got %d", len(escalations))
	}
}

// TestGapHealThenDuplicate ensures that after a gap is healed, a subsequent repeat is a duplicate.
func TestGapHealThenDuplicate(t *testing.T) {
	t.Parallel()
	tracker := NewMessageTracker()

	// Create two holes and escalate
	if _, err := tracker.RecordReceived(5, 1); err != nil {
		t.Fatalf("unexpected error on seq 1: %v", err)
	}
	if _, err := tracker.RecordReceived(5, 4); err != nil {
		t.Fatalf("unexpected error on seq 4: %v", err)
	}
	if escalations := tracker.AgeOut(time.Now()); len(escalations) != 2 {
		t.Fatalf("expected 2 escalations, got %d", len(escalations))
	}

	// Heal gap for seq 2
	if _, err := tracker.RecordReceived(5, 2); err != nil {
		t.Fatalf("unexpected error healing gap seq 2: %v", err)
	}
	// Sending seq 2 again should be a duplicate
	if _, err := tracker.RecordReceived(5, 2); !errors.Is(err, ErrMessageDuplicate) {
		t.Fatalf("expected ErrMessageDuplicate after heal, got %v", err)
	}
}

// TestDisorderDepthAndPendingAges validates disorder depth and pending hole age reporting.
func TestDisorderDepthAndPendingAges(t *testing.T) {
	t.Parallel()
	tracker := NewMessageTracker()

	// Receive 1 then 5 -> holes {2,3,4}; depth = highWatermark - lastReceived = 5 - 1 = 4
	if _, err := tracker.RecordReceived(6, 1); err != nil {
		t.Fatalf("unexpected error on seq 1: %v", err)
	}
	if _, err := tracker.RecordReceived(6, 5); err != nil {
		t.Fatalf("unexpected error on seq 5: %v", err)
	}
	if depth := tracker.GetDisorderDepth(); depth != 4 {
		t.Fatalf("expected disorder depth 4, got %d", depth)
	}
	// Ages should reflect three pending holes with positive durations
	time.Sleep(5 * time.Millisecond)
	ages := tracker.GetPendingHoleAges()
	if len(ages) != 3 {
		t.Fatalf("expected 3 pending hole ages, got %d", len(ages))
	}
	for i, d := range ages {
		if d <= 0 {
			t.Fatalf("expected positive age for hole %d, got %v", i, d)
		}
	}

	// Heal head-of-line hole 2 -> depth becomes 3; ages now for {3,4}
	if _, err := tracker.RecordReceived(6, 2); err != nil {
		t.Fatalf("unexpected error healing seq 2: %v", err)
	}
	if depth := tracker.GetDisorderDepth(); depth != 3 {
		t.Fatalf("expected disorder depth 3 after healing 2, got %d", depth)
	}
	ages = tracker.GetPendingHoleAges()
	if len(ages) != 2 {
		t.Fatalf("expected 2 pending hole ages after heal, got %d", len(ages))
	}
}

// TestMassiveGap ensures that a massive gap (>10000) skips tracking individual holes.
func TestMassiveGap(t *testing.T) {
	t.Parallel()
	tracker := NewMessageTracker()

	// 1. Receive initial sequence
	if _, err := tracker.RecordReceived(1, 1); err != nil {
		t.Fatalf("unexpected error on seq 1: %v", err)
	}

	// 2. Receive sequence with massive gap (> 10000)
	// This should trigger the "Massive Gap" logic
	if _, err := tracker.RecordReceived(1, 20000); err != nil {
		t.Fatalf("unexpected error on seq 20000: %v", err)
	}

	// 3. Verify state
	// The tracker should have updated lastReceived to 20000
	// But it should NOT have created ~20000 entries in missingPerPartition

	// We can check this by verifying that AgeOut returns 0 escalations immediately,
	// because the massive gap logic skips adding them to the missing map.
	if escalations := tracker.AgeOut(time.Now().Add(1 * time.Hour)); len(escalations) != 0 {
		t.Fatalf("expected 0 escalations for massive gap (should be skipped), got %d", len(escalations))
	}

	// Verify physical received count is 2
	if count := tracker.GetPhysicalReceivedCount(); count != 2 {
		t.Errorf("expected physical received count 2, got %d", count)
	}
}

// TestOutOfOrderFillAdvancesWindow verifies that filling a hole advances the window
// past subsequent already-received messages.
func TestOutOfOrderFillAdvancesWindow(t *testing.T) {
	t.Parallel()
	tracker := NewMessageTracker()

	// 1. Receive 1
	if _, err := tracker.RecordReceived(1, 1); err != nil {
		t.Fatalf("unexpected error on seq 1: %v", err)
	}

	// 2. Receive 3 (creates hole at 2)
	if _, err := tracker.RecordReceived(1, 3); err != nil {
		t.Fatalf("unexpected error on seq 3: %v", err)
	}
	// Verify state: lastReceived=1, missing={2}, highWatermark=3
	if _, lr := tracker.GetPartitionState(1); lr != 1 {
		t.Fatalf("expected lastReceived 1, got %d", lr)
	}

	// 3. Receive 2 (fills hole)
	if _, err := tracker.RecordReceived(1, 2); err != nil {
		t.Fatalf("unexpected error on seq 2: %v", err)
	}

	// 4. Verify state: lastReceived should be 3 (because 3 was already received)
	// Before fix, it would be 2.
	if _, lr := tracker.GetPartitionState(1); lr != 3 {
		t.Fatalf("expected lastReceived 3, got %d", lr)
	}

	// 5. Receive 4
	// Should be normal advance, not a gap
	if _, err := tracker.RecordReceived(1, 4); err != nil {
		t.Fatalf("unexpected error on seq 4: %v", err)
	}
	if _, lr := tracker.GetPartitionState(1); lr != 4 {
		t.Fatalf("expected lastReceived 4, got %d", lr)
	}
}
