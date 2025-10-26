# Technical Constraints

## Overview

This document outlines the technical constraints and existing infrastructure that shape the design of the tFDC System.

## Existing Infrastructure & Libraries

### Election Agent

**Description**: Leader election service using distributed locking mechanism

**Integration**:
- Defenders use gRPC client to acquire leadership
- Provides split-brain prevention
- Automatic lock expiration and re-election

### Mebo

**Description**: Space-efficient compression library for packing multiple SVIDs and their data points into compact blob format.

**Library URL**: https://github.com/arloliu/mebo

**Integration**:
- Data Collector uses Mebo to pack SVID time-series data
- Packed Mebo blob stored as T-chart in Cassandra
- Multiple Mebo metric blobs can belong to one context (if context spans multiple storage chunks)
- Average compression: ~3-4 bytes per data point

### Cassandra
**Purpose**: Persistent storage for T-charts and U-charts

**Schema**:
- T-charts: Raw time-series data from equipment
- U-charts: Statistical results

**Scale**:
- Multi-datacenter replication
- Handles 50-200 GB daily data volume
- Optimized for time-series workloads

### Kubernetes
**Purpose**: Container orchestration and scaling

**Features Used**:
- StatefulSet for defender pods (only for stable network identity, defender is stateless design)
- HPA (Horizontal Pod Autoscaler) for dynamic scaling
- Rolling update strategy
- Downward API for pod metadata

## Architectural Constraints

### No External Coordinators
**Requirement**: Self-coordinating without Zookeeper or Consul

**Rationale**:
- Reduce operational complexity
- Minimize external dependencies
- Improve system reliability

**Solution**: Embedded assignment controller in leader defender process

### Message Broker Choice
**Selected**: NATS JetStream

**Constraints**:
- Must support dynamic subject-based routing
- No consumer group rebalancing overhead
- Built-in key-value store for state
- Kubernetes-native deployment

**Why Not Kafka?**:
- Kafka consumer group rebalancing causes stop-the-world pauses
- Poor fit for frequent topology changes (scaling, rolling updates)
- See [Kafka Limitations](../02-problem-analysis/kafka-limitations.md)

### At-Least-Once Delivery
**Constraint**: System must guarantee at-least-once delivery semantics

**Implications**:
- Defender processing must be idempotent
- Duplicate messages handled safely
- Message acknowledgment after successful processing

## Performance Constraints

### Process Time Budget
**Target**: < 5 seconds per cycle

**Breakdown** (typical scenario):
1. Fetch T-chart from Cassandra: ~700 ms
2. Check cache for U-chart history: ~10 ms (hit) or ~300 ms (miss)
3. Calculate U-chart by EQ: ~200 ms
4. Evaluate defense strategy: ~100 ms
5. Store U-chart to Cassandra: ~300 ms
6. Update cache: ~20 ms

**Total**: ~1,330 ms (cache hit) to ~1,620 ms (cache miss)

**Buffer**: ~3.4-3.7 seconds for network variance and GC pauses

**Throughput Capacity**:
- **Message Rate**: 1.25-5 messages/sec (1,500-6,000 chambers / 1,200 seconds avg cycle)
- **Per Defender**: ~0.042-0.167 messages/sec per defender (30 defenders)
- **Peak Load**: ~0.05-0.06 messages/sec per defender (100 defenders)

With average 1.5s process time, each defender can handle ~0.67 messages/sec, providing significant headroom.

### Cache Hit Rate Requirement
**Target**: > 95% after warm-up

**Implication**: Chamber assignments should remain stable to maintain cache locality

**Solution**: Consistent hashing with affinity preservation

### Rebalancing Frequency Limit
**Constraint**: Minimize rebalancing events

**Target**: < 2 rebalances per hour under normal conditions

**Rationale**: Each rebalance causes cache invalidation and subscription churn

## Operational Constraints

### Deployment Strategy Requirements

**Context**: Data collectors and defenders release new versions regularly (weekly/monthly)

**Mandatory Capabilities**:

1. **Rolling Update** (Standard Updates)
   - Zero downtime during version upgrades
   - Update all defenders without service interruption
   - Complete cluster update in < 3 minutes (30 defenders × 5-10s per pod)
   - **Constraint**: Same defender ID must retain same assignments during update

2. **Blue-Green Deployment** (Major Releases)
   - Run old and new versions simultaneously for validation
   - Support full traffic split (e.g., 30 old + 30 new = 60 defenders)
   - Instant cutover or rollback capability
   - **Use case**: Major version changes, schema migrations, risky releases

3. **Canary Deployment** (Risk Mitigation)
   - Deploy small percentage of new version (e.g., 10% = 3 defenders)
   - Monitor canary metrics vs stable fleet
   - Progressive rollout if canary succeeds
   - Quick rollback if canary fails
   - **Use case**: Gradual validation, A/B testing, performance testing

4. **Progressive Rollout** (Gradual Migration)
   - Support incremental scaling: 10% → 25% → 50% → 100%
   - Pause at each stage for validation
   - Automatic or manual progression
   - **Use case**: Large-scale changes, confidence building

**Technical Implications**:
- Must support dynamic defender count (not fixed to partition count like Kafka)
- Must support stable defender IDs for zero-reassignment rolling updates
- Must work with standard Kubernetes Deployment (not limited to StatefulSet)
- Must integrate with GitOps tools (Argo Rollouts, Flagger)

### Graceful Shutdown
**Requirement**: 30-second termination grace period

**Process**:
1. Receive SIGTERM from Kubernetes
2. Stop accepting new messages
3. Finish processing in-flight messages
4. Unsubscribe from NATS subjects
5. Release stable ID claim (if using ID pool)
6. Release leader lock (if leader)
7. Exit process
5. Release leader lock (if leader)
6. Exit process

### Observability Requirement
**Constraint**: Must integrate with existing monitoring stack

**Stack**:
- Prometheus for metrics collection
- Grafana for dashboards
- AlertManager for alerting

**Metrics Required**:
- Cluster state machine transitions
- Assignment rebalancing events
- Process time percentiles (p50, p95, p99)
- Cache hit rate
- Message throughput

## Resource Constraints

### Defender Pod Resources
**Minimum**:
```yaml
requests:
  memory: "8Gi"
  cpu: "8000m"  # 8 vCores
```

**Maximum**:
```yaml
limits:
  memory: "16Gi"
  cpu: "16000m"  # 16 vCores
```

**Rationale**:
- U-chart calculations are CPU and memory intensive
- Each defender processes 50-200 chambers (at 30 defenders) with complex statistical calculations
- Leader defender needs same resources (assignment controller overhead ~100 MB memory, ~50m CPU should be enough)
- High CPU allocation enables parallel processing of multiple chamber calculations### Cache Storage
**Per Defender**: ~100 MB (in-memory SQLite)

**Purpose**: Store U-chart history only (not T-chart data)

**Rationale**:
- Stateless defender design (StatefulSet only for stable network identity)
- U-chart history is compact (UCL, LCL, mean, stddev per cycle)
- Cache rebuilt after pod restart (warm-up period)
- Consistent hashing ensures same chambers reassigned = faster cache rebuild
- Eliminates PVC management complexity and attachment delays


## Compliance and Security Constraints

### Data Retention
**Requirement**: T-charts for 3 days, U-charts for 1 year

**Cassandra TTL**: By table

**Archive Strategy**: Write to another offline Cassandra in the background.

## Migration Constraints

### Backward Compatibility
**Requirement**: V2(NATS-based) system must coexist with V1(Kafka-based) during migration

**Strategy**: Feature flag in configuration to enable/disable assignment controller

### Data Migration
**Requirement**: No downtime during migration from V1 to V2

**Approach**: Blue-green deployment with traffic shifting

### Rollback Plan
**Requirement**: Ability to rollback to V1 if V2 has issues

**Implementation**: Kubernetes Deployment with previous image tag

## Related Documents

- [System Overview](./overview.md) - Key requirements and scale
- [Performance Goals](./performance-goals.md) - Detailed SLA targets
- [Kafka Limitations](../02-problem-analysis/kafka-limitations.md) - Why Kafka doesn't fit
