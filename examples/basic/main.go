// Package main demonstrates basic Parti usage with default settings.
//
// This example shows:
//   - Creating a runtime config with defaults
//   - Setting up a static partition source
//   - Initializing a Manager with minimal configuration
//   - Handling assignment changes via hooks
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func main() {
	// Connect to NATS
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = nats.DefaultURL
	}

	nc, err := nats.Connect(natsURL)
	if err != nil {
		log.Fatalf("Failed to connect to NATS: %v", err)
	}
	defer nc.Close()

	// Create configuration with defaults
	cfg := parti.Config{
		WorkerIDPrefix: "example-worker",
		WorkerIDMin:    0,
		WorkerIDMax:    9,
	}

	// Define partitions (in real scenario, use dynamic source like Cassandra)
	partitions := []parti.Partition{
		{Keys: []string{"partition-0"}, Weight: 100},
		{Keys: []string{"partition-1"}, Weight: 100},
		{Keys: []string{"partition-2"}, Weight: 100},
		{Keys: []string{"partition-3"}, Weight: 100},
		{Keys: []string{"partition-4"}, Weight: 100},
	}

	// Create static partition source
	src := source.NewStatic(partitions)

	// Create assignment strategy
	strat := strategy.NewConsistentHash()

	// Set up hooks to handle assignment changes
	hooks := &parti.Hooks{
		OnAssignmentChanged: func(ctx context.Context, added, removed []parti.Partition) error {
			fmt.Printf("Assignment changed:\n")
			fmt.Printf("  Added: %d partitions\n", len(added))
			for _, p := range added {
				fmt.Printf("    + %v\n", p.Keys)
			}
			fmt.Printf("  Removed: %d partitions\n", len(removed))
			for _, p := range removed {
				fmt.Printf("    - %v\n", p.Keys)
			}
			return nil
		},
		OnStateChanged: func(ctx context.Context, from, to parti.State) error {
			fmt.Printf("State transition: %d -> %d\n", from, to)
			return nil
		},
	}

	// Create JetStream context then Manager with strategy
	js, err := jetstream.New(nc)
	if err != nil {
		log.Fatalf("Failed to init JetStream: %v", err)
	}

	mgr, err := parti.NewManager(&cfg, js, src, strat, parti.WithHooks(hooks))
	if err != nil {
		log.Fatalf("Failed to create manager: %v", err)
	}

	// Start manager (blocks until initialized)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Println("Starting Parti Manager...")
	if err := mgr.Start(ctx); err != nil {
		log.Fatalf("Failed to start manager: %v", err)
	}

	fmt.Printf("Manager started successfully!\n")
	fmt.Printf("Worker ID: %s\n", mgr.WorkerID())
	fmt.Printf("Is Leader: %v\n", mgr.IsLeader())

	// Print current assignment
	assignment := mgr.CurrentAssignment()
	fmt.Printf("Current assignment (version %d):\n", assignment.Version)
	for _, p := range assignment.Partitions {
		fmt.Printf("  - %v (weight: %d)\n", p.Keys, p.Weight)
	}

	// Wait for interrupt signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("\nManager running. Press Ctrl+C to stop.")
	<-sigCh

	// Graceful shutdown
	fmt.Println("\nShutting down...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := mgr.Stop(shutdownCtx); err != nil {
		log.Printf("Error during shutdown: %v", err)
	}

	fmt.Println("Shutdown complete.")
}
