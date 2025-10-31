package metrics

import (
	"testing"

	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

func TestNewNop(t *testing.T) {
	metrics := NewNop()

	require.NotNil(t, metrics)
	require.IsType(t, &NopMetrics{}, metrics)
}

func TestNopMetrics_RecordStateTransition(t *testing.T) {
	metrics := NewNop()

	// Should not panic with various inputs
	require.NotPanics(t, func() {
		metrics.RecordStateTransition(types.StateInit, types.StateStable, 1.5)
		metrics.RecordStateTransition(0, 0, 0)
		metrics.RecordStateTransition(types.State(999), types.State(1000), -1.0)
	})
}

func TestNopMetrics_RecordAssignmentChange(t *testing.T) {
	metrics := NewNop()

	// Should not panic with various inputs
	require.NotPanics(t, func() {
		metrics.RecordAssignmentChange(5, 3, 42)
		metrics.RecordAssignmentChange(0, 0, 0)
		metrics.RecordAssignmentChange(-1, -1, -1)
	})
}

func TestNopMetrics_RecordHeartbeat(t *testing.T) {
	metrics := NewNop()

	// Should not panic with various inputs
	require.NotPanics(t, func() {
		metrics.RecordHeartbeat("worker-0", true)
		metrics.RecordHeartbeat("worker-0", false)
		metrics.RecordHeartbeat("", true)
		metrics.RecordHeartbeat("test-worker-with-long-name", false)
	})
}

func TestNopMetrics_RecordLeadershipChange(t *testing.T) {
	metrics := NewNop()

	// Should not panic with various inputs
	require.NotPanics(t, func() {
		metrics.RecordLeadershipChange("worker-0")
		metrics.RecordLeadershipChange("")
		metrics.RecordLeadershipChange("new-leader")
	})
}

func TestNopMetrics_CalculatorMetrics(t *testing.T) {
	metrics := NewNop()

	// Should not panic with calculator metrics
	require.NotPanics(t, func() {
		metrics.RecordRebalanceDuration(1.5, "cold_start")
		metrics.RecordRebalanceDuration(0, "")
		metrics.RecordRebalanceAttempt("planned_scale", true)
		metrics.RecordRebalanceAttempt("emergency", false)
		metrics.RecordPartitionCount(100)
		metrics.RecordPartitionCount(0)
		metrics.RecordKVOperationDuration("get", 0.001)
		metrics.RecordKVOperationDuration("put", 0.002)
		metrics.RecordStateChangeDropped()
		metrics.RecordEmergencyRebalance(3)
		metrics.RecordEmergencyRebalance(0)
		metrics.RecordWorkerChange(5, 2)
		metrics.RecordWorkerChange(0, 0)
		metrics.RecordWorkerChange(-1, -1)
		metrics.RecordActiveWorkers(10)
		metrics.RecordActiveWorkers(0)
	})
}

func TestNopMetrics_WorkerMetrics(t *testing.T) {
	metrics := NewNop()

	// Should not panic with worker heartbeat metrics
	require.NotPanics(t, func() {
		metrics.RecordHeartbeat("worker-1", true)
		metrics.RecordHeartbeat("worker-2", false)
		metrics.RecordHeartbeat("", true)
	})
}

func TestNopMetrics_InterfaceImplementation(t *testing.T) {
	metrics := NewNop()

	// Verify it implements all sub-interfaces
	require.Implements(t, (*types.ManagerMetrics)(nil), metrics)
	require.Implements(t, (*types.CalculatorMetrics)(nil), metrics)
	require.Implements(t, (*types.WorkerMetrics)(nil), metrics)
	require.Implements(t, (*types.AssignmentMetrics)(nil), metrics)
	require.Implements(t, (*types.MetricsCollector)(nil), metrics)
}

func BenchmarkNopMetrics_RecordStateTransition(b *testing.B) {
	metrics := NewNop()
	for b.Loop() {
		metrics.RecordStateTransition(types.StateInit, types.StateStable, 1.5)
	}
}

func BenchmarkNopMetrics_RecordAssignmentChange(b *testing.B) {
	metrics := NewNop()
	for b.Loop() {
		metrics.RecordAssignmentChange(5, 3, 42)
	}
}

func BenchmarkNopMetrics_RecordHeartbeat(b *testing.B) {
	metrics := NewNop()
	for b.Loop() {
		metrics.RecordHeartbeat("worker-0", true)
	}
}

func BenchmarkNopMetrics_RecordLeadershipChange(b *testing.B) {
	metrics := NewNop()
	for b.Loop() {
		metrics.RecordLeadershipChange("worker-0")
	}
}

func BenchmarkNopMetrics_RecordRebalanceDuration(b *testing.B) {
	metrics := NewNop()
	for b.Loop() {
		metrics.RecordRebalanceDuration(1.5, "cold_start")
	}
}

func BenchmarkNopMetrics_RecordWorkerChange(b *testing.B) {
	metrics := NewNop()
	for b.Loop() {
		metrics.RecordWorkerChange(5, 2)
	}
}
