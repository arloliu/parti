# Parti Static Partitioning

> The `partition` package for application-level partitioning.

> **Note:** For JetStream consumers, use the `consumer` package instead.
> Use `consumer.Static` or `consumer.Dynamic` for JetStream consumption.
> See [Consumer Package](CONSUMERS.md).

**Related Documentation:**
- [Docs README](README.md) - Documentation map
- [Consumer Package](CONSUMERS.md) - Unified JetStream consumers
- [Strategies Guide](STRATEGIES.md) - Assignment strategies

---

## Table of Contents

1. [Overview](#overview)
2. [PartitionConfig](#partitionconfig)
3. [Publisher (Core NATS)](#publisher-core-nats)
4. [JSPublisher (JetStream)](#jspublisher-jetstream)
5. [Subscriber (Core NATS)](#subscriber-core-nats)
6. [Functional Options](#functional-options)
7. [StatefulSet Helpers](#statefulset-helpers)
8. [Use Cases](#use-cases)

---

## Overview

The `partition` package provides **static partition-based publishing and subscribing**. It
deterministically maps a partition key to a fixed partition index (0 to N-1) using xxh3
hashing, and constructs NATS subjects from a template pattern containing `{{partition}}`
and optionally `{{key}}` placeholders.

This is different from Parti's dynamic core (`parti.Manager`), which assigns partitions
to workers at runtime. The `partition` package answers: *"Given a user ID or order ID,
which NATS subject should this message be published to?"*

### Import

```go
import "github.com/arloliu/parti/v2/partition"
```

### Relationship to Core Parti

```
    ┌──────────────────────────────────────────────────────────────┐
    │                     Application Code                          │
    │                                                               │
    │   orderID := "order-12345"                                    │
    │                                                               │
    │   ┌─────────────────────────────────────────────────────────┐│
    │   │ partition.JSPublisher                                   ││
    │   │                                                         ││
    │   │   pub.Publish(ctx, orderID, payload)                    ││
    │   │   // Routes to: "orders.order-12345.7"                  ││
    │   └─────────────────────────────────────────────────────────┘│
    │                              │                                │
    │                              ▼                                │
    │   ┌─────────────────────────────────────────────────────────┐│
    │   │ consumer.Dynamic (via parti.Manager)                    ││
    │   │                                                         ││
    │   │   // Manager assigns partition "7" to this worker       ││
    │   │   // Dynamic consumer subscribes to "orders.*.7"        ││
    │   └─────────────────────────────────────────────────────────┘│
    │                                                               │
    └───────────────────────────────────────────────────────────────┘
```

---

## PartitionConfig

All types in the `partition` package share a single `PartitionConfig`:

```go
type PartitionConfig struct {
    NumPartitions  int          // Total number of partitions (required, > 0)
    SubjectPattern string       // Subject template with {{partition}} placeholder (required)
    HashSeed       uint64       // Optional seed for consistent hash (0 = default)
    Logger         types.Logger // Optional logger
}
```

**Subject Pattern Placeholders:**
- `{{partition}}` — Replaced with partition index (0 to N-1). **Required.**
- `{{key}}` — Replaced with the partition key. **Optional.**

**Examples:**

| Pattern                                | Key         | Result                       |
|----------------------------------------|-------------|------------------------------|
| `events.{{partition}}`                 | `user-123`  | `events.2`                   |
| `events.{{key}}.{{partition}}`         | `user-123`  | `events.user-123.2`          |
| `orders.{{partition}}.{{key}}.created` | `order-456` | `orders.1.order-456.created` |

Build a config using the functional-options constructor:

```go
cfg := partition.NewConfig(
    partition.WithNumPartitions(16),
    partition.WithSubjectPattern("events.{{key}}.{{partition}}"),
    partition.WithHashSeed(42),
)
```

---

## Publisher (Core NATS)

`Publisher` routes messages to partitioned core-NATS subjects using a `*nats.Conn`.

```go
func NewPublisher(nc *nats.Conn, config PartitionConfig) (*Publisher, error)
func NewPublisherWithOptions(nc *nats.Conn, opts ...Option) (*Publisher, error)
```

**Methods:**

| Method                                 | Description                                           |
|----------------------------------------|-------------------------------------------------------|
| `Publish(ctx, key, data)`              | Publish payload to partition for key                  |
| `PublishMsg(ctx, key, msg)`            | Publish a `*nats.Msg` to partition for key            |
| `GetPartition(key) int`                | Return partition index (0 to N-1) for a key           |
| `GetSubject(key) string`               | Return fully expanded subject for a key               |
| `GetSubjectForPartition(n) (string, error)` | Return subject for a specific partition index    |
| `NumPartitions() int`                  | Return configured partition count                     |

**Example:**

```go
nc, _ := nats.Connect(nats.DefaultURL)

pub, err := partition.NewPublisher(nc, partition.PartitionConfig{
    NumPartitions:  16,
    SubjectPattern: "events.{{key}}.{{partition}}",
})
if err != nil {
    log.Fatal(err)
}

if err := pub.Publish(ctx, "user-12345", payload); err != nil {
    log.Fatal(err)
}
```

---

## JSPublisher (JetStream)

`JSPublisher` routes messages to partitioned JetStream subjects with publish acknowledgment.

```go
func NewJSPublisher(js jetstream.JetStream, config PartitionConfig) (*JSPublisher, error)
func NewJSPublisherWithOptions(js jetstream.JetStream, opts ...Option) (*JSPublisher, error)
```

**Methods:**

| Method                                  | Description                                        |
|-----------------------------------------|----------------------------------------------------|
| `Publish(ctx, key, data)`               | Publish with JetStream ack; returns `*PubAck`      |
| `PublishAsync(key, data)`               | Publish asynchronously; returns `PubAckFuture`     |
| `PublishMsg(ctx, key, msg)`             | Publish a `*nats.Msg` with JetStream ack           |
| `PublishMsgAsync(key, msg)`             | Publish a `*nats.Msg` asynchronously               |
| `GetPartition(key) int`                 | Return partition index (0 to N-1) for a key        |
| `GetSubject(key) string`                | Return fully expanded subject for a key            |
| `GetSubjectForPartition(n) (string, error)` | Return subject for a specific partition index  |
| `NumPartitions() int`                   | Return configured partition count                  |

**Example:**

```go
js, _ := jetstream.New(nc)

pub, err := partition.NewJSPublisher(js, partition.PartitionConfig{
    NumPartitions:  16,
    SubjectPattern: "orders.{{partition}}",
})
if err != nil {
    log.Fatal(err)
}

ack, err := pub.Publish(ctx, "order-12345", payload)
if err != nil {
    log.Fatal(err)
}
log.Printf("Published to stream %s, seq %d", ack.Stream, ack.Sequence)
```

---

## Subscriber (Core NATS)

`Subscriber` consumes messages from a single static partition using core NATS.

```go
func NewSubscriber(nc *nats.Conn, config PartitionConfig, partition int, handler NATSMessageHandler) (*Subscriber, error)
func NewSubscriberWithOptions(nc *nats.Conn, partition int, handler NATSMessageHandler, opts ...Option) (*Subscriber, error)
```

**Methods:**

| Method          | Description                               |
|-----------------|-------------------------------------------|
| `Start(ctx)`    | Begin consuming messages                  |
| `Stop(ctx)`     | Gracefully stop subscription              |
| `Partition() int` | Return this subscriber's partition index |
| `Subject() string` | Return the NATS subject subscribed to  |

> **JetStream consumption:** Use `consumer.NewStatic` or `consumer.NewDynamic` from the
> `consumer` package instead of `Subscriber`. See [Consumer Package](CONSUMERS.md).

**Example:**

```go
sub, err := partition.NewSubscriber(
    nc,
    partition.PartitionConfig{
        NumPartitions:  4,
        SubjectPattern: "events.{{key}}.{{partition}}",
    },
    0,
    partition.NATSMessageHandlerFunc(func(ctx context.Context, msg *nats.Msg) error {
        log.Printf("Received: %s", string(msg.Data))
        return nil
    }),
)
if err != nil {
    log.Fatal(err)
}
if err := sub.Start(ctx); err != nil {
    log.Fatal(err)
}
```

---

## Functional Options

`NewConfig`, `NewPublisherWithOptions`, `NewJSPublisherWithOptions`, and
`NewSubscriberWithOptions` accept functional options:

| Option                      | Description                            |
|-----------------------------|----------------------------------------|
| `WithNumPartitions(n)`      | Set total number of partitions         |
| `WithSubjectPattern(p)`     | Set subject template                   |
| `WithHashSeed(seed)`        | Set hash seed for deterministic routing|
| `WithLogger(logger)`        | Set logger                             |

---

## StatefulSet Helpers

The package provides helpers for Kubernetes StatefulSet deployments:

```go
// GetPartitionFromEnv derives partition index from environment:
//   1. PARTITION_INDEX env var (explicit override)
//   2. HOSTNAME env var (StatefulSet pod name, e.g. "worker-2" -> 2)
//   3. os.Hostname() as fallback
partitionIndex, err := partition.GetPartitionFromEnv()

// ParseStatefulSetOrdinal extracts the integer ordinal from a hostname string.
// Example: "worker-3" -> 3
ordinal, err := partition.ParseStatefulSetOrdinal("worker-3")
```

---

## Use Cases

### Ordered Processing

Route all messages for the same entity (user, order) to the same partition:

```go
pub, _ := partition.NewJSPublisher(js, partition.PartitionConfig{
    NumPartitions:  32,
    SubjectPattern: "events.{{key}}.{{partition}}",
})

// All events for "user-123" consistently route to the same partition
pub.Publish(ctx, "user-123", payload)
```

### StatefulSet Fixed Partition

Each pod handles exactly one partition based on its ordinal:

```go
partitionIndex, err := partition.GetPartitionFromEnv()
if err != nil {
    log.Fatal(err)
}

// Publisher (shared by all pods)
pub, _ := partition.NewJSPublisher(js, partition.PartitionConfig{
    NumPartitions:  4,
    SubjectPattern: "orders.{{partition}}",
})

// Consumer (this pod only handles its own partition)
handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    log.Printf("Processing: %s", msg.Subject())
    return nil
})
sc, _ := consumer.NewStatic(js, "orders",
    fmt.Sprintf("processor-%d", partitionIndex),
    "orders.{{partition}}",
    4, partitionIndex, handler)
_ = sc.Start(ctx)
```

See the [partition package README](../partition/README.md) for more examples.
