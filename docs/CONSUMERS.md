# Parti Consumer Package

> Unified JetStream consumer types for partitioned workloads.

**Related Documentation:**
- [Docs README](README.md) - Documentation map
- [Architecture](ARCHITECTURE.md) - System architecture and concepts
- [Lifecycle Guide](LIFECYCLE.md) - Worker states and handoff
- [Strategies Guide](STRATEGIES.md) - Assignment strategies

---

## Table of Contents

1. [Overview](#overview)
2. [Quick Start](#quick-start)
3. [Consumer Types](#consumer-types)
   - [Queue](#queue)
   - [Static](#static)
   - [Dynamic](#dynamic)
   - [Broadcast](#broadcast)
4. [Message Handler](#message-handler)
5. [Functional Options](#functional-options)
6. [Migrating from Legacy Packages](#migrating-from-legacy-packages)
7. [Legacy Packages (Deprecated)](#legacy-packages-deprecated)

---

## Overview

The `consumer` package provides a unified API for JetStream consumers in partitioned workloads:

| Consumer    | Purpose                                    | Coordination | Lifecycle        |
|-------------|--------------------------------------------|--------------|------------------|
| `Queue`     | Load-balanced workers (queue group)        | None         | Start → Stop     |
| `Static`    | Fixed partition (StatefulSet ordinal)      | None         | Start → Stop     |
| `Dynamic`   | Manager-assigned partitions (Parti core)   | Via Manager  | Update → Stop    |
| `Broadcast` | Fan-out to all instances                   | None         | Start → Stop     |

### Import

```go
import "github.com/arloliu/parti/consumer"
```

> **Migration Note:** The `consumer` package replaces the legacy `subscription` and `partition`
> packages, which are now deprecated. See [Migrating from Legacy Packages](#migrating-from-legacy-packages).

---

## Quick Start

### Queue Consumer (Load-Balanced Workers)

```go
handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    fmt.Printf("Processing: %s\n", msg.Subject())
    return nil // auto-ack
})

c, err := consumer.NewQueue(js, "JOBS", "job-workers", "jobs.>", handler)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

if err := c.Start(ctx); err != nil {
    log.Fatal(err)
}
```

### Dynamic Consumer (Parti Manager)

```go
c, err := consumer.NewDynamic(js, "ORDERS", "order-processor", "orders.{{.PartitionID}}", handler)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

// Register with Parti Manager for automatic partition assignment
mgr, _ := parti.NewManager(cfg, js, src, strategy.NewConsistentHash(),
    parti.WithWorkerConsumerUpdater(c),
)
```

---

## Consumer Types

### Queue

Load-balanced consumer where multiple instances share one durable consumer. Each message is delivered to exactly one instance (queue group semantics).

**Use Cases:**
- Classic worker queue patterns
- Stateless message processing
- Horizontal scaling without coordination

**Lifecycle:** `Start(ctx) → Stop(ctx)`

```go
c, err := consumer.NewQueue(
    js,              // jetstream.JetStream
    "JOBS",          // streamName
    "job-workers",   // consumerName (shared across instances)
    "jobs.>",        // filterSubject
    handler,         // MessageHandler
    // options...
)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

if err := c.Start(ctx); err != nil {
    log.Fatal(err)
}
```

**Key Methods:**

| Method        | Description                                    |
|---------------|------------------------------------------------|
| `Start(ctx)`  | Begin consuming messages                       |
| `Stop(ctx)`   | Gracefully stop with context timeout           |

---

### Static

Consumer bound to a single, fixed partition. Use for StatefulSet deployments where pod ordinal determines partition assignment.

**Use Cases:**
- Kubernetes StatefulSet (pod ordinal → partition)
- Fixed partition ownership
- Zero-coordination partitioning

**Lifecycle:** `Start(ctx) → Stop(ctx)`

```go
c, err := consumer.NewStatic(
    js,                         // jetstream.JetStream
    "EVENTS",                   // streamName
    "processor-0",              // consumerName
    "events.{{partition}}",     // subjectPattern
    10,                         // numPartitions (total)
    0,                          // partition (this instance)
    handler,                    // MessageHandler
    // options...
)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

if err := c.Start(ctx); err != nil {
    log.Fatal(err)
}
```

**Subject Pattern Placeholders:**
- `{{partition}}` - Replaced with partition index (required)
- `{{key}}` - Replaced with partition key (optional, becomes `*` for subscription)

**Key Methods:**

| Method        | Description                                    |
|---------------|------------------------------------------------|
| `Start(ctx)`  | Begin consuming messages                       |
| `Stop(ctx)`   | Gracefully stop with context timeout           |
| `Partition()` | Returns the partition index                    |
| `Subject()`   | Returns the filter subject                     |

---

### Dynamic

Partition-aware consumer that receives assignments from a Parti Manager. Manages multiple internal consumers based on assigned partitions.

**Use Cases:**
- Parti-coordinated workloads
- Dynamic partition assignment
- Elastic scaling with partition rebalancing

**Lifecycle:** `Update(ctx, workerID, partitions) → Stop(ctx)`

```go
c, err := consumer.NewDynamic(
    js,                              // jetstream.JetStream
    "ORDERS",                        // streamName
    "order-processor",               // consumerPrefix
    "orders.{{.PartitionID}}",       // subjectTemplate (Go template)
    handler,                         // MessageHandler
    // options...
)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

// Register with Manager for automatic updates
mgr, _ := parti.NewManager(cfg, js, src, strategy.NewConsistentHash(),
    parti.WithWorkerConsumerUpdater(c),
)
```

**Manual Update (without Manager):**

```go
partitions := []types.Partition{
    {Keys: []string{"partition-0"}},
    {Keys: []string{"partition-1"}},
}
if err := c.Update(ctx, "worker-0", partitions); err != nil {
    log.Fatal(err)
}
```

**Key Methods:**

| Method                                      | Description                                    |
|---------------------------------------------|------------------------------------------------|
| `Update(ctx, workerID, partitions)`         | Update partition assignments                   |
| `Stop(ctx)`                                 | Gracefully stop all partition consumers        |
| `UpdateWorkerConsumer(ctx, id, partitions)` | Implements `WorkerConsumerUpdater` interface   |

---

### Broadcast

Fan-out consumer where every instance receives every message. Uses a unique durable name per instance.

**Use Cases:**
- Cache invalidation
- Configuration updates
- Audit logging
- Global event notifications

**Lifecycle:** `Start(ctx) → Stop(ctx)`

> **Important:** The stream MUST use `LimitsPolicy` or `InterestPolicy`. `WorkQueuePolicy` is incompatible because it delivers each message to exactly one consumer.

```go
c, err := consumer.NewBroadcast(
    js,                // jetstream.JetStream
    "EVENTS",          // streamName
    "cache-updater",   // consumerPrefix
    "events.>",        // filterSubject
    handler,           // MessageHandler
    consumer.WithInstanceID("pod-abc123"), // unique per instance
)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

if err := c.Start(ctx); err != nil {
    log.Fatal(err)
}
```

**Key Methods:**

| Method        | Description                                    |
|---------------|------------------------------------------------|
| `Start(ctx)`  | Begin consuming messages                       |
| `Stop(ctx)`   | Gracefully stop with context timeout           |

---

## Message Handler

All consumer types use the unified `MessageHandler` interface:

```go
type MessageHandler interface {
    HandleMessage(ctx context.Context, msg jetstream.Msg) error
}
```

**Functional Adapter:**

```go
handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    // Process message
    data := msg.Data()
    _ = data

    return nil  // nil = auto-ack
})
```

**Auto-Acknowledgement (default):**
- `return nil` → Message acknowledged (Ack)
- `return error` → Message negatively acknowledged (Nak)

**Manual Acknowledgement:**

```go
c, _ := consumer.NewQueue(js, "stream", "consumer", "subject.>", handler,
    consumer.WithManualAck(true),
)

// In handler, you must call one of:
msg.Ack()           // Acknowledge
msg.Nak()           // Negative ack (immediate redeliver)
msg.NakWithDelay(d) // Negative ack with delay
msg.Term()          // Terminate (no redeliver)
```

---

## Functional Options

All consumers accept functional options for customization:

```go
c, _ := consumer.NewQueue(js, "stream", "consumer", "subject.>", handler,
    consumer.WithLogger(myLogger),
    consumer.WithMetrics(myCollector),
    consumer.WithAckWait(60*time.Second),
    consumer.WithBatchSize(100),
    consumer.WithMaxDeliver(5),
)
```

**Common Options:**

| Option                           | Description                                    |
|----------------------------------|------------------------------------------------|
| `WithLogger(logger)`             | Set custom logger                              |
| `WithMetrics(collector)`         | Set metrics collector                          |
| `WithAckWait(duration)`          | Time before message redelivery                 |
| `WithBatchSize(n)`               | Messages per fetch                             |
| `WithMaxDeliver(n)`              | Max redelivery attempts                        |
| `WithMaxAckPending(n)`           | Max unacked messages                           |
| `WithFetchTimeout(duration)`     | Max wait when pulling batch                    |
| `WithManualAck(bool)`            | Disable auto-acknowledgement                   |
| `WithInactiveThreshold(duration)`| Consumer cleanup threshold                     |

**Broadcast-Specific Options:**

| Option                           | Description                                    |
|----------------------------------|------------------------------------------------|
| `WithInstanceID(id)`             | Set unique instance identifier                 |

**Dynamic-Specific Options:**

| Option                           | Description                                    |
|----------------------------------|------------------------------------------------|
| `WithProcessingGate(cfg)`        | Enable processing gate for ownership control   |
| `WithDrainOnRemove(bool)`        | Drain messages when partitions are removed     |

---

## Migrating from Legacy Packages

The `consumer` package replaces the legacy `subscription` and `partition` packages with a unified API.

### subscription.WorkerConsumer → consumer.Dynamic

```go
// Legacy (deprecated)
import "github.com/arloliu/parti/subscription"

cfg := subscription.WorkerConsumerConfig{
    StreamName:      "ORDERS",
    ConsumerPrefix:  "processor",
    SubjectTemplate: "orders.{{.PartitionID}}",
}
wc, _ := subscription.NewWorkerConsumer(js, cfg, handler)
defer wc.Close(ctx)

// New (recommended)
import "github.com/arloliu/parti/consumer"

c, _ := consumer.NewDynamic(js, "ORDERS", "processor", "orders.{{.PartitionID}}", handler)
defer c.Stop(ctx)
```

### subscription.BroadcastConsumer → consumer.Broadcast

```go
// Legacy (deprecated)
cfg := subscription.BroadcastConsumerConfig{
    StreamName:     "EVENTS",
    ConsumerPrefix: "broadcast",
    WildcardFilter: "events.>",
}
bc, _ := subscription.NewBroadcastConsumer(js, cfg, handler)
defer bc.Close(ctx)

// New (recommended)
c, _ := consumer.NewBroadcast(js, "EVENTS", "broadcast", "events.>", handler)
defer c.Stop(ctx)
```

### partition.JSConsumer → consumer.Static

```go
// Legacy (deprecated)
import "github.com/arloliu/parti/partition"

cfg := partition.ConsumerConfig{
    StreamName:     "EVENTS",
    ConsumerName:   "processor-0",
    SubjectPattern: "events.{{partition}}",
    NumPartitions:  10,
    Partition:      0,
}
pc, _ := partition.NewJSConsumer(js, cfg, handler)
defer pc.Stop(ctx)

// New (recommended)
c, _ := consumer.NewStatic(js, "EVENTS", "processor-0", "events.{{partition}}", 10, 0, handler)
defer c.Stop(ctx)
```

### Key Differences

| Aspect              | Legacy Packages                    | consumer Package                  |
|---------------------|------------------------------------|-----------------------------------|
| API Style           | Config struct + constructor        | Positional args + options         |
| Method Names        | `Close()` / mixed                  | `Stop()` (consistent)             |
| Package Count       | 2 packages                         | 1 unified package                 |
| Message Handler     | Package-specific                   | Unified `MessageHandler`          |

---

## Legacy Packages (Deprecated)

> **Deprecation Notice:** The `subscription` and `partition` packages are deprecated.
> Use the `consumer` package instead for new code. Existing code will continue to work
> but should be migrated to the new package.

### subscription Package

The `subscription` package provided `WorkerConsumer` and `BroadcastConsumer` for JetStream integration with Parti.

**Status:** Deprecated. Use `consumer.Dynamic` and `consumer.Broadcast` instead.

### partition Package

The `partition` package provided `JSConsumer` for static partitioning scenarios (e.g., StatefulSet ordinal-based assignment).

**Status:** Deprecated. Use `consumer.Static` instead.

### CompositeConsumerUpdater

The `parti.CompositeConsumerUpdater` continues to work with the new consumer types:

```go
dynamic, _ := consumer.NewDynamic(js, "orders", "processor", "orders.{{.PartitionID}}", handler1)
broadcast, _ := consumer.NewBroadcast(js, "events", "updater", "events.>", handler2)

composite := parti.NewCompositeConsumerUpdater(dynamic, broadcast)

mgr, _ := parti.NewManager(cfg, js, src, strategy.NewConsistentHash(),
    parti.WithWorkerConsumerUpdater(composite),
)
```

---

## Architecture

### Dynamic Consumer Internals

```
    ┌──────────────────────────────────────────────────────┐
    │                    Dynamic Consumer                   │
    │                                                       │
    │   ┌─────────────┐    ┌─────────────────────────────┐ │
    │   │   Manager   │    │  Per-Partition Consumers    │ │
    │   │  Callback   │───▶│  orders.0, orders.1, ...    │ │
    │   └─────────────┘    └──────────────┬──────────────┘ │
    │                                      │                │
    │   Subject Template:                  │  Filter:       │
    │   "orders.{{.PartitionID}}"          │  Multi-subject │
    └──────────────────────────────────────┼────────────────┘
                                           │
                                           ▼
                                    ┌─────────────┐
                                    │   Stream    │
                                    │  "ORDERS"   │
                                    └─────────────┘
```

### Queue Consumer Internals

```
    ┌────────────────────────────────────────────────────────┐
    │                     Queue Consumer                      │
    │                                                         │
    │   ┌────────────┐  ┌────────────┐  ┌────────────┐       │
    │   │ Instance A │  │ Instance B │  │ Instance C │       │
    │   │  (shared)  │  │  (shared)  │  │  (shared)  │       │
    │   └─────┬──────┘  └─────┬──────┘  └─────┬──────┘       │
    │         │               │               │               │
    │         └───────────────┼───────────────┘               │
    │                         │                               │
    │   Consumer Name:        │  Durable: "job-workers"       │
    │   (shared by all)       │  Load-balanced delivery       │
    └─────────────────────────┼───────────────────────────────┘
                              │
                              ▼
                       ┌─────────────┐
                       │   Stream    │
                       │   "JOBS"    │
                       └─────────────┘
```

---

## Thread Safety

All consumer types are thread-safe. Lifecycle methods (`Start`, `Stop`, `Update`) are serialized internally to prevent race conditions.

| Method             | Thread Safety                                 |
|--------------------|-----------------------------------------------|
| `Start(ctx)`       | Serialized, call once                         |
| `Stop(ctx)`        | Serialized, idempotent                        |
| `Update(...)`      | Serialized, can call multiple times           |
| Message Handler    | Called from single goroutine per consumer     |

---

## Error Handling

**Constructor Errors:**
- Validation failures (nil JetStream, missing required fields)
- Not exported as sentinel errors; use string matching if needed

**Runtime Errors:**
- `context.DeadlineExceeded` - Stop timed out
- `subscription.ErrWorkerIDMutation` - Dynamic consumer workerID changed unexpectedly
- `subscription.ErrMaxSubjectsExceeded` - Partition count exceeds limit
