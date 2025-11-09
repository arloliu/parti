# Parti User Guide

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
6. [Assignment Strategies](#assignment-strategies)
7. [Partition Sources](#partition-sources)
8. [Hooks & Callbacks](#hooks--callbacks)
9. [Error Handling](#error-handling)
10. [Degraded Mode](#degraded-mode)
11. [Best Practices](#best-practices)

---

## Introduction

### What is Parti?

Parti is a Go library for NATS-based work partitioning that provides dynamic partition assignment across worker instances with stable worker IDs and leader-based coordination.

### Key Features

- **Stable Worker IDs**: Workers claim stable IDs (e.g., "worker-0", "worker-1") for consistent assignment during rolling updates
- **Leader-Based Assignment**: One worker calculates assignments without external coordination
- **Adaptive Rebalancing**: Different stabilization windows for cold start (30s) vs planned scale (10s)
- **Cache Affinity**: Preserves >80% partition locality during rebalancing with consistent hashing
- **Weighted Assignment**: Supports partition weights for load balancing
- **Graceful Operations**: Proper shutdown, rolling updates, crash recovery

### Use Cases

1. **Distributed Message Consumers**: Replace Kafka consumer groups with NATS-based partitioning
2. **Work Queue Processors**: Stable partition assignment for stateful processing
3. **Microservices Coordination**: Coordinated work distribution across service instances

---

## Getting Started

### Prerequisites

- **Go**: Version 1.24 or later
- **NATS Server**: Version 2.9.0+ with JetStream enabled
- **NATS KV Store**: Configured and accessible

### Installation

```bash
go get github.com/arloliu/parti
```

### Quick Start Example

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
    "github.com/nats-io/nats.go"
)

func main() {
    // 1. Connect to NATS
    nc, err := nats.Connect(nats.DefaultURL)
    if err != nil {
        log.Fatal(err)
    }
    defer nc.Close()

    // 2. Define partitions (static example)
    partitions := []parti.Partition{
        {Keys: []string{"topic1", "partition-0"}, Weight: 100},
        {Keys: []string{"topic1", "partition-1"}, Weight: 100},
        {Keys: []string{"topic1", "partition-2"}, Weight: 100},
    }
    src := source.NewStatic(partitions)

    // 3. Configure manager
    cfg := &parti.Config{
        WorkerIDPrefix:    "worker",
        WorkerIDMin:       0,
        WorkerIDMax:       63,
        HeartbeatInterval: 2 * time.Second,
        HeartbeatTTL:      6 * time.Second,
    }

    // 4. Create assignment strategy
    assignStrategy := strategy.NewConsistentHash(
        strategy.WithVirtualNodes(150),
    )

    // 5. Set up hooks for assignment changes
    hooks := &parti.Hooks{
        OnAssignmentChanged: func(ctx context.Context, added, removed []parti.Partition) error {
            log.Printf("Added partitions: %v", added)
            log.Printf("Removed partitions: %v", removed)

            // Subscribe to added partitions
            for _, p := range added {
                log.Printf("Subscribing to partition: %v", p.Keys)
                // Your subscription logic here
            }

            // Unsubscribe from removed partitions
            for _, p := range removed {
                log.Printf("Unsubscribing from partition: %v", p.Keys)
                // Your unsubscription logic here
            }

            return nil
        },
    }

    // 6. Create manager
    js, _ := jetstream.New(nc)
    mgr, err := parti.NewManager(cfg, js, src, assignStrategy, parti.WithHooks(hooks))
    if err != nil {
        log.Fatal(err)
    }

    // 7. Start manager
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    if err := mgr.Start(ctx); err != nil {
        log.Fatalf("Failed to start manager: %v", err)
    }

    log.Printf("Manager started with worker ID: %s", mgr.WorkerID())
    log.Printf("Is leader: %v", mgr.IsLeader())

    // 8. Handle graceful shutdown
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
    <-sigCh

    log.Println("Shutting down...")
    if err := mgr.Stop(ctx); err != nil {
        log.Printf("Error during shutdown: %v", err)
    }
}
```

---

## Core Concepts

### Worker Lifecycle States

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

Shutdown:
```
STABLE → SHUTDOWN → [TERMINATED]
```

#### State Descriptions

| State | Description | Duration |
|-------|-------------|----------|
| `Init` | Initial state before any operations | Instant |
| `ClaimingID` | Claiming a stable worker ID from NATS KV | 1-2s |
| `Election` | Participating in leader election | 2-5s |
| `WaitingAssignment` | Waiting for initial partition assignment | 1-3s |
| `Stable` | Normal operation with stable assignment | Continuous |
| `Scaling` | Stabilization window after topology change | 10-30s |
| `Rebalancing` | Leader calculating new assignments | 1-5s |
| `Emergency` | Emergency rebalancing (crash detected) | Immediate |
| `Shutdown` | Graceful shutdown in progress | 5-10s |

### Stable Worker IDs

Each worker claims a stable ID from a configured range (e.g., `worker-0` to `worker-63`). Stable IDs enable:

- **Consistent assignment** during rolling updates
- **Cache affinity** preservation (>80% locality)
- **Predictable scaling** behavior

Workers search sequentially for available IDs and claim them atomically in NATS KV with a TTL. The ID is renewed periodically via heartbeat.

### Leader Election

After claiming an ID, workers participate in leader election:

- **Single Leader**: One worker becomes leader via election agent or NATS KV
- **Leader Responsibilities**: Monitor heartbeats, detect topology changes, calculate assignments
- **Automatic Failover**: New election triggered if leader crashes

### Partition Assignment

The leader calculates partition assignments based on:

1. **Active Workers**: Workers with valid heartbeats
2. **Available Partitions**: From configured `PartitionSource`
3. **Assignment Strategy**: Consistent hashing, round-robin, or custom
4. **Partition Weights**: For load balancing (optional)

Assignments are versioned and published to NATS KV for all workers to consume.

---

## Configuration Guide

### Config Structure

```go
type Config struct {
    // Worker Identity
    WorkerIDPrefix string        // Prefix for worker IDs (default: "worker")
    WorkerIDMin    int            // Minimum ID number (default: 0)
    WorkerIDMax    int            // Maximum ID number (default: 99)
    WorkerIDTTL    time.Duration  // TTL for ID claims (default: 30s)

    // Heartbeat Configuration
    HeartbeatInterval time.Duration  // Heartbeat publish interval (default: 2s)
    HeartbeatTTL      time.Duration  // Heartbeat validity duration (default: 6s)

    // Stabilization Windows
    ColdStartWindow    time.Duration  // Window for cold start (default: 30s)
    PlannedScaleWindow time.Duration  // Window for planned scale (default: 10s)
    EmergencyGracePeriod time.Duration // Grace period before emergency (default: 1.5s)

    // Timeouts
    OperationTimeout time.Duration  // Timeout for KV/partition ops (default: 10s)
    ElectionTimeout  time.Duration  // Timeout for election ops (default: 5s)
    StartupTimeout   time.Duration  // Timeout for Start() (default: 30s)
    ShutdownTimeout  time.Duration  // Timeout for Stop() (default: 10s)

    // Assignment Configuration
    Assignment AssignmentConfig

    // KV Bucket Configuration (optional)
    KVBucket KVBucketConfig
}

type AssignmentConfig struct {
    MinRebalanceThreshold float64       // Min imbalance to rebalance (default: 0.15)
    MinRebalanceInterval  time.Duration // Min time between rebalances (default: 10s)
}
```

### Configuration Defaults

The library provides sensible defaults via `parti.SetDefaults()`:

```go
cfg := &parti.Config{
    WorkerIDPrefix: "worker",
    WorkerIDMax:    63,
}
parti.SetDefaults(cfg)
// Now cfg has all default values filled in
```

### Development Configuration

For local development with fewer workers:

```go
cfg := &parti.Config{
    WorkerIDPrefix:       "dev-worker",
    WorkerIDMin:          0,
    WorkerIDMax:          2,
    WorkerIDTTL:          10 * time.Second,
    HeartbeatInterval:    1 * time.Second,
    HeartbeatTTL:         3 * time.Second,
    ColdStartWindow:      5 * time.Second,
    PlannedScaleWindow:   2 * time.Second,
    OperationTimeout:     5 * time.Second,
    ElectionTimeout:      3 * time.Second,
    StartupTimeout:       15 * time.Second,
    ShutdownTimeout:      5 * time.Second,
}
cfg.Assignment.MinRebalanceThreshold = 0.1
cfg.Assignment.MinRebalanceInterval = 2 * time.Second
```

### Production Configuration

For production with 64 workers:

```go
cfg := &parti.Config{
    WorkerIDPrefix:       "worker",
    WorkerIDMin:          0,
    WorkerIDMax:          199,        // Support up to 200 workers
    WorkerIDTTL:          30 * time.Second,
    HeartbeatInterval:    2 * time.Second,
    HeartbeatTTL:         6 * time.Second,
    ColdStartWindow:      30 * time.Second,
    PlannedScaleWindow:   10 * time.Second,
    OperationTimeout:     10 * time.Second,
    ElectionTimeout:      5 * time.Second,
    StartupTimeout:       30 * time.Second,
    ShutdownTimeout:      10 * time.Second,
}
cfg.Assignment.MinRebalanceThreshold = 0.15
cfg.Assignment.MinRebalanceInterval = 10 * time.Second
```

### Tuning Guidelines

#### Heartbeat Configuration

- **Faster Detection**: Reduce `HeartbeatInterval` (increases network traffic)
- **Slower Detection**: Increase `HeartbeatInterval` (reduces overhead)
- **Rule of Thumb**: `HeartbeatTTL = 3 × HeartbeatInterval`

#### Stabilization Windows

- **Cold Start**: Allow time for all initial workers to start (30s recommended)
- **Planned Scale**: Shorter for faster response during rolling updates (10s recommended)
- **Emergency Grace**: Prevents flapping from transient issues (1.5s recommended)

#### Rebalance Control

- **Threshold**: Higher values (0.2) reduce rebalancing frequency
- **Interval**: Longer intervals (15s) prevent thrashing during rapid changes
- **Trade-off**: Balance between load distribution and stability

---

## Worker Lifecycle

### Starting a Worker

```go
ctx := context.Background()
err := mgr.Start(ctx)
if err != nil {
    log.Fatalf("Failed to start: %v", err)
}
```

`Start()` performs:
1. Claims stable worker ID
2. Starts heartbeat publisher
3. Participates in leader election
4. Waits for initial assignment
5. Returns once stable state reached

### Stopping a Worker

```go
ctx := context.Background()
err := mgr.Stop(ctx)
if err != nil {
    log.Printf("Error during stop: %v", err)
}
```

`Stop()` performs:
1. Transitions to shutdown state
2. Stops heartbeat publisher
3. Releases leader lock (if leader)
4. Releases stable worker ID
5. Cleans up internal resources

### Checking Worker Status

```go
// Get current worker ID
workerID := mgr.WorkerID()

// Check if this worker is the leader
isLeader := mgr.IsLeader()

// Get current state
state := mgr.State()

// Get current assignment
assignment := mgr.CurrentAssignment()
log.Printf("Version: %d, Partitions: %d", assignment.Version, len(assignment.Partitions))
```

### Refreshing Partitions

When your application knows partitions have changed:

```go
ctx := context.Background()
err := mgr.RefreshPartitions(ctx)
if err != nil {
    log.Printf("Failed to refresh partitions: %v", err)
}
```

This triggers the leader to recalculate assignments with the updated partition list.

---

## Assignment Strategies

### Consistent Hashing (Recommended)

Uses a hash ring with virtual nodes to minimize partition movement during scaling.

```go
strategy := strategy.NewConsistentHash(
    strategy.WithVirtualNodes(150),  // More nodes = better distribution
)
```

**Characteristics:**
- **Cache Affinity**: >80% partition locality preserved during rebalancing
- **Movement**: Only 15-20% of partitions move when scaling
- **Distribution**: Even distribution with virtual nodes
- **Performance**: O(W × V + P × log(W × V)) where W=workers, V=vnodes, P=partitions

**When to Use:**
- Stateful processing with caching
- Rolling updates with minimal disruption
- Large partition counts (1000+)

### Round Robin

Simple strategy that distributes partitions evenly in a round-robin fashion.

```go
strategy := strategy.NewRoundRobin()
```

**Characteristics:**
- **Movement**: ~50% of partitions move during scaling
- **Distribution**: Perfectly even
- **Simplicity**: Predictable and easy to debug
- **Performance**: O(P) where P=partitions

**When to Use:**
- Stateless processing (no cache)
- Small partition counts (<100)
- Simple deployment scenarios

### Custom Strategy

Implement the `AssignmentStrategy` interface:

```go
type CustomStrategy struct {
    // your fields
}

func (s *CustomStrategy) Assign(
    workers []string,
    partitions []parti.Partition,
) (map[string][]parti.Partition, error) {
    assignments := make(map[string][]parti.Partition)

    // Your assignment logic here
    // Example: Weighted round-robin
    for i, p := range partitions {
        workerIdx := i % len(workers)
        workerID := workers[workerIdx]
        assignments[workerID] = append(assignments[workerID], p)
    }

    return assignments, nil
}
```

---

## Partition Sources

### Static Source

Fixed list of partitions defined at startup:

```go
partitions := []parti.Partition{
    {Keys: []string{"topic1", "p0"}, Weight: 100},
    {Keys: []string{"topic1", "p1"}, Weight: 150},
    {Keys: []string{"topic2", "p0"}, Weight: 100},
}
src := source.NewStatic(partitions)
```

### Custom Source

Implement the `PartitionSource` interface for dynamic discovery:

```go
type DatabasePartitionSource struct {
    db *sql.DB
}

func (s *DatabasePartitionSource) ListPartitions(ctx context.Context) ([]parti.Partition, error) {
    rows, err := s.db.QueryContext(ctx, "SELECT id, weight FROM partitions WHERE active = true")
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var partitions []parti.Partition
    for rows.Next() {
        var id string
        var weight int64
        if err := rows.Scan(&id, &weight); err != nil {
            return nil, err
        }
        partitions = append(partitions, parti.Partition{
            Keys:   []string{"work", id},
            Weight: weight,
        })
    }

    return partitions, rows.Err()
}
```

### Partition Structure

```go
type Partition struct {
    Keys   []string  // Hierarchical partition keys
    Weight int64     // Relative processing cost (default: 100)
}
```

**Examples:**

```go
// Kafka-style: topic + partition number
{Keys: []string{"orders", "0"}, Weight: 100}

// Multi-level: datacenter + service + shard
{Keys: []string{"us-west", "payment", "shard-1"}, Weight: 150}

// Simple: single identifier
{Keys: []string{"queue-a"}, Weight: 100}
```

---

## Hooks & Callbacks

### Hook Types

```go
type Hooks struct {
    // Called when partition assignment changes
    OnAssignmentChanged func(ctx context.Context, added, removed []Partition) error

    // Called when worker state transitions
    OnStateChanged func(ctx context.Context, from, to State) error

    // Called when a recoverable error occurs
    OnError func(ctx context.Context, err error) error
}
```

### Assignment Change Hook

React to partition additions/removals. When using the single-consumer JetStream pattern via `WithWorkerConsumerUpdater`, keep this hook lightweight (metrics, logging) because the Manager will already have reconciled the durable consumer subjects before invoking it.

```go
hooks := &parti.Hooks{
    OnAssignmentChanged: func(ctx context.Context, added, removed []parti.Partition) error {
        metrics.RecordAssignmentDelta(added, removed) // example metrics side effect
        return nil
    },
}
```

### State Change Hook

Monitor worker state transitions:

```go
hooks := &parti.Hooks{
    OnStateChanged: func(ctx context.Context, from, to parti.State) error {
        log.Printf("State transition: %s -> %s", from, to)

        // React to specific transitions
        switch to {
        case parti.StateStable:
            log.Println("Worker is now stable and processing")
        case parti.StateEmergency:
            log.Println("Emergency rebalancing triggered!")
        case parti.StateShutdown:
            log.Println("Worker shutting down")
        }

        return nil
    },
}
```

### Error Hook

Handle recoverable errors:

```go
hooks := &parti.Hooks{
    OnError: func(ctx context.Context, err error) error {
        log.Printf("Recoverable error: %v", err)

        // Report to error tracking system
        reportError(err)

        return nil
    },
}
```

### Hook Execution Behavior

**IMPORTANT**: Hooks run asynchronously in background goroutines:

- Hooks may not complete before `Stop()` returns
- Context is cancelled during shutdown
- Hook errors are logged but don't fail manager operations
- Hooks should complete quickly (<1 second recommended)
- Make hooks idempotent (may be called multiple times)

---

## Error Handling

### Error Types

```go
// Configuration errors
parti.ErrInvalidConfig
parti.ErrNATSConnectionRequired
parti.ErrPartitionSourceRequired
parti.ErrAssignmentStrategyRequired

// Operational errors
parti.ErrStableIDExhausted      // All IDs in range claimed
parti.ErrNATSConnectionLost      // NATS connection lost
parti.ErrAssignmentVersionMismatch // Stale assignment
parti.ErrElectionFailed          // Leader election failed
```

### Error Handling Patterns

#### Startup Errors

```go
js, _ := jetstream.New(nc)
mgr, err := parti.NewManager(cfg, js, src, strategy)
if err != nil {
    if errors.Is(err, parti.ErrInvalidConfig) {
        log.Fatal("Invalid configuration:", err)
    }
    log.Fatal("Failed to create manager:", err)
}

ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

if err := mgr.Start(ctx); err != nil {
    if errors.Is(err, parti.ErrStableIDExhausted) {
        log.Fatal("All worker IDs are claimed - increase WorkerIDMax")
    }
    if errors.Is(err, context.DeadlineExceeded) {
        log.Fatal("Startup timeout - check NATS connectivity")
    }
    log.Fatal("Failed to start:", err)
}
```

#### Runtime Errors

Use the error hook to handle recoverable errors:

```go
hooks := &parti.Hooks{
    OnError: func(ctx context.Context, err error) error {
        if errors.Is(err, parti.ErrNATSConnectionLost) {
            log.Println("NATS connection lost - manager will retry")
            // Alert operations team
            sendAlert("NATS connection lost")
        }
        return nil
    },
}
```

---

## Degraded Mode

### Overview

**Degraded mode** is a resilience feature that allows workers to continue processing with cached partition assignments when NATS connectivity is lost or KV operations repeatedly fail.

**Philosophy**: *"Stale but stable is better than fresh but broken"*

### How It Works

When a worker detects sustained NATS connectivity issues:

1. **Enters Degraded Mode** (`StateDegraded`)
2. **Freezes current assignment** (no changes accepted)
3. **Continues processing** with cached partitions
4. **Emits periodic alerts** (Info → Warn → Error → Critical)
5. **Automatically recovers** when connectivity is restored

**Key Behavior**:
- ✅ Partitions remain assigned (frozen cache)
- ✅ Workers continue processing their partitions
- ✅ No emergency rebalancing triggered by staleness
- ❌ No new assignments accepted
- ❌ No rebalancing operations performed

### Entry Conditions

Workers enter degraded mode when:

1. **NATS Disconnection**: Connection lost for `EnterThreshold` duration
2. **KV Errors**: `KVErrorThreshold` consecutive errors within `KVErrorWindow`

### Exit Conditions

Workers exit degraded mode when:

1. **NATS Reconnection**: Connection stable for `ExitThreshold` duration
2. **KV Operations Succeed**: Successful KV operations resume

### Configuration

#### DegradedBehaviorConfig

Controls when degraded mode is entered/exited:

```go
cfg.DegradedBehavior = parti.DegradedBehaviorConfig{
    EnterThreshold:      10 * time.Second,  // Time without NATS before degraded
    ExitThreshold:       5 * time.Second,   // Time with NATS before recovery
    KVErrorThreshold:    5,                 // Consecutive KV errors
    KVErrorWindow:       30 * time.Second,  // Window for counting errors
    RecoveryGracePeriod: 30 * time.Second,  // Grace after recovery
}
```

#### DegradedAlertConfig

Controls alert emission during degraded mode:

```go
cfg.DegradedAlert = parti.DegradedAlertConfig{
    InfoThreshold:     1 * time.Minute,   // Info alert after 1 min
    WarnThreshold:     5 * time.Minute,   // Warn alert after 5 min
    ErrorThreshold:    15 * time.Minute,  // Error alert after 15 min
    CriticalThreshold: 30 * time.Minute,  // Critical alert after 30 min
    AlertInterval:     30 * time.Second,  // Min time between alerts
}
```

### Configuration Presets

Use presets for common scenarios:

```go
// Conservative: Longer thresholds, less noisy
cfg.DegradedAlert = parti.DegradedAlertPreset("conservative")
cfg.DegradedBehavior = parti.DegradedBehaviorPreset("conservative")

// Balanced: Default configuration
cfg.DegradedAlert = parti.DegradedAlertPreset("balanced")
cfg.DegradedBehavior = parti.DegradedBehaviorPreset("balanced")

// Aggressive: Shorter thresholds, early detection
cfg.DegradedAlert = parti.DegradedAlertPreset("aggressive")
cfg.DegradedBehavior = parti.DegradedBehaviorPreset("aggressive")
```

### Handling Degraded Alerts

Use the `OnDegradedAlert` hook to receive alerts:

```go
hooks := &parti.Hooks{
    OnDegradedAlert: func(ctx context.Context, level string, duration time.Duration) error {
        switch level {
        case "Critical":
            // Page on-call engineer
            pagerDuty.Alert("Parti degraded for %v - NATS connectivity issue", duration)

        case "Error":
            // Send urgent notification
            slack.Notify("#ops-urgent", "Parti degraded for %v", duration)

        case "Warn":
            // Log warning
            log.Warn("Parti degraded for %v - check NATS health", duration)

        case "Info":
            // Informational log
            log.Info("Parti entered degraded mode (%v)", duration)
        }
        return nil
    },
}
```

### Monitoring Degraded Mode

Track degraded mode with metrics:

```go
type PrometheusCollector struct {
    degradedMode      prometheus.Gauge
    cacheAge          prometheus.Gauge
    alertLevel        prometheus.Gauge
    degradedDuration  prometheus.Histogram
    alertsTotal       *prometheus.CounterVec
    cacheFallback     prometheus.Counter
}

func (c *PrometheusCollector) SetDegradedMode(value float64) {
    c.degradedMode.Set(value) // 1.0 = degraded, 0.0 = normal
}

func (c *PrometheusCollector) SetCacheAge(duration time.Duration) {
    c.cacheAge.Set(duration.Seconds())
}

func (c *PrometheusCollector) IncrementAlertEmitted(level string) {
    c.alertsTotal.WithLabelValues(level).Inc()
}
```

**Key Metrics**:
- `parti_degraded_mode`: Current state (1.0 = degraded, 0.0 = normal)
- `parti_cache_age_seconds`: Age of cached assignment
- `parti_alert_level`: Current alert level (0-4)
- `parti_degraded_duration_seconds`: Total time in degraded mode
- `parti_degraded_alerts_total`: Count of alerts by level
- `parti_cache_fallback_total`: Count of cache fallback operations

### Recovery Scenarios

#### Scenario 1: Transient NATS Outage

```
[T+0s]  NATS connection lost
[T+10s] Worker enters degraded mode
[T+15s] Alert: [INFO] Degraded mode active
[T+30s] NATS connection restored
[T+35s] Worker exits degraded mode
[T+35s] Normal operation resumes
```

#### Scenario 2: Prolonged NATS Outage

```
[T+0s]   NATS connection lost
[T+10s]  Worker enters degraded mode
[T+1m]   Alert: [INFO] Degraded mode active
[T+5m]   Alert: [WARN] Degraded mode persisting
[T+15m]  Alert: [ERROR] Prolonged degraded mode
[T+30m]  Alert: [CRITICAL] Extended degraded mode
[T+45m]  NATS connection restored
[T+50s]  Worker exits degraded mode
```

#### Scenario 3: Split-Brain Recovery

```
[T+0s]   NATS outage affects all workers
[T+10s]  All workers enter degraded mode
[T+30s]  NATS restored, leader recovers first
[T+35s]  Leader exits degraded (RecoveryGracePeriod starts)
[T+40s]  Other workers exit degraded
[T+65s]  RecoveryGracePeriod ends
[T+70s]  Leader can trigger emergency rebalancing if needed
```

**Why Recovery Grace Period?**
Prevents false emergency rebalancing when leader recovers before followers. Gives all workers time to reconnect before the leader considers missing workers as failures.

### Best Practices

#### 1. Alert Configuration

- **Conservative for production**: Longer thresholds reduce alert noise
- **Aggressive for development**: Shorter thresholds catch issues early
- **Tune alert intervals**: Balance between visibility and noise

#### 2. Recovery Grace Period

- **Longer than HeartbeatTTL**: Allow time for all workers to reconnect
- **Production default**: 30-60 seconds for distributed environments
- **Fast networks**: 10-15 seconds for local/fast networks

#### 3. Operational Response

- **Info alerts**: Monitor, no immediate action needed
- **Warn alerts**: Investigate NATS health, check connectivity
- **Error alerts**: Escalate to ops team, prepare for manual intervention
- **Critical alerts**: Page on-call, immediate NATS recovery needed

#### 4. Testing

Test degraded mode behavior:

```go
// Simulate NATS outage in integration tests
// (See test/integration/degraded_mode_test.go)

// Verify assignment stability
initialAssignment := mgr.CurrentAssignment()
// ... simulate NATS outage ...
currentAssignment := mgr.CurrentAssignment()
assert.Equal(t, initialAssignment.Version, currentAssignment.Version)
```

---

## Best Practices

### 1. Configuration

- **Use defaults**: Call `parti.SetDefaults()` before customization
- **Validate configuration**: The library validates on `NewManager()`, but catch errors early
- **Separate configs**: Use different configs for dev/staging/prod
- **Document tuning**: Comment why you changed defaults

### 2. Worker Lifecycle

- **Start early**: Start manager early in application lifecycle
- **Handle errors**: Check `Start()` return value before proceeding
- **Graceful shutdown**: Always call `Stop()` with a context timeout
- **Signal handling**: Use proper signal handling for graceful shutdown

```go
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
<-sigCh

shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

if err := mgr.Stop(shutdownCtx); err != nil {
    log.Printf("Error during shutdown: %v", err)
}
```

### 3. Partition Management

- **Static partitions**: Use `source.NewStatic()` for fixed partitions
- **Dynamic partitions**: Implement `PartitionSource` for database-backed partitions
- **Refresh carefully**: Call `RefreshPartitions()` only when necessary
- **Weight appropriately**: Use weights (50-500 range) for load balancing

### 4. Assignment Strategy

- **Use consistent hashing**: For stateful workloads with caching
- **Use round-robin**: For stateless workloads or testing
- **Custom strategies**: Test thoroughly with `strategy_test.go` patterns
- **Virtual nodes**: 100-300 range (more = better distribution, more memory)

### 5. Hooks

- **Keep hooks fast**: Complete in <1 second
- **Respect context**: Check `ctx.Done()` for cancellation
- **Handle errors**: Return errors from hooks for logging
- **Make idempotent**: Hooks may be called multiple times
- **Avoid blocking**: Don't block on long I/O operations

### 6. Observability

- **Use metrics**: Implement `MetricsCollector` for production
- **Use logging**: Implement `Logger` with structured logging
- **Monitor states**: Track state transitions for debugging
- **Track assignments**: Log assignment changes with versions

### 7. Testing

- **Use embedded NATS**: Test with `parti/testing` package
- **Test failure scenarios**: Simulate crashes, network partitions
- **Test scaling**: Verify behavior during rolling updates
- **Integration tests**: Use `testing.Short()` guard for long tests

### 8. Production Deployment

- **Set WorkerIDMax appropriately**: (max workers × 2) for headroom
- **Use election agent**: For production (avoid NATS KV-based election)
- **Monitor heartbeats**: Track worker health via metrics
- **Plan for failures**: Test disaster recovery scenarios

---

## Common Patterns

### Pattern 1: Kubernetes Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: worker
spec:
  replicas: 10
  template:
    spec:
      containers:
      - name: worker
        env:
        - name: WORKER_ID_MAX
          value: "63"  # Support 64 workers
        - name: NATS_URL
          value: "nats://nats:4222"
```

With configuration from environment:

```go
cfg := &parti.Config{
    WorkerIDMax: getEnvInt("WORKER_ID_MAX", 63),
}
```

### Pattern 2: NATS JetStream (Single Durable Consumer)

```go
// Preferred pattern: single durable pull consumer whose FilterSubjects are updated
// automatically by the Manager via parti.WithWorkerConsumerUpdater.
helper, err := subscription.NewDurableHelper(nc, subscription.DurableConfig{
    StreamName:      "work-stream",
    ConsumerPrefix:  "worker",
    SubjectTemplate: "work.{{.PartitionID}}", // PartitionID = keys joined with '.'
    BatchSize:       50,
}, subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    // Process message
    // Decode / handle business logic here
    return msg.Ack()
}))
if err != nil { log.Fatalf("helper init: %v", err) }

js, _ := jetstream.New(nc)
mgr, err := parti.NewManager(cfg, js, src, strategy, parti.WithWorkerConsumerUpdater(helper))
if err != nil { log.Fatalf("manager init: %v", err) }
if err := mgr.Start(context.Background()); err != nil { log.Fatalf("start: %v", err) }

// (Optional) lightweight hook for metrics
hooks := &parti.Hooks{OnAssignmentChanged: func(ctx context.Context, added, removed []parti.Partition) error {
    metrics.RecordAssignmentDelta(added, removed)
    return nil
}}
```

### Pattern 3: Database-Backed Partitions

```go
type DBPartitionSource struct {
    db *sql.DB
}

func (s *DBPartitionSource) ListPartitions(ctx context.Context) ([]parti.Partition, error) {
    rows, err := s.db.QueryContext(ctx, `
        SELECT tenant_id, shard_id, processing_weight
        FROM partitions
        WHERE active = true
        ORDER BY tenant_id, shard_id
    `)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var partitions []parti.Partition
    for rows.Next() {
        var tenantID, shardID string
        var weight int64
        if err := rows.Scan(&tenantID, &shardID, &weight); err != nil {
            return nil, err
        }
        partitions = append(partitions, parti.Partition{
            Keys:   []string{tenantID, shardID},
            Weight: weight,
        })
    }

    return partitions, rows.Err()
}

// Refresh periodically
go func() {
    ticker := time.NewTicker(5 * time.Minute)
    defer ticker.Stop()
    for range ticker.C {
        if err := mgr.RefreshPartitions(ctx); err != nil {
            log.Printf("Failed to refresh partitions: %v", err)
        }
    }
}()
```

---

## Troubleshooting

### Problem: Worker fails to start

**Symptoms**: `Start()` returns error

**Common Causes**:
- NATS connection not established
- All worker IDs claimed (`ErrStableIDExhausted`)
- Invalid configuration (`ErrInvalidConfig`)
- Timeout during startup

**Solutions**:
```go
// Check NATS connection
if !nc.IsConnected() {
    log.Fatal("NATS not connected")
}

// Increase WorkerIDMax if IDs exhausted
cfg.WorkerIDMax = 199  // Support more workers

// Increase startup timeout
cfg.StartupTimeout = 60 * time.Second
```

### Problem: Frequent rebalancing

**Symptoms**: High `OnAssignmentChanged` callback frequency

**Common Causes**:
- Stabilization windows too short
- Rebalance interval too short
- Workers flapping (starting/stopping rapidly)

**Solutions**:
```go
// Increase stabilization windows
cfg.PlannedScaleWindow = 20 * time.Second

// Increase rebalance interval
cfg.Assignment.MinRebalanceInterval = 15 * time.Second

// Increase rebalance threshold
cfg.Assignment.MinRebalanceThreshold = 0.20
```

### Problem: Split-brain (multiple leaders)

**Symptoms**: Multiple workers think they're leader

**Common Causes**:
- Using NATS KV election (not recommended for production)
- Network partition

**Solutions**:
```go
// Use external election agent
electionAgent := NewConsulElectionAgent(consulClient)
js, _ := jetstream.New(nc)
mgr := parti.NewManager(cfg, js, src, strategy,
    parti.WithElectionAgent(electionAgent),
)
```

### Problem: Slow assignment updates

**Symptoms**: Workers take long to receive assignments

**Common Causes**:
- High partition count (10,000+)
- Slow assignment strategy
- Network latency to NATS

**Solutions**:
```go
// Use consistent hashing (faster than custom strategies)
strategy := strategy.NewConsistentHash()

// Reduce virtual nodes if memory is concern
strategy := strategy.NewConsistentHash(
    strategy.WithVirtualNodes(100),  // Reduce from 150
)

// Check NATS latency
log.Printf("NATS RTT: %v", nc.RTT())
```

---

## Next Steps

- **API Reference**: See [API_REFERENCE.md](API_REFERENCE.md) for detailed interface documentation
- **Operations Guide**: See [OPERATIONS.md](OPERATIONS.md) for deployment and monitoring
- **Examples**: Check `examples/` directory for complete working examples
- **Design Docs**: Read `docs/design/` for architectural details
