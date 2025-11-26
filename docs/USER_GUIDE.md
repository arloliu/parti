# Parti User Guide

> **Let's parti(tion), work, scale effortlessly**

**Version**: 1.0.0
**Last Updated**: November 2, 2025
**Library**: `github.com/arloliu/parti`

---

## Table of Contents

1. [Introduction](#introduction)
2. [Getting Started](#getting-started)
3. [Core Concepts](#core-concepts)
4. [Configuration Guide](#configuration-guide)
5. [Worker Lifecycle](#worker-lifecycle)
6. [Two-Phase Handoff](#two-phase-handoff)
7. [Degraded Mode](#degraded-mode)
8. [Worker Consumer](#worker-consumer)
9. [Processing Gate](#processing-gate)
10. [Assignment Strategies](#assignment-strategies)
11. [Partition Sources](#partition-sources)
12. [Hooks & Callbacks](#hooks--callbacks)
13. [Error Handling](#error-handling)

---

## Introduction

### What is Parti?

Parti is a Go library for NATS-based work partitioning that provides dynamic partition assignment across worker instances with stable worker IDs and leader-based coordination.

### Key Features

- **Stable Worker IDs**: Workers claim stable IDs (e.g., "worker-0", "worker-1") for consistent assignment during rolling updates
- **Leader-Based Assignment**: One worker calculates assignments without external coordination
- **Two-Phase Handoff**: Safer partition reassignment using Prepare/Commit protocol
- **Degraded Mode**: High availability during NATS outages using cached assignments
- **Processing Gate**: Strict ownership enforcement for message processing
- **Cache Affinity**: Preserves >80% partition locality during rebalancing with consistent hashing

---

## Getting Started

### Prerequisites

- **Go**: Version 1.25 or later
- **NATS Server**: Version 2.10.0+ with JetStream enabled

### Installation

```bash
go get github.com/arloliu/parti
```

See [README.md](../README.md) for a Quick Start example.

---

## Core Concepts

### Manager

The `Manager` is the central component that coordinates worker identity, leader election, and partition assignment. Each worker instance runs exactly one Manager.

### Worker ID

A stable identifier (e.g., `worker-0`) claimed from NATS KV. It persists across restarts within a TTL window, ensuring that a restarting pod reclaims its previous ID and partitions.

### Partition

A logical unit of work identified by a list of keys (e.g., `["orders", "0"]`). Partitions are the atomic units of assignment.

### Assignment

A mapping of partitions to workers. Assignments are versioned and stored in NATS KV.

---

## Configuration Guide

### Config Structure

```go
type Config struct {
    // Worker Identity
    WorkerIDPrefix string        // Prefix for worker IDs (default: "worker")
    WorkerIDMax    int           // Maximum ID number (default: 999)
    WorkerIDTTL    time.Duration // TTL for ID claims (default: 30s)

    // Heartbeat Configuration
    HeartbeatInterval time.Duration // Heartbeat publish interval (default: 2s)
    HeartbeatTTL      time.Duration // Heartbeat validity duration (default: 6s)

    // Stabilization Windows
    ColdStartWindow      time.Duration // Window for cold start (default: 30s)
    PlannedScaleWindow   time.Duration // Window for planned scale (default: 10s)
    EmergencyGracePeriod time.Duration // Grace period before emergency (default: 1.5s)

    // Assignment Configuration
    RebalanceCooldown time.Duration // Min time between rebalances (default: 10s)

    // Handoff Configuration
    Handoff HandoffConfig

    // Degraded Mode Configuration
    DegradedBehavior DegradedBehaviorConfig
    DegradedAlert    DegradedAlertConfig

    // KV Bucket Configuration
    KVBucket KVBucketConfig
}
```

### Handoff Configuration

Controls the two-phase handoff process.

```go
type HandoffConfig struct {
    EnableTwoPhaseHandoff bool          // Enable prepare/commit protocol (default: false)
    PrepareTimeout        time.Duration // Max time to wait for prepare ack (default: 10s)
    CommitTimeout         time.Duration // Max time to wait for commit ack (default: 10s)
    StateCheckInterval    time.Duration // Interval to check handoff state (default: 500ms)
}
```

### Degraded Mode Configuration

Controls behavior during NATS outages.

```go
type DegradedBehaviorConfig struct {
    EnterThreshold      time.Duration // Time without NATS before entering degraded (default: 10s)
    ExitThreshold       time.Duration // Time with NATS before exiting degraded (default: 5s)
    KVErrorThreshold    int           // Consecutive KV errors to trigger degraded (default: 5)
    KVErrorWindow       time.Duration // Time window for counting KV errors (default: 30s)
}

type DegradedAlertConfig struct {
    InfoThreshold     time.Duration // Duration to trigger Info alert (default: 1m)
    WarnThreshold     time.Duration // Duration to trigger Warn alert (default: 5m)
    ErrorThreshold    time.Duration // Duration to trigger Error alert (default: 15m)
    CriticalThreshold time.Duration // Duration to trigger Critical alert (default: 30m)
    AlertInterval     time.Duration // Minimum time between alerts (default: 30s)
}
```

---

## Worker Lifecycle

Workers progress through a defined state machine:

```
INIT → CLAIMING_ID → ELECTION → WAITING_ASSIGNMENT → STABLE
```

During scaling:
```
STABLE → SCALING → REBALANCING → STABLE
```

Emergency (crash detected):
```
STABLE → EMERGENCY → STABLE
```

Degraded (NATS outage):
```
STABLE → DEGRADED → STABLE
```

---

## Two-Phase Handoff

When `EnableTwoPhaseHandoff` is true, the manager uses a Prepare/Commit protocol for partition reassignment to ensure zero overlap in processing.

### The Protocol

1.  **Prepare Phase**:
    *   Leader calculates new assignment.
    *   Leader publishes "Prepare" intent.
    *   Workers receive intent and **stop processing** partitions that are being moved (revoked).
    *   Workers acknowledge "Prepare" completion.

2.  **Commit Phase**:
    *   Once all workers acknowledge Prepare (or timeout), Leader publishes "Commit" intent.
    *   Workers receive intent and **start processing** newly assigned partitions.
    *   Workers acknowledge "Commit" completion.

3.  **Stable Phase**:
    *   Once all workers acknowledge Commit, the assignment is finalized.

### Benefits

*   **Consistency**: Guarantees that a partition is never processed by two workers at the same time.
*   **Safety**: Prevents race conditions during rebalancing.

---

## Degraded Mode

**Degraded mode** allows workers to continue processing with cached partition assignments when NATS connectivity is lost.

**Philosophy**: *"Stale but stable is better than fresh but broken"*

### Behavior

When a worker enters `StateDegraded`:
1.  **Freezes Assignment**: The current partition assignment is locked.
2.  **Continues Processing**: The worker keeps processing its assigned partitions.
3.  **Emits Alerts**: Periodic alerts are triggered via `OnDegradedAlert` hook based on duration.
4.  **Ignores Updates**: No new assignments or rebalancing attempts are made.

### Recovery

When NATS connectivity is restored for `ExitThreshold`:
1.  Worker transitions back to `StateStable` (or previous state).
2.  Resumes participation in elections and updates.

### Configuration

Enable and tune via `DegradedBehaviorConfig` and `DegradedAlertConfig`.

---

## Worker Consumer

The `WorkerConsumer` is a high-level helper that manages NATS JetStream subscriptions for assigned partitions. It automatically updates filter subjects when assignments change, minimizing consumer churn and ensuring efficient message delivery.

### Why Use WorkerConsumer?

*   **Single Durable Consumer**: Manages one durable consumer per worker instead of one per partition, reducing broker load.
*   **Dynamic Updates**: Updates filter subjects on-the-fly without restarting the consumer.
*   **Resilience**: Built-in retries, backoff, and health monitoring.
*   **Integration**: Seamlessly integrates with `Manager` via `WithWorkerConsumerUpdater`.

### Basic Usage

Create the consumer and pass it to the manager:

```go
// 1. Define the handler
handler := subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    // Process message
    log.Printf("Received: %s", msg.Subject())
    msg.Ack()
    return nil
})

// 2. Configure and create the consumer
consumer, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
    StreamName:      "EVENTS",
    ConsumerPrefix:  "processor",
    SubjectTemplate: "events.{{.PartitionID}}", // Maps partition ID to subject
    AckWait:         30 * time.Second,
    MaxDeliver:      3,
}, handler)
if err != nil {
    log.Fatal(err)
}

// 3. Create dependencies
// Partition Source: Static list for this example
src := source.NewStatic([]parti.Partition{
    {Keys: []string{"orders", "0"}},
    {Keys: []string{"orders", "1"}},
    {Keys: []string{"orders", "2"}},
})

// Assignment Strategy: Consistent Hashing
strat := strategy.NewConsistentHash(
    strategy.WithVirtualNodes(150),
)

// 4. Register with Manager
mgr, err := parti.NewManager(cfg, js, src, strat,
    parti.WithWorkerConsumerUpdater(consumer),
)
if err != nil {
    log.Fatal(err)
}

// 5. Start the Manager
if err := mgr.Start(context.Background()); err != nil {
    log.Fatal(err)
}
defer mgr.Stop()

// 6. Wait for shutdown signal
<-ctx.Done()
```

### Manual Acknowledgement

By default, `WorkerConsumer` automatically acknowledges messages if the handler returns `nil`. For more control (e.g., asynchronous processing), enable `ManualAck`.

```go
consumer, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
    // ...
    ManualAck:     true,
    MaxAckPending: 100, // Limit concurrent messages
}, handler)

// In handler:
func handle(ctx context.Context, msg jetstream.Msg) error {
    go func() {
        process(msg)
        msg.Ack() // Must Ack manually
    }()
    return nil // Return immediately
}
```

### Health Monitoring

Check the consumer's health status:

```go
health := consumer.Health()
if !health.Healthy {
    log.Printf("Consumer unhealthy! Failures: %d", health.ConsecutiveFailures)
}
```

---

## Processing Gate

The **Processing Gate** is a mechanism in `WorkerConsumer` to enforce partition ownership at the message processing level.

### How It Works

When enabled, the gate checks if the worker currently "owns" the partition for an incoming message before invoking the handler.

*   **Allowed**: Message is processed.
*   **Denied**: Message is NAKed (with backoff) or dropped.

### Configuration

```go
consumer, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
    // ... other config ...
    ProcessingGate: subscription.ProcessingGateConfig{
        Enabled: true,
        AllowedStates: []parti.State{parti.StateStable, parti.StateCommit},
    },
}, handler)
```

### Integration with Handoff

The Processing Gate is aware of the Two-Phase Handoff states.
*   **Prepare Phase**: Gate closes for revoked partitions.
*   **Commit Phase**: Gate opens for new partitions.

---

## Assignment Strategies

### Consistent Hashing (Recommended)

Uses a hash ring with virtual nodes to minimize partition movement.

```go
strategy := strategy.NewConsistentHash(
    strategy.WithVirtualNodes(150),
)
```

### Weighted Consistent Hashing

Advanced strategy that balances load based on partition weights and handles "extreme" partitions (heavy hitters).

*   **Load Balancing**: Distributes partitions to keep worker load within a threshold (default 1.2x average).
*   **Extreme Partition Handling**: Isolates extremely heavy partitions (e.g., >20x average weight) to dedicated workers if possible.

```go
strategy := strategy.NewWeightedConsistentHash(
    strategy.WithWeightedVirtualNodes(150),
    strategy.WithOverloadThreshold(1.2), // Max 20% deviation from average load
    strategy.WithExtremeThreshold(20.0), // Partitions 20x heavier than avg are "extreme"
)
```

### Round Robin

Simple even distribution.

```go
strategy := strategy.NewRoundRobin()
```

---

## Partition Sources

### Static Source

Fixed list of partitions.

```go
src := source.NewStatic([]parti.Partition{
    {Keys: []string{"orders", "0"}},
    {Keys: []string{"orders", "1"}},
})
```

### NATS KV Source

Dynamic source backed by a NATS KeyValue bucket. Updates to the KV key automatically trigger rebalancing.

```go
// 1. Create/Open KV bucket
kv, _ := kvutil.EnsureKVBucket(ctx, js, "config", 0)

// 2. Create source watching key "partitions"
src := source.NewNatsKV(kv, "partitions", logger)

// 3. Update partitions at runtime (from any process)
partitions := []parti.Partition{...}
if err := src.Update(ctx, partitions); err != nil {
    log.Fatal(err)
}
```

### Custom Source

Implement `PartitionSource` interface for dynamic discovery (e.g., from DB).

---

## Hooks & Callbacks

### Available Hooks

```go
type Hooks struct {
    OnAssignmentChanged func(ctx context.Context, added, removed []Partition) error
    OnStateChanged      func(ctx context.Context, from, to State) error
    OnError             func(ctx context.Context, err error) error
    OnDegradedAlert     func(ctx context.Context, level string, duration time.Duration) error
}
```

### OnDegradedAlert

Called periodically when the manager is in degraded mode.

```go
hooks.OnDegradedAlert = func(ctx context.Context, level string, duration time.Duration) error {
    log.Printf("ALERT [%s]: System degraded for %v", level, duration)
    return nil
}
```

---

## Error Handling

### Common Errors

*   `ErrStableIDExhausted`: Increase `WorkerIDMax`.
*   `ErrNATSConnectionLost`: Check network/broker.
*   `ErrDegradedMode`: Operation blocked due to degraded state.

### Recovery

The Manager automatically attempts to recover from most errors (lost connection, lost leadership). Use `OnError` hook for monitoring.
