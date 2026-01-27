# Parti Consumer Helpers

> JetStream consumer management for partitioned workloads.

**Related Documentation:**
- [User Guide](USER_GUIDE.md) - Getting started and overview
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
import "github.com/arloliu/parti/subscription"

// Create worker consumer
wc := subscription.NewWorkerConsumer(
    js,                           // JetStream context
    "ORDERS",                     // Stream name
    "orders-processor",           // Durable name
    "orders.>",                   // Subject pattern
    func(partID string) string {  // Partition to subject
        return fmt.Sprintf("orders.%s", partID)
    },
    subscription.WithBatchSize(50),
    subscription.WithAckWait(30*time.Second),
)

// Get consumer info for manager
consumerInfo := wc.ConsumerInfo()

// Register with manager for updates
mgr.SetConsumerUpdater(wc)

// Use in message handler
ctx, msgs, err := wc.Fetch(10)
if err != nil {
    return err
}
for _, msg := range msgs {
    // Process message
    msg.Ack()
}
```

### Options

```go
// Batch settings
subscription.WithBatchSize(100)           // Messages per fetch
subscription.WithAckWait(30*time.Second)  // Ack timeout

// Consumer configuration
subscription.WithMaxDeliver(5)            // Max redelivery attempts
subscription.WithMaxAckPending(1000)      // Max unacked messages

// Filtering
subscription.WithDeliverPolicy(nats.DeliverNew()) // Start from new messages
```

### Key Methods

| Method            | Description                                        |
|-------------------|----------------------------------------------------|
| `ConsumerInfo()`  | Returns consumer metadata for manager registration |
| `Update(ids)`     | Called by manager on assignment changes            |
| `Fetch(n)`        | Fetches up to n messages                           |
| `Messages()`      | Returns message channel for continuous processing  |
| `Stop()`          | Gracefully stops the consumer                      |
| `Drain()`         | Drains pending messages before stopping            |

### Assignment Updates

When the manager calls `Update([]string{"0", "1", "2"})`:

1. Builds subject filter: `["orders.0", "orders.1", "orders.2"]`
2. Updates JetStream consumer filter
3. New fetches receive only matching messages

### Error Handling

```go
ctx, msgs, err := wc.Fetch(10)
if err != nil {
    if errors.Is(err, nats.ErrTimeout) {
        // No messages available, retry
        continue
    }
    if errors.Is(err, subscription.ErrConsumerNotReady) {
        // Consumer being updated, wait
        time.Sleep(100 * time.Millisecond)
        continue
    }
    return err
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
// Create broadcast consumer
bc := subscription.NewBroadcastConsumer(
    js,                       // JetStream context
    "EVENTS",                 // Stream name
    "broadcast-events",       // Durable name
    "events.broadcast.>",     // Subject pattern (all partitions)
    subscription.WithBatchSize(10),
)

// Register with manager (receives all partitions)
mgr.SetBroadcastUpdater(bc)

// Handle broadcast messages
for msg := range bc.Messages() {
    handleBroadcast(msg)
    msg.Ack()
}
```

### Comparison with WorkerConsumer

```go
// WorkerConsumer: receives orders.0, orders.1 (if assigned)
wc := subscription.NewWorkerConsumer(js, "ORDERS", "worker", "orders.>",
    func(id string) string { return "orders." + id })

// BroadcastConsumer: receives all events.broadcast.* messages
bc := subscription.NewBroadcastConsumer(js, "EVENTS", "broadcast", "events.broadcast.>")
```

### Methods

`BroadcastConsumer` shares the same interface as `WorkerConsumer`:

| Method            | Description                              |
|-------------------|------------------------------------------|
| `ConsumerInfo()`  | Returns consumer metadata                |
| `Fetch(n)`        | Fetches up to n messages                 |
| `Messages()`      | Returns message channel                  |
| `Stop()`          | Gracefully stops the consumer            |

Note: `Update()` is a no-op for BroadcastConsumer since it always receives all messages.

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
import "github.com/arloliu/parti"

// Create individual consumers
ordersConsumer := subscription.NewWorkerConsumer(...)
paymentsConsumer := subscription.NewWorkerConsumer(...)
eventsConsumer := subscription.NewBroadcastConsumer(...)

// Combine into composite
composite := parti.NewCompositeConsumerUpdater(
    ordersConsumer,
    paymentsConsumer,
    eventsConsumer,
)

// Register single updater with manager
mgr.SetConsumerUpdater(composite)
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

The gate automatically updates when:
- `OnAssignment` hook fires (new assignment)
- `OnPartitionPrepare` hook fires (prepare phase starts)
- `OnPartitionCommit` hook fires (commit phase completes)

No manual synchronization needed—the gate stays in sync with the manager's state.
