package assignment

import (
	"slices"
	"sync"
	"testing"
	"time"

	pmetrics "github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// WorkerChange captures added/removed counts for verification.
type WorkerChange struct {
	added   int
	removed int
}

// MockMetricsCollector captures metric calls for verification.
type MockMetricsCollector struct {
	*pmetrics.NopMetrics // Pick up future MetricsCollector methods automatically.
	mu                   sync.Mutex

	// Captured data
	workerChanges []WorkerChange
	activeWorkers []int
}

// Implement only the methods we need to verify
func (m *MockMetricsCollector) RecordWorkerChange(added, removed int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workerChanges = append(m.workerChanges, WorkerChange{added: added, removed: removed})
}

func (m *MockMetricsCollector) RecordActiveWorkers(count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeWorkers = append(m.activeWorkers, count)
}

// Stub out other methods to avoid panics
func (m *MockMetricsCollector) RecordEmergencyRebalance(disappearedWorkers int)              {}
func (m *MockMetricsCollector) RecordRebalanceAttempt(reason string, success bool)           {}
func (m *MockMetricsCollector) RecordRebalanceDuration(duration float64, reason string)      {}
func (m *MockMetricsCollector) RecordPartitionCount(count int)                               {}
func (m *MockMetricsCollector) RecordOrphanedPartitions(count int)                           {}
func (m *MockMetricsCollector) RecordAssignmentChange(added, removed int, version int64)     {}
func (m *MockMetricsCollector) RecordKVOperationDuration(operation string, duration float64) {}
func (m *MockMetricsCollector) RecordStateChangeDropped()                                    {}
func (m *MockMetricsCollector) RecordCacheUsage(cacheType string, age float64)               {}
func (m *MockMetricsCollector) IncrementCacheFallback(reason string)                         {}

// TestCalculator_EmergencyMetrics verifies that worker topology metrics
// are recorded even during emergency rebalancing.
func TestCalculator_EmergencyMetrics(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-emergency-metrics-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-emergency-metrics-heartbeat")

	// Setup: 2 workers initially
	workers := []string{"worker-1", "worker-2"}
	for _, w := range workers {
		_, err := heartbeatKV.Put(ctx, "worker-hb."+w, []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)
	}

	source := &mockSource{
		partitions: []types.Partition{{Keys: []string{"p1"}}},
	}
	strategy := &mockStrategy{}
	metrics := &MockMetricsCollector{NopMetrics: pmetrics.NewNop()}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         200 * time.Millisecond,
		EmergencyGracePeriod: 100 * time.Millisecond,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             1 * time.Second,
		Metrics:              metrics, // Inject mock metrics
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial assignment
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > 0
	}, 2*time.Second, 50*time.Millisecond)

	// Clear initial metrics
	metrics.mu.Lock()
	metrics.workerChanges = nil
	metrics.activeWorkers = nil
	metrics.mu.Unlock()

	// Trigger Emergency: Delete worker-2
	err = heartbeatKV.Delete(ctx, "worker-hb.worker-2")
	require.NoError(t, err)

	// Wait for emergency rebalance and metrics recording
	require.Eventually(t, func() bool {
		metrics.mu.Lock()
		defer metrics.mu.Unlock()
		return len(metrics.workerChanges) > 0
	}, 3*time.Second, 50*time.Millisecond)

	// Verify metrics were recorded
	metrics.mu.Lock()
	defer metrics.mu.Unlock()

	require.NotEmpty(t, metrics.workerChanges, "should record worker changes")
	require.NotEmpty(t, metrics.activeWorkers, "should record active workers")

	foundChange := false
	for _, change := range metrics.workerChanges {
		if change.added == 0 && change.removed == 1 {
			foundChange = true
			break
		}
	}
	require.True(t, foundChange, "should record removal of 1 worker")

	foundActive := slices.Contains(metrics.activeWorkers, 1)
	require.True(t, foundActive, "should record 1 active worker")
}
