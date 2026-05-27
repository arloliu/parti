package coordinator

import (
	"context"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Snapshot-overlap classifier (Gap 11)
// ---------------------------------------------------------------------------

// TestSnapshotOverlapClassifier_FiresAfterGraceWindow verifies that the
// snapshot-overlap classifier increments snapshotOverlapCount when two
// workers' latest AssignmentReports intersect on a partition and the
// overlap persists longer than the graceWindow.
//
// TDD: this test is written BEFORE the classifier is implemented; it must
// initially fail because NewSnapshotOverlapClassifier is undefined.
func TestSnapshotOverlapClassifier_FiresAfterGraceWindow(t *testing.T) {
	t.Parallel()

	grace := 20 * time.Millisecond
	c := NewSnapshotOverlapClassifier(grace, nil)

	// Ingest conflicting assignment snapshots: workers A and B both own partition 0.
	c.IngestAssignment("worker-A", []int{0, 1})
	c.IngestAssignment("worker-B", []int{0, 2}) // overlap on partition 0

	// First Check() stamps firstSeen. No violation yet.
	c.Check(time.Now())
	if v := c.ViolationCount(); v != 0 {
		t.Errorf("expected 0 violations on first check, got %d", v)
	}

	// After grace window: second Check() fires the violation.
	time.Sleep(grace + 5*time.Millisecond)
	c.Check(time.Now())

	if v := c.ViolationCount(); v == 0 {
		t.Error("expected ViolationCount > 0 after grace window, got 0")
	}
}

// TestSnapshotOverlapClassifier_ClearedWhenOverlapResolved verifies that a
// previously-overlapping partition no longer fires after the overlap is
// resolved (the second worker drops the partition).
func TestSnapshotOverlapClassifier_ClearedWhenOverlapResolved(t *testing.T) {
	t.Parallel()

	grace := 20 * time.Millisecond
	c := NewSnapshotOverlapClassifier(grace, nil)

	c.IngestAssignment("worker-A", []int{0, 1})
	c.IngestAssignment("worker-B", []int{0, 2}) // overlap on 0

	// First Check() stamps firstSeen; sleep past grace, then second Check() fires.
	c.Check(time.Now())
	time.Sleep(grace + 5*time.Millisecond)
	c.Check(time.Now())
	if v := c.ViolationCount(); v == 0 {
		t.Fatal("expected violation after grace; got 0")
	}

	// Resolve overlap: worker-B drops partition 0.
	c.IngestAssignment("worker-B", []int{2})

	// The overlap entry should be cleared. Re-check — count must not grow.
	beforeResolve := c.ViolationCount()
	c.Check(time.Now()) // clears the active entry
	time.Sleep(grace + 5*time.Millisecond)
	c.Check(time.Now())
	if c.ViolationCount() > beforeResolve {
		t.Errorf("ViolationCount grew after overlap resolved: was %d, now %d",
			beforeResolve, c.ViolationCount())
	}
}

// TestSnapshotOverlapClassifier_NoOverlapNoViolation verifies that workers
// with disjoint assignments produce no violations.
func TestSnapshotOverlapClassifier_NoOverlapNoViolation(t *testing.T) {
	t.Parallel()

	c := NewSnapshotOverlapClassifier(10*time.Millisecond, nil)
	c.IngestAssignment("worker-A", []int{0, 1})
	c.IngestAssignment("worker-B", []int{2, 3})

	time.Sleep(20 * time.Millisecond)
	c.Check(time.Now())

	if v := c.ViolationCount(); v != 0 {
		t.Errorf("expected 0 violations for disjoint assignments, got %d", v)
	}
}

// ---------------------------------------------------------------------------
// Leader-uniqueness watcher (Gap 14)
// ---------------------------------------------------------------------------

// TestLeaderUniquenessWatcher_TwoLeadersViolation verifies that
// DoubleLeaderObservations() returns > 0 when two workers simultaneously
// report IsLeader()==true via the goroutine registry (pre-chaos).
func TestLeaderUniquenessWatcher_TwoLeadersViolation(t *testing.T) {
	t.Parallel()

	registry := NewGoroutineRegistry()
	w1 := &stubWorker{leader: true}
	w2 := &stubWorker{leader: true}
	registry.Register("w1", WorkerGoroutine, nil, func(_ context.Context) {}, w1)
	registry.Register("w2", WorkerGoroutine, nil, func(_ context.Context) {}, w2)

	watcher := NewLeaderUniquenessWatcher(registry)
	watcher.poll()

	if watcher.DoubleLeaderObservations() == 0 {
		t.Error("expected DoubleLeaderObservations > 0 when two workers are leaders, got 0")
	}
}

// TestLeaderUniquenessWatcher_PostChaosDoubleLeaderNotCounted verifies that
// double-leader observations after MarkChaosStarted do NOT increment the
// fail-run counter (they go to the informational post-chaos bucket instead).
func TestLeaderUniquenessWatcher_PostChaosDoubleLeaderNotCounted(t *testing.T) {
	t.Parallel()

	registry := NewGoroutineRegistry()
	w1 := &stubWorker{leader: true}
	w2 := &stubWorker{leader: true}
	registry.Register("w1", WorkerGoroutine, nil, func(_ context.Context) {}, w1)
	registry.Register("w2", WorkerGoroutine, nil, func(_ context.Context) {}, w2)

	watcher := NewLeaderUniquenessWatcher(registry)
	watcher.MarkChaosStarted()
	watcher.poll()

	if n := watcher.DoubleLeaderObservations(); n != 0 {
		t.Errorf("expected 0 fail-run violations post-chaos, got %d", n)
	}
}

// TestLeaderUniquenessWatcher_OneLeaderNoViolation verifies that no violation
// fires when exactly one worker is the leader.
func TestLeaderUniquenessWatcher_OneLeaderNoViolation(t *testing.T) {
	t.Parallel()

	registry := NewGoroutineRegistry()
	w1 := &stubWorker{leader: true}
	w2 := &stubWorker{leader: false}
	registry.Register("w1", WorkerGoroutine, nil, func(_ context.Context) {}, w1)
	registry.Register("w2", WorkerGoroutine, nil, func(_ context.Context) {}, w2)

	watcher := NewLeaderUniquenessWatcher(registry)
	watcher.poll()

	if n := watcher.DoubleLeaderObservations(); n != 0 {
		t.Errorf("expected 0 double-leader observations with one leader, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// State-reconcile watcher (Gap 15)
// ---------------------------------------------------------------------------

// TestStateReconcileWatcher_StableNoMessageStaleAssignment verifies that a
// worker reporting StateStable with a non-empty assignment older than k and
// no recent messages triggers a state_reconcile_violation.
func TestStateReconcileWatcher_StableNoMessageStaleAssignment(t *testing.T) {
	t.Parallel()

	registry := NewGoroutineRegistry()
	w := &stubWorker{
		state:          stateStableValue,
		stableWorkerID: "w1",
	}
	registry.Register("w1", WorkerGoroutine, nil, func(_ context.Context) {}, w)

	k := 50 * time.Millisecond
	watcher := NewStateReconcileWatcher(registry, k)
	// Pre-record a non-empty assignment that is already older than k.
	watcher.RecordAssignment("w1", []int{0, 1}, time.Now().Add(-2*k))
	// No messages recorded.
	watcher.poll(time.Now())

	if watcher.StateReconcileViolations() == 0 {
		t.Error("expected StateReconcileViolations > 0 for StateStable worker with stale non-empty assignment and no messages")
	}
}

// TestStateReconcileWatcher_StableNoMessageNoAssignment verifies that a
// worker reporting StateStable with no assignment at all does NOT trigger a
// state_reconcile_violation (legitimately scaled-down workers have zero partitions).
func TestStateReconcileWatcher_StableNoMessageNoAssignment(t *testing.T) {
	t.Parallel()

	registry := NewGoroutineRegistry()
	w := &stubWorker{
		state:          stateStableValue,
		stableWorkerID: "w1",
	}
	registry.Register("w1", WorkerGoroutine, nil, func(_ context.Context) {}, w)

	k := 50 * time.Millisecond
	watcher := NewStateReconcileWatcher(registry, k)
	// No assignment recorded, no messages.
	watcher.poll(time.Now())

	if n := watcher.StateReconcileViolations(); n != 0 {
		t.Errorf("expected 0 violations for StateStable worker with no assignment (scaled-down), got %d", n)
	}
}

// TestStateReconcileWatcher_StableWithRecentMessage verifies no violation
// when a StateStable worker has a recent message.
func TestStateReconcileWatcher_StableWithRecentMessage(t *testing.T) {
	t.Parallel()

	registry := NewGoroutineRegistry()
	w := &stubWorker{
		state:          stateStableValue,
		stableWorkerID: "w1",
	}
	registry.Register("w1", WorkerGoroutine, nil, func(_ context.Context) {}, w)

	k := 1 * time.Second
	watcher := NewStateReconcileWatcher(registry, k)
	// Record a recent message for w1 on partition 0.
	watcher.RecordMessage("w1", 0)

	watcher.poll(time.Now())

	if n := watcher.StateReconcileViolations(); n != 0 {
		t.Errorf("expected 0 violations for worker with recent message, got %d", n)
	}
}

// TestStateReconcileWatcher_StableWithNonEmptyRecentAssignment verifies no
// violation when a StateStable worker has a fresh non-empty assignment.
func TestStateReconcileWatcher_StableWithNonEmptyRecentAssignment(t *testing.T) {
	t.Parallel()

	registry := NewGoroutineRegistry()
	w := &stubWorker{
		state:          stateStableValue,
		stableWorkerID: "w1",
	}
	registry.Register("w1", WorkerGoroutine, nil, func(_ context.Context) {}, w)

	k := 1 * time.Second
	watcher := NewStateReconcileWatcher(registry, k)
	// Record a fresh non-empty assignment.
	watcher.RecordAssignment("w1", []int{0, 1}, time.Now())

	watcher.poll(time.Now())

	if n := watcher.StateReconcileViolations(); n != 0 {
		t.Errorf("expected 0 violations with fresh non-empty assignment, got %d", n)
	}
}

// TestStateReconcileWatcher_NonStableStateNoViolation verifies that the oracle
// is silent for workers not reporting StateStable.
func TestStateReconcileWatcher_NonStableStateNoViolation(t *testing.T) {
	t.Parallel()

	registry := NewGoroutineRegistry()
	// Worker is NOT in StateStable; oracle is silent.
	w := &stubWorker{
		state:          stateWaitingAssignmentValue,
		stableWorkerID: "w1",
	}
	registry.Register("w1", WorkerGoroutine, nil, func(_ context.Context) {}, w)

	k := 50 * time.Millisecond
	watcher := NewStateReconcileWatcher(registry, k)
	watcher.poll(time.Now())

	if n := watcher.StateReconcileViolations(); n != 0 {
		t.Errorf("expected 0 violations for non-stable worker, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// Stub helpers used only in this test file.
// These are defined here so they don't pollute the production API.
// ---------------------------------------------------------------------------

// stubWorker implements the WorkerObserver interface for watcher unit
// tests without requiring a real parti.Manager.
type stubWorker struct {
	leader         bool
	state          int // raw int so we don't import parti here
	stableWorkerID string
	claimLost      bool
}

func (s *stubWorker) IsLeader() bool          { return s.leader }
func (s *stubWorker) WorkerStateInt() int     { return s.state }
func (s *stubWorker) StableWorkerID() string  { return s.stableWorkerID }
func (s *stubWorker) ClaimLostObserved() bool { return s.claimLost }

// stateStableValue mirrors parti.StateStable (== 4 per types/state.go).
// Hardcoded to avoid a circular import: coordinator_test → worker → coordinator.
const stateStableValue = 4
const stateWaitingAssignmentValue = 3
