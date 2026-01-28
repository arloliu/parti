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
    WorkerIDTTL    time.Duration // TTL for ID claims (default: 30s)

    // Heartbeat Configuration
    HeartbeatInterval time.Duration // Heartbeat publish interval (default: 2s)
    HeartbeatTTL      time.Duration // Heartbeat validity duration (default: 6s)

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
    WorkerIDTTL:    30*time.Second, // TTL for ID claims
}
```

### Configuration Options

| Option           | Default    | Description                              |
|------------------|------------|------------------------------------------|
| `WorkerIDPrefix` | `"worker"` | Prefix for generated worker IDs          |
| `WorkerIDMin`    | `0`        | First ID number in the pool              |
| `WorkerIDMax`    | `999`      | Last ID number in the pool               |
| `WorkerIDTTL`    | `30s`      | How long an ID claim remains valid       |

### Recommendations

- **WorkerIDMax**: Set to at least 2x expected peak worker count
- **WorkerIDTTL**: 3-5x HeartbeatInterval (default 30s is good)
- **WorkerIDPrefix**: Use application name for multi-app clusters

---

## Heartbeat Configuration

Controls health signal publishing and failure detection:

```go
cfg := &parti.Config{
    HeartbeatInterval: 2 * time.Second,  // Publish interval
    HeartbeatTTL:      6 * time.Second,  // Validity duration
}
```

### Configuration Options

| Option              | Default | Description                               |
|---------------------|---------|-------------------------------------------|
| `HeartbeatInterval` | `2s`    | How often workers publish heartbeats      |
| `HeartbeatTTL`      | `6s`    | How long a heartbeat is considered valid  |

### Recommendations

- **HeartbeatTTL**: Should be 2-3x HeartbeatInterval
- Faster intervals = faster failure detection, more KV traffic
- Slower intervals = reduced overhead, slower detection

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
        HandoffBucket: "parti-handoff",       // Handoff claims
        HandoffTTL:    2 * time.Minute,       // Claim validity
    },
}
```

### Bucket Purposes

| Bucket             | Purpose                   | Recommended TTL                |
|--------------------|---------------------------|--------------------------------|
| `StableIDBucket`   | Worker ID claims          | Auto (WorkerIDTTL)             |
| `ElectionBucket`   | Leader lease              | Lease-based                    |
| `HeartbeatBucket`  | Worker health signals     | Auto (HeartbeatTTL)            |
| `AssignmentBucket` | Partition assignments     | 0 (no expiration) or very long |
| `HandoffBucket`    | Two-phase handoff claims  | 2-5 minutes                    |

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
