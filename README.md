# Parti

[![Go Reference](https://pkg.go.dev/badge/github.com/arloliu/parti/v2.svg)](https://pkg.go.dev/github.com/arloliu/parti/v2)
[![Go Report Card](https://goreportcard.com/badge/github.com/arloliu/parti/v2)](https://goreportcard.com/report/github.com/arloliu/parti/v2)
[![License: Apache](https://img.shields.io/badge/License-Apache-blue.svg)](LICENSE)

**Parti** is a Go library for building partitioned workloads on NATS. It provides a complete toolkit for sharding work across workers — **dynamic partitioning** with leader-coordinated rebalancing, **static partitioning** for fixed-topology deployments (e.g. Kubernetes StatefulSets), and **resilient JetStream consumers** with auto-recovery from durable deletion.

| Its headline capability is solving the coordination gap NATS leaves open when workers and partitions change at runtime.

## The Problem

You want to shard work across a fleet of workers — each worker owns a subset of partitions (shards, tenants, key ranges). At runtime, two things change independently:

- **The worker fleet** — pods scale up and down, deploys roll, instances crash and restart.
- **The partition set** — tenants are added or removed, shards split or merge, keyspaces grow.

NATS JetStream gives you durable consumers, but **no built-in way to rebalance partition ownership across a changing fleet**. Without a coordination layer, you end up with two workers consuming the same partition during a reassignment, partitions left unprocessed during a deploy, or assignments that churn every time a pod restarts.

## What Parti Provides

Parti is the coordination layer NATS doesn't ship with — it handles the seamless re-binding of partitions to workers when either side changes:

- **Stable worker IDs** across restarts so rolling updates don't churn assignments.
- **Leader-elected assignment** so every worker agrees on who owns what.
- **Two-phase handoff (Prepare/Commit)** so no partition is processed by two workers during reassignment.
- **Dynamic partition discovery** so you can add or remove partitions without restarting workers.
- **Cache-affinity rebalancing** so >80% partition locality is preserved when the fleet size changes.
- **Degraded-mode operation** so workers keep processing with cached assignments when NATS connectivity is lost.

Use it for stream processors, job queues, sharded caches, or any system where a dynamic set of workers must own a dynamic set of partitions.

## Key Features

### Dynamic Partitioning (`parti.Manager`)

- **Stable Worker IDs**: Workers claim stable IDs (e.g., `worker-0`, `worker-1`) that persist across restarts, minimizing assignment churn during rolling updates.
- **Leader-Based Assignment**: A single leader worker calculates assignments, ensuring consistency and preventing split-brain scenarios.
- **Dynamic Partition Discovery**: Supports dynamic partition updates via NATS KV without restarting workers.
- **Two-Phase Handoff**: Implements a Prepare/Commit protocol for partition reassignment, ensuring no partition is processed by two workers simultaneously.
- **Degraded Mode**: Continues operation using cached assignments when NATS connectivity is lost, prioritizing availability over strict consistency during outages.
- **Processing Gate**: Controls message processing flow based on assignment status, preventing processing of revoked partitions.
- **Cache Affinity**: Preserves >80% partition locality during rebalancing using consistent hashing.
- **Weighted Assignment**: Supports partition weights for uneven workload distribution.

### Consumer Auto-Recovery

- **Automatic Durable Recreation**: When a JetStream durable consumer is unexpectedly deleted (server restart, `InactiveThreshold` expiry, administrative action), the consumer detects the deletion and recreates itself automatically — no worker restart required.
- **Four Recovery Strategies**: Choose how the recreated consumer resumes — skip missed messages ([`RecoverFromNew`](https://pkg.go.dev/github.com/arloliu/parti/v2/consumer#RecoveryStrategy)), replay from the last auto-acknowledged message ([`RecoverFromLastProcessed`](https://pkg.go.dev/github.com/arloliu/parti/v2/consumer#RecoveryStrategy)), or replay from the beginning ([`RecoverFromBeginning`](https://pkg.go.dev/github.com/arloliu/parti/v2/consumer#RecoveryStrategy)).
- **Eager Validation**: Incompatible combinations (e.g., `RecoverFromLastProcessed` on a `Queue` consumer) are rejected at construction time with [`ErrInvalidConfig`](https://pkg.go.dev/github.com/arloliu/parti/v2/consumer#ErrInvalidConfig), not at runtime.
- **Opt-In, Zero-Cost Default**: Recovery is disabled (`RecoveryDisabled`) by default — enabling it requires a single option.

See [Consumer Helpers](docs/CONSUMERS.md#auto-recovery) for the full strategy matrix and per-consumer support table.

### Static Partitioning (`partition` package)

- **Deterministic Routing**: Messages are routed to fixed partitions using xxh3 hashing on partition keys.
- **StatefulSet Integration**: Designed for Kubernetes StatefulSet deployments where each pod handles a fixed partition based on its ordinal.
- **Dual Protocol Support**: Works with both core NATS (`Publisher`/`Subscriber`) and JetStream (`JSPublisher`). Consuming from JetStream partitions is handled by the `consumer` package (`Static`, `Dynamic`).
- **Subject Pattern Templates**: Flexible subject patterns with `{{partition}}` and `{{key}}` placeholders.
- **Zero Coordination**: No leader election or external coordination required—simple and predictable.

See the [partition package README](partition/README.md) for detailed documentation.

## Installation

```bash
go get github.com/arloliu/parti/v2
```

## Quick Start

Here's a complete example of setting up a worker with Parti.

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/arloliu/parti/v2"
    "github.com/arloliu/parti/v2/consumer"
    "github.com/arloliu/parti/v2/source"
    "github.com/arloliu/parti/v2/strategy"
    "github.com/nats-io/nats.go"
    "github.com/nats-io/nats.go/jetstream"
)

func main() {
    // 1. Connect to NATS
    nc, err := nats.Connect(nats.DefaultURL)
    if err != nil {
        log.Fatal(err)
    }
    defer nc.Close()

    js, err := jetstream.New(nc)
    if err != nil {
        log.Fatal(err)
    }

    // 2. Define partitions (e.g., 10 partitions)
    var partitions []parti.Partition
    for i := 0; i < 10; i++ {
        partitions = append(partitions, parti.Partition{
            Keys: []string{"orders", fmt.Sprintf("%d", i)},
        })
    }

    // 3. Configure Manager
    cfg := &parti.Config{
        WorkerIDPrefix: "worker",
        WorkerIDMax:    10,
    }
    if err := parti.SetDefaults(cfg); err != nil {
        log.Fatal(err)
    }

    // 4. Create Manager components
    src := source.NewStatic(partitions)
    hashStrategy := strategy.NewConsistentHash()

    // 5. Create a Dynamic consumer (manages per-partition JetStream consumers)
    // Parti Manager calls consumer.Update() when assignments change.
    handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
        log.Printf("Processing message on subject: %s", msg.Subject())
        return nil // auto-ack on nil return
    })

    dyn, err := consumer.NewDynamic(js, "ORDERS", "processor",
        "orders.{{.PartitionID}}.complete", handler,
        consumer.WithProcessingGate(&consumer.ProcessingGateConfig{Enabled: true}),
    )
    if err != nil {
        log.Fatal(err)
    }
    defer dyn.Stop(context.Background())

    // 6. Create and Start Manager
    mgr, err := parti.NewManager(cfg, js, src, hashStrategy,
        parti.WithWorkerConsumerUpdater(dyn), // Manager calls dyn.Update on assignment changes
    )
    if err != nil {
        log.Fatal(err)
    }

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    // Start returns once the synchronous sanity-check phase
    // (worker-ID claim, KV buckets, election, heartbeat, calculator)
    // succeeds. The initial assignment fetch + apply runs in the
    // background. Use WaitState to block until the manager is ready
    // to process work.
    if err := mgr.Start(ctx); err != nil {
        log.Fatal(err)
    }
    if err := <-mgr.WaitState(parti.StateStable, 30*time.Second); err != nil {
        log.Fatal(err)
    }

    log.Printf("Worker started with ID: %s", mgr.WorkerID())

    // Wait for shutdown signal
    sig := make(chan os.Signal, 1)
    signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
    <-sig

    log.Println("Shutting down...")
    mgr.Stop(ctx)
}
```

## Documentation

- **[Migrating from v1 to v2](docs/MIGRATING_TO_V2.md)**: Breaking changes, import paths, and step-by-step checklist.
- **Guides**
    - [User Guide](docs/USER_GUIDE.md): Introduction, quick start, and core concepts.
    - [Configuration](docs/CONFIGURATION.md): Configuration options, presets, and tuning.
    - [Operations](docs/OPERATIONS.md): Operational guides, metrics, and troubleshooting.
- **Architecture & Concepts**
    - [Architecture](docs/ARCHITECTURE.md): System design, components, and data flow.
    - [Lifecycle](docs/LIFECYCLE.md): Worker states, handoff protocol, and failure handling.
    - [Strategies](docs/STRATEGIES.md): Assignment strategies (consistent hash, etc.).
- **Features**
    - [Consumer Helpers](docs/CONSUMERS.md): JetStream consumer management.
    - [Static Partitioning](docs/STATIC_PARTITIONING.md): Deterministic partitioning without coordination.
- **Reference**
    - [API Reference](docs/API_REFERENCE.md): Detailed API documentation.
    - [Reference](docs/REFERENCE.md): Hooks, error codes, and glossary.

## License

Apache 2.0 License
