package types

import "context"

// Hooks defines callbacks for Manager lifecycle events.
//
// All hooks are optional and called asynchronously to avoid blocking
// the state machine. Hooks should be idempotent and handle errors gracefully.
type Hooks struct {
	// OnAssignmentChanged is called when partition assignment changes.
	// added: partitions newly assigned to this worker
	// removed: partitions no longer assigned to this worker
	OnAssignmentChanged func(ctx context.Context, added, removed []Partition) error

	// OnStateChanged is called when worker state transitions.
	OnStateChanged func(ctx context.Context, from, to State) error

	// OnError is called when a recoverable error occurs.
	OnError func(ctx context.Context, err error) error
}
