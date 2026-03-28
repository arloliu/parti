# Parti Architecture

> System architecture and core concepts for the Parti library.

**Related Documentation:**
- [Docs README](README.md) - Documentation map
- [Configuration Guide](CONFIGURATION.md) - Configuration options
- [Lifecycle & State Management](LIFECYCLE.md) - Worker states and handoff
- [Consumer Helpers](CONSUMERS.md) - JetStream consumer management

---

## Table of Contents

1. [System Architecture](#system-architecture)
2. [Component Responsibilities](#component-responsibilities)
3. [Data Flow](#data-flow)
4. [Core Concepts](#core-concepts)
5. [NATS KV Buckets](#nats-kv-buckets)

---

## System Architecture

Parti uses NATS JetStream KeyValue buckets for coordination between workers:

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                              NATS JetStream                                  │
│  ┌─────────────┐ ┌─────────────┐ ┌─────────────┐ ┌─────────────────────────┐ │
│  │ StableID KV │ │ Election KV │ │Heartbeat KV │ │    Assignment KV        │ │
│  │ (claims)    │ │ (leader)    │ │ (health)    │ │ (partition→worker)      │ │
│  └──────┬──────┘ └──────┬──────┘ └──────┬──────┘ └───────────┬─────────────┘ │
│         │               │               │                    │               │
│         └───────────────┴───────────────┴────────────────────┘               │
│                                   │                                          │
└───────────────────────────────────┼──────────────────────────────────────────┘
                                    │
        ┌───────────────────────────┼───────────────────────────┐
        │                           │                           │
        ▼                           ▼                           ▼
┌───────────────────┐   ┌───────────────────┐   ┌───────────────────┐
│    Worker 0       │   │    Worker 1       │   │    Worker 2       │
│   (LEADER)        │   │   (FOLLOWER)      │   │   (FOLLOWER)      │
│                   │   │                   │   │                   │
│ ┌───────────────┐ │   │ ┌───────────────┐ │   │ ┌───────────────┐ │
│ │   Manager     │ │   │ │   Manager     │ │   │ │   Manager     │ │
│ │  ┌──────────┐ │ │   │ │  ┌─────────┐  │ │   │ │  ┌─────────┐  │ │
│ │  │Claimer   │ │ │   │ │  │Claimer  │  │ │   │ │  │Claimer  │  │ │
│ │  │Election  │ │ │   │ │  │Election │  │ │   │ │  │Election │  │ │
│ │  │Calculator│ │ │   │ │  │(watch)  │  │ │   │ │  │(watch)  │  │ │
│ │  │Heartbeat │ │ │   │ │  │Heartbeat│  │ │   │ │  │Heartbeat│  │ │
│ │  └──────────┘ │ │   │ │  └─────────┘  │ │   │ │  └─────────┘  │ │
│ └───────────────┘ │   │ └───────────────┘ │   │ └───────────────┘ │
│                   │   │                   │   │                   │
│ ┌───────────────┐ │   │ ┌───────────────┐ │   │ ┌───────────────┐ │
│ │WorkerConsumer │ │   │ │WorkerConsumer │ │   │ │WorkerConsumer │ │
│ │  [P0, P1, P2] │ │   │ │  [P3, P4, P5] │ │   │ │  [P6, P7, P8] │ │
│ └───────────────┘ │   │ └───────────────┘ │   │ └───────────────┘ │
└───────────────────┘   └───────────────────┘   └───────────────────┘
```

---

## Component Responsibilities

| Component               | Responsibility                                                                  |
|-------------------------|---------------------------------------------------------------------------------|
| **Manager**             | Central coordinator for worker lifecycle, election, and assignment distribution |
| **Claimer**             | Claims and renews stable worker IDs from NATS KV                                |
| **Election Agent**      | Manages leader election using NATS KV lease semantics                           |
| **Calculator**          | Leader-only: calculates partition assignments using chosen strategy             |
| **Heartbeat Publisher** | Publishes periodic health signals for failure detection                         |
| **WorkerConsumer**      | Manages JetStream consumers for assigned partitions                         |
| **ProcessingGate**      | Enforces ownership before processing messages                                   |

### Manager

The `Manager` is the central component that coordinates worker identity, leader election, and partition assignment. Each worker instance runs exactly one Manager.

**Responsibilities:**
- Claim and renew stable worker ID
- Participate in leader election
- Watch for assignment changes
- Invoke hooks on lifecycle events
- Manage WorkerConsumer partition filters

### Leader vs Follower

Only **one worker** is the leader at any time:

| Role         | Responsibilities                                   |
|--------------|----------------------------------------------------|
| **Leader**   | Calculate assignments, publish to KV, run sweepers |
| **Follower** | Watch assignment KV, apply local updates           |

Leadership is acquired via NATS KV lease semantics and automatically transferred on leader failure.

---

## Data Flow

### Assignment Distribution

```
                    Partition Source
                          │
                          ▼
              ┌───────────────────────┐
              │   Leader Calculator   │ ◄─── Assignment Strategy
              └───────────────────────┘
                          │
                          ▼
              ┌───────────────────────┐
              │   Assignment KV       │
              │  (version: N)         │
              └───────────────────────┘
                          │
          ┌───────────────┼───────────────┐
          ▼               ▼               ▼
    ┌──────────┐    ┌──────────┐    ┌──────────┐
    │ Worker 0 │    │ Worker 1 │    │ Worker 2 │
    │ Watch KV │    │ Watch KV │    │ Watch KV │
    └────┬─────┘    └────┬─────┘    └────┬─────┘
         │               │               │
         ▼               ▼               ▼
    ┌──────────┐    ┌──────────┐    ┌──────────┐
    │ Consumer │    │ Consumer │    │ Consumer │
    │ Updater  │    │ Updater  │    │ Updater  │
    └──────────┘    └──────────┘    └──────────┘
```

### Message Processing Flow

```
                    JetStream
                        │
                        ▼
              ┌─────────────────┐
              │  WorkerConsumer │
              └────────┬────────┘
                       │
                       ▼
              ┌─────────────────┐
              │ ProcessingGate  │ ◄─── Ownership check
              └────────┬────────┘
                       │
            ┌──────────┴──────────┐
            │                     │
         Allowed               Denied
            │                     │
            ▼                     ▼
    ┌───────────────┐     ┌───────────────┐
    │ MessageHandler│     │  NAK + delay  │
    └───────────────┘     └───────────────┘
```

---

## Core Concepts

### Worker ID

A stable identifier (e.g., `worker-0`) claimed from NATS KV. It persists across restarts within a TTL window, ensuring that a restarting pod reclaims its previous ID and partitions.

**Key Properties:**
- Claimed from a pool: `[WorkerIDMin, WorkerIDMax]`
- Renewed every `TTL/3`
- Preserved during rolling updates
- Enables cache affinity

### Partition

A logical unit of work identified by a list of keys (e.g., `["orders", "0"]`). Partitions are the atomic units of assignment.

```go
type Partition struct {
    Keys   []string  // Unique identifier (e.g., ["orders", "region-us"])
    Weight int64     // Relative processing cost (default: 100)
}

// Helper methods
p.SubjectKey()  // "orders.region-us" (for NATS subjects)
p.ID()          // "orders-region-us" (for durable names)
p.HashID()      // uint64 hash for consistent hashing
```

### Assignment

A mapping of partitions to workers. Assignments are:
- **Versioned**: Each update increments version
- **Stored in NATS KV**: Durable and watchable
- **Calculated by leader**: Using chosen strategy

```go
type Assignment struct {
    Version    uint64
    WorkerID   string
    Partitions []Partition
    UpdatedAt  time.Time
}
```

---

## NATS KV Buckets

Parti uses four (optionally five) KV buckets:

| Bucket             | Purpose                  | Key Pattern            | TTL           |
|--------------------|--------------------------|------------------------|---------------|
| `parti-stableid`   | Worker ID claims         | `worker-0`, `worker-1` | WorkerIDTTL   |
| `parti-election`   | Leader lease             | `leader`               | Lease-based   |
| `parti-heartbeat`  | Worker health signals    | `worker-0`, etc.       | HeartbeatTTL  |
| `parti-assignment` | Partition assignments    | `worker-0`, etc.       | AssignmentTTL |
| `parti-handoff`    | Two-phase handoff claims | `claims/partition-id`  | HandoffTTL    |

### Bucket Interactions

```
Worker Startup:
  1. StableID KV → Claim "worker-N"
  2. Election KV → Request leadership
  3. Heartbeat KV → Start publishing
  4. Assignment KV → Watch for assignments

Leader Actions:
  1. Heartbeat KV → Watch all workers
  2. Calculate new assignments
  3. Assignment KV → Publish assignments
  4. (Optional) Handoff KV → Coordinate two-phase handoff
```

See [Configuration Guide](CONFIGURATION.md) for bucket configuration options.
