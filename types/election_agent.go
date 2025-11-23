package types

import "context"

// ElectionAgent handles leader election for assignment coordination.
//
// Leader election ensures exactly one worker calculates and distributes
// partition assignments. The leader is responsible for:
//   - Monitoring worker join/leave events
//   - Calculating new assignments
//   - Publishing assignments to all workers
//
// Implementations can use:
//   - NATS KV (built-in, recommended)
//   - External agents (Consul, etcd, Zookeeper)
//   - Custom coordination services
//
// The Manager calls ElectionAgent methods during:
//   - Startup (request leadership)
//   - Background loop (renew leadership)
//   - Shutdown (release leadership)
type ElectionAgent interface {
	// RequestLeadership attempts to acquire leadership.
	//
	// Should use a lease-based mechanism with the specified duration.
	// If already leader, should extend the lease.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeout
	//   - workerID: The worker ID requesting leadership
	//   - leaseDuration: Lease duration in seconds
	//
	// Returns:
	//   - bool: true if leadership acquired/held, false otherwise
	//   - error: Election error (nil on success)
	RequestLeadership(ctx context.Context, workerID string, leaseDuration int64) (bool, error)

	// RenewLeadership renews the current leadership lease.
	//
	// Called periodically by the leader to maintain leadership.
	// Should fail if leadership was lost (another worker became leader).
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeout
	//
	// Returns:
	//   - error: Renewal error (nil on success, indicates leadership lost)
	RenewLeadership(ctx context.Context) error

	// ReleaseLeadership voluntarily releases leadership.
	//
	// Called during graceful shutdown to allow fast leader failover.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeout
	//
	// Returns:
	//   - error: Release error (nil on success)
	ReleaseLeadership(ctx context.Context) error

	// IsLeader checks if this worker is currently the leader.
	//
	// Used for state verification and metrics.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeout
	//
	// Returns:
	//   - bool: true if this worker is the leader
	//   - error: Check error (nil on success)
	IsLeader(ctx context.Context) (bool, error)
}
