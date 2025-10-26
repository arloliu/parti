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
