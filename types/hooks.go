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
	// OnAssignmentChanged is called when partition assignment changes.
	// added: partitions newly assigned to this worker
	// removed: partitions no longer assigned to this worker
	OnAssignmentChanged func(ctx context.Context, added, removed []Partition) error

	// OnStateChanged is called when worker state transitions.
	OnStateChanged func(ctx context.Context, from, to State) error

	// OnError is called when a recoverable error occurs.
	OnError func(ctx context.Context, err error) error
}
