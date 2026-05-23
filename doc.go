// Package parti is a Go library for building partitioned workloads on NATS. It provides a
// complete toolkit for sharding work across workers: dynamic partitioning with leader-coordinated
// rebalancing, static partitioning for fixed-topology deployments (e.g. Kubernetes StatefulSets),
// and resilient JetStream consumers with auto-recovery from durable deletion.
//
// Its headline capability is solving the coordination gap NATS leaves open when both the worker
// fleet and the partition set change at runtime — providing stable worker identities,
// cache-affinity-aware rebalancing, and adaptive stabilization without external coordination
// services. Static partitioning and consumer auto-recovery live in the partition and consumer
// subpackages respectively.
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
//	// Connect with MaxReconnects(-1) so the client rides through transient
//	// NATS outages instead of going CLOSED. See docs/OPERATIONS.md
//	// "NATS Client Connection" for the full posture.
//	natsConn, _ := nats.Connect(natsURL, nats.MaxReconnects(-1), nats.RetryOnFailedConnect(true))
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
//   - Static Partitioning: Zero-coordination routing for StatefulSet-style deployments (partition subpackage)
//   - Consumer Auto-Recovery: JetStream consumers detect and recreate deleted durables automatically (consumer subpackage)
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
