package types

import "context"

// Hooks defines callbacks for Manager lifecycle events.
//
// All hooks are optional and called asynchronously in background goroutines
// to avoid blocking the state machine. Hooks receive the manager's lifecycle
// context which will be cancelled during shutdown.
//
// IMPORTANT: Hook execution behavior:
//   - Hooks run concurrently and may not complete before Stop() returns
//   - The context passed to hooks is cancelled when manager stops
//   - Hook errors are logged but don't fail manager operations
//
// Best practices for hook implementation:
//   - Complete quickly (< 1 second recommended)
//   - Respect context cancellation
//   - Don't block on long I/O operations
//   - Make hooks idempotent (may be called multiple times)
//   - Handle errors gracefully (return error for logging)
//
// Example:
//
//	hooks := &parti.Hooks{
//	    OnStateChanged: func(ctx context.Context, from, to parti.State) error {
//	        select {
//	        case <-ctx.Done():
//	            return ctx.Err()  // Manager is shutting down
//	        case metricsChan <- StateMetric{from, to}:
//	            return nil
//	        case <-time.After(500 * time.Millisecond):
//	            return errors.New("metric send timeout")
//	        }
//	    },
//	}
type Hooks struct {
	// OnAssignmentChanged is called when this worker's partition assignment changes.
	//
	// Parameters:
	//   - oldPartitions: previous complete assignment set (may be empty on first assignment)
	//   - newPartitions: new complete assignment set (may be empty if worker loses all partitions)
	//
	// Invocation behavior:
	//   - Triggered ONLY when the assignment KV key is updated with a new version
	//   - NOT triggered on manager startup if no assignment exists for the worker
	//   - If a worker joins but receives no partitions (more workers than partitions),
	//     this hook will NOT be called since no assignment key is created
	//   - Receiving an empty newPartitions slice indicates the worker should release
	//     all partition-related resources (caches, connections, etc.)
	//
	// Example scenarios:
	//   - First assignment: oldPartitions=[], newPartitions=[p1,p2,p3]
	//   - Rebalance: oldPartitions=[p1,p2], newPartitions=[p1,p3]
	//   - All partitions removed: oldPartitions=[p1,p2], newPartitions=[]
	OnAssignmentChanged func(ctx context.Context, oldPartitions, newPartitions []Partition) error

	// OnStateChanged is called when worker state transitions.
	//
	// Invocation behavior:
	//   - Called asynchronously in a background goroutine
	//   - Triggered on all valid state transitions (see Manager state machine)
	//   - Always invoked when entering or exiting degraded mode
	//
	// Parameters:
	//   - from: previous state
	//   - to: new state
	OnStateChanged func(ctx context.Context, from, to State) error

	// OnError is called when a recoverable error occurs.
	//
	// Invocation behavior:
	//   - Called for transient errors that don't cause manager shutdown
	//   - May be called multiple times in rapid succession during issues
	//
	// Parameters:
	//   - err: the error that occurred
	OnError func(ctx context.Context, err error) error

	// OnLeadershipChanged is called when the worker acquires or loses leadership.
	//
	// Invocation behavior:
	//   - Called immediately after leadership status changes
	//   - Called with isLeader=true when initially elected as leader
	//   - Called with isLeader=false when leadership is lost (lease expiry, etc.)
	//   - Called with isLeader=true when a follower successfully claims vacant leadership
	//   - NOT called if worker remains a follower on startup
	//
	// Parameters:
	//   - isLeader: true if the worker is now the leader, false if leadership was lost
	OnLeadershipChanged func(ctx context.Context, isLeader bool) error

	// OnPartitionsAssigned is called when new partitions are assigned to this worker.
	//
	// This is a convenience hook derived from OnAssignmentChanged that provides
	// only the newly added partitions (not the full assignment set).
	//
	// Invocation behavior:
	//   - Called ONLY when there are partitions being added (len(added) > 0)
	//   - NOT called if assignment changes but no new partitions are added
	//   - NOT called for workers that never receive an assignment
	//
	// Parameters:
	//   - partitions: slice of newly assigned partitions (never empty when called)
	OnPartitionsAssigned func(ctx context.Context, partitions []Partition) error

	// OnPartitionsRevoked is called when partitions are removed from this worker.
	//
	// This is a convenience hook derived from OnAssignmentChanged that provides
	// only the removed partitions (not the full assignment set).
	//
	// Invocation behavior:
	//   - Called ONLY when there are partitions being removed (len(removed) > 0)
	//   - Called when a worker loses all its partitions (e.g., scaling down)
	//   - NOT called if assignment changes but no partitions are removed
	//   - Use this hook to clean up partition-specific resources (caches, connections)
	//
	// Parameters:
	//   - partitions: slice of revoked partitions (never empty when called)
	OnPartitionsRevoked func(ctx context.Context, partitions []Partition) error

	// OnDegraded is called when the manager enters degraded mode.
	//
	// Invocation behavior:
	//   - Called once when transitioning into degraded mode
	//   - NOT called repeatedly while in degraded mode
	//   - OnStateChanged is also called with the degraded state transition
	//
	// Parameters:
	//   - reason: description of the cause (e.g., "NATS connection down",
	//     "KV error threshold exceeded")
	OnDegraded func(ctx context.Context, reason string) error
}
