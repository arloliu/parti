# partition

Static partition-based publishing and subscribing for NATS and JetStream.

## Overview

The `partition` package provides deterministic message routing using partition keys. It maps keys to partition indices using xxh3 hashing and expands subject patterns containing `{{partition}}` and optionally `{{key}}` placeholders.

**Design Goals:**

- **Deterministic Routing**: Same key always routes to the same partition
- **StatefulSet Integration**: Each Kubernetes pod handles a fixed partition based on its ordinal
- **Dual Protocol Support**: Works with both core NATS and JetStream
- **Zero External Dependencies**: Hash computation uses xxh3 with no external coordination

**Use Cases:**

- Ordered processing where messages with the same key must be handled by the same worker
- Sharded workloads across StatefulSet pods
- Event sourcing with partition-based aggregation
- Fan-out patterns with consistent message routing

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Publisher                                       │
│  key="user-123" → hash(key) % N → partition=2 → "events.user-123.2"         │
└─────────────────────────────────────────────────────────────────────────────┘
                                      │
                                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           NATS / JetStream                                   │
│                     Subject: events.*.0, events.*.1, ...                     │
└─────────────────────────────────────────────────────────────────────────────┘
                    │                 │                 │
                    ▼                 ▼                 ▼
              ┌──────────┐     ┌──────────┐     ┌──────────┐
              │ Pod-0    │     │ Pod-1    │     │ Pod-2    │
              │ Part: 0  │     │ Part: 1  │     │ Part: 2  │
              └──────────┘     └──────────┘     └──────────┘
```

## Key Concepts

| Concept             | Description                                                                                    |
|---------------------|------------------------------------------------------------------------------------------------|
| **Partition Key**   | A string used to deterministically route messages. Same key always maps to same partition.     |
| **Subject Pattern** | A NATS subject template with `{{partition}}` (required) and `{{key}}` (optional) placeholders. |
| **Partition Index** | Integer from 0 to N-1 derived from `hash(key) % NumPartitions`.                                |
| **Hash Seed**       | Optional seed for the hash function. Different seeds produce different distributions.          |

## Subject Patterns

The subject pattern defines how partition keys map to NATS subjects.

### Pattern Examples

| Pattern                                | Key         | Result                       |
|----------------------------------------|-------------|------------------------------|
| `events.{{partition}}`                 | `user-123`  | `events.2`                   |
| `events.{{key}}.{{partition}}`         | `user-123`  | `events.user-123.2`          |
| `orders.{{partition}}.{{key}}.created` | `order-456` | `orders.1.order-456.created` |
| `jobs.{{partition}}`                   | (any)       | `jobs.0`                     |

### Pattern Rules

1. **`{{partition}}` is required** - Every pattern must include the partition placeholder
2. **No empty tokens** - Pattern must not produce empty NATS subject tokens (e.g., `events..{{partition}}` is invalid)
3. **Consumer wildcard** - When `{{key}}` is present, consumers subscribe with `*` for the key token (e.g., `events.*.2`)

## Installation

```bash
go get github.com/arloliu/parti/v2/partition
```

## Quick Start

### Core NATS Publisher + Subscriber

```go
package main

import (
    "context"
    "log"

    "github.com/arloliu/parti/v2/partition"
    "github.com/nats-io/nats.go"
)

func main() {
    ctx := context.Background()
    nc, _ := nats.Connect("nats://localhost:4222")
    defer nc.Close()

    // Publisher
    pub, err := partition.NewPublisher(nc, partition.PartitionConfig{
        NumPartitions:  4,
        SubjectPattern: "events.{{key}}.{{partition}}",
    })
    if err != nil {
        log.Fatal(err)
    }

    // Subscriber for partition 0
    sub, err := partition.NewSubscriber(
        nc,
        partition.PartitionConfig{
            NumPartitions:  4,
            SubjectPattern: "events.{{key}}.{{partition}}",
        },
        0, // partition index
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

    // Publish - routes to partition based on key hash
    key := "user-123"
    if err := pub.Publish(ctx, key, []byte("hello")); err != nil {
        log.Fatal(err)
    }
}
```

### JetStream Publisher + Consumer

For JetStream consumption, use `partition.JSPublisher` for publishing and
`consumer.NewStatic()` from the [`consumer`](../consumer/) package for consuming:

```go
package main

import (
    "context"
    "log"

    "github.com/arloliu/parti/v2/consumer"
    "github.com/arloliu/parti/v2/partition"
    "github.com/nats-io/nats.go"
    "github.com/nats-io/nats.go/jetstream"
)

func main() {
    ctx := context.Background()
    nc, _ := nats.Connect("nats://localhost:4222")
    defer nc.Close()

    js, _ := jetstream.New(nc)

    // Create stream (once)
    js.CreateStream(ctx, jetstream.StreamConfig{
        Name:     "events",
        Subjects: []string{"events.*.0", "events.*.1", "events.*.2", "events.*.3"},
    })

    // Publisher (from partition package)
    pub, err := partition.NewJSPublisher(js, partition.PartitionConfig{
        NumPartitions:  4,
        SubjectPattern: "events.{{key}}.{{partition}}",
    })
    if err != nil {
        log.Fatal(err)
    }

    // Consumer for partition 0 (from consumer package)
    handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
        log.Printf("Received: %s", string(msg.Data()))
        return nil // auto-ack
    })

    sc, err := consumer.NewStatic(js, "events", "processor-0",
        "events.{{key}}.{{partition}}", 4, 0, handler)
    if err != nil {
        log.Fatal(err)
    }

    if err := sc.Start(ctx); err != nil {
        log.Fatal(err)
    }

    // Publish with acknowledgment
    ack, err := pub.Publish(ctx, "user-123", []byte("hello"))
    if err != nil {
        log.Fatal(err)
    }
    log.Printf("Published to stream %s, seq %d", ack.Stream, ack.Sequence)
}
```

## Kubernetes StatefulSet Integration

The package provides helpers to derive partition index from StatefulSet pod ordinals.

### Environment-Based Partition Detection

```go
// GetPartitionFromEnv checks:
// 1. PARTITION_INDEX environment variable (explicit override)
// 2. HOSTNAME environment variable (StatefulSet pod name)
// 3. os.Hostname() as fallback
partitionIndex, err := partition.GetPartitionFromEnv()
if err != nil {
    log.Fatal(err)
}

sc, err := consumer.NewStatic(js, "events",
    fmt.Sprintf("processor-%d", partitionIndex),
    "events.{{key}}.{{partition}}",
    numPartitions, partitionIndex, handler)
```

### Kubernetes Downward API Configuration

Use the Kubernetes Downward API to expose pod metadata as environment variables.

#### StatefulSet YAML

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: event-processor
spec:
  serviceName: event-processor
  replicas: 4
  selector:
    matchLabels:
      app: event-processor
  template:
    metadata:
      labels:
        app: event-processor
    spec:
      containers:
      - name: processor
        image: myregistry/event-processor:latest
        env:
        # Option 1: Use HOSTNAME (works on all Kubernetes versions)
        # Pod names follow pattern: <statefulset-name>-<ordinal>
        # e.g., event-processor-0, event-processor-1, ...
        # GetPartitionFromEnv() automatically parses the ordinal from HOSTNAME

        # Option 2: Pod index label via Downward API (Kubernetes 1.28+)
        # Uses the built-in pod index label added by StatefulSet controller
        - name: PARTITION_INDEX
          valueFrom:
            fieldRef:
              fieldPath: metadata.labels['apps.kubernetes.io/pod-index']

        # Option 3: Pod name via Downward API (Kubernetes 1.24 and earlier)
        # Use when HOSTNAME is not reliable (e.g., custom networking)
        # GetPartitionFromEnv() parses ordinal from pod name
        - name: HOSTNAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name

        # Total partitions from ConfigMap or direct value
        - name: NUM_PARTITIONS
          value: "4"

        resources:
          requests:
            cpu: "100m"
            memory: "128Mi"
          limits:
            cpu: "500m"
            memory: "256Mi"
```

> **Note:** For Kubernetes < 1.28, the `apps.kubernetes.io/pod-index` label doesn't exist. Use Option 1 (HOSTNAME) or Option 3 (pod name via Downward API) instead. The `GetPartitionFromEnv()` function automatically parses the ordinal suffix from StatefulSet pod names (e.g., `worker-3` → `3`).

#### Application Code

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"
    "strconv"

    "github.com/arloliu/parti/v2/consumer"
    "github.com/arloliu/parti/v2/partition"
    "github.com/nats-io/nats.go"
    "github.com/nats-io/nats.go/jetstream"
)

func main() {
    ctx := context.Background()

    // Get partition from environment (PARTITION_INDEX or HOSTNAME)
    partitionIndex, err := partition.GetPartitionFromEnv()
    if err != nil {
        log.Fatalf("Failed to determine partition: %v", err)
    }

    // Get total partitions from environment
    numPartitions, _ := strconv.Atoi(os.Getenv("NUM_PARTITIONS"))
    if numPartitions == 0 {
        numPartitions = 4 // default
    }

    log.Printf("Starting processor for partition %d of %d", partitionIndex, numPartitions)

    nc, err := nats.Connect(os.Getenv("NATS_URL"))
    if err != nil {
        log.Fatal(err)
    }
    defer nc.Close()

    js, _ := jetstream.New(nc)

    handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
        // Process message
        log.Printf("Processing: %s", string(msg.Data()))
        return nil
    })

    sc, err := consumer.NewStatic(js, "events",
        fmt.Sprintf("processor-%d", partitionIndex),
        "events.{{key}}.{{partition}}",
        numPartitions, partitionIndex, handler)
    if err != nil {
        log.Fatal(err)
    }

    if err := sc.Start(ctx); err != nil {
        log.Fatal(err)
    }

    // Block forever
    select {}
}
```

### Helm Chart Values Example

```yaml
# values.yaml
replicaCount: 4

env:
  NATS_URL: "nats://nats:4222"
  NUM_PARTITIONS: "4"

# Each pod automatically gets HOSTNAME=<release>-<ordinal>
```

## Configuration Reference

### PartitionConfig

| Field            | Type           | Required | Default | Description                                                             |
|------------------|----------------|----------|---------|-------------------------------------------------------------------------|
| `NumPartitions`  | `int`          | Yes      | -       | Total number of partitions (1 to N)                                     |
| `SubjectPattern` | `string`       | Yes      | -       | Subject template with `{{partition}}` placeholder                       |
| `HashSeed`       | `uint64`       | No       | `0`     | Seed for hash function. Different seeds produce different distributions |
| `Logger`         | `types.Logger` | No       | no-op   | Structured logger instance                                              |

> **JetStream Consumer Configuration:** For JetStream consumer settings (AckWait, MaxDeliver,
> BatchSize, etc.), see the [`consumer` package documentation](../consumer/).

## Advanced Usage

```go
pub, _ := partition.NewJSPublisher(js, config)

// Publish without waiting for ack
future, err := pub.PublishAsync("user-123", []byte("async message"))
if err != nil {
    log.Fatal(err)
}

// Check ack later
select {
case ack := <-future.Ok():
    log.Printf("Confirmed: seq=%d", ack.Sequence)
case err := <-future.Err():
    log.Printf("Failed: %v", err)
}
```

### Functional Options API

Alternative construction using functional options:

```go
// Publisher
pub, err := partition.NewPublisherWithOptions(
    nc,
    partition.WithNumPartitions(4),
    partition.WithSubjectPattern("events.{{key}}.{{partition}}"),
    partition.WithHashSeed(12345),
)

// JetStream Publisher
jsPub, err := partition.NewJSPublisherWithOptions(
    js,
    partition.WithNumPartitions(4),
    partition.WithSubjectPattern("events.{{key}}.{{partition}}"),
)

// Subscriber (core NATS)
sub, err := partition.NewSubscriberWithOptions(
    nc,
    0, // partition
    handler,
    partition.WithNumPartitions(4),
    partition.WithSubjectPattern("events.{{key}}.{{partition}}"),
)
```

### Hash Seed for Distribution Control

Different hash seeds produce different key-to-partition mappings:

```go
// Production environment
prodPub, _ := partition.NewPublisher(nc, partition.PartitionConfig{
    NumPartitions:  4,
    SubjectPattern: "events.{{partition}}",
    HashSeed:       0, // default
})

// Staging with different distribution
stagingPub, _ := partition.NewPublisher(nc, partition.PartitionConfig{
    NumPartitions:  4,
    SubjectPattern: "events.{{partition}}",
    HashSeed:       42, // different seed
})

key := "user-123"
// prodPub.GetPartition(key) may differ from stagingPub.GetPartition(key)
```

## API Reference

### Publisher Methods

| Method                                              | Description                                          |
|-----------------------------------------------------|------------------------------------------------------|
| `Publish(ctx, key, data) error`                     | Publish to partition determined by key               |
| `GetPartition(key) int`                             | Get partition index for key                          |
| `GetSubject(key) string`                            | Get full subject for key                             |
| `GetSubjectForPartition(partition) (string, error)` | Get subject for specific partition (key becomes `*`) |
| `NumPartitions() int`                               | Get total partition count                            |

### JSPublisher Methods

| Method                                                             | Description                        |
|--------------------------------------------------------------------|------------------------------------|
| `Publish(ctx, key, data, opts...) (*jetstream.PubAck, error)`      | Publish with ack                   |
| `PublishAsync(key, data, opts...) (jetstream.PubAckFuture, error)` | Async publish                      |
| `GetPartition(key) int`                                            | Get partition index for key        |
| `GetSubject(key) string`                                           | Get full subject for key           |
| `GetSubjectForPartition(partition) (string, error)`                | Get subject for specific partition |
| `NumPartitions() int`                                              | Get total partition count          |

### Subscriber Methods

| Method             | Description              |
|--------------------|--------------------------|
| `Start(ctx) error` | Start consuming messages |
| `Stop() error`     | Stop consuming           |
| `Partition() int`  | Get assigned partition   |
| `Subject() string` | Get subscription subject |

> **JetStream Consumer Methods:** For JetStream consumer API (Start, Stop, Update),
> see `consumer.Static`, `consumer.Dynamic`, and `consumer.Broadcast` in the
> [`consumer` package](../consumer/).

## Error Handling

| Error                    | Cause                                           |
|--------------------------|-------------------------------------------------|
| `ErrEmptyKey`            | Empty string passed as partition key            |
| `ErrInvalidKey`          | Key contains invalid characters (`.`, `*`, `>`) |
| `ErrInvalidPattern`      | Subject pattern is malformed                    |
| `ErrPatternEmptyToken`   | Pattern produces empty NATS subject tokens      |
| `ErrPartitionOutOfRange` | Partition index >= NumPartitions                |

## Best Practices

1. **Immutable NumPartitions**: Avoid changing `NumPartitions` after deployment. It changes the hash mapping and causes message redistribution.

2. **Consistent Configuration**: Use the same `PartitionConfig` (NumPartitions, SubjectPattern, HashSeed) for publishers and consumers.

3. **StatefulSet Replicas = NumPartitions**: Match StatefulSet replicas to partition count for even distribution.

4. **Unique Consumer Names**: Use partition index in consumer names (e.g., `processor-0`, `processor-1`).

5. **Stream Subject Wildcards**: Configure JetStream streams to capture all partitions:
   ```go
   // For pattern "events.{{key}}.{{partition}}" with 4 partitions
   Subjects: []string{"events.*.0", "events.*.1", "events.*.2", "events.*.3"}
   // Or use broader wildcard
   Subjects: []string{"events.*.*"}
   ```

## Notes

- Configuration defaults are applied during validation
- The hash function is xxh3 (extremely fast, good distribution)
- Partition assignment is deterministic: same key + seed + NumPartitions = same partition
- For JetStream consumption, use the [`consumer` package](../consumer/) (`consumer.NewStatic`, `consumer.NewDynamic`)
