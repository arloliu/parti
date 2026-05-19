package coordinator

import (
	"errors"
	"testing"
	"time"
)

// TestRecordReceived_FirstSeqIsOne is a regression test: the steady-state
// happy path (first observation is seq=1) must continue to work.
func TestRecordReceived_FirstSeqIsOne(t *testing.T) {
	tr := NewMessageTracker()
	if _, err := tr.RecordReceived(7, 1); err != nil {
		t.Fatalf("RecordReceived(7, 1): %v", err)
	}
	_, last := tr.GetPartitionState(7)
	if last != 1 {
		t.Errorf("lastReceived = %d, want 1", last)
	}
	if got := tr.GetPhysicalReceivedCount(); got != 1 {
		t.Errorf("physicalReceivedCount = %d, want 1", got)
	}
}

// TestRecordReceived_FirstSeqOutOfOrder covers B1's false-positive case:
// seq=2 arrives before seq=1 at startup. Pre-fix this seeded
// lastReceived=2 and the subsequent seq=1 was misclassified as a
// duplicate. Post-fix the seed is 0, and seq=1 heals the missing entry
// the seq=2 receipt created.
func TestRecordReceived_FirstSeqOutOfOrder(t *testing.T) {
	tr := NewMessageTracker()

	if _, err := tr.RecordReceived(0, 2); err != nil {
		t.Fatalf("RecordReceived(0, 2): %v", err)
	}

	healed, err := tr.RecordReceived(0, 1)
	if err != nil {
		t.Fatalf("RecordReceived(0, 1) must not return error (pre-fix returned duplicate); got %v", err)
	}
	if len(healed) != 1 {
		t.Errorf("healed durations = %d, want 1", len(healed))
	}

	_, last := tr.GetPartitionState(0)
	if last != 2 {
		t.Errorf("lastReceived = %d, want 2", last)
	}
	if got := tr.GetHolesHealedCount(); got != 1 {
		t.Errorf("holesHealedCount = %d, want 1", got)
	}
	st := tr.GetStats()
	if st.DuplicateCount != 0 {
		t.Errorf("DuplicateCount = %d, want 0 (seq=1 must not be classified as duplicate)", st.DuplicateCount)
	}
}

// TestRecordReceived_FirstObservationIsGap covers B1's false-negative
// case plus the round-1 reviewer's phantom-hole regression. Receiving
// seq=3, 4, 5 in order (without 1, 2 yet) must record exactly {1, 2} as
// missing — NOT {1, 2, 3} or {1, 2, 3, 4}. Then receiving 1 and 2 must
// drive lastReceived all the way to 5 via the window-advance loop.
func TestRecordReceived_FirstObservationIsGap(t *testing.T) {
	tr := NewMessageTracker()

	for _, s := range []int64{3, 4, 5} {
		if _, err := tr.RecordReceived(11, s); err != nil {
			t.Fatalf("RecordReceived(11, %d): %v", s, err)
		}
	}

	// After 3, 4, 5: missing set must be exactly {1, 2}. Verify via
	// GetPendingHoles which counts entries in missingPerPartition.
	if got := tr.GetPendingHoles(); got != 2 {
		t.Fatalf("pendingHoles = %d, want 2 (phantom hole bug: range-fill re-added already-seen seqs)", got)
	}
	if got := tr.GetPhysicalReceivedCount(); got != 3 {
		t.Errorf("physicalReceivedCount = %d, want 3", got)
	}

	// Now heal: 1 then 2. Window must advance to 5 because 3, 4, 5 were
	// already observed (high watermark = 5, missing set = empty).
	if _, err := tr.RecordReceived(11, 1); err != nil {
		t.Fatalf("RecordReceived(11, 1): %v", err)
	}
	if _, err := tr.RecordReceived(11, 2); err != nil {
		t.Fatalf("RecordReceived(11, 2): %v", err)
	}

	_, last := tr.GetPartitionState(11)
	if last != 5 {
		t.Errorf("lastReceived = %d, want 5 (window-advance must consume 3,4,5)", last)
	}
	if got := tr.GetHolesHealedCount(); got != 2 {
		t.Errorf("holesHealedCount = %d, want 2", got)
	}
	st := tr.GetStats()
	if st.GapCount != 0 {
		t.Errorf("GapCount = %d, want 0", st.GapCount)
	}
	if st.DuplicateCount != 0 {
		t.Errorf("DuplicateCount = %d, want 0", st.DuplicateCount)
	}
}

// TestRecordReceived_FirstObsGap_AgedOut exercises the phantom-hole
// failure case end-to-end through AgeOut: if seq=3 is wrongly added to
// missing (the reviewer's P0), AgeOut would escalate seqs 1, 2, AND 3 to
// gaps. The fix ensures only the genuinely-missing {1, 2} are escalated.
func TestRecordReceived_FirstObsGap_AgedOut(t *testing.T) {
	tr := NewMessageTracker()

	// First, three out-of-order receipts to set up the phantom-hole risk.
	for _, s := range []int64{3, 4, 5} {
		if _, err := tr.RecordReceived(11, s); err != nil {
			t.Fatalf("RecordReceived(11, %d): %v", s, err)
		}
	}

	// Age everything: cutoff in the future relative to firstSeen timestamps.
	cutoff := time.Now().Add(1 * time.Hour)
	escalations := tr.AgeOut(cutoff)

	// Exactly 2 gap escalations (seqs 1 and 2). If the phantom-hole bug
	// is present, this is 3 (also includes seq 3).
	if len(escalations) != 2 {
		t.Fatalf("AgeOut escalations = %d, want 2 (phantom-hole regression?); details: %v",
			len(escalations), escalations)
	}

	seen := map[int64]bool{}
	for _, e := range escalations {
		var gerr *MessageGapError
		if !errors.As(e, &gerr) {
			t.Fatalf("escalation %v: not a *MessageGapError", e)
		}
		seen[gerr.ExpectedSeq] = true
	}
	if !seen[1] || !seen[2] {
		t.Errorf("expected seqs {1, 2} escalated; got %v", seen)
	}
	if seen[3] {
		t.Error("seq 3 must NOT be escalated (was physically observed)")
	}

	// After AgeOut consumes seqs 1, 2 the window must advance to 5.
	_, last := tr.GetPartitionState(11)
	if last != 5 {
		t.Errorf("after AgeOut: lastReceived = %d, want 5", last)
	}
}
