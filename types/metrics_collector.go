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
