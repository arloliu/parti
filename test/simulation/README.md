# Parti Simulation

Long-running simulation to validate message delivery correctness and partition rebalancing behavior under realistic workloads.

## Overview

This simulation tests parti's core guarantees:
- **No message loss**: Every sent message must be received exactly once
- **No duplication**: No message should be received more than once
- **Stable assignments**: Workers maintain partition locality during rebalancing

## Architecture

```
Producer → NATS JetStream → Worker (parti.Manager + DurableHelper) → Coordinator
   ↓                           ↓                                        ↓
Reports sent               Consumes &                              Validates
messages                   reports received                        correctness
```

## Quick Start

### Build

```bash
cd test/simulation
go build -o bin/simulation ./cmd/simulation
```

### Run (All-in-One Mode)

```bash
# Development config: 100 partitions, 10 workers, 10 msg/sec
./bin/simulation --config configs/dev.yaml
```

### Run (Separate Processes)

```bash
# Terminal 1: Coordinator
./bin/simulation --config configs/dev.yaml &

# Terminal 2-6: Producers (5 total)
PRODUCER_ID=producer-0 ./bin/simulation --config configs/dev.yaml &
PRODUCER_ID=producer-1 ./bin/simulation --config configs/dev.yaml &
# ... etc

# Terminal 7-16: Workers (10 total)
WORKER_ID=worker-0 ./bin/simulation --config configs/dev.yaml &
WORKER_ID=worker-1 ./bin/simulation --config configs/dev.yaml &
# ... etc
```

## Configuration

See `configs/dev.yaml` for a complete example. Key parameters:

### Partitions
- `count`: Total number of partitions (e.g., 1500 for production scale)
- `message_rate_per_partition`: Messages per second per partition (e.g., 0.01 = 15 msg/sec total for 1500 partitions)
- `distribution`: "uniform" or "exponential" weight distribution

### Producers
- `count`: Number of producer processes
- Static partition assignment (equal distribution)

### Workers
- `count`: Number of worker processes
- `assignment_strategy`: "WeightedConsistentHash", "ConsistentHash", or "RoundRobin"
- `processing_delay`: Simulated message processing time

### NATS
- `mode`: "embedded" (in-process) or "external" (separate server)
- `url`: NATS connection URL (for external mode)

## Monitoring

The simulation prints periodic reports:

```
=== Simulation Report ===
Total Partitions: 100
Total Sent:       12500
Total Received:   12498
Gaps Detected:    0
Duplicates:       0
✅ SUCCESS: No message loss or duplication detected
```

## Implementation Status

### ✅ Phase 1: Core Simulation (COMPLETE)
- [x] Configuration system (YAML with validation)
- [x] Producer with static partition assignment & weight distribution
- [x] Worker with parti.Manager + DurableHelper integration
- [x] Coordinator with gap/duplicate detection
- [x] All-in-one mode for easy testing
- [x] Embedded NATS support
- [x] Development config (100 partitions, 10 workers)

### ✅ Phase 2: Chaos Engineering (COMPLETE)
- [x] Chaos controller with event scheduling
- [x] Process manager for dynamic worker lifecycle
- [x] Chaos events: worker_crash, worker_restart, scale_up, scale_down, leader_failure
- [x] Configurable chaos intervals (e.g., 10-30 minutes)
- [x] Integration with runAllInOne mode

### ✅ Phase 3: Advanced Features (COMPLETE)
- [x] Checkpoint system for state persistence
- [x] Exponential weight generator (5% extreme, 95% normal)
- [x] Weighted partition assignment
- [x] Stress config (1500 partitions, 100 msg/sec)
- [x] Extreme config (1500 partitions, 1500 msg/sec)
- [x] Resume from checkpoint support

### ✅ Phase 4: Observability (COMPLETE)
- [x] Prometheus metrics collector
- [x] Prometheus HTTP server integration
- [x] Docker Compose stack (NATS, Prometheus, Grafana)
- [x] Pre-configured Grafana dashboard
- [x] System metrics (goroutines, memory)
- [x] Real-time monitoring of message flow

## Quick Start Guides

### 1. Basic Verification (2 minutes)
```bash
# Run quick test with chaos
./quick-test.sh

# Expected output:
# ✓ Simulation started
# ✓ Chaos controller started
# ✓ Chaos events injected: 3 events
# ✓✓✓ SUCCESS: No message loss or duplication!
```

### 2. Observability Stack (with Grafana)
```bash
# Start Docker stack
cd docker
docker-compose up -d

# Run simulation with metrics
cd ..
./bin/simulation -config configs/realistic.yaml

# View dashboards
# - Grafana: http://localhost:3000 (admin/admin)
# - Prometheus: http://localhost:9090
# - NATS: http://localhost:8222
```

### 3. Long-Running Stress Test
```bash
# 12-hour test with 1500 partitions, chaos, checkpoints
./bin/simulation -config configs/stress.yaml

# Features:
# - Checkpoints every 15 minutes
# - Chaos events every 5-15 minutes
# - Exponential weight distribution (5% hot partitions)
# - 100 workers, 10 producers
# - ~100 msg/sec throughput
```

## Configuration Examples

### Development (Quick Testing)
```yaml
simulation:
  duration: 1h
  mode: all-in-one

partitions:
  count: 100
  message_rate_per_partition: 0.1
  distribution: uniform

workers:
  count: 10

chaos:
  enabled: false

metrics:
  prometheus:
    enabled: false
```

### Realistic (8-Hour Test)
```yaml
simulation:
  duration: 8h
  mode: all-in-one

partitions:
  count: 1500
  message_rate_per_partition: 0.01  # 15 msg/sec total
  distribution: exponential
  weights:
    exponential:
      extreme_percent: 0.05    # 5% hot partitions
      extreme_weight: 100
      normal_weight: 1

workers:
  count: 100
  assignment_strategy: WeightedConsistentHash

chaos:
  enabled: true
  events:
    - worker_crash
    - worker_restart
    - scale_up
    - scale_down
  interval: 10m-30m

metrics:
  prometheus:
    enabled: true
    port: 9091

checkpoint:
  enabled: true
  interval: 30m
  path: "./checkpoints"
```

## Testing Strategy

### Development Testing (1-2 hours)
```bash
./bin/simulation -config configs/dev.yaml
```
**Goal**: Verify basic functionality and catch regressions quickly

### Production Testing (8-24+ hours)
```yaml
partitions:
  count: 1500
  message_rate_per_partition: 0.01  # ~15 msg/sec total
workers:
  count: 100
```
**Goal**: Validate long-term stability and rare edge cases

## Architecture Details

### Message Flow
1. **Producer** generates messages with dual sequences:
   - Partition sequence: Per-partition monotonic counter (gap detection)
   - Producer sequence: Per-producer monotonic counter (debugging)
2. **NATS JetStream** provides durable storage and delivery guarantees
3. **Worker** consumes via parti.Manager for dynamic assignment:
   - Subscribes to assigned partitions only
   - Updates subscriptions on rebalancing
   - Reports received messages to coordinator
4. **Coordinator** validates correctness:
   - Tracks expected vs actual sequences per partition
   - Detects gaps (message loss)
   - Detects duplicates (redelivery issues)

### Partition Assignment
- **Static (Producers)**: Fixed partition ranges per producer
  - Producer 0: partitions 0-299
  - Producer 1: partitions 300-599
  - etc.
- **Dynamic (Workers)**: parti.Manager handles assignment
  - WeightedConsistentHash for cache affinity
  - Automatic rebalancing on worker changes
  - Emergency mode for failure recovery

### Weight Distribution
- **Uniform**: All partitions have weight 1 (balanced load)
- **Exponential**: 5% extreme (weight 100), 95% normal (weight 1)
  - Tests weighted assignment strategies
  - Simulates real-world hotspot scenarios

## Files

```
test/simulation/
├── cmd/
│   └── simulation/
│       └── main.go                  # Main entry point
├── internal/
│   ├── config/                      # YAML configuration
│   │   ├── config.go
│   │   ├── defaults.go
│   │   └── validation.go
│   ├── producer/                    # Message generation
│   │   ├── message.go
│   │   ├── producer.go
│   │   └── weight_generator.go
│   ├── worker/                      # Message consumption
│   │   └── worker.go
│   ├── coordinator/                 # Validation & tracking
│   │   ├── coordinator.go
│   │   └── tracker.go
│   └── natsutil/                    # NATS utilities
│       └── embedded.go
├── configs/
│   └── dev.yaml                     # Development config
├── bin/
│   └── simulation                   # Compiled binary
└── IMPLEMENTATION_PLAN.md           # 4-phase roadmap
```

## Contributing

See `IMPLEMENTATION_PLAN.md` for the complete development roadmap and future enhancements.
