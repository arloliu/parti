# Parti Strategies & Sources

> Partition assignment strategies and partition sources.

**Related Documentation:**
- [Docs README](README.md) - Documentation map
- [Architecture](ARCHITECTURE.md) - System architecture and concepts
- [Configuration Guide](CONFIGURATION.md) - Configuration options
- [Static Partitioning](STATIC_PARTITIONING.md) - The partition package

---

## Table of Contents

- [Parti Strategies \& Sources](#parti-strategies--sources)
  - [Table of Contents](#table-of-contents)
  - [Assignment Strategies](#assignment-strategies)
    - [Import](#import)
    - [Strategy Interface](#strategy-interface)
    - [ConsistentHash](#consistenthash)
    - [WeightedConsistentHash](#weightedconsistenthash)
    - [RoundRobin](#roundrobin)
    - [Custom Strategies](#custom-strategies)
  - [Partition Sources](#partition-sources)
    - [Import](#import-1)
    - [Source Interface](#source-interface)
    - [Static Source](#static-source)
    - [NatsKV Source](#natskv-source)
    - [Watchable Sources](#watchable-sources)
    - [Custom Sources](#custom-sources)
  - [Strategy Selection Guide](#strategy-selection-guide)
  - [Source Selection Guide](#source-selection-guide)

---

## Assignment Strategies

Assignment strategies determine **how partitions are distributed across workers**. The `strategy` package provides three built-in strategies.

### Import

```go
import "github.com/arloliu/parti/strategy"
```

### Strategy Interface

All strategies implement:

```go
type AssignmentStrategy interface {
    Assign(workers []string, partitions []Partition) (map[string][]Partition, error)
}
```

### ConsistentHash

The default strategy. Distributes partitions using consistent hashing for stable assignments during scaling.

**Key Properties:**
- ~80% partition affinity during rebalancing
- Even distribution with virtual nodes
- Deterministic assignment (same inputs = same outputs)

**How It Works:**

```
                    Hash Ring (Simplified)
                         ┌───────┐
                      ╱──│ W1-v1 │──╲
                   ╱──   └───────┘   ──╲
                ╱──                      ──╲
             ┌──────┐                  ┌──────┐
             │ P0   │                  │ W2-v1│
             └──────┘                  └──────┘
                │                          │
           ┌────┴────┐                     │
           │         │                     │
        ┌──────┐  ┌──────┐            ┌──────┐
        │ W1-v2│  │ P1   │            │ W1-v3│
        └──────┘  └──────┘            └──────┘
                     │                     │
                  ╲──│──               ──│─╱
                     ╲── ┌──────┐  ──╱
                         │ P2   │
                         └──────┘

    Partition P0 → maps to Worker W1 (closest clockwise)
    Partition P1 → maps to Worker W1 (closest clockwise)
    Partition P2 → maps to Worker W2 (closest clockwise)
```

**Usage:**

```go
import "github.com/arloliu/parti/strategy"

// Default: 100 virtual nodes
s := strategy.NewConsistentHash()

// Custom virtual nodes (more = better distribution, more memory)
s := strategy.NewConsistentHash(strategy.WithVirtualNodes(200))

// Use with manager (positional args: config, jetstream, source, strategy)
mgr, _ := parti.NewManager(cfg, js, src, s)
```

**Options:**

| Option               | Default | Description                          |
|----------------------|---------|--------------------------------------|
| `WithVirtualNodes(n)`| 100     | Virtual nodes per worker (10-1000)   |

**When to Use:**
- Default choice for most workloads
- When cache affinity matters (stateful partitions)
- Rolling updates where consistency helps

---

### WeightedConsistentHash

Extension of ConsistentHash that respects partition weights for uneven load distribution.

**Key Properties:**
- Honors `Partition.Weight` field
- Higher weight = more resources allocated
- Maintains consistent hashing benefits

**How It Works:**

Weights influence virtual node distribution:

```
    Partitions with weights:
    P0 (weight=1), P1 (weight=3), P2 (weight=1)

    Hash Ring Distribution:
    ┌─────────────────────────────────────────────┐
    │                                             │
    │   W1: receives P0 (weight=1)                │
    │   W2: receives P1 (weight=3)                │
    │   W3: receives P2 (weight=1)                │
    │                                             │
    │   Total weights: 5                          │
    │   W1 handles: 20% of total load             │
    │   W2 handles: 60% of total load             │
    │   W3 handles: 20% of total load             │
    │                                             │
    └─────────────────────────────────────────────┘
```

**Usage:**

```go
import (
    "github.com/arloliu/parti"
    "github.com/arloliu/parti/strategy"
)

// Create weighted strategy
s := strategy.NewWeightedConsistentHash()

// Define partitions with weights
partitions := []parti.Partition{
    {ID: "0", Weight: 1.0},  // Light load
    {ID: "1", Weight: 3.0},  // Heavy load (3x partition 0)
    {ID: "2", Weight: 1.5},  // Medium load
}

// Use with static source
src := source.NewStatic(partitions)

// Create manager with weighted strategy
mgr, _ := parti.NewManager(cfg, js, src, s)
```

**Options:**

| Option               | Default | Description                        |
|----------------------|---------|------------------------------------|
| `WithVirtualNodes(n)`| 100     | Virtual nodes per worker           |
| `WithWeightPrecision(p)` | 2   | Decimal precision for weights      |

**When to Use:**
- Partitions have known, varying loads
- Tenant-based partitioning (larger tenants = higher weight)
- Resource-proportional distribution needed

---

### RoundRobin

Simplest strategy. Distributes partitions evenly in order.

**Key Properties:**
- Perfect even distribution
- No consistency guarantees during rebalancing
- Lowest CPU overhead

**How It Works:**

```
    Workers: [W1, W2, W3]
    Partitions: [P0, P1, P2, P3, P4, P5]

    Assignment (round-robin):
    ┌─────────────────────────────────────────────┐
    │                                             │
    │   W1: [P0, P3]                              │
    │   W2: [P1, P4]                              │
    │   W3: [P2, P5]                              │
    │                                             │
    └─────────────────────────────────────────────┘

    If W3 leaves:
    ┌─────────────────────────────────────────────┐
    │                                             │
    │   W1: [P0, P2, P4]   ← P2, P4 reassigned    │
    │   W2: [P1, P3, P5]   ← P3, P5 reassigned    │
    │                                             │
    └─────────────────────────────────────────────┘
```

**Usage:**

```go
import "github.com/arloliu/parti/strategy"

s := strategy.NewRoundRobin()

// Use with manager (positional args: config, jetstream, source, strategy)
mgr, _ := parti.NewManager(cfg, js, src, s)
```

**When to Use:**
- Stateless partition processing
- Equal partition sizes
- Simplicity over consistency

---

### Custom Strategies

Implement the `AssignmentStrategy` interface:

```go
package custom

import "github.com/arloliu/parti"

type AffinityStrategy struct {
    affinityMap map[string]string  // partition -> preferred worker
}

func NewAffinityStrategy(affinities map[string]string) *AffinityStrategy {
    return &AffinityStrategy{affinityMap: affinities}
}

func (s *AffinityStrategy) Assign(
    workers []string,
    partitions []parti.Partition,
) (map[string][]Partition, error) {
    result := make(map[string][]parti.Partition)

    // Initialize all workers
    for _, w := range workers {
        result[w] = []parti.Partition{}
    }

    for _, p := range partitions {
        // Check for preferred worker
        if preferred, ok := s.affinityMap[p.ID]; ok {
            if contains(workers, preferred) {
                result[preferred] = append(result[preferred], p)
                continue
            }
        }
        // Fallback to least-loaded worker
        worker := leastLoaded(result, workers)
        result[worker] = append(result[worker], p)
    }

    return result, nil
}
```

---

## Partition Sources

Partition sources define **where partition definitions come from**. The `source` package provides built-in sources.

### Import

```go
import "github.com/arloliu/parti/source"
```

### Source Interface

All sources implement:

```go
type PartitionSource interface {
    GetPartitions(ctx context.Context) ([]Partition, error)
}
```

Watchable sources additionally implement:

```go
type WatchablePartitionSource interface {
    PartitionSource
    Watch(ctx context.Context) (<-chan []Partition, error)
}
```

### Static Source

Fixed partition list defined at startup.

```go
import (
    "github.com/arloliu/parti"
    "github.com/arloliu/parti/source"
)

// Simple: just partition IDs
partitions := []parti.Partition{
    {ID: "0"},
    {ID: "1"},
    {ID: "2"},
}
src := source.NewStatic(partitions)

// With weights
partitions := []parti.Partition{
    {ID: "tenant-a", Weight: 2.0},
    {ID: "tenant-b", Weight: 1.0},
    {ID: "tenant-c", Weight: 3.0},
}
src := source.NewStatic(partitions)

// With metadata
partitions := []parti.Partition{
    {ID: "0", Metadata: map[string]string{"region": "us-east"}},
    {ID: "1", Metadata: map[string]string{"region": "us-west"}},
}
src := source.NewStatic(partitions)

// Use with manager
mgr, _ := parti.NewManager(cfg, js, src, strategy.NewConsistentHash())
```

**When to Use:**
- Known, fixed partition count
- Simple deployments
- Testing and development

---

### NatsKV Source

Dynamic partition definitions stored in NATS KV.

```go
import "github.com/arloliu/parti/source"

// Create source from KV bucket
src := source.NewNatsKV(js, "partitions-bucket")

// Partitions are stored as JSON in KV:
// Key: "partitions"
// Value: [{"id":"0","weight":1},{"id":"1","weight":2}]

mgr, _ := parti.NewManager(cfg, js, src, strategy.NewConsistentHash())
```

**Features:**
- Implements `WatchablePartitionSource`
- Automatic rebalancing on partition changes
- Cluster-wide partition registry

**KV Structure:**

```
Bucket: partitions-bucket
├── partitions (JSON array of partition definitions)
└── _metadata (optional source metadata)
```

**When to Use:**
- Dynamic partition count
- Partition definitions managed externally
- Multi-cluster coordination

---

### Watchable Sources

Sources implementing `WatchablePartitionSource` enable automatic rebalancing:

```go
// Check if source is watchable
if watchable, ok := src.(source.WatchablePartitionSource); ok {
    changes, err := watchable.Watch(ctx)
    if err != nil {
        return err
    }

    go func() {
        for partitions := range changes {
            log.Printf("Partitions changed: %d partitions", len(partitions))
            // Manager handles this automatically
        }
    }()
}
```

**Flow:**

```
    ┌────────────────┐     Watch()     ┌─────────────┐
    │  NatsKV Source │────────────────▶│   Manager   │
    │                │                  │             │
    │  partitions    │   <-chan []P    │  Triggers   │
    │  bucket        │◀────────────────│  rebalance  │
    └────────────────┘                  └─────────────┘
           │
           │ KV Update
           ▼
    ┌────────────────┐
    │ External Admin │
    │ (add/remove    │
    │  partitions)   │
    └────────────────┘
```

---

### Custom Sources

Implement `PartitionSource` for custom partition providers:

```go
package custom

import (
    "context"
    "github.com/arloliu/parti"
)

type DatabaseSource struct {
    db *sql.DB
}

func NewDatabaseSource(db *sql.DB) *DatabaseSource {
    return &DatabaseSource{db: db}
}

func (s *DatabaseSource) GetPartitions(ctx context.Context) ([]parti.Partition, error) {
    rows, err := s.db.QueryContext(ctx,
        "SELECT id, weight FROM partitions WHERE active = true")
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var partitions []parti.Partition
    for rows.Next() {
        var p parti.Partition
        if err := rows.Scan(&p.ID, &p.Weight); err != nil {
            return nil, err
        }
        partitions = append(partitions, p)
    }

    return partitions, rows.Err()
}

// Optional: implement WatchablePartitionSource for auto-rebalancing
func (s *DatabaseSource) Watch(ctx context.Context) (<-chan []parti.Partition, error) {
    ch := make(chan []parti.Partition)

    go func() {
        defer close(ch)
        ticker := time.NewTicker(30 * time.Second)
        defer ticker.Stop()

        var lastHash string
        for {
            select {
            case <-ctx.Done():
                return
            case <-ticker.C:
                partitions, err := s.GetPartitions(ctx)
                if err != nil {
                    continue
                }
                hash := hashPartitions(partitions)
                if hash != lastHash {
                    lastHash = hash
                    ch <- partitions
                }
            }
        }
    }()

    return ch, nil
}
```

---

## Strategy Selection Guide

| Requirement                     | Recommended Strategy       |
|---------------------------------|----------------------------|
| Default, general purpose        | ConsistentHash             |
| Cache/state affinity needed     | ConsistentHash             |
| Uneven partition loads          | WeightedConsistentHash     |
| Simple, stateless workloads     | RoundRobin                 |
| Custom placement rules          | Custom Strategy            |

## Source Selection Guide

| Requirement                     | Recommended Source         |
|---------------------------------|----------------------------|
| Fixed partition count           | Static                     |
| Dynamic partitions              | NatsKV                     |
| External management             | NatsKV or Custom           |
| Database-driven partitions      | Custom (DatabaseSource)    |
| API-driven partitions           | Custom (APISource)         |

See [Architecture](ARCHITECTURE.md) for how strategies and sources fit into the overall system.
