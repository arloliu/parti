# Parti Consumer Helpers

> JetStream consumer management for partitioned workloads.

**Related Documentation:**
- [Docs README](README.md) - Documentation map
- [Architecture](ARCHITECTURE.md) - System architecture and concepts
- [Lifecycle Guide](LIFECYCLE.md) - Worker states and handoff
- [Strategies Guide](STRATEGIES.md) - Assignment strategies

---

## Table of Contents

1. [Overview](#overview)
2. [WorkerConsumer](#workerconsumer)
3. [BroadcastConsumer](#broadcastconsumer)
4. [CompositeConsumerUpdater](#compositeconsumerupdater)
5. [ProcessingGate](#processinggate)

---

## Overview

The `subscription` package provides helpers to integrate NATS JetStream consumers with Parti's partition management:

| Helper                    | Purpose                                       |
|---------------------------|-----------------------------------------------|
| `WorkerConsumer`          | Partition-filtered consumer for worker queues |
| `BroadcastConsumer`       | All-partition consumer for broadcasts         |
| `CompositeConsumerUpdater`| Manages multiple consumers as one unit        |

These helpers handle consumer lifecycle and filter updates automatically based on partition assignments.

### Import

```go
import "github.com/arloliu/parti/subscription"
```

---

## WorkerConsumer

`WorkerConsumer` creates a JetStream consumer that receives messages **only for assigned partitions**. When assignments change, it automatically updates the consumer's subject filter.

### Architecture

```
    ┌──────────────────────────────────────────────────────┐
    │                    WorkerConsumer                    │
    │                                                      │
    │   ┌─────────────┐    ┌─────────────┐                 │
    │   │  Partition  │    │  JetStream  │                 │
    │   │  Callback   │───▶│  Consumer   │                 │
    │   └─────────────┘    └──────┬──────┘                 │
    │                             │                        │
    │   Subject: "orders.>"       │  Filter: "orders.0",   │
    │                             │          "orders.1"    │
    └─────────────────────────────┼────────────────────────┘
                                  │
                                  ▼
                           ┌─────────────┐
                           │   Stream    │
                           │  "ORDERS"   │
                           └─────────────┘
```

### Basic Usage

```go
import (
    "context"
    "time"
    "github.com/arloliu/parti"
    "github.com/arloliu/parti/subscription"
    "github.com/nats-io/nats.go/jetstream"
)

// Configure worker consumer
cfg := subscription.WorkerConsumerConfig{
    StreamName:      "ORDERS",
    ConsumerPrefix:  "orders-processor",
    SubjectTemplate: "orders.{{.PartitionID}}",
    BatchSize:       50,
    AckWait:         30 * time.Second,
}

// Create message handler
handler := subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    // Process message - return nil for auto-ack, error for auto-nak
    return processMessage(msg)
})

// Create worker consumer
wc, err := subscription.NewWorkerConsumer(js, cfg, handler)
if err != nil {
    log.Fatal(err)
}

// Create partition source
partitions := []parti.Partition{{ID: "0"}, {ID: "1"}, {ID: "2"}}
src := source.NewStatic(partitions)

// Register with manager for automatic partition-based filter updates
mgr, _ := parti.NewManager(mgrCfg, js, src, strategy.NewConsistentHash(),
    parti.WithWorkerConsumerUpdater(wc),
)
```

### Configuration Options

```go
cfg := subscription.WorkerConsumerConfig{
    // Required fields
    StreamName:      "ORDERS",                      // JetStream stream name
    ConsumerPrefix:  "orders-processor",            // Durable name prefix
    SubjectTemplate: "orders.{{.PartitionID}}",     // Template for subjects

    // Batch settings
    BatchSize:    100,                // Messages per fetch (default: 1)
    FetchTimeout: 5 * time.Second,    // Max wait when pulling batch (default: 5s)

    // Consumer configuration
    AckWait:       30 * time.Second,  // Time before redelivery (default: 30s)
    MaxDeliver:    5,                 // Max redelivery attempts (default: -1 unlimited)
    MaxAckPending: 1000,              // Max unacked messages (default: 0 = server default)

    // Processing options
    ManualAck:     false,             // When true, handler must call msg.Ack/Nak
    DrainOnRemove: true,              // Graceful drain when partitions removed
}
```

### Key Methods

| Method                     | Description                                              |
|----------------------------|----------------------------------------------------------|
| `UpdateWorkerConsumer()`   | Called by manager on assignment changes                  |
| `Close(ctx)`               | Gracefully stops the consumer with context timeout       |

### Assignment Updates

When the manager calls `Update([]string{"0", "1", "2"})`:

1. Builds subject filter: `["orders.0", "orders.1", "orders.2"]`
2. Updates JetStream consumer filter
3. New fetches receive only matching messages

### Error Handling

The handler-based API simplifies error handling:

```go
// Handler returns error for auto-nak, nil for auto-ack
handler := subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    if err := processMessage(msg); err != nil {
        // Returning error triggers automatic Nak
        return fmt.Errorf("process failed: %w", err)
    }
    // Returning nil triggers automatic Ack
    return nil
})

// For manual ack control, use ManualAck: true in config
cfg := subscription.WorkerConsumerConfig{
    StreamName:      "ORDERS",
    ConsumerPrefix:  "processor",
    SubjectTemplate: "orders.{{.PartitionID}}",
    ManualAck:       true,  // Handler must call msg.Ack/Nak/Term
}
```

---

## BroadcastConsumer

`BroadcastConsumer` creates a JetStream consumer that receives messages for **all partitions**, regardless of the worker's assignment. Useful for configuration updates, cache invalidation, or global events.

### When to Use

| Use Case                    | Consumer Type    |
|-----------------------------|------------------|
| Process assigned orders     | WorkerConsumer   |
| Invalidate cache globally   | BroadcastConsumer|
| Receive config updates      | BroadcastConsumer|
| Process user-specific data  | WorkerConsumer   |

### Basic Usage

```go
import (
    "context"
    "github.com/arloliu/parti"
    "github.com/arloliu/parti/subscription"
    "github.com/nats-io/nats.go/jetstream"
)

// Configure broadcast consumer
cfg := subscription.BroadcastConsumerConfig{
    StreamName:     "EVENTS",
    ConsumerPrefix: "broadcast-events",
    WildcardFilter: "events.broadcast.>",  // Receives ALL matching messages
    BatchSize:      10,
}

// Create message handler
handler := subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    handleBroadcast(msg)
    return nil
})

// Create broadcast consumer
bc, err := subscription.NewBroadcastConsumer(js, cfg, handler)
if err != nil {
    log.Fatal(err)
}
defer bc.Close(context.Background())

// Create partition source
partitions := []parti.Partition{{ID: "0"}, {ID: "1"}, {ID: "2"}}
src := source.NewStatic(partitions)

// Register with manager (partition updates are ignored - receives all messages)
mgr, _ := parti.NewManager(mgrCfg, js, src, strategy.NewConsistentHash(),
    parti.WithWorkerConsumerUpdater(bc),
)
```

### Comparison with WorkerConsumer

```go
// WorkerConsumer: receives only assigned partitions (e.g., orders.0, orders.1)
wcCfg := subscription.WorkerConsumerConfig{
    StreamName:      "ORDERS",
    ConsumerPrefix:  "worker",
    SubjectTemplate: "orders.{{.PartitionID}}",
}
wc, _ := subscription.NewWorkerConsumer(js, wcCfg, handler)

// BroadcastConsumer: receives ALL messages matching the wildcard filter
bcCfg := subscription.BroadcastConsumerConfig{
    StreamName:     "EVENTS",
    ConsumerPrefix: "broadcast",
    WildcardFilter: "events.broadcast.>",
}
bc, _ := subscription.NewBroadcastConsumer(js, bcCfg, handler)
```

### Methods

Both `WorkerConsumer` and `BroadcastConsumer` implement `WorkerConsumerUpdater`:

| Method                     | Description                                              |
|----------------------------|----------------------------------------------------------|
| `UpdateWorkerConsumer()`   | For BroadcastConsumer, starts the loop (ignores partitions) |
| `Close(ctx)`               | Gracefully stops the consumer                            |

Note: `UpdateWorkerConsumer()` ignores the partition list for BroadcastConsumer since it always receives all messages matching the wildcard filter.

---

## CompositeConsumerUpdater

`CompositeConsumerUpdater` manages multiple consumers as a single unit, simplifying updates when a worker has multiple consumer types.

### Use Cases

- Multiple streams with different partition schemes
- Mixed worker and broadcast consumers
- Coordinated lifecycle management

### Architecture

```
    ┌─────────────────────────────────────────────────────────┐
    │               CompositeConsumerUpdater                   │
    │                                                          │
    │  ┌───────────────┐  ┌───────────────┐  ┌──────────────┐ │
    │  │WorkerConsumer │  │WorkerConsumer │  │BroadcastConsumer│ │
    │  │   (orders)    │  │   (payments)  │  │   (events)   │ │
    │  └───────────────┘  └───────────────┘  └──────────────┘ │
    │                                                          │
    │        Update() propagates to all children               │
    └─────────────────────────────────────────────────────────┘
```

### Basic Usage

```go
import (
    "github.com/arloliu/parti"
    "github.com/arloliu/parti/subscription"
)

// Create individual consumers with their configs
ordersConsumer, _ := subscription.NewWorkerConsumer(js, ordersCfg, ordersHandler)
paymentsConsumer, _ := subscription.NewWorkerConsumer(js, paymentsCfg, paymentsHandler)
eventsConsumer, _ := subscription.NewBroadcastConsumer(js, eventsCfg, eventsHandler)

// Combine into composite updater
composite := parti.NewCompositeConsumerUpdater(
    ordersConsumer,
    paymentsConsumer,
    eventsConsumer,
)

// Create partition source
partitions := []parti.Partition{{ID: "0"}, {ID: "1"}, {ID: "2"}}
src := source.NewStatic(partitions)

// Register single updater with manager
mgr, _ := parti.NewManager(cfg, js, src, strategy.NewConsistentHash(),
    parti.WithWorkerConsumerUpdater(composite),
)
```

### Update Propagation

When partition assignments change:

```go
// Manager calls composite.Update(ctx, []string{"0", "1", "2"})
// Which propagates to:
//   - ordersConsumer.Update(ctx, []string{"0", "1", "2"})
//   - paymentsConsumer.Update(ctx, []string{"0", "1", "2"})
//   - eventsConsumer.Update(ctx, []string{"0", "1", "2"})  // (no-op for broadcast)
```

### Error Handling

If any child consumer fails to update:

```go
func (c *CompositeConsumerUpdater) Update(ctx context.Context, ids []string) error {
    var errs []error
    for _, updater := range c.updaters {
        if err := updater.Update(ctx, ids); err != nil {
            errs = append(errs, err)
        }
    }
    return errors.Join(errs...)
}
```

### Methods

| Method            | Description                           |
|-------------------|---------------------------------------|
| `Update(ctx,ids)` | Propagates to all child updaters      |
| `Add(updater)`    | Adds a new consumer to the composite  |
| `Remove(updater)` | Removes a consumer from the composite |

---

## ProcessingGate

`ProcessingGate` provides safe message processing during partition reassignment. It prevents processing messages for partitions that are being handed off.

### The Problem

During two-phase handoff:
1. Leader sends `Prepare` for partition P1 to move from Worker A to Worker B
2. Worker A must stop processing P1 immediately
3. Worker A might still have in-flight messages for P1

### How ProcessingGate Solves This

```
    ┌─────────────────────────────────────────────────────────┐
    │                   ProcessingGate                         │
    │                                                          │
    │   Active Partitions: {0, 1, 2}                          │
    │   Pending Handoff:   {1}        ◄── P1 being moved      │
    │                                                          │
    │   AllowProcessing("0") → true   ✓ Process                │
    │   AllowProcessing("1") → false  ✗ Skip (handoff)        │
    │   AllowProcessing("2") → true   ✓ Process                │
    │                                                          │
    └─────────────────────────────────────────────────────────┘
```

### Basic Usage

```go
// Create gate with manager
gate := subscription.NewProcessingGate(mgr)

// In message handler
func handleMessage(msg *nats.Msg) {
    partitionID := extractPartitionID(msg.Subject)

    // Check if processing is allowed
    if !gate.AllowProcessing(partitionID) {
        // Re-queue or skip - partition is being handed off
        msg.Nak()
        return
    }

    // Safe to process
    processMessage(msg)
    msg.Ack()
}
```

### Integration with Consumer

```go
func processLoop(wc *subscription.WorkerConsumer, gate *subscription.ProcessingGate) {
    for msg := range wc.Messages() {
        partitionID := extractPartitionID(msg.Subject)

        if !gate.AllowProcessing(partitionID) {
            msg.NakWithDelay(1 * time.Second)  // Retry later
            continue
        }

        if err := process(msg); err != nil {
            msg.Nak()
            continue
        }

        msg.Ack()
    }
}
```

### Thread Safety

`ProcessingGate` is fully thread-safe:

```go
// Safe to call from multiple goroutines
go func() { gate.AllowProcessing("0") }()
go func() { gate.AllowProcessing("1") }()
```

### Methods

| Method                    | Description                              |
|---------------------------|------------------------------------------|
| `AllowProcessing(id)`     | Returns true if partition can be processed |
| `GetActivePartitions()`   | Returns list of currently active partitions |
| `GetPendingHandoffs()`    | Returns list of partitions being handed off |

### Automatic Updates

The gate makes decisions based on an ownership resolver (partition owner + handoff state).

When used via `WorkerConsumer` with `ProcessingGate.Enabled = true`, the worker consumer will
auto-create and manage a claim-based resolver backed by the handoff KV bucket and keep it
up to date via KV watching. No manager hooks are required.
