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
//	js, _ := jetstream.New(conn)
//	mgr, err := parti.NewManager(&cfg, js, src, strategy, parti.WithMetrics(metrics))
//	if err != nil { /* handle */ }
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

// WorkerConsumerMetrics implementation

// IncrementWorkerConsumerControlRetry discards the control-plane retry counter.
func (n *NopMetrics) IncrementWorkerConsumerControlRetry(_ /* op */ string) {
	// No-op
}

// RecordWorkerConsumerRetryBackoff discards the backoff duration observation.
func (n *NopMetrics) RecordWorkerConsumerRetryBackoff(_ /* op */ string, _ /* seconds */ float64) {
	// No-op
}

// SetWorkerConsumerSubjectsCurrent discards the gauge set.
func (n *NopMetrics) SetWorkerConsumerSubjectsCurrent(_ /* count */ int) {
	// No-op
}

// IncrementWorkerConsumerSubjectChange discards subject change increments.
func (n *NopMetrics) IncrementWorkerConsumerSubjectChange(_ /* kind */ string, _ /* count */ int) {
	// No-op
}

// IncrementWorkerConsumerGuardrailViolation discards guardrail violation increments.
func (n *NopMetrics) IncrementWorkerConsumerGuardrailViolation(_ /* kind */ string) {
	// No-op
}

// IncrementWorkerConsumerSubjectThresholdWarning discards threshold warning increments.
func (n *NopMetrics) IncrementWorkerConsumerSubjectThresholdWarning() {
	// No-op
}

// RecordWorkerConsumerUpdate discards update result increments.
func (n *NopMetrics) RecordWorkerConsumerUpdate(_ /* result */ string) {
	// No-op
}

// ObserveWorkerConsumerUpdateLatency discards latency observations.
func (n *NopMetrics) ObserveWorkerConsumerUpdateLatency(_ /* seconds */ float64) {
	// No-op
}

// IncrementWorkerConsumerIteratorRestart discards iterator restart increments.
func (n *NopMetrics) IncrementWorkerConsumerIteratorRestart(_ /* reason */ string) {
	// No-op
}

// IncrementWorkerConsumerIteratorEscalation discards iterator escalation increments.
func (n *NopMetrics) IncrementWorkerConsumerIteratorEscalation() {
	// No-op
}

// SetWorkerConsumerConsecutiveIteratorFailures discards consecutive failure gauge updates.
func (n *NopMetrics) SetWorkerConsumerConsecutiveIteratorFailures(_ /* count */ int) {
	// No-op
}

// SetWorkerConsumerHealthStatus discards health status updates.
func (n *NopMetrics) SetWorkerConsumerHealthStatus(_ /* healthy */ bool) {
	// No-op
}

// IncrementWorkerConsumerRecreationAttempt discards recreation attempt increments.
func (n *NopMetrics) IncrementWorkerConsumerRecreationAttempt(_ /* reason */ string) {
	// No-op
}

// RecordWorkerConsumerRecreation discards recreation outcome increments.
func (n *NopMetrics) RecordWorkerConsumerRecreation(_ /* result */ string, _ /* reason */ string) {
	// No-op
}

// ObserveWorkerConsumerRecreationDuration discards recreation duration observations.
func (n *NopMetrics) ObserveWorkerConsumerRecreationDuration(_ /* seconds */ float64) {
	// No-op
}
