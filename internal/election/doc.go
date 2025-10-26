// Package election provides leader election implementations for parti.
//
// Leader election ensures exactly one worker calculates and distributes
// partition assignments at any given time. This prevents conflicts and
// ensures consistent assignment distribution across all workers.
//
// # NATS KV Election
//
// The primary implementation uses NATS KV store for leader election:
//   - Atomic operations prevent split-brain scenarios
//   - TTL-based leases enable automatic failover
//   - Revision checking ensures leadership integrity
//   - Minimal latency for leadership acquisition
//
// # Usage
//
// Basic leader election setup:
//
//	// Create KV bucket for election
//	kv, _ := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
//	    Bucket:  "parti-election",
//	    TTL:     30 * time.Second,
//	    Storage: jetstream.FileStorage,
//	})
//
//	// Create election agent
//	election := election.NewNATS(kv, "leader")
//
//	// Request leadership
//	isLeader, err := election.RequestLeadership(ctx, workerID, 30)
//	if err != nil {
//	    log.Fatalf("Failed to request leadership: %v", err)
//	}
//
//	if isLeader {
//	    // Start background renewal
//	    go func() {
//	        ticker := time.NewTicker(10 * time.Second)
//	        defer ticker.Stop()
//	        for range ticker.C {
//	            if err := election.RenewLeadership(ctx); err != nil {
//	                log.Printf("Lost leadership: %v", err)
//	                break
//	            }
//	        }
//	    }()
//
//	    // Perform leader tasks...
//	}
//
//	// Release leadership on shutdown
//	defer election.ReleaseLeadership(ctx)
//
// # Leadership Lifecycle
//
// Leader election follows a strict lifecycle:
//
//  1. Request: Worker requests leadership with RequestLeadership()
//  2. Acquire: If successful, worker becomes leader
//  3. Renew: Leader periodically renews lease with RenewLeadership()
//  4. Release: Leader releases on shutdown with ReleaseLeadership()
//  5. Failover: If leader crashes, TTL expires and new leader elected
//
// # Failover Behavior
//
// Automatic failover occurs when:
//   - Leader crashes (TTL expires after ~30s)
//   - Leader releases leadership (immediate)
//   - Network partition (TTL-based timeout)
//
// The recommended renewal interval is TTL/3 to provide safety margin.
// For a 30s TTL, renew every 10s.
//
// # Concurrency Safety
//
// The NATSElection implementation is NOT thread-safe. Each worker
// should have a single election instance. Multiple goroutines should
// not call methods concurrently.
//
// # Error Handling
//
// Common errors:
//   - ErrNotLeader: Attempted operation requires leadership
//   - ErrLeadershipLost: Leadership was lost (another worker took over)
//   - ErrInvalidDuration: Invalid lease duration (must be > 0)
//
// # Performance Characteristics
//
// Operation latencies (typical):
//   - RequestLeadership: 1-5ms (atomic KV Create)
//   - RenewLeadership: 1-3ms (KV Update with revision)
//   - ReleaseLeadership: 1-3ms (KV Delete)
//   - IsLeader: 1-3ms (KV Get)
//
// Failover time:
//   - Immediate: On explicit Release (0-100ms)
//   - Automatic: On crash (TTL + detection time, ~30-35s)
package election
