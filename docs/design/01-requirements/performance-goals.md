# Performance Goals and SLA Targets

## Overview

This document defines detailed performance goals, SLA targets, and timing requirements for the distributed worker.

## Process Time SLA

### Target: < 5 seconds (95th percentile)

**Measurement**: Time from message receipt to acknowledgment

**Detailed Breakdown**:

| Step | Target Time | Notes |
|------|-------------|-------|
| Fetch T-chart from Cassandra | 500 ms | 95th percentile |
| Cache lookup (hit) | 1 ms | In-memory SQLite |
| Cache lookup (miss) | 100 ms | Fetch from Cassandra |
| Calculate T-charts | 1000 ms | Typical Context |
| Calculate T-charts | 3000 ms | Long Context |
| Evaluate defense strategy | 100 ms | Rule evaluation |
| Raise alarms (if needed) | 50 ms | Publish to alarm system |
| Store U-chart to Cassandra | 100 ms | Write with consistency=ONE |
| Update local cache | 20 ms | SQLite write |
| **Total (typical context)** | **~1.9 seconds** | **Typical case** |
| **Total (cache miss + long context)** | **~3.9 seconds** | **Cold cache & Long Context** |

### Performance Percentiles

| Percentile | Target | Acceptable | Critical |
|------------|--------|------------|----------|
| p50 (median) | < 1 second | < 1.5 seconds | < 2 seconds |
| p95 | < 3 seconds | < 5 seconds | < 7 seconds |
| p99 | < 5 seconds | < 8 seconds | < 10 seconds |
| p99.9 | < 8 seconds | < 12 seconds | < 15 seconds |

### Performance Degradation Triggers

**Automatic Scaling** if p95 > 5 seconds for 5 consecutive minutes
**Alert** if p99 > 10 seconds for 2 consecutive minutes
**Critical Alert** if p50 > 5 seconds

## Availability SLA

### Uptime Target: 99.9%

**Downtime Budget**:
- Per month: 43 minutes
- Per quarter: 2.16 hours
- Per year: 8.77 hours

### Failure Scenarios

| Scenario | Recovery Time | Downtime | Acceptable |
|----------|---------------|----------|------------|
| Single defender crash | 6-10 seconds | None | ✓ Yes |
| Leader defender crash | 8-12 seconds | None | ✓ Yes |
| NATS node failure | Transparent | None (automatic failover) | ✓ Yes |
| Cassandra node failure | Transparent | None (replication) | ✓ Yes |
| Cassandra cluster failure | 5 minutes(switch to standby) | **None (DCQV Low)** | ✓ Yes |
| Election Agent failure | Transparent | None (re-election) | ✓ Yes |
| Kubernetes node drain | 30-60 seconds | None (graceful shutdown) | ✓ Yes |
| Rolling update (60 defenders) | 60-300 seconds | None | ✓ Yes |

### Availability Calculation

```
Total time per month: 30 days × 24 hours × 60 minutes = 43,200 minutes
Downtime budget: 43 minutes
Uptime required: 43,200 - 43 = 43,157 minutes

Percentage: 43,157 / 43,200 = 99.9%
```

## Failover and Recovery Goals

### Defender Crash Recovery
**Detection Time**: 6 seconds (3 missed heartbeats × 2s interval)

**Redistribution Time**: 2-4 seconds (emergency rebalancing)

**Total Recovery**: 8-10 seconds

**Message Impact**: Messages queued in NATS, processed after reassignment

### Leader Failover
**Detection Time**: 10 seconds (leader election TTL)

**New Leader Takeover**: 1-2 second (assignment controller initialization)

**Total Failover**: 11-13 seconds

**Impact**: Assignment paused during failover, defenders continue processing

### Rolling Update Performance
**Per Pod Update**: 30-60 seconds

**Total Cluster Update** (50 defenders): 30-300 seconds (parallel updates)

**Zero Rebalancing**: Same defender ID = same assignments

**Cache Preservation**: Same assignments = same chambers = warm cache rebuild (not PVC-backed)

## Cache Performance Goals

### Cache Hit Rate
**Target**: > 95% after warm-up (fetch from Cassandra)

**Measurement**: `cache_hits / (cache_hits + cache_misses)`

### Cache Hit Rate by Scenario

| Scenario | Expected Hit Rate | Notes |
|----------|------------------|-------|
| Stable operation | 97-99% | Minimal assignment changes |
| After cold start | 0% → 95% | warm-up period |
| After scale up | 85-90% | Moved chambers = cache miss |
| After crash recovery | 90-95% | Only affected chambers miss |
| During rolling update | 98-99% | Same assignments = cache preserved |


## Rebalancing Performance Goals

### Rebalancing Frequency
**Target**: < 2 rebalances per day (normal operation)

**Maximum Acceptable**: < 2 rebalances per hour (during scaling events)

### Rebalancing Time

| Scenario | Target Time | Chambers Moved | Notes |
|----------|-------------|----------------|-------|
| Cold start (0 → 30) | 45-60 seconds | ~2400 (all) | Initial assignment |
| Scale up (30 → 45) | 30-40 seconds | ~500 (20%) | Incremental |
| Scale down (45 → 30) | 30-40 seconds | ~800 (33%) | Graceful removal |
| Crash recovery | 8-10 seconds | ~80 (3%) | Emergency |
| Rolling update | 0 seconds | 0 | No rebalancing |

### Chamber Movement Budget
**Target**: Move < 20% of chambers during scale up/down

**Rationale**: Minimize cache invalidation

**Enforcement**: Consistent hashing with affinity preservation

## Latency Goals

### Message Delivery Latency
**NATS JetStream**: < 10 ms (p95)

**End-to-End** (Data Collector → Defender): < 50 ms (p95)

### Cassandra Latency
**Read** (T-chart fetch): < 500 ms (p95)

**Write** (U-chart store): < 100 ms (p95)

**Consistency**: ONE (prioritize latency over consistency)

### Election Agent Latency
**Leader Acquisition**: < 100 ms (p95)

**Lock Renewal**: < 20 ms (p95)

**Re-election**: 10 seconds (after leader crash)

## Resource Utilization Goals

### Defender Pod Resources

**CPU Usage**:
- Typical load: 50-70% of allocated resources (4-11 vCores active)
- Peak load (calculation bursts): 80-90% of allocated resources (6-14 vCores active)
- Allocation: 8-16 vCores per defender

**Memory Usage**:
- Typical: 8-12 GB (cache + calculation working set)
- Peak: 12-16 GB (large context calculations + cache)
- Allocation: 8-16 GB per defender

### Cache Storage
**Per Defender**: ~100 MB (in-memory SQLite, U-chart history only)

**Growth Rate**: Minimal (only stores recent U-chart history per chamber)

**Cleanup**: Auto-eviction of oldest entries when limit reached

## Scalability Goals

### Linear Scaling
**Target**: 90-95% efficiency when scaling

**Measurement**:
```
Efficiency = (New Throughput / New Defender Count) / (Old Throughput / Old Defender Count)
```

## Monitoring and Alerting Goals

### Metrics Collection
**Scrape Interval**: 15 seconds (Prometheus)

**Retention**: 7 days (Prometheus), 1 year (long-term storage)

### Alert Response Time
**Critical Alert**: Page 3999 1st on-call engineer immediately

**Warning Alert**: Create ticket for investigation (nice to have)

**Info Alert**: Dashboard notification only

### Key Metrics SLIs (Service Level Indicators)

| Metric | SLI Target | Alert Threshold |
|--------|-----------|-----------------|
| Process time (p95) | < 5s | > 7s for 5 min |
| Cache hit rate | > 95% | < 90% for 10 min |
| Rebalance frequency | < 2/day | > 2/hour |
| Defender availability | 100% | < 95% for 5 min |

## Performance Testing Goals

### Load Testing
**Scenario**: Simulate 2000 tools × 2 chambers = 4000 chambers

**Duration**: 8 hours continuous load

**Success Criteria**:
- p95 process time < 5 seconds
- No memory leaks (stable memory usage)
- No goroutine leaks

### Stress Testing
**Scenario**: 2x peak load (3000 msg/s)

**Duration**: 30 minutes

**Success Criteria**:
- System remains stable
- p99 process time < 10 seconds
- No crashes or OOMKilled pods

### Chaos Testing
**Scenarios**:
1. Random defender crash every 5 minutes
2. Leader crash during rebalancing
3. NATS node failure
4. Cassandra node failure
5. Network partition

**Success Criteria**:
- Automatic recovery < 15 seconds
- No message loss
- No data corruption

## Related Documents

- [System Overview](./overview.md) - Key requirements and scale
- [Constraints](./constraints.md) - Technical constraints
- [Monitoring](../07-monitoring/metrics.md) - Metrics definitions
