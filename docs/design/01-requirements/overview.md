# System Overview

## System Scale

### Equipment Scale
- **Total Equipment (Tools)**: 1,500 - 2,000 units
- **Chambers per Tool**: 1-4 chambers (each processed independently)
- **Total Processing Units**: ~3,000-6,000 chambers (1,500-2,000 tools × 2-3 average chambers)

### Defender Cluster Scale
- **Minimum Configuration**: 30 defender replicas
- **Maximum Configuration**: 150 defender replicas (high-load scenarios)
- **Scaling Strategy**: Workload-dependent, dynamically adjusted via Kubernetes HPA

### Data Volume
- **Message Throughput**: 1.25 - 10 notifications per second (1,500-6,000 chambers / 600-1200 seconds average collection cycle)
- **Data Size per Batch**: 1 KB - 64 KB per batch from eqp-hub
- **Collection Duration**: 5 minutes to 2 hours (varies by equipment configuration, avg 20 minutes)
- **Daily Data Volume**: TBD, depending on configuration

## Key Performance Requirements

### Process Time
**Target**: < 5 seconds per defense process

**High-Level Process Steps**:
1. Fetch T-chart data from Cassandra
2. Retrieve historical U-chart data if needed (from cache or Cassandra)
3. Calculate T-charts or U-charts to new U-chart
4. Store U-chart result to Cassandra
5. Evaluate defense strategy rules
6. Raise alarms if thresholds exceeded
7. Update local cache

### Delivery Semantics
- **Guarantee**: At-least-once delivery
- **Processing**: Idempotent (duplicate messages handled safely)
- **Reliability**: No data loss during defender failures or rolling updates

### Availability
- **Target**: 99.9% uptime (< 43 minutes downtime per month)
- **Failover Time**: < 10 seconds for defender crash recovery
- **Rolling Update Downtime**: 30-75 seconds (zero rebalancing)

## Assignment Granularity

### Basic Assignment Unit
**Chamber-level**: `tool_id + chamber_id`

**Rationale**:
- Each chamber processes independently
- Finer-grained load balancing
- Better workload distribution across defenders

### Workload Weight Calculation

```
Weight = SVID_count × Data_Collection_Freq × Context_Duration_Seconds
```

**Example**:
- 100 SVIDs (metrics collected)
- 1 Hz collection frequency (1 data point/second)
- 1,200 seconds context duration (20 minutes)
- **Weight** = 100 × 1 × 1,200 = **120,000 data points per cycle**

**Purpose**: Weight provides a workload hint for assignment controller to balance load across defenders.

## Message Flow Architecture

### One-Directional Flow
**Data Collector → Defender**

```
EQP Hub → Data Collector → NATS JetStream → Defender → Cassandra
                                    ↓
                            (completion notification)
```

**No backward communication**:
- Defenders do not send responses to data collectors
- Data collectors fire-and-forget notifications
- Decoupled architecture for scalability

## Core Requirements Summary

| Requirement | Specification |
|-------------|---------------|
| **Scale** | 1,500-2,000 tools, 30-150 defenders |
| **Assignment Unit** | Chamber-level (tool_id + chamber_id) |
| **Process Time** | < 5 seconds per cycle |
| **Throughput** | 1.25-10 messages/sec  |
| **Data Volume** | Vary |
| **Context Duration** | 5 minutes - 2 hours |
| **Message Flow** | One-directional (Collector → Defender) |
| **Delivery** | At-least-once (idempotent) |
| **Coordination** | Self-coordinating (no Zookeeper/Consul) |
| **Failover** | < 10 seconds |
| **Availability** | 99.9% uptime |

## Non-Functional Requirements

### Scalability
- Horizontal scaling via Kubernetes HPA
- Linear performance scaling (2x defenders = 2x capacity)
- No central bottleneck in architecture

### Reliability
- Automatic failover on defender crash
- Zero data loss during failures
- Graceful degradation during scaling events

### Maintainability
- Self-coordinating (minimal operational overhead)
- Observable via Prometheus metrics
- Configuration-driven behavior

### Performance
- Cache hit rate > 95% (after warm-up)
- Assignment rebalancing < 60 seconds
- Rolling updates without rebalancing

## Related Documents

- [Constraints](./constraints.md) - Technical constraints and existing infrastructure
- [Performance Goals](./performance-goals.md) - Detailed SLA targets and timing requirements
- [High-Level Architecture](../03-architecture/high-level-design.md) - System architecture overview
