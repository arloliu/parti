package coordinator

import (
	"testing"
	"time"
)

func TestSourceConvergence_ExpectedSetMatched(t *testing.T) {
	o := NewSourceConvergenceOracle()
	o.ExpectAfter("add_p3", []int{0, 1, 2, 3}, nil, 1*time.Second, false)

	// First snapshot is incomplete — expected unmet.
	o.CheckSnapshot(idSet([]int{0, 1, 2}))
	if got := o.Observed(); got != 0 {
		t.Fatalf("Observed=%d before convergence; want 0", got)
	}

	// Second snapshot includes the expected partition.
	o.CheckSnapshot(idSet([]int{0, 1, 2, 3}))
	if got := o.Observed(); got != 1 {
		t.Fatalf("Observed=%d after convergence; want 1", got)
	}

	// Subsequent matching snapshots don't double-count.
	o.CheckSnapshot(idSet([]int{0, 1, 2, 3}))
	if got := o.Observed(); got != 1 {
		t.Fatalf("Observed=%d (double-counted); want 1", got)
	}
}

func TestSourceConvergence_ForbiddenSetEnforced(t *testing.T) {
	o := NewSourceConvergenceOracle()
	o.ExpectAfter("remove_p3", []int{0, 1, 2}, []int{3}, 1*time.Second, false)

	// Stale snapshot still containing forbidden id — not yet converged.
	o.CheckSnapshot(idSet([]int{0, 1, 2, 3}))
	if got := o.Observed(); got != 0 {
		t.Fatalf("Observed=%d; forbidden=3 still present; want 0", got)
	}

	// Workers caught up — partition 3 dropped.
	o.CheckSnapshot(idSet([]int{0, 1, 2}))
	if got := o.Observed(); got != 1 {
		t.Fatalf("Observed=%d; want 1", got)
	}
}

func TestSourceConvergence_DeadlineMissed(t *testing.T) {
	o := NewSourceConvergenceOracle()
	o.ExpectAfter("never_meets", []int{0, 1, 2, 99}, nil, 10*time.Millisecond, false)

	// Snapshot never includes 99.
	o.CheckSnapshot(idSet([]int{0, 1, 2}))
	time.Sleep(30 * time.Millisecond)
	o.Sweep(time.Now())

	if got := o.Missing(); got != 1 {
		t.Fatalf("Missing=%d; want 1", got)
	}
	if got := o.Observed(); got != 0 {
		t.Fatalf("Observed=%d; want 0", got)
	}
}

func TestSourceConvergence_SpuriousUnavailableCounted(t *testing.T) {
	o := NewSourceConvergenceOracle()
	o.ExpectAfter("clean_stall", []int{0, 1, 2}, nil, 1*time.Second, false /* disallow */)

	o.ObserveSourceUnavailable("worker-0", "source-unavailable:parti-sim-source: ErrNoResponders")
	o.ObserveSourceUnavailable("worker-1", "source-unavailable:parti-sim-source: ErrNoResponders")

	if got := o.SpuriousSourceUnavailable(); got != 2 {
		t.Fatalf("SpuriousSourceUnavailable=%d; want 2", got)
	}
}

func TestSourceConvergence_AllowedUnavailableIgnored(t *testing.T) {
	o := NewSourceConvergenceOracle()
	o.ExpectAfter("composed_bucket_delete", []int{0, 1, 2}, nil, 1*time.Second, true /* allow */)

	o.ObserveSourceUnavailable("worker-0", "source-unavailable:parti-sim-source: ErrBucketNotFound")

	if got := o.SpuriousSourceUnavailable(); got != 0 {
		t.Fatalf("SpuriousSourceUnavailable=%d (allowed but counted); want 0", got)
	}
}

func TestSourceConvergence_SnapshotAfterDeadlineDoesNotComplete(t *testing.T) {
	o := NewSourceConvergenceOracle()
	o.ExpectAfter("late_snapshot", []int{0, 1}, nil, 10*time.Millisecond, false)

	time.Sleep(30 * time.Millisecond)
	// Matching snapshot AFTER deadline — should NOT count.
	o.CheckSnapshot(idSet([]int{0, 1}))
	if got := o.Observed(); got != 0 {
		t.Fatalf("Observed=%d; late-snapshot should not satisfy; want 0", got)
	}

	o.Sweep(time.Now())
	if got := o.Missing(); got != 1 {
		t.Fatalf("Missing=%d; want 1", got)
	}
}
