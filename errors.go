package parti

import "github.com/arloliu/parti/v2/types"

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
	ErrInvalidPreset              = types.ErrInvalidPreset
	// ErrStreamMissing is the operator-actionable umbrella error surfaced
	// when the dynamic partition consumer's recovery flow determines that
	// the underlying JetStream stream is absent (or post-hook recovery
	// fails for any reason — restored consumer config mismatch, etc.).
	// Callers branch via errors.Is(err, parti.ErrStreamMissing) inside
	// their Hooks.OnError handler. See [types.ErrStreamMissing] for the
	// full contract.
	ErrStreamMissing = types.ErrStreamMissing
)
