# Simulation Test Implementation Plan

## Overview

This document outlines the implementation plan for the long-duration simulation test system that validates Parti's correctness, stability, and performance under realistic production workloads.

**Goals:**
- ✅ Validate message delivery correctness (no loss, no duplication)
- ✅ Test partition rebalancing during worker scaling
- ✅ Validate weighted partition assignment with exponential distribution
- ✅ Test chaos scenarios (crashes, network issues, leader failures)
- ✅ Run stable for 8-24+ hours under realistic load

**Test Scale:**
- **Development**: 100 partitions, 10 workers, ~10 msg/sec
- **Realistic**: 1500 partitions, 100 workers, ~15 msg/sec (exponential weights: 5% extreme, 95% normal)
- **Stress**: 1500 partitions, 100 workers, ~100 msg/sec

---

## Architecture

### Component Overview

```
┌─────────────────────────────────────────────────────────────────┐
│                         Coordinator                              │
│  - Message sequence tracking (gaps/duplicates)                   │
│  - Chaos event injection (crashes, scaling, network)             │
│  - Checkpoint/restore state                                      │
│  - Validation and reporting                                      │
└─────────────────────────────────────────────────────────────────┘
                              │
                    ┌─────────┴─────────┐
                    │    NATS JetStream  │
                    │  (embedded/external)│
                    └─────────┬─────────┘
                              │
         ┌────────────────────┼────────────────────┐
         │                    │                    │
    ┌────▼─────┐        ┌────▼─────┐        ┌────▼─────┐
    │ Producer │        │ Producer │        │ Producer │
    │   (0)    │        │   (1)    │        │  (N-1)   │
    │          │        │          │        │          │
    │ Static   │        │ Static   │        │ Static   │
    │ Assign:  │        │ Assign:  │        │ Assign:  │
    │ P0-P149  │        │ P150-299 │        │ P1350-   │
    │          │        │          │        │  1499    │
    └──────────┘        └──────────┘        └──────────┘
         │                    │                    │
         │        Per-partition rate: 1 msg/sec    │
         │        With exponential weights:        │
         │        - 5% extreme (weight=100)        │
         │        - 95% normal (weight=1)          │
         │                                         │
         └────────────────────┬────────────────────┘
                              │
                    ┌─────────▼─────────┐
                    │    NATS JetStream  │
                    │   1500 subjects    │
                    └─────────┬─────────┘
                              │
         ┌────────────────────┼────────────────────┐
         │                    │                    │
    ┌────▼─────┐        ┌────▼─────┐        ┌────▼─────┐
    │  Worker  │        │  Worker  │        │  Worker  │
    │   (0)    │        │   (1)    │        │  (M-1)   │
    │          │        │          │        │          │
    │ parti.   │        │ parti.   │        │ parti.   │
    │ Manager  │        │ Manager  │        │ Manager  │
    │   +      │        │   +      │        │   +      │
    │ Durable  │        │ Durable  │        │ Durable  │
    │ Helper   │        │ Helper   │        │ Helper   │
    │          │        │          │        │          │
    │ Dynamic  │        │ Dynamic  │        │ Dynamic  │
    │ Assign   │        │ Assign   │        │ Assign   │
    │ by       │        │ by       │        │ by       │
    │ Strategy │        │ Strategy │        │ Strategy │
    └──────────┘        └──────────┘        └──────────┘
         │                    │                    │
         └────────────────────┼────────────────────┘
                              │
                    ┌─────────▼─────────┐
                    │  Prometheus :9090  │
                    │  (Docker Compose)  │
                    └───────────────────┘
```

### Message Flow

```
1. Producer publishes message to partition subject:
   Subject: "simulation.partition.{partitionID}"
   Message: {
     PartitionID: 42,
     ProducerID: "producer-0",
     PartitionSequence: 1234,
     ProducerSequence: 5678,
     Timestamp: 1699012345678900,
     Weight: 100  // or 1 for normal partitions
   }

2. Producer reports to Coordinator:
   "Sent partition=42 seq=1234 from producer-0"

3. Worker receives message via DurableHelper:
   - Validates sequence numbers
   - Simulates processing (10-100ms delay)
   - ACKs message
   - Reports to Coordinator

4. Coordinator tracks:
   - Last sent sequence per partition
   - Last received sequence per partition
   - Detects gaps/duplicates in real-time
   - Triggers chaos events
```

---

## Directory Structure

```
test/simulation/
├── cmd/
│   └── simulation/
│       └── main.go                    # Single binary with modes
│
├── internal/
│   ├── config/
│   │   ├── config.go                  # Configuration structures
│   │   ├── validation.go              # Config validation
│   │   └── defaults.go                # Default values
│   │
│   ├── producer/
│   │   ├── producer.go                # Message producer
│   │   ├── rate_controller.go         # Per-partition rate control
│   │   ├── weight_generator.go        # Exponential weight distribution
│   │   └── message.go                 # Message structure
│   │
│   ├── worker/
│   │   ├── worker.go                  # Worker using parti.Manager + DurableHelper
│   │   ├── processor.go               # Message processing logic
│   │   └── metrics.go                 # Worker-level metrics
│   │
│   ├── coordinator/
│   │   ├── coordinator.go             # Central coordinator
│   │   ├── tracker.go                 # Message sequence tracking
│   │   ├── chaos.go                   # Chaos event controller
│   │   ├── checkpoint.go              # State checkpointing
│   │   └── validator.go               # Gap/duplicate detection
│   │
│   ├── metrics/
│   │   ├── prometheus.go              # Prometheus metrics exporter
│   │   └── collector.go               # Metrics aggregation
│   │
│   └── natsutil/
│       └── embedded.go                # Embedded NATS server helper
│
├── configs/
│   ├── dev.yaml                       # Development (100 partitions, 10 workers)
│   ├── realistic.yaml                 # Realistic (1500 partitions, 100 workers, weighted)
│   ├── stress.yaml                    # Stress (1500 partitions, 100 workers, 100 msg/sec)
│   └── extreme.yaml                   # Extreme (1500 partitions, high rate)
│
├── docker/
│   ├── docker-compose.yml             # Prometheus + Grafana
│   └── prometheus.yml                 # Prometheus scrape config
│
├── IMPLEMENTATION_PLAN.md             # This file
├── README.md                          # Usage guide
└── ARCHITECTURE.md                    # Detailed architecture doc
```

---

## Data Structures

### Configuration (`internal/config/config.go`)

```go
package config

import "time"

// Config is the root configuration structure
type Config struct {
    Simulation  SimulationConfig  `yaml:"simulation"`
    Partitions  PartitionsConfig  `yaml:"partitions"`
    Producers   ProducersConfig   `yaml:"producers"`
    Workers     WorkersConfig     `yaml:"workers"`
    Coordinator CoordinatorConfig `yaml:"coordinator"`
    Chaos       ChaosConfig       `yaml:"chaos"`
    NATS        NATSConfig        `yaml:"nats"`
    Metrics     MetricsConfig     `yaml:"metrics"`
    Checkpoint  CheckpointConfig  `yaml:"checkpointing"`
}

type SimulationConfig struct {
    Duration time.Duration `yaml:"duration"`  // e.g., "12h"
    Mode     string        `yaml:"mode"`      // "all-in-one", "producer", "worker", "coordinator"
}

type PartitionsConfig struct {
    Count                   int                `yaml:"count"`                      // Total partitions (e.g., 1500)
    MessageRatePerPartition float64            `yaml:"message_rate_per_partition"` // Messages per second per partition
    Distribution            string             `yaml:"distribution"`               // "uniform", "exponential"
    Weights                 WeightsConfig      `yaml:"weights"`
}

type WeightsConfig struct {
    Exponential ExponentialWeightsConfig `yaml:"exponential"`
}

type ExponentialWeightsConfig struct {
    ExtremePercent float64 `yaml:"extreme_percent"` // 0.05 = 5% of partitions
    ExtremeWeight  int64   `yaml:"extreme_weight"`  // Weight for extreme partitions (e.g., 100)
    NormalWeight   int64   `yaml:"normal_weight"`   // Weight for normal partitions (e.g., 1)
}

type ProducersConfig struct {
    Count                  int             `yaml:"count"`                     // Number of producer processes
    PartitionsPerProducer  int             `yaml:"partitions_per_producer"`   // Static assignment
    RateVariation          RateVariationConfig `yaml:"rate_variation"`
}

type RateVariationConfig struct {
    Enabled       bool          `yaml:"enabled"`
    Pattern       string        `yaml:"pattern"`         // "global"
    MinMultiplier float64       `yaml:"min_multiplier"`  // e.g., 0.5
    MaxMultiplier float64       `yaml:"max_multiplier"`  // e.g., 2.0
    RampInterval  time.Duration `yaml:"ramp_interval"`   // e.g., "5m"
}

type WorkersConfig struct {
    Count              int                    `yaml:"count"`                // Number of worker processes
    AssignmentStrategy string                 `yaml:"assignment_strategy"`  // "WeightedConsistentHash", "ConsistentHash", "RoundRobin"
    StrategyConfig     map[string]interface{} `yaml:"strategy_config"`      // Strategy-specific config
    ProcessingDelay    ProcessingDelayConfig  `yaml:"processing_delay"`
}

type ProcessingDelayConfig struct {
    Min time.Duration `yaml:"min"` // e.g., "10ms"
    Max time.Duration `yaml:"max"` // e.g., "100ms"
}

type CoordinatorConfig struct {
    ValidationWindow time.Duration `yaml:"validation_window"` // e.g., "10m"
}

type ChaosConfig struct {
    Enabled  bool          `yaml:"enabled"`
    Events   []string      `yaml:"events"`   // ["worker_crash", "worker_restart", ...]
    Interval string        `yaml:"interval"` // "10-30m" (random between 10-30 minutes)
}

type NATSConfig struct {
    Mode      string        `yaml:"mode"` // "embedded", "external"
    URL       string        `yaml:"url"`  // "nats://localhost:4222"
    JetStream JetStreamConfig `yaml:"jetstream"`
}

type JetStreamConfig struct {
    MaxMemory      string `yaml:"max_memory"`       // "10GB"
    MaxFileStorage string `yaml:"max_file_storage"` // "50GB"
}

type MetricsConfig struct {
    Prometheus PrometheusConfig `yaml:"prometheus"`
}

type PrometheusConfig struct {
    Enabled bool `yaml:"enabled"`
    Port    int  `yaml:"port"` // 9090
}

type CheckpointConfig struct {
    Enabled  bool          `yaml:"enabled"`
    Interval time.Duration `yaml:"interval"` // e.g., "30m"
    Path     string        `yaml:"path"`     // "./checkpoints"
}
```

### Message Structure (`internal/producer/message.go`)

```go
package producer

import "encoding/json"

// SimulationMessage represents a message sent by producers
type SimulationMessage struct {
    PartitionID       int    `json:"partition_id"`        // 0-1499
    ProducerID        string `json:"producer_id"`         // "producer-0"
    PartitionSequence int64  `json:"partition_sequence"`  // Per-partition sequence: 0, 1, 2, 3...
    ProducerSequence  int64  `json:"producer_sequence"`   // Per-producer sequence: 0, 1, 2, 3...
    Timestamp         int64  `json:"timestamp"`           // Unix microseconds
    Weight            int64  `json:"weight"`              // Partition weight (100 or 1)
}

// Marshal serializes the message to JSON
func (m *SimulationMessage) Marshal() ([]byte, error) {
    return json.Marshal(m)
}

// Unmarshal deserializes the message from JSON
func Unmarshal(data []byte) (*SimulationMessage, error) {
    var msg SimulationMessage
    err := json.Unmarshal(data, &msg)
    if err != nil {
        return nil, err
    }
    return &msg, nil
}
```

### Tracking State (`internal/coordinator/tracker.go`)

```go
package coordinator

import "sync"

// MessageTracker tracks sent and received message sequences
type MessageTracker struct {
    mu                       sync.RWMutex
    lastSentPerPartition     map[int]int64     // partitionID -> last sequence sent
    lastReceivedPerPartition map[int]int64     // partitionID -> last sequence received
    lastSentPerProducer      map[string]int64  // producerID -> last sequence sent
    gaps                     []MessageGap      // Detected gaps
    duplicates               []MessageDup      // Detected duplicates
}

type MessageGap struct {
    PartitionID     int
    ExpectedSeq     int64
    ReceivedSeq     int64
    DetectedAt      int64  // Unix microseconds
}

type MessageDup struct {
    PartitionID     int
    Sequence        int64
    DetectedAt      int64  // Unix microseconds
}

// RecordSent records that a message was sent
func (t *MessageTracker) RecordSent(partitionID int, producerID string, partitionSeq, producerSeq int64) {
    t.mu.Lock()
    defer t.mu.Unlock()

    t.lastSentPerPartition[partitionID] = partitionSeq
    t.lastSentPerProducer[producerID] = producerSeq
}

// RecordReceived records that a message was received and validates sequence
func (t *MessageTracker) RecordReceived(partitionID int, partitionSeq int64) error {
    t.mu.Lock()
    defer t.mu.Unlock()

    lastReceived, exists := t.lastReceivedPerPartition[partitionID]
    if !exists {
        // First message for this partition
        t.lastReceivedPerPartition[partitionID] = partitionSeq
        return nil
    }

    expectedSeq := lastReceived + 1

    // Check for gap
    if partitionSeq > expectedSeq {
        gap := MessageGap{
            PartitionID: partitionID,
            ExpectedSeq: expectedSeq,
            ReceivedSeq: partitionSeq,
            DetectedAt:  time.Now().UnixMicro(),
        }
        t.gaps = append(t.gaps, gap)
        return fmt.Errorf("message gap detected: partition=%d expected=%d received=%d",
            partitionID, expectedSeq, partitionSeq)
    }

    // Check for duplicate
    if partitionSeq <= lastReceived {
        dup := MessageDup{
            PartitionID: partitionID,
            Sequence:    partitionSeq,
            DetectedAt:  time.Now().UnixMicro(),
        }
        t.duplicates = append(t.duplicates, dup)
        return fmt.Errorf("duplicate message detected: partition=%d seq=%d",
            partitionID, partitionSeq)
    }

    t.lastReceivedPerPartition[partitionID] = partitionSeq
    return nil
}

// GetStats returns current tracking statistics
func (t *MessageTracker) GetStats() TrackerStats {
    t.mu.RLock()
    defer t.mu.RUnlock()

    return TrackerStats{
        TotalPartitions:    len(t.lastSentPerPartition),
        TotalSent:          sumValues(t.lastSentPerPartition),
        TotalReceived:      sumValues(t.lastReceivedPerPartition),
        GapCount:           len(t.gaps),
        DuplicateCount:     len(t.duplicates),
    }
}

type TrackerStats struct {
    TotalPartitions int
    TotalSent       int64
    TotalReceived   int64
    GapCount        int
    DuplicateCount  int
}
```

---

## Implementation Phases

### Phase 1: Core Simulation (Week 1)

**Goal**: Basic producer/worker/coordinator with message validation

**Deliverables**:
1. ✅ Configuration system (`internal/config`)
   - YAML parsing
   - Validation
   - Default values

2. ✅ Producer implementation (`internal/producer`)
   - Static partition assignment
   - Per-partition rate control
   - Message generation with sequences
   - Report sent messages to coordinator

3. ✅ Worker implementation (`internal/worker`)
   - Create parti.Manager with configurable strategy
   - Use DurableHelper for consumption
   - Message validation
   - Simulated processing delay
   - Report received messages to coordinator

4. ✅ Coordinator implementation (`internal/coordinator`)
   - Message tracking (sent/received)
   - Gap/duplicate detection
   - Real-time validation
   - Abort on critical failure

5. ✅ Main binary (`cmd/simulation/main.go`)
   - Mode selection (all-in-one, producer, worker, coordinator)
   - Graceful shutdown

6. ✅ Basic configuration (`configs/dev.yaml`)
   - 100 partitions
   - 10 workers
   - Uniform weights
   - 10 msg/sec total

**Success Criteria**:
- Run for 1 hour without message loss
- Detect injected gaps/duplicates
- Clean shutdown on SIGTERM

**Testing**:
```bash
# Run in all-in-one mode
cd test/simulation
go run cmd/simulation/main.go --config=configs/dev.yaml

# Should output:
# [INFO] Starting simulation in all-in-one mode
# [INFO] Partitions: 100, Workers: 10, Rate: 0.1 msg/sec per partition
# [INFO] Started 10 producers
# [INFO] Started 10 workers
# [INFO] Started coordinator
# ... (running for 1 hour)
# [INFO] Simulation complete: 3600 messages sent, 3600 received, 0 gaps, 0 duplicates
```

---

### Phase 2: Chaos Engineering (Week 2)

**Goal**: Add chaos event injection and validation

**Deliverables**:
1. ✅ Chaos controller (`internal/coordinator/chaos.go`)
   - Random event scheduling
   - Event types:
     - `worker_crash`: Kill random worker (SIGKILL)
     - `worker_restart`: Graceful shutdown + restart
     - `scale_up`: Add N workers
     - `scale_down`: Remove N workers
     - `partition_changes`: Add/remove partitions
     - `nats_disconnect`: Simulate NATS connection loss
     - `leader_failure`: Kill current leader worker
     - `producer_crash`: Kill random producer

2. ✅ Process management
   - Spawn/kill worker processes
   - Track worker states
   - Preserve stable IDs on restart

3. ✅ Enhanced coordinator
   - Validation window (wait after chaos)
   - Detect rebalancing completion
   - Track partition migration

4. ✅ Chaos configuration (`configs/realistic.yaml`)
   - 1500 partitions
   - 100 workers
   - Exponential weights (5% extreme, 95% normal)
   - Chaos enabled
   - 15 msg/sec total

**Success Criteria**:
- Run for 8 hours with chaos events every 10-30 minutes
- No message loss during worker crashes
- No message loss during scaling events
- Successful rebalancing after partition changes
- Clean recovery from leader failure

**Testing**:
```bash
# Run realistic simulation with chaos
go run cmd/simulation/main.go --config=configs/realistic.yaml

# Expected chaos events:
# [10:15:30] [CHAOS] Injecting event: worker_crash (worker-42)
# [10:15:35] [INFO] Rebalancing detected: 15 partitions moving
# [10:16:00] [INFO] Validation: No gaps detected
# [10:25:45] [CHAOS] Injecting event: scale_up (adding 10 workers)
# [10:26:30] [INFO] Rebalancing complete: 100 -> 110 workers
# ... (8 hours of chaos)
# [18:15:30] [INFO] Simulation complete: 432000 messages, 0 gaps, 0 duplicates
```

---

### Phase 3: Advanced Features (Week 3)

**Goal**: Checkpointing, weighted distribution, enhanced metrics

**Deliverables**:
1. ✅ Checkpoint system (`internal/coordinator/checkpoint.go`)
   - Periodic state snapshots
   - Save to disk (JSON)
   - Resume from checkpoint
   - State includes:
     - Producer sequences
     - Worker assignments
     - Tracker state

2. ✅ Weight generator (`internal/producer/weight_generator.go`)
   - Exponential distribution (5% extreme, 95% normal)
   - Configurable ratios
   - Deterministic generation (same seed = same weights)

3. ✅ Enhanced metrics (`internal/metrics`)
   - Per-partition metrics
   - Per-worker metrics
   - Rebalancing metrics
   - Chaos event counters

4. ✅ Advanced configs
   - `stress.yaml`: 1500 partitions, 100 msg/sec
   - `extreme.yaml`: 1500 partitions, 1500 msg/sec

**Success Criteria**:
- Run for 24 hours with checkpoints every 30 minutes
- Resume from checkpoint after coordinator crash
- Weighted distribution verified (5% partitions get 84% of load)
- Workers with extreme partitions show higher load

**Testing**:
```bash
# Start 24-hour run
go run cmd/simulation/main.go --config=configs/stress.yaml

# After 6 hours, kill coordinator (Ctrl+C)
# Restart and resume
go run cmd/simulation/main.go --config=configs/stress.yaml --resume

# Should see:
# [INFO] Resuming from checkpoint: checkpoint_20251104_060000.json
# [INFO] Restored state: 2160000 messages sent, 2160000 received
# [INFO] Continuing simulation...
```

---

### Phase 4: Observability (Week 4)

**Goal**: Prometheus metrics, Grafana dashboards, Docker Compose

**Deliverables**:
1. ✅ Prometheus metrics exporter (`internal/metrics/prometheus.go`)
   - Standard Go metrics (goroutines, memory, GC)
   - Custom metrics:
     - `simulation_messages_sent_total{partition}` - Counter
     - `simulation_messages_received_total{partition}` - Counter
     - `simulation_message_gaps_total` - Counter
     - `simulation_message_duplicates_total` - Counter
     - `simulation_workers_active` - Gauge
     - `simulation_partitions_per_worker` - Histogram
     - `simulation_message_processing_duration_seconds` - Histogram
     - `simulation_chaos_events_total{type}` - Counter
     - `simulation_rebalancing_duration_seconds` - Histogram

2. ✅ Docker Compose setup (`docker/docker-compose.yml`)
   - NATS server (optional)
   - Prometheus
   - Grafana
   - Pre-configured dashboards

3. ✅ Grafana dashboard
   - Message throughput over time
   - Worker count over time
   - Partition distribution heatmap
   - Chaos events timeline
   - Error rate (gaps/duplicates)

4. ✅ Documentation
   - `README.md`: Usage guide
   - `ARCHITECTURE.md`: Detailed design
   - Example outputs

**Success Criteria**:
- Prometheus scrapes metrics every 15 seconds
- Grafana dashboard shows real-time state
- Can observe rebalancing events visually
- Can correlate chaos events with metrics

**Testing**:
```bash
# Start infrastructure
cd test/simulation/docker
docker-compose up -d

# Run simulation
cd ..
go run cmd/simulation/main.go --config=configs/realistic.yaml

# Access Grafana: http://localhost:3000
# Access Prometheus: http://localhost:9090
# View metrics: http://localhost:9090/metrics

# Expected Grafana panels:
# - Message Rate: 15 msg/sec steady state
# - Worker Count: 100 workers, occasional changes during scale events
# - Partition Distribution: Relatively even across workers
# - Chaos Events: Timeline with markers
# - Gaps/Duplicates: Should remain at 0
```

---

## Configuration Examples

### Development Config (`configs/dev.yaml`)

```yaml
simulation:
  duration: 1h
  mode: all-in-one

partitions:
  count: 100
  message_rate_per_partition: 0.1  # 10 msg/sec total
  distribution: uniform

producers:
  count: 10
  partitions_per_producer: 10
  rate_variation:
    enabled: false

workers:
  count: 10
  assignment_strategy: ConsistentHash
  processing_delay:
    min: 10ms
    max: 50ms

coordinator:
  validation_window: 5m

chaos:
  enabled: false

nats:
  mode: embedded
  url: "nats://localhost:4222"

metrics:
  prometheus:
    enabled: true
    port: 9090

checkpointing:
  enabled: false
```

### Realistic Config (`configs/realistic.yaml`)

```yaml
simulation:
  duration: 12h
  mode: all-in-one

partitions:
  count: 1500
  message_rate_per_partition: 0.01  # 15 msg/sec total
  distribution: exponential
  weights:
    exponential:
      extreme_percent: 0.05   # 5% of partitions (75 partitions)
      extreme_weight: 100     # Extreme partitions weight=100
      normal_weight: 1        # Normal partitions weight=1

producers:
  count: 10
  partitions_per_producer: 150
  rate_variation:
    enabled: true
    pattern: global
    min_multiplier: 0.5      # 0.005 msg/sec per partition (7.5 msg/sec total)
    max_multiplier: 2.0      # 0.02 msg/sec per partition (30 msg/sec total)
    ramp_interval: 5m

workers:
  count: 100
  assignment_strategy: WeightedConsistentHash
  strategy_config:
    virtual_nodes: 150
    overload_threshold: 1.3
    extreme_threshold: 2.0
  processing_delay:
    min: 10ms
    max: 100ms

coordinator:
  validation_window: 10m

chaos:
  enabled: true
  events:
    - worker_crash
    - worker_restart
    - scale_up
    - scale_down
    - partition_changes
    - nats_disconnect
    - leader_failure
    - producer_crash
  interval: 10-30m  # Random between 10-30 minutes

nats:
  mode: embedded
  url: "nats://localhost:4222"
  jetstream:
    max_memory: 10GB
    max_file_storage: 50GB

metrics:
  prometheus:
    enabled: true
    port: 9090

checkpointing:
  enabled: true
  interval: 30m
  path: ./checkpoints
```

### Stress Config (`configs/stress.yaml`)

```yaml
simulation:
  duration: 12h
  mode: all-in-one

partitions:
  count: 1500
  message_rate_per_partition: 0.067  # 100 msg/sec total (1 msg/sec per worker)
  distribution: exponential
  weights:
    exponential:
      extreme_percent: 0.05
      extreme_weight: 100
      normal_weight: 1

producers:
  count: 10
  partitions_per_producer: 150
  rate_variation:
    enabled: true
    pattern: global
    min_multiplier: 0.5      # 50 msg/sec total
    max_multiplier: 2.0      # 200 msg/sec total
    ramp_interval: 5m

workers:
  count: 100
  assignment_strategy: WeightedConsistentHash
  strategy_config:
    virtual_nodes: 150
    overload_threshold: 1.3
    extreme_threshold: 2.0
  processing_delay:
    min: 10ms
    max: 100ms

coordinator:
  validation_window: 10m

chaos:
  enabled: true
  events:
    - worker_crash
    - worker_restart
    - scale_up
    - scale_down
    - partition_changes
    - nats_disconnect
    - leader_failure
    - producer_crash
  interval: 10-30m

nats:
  mode: embedded
  url: "nats://localhost:4222"
  jetstream:
    max_memory: 10GB
    max_file_storage: 50GB

metrics:
  prometheus:
    enabled: true
    port: 9090

checkpointing:
  enabled: true
  interval: 30m
  path: ./checkpoints
```

---

## Testing Strategy

### Unit Tests

Each package should have comprehensive unit tests:

```
internal/config/config_test.go
  - TestLoadConfig_Valid
  - TestLoadConfig_Invalid
  - TestValidateConfig
  - TestApplyDefaults

internal/producer/producer_test.go
  - TestProducer_SendMessage
  - TestProducer_RateControl
  - TestProducer_SequenceIncrement

internal/producer/weight_generator_test.go
  - TestExponentialWeights_5Percent
  - TestExponentialWeights_Deterministic

internal/worker/worker_test.go
  - TestWorker_ProcessMessage
  - TestWorker_HandleRebalancing
  - TestWorker_GracefulShutdown

internal/coordinator/tracker_test.go
  - TestTracker_RecordSent
  - TestTracker_RecordReceived
  - TestTracker_DetectGap
  - TestTracker_DetectDuplicate

internal/coordinator/chaos_test.go
  - TestChaos_ScheduleEvents
  - TestChaos_InjectEvent
```

### Integration Tests

End-to-end scenarios in `test/simulation/integration_test.go`:

```go
// TestBasicSimulation runs a 5-minute simulation without chaos
func TestBasicSimulation(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping integration test in short mode")
    }

    cfg := &config.Config{
        Simulation: config.SimulationConfig{
            Duration: 5 * time.Minute,
            Mode:     "all-in-one",
        },
        Partitions: config.PartitionsConfig{
            Count:                   10,
            MessageRatePerPartition: 1.0,
            Distribution:            "uniform",
        },
        Workers: config.WorkersConfig{
            Count: 3,
        },
        Chaos: config.ChaosConfig{
            Enabled: false,
        },
    }

    // Run simulation
    result := runSimulation(t, cfg)

    // Verify no message loss
    assert.Equal(t, result.SentCount, result.ReceivedCount)
    assert.Equal(t, 0, result.GapCount)
    assert.Equal(t, 0, result.DuplicateCount)
}

// TestChaosSimulation runs with worker crashes
func TestChaosSimulation(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping integration test in short mode")
    }

    cfg := &config.Config{
        // ... similar to above
        Chaos: config.ChaosConfig{
            Enabled: true,
            Events:  []string{"worker_crash", "worker_restart"},
            Interval: "30s-60s",
        },
    }

    result := runSimulation(t, cfg)

    // Should survive chaos without message loss
    assert.Equal(t, result.SentCount, result.ReceivedCount)
    assert.Equal(t, 0, result.GapCount)
    assert.GreaterOrEqual(t, result.ChaosEventCount, 5) // At least 5 chaos events in 5 minutes
}
```

### Validation Tests

Special tests to verify the simulation detects issues:

```go
// TestDetectGap injects a gap and verifies detection
func TestDetectGap(t *testing.T) {
    tracker := coordinator.NewMessageTracker()

    // Send normal messages
    tracker.RecordSent(0, "producer-0", 1, 1)
    tracker.RecordReceived(0, 1)

    tracker.RecordSent(0, "producer-0", 2, 2)
    tracker.RecordReceived(0, 2)

    // Skip sequence 3 (simulate lost message)
    tracker.RecordSent(0, "producer-0", 3, 3)
    // Don't record received

    // Send sequence 4
    tracker.RecordSent(0, "producer-0", 4, 4)
    err := tracker.RecordReceived(0, 4)

    // Should detect gap
    assert.Error(t, err)
    assert.Contains(t, err.Error(), "gap detected")

    stats := tracker.GetStats()
    assert.Equal(t, 1, stats.GapCount)
}
```

---

## Success Metrics

### Correctness
- ✅ Zero message loss across all test runs
- ✅ Zero message duplication
- ✅ All chaos events handled gracefully
- ✅ Successful recovery from checkpoints

### Performance
- ✅ System throughput matches configured rate
- ✅ Message processing latency < 100ms (p99)
- ✅ Rebalancing completes within 60 seconds
- ✅ Memory usage stable (no leaks)
- ✅ Goroutine count stable (no leaks)

### Stability
- ✅ Run for 24+ hours without crashes
- ✅ No deadlocks or hangs
- ✅ Graceful shutdown completes within 30 seconds
- ✅ CPU usage < 50% during steady state

### Observability
- ✅ All metrics exposed in Prometheus
- ✅ Grafana dashboards functional
- ✅ Logs provide actionable information
- ✅ Easy to diagnose failures

---

## Timeline

| Week | Phase | Deliverables | Hours |
|------|-------|--------------|-------|
| 1 | Core Simulation | Config, Producer, Worker, Coordinator, Basic validation | 30-40 |
| 2 | Chaos Engineering | Chaos controller, Process management, Enhanced validation | 30-40 |
| 3 | Advanced Features | Checkpointing, Weight distribution, Enhanced metrics | 25-35 |
| 4 | Observability | Prometheus, Grafana, Docker Compose, Documentation | 20-30 |

**Total Estimated Time**: 105-145 hours (approximately 3-4 weeks of full-time work)

---

## Next Steps

1. ✅ Review and approve this implementation plan
2. ✅ Set up initial directory structure
3. ✅ Implement Phase 1 (Core Simulation)
4. ✅ Run initial 1-hour validation tests
5. ✅ Iterate based on findings

**Ready to start implementation?** Let's begin with Phase 1!
