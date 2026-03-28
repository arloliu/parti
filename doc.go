// Package parti provides a Go library for NATS-based work partitioning with stable worker IDs
// and leader-based coordination.
//
// Parti enables distributed systems to dynamically assign work partitions across worker instances
// without requiring external coordination services. It provides stable worker identities,
// cache-affinity-aware rebalancing, and adaptive stabilization for different scaling scenarios.
//
// # Quick Start
//
// Basic usage with default settings:
//
//	import (
//	    "github.com/arloliu/parti/v2"
//	    "github.com/arloliu/parti/v2/source"
//	    "github.com/arloliu/parti/v2/strategy"
//	)
//
//	cfg := parti.Config{
//	    WorkerIDPrefix: "worker",
//	    WorkerIDMin:    0,
//	    WorkerIDMax:    999,
//	}
//
//	partitions := []parti.Partition{{Keys: []string{"0"}}, {Keys: []string{"1"}}, {Keys: []string{"2"}}}
//	src := source.NewStatic(partitions)
//	js, _ := jetstream.New(natsConn)
//	assignmentStrategy := strategy.NewConsistentHash()
//	mgr, _ := parti.NewManager(&cfg, js, src, assignmentStrategy)
//
//	if err := mgr.Start(ctx); err != nil {
//	    log.Fatal(err)
//	}
//	defer mgr.Stop(context.Background())
//
// # Key Features
//
//   - Stable Worker IDs: Workers claim stable IDs for consistent assignment during rolling updates
//   - Leader-Based Assignment: One worker calculates assignments without external coordination
//   - Adaptive Rebalancing: Different stabilization windows for cold start (30s) vs planned scale (10s)
//   - Cache Affinity: Preserves >80% partition locality during rebalancing
//   - Weighted Assignment: Supports partition weights for load balancing
//
// # Architecture
//
// Workers progress through a state machine:
//
//	INIT → CLAIMING_ID → ELECTION → WAITING_ASSIGNMENT → STABLE
//
// The leader monitors heartbeats, detects topology changes, and publishes new assignments.
// All workers watch for assignment updates and trigger callbacks when their partitions change.
//
// # Advanced Usage
//
// Custom strategy with options:
//
//	import (
//	    "github.com/arloliu/parti/v2"
//	    "github.com/arloliu/parti/v2/source"
//	    "github.com/arloliu/parti/v2/strategy"
//	)
//
//	assignmentStrategy := strategy.NewConsistentHash(
//	    strategy.WithVirtualNodes(300),
//	)
//
//	hooks := &parti.Hooks{
//	    OnAssignmentChanged: func(ctx context.Context, oldPartitions, newPartitions []parti.Partition) error {
//	        // Handle full assignment change; derive added/removed by diffing old vs new if needed.
//	        return nil
//	    },
//	}
//
//	partitions := []parti.Partition{{Keys: []string{"0"}}, {Keys: []string{"1"}}, {Keys: []string{"2"}}}
//	src := source.NewStatic(partitions)
//	js, _ := jetstream.New(natsConn)
//	mgr, _ := parti.NewManager(&cfg, js, src, assignmentStrategy,
//	    parti.WithHooks(hooks),
//	)
//
// See the examples/ directory for complete working examples.
package parti
