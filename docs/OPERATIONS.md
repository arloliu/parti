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
| NATS Server   | 2.10.0+ with JetStream enabled       |
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

### KV Bucket Pre-Creation (Optional)

Parti auto-creates KV buckets, but you can pre-create them for custom settings:

```bash
# Create buckets with custom retention
nats kv add parti-my-cluster-stableid --replicas=3 --ttl=1h
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
    image: nats:2.10-alpine
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
    ScalingWindow:     10 * time.Second,  // Wait for stability
    ColdStartWindow:   30 * time.Second,  // Fresh cluster
    WorkerIDTTL:       30 * time.Second,  // ID claim duration
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
    ScalingWindow:   15 * time.Second,  // Increase from 10s
    ColdStartWindow: 45 * time.Second,  // Increase from 30s
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

### Resource Estimates

| Workers | NATS Memory | KV Storage | Network (steady) |
|---------|-------------|------------|------------------|
| 10      | 50 MB       | 1 MB       | 10 KB/s          |
| 50      | 100 MB      | 5 MB       | 50 KB/s          |
| 100     | 200 MB      | 10 MB      | 100 KB/s         |

### Performance Tuning

For high-throughput scenarios:

```go
cfg := &parti.Config{
    HeartbeatInterval:      3 * time.Second,   // Faster detection
    HeartbeatMissThreshold: 2,                  // Quicker failover
    ScalingWindow:          5 * time.Second,   // Faster rebalancing
}
```

For stability over speed:

```go
cfg := &parti.Config{
    HeartbeatInterval:      10 * time.Second,  // Less network traffic
    HeartbeatMissThreshold: 4,                  // Tolerate brief issues
    ScalingWindow:          20 * time.Second,  // Avoid churn
}
```

See [Configuration Guide](CONFIGURATION.md) for all options.
