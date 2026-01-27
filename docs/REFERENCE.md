# Parti Reference

> Hooks, error handling, best practices, and glossary.

**Related Documentation:**
- [User Guide](USER_GUIDE.md) - Getting started and overview
- [Architecture](ARCHITECTURE.md) - System architecture and concepts
- [Configuration Guide](CONFIGURATION.md) - Configuration options
- [Lifecycle Guide](LIFECYCLE.md) - Worker states and handoff

---

## Table of Contents

1. [Hooks & Callbacks](#hooks--callbacks)
2. [Error Handling](#error-handling)
3. [Best Practices](#best-practices)
4. [Glossary](#glossary)

---

## Hooks & Callbacks

Hooks enable integration with external systems for monitoring, alerting, and custom logic.

### Available Hooks

```go
type Hooks struct {
    // State changes
    OnStateChanged      func(ctx context.Context, from, to State) error

    // Leadership
    OnBecameLeader      func(ctx context.Context) error
    OnLostLeadership    func(ctx context.Context) error

    // Assignment changes
    OnAssignment        func(ctx context.Context, partitions []Partition) error

    // Two-phase handoff (when enabled)
    OnPartitionPrepare  func(ctx context.Context, partitions []Partition, incoming bool) error
    OnPartitionCommit   func(ctx context.Context, partitions []Partition, incoming bool) error

    // Degraded mode
    OnDegraded          func(ctx context.Context, reason string) error
    OnDegradedAlert     func(ctx context.Context, level AlertLevel, duration time.Duration) error
    OnRecovered         func(ctx context.Context) error

    // Errors
    OnError             func(ctx context.Context, err error) error
}
```

### Hook Behavior

| Hook                 | When Called                                    | Use Case                    |
|----------------------|------------------------------------------------|-----------------------------|
| `OnStateChanged`     | Every state transition                         | Metrics, logging            |
| `OnBecameLeader`     | Worker wins election                           | Initialize leader resources |
| `OnLostLeadership`   | Worker loses election                          | Cleanup leader resources    |
| `OnAssignment`       | Partition assignment changes                   | Update consumers, caches    |
| `OnPartitionPrepare` | Two-phase: prepare phase                       | Stop processing outgoing    |
| `OnPartitionCommit`  | Two-phase: commit phase                        | Start processing incoming   |
| `OnDegraded`         | Entering degraded mode                         | Alert on-call               |
| `OnDegradedAlert`    | Escalating alerts during degraded              | Escalation workflow         |
| `OnRecovered`        | Exiting degraded mode                          | Clear alerts                |
| `OnError`            | Recoverable errors                             | Error tracking              |

### Hook Implementation

```go
hooks := &parti.Hooks{
    OnStateChanged: func(ctx context.Context, from, to parti.State) error {
        metrics.RecordStateChange(from.String(), to.String())
        log.Info("state transition",
            "from", from,
            "to", to)
        return nil
    },

    OnBecameLeader: func(ctx context.Context) error {
        log.Info("became leader, initializing leader resources")
        return initLeaderResources(ctx)
    },

    OnAssignment: func(ctx context.Context, partitions []parti.Partition) error {
        ids := make([]string, len(partitions))
        for i, p := range partitions {
            ids[i] = p.ID
        }
        log.Info("received assignment", "partitions", ids)
        return updateConsumerFilters(ctx, ids)
    },

    OnDegraded: func(ctx context.Context, reason string) error {
        alerting.SendWarning("Worker entered degraded mode: %s", reason)
        return nil
    },

    OnDegradedAlert: func(ctx context.Context, level parti.AlertLevel, duration time.Duration) error {
        switch level {
        case parti.AlertLevelInfo:
            log.Info("degraded mode", "duration", duration)
        case parti.AlertLevelWarn:
            alerting.SendWarning("Degraded for %v", duration)
        case parti.AlertLevelError:
            alerting.SendError("Degraded for %v - action required", duration)
        case parti.AlertLevelCritical:
            alerting.PageOnCall("CRITICAL: Degraded for %v", duration)
        }
        return nil
    },

    OnError: func(ctx context.Context, err error) error {
        log.Error("parti error", "error", err)
        metrics.IncrementErrorCount()
        return nil  // Don't propagate - already handled
    },
}

mgr, err := parti.NewManager(cfg, parti.WithHooks(hooks))
```

### Hook Error Handling

- Hook errors are logged but don't stop the manager
- Return `nil` to indicate successful handling
- Hooks should be idempotent (may be called multiple times)
- Use context for cancellation in long-running hooks

---

## Error Handling

### Sentinel Errors

```go
import "github.com/arloliu/parti"

// Manager errors
parti.ErrNotStarted         // Manager not started
parti.ErrAlreadyStarted     // Manager already started
parti.ErrShutdown           // Manager is shutting down

// Stable ID errors
parti.ErrNoAvailableID      // All worker IDs in pool are claimed
parti.ErrNotClaimed         // Worker has not claimed an ID
parti.ErrAlreadyClosed      // Claimer already closed

// Election errors
parti.ErrNoLeader           // No leader elected
parti.ErrNotLeader          // Worker is not the leader

// Assignment errors
parti.ErrNoAssignment       // No partition assignment yet
parti.ErrPartitionNotOwned  // Partition not assigned to this worker
```

### Error Checking

```go
import "errors"

if err := mgr.Start(ctx); err != nil {
    if errors.Is(err, parti.ErrAlreadyStarted) {
        // Already running, continue
        return nil
    }
    return fmt.Errorf("start manager: %w", err)
}

assignment, err := mgr.GetAssignment()
if err != nil {
    if errors.Is(err, parti.ErrNoAssignment) {
        // Wait for assignment
        time.Sleep(100 * time.Millisecond)
        continue
    }
    return err
}
```

### Error Categories

| Category      | Errors                              | Action                     |
|---------------|-------------------------------------|----------------------------|
| Startup       | `ErrNoAvailableID`, `ErrNoLeader`   | Check config, retry later  |
| Runtime       | `ErrNoAssignment`, `ErrNotLeader`   | Wait, normal operation     |
| Shutdown      | `ErrShutdown`, `ErrAlreadyClosed`   | Expected during shutdown   |
| Programming   | `ErrNotStarted`, `ErrPartitionNotOwned` | Fix code logic        |

---

## Best Practices

### Configuration

**Production Configuration:**

```go
cfg := &parti.Config{
    ClusterName:     "production",
    WorkerIDPrefix:  "worker",
    WorkerIDMin:     0,
    WorkerIDMax:     99,               // Allow 100 workers
    WorkerIDTTL:     30 * time.Second,

    HeartbeatInterval:     5 * time.Second,
    HeartbeatMissThreshold: 3,

    ColdStartWindow:   30 * time.Second,
    ScalingWindow:     10 * time.Second,

    EnableTwoPhaseHandoff: true,
}
```

**Development Configuration:**

```go
cfg := &parti.Config{
    ClusterName:     "dev",
    WorkerIDPrefix:  "dev-worker",
    WorkerIDMin:     0,
    WorkerIDMax:     9,
    WorkerIDTTL:     10 * time.Second,

    HeartbeatInterval:     1 * time.Second,
    HeartbeatMissThreshold: 2,

    ColdStartWindow:   5 * time.Second,
    ScalingWindow:     2 * time.Second,

    EnableTwoPhaseHandoff: false,
}
```

### Graceful Shutdown

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

// Start manager
if err := mgr.Start(ctx); err != nil {
    log.Fatal(err)
}

// Handle shutdown signals
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

<-sigCh
log.Info("shutdown signal received")

// Graceful shutdown with timeout
shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
defer shutdownCancel()

if err := mgr.Stop(shutdownCtx); err != nil {
    log.Error("shutdown error", "error", err)
}
```

### Partition Count

| Workers | Recommended Partitions | Reason                        |
|---------|------------------------|-------------------------------|
| 1-5     | 16-32                  | Room to grow                  |
| 5-20    | 32-64                  | Good balance                  |
| 20-100  | 64-256                 | Fine-grained distribution     |
| 100+    | 256-1024               | Consider sharding by cluster  |

**Formula:** `partitions = 4-8 × max_expected_workers`

### Monitoring

Key metrics to track:

```go
// State transitions
hooks.OnStateChanged = func(ctx context.Context, from, to State) error {
    stateTransitions.WithLabelValues(from.String(), to.String()).Inc()
    return nil
}

// Partition count per worker
hooks.OnAssignment = func(ctx context.Context, partitions []Partition) error {
    assignedPartitions.Set(float64(len(partitions)))
    return nil
}

// Leadership changes
hooks.OnBecameLeader = func(ctx context.Context) error {
    leadershipGauge.Set(1)
    return nil
}
hooks.OnLostLeadership = func(ctx context.Context) error {
    leadershipGauge.Set(0)
    return nil
}

// Degraded mode duration
hooks.OnDegraded = func(ctx context.Context, reason string) error {
    degradedModeGauge.Set(1)
    return nil
}
hooks.OnRecovered = func(ctx context.Context) error {
    degradedModeGauge.Set(0)
    return nil
}
```

### Testing

```go
func TestPartitionProcessing(t *testing.T) {
    // Use test NATS server
    ns := natstest.RunServer()
    defer ns.Shutdown()

    nc, _ := nats.Connect(ns.ClientURL())
    js, _ := nc.JetStream()

    // Create test config with fast timeouts
    cfg := &parti.Config{
        ClusterName:       "test",
        WorkerIDPrefix:    "test-worker",
        WorkerIDMax:       9,
        WorkerIDTTL:       2 * time.Second,
        HeartbeatInterval: 500 * time.Millisecond,
        ColdStartWindow:   1 * time.Second,
        ScalingWindow:     500 * time.Millisecond,
    }

    mgr, err := parti.NewManager(cfg,
        parti.WithJetStream(js),
    )
    require.NoError(t, err)

    ctx := t.Context()
    require.NoError(t, mgr.Start(ctx))
    defer mgr.Stop(ctx)

    // Wait for stable state
    require.Eventually(t, func() bool {
        return mgr.GetState() == parti.StateStable
    }, 5*time.Second, 100*time.Millisecond)

    // Test partition assignment
    assignment, err := mgr.GetAssignment()
    require.NoError(t, err)
    require.NotEmpty(t, assignment)
}
```

---

## Glossary

| Term                    | Definition                                                                                          |
|-------------------------|-----------------------------------------------------------------------------------------------------|
| **Assignment**          | Mapping of partitions to workers; calculated by the leader                                          |
| **Assignment Strategy** | Algorithm for distributing partitions across workers (ConsistentHash, RoundRobin, etc.)             |
| **Broadcast Consumer**  | JetStream consumer receiving all partition messages, regardless of assignment                        |
| **Claimer**             | Internal component managing stable worker ID claiming and renewal                                   |
| **Cold Start Window**   | Stabilization delay after fresh cluster start (default: 30s)                                        |
| **Commit Phase**        | Second phase of two-phase handoff; new owner starts processing                                      |
| **Consistent Hashing**  | Hash-based assignment providing stable mappings during worker changes                               |
| **Consumer Updater**    | Interface for updating JetStream consumer filters on assignment changes                              |
| **Degraded Mode**       | Operation mode when NATS is unreachable; uses cached assignments                                    |
| **Election**            | Process of choosing a single leader among workers via NATS KV                                       |
| **Handoff**             | Process of transferring partition ownership between workers                                          |
| **Heartbeat**           | Periodic signal indicating worker liveness                                                           |
| **Leader**              | Single worker responsible for calculating and publishing assignments                                 |
| **Partition**           | Logical division of work; has ID, optional weight, and metadata                                     |
| **Partition Source**    | Provider of partition definitions (Static, NatsKV, custom)                                          |
| **Partitioner**         | Application-level component mapping keys to partition IDs                                            |
| **Prepare Phase**       | First phase of two-phase handoff; old owner stops processing                                        |
| **Processing Gate**     | Component preventing message processing during handoff                                               |
| **Rebalancing**         | Redistributing partitions after worker count changes                                                 |
| **Recovery Grace**      | Period after degraded mode exit before emergency rebalancing is triggered                            |
| **Scaling Window**      | Stabilization delay after worker joins/leaves established cluster (default: 10s)                   |
| **Stable ID**           | Persistent worker identifier claimed from a pool; survives restarts within TTL                      |
| **State**               | Current phase in worker lifecycle (Init, Stable, Scaling, Degraded, etc.)                           |
| **Two-Phase Handoff**   | Prepare/Commit protocol ensuring zero overlap in partition processing                                |
| **Virtual Nodes**       | Multiple hash ring positions per worker for better distribution                                      |
| **Weight**              | Partition attribute influencing assignment distribution                                              |
| **Worker**              | Instance running Parti Manager; processes assigned partitions                                        |
| **Worker Consumer**     | JetStream consumer receiving only assigned partition messages                                        |

---

## Quick Reference

### Manager Lifecycle

```go
mgr.Start(ctx)           // Start manager
mgr.Stop(ctx)            // Graceful shutdown
mgr.GetState()           // Current state
mgr.GetAssignment()      // Current partition assignment
mgr.OwnsPartition(id)    // Check partition ownership
mgr.IsLeader()           // Check if this worker is leader
mgr.GetWorkerID()        // Get stable worker ID
```

### State Checks

```go
switch mgr.GetState() {
case parti.StateStable:      // Normal operation
case parti.StateScaling:     // Scaling in progress
case parti.StateRebalancing: // Rebalancing in progress
case parti.StateDegraded:    // NATS connectivity issues
case parti.StateShutdown:    // Shutting down
}
```

### Common Patterns

```go
// Wait for stable
for mgr.GetState() != parti.StateStable {
    time.Sleep(100 * time.Millisecond)
}

// Check ownership before processing
if !mgr.OwnsPartition(partitionID) {
    return ErrNotOwner
}

// Leader-only operations
if mgr.IsLeader() {
    performLeaderTask()
}
```

See individual guides for detailed documentation on each topic.
