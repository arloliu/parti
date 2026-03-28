package parti_test

import (
	"context"
	"fmt"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/nats-io/nats.go/jetstream"
)

// This example demonstrates the basic usage of NewManager with default settings.
// In production, obtain js from nats.Connect + jetstream.New.
func ExampleNewManager() {
	// In production, obtain from nats.Connect + jetstream.New
	var js jetstream.JetStream

	cfg := &parti.Config{
		WorkerIDPrefix: "worker",
		WorkerIDMin:    0,
		WorkerIDMax:    999,
	}

	partitions := []parti.Partition{
		{Keys: []string{"orders", "0"}},
		{Keys: []string{"orders", "1"}},
		{Keys: []string{"orders", "2"}},
	}
	src := source.NewStatic(partitions)
	hashStrategy := strategy.NewConsistentHash()

	mgr, err := parti.NewManager(cfg, js, src, hashStrategy)
	if err != nil {
		return // requires live NATS; see integration tests for full examples
	}
	defer func() { _ = mgr.Stop(context.Background()) }()

	if err := mgr.Start(context.Background()); err != nil {
		// Always call Stop on Start failure to release resources.
		return
	}

	fmt.Printf("Worker started with ID: %s\n", mgr.WorkerID())
	// Output:
}

// This example demonstrates using NewManager with hooks for lifecycle notifications.
func ExampleNewManager_withHooks() {
	var js jetstream.JetStream

	hooks := &parti.Hooks{
		OnAssignmentChanged: func(ctx context.Context, oldPartitions, newPartitions []parti.Partition) error {
			fmt.Printf("Assignment changed: %d -> %d partitions\n",
				len(oldPartitions), len(newPartitions))
			return nil
		},
		OnStateChanged: func(ctx context.Context, from, to parti.State) error {
			fmt.Printf("State: %s -> %s\n", from, to)
			return nil
		},
		OnLeadershipChanged: func(ctx context.Context, isLeader bool) error {
			fmt.Printf("Leadership changed: isLeader=%v\n", isLeader)
			return nil
		},
	}

	cfg := &parti.Config{
		WorkerIDPrefix: "worker",
		WorkerIDMax:    10,
	}
	src := source.NewStatic([]parti.Partition{{Keys: []string{"p0"}}})
	hashStrategy := strategy.NewConsistentHash()

	_, err := parti.NewManager(cfg, js, src, hashStrategy,
		parti.WithHooks(hooks),
	)
	if err != nil {
		return // requires live NATS; see integration tests for full examples
	}
	// Output:
}

// This example demonstrates using NewManager with a custom strategy.
func ExampleNewManager_withStrategy() {
	var js jetstream.JetStream

	cfg := &parti.Config{
		WorkerIDPrefix: "processor",
		WorkerIDMax:    50,
	}

	partitions := []parti.Partition{
		{Keys: []string{"shard-0"}, Weight: 100},
		{Keys: []string{"shard-1"}, Weight: 200},
		{Keys: []string{"shard-2"}, Weight: 150},
	}
	src := source.NewStatic(partitions)

	// Use consistent hash with more virtual nodes for better distribution
	hashStrategy := strategy.NewConsistentHash(
		strategy.WithVirtualNodes(300),
	)

	_, err := parti.NewManager(cfg, js, src, hashStrategy)
	if err != nil {
		return // requires live NATS; see integration tests for full examples
	}
	// Output:
}

// This example demonstrates using DefaultConfig as a starting point
// and customizing specific fields.
func ExampleDefaultConfig() {
	cfg := parti.DefaultConfig()

	// Override only what you need
	cfg.WorkerIDPrefix = "my-service"
	cfg.WorkerIDMax = 50
	cfg.HeartbeatInterval = 5 * time.Second
	cfg.HeartbeatTTL = 15 * time.Second

	fmt.Printf("Prefix: %s, MaxWorkers: %d, HeartbeatInterval: %v, HeartbeatTTL: %v\n",
		cfg.WorkerIDPrefix, cfg.WorkerIDMax+1, cfg.HeartbeatInterval, cfg.HeartbeatTTL)
	// Output: Prefix: my-service, MaxWorkers: 51, HeartbeatInterval: 5s, HeartbeatTTL: 15s
}

// This example demonstrates using SetDefaults to fill in default values
// for a partially configured Config, such as one loaded from YAML.
func ExampleSetDefaults() {
	// Simulate a Config loaded from YAML with only some fields set
	cfg := parti.Config{
		WorkerIDPrefix: "custom-worker",
		WorkerIDMax:    50,
	}

	// Fill remaining fields with defaults
	if err := parti.SetDefaults(&cfg); err != nil {
		fmt.Printf("Failed to set defaults: %v\n", err)
		return
	}

	fmt.Printf("Prefix: %s, HeartbeatInterval: %v\n", cfg.WorkerIDPrefix, cfg.HeartbeatInterval)
	// Output: Prefix: custom-worker, HeartbeatInterval: 2s
}

// This example demonstrates composing multiple WorkerConsumerUpdater
// instances using CompositeConsumerUpdater.
func ExampleNewCompositeConsumerUpdater() {
	// In production, these would be real consumer instances (e.g., Dynamic, Broadcast).
	// NewCompositeConsumerUpdater filters nil updaters, so only non-nil ones are kept.
	composite := parti.NewCompositeConsumerUpdater( /* updater1, updater2 */ )

	// Add updaters later via Add
	fmt.Printf("Composite updater created, initial count: %d\n", composite.Len())
	// Output: Composite updater created, initial count: 0
}
