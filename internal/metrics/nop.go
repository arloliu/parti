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

// ManagerMetrics implementation

// RecordStateTransition discards the state transition metric.
func (n *NopMetrics) RecordStateTransition(_ /* from */, _ /* to */ types.State, _ /* duration */ float64) {
	// No-op
}

// RecordLeadershipChange discards the leadership change metric.
func (n *NopMetrics) RecordLeadershipChange(_ /* newLeader */ string) {
	// No-op
}

// RecordDegradedDuration discards the degraded mode duration metric.
func (n *NopMetrics) RecordDegradedDuration(_ /* duration */ float64) {
	// No-op
}

// SetDegradedMode discards the degraded mode status metric.
func (n *NopMetrics) SetDegradedMode(_ /* degraded */ float64) {
	// No-op
}

// SetCacheAge discards the cache age metric.
func (n *NopMetrics) SetCacheAge(_ /* age */ float64) {
	// No-op
}

// SetAlertLevel discards the alert level metric.
func (n *NopMetrics) SetAlertLevel(_ /* level */ int) {
	// No-op
}

// IncrementAlertEmitted discards the alert emission counter.
func (n *NopMetrics) IncrementAlertEmitted(_ /* level */ string) {
	// No-op
}

// CalculatorMetrics implementation

// RecordRebalanceDuration discards the rebalance duration metric.
func (n *NopMetrics) RecordRebalanceDuration(_ /* duration */ float64, _ /* reason */ string) {
	// No-op
}

// RecordRebalanceAttempt discards the rebalance attempt metric.
func (n *NopMetrics) RecordRebalanceAttempt(_ /* reason */ string, _ /* success */ bool) {
	// No-op
}

// RecordPartitionCount discards the partition count metric.
func (n *NopMetrics) RecordPartitionCount(_ /* count */ int) {
	// No-op
}

// RecordKVOperationDuration discards the KV operation duration metric.
func (n *NopMetrics) RecordKVOperationDuration(_ /* operation */ string, _ /* duration */ float64) {
	// No-op
}

// RecordStateChangeDropped discards the state change dropped metric.
func (n *NopMetrics) RecordStateChangeDropped() {
	// No-op
}

// RecordEmergencyRebalance discards the emergency rebalance metric.
func (n *NopMetrics) RecordEmergencyRebalance(_ /* disappearedWorkers */ int) {
	// No-op
}

// RecordWorkerChange discards the worker topology change metric.
func (n *NopMetrics) RecordWorkerChange(_ /* added */, _ /* removed */ int) {
	// No-op
}

// RecordActiveWorkers discards the active workers metric.
func (n *NopMetrics) RecordActiveWorkers(_ /* count */ int) {
	// No-op
}

// RecordCacheUsage discards the cache usage metric.
func (n *NopMetrics) RecordCacheUsage(_ /* cacheType */ string, _ /* age */ float64) {
	// No-op
}

// IncrementCacheFallback discards the cache fallback counter.
func (n *NopMetrics) IncrementCacheFallback(_ /* reason */ string) {
	// No-op
}

// WorkerMetrics implementation

// RecordHeartbeat discards the heartbeat metric.
func (n *NopMetrics) RecordHeartbeat(_ /* workerID */ string, _ /* success */ bool) {
	// No-op
}

// AssignmentMetrics implementation

// RecordAssignmentChange discards the assignment change metric.
func (n *NopMetrics) RecordAssignmentChange(_ /* added */, _ /* removed */ int, _ /* version */ int64) {
	// No-op
}
