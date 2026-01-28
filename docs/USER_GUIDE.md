# Parti User Guide

> **Let's parti(tion), work, scale effortlessly**

**Version**: 1.7.0
**Last Updated**: January 27, 2026
**Library**: `github.com/arloliu/parti`

---

## Documentation Overview

This user guide provides an introduction to Parti. For detailed documentation, see the focused guides below:

| Document                                       | Description                                      |
|------------------------------------------------|--------------------------------------------------|
| [Architecture](ARCHITECTURE.md)                | System architecture, components, data flow       |
| [Configuration Guide](CONFIGURATION.md)        | Configuration options, presets, tuning           |
| [Lifecycle Guide](LIFECYCLE.md)                | Worker states, stable IDs, handoff, degraded mode|
| [Consumer Helpers](CONSUMERS.md)               | WorkerConsumer, BroadcastConsumer, ProcessingGate|
| [Strategies & Sources](STRATEGIES.md)          | Assignment strategies, partition sources         |
| [Static Partitioning](STATIC_PARTITIONING.md)  | The partition package for key-based routing      |
| [Reference](REFERENCE.md)                      | Hooks, errors, best practices, glossary          |
| [API Reference](API_REFERENCE.md)              | Detailed API documentation                       |

---

## Table of Contents

1. [Introduction](#introduction)
2. [Getting Started](#getting-started)
3. [Quick Start](#quick-start)
4. [Core Concepts](#core-concepts)
5. [When to Use Parti](#when-to-use-parti)
6. [Next Steps](#next-steps)

---

## Introduction

### What is Parti?

Parti is a Go library for NATS-based work partitioning that provides dynamic partition assignment across worker instances with stable worker IDs and leader-based coordination.

### Key Features

| Feature                  | Description                                              |
|--------------------------|----------------------------------------------------------|
| **Stable Worker IDs**    | Workers claim stable IDs for consistent assignment       |
| **Leader-Based Assignment** | Single leader calculates assignments without coordination |
| **Two-Phase Handoff**    | Prepare/Commit protocol for safe partition reassignment  |
| **Degraded Mode**        | High availability during NATS outages                    |
| **Processing Gate**      | Strict ownership enforcement for message processing      |
| **Cache Affinity**       | Preserves >80% partition locality during rebalancing     |
| **Weighted Assignment**  | Partition weights for uneven workload distribution       |
| **Static Partitioning**  | Zero-coordination mode for StatefulSet deployments       |

### Architecture at a Glance

```
┌─────────────────────────────────────────────────────────────────────────┐
│                              NATS JetStream                              │
│  ┌─────────────┐ ┌─────────────┐ ┌─────────────┐ ┌───────────────────┐  │
│  │ StableID KV │ │ Election KV │ │Heartbeat KV │ │  Assignment KV    │  │
│  │ (claims)    │ │ (leader)    │ │ (health)    │ │ (partition→worker)│  │
│  └──────┬──────┘ └──────┬──────┘ └──────┬──────┘ └─────────┬─────────┘  │
└─────────┼───────────────┼───────────────┼───────────────────┼───────────┘
          │               │               │                   │
          └───────────────┴───────────────┴───────────────────┘
                                    │
        ┌───────────────────────────┼───────────────────────────┐
        │                           │                           │
        ▼                           ▼                           ▼
┌───────────────────┐   ┌───────────────────┐   ┌───────────────────┐
│     Worker 0      │   │     Worker 1      │   │     Worker 2      │
│   ┌───────────┐   │   │   ┌───────────┐   │   │   ┌───────────┐   │
│   │  Manager  │   │   │   │  Manager  │   │   │   │  Manager  │   │
│   │  (Leader) │   │   │   │ (Follower)│   │   │   │ (Follower)│   │
│   └───────────┘   │   │   └───────────┘   │   │   └───────────┘   │
│   Partitions:     │   │   Partitions:     │   │   Partitions:     │
│   [P0, P3, P6]    │   │   [P1, P4, P7]    │   │   [P2, P5, P8]    │
└───────────────────┘   └───────────────────┘   └───────────────────┘
```

See [Architecture Guide](ARCHITECTURE.md) for detailed documentation.

---

## Getting Started

### Prerequisites

- **Go**: Version 1.25 or later
- **NATS Server**: Version 2.10.0+ with JetStream enabled

### Installation

```bash
go get github.com/arloliu/parti
```

### Package Structure

```
github.com/arloliu/parti
├── parti          # Core: Manager, Config, types
├── subscription   # Consumer helpers: WorkerConsumer, BroadcastConsumer
├── strategy       # Assignment strategies: ConsistentHash, RoundRobin
├── source         # Partition sources: Static, NatsKV
├── partition      # Static partitioning: HashPartitioner
└── types          # Shared types: State, Hooks, Partition
```

---

## Quick Start

### Basic Manager Setup

```go
package main

import (
    "context"
    "log"
    "time"

    "github.com/arloliu/parti"
    "github.com/arloliu/parti/source"
    "github.com/arloliu/parti/strategy"
    "github.com/nats-io/nats.go"
    "github.com/nats-io/nats.go/jetstream"
)

func main() {
    // Connect to NATS with JetStream
    nc, _ := nats.Connect(nats.DefaultURL)
    js, _ := jetstream.New(nc)

    // Configure the manager
    cfg := &parti.Config{
        ClusterName:       "my-cluster",
        WorkerIDPrefix:    "worker",
        WorkerIDMax:       99,
        HeartbeatInterval: 5 * time.Second,
    }

    // Define partitions
    partitions := []parti.Partition{
        {ID: "0"}, {ID: "1"}, {ID: "2"}, {ID: "3"},
    }
    src := source.NewStatic(partitions)

    // Create manager with positional arguments:
    // (config, jetstream, source, strategy, ...options)
    mgr, err := parti.NewManager(cfg, js, src, strategy.NewConsistentHash())
    if err != nil {
        log.Fatal(err)
    }

    // Start and wait for stable state
    ctx := context.Background()
    if err := mgr.Start(ctx); err != nil {
        log.Fatal(err)
    }

    // Wait for assignment
    for mgr.State() != parti.StateStable {
        time.Sleep(100 * time.Millisecond)
    }

    // Get assigned partitions
    assignment := mgr.CurrentAssignment()
    log.Printf("Assigned partitions: %v", assignment.Partitions)

    // Process work...

    // Graceful shutdown
    mgr.Stop(ctx)
}
```

### With Consumer Helper

```go
import (
    "context"
    "github.com/arloliu/parti/subscription"
    "github.com/nats-io/nats.go/jetstream"
)

// Configure worker consumer
cfg := subscription.WorkerConsumerConfig{
    StreamName:      "ORDERS",
    ConsumerPrefix:  "order-processor",
    SubjectTemplate: "orders.{{.PartitionID}}",  // Template with partition placeholder
}

// Create message handler
handler := subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    processOrder(msg)
    return nil  // Return nil for auto-ack, error for auto-nak
})

// Create worker consumer
wc, err := subscription.NewWorkerConsumer(js, cfg, handler)
if err != nil {
    log.Fatal(err)
}

// Create partition source and manager
src := source.NewStatic(partitions)
mgr, _ := parti.NewManager(mgrCfg, js, src, strategy.NewConsistentHash(),
    parti.WithWorkerConsumerUpdater(wc),
)
```

See [Consumer Helpers](CONSUMERS.md) for complete documentation.

---

## Core Concepts

### Partition

A logical division of work. Each partition has:
- **ID**: Unique identifier (e.g., "0", "tenant-a")
- **Weight**: Optional load factor (default: 1.0)
- **Metadata**: Optional key-value data

### Worker

An instance running the Parti Manager. Workers:
- Claim stable IDs from a pool
- Participate in leader election
- Receive partition assignments
- Process assigned partitions

### Leader

A single worker responsible for:
- Calculating partition assignments
- Publishing assignments to NATS KV
- Coordinating two-phase handoffs

### Assignment Strategy

Algorithm determining how partitions distribute across workers:
- **ConsistentHash**: Stable assignments, ~80% affinity during scaling
- **WeightedConsistentHash**: Respects partition weights
- **RoundRobin**: Simple even distribution

See [Strategies & Sources](STRATEGIES.md) for details.

### State Machine

Workers progress through defined states:

```
INIT → CLAIMING_ID → ELECTION → WAITING_ASSIGNMENT → STABLE
                                                        ↓
                                    SCALING ←→ REBALANCING
                                        ↓
                                    DEGRADED
                                        ↓
                                    SHUTDOWN
```

See [Lifecycle Guide](LIFECYCLE.md) for complete state documentation.

---

## When to Use Parti

### Decision Matrix

| Scenario                                            | Recommended Approach                      |
|-----------------------------------------------------|-------------------------------------------|
| Dynamic worker scaling with partition rebalancing   | `parti.Manager` (dynamic partitioning)    |
| Kubernetes StatefulSet with fixed pod count         | `partition` package (static partitioning) |
| Global fan-out events (cache invalidation, control) | `BroadcastConsumer`                       |
| Partitioned workloads with strict ownership         | `WorkerConsumer` + `ProcessingGate`       |
| Stateful partition processing (caches, connections) | Enable two-phase handoff                  |
| High availability during NATS outages               | Configure degraded mode                   |

### Use Case Examples

**Order Processing System:**
- 16 partitions by order ID hash
- WorkerConsumer for order events
- Two-phase handoff for in-flight orders
- ConsistentHash strategy for cache affinity

**Multi-Tenant SaaS:**
- Partitions per tenant
- WeightedConsistentHash (large tenants = higher weight)
- BroadcastConsumer for global config updates

**Real-Time Analytics:**
- Time-window partitions
- RoundRobin strategy (stateless)
- Degraded mode for availability

---

## Next Steps

1. **[Architecture](ARCHITECTURE.md)**: Understand system design and components
2. **[Configuration](CONFIGURATION.md)**: Configure for your environment
3. **[Lifecycle](LIFECYCLE.md)**: Learn about worker states and handoff
4. **[Consumer Helpers](CONSUMERS.md)**: Set up JetStream consumers
5. **[Strategies](STRATEGIES.md)**: Choose assignment strategy and partition source
6. **[Reference](REFERENCE.md)**: Hooks, error handling, best practices

### Examples

See the [examples/](../examples/) directory for complete working examples:
- `examples/basic/` - Simple manager setup
- `examples/defender/` - Production-grade with hooks and monitoring
- `examples/custom-strategy/` - Custom assignment strategy

### Getting Help

- [API Reference](API_REFERENCE.md) - Detailed API documentation
- [Design Documents](design/) - Internal design decisions
- [GitHub Issues](https://github.com/arloliu/parti/issues) - Bug reports and feature requests
