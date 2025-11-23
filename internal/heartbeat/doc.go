// Package heartbeat provides periodic health monitoring for workers through NATS KV.
//
// The heartbeat mechanism enables worker health tracking and crash detection using
// TTL-based expiration. Workers periodically publish timestamps to NATS KV, and
// the leader monitors these heartbeats to detect failed workers.
//
// # Design Overview
//
// The heartbeat system uses NATS JetStream KV as a distributed heartbeat registry:
//
//   - Workers publish heartbeats at 2-second intervals
//   - Keys have ~6-second TTL (3x interval for crash detection)
//   - Leader monitors heartbeats to detect worker failures
//   - Failed workers are detected after 3 missed heartbeats
//
// # Publisher Lifecycle
//
// The Publisher manages the complete heartbeat lifecycle:
//
//  1. Create publisher with New(kv, prefix, interval)
//  2. Set worker ID with SetWorkerID(workerID)
//  3. Start publishing with Start(ctx)
//  4. Stop publishing with Stop()
//
// Example:
//
//	// Create publisher
//	publisher := heartbeat.New(kv, "worker-hb", 2*time.Second)
//	publisher.SetWorkerID("worker-1")
//
//	// Start publishing
//	err := publisher.Start(ctx)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer publisher.Stop()
//
//	// Heartbeats are now published every 2 seconds
//
// # Key Format
//
// Heartbeats are stored in NATS KV with the following key format:
//
//	{prefix}.{workerID}
//
// Example: "worker-hb.worker-1"
//
// # Thread Safety
//
// The Publisher is thread-safe and can be accessed concurrently from multiple
// goroutines. All methods use proper synchronization to protect internal state.
//
// # Crash Detection
//
// The TTL-based expiration enables automatic crash detection:
//
//   - Normal operation: Worker publishes every 2 seconds, TTL resets to 6 seconds
//   - Worker crashes: No more publishes, key expires after 6 seconds
//   - Leader detects: Missing heartbeat indicates worker failure
//
// This provides a balance between quick crash detection (6 seconds) and tolerance
// for network hiccups (3 missed heartbeats before declaring failure).
package heartbeat
