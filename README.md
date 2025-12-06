# Parti

[![Go Reference](https://pkg.go.dev/badge/github.com/arloliu/parti.svg)](https://pkg.go.dev/github.com/arloliu/parti)
[![Go Report Card](https://goreportcard.com/badge/github.com/arloliu/parti)](https://goreportcard.com/report/github.com/arloliu/parti)
[![License: Apache](https://img.shields.io/badge/License-Apache-blue.svg)](LICENSE)

**Parti** is a Go library for NATS-based work partitioning that provides dynamic partition assignment across worker instances with stable worker IDs, leader-based coordination, and robust failure handling.

It is designed for building distributed systems where work needs to be sharded across a dynamic set of workers, such as stream processors, job queues, or sharded databases.

## Key Features

- **Stable Worker IDs**: Workers claim stable IDs (e.g., `worker-0`, `worker-1`) that persist across restarts, minimizing assignment churn during rolling updates.
- **Leader-Based Assignment**: A single leader worker calculates assignments, ensuring consistency and preventing split-brain scenarios.
- **Dynamic Partition Discovery**: Supports dynamic partition updates via NATS KV without restarting workers.
- **Two-Phase Handoff**: Implements a Prepare/Commit protocol for partition reassignment, ensuring no partition is processed by two workers simultaneously.
- **Degraded Mode**: Continues operation using cached assignments when NATS connectivity is lost, prioritizing availability over strict consistency during outages.
- **Processing Gate**: Controls message processing flow based on assignment status, preventing processing of revoked partitions.
- **Cache Affinity**: Preserves >80% partition locality during rebalancing using consistent hashing.
- **Weighted Assignment**: Supports partition weights for uneven workload distribution.

## Installation

```bash
go get github.com/arloliu/parti
```

## Quick Start

Here's a complete example of setting up a worker with Parti.

```go
package main

import (
    "context"
    "log"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/arloliu/parti"
    "github.com/arloliu/parti/source"
    "github.com/arloliu/parti/strategy"
    "github.com/arloliu/parti/subscription"
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
            Keys: []string{"orders", string(rune('0' + i))},
        })
    }

    // 3. Configure Manager
    cfg := &parti.Config{
        WorkerIDPrefix: "worker",
        WorkerIDMax:    10,
        WorkerIDTTL:    10 * time.Second,
    }
    parti.SetDefaults(cfg)

    // 4. Create Manager components
    src := source.NewStatic(partitions)
    strat := strategy.NewConsistentHash()

    // 5. Create WorkerConsumer (handles NATS subscriptions)
    // This automatically manages a JetStream consumer for assigned partitions
    consumer, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
        StreamName:      "ORDERS",
        ConsumerPrefix:  "processor",
        SubjectTemplate: "orders.{{.PartitionID}}.complete", // e.g., orders.0.complete
        ProcessingGate: &subscription.ProcessingGateConfig{
            Enabled: true, // Block processing if partition is revoked
        },
    }, handleMessage)
    if err != nil {
        log.Fatal(err)
    }

    // 6. Create and Start Manager
    mgr, err := parti.NewManager(cfg, js, src, strat,
        parti.WithWorkerConsumerUpdater(consumer), // Link consumer to manager
    )
    if err != nil {
        log.Fatal(err)
    }

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    if err := mgr.Start(ctx); err != nil {
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

func handleMessage(ctx context.Context, msg jetstream.Msg) error {
    log.Printf("Processing message on subject: %s", msg.Subject())
    msg.Ack()
    return nil
}
```

## Documentation

- [User Guide](docs/USER_GUIDE.md): Detailed guides on configuration, handoff, and degraded mode.
- [API Reference](docs/API_REFERENCE.md): Comprehensive API documentation.

## License

Apache 2.0 License
