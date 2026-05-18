package coordinator

import (
	"errors"
	"sync"
	"testing"
)

// Refined cross-worker duplicate classifier with an owner-snapshot
// discriminator. The classifier consults the leader-reported assignment
// at the moment of receipt to distinguish legitimate handoff redelivery
// from a real exclusive-consumption violation. The §3 classification
// table is documented in the source for classifyKnownDuplicateLocked.
// Each row in that table maps to one test below; the tests document
// intent (what classification means under which snapshot state) rather
// than just exercising code paths.

// stubLookup returns a deterministic owner-set + initialized flag for
// the partition under test. Other partitions return empty/uninitialized.
func stubLookup(partition int, owners []string, initialized bool) OwnerLookupFunc {
	return func(pid int) ([]string, bool) {
		if pid != partition {
			return nil, false
		}
		return owners, initialized
	}
}

// Row 1: origWorker == workerID — legitimate redelivery, no snapshot
// consulted. Covered by the existing tracker_ownership_test.go suite
// (TestRecordReceived_SameWorkerDuplicate_IsRedelivery); restated here
// against the new ownerLookup path to confirm row 1 still wins when
// owner lookup IS installed.
func TestClassify_SameWorkerRedelivery_WithOwnerLookup(t *testing.T) {
	tr := NewMessageTracker()
	tr.SetOwnerLookup(stubLookup(7, []string{"worker-A"}, true))

	if _, err := tr.RecordReceivedFromWorker(7, 1, "worker-A"); err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	_, err := tr.RecordReceivedFromWorker(7, 1, "worker-A")
	if !errors.Is(err, ErrMessageRedelivery) {
		t.Fatalf("expected redelivery; got %v", err)
	}
}

// Row 2: nil ownerLookup — legacy fallback preserves prior behavior.
// Any cross-worker duplicate is a violation regardless of snapshot.
func TestClassify_NilLookup_LegacyViolation(t *testing.T) {
	tr := NewMessageTracker()
	// No SetOwnerLookup — explicit nil.

	if _, err := tr.RecordReceivedFromWorker(7, 1, "worker-A"); err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	_, err := tr.RecordReceivedFromWorker(7, 1, "worker-B")
	if !errors.Is(err, ErrMessageOwnershipViolation) {
		t.Fatalf("expected violation; got %v", err)
	}
	var ove *MessageOwnershipViolationError
	if !errors.As(err, &ove) {
		t.Fatalf("err should unwrap to violation; got %T", err)
	}
	if ove.Reason != "legacy_no_lookup" {
		t.Errorf("Reason = %q, want legacy_no_lookup", ove.Reason)
	}
}

// Row 3: ConcurrentOwners — owner snapshot reports >1 worker. The most
// severe class. Hits regardless of orig/receiving membership.
func TestClassify_ConcurrentOwners_CurrentAndThird(t *testing.T) {
	tr := NewMessageTracker()
	// First record from worker-A with snapshot=[worker-A] (single owner).
	tr.SetOwnerLookup(stubLookup(7, []string{"worker-A"}, true))
	if _, err := tr.RecordReceivedFromWorker(7, 1, "worker-A"); err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	// Now flip the snapshot to two concurrent owners not including orig.
	tr.SetOwnerLookup(stubLookup(7, []string{"worker-B", "worker-C"}, true))

	_, err := tr.RecordReceivedFromWorker(7, 1, "worker-B")
	if !errors.Is(err, ErrMessageOwnershipViolation) {
		t.Fatalf("expected violation (concurrent owners); got %v", err)
	}
	var ove *MessageOwnershipViolationError
	if !errors.As(err, &ove) {
		t.Fatalf("err type: %T", err)
	}
	if !ove.ConcurrentOwners {
		t.Errorf("ConcurrentOwners flag should be true; got %+v", ove)
	}
	if ove.Reason != "concurrent_owners" {
		t.Errorf("Reason = %q, want concurrent_owners", ove.Reason)
	}
	if got := tr.GetConcurrentOwnersViolationCount(); got != 1 {
		t.Errorf("ConcurrentOwnersViolationCount = %d, want 1", got)
	}
}

// Row 3 cardinality-3 variant — both orig and receiver are in the
// snapshot along with a third worker. Still violation, still concurrent.
func TestClassify_ConcurrentOwners_AllThree(t *testing.T) {
	tr := NewMessageTracker()
	tr.SetOwnerLookup(stubLookup(7, []string{"worker-A"}, true))
	if _, err := tr.RecordReceivedFromWorker(7, 1, "worker-A"); err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	tr.SetOwnerLookup(stubLookup(7, []string{"worker-A", "worker-B", "worker-C"}, true))

	_, err := tr.RecordReceivedFromWorker(7, 1, "worker-B")
	var ove *MessageOwnershipViolationError
	if !errors.As(err, &ove) {
		t.Fatalf("err type: %T", err)
	}
	if !ove.ConcurrentOwners {
		t.Errorf("ConcurrentOwners should be true with 3-way snapshot; got %+v", ove)
	}
}

// Row 4: handoff redelivery — snapshot reports the new owner only.
// This is the scenario the prior classifier flagged as a false positive.
func TestClassify_HandoffRedelivery_NewOwnerOnly(t *testing.T) {
	tr := NewMessageTracker()
	tr.SetOwnerLookup(stubLookup(7, []string{"worker-A"}, true))
	if _, err := tr.RecordReceivedFromWorker(7, 1, "worker-A"); err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	// Reassignment: partition handed off to worker-B; worker-B is now sole owner.
	tr.SetOwnerLookup(stubLookup(7, []string{"worker-B"}, true))

	_, err := tr.RecordReceivedFromWorker(7, 1, "worker-B")
	if !errors.Is(err, ErrMessageRedelivery) {
		t.Fatalf("expected redelivery (handoff); got %v", err)
	}
	if got := tr.GetOwnershipViolationCount(); got != 0 {
		t.Errorf("ViolationCount = %d, want 0 (handoff should not be a violation)", got)
	}
}

// Row 5: stale receipt — original worker is still listed as sole owner;
// the receiving worker is processing a partition it shouldn't be.
func TestClassify_StaleReceipt_OriginalStillOwner(t *testing.T) {
	tr := NewMessageTracker()
	tr.SetOwnerLookup(stubLookup(7, []string{"worker-A"}, true))
	if _, err := tr.RecordReceivedFromWorker(7, 1, "worker-A"); err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	// Snapshot still says worker-A owns it; worker-B has no business
	// processing this seq.
	_, err := tr.RecordReceivedFromWorker(7, 1, "worker-B")
	if !errors.Is(err, ErrMessageOwnershipViolation) {
		t.Fatalf("expected violation (stale receipt); got %v", err)
	}
	var ove *MessageOwnershipViolationError
	if !errors.As(err, &ove) {
		t.Fatalf("err type: %T", err)
	}
	if ove.Reason != "stale_receipt" {
		t.Errorf("Reason = %q, want stale_receipt", ove.Reason)
	}
	if ove.ConcurrentOwners {
		t.Error("ConcurrentOwners should be false on stale-receipt; got true")
	}
}

// Row 6: stranger receiver — neither worker is in the snapshot. Some
// third worker owns the partition; both orig and receiver are wrong.
// Real violation.
func TestClassify_StrangerReceiver_NeitherAssigned(t *testing.T) {
	tr := NewMessageTracker()
	tr.SetOwnerLookup(stubLookup(7, []string{"worker-A"}, true))
	if _, err := tr.RecordReceivedFromWorker(7, 1, "worker-A"); err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	// Snapshot now says worker-C owns partition 7; worker-A is gone and
	// worker-B has no business processing.
	tr.SetOwnerLookup(stubLookup(7, []string{"worker-C"}, true))
	_, err := tr.RecordReceivedFromWorker(7, 1, "worker-B")
	if !errors.Is(err, ErrMessageOwnershipViolation) {
		t.Fatalf("expected violation (stranger); got %v", err)
	}
	var ove *MessageOwnershipViolationError
	if !errors.As(err, &ove) {
		t.Fatalf("err type: %T", err)
	}
	if ove.Reason != "stranger_receiver" {
		t.Errorf("Reason = %q, want stranger_receiver", ove.Reason)
	}
}

// Row 7: OwnershipInconclusive — snapshot initialized but empty for
// this partition (mid-handoff window). Blocks Outcome A but isn't a
// hard violation.
func TestClassify_OwnershipInconclusive_SnapshotInitialized(t *testing.T) {
	tr := NewMessageTracker()
	// First seed an initialized snapshot via a real receipt.
	tr.SetOwnerLookup(stubLookup(7, []string{"worker-A"}, true))
	if _, err := tr.RecordReceivedFromWorker(7, 1, "worker-A"); err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	// Now flip to an empty-but-initialized snapshot (chaos churn, prior
	// owner pruned, new owner not yet reported).
	tr.SetOwnerLookup(stubLookup(7, []string{}, true))

	_, err := tr.RecordReceivedFromWorker(7, 1, "worker-B")
	if !errors.Is(err, ErrMessageOwnershipInconclusive) {
		t.Fatalf("expected inconclusive; got %v (kind %T)", err, err)
	}
	if got := tr.GetOwnershipInconclusiveCount(); got != 1 {
		t.Errorf("InconclusiveCount = %d, want 1", got)
	}
	if got := tr.GetOwnershipViolationCount(); got != 0 {
		t.Errorf("ViolationCount = %d, want 0 (inconclusive should not count as violation)", got)
	}
}

// Row 8 (pre-chaos): OwnershipUnobserved — snapshot has never been
// initialized. Pre-chaos bucket.
func TestClassify_OwnershipUnobserved_PreChaos(t *testing.T) {
	tr := NewMessageTracker()
	tr.SetOwnerLookup(stubLookup(7, nil, false)) // uninitialized

	// Record first receipt with snapshot uninitialized — recorded normally.
	if _, err := tr.RecordReceivedFromWorker(7, 1, "worker-A"); err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	_, err := tr.RecordReceivedFromWorker(7, 1, "worker-B")
	if !errors.Is(err, ErrMessageOwnershipUnobserved) {
		t.Fatalf("expected unobserved; got %v", err)
	}
	pre, post := tr.GetOwnershipUnobservedCounts()
	if pre != 1 || post != 0 {
		t.Errorf("counts = (pre=%d, post=%d), want (1, 0) — chaos not started", pre, post)
	}
}

// Row 8 (post-chaos): OwnershipUnobserved after MarkChaosStarted.
// Must route to the post-chaos counter, which blocks Outcome A.
func TestClassify_OwnershipUnobserved_PostChaos(t *testing.T) {
	tr := NewMessageTracker()
	tr.SetOwnerLookup(stubLookup(7, nil, false))

	if _, err := tr.RecordReceivedFromWorker(7, 1, "worker-A"); err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	tr.MarkChaosStarted()
	_, err := tr.RecordReceivedFromWorker(7, 1, "worker-B")
	if !errors.Is(err, ErrMessageOwnershipUnobserved) {
		t.Fatalf("expected unobserved; got %v", err)
	}
	pre, post := tr.GetOwnershipUnobservedCounts()
	if pre != 0 || post != 1 {
		t.Errorf("counts = (pre=%d, post=%d), want (0, 1) — chaos started", pre, post)
	}
}

// Reproduce a historical chaos finding (partition=19 seq=153 from
// worker-5, then worker-0) with a mocked snapshot showing worker-0 as
// the new sole owner. Under the refined classifier this is legitimate
// handoff redelivery, not a violation — JetStream's at-least-once
// semantics guarantee a new partition owner will see un-acked
// sequences from the prior owner after reassignment.
func TestClassify_HistoricalHandoffScenario_IsRedelivery(t *testing.T) {
	tr := NewMessageTracker()
	tr.SetOwnerLookup(stubLookup(19, []string{"worker-5"}, true))
	if _, err := tr.RecordReceivedFromWorker(19, 153, "worker-5"); err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	// Handoff to worker-0 after worker_restart + network_disconnect_leader.
	tr.SetOwnerLookup(stubLookup(19, []string{"worker-0"}, true))

	_, err := tr.RecordReceivedFromWorker(19, 153, "worker-0")
	if !errors.Is(err, ErrMessageRedelivery) {
		t.Fatalf("expected handoff redelivery; got %v", err)
	}
	if got := tr.GetOwnershipViolationCount(); got != 0 {
		t.Fatalf("ViolationCount = %d, want 0 — partition=19 seq=153 was a classifier false positive", got)
	}
}

// CurrentOwnersOf returns ([], false) when no AssignmentReport has been
// consumed. The classifier must route empty-uninitialized to row 8
// (Unobserved), not row 4 (Redelivery).
func TestCurrentOwnersOf_BeforeAnyAssignment(t *testing.T) {
	c := NewCoordinator(10, nil, DupTraceSettings{}, false, "")
	defer c.closeStopCh()
	owners, init := c.CurrentOwnersOf(7)
	if init {
		t.Error("snapshotInitialized should be false before any AssignmentReport")
	}
	if len(owners) != 0 {
		t.Errorf("owners = %v, want empty", owners)
	}
}

// After an AssignmentReport is processed, CurrentOwnersOf must reflect
// the published snapshot. This verifies rebuildOwnerSnapshotLocked
// publishes to the atomic.Pointer on the assignment path.
func TestCurrentOwnersOf_AfterFirstAssignment(t *testing.T) {
	c := NewCoordinator(10, nil, DupTraceSettings{}, false, "")
	defer c.closeStopCh()

	// Simulate ingestion by directly writing workerAssignments and
	// invoking rebuild — equivalent to processAssignments' inner work.
	c.workerAssignments["worker-0"] = map[int]struct{}{7: {}, 8: {}}
	c.rebuildOwnerSnapshotLocked()

	owners, init := c.CurrentOwnersOf(7)
	if !init {
		t.Error("snapshotInitialized should be true after rebuild")
	}
	if len(owners) != 1 || owners[0] != "worker-0" {
		t.Errorf("owners = %v, want [worker-0]", owners)
	}
	// Partition with no owner returns empty + initialized=true.
	owners2, init2 := c.CurrentOwnersOf(99)
	if !init2 {
		t.Error("snapshotInitialized should remain true for other partitions")
	}
	if len(owners2) != 0 {
		t.Errorf("partition 99 owners = %v, want empty", owners2)
	}
}

// Race-detector regression test: confirms the atomic.Pointer design
// closes the data race that codex P0-3 flagged. Concurrent rebuilds
// (publisher) and CurrentOwnersOf reads (consumer) under -race must
// not trip Go's race detector or cause "concurrent map iteration".
func TestCurrentOwnersOf_ConcurrentIngestion(t *testing.T) {
	c := NewCoordinator(50, nil, DupTraceSettings{}, false, "")
	defer c.closeStopCh()

	var wg sync.WaitGroup
	const iterations = 500

	// Writer: simulate AssignmentReport ingestion (single goroutine,
	// matching the real processAssignments contract).
	wg.Go(func() {
		for i := range iterations {
			c.workerAssignments["worker-A"] = map[int]struct{}{i % 50: {}}
			c.rebuildOwnerSnapshotLocked()
		}
	})

	// Reader: simulate the receipt path repeatedly calling the lookup.
	wg.Go(func() {
		for i := range iterations {
			_, _ = c.CurrentOwnersOf(i % 50)
		}
	})

	wg.Wait()
}

// MarkChaosStarted must be idempotent and capture the timestamp only
// once. Concurrent calls must not double-set.
func TestMarkChaosStarted_IdempotentAndAtomic(t *testing.T) {
	c := NewCoordinator(10, nil, DupTraceSettings{}, false, "")
	defer c.closeStopCh()

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			c.MarkChaosStarted()
		})
	}
	wg.Wait()

	first := c.FirstChaosEventAt()
	if first.IsZero() {
		t.Fatal("FirstChaosEventAt should be set after MarkChaosStarted")
	}
	c.MarkChaosStarted()
	second := c.FirstChaosEventAt()
	if !first.Equal(second) {
		t.Errorf("FirstChaosEventAt changed across calls: first=%v second=%v", first, second)
	}
}

// The shutdown-invariant path calls coord.TriggerFailure during
// ctx.Done shutdown, which closes stopCh; Coordinator.Start(ctx) also
// closes stopCh on its own ctx.Done branch. Without the stopOnce
// guard, the second close panics with "close of closed channel".
// This test exercises both close paths against the same coordinator
// instance — must not panic.
func TestCoordinator_StopChDoubleClose_IsIdempotent(t *testing.T) {
	c := NewCoordinator(10, nil, DupTraceSettings{}, false, "")

	// First close (e.g., normal Start(ctx) shutdown).
	c.closeStopCh()
	// Second close from the TriggerFailure path — must not panic.
	c.TriggerFailure("Stability invariants failed at shutdown", errors.New("test"))
	// Third direct close attempt — must not panic.
	c.closeStopCh()

	// Channel must be closed (receive returns immediately).
	select {
	case <-c.stopCh:
	default:
		t.Fatal("stopCh should be closed after closeStopCh")
	}
}

// Codex post-impl review P1: an empty AssignmentReport (worker reports
// zero partitions, e.g., immediately after a graceful drain) must flip
// snapshotInitialized to true. Pre-fix this branch returned
// initialized=false because rebuild only flipped on non-empty
// assignments, masking real inconclusive duplicates as "unobserved".
func TestCurrentOwnersOf_EmptyAssignmentInitializesSnapshot(t *testing.T) {
	c := NewCoordinator(10, nil, DupTraceSettings{}, false, "")
	defer c.closeStopCh()

	// Worker reports an empty partition set — equivalent to processing
	// an AssignmentReport{WorkerID: "worker-X", Partitions: nil}.
	c.workerAssignments["worker-A"] = map[int]struct{}{}
	c.rebuildOwnerSnapshotLocked()

	owners, init := c.CurrentOwnersOf(7)
	if !init {
		t.Error("snapshotInitialized should be true after any AssignmentReport, even empty")
	}
	if len(owners) != 0 {
		t.Errorf("owners = %v, want empty", owners)
	}
}

// CurrentOwnersOf returned slices are immutable shares of the snapshot.
// Verify the documented contract: callers who mutate the returned
// slice would corrupt the snapshot — so callers must not mutate. The
// test asserts the returned slice is what the snapshot stored, not a
// per-call copy (this is a deliberate design choice for cheapness).
func TestCurrentOwnersOf_ReturnedSliceIsSnapshotShare(t *testing.T) {
	c := NewCoordinator(10, nil, DupTraceSettings{}, false, "")
	defer c.closeStopCh()

	c.workerAssignments["worker-A"] = map[int]struct{}{7: {}}
	c.workerAssignments["worker-B"] = map[int]struct{}{7: {}}
	c.rebuildOwnerSnapshotLocked()

	owners1, _ := c.CurrentOwnersOf(7)
	owners2, _ := c.CurrentOwnersOf(7)
	if len(owners1) != 2 || len(owners2) != 2 {
		t.Fatalf("expected 2 owners; got %v / %v", owners1, owners2)
	}
	// Both reads should return the same slice header (no per-call copy).
	if &owners1[0] != &owners2[0] {
		t.Error("expected reads of the same snapshot to share storage")
	}
}
