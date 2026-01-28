package parti

import "github.com/arloliu/parti/types"

// Re-export sentinel errors from the internal types package.
//
// These are stable, comparable values intended for use with errors.Is().
// Keeping them in the root package provides a convenient public API while
// allowing internal packages to depend on the `types` subpackage without
// importing the root package.
var (
	ErrInvalidConfig              = types.ErrInvalidConfig
	ErrNATSConnectionRequired     = types.ErrNATSConnectionRequired
	ErrPartitionSourceRequired    = types.ErrPartitionSourceRequired
	ErrAssignmentStrategyRequired = types.ErrAssignmentStrategyRequired
	ErrAlreadyStarted             = types.ErrAlreadyStarted
	ErrNotStarted                 = types.ErrNotStarted
	ErrNotImplemented             = types.ErrNotImplemented
	ErrNoWorkersAvailable         = types.ErrNoWorkersAvailable
	ErrInvalidWorkerID            = types.ErrInvalidWorkerID
	ErrElectionFailed             = types.ErrElectionFailed
	ErrConnectivity               = types.ErrConnectivity
	ErrDegraded                   = types.ErrDegraded
	ErrIDClaimFailed              = types.ErrIDClaimFailed
	ErrAssignmentFailed           = types.ErrAssignmentFailed
)
