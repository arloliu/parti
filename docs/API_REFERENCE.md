# Parti API Reference

**Version**: 1.0.0
**Last Updated**: November 2, 2025
**Library**: `github.com/arloliu/parti`

---

## Table of Contents

1. [Manager Interface](#manager-interface)
2. [Stable ID Renewal Lifecycle](#stable-id-renewal-lifecycle)
3. [Core Interfaces](#core-interfaces)
4. [Configuration Types](#configuration-types)
5. [Data Types](#data-types)
6. [Strategy Package](#strategy-package)
7. [Source Package](#source-package)
8. [Subscription Package](#subscription-package)
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
- `conn`: NATS connection for coordination
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
    WorkerIDMax:    63,
}
parti.SetDefaults(cfg)

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

For details and examples, see the User Guide: Stable ID Renewal Lifecycle.
https://github.com/arloliu/parti/blob/main/docs/USER_GUIDE.md#stable-id-renewal-lifecycle

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
type MetricsCollector interface {
    // Manager Metrics
    RecordStateTransition(from, to State, duration time.Duration)
    RecordStateDuration(state State, duration time.Duration)
    RecordAssignmentChange(added, removed int, version int64)
    RecordAssignmentCalculationTime(duration time.Duration)
    RecordAffinityScore(score float64)
    RecordHeartbeat(workerID string, success bool)
    RecordLeadershipChange(newLeader string, duration time.Duration)

    // Degraded Mode Metrics (NEW)
    RecordDegradedDuration(duration time.Duration)    // Total time in degraded mode
    SetDegradedMode(value float64)                    // Current degraded state (1.0 = degraded, 0.0 = normal)
    SetCacheAge(duration time.Duration)               // Age of cached assignment
    SetAlertLevel(level int)                          // Current alert level (0-3)
    IncrementAlertEmitted(level string)               // Count alerts by level

    // Calculator Metrics (NEW)
    RecordCacheUsage(hit bool)                        // Cache hit/miss tracking
    IncrementCacheFallback()                          // Count cache fallback operations
}
```

**New Degraded Mode Metrics**:

#### RecordDegradedDuration
Records total time spent in degraded mode.
```go
// Called on exit from degraded mode
collector.RecordDegradedDuration(5 * time.Minute)
```

#### SetDegradedMode
Sets current degraded mode state (gauge).
```go
// 1.0 when degraded, 0.0 when normal
collector.SetDegradedMode(1.0) // Entering degraded
collector.SetDegradedMode(0.0) // Exiting degraded
```

#### SetCacheAge
Sets age of cached assignment (gauge).
```go
// Updated periodically while in degraded mode
collector.SetCacheAge(2 * time.Minute)
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
Records calculator cache hit/miss.
```go
collector.RecordCacheUsage(true)  // Cache hit
collector.RecordCacheUsage(false) // Cache miss
```

#### IncrementCacheFallback
Increments counter when calculator falls back to cache.
```go
collector.IncrementCacheFallback() // Using stale cache
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

func (c *PrometheusCollector) IncrementCacheFallback() {
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
mgr := parti.NewManager(cfg, js, src, strategy,
    parti.WithLogger(sugar),
)
```

---

### Hooks

Defines callbacks for Manager lifecycle events.

```go
type Hooks struct {
    OnAssignmentChanged func(ctx context.Context, added, removed []Partition) error
    OnStateChanged      func(ctx context.Context, from, to State) error
    OnError             func(ctx context.Context, err error) error
    OnDegradedAlert     func(ctx context.Context, level string, duration time.Duration) error // NEW
}
```

**Fields**:

#### OnAssignmentChanged

Called when partition assignment changes.

**Parameters**:
- `ctx`: Lifecycle context (cancelled during shutdown)
- `added`: Partitions newly assigned to this worker
- `removed`: Partitions no longer assigned to this worker

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

#### OnDegradedAlert

**NEW**: Called when degraded mode alert threshold is exceeded.

**Parameters**:
- `ctx`: Lifecycle context (cancelled during shutdown)
- `level`: Alert level - "Info", "Warn", "Error", or "Critical"
- `duration`: How long the manager has been in degraded mode

**Returns**:
- `error`: Error for logging (doesn't fail manager operation)

**Execution**: Asynchronous in background goroutine.

**Example**:
```go
hooks := &parti.Hooks{
    OnDegradedAlert: func(ctx context.Context, level string, duration time.Duration) error {
        switch level {
        case "Critical":
            pagerDuty.Alert("Parti degraded for %v", duration)
        case "Error":
            slack.Notify("#ops", "Parti degraded for %v", duration)
        case "Warn":
            log.Warn("Parti degraded for %v", duration)
        default:
            log.Info("Parti degraded for %v", duration)
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
    WorkerIDPrefix string
    WorkerIDMin    int
    WorkerIDMax    int
    WorkerIDTTL    time.Duration

    // Heartbeat Configuration
    HeartbeatInterval time.Duration
    HeartbeatTTL      time.Duration

    // Stabilization Windows
    ColdStartWindow      time.Duration
    PlannedScaleWindow   time.Duration
    EmergencyGracePeriod time.Duration

    // Timeouts
    OperationTimeout time.Duration
    ElectionTimeout  time.Duration
    StartupTimeout   time.Duration
    ShutdownTimeout  time.Duration

    // Assignment Configuration
    Assignment AssignmentConfig

    // KV Bucket Configuration
    KVBucket KVBucketConfig
}
```

**Methods**:

#### SetDefaults

Fills in missing configuration values with defaults.

```go
func SetDefaults(cfg *Config)
```

**Example**:
```go
cfg := &Config{WorkerIDMax: 63}
parti.SetDefaults(cfg)
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

### AssignmentConfig

Controls rebalancing behavior.

```go
type AssignmentConfig struct {
    MinRebalanceThreshold float64       // Min imbalance ratio (0.0-1.0)
    MinRebalanceInterval  time.Duration // Min time between rebalances
}
```

**Fields**:
- `MinRebalanceThreshold`: Minimum partition imbalance ratio that triggers rebalancing (default: 0.15)
- `MinRebalanceInterval`: Minimum time between rebalancing operations (default: 10s)

---

### KVBucketConfig

Configures NATS JetStream KV bucket names and TTLs.

```go
type KVBucketConfig struct {
    StableIDBucket   string        // Bucket for worker ID claims
    ElectionBucket   string        // Bucket for leader election
    HeartbeatBucket  string        // Bucket for heartbeats
    AssignmentBucket string        // Bucket for assignments
    AssignmentTTL    time.Duration // TTL for assignments (0 = no expiration)
}
```

**Defaults**:
- `StableIDBucket`: "parti-stable-ids"
- `ElectionBucket`: "parti-election"
- `HeartbeatBucket`: "parti-heartbeats"
- `AssignmentBucket`: "parti-assignments"
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
- `InfoThreshold`: Duration in degraded mode before Info alert (default: 1 minute)
- `WarnThreshold`: Duration before Warn alert (default: 5 minutes)
- `ErrorThreshold`: Duration before Error alert (default: 15 minutes)
- `CriticalThreshold`: Duration before Critical alert (default: 30 minutes)
- `AlertInterval`: Minimum time between repeated alerts (default: 30 seconds)

**Alert Escalation**:
Alerts escalate as degraded mode persists:
```
1 min: [INFO] Degraded mode active
5 min: [WARN] Degraded mode persisting
15 min: [ERROR] Prolonged degraded mode
30 min: [CRITICAL] Extended degraded mode
```

**Validation Rules**:
- Thresholds must be in ascending order: Info ≤ Warn ≤ Error ≤ Critical
- AlertInterval must be positive (> 0)
- Zero thresholds are valid (disables that alert level)

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

**Preset Configurations**:
```go
// Conservative (longer thresholds, less noisy)
cfg.DegradedAlert = parti.DegradedAlertPreset("conservative")

// Balanced (default)
cfg.DegradedAlert = parti.DegradedAlertPreset("balanced")

// Aggressive (shorter thresholds, early detection)
cfg.DegradedAlert = parti.DegradedAlertPreset("aggressive")
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
- `RecoveryGracePeriod`: Time after recovery before leaders can trigger emergency rebalancing (default: 30s)

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
cfg.DegradedBehavior = parti.DegradedBehaviorPreset("conservative")

// Balanced (default)
cfg.DegradedBehavior = parti.DegradedBehaviorPreset("balanced")

// Aggressive (shorter thresholds, faster degraded entry)
cfg.DegradedBehavior = parti.DegradedBehaviorPreset("aggressive")
```
- `AssignmentBucket`: "parti-assignments"
- `AssignmentTTL`: 0 (no expiration)

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
    Weight int64     // Relative processing cost (default: 100)
}
```

**Fields**:
- `Keys`: Uniquely identify this partition (e.g., ["topic", "partition_id"])
- `Weight`: Relative processing cost for load balancing (default: 100)

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

## Strategy Package

Package `github.com/arloliu/parti/strategy` provides built-in assignment strategies.

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

## Source Package

Package `github.com/arloliu/parti/source` provides built-in partition sources.

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

## Subscription Package

Package `github.com/arloliu/parti/subscription` provides helpers for integrating JetStream message processing with partition assignments.

### DurableHelper (Single-Consumer Mode)

Manages a single durable pull consumer per worker whose FilterSubjects set is updated on assignment changes. This minimizes consumer churn and supports hot-reload of subjects without restarting the pull loop.

```go
func NewDurableHelper(conn *nats.Conn, cfg DurableConfig, handler MessageHandler) (*DurableHelper, error)
```

**DurableConfig**:
```go
type DurableConfig struct {
    StreamName       string        // Required: JetStream stream name
    ConsumerPrefix   string        // Required: Durable name prefix, final name <prefix>-<workerID>
    SubjectTemplate  string        // Required: Go template: .PartitionID => keys joined with "."
    AckPolicy        jetstream.AckPolicy
    AckWait          time.Duration
    MaxDeliver       int
    InactiveThreshold time.Duration
    BatchSize        int
    MaxWaiting       int
    FetchTimeout     time.Duration
    MaxRetries       int
    RetryBackoff     time.Duration
    Logger           parti.Logger
}
```

**Key Method**:

```go
func (dh *DurableHelper) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []parti.Partition) error
```

Updates (or creates) the durable consumer named `<ConsumerPrefix>-<workerID>` with a deduplicated, sorted set of subjects rendered from `SubjectTemplate` for each partition. Idempotent: re-applying the same partition set is a no-op. Starts a resilient pull loop on first invocation; subsequent updates hot-reload subjects without restarting the loop.

```go
func (dh *DurableHelper) WorkerConsumerInfo(ctx context.Context) (*jetstream.ConsumerInfo, error)
```

Returns the current JetStream ConsumerInfo for the worker-level durable consumer.
Errors if UpdateWorkerConsumer has not yet initialized the consumer.

**Example**:
```go
js, _ := jetstream.New(nc)
helper, err := subscription.NewDurableHelperJS(js, subscription.DurableConfig{
    StreamName:      "events",
    ConsumerPrefix:  "worker",
    SubjectTemplate: "events.{{.PartitionID}}",
}, subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    // process message and ACK
    return msg.Ack()
}))
if err != nil { log.Fatal(err) }

mgr, err := parti.NewManager(cfg, js, src, strategy, parti.WithWorkerConsumerUpdater(helper))
if err != nil { log.Fatal(err) }
_ = mgr.Start(context.Background())
```

### WorkerConsumerUpdater Option

Functional option enabling Manager-driven consumer reconciliation.

```go
type WorkerConsumerUpdater interface {
    UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []parti.Partition) error
}

func WithWorkerConsumerUpdater(updater WorkerConsumerUpdater) Option
```

When provided, the Manager invokes `UpdateWorkerConsumer` after initial assignment and on every subsequent change prior to calling `Hooks.OnAssignmentChanged`, allowing hooks to focus on lightweight side effects (metrics, logging) instead of subscription plumbing.

### WorkerConsumer Health & Metrics

The single durable worker consumer exposes health and emits metrics for operational visibility.

```go
type WorkerConsumerHealth struct {
        ConsecutiveFailures int  // current consecutive iterator/handler failures
        Healthy             bool // derived: true if ConsecutiveFailures <= HealthFailureThreshold
}

func (wc *WorkerConsumer) Health() WorkerConsumerHealth
```

Configuration fields in `WorkerConsumerConfig` impacting health & retries:
```go
HealthFailureThreshold int     // tolerated consecutive failures (default 3)
RetryBase              time.Duration
RetryMultiplier        float64
RetryMax               time.Duration
MaxControlRetries      int
MaxSubjects            int    // subject cap (default 500)
AllowWorkerIDChange    bool   // guardrail override (default false)
RetrySeed              int64  // deterministic jitter seed for tests (0 => use global PRNG)
```

Emission Points (names subject to implementation in your MetricsCollector):
- Update lifecycle:
    - `worker_consumer_update_total{result="success|failure|noop"}`
    - `worker_consumer_update_latency_seconds` (histogram)
- Subjects & guardrails:
    - `worker_consumer_subjects_current`
    - `worker_consumer_subject_changes_total{type="add|remove"}`
    - `worker_consumer_guardrail_violations_total{type="max_subjects|workerid_mutation"}`
    - `worker_consumer_subject_threshold_warnings_total`
- Iterator loop resilience:
    - `worker_consumer_iterator_restarts_total{reason="heartbeat|transient"}`
    - `worker_consumer_consecutive_iterator_failures`
    - `worker_consumer_health_status` (1 healthy, 0 unhealthy)
- Control-plane retry & jitter:
    - `worker_consumer_control_retries_total{op="create_update|iterate"}`
    - `worker_consumer_retry_backoff_seconds{op}`

Jitter Backoff:
Uses decorrelated jitter ("Full Jitter") via `jitterBackoff(prev, base, multiplier, cap)` with `math/rand/v2` (non-crypto). A deterministic RNG is created only when `RetrySeed` is non-zero (tests). Production path uses the package-level PRNG.

Example snippet:
```go
wc, _ := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{ /* ... */ }, handler)
// After updates applied:
h := wc.Health()
if !h.Healthy { log.Printf("unhealthy; consecutive failures=%d", h.ConsecutiveFailures) }
```

### Legacy Helper (Per-Partition Subscriptions)

`Helper` and its `UpdateSubscriptions` method remain for scenarios requiring one NATS subscription per partition. New integrations SHOULD prefer `DurableHelper` for better scalability.

---

## Testing Package

Package `github.com/arloliu/parti/testing` provides utilities for testing.

### Embedded NATS

Starts an embedded NATS server for testing.

```go
func StartEmbeddedNATS(t *testing.T, opts ...Option) *nats.Conn
```

**Example**:
```go
func TestMyFeature(t *testing.T) {
    nc := partitesting.StartEmbeddedNATS(t)
    defer nc.Close()

    // Use nc for testing
}
```

---

## Error Types

### Configuration Errors

```go
var (
    ErrInvalidConfig             = errors.New("invalid configuration")
    ErrNATSConnectionRequired    = errors.New("NATS connection required")
    ErrPartitionSourceRequired   = errors.New("partition source required")
    ErrAssignmentStrategyRequired = errors.New("assignment strategy required")
)
```

### Operational Errors

```go
var (
    ErrStableIDExhausted       = errors.New("all stable IDs in range are claimed")
    ErrNATSConnectionLost      = errors.New("NATS connection lost")
    ErrAssignmentVersionMismatch = errors.New("assignment version mismatch")
    ErrElectionFailed          = errors.New("leader election failed")
    ErrDegradedMode            = errors.New("manager in degraded mode") // NEW
    ErrDegradedAlert           = errors.New("degraded mode alert")      // NEW
)
```

**New Degraded Mode Errors**:

#### ErrDegradedMode

Returned by operations that cannot proceed while the manager is in degraded mode.

```go
assignment, err := mgr.CurrentAssignment()
if errors.Is(err, parti.ErrDegradedMode) {
    log.Warn("Using stale assignment from cache")
}
```

#### ErrDegradedAlert

Wrapped error returned by `OnDegradedAlert` hook when alerting about prolonged degraded mode.
Contains alert level and duration information.

```go
hooks := &parti.Hooks{
    OnDegradedAlert: func(ctx context.Context, level string, duration time.Duration) error {
        log.Printf("DEGRADED ALERT [%s]: Cache age %v", level, duration)
        return nil
    },
}
```

### Error Checking

Use `errors.Is()` for error checking:

```go
if errors.Is(err, parti.ErrStableIDExhausted) {
    log.Fatal("Increase WorkerIDMax in configuration")
}
```

---

## Functional Options

### WithStrategy

Sets the assignment strategy (deprecated - use positional parameter).

```go
func WithStrategy(strategy AssignmentStrategy) Option
```

**Example**:
```go
strategy := strategy.NewConsistentHash()
js, _ := jetstream.New(nc)
mgr := parti.NewManager(cfg, js, src, strategy)
```

---

### WithElectionAgent

Sets a custom election agent.

```go
func WithElectionAgent(agent ElectionAgent) Option
```

**Example**:
```go
agent := NewConsulElectionAgent(consulClient)
js, _ := jetstream.New(nc)
mgr := parti.NewManager(cfg, js, src, strategy,
    parti.WithElectionAgent(agent),
)
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
mgr := parti.NewManager(cfg, js, src, strategy,
    parti.WithHooks(hooks),
)
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
mgr := parti.NewManager(cfg, js, src, strategy,
    parti.WithMetrics(collector),
)
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
mgr := parti.NewManager(cfg, js, src, strategy,
    parti.WithLogger(logger.Sugar()),
)
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

- Minimum: NATS Server 2.9.0+
- Recommended: NATS Server 2.10.0+
- JetStream: Required
- KV Store: Required

### Go Version

- Minimum: Go 1.24+
- Recommended: Latest stable Go version
