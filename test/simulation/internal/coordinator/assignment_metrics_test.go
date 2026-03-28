package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/test/simulation/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

// TestCoordinatorAssignmentMetrics verifies unassigned partition calculation and locality progression.
func TestCoordinatorAssignmentMetrics(t *testing.T) {
	collector := metrics.NewCollectorWithRegistry(prometheus.NewRegistry())
	coord := NewCoordinator(10, collector, DupTraceSettings{}, false, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go coord.Start(ctx)

	ch := coord.GetAssignmentsChannel()

	// First worker claims half the partitions. Gating logic should treat completeness as 100%
	// until full coverage is first achieved, so unassigned should report 0.
	ch <- AssignmentReport{WorkerID: "w1", Partitions: []int{0, 1, 2, 3, 4}}
	time.Sleep(20 * time.Millisecond)
	unassigned, locality, moved := collector.GetAssignmentMetrics()
	if unassigned != 0 { // gating suppresses partial coverage reporting
		t.Fatalf("expected 0 unassigned during convergence gating, got %d", unassigned)
	}
	// Locality baseline should be 1.0 (first snapshot)
	if locality != 1.0 {
		t.Fatalf("expected locality 1.0 on first snapshot got %f", locality)
	}
	if moved != 0 {
		t.Fatalf("expected moved=0 on first snapshot got %d", moved)
	}

	// Second worker claims remaining partitions; gating ends (full coverage achieved)
	ch <- AssignmentReport{WorkerID: "w2", Partitions: []int{5, 6, 7, 8, 9}}
	time.Sleep(20 * time.Millisecond)
	unassigned, locality, moved = collector.GetAssignmentMetrics()
	if unassigned != 0 {
		t.Fatalf("expected 0 unassigned after second report got %d", unassigned)
	}
	if locality <= 0.0 || locality > 1.0 { // locality should be in (0,1]; exact value depends on previous union size
		t.Fatalf("locality ratio out of range: %f", locality)
	}
	// After full coverage we expect convergence metric to have one sample.
	convCount, convSum := collector.ColdStartConvergenceSummary()
	if convCount == 0 || convSum <= 0 {
		t.Fatalf("expected cold start convergence metric recorded, got count=%d sum=%f", convCount, convSum)
	}
	// Moved partitions total should remain 0 since no partition moved yet (only additions)
	if moved != 0 {
		t.Fatalf("expected moved=0 before any reassignment got %d", moved)
	}

	// Reassignment of one partition from w1 to w2 (simulate movement)
	ch <- AssignmentReport{WorkerID: "w2", Partitions: []int{5, 6, 7, 8, 9, 0}}
	ch <- AssignmentReport{WorkerID: "w1", Partitions: []int{1, 2, 3, 4}} // w1 loses partition 0
	time.Sleep(40 * time.Millisecond)
	unassigned, _, moved = collector.GetAssignmentMetrics()
	if unassigned != 0 {
		t.Fatalf("expected still 0 unassigned after movement got %d", unassigned)
	}
	if moved == 0 {
		t.Fatalf("expected moved > 0 after partition movement, got %d", moved)
	}
}
