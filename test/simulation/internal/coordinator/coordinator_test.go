package coordinator

import (
	"testing"
	"time"
)

// TestPruneStaleRecoveries verifies that stale recovery contexts are pruned.
func TestPruneStaleRecoveries(t *testing.T) {
	t.Parallel()
	coord := NewCoordinator(10, nil, DupTraceSettings{})
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
