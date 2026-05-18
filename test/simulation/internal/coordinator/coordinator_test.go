package coordinator

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/arloliu/parti/v2/test/simulation/internal/metrics"
)

// TestStartLatencyClassifier_HonorsCohortFlag verifies the classifier
// buckets samples by the report's IsInitialCohort stamp instead of the
// coordinator's receive-time view of baselineLocked. Without this
// invariant, a chaos-delayed initial-cohort worker whose assignment
// report drains before its startLatency report (flipping baselineLocked
// before the trailing sample is classified) gets misbucketed as a
// takeover — a flaky path observed under network_disconnect_leader chaos.
func TestStartLatencyClassifier_HonorsCohortFlag(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	mc := metrics.NewCollectorWithRegistry(reg)
	coord := NewCoordinator(1, mc, DupTraceSettings{}, false, "")
	coord.ConfigureSlowStartBudgets(5*time.Second, 5*time.Second)

	go coord.Start(t.Context())

	ch := coord.GetStartLatencyChannel()
	ch <- StartLatencyReport{WorkerID: "w-initial-slow", Latency: 100 * time.Millisecond, IsInitialCohort: true}
	ch <- StartLatencyReport{WorkerID: "w-takeover-fast", Latency: 100 * time.Millisecond, IsInitialCohort: false}

	// Poll the bucketed maps; processing is async via the assignments goroutine.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		coord.startLatencyMu.Lock()
		gotInitial := coord.initialStartLatencies["w-initial-slow"]
		gotTakeover := coord.takeoverStartLatencies["w-takeover-fast"]
		coord.startLatencyMu.Unlock()
		if gotInitial > 0 && gotTakeover > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	coord.startLatencyMu.Lock()
	defer coord.startLatencyMu.Unlock()
	if _, ok := coord.initialStartLatencies["w-initial-slow"]; !ok {
		t.Errorf("initialStartLatencies missing w-initial-slow (got %+v)", coord.initialStartLatencies)
	}
	if _, ok := coord.takeoverStartLatencies["w-takeover-fast"]; !ok {
		t.Errorf("takeoverStartLatencies missing w-takeover-fast (got %+v)", coord.takeoverStartLatencies)
	}
	if _, wrong := coord.takeoverStartLatencies["w-initial-slow"]; wrong {
		t.Error("w-initial-slow incorrectly bucketed as takeover")
	}
	if _, wrong := coord.initialStartLatencies["w-takeover-fast"]; wrong {
		t.Error("w-takeover-fast incorrectly bucketed as initial")
	}
}

// TestPruneStaleRecoveries verifies that stale recovery contexts are pruned.
func TestPruneStaleRecoveries(t *testing.T) {
	t.Parallel()
	coord := NewCoordinator(10, nil, DupTraceSettings{}, false, "")
	coord.ConfigureCatchUpSLO(true, 1*time.Second, 100, 0)

	// Manually inject a stale recovery
	coord.catchMu.Lock()
	coord.workerRecovery["stale-worker"] = &workerRecoveryContext{
		startedAt: time.Now().Add(-1 * time.Hour),
		deadline:  1 * time.Second,
	}
	coord.workerLastSeen["stale-worker"] = time.Now().Add(-1 * time.Hour)

	// Inject a fresh recovery
	coord.workerRecovery["fresh-worker"] = &workerRecoveryContext{
		startedAt: time.Now(),
		deadline:  1 * time.Second,
	}
	coord.workerLastSeen["fresh-worker"] = time.Now()
	coord.catchMu.Unlock()

	// Prune
	coord.PruneStaleRecoveries(10 * time.Minute)

	coord.catchMu.Lock()
	defer coord.catchMu.Unlock()

	if _, exists := coord.workerRecovery["stale-worker"]; exists {
		t.Error("expected stale-worker recovery to be pruned")
	}
	if _, exists := coord.workerLastSeen["stale-worker"]; exists {
		t.Error("expected stale-worker lastSeen to be pruned")
	}

	if _, exists := coord.workerRecovery["fresh-worker"]; !exists {
		t.Error("expected fresh-worker recovery to remain")
	}
	if _, exists := coord.workerLastSeen["fresh-worker"]; !exists {
		t.Error("expected fresh-worker lastSeen to remain")
	}
}
