# Chapter 5: Operational Scenarios

## Overview

This chapter describes key operational scenarios that demonstrate how the defender cluster behaves in real-world situations. Each scenario includes detailed timelines, sequence diagrams, failure modes, and observability guidelines.

## Scenarios

### 1. Cold Start
**File**: [`cold-start.md`](./cold-start.md)

**Description**: First deployment of the defender cluster (0 → 30 defenders)

**Key Phases**:
1. Pod creation (T+0-3s)
2. Stable ID claiming (T+3-5s)
3. Leader election (T+5-5.5s)
4. Heartbeat collection (T+5-35s) - 30s stabilization window
5. Assignment calculation (T+35-35.5s)
6. Subscription setup (T+35.5-40s)
7. Message processing begins (T+40s onward)

**Timeline**: ~45-60 seconds from first pod to full operation

**Success Criteria**:
- All 30 defenders claim unique stable IDs
- Exactly 1 leader elected
- All 2400 chambers covered with no gaps
- Message processing begins within 60 seconds

---

### 2. Scale-Up
**File**: To be created

**Description**: Adding defenders to running cluster (30 → 45 defenders)

**Key Phases**:
1. New pods created (Kubernetes scales ReplicaSet)
2. New defenders claim available stable IDs (defender-30 to defender-44)
3. New defenders join election, publish heartbeats
4. Leader detects topology change (30 → 45 defenders)
5. Stabilization window (30 seconds)
6. Leader recalculates assignments (2400 chambers → 45 defenders)
7. All defenders update subscriptions (~53 chambers per defender)

**Timeline**: ~30-40 seconds from scale command to rebalancing complete

**Impact**:
- Chamber redistribution: ~1000 chambers move (consistent hashing)
- Most chambers (60-70%) stay with original defenders (locality)
- No message loss (NATS redelivery during subscription changes)

---

### 3. Scale-Down
**File**: To be created

**Description**: Gracefully removing defenders from running cluster (30 → 20 defenders)

**Key Phases**:
1. Kubernetes sends SIGTERM to 10 defenders
2. Defenders drain in-flight messages (up to 25 seconds)
3. Defenders unsubscribe from chambers
4. Defenders release stable IDs
5. Leader detects heartbeat loss (or immediate notification)
6. Stabilization window (30 seconds after last defender leaves)
7. Leader recalculates assignments (2400 chambers → 20 defenders)
8. Remaining defenders update subscriptions (~120 chambers per defender)

**Timeline**: ~40-60 seconds from scale command to rebalancing complete

**Success Criteria**:
- All scaled-down defenders gracefully release IDs
- No message loss during drain period
- Remaining defenders handle increased load (~120 chambers each)

---

### 4. Crash Recovery
**File**: [`crash-recovery.md`](./crash-recovery.md)

**Description**: Handling unexpected defender failures

**Crash Types**:
- **Type 1**: Sudden crash (OOM kill, segfault, node failure)
- **Type 2**: Freeze/deadlock (unresponsive process)
- **Type 3**: Network partition (lost connectivity)

**Key Phases**:
1. Defender crashes (T+5s)
2. Heartbeat stops, TTL counts down (T+5-35s)
3. Heartbeat expires, leader detects crash (T+35s)
4. Emergency rebalancing (no stabilization window, T+35-38s)
5. Affected chambers redistributed to surviving defenders
6. Kubernetes detects crash, creates replacement pod (T+40s)
7. New pod claims same stable ID (T+43s)
8. New pod rejoins cluster (T+45s)
9. Final rebalancing after stabilization window (T+75-77s)

**Timeline**: ~8-10 seconds from crash to chamber recovery, ~40 seconds to full restoration

**Impact**:
- Message processing downtime: 2-3 seconds for affected chambers
- Chamber movements: ~80 chambers (crashed defender's load) + ~10-15 (recovery rebalancing)
- No message loss (NATS redelivery)

---

### 5. Rolling Update
**File**: [`rolling-update.md`](./rolling-update.md)

**Description**: Deploying new defender version with zero chamber reassignment

**Key Innovation**: Stable IDs enable zero-rebalancing updates

**Strategies**:

#### Strategy 1: Surge (Recommended)
- **Configuration**: `maxSurge: 10, maxUnavailable: 0`
- **Behavior**: Create 10 new pods → terminate 10 old pods → repeat
- **Timeline**: ~30-40 seconds (3 batches × 10-15 seconds)
- **Advantages**: Zero unavailability, faster completion

#### Strategy 2: Sequential
- **Configuration**: `maxSurge: 0, maxUnavailable: 5`
- **Behavior**: Terminate 5 old pods → create 5 new pods → repeat
- **Timeline**: ~60-75 seconds (6 batches × 10-12 seconds)
- **Advantages**: Lower resource usage (no surge)

**Key Phases**:
1. Kubernetes creates new pods (v2.2.0)
2. Old pods receive SIGTERM, drain messages, release stable IDs
3. New pods claim same stable IDs (defender-0, defender-1, ...)
4. New pods receive same assignments (no topology change)
5. New pods subscribe to same chambers (zero reassignment)
6. Repeat for all 30 defenders

**Success Criteria**:
- All 30 defenders replaced (v2.1.0 → v2.2.0)
- **Zero chamber reassignments** (assignment map version unchanged)
- Zero message loss
- 100% availability (surge strategy)
- Completion time: < 60 seconds

---

### 6. Blue-Green Deployment
**File**: To be created

**Description**: Zero-downtime deployment with parallel clusters

**Configuration**:
- **Blue cluster**: 30 defenders (v2.1.0) - currently active
- **Green cluster**: 30 defenders (v2.2.0) - being tested

**Key Phases**:
1. Deploy green cluster (30 new defenders with IDs defender-30 to defender-59)
2. Green cluster claims stable IDs, receives assignments
3. Both clusters process messages (60 defenders total, ~40 chambers each)
4. Test green cluster (canary traffic, monitoring)
5. If green cluster healthy, scale down blue cluster (60 → 30)
6. If green cluster has issues, scale down green cluster (60 → 30)

**Timeline**: ~5-10 minutes (including testing period)

**Advantages**:
- Instant rollback (scale up blue cluster if needed)
- Parallel testing with production traffic
- Zero downtime

**Challenges**:
- Higher resource usage (60 defenders during transition)
- Need stable ID range planning (0-29 vs 30-59)

---

### 7. Canary Deployment
**File**: To be created

**Description**: Gradual rollout with testing (10% → 25% → 50% → 100%)

**Key Phases**:

#### Phase 1: 10% Canary (3 defenders)
- Deploy 3 defenders with v2.2.0 (defender-30, defender-31, defender-32)
- Monitor metrics: error rate, process time, cache hit rate
- If metrics healthy, proceed to Phase 2
- If issues detected, rollback (scale down 3 canary defenders)

#### Phase 2: 25% Canary (8 defenders)
- Scale canary deployment to 8 defenders
- Continue monitoring
- If healthy, proceed to Phase 3

#### Phase 3: 50% Canary (15 defenders)
- Scale canary deployment to 15 defenders
- Continue monitoring
- If healthy, proceed to Phase 4

#### Phase 4: 100% Deployment (30 defenders)
- Scale canary deployment to 30 defenders
- Scale down blue deployment to 0
- Rolling update complete

**Timeline**: ~30-60 minutes (including monitoring between phases)

**Advantages**:
- Minimize blast radius (only 10% affected if issues)
- Gradual confidence building
- Easy rollback at each phase

---

## Scenario Comparison

| Scenario | Duration | Rebalancing? | Chamber Movements | Downtime | Risk Level |
|----------|----------|--------------|-------------------|----------|------------|
| **Cold Start** | 45-60s | Yes (initial) | 0 (initial distribution) | Full (new cluster) | Low |
| **Scale-Up** | 30-40s | Yes (planned) | ~1000 (30→45 defenders) | None | Low |
| **Scale-Down** | 40-60s | Yes (planned) | ~800 (30→20 defenders) | None | Medium |
| **Crash Recovery** | 8-10s (emergency)<br>40s (full) | Yes (emergency) | ~90 (crashed + recovery) | 2-3s (affected chambers) | High |
| **Rolling Update** | 30-60s | **No** | **0 (stable IDs)** | None (surge) | Low |
| **Blue-Green** | 5-10m | Yes (transition) | ~1200 (60→30 defenders) | None | Very Low |
| **Canary** | 30-60m | Yes (gradual) | ~400 (10%→25%→50%→100%) | None | Very Low |

## Observability During Scenarios

### Key Metrics to Monitor

| Metric | Cold Start | Scale-Up/Down | Crash | Rolling Update |
|--------|------------|---------------|-------|----------------|
| `defender_state{state="STABLE"}` | 0→30 | 30→45 or 30→20 | 30→29→30 | 30 (unchanged) |
| `defender_assignment_version` | 1 (initial) | Increments | Increments twice | **Unchanged** |
| `defender_rebalance_events_total` | +1 | +1 | +2 (emergency+recovery) | **0 (no increase)** |
| `defender_chamber_movements_total` | 0 (initial) | ~1000 | ~90 | **0 (no movements)** |
| `defender_messages_processed_total` | Starts increasing | Continuous | Brief pause | Continuous |

### Alert Thresholds

| Alert | Condition | Severity | Action |
|-------|-----------|----------|--------|
| Cluster not converging | `defender_state{state="STABLE"}` < 80% for > 5m | Critical | Check logs, failed pods |
| Excessive rebalancing | `defender_rebalance_events_total` > 5/hour | High | Investigate instability |
| Leader election failures | `defender_is_leader` sum = 0 for > 30s | Critical | Check election agent |
| High message latency | `defender_process_time_seconds` p99 > 10s | High | Check Cassandra, cache |
| Failed rolling update | `defender_version{version="new"}` stalled for > 10m | High | Check image pull, probes |

## Related Chapters

- [Chapter 3: Architecture](../03-architecture/) - High-level design and state machine
- [Chapter 4: Components](../04-components/) - Component details (defender, NATS, Cassandra)
- [Chapter 7: Monitoring](../07-monitoring/) - Prometheus metrics and Grafana dashboards

## Planned Documentation

### Scenarios to be Created
- `scale-up.md` - Detailed scale-up scenario (30→45 defenders)
- `scale-down.md` - Detailed scale-down scenario (30→20 defenders)
- `blue-green.md` - Blue-green deployment strategy
- `canary.md` - Canary deployment strategy with gradual rollout

### Additional Scenarios to Consider
- `leader-election.md` - Leader election and failover
- `network-partition.md` - NATS cluster partition handling
- `cassandra-failure.md` - Cassandra node failure recovery
- `load-spike.md` - Handling sudden traffic increase
- `multi-cluster.md` - Multi-cluster deployment (geo-distributed)
