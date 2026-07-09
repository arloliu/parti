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
	labels         []string
}

func (s *stubWorker) IsLeader() bool          { return s.leader }
func (s *stubWorker) WorkerStateInt() int     { return s.state }
func (s *stubWorker) StableWorkerID() string  { return s.stableWorkerID }
func (s *stubWorker) ClaimLostObserved() bool { return s.claimLost }
func (s *stubWorker) WorkerLabels() []string  { return s.labels }

// stateStableValue mirrors parti.StateStable (== 4 per types/state.go).
// Hardcoded to avoid a circular import: coordinator_test → worker → coordinator.
const stateStableValue = 4
const stateWaitingAssignmentValue = 3

// ---------------------------------------------------------------------------
// Label-affinity oracle
// ---------------------------------------------------------------------------

func TestLabelAffinityOracle_NoViolationWhenOwnerHasLabel(t *testing.T) {
	t.Parallel()
	registry := NewGoroutineRegistry()
	registry.Register("worker-A", WorkerGoroutine, func() {}, nil, &stubWorker{labels: []string{"vip-a"}})

	o := NewLabelAffinityOracle(map[int]string{0: "vip-a"}, 5*time.Second, registry)
	o.IngestAssignment("worker-A", []int{0})
	o.Check(time.Now())

	if v := o.ViolationCount(); v != 0 {
		t.Errorf("expected 0 violations, got %d", v)
	}
}

func TestLabelAffinityOracle_ViolationAfterGraceWhenOwnerLacksLabel(t *testing.T) {
	t.Parallel()
	registry := NewGoroutineRegistry()
	registry.Register("worker-A", WorkerGoroutine, func() {}, nil, &stubWorker{labels: nil}) // owns the partition, unlabeled
	// worker-B is a standing eligible vip-a carrier that owns nothing. It keeps
	// vip-a non-empty fleet-wide so the mismatch is AVOIDABLE (an eligible
	// worker exists but isn't being used) — i.e. the case the oracle exists to
	// catch — rather than the Task 11b fleet-wide-empty terminal state, which
	// is exempt. Without it, a lone unlabeled owner would be exempt and no
	// violation would fire, defeating this boundary test.
	registry.Register("worker-B", WorkerGoroutine, func() {}, nil, &stubWorker{labels: []string{"vip-a"}})

	grace := 20 * time.Millisecond
	o := NewLabelAffinityOracle(map[int]string{0: "vip-a"}, grace, registry)
	// This test exercises the spillGrace boundary itself, not the settle
	// allowance (that's covered by
	// TestLabelAffinityOracle_NoViolationWithinSettleAllowanceAfterGrace
	// below). Override the production settleAllowance
	// (labelAffinitySettleAllowance) down to 0 so a millisecond-scale sleep
	// can still cross the effective threshold without the test taking
	// multiple real seconds.
	o.settleAllowance = 0
	o.IngestAssignment("worker-A", []int{0})

	// First Check(): enters the park/grace window, no violation yet.
	o.Check(time.Now())
	if v := o.ViolationCount(); v != 0 {
		t.Fatalf("expected 0 violations on first check (still in grace), got %d", v)
	}

	// After grace: violation fires.
	time.Sleep(grace + 15*time.Millisecond)
	o.Check(time.Now())
	if v := o.ViolationCount(); v == 0 {
		t.Error("expected a violation after the grace window elapsed with no affine owner")
	}
}

// TestLabelAffinityOracle_NoViolationWithinSettleAllowanceAfterGrace verifies
// the fix this task adds: a mismatch that has outlived spillGrace but not yet
// spillGrace+settleAllowance must NOT count as a violation (the "rebalance /
// handoff" phase docs/LABELS.md's worst-case-stall formula documents as a
// separate, expected phase after LabelSpillGrace elapses). A prior version of
// LabelAffinityOracle.Check compared directly against spillGrace with no
// added slack, which flagged violations inside this exact window.
func TestLabelAffinityOracle_NoViolationWithinSettleAllowanceAfterGrace(t *testing.T) {
	t.Parallel()
	registry := NewGoroutineRegistry()
	registry.Register("worker-A", WorkerGoroutine, func() {}, nil, &stubWorker{labels: nil}) // owns the partition, unlabeled
	// Standing eligible vip-a carrier (owns nothing) — keeps vip-a non-empty so
	// the mismatch is avoidable and the settle-allowance boundary is genuinely
	// exercised rather than short-circuited by the Task 11b fleet-wide-empty
	// exemption. See TestLabelAffinityOracle_ViolationAfterGraceWhenOwnerLacksLabel.
	registry.Register("worker-B", WorkerGoroutine, func() {}, nil, &stubWorker{labels: []string{"vip-a"}})

	grace := 20 * time.Millisecond
	o := NewLabelAffinityOracle(map[int]string{0: "vip-a"}, grace, registry)
	// Use a short, deterministic settle allowance for the test instead of the
	// production labelAffinitySettleAllowance value so this test runs in
	// milliseconds while still exercising the exact same comparison logic.
	o.settleAllowance = 100 * time.Millisecond
	o.IngestAssignment("worker-A", []int{0})

	o.Check(time.Now()) // enters park/grace window

	// Past spillGrace (20ms) but well within spillGrace+settleAllowance
	// (120ms): must NOT violate. Pre-fix behavior compared directly against
	// spillGrace and would have flagged this as a violation.
	time.Sleep(60 * time.Millisecond)
	o.Check(time.Now())
	if v := o.ViolationCount(); v != 0 {
		t.Fatalf("expected 0 violations while within grace+settleAllowance (age~60ms, threshold=120ms), got %d", v)
	}

	// Past spillGrace+settleAllowance (120ms total): violation fires.
	time.Sleep(80 * time.Millisecond) // cumulative age ~140ms > 120ms threshold
	o.Check(time.Now())
	if v := o.ViolationCount(); v == 0 {
		t.Error("expected a violation once spillGrace+settleAllowance elapsed with no affine owner")
	}
}

func TestLabelAffinityOracle_NoViolationWithinGraceWindow(t *testing.T) {
	t.Parallel()
	registry := NewGoroutineRegistry()
	registry.Register("worker-A", WorkerGoroutine, func() {}, nil, &stubWorker{labels: nil})
	// Standing eligible vip-a carrier (owns nothing) so the mismatch is
	// avoidable — this test proves the WITHIN-grace path (park set, not yet
	// violated), which the Task 11b fleet-wide-empty exemption would otherwise
	// mask if vip-a had no carrier at all.
	registry.Register("worker-B", WorkerGoroutine, func() {}, nil, &stubWorker{labels: []string{"vip-a"}})

	grace := 1 * time.Hour // long enough that this test can't outlast it
	o := NewLabelAffinityOracle(map[int]string{0: "vip-a"}, grace, registry)
	o.IngestAssignment("worker-A", []int{0})
	o.Check(time.Now())
	o.Check(time.Now())
	if v := o.ViolationCount(); v != 0 {
		t.Errorf("expected 0 violations while still within the grace/park window, got %d", v)
	}
}

func TestLabelAffinityOracle_RecoveryClearsParkState(t *testing.T) {
	t.Parallel()
	registry := NewGoroutineRegistry()
	registry.Register("worker-A", WorkerGoroutine, func() {}, nil, &stubWorker{labels: nil})
	// Standing eligible vip-a carrier (owns nothing): keeps vip-a non-empty so
	// the FIRST Check genuinely parks the mismatch (rather than being exempted
	// by Task 11b's fleet-wide-empty rule), so this test truly exercises
	// "recovery clears a SET park state" and not merely "an affine worker
	// yields zero violations".
	registry.Register("worker-C", WorkerGoroutine, func() {}, nil, &stubWorker{labels: []string{"vip-a"}})

	grace := 15 * time.Millisecond
	o := NewLabelAffinityOracle(map[int]string{0: "vip-a"}, grace, registry)
	o.IngestAssignment("worker-A", []int{0})
	o.Check(time.Now()) // enters park (vip-a non-empty via worker-C, so not exempt)

	// A now-affine worker takes over before grace elapses.
	registry.Register("worker-B", WorkerGoroutine, func() {}, nil, &stubWorker{labels: []string{"vip-a"}})
	o.IngestAssignment("worker-B", []int{0})
	o.ForgetWorker("worker-A")
	o.Check(time.Now())

	time.Sleep(grace + 15*time.Millisecond)
	o.Check(time.Now())
	if v := o.ViolationCount(); v != 0 {
		t.Errorf("expected 0 violations after an affine worker took over before grace elapsed, got %d", v)
	}
}

// TestLabelAffinityOracle_SetLiveLabelOverrideTriggersImmediateRecheck
// reproduces the Task 10d bug: SetLiveLabelOverride used to update the
// override bookkeeping without ever calling Check(), so relabel events that
// CURE and then re-introduce a mismatch — without any intervening
// AssignmentReport / Check() call — never reset the parkedSince clock. A
// later, unrelated Check() call would then compute the mismatch's "age"
// against the ORIGINAL (much older) onset instead of the true onset of the
// current mismatch, and could fire a violation purely from that stale
// clock even though the live mismatch had only just re-started and was
// still well within spillGrace+settleAllowance. This mirrors the empirical
// diagnostic run that found a violation reported at age=127.7s spanning two
// full curing relabel events with zero intervening assignment-report
// activity.
//
// Sequence: park a mismatch at T0 -> CURE via SetLiveLabelOverride at T1
// (no Check() in between) -> UN-CURE via SetLiveLabelOverride at T2 (no
// Check() in between) -> Check() at T3, chosen so that (T3-T2) is
// comfortably under threshold (the current mismatch, if timed correctly,
// should not violate yet) but (T3-T0) is comfortably OVER threshold (the
// stale clock, if left unreset, would incorrectly violate).
//
// With the fix, both SetLiveLabelOverride calls (T1 cure, T2 un-cure)
// immediately re-check in the same critical section as the override
// update: T1 observes affine and clears parkedSince; T2 observes the
// renewed mismatch and re-parks it fresh at T2. Check() at T3 then
// correctly measures the age from T2, sees it under threshold, and fires
// no violation. Before the fix, SetLiveLabelOverride never re-checks, so
// parkedSince is still T0 when Check() runs at T3, and the stale
// (T3-T0) age incorrectly exceeds the threshold, firing a spurious
// violation for a mismatch that had actually only just resumed.
func TestLabelAffinityOracle_SetLiveLabelOverrideTriggersImmediateRecheck(t *testing.T) {
	t.Parallel()
	registry := NewGoroutineRegistry()
	// worker-A's registry-derived labels are unlabeled and never change —
	// mirrors the label_heartbeat_takeover primitive, which rewrites a
	// worker's heartbeat KV entry directly without touching its
	// Manager/Config, so the registry read alone can never observe either
	// the cure or the later un-cure; SetLiveLabelOverride is the only
	// signal for both.
	registry.Register("worker-A", WorkerGoroutine, func() {}, nil, &stubWorker{labels: nil})
	// worker-B is a standing eligible vip-a carrier that owns nothing. It keeps
	// vip-a non-empty fleet-wide for the whole test so the Task 11b
	// fleet-wide-empty exemption never applies — otherwise the T2 un-cure
	// (worker-A reverts to unlabeled with no other carrier) would leave vip-a
	// with zero carriers and the exemption, not the parkedSince-reset this test
	// is asserting, would be what keeps T3's count at zero. With worker-B
	// present the mismatch stays avoidable and this test isolates the Task 10d
	// immediate-recheck behavior exactly as intended.
	registry.Register("worker-B", WorkerGoroutine, func() {}, nil, &stubWorker{labels: []string{"vip-a"}})

	grace := 20 * time.Millisecond
	o := NewLabelAffinityOracle(map[int]string{0: "vip-a"}, grace, registry)
	o.settleAllowance = 40 * time.Millisecond // threshold = grace+settle = 60ms
	o.IngestAssignment("worker-A", []int{0})

	// T0: park the mismatch (worker-A owns partition 0 but lacks "vip-a").
	o.Check(time.Now())

	// T1 (T1-T0 ~= 25ms): CURE via a live label override. No Check() call
	// happens between T0 and T1.
	time.Sleep(25 * time.Millisecond)
	o.SetLiveLabelOverride("worker-A", []string{"vip-a"})

	// T2 (T2-T0 ~= 50ms): UN-CURE — revert the override, re-introducing the
	// mismatch. No Check() call happens between T1 and T2.
	time.Sleep(25 * time.Millisecond)
	o.SetLiveLabelOverride("worker-A", nil)

	// T3 (T3-T2 ~= 20ms, well under the 60ms threshold; T3-T0 ~= 70ms, well
	// over the 60ms threshold): an unrelated Check() call, as would happen
	// from some other event in the real coordinator.
	time.Sleep(20 * time.Millisecond)
	o.Check(time.Now())

	if v := o.ViolationCount(); v != 0 {
		t.Errorf("expected 0 violations: the current mismatch (since T2, ~20ms old) is well within grace+settleAllowance (60ms); "+
			"a violation here means parkedSince was not reset by SetLiveLabelOverride's cure/un-cure, got %d", v)
	}
}

func TestLabelAffinityOracle_UnlabeledPartitionNeverChecked(t *testing.T) {
	t.Parallel()
	registry := NewGoroutineRegistry()
	registry.Register("worker-A", WorkerGoroutine, func() {}, nil, &stubWorker{labels: nil})

	o := NewLabelAffinityOracle(map[int]string{}, 10*time.Millisecond, registry) // no labeled partitions at all
	o.IngestAssignment("worker-A", []int{0, 1, 2})
	time.Sleep(20 * time.Millisecond)
	o.Check(time.Now())
	if v := o.ViolationCount(); v != 0 {
		t.Errorf("expected 0 violations when no partition carries a label, got %d", v)
	}
}

// TestLabelAffinityOracle_NoViolationWhenLabelFleetWideEmpty proves the Task
// 11b exemption: when a labeled partition's owner lacks the required label AND
// no live worker anywhere in the fleet carries that label (the label's entire
// pool is permanently gone), the mismatch is the documented, PERMANENT terminal
// spill state (docs/LABELS.md "Parking and spill": a labeled partition whose
// pool has gone completely empty legitimately spills to an unlabeled fallback
// worker) — NOT an avoidable mismatch awaiting eventual correction. There is no
// worker anywhere that could ever make the partition affine again, so the
// oracle must NOT keep re-firing a violation every threshold window forever.
//
// Pre-fix this test FAILS: the oracle parks the mismatch and, once
// spillGrace+settleAllowance elapses, fires a violation because its only exit
// condition was "the current owner carries the label," which can never become
// true once the label has zero carriers fleet-wide.
func TestLabelAffinityOracle_NoViolationWhenLabelFleetWideEmpty(t *testing.T) {
	t.Parallel()
	registry := NewGoroutineRegistry()
	// worker-A owns the labeled partition but does NOT carry vip-a, and NO
	// other live worker carries vip-a either — the label's pool is fleet-wide
	// empty (mirrors label_pool_outage_spill / label_emergency_carveout after
	// every vip-a worker has been permanently killed).
	registry.Register("worker-A", WorkerGoroutine, func() {}, nil, &stubWorker{labels: nil})

	grace := 20 * time.Millisecond
	o := NewLabelAffinityOracle(map[int]string{0: "vip-a"}, grace, registry)
	o.settleAllowance = 0 // exercise the spillGrace boundary directly (see sibling tests)
	o.IngestAssignment("worker-A", []int{0})

	// First Check(): observes the mismatch; the exemption suppresses it.
	o.Check(time.Now())
	// Advance well past spillGrace+settleAllowance and check repeatedly — a
	// non-exempt mismatch would fire (and re-fire) here.
	time.Sleep(grace + 15*time.Millisecond)
	o.Check(time.Now())
	o.Check(time.Now())

	if v := o.ViolationCount(); v != 0 {
		t.Errorf("expected 0 violations: vip-a has no live carrier fleet-wide, so the mismatch is the "+
			"documented permanent terminal spill state, not an avoidable mismatch; got %d", v)
	}
}

// TestLabelAffinityOracle_ViolationWhenEligibleWorkerExists proves the Task 11b
// exemption does NOT mask a REAL, avoidable mismatch. Here an eligible worker
// (worker-B) DOES carry vip-a — so the label's pool is NOT fleet-wide empty —
// yet the partition is owned by an unlabeled worker (worker-A). An eligible
// worker exists and simply isn't being used: this is exactly the mismatch the
// oracle exists to catch, and the fleet-wide-empty exemption must NOT suppress
// it. (This test passes both pre- and post-fix; it guards the exemption's
// lower bound.)
func TestLabelAffinityOracle_ViolationWhenEligibleWorkerExists(t *testing.T) {
	t.Parallel()
	registry := NewGoroutineRegistry()
	registry.Register("worker-A", WorkerGoroutine, func() {}, nil, &stubWorker{labels: nil})               // owns partition, unlabeled
	registry.Register("worker-B", WorkerGoroutine, func() {}, nil, &stubWorker{labels: []string{"vip-a"}}) // eligible, unused

	grace := 20 * time.Millisecond
	o := NewLabelAffinityOracle(map[int]string{0: "vip-a"}, grace, registry)
	o.settleAllowance = 0
	o.IngestAssignment("worker-A", []int{0})

	o.Check(time.Now())
	time.Sleep(grace + 15*time.Millisecond)
	o.Check(time.Now())

	if v := o.ViolationCount(); v == 0 {
		t.Error("expected a violation: an eligible vip-a worker (worker-B) exists but the partition is owned by " +
			"an unlabeled worker — an avoidable mismatch the exemption must not suppress")
	}
}

// TestLabelAffinityOracle_ExemptionIsStateBasedNotSticky proves the exemption
// is recomputed fresh on every Check() from the current liveLabels, with no
// sticky "once exempt, always exempt" latch. It starts fleet-wide empty (no
// violation fires even past threshold), then an eligible vip-a worker appears
// (the pool becomes non-empty) while the partition's owner still lacks the
// label; after advancing past threshold again, a violation NOW fires. The
// re-entry to normal checking falls out naturally from checkLocked rebuilding
// liveLabels every call and resetting parkedSince during the exempt phase, so
// the freshly-avoidable mismatch gets a full threshold window before it counts.
//
// Pre-fix this test FAILS at phase 1 (the fleet-wide-empty mismatch fires a
// violation because there is no exemption yet).
func TestLabelAffinityOracle_ExemptionIsStateBasedNotSticky(t *testing.T) {
	t.Parallel()
	registry := NewGoroutineRegistry()
	// Phase 1 setup: worker-A owns partition 0, lacks vip-a, and no worker
	// anywhere carries vip-a — fleet-wide empty.
	registry.Register("worker-A", WorkerGoroutine, func() {}, nil, &stubWorker{labels: nil})

	grace := 20 * time.Millisecond
	o := NewLabelAffinityOracle(map[int]string{0: "vip-a"}, grace, registry)
	o.settleAllowance = 0
	o.IngestAssignment("worker-A", []int{0})

	// Phase 1: fleet-wide empty — exempt, no violation even past threshold.
	o.Check(time.Now())
	time.Sleep(grace + 15*time.Millisecond)
	o.Check(time.Now())
	if v := o.ViolationCount(); v != 0 {
		t.Fatalf("phase 1: expected 0 violations while vip-a is fleet-wide empty, got %d", v)
	}

	// Phase 2: an eligible vip-a worker appears (pool no longer empty). The
	// partition's owner (worker-A) still lacks the label, so the mismatch is
	// now avoidable. The exemption must stop applying immediately, and the
	// park clock restarts fresh from this observation.
	registry.Register("worker-B", WorkerGoroutine, func() {}, nil, &stubWorker{labels: []string{"vip-a"}})

	o.Check(time.Now()) // re-parks fresh (exemption no longer applies)
	if v := o.ViolationCount(); v != 0 {
		t.Fatalf("phase 2: expected 0 violations immediately after the eligible worker appears "+
			"(fresh park window), got %d — a stale park clock leaked across the exempt phase", v)
	}
	time.Sleep(grace + 15*time.Millisecond)
	o.Check(time.Now())
	if v := o.ViolationCount(); v == 0 {
		t.Error("phase 2: expected a violation once an eligible vip-a worker exists but the partition " +
			"remains owned by an unlabeled worker past the threshold")
	}
}
