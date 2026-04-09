# Parti API Reference

**Version**: 2.0.0
**Last Updated**: December 6, 2025
**Library**: `github.com/arloliu/parti/v2`

---

## Table of Contents

1. [Manager Interface](#manager-interface)
2. [Stable ID Renewal Lifecycle](#stable-id-renewal-lifecycle)
3. [Core Interfaces](#core-interfaces)
4. [Configuration Types](#configuration-types)
5. [Data Types](#data-types)
6. [Strategy Package](#strategy-package)
7. [Source Package](#source-package)
8. [Consumer Package](#consumer-package)
9. [Testing Package](#testing-package)
10. [Error Types](#error-types)

---

## Manager Interface

### NewManager

Creates a new Manager instance with the provided configuration.

```go
func NewManager(
    cfg *Config,
    js jetstream.JetStream,
    source PartitionSource,
    strategy AssignmentStrategy,
    opts ...Option,
) (*Manager, error)
```

**Parameters**:
- `cfg`: Runtime configuration with parsed durations
- `js`: JetStream context for KV and messaging coordination
- `source`: Partition source for discovering partitions
- `strategy`: Assignment strategy for distributing partitions
- `opts`: Optional configuration (hooks, metrics, logger, election agent)

**Returns**:
- `*Manager`: Initialized manager instance
- `error`: Validation error if configuration is invalid

**Example**:
```go
cfg := &parti.Config{
    WorkerIDPrefix: "worker",
    WorkerIDMax:    999,
}
if err := parti.SetDefaults(cfg); err != nil {
    log.Fatal(err)
}

src := source.NewStatic(partitions)
strategy := strategy.NewConsistentHash()
js, _ := jetstream.New(natsConn)
mgr, err := parti.NewManager(cfg, js, src, strategy)
if err != nil {
    log.Fatal(err)
}

// Note: After startup, a stable worker ID is claimed and renewed in the background.
// See the Stable ID Renewal Lifecycle section for details.
```

---

### Manager Methods

#### Start

Initializes and runs the manager.

```go
func (m *Manager) Start(ctx context.Context) error
```

Blocks until worker ID is claimed and initial assignment is received. Returns error if startup fails or context is cancelled.

**Parameters**:
- `ctx`: Context for cancellation and timeout

**Returns**:
- `error`: Startup error or nil on success

**Startup Sequence**:
1. Claims stable worker ID from NATS KV
2. Starts heartbeat publisher
3. Participates in leader election
4. Waits for initial partition assignment
5. Transitions to stable state

**Example**:
```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

if err := mgr.Start(ctx); err != nil {
    log.Fatalf("Failed to start: %v", err)
}
```

---

#### Stop

Gracefully shuts down the manager.

```go
func (m *Manager) Stop(ctx context.Context) error
```

Releases stable ID, stops heartbeat, and unsubscribes from NATS topics.

**Parameters**:
- `ctx`: Context for cancellation and timeout

**Returns**:
- `error`: Shutdown error or nil on success

**Shutdown Sequence**:
1. Transitions to shutdown state
2. Stops heartbeat publisher
3. Releases leader lock (if leader)
4. Releases stable worker ID
5. Cleans up internal resources

**Example**:
```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()

if err := mgr.Stop(ctx); err != nil {
    log.Printf("Error during shutdown: %v", err)
}
```

---

#### WorkerID

Returns the claimed stable worker ID.

```go
func (m *Manager) WorkerID() string
```

**Returns**:
- `string`: Worker ID (e.g., "worker-0") or empty string if not yet claimed

**Thread Safety**: Safe for concurrent use.

**Example**:
```go
workerID := mgr.WorkerID()
log.Printf("Running as: %s", workerID)
```

---

#### IsLeader

Returns true if this worker is the current leader.

```go
func (m *Manager) IsLeader() bool
```

**Returns**:
- `bool`: true if this worker is the leader, false otherwise

**Thread Safety**: Safe for concurrent use. Value may change over time due to elections.

**Example**:
```go
if mgr.IsLeader() {
    log.Println("This worker is the leader")
}
```

---

#### CurrentAssignment

Returns the current partition assignment.

```go
func (m *Manager) CurrentAssignment() Assignment
```

**Returns**:
- `Assignment`: Current assignment with version, lifecycle, and partitions

**Thread Safety**: Safe for concurrent use. Returns a copy of the assignment.

**Example**:
```go
assignment := mgr.CurrentAssignment()
log.Printf("Version: %d, Partitions: %d, Lifecycle: %s",
    assignment.Version,
    len(assignment.Partitions),
    assignment.Lifecycle,
)
```

---

#### State

Returns the current worker state.

```go
func (m *Manager) State() State
```

**Returns**:
- `State`: Current state (Init, ClaimingID, Election, WaitingAssignment, Stable, Scaling, Rebalancing, Emergency, Shutdown)

**Thread Safety**: Safe for concurrent use. Atomic read.

**Example**:
```go
state := mgr.State()
log.Printf("Current state: %s", state)
```

---

#### RefreshPartitions

Triggers partition discovery refresh.

```go
func (m *Manager) RefreshPartitions(ctx context.Context) error
```

Used when application knows partitions have changed. The leader will recalculate assignments after refresh.

**Parameters**:
- `ctx`: Context for cancellation and timeout

**Returns**:
- `error`: Error if refresh fails or nil on success

**Thread Safety**: Safe for concurrent use.

**Example**:
```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := mgr.RefreshPartitions(ctx); err != nil {
    log.Printf("Failed to refresh: %v", err)
}
```

---

## Stable ID Renewal Lifecycle

Short version (5 bullets):

- StartRenewal() starts the background renewal loop (no ctx), renews every ttl/3 (min 100ms), and is idempotent.
- Claim(ctx) is required first; otherwise StartRenewal() returns ErrNotClaimed.
- Release(ctx) stops the loop and deletes the key; calling it again returns ErrNotClaimed.
- Close() stops the loop but keeps the key; after Close(), StartRenewal() returns ErrAlreadyClosed.
- Renewal tick uses an internal short timeout (100ms–5s); failures are logged and retried next tick.

> For details and examples, see the User Guide: Stable ID Renewal Lifecycle.
https://github.com/arloliu/parti/blob/main/docs/LIFECYCLE.md#stable-id-renewal-lifecycle

## Core Interfaces

### AssignmentStrategy

Calculates partition assignments for workers.

```go
type AssignmentStrategy interface {
    Assign(workers []string, partitions []Partition) (map[string][]Partition, error)
}
```

**Methods**:

#### Assign

Calculates assignments given workers and partitions.

**Parameters**:
- `workers`: List of worker IDs (e.g., ["worker-0", "worker-1"])
- `partitions`: List of partitions to assign

**Returns**:
- `map[string][]Partition`: Map from workerID to assigned partitions
- `error`: Assignment error (e.g., no workers available)

**Example Implementation**:
```go
type MyStrategy struct{}

func (s *MyStrategy) Assign(
    workers []string,
    partitions []Partition,
) (map[string][]Partition, error) {
    if len(workers) == 0 {
        return nil, errors.New("no workers available")
    }

    assignments := make(map[string][]Partition)
    for i, p := range partitions {
        workerID := workers[i%len(workers)]
        assignments[workerID] = append(assignments[workerID], p)
    }

    return assignments, nil
}
```

---

### PartitionSource

Discovers available partitions.

```go
type PartitionSource interface {
    ListPartitions(ctx context.Context) ([]Partition, error)
}
```

**Methods**:

#### ListPartitions

Returns all available partitions.

**Parameters**:
- `ctx`: Context for cancellation and timeout

**Returns**:
- `[]Partition`: List of available partitions
- `error`: Discovery error or nil on success

**When Called**:
- During manager initialization
- When `RefreshPartitions()` is triggered
- Periodically by leader (if configured)

**Example Implementation**:
```go
type DBPartitionSource struct {
    db *sql.DB
}

func (s *DBPartitionSource) ListPartitions(ctx context.Context) ([]Partition, error) {
    rows, err := s.db.QueryContext(ctx, "SELECT id, weight FROM partitions")
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var partitions []Partition
    for rows.Next() {
        var id string
        var weight int64
        if err := rows.Scan(&id, &weight); err != nil {
            return nil, err
        }
        partitions = append(partitions, Partition{
            Keys:   []string{id},
            Weight: weight,
        })
    }

    return partitions, rows.Err()
}
```

---

### ElectionAgent

Handles leader election (optional).

```go
type ElectionAgent interface {
    RequestLeadership(ctx context.Context, workerID string, leaseDuration time.Duration) (bool, error)
    RenewLeadership(ctx context.Context) error
    ReleaseLeadership(ctx context.Context) error
    IsLeader(ctx context.Context) bool
}
```

**Methods**:

#### RequestLeadership

Requests leadership with a lease duration.

**Parameters**:
- `ctx`: Context for cancellation and timeout
- `workerID`: ID of worker requesting leadership
- `leaseDuration`: Duration of leadership lease

**Returns**:
- `bool`: true if leadership granted, false otherwise
- `error`: Election error or nil

#### RenewLeadership

Renews the current leadership lease.

**Parameters**:
- `ctx`: Context for cancellation and timeout

**Returns**:
- `error`: Renewal error or nil on success

#### ReleaseLeadership

Releases the leadership.

**Parameters**:
- `ctx`: Context for cancellation and timeout

**Returns**:
- `error`: Release error or nil on success

#### IsLeader

Checks if this worker is the current leader.

**Parameters**:
- `ctx`: Context for cancellation and timeout

**Returns**:
- `bool`: true if leader, false otherwise

**Example Implementation** (Consul):
```go
type ConsulElectionAgent struct {
    client *api.Client
    session string
    key    string
}

func (a *ConsulElectionAgent) RequestLeadership(
    ctx context.Context,
    workerID string,
    leaseDuration time.Duration,
) (bool, error) {
    // Consul lock acquisition logic
    kv := a.client.KV()
    acquired, _, err := kv.Acquire(&api.KVPair{
        Key:     a.key,
        Value:   []byte(workerID),
        Session: a.session,
    }, nil)
    return acquired, err
}
```

---

### MetricsCollector

Collects metrics for observability.

```go
// MetricsCollector is a composite interface; it embeds ManagerMetrics,
// CalculatorMetrics, WorkerMetrics, AssignmentMetrics, and WorkerConsumerMetrics.
type MetricsCollector interface {
    // Manager Metrics
    RecordStateTransition(from, to State, duration float64)
    RecordLeadershipChange(newLeader string)
    RecordDegradedDuration(duration float64) // Seconds spent in degraded mode
    SetDegradedMode(value float64)           // 1.0 = degraded, 0.0 = normal
    SetCacheAge(age float64)                 // Age of cached assignment in seconds
    SetAlertLevel(level int)                 // Current alert level (0-3)
    IncrementAlertEmitted(level string)      // Count alerts by level

    // Assignment Metrics
    RecordAssignmentChange(added, removed int, version int64)

    // Worker Metrics
    RecordHeartbeat(workerID string, success bool)

    // Calculator Metrics
    RecordCacheUsage(cacheType string, age float64) // Type ("workers","assignments") and age in seconds
    IncrementCacheFallback(reason string)            // Reason ("connectivity_error","timeout","unknown")

    // ... see types.WorkerConsumerMetrics for consumer-specific methods
}
```

**New Degraded Mode Metrics**:

#### RecordDegradedDuration
Records total time spent in degraded mode (value in seconds).
```go
// Called on exit from degraded mode (300 seconds = 5 minutes)
collector.RecordDegradedDuration(300)
```

#### SetDegradedMode
Sets current degraded mode state (gauge).
```go
// 1.0 when degraded, 0.0 when normal
collector.SetDegradedMode(1.0) // Entering degraded
collector.SetDegradedMode(0.0) // Exiting degraded
```

#### SetCacheAge
Sets age of cached assignment data in seconds (gauge).
```go
// Updated periodically while in degraded mode (120 seconds = 2 minutes)
collector.SetCacheAge(120)
```

#### SetAlertLevel
Sets current alert level (gauge).
```go
// 0=none, 1=info, 2=warn, 3=error, 4=critical
collector.SetAlertLevel(2) // Warn level active
```

#### IncrementAlertEmitted
Increments counter for emitted alerts by level.
```go
collector.IncrementAlertEmitted("Critical")
```

#### RecordCacheUsage
Records when cached data is used instead of fresh KV data.
```go
collector.RecordCacheUsage("workers", 5.2)      // Using cached workers list, 5.2s old
collector.RecordCacheUsage("assignments", 0.5)  // Using cached assignments, 0.5s old
```

#### IncrementCacheFallback
Increments counter when calculator falls back to cache.
```go
collector.IncrementCacheFallback("connectivity_error") // Network connectivity issue
```

**Methods**: See [Observability Section](#observability) for details.

**Example Implementation** (Prometheus):
```go
type PrometheusCollector struct {
    stateGauge          prometheus.Gauge
    degradedModeGauge   prometheus.Gauge
    cacheAgeGauge       prometheus.Gauge
    alertLevelGauge     prometheus.Gauge
    alertsTotal         *prometheus.CounterVec
    cacheFallbackTotal  prometheus.Counter
    // ... other metrics
}

func (c *PrometheusCollector) SetDegradedMode(value float64) {
    c.degradedModeGauge.Set(value)
}

func (c *PrometheusCollector) IncrementAlertEmitted(level string) {
    c.alertsTotal.WithLabelValues(level).Inc()
}

func (c *PrometheusCollector) IncrementCacheFallback(reason string) {
    c.cacheFallbackTotal.Inc()
}
```

---

### Logger

Provides logging capabilities.

```go
type Logger interface {
    Debug(msg string, keysAndValues ...any)
    Info(msg string, keysAndValues ...any)
    Warn(msg string, keysAndValues ...any)
    Error(msg string, keysAndValues ...any)
}
```

**Methods**: Standard leveled logging with structured key-value pairs.

**Compatible With**:
- `zap.SugaredLogger`
- `logrus.Logger` (with adapter)
- `slog.Logger`

**Example Implementation** (zap):
```go
logger, _ := zap.NewProduction()
sugar := logger.Sugar()

js, _ := jetstream.New(nc)
mgr, err := parti.NewManager(cfg, js, src, strategy,
    parti.WithLogger(sugar),
)
if err != nil {
    log.Fatal(err)
}
```

---

### Hooks

Defines callbacks for Manager lifecycle events.

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

**Fields**:

#### OnAssignmentChanged

Called when this worker's partition assignment changes.

**Parameters**:
- `ctx`: Lifecycle context (cancelled during shutdown)
- `oldPartitions`: Previous complete assignment set
- `newPartitions`: New complete assignment set

**Returns**:
- `error`: Error for logging (doesn't fail manager operation)

**Execution**: Asynchronous in background goroutine.

#### OnStateChanged

Called when worker state transitions.

**Parameters**:
- `ctx`: Lifecycle context (cancelled during shutdown)
- `from`: Previous state
- `to`: New state

**Returns**:
- `error`: Error for logging (doesn't fail manager operation)

**Execution**: Asynchronous in background goroutine.

#### OnError

Called when a recoverable error occurs.

**Parameters**:
- `ctx`: Lifecycle context (cancelled during shutdown)
- `err`: The error that occurred

**Returns**:
- `error`: Error for logging (doesn't fail manager operation)

**Execution**: Asynchronous in background goroutine.

#### OnLeadershipChanged

Called when the worker acquires or loses leadership.

**Parameters**:
- `ctx`: Lifecycle context (cancelled during shutdown)
- `isLeader`: true if the worker is now the leader, false otherwise

**Returns**:
- `error`: Error for logging (doesn't fail manager operation)

**Execution**: Asynchronous in background goroutine.

#### OnPartitionsAssigned

Called when new partitions are assigned to this worker. Convenience hook derived from OnAssignmentChanged.

**Parameters**:
- `ctx`: Lifecycle context (cancelled during shutdown)
- `partitions`: List of newly assigned partitions

**Returns**:
- `error`: Error for logging (doesn't fail manager operation)

**Execution**: Asynchronous in background goroutine.

#### OnPartitionsRevoked

Called when partitions are removed from this worker. Convenience hook derived from OnAssignmentChanged.

**Parameters**:
- `ctx`: Lifecycle context (cancelled during shutdown)
- `partitions`: List of removed partitions

**Returns**:
- `error`: Error for logging (doesn't fail manager operation)

**Execution**: Asynchronous in background goroutine.

#### OnDegraded

Called once when the manager transitions into degraded mode.

**Parameters**:
- `ctx`: Lifecycle context (cancelled during shutdown)
- `reason`: Description of the cause (e.g., "NATS connection down", "KV error threshold exceeded")

**Returns**:
- `error`: Error for logging (doesn't fail manager operation)

**Execution**: Asynchronous in background goroutine.

**Example**:
```go
hooks := &parti.Hooks{
    OnLeadershipChanged: func(ctx context.Context, isLeader bool) error {
        if isLeader {
            log.Info("I am the leader now")
        } else {
            log.Info("I am no longer the leader")
        }
        return nil
    },
}
```

---

## Configuration Types

### Config

Main configuration structure.

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
    ColdStartWindow       time.Duration // Window for cold start (default: 30s)
    PlannedScaleWindow    time.Duration // Window for planned scale (default: 10s)
    EmergencyGracePeriod  time.Duration // Grace period before emergency (default: 0 = auto = 1.5 * HeartbeatInterval)
    RestartDetectionRatio float64       // Ratio for restart classification (default: 0.5)

    // Timeouts
    OperationTimeout time.Duration // Timeout for KV operations (default: 10s)
    ElectionTimeout  time.Duration // Timeout for leader election (default: 5s)
    StartupTimeout   time.Duration // Timeout for manager startup (default: 30s)
    ShutdownTimeout  time.Duration // Timeout for graceful shutdown (default: 10s)

    // Assignment Configuration
    RebalanceCooldown time.Duration // Min time between rebalances (default: 10s)

    // Handoff Configuration
    EnableTwoPhaseHandoff bool          // Enable prepare/commit protocol (default: false)
    Handoff               HandoffConfig // Tuning for handoff process

    // Degraded Mode Configuration
    DegradedBehavior DegradedBehaviorConfig // Degraded mode behavior
    DegradedAlert    DegradedAlertConfig    // Degraded mode alerts

    // KV Bucket Configuration
    KVBuckets KVBucketConfig // KV bucket names and TTLs
}
```

**Methods**:

#### SetDefaults

Fills in missing configuration values with defaults.

```go
func SetDefaults(cfg *Config) error
```

**Example**:
```go
cfg := &Config{WorkerIDMax: 999}
if err := parti.SetDefaults(cfg); err != nil {
    log.Fatal(err)
}
// Now cfg has all defaults filled in
```

#### Validate

Validates configuration values.

```go
func (c *Config) Validate() error
```

**Returns**:
- `error`: Validation error or nil if valid

**Example**:
```go
if err := cfg.Validate(); err != nil {
    log.Fatalf("Invalid config: %v", err)
}
```

---

### HandoffConfig

Controls the two-phase handoff process.

```go
type HandoffConfig struct {
    SweepInterval     time.Duration // Interval to sweep stale claims
    MaxRetries        int           // Max CAS retries for claims
    BaseBackoff       time.Duration // Initial backoff for retries
    MaxBackoff        time.Duration // Max backoff for retries
    Jitter            float64       // Jitter factor
    DelayAfterPrepare time.Duration // Artificial delay after prepare
    DelayBeforeStable time.Duration // Artificial delay before stable
}
```

**Fields**:
- `SweepInterval`: Interval to sweep stale claims (default: 30s)
- `MaxRetries`: Max CAS retries for claims (default: 3)
- `BaseBackoff`: Initial backoff for retries (default: 50ms)
- `MaxBackoff`: Max backoff for retries (default: 500ms)
- `Jitter`: Jitter factor (default: 0.2)
- `DelayAfterPrepare`: Artificial delay after prepare (default: 0)
- `DelayBeforeStable`: Artificial delay before stable (default: 0)

---

### DegradedBehaviorConfig

Controls when the manager enters and exits degraded mode.

```go
type DegradedBehaviorConfig struct {
    EnterThreshold      time.Duration // Time without NATS before entering degraded
    ExitThreshold       time.Duration // Time with NATS before exiting degraded
    KVErrorThreshold    int           // Consecutive KV errors to trigger degraded
    KVErrorWindow       time.Duration // Time window for counting KV errors
    RecoveryGracePeriod time.Duration // Grace period after recovery before rebalancing
}
```

**Fields**:
- `EnterThreshold`: Time without NATS connectivity before entering degraded mode (default: 10s)
- `ExitThreshold`: Time with restored NATS before exiting degraded mode (default: 5s)
- `KVErrorThreshold`: Number of consecutive KV errors to trigger degraded mode (default: 5)
- `KVErrorWindow`: Time window for counting KV errors (default: 30s)
- `RecoveryGracePeriod`: Time after recovery before leaders can trigger emergency rebalancing (default: 15s)

---

### DegradedAlertConfig

Controls alert emission during degraded mode.

```go
type DegradedAlertConfig struct {
    InfoThreshold     time.Duration // Duration to trigger Info alert
    WarnThreshold     time.Duration // Duration to trigger Warn alert
    ErrorThreshold    time.Duration // Duration to trigger Error alert
    CriticalThreshold time.Duration // Duration to trigger Critical alert
    AlertInterval     time.Duration // Minimum time between alerts
}
```

**Fields**:
- `InfoThreshold`: Duration in degraded mode before Info alert (default: 30s)
- `WarnThreshold`: Duration before Warn alert (default: 2m)
- `ErrorThreshold`: Duration before Error alert (default: 5m)
- `CriticalThreshold`: Duration before Critical alert (default: 10m)
- `AlertInterval`: Minimum time between repeated alerts (default: 1m)

---

### KVBucketConfig

Configures NATS JetStream KV bucket names and TTLs.

```go
type KVBucketConfig struct {
    StableIDBucket   string        // Bucket for worker ID claims
    ElectionBucket   string        // Bucket for leader election
    HeartbeatBucket  string        // Bucket for heartbeats
    AssignmentBucket string        // Bucket for assignments
    HandoffBucket    string        // Bucket for handoff coordination
    AssignmentTTL    time.Duration // TTL for assignments (0 = no expiration)
}
```

**Defaults**:
- `StableIDBucket`: "parti-stableid"
- `ElectionBucket`: "parti-election"
- `HeartbeatBucket`: "parti-heartbeat"
- `AssignmentBucket`: "parti-assignment"
- `HandoffBucket`: "parti-handoff"
- `AssignmentTTL`: 0 (no expiration - assignments persist)

---

### DegradedAlertConfig

**NEW**: Controls alert emission during degraded mode.

```go
type DegradedAlertConfig struct {
    InfoThreshold     time.Duration // Duration to trigger Info alert
    WarnThreshold     time.Duration // Duration to trigger Warn alert
    ErrorThreshold    time.Duration // Duration to trigger Error alert
    CriticalThreshold time.Duration // Duration to trigger Critical alert
    AlertInterval     time.Duration // Minimum time between alerts
}
```

**Fields**:
- `InfoThreshold`: Duration in degraded mode before Info alert (default: 30s)
- `WarnThreshold`: Duration before Warn alert (default: 2m)
- `ErrorThreshold`: Duration before Error alert (default: 5m)
- `CriticalThreshold`: Duration before Critical alert (default: 10m)
- `AlertInterval`: Minimum time between repeated alerts (default: 1m)

**Alert Escalation**:
Alerts escalate as degraded mode persists:
```
30s: [INFO] Degraded mode active
 2m: [WARN] Degraded mode persisting
 5m: [ERROR] Prolonged degraded mode
10m: [CRITICAL] Extended degraded mode
```

**Validation Rules**:
- Thresholds must be in ascending order: Info ≤ Warn ≤ Error ≤ Critical
- All threshold values and AlertInterval must be positive (> 0)

**Example**:
```go
cfg.DegradedAlert = parti.DegradedAlertConfig{
    InfoThreshold:     30 * time.Second,
    WarnThreshold:     2 * time.Minute,
    ErrorThreshold:    5 * time.Minute,
    CriticalThreshold: 10 * time.Minute,
    AlertInterval:     1 * time.Minute,
}
```

---

### DegradedBehaviorConfig

**NEW**: Controls when the manager enters and exits degraded mode.

```go
type DegradedBehaviorConfig struct {
    EnterThreshold      time.Duration // Time without NATS before entering degraded
    ExitThreshold       time.Duration // Time with NATS before exiting degraded
    KVErrorThreshold    int           // Consecutive KV errors to trigger degraded
    KVErrorWindow       time.Duration // Time window for counting KV errors
    RecoveryGracePeriod time.Duration // Grace period after recovery before rebalancing
}
```

**Fields**:
- `EnterThreshold`: Time without NATS connectivity before entering degraded mode (default: 10s)
- `ExitThreshold`: Time with restored NATS before exiting degraded mode (default: 5s)
- `KVErrorThreshold`: Number of consecutive KV errors to trigger degraded mode (default: 5)
- `KVErrorWindow`: Time window for counting KV errors (default: 30s)
- `RecoveryGracePeriod`: Time after recovery before leaders can trigger emergency rebalancing (default: 15s)

**Behavior**:
- **Enter Degraded**: Triggered by sustained NATS disconnection or repeated KV errors
- **Exit Degraded**: Requires stable NATS connectivity for `ExitThreshold` duration
- **Recovery Grace**: Prevents false emergency rebalancing after leader recovers first

**Validation Rules**:
- All threshold values must be non-negative (≥ 0)
- Zero values are valid (immediate behavior, no grace period)

**Example**:
```go
cfg.DegradedBehavior = parti.DegradedBehaviorConfig{
    EnterThreshold:      5 * time.Second,
    ExitThreshold:       3 * time.Second,
    KVErrorThreshold:    3,
    KVErrorWindow:       10 * time.Second,
    RecoveryGracePeriod: 15 * time.Second,
}
```

**Preset Configurations**:
```go
// Conservative (longer thresholds, slower degraded entry)
cfg.DegradedBehavior, _ = parti.DegradedBehaviorPreset("conservative")

// Balanced (default)
cfg.DegradedBehavior, _ = parti.DegradedBehaviorPreset("balanced")

// Aggressive (shorter thresholds, faster degraded entry)
cfg.DegradedBehavior, _ = parti.DegradedBehaviorPreset("aggressive")
```

---

## Data Types

### State

Represents the worker lifecycle state.

```go
type State int

const (
    StateInit              State = iota
    StateClaimingID
    StateElection
    StateWaitingAssignment
    StateStable
    StateScaling
    StateRebalancing
    StateEmergency
    StateDegraded         // NEW: Degraded mode (stale cache, NATS disconnected)
    StateShutdown
)
```

**State Descriptions**:
- `StateInit`: Initial state before startup
- `StateClaimingID`: Claiming a stable worker ID
- `StateElection`: Participating in leader election
- `StateWaitingAssignment`: Waiting for initial partition assignment
- `StateStable`: Normal operation with active assignments
- `StateScaling`: New workers joining, preparing to rebalance
- `StateRebalancing`: Actively redistributing partitions
- `StateEmergency`: Critical worker failure, emergency rebalancing
- `StateDegraded`: **Degraded mode** - Using stale cache due to NATS connectivity loss
- `StateShutdown`: Graceful shutdown in progress

**Degraded Mode Behavior**:
When a worker enters `StateDegraded`:
- Continues processing with cached partition assignments (frozen)
- Does not accept new assignments or trigger rebalancing
- Emits periodic alerts (Info → Warn → Error → Critical) based on cache age
- **Never escalates to Emergency** (staleness is not a critical failure)
- Automatically exits when NATS connectivity is restored

**Philosophy**: *"Stale but stable is better than fresh but broken"*

**Methods**:

#### String

Returns the string representation of the state.

```go
func (s State) String() string
```

**Example**:
```go
state := mgr.State()
fmt.Printf("Current state: %s", state.String())
```

---

### Partition

Represents a logical work partition.

```go
type Partition struct {
    Keys   []string  // Hierarchical partition keys
    Weight int64     // Relative processing cost (0 = strategy default)
}
```

**Fields**:
- `Keys`: Uniquely identify this partition (e.g., ["topic", "partition_id"])
- `Weight`: Relative processing cost for load balancing. `0` means "use the strategy's default weight"; negative values are treated as `0`.

**Example**:
```go
p := Partition{
    Keys:   []string{"orders", "0"},
    Weight: 150,
}
```

**Helpers**:

```go
func (p Partition) SubjectKey() string // Keys joined with '.' (e.g., "orders.0")
func (p Partition) ID() string         // Keys joined with '-' (e.g., "orders-0")
func (p Partition) Compare(q Partition) int // Lexicographic key comparison: -1,0,+1
```

Use `SubjectKey()` for JetStream subject templating and `FilterSubjects` construction.
Use `ID()` for durable names (e.g., `<ConsumerPrefix>-<ID()>`) and hashing contexts.
Use `Compare()` as a stable, allocation-free tie-breaker (keys only, weight ignored) in ordering.

---

### Assignment

Contains the current partition assignment for a worker.

```go
type Assignment struct {
    Version    int64       // Monotonically increasing version
    Lifecycle  string      // Assignment phase (e.g., "stable", "scaling")
    Partitions []Partition // Assigned partitions
}
```

**Fields**:
- `Version`: Monotonically increasing assignment version
- `Lifecycle`: Assignment phase ("cold_start", "post_cold_start", "stable")
- `Partitions`: List of partitions assigned to this worker

**Example**:
```go
assignment := mgr.CurrentAssignment()
for _, p := range assignment.Partitions {
    log.Printf("Assigned: %v (weight: %d)", p.Keys, p.Weight)
}
```

---

### HandoffState

Represents the state of the two-phase handoff process.

```go
type HandoffState int

const (
    HandoffStateUnknown HandoffState = iota
    HandoffStateStable
    HandoffStatePrepare
    HandoffStateCommit
)
```

---

## Strategy Package

Package `github.com/arloliu/parti/v2/strategy` provides built-in assignment strategies.

### ConsistentHash

Consistent hashing with virtual nodes.

```go
func NewConsistentHash(opts ...ConsistentHashOption) *ConsistentHash
```

**Options**:

#### WithVirtualNodes

Sets the number of virtual nodes per worker.

```go
func WithVirtualNodes(nodes int) ConsistentHashOption
```

**Example**:
```go
strategy := strategy.NewConsistentHash(
    strategy.WithVirtualNodes(300),
)
```

#### WithHashSeed

Sets a custom hash seed.

```go
func WithHashSeed(seed uint64) ConsistentHashOption
```

**Example**:
```go
strategy := strategy.NewConsistentHash(
    strategy.WithHashSeed(12345),
)
```

---

### RoundRobin

Simple round-robin distribution.

```go
func NewRoundRobin() *RoundRobin
```

**Example**:
```go
strategy := strategy.NewRoundRobin()
```

---

### WeightedConsistentHash

Weighted consistent hashing with overload protection and extreme partition handling.

```go
func NewWeightedConsistentHash(opts ...WeightedConsistentHashOption) *WeightedConsistentHash
```

**Options**:

#### WithWeightedVirtualNodes
Sets the number of virtual nodes per worker (default: 150).

#### WithOverloadThreshold
Sets the maximum allowed load variance (default: 1.2 or 120%).

#### WithExtremeThreshold
Sets the threshold for identifying "extreme" (heavy) partitions (default: 20.0).

#### WithMinPartitionCount
Sets the minimum partition count factor (default: 0.3).

**Example**:
```go
strategy := strategy.NewWeightedConsistentHash(
    strategy.WithWeightedVirtualNodes(200),
    strategy.WithOverloadThreshold(1.1), // Tighter balance
)
```

---

## Source Package

Package `github.com/arloliu/parti/v2/source` provides built-in partition sources.

### Static

Fixed list of partitions.

```go
func NewStatic(partitions []Partition) *Static
```

**Example**:
```go
partitions := []parti.Partition{
    {Keys: []string{"topic1", "0"}, Weight: 100},
    {Keys: []string{"topic1", "1"}, Weight: 100},
}
src := source.NewStatic(partitions)
```

---

### NatsKV

NATS KeyValue-backed partition source that supports dynamic updates.

```go
func NewNatsKV(kv jetstream.KeyValue, key string, logger types.Logger) *NatsKV
```

**Methods**:

#### Update

Updates the partition list in the KV bucket with automatic Gzip compression for large payloads.

```go
func (s *NatsKV) Update(ctx context.Context, partitions []types.Partition) error
```

**Parameters**:
- `ctx`: Context for cancellation
- `partitions`: New list of partitions to store

**Returns**:
- `error`: Update error

**Example**:
```go
kv, _ := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions"})
src := source.NewNatsKV(kv, "config", logger)

// Update partitions (automatically compressed if needed)
err := src.Update(ctx, newPartitions)
```

---

## Consumer Package

Package `github.com/arloliu/parti/v2/consumer` provides unified JetStream consumer types for partitioned workloads. This package replaces the legacy `subscription` and `partition` packages.

### Consumer Types

| Type        | Use Case                               | Lifecycle        |
|-------------|----------------------------------------|------------------|
| `Queue`     | Load-balanced workers (queue group)    | Start → Stop     |
| `Static`    | Fixed partition (StatefulSet ordinal)  | Start → Stop     |
| `Dynamic`   | Manager-assigned partitions            | Update → Stop    |
| `Broadcast` | Fan-out to all instances               | Start → Stop     |

### NewQueue

Creates a load-balanced consumer where multiple instances share one durable.

```go
func NewQueue(
    js jetstream.JetStream,
    streamName string,
    consumerName string,
    filterSubject string,
    handler MessageHandler,
    opts ...QueueOption,
) (*Queue, error)
```

**Parameters**:
- `js`: JetStream context
- `streamName`: JetStream stream name
- `consumerName`: Shared durable consumer name
- `filterSubject`: Subject filter (supports wildcards)
- `handler`: Message handler callback
- `opts`: Functional options

**Returns**:
- `*Queue`: Initialized queue consumer
- `error`: Validation error

**Example**:
```go
c, err := consumer.NewQueue(js, "JOBS", "job-workers", "jobs.>", handler)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

if err := c.Start(ctx); err != nil {
    log.Fatal(err)
}
```

---

### NewStatic

Creates a consumer bound to a single, fixed partition.

```go
func NewStatic(
    js jetstream.JetStream,
    streamName string,
    consumerName string,
    subjectPattern string,
    numPartitions int,
    partition int,
    handler MessageHandler,
    opts ...StaticOption,
) (*Static, error)
```

**Parameters**:
- `js`: JetStream context
- `streamName`: JetStream stream name
- `consumerName`: Durable consumer name
- `subjectPattern`: Subject template with `{{partition}}` placeholder
- `numPartitions`: Total number of partitions
- `partition`: This instance's partition index (0 to numPartitions-1)
- `handler`: Message handler callback
- `opts`: Functional options

**Example**:
```go
c, err := consumer.NewStatic(js, "EVENTS", "processor-0", "events.{{partition}}", 10, 0, handler)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

if err := c.Start(ctx); err != nil {
    log.Fatal(err)
}
```

---

### NewDynamic

Creates a partition-aware consumer that receives assignments from a Parti Manager.

```go
func NewDynamic(
    js jetstream.JetStream,
    streamName string,
    consumerPrefix string,
    subjectTemplate string,
    handler MessageHandler,
    opts ...DynamicOption,
) (*Dynamic, error)
```

**Parameters**:
- `js`: JetStream context
- `streamName`: JetStream stream name
- `consumerPrefix`: Prefix for durable consumer names
- `subjectTemplate`: Go text/template with `{{.PartitionID}}`
- `handler`: Message handler callback
- `opts`: Functional options

**Example**:
```go
c, err := consumer.NewDynamic(js, "ORDERS", "processor", "orders.{{.PartitionID}}", handler)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

// Register with Manager for automatic updates
mgr, _ := parti.NewManager(cfg, js, src, strategy.NewConsistentHash(),
    parti.WithWorkerConsumerUpdater(c),
)
```

---

### NewBroadcast

Creates a fan-out consumer where every instance receives every message.

```go
func NewBroadcast(
    js jetstream.JetStream,
    streamName string,
    consumerPrefix string,
    filterSubject string,
    handler MessageHandler,
    opts ...BroadcastOption,
) (*Broadcast, error)
```

**Parameters**:
- `js`: JetStream context
- `streamName`: JetStream stream name
- `consumerPrefix`: Prefix for durable consumer name
- `filterSubject`: Subject filter (supports wildcards)
- `handler`: Message handler callback
- `opts`: Functional options

**Example**:
```go
c, err := consumer.NewBroadcast(js, "EVENTS", "cache-updater", "events.>", handler,
    consumer.WithInstanceID("pod-abc123"),
)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

if err := c.Start(ctx); err != nil {
    log.Fatal(err)
}
```

---

### MessageHandler

Interface for processing messages.

```go
type MessageHandler interface {
    Handle(ctx context.Context, msg jetstream.Msg) error
}
```

**Functional Adapter**:
```go
type MessageHandlerFunc func(ctx context.Context, msg jetstream.Msg) error

func (f MessageHandlerFunc) Handle(ctx context.Context, msg jetstream.Msg) error {
    return f(ctx, msg)
}
```

---

### Consumer Methods

#### Queue Methods

| Method        | Description                              |
|---------------|------------------------------------------|
| `Start(ctx)`  | Begin consuming messages                 |
| `Stop(ctx)`   | Gracefully stop with context timeout     |

#### Static Methods

| Method        | Description                              |
|---------------|------------------------------------------|
| `Start(ctx)`  | Begin consuming messages                 |
| `Stop(ctx)`   | Gracefully stop with context timeout     |
| `Partition()` | Returns the partition index              |
| `Subject()`   | Returns the filter subject               |

#### Dynamic Methods

| Method                                      | Description                              |
|---------------------------------------------|------------------------------------------|
| `Update(ctx, workerID, partitions)`         | Update partition assignments             |
| `Stop(ctx)`                                 | Gracefully stop all partition consumers  |
| `UpdateWorkerConsumer(ctx, id, partitions)` | Implements `WorkerConsumerUpdater`       |

#### Broadcast Methods

| Method        | Description                              |
|---------------|------------------------------------------|
| `Start(ctx)`  | Begin consuming messages                 |
| `Stop(ctx)`   | Gracefully stop with context timeout     |

---

### Consumer Options

All consumers accept functional options:

```go
c, _ := consumer.NewQueue(js, "stream", "consumer", "subject.>", handler,
    consumer.WithLogger(myLogger),
    consumer.WithAckWait(60*time.Second),
    consumer.WithBatchSize(100),
)
```

**Common Options**:

| Option                           | Description                                    |
|----------------------------------|------------------------------------------------|
| `WithLogger(logger)`             | Set custom logger                              |
| `WithMetrics(collector)`         | Set metrics collector                          |
| `WithAckWait(duration)`          | Time before message redelivery                 |
| `WithBatchSize(n)`               | Messages per fetch                             |
| `WithMaxDeliver(n)`              | Max redelivery attempts                        |
| `WithMaxAckPending(n)`           | Max unacked messages                           |
| `WithFetchTimeout(duration)`     | Max wait when pulling batch                    |
| `WithManualAck(bool)`            | Disable auto-acknowledgement                   |
| `WithInactiveThreshold(duration)`| Consumer cleanup threshold                     |
| `WithRecoveryStrategy(strategy)` | Auto-recovery on unexpected consumer deletion  |

**Type-Specific Options**:

| Option                           | Consumer Types | Description                          |
|----------------------------------|----------------|--------------------------------------|
| `WithInstanceID(id)`             | Broadcast      | Set unique instance identifier       |
| `WithProcessingGate(cfg)`        | Dynamic        | Enable processing gate               |
| `WithDrainOnRemove(bool)`        | Dynamic        | Drain messages on partition removal  |

---

## Testing Package

Package `github.com/arloliu/parti/v2/partitest` provides utilities for testing.

### Embedded NATS

Starts an embedded NATS server for testing.

```go
func StartEmbeddedNATS(t *testing.T) (*server.Server, *nats.Conn)
```

**Example**:
```go
func TestMyFeature(t *testing.T) {
    _, nc := partitesting.StartEmbeddedNATS(t)

    // Use nc for testing. Cleanup is automatic via t.Cleanup().
}
```

---

## Error Types

Sentinel errors are defined in the `types` package and re-exported in the root package for convenience.

### Configuration Errors

```go
var (
    ErrInvalidConfig             = errors.New("invalid configuration")
    ErrNATSConnectionRequired    = errors.New("NATS connection is required")
    ErrPartitionSourceRequired   = errors.New("partition source is required")
    ErrAssignmentStrategyRequired = errors.New("assignment strategy is required")
)
```

### Lifecycle Errors

```go
var (
    ErrAlreadyStarted = errors.New("manager already started")
    ErrNotStarted     = errors.New("manager not started")
)
```

### Operational Errors

```go
var (
    ErrElectionFailed    = errors.New("leader election failed")
    ErrConnectivity      = errors.New("connectivity issue")
    ErrDegraded          = errors.New("degraded operation: using cached data")
    ErrIDClaimFailed     = errors.New("failed to claim stable worker ID")
    ErrAssignmentFailed  = errors.New("assignment failed")
)
```

Degraded mode can be observed via `mgr.State() == parti.StateDegraded`, `Hooks.OnStateChanged` transitions, and `Hooks.OnDegraded`.

Some operations may return sentinel errors like `parti.ErrConnectivity` or `parti.ErrDegraded` for `errors.Is()` checks.

### Error Checking

Use `errors.Is()` for error checking:

```go
if errors.Is(err, parti.ErrInvalidConfig) {
    log.Fatal("Fix configuration")
}
```

---

## Functional Options

### WithElectionAgent

Sets a custom election agent.

```go
func WithElectionAgent(agent ElectionAgent) Option
```

**Example**:
```go
agent := NewConsulElectionAgent(consulClient)
js, _ := jetstream.New(nc)
mgr, err := parti.NewManager(cfg, js, src, strategy,
    parti.WithElectionAgent(agent),
)
if err != nil {
    log.Fatal(err)
}
```

---

### WithHooks

Sets lifecycle hooks.

```go
func WithHooks(hooks *Hooks) Option
```

**Example**:
```go
hooks := &parti.Hooks{
    OnStateChanged: func(ctx context.Context, from, to parti.State) error {
        log.Printf("State: %s -> %s", from, to)
        return nil
    },
}
js, _ := jetstream.New(nc)
mgr, err := parti.NewManager(cfg, js, src, strategy,
    parti.WithHooks(hooks),
)
if err != nil {
    log.Fatal(err)
}
```

---

### WithMetrics

Sets a metrics collector.

```go
func WithMetrics(metrics MetricsCollector) Option
```

**Example**:
```go
collector := NewPrometheusCollector()
js, _ := jetstream.New(nc)
mgr, err := parti.NewManager(cfg, js, src, strategy,
    parti.WithMetrics(collector),
)
if err != nil {
    log.Fatal(err)
}
```

---

### WithLogger

Sets a logger.

```go
func WithLogger(logger Logger) Option
```

**Example**:
```go
logger, _ := zap.NewProduction()
js, _ := jetstream.New(nc)
mgr, err := parti.NewManager(cfg, js, src, strategy,
    parti.WithLogger(logger.Sugar()),
)
if err != nil {
    log.Fatal(err)
}
```

---

## Thread Safety

| Component | Thread Safety | Notes |
|-----------|--------------|-------|
| `Manager.Start()` | Not reentrant | Call once per manager |
| `Manager.Stop()` | Not reentrant | Call once per manager |
| `Manager.WorkerID()` | Thread-safe | Read-only after claim |
| `Manager.IsLeader()` | Thread-safe | May change over time |
| `Manager.CurrentAssignment()` | Thread-safe | Returns copy |
| `Manager.State()` | Thread-safe | Atomic read |
| `Manager.RefreshPartitions()` | Thread-safe | Can call concurrently |
| Hooks callbacks | Sequential | Never called concurrently |

---

## Version Compatibility

### Semantic Versioning

- `v1.x.x`: Stable API, backward compatible
- `v2.x.x`: Breaking changes (if needed)

### NATS Compatibility

- Minimum: NATS Server 2.10.0+
- Recommended: NATS Server 2.12.0+
- JetStream: Required
- KV Store: Required

### Go Version

- Minimum: Go 1.25+
- Recommended: Latest stable Go version
