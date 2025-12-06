# Parti User Guide

> **Let's parti(tion), work, scale effortlessly**

**Version**: 1.3.0
**Last Updated**: December 6, 2025
**Library**: `github.com/arloliu/parti`

---

## Table of Contents

1. [Introduction](#introduction)
2. [Getting Started](#getting-started)
3. [Core Concepts](#core-concepts)
4. [Configuration Guide](#configuration-guide)
5. [Worker Lifecycle](#worker-lifecycle)
6. [Stable ID Renewal Lifecycle](#stable-id-renewal-lifecycle)
7. [Two-Phase Handoff](#two-phase-handoff)
8. [Degraded Mode](#degraded-mode)
9. [Worker Consumer](#worker-consumer)
10. [Processing Gate](#processing-gate)
11. [Assignment Strategies](#assignment-strategies)
12. [Partition Sources](#partition-sources)
13. [Hooks & Callbacks](#hooks--callbacks)
14. [Error Handling](#error-handling)

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
    WorkerIDMin    int           // Minimum ID number (default: 0)
    WorkerIDMax    int           // Maximum ID number (default: 999)
    WorkerIDTTL    time.Duration // TTL for ID claims (default: 30s)

    // Heartbeat Configuration
    HeartbeatInterval time.Duration // Heartbeat publish interval (default: 2s)
    HeartbeatTTL      time.Duration // Heartbeat validity duration (default: 6s)

    // Stabilization Windows
    ColdStartWindow      time.Duration // Window for cold start (default: 30s)
    PlannedScaleWindow   time.Duration // Window for planned scale (default: 10s)
    EmergencyGracePeriod time.Duration // Grace period before emergency (default: 0 = auto = 1.5 * HeartbeatInterval)

    // Assignment Configuration
    RebalanceCooldown time.Duration // Min time between rebalances (default: 10s)

    // Handoff Configuration
    EnableTwoPhaseHandoff bool          // Enable prepare/commit protocol (default: false)
    Handoff               HandoffConfig // Tuning for handoff process

    // Degraded Mode Configuration
    DegradedBehavior DegradedBehaviorConfig
    DegradedAlert    DegradedAlertConfig

    // KV Bucket Configuration
    KVBuckets KVBucketConfig
}
```

### Handoff Configuration

Controls the two-phase handoff process.

```go
type HandoffConfig struct {
    SweepInterval     time.Duration // Interval to sweep stale claims (default: 30s)
    MaxRetries        int           // Max CAS retries for claims (default: 3)
    BaseBackoff       time.Duration // Initial backoff for retries (default: 50ms)
    MaxBackoff        time.Duration // Max backoff for retries (default: 500ms)
    Jitter            float64       // Jitter factor (default: 0.2)
    DelayAfterPrepare time.Duration // Artificial delay after prepare (default: 0)
    DelayBeforeStable time.Duration // Artificial delay before stable (default: 0)
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
    RecoveryGracePeriod time.Duration // Grace period after recovery (default: 15s)
}

type DegradedAlertConfig struct {
    InfoThreshold     time.Duration // Duration to trigger Info alert (default: 30s)
    WarnThreshold     time.Duration // Duration to trigger Warn alert (default: 2m)
    ErrorThreshold    time.Duration // Duration to trigger Error alert (default: 5m)
    CriticalThreshold time.Duration // Duration to trigger Critical alert (default: 10m)
    AlertInterval     time.Duration // Minimum time between alerts (default: 1m)
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

## Stable ID Renewal Lifecycle

**Stable Worker IDs** are the foundation of Parti's partition affinity. Each worker claims a stable ID from a pool (e.g., `worker-0`, `worker-1`) stored in NATS KV, which persists across restarts for a TTL window.

### How It Works

The Stable ID lifecycle consists of four key operations managed by an internal `Claimer`:

1. **Claim(ctx)**: Acquires the first available ID from the pool `[WorkerIDMin, WorkerIDMax]`
   - Uses NATS KV `Create` semantics for atomic claiming
   - Tries each ID sequentially until finding an available one
   - Returns `ErrNoAvailableID` if the pool is exhausted
   - ID is valid for `WorkerIDTTL` duration

2. **StartRenewal()**: Starts background renewal to keep the ID alive
   - Renews every `ttl/3` (minimum 100ms)
   - Must be called after `Claim()`
   - Idempotent: subsequent calls return `ErrRenewalAlreadyStarted`
   - Each renewal uses a short timeout context (100ms–5s)
   - Failures are logged but don't stop the loop

3. **Release(ctx)**: Stops renewal and deletes the KV key
   - Frees the ID for immediate reuse by other workers
   - Idempotent: subsequent calls return `ErrNotClaimed`
   - Waits for renewal goroutine to stop before returning

4. **Close()**: Stops renewal but **keeps** the KV key
   - Used for handoff scenarios where the ID should remain claimed
   - After `Close()`, `StartRenewal()` returns `ErrAlreadyClosed`

### Manager Integration

The Manager handles the Stable ID lifecycle automatically:

```go
// During Start():
// 1. Claim ID
workerID, err := claimer.Claim(ctx)
if err != nil {
    return fmt.Errorf("claim ID: %w", err)
}

// 2. Start background renewal (no context needed)
if err := claimer.StartRenewal(); err != nil {
    return fmt.Errorf("start renewal: %w", err)
}

// During Stop():
// Release ID (stops renewal + deletes key)
_ = claimer.Release(ctx)
```

### Key Behaviors

**Renewal Timing:**
- Interval: `WorkerIDTTL / 3` (minimum 100ms)
- Each tick uses its own timeout: `min(max(ttl/3, 100ms), 5s)`
- Failures are retried on the next tick

**Restart Behavior:**
- If a worker restarts within the TTL window, it will reclaim its previous ID
- This preserves partition affinity during rolling updates

**Pool Exhaustion:**
- If all IDs are in use, new workers return `ErrNoAvailableID`
- Increase `WorkerIDMax` to allow more concurrent workers

**Thread Safety:**
- All operations are safe for concurrent use
- Internal state protected by atomics and sync primitives

### Configuration

Control Stable ID behavior via the `Config` struct:

```go
cfg := &parti.Config{
    WorkerIDPrefix: "worker",     // Prefix for IDs
    WorkerIDMin:    0,             // Minimum ID number
    WorkerIDMax:    999,           // Maximum ID number (1000 workers)
    WorkerIDTTL:    30*time.Second, // TTL for ID claims
}
```

**Recommendations:**
- `WorkerIDTTL`: 3-5x `HeartbeatInterval` (default 30s)
- `WorkerIDMax`: Set to maximum expected workers + buffer

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
    // Optional: Enable Processing Gate for strict ownership
    ProcessingGate: &subscription.ProcessingGateConfig{
        Enabled: true,
        AllowedStates: []types.HandoffState{types.HandoffStateStable},
    },
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

### Advanced Features

#### Pull Gating

**Pull gating** optimizes message delivery by suppressing pulls for partitions the worker doesn't own or during disallowed handoff states. This reduces unnecessary NAK churn.

```go
consumer, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
    StreamName:      "EVENTS",
    ConsumerPrefix:  "processor",
    SubjectTemplate: "events.{{.PartitionID}}",

    // Enable pull gating
    PullGatingEnabled: true,

    // Gate configuration required for pull gating
    ProcessingGate: &subscription.ProcessingGateConfig{
        Enabled: true,
        AllowedStates: []types.HandoffState{types.HandoffStateStable},
    },
}, handler)
```

**Benefits:**
- Reduces server load by not fetching messages that will be NAKed
- Minimizes message redelivery delays during handoffs
- Improves overall system efficiency

#### Graceful Drain on Remove

When a partition is removed from a worker's assignment, you can enable graceful draining to wait for pending acknowledgements before canceling the consumer loop.

```go
consumer, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
    StreamName:      "EVENTS",
    ConsumerPrefix:  "processor",
    SubjectTemplate: "events.{{.PartitionID}}",

    // Enable drain on remove
    DrainOnRemove:        true,
    DrainOnRemoveTimeout: 10 * time.Second,
}, handler)
```

**Behavior:**
- When a subject is removed, the consumer stops issuing new pulls
- Waits for pending acknowledgements to reach zero
- Times out after `DrainOnRemoveTimeout` if ACKs don't complete
- Reduces NAK churn and message gaps during scale-down

**Use Case**: Stateful message processing where in-flight work should complete before reassignment.

#### Concurrency Limits

Control the maximum number of concurrent per-subject consumers:

```go
consumer, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
    StreamName:      "EVENTS",
    ConsumerPrefix:  "processor",
    SubjectTemplate: "events.{{.PartitionID}}",

    // Limit concurrent subjects
    MaxConcurrentSubjects: 100,

    // Control in-flight messages per subject
    MaxAckPending: 10,
}, handler)
```

**Configuration:**
- **`MaxConcurrentSubjects`**: Caps total number of per-subject consumer loops (default: unlimited)
- **`MaxAckPending`**: Limits unacknowledged messages per subject consumer (default: server default)

#### Retry and Backoff Configuration

Customize retry behavior for control-plane operations:

```go
consumer, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
    StreamName:      "EVENTS",
    ConsumerPrefix:  "processor",
    SubjectTemplate: "events.{{.PartitionID}}",

    Retry: subscription.RetryConfig{
        Backoff:    100 * time.Millisecond,
        Max:        5 * time.Second,
        Multiplier: 1.6,
        Base:       200 * time.Millisecond,
    },
}, handler)
```

**Use Case**: Tune retry behavior for environments with transient network issues or high JetStream load.

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
    ProcessingGate: &subscription.ProcessingGateConfig{
        Enabled: true,
        AllowedStates: []types.HandoffState{types.HandoffStateStable, types.HandoffStateCommit},
        WarmupDuration: 10 * time.Second,
        WarmupAllowedStates: []types.HandoffState{types.HandoffStateStable},
        NakDelay: 100 * time.Millisecond,
        NakJitter: 0.2,
        Debug: false,
    },
}, handler)
```

**Configuration Options:**

- **`Enabled`**: Toggle the gate on/off (default: `false`)
- **`AllowedStates`**: States that permit message processing (default: `[StateStable]`)
  - `[StateStable]`: Strict consistency - only process when ownership is stable
  - `[StateStable, StateCommit]`: Higher availability - allow processing during handoff
- **`WarmupDuration`**: Duration of warm-up phase with relaxed state restrictions (default: `0` - disabled)
- **`WarmupAllowedStates`**: States permitted during warm-up (default: `[StateStable]` if `WarmupDuration > 0`)
- **`NakDelay`**: Base delay for NAK when denied (default: `100ms`)
- **`NakJitter`**: Fractional jitter applied to NakDelay (default: `0.0`)
- **`Debug`**: Enable verbose logging for NAK decisions (default: `false`)

### Warm-up Phase

The warm-up phase provides a grace period during consumer startup when state restrictions can be relaxed:

```go
ProcessingGate: &subscription.ProcessingGateConfig{
    Enabled: true,
    // Steady-state: Allow Stable and Commit states
    AllowedStates: []types.HandoffState{
        types.HandoffStateStable,
        types.HandoffStateCommit,
    },
    // Warm-up: Only allow Stable state for 10 seconds
    WarmupDuration: 10 * time.Second,
    WarmupAllowedStates: []types.HandoffState{
        types.HandoffStateStable,
    },
}
```

**Use Case**: Prevents processing messages in transient states immediately after startup, giving the ownership resolver time to synchronize with the current assignment state.

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
    OnAssignmentChanged  func(ctx context.Context, oldPartitions, newPartitions []Partition) error
    OnStateChanged       func(ctx context.Context, from, to State) error
    OnError              func(ctx context.Context, err error) error
    OnLeadershipChanged  func(ctx context.Context, isLeader bool) error
    OnPartitionsAssigned func(ctx context.Context, partitions []Partition) error
    OnPartitionsRevoked  func(ctx context.Context, partitions []Partition) error
    OnDegraded           func(ctx context.Context, reason string) error
}
```

### OnDegraded

Called when the manager enters degraded mode.

```go
hooks.OnDegraded = func(ctx context.Context, reason string) error {
    log.Printf("ALERT: System degraded due to %s", reason)
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
