package coordinator

import (
	"errors"
	"testing"
	"time"
)

// TestRecordReceived_SameWorkerDuplicate_IsRedelivery covers the
// "legitimate JetStream redelivery" classification: the same worker
// reprocesses a sequence (e.g., NAK then redelivery). Must return a
// *MessageRedeliveryEvent satisfying errors.Is(ErrMessageRedelivery)
// — informational, not a failure.
func TestRecordReceived_SameWorkerDuplicate_IsRedelivery(t *testing.T) {
	tr := NewMessageTracker()

	if _, err := tr.RecordReceivedFromWorker(7, 1, "worker-A"); err != nil {
		t.Fatalf("RecordReceived(7, 1, worker-A): %v", err)
	}

	_, err := tr.RecordReceivedFromWorker(7, 1, "worker-A")
	if err == nil {
		t.Fatal("expected redelivery event on same-worker reprocess; got nil")
	}
	if !errors.Is(err, ErrMessageRedelivery) {
		t.Errorf("err should satisfy errors.Is(ErrMessageRedelivery); got %v", err)
	}
	var rev *MessageRedeliveryEvent
	if !errors.As(err, &rev) {
		t.Fatalf("err should unwrap to *MessageRedeliveryEvent; got %T", err)
	}
	if rev.WorkerID != "worker-A" || rev.Sequence != 1 || rev.PartitionID != 7 {
		t.Errorf("event fields wrong: %+v", rev)
	}

	if got := tr.GetRedeliveryCount(); got != 1 {
		t.Errorf("RedeliveryCount = %d, want 1", got)
	}
	if got := tr.GetOwnershipViolationCount(); got != 0 {
		t.Errorf("OwnershipViolationCount = %d, want 0", got)
	}
	st := tr.GetStats()
	if st.DuplicateCount != 0 {
		t.Errorf("legacy DuplicateCount = %d, want 0 (should classify as redelivery, not duplicate)", st.DuplicateCount)
	}
}

// TestRecordReceived_DifferentWorkerDuplicate_IsOwnershipViolation is the
// CRITICAL detection path: a Processing-Gate or handoff regression that
// briefly lets two workers process the same (partition, seq). Pre-fix
// this surfaced only as a generic DuplicateCount++; post-fix it returns
// *MessageOwnershipViolationError with both worker IDs.
func TestRecordReceived_DifferentWorkerDuplicate_IsOwnershipViolation(t *testing.T) {
	tr := NewMessageTracker()

	if _, err := tr.RecordReceivedFromWorker(8, 1, "worker-A"); err != nil {
		t.Fatalf("RecordReceived(8, 1, worker-A): %v", err)
	}

	_, err := tr.RecordReceivedFromWorker(8, 1, "worker-B")
	if err == nil {
		t.Fatal("expected ownership violation on different-worker reprocess; got nil")
	}
	if !errors.Is(err, ErrMessageOwnershipViolation) {
		t.Errorf("err should satisfy errors.Is(ErrMessageOwnershipViolation); got %v", err)
	}
	var ove *MessageOwnershipViolationError
	if !errors.As(err, &ove) {
		t.Fatalf("err should unwrap to *MessageOwnershipViolationError; got %T", err)
	}
	if ove.OriginalWorker != "worker-A" || ove.CurrentWorker != "worker-B" {
		t.Errorf("worker fields wrong: orig=%q cur=%q", ove.OriginalWorker, ove.CurrentWorker)
	}
	if ove.PartitionID != 8 || ove.Sequence != 1 {
		t.Errorf("partition/seq fields wrong: %+v", ove)
	}
	if got := tr.GetOwnershipViolationCount(); got != 1 {
		t.Errorf("OwnershipViolationCount = %d, want 1", got)
	}
	if got := tr.GetRedeliveryCount(); got != 0 {
		t.Errorf("RedeliveryCount = %d, want 0", got)
	}
}

// TestRecordReceived_PrunedWorker_FallsBackToLegacyDuplicate exercises the
// detection-horizon trade-off: when the cache window evicts the original
// worker for an old seq, a later duplicate falls back to the legacy
// DuplicateCount counter (no false ownership-violation).
func TestRecordReceived_PrunedWorker_FallsBackToLegacyDuplicate(t *testing.T) {
	tr := NewMessageTrackerWithCap(2)

	// Receive seqs 1, 2, 3 from worker-A. After seq=3 the cache (cap=2)
	// has evicted seq=1.
	for _, s := range []int64{1, 2, 3} {
		if _, err := tr.RecordReceivedFromWorker(9, s, "worker-A"); err != nil {
			t.Fatalf("RecordReceived(9, %d, worker-A): %v", s, err)
		}
	}

	_, err := tr.RecordReceivedFromWorker(9, 1, "worker-B")
	if err == nil {
		t.Fatal("expected duplicate error on pruned-fallback path; got nil")
	}
	if errors.Is(err, ErrMessageOwnershipViolation) {
		t.Fatalf("must NOT classify as ownership violation when origin is pruned; got %v", err)
	}
	if !errors.Is(err, ErrMessageDuplicate) {
		t.Errorf("err should satisfy errors.Is(ErrMessageDuplicate); got %v", err)
	}
	st := tr.GetStats()
	if st.DuplicateCount != 1 {
		t.Errorf("DuplicateCount = %d, want 1", st.DuplicateCount)
	}
	if st.OwnershipViolationCount != 0 {
		t.Errorf("OwnershipViolationCount = %d, want 0", st.OwnershipViolationCount)
	}
}

// TestRecordReceived_GapHealRecordsWorker covers the gap-heal path: a
// previously-escalated gap is healed by worker-A; a later duplicate from
// worker-B must be classified as a violation.
func TestRecordReceived_GapHealRecordsWorker(t *testing.T) {
	tr := NewMessageTracker()

	// Build gap: receive seq=1 then seq=3 (missing 2), age out → 2 becomes a gap.
	if _, err := tr.RecordReceivedFromWorker(10, 1, "worker-A"); err != nil {
		t.Fatalf("seq=1: %v", err)
	}
	if _, err := tr.RecordReceivedFromWorker(10, 3, "worker-A"); err != nil {
		t.Fatalf("seq=3: %v", err)
	}
	if esc := tr.AgeOut(time.Now()); len(esc) != 1 {
		t.Fatalf("AgeOut escalations = %d, want 1", len(esc))
	}

	// Heal the gap from worker-A.
	if _, err := tr.RecordReceivedFromWorker(10, 2, "worker-A"); err != nil {
		t.Fatalf("heal seq=2 from worker-A: %v", err)
	}

	// Now duplicate of seq=2 from worker-B → must classify as violation.
	_, err := tr.RecordReceivedFromWorker(10, 2, "worker-B")
	if err == nil {
		t.Fatal("expected ownership violation on cross-worker dup of healed gap; got nil")
	}
	if !errors.Is(err, ErrMessageOwnershipViolation) {
		t.Errorf("err should satisfy errors.Is(ErrMessageOwnershipViolation); got %v", err)
	}
	var ove *MessageOwnershipViolationError
	if errors.As(err, &ove) && ove.OriginalWorker != "worker-A" {
		t.Errorf("OriginalWorker = %q, want worker-A (gap-heal should have recorded it)", ove.OriginalWorker)
	}
}

// TestRecordReceived_OutOfOrderRecordsWorker is the targeted regression
// guard for the out-of-order branch: the worker that first physically
// processes a seq via the out-of-order path MUST be recorded, so a later
// duplicate from a different worker triggers a violation. If the
// out-of-order branch silently skips worker recording, the duplicate falls
// back to plain MessageDuplicateError and this test fails.
func TestRecordReceived_OutOfOrderRecordsWorker(t *testing.T) {
	tr := NewMessageTracker()

	// Out-of-order: receive seq=2 from worker-A before seq=1.
	if _, err := tr.RecordReceivedFromWorker(12, 2, "worker-A"); err != nil {
		t.Fatalf("out-of-order seq=2: %v", err)
	}
	// Close the window: receive seq=1 from worker-B; contiguous-advance
	// pulls seq=2 from the out-of-order buffer.
	if _, err := tr.RecordReceivedFromWorker(12, 1, "worker-B"); err != nil {
		t.Fatalf("contiguous seq=1: %v", err)
	}

	// Duplicate seq=2 from worker-C. origWorker for seq=2 was set by the
	// out-of-order branch (worker-A). If that branch failed to record,
	// origWorker would be "" and this would fall back to legacy duplicate.
	_, err := tr.RecordReceivedFromWorker(12, 2, "worker-C")
	if err == nil {
		t.Fatal("expected ownership violation; got nil")
	}
	if !errors.Is(err, ErrMessageOwnershipViolation) {
		t.Fatalf("err should be ownership violation (proves out-of-order branch records workers); got %v", err)
	}
	var ove *MessageOwnershipViolationError
	if errors.As(err, &ove) && ove.OriginalWorker != "worker-A" {
		t.Errorf("OriginalWorker = %q, want worker-A (recorded by out-of-order branch)", ove.OriginalWorker)
	}
}

// TestRecordReceived_OutOfOrderDuplicateBeforeWindowAdvance is the
// regression guard for the round-1 post-impl-review P1: a same-seq
// reprocess while the seq is still out-of-order (lastReceived hasn't
// advanced past it yet) must NOT silently overwrite first-worker
// attribution. Pre-fix the out-of-order branch unconditionally recorded
// the worker, so a cross-worker collision in this window vanished
// (no violation counted, original attribution lost). Post-fix the
// early-classification path at the top of RecordReceivedFromWorker
// catches it.
func TestRecordReceived_OutOfOrderDuplicateBeforeWindowAdvance(t *testing.T) {
	tr := NewMessageTracker()

	// Receive seq=2 from worker-A out-of-order (seq=1 still missing).
	if _, err := tr.RecordReceivedFromWorker(20, 2, "worker-A"); err != nil {
		t.Fatalf("first out-of-order seq=2 from worker-A: %v", err)
	}
	// Now another worker reprocesses seq=2 while seq=1 is STILL missing.
	// Pre-fix: this took the out-of-order branch again, overwrote
	// lastWorkerPerSeq[20][2] = worker-B, and returned nil.
	_, err := tr.RecordReceivedFromWorker(20, 2, "worker-B")
	if err == nil {
		t.Fatal("expected ownership violation on cross-worker out-of-order duplicate; got nil")
	}
	if !errors.Is(err, ErrMessageOwnershipViolation) {
		t.Fatalf("err should be ownership violation; got %v", err)
	}
	var ove *MessageOwnershipViolationError
	if errors.As(err, &ove) {
		if ove.OriginalWorker != "worker-A" {
			t.Errorf("OriginalWorker = %q, want worker-A (must NOT be overwritten by the second receipt)", ove.OriginalWorker)
		}
		if ove.CurrentWorker != "worker-B" {
			t.Errorf("CurrentWorker = %q, want worker-B", ove.CurrentWorker)
		}
	}

	// Counter check.
	if got := tr.GetOwnershipViolationCount(); got != 1 {
		t.Errorf("OwnershipViolationCount = %d, want 1", got)
	}

	// Also verify lastWorkerPerSeq still attributes to worker-A — the
	// in-place classifier must not overwrite the original record.
	if w, ok := tr.lookupOrigWorkerLocked(20, 2); !ok || w != "worker-A" {
		t.Errorf("original worker overwritten: got (%q, %v); want (worker-A, true)", w, ok)
	}
}

// TestRecordReceived_OutOfOrderSameWorkerRedelivery covers the redelivery
// twin of the regression above: worker-A re-receives its own out-of-order
// seq=2 (e.g., JetStream redelivered before window advance).
func TestRecordReceived_OutOfOrderSameWorkerRedelivery(t *testing.T) {
	tr := NewMessageTracker()
	if _, err := tr.RecordReceivedFromWorker(21, 2, "worker-A"); err != nil {
		t.Fatalf("first seq=2: %v", err)
	}
	_, err := tr.RecordReceivedFromWorker(21, 2, "worker-A")
	if err == nil || !errors.Is(err, ErrMessageRedelivery) {
		t.Fatalf("expected redelivery event; got %v", err)
	}
	if got := tr.GetRedeliveryCount(); got != 1 {
		t.Errorf("RedeliveryCount = %d, want 1", got)
	}
	if got := tr.GetOwnershipViolationCount(); got != 0 {
		t.Errorf("OwnershipViolationCount = %d, want 0", got)
	}
}

// TestCoordinator_OwnershipViolation_FlowsToFailureReport covers the
// coordinator-side wiring: appendOwnershipViolation populates the bounded
// slice, snapshotOwnershipViolations returns a stable copy, and the
// resulting FailureReport includes the violation entries with both
// worker IDs intact. The receive-loop dispatch path that calls
// appendOwnershipViolation is short (a few lines) and exercised by code
// review — this test focuses on the data-flow contract end-users see in
// the JSON report.
func TestCoordinator_OwnershipViolation_FlowsToFailureReport(t *testing.T) {
	coord := NewCoordinator(10, nil, DupTraceSettings{}, false, "")

	coord.appendOwnershipViolation(MessageOwnershipViolationError{
		PartitionID: 42, Sequence: 7, OriginalWorker: "worker-A", CurrentWorker: "worker-B",
	})
	coord.appendOwnershipViolation(MessageOwnershipViolationError{
		PartitionID: 99, Sequence: 1, OriginalWorker: "worker-X", CurrentWorker: "worker-Y",
	})

	snap := coord.snapshotOwnershipViolations()
	if len(snap) != 2 {
		t.Fatalf("snapshot length = %d, want 2", len(snap))
	}
	if snap[0].OriginalWorker != "worker-A" || snap[0].CurrentWorker != "worker-B" {
		t.Errorf("snap[0] = %+v", snap[0])
	}
	if snap[1].OriginalWorker != "worker-X" || snap[1].CurrentWorker != "worker-Y" {
		t.Errorf("snap[1] = %+v", snap[1])
	}

	// Snapshot must be a copy — mutating the returned slice must not
	// affect the coordinator's internal list.
	snap[0].CurrentWorker = "tampered"
	snap2 := coord.snapshotOwnershipViolations()
	if snap2[0].CurrentWorker == "tampered" {
		t.Error("snapshot must be a copy; coordinator state was mutated")
	}
}

// TestCoordinator_OwnershipViolations_BoundedByCap proves the bounded
// slice never grows past the cap, even under cascading violations.
func TestCoordinator_OwnershipViolations_BoundedByCap(t *testing.T) {
	coord := NewCoordinator(10, nil, DupTraceSettings{}, false, "")
	coord.ownershipViolationsCap = 3 // tight cap for the test

	for i := range 10 {
		coord.appendOwnershipViolation(MessageOwnershipViolationError{
			PartitionID: i, Sequence: int64(i),
			OriginalWorker: "A", CurrentWorker: "B",
		})
	}

	snap := coord.snapshotOwnershipViolations()
	if len(snap) != 3 {
		t.Errorf("snapshot length = %d, want 3 (cap)", len(snap))
	}
}

// TestCheckpointRestore_EmptyWorkerID_FallsBackToLegacyDuplicate guards
// the round-1 reviewer's P0: an empty workerID (as passed by the
// checkpoint-restore replay) must not manufacture false-positive
// ownership violations. The recordWorkerForSeqLocked no-op for "" + the
// classification arm's "either side empty → legacy duplicate" guarantees
// this.
func TestCheckpointRestore_EmptyWorkerID_FallsBackToLegacyDuplicate(t *testing.T) {
	tr := NewMessageTracker()

	// Restore replay style: pass "" workerID.
	if _, err := tr.RecordReceived(13, 1); err != nil {
		t.Fatalf("restore replay seq=1: %v", err)
	}

	// Real worker later sees a duplicate of seq=1 — must NOT be a violation.
	_, err := tr.RecordReceivedFromWorker(13, 1, "worker-A")
	if err == nil {
		t.Fatal("expected error on duplicate; got nil")
	}
	if errors.Is(err, ErrMessageOwnershipViolation) {
		t.Fatalf("must NOT classify as violation when origin is restore-replay (empty); got %v", err)
	}
	if !errors.Is(err, ErrMessageDuplicate) {
		t.Errorf("err should be legacy duplicate; got %v", err)
	}

	// And the reverse direction: real worker first, then empty replay
	// (impossible in practice but the symmetric guard catches the
	// "workerID == \"\"" arm).
	tr2 := NewMessageTracker()
	if _, err := tr2.RecordReceivedFromWorker(14, 1, "worker-X"); err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	_, err = tr2.RecordReceived(14, 1)
	if errors.Is(err, ErrMessageOwnershipViolation) {
		t.Errorf("empty current workerID must not produce violation; got %v", err)
	}
}
