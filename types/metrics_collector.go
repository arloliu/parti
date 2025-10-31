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
