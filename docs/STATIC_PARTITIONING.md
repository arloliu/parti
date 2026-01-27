# Parti Static Partitioning

> The `partition` package for application-level partitioning.

**Related Documentation:**
- [User Guide](USER_GUIDE.md) - Getting started and overview
- [Strategies Guide](STRATEGIES.md) - Assignment strategies
- [Consumer Helpers](CONSUMERS.md) - JetStream subscription management

---

## Table of Contents

1. [Overview](#overview)
2. [Partitioner Interface](#partitioner-interface)
3. [HashPartitioner](#hashpartitioner)
4. [Partition Helpers](#partition-helpers)
5. [Use Cases](#use-cases)

---

## Overview

The `partition` package provides **application-level partitioning**—determining which partition a piece of data belongs to based on a partition key.

This is different from Parti's core functionality (which assigns partitions to workers). The `partition` package answers: *"Given a user ID or order ID, which partition should handle it?"*

### Import

```go
import "github.com/arloliu/parti/partition"
```

### Relationship to Core Parti

```
    ┌──────────────────────────────────────────────────────────────┐
    │                     Application Code                          │
    │                                                               │
    │   orderID := "order-12345"                                    │
    │                                                               │
    │   ┌─────────────────────────────────────────────────────────┐│
    │   │ partition.HashPartitioner                               ││
    │   │                                                         ││
    │   │   partitionID := partitioner.Partition(orderID)         ││
    │   │   // Returns: "7" (of 16 partitions)                    ││
    │   └─────────────────────────────────────────────────────────┘│
    │                              │                                │
    │                              ▼                                │
    │   ┌─────────────────────────────────────────────────────────┐│
    │   │ parti.Manager                                           ││
    │   │                                                         ││
    │   │   assignment := mgr.GetAssignment()                     ││
    │   │   // Worker has partitions: ["5", "6", "7", "8"]        ││
    │   │                                                         ││
    │   │   if contains(assignment, partitionID) {                ││
    │   │       process(orderID)  // This worker handles it       ││
    │   │   }                                                     ││
    │   └─────────────────────────────────────────────────────────┘│
    │                                                               │
    └───────────────────────────────────────────────────────────────┘
```

---

## Partitioner Interface

```go
type Partitioner interface {
    // Partition returns the partition ID for the given key
    Partition(key string) string

    // PartitionCount returns the total number of partitions
    PartitionCount() int
}
```

---

## HashPartitioner

The built-in `HashPartitioner` uses consistent hashing to map keys to partitions.

### Basic Usage

```go
import "github.com/arloliu/parti/partition"

// Create partitioner with 16 partitions
p := partition.NewHashPartitioner(16)

// Partition a key
partitionID := p.Partition("user-12345")
// Returns: "7" (deterministic for this key)

// Same key always maps to same partition
p.Partition("user-12345") // "7"
p.Partition("user-12345") // "7"

// Different keys distribute across partitions
p.Partition("user-67890") // "3"
p.Partition("order-abc")  // "12"
```

### Integration with Parti Manager

```go
import (
    "github.com/arloliu/parti"
    "github.com/arloliu/parti/partition"
    "github.com/arloliu/parti/source"
)

// Create partitioner and source with same partition count
const partitionCount = 16

partitioner := partition.NewHashPartitioner(partitionCount)

// Generate partition definitions for the manager
partitions := make([]parti.Partition, partitionCount)
for i := range partitions {
    partitions[i] = parti.Partition{ID: strconv.Itoa(i)}
}
src := source.NewStatic(partitions)

// Create manager
mgr, err := parti.NewManager(cfg,
    parti.WithPartitionSource(src),
)

// In message handler: route by partition
func handleMessage(msg *nats.Msg) {
    userID := extractUserID(msg)
    partitionID := partitioner.Partition(userID)

    // Check if this worker owns the partition
    if mgr.OwnsPartition(partitionID) {
        processMessage(msg)
    } else {
        // Route to correct worker or re-queue
        forwardToPartition(msg, partitionID)
    }
}
```

### Options

```go
// Default hash function (xxHash)
p := partition.NewHashPartitioner(16)

// Custom hash function
p := partition.NewHashPartitioner(16,
    partition.WithHashFunc(fnv.New64a),
)
```

---

## Partition Helpers

### GeneratePartitionIDs

Create partition ID slices for initialization:

```go
import "github.com/arloliu/parti/partition"

// Generate ["0", "1", "2", ..., "15"]
ids := partition.GeneratePartitionIDs(16)

// Convert to Partition slice
partitions := make([]parti.Partition, len(ids))
for i, id := range ids {
    partitions[i] = parti.Partition{ID: id}
}
```

### GetPartitionIndex

Convert partition ID back to index:

```go
idx := partition.GetPartitionIndex("7")  // Returns: 7
idx := partition.GetPartitionIndex("15") // Returns: 15
```

---

## Use Cases

### User-Based Partitioning

Route all operations for a user to the same partition:

```go
partitioner := partition.NewHashPartitioner(32)

func handleUserRequest(userID string, request Request) {
    partitionID := partitioner.Partition(userID)

    // All requests for this user go to same partition
    // Enables user-level ordering and caching
    subject := fmt.Sprintf("requests.%s", partitionID)
    js.Publish(subject, encodeRequest(request))
}
```

### Order Processing

Ensure order events are processed in sequence:

```go
partitioner := partition.NewHashPartitioner(16)

func publishOrderEvent(orderID string, event OrderEvent) {
    partitionID := partitioner.Partition(orderID)

    // All events for this order go to same partition
    // Worker processes them in order
    subject := fmt.Sprintf("orders.%s", partitionID)
    js.Publish(subject, encodeEvent(event))
}
```

### Tenant Isolation

Route tenant data to dedicated partitions:

```go
// Map tenants to dedicated partitions
tenantPartitions := map[string]string{
    "tenant-a": "0",
    "tenant-b": "1",
    "tenant-c": "2",
}

func getPartition(tenantID string) string {
    if p, ok := tenantPartitions[tenantID]; ok {
        return p
    }
    // Fallback: hash smaller tenants across remaining partitions
    return partitioner.Partition(tenantID)
}
```

### Multi-Key Partitioning

Combine multiple keys for composite partitioning:

```go
partitioner := partition.NewHashPartitioner(64)

func getPartitionKey(tenantID, userID string) string {
    // Combine keys for partitioning
    compositeKey := fmt.Sprintf("%s:%s", tenantID, userID)
    return partitioner.Partition(compositeKey)
}
```

---

## Best Practices

### Partition Count Selection

| Factor                   | Recommendation                    |
|--------------------------|-----------------------------------|
| Expected worker count    | 4-8x max workers                  |
| Load distribution        | More partitions = better balance  |
| Rebalancing overhead     | Fewer partitions = faster rebalance |
| Typical range            | 16-256 partitions                 |

**Rule of Thumb:** Start with `8 × max_workers` partitions.

### Key Design

Good partition keys:
- Evenly distributed (avoid hot spots)
- Stable (don't change for same entity)
- Meaningful (related data same partition)

```go
// Good: User ID - stable, well-distributed
partitioner.Partition(userID)

// Good: Order ID with prefix stripped
partitioner.Partition(strings.TrimPrefix(orderID, "ORD-"))

// Bad: Timestamp - creates hot partitions
partitioner.Partition(time.Now().Format("2006-01-02"))

// Bad: Status - only a few values, poor distribution
partitioner.Partition(orderStatus) // "pending", "shipped", "delivered"
```

### Consistency with Manager

Ensure the partitioner and manager agree on partition count:

```go
const partitionCount = 32

// Partitioner for routing
partitioner := partition.NewHashPartitioner(partitionCount)

// Source for manager - MUST match partitioner count
partitions := make([]parti.Partition, partitionCount)
for i := range partitions {
    partitions[i] = parti.Partition{ID: strconv.Itoa(i)}
}

mgr, _ := parti.NewManager(cfg,
    parti.WithPartitionSource(source.NewStatic(partitions)),
)

// Now partitioner.Partition(key) returns IDs that manager understands
```

See [Strategies Guide](STRATEGIES.md) for how the manager assigns these partitions to workers.
