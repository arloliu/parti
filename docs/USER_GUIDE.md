# Parti User Guide

> **Let's parti(tion), work, scale effortlessly**

**Version**: 2.0.0
**Last Updated**: 2026-04-09
**Library**: `github.com/arloliu/parti/v2`

---

## Documentation Overview

This user guide provides an introduction to Parti. For detailed documentation, see the focused guides below:

| Document                                       | Description                                      |
|------------------------------------------------|--------------------------------------------------|
| [Architecture](ARCHITECTURE.md)                | System architecture, components, data flow       |
| [Configuration Guide](CONFIGURATION.md)        | Configuration options, presets, tuning           |
| [Lifecycle Guide](LIFECYCLE.md)                | Worker states, stable IDs, handoff, degraded mode|
| [Consumer Helpers](CONSUMERS.md)               | Queue, Dynamic, Broadcast, ProcessingGate, and auto-recovery |
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

Parti is a Go library for building partitioned workloads on NATS. It provides a complete toolkit for sharding work across workers — **dynamic partitioning** with leader-coordinated rebalancing (stable worker IDs, two-phase handoff, cache-affinity rebalancing), **static partitioning** for fixed-topology deployments such as Kubernetes StatefulSets, and **resilient JetStream consumers** with auto-recovery from durable deletion. Its headline capability is solving the coordination gap NATS leaves open when both the worker fleet and the partition set change at runtime.

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
| **Auto-Recovery**        | Consumers recreate deleted durables automatically        |

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
go get github.com/arloliu/parti/v2
```

### Package Structure

```
github.com/arloliu/parti/v2    # Root package `parti`: Manager, Config, hooks, errors
├── consumer                   # JetStream consumers: Queue, Static, Dynamic, Broadcast
├── partition                  # Static partition routing and publisher/subscriber helpers
├── strategy                   # Assignment strategies: ConsistentHash, WeightedConsistentHash, RoundRobin
├── source                     # Partition sources: Static, NatsKV
├── types                      # Shared interfaces and metric contracts
├── jsutil                     # JetStream helper utilities
├── kvutil                     # Key-value helper utilities
└── partitest                  # Test helpers for Parti-based systems
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

    "github.com/arloliu/parti/v2"
    "github.com/arloliu/parti/v2/source"
    "github.com/arloliu/parti/v2/strategy"
    "github.com/nats-io/nats.go"
    "github.com/nats-io/nats.go/jetstream"
)

func main() {
    // Connect to NATS with JetStream
    nc, _ := nats.Connect(nats.DefaultURL)
    js, _ := jetstream.New(nc)

    // Configure the manager
    cfg := &parti.Config{
        WorkerIDPrefix:    "worker",
        WorkerIDMax:       99,
        HeartbeatInterval: 5 * time.Second,
    }

    // Define partitions
    partitions := []parti.Partition{
        {Keys: []string{"0"}},
        {Keys: []string{"1"}},
        {Keys: []string{"2"}},
        {Keys: []string{"3"}},
    }
    src := source.NewStatic(partitions)

    // Create manager with positional arguments:
    // (config, jetstream, source, strategy, ...options)
    mgr, err := parti.NewManager(cfg, js, src, strategy.NewConsistentHash())
    if err != nil {
        log.Fatal(err)
    }

    // Start and wait for stable state. Manager.Start returns once the
    // synchronous sanity-check phase completes (StateWaitingAssignment);
    // the initial assignment fetch + apply runs in the background.
    ctx := context.Background()
    if err := mgr.Start(ctx); err != nil {
        log.Fatal(err)
    }

    // Block until the background runner has applied the initial
    // assignment and the manager has reached StateStable.
    if err := <-mgr.WaitState(parti.StateStable, 30*time.Second); err != nil {
        log.Fatalf("manager did not reach StateStable: %v", err)
    }

    // Get assigned partitions
    assignment := mgr.CurrentAssignment()
    log.Printf("Assigned partitions: %v", assignment.Partitions)

    // Process work...

    // Graceful shutdown
    mgr.Stop(ctx)
}
```

### With Dynamic Consumer

```go
import (
    "context"
    "github.com/arloliu/parti/v2/consumer"
    "github.com/nats-io/nats.go/jetstream"
)

// Create message handler
handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    processOrder(msg)
    return nil  // Return nil for auto-ack, error for auto-nak
})

// Create dynamic consumer with positional args + options
c, err := consumer.NewDynamic(
    js,                              // JetStream context
    "ORDERS",                        // streamName
    "order-processor",               // consumerPrefix
    "orders.{{.PartitionID}}",       // subjectTemplate
    handler,                         // MessageHandler
)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

// Create partition source and manager
src := source.NewStatic(partitions)
mgr, _ := parti.NewManager(mgrCfg, js, src, strategy.NewConsistentHash(),
    parti.WithWorkerConsumerUpdater(c),
)
```

See [Consumer Package](CONSUMERS.md) for complete documentation.

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
| Dynamic worker scaling with partition rebalancing   | `parti.Manager` + `consumer.Dynamic`      |
| Kubernetes StatefulSet with fixed pod count         | `consumer.Static`                         |
| Global fan-out events (cache invalidation, control) | `consumer.Broadcast`                      |
| Partitioned workloads with strict ownership         | `consumer.Dynamic` + ProcessingGate       |
| Load-balanced workers (queue group)                 | `consumer.Queue`                          |
| Stateful partition processing (caches, connections) | Enable two-phase handoff                  |
| High availability during NATS outages               | Configure degraded mode                   |
| Consumer durables deleted by server or admin        | `consumer.WithRecoveryStrategy(consumer.RecoverFromNew)` or `consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed)` |

### Use Case Examples

**Order Processing System:**
- 16 partitions by order ID hash
- `consumer.Dynamic` for order events
- Two-phase handoff for in-flight orders
- ConsistentHash strategy for cache affinity

**Multi-Tenant SaaS:**
- Partitions per tenant
- WeightedConsistentHash (large tenants = higher weight)
- `consumer.Broadcast` for global config updates

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
