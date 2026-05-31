package types

// MetricsCollector defines methods for recording operational metrics.
//
// Implementations should be non-blocking and handle failures gracefully.
// All methods are called from internal goroutines and must be thread-safe.
//
// This interface composes smaller, domain-focused interfaces for better modularity.
type MetricsCollector interface {
	ManagerMetrics
	CalculatorMetrics
	WorkerMetrics
	AssignmentMetrics
	PublisherMetrics
	GCMetrics
	AuditMetrics
	WorkerConsumerMetrics
}

// ManagerMetrics defines metrics for manager-level operations.
type ManagerMetrics interface {
	// RecordStateTransition records a manager state transition event.
	RecordStateTransition(from, to State, duration float64)

	// RecordLeadershipChange records a leadership change.
	RecordLeadershipChange(newLeader string)

	// RecordDegradedDuration records the duration spent in degraded mode.
	//
	// Parameters:
	//   - duration: Time spent in degraded mode
	RecordDegradedDuration(duration float64)

	// SetDegradedMode sets the current degraded mode status (0 or 1).
	//
	// Parameters:
	//   - degraded: 1.0 if in degraded mode, 0.0 otherwise
	SetDegradedMode(degraded float64)

	// SetCacheAge sets the age of cached assignment data in seconds.
	//
	// Parameters:
	//   - age: Age of cached data (0 if no cache)
	SetCacheAge(age float64)

	// SetAlertLevel sets the current degraded mode alert level (0-3).
	//
	// Parameters:
	//   - level: Alert level (0=none, 1=info, 2=warn, 3=error, 4=critical)
	SetAlertLevel(level int)

	// IncrementAlertEmitted tracks alert emission by level for spam detection.
	//
	// Parameters:
	//   - level: Alert level name ("info", "warn", "error", "critical")
	IncrementAlertEmitted(level string)

	// RecordApplyAttempt records each invocation of the manager's apply
	// pipeline (applyAssignmentWithPrev) BEFORE the (V, LR) stale gate runs.
	// Used to diagnose Raft-re-election bursts: a clean coalescing should
	// produce one apply per (worker, version); a leaky watcher path produces
	// N for N watcher deliveries.
	//
	// Parameters:
	//   - workerID: Stable worker identifier (caller passes m.WorkerID()).
	//   - version: The candidate assignment's Version field.
	RecordApplyAttempt(workerID string, version int64)

	// RecordHandoffRemovalPending records one deferral of a handoff transfer
	// removal by the manager's removal guard: the worker was about to remove a
	// partition subject during a rebalance, but the gaining worker had not yet
	// committed its ownership claim, so the removal was blocked (the apply
	// returns ErrRemovalPending and is retried). A sustained rate indicates a
	// transfer that is stuck before commit.
	//
	// Parameters:
	//   - workerID: Stable worker identifier (caller passes m.WorkerID()).
	RecordHandoffRemovalPending(workerID string)
}

// AuditMetrics defines metrics for the leader-side apply audit (§4.2) and the
// worker-side commit-path state machine (§3.6 / §4.4).
//
// Audit metrics are emitted by the calculator's audit loop; commit-path
// metrics are emitted by the manager when applying or rejecting a commit.
// All methods must be safe to call concurrently.
type AuditMetrics interface {
	// RecordAuditCounts records the classification counts (fully applied,
	// behind, unverifiable) for one audit pass as gauges.
	//
	// Parameters:
	//   - fullyApplied: Workers whose receipts match the live commit exactly.
	//   - behind: Workers whose receipts disagree on version, digest,
	//     source revision, or are missing payload refs.
	//   - unverifiable: Workers without CapAckV1 or whose heartbeat is missing.
	RecordAuditCounts(fullyApplied, behind, unverifiable int)

	// RecordWorkerBehind records a single behind-classified worker
	// observation during one audit pass.
	//
	// Parameters:
	//   - workerID: The worker classified as behind.
	//   - commitVersion: The commit.Version at the time of the audit.
	RecordWorkerBehind(workerID string, commitVersion int64)

	// RecordAuditEscalationSkipped records a skipped escalation with a reason
	// label. Reasons:
	//   - "cap_missing_behind": The behind worker lacks required capabilities.
	//   - "cap_missing_targets": No fully-applied worker has the full safety chain.
	//   - "direct_mode": cfg.EnableTwoPhaseHandoff is false.
	//
	// Parameters:
	//   - reason: One of the literals above.
	//   - workerID: The behind worker (may be "" for non-worker-scoped reasons).
	RecordAuditEscalationSkipped(reason, workerID string)

	// RecordStaleLeaderRejected counts assignments/commits rejected by the
	// worker-side stale-leader fence (§4.5).
	RecordStaleLeaderRejected()

	// RecordStaleSnapshotStoreDropped counts assignment candidates dropped
	// by the pre-Apply / refresh stale-snapshot gate (W15+W16, PR-2). A
	// nonzero counter indicates concurrent apply paths racing for the
	// same worker; small numbers are expected under churn (rolling
	// upgrade, leader handoff).
	//
	// Semantics: this counter increments BEFORE handoffCoordinator.Apply
	// runs (for applyAssignmentWithPrev's pre-Apply gate) or BEFORE the
	// snapshot Store (for refreshAssignmentFromNATS via monotonicStore).
	// It does NOT fire on Apply errors — those route through
	// scheduleApplyRetry without firing this counter.
	RecordStaleSnapshotStoreDropped()

	// RecordCommitPayloadMissing counts case (c) commits where the worker
	// appears in Workers but Payloads[worker] is missing.
	RecordCommitPayloadMissing()

	// RecordPayloadFetchError counts case (c) commits where the worker's
	// payload key could not be fetched from KV.
	RecordPayloadFetchError()

	// RecordPayloadDecompressError counts case (c) commits whose payload bytes
	// failed gzip decompression.
	RecordPayloadDecompressError()

	// RecordPayloadDecodeError counts case (c) commits whose decompressed
	// payload failed JSON decoding.
	RecordPayloadDecodeError()

	// RecordPayloadHashMismatch counts case (c) commits whose payload bytes
	// did not match the ref.PayloadHash sha256 digest.
	RecordPayloadHashMismatch()

	// RecordSetDigestMismatch counts case (c) commits whose decoded
	// AssignmentPayload's set digest did not match ref.SetDigest.
	RecordSetDigestMismatch()
}

// CalculatorMetrics defines metrics for calculator operations.
type CalculatorMetrics interface {
	// RecordRebalanceDuration records the time taken for a rebalance operation.
	//
	// Parameters:
	//   - duration: Time taken in seconds
	//   - reason: Rebalance reason ("cold_start", "planned_scale", "emergency", "restart")
	RecordRebalanceDuration(duration float64, reason string)

	// RecordRebalanceAttempt records a rebalance attempt (success or failure).
	//
	// Parameters:
	//   - reason: Rebalance reason
	//   - success: true if rebalance succeeded, false otherwise
	RecordRebalanceAttempt(reason string, success bool)

	// RecordPartitionCount sets the current partition count (gauge metric).
	//
	// Parameters:
	//   - count: Current number of partitions being managed
	RecordPartitionCount(count int)

	// RecordKVOperationDuration records NATS KV operation latency.
	//
	// Parameters:
	//   - operation: Operation type ("get", "put", "delete", "watch")
	//   - duration: Time taken in seconds
	RecordKVOperationDuration(operation string, duration float64)

	// RecordStateChangeDropped records when state change notifications are dropped due to slow subscribers.
	RecordStateChangeDropped()

	// RecordEmergencyRebalance records an emergency rebalance trigger.
	//
	// Parameters:
	//   - disappearedWorkers: Number of workers that disappeared suddenly
	RecordEmergencyRebalance(disappearedWorkers int)

	// RecordWorkerChange records worker topology changes detected by the calculator.
	//
	// Parameters:
	//   - added: Number of workers added (0 if none)
	//   - removed: Number of workers removed (0 if none)
	RecordWorkerChange(added, removed int)

	// RecordOrphanedPartitions records the number of partitions that were not assigned to any worker.
	//
	// Parameters:
	//   - count: Number of orphaned partitions
	RecordOrphanedPartitions(count int)

	// RecordActiveWorkers sets the current active worker count (gauge metric).
	//
	// Parameters:
	//   - count: Current number of active workers
	RecordActiveWorkers(count int)

	// RecordCacheUsage records when cached data is used instead of fresh KV data.
	//
	// Parameters:
	//   - cacheType: Type of cache used ("workers", "assignments")
	//   - age: Age of cached data in seconds
	RecordCacheUsage(cacheType string, age float64)

	// IncrementCacheFallback increments the counter for cache fallback events.
	//
	// Parameters:
	//   - reason: Reason for fallback ("connectivity_error", "timeout", "unknown")
	IncrementCacheFallback(reason string)
}

// WorkerMetrics defines metrics for individual worker heartbeat operations.
//
// These metrics are recorded by individual workers publishing their heartbeats,
// not by the calculator monitoring workers.
type WorkerMetrics interface {
	// RecordHeartbeat records a heartbeat event from an individual worker.
	//
	// Parameters:
	//   - workerID: The ID of the worker publishing the heartbeat
	//   - success: true if heartbeat was successfully published, false otherwise
	RecordHeartbeat(workerID string, success bool)
}

// AssignmentMetrics defines metrics for partition assignment operations.
type AssignmentMetrics interface {
	// RecordAssignmentChange records partition assignment changes.
	RecordAssignmentChange(added, removed int, version int64)
}

// PublisherMetrics defines metrics for the refs-always assignment publisher.
//
// All counters are increment-only; histograms accept observations in the
// natural unit named by the method (bytes for *_bytes_written, etc.).
//
// Implementations must be safe to call concurrently. Methods must not block.
type PublisherMetrics interface {
	// IncrementPayloadsCreated increments the counter of new content-addressable
	// payload keys written via kv.Create. Excludes reused keys.
	IncrementPayloadsCreated()

	// IncrementPayloadsReused increments the counter of payload keys whose
	// content already existed (kv.Create returned ErrKeyExists and the
	// stored bytes verified against the computed PayloadHash).
	IncrementPayloadsReused()

	// ObservePayloadBytesWritten observes the size in bytes of a payload
	// blob written via kv.Create. Recorded only on the create path; reused
	// payloads do not contribute.
	ObservePayloadBytesWritten(bytes int)

	// ObserveCommitBytesWritten observes the size in bytes of an
	// AssignmentCommit blob successfully written via the CAS commit point.
	ObserveCommitBytesWritten(bytes int)

	// IncrementBatchAborted increments the publisher batch abort counter
	// labeled by reason. Reasons:
	//   - "coverage_mismatch"
	//   - "leadership_lost_pre_alias"
	//   - "leadership_lost_post_alias"
	//   - "alias_barrier_failed"
	//   - "commit_cas_failed"
	//   - "shutdown" (leader stopCh closed between rebalance start and commit CAS)
	IncrementBatchAborted(reason string)

	// IncrementAliasBarrierFailed increments the counter of legacy-alias-barrier
	// failures (bounded retry + jitter exhausted). Each call corresponds to a
	// single batch abort.
	IncrementAliasBarrierFailed()

	// IncrementAliasVisibleUncommitted increments the documented mixed-version
	// exposure counter: legacy aliases were written for the batch but the
	// commit CAS was either skipped (post-alias leadership recheck failed) or
	// failed.
	IncrementAliasVisibleUncommitted()

	// IncrementCommitAborts increments the counter of commit-CAS attempt
	// failures. This counter does NOT increment on pre-CAS aborts (coverage,
	// leadership-loss, alias-barrier) — only when kv.Update against
	// assignment._commit was actually issued and lost.
	IncrementCommitAborts()
}

// GCMetrics defines metrics for the assignment payload garbage collector.
type GCMetrics interface {
	// IncrementPayloadDeleteErrors increments the counter of payload-key
	// delete failures during a GC pass. GC failures are non-fatal; they are
	// surfaced via this metric so operators can detect KV degradation.
	IncrementPayloadDeleteErrors()
}

// WorkerConsumerMetrics defines metrics for the single durable consumer helper.
type WorkerConsumerMetrics interface {
	// IncrementWorkerConsumerControlRetry increments retry attempts by operation (create_update, iterate, info).
	//
	// Parameters:
	//   - op: Operation name ("create_update", "iterate", "info")
	IncrementWorkerConsumerControlRetry(op string)

	// RecordWorkerConsumerRetryBackoff records backoff delay durations (seconds) by operation.
	// Typically emitted alongside retry increments to populate histogram buckets.
	//
	// Parameters:
	//   - op: Operation name
	//   - seconds: Backoff duration in seconds
	RecordWorkerConsumerRetryBackoff(op string, seconds float64)

	// SetWorkerConsumerSubjectsCurrent sets the current number of subjects (gauge).
	SetWorkerConsumerSubjectsCurrent(count int)

	// IncrementWorkerConsumerSubjectChange increments add/remove counts on subject diffs.
	IncrementWorkerConsumerSubjectChange(kind string, count int)

	// IncrementWorkerConsumerGuardrailViolation increments violations (max_subjects, workerid_mutation).
	IncrementWorkerConsumerGuardrailViolation(kind string)

	// IncrementWorkerConsumerSubjectThresholdWarning increments threshold warning events.
	IncrementWorkerConsumerSubjectThresholdWarning()

	// RecordWorkerConsumerUpdate increments update results (success|failure|noop).
	RecordWorkerConsumerUpdate(result string)

	// ObserveWorkerConsumerUpdateLatency records update latency in seconds.
	ObserveWorkerConsumerUpdateLatency(seconds float64)

	// IncrementWorkerConsumerIteratorRestart increments iterator restart counts by reason (transient|heartbeat).
	IncrementWorkerConsumerIteratorRestart(reason string)

	// IncrementWorkerConsumerIteratorEscalation increments the counter when iterator failures
	// escalate to a consumer refresh action. This captures bursts of failures that
	// warrant proactive intervention beyond simple iterator recreation.
	IncrementWorkerConsumerIteratorEscalation(reason string)
	// Reason indicates the cause (e.g., "burst").

	// SetWorkerConsumerConsecutiveIteratorFailures sets the current consecutive iterator failures gauge.
	SetWorkerConsumerConsecutiveIteratorFailures(count int)

	// SetWorkerConsumerHealthStatus sets worker consumer health status gauge (1 healthy, 0 unhealthy).
	SetWorkerConsumerHealthStatus(healthy bool)

	// IncrementWorkerConsumerRecreationAttempt increments recreation attempts by reason.
	//
	// Parameters:
	//   - reason: Reason category ("not_found"|"iterator_error"|"unknown")
	IncrementWorkerConsumerRecreationAttempt(reason string)

	// RecordWorkerConsumerRecreation records recreation outcomes by result and reason.
	//
	// Parameters:
	//   - result: "success"|"failure"
	//   - reason: "not_found"|"iterator_error"|"unknown"
	RecordWorkerConsumerRecreation(result string, reason string)

	// ObserveWorkerConsumerRecreationDuration observes total recreation latency in seconds.
	//
	// Parameters:
	//   - seconds: Total duration of a recreation attempt sequence (success or terminal failure)
	ObserveWorkerConsumerRecreationDuration(seconds float64)

	// IncrementWorkerConsumerPullSuppressed increments the counter when a pull is suppressed
	// due to ownership/state gating (v2 per-subject mode). Reason examples: "not_owner", "state_blocked".
	IncrementWorkerConsumerPullSuppressed(reason string)
}
