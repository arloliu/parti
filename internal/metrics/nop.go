package metrics

import "github.com/arloliu/parti/types"

// NopMetrics implements a no-op metrics collector.
//
// All metrics are discarded. Useful for testing or when external
// metrics collection is used.
type NopMetrics struct{}

// Compile-time assertion that NopMetrics implements MetricsCollector.
var _ types.MetricsCollector = (*NopMetrics)(nil)

// NewNop creates a new no-op metrics collector.
//
// Returns:
//   - *NopMetrics: A new no-op metrics collector instance
//
// Example:
//
//	metrics := metrics.NewNop()
//	mgr := parti.NewManager(&cfg, conn, src, strategy, parti.WithMetrics(metrics))
func NewNop() *NopMetrics {
	return &NopMetrics{}
}

// RecordStateTransition discards the state transition metric.
func (n *NopMetrics) RecordStateTransition(_ /* from */, _ /* to */ types.State, _ /* duration */ float64) {
	// No-op
}

// RecordAssignmentChange discards the assignment change metric.
func (n *NopMetrics) RecordAssignmentChange(_ /* added */, _ /* removed */ int, _ /* version */ int64) {
	// No-op
}

// RecordHeartbeat discards the heartbeat metric.
func (n *NopMetrics) RecordHeartbeat(_ /* workerID */ string, _ /* success */ bool) {
	// No-op
}

// RecordLeadershipChange discards the leadership change metric.
func (n *NopMetrics) RecordLeadershipChange(_ /* newLeader */ string) {
	// No-op
}
