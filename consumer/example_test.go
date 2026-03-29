package consumer_test

import (
	"context"
	"fmt"
	"time"

	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// This example demonstrates how to create a Queue consumer for load-balanced
// message processing across multiple worker instances.
//
// In production, you would obtain js from nats.Connect + jetstream.New.
func Example_queueConsumer() {
	// Placeholder - in production, obtain from nats.Connect + jetstream.New
	var js jetstream.JetStream

	handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		fmt.Printf("Processing job: %s\n", string(msg.Data()))
		return nil // auto-ack on success
	})

	q, err := consumer.NewQueue(
		js,
		"JOBS",          // streamName
		"job-processor", // consumerName (shared across replicas)
		"jobs.>",        // filterSubject
		handler,
		consumer.WithAckWait(30*time.Second),
		consumer.WithBatchSize(10),
	)
	if err != nil {
		fmt.Printf("Failed to create queue: %v\n", err)
		return
	}

	ctx := context.Background()

	if err := q.Start(ctx); err != nil {
		fmt.Printf("Failed to start: %v\n", err)
		return
	}

	// Consumer is now processing messages in the background.
	// In a real application, wait for shutdown signal, then:
	_ = q.Stop(ctx)

	// Output: Failed to create queue: invalid consumer configuration: JetStream context is required
}

// This example demonstrates how to create a Static consumer for StatefulSet
// deployments where each pod handles a fixed partition.
func Example_staticConsumer() {
	var js jetstream.JetStream

	handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		fmt.Printf("Processing event: %s\n", string(msg.Data()))
		return nil
	})

	// Pod ordinal determines partition (e.g., pod-0 → partition 0)
	podOrdinal := 0
	numPartitions := 10

	c, err := consumer.NewStatic(
		js,
		"EVENTS",               // streamName
		"processor-0",          // consumerName (unique per partition)
		"events.{{partition}}", // subjectPattern
		numPartitions,          // total partitions
		podOrdinal,             // this pod's partition
		handler,
	)
	if err != nil {
		fmt.Printf("Failed to create static consumer: %v\n", err)
		return
	}

	ctx := context.Background()

	if err := c.Start(ctx); err != nil {
		fmt.Printf("Failed to start: %v\n", err)
		return
	}

	// This consumer only receives messages for partition 0
	fmt.Printf("Consuming partition %d, subject: %s\n", c.Partition(), c.Subject())

	// Cleanup
	_ = c.Stop(ctx)

	// Output: Failed to create static consumer: invalid consumer configuration: JetStream context is required
}

// This example demonstrates how to create a Dynamic consumer that receives
// partition assignments from the Parti Manager.
func Example_dynamicConsumer() {
	var js jetstream.JetStream

	handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		fmt.Printf("Processing order event: %s\n", string(msg.Data()))
		return nil
	})

	c, err := consumer.NewDynamic(
		js,
		"ORDERS",                  // streamName
		"worker",                  // consumerPrefix
		"orders.{{.PartitionID}}", // subjectTemplate (Go template syntax)
		handler,
	)
	if err != nil {
		fmt.Printf("Failed to create dynamic consumer: %v\n", err)
		return
	}

	ctx := context.Background()

	// In a real application, the Parti Manager calls Update automatically.
	// Here we demonstrate manual usage:
	partitions := []types.Partition{
		{Keys: []string{"partition-0"}},
		{Keys: []string{"partition-1"}},
	}

	if err := c.Update(ctx, "worker-0", partitions); err != nil {
		fmt.Printf("Failed to update: %v\n", err)
		return
	}

	// Consumer is now processing messages for partition-0 and partition-1
	// Cleanup
	_ = c.Stop(ctx)

	// Output: Failed to create dynamic consumer: invalid consumer configuration: JetStream context is required
}

// This example demonstrates how to create a Broadcast consumer where every
// instance receives every message (fan-out pattern).
func Example_broadcastConsumer() {
	var js jetstream.JetStream

	handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		fmt.Printf("Cache invalidation: %s\n", string(msg.Data()))
		return nil
	})

	c, err := consumer.NewBroadcast(
		js,
		"CACHE-EVENTS",  // streamName
		"cache-updater", // consumerPrefix
		"cache.>",       // filterSubject (wildcard for all cache events)
		handler,
		consumer.WithInstanceID("pod-abc123"), // unique per instance
	)
	if err != nil {
		fmt.Printf("Failed to create broadcast consumer: %v\n", err)
		return
	}

	ctx := context.Background()

	if err := c.Start(ctx); err != nil {
		fmt.Printf("Failed to start: %v\n", err)
		return
	}

	// Every instance running this code receives all cache.> messages
	// Cleanup
	_ = c.Stop(ctx)

	// Output: Failed to create broadcast consumer: invalid consumer configuration: JetStream context is required
}

// This example demonstrates how to use the MessageHandlerFunc adapter to
// convert a function into a MessageHandler.
func Example_messageHandler() {
	// Simple handler that processes and auto-acks
	simple := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		data := msg.Data()
		// Process data...
		_ = data
		return nil // nil = auto-ack
	})

	// Handler that explicitly signals failure (triggers NAK)
	withError := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		if err := processMessage(msg); err != nil {
			return err // non-nil = auto-NAK
		}
		return nil
	})

	// Handler for manual ack mode (requires WithManualAck option)
	manual := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		if err := processMessage(msg); err != nil {
			_ = msg.Term() // terminal failure, don't redeliver
			return err
		}
		_ = msg.Ack() // explicit ack
		return nil
	})

	// All handlers implement MessageHandler interface
	fmt.Printf("Created %d handlers\n", 3)

	// Use any handler with a consumer constructor
	_ = simple
	_ = withError
	_ = manual
	// Output: Created 3 handlers
}

func processMessage(_ jetstream.Msg) error {
	// Placeholder for message processing logic
	return nil
}

// This example demonstrates how to wrap a message handler with automatic
// heartbeats for long-running processing using WIPHandler.
//
// WIPHandler periodically calls msg.InProgress() to extend the AckWait deadline,
// preventing JetStream from redelivering messages while processing is still active.
func Example_wipHandler() {
	// Base handler that performs long-running processing
	slowHandler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		// Simulate long-running work (e.g., ML inference, large file processing)
		// This could take 30+ seconds
		fmt.Println("Starting long-running processing...")

		// Normally this would timeout and redeliver, but WIPHandler
		// sends InProgress() every 10 seconds to keep it alive
		// time.Sleep(45 * time.Second)

		fmt.Println("Processing complete")

		return nil
	})

	// Wrap with WIPHandler for automatic heartbeats
	// Interval should be < AckWait/2 (e.g., AckWait=30s, Interval=10s)
	wrappedHandler := consumer.NewWIPHandler(slowHandler, consumer.WIPConfig{
		Interval: 10 * time.Second,
		// Logger: myLogger, // Optional: log heartbeat errors
	})

	// Use with any consumer type
	var js jetstream.JetStream
	_, err := consumer.NewQueue(
		js,
		"LONG-JOBS",
		"slow-processor",
		"jobs.slow.>",
		wrappedHandler,
		consumer.WithAckWait(30*time.Second), // Heartbeat at 10s < 30s/2
	)
	if err != nil {
		fmt.Printf("Failed to create queue: %v\n", err)
		return
	}

	// Output: Failed to create queue: invalid consumer configuration: JetStream context is required
}

// This example demonstrates using WIPHandler with Dynamic consumer
// for partition-aware long-running processing.
func Example_wipHandlerWithDynamic() {
	// Handler that processes large datasets per partition
	handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		fmt.Printf("Processing batch for subject: %s\n", msg.Subject())
		// Long-running batch processing...
		return nil
	})

	// Wrap with heartbeats
	wrapped := consumer.NewWIPHandler(handler, consumer.WIPConfig{
		Interval: 15 * time.Second, // AckWait is 60s, so 15s is safe (< 30s)
	})

	var js jetstream.JetStream
	_, err := consumer.NewDynamic(
		js,
		"BATCH-EVENTS",
		"batch-worker",
		"batch.{{.PartitionID}}.events",
		wrapped,
		consumer.WithAckWait(60*time.Second),
	)
	if err != nil {
		fmt.Printf("Failed to create dynamic consumer: %v\n", err)
		return
	}

	// Output: Failed to create dynamic consumer: invalid consumer configuration: JetStream context is required
}
