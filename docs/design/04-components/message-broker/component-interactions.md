# Component Interactions

## Overview

This document describes how defenders and other components interact with NATS JetStream for message delivery, cluster coordination, and state management.

## Defender → NATS JetStream

### Message Subscription

**Operation**: Subscribe to chamber completion notifications

**Protocol**: NATS JetStream WorkQueue consumer with filtered subjects

**Flow**:
1. Defender receives assignment map from `defender-assignments` KV
2. Defender creates/updates ephemeral pull consumer with filter subjects:
   ```
   dc.{tool_id1}.{chamber_id1}.completed
   dc.{tool_id2}.{chamber_id2}.completed
   ...
   ```
3. Consumer configuration:
   - **Ack Policy**: Explicit (defender must ack after processing)
   - **Ack Wait**: 30 seconds (redelivery if not acked)
   - **Max Deliver**: 3 attempts
   - **Max Ack Pending**: 10 messages (flow control)
4. Defender polls consumer for messages (pull-based)
5. After processing, defender sends explicit ACK
6. On processing failure, defender sends NAK (triggers redelivery)

**Dynamic Subscription Updates**:
- Assignment map changes → Defender updates consumer filter subjects
- Add chambers: Consumer starts receiving those messages
- Remove chambers: Consumer stops receiving those messages
- No coordination needed (NATS handles routing automatically)

### Message Processing Acknowledgment

**ACK (Success)**:
```go
msg.Ack() // Message processed successfully
```

**NAK (Retry)**:
```go
msg.Nak() // Processing failed, redeliver
```

**Term (Permanent Failure)**:
```go
msg.Term() // Don't redeliver, move to dead letter queue
```

## Defender → NATS KV (Stable ID Pool)

### ID Claiming (Startup)

**Operation**: Claim a stable ID from the pool

**Protocol**: Sequential search with atomic Create or Compare-And-Swap

**Flow**:
1. Defender starts, tries defender-0:
   ```bash
   nats kv put stable-ids defender-0 '{"defender_id":"defender-0",...}'
   ```
2. If key exists, check if stale (last_heartbeat > 30s ago)
3. If stale, perform Compare-And-Swap:
   ```bash
   nats kv update stable-ids defender-0 '{"defender_id":"defender-0",...}' --revision=<old_revision>
   ```
4. If taken by active defender, try defender-1, defender-2, etc.
5. Once claimed, start heartbeat goroutine

**Data Written**:
```json
{
  "defenderId": "defender-5",
  "podUid": "550e8400-e29b-41d4-a716-446655440000",
  "podName": "defender-7f8d9c-xyzab",
  "claimedAt": "2025-10-24T10:00:00Z",
  "lastHeartbeat": "2025-10-24T10:30:45Z",
  "ttlSeconds": 10
}
```

### Heartbeat Maintenance

**Operation**: Maintain ID claim via periodic updates

**Protocol**: KV update every 2 seconds

**Flow**:
```go
ticker := time.NewTicker(2 * time.Second)
for range ticker.C {
    data := map[string]interface{}{
        "defenderId":    "defender-5",
        "podUid":        podUID,
        "podName":       podName,
        "claimedAt":     claimedAt,
        "lastHeartbeat": time.Now(),
        "ttlSeconds":    10,
    }
    kv.Put("defender-5", marshal(data))
}
```

**TTL Behavior**:
- KV entry has 30-second TTL
- Each update resets TTL to 30 seconds
- If defender crashes (no updates for 30s), entry expires
- Expired entry → ID becomes available for claiming

### Graceful Release (Shutdown)

**Operation**: Release ID on SIGTERM

**Protocol**: Delete KV entry

**Flow**:
```go
// SIGTERM handler
signal.Notify(sigChan, syscall.SIGTERM)
<-sigChan

// Drain in-flight messages (up to 25s)
consumer.Drain()

// Release stable ID
kv.Delete("defender-5")

// Exit
os.Exit(0)
```

## Defender → NATS KV (Heartbeats)

### Heartbeat Publishing

**Operation**: Publish defender health status

**Protocol**: KV update every 2 seconds

**Flow**:
```go
ticker := time.NewTicker(2 * time.Second)
for range ticker.C {
    data := map[string]interface{}{
        "defenderId":        "defender-5",
        "podName":           podName,
        "timestamp":         time.Now(),
        "isLeader":          false,
        "state":             "STABLE",
        "assignedChambers":  80,
        "messagesProcessed": msgCount,
    }
    kv.Put("defender-5", marshal(data))
}
```

**Data Written**:
```json
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

**TTL**: 30 seconds (expires if not updated)

## Defender → NATS KV (Assignment Map)

### Reading Assignments (Follower)

**Operation**: Watch for assignment map updates

**Protocol**: KV watcher on `defender-assignments` bucket

**Flow**:
```go
watcher, _ := kv.Watch("current")
for entry := range watcher.Updates() {
    newAssignments := unmarshal(entry.Value())

    // Compare with current assignments
    added, removed := diff(currentAssignments, newAssignments)

    // Update consumer filter subjects
    for _, chamber := range added {
        consumer.AddFilterSubject(fmt.Sprintf("dc.*.%s.completed", chamber))
    }
    for _, chamber := range removed {
        consumer.RemoveFilterSubject(fmt.Sprintf("dc.*.%s.completed", chamber))
    }

    currentAssignments = newAssignments
}
```

### Writing Assignments (Leader)

**Operation**: Publish new assignment map after calculation

**Protocol**: Compare-And-Swap with version check

**Flow**:
```go
// Calculate new assignments (consistent hashing)
newMap := calculateAssignments(defenders, chambers)
newMap.Version = currentVersion + 1

// Atomic update with version check
revision, err := kv.Update("current", marshal(newMap), currentRevision)
if err != nil {
    // Conflict: another leader updated, re-read and retry
    currentMap, _ := kv.Get("current")
    currentVersion = currentMap.Version
    currentRevision = currentMap.Revision
    // Recalculate with latest state
}
```

**Optimistic Locking**:
- Version field in assignment map prevents lost updates
- Compare-And-Swap ensures only one leader writes
- Failed update → Read latest, recalculate, retry

## Leader → NATS KV (Heartbeats)

### Monitoring Cluster Health

**Operation**: Watch all defender heartbeats

**Protocol**: KV watcher on `defender-heartbeats` bucket (wildcard)

**Flow**:
```go
watcher, _ := kv.WatchAll()
activeDefenders := make(map[string]time.Time)

for entry := range watcher.Updates() {
    if entry.Operation() == nats.KeyValuePut {
        // Heartbeat received
        defenderID := entry.Key()
        data := unmarshal(entry.Value())
        activeDefenders[defenderID] = data.Timestamp
    } else if entry.Operation() == nats.KeyValueDelete {
        // Heartbeat expired (defender crashed)
        defenderID := entry.Key()
        delete(activeDefenders, defenderID)

        // Trigger emergency rebalancing
        triggerEmergencyRebalance(defenderID)
    }
}
```

**Crash Detection**:
1. Defender stops sending heartbeats (crash/hang/network partition)
2. KV entry TTL expires after 30 seconds
3. NATS sends DELETE notification to watcher
4. Leader detects deletion, identifies crashed defender
5. Leader triggers immediate recalculation (no stabilization window)

## Data Collector → NATS JetStream

### Publishing Completion Notifications

**Operation**: Notify defenders when T-chart generation completes

**Protocol**: NATS JetStream publish to subject

**Flow**:
```go
// After storing T-chart to Cassandra
subject := fmt.Sprintf("dc.%s.%s.completed", toolID, chamberID)
data := map[string]interface{}{
    "toolId":     toolID,
    "chamberId":  chamberID,
    "contextId":  contextID,
    "tChartKey":  tchartKey,
    "timestamp":  time.Now(),
}

js.Publish(subject, marshal(data))
```

**Subject Pattern**: `dc.{tool_id}.{chamber_id}.completed`

**Message Data**:
```json
{
  "toolId": "tool001",
  "chamberId": "chamber1",
  "contextId": "ctx_20251024_103045",
  "tChartKey": "tool001:chamber1:ctx_20251024_103045",
  "timestamp": "2025-10-24T10:30:45Z"
}
```

**Guarantees**:
- At-least-once delivery (JetStream WorkQueue)
- Durable storage (survives NATS restart)
- Automatic redelivery if not acked within 30s

## Defender → Cassandra

### Reading T-Charts

**Operation**: Fetch T-chart for calculation

**Protocol**: CQL SELECT with partition key

**Flow**:
```go
query := `SELECT data FROM t_charts
          WHERE tool_id = ? AND chamber_id = ? AND context_id = ?`
result := session.Query(query, toolID, chamberID, contextID).Scan(&blob)

// Decompress Mebo blob
tchartData := mebo.Decompress(blob)
```

**Latency**: 50-100ms (p95) with local datacenter

### Reading U-Chart History

**Operation**: Fetch recent U-chart values for cache miss

**Protocol**: CQL SELECT with LIMIT

**Flow**:
```go
query := `SELECT context_id, value, timestamp FROM u_charts
          WHERE tool_id = ? AND chamber_id = ?
          ORDER BY timestamp DESC LIMIT 100`
rows := session.Query(query, toolID, chamberID).Iter()
```

**Latency**: 50-100ms (p95)

### Writing U-Charts

**Operation**: Store calculated U-chart results

**Protocol**: CQL INSERT with TTL

**Flow**:
```go
query := `INSERT INTO u_charts
          (tool_id, chamber_id, context_id, value, timestamp)
          VALUES (?, ?, ?, ?, ?)
          USING TTL 2592000` // 30 days
session.Query(query, toolID, chamberID, contextID, uchartValue, time.Now()).Exec()
```

**Latency**: 10-20ms (p95) with async writes

## Defender → Election Agent

### Leader Election (All Defenders)

**Operation**: Participate in leader election

**Protocol**: gRPC with Raft consensus

**Flow**:
```go
// On startup, after claiming stable ID
client := election.NewElectionClient(conn)

// Attempt to acquire leadership
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
resp, err := client.AcquireLeadership(ctx, &election.AcquireRequest{
    DefenderID: "defender-5",
    TTL:        10, // 10-second lease
})

if resp.IsLeader {
    // Became leader, start assignment controller
    startAssignmentController()

    // Renew leadership every 5 seconds
    go renewLeadership(resp.LeadershipToken)
} else {
    // Not leader, start follower mode
    startFollowerMode()
}
```

### Leadership Renewal (Leader Only)

**Operation**: Maintain leadership lease

**Protocol**: gRPC renewal every 5 seconds (half of TTL)

**Flow**:
```go
ticker := time.NewTicker(5 * time.Second)
for range ticker.C {
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    _, err := client.RenewLeadership(ctx, &election.RenewRequest{
        LeadershipToken: token,
    })

    if err != nil {
        // Lost leadership, step down
        log.Error("Lost leadership, stepping down")
        stopAssignmentController()
        return
    }
}
```

**Leadership Loss Scenarios**:
1. **Leader crashes**: Lease expires after 10s, election agent elects new leader
2. **Network partition**: Leader can't renew, loses leadership after 10s
3. **Explicit release**: Leader calls `ReleaseLeadership()` on shutdown

## Related Documents

- [NATS JetStream Setup](./nats-jetstream-setup.md) - Stream and KV configuration
- [Data Flow](../../03-architecture/data-flow.md) - End-to-end message flows
- [State Machine](../../03-architecture/state-machine.md) - Cluster state transitions
