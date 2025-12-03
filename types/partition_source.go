package types

import "context"

// PartitionSource discovers and provides the list of available partitions.
//
// Implementations can query various backends:
//   - Cassandra: the signal sources from system tables
//   - Static: fixed list for testing
//   - Custom: any dynamic partition discovery logic
//
// The Manager calls List during:
//   - Startup (initial discovery)
//   - RefreshPartitions() (manual refresh)
//   - Periodic refresh (if configured)
type PartitionSource interface {
	// Start initializes the source (e.g., starts watchers).
	//
	// Parameters:
	//   - ctx: Context for initialization
	//
	// Returns:
	//   - error: Initialization error
	Start(ctx context.Context) error

	// List returns all available partitions.
	//
	// Implementations should:
	//   - Return consistent results for the same backend state
	//   - Handle context cancellation gracefully
	//   - Return errors for transient failures (will be retried)
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeout
	//
	// Returns:
	//   - []Partition: List of discovered partitions
	//   - error: Discovery error (nil on success)
	List(ctx context.Context) ([]Partition, error)

	// Stop cleans up resources (e.g., stops watchers).
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeout
	//
	// Returns:
	//   - error: Cleanup error
	Stop(ctx context.Context) error
}

// PartitionUpdater allows updating the partition definition in the backend.
//
// This interface is optional and typically used by admin tools or tests
// to modify the source of truth (e.g., NATS KV, file).
type PartitionUpdater interface {
	// Update updates the partition list in the backend.
	//
	// Parameters:
	//   - ctx: Context for the operation
	//   - partitions: New list of partitions
	//
	// Returns:
	//   - error: Update error
	Update(ctx context.Context, partitions []Partition) error
}

// WatchablePartitionSource is an extension for sources that can signal updates.
//
// Implementations of this interface allow the Calculator to react immediately
// to partition list changes without polling.
type WatchablePartitionSource interface {
	PartitionSource

	// Watch returns a channel that emits a signal when the partition list changes.
	//
	// The channel should be closed when the context is canceled or the source is stopped.
	// The caller should drain the channel to prevent blocking the source.
	//
	// Parameters:
	//   - ctx: Context for the watcher
	//
	// Returns:
	//   - <-chan struct{}: Signal channel
	Watch(ctx context.Context) <-chan struct{}
}

// PartitionSourceUpdater combines PartitionSource and PartitionUpdater.
type PartitionSourceUpdter interface {
	PartitionSource
	PartitionUpdater
}
