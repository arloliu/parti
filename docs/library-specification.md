# Parti Library Specification v1.0

**Document Version**: 1.0.0
**Date**: October 26, 2025
**Status**: Draft
**Library**: `github.com/arloliu/parti`

---

## Table of Contents

1. [Overview](#overview)
2. [Architecture](#architecture)
3. [Core Interfaces](#core-interfaces)
4. [Configuration](#configuration)
5. [State Machine](#state-machine)
6. [Assignment Strategy](#assignment-strategy)
7. [Leader Election](#leader-election)
8. [Partition Discovery](#partition-discovery)
9. [Subscription Management](#subscription-management)
10. [Error Handling](#error-handling)
11. [Observability](#observability)
12. [Thread Safety](#thread-safety)
13. [Performance Guarantees](#performance-guarantees)

---

## Overview

### Purpose

Parti is a Go library for NATS-based work partitioning that provides:
- Dynamic partition assignment across worker instances
- Leader-based coordination without external services
- Stable worker IDs for consistent assignment during rolling updates
- Cache-affinity-aware rebalancing strategies

### Key Features

- **Stable Worker IDs**: Workers claim stable IDs (e.g., "worker-0", "worker-1") for consistent assignment
- **Leader-Based Assignment**: One worker becomes leader and calculates partition assignments
- **Adaptive Rebalancing**: Different stabilization windows for cold start (30s) vs planned scale (10s)
- **Cache Affinity**: Preserves >80% partition locality during rebalancing
- **Weighted Assignment**: Supports partition weights for load balancing
- **Graceful Operations**: Proper shutdown, rolling updates, crash recovery

### Target Use Cases

1. **Distributed message consumers** replacing Kafka consumer groups
2. **Work queue processors** needing stable partition assignment
3. **Microservices** requiring coordinated work distribution

---

## Architecture

### High-Level Components

```
┌─────────────────────────────────────────────────────────────┐
│                         Manager                             │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐       │
│  │   Stable ID  │  │   Election   │  │  Assignment  │       │
│  │   Claimer    │  │   Controller │  │  Controller  │       │
│  └──────────────┘  └──────────────┘  └──────────────┘       │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐       │
│  │  Heartbeat   │  │  Partition   │  │ Subscription │       │
│  │  Publisher   │  │  Discovery   │  │   Helper     │       │
│  └──────────────┘  └──────────────┘  └──────────────┘       │
└─────────────────────────────────────────────────────────────┘
                            │
                    ┌───────┴───────┐
                    │               │
            ┌───────▼─────┐   ┌────▼─────┐
            │  NATS KV    │   │ Election │
            │  Storage    │   │  Agent   │
            └─────────────┘   └──────────┘
```

### State Machine

Workers progress through states:
```
INIT → CLAIMING_ID → ELECTION → WAITING_ASSIGNMENT → STABLE
                                           ↓
                     SCALING ← ─ ─ ─ ─ ─ ─ ┘
                        ↓
                   REBALANCING
                        ↓
                     STABLE
```

Emergency path (crash detected):
```
STABLE → EMERGENCY → STABLE
```

Shutdown path:
```
STABLE → SHUTDOWN → [TERMINATED]
```

---

## Core Interfaces

### Manager Interface

```go
// Manager coordinates workers in a distributed system
type Manager interface {
    // Start initializes and runs the manager
    // Blocks until worker ID claimed and initial assignment received
    // Returns error if startup fails or context is cancelled
    Start(ctx context.Context) error

    // Stop gracefully shuts down the manager
    // Releases stable ID, stops heartbeat, unsubscribes
    Stop(ctx context.Context, cfg ShutdownConfig) error

    // WorkerID returns the claimed stable worker ID
    // Returns empty string if not yet claimed
    WorkerID() string

    // IsLeader returns true if this worker is the current leader
    IsLeader() bool

    // CurrentAssignment returns the current partition assignment
    // Includes version, lifecycle state, and assigned partitions
    CurrentAssignment() Assignment

    // State returns the current worker state
    State() State

    // RefreshPartitions triggers partition discovery refresh
    // Used when application knows partitions have changed
    RefreshPartitions(ctx context.Context) error
}
```

### PartitionSource Interface

```go
// PartitionSource discovers available partitions
type PartitionSource interface {
    // ListPartitions returns all available partitions
    // Called during initialization and when RefreshPartitions is triggered
    ListPartitions(ctx context.Context) ([]Partition, error)
}
```

### AssignmentStrategy Interface

```go
// AssignmentStrategy calculates partition assignments for workers
type AssignmentStrategy interface {
    // Assign calculates assignments given workers and partitions
    // workers: list of worker IDs (e.g., ["worker-0", "worker-1"])
    // partitions: list of partitions to assign
    // Returns: map[workerID][]Partition
    Assign(workers []string, partitions []Partition) (map[string][]Partition, error)
}
```

### ElectionAgent Interface

```go
// ElectionAgent handles leader election (optional)
// Default implementation uses NATS KV-based election
// Production deployments should use external election-agent
type ElectionAgent interface {
    // RequestLeadership requests leadership with a lease duration
    RequestLeadership(ctx context.Context, workerID string, leaseDuration time.Duration) (bool, error)

    // RenewLeadership renews the current leadership lease
    RenewLeadership(ctx context.Context) error

    // ReleaseLeadership releases the leadership
    ReleaseLeadership(ctx context.Context) error

    // IsLeader checks if this worker is the current leader
    IsLeader(ctx context.Context) bool
}
```

---

## Configuration

### Serializable Config (JSON/YAML)

```go
type Config struct {
    // Worker identity
    WorkerIDPrefix string `json:"worker_id_prefix" yaml:"worker_id_prefix"`
    WorkerIDMin    int    `json:"worker_id_min" yaml:"worker_id_min"`
    WorkerIDMax    int    `json:"worker_id_max" yaml:"worker_id_max"`
    WorkerIDTTL    string `json:"worker_id_ttl" yaml:"worker_id_ttl"`

    // Timing
    HeartbeatInterval string `json:"heartbeat_interval" yaml:"heartbeat_interval"`
    HeartbeatTTL      string `json:"heartbeat_ttl" yaml:"heartbeat_ttl"`

    // State machine timing
    ColdStartWindow       string  `json:"cold_start_window" yaml:"cold_start_window"`
    PlannedScaleWindow    string  `json:"planned_scale_window" yaml:"planned_scale_window"`
    RestartDetectionRatio float64 `json:"restart_detection_ratio" yaml:"restart_detection_ratio"`

    // Context timeout control
    OperationTimeout string `json:"operation_timeout" yaml:"operation_timeout"`
    ElectionTimeout  string `json:"election_timeout" yaml:"election_timeout"`
    StartupTimeout   string `json:"startup_timeout" yaml:"startup_timeout"`
    ShutdownTimeout  string `json:"shutdown_timeout" yaml:"shutdown_timeout"`

    // Assignment rebalancing policy
    Assignment AssignmentConfig `json:"assignment" yaml:"assignment"`
}

type AssignmentConfig struct {
    MinRebalanceThreshold float64 `json:"min_rebalance_threshold" yaml:"min_rebalance_threshold"`
    RebalanceCooldown     string  `json:"rebalance_cooldown" yaml:"rebalance_cooldown"`
}
```

### Default Values

| Field | Default | Description |
|-------|---------|-------------|
| `worker_id_prefix` | `"worker"` | Prefix for worker IDs |
| `worker_id_min` | `0` | Minimum worker ID number |
| `worker_id_max` | `99` | Maximum worker ID number |
| `worker_id_ttl` | `"30s"` | TTL for worker ID claims |
| `heartbeat_interval` | `"2s"` | How often to publish heartbeat |
| `heartbeat_ttl` | `"6s"` | Heartbeat validity duration |
| `cold_start_window` | `"30s"` | Stabilization for cold start |
| `planned_scale_window` | `"10s"` | Stabilization for planned scale |
| `restart_detection_ratio` | `0.5` | Threshold for restart detection |
| `operation_timeout` | `"10s"` | Default timeout for Manager operations (NATS KV, partition discovery) |
| `election_timeout` | `"5s"` | Timeout for ElectionAgent operations (RequestLeadership, RenewLeadership, ReleaseLeadership) |
| `startup_timeout` | `"30s"` | Maximum time for Manager.Start() to complete (ID claim + election + initial assignment) |
| `shutdown_timeout` | `"10s"` | Maximum time for Manager.Stop() to complete (ID release + cleanup) |
| `min_rebalance_threshold` | `0.15` | Minimum change % to rebalance |
| `rebalance_cooldown` | `"10s"` | Cooldown between rebalances |

### Runtime Dependencies (Functional Options)

```go
// Required parameters (explicit in function signature)
func NewManager(
    cfg *Config,
    natsConn *nats.Conn,
    partitionSource PartitionSource,
    opts ...Option,
) (*Manager, error)

// Optional parameters (with defaults)
func WithStrategy(strategy AssignmentStrategy) Option
    // Default: WeightedConsistentHashStrategy(150)

func WithElectionAgent(agent ElectionAgent) Option
    // Default: nil (uses NATS KV-based election)

func WithHooks(hooks *Hooks) Option
    // Default: nil (no callbacks)

func WithMetrics(metrics MetricsCollector) Option
    // Default: nopMetricsCollector

func WithLogger(logger Logger) Option
    // Default: nopLogger
```

### Context Timeout Usage

The timeout fields in `Config` control how the Manager creates context deadlines for various operations:

| Timeout Field | Used For | Behavior |
|--------------|----------|----------|
| `operation_timeout` | NATS KV operations (read/write), `PartitionSource.ListPartitions()`, internal state updates | Creates child context with timeout for each operation |
| `election_timeout` | All `ElectionAgent` method calls (`RequestLeadership`, `RenewLeadership`, `ReleaseLeadership`, `IsLeader`) | Creates child context with timeout for each election operation |
| `startup_timeout` | `Manager.Start()` overall timeout | Wraps parent context; fails if ID claim + election + initial assignment exceeds timeout |
| `shutdown_timeout` | `Manager.Stop()` overall timeout | Wraps parent context; fails if ID release + heartbeat stop + cleanup exceeds timeout |

**Implementation Pattern**:
```go
// Example: Manager uses operation_timeout for NATS KV operations
func (m *Manager) claimStableID(parentCtx context.Context) error {
    ctx, cancel := context.WithTimeout(parentCtx, m.cfg.OperationTimeout)
    defer cancel()

    // NATS KV operations with timeout context
    return m.kvStore.Put(ctx, key, value)
}

// Example: Manager uses election_timeout for ElectionAgent calls
func (m *Manager) requestLeadership(parentCtx context.Context) (bool, error) {
    if m.electionAgent == nil {
        return m.natsElection(parentCtx) // fallback uses operation_timeout
    }

    ctx, cancel := context.WithTimeout(parentCtx, m.cfg.ElectionTimeout)
    defer cancel()

    return m.electionAgent.RequestLeadership(ctx, m.workerID, m.cfg.HeartbeatTTL)
}

// Example: Caller provides parent context, Manager enforces startup_timeout
func (m *Manager) Start(ctx context.Context) error {
    // Create timeout context based on config
    startupCtx, cancel := context.WithTimeout(ctx, m.cfg.StartupTimeout)
    defer cancel()

    // All startup operations use startupCtx or its children
    if err := m.claimStableID(startupCtx); err != nil {
        return err
    }
    // ... rest of startup
}
```

**Notes**:
- Parent context cancellation always propagates to child contexts
- If parent context has shorter deadline than config timeout, parent wins
- Application can override by providing context with its own deadline

---

## State Machine

### States

| State | Description | Entry Conditions | Exit Conditions |
|-------|-------------|------------------|-----------------|
| `INIT` | Initial state | Worker starts | Start() called |
| `CLAIMING_ID` | Attempting to claim stable ID | From INIT | ID claimed or error |
| `ELECTION` | Participating in leader election | ID claimed | Leader determined |
| `WAITING_ASSIGNMENT` | Waiting for assignment map | Election complete | Assignment received |
| `STABLE` | Normal operation | Assignment received | Topology change or shutdown |
| `SCALING` | Stabilization window | Topology change detected | Window expires |
| `REBALANCING` | Leader calculating new assignment | Stabilization complete | Assignment published |
| `EMERGENCY` | Emergency rebalancing (crash) | Crash detected | Assignment published |
| `SHUTDOWN` | Graceful shutdown | Stop() called | Cleanup complete |

### State Transitions

```
Normal startup:
  INIT → CLAIMING_ID → ELECTION → WAITING_ASSIGNMENT → STABLE

Planned scale (gradual):
  STABLE → SCALING(10s) → REBALANCING → STABLE

Cold start (0→many workers):
  STABLE → SCALING(30s) → REBALANCING → STABLE

Restart detected (many→0→many):
  STABLE → SCALING(30s) → REBALANCING → STABLE

Worker crash:
  STABLE → EMERGENCY(immediate) → STABLE

Graceful shutdown:
  STABLE → SHUTDOWN → [TERMINATED]
```

### Lifecycle States

Cluster lifecycle state tracked in assignment metadata:

| Lifecycle State | Description | Window |
|----------------|-------------|--------|
| `cold_start` | Initial deployment or restart detected | 30s |
| `post_cold_start` | First successful assignment after cold start | - |
| `stable` | Normal operation | 10s for planned scale |

---

## Assignment Strategy

### Built-in Strategies

#### 1. WeightedConsistentHashStrategy (Recommended)

**Parameters:**
- `virtualNodes` (int): Number of virtual nodes per worker (default: 150)

**Algorithm:**
1. Build hash ring with virtual nodes for each worker
2. Place each partition on ring based on hash of partition keys
3. Assign partition to nearest clockwise virtual node
4. Apply weight balancing if partition weights differ
5. Track affinity score compared to previous assignment

**Characteristics:**
- Minimizes partition movement during scaling (~15-20% movement)
- Preserves >80% cache affinity
- Even distribution with virtual nodes
- Zero hash collisions (64-bit xxHash)

#### 2. RoundRobinStrategy

**Parameters:** None

**Algorithm:**
1. Sort workers and partitions
2. Distribute partitions evenly in round-robin fashion

**Characteristics:**
- Simple and predictable
- Even distribution
- High partition movement during scaling (~50%)
- No cache affinity

### Custom Strategy Implementation

Applications can implement `AssignmentStrategy` interface:

```go
type MyStrategy struct {
    // custom fields
}

func (s *MyStrategy) Assign(workers []string, partitions []Partition) (map[string][]Partition, error) {
    assignments := make(map[string][]Partition)
    // Your assignment logic here
    return assignments, nil
}
```

---

## Leader Election

### Election Flow

1. **All workers** participate in election after claiming stable ID
2. **Election Agent** (or NATS KV fallback) determines single leader
3. **Leader** reads assignment map and active heartbeats
4. **Followers** wait for assignment updates

### Leadership Responsibilities

Leader performs:
- Monitor worker heartbeats (every 2s)
- Detect topology changes (joins, leaves, crashes)
- Calculate new assignments when needed
- Publish assignment map to NATS KV
- Track assignment version and lifecycle state

### Failover

When leader crashes:
1. Other workers detect missed heartbeats
2. New election triggered automatically
3. New leader reads last assignment version
4. Continues from previous state (no reset)

### Split-Brain Prevention

- **With Election Agent**: Distributed consensus prevents split-brain
- **With NATS KV**: Last-writer-wins + TTL-based leases

---

## Partition Discovery

### Discovery Flow

1. **Initialization**: Call `ListPartitions()` during startup
2. **Refresh**: Application calls `RefreshPartitions()` when partitions change
3. **Leader Action**: Leader recalculates assignments after partition change

### Partition Structure

```go
type Partition struct {
    Keys   []string  // Hierarchical keys (e.g., ["tool001", "chamber1"])
    Weight int64     // Optional weight for load balancing
}

// Subject generates NATS subject from partition keys
func (p Partition) Subject(prefix string) string {
    // Returns: "prefix.tool001.chamber1.completed"
}
```

### Built-in Sources

**StaticSource**: Fixed list of partitions
```go
source := parti.StaticSource([]parti.Partition{
    {Keys: []string{"tool001", "chamber1"}, Weight: 100},
    {Keys: []string{"tool001", "chamber2"}, Weight: 150},
})
```

**Custom Source**: Implement `PartitionSource` interface
```go
type CassandraPartitionSource struct {
    session *gocql.Session
}

func (s *CassandraPartitionSource) ListPartitions(ctx context.Context) ([]parti.Partition, error) {
    // Query database for partitions
    return partitions, nil
}
```

---

## Subscription Management

### Subscription Helper (Recommended)

Parti provides a subscription helper with automatic reconciliation:

```go
// Create subscription helper
subHelper := parti.NewSubscriptionHelper(natsConn, parti.SubscriptionConfig{
    Stream:          "work-stream",
    MaxRetries:      3,
    RetryBackoff:    time.Second,
    ReconcileInterval: 30 * time.Second,
})

// Use in hooks
hooks := &parti.Hooks{
    OnAssignmentChanged: func(ctx context.Context, added, removed []parti.Partition) error {
        return subHelper.UpdateSubscriptions(ctx, added, removed, messageHandler)
    },
}
```

**Features:**
- Automatic retry on subscription failures
- Background reconciliation loop
- Metrics for subscription health
- Thread-safe operation

### Manual Subscription (Advanced)

Applications can manage subscriptions directly:

```go
hooks := &parti.Hooks{
    OnAssignmentChanged: func(ctx context.Context, added, removed []parti.Partition) error {
        for _, p := range removed {
            unsubscribe(p)
        }
        for _, p := range added {
            if err := subscribe(p); err != nil {
                return err
            }
        }
        return nil
    },
}
```

---

## Error Handling

### Error Types

```go
// ErrStableIDExhausted when all IDs in range are claimed
var ErrStableIDExhausted = errors.New("all stable IDs in range are claimed")

// ErrNATSConnectionLost when NATS connection is lost
var ErrNATSConnectionLost = errors.New("NATS connection lost")

// ErrAssignmentVersionMismatch when assignment version is stale
var ErrAssignmentVersionMismatch = errors.New("assignment version mismatch")

// ErrElectionFailed when leader election fails
var ErrElectionFailed = errors.New("leader election failed")
```

### Retry Policy

Configurable retry behavior:

```go
type RetryConfig struct {
    MaxRetries   int           // Default: 3
    InitialDelay time.Duration // Default: 1s
    MaxDelay     time.Duration // Default: 30s
    Backoff      float64       // Default: 2.0 (exponential)
}
```

Applied to:
- Stable ID claiming
- Assignment map updates
- Partition discovery
- Subscription operations

### Error Callbacks

```go
type Hooks struct {
    // ... other hooks ...

    // OnError is called when a recoverable error occurs
    OnError func(ctx context.Context, err error) error
}
```

---

## Observability

### Metrics Interface

```go
type MetricsCollector interface {
    // State metrics
    RecordStateTransition(from, to State, duration time.Duration)
    RecordStateDuration(state State, duration time.Duration)

    // Assignment metrics
    RecordAssignmentChange(added, removed int, version int64)
    RecordAssignmentCalculationTime(duration time.Duration)
    RecordAffinityScore(score float64)

    // Health metrics
    RecordHeartbeat(workerID string, success bool)
    RecordLeadershipChange(newLeader string, duration time.Duration)
}
```

### Recommended Metrics

**Gauges:**
- `parti_worker_state` - Current worker state (0-8)
- `parti_is_leader` - Whether this worker is leader (0 or 1)
- `parti_assigned_partitions` - Number of assigned partitions
- `parti_assignment_version` - Current assignment version

**Counters:**
- `parti_state_transitions_total` - State transition count by from/to
- `parti_assignment_changes_total` - Assignment change count
- `parti_leadership_changes_total` - Leadership change count
- `parti_heartbeat_failures_total` - Failed heartbeat count

**Histograms:**
- `parti_state_duration_seconds` - Time spent in each state
- `parti_assignment_calculation_seconds` - Assignment calculation time
- `parti_affinity_score` - Cache affinity score (0.0-1.0)

### Logger Interface

```go
type Logger interface {
    Debug(msg string, keysAndValues ...any)
    Info(msg string, keysAndValues ...any)
    Warn(msg string, keysAndValues ...any)
    Error(msg string, keysAndValues ...any)
    Fatal(msg string, keysAndValues ...any)
}
```

Compatible with:
- `zap.SugaredLogger`
- `logrus` (with adapter)
- Custom implementations

---

## Thread Safety

### Guarantees

| Component | Thread Safety | Notes |
|-----------|--------------|-------|
| `Manager.Start()` | Not reentrant | Call once per manager |
| `Manager.Stop()` | Not reentrant | Call once per manager |
| `Manager.WorkerID()` | Thread-safe | Read-only after claim |
| `Manager.IsLeader()` | Thread-safe | May change over time |
| `Manager.CurrentAssignment()` | Thread-safe | Returns copy |
| `Manager.State()` | Thread-safe | Atomic read |
| `Manager.RefreshPartitions()` | Thread-safe | Can call concurrently |
| `Hooks callbacks` | Sequential | Never called concurrently |

### Concurrency Model

- **Internal goroutines**: Manager spawns goroutines for heartbeat, monitoring
- **Callback execution**: All callbacks execute in dedicated goroutine (never concurrent)
- **State updates**: Atomic state transitions
- **Assignment updates**: Copy-on-write pattern

---

## Performance Guarantees

### Startup Time

- Stable ID claim: < 1s (sequential search)
- Election: < 2s (with election agent)
- Initial assignment: < 5s (cold start, 64 workers, 5000 partitions)

### Assignment Calculation

- Time complexity: O(W × V + P × log(W × V))
  - W = workers, V = virtual nodes, P = partitions
- Typical: < 100ms (64 workers, 150 vnodes, 5000 partitions)
- Memory: ~10 MB (64 workers, 150 vnodes, 5000 partitions)

### Rebalancing

- Emergency (crash): < 1s (immediate)
- Planned scale: 10s window + calculation time
- Cold start: 30s window + calculation time
- Partition movement: ~15-20% (with consistent hashing)

### Memory Footprint

- Base: ~5 MB per worker
- Per partition: ~200 bytes
- Per assignment entry: ~100 bytes
- Example (5000 partitions, 64 workers): ~8 MB

### Network Traffic

- Heartbeat: 100 bytes every 2s
- Assignment map: ~50 KB (5000 partitions, 64 workers)
- Stable ID: 50 bytes (claim + renewals)

---

## Version Compatibility

### Semantic Versioning

- `v1.x.x`: Stable API, backward compatible
- `v2.x.x`: Breaking changes (if needed)
- `v0.x.x`: Development versions (pre-release)

### NATS Compatibility

- Minimum: NATS Server 2.9.0+
- Recommended: NATS Server 2.10.0+
- JetStream required: Yes
- KV Store required: Yes

### Go Version

- Minimum: Go 1.24+
- Recommended: Latest stable Go version

---

## Appendix: Example Configurations

### Development (Single Node)

```yaml
worker_id_prefix: "dev-worker"
worker_id_min: 0
worker_id_max: 2
worker_id_ttl: "10s"
heartbeat_interval: "1s"
heartbeat_ttl: "3s"
cold_start_window: "5s"
planned_scale_window: "2s"
restart_detection_ratio: 0.5
operation_timeout: "5s"
election_timeout: "3s"
startup_timeout: "15s"
shutdown_timeout: "5s"
assignment:
  min_rebalance_threshold: 0.1
  rebalance_cooldown: "2s"
```

### Production (64 Workers)

```yaml
worker_id_prefix: "worker"
worker_id_min: 0
worker_id_max: 199
worker_id_ttl: "30s"
heartbeat_interval: "2s"
heartbeat_ttl: "6s"
cold_start_window: "30s"
planned_scale_window: "10s"
restart_detection_ratio: 0.5
operation_timeout: "10s"
election_timeout: "5s"
startup_timeout: "30s"
shutdown_timeout: "10s"
assignment:
  min_rebalance_threshold: 0.15
  rebalance_cooldown: "10s"
```

### High Churn (Frequent Scaling)

```yaml
worker_id_prefix: "dynamic-worker"
worker_id_min: 0
worker_id_max: 499
worker_id_ttl: "20s"
heartbeat_interval: "1s"
heartbeat_ttl: "4s"
cold_start_window: "20s"
planned_scale_window: "5s"
restart_detection_ratio: 0.3
operation_timeout: "8s"
election_timeout: "4s"
startup_timeout: "25s"
shutdown_timeout: "8s"
assignment:
  min_rebalance_threshold: 0.2
  rebalance_cooldown: "5s"
```
