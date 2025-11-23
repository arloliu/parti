package types

import "context"

// HandoffState represents the ownership handoff state for a partition during two-phase handoff.
//
// States generally follow the sequence: Stable → Prepare → Commit → Stable.
// The Processing Gate uses these states to determine whether a partition message
// should be processed by the current owner.
type HandoffState int

const (
	// HandoffStateUnknown indicates the state is not known (partition not found in claims).
	HandoffStateUnknown HandoffState = iota

	// HandoffStateStable indicates normal operation with stable ownership.
	HandoffStateStable

	// HandoffStatePrepare indicates the first phase of handoff (old owner still processing).
	HandoffStatePrepare

	// HandoffStateCommit indicates the second phase of handoff (new owner can start processing).
	HandoffStateCommit
)

// OwnershipResolver provides the current owner and handoff state for a partition.
//
// Implementations should provide O(1) reads without lock contention for use in
// the message processing hot path. The returned `ok` value indicates whether the
// partition was found in the resolver's cache.
type OwnershipResolver interface {
	// GetOwner returns ownership information for a partition.
	//
	// Parameters:
	//   - partitionID: Partition identifier to look up
	//
	// Returns:
	//   - string: Owner worker ID (empty if not found)
	//   - HandoffState: Current handoff state
	//   - int64: Claim epoch number
	//   - bool: true if partition found, false otherwise
	GetOwner(partitionID string) (string, HandoffState, int64, bool)

	// ForceRefreshPartition performs a best-effort on-demand refresh for a specific partition.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - partitionID: Partition identifier to refresh
	//
	// Returns:
	//   - error: Non-nil if the refresh failed
	ForceRefreshPartition(ctx context.Context, partitionID string) error
}
