# Kafka Limitations for Dynamic Defender Cluster

## Overview

This document explains why Apache Kafka is not suitable for the distributed system, focusing on the pain points of Kafka's consumer group rebalancing mechanism when dealing with topology changes.

*The critical pain points are rolling update and code start stability.*

## System Context

**Key Characteristics**:
- **Message Throughput**: 1.25-10 messages/sec (LOW throughput, high latency sensitivity)
- **Total Chambers**: 1,500-6,000 chambers (each needs independent processing)
- **Topology Changes**: Frequent (scaling, rolling updates, failures)
- **Latency SLA**: < 5 seconds per worker defense process

## Kafka-Based Partition Design

### Architecture Overview

In a practical Kafka-based design, we will not create 1,500-6,000 partitions (one per chamber), as this would lose the meaning of "partition" and create massive operational overhead. Instead:

**Current Design**:
```
Kafka Topic: equipment events
Partitions: 64 (or 32-128, configurable)
Rellancing: Incremental Cooperative Rebalancing
Consumer Group: defenders
Replicas: 64 defenders (SAME as partition count)

Assignment Strategy:
├─ 1:1 mapping between defender and partition
│  Example: defender-0 consumes partition-0
│           defender-1 consumes partition-1
│           ...
│           defender-63 consumes partition-63
│
└─ Each defender processes all chambers in its partition
   Example: Partition-0 contains messages for chambers:
            [tool001:1, tool005:2, tool012:3, ..., tool089:4]
            Defender-0 processes all of these chambers sequentially
```

### How It Works

**Message Publishing Pesudo Code**:
```go
// Data collector publishes message
msg := CompletionMessage{
    ToolID:    "tool001",
    ChamberID: "1",
    ContextID:   "12345678",
}

// Kafka partitions by tool_id + chamber_id
partitionKey := fmt.Sprintf("%s:%s", msg.ToolID, msg.ChamberID) // consistent hashing
kafka.Publish("equipment-events", partitionKey, msg)
// → Message goes to Partition-17 (determined by hash(partitionKey) % 64)
```

**Message Consumption Pesudo Code**:
```go
// Receives message from Partition-0
msg := <-consumer.Messages()

process(msg)
```

### Partition Distribution Example

With **64 partitions** and **1,500-6,000 chambers**:

```
Each partition contains: 1,500-6,000 / 64 ≈ 23-94 chambers

Partition-0: [tool001:1, tool003:2, tool007:1, ..., tool089:3]  (23-94 chambers)
Partition-1: [tool002:1, tool004:3, tool009:2, ..., tool091:1]  (23-94 chambers)
...
Partition-63: [tool005:2, tool011:1, tool015:4, ..., tool099:2] (23-94 chambers)
```

With **64 defenders** (1:1 mapping):
```
Defender-0  consumes Partition-0  → processes 23-94 chambers
Defender-1  consumes Partition-1  → processes 23-94 chambers
...
Defender-63 consumes Partition-63 → processes 23-94 chambers
```

### Why This Design?

**Pros**:
- Reasonable partition count (64 vs 6,000)
- Kafka manages partition assignment automatically
- Leverages Kafka's durability and replication
- Standard Kafka consumer group pattern

**Cons** (detailed in following sections):
- **Fixed scaling**: Replica count must equal partition count (64 partitions = 64 defenders, cannot scale to 30 or 100)
- **Stop-the-world rebalancing**: All consumers pause during topology changes
- **Frequent rebalancing**: Every defender join/leave triggers cluster-wide rebalance
- **Coarse-grained load balancing**: Each partition contains 23-94 chambers with different weights
- **No weighted load balancing**: Cannot assign based on chamber weight, some defenders overloaded

## Core Problem: Consumer Group Rebalancing

### What is Consumer Group Rebalancing?

When consumers join or leave a Kafka consumer group, the Kafka broker automatically redistributes partitions** among all consumers. This process is called **rebalancing**.

**Rebalancing Triggers**:
1. New consumer joins the group
2. Existing consumer leaves (graceful shutdown)
3. Consumer crashes (heartbeat timeout)
4. Consumer session timeout
5. Partition count changes

### Why Rebalancing is Painful

**Two Rebalancing Protocols**:

1. **Eager Rebalancing** (older, default before Kafka 2.4):
   - **ALL consumers** stop processing during rebalance
   - Surrender all partitions, then receive new assignments
   - Stop-the-world behavior

2. **Cooperative (Incremental) Rebalancing** (Kafka 2.4+, What We Choose):
   - Only affected consumers stop processing
   - Consumers surrender ONLY partitions being moved
   - Unaffected partitions continue processing
   - **Still requires coordination and multiple rounds**

**Even with Cooperative Rebalancing**:
- Duration: 2-5 seconds per rebalance (improved, but still exists)
- Multiple rounds of coordination required
- Affected partitions still pause during movement
- Does NOT eliminate rebalancing, just reduces impact

**Impact Multiplier**:
- Multiple rebalances during cold start (one per defender joining)
- Cascading rebalances during scaling events
- Rebalances during rolling updates

## Pain Point #1: Cold Start (0 → 64 Defenders)

### Problem

When starting 64 defenders simultaneously (matching 64 partitions):

**Timeline (Cooperative Rebalancing)**:
```
T+0s:     Defender-0 joins → Rebalance #1 (2-5s, minimal disruption)
T+1s:     Defender-1 joins → Rebalance #2 (2-5s, only affected partitions pause)
T+2s:     Defender-2 joins → Rebalance #3 (2-5s, incremental partition movement)
...
T+63s:    Defender-63 joins → Rebalance #64 (2-5s, final adjustment)

Total rebalancing time: 64 × 2-5s = 128-320 seconds (2.1-5.3 minutes)
```

**Impact**:
- 2.1-5.3 minutes before cluster is fully operational and stable
- **160-1,600 messages** affected (at 1.25-5 msg/sec × 128-320s)
- **64 rebalancing coordination rounds**
- Complexity: Requires multiple coordination rounds per rebalance
- Much slower than single assignment calculation

### NATS JetStream Solution

**Timeline**:
```
T+0s:     Defender-0 becomes leader, starts assignment controller
T+0-30s:  Defenders 1-29 join gradually, publish heartbeats
T+30s:    Leader performs SINGLE assignment calculation
T+31s:    All defenders subscribe to their assigned chambers

Total time to operational: 31 seconds (2x-10x faster)
```

**Benefits**:
- Single assignment event per 30 seconds(not 30 rebalances)
- No stop-the-world pauses, the leader can start processing requests immedially
- Defenders can start processing immediately after assignment
- Batched decision-making (stabilization window)

## Pain Point #2: Cannot Scale Independently

### Problem

**Kafka Constraint**: Defender count MUST equal partition count (64 defenders for 64 partitions)

**Scenario 1: Want to scale down to 30 defenders** (reduce cost during low load):
```
IMPOSSIBLE: 30 defenders cannot consume 64 partitions with 1:1 mapping Would require multiple partitions per defender (breaks 1:1 design)
Or must reduce partition count → requires topic recreation
```

**Scenario 2: Want to scale up to 100 defenders** (handle high load):
```
IMPOSSIBLE: 100 defenders cannot map to 64 partitions with 1:1 mapping Would require creating more partitions.
Requires topic recreation Or 36 defenders sit idle (waste resources)
```

**Impact**:
- **No dynamic scaling**: Stuck with 64 defenders or need expensive topic recreation
- **Cannot handle increasing load**: Cannot scale up to 100 during high load
- **Inflexible**: Partition count decision made at design time, cannot change

### NATS JetStream Solution

**Dynamic Scaling: 30 ↔ 100 defenders without constraints**

**Scale Down (64 → 30 defenders)**:
```
T+0-30s:  34 defenders terminate gracefully.
          Leader recalculates assignments immedially when defender terminated.
          Remaining 30 defenders update subscriptions
T+31s:    System operational with 30 defenders

Each defender now handles: 1,500-6,000 / 30 = 50-200 chambers (vs 23-94 previously)
```

**Scale Up (64 → 100 defenders)**:
```
T+0-30s:  36 new defenders join, publish heartbeats
T+30s:    Leader recalculates chamber assignments (100 defenders, 1,500-6,000 chambers)
T+31s:    All defenders update subscriptions
T+32s:    System operational with 100 defenders

Each defender now handles: 1,500-6,000 / 100 = 15-60 chambers (lighter load)
```

**Benefits**:
- **No partition constraint**: Can scale to any number (30, 64, 100, etc.)
- **Workload-dependent scaling**: Scale based on actual load, not partition count
- **Cost optimization**: Scale down during low load
- **Performance optimization**: Scale up during increasing load

## Pain Point #3: Defender Crash

### Problem

When a defender crashes:

**Kafka Behavior (Cooperative Rebalancing)**:
```
T+0s:     Defender-15 crashes (was consuming Partition-15)
T+10s:    Kafka detects missing heartbeat
T+10s:    Rebalance triggered, only Partition-15 needs reassignment
T+10-18s: 62 defenders continue processing uninterrupted
          1 defender (e.g., Defender-16) pauses to accept Partition-15
T+18s:    Rebalancing complete, Defender-16 now handles 2 partitions

Total downtime: 8-10 seconds (only for Partition-15, ~23-94 chambers affected)
```

**Impact**:
- Only 1 defender pauses (Defender-16 accepting new partition)
- 62 defenders continue processing uninterrupted
- Still 8-10 seconds recovery time (vs 8s target)
- Unbalanced load: Defender-16 now handles 2 partitions (46-188 chambers)
- Still requires rebalancing coordination

### NATS JetStream Solution

**Timeline** (30-defender configuration):
```
T+0s:     Defender-15 crashes
T+6s:     Leader detects missing heartbeat (3 missed × 2s)
T+6s:     Leader enters EMERGENCY state
T+7s:     Leader redistributes 50-200 chambers to remaining 29 defenders
T+8s:     Affected defenders subscribe to new chambers

Total: 8 seconds, only affected defenders update
```

**Benefits**:
- Only 29 defenders pick up ~2-7 extra chambers each (evenly distributed)
- Other defenders continue processing uninterrupted
- Isolated failure impact
- Balanced load: th 29 defender share the extra loading
- equal recovery time (8s vs 8-10s)

## Pain Point #4: Rolling Update (CRITICAL PART)

### Problem

Updating 64 defenders from v1.0 to v2.0:

**Kafka Behavior (Cooperative Rebalancing)**:
```
Update defender-1:
  T+0s:   Defender-1 v1.0 leaves → Rebalance #1 (only Partition-1 affected, 2-5s)
  T+5s:   Defender-1 v2.0 joins → Rebalance #2 (only Partition-1 affected, 2-5s)

Update defender-2:
  T+10s:  Defender-2 v1.0 leaves → Rebalance #3 (only Partition-2 affected, 2-5s)
  T+15s:  Defender-2 v2.0 joins → Rebalance #4 (only Partition-2 affected, 2-5s)

...

Total: 128 rebalances (2 per defender × 64)
Duration: 128 × 2-5s = 256-640 seconds (4.3-10.7 minutes)
Improvement: 50% faster, but STILL 128 coordination rounds!
```

**Impact (Cooperative Rebalancing)**:
- Only affected partitions pause during each update
- 4.3-10.7 minutes
- **Still 128 rebalancing coordination rounds** (2 per defender)
- **640-3,200 messages** affected (at 2.5 msg/sec × 256-640s)
- Cache invalidation: Partition-1 moves to temp defender, then back (cache lost twice)
- Unacceptable for production (4-10 minutes vs < 3 minute target)

### NATS JetStream Solution

**Timeline** (30-defender configuration):
```
T+0s:     Defender-1 v1.0 gracefully shuts down
T+5s:     Defender-1 v2.0 starts with SAME ID
T+6s:     Leader detects version change, NO REBALANCING
T+7s:     Defender-1 v2.0 resumes same chamber assignments

(Repeat for all 30 defenders, can be parallel or sequential)

Total: 30-150 seconds, ZERO rebalances
```

**Benefits**:
- Zero rebalancing events (vs 128 with Kafka)
- Same defender ID = same assignments
- Graceful, low-impact updates
- **20-80x faster** (30-150s vs 640-1,280s)
- Possible: Local Cache(SQLite) preserved (if we use PVC, but it might not a good idea)

## Pain Point #5: Load Imbalance Due to Coarse-Grained Partitioning

### Problem

**Kafka Constraint**: Each partition contains 23-94 chambers with DIFFERENT weights

**Architecture**:
```
Kafka Topic: equipment-events (64 partitions)
  ↓
64 Defenders (1:1 mapping)
  ↓
Defender-0 → Partition-0 → 23-94 chambers
Defender-1 → Partition-1 → 23-94 chambers
...
```

**Core Problem**: **Chambers have different weights(context size), but partitions treat them equally**

**Chamber Weight Formula**:
```
Weight = SVID_count × Data_Collection_Freq × Context_Duration_Seconds

Example chambers:
- tool001:1 → 100 SVIDs × 10 Hz × 1,200s = 1,200,000 (high weight)
- tool002:1 → 50 SVIDs × 1 Hz × 600s = 30,000 (low weight)
```

**What Happens with Hash-Based Partitioning**:
```
Partition-0: [tool001:1 (1.2M), tool002:1 (30K), tool003:1 (50K), ...]
             Total weight: 2,500,000 (HEAVY)

Partition-1: [tool010:1 (40K), tool011:1 (35K), tool012:1 (45K), ...]
             Total weight: 800,000 (LIGHT)

Load imbalance: Partition-0 is 3x heavier than Partition-1
                But Defender-0 and Defender-1 have same resources!
```

**Example**:
```
Defender-0 handles Partition-0 (2.5M weight):
 Overloaded, high latency, potential timeouts
 Cannot offload heavy chamber (tool001:1) to another defender
 Stuck with this partition permanently

Defender-1 handles Partition-1 (800K weight):
   Underutilized, has spare capacity
 Cannot accept chambers from Defender-0 (different partition)
```

**Impact**:
- **Load imbalance**: Some defenders overloaded, others idle (due to random hash distribution)
- **Cannot rebalance**: Moving partition = moving ALL 23-94 chambers (too disruptive)
- **Resource waste**: Cannot utilize spare capacity in underloaded defenders
- **Performance degradation**: Overloaded defenders cause high latency

### NATS JetStream Solution

**Fine-Grained Subject-Based Routing**:
```
Each chamber has dedicated subject: dc.{tool_id}.{chamber_id}.completed

Assignment controller can move individual chambers:
  tool001:1 → defender-3
  tool001:2 → defender-7
  tool002:1 → defender-3
  (each chamber independently assigned)
```

**Benefits**:
- **Chamber-level assignment**: Move individual chambers between defenders
- **Weighted consistent hashing**: Assign based on chamber weight (SVID_count × freq × duration)
- **Fine-grained load balancing**: Redistribute single high-weight chamber to balance load
- **Cache-friendly**: Only moved chambers invalidate cache, others preserved


## Summary of Kafka Constraints

### Fundamental Limitations (Even with Cooperative Rebalancing)

**1:1 Partition-Defender Mapping Creates**:

1. **Fixed Scaling**:
   - Must have exactly 64 defenders (matching 64 partitions)
   - Cannot scale to 30 (cost optimization) or 100 (peak load)
   - Partition count decision locked at design time

2. **Load Imbalance**:
   - Hash-based partitioning distributes chambers randomly
   - Some partitions heavy (2.5M weight), others light (800K weight)
   - Cannot move individual chambers to balance load

3. **Massive Rebalancing Overhead**:
   - Cold start: 64 rebalances × 5-10s = 320-640 seconds
   - Rolling update: 128 rebalances × 5-10s = 640-1,280 seconds
   - Crash recovery: 20-30 seconds cluster-wide pause

4. **Disproportionate to Workload**:
   - System processes only 1.25-5 messages/sec
   - Rebalancing time >> actual processing time
   - 640s rebalancing to handle 1,600-3,200 messages that would take 320-640s to process normally

### NATS JetStream Solution

**Fine-Grained, Dynamic Assignment**:
- Scale to any replica count (30, 64, 100) independently
- Weighted consistent hashing balances load by chamber weight
- Single assignment event (batched topology changes)
- Zero rebalancing overhead during rolling updates
- Rebalancing proportional to workload (not replica count)

## Comparison Summary

| Aspect | Kafka Eager | Kafka Cooperative | NATS JetStream |
|--------|-------------|-------------------|----------------|
| **Replica Count** | Fixed at 64 | Fixed at 64 | Flexible (30-100) |
| **Cold Start** | 320-640s (64 rebal, all stop) | 128-320s (64 rebal, incremental) | 31s (1 assignment) |
| **Scale Up/Down** | IMPOSSIBLE | IMPOSSIBLE | 32s (1 assignment) |
| **Crash Recovery** | 20-30s (all stop) | 8-10s (1 partition affected) | 8s (isolated) |
| **Rolling Update** | 640-1,280s (128 rebal, all stop) | 256-640s (128 rebal, incremental) | 30-150s (0 rebalances) |
| **Assignment Granularity** | Partition (23-94 chambers) | Partition (23-94 chambers) | Chamber (1 chamber) |
| **Load Balancing** | Hash-based (unbalanced) | Hash-based (unbalanced) | Weighted (balanced) |
| **Stop-the-World** | Yes (all consumers) | No (affected only) | No |
| **Rebalancing Events** | 64-128 events | 64-128 events | 0-1 events |
| **Improvement vs Eager** | Baseline | 50% faster, better isolation | 10-40x faster, zero rebal |

### NATS JetStream Advantages

#### 1. No Consumer Group Rebalancing
- Assignment controlled by leader defender
- No automatic broker-side rebalancing
- Explicit, controlled assignment changes

#### 2. Subject-Based Routing
- One subject per chamber
- Defenders subscribe to specific subjects
- Dynamic subscription changes

#### 3. Batch Assignment
- Leader collects topology changes (stabilization window)
- Single assignment calculation
- Minimizes churn

### 4. State Machine Intelligence
- Distinguishes cold start vs scale up vs rolling update
- Different strategies for different scenarios
- Prevents thrashing

### 5. Cache Affinity
- Consistent hashing preserves assignments
- 80%+ chambers unchanged during scale up
- 100% preserved during rolling update


## Related Documents

- [Operational Challenges](./operational-challenges.md) - Detailed scenario analysis
- [Comparison Matrix](./comparison-matrix.md) - Feature-by-feature comparison
- [NATS JetStream Setup](../04-components/message-broker/nats-jetstream-setup.md) - NATS configuration
