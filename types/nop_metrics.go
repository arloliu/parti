package types

// NopMetrics is an exported composite no-op that satisfies the full
// MetricsCollector interface and its optional LabelMetrics extension, with
// every method discarding its argument.
//
// It exists to be embedded. MetricsCollector is a union of eight
// domain-focused sub-interfaces, so a consumer that only cares about a few
// signals — say the label gauges — would otherwise have to stub dozens of
// unrelated methods. Embed NopMetrics and override just the methods you need.
// Overrides must guard any shared state themselves: the manager calls collector
// methods from internal goroutines, so a health reader running concurrently
// needs the same lock (see the runnable example in docs/LABELS.md):
//
//	type labelCollector struct {
//	    types.NopMetrics // supplies no-ops for every other method
//	    mu               sync.Mutex
//	    parked           map[string]int
//	}
//
//	func (c *labelCollector) RecordParkedPartitions(label string, count int) {
//	    c.mu.Lock()
//	    defer c.mu.Unlock()
//	    c.parked[label] = count
//	}
//
//	mgr, _ := parti.NewManager(cfg, js, src, strategy, parti.WithMetrics(&labelCollector{...}))
//
// The zero value is ready to use. NopMetrics' own methods are non-blocking and
// safe for concurrent use, matching the MetricsCollector contract.
type NopMetrics struct{}

// Compile-time assertions that NopMetrics satisfies the collector interface and
// its optional label extension.
var (
	_ MetricsCollector = (*NopMetrics)(nil)
	_ LabelMetrics     = (*NopMetrics)(nil)
)

// ManagerMetrics implementation

// RecordStateTransition discards the state transition metric.
func (NopMetrics) RecordStateTransition(_ /* from */, _ /* to */ State, _ /* duration */ float64) {
}

// RecordLeadershipChange discards the leadership change metric.
func (NopMetrics) RecordLeadershipChange(_ /* newLeader */ string) {}

// RecordDegradedDuration discards the degraded mode duration metric.
func (NopMetrics) RecordDegradedDuration(_ /* duration */ float64) {}

// SetDegradedMode discards the degraded mode status metric.
func (NopMetrics) SetDegradedMode(_ /* degraded */ float64) {}

// SetCacheAge discards the cache age metric.
func (NopMetrics) SetCacheAge(_ /* age */ float64) {}

// SetAlertLevel discards the alert level metric.
func (NopMetrics) SetAlertLevel(_ /* level */ int) {}

// IncrementAlertEmitted discards the alert emission counter.
func (NopMetrics) IncrementAlertEmitted(_ /* level */ string) {}

// RecordApplyAttempt discards the apply-attempt counter.
func (NopMetrics) RecordApplyAttempt(_ /* workerID */ string, _ /* version */ int64) {}

// RecordHandoffRemovalPending discards the handoff-removal-pending counter.
func (NopMetrics) RecordHandoffRemovalPending(_ /* workerID */ string) {}

// CalculatorMetrics implementation

// RecordRebalanceDuration discards the rebalance duration metric.
func (NopMetrics) RecordRebalanceDuration(_ /* duration */ float64, _ /* reason */ string) {}

// RecordRebalanceAttempt discards the rebalance attempt metric.
func (NopMetrics) RecordRebalanceAttempt(_ /* reason */ string, _ /* success */ bool) {}

// RecordPartitionCount discards the partition count metric.
func (NopMetrics) RecordPartitionCount(_ /* count */ int) {}

// RecordKVOperationDuration discards the KV operation duration metric.
func (NopMetrics) RecordKVOperationDuration(_ /* operation */ string, _ /* duration */ float64) {}

// RecordStateChangeDropped discards the state change dropped metric.
func (NopMetrics) RecordStateChangeDropped() {}

// RecordEmergencyRebalance discards the emergency rebalance metric.
func (NopMetrics) RecordEmergencyRebalance(_ /* disappearedWorkers */ int) {}

// RecordWorkerChange discards the worker topology change metric.
func (NopMetrics) RecordWorkerChange(_ /* added */, _ /* removed */ int) {}

// RecordOrphanedPartitions discards the orphaned partitions metric.
func (NopMetrics) RecordOrphanedPartitions(_ /* count */ int) {}

// RecordActiveWorkers discards the active workers metric.
func (NopMetrics) RecordActiveWorkers(_ /* count */ int) {}

// RecordCacheUsage discards the cache usage metric.
func (NopMetrics) RecordCacheUsage(_ /* cacheType */ string, _ /* age */ float64) {}

// IncrementCacheFallback discards the cache fallback counter.
func (NopMetrics) IncrementCacheFallback(_ /* reason */ string) {}

// WorkerMetrics implementation

// RecordHeartbeat discards the heartbeat metric.
func (NopMetrics) RecordHeartbeat(_ /* workerID */ string, _ /* success */ bool) {}

// AssignmentMetrics implementation

// RecordAssignmentChange discards the assignment change metric.
func (NopMetrics) RecordAssignmentChange(_ /* added */, _ /* removed */ int, _ /* version */ int64) {
}

// PublisherMetrics implementation

// IncrementPayloadsCreated discards the payloads-created counter.
func (NopMetrics) IncrementPayloadsCreated() {}

// IncrementPayloadsReused discards the payloads-reused counter.
func (NopMetrics) IncrementPayloadsReused() {}

// ObservePayloadBytesWritten discards the payload bytes-written histogram.
func (NopMetrics) ObservePayloadBytesWritten(_ /* bytes */ int) {}

// ObserveCommitBytesWritten discards the commit bytes-written histogram.
func (NopMetrics) ObserveCommitBytesWritten(_ /* bytes */ int) {}

// IncrementBatchAborted discards the batch-aborted counter.
func (NopMetrics) IncrementBatchAborted(_ /* reason */ string) {}

// IncrementAliasBarrierFailed discards the alias-barrier-failed counter.
func (NopMetrics) IncrementAliasBarrierFailed() {}

// IncrementAliasVisibleUncommitted discards the alias-visible-uncommitted counter.
func (NopMetrics) IncrementAliasVisibleUncommitted() {}

// IncrementCommitAborts discards the commit-aborts counter.
func (NopMetrics) IncrementCommitAborts() {}

// GCMetrics implementation

// IncrementPayloadDeleteErrors discards the payload-delete-errors counter.
func (NopMetrics) IncrementPayloadDeleteErrors() {}

// AuditMetrics implementation

// RecordAuditCounts discards the audit classification gauge.
func (NopMetrics) RecordAuditCounts(_ /* fullyApplied */, _ /* behind */, _ /* unverifiable */ int) {
}

// RecordWorkerBehind discards the behind-worker observation.
func (NopMetrics) RecordWorkerBehind(_ /* workerID */ string, _ /* commitVersion */ int64) {}

// RecordAuditEscalationSkipped discards the escalation-skipped counter.
func (NopMetrics) RecordAuditEscalationSkipped(_ /* reason */, _ /* workerID */ string) {}

// RecordStaleLeaderRejected discards the stale-leader rejection counter.
func (NopMetrics) RecordStaleLeaderRejected() {}

// RecordStaleSnapshotStoreDropped discards the stale-snapshot-Store gate counter.
func (NopMetrics) RecordStaleSnapshotStoreDropped() {}

// RecordCommitPayloadMissing discards the malformed-commit counter.
func (NopMetrics) RecordCommitPayloadMissing() {}

// RecordPayloadFetchError discards the payload-fetch error counter.
func (NopMetrics) RecordPayloadFetchError() {}

// RecordPayloadDecompressError discards the payload-decompress error counter.
func (NopMetrics) RecordPayloadDecompressError() {}

// RecordPayloadDecodeError discards the payload-decode error counter.
func (NopMetrics) RecordPayloadDecodeError() {}

// RecordPayloadHashMismatch discards the payload hash-mismatch counter.
func (NopMetrics) RecordPayloadHashMismatch() {}

// RecordSetDigestMismatch discards the set-digest mismatch counter.
func (NopMetrics) RecordSetDigestMismatch() {}

// WorkerConsumerMetrics implementation

// IncrementWorkerConsumerControlRetry discards the control-plane retry counter.
func (NopMetrics) IncrementWorkerConsumerControlRetry(_ /* op */ string) {}

// RecordWorkerConsumerRetryBackoff discards the backoff duration observation.
func (NopMetrics) RecordWorkerConsumerRetryBackoff(_ /* op */ string, _ /* seconds */ float64) {}

// SetWorkerConsumerSubjectsCurrent discards the gauge set.
func (NopMetrics) SetWorkerConsumerSubjectsCurrent(_ /* count */ int) {}

// IncrementWorkerConsumerSubjectChange discards subject change increments.
func (NopMetrics) IncrementWorkerConsumerSubjectChange(_ /* kind */ string, _ /* count */ int) {}

// IncrementWorkerConsumerGuardrailViolation discards guardrail violation increments.
func (NopMetrics) IncrementWorkerConsumerGuardrailViolation(_ /* kind */ string) {}

// IncrementWorkerConsumerSubjectThresholdWarning discards threshold warning increments.
func (NopMetrics) IncrementWorkerConsumerSubjectThresholdWarning() {}

// RecordWorkerConsumerUpdate discards update result increments.
func (NopMetrics) RecordWorkerConsumerUpdate(_ /* result */ string) {}

// ObserveWorkerConsumerUpdateLatency discards latency observations.
func (NopMetrics) ObserveWorkerConsumerUpdateLatency(_ /* seconds */ float64) {}

// IncrementWorkerConsumerIteratorRestart discards iterator restart increments.
func (NopMetrics) IncrementWorkerConsumerIteratorRestart(_ /* reason */ string) {}

// IncrementWorkerConsumerIteratorEscalation discards iterator escalation increments.
func (NopMetrics) IncrementWorkerConsumerIteratorEscalation(_ /* reason */ string) {}

// SetWorkerConsumerConsecutiveIteratorFailures discards consecutive failure gauge updates.
func (NopMetrics) SetWorkerConsumerConsecutiveIteratorFailures(_ /* count */ int) {}

// SetWorkerConsumerHealthStatus discards health status updates.
func (NopMetrics) SetWorkerConsumerHealthStatus(_ /* healthy */ bool) {}

// IncrementWorkerConsumerRecreationAttempt discards recreation attempt increments.
func (NopMetrics) IncrementWorkerConsumerRecreationAttempt(_ /* reason */ string) {}

// RecordWorkerConsumerRecreation discards recreation outcome increments.
func (NopMetrics) RecordWorkerConsumerRecreation(_ /* result */ string, _ /* reason */ string) {}

// ObserveWorkerConsumerRecreationDuration discards recreation duration observations.
func (NopMetrics) ObserveWorkerConsumerRecreationDuration(_ /* seconds */ float64) {}

// IncrementWorkerConsumerPullSuppressed discards pull suppression increments.
func (NopMetrics) IncrementWorkerConsumerPullSuppressed(_ /* reason */ string) {}

// LabelMetrics implementation (optional extension interface)

// RecordLabelPoolSize discards the per-label worker pool size gauge.
func (NopMetrics) RecordLabelPoolSize(_ /* label */ string, _ /* workers */ int) {}

// RecordParkedPartitions discards the per-label parked-partition count gauge.
func (NopMetrics) RecordParkedPartitions(_ /* label */ string, _ /* count */ int) {}

// IncrementLabelSpill discards the label-spill counter.
func (NopMetrics) IncrementLabelSpill(_ /* label */ string) {}

// IncrementLabelChangeTrigger discards the label-change-triggered-rebalance counter.
func (NopMetrics) IncrementLabelChangeTrigger() {}

// IncrementLabelIncarnationReject discards the stale-incarnation label rejection counter.
func (NopMetrics) IncrementLabelIncarnationReject() {}

// IncrementUnlabeledFallback discards the unlabeled-fallback counter.
func (NopMetrics) IncrementUnlabeledFallback() {}

// ConsumerCreateThrottleObserver implementation (optional sidecar).
//
// These two methods are not part of MetricsCollector; they let an embedded
// NopMetrics also satisfy the internal consumer-create throttle observer that
// the durable helper type-asserts for, so a collector embedding NopMetrics
// gets a complete no-op base with no extra stubbing.

// IncrementConsumerCreateThrottled discards the consumer-create throttle counter.
func (NopMetrics) IncrementConsumerCreateThrottled() {}

// ObserveConsumerCreateThrottleWait discards the throttle wait observation.
func (NopMetrics) ObserveConsumerCreateThrottleWait(_ /* seconds */ float64) {}
