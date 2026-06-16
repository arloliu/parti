# Parti Operations Guide

> Deployment, monitoring, and operational procedures for Parti.

**Related Documentation:**
- [Docs README](README.md) - Documentation map
- [Configuration Guide](CONFIGURATION.md) - Configuration options
- [Lifecycle Guide](LIFECYCLE.md) - Worker states and degraded mode
- [Reference](REFERENCE.md) - Hooks, errors, best practices

---

## Table of Contents

1. [Prerequisites](#prerequisites)
2. [Deployment Patterns](#deployment-patterns)
3. [Health Checks](#health-checks)
4. [Observability](#observability)
5. [Operational Procedures](#operational-procedures)
6. [Troubleshooting](#troubleshooting)

---

## Prerequisites

### System Requirements

| Component     | Requirement                          |
|---------------|--------------------------------------|
| Go            | 1.25 or later                        |
| NATS Server   | 2.10.0+ (2.12+ recommended at scale) |
| Memory        | 50-100 MB per worker (typical)       |
| Network       | Low-latency connection to NATS       |

### NATS Server Configuration

Ensure JetStream is enabled:

```conf
# nats-server.conf
jetstream {
    store_dir: "/data/jetstream"
    max_memory_store: 1GB
    max_file_store: 10GB
}
```

Recommended settings for production:

```conf
jetstream {
    store_dir: "/data/jetstream"
    max_memory_store: 4GB
    max_file_store: 100GB

    # Domain for multi-cluster (optional)
    domain: "production"
}

# Clustering for high availability
cluster {
    name: "nats-cluster"
    listen: 0.0.0.0:6222
    routes: [
        "nats://nats-1:6222",
        "nats://nats-2:6222",
        "nats://nats-3:6222"
    ]
}
```

**Cluster size and replicas.** Run **at least 3 nodes** and use **R=3** for the
application stream and parti's KV buckets — this is the HA posture validated by
the scaling study (3-node cluster, RF=3 stream, R=3 consumers). A single node
has no failover; 5 nodes (RF=5) only buys extra message-stream durability and is
not required.

**Metacontroller snapshots at large fleets (NATS ≥ 2.12).** Each parti partition
is one JetStream durable, so a large fleet (≈10k+ consumers) drives the JetStream
metacontroller's Raft snapshot. On **NATS ≥ 2.12 snapshots are asynchronous** —
at 10k consumers a snapshot is a ~30 ms background operation, ~20–40× below the
pre-async blocking behavior. Guidance:

- **Stay on NATS ≥ 2.12** for any fleet past a few thousand partitions.
- **Do not lower `meta_compact_size`** — it is gated by an internal floor and has
  no effect at this scale (it cannot make snapshots cheaper).
- **Do not set `JetStreamMetaCompactSync`** — it forces blocking snapshots,
  reintroducing the cost async removed.

There is effectively no metacontroller knob to turn at ≤10k consumers on ≥2.12.

### NATS Client Connection

Parti expects the caller-owned `*nats.Conn` to be configured to ride
through transient NATS outages. The library's recovery design assumes
the connection eventually reconnects; a connection that gives up turns
a transient outage into a permanent `CLOSED` zombie that the manager's
connection monitor then escalates into degraded mode and pod rotation.

**Recommended posture:**

```go
nc, err := nats.Connect(natsURL,
    // Unlimited reconnect budget — required for the library's
    // recovery design. A finite cap will WARN at Manager.Start
    // and, on a sustained outage, force degraded mode + pod
    // rotation.
    nats.MaxReconnects(-1),

    // Spread reconnect attempts and add jitter so a NATS outage
    // does not produce a thundering herd of synchronized reconnects
    // when service is restored.
    nats.ReconnectWait(2*time.Second),
    nats.ReconnectJitter(500*time.Millisecond, 2*time.Second),

    // Tolerate NATS startup races — return success once the client
    // is connected, even if the first dial attempt failed.
    nats.RetryOnFailedConnect(true),
)
```

**Why `MaxReconnects(-1)` is required.** With a finite cap, the
`*nats.Conn` exhausts its reconnect budget on a sustained outage and
goes `CLOSED`. The Parti manager's connection monitor detects this
and enters degraded mode (`OnDegraded` fires; readiness probe trips
on the next health check) — the pod then rotates, which is more
disruptive than letting the client reconnect. The library emits a
`Warn`-level log line at `Manager.Start` if `MaxReconnect` is not
negative; the warning is informational only, not blocking.

**Other knobs.** `nats.Name(...)`, `nats.UserCredentials(...)`,
`nats.RootCAs(...)`, etc. are orthogonal to the recovery posture —
configure as your environment requires.

### Degraded Reason Taxonomy

`StateDegraded` is a readiness signal. The `OnDegraded` reason tells
operators whether the expected response is ride-through, in-process
recovery, rotation, or caller-owned recovery.

| Reason | Class | Operator action |
|---|---|---|
| `NATS connection down` | ride-through if reconnecting | Keep readiness degraded until NATS is stable; rotate only if the connection is closed or the outage exceeds policy. |
| `kv-unavailable` | connected but KV quorum unavailable | Keep readiness degraded; rotation is acceptable if the outage exceeds SLO. |
| `KV error threshold exceeded` | Parti-owned coordination data missing/lost | Restart or rotate workers after confirming bucket loss. |
| `bucket-recreated:<bucket>` | ambiguous Parti-owned data loss | Restart or rotate workers; inspect JetStream storage before trusting the recreated bucket. |
| `startup-timeout` | startup apply/wait did not reach Stable in budget | Readiness rotation unless the runner recovers before the pod is replaced. |
| `assignment-watcher-exhausted` | assignment watcher retry envelope exhausted | Restart or rotate the worker; inspect the assignment bucket and NATS logs. |
| `stream-missing-recovery-exhausted` | **terminal** — dynamic consumer stream missing, recovery exhausted | The worker stays `Degraded` permanently until restarted or rotated; the dead partition-consumer loop cannot restart in-process and stream recreation alone does not revive it. Recreate the stream, then rotate the worker. |
| `source-unavailable:<bucket>` | caller-owned source bucket unavailable | Caller/operator recovers the source bucket; Parti does not recreate it. |

**Recovery bound — worker-ID lease (`WorkerIDTTL`).** M5 recover-to-Stable across
a NATS outage is bounded by `WorkerIDTTL` (default 75s; the stableID bucket
`MaxAge` is reconciled to it). An outage that exceeds `WorkerIDTTL` ages out the
worker-ID lease; on reconnect the renewal sees a revision mismatch, surfaces a
bare `ErrClaimLost`, and the worker **self-stops to `StateShutdown`** — the
deliberate split-brain-safe behavior, since a peer may have taken the slot. The
boundary is minute-scale at defaults (renewal cadence ~`WorkerIDTTL/3`; purge at
last-renewal + `WorkerIDTTL`), so a ~1-minute blip can rotate the **whole fleet**
(every disconnected worker crosses the boundary together). Recovery is
orchestrator rotation (the readiness probe sees `StateShutdown` and the pod is
replaced); Parti does not auto-reclaim a lease that aged out, because at the
`ErrClaimLost` surface it cannot distinguish "my lease expired, slot empty" from
"a peer took the slot". Raise `WorkerIDTTL` above the worst-case outage to move
the boundary.

### KV Bucket Pre-Creation (Optional)

Parti auto-creates KV buckets, but you can pre-create them for custom settings:

```bash
# Create buckets with custom retention
# NOTE: the stableID bucket TTL is reconciled to WorkerIDTTL on Manager.Start;
# any TTL specified here (including --ttl=0 / unlimited) will be overwritten.
# Omit --ttl for the stableID bucket and let Parti set it from WorkerIDTTL.
nats kv add parti-my-cluster-stableid --replicas=3
nats kv add parti-my-cluster-election --replicas=3
nats kv add parti-my-cluster-heartbeat --replicas=3 --ttl=5m
nats kv add parti-my-cluster-assignment --replicas=3
```

---

## Deployment Patterns

### Kubernetes Deployment

#### Basic Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: parti-worker
spec:
  replicas: 3
  selector:
    matchLabels:
      app: parti-worker
  template:
    metadata:
      labels:
        app: parti-worker
    spec:
      containers:
      - name: worker
        image: your-app:latest
        env:
        - name: NATS_URL
          value: "nats://nats:4222"
        - name: PARTI_CLUSTER
          value: "production"
        - name: PARTI_WORKER_MAX
          value: "99"
        resources:
          requests:
            memory: "64Mi"
            cpu: "100m"
          limits:
            memory: "256Mi"
            cpu: "500m"
        livenessProbe:
          httpGet:
            path: /health/live
            port: 8080
          initialDelaySeconds: 10
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /health/ready
            port: 8080
          initialDelaySeconds: 5
          periodSeconds: 5
```

#### Rolling Update Strategy

```yaml
spec:
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  minReadySeconds: 30  # Allow stabilization window
```

#### Pod Disruption Budget

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: parti-worker-pdb
spec:
  minAvailable: 2
  selector:
    matchLabels:
      app: parti-worker
```

### Docker Compose

```yaml
version: '3.8'

services:
  nats:
    image: nats:2.14-alpine  # 2.10 is the documented minimum; 2.12+ recommended at scale
    command: ["--jetstream", "--store_dir=/data"]
    volumes:
      - nats-data:/data
    ports:
      - "4222:4222"
      - "8222:8222"  # Monitoring

  worker-1:
    image: your-app:latest
    environment:
      NATS_URL: nats://nats:4222
      PARTI_CLUSTER: local
      PARTI_WORKER_MAX: 9
    depends_on:
      - nats
    restart: unless-stopped

  worker-2:
    image: your-app:latest
    environment:
      NATS_URL: nats://nats:4222
      PARTI_CLUSTER: local
      PARTI_WORKER_MAX: 9
    depends_on:
      - nats
    restart: unless-stopped

volumes:
  nats-data:
```

### Standalone Deployment

```bash
#!/bin/bash
# start-worker.sh

export NATS_URL="nats://nats.example.com:4222"
export PARTI_CLUSTER="production"
export PARTI_WORKER_MAX="99"
export PARTI_HEARTBEAT_INTERVAL="5s"

# Start with graceful shutdown handling
exec ./your-app
```

Systemd service file:

```ini
[Unit]
Description=Parti Worker
After=network.target

[Service]
Type=simple
User=parti
WorkingDirectory=/opt/parti
ExecStart=/opt/parti/worker
Restart=always
RestartSec=5
TimeoutStopSec=30
KillMode=mixed
KillSignal=SIGTERM

Environment=NATS_URL=nats://localhost:4222
Environment=PARTI_CLUSTER=production

[Install]
WantedBy=multi-user.target
```

---

## Health Checks

### Health Check Endpoints

Implement health endpoints in your application:

```go
package main

import (
    "encoding/json"
    "net/http"

    "github.com/arloliu/parti/v2"
)

type HealthHandler struct {
    mgr *parti.Manager
}

// Liveness: Is the process running?
func (h *HealthHandler) LivenessHandler(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK)
    w.Write([]byte("OK"))
}

// Readiness: Can the worker accept work?
func (h *HealthHandler) ReadinessHandler(w http.ResponseWriter, r *http.Request) {
  state := h.mgr.State()

    switch state {
    case parti.StateStable, parti.StateScaling, parti.StateRebalancing:
        w.WriteHeader(http.StatusOK)
        json.NewEncoder(w).Encode(map[string]any{
            "status":    "ready",
            "state":     state.String(),
            "worker_id": h.mgr.WorkerID(),
            "leader":    h.mgr.IsLeader(),
        })
    case parti.StateDegraded:
        // Degraded but still processing
        w.WriteHeader(http.StatusOK)
        json.NewEncoder(w).Encode(map[string]any{
            "status": "degraded",
            "state":  state.String(),
        })
    default:
        w.WriteHeader(http.StatusServiceUnavailable)
        json.NewEncoder(w).Encode(map[string]any{
            "status": "not_ready",
            "state":  state.String(),
        })
    }
}

// Detailed status for debugging
func (h *HealthHandler) StatusHandler(w http.ResponseWriter, r *http.Request) {
  assignment := h.mgr.CurrentAssignment()

  partitionIDs := make([]string, len(assignment.Partitions))
  for i, p := range assignment.Partitions {
        partitionIDs[i] = p.ID
    }

    json.NewEncoder(w).Encode(map[string]any{
      "state":           h.mgr.State().String(),
      "worker_id":       h.mgr.WorkerID(),
        "is_leader":       h.mgr.IsLeader(),
      "partition_count": len(assignment.Partitions),
        "partitions":      partitionIDs,
    })
}
```

### Health Check Configuration

| Check      | Endpoint        | Success States                           | Timeout |
|------------|-----------------|------------------------------------------|---------|
| Liveness   | `/health/live`  | Always (process running)                 | 1s      |
| Readiness  | `/health/ready` | Stable, Scaling, Rebalancing, Degraded   | 2s      |
| Startup    | `/health/ready` | Same as readiness                        | 60s     |

---

## Observability

### Metrics

Export Prometheus metrics using hooks:

```go
import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

var (
    stateGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
        Name: "parti_worker_state",
        Help: "Current worker state (1=active)",
    }, []string{"state"})

    partitionGauge = promauto.NewGauge(prometheus.GaugeOpts{
        Name: "parti_assigned_partitions",
        Help: "Number of assigned partitions",
    })

    leaderGauge = promauto.NewGauge(prometheus.GaugeOpts{
        Name: "parti_is_leader",
        Help: "Whether this worker is the leader (1=yes, 0=no)",
    })

    stateTransitions = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "parti_state_transitions_total",
        Help: "Total state transitions",
    }, []string{"from", "to"})

    degradedDuration = promauto.NewHistogram(prometheus.HistogramOpts{
        Name:    "parti_degraded_duration_seconds",
        Help:    "Duration of degraded mode episodes",
        Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600},
    })
)

func metricsHooks() *parti.Hooks {
    var degradedStart time.Time

    return &parti.Hooks{
        OnStateChanged: func(ctx context.Context, from, to parti.State) error {
            // Reset all states
            for _, s := range []string{"init", "claiming_id", "election",
                "waiting_assignment", "stable", "scaling", "rebalancing",
                "emergency", "degraded", "shutdown"} {
                stateGauge.WithLabelValues(s).Set(0)
            }
            stateGauge.WithLabelValues(to.String()).Set(1)
            stateTransitions.WithLabelValues(from.String(), to.String()).Inc()

            if to == parti.StateDegraded {
              degradedStart = time.Now()
            }
            if from == parti.StateDegraded && to != parti.StateDegraded {
              if !degradedStart.IsZero() {
                degradedDuration.Observe(time.Since(degradedStart).Seconds())
                degradedStart = time.Time{}
              }
            }
            return nil
        },

          OnAssignmentChanged: func(ctx context.Context, _old, newPartitions []parti.Partition) error {
            partitionGauge.Set(float64(len(newPartitions)))
            return nil
        },

          OnLeadershipChanged: func(ctx context.Context, isLeader bool) error {
            if isLeader {
              leaderGauge.Set(1)
            } else {
              leaderGauge.Set(0)
            }
            return nil
        },
    }
}
```

### Key Metrics to Monitor

| Metric                          | Description                    | Alert Threshold              |
|---------------------------------|--------------------------------|------------------------------|
| `parti_worker_state`            | Current state                  | degraded > 5min              |
| `parti_assigned_partitions`     | Partition count                | Variance > 50% across workers|
| `parti_is_leader`               | Leadership status              | No leader for > 30s          |
| `parti_state_transitions_total` | State changes                  | High churn rate              |
| `parti_degraded_duration`       | Time in degraded mode          | p99 > 60s                    |

### Logging

Structured logging example:

```go
hooks := &parti.Hooks{
    OnStateChanged: func(ctx context.Context, from, to parti.State) error {
        slog.Info("state transition",
            "from", from.String(),
            "to", to.String(),
      "worker_id", mgr.WorkerID(),
        )
        return nil
    },

  OnAssignmentChanged: func(ctx context.Context, _old, newPartitions []parti.Partition) error {
    ids := make([]string, len(newPartitions))
    for i, p := range newPartitions {
            ids[i] = p.ID
        }
        slog.Info("assignment received",
      "partition_count", len(newPartitions),
            "partitions", ids,
        )
        return nil
    },

    OnError: func(ctx context.Context, err error) error {
        slog.Error("parti error",
            "error", err,
        )
        return nil
    },
}
```

### Recommended Log Levels

| Event                  | Level  | Description                          |
|------------------------|--------|--------------------------------------|
| State transition       | INFO   | Normal lifecycle                     |
| Assignment change      | INFO   | Partition reassignment               |
| Became leader          | INFO   | Leadership acquired                  |
| Lost leadership        | INFO   | Leadership released                  |
| Degraded mode enter    | WARN   | NATS connectivity issues             |
| Degraded mode exit     | INFO   | Connectivity restored                |
| Handoff prepare        | DEBUG  | Two-phase handoff details            |
| Handoff commit         | DEBUG  | Two-phase handoff details            |
| Recoverable error      | ERROR  | Errors handled internally            |

---

## Operational Procedures

### Scaling Up

1. Deploy new worker instances
2. Workers automatically claim stable IDs
3. Leader detects new workers via heartbeat
4. Stabilization window starts (10s default)
5. Leader calculates new assignment
6. Partitions redistribute

**No manual intervention required.**

### Scaling Down

1. Stop worker gracefully (SIGTERM)
2. Worker releases stable ID
3. Leader detects missing heartbeat
4. Stabilization window starts
5. Leader redistributes orphaned partitions

**Ensure graceful shutdown:**

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

if err := mgr.Stop(ctx); err != nil {
    log.Error("shutdown error", "error", err)
}
```

### Rolling Updates

1. Set `maxUnavailable: 0` in Kubernetes
2. New pod starts and becomes ready
3. Old pod receives SIGTERM
4. Old pod gracefully shuts down
5. Partitions rebalance

**Recommended settings:**

```go
cfg := &parti.Config{
    PlannedScaleWindow: 10 * time.Second,  // Wait for stability
    ColdStartWindow:    30 * time.Second,  // Fresh cluster
    WorkerIDTTL:        30 * time.Second,  // ID claim duration
}
```

### Leader Failover

Automatic process:
1. Current leader stops or crashes
2. Other workers detect missing heartbeat
3. New election occurs (NATS KV atomic operations)
4. New leader takes over assignment calculation
5. Existing assignments remain stable

**Recovery time:** ~2-3 heartbeat intervals (10-15s default)

### Election Bucket Storage Migration

The election bucket's storage type changed from `MemoryStorage` to
`FileStorage` so the leadership lease key survives a NATS node
restart. Previously, every NATS node restart lost the lease and
triggered fleet-wide leadership churn.

**Existing clusters need a one-time bucket replacement** because
`EnsureKVBucketWithRetry` is get-first — it does not upgrade an
existing `MemoryStorage` bucket to `FileStorage`.

Pre-flight: confirm the F1 epoch fence is in your build (look in
release notes, or check operational logs for `bucket-recreated`
warn entries — any prior occurrence confirms the fence is wired).
The fence is the migration's safety net.

Migration steps, during a planned maintenance window:

```bash
# 1. Delete the bucket (this also removes the backing KV_<bucket> stream).
nats kv rm parti-<cluster>-election

# 2. Paranoia check — confirm the backing stream is gone.
nats stream ls | grep KV_parti-<cluster>-election  # expect no output

# 3. Recreate with the desired replica count (recommended: 3 in a
#    multi-node cluster). The new bucket MUST be FileStorage; this
#    matches what the runtime creates on a fresh deploy.
nats kv add parti-<cluster>-election --replicas=3 --storage=file

# 4. Rolling restart of Parti workers (any order is fine).
```

Workers that observed the deletion before their own restart enter
degraded mode via the F1 epoch fence (reason
`bucket-recreated:parti-<cluster>-election`); the existing
`OnDegraded → readiness probe → pod rotation` path completes the
migration automatically.

Post-migration verification:

```bash
nats kv info parti-<cluster>-election | grep -i storage  # expect "File"
```

After the migration a single-node restart in a 3-replica cluster
no longer causes leadership churn.

### Heartbeat Bucket Storage Migration

The heartbeat bucket's default storage type changed from `MemoryStorage` to
`FileStorage` so the heartbeat stream survives a single-node NATS restart.
Previously, a single-node restart dropped the `MemoryStorage` heartbeat
stream; the heartbeat publisher's `Put` then kept failing against the dead
stream and the fleet flapped `Degraded`↔`Stable` without ever holding Stable.

**Existing clusters need a one-time bucket replacement** because
`EnsureKVBucketWithRetry` is get-first — it does not upgrade an existing
`MemoryStorage` bucket to `FileStorage`.

**Until migrated, an existing bucket keeps `MemoryStorage`,** so a single-node
NATS restart still loses its heartbeat stream. The heartbeat-reachability
recovery guard holds such a worker in **terminal `Degraded`** — it does not
flap back to `Stable`. (A missing heartbeat stream degrades with the
whole-bucket-loss reason `KV error threshold exceeded`, and the guard refuses
the recovery exit while the bucket is unreachable.) This is loud and
rotatable: `OnDegraded → readiness probe → pod rotation` rotates the worker,
and because the lost bucket no longer exists, the rotating pod re-creates it
as `FileStorage` — so a rotation completes the migration automatically. You
can also migrate explicitly during a maintenance window (below) instead of
waiting for a restart to trigger it.

Migration steps, during a planned maintenance window:

```bash
# 1. Delete the bucket (also removes the backing KV_<bucket> stream).
nats kv rm parti-<cluster>-heartbeat

# 2. Paranoia check — confirm the backing stream is gone.
nats stream ls | grep KV_parti-<cluster>-heartbeat  # expect no output

# 3. Recreate as FileStorage (match your HeartbeatTTL and replica count).
nats kv add parti-<cluster>-heartbeat --storage=file --ttl=15s --replicas=1

# 4. Rolling restart of Parti workers (any order is fine).
```

Workers that observed the deletion enter degraded via the epoch fence (reason
`bucket-recreated:parti-<cluster>-heartbeat`) and, with the Phase-1 live epoch
re-probe, stay terminally `Degraded`. The existing
`OnDegraded → readiness probe → pod rotation` path (plus the rolling restart
in step 4) completes the migration.

Post-migration verification:

```bash
nats kv info parti-<cluster>-heartbeat | grep -i storage  # expect "File"
```

### Tuning OperationTimeout vs ElectionTimeout

The leader's renew loop has three attempts within `ElectionTimeout`
to refresh the lease; each attempt's timeout is `OperationTimeout`.
If `OperationTimeout > ElectionTimeout / 3`, a single slow renew
can consume the entire lease budget and produce a false leadership
flip. The library logs a one-shot WARN at `Manager.Start` when this
ratio is exceeded. The recommended posture:

```
OperationTimeout <= ElectionTimeout / 3
```

At the default pair (both 10s) the warning fires; in production
prefer something like `OperationTimeout=3s, ElectionTimeout=10s`.

### NATS Cluster Maintenance

When performing NATS maintenance:

1. **Rolling NATS restart:** No action needed, workers enter degraded mode briefly
2. **Full NATS outage:**
   - Workers enter degraded mode
   - Continue processing with cached assignments
   - Resume normal operation when NATS returns
3. **NATS cluster migration:**
   - Update connection URLs in worker config
   - Perform rolling restart of workers

---

## Troubleshooting

### Common Issues

#### Workers Not Getting Assignments

**Symptoms:** Workers stuck in `WaitingAssignment` state

**Causes:**
- No leader elected
- Leader can't access NATS KV
- Partition source returns empty list

**Resolution:**
```bash
# Check leader election bucket
nats kv get parti-<cluster>-election leader

# Check heartbeat bucket for active workers
nats kv ls parti-<cluster>-heartbeat

# Check assignment bucket
nats kv get parti-<cluster>-assignment current
```

#### High Partition Churn

**Symptoms:** Frequent assignment changes, high `state_transitions` count

**Causes:**
- Stabilization window too short
- Workers frequently restarting
- Network instability

**Resolution:**
```go
cfg := &parti.Config{
    PlannedScaleWindow: 15 * time.Second,  // Increase from 10s
    ColdStartWindow:    45 * time.Second,  // Increase from 30s
}
```

#### Workers Stuck in Degraded Mode

**Symptoms:** Workers remain in `Degraded` state

**Causes:**
- NATS connectivity issues
- KV bucket access problems
- Network partitioning

**Resolution:**
```bash
# Check NATS connectivity
nats server ping

# Check JetStream status
nats server report jetstream

# Check KV bucket health
nats kv info parti-<cluster>-stableid
```

#### Live NATS Data Loss (Bucket Wipe While Workers Run)

**Symptoms:** All running workers transition to `Degraded`; `OnDegraded` fires with reason "KV error threshold exceeded"; assignment versions stay frozen at their pre-incident values; no leader is publishing fresh updates.

**Causes (what actually wiped the KV buckets):**
- Single-node JetStream with ephemeral storage (container local disk, `emptyDir` PVC) restarted without app-side restart
- Operator ran `nats kv rm` against a Parti-managed bucket
- JetStream cluster peer promoted with empty state before Raft replication caught up
- Disk corruption on the leading replica of a non-replicated (R=1) stream

**Why the process does not self-heal:** Parti deliberately does not auto-recreate buckets from the live publish path. Recreating on a transient `ErrStreamNotFound` during a JetStream leader reshuffle would cause the data loss it was trying to prevent (the bucket would have come back naturally). Parti cannot distinguish "data permanently gone" from "data coming back", so it surfaces the problem via `Degraded` and leaves the recovery decision to the operator (or k8s).

**Resolution:**
```bash
# Confirm the wipe actually happened (buckets missing).
nats kv ls | grep parti-<cluster>

# Inspect JetStream for the underlying cause (node restart, peer promotion).
nats server report jetstream --json

# Restart the workers. The restart path recreates buckets via
# ensureKVBucket and is covered by TestManager_Restart_AfterNATSBucketLoss.
kubectl rollout restart deployment/parti
```

**Prevention:**
- Use replicated JetStream (R ≥ 3) for Parti's KV buckets so a single node's data loss does not wipe state
- Use persistent storage (not `emptyDir`) for JetStream file storage
- Gate `nats kv rm` behind an operator runbook; the command is not distinguishable from accidental wipe at the Parti level
- Wire `OnDegraded` to fail a k8s readiness probe (see `examples/degraded-readiness`) so pods are rotated automatically instead of drifting

**Expected log noise during the incident:** while the buckets are missing the recovery loop retries `refreshAssignmentFromNATS` each second and surfaces `failed to refresh assignment during recovery` warnings plus periodic `KV error threshold exceeded` entries. This is expected — it stops as soon as the buckets are restored (by a process restart) and the manager exits Degraded.

#### Worker Self-Stop After Stable ID Claim Loss

**Symptoms:** A single worker stops itself; `OnError` fires with an error wrapping `worker ID claim lost`; the worker logs `stable ID claim lost, stopping renewal` followed by `stable worker ID claim lost, stopping worker`.

**Causes:**
- The worker lost stableID KV connectivity for longer than `WorkerIDTTL`, so its claim key expired and another worker reclaimed the ID
- The worker's key went stale (missed ~3 consecutive renewals) and was taken over by a restarting worker

**Why parti does not auto-recover:** A lost claim is unrecoverable in place — the stable ID now belongs to another worker, and renewing would clobber the new owner's key. By the time a claim is lost the worker has also missed its heartbeat lease, so the cluster has already reassigned its partitions. Parti therefore stops the worker and revokes its consumer rather than fight the new owner.

**Resolution:** The embedding application must start a **fresh worker** — parti does not restart itself. Wire `OnError` to trigger a process restart, or run the worker under a supervisor (systemd, k8s) that restarts the process on exit. The restarted worker claims a new ID from the pool (or reclaims its old one if still within the TTL window).

#### Stable ID Pool Exhaustion

**Symptoms:** New workers fail to start with an error indicating no available worker IDs

**Causes:**
- `WorkerIDMax` too low for worker count
- Stale ID claims from crashed workers

**Resolution:**
```go
// Increase pool size
cfg := &parti.Config{
    WorkerIDMax: 999,  // Allow up to 1000 workers
    WorkerIDTTL: 30 * time.Second,  // Ensure timely expiration
}
```

```bash
# Check claimed IDs
nats kv ls parti-<cluster>-stableid

# Manually purge stale claim (if necessary)
nats kv del parti-<cluster>-stableid worker-5
```

### Debug Commands

```bash
# List all Parti KV buckets
nats kv ls | grep parti

# Watch real-time changes
nats kv watch parti-<cluster>-assignment

# Export current state
nats kv get parti-<cluster>-assignment current > assignment.json

# Check worker heartbeats
nats kv ls parti-<cluster>-heartbeat

# Monitor NATS JetStream health
nats server report jetstream --json
```

### Log Analysis

Key log patterns to search for:

```bash
# State transitions
grep "state transition" /var/log/app.log

# Assignment changes
grep "assignment received" /var/log/app.log

# Degraded mode events
grep -E "degraded|recovered" /var/log/app.log

# Errors
grep "parti error" /var/log/app.log
```

---

## Capacity Planning

### Worker Count Guidelines

| Partitions | Recommended Workers | Notes                           |
|------------|---------------------|----------------------------------|
| 16         | 2-8                 | Good for development             |
| 32         | 4-16                | Small production                 |
| 64         | 8-32                | Medium production                |
| 128        | 16-64               | Large production                 |
| 256+       | 32-128              | Consider cluster sharding        |

> This table sizes the **worker** count (how many pods share the load); it is a
> different axis from the **partition** count. With `consumer.Dynamic` (one
> consumer per partition), parti has been validated to **10,000 partitions** on a
> 3-node cluster — see "NATS-Side Cost" below. Partition count is bounded by
> NATS-side RSS, not by parti.

### Resource Estimates (per worker pod)

| Workers | NATS Memory | KV Storage | Network (steady) |
|---------|-------------|------------|------------------|
| 10      | 50 MB       | 1 MB       | 10 KB/s          |
| 50      | 100 MB      | 5 MB       | 50 KB/s          |
| 100     | 200 MB      | 10 MB      | 100 KB/s         |

### NATS-Side Cost (partition scaling)

The table above estimates the **worker pod** footprint. The NATS **server-side**
cost scales with the number of partitions (one JetStream durable each). The
scaling study measured the recommended config (`consumer.Dynamic` with memory
consumer state + R=3) and fit an affine model, validated to **N = 10,000**
partitions:

| Resource | Scaling | Notes |
|----------|---------|-------|
| Cluster RSS | ~**0.793 MiB per partition** (+ ~90 MiB baseline) | The binding constraint — size NATS memory by partition count |
| Latency | flat **~1.3 ms P95/P99**, independent of N | No per-partition fetch tax vs the JetStream floor |
| Write IOPS / CPU | sub-linear in N | Never the wall on modern NVMe/gp3 |

Worked point: at **N = 5,000** (memory consumer state + R=3) the cluster sits at
~4 GiB RSS, ~1.3 ms P99. **RSS is the first ceiling you hit**, not IOPS or CPU —
provision NATS memory accordingly. For consumer-state storage tuning (the IOPS
lever behind this config), see [`CONSUMERS.md`](CONSUMERS.md); for pushing
partition count far beyond 10k with a bounded consumer count, see
[`SCALING.md`](SCALING.md).

### Performance Tuning

For high-throughput scenarios:

```go
cfg := &parti.Config{
    HeartbeatInterval:  3 * time.Second,   // Faster detection
    HeartbeatTTL:       6 * time.Second,   // 2× interval — quicker failover
    PlannedScaleWindow: 5 * time.Second,   // Faster rebalancing
}
```

For stability over speed:

```go
cfg := &parti.Config{
    HeartbeatInterval:  10 * time.Second,  // Less network traffic
    HeartbeatTTL:       40 * time.Second,  // 4× interval — tolerate brief issues
    PlannedScaleWindow: 20 * time.Second,  // Avoid churn
}
```

See [Configuration Guide](CONFIGURATION.md) for all options.


---

## Consumer-Create Rate Limiting

**Default:** opt-in, default OFF. Existing deployments upgrading to this version see no behaviour change.

A large dynamic-partition assignment or a mass consumer-recovery event can cause a "consumer-create storm" — a worker issuing hundreds or thousands of `CreateOrUpdateConsumer` RPCs in rapid succession. `WithConsumerCreateRate(perSec, burst)` installs a per-worker token-bucket that gates every physical RPC attempt (including retries) across the initial-assignment add loop and the per-partition recovery/recreation paths.

### Sizing

```
rate ≈ cluster-create-budget / max-workers
```

The NATS cluster's safe aggregate consumer-create rate depends on its replica count, storage type, and stream count. Measure under load and divide by `max-workers` (your worst-case pod count after scale-down). Starting values for most deployments:

```go
consumer.WithConsumerCreateRate(100, 256)  // 100 creates/s, burst 256
```

### Gate-dependency for handoff overlap

Two-phase handoff alone does **not** prevent processing overlap (see [`LIFECYCLE.md`](LIFECYCLE.md) §Two-Phase Handoff and `CONSUMERS.md` §Consumer-Create Rate Limiting). When the processing gate is **OFF** (the default), enabling create-rate limiting **lengthens the period during which old and new owners are both active** to the full paced-apply duration. Co-enable the processing gate / pull-gating to suppress pulls for not-yet-committed partitions if overlap must be minimised.

### StartupTimeout interaction

A paced large cold start may exceed `StartupTimeout` (default 60 s). The startup watchdog transitions the worker to `Degraded` (reason: `startup-timeout`) for probe rotation but does **not** abort the apply (which continues to completion). Size accordingly:

```
StartupTimeout ≥ ColdStartWindow + ElectionTimeout + ceil(partitionCount / rate) + headroom
```

Example: 20 000 partitions at 100/s ≈ 200 s apply duration; set `StartupTimeout` to 5–10 min.

### Migration note

The rate limiter is opt-in. To enable on an existing deployment:

1. Measure your cluster's safe consumer-create rate under load.
2. Add `WithConsumerCreateRate(rate, burst)` to your `NewDynamic` call.
3. If you are running two-phase handoff without the processing gate, co-enable `WithProcessingGate` to prevent lengthened overlap.
4. Consider increasing `StartupTimeout` for large cold starts.
5. Roll out gradually (canary / blue-green); monitor `parti_worker_consumer_create_throttled_total` and `parti_worker_consumer_create_throttle_wait_seconds` (Prometheus sidecar metrics) to confirm the limiter is active and the delay distribution is within target.

### Claim-write residual (out of scope)

The consumer-create limiter does not gate **claim-write** RPCs (`PutIfEpoch` calls in the two-phase coordinator and the startup handoff hygiene loops). Those are a separate, related flood vector documented in [`docs/plans/consumer-create-rate-limit/10-claim-write-ratelimit-plan.md`](plans/consumer-create-rate-limit/10-claim-write-ratelimit-plan.md).
