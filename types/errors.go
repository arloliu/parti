package types

import (
	"errors"
	"strings"
)

// Sentinel errors for the Parti library.
//
// These errors provide type-safe error checking using errors.Is() and errors.As().
// All components should use these sentinel errors for known error conditions
// and wrap external errors with context using fmt.Errorf("%s: %w", msg, err).
//
// Error Naming Convention:
//   - Use descriptive names with Err prefix
//   - Group by component (Manager, Calculator, WorkerMonitor, etc.)
//   - Use consistent messages across similar error types

// Manager errors - Public API errors returned by Manager component.
var (
	// ErrInvalidConfig is returned when the configuration is invalid.
	ErrInvalidConfig = errors.New("invalid configuration")

	// ErrNATSConnectionRequired is returned when NATS connection is nil.
	ErrNATSConnectionRequired = errors.New("NATS connection is required")

	// ErrPartitionSourceRequired is returned when partition source is nil.
	ErrPartitionSourceRequired = errors.New("partition source is required")

	// ErrAssignmentStrategyRequired is returned when assignment strategy is nil.
	ErrAssignmentStrategyRequired = errors.New("assignment strategy is required")

	// ErrAlreadyStarted is returned when Start is called on an already running manager.
	ErrAlreadyStarted = errors.New("manager already started")

	// ErrNotStarted is returned when operations require a started manager.
	ErrNotStarted = errors.New("manager not started")

	// ErrNotImplemented is returned for functionality not yet implemented.
	ErrNotImplemented = errors.New("not implemented")

	// ErrNoWorkersAvailable is returned when trying to assign partitions with no workers.
	ErrNoWorkersAvailable = errors.New("no workers available")

	// ErrInvalidWorkerID is returned when a worker ID is invalid.
	ErrInvalidWorkerID = errors.New("invalid worker ID")

	// ErrElectionFailed is returned when leader election fails.
	ErrElectionFailed = errors.New("leader election failed")

	// ErrConnectivity indicates a NATS/KV connectivity issue.
	// This is used to distinguish network failures from application errors
	// and triggers degraded mode operation with cached data.
	ErrConnectivity = errors.New("connectivity issue")

	// ErrDegraded indicates the system is operating in degraded mode.
	// Operations may use cached data due to unavailable NATS connection.
	ErrDegraded = errors.New("degraded operation: using cached data")

	// ErrIDClaimFailed is returned when stable ID claiming fails.
	ErrIDClaimFailed = errors.New("failed to claim stable worker ID")

	// ErrAssignmentFailed is returned when assignment calculation or distribution fails.
	ErrAssignmentFailed = errors.New("assignment failed")
)

// Calculator errors - Internal assignment calculator component errors.
var (
	// ErrCalculatorAlreadyStarted is returned when Start is called on an already running calculator.
	ErrCalculatorAlreadyStarted = errors.New("calculator already started")

	// ErrCalculatorNotStarted is returned when operations require a started calculator.
	ErrCalculatorNotStarted = errors.New("calculator not started")
)

// WorkerMonitor errors - Internal worker monitoring component errors.
var (
	// ErrWorkerMonitorAlreadyStarted is returned when Start is called on an already running monitor.
	ErrWorkerMonitorAlreadyStarted = errors.New("worker monitor already started")

	// ErrWorkerMonitorAlreadyStopped is returned when Stop is called on an already stopped monitor.
	ErrWorkerMonitorAlreadyStopped = errors.New("worker monitor already stopped")

	// ErrWorkerMonitorNotStarted is returned when Stop is called before Start.
	ErrWorkerMonitorNotStarted = errors.New("worker monitor not started")

	// ErrWatcherFailed is returned when NATS KV watcher operations fail.
	ErrWatcherFailed = errors.New("watcher operation failed")
)

// AssignmentPublisher errors - Internal assignment publishing component errors.
var (
	// ErrPublishFailed is returned when publishing assignment to NATS KV fails.
	ErrPublishFailed = errors.New("failed to publish assignment")

	// ErrDeleteFailed is returned when deleting assignment from NATS KV fails.
	ErrDeleteFailed = errors.New("failed to delete assignment")
)

// Common errors - Shared errors used across multiple components.
var (
	// ErrContextCanceled is returned when an operation is canceled by context.
	ErrContextCanceled = errors.New("operation canceled by context")

	// ErrNoKeysFound is returned when NATS KV returns no keys (expected condition).
	ErrNoKeysFound = errors.New("no keys found")
)

// IsNoKeysFoundError checks if an error indicates that no keys were found in NATS KV.
//
// This function handles NATS-specific "no keys found" errors which may come as:
//   - Direct error: "nats: no keys found"
//   - Wrapped error: "failed to list KV keys: nats: no keys found"
//
// Parameters:
//   - err: The error to check
//
// Returns:
//   - bool: true if the error indicates no keys were found, false otherwise
func IsNoKeysFoundError(err error) bool {
	if err == nil {
		return false
	}
	// Check against our sentinel error first
	if errors.Is(err, ErrNoKeysFound) {
		return true
	}
	// Check for NATS-specific error message (handles both direct and wrapped errors)
	return strings.Contains(err.Error(), "no keys found")
}
