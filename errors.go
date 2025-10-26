package parti

import "errors"

// Sentinel errors returned by the Manager.
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

	// ErrNotStarted is returned when Stop is called on a manager that hasn't been started.
	ErrNotStarted = errors.New("manager not started")

	// ErrNotImplemented is returned for functionality not yet implemented.
	ErrNotImplemented = errors.New("not implemented")

	// ErrNoWorkersAvailable is returned when trying to assign partitions with no workers.
	ErrNoWorkersAvailable = errors.New("no workers available")

	// ErrInvalidWorkerID is returned when a worker ID is invalid.
	ErrInvalidWorkerID = errors.New("invalid worker ID")

	// ErrElectionFailed is returned when leader election fails.
	ErrElectionFailed = errors.New("leader election failed")

	// ErrIDClaimFailed is returned when stable ID claiming fails.
	ErrIDClaimFailed = errors.New("failed to claim stable worker ID")

	// ErrAssignmentFailed is returned when assignment calculation or distribution fails.
	ErrAssignmentFailed = errors.New("assignment failed")
)
