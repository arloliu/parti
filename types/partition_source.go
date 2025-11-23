package types

import "context"

// PartitionSource discovers and provides the list of available partitions.
//
// Implementations can query various backends:
//   - Cassandra: the signal sources from system tables
//   - Static: fixed list for testing
//   - Custom: any dynamic partition discovery logic
//
// The Manager calls ListPartitions during:
//   - Startup (initial discovery)
//   - RefreshPartitions() (manual refresh)
//   - Periodic refresh (if configured)
type PartitionSource interface {
	// ListPartitions returns all available partitions.
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
	ListPartitions(ctx context.Context) ([]Partition, error)
}
