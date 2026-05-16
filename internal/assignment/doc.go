// Package assignment provides partition assignment calculation and distribution.
//
// The assignment package implements the leader-based assignment coordination
// system. The Calculator runs only on the leader worker and is responsible for:
//
//   - Monitoring worker health via heartbeat tracking
//   - Calculating partition assignments using configured strategies
//   - Publishing assignments to NATS KV for worker discovery
//   - Handling rebalancing on worker join/leave events
//   - Applying rebalance cooldowns to prevent thrashing
//
// # Design Overview
//
// The assignment system uses a leader-based approach where one worker (the leader)
// is responsible for calculating and distributing assignments to all workers:
//
//  1. Leader monitors worker heartbeats in NATS KV
//  2. Leader detects worker join/leave by tracking heartbeat changes
//  3. Leader calculates new assignments using AssignmentStrategy
//  4. Leader publishes assignments via the refs-always commit protocol (three
//     protocol keys: _commit, _commit_log.<V>, _payload.<hex>)
//  5. Workers watch assignment._commit and fetch their payload by key
//
// # Calculator Lifecycle
//
// The Calculator should only be started on the leader worker:
//
//  1. Create calculator with NewCalculator(&Config{...})
//  2. Start calculator with Start(ctx) (performs initial assignment)
//  3. Calculator monitors workers in background
//  4. Stop calculator with Stop() when stepping down from leadership
//
// Example:
//
//	// Leader worker creates and starts calculator
//	calc, err := assignment.NewCalculator(&assignment.Config{
//	    AssignmentKV:         assignmentKV,
//	    HeartbeatKV:          heartbeatKV,
//	    AssignmentPrefix:     "assignment",
//	    Source:               partitionSource,
//	    Strategy:             assignmentStrategy,
//	    HeartbeatPrefix:      "worker-hb",
//	    HeartbeatTTL:         15 * time.Second,
//	    EmergencyGracePeriod: 7500 * time.Millisecond,
//	    Cooldown:             10 * time.Second,
//	    ColdStartWindow:      30 * time.Second,
//	    PlannedScaleWindow:   10 * time.Second,
//	})
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	// Start assignment calculation
//	err = calc.Start(ctx)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer calc.Stop(ctx)
//
//	// Calculator now monitors workers and triggers rebalancing
//
// # Rebalancing Strategy
//
// The calculator uses adaptive stabilization windows to balance responsiveness
// and stability:
//
//   - Cold start detection: Many workers appear at once (>50% of expected)
//   - Cold start window: 30 seconds (wait for most workers to start)
//   - Planned scale window: 10 seconds (quick response to single worker changes)
//   - Rebalance cooldown: 10 seconds (prevent excessive rebalancing)
//
// # Assignment Distribution
//
// Assignments are published to NATS KV using a three-key commit-publisher
// protocol (refs-always model):
//
//   - "{prefix}._commit" — current commit object mapping each worker ID to the
//     content-addressable payload key that holds its assignment slice.
//   - "{prefix}._commit_log.<V>" — append-only commit-log entry per assignment
//     version V; used by the GC to determine which payload keys are live.
//   - "{prefix}._payload.<hex(sha256)>" — content-addressable assignment blobs.
//     Identical assignment slices for different workers share a single payload key.
//
// Workers watch "{prefix}._commit" and fetch the payload keys referenced by
// their entry. A background GC pass (CommitGC) reaps payload keys that are not
// referenced by any recent commit-log entry.
//
// # Worker Health Monitoring
//
// The calculator monitors worker health by:
//
//   - Listing all heartbeat keys (format: "{hbPrefix}.{workerID}")
//   - Extracting worker IDs from heartbeat keys
//   - Detecting changes in the worker set (joins/leaves)
//   - Triggering rebalancing when worker set changes
//
// Workers are considered active if their heartbeat key exists in NATS KV.
// The heartbeat TTL ensures dead workers are automatically removed.
//
// # Thread Safety
//
// The Calculator is thread-safe and can be accessed concurrently from multiple
// goroutines. All public methods use proper synchronization to protect internal
// state.
//
// # Configuration Options
//
// The calculator supports several configuration options:
//
//   - Cooldown: Minimum time between rebalances (default: 10s)
//   - RestartRatio: Fraction of workers indicating cold start (default: 0.5)
//   - ColdStartWindow: Stabilization time for cold starts (default: 30s)
//   - PlannedScaleWindow: Stabilization time for planned scaling (default: 10s)
//
// # Integration with Manager
//
// The Calculator is integrated into the Manager's leader election logic:
//
//  1. Worker wins election and becomes leader
//  2. Manager creates and starts Calculator
//  3. Calculator performs initial assignment
//  4. Calculator monitors workers and handles rebalancing
//  5. Worker loses election and steps down
//  6. Manager stops Calculator
//
// Only the leader worker runs the Calculator. Follower workers watch
// assignment._commit in NATS KV and fetch their payload by key on each update.
package assignment
