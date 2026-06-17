# Parti Configuration Guide

> Complete configuration reference for the Parti library.

**Related Documentation:**
- [Docs README](README.md) - Documentation map
- [Architecture](ARCHITECTURE.md) - System architecture and concepts
- [Lifecycle & State Management](LIFECYCLE.md) - Worker states and handoff

---

## Table of Contents

1. [Config Structure](#config-structure)
2. [Worker Identity](#worker-identity)
3. [Heartbeat Configuration](#heartbeat-configuration)
4. [Stabilization Windows](#stabilization-windows)
5. [KV Bucket Configuration](#kv-bucket-configuration)
6. [Handoff Configuration](#handoff-configuration)
7. [Degraded Mode Configuration](#degraded-mode-configuration)
8. [Functional Options](#functional-options)
9. [Configuration Presets](#configuration-presets)

---

## Config Structure

The main `Config` struct controls all Manager behavior:

```go
type Config struct {
    // Worker Identity
    WorkerIDPrefix string        // Prefix for worker IDs (default: "worker")
    WorkerIDMin    int           // Minimum ID number (default: 0)
    WorkerIDMax    int           // Maximum ID number (default: 999)
    WorkerIDTTL    time.Duration // TTL for ID claims (default: 75s)

    // Heartbeat Configuration
    HeartbeatInterval time.Duration // Heartbeat publish interval (default: 5s)
    HeartbeatTTL      time.Duration // Heartbeat validity duration (default: 15s)

    // Stabilization Windows
    ColdStartWindow      time.Duration // Window for cold start (default: 30s)
    PlannedScaleWindow   time.Duration // Window for planned scale (default: 10s)
    EmergencyGracePeriod time.Duration // Grace period before emergency (default: 0 = auto)

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

### Applying Defaults

Always call `SetDefaults` before using a config:

```go
cfg := &parti.Config{
    WorkerIDPrefix: "myapp",
    WorkerIDMax:    100,
}
parti.SetDefaults(cfg)

// Now all defaults are applied
mgr, err := parti.NewManager(cfg, js, src, strategy)
```

---

## Worker Identity

Controls how workers claim stable IDs:

```go
cfg := &parti.Config{
    WorkerIDPrefix: "worker",     // Prefix for IDs → "worker-0", "worker-1"
    WorkerIDMin:    0,            // Minimum ID number
    WorkerIDMax:    999,          // Maximum ID number (supports 1000 workers)
    WorkerIDTTL:    75*time.Second, // TTL for ID claims
}
```

### Configuration Options

| Option           | Default    | Description                              |
|------------------|------------|------------------------------------------|
| `WorkerIDPrefix` | `"worker"` | Prefix for generated worker IDs          |
| `WorkerIDMin`    | `0`        | First ID number in the pool              |
| `WorkerIDMax`    | `999`      | Last ID number in the pool               |
| `WorkerIDTTL`    | `75s`      | How long an ID claim remains valid (must be `>= HeartbeatTTL`) |

### Recommendations

- **WorkerIDMax**: Set to at least 2x expected peak worker count
- **WorkerIDTTL**: 3-5x HeartbeatTTL (default 75s is 5x the default HeartbeatTTL); must be `>= HeartbeatTTL` or Start fails validation
- **WorkerIDPrefix**: Use application name for multi-app clusters

---

## Heartbeat Configuration

Controls health signal publishing and failure detection:

```go
cfg := &parti.Config{
    HeartbeatInterval: 5 * time.Second,   // Publish interval
    HeartbeatTTL:      15 * time.Second,  // Validity duration
}
```

### Configuration Options

| Option              | Default | Description                               |
|---------------------|---------|-------------------------------------------|
| `HeartbeatInterval` | `5s`    | How often workers publish heartbeats      |
| `HeartbeatTTL`      | `15s`   | How long a heartbeat is considered valid  |

### Recommendations

- **HeartbeatTTL**: Should be 2-3x HeartbeatInterval
- Faster intervals = faster failure detection, higher KV write load (one write per worker per interval, amplified by JetStream replication and fsync)
- Slower intervals = lower PVC IOPS, slower detection. Defaults are tuned for low IOPS on file-backed JetStream clusters (e.g., NetApp PVC with quota)
- For fast failover, override to `HeartbeatInterval: 2s, HeartbeatTTL: 6s` (previous default)

---

## Stabilization Windows

Controls how long to wait before acting on worker changes:

```go
cfg := &parti.Config{
    ColdStartWindow:      30 * time.Second, // Initial cluster formation
    PlannedScaleWindow:   10 * time.Second, // Scale up/down events
    EmergencyGracePeriod: 0,                // 0 = auto = 1.5 * HeartbeatInterval
}
```

### Configuration Options

| Option                 | Default | Description                                    |
|------------------------|---------|------------------------------------------------|
| `ColdStartWindow`      | `30s`   | Wait time during initial cluster formation     |
| `PlannedScaleWindow`   | `10s`   | Wait time for scale up/down before rebalancing |
| `EmergencyGracePeriod` | `0`     | Grace period before emergency rebalance        |

### When Each Window Applies

- **Cold Start**: First time cluster forms, no existing assignments
- **Planned Scale**: Workers joining or leaving gracefully
- **Emergency**: Worker crash detected (heartbeat timeout)

---

## KV Bucket Configuration

Parti uses multiple NATS KV buckets for coordination:

```go
cfg := &parti.Config{
    KVBuckets: parti.KVBucketConfig{
        StableIDBucket:   "parti-stableid",   // Worker ID claims
        ElectionBucket:   "parti-election",   // Leader election
        HeartbeatBucket:  "parti-heartbeat",  // Health signals
        AssignmentBucket: "parti-assignment", // Partition assignments
        AssignmentTTL:    0,                  // 0 = no expiration

        // Two-phase handoff (optional)
        HandoffBucket: "parti-handoff",       // Handoff claims (bucket has no TTL)
        HandoffTTL:    2 * time.Minute,       // Stuck-handoff sweep TTL, not a bucket TTL
    },
}
```

### Bucket Purposes and Storage Type

| Bucket             | Purpose                   | Recommended TTL                | Default Storage |
|--------------------|---------------------------|--------------------------------|-----------------|
| `StableIDBucket`   | Worker ID claims          | Auto (WorkerIDTTL)             | File            |
| `ElectionBucket`   | Leader lease              | Lease-based                    | File            |
| `HeartbeatBucket`  | Worker health signals     | Auto (HeartbeatTTL)            | File            |
| `AssignmentBucket` | Partition assignments     | 0 (no expiration) or very long | File            |
| `HandoffBucket`    | Two-phase handoff claims  | 2-5 minutes                    | File            |

All five coordination buckets use `FileStorage`. The election and heartbeat buckets were switched from `MemoryStorage` to `FileStorage` (in v2.5.0 and v2.6.0 respectively) because a single-node JetStream restart lost the in-memory stream and flapped the fleet `Degraded`↔`Stable`; persisting them survives the restart. The added write IOPS is a flat, partition-count-independent term — moving these coordination buckets to memory was measured to save only ~1–2% of cluster write IOPS (they are not the cost driver), so the durability win dominates. See the "Election Bucket Storage Migration" and "Heartbeat Bucket Storage Migration" sections in [`OPERATIONS.md`](OPERATIONS.md) for migrating existing clusters.

> **Note:** this is the storage type for parti's *coordination* KV buckets. It is unrelated to per-consumer state storage (`WithConsumerMemoryStorage`), which is a separate, conditional IOPS lever covered in [`CONSUMERS.md`](CONSUMERS.md).

If a bucket with a different storage type already exists (e.g., from a prior parti version or pre-provisioned by ops), parti opens it as-is and logs a `Warn` pointing at the manual migration path: remove the bucket with `nats kv rm <bucket>` during a maintenance window, then restart pods so parti recreates it as `FileStorage`.

### Multi-Application Clusters

Use unique bucket names per application:

```go
cfg := &parti.Config{
    KVBuckets: parti.KVBucketConfig{
        StableIDBucket:   "myapp-stableid",
        ElectionBucket:   "myapp-election",
        HeartbeatBucket:  "myapp-heartbeat",
        AssignmentBucket: "myapp-assignment",
    },
}
```

---

## Handoff Configuration

Controls the two-phase handoff process (when `EnableTwoPhaseHandoff` is true):

```go
cfg := &parti.Config{
    EnableTwoPhaseHandoff: true,
    Handoff: parti.HandoffConfig{
        SweepInterval:     30 * time.Second,   // Stale claim cleanup
        MaxRetries:        3,                   // CAS retry limit
        BaseBackoff:       50 * time.Millisecond,
        MaxBackoff:        500 * time.Millisecond,
        Jitter:            0.2,
        DelayAfterPrepare: 0,                   // Testing only
        DelayBeforeStable: 0,                   // Testing only
        PhaseConcurrency:  0,                   // 0 = default 20 in-flight per phase
        ClaimWritePerSec:       0,              // 0 = claim-write rate limiting off
        ClaimWriteBurst:        0,              // burst; must be >=1 when PerSec > 0
        ClaimWriteClusterRate:  0,              // 0 = static per-worker (adaptive overlay off)
    },
}
```

### Configuration Options

| Option              | Default  | Description                                |
|---------------------|----------|--------------------------------------------|
| `SweepInterval`     | `30s`    | How often to clean up stale claims         |
| `MaxRetries`        | `3`      | Max CAS retries for claim updates          |
| `BaseBackoff`       | `50ms`   | Initial retry backoff                      |
| `MaxBackoff`        | `500ms`  | Maximum retry backoff                      |
| `Jitter`            | `0.2`    | Backoff randomization factor (0.0-1.0)     |
| `DelayAfterPrepare` | `0`      | Artificial delay after prepare (testing)   |
| `DelayBeforeStable` | `0`      | Artificial delay before stable (testing)   |
| `PhaseConcurrency`  | `20`     | Max in-flight per-partition KV ops per phase (simultaneity cap) |
| `ClaimWritePerSec`         | `0` (off)| Per-worker token-bucket rate cap on physical claim-writes (throughput cap); opt-in. See [OPERATIONS.md §Claim-Write Rate Limiting](OPERATIONS.md#claim-write-rate-limiting) |
| `ClaimWriteBurst`          | `0`      | Token-bucket burst for `ClaimWritePerSec`; must be ≥ 1 when the rate is > 0 |
| `ClaimWriteClusterRate`    | `0` (off)| Fleet-size-aware overlay: effective rate = `min(ClaimWritePerSec, ClaimWriteClusterRate/N)`; requires `ClaimWritePerSec > 0` and `EnableTwoPhaseHandoff`. See [OPERATIONS.md §Claim-Write Rate Limiting](OPERATIONS.md#claim-write-rate-limiting) |

See [Lifecycle Guide](LIFECYCLE.md#two-phase-handoff) for details on the handoff protocol.

---

## Degraded Mode Configuration

Controls behavior during NATS connectivity issues:

### Behavior Configuration

```go
cfg := &parti.Config{
    DegradedBehavior: parti.DegradedBehaviorConfig{
        EnterThreshold:      10 * time.Second, // Time to enter degraded
        ExitThreshold:       5 * time.Second,  // Time to exit degraded
        KVErrorThreshold:    5,                 // Consecutive KV errors
        KVErrorWindow:       30 * time.Second, // Error counting window
        RecoveryGracePeriod: 15 * time.Second, // Post-recovery grace
    },
}
```

| Option                | Default | Description                                      |
|-----------------------|---------|--------------------------------------------------|
| `EnterThreshold`      | `10s`   | Time without NATS before entering degraded       |
| `ExitThreshold`       | `5s`    | Time with NATS before exiting degraded           |
| `KVErrorThreshold`    | `5`     | Consecutive KV errors to trigger degraded        |
| `KVErrorWindow`       | `30s`   | Time window for counting KV errors               |
| `RecoveryGracePeriod` | `15s`   | Wait after recovery before emergency rebalance   |

### Alert Configuration

```go
cfg := &parti.Config{
    DegradedAlert: parti.DegradedAlertConfig{
        InfoThreshold:     30 * time.Second,   // Escalate to Info
        WarnThreshold:     2 * time.Minute,    // Escalate to Warn
        ErrorThreshold:    5 * time.Minute,    // Escalate to Error
        CriticalThreshold: 10 * time.Minute,   // Escalate to Critical
        AlertInterval:     1 * time.Minute,    // Time between alerts
    },
}
```

See [Lifecycle Guide](LIFECYCLE.md#degraded-mode) for details on degraded mode behavior.

---

## Functional Options

Additional configuration via `NewManager` options:

```go
mgr, err := parti.NewManager(cfg, js, src, strategy,
    parti.WithLogger(logger),
    parti.WithMetrics(metricsCollector),
    parti.WithHooks(hooks),
    parti.WithElectionAgent(customAgent),
    parti.WithWorkerConsumerUpdater(consumer),
)
```

### Available Options

| Option                       | Description                              |
|------------------------------|------------------------------------------|
| `WithLogger(Logger)`         | Set custom logger                        |
| `WithMetrics(MetricsCollector)` | Set metrics collector                 |
| `WithHooks(*Hooks)`          | Set lifecycle hooks                      |
| `WithElectionAgent(ElectionAgent)` | Use custom election agent          |
| `WithWorkerConsumerUpdater(...)` | Register consumer updater            |

---

## Configuration Presets

### Development/Testing

```go
cfg := &parti.Config{
    WorkerIDMax:           10,
    WorkerIDTTL:           5 * time.Second,
    HeartbeatInterval:     500 * time.Millisecond,
    HeartbeatTTL:          2 * time.Second,
    ColdStartWindow:       2 * time.Second,
    PlannedScaleWindow:    1 * time.Second,
    RebalanceCooldown:     1 * time.Second,
}
```

### Production (Balanced)

```go
cfg := &parti.Config{
    WorkerIDMax:           999,
    WorkerIDTTL:           30 * time.Second,
    HeartbeatInterval:     2 * time.Second,
    HeartbeatTTL:          6 * time.Second,
    ColdStartWindow:       30 * time.Second,
    PlannedScaleWindow:    10 * time.Second,
    RebalanceCooldown:     10 * time.Second,
    EnableTwoPhaseHandoff: true,
}
```

### Production (High Availability)

```go
cfg := &parti.Config{
    WorkerIDMax:           999,
    WorkerIDTTL:           30 * time.Second,
    HeartbeatInterval:     1 * time.Second,  // Faster detection
    HeartbeatTTL:          3 * time.Second,
    ColdStartWindow:       15 * time.Second, // Faster startup
    PlannedScaleWindow:    5 * time.Second,
    RebalanceCooldown:     5 * time.Second,
    EnableTwoPhaseHandoff: true,
    DegradedBehavior: parti.DegradedBehaviorConfig{
        EnterThreshold: 5 * time.Second,  // Faster degraded entry
        ExitThreshold:  3 * time.Second,
    },
}
```
