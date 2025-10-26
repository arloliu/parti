# Parti Library Design Discussion 🎉

**Date**: October 25, 2025
**Library Name**: `parti` (partition + party!)
**Purpose**: Design a generic Go library for NATS-based work partitioning to help migrate from Kafka

**Repository**: `github.com/arloliu/parti`

---

## Decision Summary

✅ **Library Name**: `parti`
✅ **Why**: Short (5 chars), easy to type, clever pun, clear purpose (partition)
✅ **Approach**: Standalone reusable library with generic terminology

---

## Current State (Kafka-based)

Your existing system:
- **Data Collector**: Publishes completion notifications to Kafka topics
- **Defender**: Consumes from Kafka partitions (partition assignment via Kafka consumer group)
- **Partition Assignment**: Kafka automatically handles it
- **No explicit leader election**: Kafka consumer group protocol manages coordination

## Target State (NATS-based with Parti)

With Parti library:
- **Data Collector**: Publishes to NATS subjects (`dc.{tool}.{chamber}.completed`)
- **Defender**: Uses Parti to manage worker lifecycle, subscribes based on assignments
- **Assignment Controller**: Parti's leader calculates and distributes work
- **Stable IDs**: Parti manages worker ID claiming
- **Leader Election**: Parti handles election and leadership
- **Heartbeats**: Parti tracks worker health

---

## Generic Terminology (Not Domain-Specific)

To make Parti reusable across different use cases:

| Your Domain | Generic Term in Parti | Description |
|-------------|----------------------|-------------|
| Defender | **Worker** | Consumer/processor instance |
| Chamber | **Partition** | Work unit with hierarchical keys |
| Tool + Chamber | **Partition Keys** | Multi-level partition identifiers |
| Data Collector | **Publisher** | Produces work notifications |
| Assignment | **Assignment** | Mapping of partitions to workers |

---

## Library Design Questions

### 1. **Parti Package Structure**

Proposed structure:
```
github.com/yourorg/parti
├── go.mod
├── README.md
├── parti/                 # Core package
│   ├── manager.go         # Main Manager type
│   ├── config.go          # Configuration
│   ├── hooks.go           # Callback interfaces
│   ├── partition.go       # Partition type
│   └── worker.go          # Worker identity
│
├── assignment/            # Assignment strategies
│   ├── controller.go      # Leader's assignment logic
│   ├── consistent_hash.go # Consistent hashing
│   └── strategy.go        # Strategy interface
│
├── election/              # Leader election
│   ├── elector.go         # Election via NATS KV
│   └── lease.go           # Leadership lease management
│
├── heartbeat/             # Health monitoring
│   ├── publisher.go       # Publish worker heartbeats
│   └── monitor.go         # Monitor other workers (leader)
│
├── subscription/          # NATS subscription management
│   ├── manager.go         # Dynamic subscribe/unsubscribe
│   └── handler.go         # Message handler abstraction
│
└── examples/
    ├── simple/            # Basic usage
    ├── weighted/          # Your tool/chamber use case
    └── custom/            # Custom partition discovery
```

**Questions:**
- Does this structure make sense?
- Should we combine some packages (e.g., `election` + `heartbeat`)?
- Any missing packages?

**Your Answer:**
```
1. you create the same sub package parti in the parti lib, how about to put factory function in the root and specific details in sub package and give a good name?
2. the election package use extermal election-agent(https://github.com/arloliu/election-agent), so it's good to be a package.
3. let's start simple, add packages when really needed.

```

---

### 2. **Core API Design**

```go
package parti

import (
    "context"
    "time"
    "github.com/nats-io/nats.go"
)

// Partition represents a unit of work with hierarchical keys
type Partition struct {
    Keys   []string  // e.g., ["tool001", "chamber1"]
    Weight int64     // Optional weight for load balancing
}

// Subject generates NATS subject for this partition
func (p Partition) Subject(prefix string) string {
    // Returns: "prefix.tool001.chamber1.completed"
}

// LifecycleState represents cluster lifecycle state
type LifecycleState string

const (
    LifecycleColdStart     LifecycleState = "cold_start"      // Initial deployment or restart
    LifecyclePostColdStart LifecycleState = "post_cold_start" // First assignment after cold start
    LifecycleStable        LifecycleState = "stable"          // Normal operation
)

// State represents worker state in the state machine
type State string

const (
    StateInit              State = "INIT"               // Initial state
    StateClaimingID        State = "CLAIMING_ID"        // Claiming stable ID
    StateElection          State = "ELECTION"           // Participating in leader election
    StateWaitingAssignment State = "WAITING_ASSIGNMENT" // Waiting for assignment
    StateStable            State = "STABLE"             // Normal operation
    StateScaling           State = "SCALING"            // Stabilization window
    StateRebalancing       State = "REBALANCING"        // Leader calculating assignments
    StateEmergency         State = "EMERGENCY"          // Emergency rebalancing
    StateShutdown          State = "SHUTDOWN"           // Graceful shutdown
)

// Assignment represents partitions assigned to a worker with metadata
type Assignment struct {
    Version    int64                    // Version number for consistency tracking
    Lifecycle  LifecycleState           // Cluster lifecycle state
    Partitions []Partition
    Timestamp  time.Time
    Metadata   map[string]interface{}   // Additional metadata (worker count, etc.)
}

// Config holds serializable configuration (can be loaded from JSON/YAML)
type Config struct {
    // Worker identity
    WorkerIDPrefix string `json:"worker_id_prefix" yaml:"worker_id_prefix"` // e.g., "defender", "worker"
    WorkerIDMin    int    `json:"worker_id_min" yaml:"worker_id_min"`       // e.g., 0
    WorkerIDMax    int    `json:"worker_id_max" yaml:"worker_id_max"`       // e.g., 199
    WorkerIDTTL    string `json:"worker_id_ttl" yaml:"worker_id_ttl"`       // e.g., "30s" (parsed to time.Duration)

    // Timing
    HeartbeatInterval string `json:"heartbeat_interval" yaml:"heartbeat_interval"` // e.g., "2s"
    HeartbeatTTL      string `json:"heartbeat_ttl" yaml:"heartbeat_ttl"`           // e.g., "6s"

    // State machine timing
    ColdStartWindow       string  `json:"cold_start_window" yaml:"cold_start_window"`             // e.g., "30s"
    PlannedScaleWindow    string  `json:"planned_scale_window" yaml:"planned_scale_window"`       // e.g., "10s"
    RestartDetectionRatio float64 `json:"restart_detection_ratio" yaml:"restart_detection_ratio"` // e.g., 0.5

    // Context timeout control
    OperationTimeout string `json:"operation_timeout" yaml:"operation_timeout"` // e.g., "10s" - Default timeout for Manager operations (NATS KV, partition discovery)
    ElectionTimeout  string `json:"election_timeout" yaml:"election_timeout"`   // e.g., "5s" - Timeout for ElectionAgent operations
    StartupTimeout   string `json:"startup_timeout" yaml:"startup_timeout"`     // e.g., "30s" - Maximum time for Manager.Start() to complete
    ShutdownTimeout  string `json:"shutdown_timeout" yaml:"shutdown_timeout"`   // e.g., "10s" - Maximum time for Manager.Stop() to complete

    // Assignment rebalancing policy
    Assignment AssignmentConfig `json:"assignment" yaml:"assignment"`
}

// AssignmentConfig controls rebalancing behavior
type AssignmentConfig struct {
    MinRebalanceThreshold float64 `json:"min_rebalance_threshold" yaml:"min_rebalance_threshold"` // e.g., 0.15
    RebalanceCooldown     string  `json:"rebalance_cooldown" yaml:"rebalance_cooldown"`           // e.g., "10s"
}

// SetDefaults sets default values for all config fields
func (c *Config) SetDefaults() {
    if c.WorkerIDPrefix == "" {
        c.WorkerIDPrefix = "worker"
    }
    if c.WorkerIDMax == 0 {
        c.WorkerIDMax = 99
    }
    if c.WorkerIDTTL == "" {
        c.WorkerIDTTL = "30s"
    }
    if c.HeartbeatInterval == "" {
        c.HeartbeatInterval = "2s"
    }
    if c.HeartbeatTTL == "" {
        c.HeartbeatTTL = "6s"
    }
    if c.ColdStartWindow == "" {
        c.ColdStartWindow = "30s"
    }
    if c.PlannedScaleWindow == "" {
        c.PlannedScaleWindow = "10s"
    }
    if c.RestartDetectionRatio == 0 {
        c.RestartDetectionRatio = 0.5
    }
    if c.OperationTimeout == "" {
        c.OperationTimeout = "10s"
    }
    if c.ElectionTimeout == "" {
        c.ElectionTimeout = "5s"
    }
    if c.StartupTimeout == "" {
        c.StartupTimeout = "30s"
    }
    if c.ShutdownTimeout == "" {
        c.ShutdownTimeout = "10s"
    }
    if c.Assignment.MinRebalanceThreshold == 0 {
        c.Assignment.MinRebalanceThreshold = 0.15
    }
    if c.Assignment.RebalanceCooldown == "" {
        c.Assignment.RebalanceCooldown = "10s"
    }
}

// Validate checks if configuration is valid
func (c *Config) Validate() error {
    if c.WorkerIDPrefix == "" {
        return errors.New("worker_id_prefix is required")
    }
    if c.WorkerIDMin < 0 {
        return errors.New("worker_id_min must be >= 0")
    }
    if c.WorkerIDMax <= c.WorkerIDMin {
        return errors.New("worker_id_max must be > worker_id_min")
    }
    // ... more validation
    return nil
}

// Option is a functional option for Manager
type Option func(*managerOptions) error

// managerOptions holds optional runtime dependencies (not serializable)
type managerOptions struct {
    strategy      AssignmentStrategy
    hooks         *Hooks
    metrics       MetricsCollector
    logger        Logger
    electionAgent ElectionAgent
}

// WithStrategy sets the assignment strategy
// Default: WeightedConsistentHashStrategy with 150 virtual nodes
func WithStrategy(strategy AssignmentStrategy) Option {
    return func(o *managerOptions) error {
        if strategy == nil {
            return errors.New("strategy cannot be nil")
        }
        o.strategy = strategy
        return nil
    }
}

// WithHooks sets the application callbacks
// Default: nil (no hooks)
func WithHooks(hooks *Hooks) Option {
    return func(o *managerOptions) error {
        o.hooks = hooks
        return nil
    }
}

// WithMetrics sets the metrics collector
// Default: nopMetricsCollector (no-op implementation)
func WithMetrics(metrics MetricsCollector) Option {
    return func(o *managerOptions) error {
        if metrics == nil {
            return errors.New("metrics cannot be nil (use nopMetricsCollector for no metrics)")
        }
        o.metrics = metrics
        return nil
    }
}

// WithLogger sets the logger
// Default: nopLogger (no-op implementation)
func WithLogger(logger Logger) Option {
    return func(o *managerOptions) error {
        if logger == nil {
            return errors.New("logger cannot be nil (use nopLogger for no logging)")
        }
        o.logger = logger
        return nil
    }
}

// WithElectionAgent sets the election agent client
// Default: nil (uses NATS KV-based leader election)
func WithElectionAgent(agent ElectionAgent) Option {
    return func(o *managerOptions) error {
        if agent == nil {
            return errors.New("election agent cannot be nil")
        }
        o.electionAgent = agent
        return nil
    }
}

// ElectionAgent interface for leader election (external service)
// Implementation: github.com/arloliu/election-agent
type ElectionAgent interface {
    // RequestLeadership requests leadership with a given lease duration
    RequestLeadership(ctx context.Context, workerID string, leaseDuration time.Duration) (bool, error)

    // RenewLeadership renews the leadership lease
    RenewLeadership(ctx context.Context) error

    // ReleaseLeadership releases the leadership
    ReleaseLeadership(ctx context.Context) error

    // IsLeader checks if this worker is the current leader
    IsLeader(ctx context.Context) bool
}

// Hooks define application callbacks
type Hooks struct {
    // Called when worker's partitions change
    OnAssignmentChanged func(ctx context.Context, added, removed []Partition) error

    // Called when worker becomes/loses leader
    OnLeadershipChanged func(ctx context.Context, isLeader bool) error

    // Called when worker ID is claimed
    OnWorkerIDClaimed func(ctx context.Context, workerID string) error

    // Optional: state transitions
    OnStateChanged func(ctx context.Context, from, to State) error
}

// Logger interface for structured logging (zap-style)
type Logger interface {
    Debug(msg string, keysAndValues ...any)
    Info(msg string, keysAndValues ...any)
    Warn(msg string, keysAndValues ...any)
    Error(msg string, keysAndValues ...any)
    Fatal(msg string, keysAndValues ...any)
}

// MetricsCollector interface for observability
type MetricsCollector interface {
    // State metrics
    RecordStateTransition(from, to State, duration time.Duration)
    RecordStateDuration(state State, duration time.Duration)

    // Assignment metrics
    RecordAssignmentChange(added, removed int, version int64)
    RecordAssignmentCalculationTime(duration time.Duration)
    RecordAffinityScore(score float64)  // Cache affinity preservation (0.0-1.0)

    // Health metrics
    RecordHeartbeat(workerID string, success bool)
    RecordLeadershipChange(newLeader string, duration time.Duration)
}

// Manager coordinates worker in distributed system
type Manager struct {
    // internal fields
}

// NewManager creates a new Manager with config, NATS connection, partition source, and options
// Config should be loaded from file (JSON/YAML)
// NATS connection and PartitionSource are required parameters (explicit in signature)
// Options provide optional runtime dependencies (hooks, strategy, metrics, logger, election agent)
func NewManager(cfg *Config, natsConn *nats.Conn, partitionSource PartitionSource, opts ...Option) (*Manager, error)

// Example usage (basic):
//   cfg := parti.LoadConfig("config.yaml")
//   mgr, err := parti.NewManager(cfg, natsConn, partitionSource)
//
// Example usage (with options):
//   mgr, err := parti.NewManager(cfg, natsConn, partitionSource,
//       parti.WithStrategy(parti.WeightedConsistentHashStrategy(150)),
//       parti.WithElectionAgent(electionAgentClient),
//       parti.WithHooks(hooks),
//       parti.WithMetrics(prometheusCollector),
//       parti.WithLogger(zapLogger),
//   )

// Start initializes and runs the manager
// Blocks until worker ID claimed and initial assignment received
func (m *Manager) Start(ctx context.Context) error

// Stop gracefully shuts down with configurable drain timeout
func (m *Manager) Stop(ctx context.Context, cfg ShutdownConfig) error

// ShutdownConfig controls graceful shutdown behavior
type ShutdownConfig struct {
    DrainTimeout time.Duration // Max time to wait for in-flight work (default: 25s)
    ForceStop    bool          // Emergency shutdown without waiting
}

// Query methods (thread-safe)
func (m *Manager) WorkerID() string
func (m *Manager) IsLeader() bool
func (m *Manager) CurrentAssignment() Assignment  // Returns versioned assignment
func (m *Manager) State() State
```

**Questions:**
- Should `Start()` block until fully ready, or return immediately and signal via callback?
- Should `Partition.Keys` be `[]string` or something more structured?
- Is `PartitionSource` interface the right approach for partition discovery?
- Should we support multiple assignment strategies, or just consistent hashing?

**Your Answer:**
```
1. Blocks until worker ID claimed and initial assignment received. use ctx and set timeout in Start
2. the []string is ok
3.The PartitionSource looks fine.
4. we should support multiple assignment strategies, make consistent hashing implementation as default. it's better to provide interface for adapting different assignment strategies.

I have concern about `OnAssignmentChanged`. it provides added & removed parameters. but the caller implements this method and failed to add or remove subscription, it might leave zombie subscriptions.

```

**Design Rationale (Based on Review):**

1. **Configuration Separation**:
   - **Serializable Config**: All timing, thresholds, and IDs can be loaded from JSON/YAML files
   - **Runtime Dependencies**: NATS connection, hooks, strategies provided via functional options
   - **Benefits**: Config files can be version-controlled, runtime dependencies injected at startup

2. **Functional Options Pattern**:
   - Clean separation between file-based config and code-based dependencies
   - Flexible and extensible (easy to add new options)
   - Clear validation and defaults
   - Type-safe with compile-time checking
   - All optional dependencies have sensible defaults:
     - Strategy: WeightedConsistentHashStrategy(150)
     - Metrics: nopMetricsCollector (no-op)
     - Logger: nopLogger (no-op)
     - Hooks: nil (no callbacks)
     - ElectionAgent: nil (fallback to NATS KV-based election)

3. **Example Config File** (`parti-config.yaml`):
   ```yaml
   worker_id_prefix: "defender"
   worker_id_min: 0
   worker_id_max: 199
   worker_id_ttl: "30s"

   heartbeat_interval: "2s"
   heartbeat_ttl: "6s"

   cold_start_window: "30s"
   planned_scale_window: "10s"
   restart_detection_ratio: 0.5

   assignment:
     min_rebalance_threshold: 0.15
     rebalance_cooldown: "10s"
   ```

4. **Example Usage**:
   ```go
   // Load config from file
   cfg, err := parti.LoadConfigFromFile("parti-config.yaml")
   cfg.SetDefaults() // Fill in any missing values
   cfg.Validate()    // Ensure config is valid

   // Setup required dependencies
   natsConn, err := nats.Connect("nats://localhost:4222")
   partitionSource := createCassandraPartitionSource()

   // Create manager with required + optional dependencies
   mgr, err := parti.NewManager(cfg, natsConn, partitionSource,
       parti.WithStrategy(parti.WeightedConsistentHashStrategy(150)),
       parti.WithHooks(&parti.Hooks{
           OnAssignmentChanged: handleAssignment,
           OnLeadershipChanged: handleLeadership,
       }),
       parti.WithMetrics(prometheusCollector),
       parti.WithLogger(zapLogger),
   )
   ```

5. **Assignment Versioning**: Critical for consistency tracking. Prevents stale assignment reads and enables proper synchronization during leadership transitions.

6. **Lifecycle Metadata**: Tracks cluster lifecycle state (`cold_start`, `post_cold_start`, `stable`) to determine correct stabilization windows (30s vs 10s).

7. **Graceful Shutdown**: Supports proper cleanup during rolling updates with configurable drain timeout (default 25s from operational docs).

8. **Metrics Interface**: Provides granular observability without forcing a specific implementation (Prometheus, StatsD, nop, etc.).

9. **Cache Affinity**: ConsistentHashStrategy will track affinity scores internally to preserve >80% partition locality during rebalancing.

---

### 3. **Partition Discovery**

How does Parti discover what partitions exist?

```go
// PartitionSource discovers available partitions
type PartitionSource interface {
    // List all partitions that exist
    ListPartitions(ctx context.Context) ([]Partition, error)

    // Watch for partition changes (optional)
    Watch(ctx context.Context) (<-chan []Partition, error)
}

// Built-in implementations:

// 1. Static list (for simple cases)
func StaticSource(partitions []Partition) PartitionSource

// 2. Dynamic (query from database, API, etc.)
type CustomSource struct {
    fetchFunc func(context.Context) ([]Partition, error)
}

// 3. NATS subject discovery (scan existing subjects)
func NATSSubjectSource(js nats.JetStreamContext, pattern string) PartitionSource
```

**For your use case** (Tool + Chamber from Cassandra):
```go
type CassandraPartitionSource struct {
    session *gocql.Session
}

func (s *CassandraPartitionSource) ListPartitions(ctx context.Context) ([]Partition, error) {
    // Query: SELECT tool_id, chamber_id, weight FROM chambers
    var partitions []Partition
    // ... scan results
    return partitions, nil
}
```

**Questions:**
- Should partition discovery be **pull-based** (Parti queries) or **push-based** (app tells Parti)?
- How often should Parti refresh the partition list?
- Should we support partition weights, or always use consistent hashing by count?
- What if partitions appear/disappear dynamically?

**Your Answer:**
```
1. pull-based, but provide method that let app notifiy the partition changes, then parti query that.
2. after initialization, wait app to notify changes
3. needs to support parition weights(app can set it to the same weight, the behaviour will like constitent hashsing), the consistent hashing + weight are better.
4. when partitions appear/disappear, app notify changes
```

---

### 4. **NATS Subscription Management**

Should Parti handle NATS subscriptions internally, or just notify via callbacks?

**Option A: Parti handles subscriptions** (opinionated)
```go
// Parti internally subscribes/unsubscribes based on assignments
cfg := &parti.Config{
    MessageHandler: func(ctx context.Context, msg *parti.Message) error {
        // Your business logic here
        return msg.Ack()
    },
}
```

**Option B: Callbacks notify, app handles** (flexible)
```go
cfg := &parti.Config{
    Hooks: &parti.Hooks{
        OnAssignmentChanged: func(ctx context.Context, added, removed []Partition) error {
            // App manages subscriptions
            for _, p := range added {
                js.Subscribe(p.Subject("dc"), myHandler)
            }
            return nil
        },
    },
}
```

**Questions:**
- Which option is better for your migration?
- Should Parti provide a helper `subscription` package but let app decide?
- How do you want to handle message acknowledgment (Ack/Nak)?

**Your Answer:**
```
1. Option B. becasue the NATSConn are provided by caller, let caller to handle subscribes/unsubscribes
2. yes, provide subscription helper
3. in subscription helper, we handle ack/nak, let app to register message handler themself. if app decides to handle themselvs, we provide OnAssignmentChanged.

```

---

### 5. **Assignment Strategy**

How should Parti distribute partitions to workers?

```go
type AssignmentStrategy interface {
    // Calculate assignments for given workers and partitions
    Assign(workers []string, partitions []Partition) (map[string][]Partition, error)
}

// Built-in strategies:
// 1. Consistent hashing (minimal movement on scale)
func ConsistentHashStrategy(virtualNodes int) AssignmentStrategy

// 2. Weighted (by partition weight)
func WeightedStrategy() AssignmentStrategy

// 3. Weighted + Consistent Hash (recommended - combines both benefits)
func WeightedConsistentHashStrategy(virtualNodes int) AssignmentStrategy

// 4. Round-robin (simple)
func RoundRobinStrategy() AssignmentStrategy
```

**Questions:**
- Do you need weighted assignment, or is consistent hashing enough?
- Should the strategy consider worker capacity/load?
- Should Parti support custom strategies via interface?

**Your Answer:**
```
1. provide flexibility to support different assgiment. the combination of consistent hash plus weight are prefered
2. yes, if the worker able to provide it's capacity and currenet loading. design a simple but flexiable solution
3. yes
```

**Design Rationale (Based on Review):**

1. **Cache Affinity Preservation**: ConsistentHashStrategy internally tracks previous assignments to maintain >80% partition locality during rebalancing, supporting >95% cache hit rate requirement.

2. **Affinity Metrics**: `RecordAffinityScore()` exposes cache affinity preservation quality (0.0-1.0) for monitoring.

3. **Weight Distribution**: Two-phase assignment (consistent hash + weight balancing) ensures even load distribution with ±20% variance tolerance.

---

### 6. **State Machine & Lifecycle**

Based on your state machine docs, Parti needs to manage:

```
INIT → CLAIMING_ID → ELECTION → WAITING_ASSIGNMENT → STABLE
```

**Questions:**
- Should Parti expose these states to the application?
- Do you need fine-grained state callbacks, or just the major events (ID claimed, leader changed, assignment changed)?
- Should Parti handle cold start detection (30s window) internally?
- What about restart detection (comparing assignment map vs active workers)?

**Your Answer:**
```
1. Yes, provide state info for caller to query
2. need fine-grained state callbacks but don't over-engineering
3. yes, and make window duration be configurable
4. comparing assignment map vs active workers. make threshold be configurable
```

**Design Rationale (Based on Review):**

1. **State Exposure**: All 8 states exposed via `State()` method for debugging and monitoring.

2. **Lifecycle Tracking**: Assignment metadata includes lifecycle state to distinguish:
   - `cold_start`: Initial deployment or cluster restart (uses 30s window)
   - `post_cold_start`: First successful assignment after cold start
   - `stable`: Normal operation (uses 10s window for planned scale)

3. **Restart Detection**: Leader compares `previous_worker_count` vs `active_heartbeats`:
   - If `previous >= 10 AND active < (previous * RestartDetectionRatio)` → treat as restart
   - Default ratio: 0.5 (50%)
   - Example: 64 defenders → 1 active = restart detected

4. **Configurable Windows**:
   - `ColdStartWindow`: Default 30s (batches all initial joins)
   - `PlannedScaleWindow`: Default 10s (fast response to gradual scaling)
   - `RestartDetectionRatio`: Default 0.5 (50% threshold)

5. **State Callbacks**: `OnStateChanged` provides visibility into state transitions for monitoring and debugging.

---

### 7. **Migration Strategy**

How do you want to migrate from Kafka to Parti?

**Option A: Big bang cutover**
- Stop all Kafka-based Defenders
- Deploy new Parti-based version
- Start up with NATS

**Option B: Shadow mode**
- Run Kafka + Parti side-by-side
- Compare results
- Gradually shift traffic

**Questions:**
- Which migration approach fits your deployment process?
- Do you need dual mode support in Parti?
- Should Parti provide migration utilities/helpers?

**Your Answer:**
```
1. Option A.
2. No, Parti focus on NATS-based soluton
3. no need
```

---

### 8. **Error Handling & Resilience**

**Questions:**
- What should happen if worker can't claim any ID (all taken)?
- What if assignment map update fails?
- Should Parti retry operations, or fail fast?
- How should Parti handle NATS connection loss?
- Should leadership automatically failover if leader crashes?

**Your Answer:**
```
1. claim a new ID larger than current max index of pool, report error, remember to preserve enough space in stable id pool
2. provide simple retry mechansim, make it configurable
3. prefer retry, but it can be controlled by config, let caller decide it needs to fail fast or retry
4. let NATS golang library handle it, the parti report errors and maybe callback to let caller to handle NATS connection
4. yes, it's the important feature
```

---

### 9. **Metrics & Observability**

Should Parti expose Prometheus metrics?

```go
// Potential metrics:
// - parti_worker_state (gauge)
// - parti_is_leader (gauge)
// - parti_assigned_partitions (gauge)
// - parti_assignment_changes_total (counter)
// - parti_leadership_changes_total (counter)
// - parti_heartbeat_failures_total (counter)
```

**Questions:**
- Should Parti provide built-in metrics, or let app handle it?
- What metrics are most important for your ops team?
- Should Parti integrate with structured logging (zerolog, zap)?

**Your Answer:**
```
1. Parti provide an interface that let app to provide it's own implementation(e.g. nop, Prometheus). this interfface defines events like assignment change, leadership change, heartbeat failure...etc.
2. the health and runtime status like indicates the parition stablibility such as heartbeat_failures, leadership_changes, assignment_changes
3. yes, but let app inject the Logger interface. the following is the zap suger logger like interface

type Logger interface {
	// Debug logs a message at DebugLevel.
	// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
	Debug(msg string, keysAndValues ...any)
	// Info logs a message at InfoLevel.
	// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
	Info(msg string, keysAndValues ...any)
	// Warn logs a message at WarnLevel.
	// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
	Warn(msg string, keysAndValues ...any)
	// Error logs a message at ErrorLevel
	// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
	Error(msg string, keysAndValues ...any)
	// Fatal logs a message at FatalLevel
	// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
	//
	// The logger then calls os.Exit(1), even if logging at FatalLevel is disabled.
	Fatal(msg string, keysAndValues ...any)
}
```

---

### 10. **Data Collector (Publisher) Side**

For Data Collector, do you need any Parti support, or just:

```go
// Simple NATS publish
js.Publish("dc.tool001.chamber1.completed", jsonData)
```

**Questions:**
- Does Data Collector need any Parti functionality?
- Or is it just a straightforward NATS publisher?
- Should we provide a helper package for publishers?

**Your Answer:**
```
for now, i think Data Collector is just a straightforward NATS publisher.
but we could provide helper package and functions that simplify the NATS Jetstream connection establishment
```

---

## Example Usage Sketch

### Your Defender Application:

**parti-config.yaml**:
```yaml
worker_id_prefix: "defender"
worker_id_min: 0
worker_id_max: 199
worker_id_ttl: "30s"

heartbeat_interval: "2s"
heartbeat_ttl: "6s"

cold_start_window: "30s"
planned_scale_window: "10s"
restart_detection_ratio: 0.5

assignment:
  min_rebalance_threshold: 0.15
  rebalance_cooldown: "10s"
```

**main.go**:
```go
package main

import (
    "context"
    "github.com/arloliu/parti"
)

func main() {
    // Load serializable config from file
    cfg, err := parti.LoadConfigFromFile("parti-config.yaml")
    if err != nil {
        log.Fatal(err)
    }
    cfg.SetDefaults() // Apply defaults for any missing values
    if err := cfg.Validate(); err != nil {
        log.Fatal(err)
    }

    // Setup runtime dependencies
    nc, _ := nats.Connect("nats://localhost:4222")

    partitionSource := &CassandraPartitionSource{
        session: cassandraSession,
    }

    hooks := &parti.Hooks{
        OnAssignmentChanged: func(ctx context.Context, added, removed []parti.Partition) error {
            // Update NATS subscriptions
            return updateSubscriptions(added, removed)
        },
        OnLeadershipChanged: func(ctx context.Context, isLeader bool) error {
            log.Info("Leadership changed", "isLeader", isLeader)
            return nil
        },
    }

    // Create manager with config + options
    mgr, err := parti.NewManager(cfg, nc, partitionSource,
        parti.WithStrategy(parti.WeightedConsistentHashStrategy(150)),
        parti.WithElectionAgent(electionAgentClient),
        parti.WithHooks(hooks),
        parti.WithMetrics(prometheusCollector),
        parti.WithLogger(zapLogger),
    )
    if err != nil {
        log.Fatal(err)
    }

    // Start with timeout context
    ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()

    if err := mgr.Start(ctx); err != nil {
        log.Fatal(err)
    }

    // Your existing business logic continues...
}
```

---

### 11. **Configuration Design: Serializable vs Runtime**

**Problem**: The original `Config` struct mixed two concerns:
1. **Serializable settings** (timing, thresholds, IDs) → should come from files
2. **Runtime dependencies** (NATS connection, hooks, interfaces) → must come from code

**Solution**: Separate into two layers:

#### Layer 1: Serializable Config (JSON/YAML)

```go
type Config struct {
    WorkerIDPrefix        string           `json:"worker_id_prefix"`
    WorkerIDMin           int              `json:"worker_id_min"`
    WorkerIDMax           int              `json:"worker_id_max"`
    WorkerIDTTL           string           `json:"worker_id_ttl"`
    HeartbeatInterval     string           `json:"heartbeat_interval"`
    HeartbeatTTL          string           `json:"heartbeat_ttl"`
    ColdStartWindow       string           `json:"cold_start_window"`
    PlannedScaleWindow    string           `json:"planned_scale_window"`
    RestartDetectionRatio float64          `json:"restart_detection_ratio"`
    OperationTimeout      string           `json:"operation_timeout"`
    ElectionTimeout       string           `json:"election_timeout"`
    StartupTimeout        string           `json:"startup_timeout"`
    ShutdownTimeout       string           `json:"shutdown_timeout"`
    Assignment            AssignmentConfig `json:"assignment"`
}

func (c *Config) SetDefaults() { /* ... */ }
func (c *Config) Validate() error { /* ... */ }
```

**Benefits**:
- Can be version-controlled
- Environment-specific configs (dev/staging/prod)
- Easy to review and audit
- No Go types in config files

**Example YAML**:
```yaml
worker_id_prefix: "defender"
worker_id_min: 0
worker_id_max: 199
worker_id_ttl: "30s"
heartbeat_interval: "2s"
heartbeat_ttl: "6s"
cold_start_window: "30s"
planned_scale_window: "10s"
restart_detection_ratio: 0.5
assignment:
  min_rebalance_threshold: 0.15
  rebalance_cooldown: "10s"
```

#### Layer 2: Runtime Dependencies (Functional Options)

**Required parameters (explicit in function signature):**
```go
func NewManager(
    cfg *Config,
    natsConn *nats.Conn,
    partitionSource PartitionSource,
    opts ...Option,
) (*Manager, error)
```

**Optional parameters (via functional options):**
```go
type Option func(*managerOptions) error

func WithStrategy(strategy AssignmentStrategy) Option { /* ... */ }
func WithElectionAgent(agent ElectionAgent) Option { /* ... */ }
func WithHooks(hooks *Hooks) Option { /* ... */ }
func WithMetrics(metrics MetricsCollector) Option { /* ... */ }
func WithLogger(logger Logger) Option { /* ... */ }
```

**Benefits**:
- **Required dependencies are explicit**: Compile error if missing NATS or PartitionSource
- Type-safe dependency injection
- Clear required vs optional dependencies
- Extensible (add new options without breaking existing code)
- Testable (easy to mock dependencies)

**Note on Election Agent**:
- **Optional**: If not provided, Parti falls back to NATS KV-based leader election
- **Recommended for production**: Use external Election Agent (github.com/arloliu/election-agent) for better split-brain prevention
- **Why optional**: Simplifies development/testing, allows gradual migration

#### Usage Pattern

```go
// 1. Load config from file
cfg, err := parti.LoadConfigFromFile("parti-config.yaml")
if err != nil {
    return err
}

// 2. Apply defaults and validate
cfg.SetDefaults()
if err := cfg.Validate(); err != nil {
    return err
}

// 3. Create runtime dependencies
nc := connectToNATS()
partitionSource := createPartitionSource()
hooks := createHooks()

// 4. Create manager with required + optional dependencies
mgr, err := parti.NewManager(cfg, nc, partitionSource,
    parti.WithStrategy(parti.WeightedConsistentHashStrategy(150)), // Optional (has default)
    parti.WithElectionAgent(electionAgentClient),        // Optional (default: NATS KV election)
    parti.WithHooks(hooks),                              // Optional (default: nil)
    parti.WithMetrics(metricsCollector),                 // Optional (default: nop)
    parti.WithLogger(logger),                            // Optional (default: nop)
)
```

#### Validation Strategy

```go
func NewManager(cfg *Config, natsConn *nats.Conn, partitionSource PartitionSource, opts ...Option) (*Manager, error) {
    // Validate required parameters (compile-time enforced, but still check for nil)
    if natsConn == nil {
        return nil, errors.New("NATS connection cannot be nil")
    }
    if partitionSource == nil {
        return nil, errors.New("partition source cannot be nil")
    }

    // Parse duration strings to time.Duration
    workerIDTTL, err := time.ParseDuration(cfg.WorkerIDTTL)
    if err != nil {
        return nil, fmt.Errorf("invalid worker_id_ttl: %w", err)
    }

    // Apply options with defaults
    options := &managerOptions{
        strategy:      WeightedConsistentHashStrategy(150), // Default: weighted consistent hash
        metrics:       &nopMetricsCollector{},              // Default: no-op metrics
        logger:        &nopLogger{},                        // Default: no-op logger
        hooks:         nil,                                 // Default: no hooks
        electionAgent: nil,                                 // Default: use NATS KV-based election
    }

    for _, opt := range opts {
        if err := opt(options); err != nil {
            return nil, fmt.Errorf("failed to apply option: %w", err)
        }
    }

    // Create manager
    return &Manager{
        config:          cfg,
        workerIDTTL:     workerIDTTL,
        natsConn:        natsConn,
        partitionSource: partitionSource,
        strategy:        options.strategy,
        hooks:           options.hooks,
        metrics:         options.metrics,
        logger:          options.logger,
        electionAgent:   options.electionAgent,
    }, nil
}
```

#### Advantages Over Single Config Struct

| Aspect | Single Config | Separated Config |
|--------|--------------|------------------|
| **File-based config** | Hard (mix of types) | Easy (all serializable) |
| **Required dependencies** | Easy to forget | Compile-time enforced |
| **Optional dependencies** | Unclear | Explicit via options |
| **Dependency injection** | Unclear | Explicit via options |
| **Testing** | Hard to mock | Easy to mock |
| **Defaults** | Scattered | Centralized in SetDefaults() |
| **Validation** | Mixed concerns | Clear separation |
| **Extensibility** | Breaks API | Add new options |
| **Type safety** | Runtime errors | Compile-time checks |

#### Alternative Considered: Builder Pattern

```go
// Not chosen - more verbose
mgr := parti.NewManagerBuilder().
    WithConfig(cfg).
    WithNATSConn(nc).
    WithPartitionSource(source).
    Build()
```

**Why functional options is better**:
- More idiomatic in Go
- Cleaner error handling
- Less boilerplate
- Used by standard library (e.g., `grpc.Dial`)

---

### 12. **Assignment Versioning & Consistency**

**Importance**: Assignment versioning is critical for consistency in distributed systems.

**Version Tracking**:
```go
type Assignment struct {
    Version    int64                    // Monotonically increasing version number
    Lifecycle  LifecycleState           // Cluster lifecycle state
    Partitions []Partition             // Assigned partitions
    Timestamp  time.Time               // When assignment was created
    Metadata   map[string]interface{}  // Additional context
}
```

**Use Cases**:
1. **Consistency Detection**: Workers can detect stale assignments by comparing versions
2. **Leadership Transitions**: New leader reads last assignment version to continue from correct state
3. **Debugging**: Version history helps trace assignment changes over time
4. **Race Condition Prevention**: Prevents workers from applying out-of-order assignments

**Version Management**:
- Leader increments version on each assignment update
- Version stored in NATS KV alongside assignment map
- Workers validate version before applying assignments
- Metrics track assignment version for observability

**Example Flow**:
```
T0: Version 50, 64 workers, lifecycle=stable
T1: Worker crash detected → Version 51, 63 workers, lifecycle=stable (emergency)
T2: Scale to 70 workers → Version 52, 70 workers, lifecycle=stable (planned scale)
T3: Leader failover → New leader reads Version 52, continues from there
```

---

### 13. **Assignment Rebalancing Policy**

**Problem**: Frequent rebalancing causes cache thrashing and performance degradation.

**Solution**: Configurable rebalancing policy with thresholds and cooldown.

**Configuration**:
```go
type AssignmentConfig struct {
    MinRebalanceThreshold float64       // Minimum change % to trigger rebalance
    RebalanceCooldown     time.Duration // Cooldown between rebalances
}
```

**Default Values** (from design docs):
- `MinRebalanceThreshold`: 0.15 (15% change required)
- `RebalanceCooldown`: 10 seconds

**Rebalancing Scenarios**:

| Scenario | Trigger | Window | Rebalance? |
|----------|---------|--------|------------|
| Single worker crash | Immediate | 0s | Yes (emergency) |
| Planned scale +5 workers | After cooldown | 10s | Yes if >15% change |
| Rolling update (same IDs) | None | N/A | No (same assignments) |
| Cold start 0→64 | After stabilization | 30s | Yes (initial assignment) |
| Restart 64→0→64 | After stabilization | 30s | Yes (detected restart) |

**Rebalancing Decision Logic**:
```go
// Pseudocode
func shouldRebalance(current, previous Assignment) bool {
    // Emergency: always rebalance
    if crashDetected {
        return true
    }

    // Check cooldown
    if time.Since(previous.Timestamp) < RebalanceCooldown {
        return false
    }

    // Check threshold
    workerDelta := abs(len(current.Workers) - len(previous.Workers))
    changePercent := float64(workerDelta) / float64(len(previous.Workers))

    return changePercent >= MinRebalanceThreshold
}
```

**Benefits**:
1. **Reduces Cache Thrashing**: Fewer rebalances = better cache hit rates
2. **Prevents Rebalancing Storms**: Cooldown prevents rapid successive rebalances
3. **Configurable**: Apps can tune based on their cache warming characteristics
4. **Emergency Override**: Crashes bypass threshold for immediate recovery

---

### 14. **Subscription Management Concerns**

**Issue**: `OnAssignmentChanged` callback provides `added` and `removed` partitions, but if the callback fails to update subscriptions properly, zombie subscriptions may remain.

**Zombie Subscription Scenario**:
```go
OnAssignmentChanged: func(ctx context.Context, added, removed []Partition) error {
    // Successfully unsubscribe from removed partitions
    for _, p := range removed {
        unsubscribe(p)  // OK
    }

    // Fail to subscribe to added partitions (network error, etc.)
    for _, p := range added {
        err := subscribe(p)
        if err != nil {
            return err  // ERROR - now we have inconsistent state
        }
    }
}
```

**Problem**:
- Removed subscriptions are cleaned up, but added subscriptions failed
- Worker's subscriptions don't match assignment
- Next assignment update might not retry the failed subscriptions

**Solutions**:

**Option A: Subscription Helper with Reconciliation** (Recommended)
```go
// Parti provides a subscription helper that handles reconciliation
type SubscriptionManager struct {
    // Tracks desired vs actual subscriptions
    // Automatically retries failed subscriptions
    // Ensures eventual consistency
}

cfg := &parti.Config{
    Hooks: &parti.Hooks{
        OnAssignmentChanged: func(ctx context.Context, added, removed []Partition) error {
            // Subscription helper handles retries and reconciliation
            return subscriptionHelper.UpdateSubscriptions(ctx, added, removed)
        },
    },
}
```

**Option B: Idempotent Full Sync**
```go
// Instead of delta (added/removed), provide full desired state
OnAssignmentSynced: func(ctx context.Context, desiredPartitions []Partition) error {
    // App implements idempotent sync
    return syncSubscriptions(desiredPartitions)
}

func syncSubscriptions(desired []Partition) error {
    current := getCurrentSubscriptions()

    // Unsubscribe from extras
    for _, p := range difference(current, desired) {
        unsubscribe(p)
    }

    // Subscribe to missing
    for _, p := range difference(desired, current) {
        subscribe(p)
    }
}
```

**Option C: Dual Callbacks** (Flexible)
```go
type Hooks struct {
    // Fine-grained control (app handles state)
    OnAssignmentChanged func(ctx context.Context, added, removed []Partition) error

    // OR full-state sync (Parti ensures consistency)
    OnAssignmentSynced func(ctx context.Context, all []Partition) error
}
```

**Recommendation**: Provide **subscription helper package** (Option A) as default, with Option C for advanced users who want full control.

**Subscription Helper Features**:
- Automatic retry on subscription failures
- Reconciliation loop to detect and fix inconsistencies
- Metrics for subscription success/failure rates
- Handles NATS consumer creation and cleanup
- Thread-safe subscription tracking

---

## Additional Questions/Concerns

**Your Input:**
```
From design review:
1. ✅ Assignment versioning - ADOPTED
2. ✅ Graceful shutdown config - ADOPTED
3. ✅ Rebalancing thresholds - ADOPTED
4. ✅ Cache affinity tracking - ADOPTED (internal to strategy)
5. ✅ State lifecycle metadata - ADOPTED
6. ✅ Granular metrics interface - ADOPTED
7. 🔄 Network partition handling - DEFER (Election Agent handles split-brain)
8. 🔄 Dynamic config updates - DEFER (v1.1+, start simple)
9. 🔄 Testing utilities - DEFER (implementation phase)
10. 📝 API stability/versioning - Document in README (semantic versioning)
```

---

## Next Steps

1. ✅ **Finalize Parti API design** - COMPLETED
2. ✅ **Create detailed library specification document** - COMPLETED (see [library-specification.md](library-specification.md))
3. 🏗️ **Define package structure and interfaces** - NEXT
4. 💻 Create example code for your use case
5. 🧪 Define testing strategy
6. 🚀 Implementation plan
7. 📖 Write migration guide (Kafka → Parti), only write it after all steps.

## Summary of Design Decisions

### Core Features (v1.0)
- ✅ Assignment versioning for consistency
- ✅ Lifecycle state tracking (cold_start, post_cold_start, stable)
- ✅ Configurable stabilization windows (30s/10s)
- ✅ Restart detection (threshold-based)
- ✅ Graceful shutdown with drain timeout
- ✅ Rebalancing policy (threshold + cooldown)
- ✅ Cache affinity preservation (>80%)
- ✅ Granular metrics interface
- ✅ Structured logging interface
- ✅ Subscription helper with reconciliation
- ✅ Weighted consistent hashing strategy
- ✅ Extensible strategy interface

### Deferred to Future Versions
- 🔄 Network partition handling (v1.1+)
- 🔄 Dynamic configuration updates (v1.1+)
- 🔄 Advanced testing utilities (implementation phase)
- 🔄 Worker capacity/load awareness (v1.2+)

### Design Principles
1. **Start Simple**: Core functionality first, add features as needed
2. **Extensibility**: Interfaces for strategies, metrics, logging
3. **Observability**: Rich metrics and state exposure for debugging
4. **Consistency**: Version tracking and lifecycle management
5. **Performance**: Cache affinity, rebalancing policies
6. **Reliability**: Graceful shutdown, subscription reconciliation

