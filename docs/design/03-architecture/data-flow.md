# Data Flow

## Overview

This document provides detailed data flows through the system, from equipment data collection to alarm generation, including all intermediate processing steps.

**Covered Flows**:
1. **SVID Collection and T-Chart Generation** - Equipment → Collector → Cassandra
2. **Completion Notification** - Collector → NATS → Defenders
3. **Stable ID Claiming** - Defender startup and ID pool management
4. **Assignment Map Updates** - Leader monitoring and assignment calculation
5. **Chamber Message Routing** - Subject-based routing to defenders
6. **U-Chart Processing** - End-to-end message handling with cache, calculation, and alarms

## End-to-End Data Flow

```mermaid
flowchart TB
    subgraph Equipment["EQP Hub"]
        Tool[Tool<br/>Chamber 1-4]
    end

    subgraph Collection["Data Collection"]
        DC[Data Collector]
        Mebo[Mebo Encoder<br/>3-4 bytes/point]
    end

    subgraph Storage["Storage"]
        Cass[(Cassandra)]
        TChart[T-Chart Table]
        UChart[U-Chart Table]
    end

    subgraph Broker["Message Broker"]
        NATS[NATS JetStream]
        Stream[dc-notifications<br/>Stream]
        Subject[dc.tool001.chamber1.completed]
    end

    subgraph Processing["Defender Processing"]
        Def[Defender-5]
        Cache[(SQLite Cache<br/>U-Chart History)]
        Calc[EQ Calculator]
        Strategy[Defense Strategy<br/>Evaluator]
    end

    subgraph Alerting["Alerting"]
        Alarm[Alarm System]
    end

    Tool -->|Write SVIDs thru gRPC<br/>10 Hz, 1 Hz| DC
    DC -->|Aggregate<br/>10-30 secs| Mebo
    Mebo -->|Compressed Blob| Cass
    Cass --> TChart
    DC -->|Publish| Stream
    Stream -->|Subject-based| Subject
    Subject -->|WorkQueue| Def

    Def -->|1: Check Cache| Cache
    Def -->|2: Fetch T-Chart| TChart
    Def -->|3: Fetch History<br>when Cache Miss| UChart
    TChart -->|Mebo BlobSet| Calc
    UChart -->|Previous History| Calc
    Calc -->|U-Chart results| Strategy
    Calc -->|Store| UChart
    Calc -->|Update| Cache
    Strategy -.->|If Threshold<br/>Exceeded| Alarm

    style Tool fill:#e1f5ff
    style Def fill:#ffe1e1
    style Calc fill:#fff4e1
    style Strategy fill:#f0e1ff
    style Alarm fill:#ffe1f0
```

## Detailed Flows

### Flow 1: SVID Collection and T-Chart Generation

**Timeline**: 10 minutes - 2 hours (context duration)

```mermaid
sequenceDiagram
    participant Tool as EQP Hub<br/>SECS/EDA
    participant DC as Data Collector
    participant Mebo as Mebo Packer
    participant Cass as Cassandra<br/>T-Charts

    Note over Tool: Context starts

    loop Every collection interval (10s - 60s)
        Tool->>DC: Send SVID values
        DC->>DC: Buffer in memory
        DC->>Mebo: Pack SVIDs by Mebo
        Mebo->>Mebo: Encode blob<br/>(~3-4 bytes per point)
        Mebo->>DC: Return blob<br/>(compressed T-Chart data)
        DC->>Cass: INSERT INTO T-chart table
        Cass-->>DC: Confirm write
    end

    Note over Tool: 10-120 minutes of collection
    Note over Tool: Context ends
    Tool->>DC: Flush queued SVIDs
    Tool->>DC: Context End

```

**Data Size Example**:
```
100 SVIDs × 1000 points × 16 bytes (timestamp+value) = 1600 KB (uncompressed)
100 SVIDs × 1000 points × 3.5 bytes (Mebo) = 350 KB (compressed)
Compression ratio: ~4.6x
```

### Flow 2: Completion Notification

**Timeline**: < 1 second after T-Chart storage

```mermaid
sequenceDiagram
    participant DC as Data Collector
    participant NATS as NATS JetStream<br/>dc-notifications
    participant Def as Defender-5

    Note over DC: T-Chart stored in Cassandra

    DC->>NATS: Publish to subject<br/>dc.tool001.chamber1.completed

    NATS->>NATS: Route by subject<br/>to subscribed defender

    NATS->>Def: Deliver message<br/>(WorkQueue pattern)

    Note over Def: defender-5 subscribed to<br/>dc.tool001.chamber1.*

    Def-->>NATS: No ack yet<br/>(processing in progress)
```


## Assignment and Subscription Flow
When a defender cold pod start with random pod name and want to claim a stable ID.

### Flow 3: Stable ID Claiming (Startup)

```mermaid
sequenceDiagram
    participant Pod as Defender Pod<br/>defender-abc123
    participant KV as NATS KV<br/>stable-ids
    participant Def as Defender Process

    Note over Pod: Pod starts

    loop Sequential ID search
        Pod->>KV: Try CREATE stable-ids.defender-0
        KV-->>Pod: AlreadyExists

        Pod->>KV: Try CREATE stable-ids.defender-1
        KV-->>Pod: AlreadyExists

        Pod->>KV: Try CREATE stable-ids.defender-5
        KV-->>Pod: Success!
    end

    Note over Pod: Claimed defender-5

    Pod->>Def: Initialize with ID="defender-5"

    loop Every 2 seconds in background
        Pod->>KV: UPDATE stable-ids.defender-5<br/>SET last_heartbeat=now()
    end

    Note over Def: Proceed with defender startup
```

### Flow 4: Assignment Map Updates

```mermaid
sequenceDiagram
    participant Defs as All Defenders
    participant KV_H as NATS KV<br/>heartbeats
    participant Leader as Leader Defender<br/>Assignment Controller
    participant KV_A as NATS KV<br/>assignments

    loop Every 2 seconds
        Defs->>KV_H: Write heartbeat<br/>(defender_id, timestamp)
    end

    loop Every 2 seconds
        Leader->>KV_H: Read all heartbeats
        Leader->>Leader: Detect topology changes

        alt New defender joined OR Defender left
            Leader->>Leader: Calculate new assignments<br/>(consistent hashing)

            Note over Leader: Assignment calculation
            rect rgb(240, 255, 240)
                Note right of Leader: 1. Build hash ring (150 vnodes × N defenders)<br/>2. Map chambers to closest vnode<br/>3. Generate assignment map
            end

            Leader->>KV_A: UPDATE defender-assignments<br/>SET assignments={...}, version++
        else No changes
            Note over Leader: No action needed
        end
    end

    Defs->>KV_A: Watch for changes<br/>(subscription)

    KV_A-->>Defs: New assignment version detected

    Defs->>Defs: Compare old vs new assignments

    Defs->>Defs: Unsubscribe from removed chambers
    Defs->>Defs: Subscribe to new chambers

    Note over Defs: Updated subscriptions in ~1 second
```

### Flow 5: Chamber Message Routing

```mermaid
flowchart LR
    DC[Data Collector] -->|Publish| S1[dc.tool001.chamber1.completed]
    DC -->|Publish| S2[dc.tool001.chamber2.completed]
    DC -->|Publish| S3[dc.tool050.chamber1.completed]

    S1 --> D5[Defender-5<br/>subscribed to tool001:chamber1]
    S2 --> D5[Defender-5<br/>subscribed to tool001:chamber2]
    S3 --> D12[Defender-12<br/>subscribed to tool050:chamber1]

    style S1 fill:#e1f5ff
    style S2 fill:#e1f5ff
    style S3 fill:#e1f5ff
    style D5 fill:#ffe1e1
    style D12 fill:#ffe1e1
```

### Flow 6: Defense Processing

**Timeline**: 2-5 seconds per message

```mermaid
sequenceDiagram
    participant NATS as NATS JetStream
    participant Def as Defender-5
    participant Cache as SQLite Cache<br/>(In-Memory)
    participant Cass as Cassandra
    participant Calc as U-Chart Calculator
    participant Strategy as Defense Strategy<br/>Evaluator
    participant Alarm as Alarm System

    Note over NATS: Message:<br/>dc.tool001.chamber1.completed

    NATS->>Def: Deliver message<br/>(WorkQueue)

    Note over Def: Extract context info<br/>(tool_id, chamber_id, context_id)

    Def->>Cache: Query U-Chart history<br/>for chamber1

    alt Cache Hit (>95% typical)
        Cache-->>Def: Return last 100 U-Chart points
        Note over Def: Cache hit: ~1.2s total
    else Cache Miss (cold start / eviction)
        Cache-->>Def: Not found
        Def->>Cass: SELECT U-Chart history<br/>LIMIT 100 ORDER BY time DESC
        Cass-->>Def: Return history
        Def->>Cache: Store in cache
        Note over Def: Cache miss: ~1.5s total
    end

    Def->>Cass: SELECT T-Chart blob<br/>WHERE context_id = ?
    Cass-->>Def: Return compressed T-Chart (Mebo format)

    Note over Def: Decompress T-Chart<br/>(Mebo → raw values)

    Def->>Calc: Calculate U-Chart<br/>(T-Chart + history)

    Note over Calc: Apply statistical formulas<br/>(SPC, EWMA, etc.)

    Calc-->>Def: Return U-Chart result<br/>(float64 values)

    Def->>Strategy: Evaluate defense rules<br/>(U-Chart + thresholds)

    alt Threshold Exceeded
        Strategy-->>Def: Alarm triggered
        Def->>Alarm: Raise alarm<br/>(chamber, severity, reason)
        Note over Alarm: Notify operators
    else Within Limits
        Strategy-->>Def: OK
        Note over Def: No action needed
    end

    par Store U-Chart
        Def->>Cass: INSERT U-Chart result<br/>WITH TTL
    and Update Cache
        Def->>Cache: Update history<br/>(append new point)
    end

    Def->>NATS: Ack message

    Note over NATS: Message completed<br/>removed from queue
```

**Performance Breakdown**:
```
Cache check:          ~1 ms
T-Chart fetch:        ~50-100 ms (Cassandra read)
T-Chart decompress:   ~10-20 ms (Mebo decode)
U-Chart calculation:  ~500-1000 ms (statistical computation)
Strategy evaluation:  ~50-100 ms (rule engine)
U-Chart storage:      ~50-100 ms (Cassandra write, async)
Cache update:         ~1 ms

Total (cache hit):    ~1.2-2 seconds
Total (cache miss):   +300ms (history fetch)
```

**Error Handling**:
```mermaid
flowchart TD
    Start[Message Received] --> CheckCache{Cache Available?}
    CheckCache -->|Yes| FetchTChart
    CheckCache -->|No| FallbackCassandra[Fetch from Cassandra]
    FallbackCassandra --> FetchTChart

    FetchTChart[Fetch T-Chart] --> TChartOK{T-Chart Found?}
    TChartOK -->|Yes| Calculate[Calculate U-Chart]
    TChartOK -->|No| Retry[Retry 3 times]
    Retry --> RetryOK{Success?}
    RetryOK -->|Yes| Calculate
    RetryOK -->|No| Nack[Nack message]

    Calculate --> CalcOK{Calculation OK?}
    CalcOK -->|Yes| Store[Store U-Chart]
    CalcOK -->|No| LogError[Log error + Nack]

    Store --> Ack[Ack message]

    style Start fill:#e1f5ff
    style Ack fill:#e1ffe1
    style Nack fill:#ffe1e1
    style LogError fill:#ffe1e1
```

## Data Structure Examples

### Stable ID Registration (NATS KV)
```json
{
  "defenderId": "defender-5",
  "podIp": "xx.xx.xx.xx",
  "podName": "defender-7f8d9c-xyzab",
  "claimedAt": "2025-10-24T10:00:00Z",
  "lastHeartbeat": "2025-10-24T10:30:45Z",
  "ttlSeconds": 10
}
```

### Assignment Map (NATS KV)
```json
{
  "version": 123,
  "timestamp": "2025-10-24T10:30:00Z",
  "state": "stable",
  "assignments": {
    "tool001:chamber1": "defender-5",
    "tool001:chamber2": "defender-5",
    "tool050:chamber1": "defender-12"
  },
  "defenderInfo": {
    "defender-5": {
      "chamberCount": 80,
      "totalWeight": 9600000
    }
  }
}
```

## Related Documents

- [High-Level Design](./high-level-design.md) - System architecture
- [Design Decisions](./design-decisions.md) - Why these flows
- [State Machine](./state-machine.md) - Cluster state transitions
- [Cassandra Schema](../04-components/storage/cassandra-schema.md) - Table definitions
