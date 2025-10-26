# NATS JetStream Configuration

## Overview

NATS JetStream provides durable message streaming for completion notifications and key-value stores for cluster coordination (assignments, heartbeats, stable IDs).

## JetStream Stream Setup

### Stream: dc-notifications

```bash
nats stream add dc-notifications \
  --subjects="dc.>" \
  --storage=file \
  --replicas=3 \
  --retention=workqueue \
  --max-age=7d \
  --max-msgs=-1 \
  --max-bytes=-1 \
  --max-msg-size=1MB \
  --discard=old \
  --dupe-window=2m
```

**Configuration Details**:

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| **Subjects** | `dc.>` | Wildcard covers all equipment notifications |
| **Storage** | `file` | Persistent storage, survives NATS restarts |
| **Replicas** | `3` | High availability, tolerates 1 node failure |
| **Retention** | `workqueue` | Messages deleted after acknowledgment |
| **Max Age** | `7 days` | Safety net for unacknowledged messages |
| **Max Messages** | `-1` (unlimited) | No message count limit |
| **Max Bytes** | `-1` (unlimited) | No storage limit (rely on max-age) |
| **Max Message Size** | `1 MB` | Completion notifications ~1-5 KB |
| **Discard Policy** | `old` | Remove oldest messages if limits reached |
| **Duplicate Window** | `2 minutes` | Prevent duplicate notifications |

### Subject Hierarchy

```
dc.{tool_id}.{chamber_id}.completion
```

**Examples**:
- `dc.tool001.chamber1.completion`
- `dc.tool002.chamber3.completion`
- `dc.tool015.chamber8.completion`

**Subject Design Rationale**:
1. **Unique per chamber**: Each `tool_id + chamber_id` combination gets a unique subject
2. **Hierarchical**: Easy to add new message types (e.g., `dc.{tool_id}.{chamber_id}.alarm`)
3. **Filterable**: Defenders subscribe to specific tool:chamber combinations they're assigned to

## Consumer Setup

### WorkQueue Consumer Pattern

Each defender creates an ephemeral pull consumer with filtered deliver subject:

```bash
# Defender-5 subscribing to assigned tool:chamber combinations
# Example: Assigned to tool001:chamber1, tool001:chamber2, tool050:chamber3, etc.
nats consumer add dc-notifications defender-5 \
  --filter="dc.tool001.chamber1.completion" \
  --filter="dc.tool001.chamber2.completion" \
  --filter="dc.tool050.chamber3.completion" \
  ... (repeat for all ~80 assigned tool:chamber pairs) \
  --deliver=all \
  --ack-policy=explicit \
  --ack-wait=30s \
  --max-deliver=3 \
  --max-ack-pending=10 \
  --replay-policy=instant
```

**Configuration Details**:

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| **Filter** | `dc.{tool_id}.{chamber_id}.completion` | Subscribe to specific tool:chamber pairs assigned to this defender |
| **Deliver Policy** | `all` | Process all messages in stream |
| **Ack Policy** | `explicit` | Defender must explicitly ack after processing |
| **Ack Wait** | `30 seconds` | Redelivery timeout if not acked |
| **Max Deliver** | `3` | Retry failed messages up to 3 times |
| **Max Ack Pending** | `10` | Limit in-flight messages per defender |
| **Replay Policy** | `instant` | Deliver messages as fast as possible |

### Dynamic Consumer Management

**On Assignment Update**:
1. Defender receives new assignment map
2. Compare with current subscriptions
3. **Add chambers**: Add filter subjects to consumer
4. **Remove chambers**: Remove filter subjects from consumer
5. NATS automatically stops delivering removed chambers
6. NATS starts delivering added chambers

**Consumer Lifecycle**:
- **Created**: When defender claims stable ID and receives first assignment
- **Updated**: Every time assignment map changes (scale-up, scale-down, rebalancing)
- **Deleted**: When defender shuts down gracefully or NATS detects crash (after 30s inactivity)

## Key-Value Store Setup

### KV Bucket: defender-assignments

Stores the current chamber-to-defender assignment map (tool+chamber → defender ID mapping).

```bash
nats kv add defender-assignments \
  --replicas=3 \
  --ttl=0 \
  --history=10 \
  --max-value-size=1MB
```

**Schema**:
```json
Key: "current"
Value: JSON
{
  "version": 123,
  "timestamp": "2025-10-24T10:30:00Z",
  "state": "stable",
  "lifecycleState": "stable",
  "firstAssignmentAt": "2025-10-24T10:00:00Z",
  "assignments": {
    "tool001:chamber1": "defender-1",
    "tool001:chamber2": "defender-1",
    "tool002:chamber1": "defender-2",
    "tool002:chamber2": "defender-2",
    ...
  },
  "defenderInfo": {
    "defender-1": {
      "chamberCount": 80,
      "totalWeight": 9600000
    },
    "defender-2": {
      "chamberCount": 80,
      "totalWeight": 9580000
    },
    ...
  }
}
```

**Field Descriptions**:
- `version`: Monotonically increasing version number (used for optimistic locking)
- `timestamp`: When this assignment map was calculated
- `state`: Current cluster state (`stable`, `scaling`, `emergency`)
- `lifecycleState`: Cluster lifecycle state for determining trigger windows
  - `cold_start`: Initial deployment, no prior assignment (version 0)
  - `post_cold_start`: First assignment completed, still use adaptive logic
  - `stable`: Normal operation (version ≥ 2), use 10s window for planned scale
- `firstAssignmentAt`: Timestamp when cluster was first initialized (for tracking cluster age)
- `assignments`: Map of `"tool_id:chamber_id"` → `"defender-N"`
- `defenderInfo`: Per-defender statistics
  - `chamberCount`: Number of chambers assigned to this defender
  - `totalWeight`: Sum of chamber weights (for load balancing verification)

**Lifecycle State Usage**:
- **Cold Start Detection**: When a new leader takes over, it reads the assignment map
  - If key doesn't exist or `lifecycleState = "cold_start"`: Use 30s stabilization window
  - If `lifecycleState = "post_cold_start"` or `"stable"`: Use 10s stabilization window
- **State Transitions**:
  - `cold_start` → `post_cold_start` (after first assignment published)
  - `post_cold_start` → `stable` (after second assignment published)
  - `stable` → `stable` (stays stable thereafter)

**Configuration**:
- **Replicas**: 3 (high availability)
- **TTL**: 0 (no expiration, assignment is persistent until updated)
- **History**: 10 (keep last 10 versions for debugging and rollback)
- **Max Value Size**: 1 MB (assignment map typically 50-100 KB for 2400 chambers)

**Update Protocol**:
1. Leader calculates new assignment map (consistent hashing)
2. Leader performs Compare-And-Set on `current` key with version check
3. All defenders watch KV bucket for changes (NATS KV watcher)
4. On change detected, defenders:
   - Fetch new assignment map
   - Compare with current subscriptions
   - Update consumer filter subjects (add/remove chambers)
5. NATS automatically routes messages to correct defenders

### KV Bucket: defender-heartbeats

Stores heartbeats from all defenders.

```bash
nats kv add defender-heartbeats \
  --replicas=3 \
  --ttl=30s \
  --history=1 \
  --max-value-size=1KB
```

**Schema**:
```
Key: defender-{N}  (e.g., defender-5)
Value: JSON
{
  "defenderId": "defender-5",
  "podName": "defender-7f8d9c-xyzab",
  "timestamp": "2025-10-24T10:30:45Z",
  "isLeader": false,
  "state": "STABLE",
  "assignedChambers": 80,
  "messagesProcessed": 12450
}
```

**Configuration**:
- **Replicas**: 3 (high availability)
- **TTL**: 30 seconds (heartbeat expires if not refreshed)
- **History**: 1 (only latest heartbeat matters)
- **Max Value Size**: 1 KB (heartbeat ~200-300 bytes)

**Heartbeat Protocol**:
1. Defender publishes heartbeat every 2 seconds
2. KV entry TTL resets to 30 seconds on each update
3. Leader watches all heartbeats (via KV watcher)
4. If heartbeat expires (30s without update), leader detects defender crash
5. Leader triggers emergency rebalancing

### KV Bucket: stable-ids

Stores claimed stable IDs with TTL-based claiming mechanism (ID pool management).

```bash
nats kv add stable-ids \
  --replicas=3 \
  --ttl=30s \
  --history=1 \
  --max-value-size=1KB
```

**Schema**:
```json
Key: "defender-{N}"  (e.g., "defender-5")
Value: JSON
{
  "defenderId": "defender-5",
  "podUid": "550e8400-e29b-41d4-a716-446655440000",
  "podName": "defender-7f8d9c-xyzab",
  "claimedAt": "2025-10-24T10:00:00Z",
  "lastHeartbeat": "2025-10-24T10:30:45Z",
  "ttlSeconds": 10
}
```

**Field Descriptions**:
- `defenderId`: Stable ID claimed (defender-0, defender-1, ..., defender-199)
- `podUid`: Kubernetes pod UID (unique identifier for pod instance)
- `podName`: Kubernetes pod name (generated name like defender-7f8d9c-xyzab)
- `claimedAt`: Timestamp when ID was first claimed
- `lastHeartbeat`: Timestamp of last heartbeat update
- `ttl_seconds`: TTL value (10 seconds, resets on each heartbeat)

**Configuration**:
- **Replicas**: 3 (high availability)
- **TTL**: 30 seconds (ID released if not maintained via heartbeat)
- **History**: 1 (only current claim matters)
- **Max Value Size**: 1 KB (claim data ~200-300 bytes)

**ID Claiming Protocol** (Sequential Search):

1. **Startup**: Defender pod starts, needs to claim a stable ID
2. **Sequential Search**: Try defender-0, defender-1, defender-2, ... up to MAX_DEFENDER_COUNT (default 200)
3. **Atomic Create**: Attempt `Create` operation on first unclaimed ID
   - Success: ID claimed, proceed to step 4
   - Conflict (key exists): Check if stale (no heartbeat > 3×TTL = 30s)
     - If stale: Perform Compare-And-Swap to takeover
     - If active: Try next ID (increment N)
4. **Heartbeat Maintenance**: Update KV entry every 2 seconds
   - Updates `last_heartbeat` timestamp
   - KV entry TTL resets to 30 seconds on each update
5. **Graceful Release**: On SIGTERM, delete KV entry immediately
6. **Crash Release**: If defender crashes, KV entry expires after 30s (automatic cleanup)

**Pool Size Configuration**:
```bash
# Default: 200 IDs (defender-0 to defender-199)
MAX_DEFENDER_COUNT=200

# Examples for different scales:
# Development: 50 IDs
# Production: 200 IDs (default)
# Large-scale: 500 IDs
```

**Why Sequential Search**:
- Simple implementation (no coordination overhead)
- Predictable ID assignment (lowest available ID)
- Works well with consistent hashing (stable ID = stable assignments)
- Automatic gap filling (reuses released IDs from bottom)

## NATS Cluster Deployment

### 3-Node NATS Cluster (Kubernetes)

```yaml
apiVersion: v1
kind: Service
metadata:
  name: nats
  namespace: fdc-system
spec:
  selector:
    app: nats
  clusterIP: None  # Headless service for StatefulSet
  ports:
  - name: client
    port: 4222
    targetPort: 4222
  - name: cluster
    port: 6222
    targetPort: 6222
  - name: monitor
    port: 8222
    targetPort: 8222
  - name: metrics
    port: 7777
    targetPort: 7777
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: nats
  namespace: fdc-system
spec:
  serviceName: nats
  replicas: 3
  selector:
    matchLabels:
      app: nats
  template:
    metadata:
      labels:
        app: nats
    spec:
      containers:
      - name: nats
        image: nats:2.10-alpine
        ports:
        - containerPort: 4222
          name: client
        - containerPort: 6222
          name: cluster
        - containerPort: 8222
          name: monitor
        - containerPort: 7777
          name: metrics
        command:
        - nats-server
        - --config
        - /etc/nats/nats-server.conf
        volumeMounts:
        - name: config
          mountPath: /etc/nats
        - name: data
          mountPath: /data
        resources:
          requests:
            cpu: 500m
            memory: 1Gi
          limits:
            cpu: 2000m
            memory: 4Gi
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8222
          initialDelaySeconds: 10
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /healthz
            port: 8222
          initialDelaySeconds: 5
          periodSeconds: 5
      volumes:
      - name: config
        configMap:
          name: nats-config
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 50Gi  # JetStream file storage
```

### NATS Configuration File

```conf
# /etc/nats/nats-server.conf

# Server settings
server_name: $POD_NAME
listen: 0.0.0.0:4222
http: 0.0.0.0:8222

# JetStream settings
jetstream {
  store_dir: /data/jetstream
  max_mem: 2Gi
  max_file: 40Gi
}

# Clustering
cluster {
  name: fdc-nats-cluster
  listen: 0.0.0.0:6222

  routes: [
    nats://nats-0.nats.fdc-system.svc:6222
    nats://nats-1.nats.fdc-system.svc:6222
    nats://nats-2.nats.fdc-system.svc:6222
  ]
}

# Monitoring
system_account: SYS

# Limits
max_connections: 1000
max_payload: 1MB
write_deadline: 10s

# Logging
debug: false
trace: false
logtime: true
```

## Performance Tuning

### NATS Server Tuning

| Parameter | Value | Impact |
|-----------|-------|--------|
| `max_connections` | `1000` | Support 100 defenders + monitoring |
| `max_payload` | `1 MB` | Notification size ~1-5 KB |
| `write_deadline` | `10s` | Timeout for slow clients |
| `max_pending_size` | `100 MB` | Buffer for slow consumers |

### JetStream Tuning

| Parameter | Value | Impact |
|-----------|-------|--------|
| `max_mem` | `2 GB` | In-memory buffer for stream |
| `max_file` | `40 GB` | Persistent storage per node |
| `max_ack_pending` | `10` | In-flight messages per defender |
| `ack_wait` | `30s` | Redelivery timeout |

## Monitoring

### NATS Metrics (Port 7777)

- `nats_server_info`: Server version and uptime
- `nats_jetstream_memory_bytes`: JetStream memory usage
- `nats_jetstream_file_bytes`: JetStream file storage usage
- `nats_stream_messages`: Messages in stream
- `nats_consumer_ack_pending`: Pending acknowledgments
- `nats_consumer_redeliveries`: Message redeliveries

### Health Checks

```bash
# Server health
curl http://nats:8222/healthz

# Stream status
nats stream info dc-notifications

# Consumer status
nats consumer info dc-notifications defender-5

# KV bucket status
nats kv status defender-assignments
```

## Related Documents

- [Component Interactions](./component-interactions.md) - Detailed protocols for defender↔NATS, defender↔Cassandra, defender↔Election Agent
- [WorkQueue Pattern](./workqueue-pattern.md) - NATS WorkQueue consumer behavior and message acknowledgment strategies
- [Failover Behavior](./failover-behavior.md) - NATS cluster failover scenarios and client reconnection
- [High-Level Design](../../03-architecture/high-level-design.md) - System architecture overview
- [Data Flow](../../03-architecture/data-flow.md) - End-to-end message flows with sequence diagrams

