# Parti Reference

> Hooks, error handling, best practices, and glossary.

**Related Documentation:**
- [Docs README](README.md) - Documentation map
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
    // OnAssignmentChanged is called when this worker's complete assignment changes.
    OnAssignmentChanged func(ctx context.Context, oldPartitions, newPartitions []Partition) error

    // OnStateChanged is called on all worker state transitions.
    OnStateChanged      func(ctx context.Context, from, to State) error

    // OnError is called when a recoverable error occurs.
    OnError             func(ctx context.Context, err error) error

    // OnLeadershipChanged is called when the worker acquires or loses leadership.
    OnLeadershipChanged func(ctx context.Context, isLeader bool) error

    // Convenience hooks derived from OnAssignmentChanged.
    OnPartitionsAssigned func(ctx context.Context, partitions []Partition) error
    OnPartitionsRevoked  func(ctx context.Context, partitions []Partition) error

    // OnDegraded is called once when the manager enters degraded mode.
    OnDegraded          func(ctx context.Context, reason string) error
}
```

### Hook Behavior

| Hook                 | When Called                                    | Use Case                    |
|----------------------|------------------------------------------------|-----------------------------|
| `OnStateChanged`     | Every state transition                         | Metrics, logging            |
| `OnLeadershipChanged`| Leadership acquired/lost                       | Leader-only initialization  |
| `OnAssignmentChanged`| This worker's complete assignment changes      | Update consumers, caches    |
| `OnPartitionsAssigned` | Partitions are added to this worker          | Initialize per-partition resources |
| `OnPartitionsRevoked`  | Partitions are removed from this worker      | Cleanup per-partition resources    |
| `OnDegraded`         | Entering degraded mode                         | Alerting / escalation              |
| `OnError`            | Recoverable errors                             | Error tracking              |

### Hook Implementation

```go
hooks := &parti.Hooks{
    OnStateChanged: func(ctx context.Context, from, to parti.State) error {
        metrics.RecordStateChange(from.String(), to.String())
        log.Info("state transition",
            "from", from,
            "to", to)

        if to == parti.StateDegraded {
            alerting.SendWarning("worker entered degraded mode")
        }
        return nil
    },

    OnLeadershipChanged: func(ctx context.Context, isLeader bool) error {
        if isLeader {
            log.Info("became leader, initializing leader resources")
            return initLeaderResources(ctx)
        }
        log.Info("lost leadership, cleaning up leader resources")
        return cleanupLeaderResources(ctx)
    },

    OnAssignmentChanged: func(ctx context.Context, _oldPartitions, newPartitions []parti.Partition) error {
        ids := make([]string, len(newPartitions))
        for i, p := range newPartitions {
            ids[i] = p.ID()
        }
        log.Info("received assignment", "partitions", ids)
        return updateConsumerFilters(ctx, ids)
    },

    OnError: func(ctx context.Context, err error) error {
        log.Error("parti error", "error", err)
        metrics.IncrementErrorCount()
        return nil  // Don't propagate - already handled
    },
}

// Create manager with hooks option
mgr, err := parti.NewManager(cfg, js, src, strategy.NewConsistentHash(),
    parti.WithHooks(hooks),
)
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
import "github.com/arloliu/parti/v2"

// Configuration / construction
parti.ErrInvalidConfig
parti.ErrNATSConnectionRequired
parti.ErrPartitionSourceRequired
parti.ErrAssignmentStrategyRequired

// Lifecycle
parti.ErrAlreadyStarted
parti.ErrNotStarted

// Runtime signals
parti.ErrConnectivity
parti.ErrDegraded
parti.ErrElectionFailed
parti.ErrNoWorkersAvailable
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

// Prefer hooks (OnAssignmentChanged / OnPartitionsAssigned) for reacting to assignment.
assignment := mgr.CurrentAssignment()
_ = assignment
```

### Error Categories

| Category      | Errors                              | Action                     |
|---------------|-------------------------------------|----------------------------|
| Startup       | `ErrInvalidConfig`, `ErrNATSConnectionRequired` | Fix config / wiring |
| Runtime       | `ErrNotStarted`                     | Start manager first        |
| Connectivity  | `ErrConnectivity`, `ErrDegraded`    | Investigate NATS/KV health |

---

## Best Practices

### Configuration

**Production Configuration:**

```go
cfg := &parti.Config{
    WorkerIDPrefix:  "worker",
    WorkerIDMin:     0,
    WorkerIDMax:     99,               // Allow 100 workers
    WorkerIDTTL:     30 * time.Second,

    HeartbeatInterval: 5 * time.Second,
    HeartbeatTTL:      15 * time.Second,  // 3× interval

    ColdStartWindow:    30 * time.Second,
    PlannedScaleWindow: 10 * time.Second,

    EnableTwoPhaseHandoff: true,
}
```

**Development Configuration:**

```go
cfg := &parti.Config{
    WorkerIDPrefix:  "dev-worker",
    WorkerIDMin:     0,
    WorkerIDMax:     9,
    WorkerIDTTL:     10 * time.Second,

    HeartbeatInterval: 1 * time.Second,
    HeartbeatTTL:      3 * time.Second,   // 3× interval

    ColdStartWindow:    5 * time.Second,
    PlannedScaleWindow: 2 * time.Second,

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
hooks := &parti.Hooks{
    OnStateChanged: func(ctx context.Context, from, to parti.State) error {
        stateTransitions.WithLabelValues(from.String(), to.String()).Inc()

        if to == parti.StateDegraded {
            degradedModeGauge.Set(1)
        }
        if from == parti.StateDegraded && to != parti.StateDegraded {
            degradedModeGauge.Set(0)
        }
        return nil
    },

    OnAssignmentChanged: func(ctx context.Context, _old, newPartitions []parti.Partition) error {
        assignedPartitions.Set(float64(len(newPartitions)))
        return nil
    },

    OnLeadershipChanged: func(ctx context.Context, isLeader bool) error {
        if isLeader {
            leadershipGauge.Set(1)
        } else {
            leadershipGauge.Set(0)
        }
        return nil
    },
}
```

### Testing

```go
func TestPartitionProcessing(t *testing.T) {
    // Use an embedded NATS server
    srv, nc := partitesting.StartEmbeddedNATS(t)
    defer srv.Shutdown()
    defer nc.Close()

    js, _ := jetstream.New(nc)

    // Create test config with fast timeouts
    cfg := &parti.Config{
        WorkerIDPrefix:     "test-worker",
        WorkerIDMax:        9,
        WorkerIDTTL:        2 * time.Second,
        HeartbeatInterval:  500 * time.Millisecond,
        HeartbeatTTL:       1500 * time.Millisecond,
        ColdStartWindow:    1 * time.Second,
        PlannedScaleWindow: 500 * time.Millisecond,
    }

    // Define test partitions
    partitions := []parti.Partition{
        {Keys: []string{"0"}},
        {Keys: []string{"1"}},
        {Keys: []string{"2"}},
    }
    src := source.NewStatic(partitions)

    // Create manager with positional args
    mgr, err := parti.NewManager(cfg, js, src, strategy.NewConsistentHash())
    require.NoError(t, err)

    ctx := t.Context()
    require.NoError(t, mgr.Start(ctx))
    defer mgr.Stop(ctx)

    // Wait for stable state
    require.Eventually(t, func() bool {
        return mgr.State() == parti.StateStable
    }, 5*time.Second, 100*time.Millisecond)

    // Test partition assignment
    assignment := mgr.CurrentAssignment()
    require.NotEmpty(t, assignment.Partitions)
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
| **Rebalancing**         | Redistributing partitions after worker or partition-source changes                                   |
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
mgr.State()              // Current state
mgr.CurrentAssignment()  // Current partition assignment
mgr.RefreshPartitions(ctx) // Leader-only: refresh partitions + trigger rebalance
mgr.IsLeader()           // Check if this worker is leader
mgr.WorkerID()           // Get stable worker ID
```

### State Checks

```go
switch mgr.State() {
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
if err := <-mgr.WaitState(parti.StateStable, 30*time.Second); err != nil {
    log.Fatalf("manager did not reach StateStable: %v", err)
}

// Check ownership before processing
owns := false
for _, p := range mgr.CurrentAssignment().Partitions {
    if p.ID() == partitionID {
        owns = true
        break
    }
}
if !owns {
    return ErrNotOwner
}

// Leader-only operations
if mgr.IsLeader() {
    performLeaderTask()
}
```

See individual guides for detailed documentation on each topic.
