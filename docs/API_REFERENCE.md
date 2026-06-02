# Parti API Reference

**Version**: 2.0.0
**Last Updated**: 2026-05-16
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

Runs the manager's synchronous sanity-check phase.

```go
func (m *Manager) Start(ctx context.Context) error
```

> **Breaking change:** the return contract changed in the upcoming
> release — `Start` no longer blocks until `StateStable`. If you are
> upgrading from v2.4.x or earlier and your code reads
> `CurrentAssignment()` immediately after `Start`, see
> [Migrating: Manager.Start returns at StateWaitingAssignment](MIGRATING_MANAGER_START.md).

Start returns once the worker has claimed a stable ID, ensured KV
buckets exist, completed the election round, and wired the heartbeat
publisher + (if leader) calculator. The state observed after `Start`
returns may be `StateWaitingAssignment`, `StateStable`, or any
calculator-driven active state (`StateScaling`, `StateRebalancing`,
`StateEmergency`) depending on race — the background runner or
calculator monitor may have advanced state before the caller observes
it. The initial assignment fetch and apply run in a background
goroutine. Callers that need to know the manager is ready to process
work should call `WaitState(StateStable, timeout)`.

A soft watchdog enters `StateDegraded` (reason: `startup-timeout`)
once if `StartupTimeout` elapses from `Start` invocation without
reaching `Stable`, providing the readiness-probe rotation signal. The
watchdog is decoupled from the runner, so it fires even when the
runner is blocked inside an unbounded `handoffCoordinator.Apply`
call. Once monitors start, `monitorNATSConnection` drives
`attemptRecoveryFromDegraded` on its `ExitThreshold` tick even
without a prior disconnect — but if the runner is still blocked
before monitors start, startup-timeout-degraded is a probe-rotation
signal until the runner returns or the pod is restarted.

**Apply boundedness:** `handoffCoordinator.Apply(m.ctx, ...)` is
unbounded per attempt (identical to pre-refactor `Start`). A stuck
consumer updater can block the runner inside one apply attempt until
`Stop`.

**Parameters**:
- `ctx`: Context for synchronous-phase cancellation and timeout

**Returns**:
- `error`: Synchronous-phase startup error or nil on success.
  Background-phase failures fall through to monitor startup and are
  recovered by existing watchers / `scheduleApplyRetry` / 
  `attemptRecoveryFromDegraded`.

**Synchronous Sequence (returns after this completes)**:
1. Claims stable worker ID from NATS KV
2. Starts partition source
3. Ensures KV buckets (election, heartbeat, assignment)
4. Participates in leader election
5. Starts heartbeat publisher
6. If leader: starts assignment calculator
7. Transitions to `StateWaitingAssignment`
8. Spawns the background runner (initial-assignment fetch + apply +
   CAS to `Stable`) and the soft watchdog

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

#### SetCapability

Sets or clears a capability bit in the Manager's runtime capability bitmask.

```go
func (m *Manager) SetCapability(capBit uint32, active bool)
```

A bit should be set only when the corresponding safety mechanism is actually wired and active, not merely configured. The Manager uses OR-only semantics for reporter-sourced bits: `CapabilityReporter.Capabilities()` results are ORed in and never cleared by the reporter path. Other components (e.g., the heartbeat publisher) may call `SetCapability` to clear bits on teardown.

**Parameters**:
- `capBit`: Capability bit to set or clear (e.g., `types.CapAckV1`)
- `active`: `true` to set the bit, `false` to clear it

**Thread Safety**: Safe for concurrent use (atomic CAS operations).

**Example**:
```go
// Manager sets CapAckV1 after the heartbeat publisher starts.
// Application code rarely needs to call SetCapability directly.
mgr.SetCapability(types.CapAckV1, true)
```

---

#### Capabilities

Returns the current capability bitmask as an atomic snapshot.

```go
func (m *Manager) Capabilities() uint32
```

The heartbeat publisher calls this on every heartbeat to embed the current runtime wire-up state. Do not cache the result — always call this method so the heartbeat reflects live state.

**Returns**:
- `uint32`: Current capability bitmask (OR of active `types.CapXxx` constants)

**Thread Safety**: Safe for concurrent use (atomic load).

**Example**:
```go
caps := mgr.Capabilities()
if caps&types.CapTwoPhaseHandoff != 0 {
    log.Println("two-phase handoff is active")
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
    Start(ctx context.Context) error
    List(ctx context.Context) ([]Partition, error)
    Stop(ctx context.Context) error
}
```

**Methods**:

#### List

Returns all available partitions.

**Parameters**:
- `ctx`: Context for cancellation and timeout

**Returns**:
- `[]Partition`: List of discovered partitions
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

func (s *DBPartitionSource) Start(_ context.Context) error { return nil }
func (s *DBPartitionSource) Stop(_ context.Context) error  { return nil }

func (s *DBPartitionSource) List(ctx context.Context) ([]Partition, error) {
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

### RevisionedPartitionSource

Optional extension interface for partition sources that track a KV revision number.

```go
type RevisionedPartitionSource interface {
    PartitionSource
    Snapshot(ctx context.Context) (partitions []Partition, revision uint64, known bool, err error)
}
```

Sources that maintain revision history (e.g., `source.NatsKV`) implement this interface. The calculator type-asserts for this interface and falls back to `List()` with `SourceRevisionKnown=false` when the assertion fails. The leader's audit uses `SourceRevisionKnown` to decide whether strict source-revision checks apply.

**Methods**:

#### Snapshot

Returns the current partition list along with the associated KV revision.

**Parameters**:
- `ctx`: Context for cancellation and timeout

**Returns**:
- `partitions`: Current partition list (nil or empty if key was deleted)
- `revision`: Last observed KV revision (0 if `known=false`)
- `known`: `true` once any KV event has been observed, including delete/purge. `false` means the source has never been written.
- `error`: Error if the snapshot could not be returned

**Semantics**: The `known` flag distinguishes a never-written source (`known=false, revision=0`) from a written-then-deleted source (`known=true, revision=deleteRev, empty partitions`). Downstream audit logic uses this distinction to determine whether strict source-revision checks apply.

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

### CapabilityReporter

Optional interface a `WorkerConsumerUpdater` MAY implement to report runtime capabilities back to the Manager.

```go
type CapabilityReporter interface {
    Capabilities() uint32
}
```

When the registered updater (or any child of a composite updater) satisfies this interface, the Manager queries `Capabilities()` after each handoff apply attempt and ORs the returned bits into its capability bitmask via `SetCapability`.

**Contract for implementors**:
- **Concurrent-safe**: `Capabilities()` may be called from the manager-apply goroutine concurrently with the updater's own `UpdateWorkerConsumer` calls.
- **Non-blocking**: invoked on every apply attempt — must not perform I/O or acquire long-held locks. An atomic load is the expected implementation.
- **Monotonic for runtime-wire-up bits**: once a capability has been successfully wired (e.g., a handler wrapped with the processing gate), the corresponding bit MUST remain set for the updater's lifetime, even if a subsequent per-subject create fails. The bit means "at least one wired component", not "all components currently wired".

**Manager integration**: the reporter pathway is OR-only — `Capabilities()` results are ORed into the Manager bitmask and never cleared via this path. Returning `0` is always safe.

**Example**:
```go
type MyConsumer struct {
    gateWired atomic.Bool
}

// Implements CapabilityReporter.
func (c *MyConsumer) Capabilities() uint32 {
    if c.gateWired.Load() {
        return types.CapProcessingGate
    }
    return 0
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

Known reasons and operator intent:

| Reason | Class | Operator action |
|---|---|---|
| `NATS connection down` | ride-through if reconnecting | Keep readiness degraded until NATS is stable; rotate only if the connection is closed or the outage exceeds policy. |
| `kv-unavailable` | connected but KV quorum unavailable | Keep readiness degraded; rotation is acceptable if the outage exceeds SLO. |
| `heartbeat-enumeration-stall` | leader's heartbeat enumeration (Keys scan) is timing out while single-key ops still succeed | Keep readiness degraded; the leader cannot see worker membership, so rotate/restart the leader if the stall is sustained. |
| `KV error threshold exceeded` | Parti-owned coordination data missing/lost | Restart or rotate workers after confirming bucket loss. |
| `bucket-recreated:<bucket>` | ambiguous Parti-owned data loss | Restart or rotate workers; inspect JetStream storage before trusting the recreated bucket. |
| `startup-timeout` | startup apply/wait did not reach Stable in budget | Readiness rotation unless the runner recovers before the pod is replaced. |
| `startup-background-panic` | a background startup goroutine panicked | Rotate the worker; inspect logs for the panic. Monitors are still started so the worker may self-recover. |
| `assignment-watcher-exhausted` | assignment watcher retry envelope exhausted | Restart or rotate the worker; inspect the assignment bucket and NATS logs. |
| `stream-missing-recovery-exhausted` | dynamic consumer stream missing and no app hook recovered it | Recover the stream or rotate workers according to application ownership. |
| `source-unavailable:<bucket>` | caller-owned source bucket unavailable | Caller/operator recovers the source bucket; Parti does not recreate it. |

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
    WorkerIDTTL    time.Duration // TTL for ID claims (default: 75s; must be >= HeartbeatTTL)

    // Heartbeat Configuration
    HeartbeatInterval time.Duration // Heartbeat publish interval (default: 5s)
    HeartbeatTTL      time.Duration // Heartbeat validity duration (default: 15s)

    // Stabilization Windows
    ColdStartWindow       time.Duration // Window for cold start (default: 30s)
    PlannedScaleWindow    time.Duration // Window for planned scale (default: 10s)
    EmergencyGracePeriod  time.Duration // Grace period before emergency (default: 0 = auto = 1.5 * HeartbeatInterval)
    RestartDetectionRatio float64       // NO-OP (orphaned, pending removal); historically classified restarts (default: 0.5)

    // Timeouts
    OperationTimeout time.Duration // Timeout for KV operations (default: 10s)
    ElectionTimeout  time.Duration // Timeout for leader election (default: 10s)
    StartupTimeout   time.Duration // Timeout for manager startup (default: 60s)
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

#### DefaultConfig

Returns a `Config` with every field populated at its default value. Use this when you want to start from defaults and tweak a few fields; it is equivalent to calling `SetDefaults` on a zero-valued `Config`.

```go
func DefaultConfig() Config
```

**Example**:
```go
cfg := parti.DefaultConfig()
cfg.WorkerIDPrefix = "orders"
cfg.WorkerIDMax = 99
```

`DefaultConfig` panics only if Parti's own struct tags are malformed (a library bug, not a runtime condition).

#### SetDefaults

Fills in missing configuration values with defaults. Prefer `DefaultConfig()` for new configurations; use `SetDefaults` when you already have a partially-populated `Config` (e.g., loaded from YAML) and want to backfill zero-valued fields.

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
    StateDegraded         // Degraded mode (fault-specific readiness signal)
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
- `StateDegraded`: **Degraded mode** - Fresh coordination, source, or consumer state is unreliable; use `OnDegraded` reason to choose ride-through, recovery, rotation, or operator-owned response
- `StateShutdown`: Graceful shutdown in progress

**Start return point:** `Manager.Start(ctx)` returns once the
synchronous sanity-check phase completes. The state observed on
return may be `StateWaitingAssignment`, `StateStable`, or any
calculator-driven active state — the background runner or calculator
monitor may have advanced state before the caller observes it. The
transition to `StateStable` (when not already there) happens in a
background goroutine after the initial assignment is fetched and
applied. To block until the manager is ready to process work, use:

```go
if err := <-mgr.WaitState(parti.StateStable, 30*time.Second); err != nil {
    log.Fatalf("manager did not reach StateStable: %v", err)
}
```

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
func (p Partition) SubjectKey() string            // Keys joined with '.' (e.g., "orders.0")
func (p Partition) ID() string                    // Keys joined with '-' (e.g., "orders-0")
func (p Partition) HashID() uint64                // Stable 64-bit XXH3 hash of Keys (0 if empty)
func (p Partition) HashIDSeed(seed uint64) uint64 // HashID with an explicit seed (seed==0 → HashID)
func (p Partition) Compare(q Partition) int       // Lexicographic key comparison: -1,0,+1
func (p Partition) CanonicalID() string           // Length-prefixed collision-safe key encoding
```

Use `SubjectKey()` for JetStream subject templating and `FilterSubjects` construction.
Use `ID()` for durable names (e.g., `<ConsumerPrefix>-<ID()>`).
Use `HashID()` / `HashIDSeed()` in custom assignment strategies or caches that need a stable, allocation-free partition hash. Chained XXH3 hashing avoids key-boundary ambiguity without concatenation.
Use `Compare()` as a stable, allocation-free tie-breaker (keys only, weight ignored) in ordering.
Use `CanonicalID()` as a stable map key or for set-membership tests — it is fully length-driven so any character (including `'/'`, `'-'`, `':'`) may appear in keys without ambiguity. Format: `"<len>:<key>/<len>:<key>/..."` (empty string when `Keys` is empty). Example: `Partition{Keys: []string{"a-b", "c"}}.CanonicalID()` → `"3:a-b/1:c"`.

---

### Assignment

Contains the current partition assignment for a worker.

```go
type Assignment struct {
    Version    int64       // Monotonically increasing version
    Lifecycle  string      // Scaling reason that drove this assignment (see below)
    Partitions []Partition // Assigned partitions
}
```

**Fields**:
- `Version`: Monotonically increasing assignment version
- `Lifecycle`: Reason the leader recomputed this assignment. One of:
  - `"cold_start"` — initial startup or a mass-failure event (uses `ColdStartWindow` to stabilize). Note: `RestartDetectionRatio` no longer participates in this classification — it is an orphaned no-op pending removal.
  - `"planned_scale"` — rolling update or planned membership change within the planned-scale window
  - `"emergency"` — worker failure detected, immediate rebalance
  - `"restart"` — previously-known worker returned after a transient absence
  - `"stable"` — no scaling event in progress (steady state)
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

### Capabilities

Bitmask constants advertising which safety mechanisms are active at runtime.

```go
const (
    CapAckV1          uint32 = 1 << 0 // worker publishes apply receipts
    CapTwoPhaseHandoff uint32 = 1 << 1 // manager runs two-phase handoff coordinator
    CapProcessingGate  uint32 = 1 << 2 // consumer handlers wrapped with processing gate
)
```

Defined in package `types` (`github.com/arloliu/parti/v2/types`). Reference these constants via the `types` import — they are not re-exported through `parti`.

A bit is set **only when the mechanism is actually wired and active**, not merely configured:

- `CapAckV1`: set by the heartbeat publisher after it successfully starts. Indicates the worker emits v1 JSON heartbeats with `AppliedVersion`, `AppliedDigest`, and `AppliedSourceRevision` fields.
- `CapTwoPhaseHandoff`: set during `Manager.Start` when `Config.EnableTwoPhaseHandoff` is `true` and the handoff coordinator initialises successfully.
- `CapProcessingGate`: set by the `Dynamic` consumer (via `CapabilityReporter`) after at least one partition handler has been wrapped with the processing gate. The leader's audit uses this bit to determine whether reassignment escalation via `audit_repair` is safe for that worker.

The current bitmask is embedded in every v1 heartbeat. The leader reads peer bitmasks from heartbeats during assignment audits.

**Example** — reading caps from a peer heartbeat:
```go
hb, err := types.DecodeHeartbeat(entry.Value())
if err != nil { ... }
if hb.Capabilities&types.CapProcessingGate != 0 {
    // Safe to use audit_repair for this worker.
}
```

---

### Heartbeat

v1 heartbeat payload published to NATS KV by every active worker.

```go
type Heartbeat struct {
    WorkerID              string    `json:"worker_id"`
    SchemaVersion         uint8     `json:"schema_version,omitempty"`
    Capabilities          uint32    `json:"capabilities,omitempty"`
    LeaderRevision        uint64    `json:"leader_revision,omitempty"`
    AppliedVersion        int64     `json:"applied_version,omitempty"`
    AppliedDigest         uint64    `json:"applied_digest,omitempty"`
    AppliedSourceRevision uint64    `json:"applied_source_revision,omitempty"`
    AppliedSourceRevKnown bool      `json:"applied_source_revision_known,omitempty"`
    AppliedAt             time.Time `json:"applied_at"`
    Timestamp             time.Time `json:"timestamp"`
}
```

Defined in package `types`.

**Wire format**:
- **v1** (`SchemaVersion >= 1`): JSON object beginning with `'{'`. All fields may be present.
- **Legacy** (`SchemaVersion == 0`): RFC3339 or RFC3339Nano timestamp string. Pre-v1 workers publish only a timestamp; all fields beyond `Timestamp` are zero. Callers must treat legacy workers as "alive but not ack-capable" — do not escalate reassignment via the audit path for them.

**Key fields**:
- `SchemaVersion`: `0` = legacy timestamp-only worker; `1` = v1 JSON worker with full capability advertising.
- `Capabilities`: OR of `CapXxx` bits active at publish time (0 for legacy workers).
- `AppliedVersion` / `AppliedDigest` / `AppliedSourceRevision`: Apply-receipt fields, populated when `CapAckV1` is set. Used by the leader audit to detect stuck workers.
- `AppliedSourceRevKnown`: `false` when the source does not implement `RevisionedPartitionSource`.

**Decoding**:

```go
func DecodeHeartbeat(b []byte) (Heartbeat, error)
```

Accepts both v1 JSON and legacy timestamp strings. The two formats are distinguished by the first byte (`'{'` = JSON). Malformed payloads return an error — silent degradation to an empty `Heartbeat` is intentionally rejected. Returns a zero-capability `Heartbeat` with `SchemaVersion=0` for valid legacy payloads.

**Example**:
```go
entry, err := kv.Get(ctx, "worker-hb.worker-0")
if err != nil { ... }
hb, err := types.DecodeHeartbeat(entry.Value())
if err != nil { ... }
log.Printf("worker %s caps=0x%x appliedAt=%s", hb.WorkerID, hb.Capabilities, hb.AppliedAt)
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

#### WithWeightedHashSeed
Sets a custom hash seed for the underlying consistent hash ring. Use this to de-correlate hash distributions across clusters that share worker IDs or partition keys.

#### WithOverloadThreshold
Sets the maximum allowed load variance (default: 1.2 or 120%).

#### WithExtremeThreshold
Sets the multiplier used to classify "extreme" (heavy) partitions for special handling (default: 20.0).

#### WithDefaultWeight
Default weight applied when a partition reports `Weight == 0`. Lets you keep `Partition.Weight` unset for the common case while still differentiating weighted partitions (default: 1; values < 1 are clamped to 1).

#### WithMinPartitionCount
Sets the minimum partition count factor — the minimum percentage of the average partition count that a worker must accept before load shedding kicks in (default: 0.3, i.e., 30% of average).

#### WithWeightedLogger
Sets the logger used for configuration warnings and debug diagnostics inside the strategy. Defaults to a no-op logger.

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

#### Modify

Atomically transforms the partition list using a CAS-retry loop. Safe for concurrent callers.

```go
func (s *NatsKV) Modify(ctx context.Context, fn func([]types.Partition) []types.Partition) error
```

`fn` receives a fresh snapshot read directly from KV (never the local cache) on every attempt. Because of CAS retries `fn` may be called more than once — it must be deterministic and side-effect-free. On `ErrKeyNotFound` (key never written), `fn` receives an empty slice.

**Parameters**:
- `ctx`: Context for the operation
- `fn`: Transform function (may be called multiple times; must be side-effect-free)

**Returns**:
- `error`: KV error, validation error from the transform result, or `source.ErrUpdateRetryExhausted` if all CAS attempts fail

**Example**:
```go
// Double the weight of all partitions.
err := src.Modify(ctx, func(current []types.Partition) []types.Partition {
    for i := range current {
        current[i].Weight *= 2
    }
    return current
})
```

---

#### AddPartitions

Adds one or more partitions to the source without disturbing concurrent mutations.

```go
func (s *NatsKV) AddPartitions(ctx context.Context, partitions ...types.Partition) error
```

Duplicate partitions (matched by `CanonicalID()`) are silently ignored — calling `AddPartitions` twice with the same partition is a no-op. Internally implemented via `Modify`, so concurrent callers are safe.

**Parameters**:
- `ctx`: Context for the operation
- `partitions`: Partitions to add (validated before any KV round-trip)

**Returns**:
- `error`: Validation error, or `source.ErrUpdateRetryExhausted` if the CAS budget is exhausted

**Example**:
```go
err := src.AddPartitions(ctx,
    types.Partition{Keys: []string{"topic", "3"}, Weight: 100},
    types.Partition{Keys: []string{"topic", "4"}, Weight: 100},
)
```

---

#### RemovePartitions

Removes one or more partitions from the source, matched by `CanonicalID()`.

```go
func (s *NatsKV) RemovePartitions(ctx context.Context, partitions ...types.Partition) error
```

Partitions not found in the current list are silently ignored. Concurrent mutations from other writers are preserved. Internally implemented via `Modify`.

**Parameters**:
- `ctx`: Context for the operation
- `partitions`: Partitions to remove (validated before any KV round-trip)

**Returns**:
- `error`: Validation error, or `source.ErrUpdateRetryExhausted` if the CAS budget is exhausted

**Example**:
```go
err := src.RemovePartitions(ctx,
    types.Partition{Keys: []string{"topic", "3"}},
)
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
| `WithInstanceID(id)`                        | Broadcast      | Set unique instance identifier       |
| `WithProcessingGate(cfg)`                   | Dynamic        | Enable processing gate               |
| `WithDrainOnRemove(enabled, timeout)`       | Dynamic        | Drain messages on partition removal  |

---

### ResolverConfig

Configures the ownership resolver used when `ProcessingGate` is enabled on a `Dynamic` consumer.

```go
type ResolverConfig struct {
    OwnershipResolver   types.OwnershipResolver // Custom resolver (optional; overrides auto-creation)
    HandoffBucketName   string                  // KV bucket for handoff claims (default: "parti-handoff")
    HandoffClaimsPrefix string                  // Key prefix for claims (default: "claims/")
    BatchWindow         time.Duration           // Coalescing window for claim updates (default: 5ms)
    BatchMaxItems       int                     // Max updates per batch (default: 1024)
    ReconcileInterval   time.Duration           // Periodic cache reconcile cadence (default: 30s)
}
```

**Key field — ReconcileInterval**:

The cadence at which the auto-created claim-based resolver re-lists the handoff bucket and reconciles its in-memory cache against KV. This is the recovery mechanism for silent watcher stalls: the NATS JetStream KV watcher does NOT surface a NATS server restart as a channel close — only an explicit stop, connection close, or subscription teardown does. After such a stall the cache stays stale for at most one reconcile period.

Choose a value shorter than `5 × Config.HeartbeatTTL` (the leader's audit grace period). With the default `HeartbeatTTL=15s` the audit grace period is 75s and the default `ReconcileInterval=30s` is comfortably inside it. If you have tuned `HeartbeatTTL` below ~6s, set `ReconcileInterval` to `HeartbeatTTL` or `HeartbeatTTL/2`.

Manager emits a one-shot `WARN` log at `Start` when `EnableTwoPhaseHandoff=true` and `5 × HeartbeatTTL < 30s`, reminding operators to lower `ReconcileInterval` accordingly.

Zero uses the default (30s). Negative values are rejected at startup. The field is ignored when `OwnershipResolver` is non-nil.

**Example**:
```go
cfg := consumer.DynamicConfig{
    // ...
    Resolver: consumer.ResolverConfig{
        HandoffBucketName: "parti-handoff",
        ReconcileInterval: 20 * time.Second, // < 5 × HeartbeatTTL when TTL is tuned low
    },
}
```

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
