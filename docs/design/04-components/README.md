# Chapter 4: Components

## Overview

This chapter provides detailed documentation for each major component in the defender cluster system.

## Components

### 1. Defender
**Directory**: `defender/`

The core processing component responsible for U-chart calculations and defense strategy evaluation.

**Key Documents**:
- [`overview.md`](./defender/overview.md) - Architecture, responsibilities, resource usage, configuration, metrics, and APIs

**Topics Covered**:
- Startup and initialization sequence
- Stable ID claiming mechanism
- Message processing workflow (T-chart → U-chart → defense strategy)
- Leader duties (assignment controller)
- Heartbeat and health management
- Graceful shutdown procedures
- Resource requirements (8-16 vCores, 8-16GB memory, 100MB cache)
- Prometheus metrics and health probes
- HTTP API endpoints

### 2. Message Broker (NATS JetStream)
**Directory**: `message-broker/`

Durable message streaming and key-value stores for cluster coordination.

**Key Documents**:
- [`nats-jetstream-setup.md`](./message-broker/nats-jetstream-setup.md) - Stream configuration, consumer setup, KV buckets, cluster deployment
- [`component-interactions.md`](./message-broker/component-interactions.md) - Detailed interaction patterns between defenders and NATS

**Topics Covered**:
- JetStream stream: `dc-notifications` (workqueue pattern)
- Consumer setup: Dynamic filter subjects per defender
- KV bucket: `defender-assignments` (assignment map with tool:chamber → defender mapping)
- KV bucket: `defender-heartbeats` (crash detection with 30s TTL)
- KV bucket: `stable-ids` (ID pool with sequential claiming, 200 IDs default, configurable)
- Complete KV schemas: Stable ID Pool, Assignment Map, Heartbeats
- Component interaction protocols: Subscribe, ACK/NAK, heartbeat maintenance, ID claiming
- 3-node NATS cluster deployment (StatefulSet)
- Performance tuning and monitoring

**Additional Documents** (to be created):
- `workqueue-pattern.md` - NATS WorkQueue pattern implementation details
- `failover-behavior.md` - NATS cluster failover and high availability

### 3. Storage (Cassandra)
**Directory**: `storage/`

Persistent storage for T-charts, U-charts, and metadata.

**Documents to be created**:
- `cassandra-schema.md` - Keyspace, tables, indexes, and data model
- `election-agent.md` - Leader election service (Raft-based)
- `data-persistence.md` - Write patterns, consistency levels, compaction strategies

### 4. Kubernetes
**Directory**: `kubernetes/`

Kubernetes deployment manifests and scaling strategies.

**Documents to be created**:
- `deployment-config.md` - Complete Deployment YAML with resource limits, probes, etc.
- `scaling-strategy.md` - HPA configuration, manual scaling procedures
- `deployment-guide.md` - Step-by-step deployment instructions

## Component Interaction

```
┌─────────────────────────────────────────────────────────────┐
│                        Kubernetes                            │
│  ┌──────────────────────────────────────────────────────┐   │
│  │         Defender Deployment (30 replicas)            │   │
│  │                                                       │   │
│  │  ┌──────┐  ┌──────┐  ┌──────┐           ┌──────┐   │   │
│  │  │ D-0  │  │ D-1  │  │ D-2  │    ...    │ D-29 │   │   │
│  │  │Leader│  │Follwr│  │Follwr│           │Follwr│   │   │
│  │  └───┬──┘  └───┬──┘  └───┬──┘           └───┬──┘   │   │
│  └──────┼─────────┼─────────┼──────────────────┼──────┘   │
│         │         │         │                  │           │
└─────────┼─────────┼─────────┼──────────────────┼───────────┘
          │         │         │                  │
          ├─────────┴─────────┴──────────────────┤
          │                                       │
    ┌─────▼──────────────────────────────────────▼────┐
    │            NATS JetStream (3 nodes)             │
    │  ┌──────────────────────────────────────────┐   │
    │  │  Stream: dc-notifications         │   │
    │  │  - Subject: dc.{tool}.{chamber}   │   │
    │  │  - Retention: workqueue                  │   │
    │  │  - Replicas: 3                           │   │
    │  └──────────────────────────────────────────┘   │
    │  ┌──────────────────────────────────────────┐   │
    │  │  KV: stable-ids (ID claiming, TTL 30s)   │   │
    │  │  KV: defender-heartbeats (TTL 30s)       │   │
    │  │  KV: defender-assignments (no TTL)       │   │
    │  └──────────────────────────────────────────┘   │
    └──────────────────────────────────────────────────┘
                         │
                         │
    ┌────────────────────▼──────────────────────────┐
    │         Cassandra (3+ nodes)                  │
    │  ┌──────────────────────────────────────┐     │
    │  │  Keyspace: defense_system            │     │
    │  │  - t_charts (raw data)               │     │
    │  │  - u_charts (calculated stats)       │     │
    │  │  - defense_events (alarms)           │     │
    │  │  Replication: NetworkTopologyStrategy│     │
    │  │  Consistency: QUORUM                 │     │
    │  └──────────────────────────────────────┘     │
    └───────────────────────────────────────────────┘
```

## Resource Summary

| Component | CPU | Memory | Storage | Replicas |
|-----------|-----|--------|---------|----------|
| **Defender** | 8-16 vCores | 8-16 GB | ~100 MB (cache) | 30 |
| **NATS** | 500m-2000m | 1-4 GB | 50 GB (persistent) | 3 |
| **Cassandra** | 4-8 vCores | 16-32 GB | 1-5 TB | 3+ |
| **Election Agent** | 100m-500m | 256-512 MB | 10 GB | 3 |

**Total Cluster Resources**:
- **CPU**: 240-480 vCores (defenders) + 13.5-24.5 vCores (infrastructure) = **254-505 vCores**
- **Memory**: 240-480 GB (defenders) + 52-100 GB (infrastructure) = **292-580 GB**
- **Storage**: 3 GB (defender cache) + 150 GB (NATS) + 3-15 TB (Cassandra) = **3-15.2 TB**

## Related Chapters

- [Chapter 1: Requirements](../01-requirements/) - System constraints and performance goals
- [Chapter 2: Problem Analysis](../02-problem-analysis/) - Kafka limitations and design challenges
- [Chapter 3: Architecture](../03-architecture/) - High-level design, decisions, data flow, state machine
- [Chapter 5: Operational Scenarios](../05-operational-scenarios/) - Cold start, scaling, crash recovery, rolling updates
