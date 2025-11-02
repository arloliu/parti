# Parti Operations Guide

**Version**: 1.0.0
**Last Updated**: November 2, 2025
**Library**: `github.com/arloliu/parti`

---

## Table of Contents

1. [Overview](#overview)
2. [Prerequisites](#prerequisites)
3. [Configuration Templates](#configuration-templates)
4. [Deployment Patterns](#deployment-patterns)
5. [Health Checks](#health-checks)
6. [Observability](#observability)
7. [Common Operations](#common-operations)
8. [Troubleshooting](#troubleshooting)

---

## Overview

This guide covers operational aspects of deploying and running Parti-based applications in production environments. It provides:

- Prerequisites for deployment
- Configuration templates for common scenarios
- Deployment patterns (Kubernetes, Docker, standalone)
- Health check implementation
- Observability best practices
- Common operational procedures
- Troubleshooting guide

**Note**: This is a foundation guide. See the full deployment guide (coming in Priority 3) for comprehensive coverage including:
- Monitoring dashboards
- Alert recommendations
- Operational runbooks
- Performance tuning
- High availability setup

---

## Prerequisites

### NATS Server Requirements

**Minimum Version**: NATS Server 2.9.0+
**Recommended Version**: NATS Server 2.10.0+

**Required Features**:
- JetStream enabled
- KV Store configured
- Sufficient resources for bucket operations

**NATS Server Configuration Example**:

```conf
# nats-server.conf
server_name: "nats-cluster"
port: 4222

jetstream {
    store_dir: "/data/jetstream"
    max_memory_store: 2GB
    max_file_store: 10GB
}

# Authentication (recommended for production)
authorization {
    users = [
        {
            user: "parti-worker"
            password: "$2a$11$..."  # bcrypt hash
            permissions {
                publish = [">"]
                subscribe = [">"]
            }
        }
    ]
}
```

### Resource Requirements

**Per Worker Instance**:
- **CPU**: 0.5-1.0 cores
- **Memory**: 256-512 MB (base) + ~200 bytes per partition
- **Network**: 10-50 Kbps (heartbeat + assignment updates)
- **Disk**: Minimal (logging only)

**NATS KV Buckets**:
- **Stable IDs**: ~50 bytes per worker
- **Heartbeats**: ~100 bytes per worker
- **Assignments**: ~50 KB per assignment (5000 partitions, 64 workers)
- **Election**: ~100 bytes

**Example Calculation** (64 workers, 5000 partitions):
- Worker memory: 256 MB + (5000 × 200 bytes) ≈ 257 MB per worker
- NATS KV total: ~3.5 MB

### Network Requirements

- **NATS connectivity**: Reliable connection to NATS server
- **Latency**: <50ms recommended for KV operations
- **Bandwidth**: 1 Mbps per worker sufficient for most deployments
- **Firewall**: Port 4222 (NATS) must be accessible

---

## Configuration Templates

### Development Configuration

For local development and testing with 2-3 workers:

```yaml
# config-dev.yaml
workerIdPrefix: "dev-worker"
workerIdMin: 0
workerIdMax: 2
workerIdTtl: "10s"

heartbeatInterval: "1s"
heartbeatTtl: "3s"

coldStartWindow: "5s"
plannedScaleWindow: "2s"
emergencyGracePeriod: "1s"

operationTimeout: "5s"
electionTimeout: "3s"
startupTimeout: "15s"
shutdownTimeout: "5s"

assignment:
  minRebalanceThreshold: 0.1
  minRebalanceInterval: "2s"
```

### Staging Configuration

For staging environment with 10-20 workers:

```yaml
# config-staging.yaml
workerIdPrefix: "staging-worker"
workerIdMin: 0
workerIdMax: 31
workerIdTtl: "30s"

heartbeatInterval: "2s"
heartbeatTtl: "6s"

coldStartWindow: "20s"
plannedScaleWindow: "8s"
emergencyGracePeriod: "1.5s"

operationTimeout: "10s"
electionTimeout: "5s"
startupTimeout: "25s"
shutdownTimeout: "10s"

assignment:
  minRebalanceThreshold: 0.15
  minRebalanceInterval: "8s"
```

### Production Configuration

For production with 64+ workers:

```yaml
# config-prod.yaml
workerIdPrefix: "worker"
workerIdMin: 0
workerIdMax: 199  # Support up to 200 workers
workerIdTtl: "30s"

heartbeatInterval: "2s"
heartbeatTtl: "6s"

coldStartWindow: "30s"
plannedScaleWindow: "10s"
emergencyGracePeriod: "1.5s"

operationTimeout: "10s"
electionTimeout: "5s"
startupTimeout: "30s"
shutdownTimeout: "10s"

assignment:
  minRebalanceThreshold: 0.15
  minRebalanceInterval: "10s"

# Optional: Custom KV bucket names
kvBucket:
  stableIdBucket: "parti-stable-ids"
  electionBucket: "parti-election"
  heartbeatBucket: "parti-heartbeats"
  assignmentBucket: "parti-assignments"
  assignmentTtl: "0s"  # No expiration
```

### High-Churn Configuration

For environments with frequent scaling:

```yaml
# config-high-churn.yaml
workerIdPrefix: "dynamic-worker"
workerIdMin: 0
workerIdMax: 499
workerIdTtl: "20s"

heartbeatInterval: "1s"
heartbeatTtl: "4s"

coldStartWindow: "20s"
plannedScaleWindow: "5s"
emergencyGracePeriod: "1s"

operationTimeout: "8s"
electionTimeout: "4s"
startupTimeout: "25s"
shutdownTimeout: "8s"

assignment:
  minRebalanceThreshold: 0.2   # Higher threshold to reduce rebalancing
  minRebalanceInterval: "5s"
```

---

## Deployment Patterns

### Kubernetes Deployment

**Deployment Manifest** (`deployment.yaml`):

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: parti-worker
  namespace: default
spec:
  replicas: 10
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
      maxSurge: 1
  selector:
    matchLabels:
      app: parti-worker
  template:
    metadata:
      labels:
        app: parti-worker
    spec:
      terminationGracePeriodSeconds: 30
      containers:
      - name: worker
        image: your-registry/parti-worker:v1.0.0
        env:
        - name: NATS_URL
          value: "nats://nats-service:4222"
        - name: CONFIG_PATH
          value: "/config/config.yaml"
        - name: WORKER_ID_MAX
          value: "63"
        volumeMounts:
        - name: config
          mountPath: /config
          readOnly: true
        resources:
          requests:
            memory: "256Mi"
            cpu: "500m"
          limits:
            memory: "512Mi"
            cpu: "1000m"
        livenessProbe:
          httpGet:
            path: /health/live
            port: 8080
          initialDelaySeconds: 10
          periodSeconds: 10
          timeoutSeconds: 5
          failureThreshold: 3
        readinessProbe:
          httpGet:
            path: /health/ready
            port: 8080
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 3
          failureThreshold: 2
      volumes:
      - name: config
        configMap:
          name: parti-config
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: parti-config
  namespace: default
data:
  config.yaml: |
    workerIdPrefix: "worker"
    workerIdMax: 63
    heartbeatInterval: "2s"
    heartbeatTtl: "6s"
    # ... rest of config
```

### Docker Compose

**Docker Compose** (`docker-compose.yaml`):

```yaml
version: '3.8'

services:
  nats:
    image: nats:2.10
    command: "-js -sd /data"
    ports:
      - "4222:4222"
      - "8222:8222"  # Monitoring
    volumes:
      - nats-data:/data

  worker:
    image: your-registry/parti-worker:v1.0.0
    environment:
      - NATS_URL=nats://nats:4222
      - CONFIG_PATH=/config/config.yaml
    volumes:
      - ./config.yaml:/config/config.yaml:ro
    depends_on:
      - nats
    deploy:
      replicas: 5
      restart_policy:
        condition: on-failure
        delay: 5s
        max_attempts: 3

volumes:
  nats-data:
```

### Standalone Deployment

**Systemd Service** (`/etc/systemd/system/parti-worker.service`):

```ini
[Unit]
Description=Parti Worker
After=network.target nats.service
Wants=nats.service

[Service]
Type=simple
User=parti
Group=parti
WorkingDirectory=/opt/parti-worker
ExecStart=/opt/parti-worker/bin/worker --config /etc/parti/config.yaml
Restart=on-failure
RestartSec=10
Environment="NATS_URL=nats://localhost:4222"

# Graceful shutdown
TimeoutStopSec=30
KillMode=mixed
KillSignal=SIGTERM

# Resource limits
LimitNOFILE=65536
MemoryLimit=512M
CPUQuota=100%

[Install]
WantedBy=multi-user.target
```

**Start Service**:
```bash
sudo systemctl enable parti-worker
sudo systemctl start parti-worker
sudo systemctl status parti-worker
```

---

## Health Checks

### HTTP Health Endpoint Implementation

```go
package main

import (
    "encoding/json"
    "net/http"
    "time"

    "github.com/arloliu/parti"
)

type HealthServer struct {
    mgr *parti.Manager
}

// LivenessProbe checks if worker is alive
func (h *HealthServer) LivenessProbe(w http.ResponseWriter, r *http.Request) {
    state := h.mgr.State()

    // Consider alive if not in shutdown
    if state == parti.StateShutdown {
        http.Error(w, "shutting down", http.StatusServiceUnavailable)
        return
    }

    w.WriteHeader(http.StatusOK)
    w.Write([]byte("OK"))
}

// ReadinessProbe checks if worker is ready to serve traffic
func (h *HealthServer) ReadinessProbe(w http.ResponseWriter, r *http.Request) {
    state := h.mgr.State()

    // Ready only when stable
    if state != parti.StateStable {
        http.Error(w, "not ready", http.StatusServiceUnavailable)
        return
    }

    // Check if has assignments
    assignment := h.mgr.CurrentAssignment()
    if len(assignment.Partitions) == 0 {
        http.Error(w, "no assignments", http.StatusServiceUnavailable)
        return
    }

    w.WriteHeader(http.StatusOK)
    w.Write([]byte("OK"))
}

// DetailedStatus provides detailed worker status
func (h *HealthServer) DetailedStatus(w http.ResponseWriter, r *http.Request) {
    assignment := h.mgr.CurrentAssignment()

    status := map[string]interface{}{
        "workerID":          h.mgr.WorkerID(),
        "state":             h.mgr.State().String(),
        "isLeader":          h.mgr.IsLeader(),
        "assignmentVersion": assignment.Version,
        "partitionCount":    len(assignment.Partitions),
        "lifecycle":         assignment.Lifecycle,
        "timestamp":         time.Now().UTC().Format(time.RFC3339),
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(status)
}

func main() {
    // ... create manager ...

    health := &HealthServer{mgr: mgr}
    http.HandleFunc("/health/live", health.LivenessProbe)
    http.HandleFunc("/health/ready", health.ReadinessProbe)
    http.HandleFunc("/status", health.DetailedStatus)

    go http.ListenAndServe(":8080", nil)

    // ... start manager ...
}
```

### Kubernetes Probes

```yaml
livenessProbe:
  httpGet:
    path: /health/live
    port: 8080
  initialDelaySeconds: 10    # Allow time for startup
  periodSeconds: 10          # Check every 10s
  timeoutSeconds: 5          # Timeout after 5s
  failureThreshold: 3        # Restart after 3 failures

readinessProbe:
  httpGet:
    path: /health/ready
    port: 8080
  initialDelaySeconds: 5     # Check soon after start
  periodSeconds: 5           # Check frequently
  timeoutSeconds: 3          # Quick timeout
  failureThreshold: 2        # Remove from service after 2 failures
```

---

## Observability

### Logging

**Structured Logging Example** (using zap):

```go
package main

import (
    "go.uber.org/zap"
    "github.com/arloliu/parti"
)

func main() {
    // Production logger
    logger, _ := zap.NewProduction()
    defer logger.Sync()
    sugar := logger.Sugar()

    // Configure manager with logger
    mgr, _ := parti.NewManager(cfg, nc, src, strategy,
        parti.WithLogger(sugar),
    )

    // ... rest of setup ...
}
```

**Log Levels**:
- **Debug**: State transitions, assignment details
- **Info**: Startup, shutdown, major events
- **Warn**: Recoverable errors, retries
- **Error**: Unrecoverable errors, failures

**Important Log Messages**:
```
INFO  Manager starting workerId=worker-5
INFO  Leader elected workerId=worker-5 isLeader=true
INFO  Assignment received version=3 partitions=75
WARN  Heartbeat missed workerId=worker-12 retries=1
ERROR Failed to claim stable ID error="all IDs claimed"
```

### Metrics (Foundation)

**Key Metrics to Track**:

| Metric | Type | Description | Labels |
|--------|------|-------------|--------|
| `parti_worker_state` | Gauge | Current worker state (0-8) | `worker_id` |
| `parti_is_leader` | Gauge | Whether this worker is leader (0/1) | `worker_id` |
| `parti_assigned_partitions` | Gauge | Number of assigned partitions | `worker_id` |
| `parti_assignment_version` | Gauge | Current assignment version | `worker_id` |
| `parti_state_transitions_total` | Counter | State transition count | `from_state`, `to_state` |
| `parti_assignment_changes_total` | Counter | Assignment change count | `worker_id` |

**Basic Prometheus Implementation**:

```go
package main

import (
    "time"
    "github.com/prometheus/client_golang/prometheus"
    "github.com/arloliu/parti"
)

type PrometheusCollector struct {
    workerState        prometheus.Gauge
    isLeader           prometheus.Gauge
    assignedPartitions prometheus.Gauge
    assignmentVersion  prometheus.Gauge
}

func NewPrometheusCollector(workerID string) *PrometheusCollector {
    return &PrometheusCollector{
        workerState: prometheus.NewGauge(prometheus.GaugeOpts{
            Name:        "parti_worker_state",
            Help:        "Current worker state",
            ConstLabels: prometheus.Labels{"worker_id": workerID},
        }),
        // ... initialize other metrics ...
    }
}

func (c *PrometheusCollector) RecordStateTransition(from, to parti.State, duration time.Duration) {
    c.workerState.Set(float64(to))
}

func (c *PrometheusCollector) RecordAssignmentChange(added, removed int, version int64) {
    c.assignmentVersion.Set(float64(version))
    c.assignedPartitions.Add(float64(added - removed))
}
```

**Note**: Full metrics implementation including Prometheus dashboards and alerts will be covered in Priority 3.

### Degraded Mode Monitoring

**NEW**: Track degraded mode status and cache health.

**Degraded Mode Metrics**:

| Metric | Type | Description | Labels |
|--------|------|-------------|--------|
| `parti_degraded_mode` | Gauge | Degraded mode state (0=normal, 1=degraded) | `worker_id` |
| `parti_cache_age_seconds` | Gauge | Age of cached assignment (seconds) | `worker_id` |
| `parti_alert_level` | Gauge | Current alert level (0-4) | `worker_id` |
| `parti_degraded_duration_seconds` | Histogram | Time spent in degraded mode | `worker_id` |
| `parti_degraded_alerts_total` | Counter | Count of degraded alerts | `worker_id`, `level` |
| `parti_cache_fallback_total` | Counter | Cache fallback operations | `worker_id` |

**Implementation Example**:

```go
type PrometheusCollector struct {
    // ... existing metrics ...

    // Degraded mode metrics
    degradedMode      prometheus.Gauge
    cacheAge          prometheus.Gauge
    alertLevel        prometheus.Gauge
    degradedDuration  prometheus.Histogram
    alertsTotal       *prometheus.CounterVec
    cacheFallback     prometheus.Counter
}

func (c *PrometheusCollector) SetDegradedMode(value float64) {
    c.degradedMode.Set(value)
}

func (c *PrometheusCollector) SetCacheAge(duration time.Duration) {
    c.cacheAge.Set(duration.Seconds())
}

func (c *PrometheusCollector) IncrementAlertEmitted(level string) {
    c.alertsTotal.WithLabelValues(level).Inc()
}

func (c *PrometheusCollector) RecordDegradedDuration(duration time.Duration) {
    c.degradedDuration.Observe(duration.Seconds())
}
```

**Alert Rules** (Prometheus):

```yaml
# Alert on degraded mode entry
- alert: PartiDegradedMode
  expr: parti_degraded_mode == 1
  for: 1m
  labels:
    severity: warning
  annotations:
    summary: "Parti worker {{ $labels.worker_id }} in degraded mode"
    description: "Worker has been in degraded mode for 1+ minutes. Check NATS connectivity."

# Alert on prolonged degraded mode
- alert: PartiDegradedModeProlonged
  expr: parti_cache_age_seconds > 300
  for: 5m
  labels:
    severity: error
  annotations:
    summary: "Parti worker {{ $labels.worker_id }} degraded for 5+ minutes"
    description: "Cache is {{ $value }}s old. Immediate NATS recovery needed."

# Alert on critical degraded duration
- alert: PartiDegradedModeCritical
  expr: parti_cache_age_seconds > 1800
  for: 1m
  labels:
    severity: critical
  annotations:
    summary: "Parti worker {{ $labels.worker_id }} degraded for 30+ minutes"
    description: "CRITICAL: Cache is {{ $value }}s old. Page on-call immediately."
```

**Dashboard Queries** (PromQL):

```promql
# Count workers in degraded mode
sum(parti_degraded_mode)

# Maximum cache age across all workers
max(parti_cache_age_seconds)

# Alert rate by level
rate(parti_degraded_alerts_total[5m])

# Workers by alert level
parti_alert_level

# Cache fallback rate
rate(parti_cache_fallback_total[5m])
```

**Operational Response**:

| Alert Level | Cache Age | Action Required |
|-------------|-----------|-----------------|
| **Info** | 1-5 min | Monitor NATS, no immediate action |
| **Warn** | 5-15 min | Investigate NATS health, check logs |
| **Error** | 15-30 min | Escalate to ops team, prepare for intervention |
| **Critical** | 30+ min | Page on-call, immediate NATS recovery |

---

## Common Operations

### Scaling Workers

#### Scale Up (Add Workers)

**Kubernetes**:
```bash
kubectl scale deployment parti-worker --replicas=15
```

**Expected Behavior**:
- New workers claim stable IDs
- Enter stabilization window (10s for planned scale)
- Leader calculates new assignments
- Partitions redistributed with >80% cache affinity

**Monitoring**:
- Watch `parti_worker_state` for new workers
- Check assignment version increments
- Verify partition coverage remains 100%

#### Scale Down (Remove Workers)

**Kubernetes**:
```bash
kubectl scale deployment parti-worker --replicas=5
```

**Expected Behavior**:
- Removed workers enter shutdown state
- Stop heartbeats
- Leader detects missing workers
- Partitions redistributed to remaining workers

**Best Practice**: Use rolling updates instead of direct scale-down to maintain cache affinity.

### Rolling Updates

**Kubernetes Rolling Update**:
```bash
kubectl set image deployment/parti-worker worker=your-registry/parti-worker:v1.1.0
```

**Configuration** (in deployment):
```yaml
strategy:
  type: RollingUpdate
  rollingUpdate:
    maxUnavailable: 1  # Replace one at a time
    maxSurge: 1        # Allow one extra during update
```

**Expected Behavior**:
- Workers replaced one-by-one
- Each replacement triggers stabilization window
- >80% cache affinity preserved throughout
- No partition gaps during update

### Partition Refresh

Trigger partition discovery refresh when partitions change:

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()

if err := mgr.RefreshPartitions(ctx); err != nil {
    log.Printf("Failed to refresh partitions: %v", err)
}
```

**When to Use**:
- Adding new partitions (topics, shards)
- Removing retired partitions
- Updating partition weights
- Dynamic partition reconfiguration

---

## Troubleshooting

### Problem: Worker fails to start

**Symptoms**:
- `Start()` returns error
- Worker stuck in `ClaimingID` or `Election` state

**Common Causes**:
1. NATS connection issues
2. All worker IDs claimed
3. Invalid configuration
4. Timeout during startup

**Diagnostic Steps**:
```bash
# Check NATS connectivity
nats-cli server info nats://localhost:4222

# Check worker logs
kubectl logs -f parti-worker-xyz

# Check worker ID claims
nats-cli kv get parti-stable-ids
```

**Solutions**:
```go
// Increase WorkerIDMax if IDs exhausted
cfg.WorkerIDMax = 199

// Increase startup timeout
cfg.StartupTimeout = 60 * time.Second

// Verify NATS connection
if !nc.IsConnected() {
    log.Fatal("NATS not connected")
}
```

### Problem: Frequent rebalancing

**Symptoms**:
- `OnAssignmentChanged` called frequently
- State transitions between Stable → Scaling → Rebalancing

**Common Causes**:
1. Workers restarting frequently
2. Stabilization windows too short
3. Rebalance threshold too low

**Diagnostic Steps**:
```bash
# Check worker uptime
kubectl get pods -l app=parti-worker

# Check state transition metrics
curl localhost:9090/metrics | grep parti_state_transitions
```

**Solutions**:
```yaml
# Increase stabilization windows
plannedScaleWindow: "20s"

# Increase rebalance interval
assignment:
  minRebalanceInterval: "15s"

# Increase threshold
assignment:
  minRebalanceThreshold: 0.20
```

### Problem: Slow assignment updates

**Symptoms**:
- Long delay between topology change and new assignments
- Workers stuck in `WaitingAssignment` state

**Common Causes**:
1. High partition count (10,000+)
2. Slow partition source
3. Network latency to NATS

**Diagnostic Steps**:
```bash
# Check assignment calculation time (if metrics enabled)
curl localhost:9090/metrics | grep parti_assignment_calculation

# Check NATS latency
nats-cli rtt nats://localhost:4222

# Check partition count
curl localhost:8080/status | jq '.partitionCount'
```

**Solutions**:
```go
// Optimize partition source caching
type CachedPartitionSource struct {
    cache []parti.Partition
    ttl   time.Time
}
```

### Problem: Workers stuck in degraded mode

**NEW**: Workers remain in `StateDegraded` despite NATS connectivity.

**Symptoms**:
- `parti_degraded_mode` metric shows 1.0
- `parti_cache_age_seconds` increasing
- Workers not receiving new assignments
- Degraded alerts continuously firing

**Common Causes**:
1. NATS connectivity intermittent or degraded
2. KV bucket operations failing
3. Network issues between workers and NATS
4. NATS cluster unhealthy
5. Exit threshold not met (flapping connectivity)

**Diagnostic Steps**:

```bash
# Check NATS server health
nats-cli server info

# Check JetStream status
nats-cli stream report

# Check KV bucket health
nats-cli kv status parti-stable-ids
nats-cli kv status parti-heartbeats
nats-cli kv status parti-assignments

# Check degraded mode metrics
curl localhost:9090/api/v1/query?query=parti_degraded_mode
curl localhost:9090/api/v1/query?query=parti_cache_age_seconds

# Check worker logs for NATS errors
kubectl logs -f parti-worker-xyz | grep -i "nats\|degraded"

# Check network connectivity to NATS
kubectl exec -it parti-worker-xyz -- nc -zv nats-server 4222
```

**Solutions**:

```go
// 1. Adjust degraded behavior thresholds
cfg.DegradedBehavior = parti.DegradedBehaviorConfig{
    EnterThreshold:      10 * time.Second,  // Enter degraded slower
    ExitThreshold:       5 * time.Second,   // Exit degraded faster
    KVErrorThreshold:    5,                 // Require more errors
    KVErrorWindow:       30 * time.Second,  // Longer error window
    RecoveryGracePeriod: 30 * time.Second,
}

// 2. Implement connection monitoring hook
hooks := &parti.Hooks{
    OnStateChanged: func(ctx context.Context, from, to parti.State) error {
        if to == parti.StateDegraded {
            log.Error("Entered degraded mode - investigating NATS health")
            // Trigger NATS health check
            checkNATSConnectivity()
        }
        if from == parti.StateDegraded && to == parti.StateStable {
            log.Info("Recovered from degraded mode")
        }
        return nil
    },
}
```

**Immediate Actions**:

```bash
# 1. Restart NATS server if unhealthy
kubectl rollout restart statefulset/nats

# 2. Check NATS resource limits
kubectl describe pod nats-0 | grep -A5 Limits

# 3. Verify JetStream resources
nats-cli stream report

# 4. If NATS healthy but workers degraded, restart workers
kubectl rollout restart deployment/parti-worker
```

**Prevention**:

- Monitor NATS server resource usage (CPU, memory, disk)
- Set up NATS cluster monitoring and alerts
- Configure NATS connection pooling and retry logic
- Implement circuit breakers for KV operations
- Use longer `ExitThreshold` to prevent flapping

### Problem: Degraded mode alerts flooding

**NEW**: Receiving excessive degraded mode alerts.

**Symptoms**:
- `parti_degraded_alerts_total` increasing rapidly
- Alert fatigue from repeated notifications
- Logs flooded with degraded alert messages

**Common Causes**:
1. Alert interval too short
2. Flapping between degraded and normal
3. Persistent NATS issues not resolved
4. Alert thresholds misconfigured

**Diagnostic Steps**:

```bash
# Check alert rate
curl localhost:9090/api/v1/query?query=rate(parti_degraded_alerts_total[5m])

# Check for state flapping
kubectl logs parti-worker-xyz | grep "Degraded\|Stable" | tail -20

# Check current alert configuration
kubectl exec -it parti-worker-xyz -- env | grep DEGRADED
```

**Solutions**:

```go
// 1. Increase alert interval
cfg.DegradedAlert = parti.DegradedAlertConfig{
    InfoThreshold:     1 * time.Minute,
    WarnThreshold:     5 * time.Minute,
    ErrorThreshold:    15 * time.Minute,
    CriticalThreshold: 30 * time.Minute,
    AlertInterval:     5 * time.Minute,  // ← Increase this
}

// 2. Use conservative preset
cfg.DegradedAlert = parti.DegradedAlertPreset("conservative")

// 3. Implement alert deduplication in hook
var lastAlertTime time.Time
var lastAlertLevel string

hooks := &parti.Hooks{
    OnDegradedAlert: func(ctx context.Context, level string, duration time.Duration) error {
        now := time.Now()

        // Deduplicate: only alert if level changed or 5+ minutes passed
        if level != lastAlertLevel || now.Sub(lastAlertTime) > 5*time.Minute {
            sendAlert(level, duration)
            lastAlertTime = now
            lastAlertLevel = level
        }
        return nil
    },
}
```

**Best Practices**:

- Use `conservative` preset in production
- Tune alert thresholds based on your SLA
- Implement alert routing (Info → logs, Critical → PagerDuty)
- Monitor alert frequency and adjust thresholds

### Problem: Worker memory leak

**Symptoms**:
- Memory usage increases over time
- OOMKilled in Kubernetes

**Common Causes**:
1. Partition data not cleaned up
2. Subscription leaks in hooks
3. Unbounded metrics collection

**Diagnostic Steps**:
```bash
# Check memory usage
kubectl top pods -l app=parti-worker

# Profile memory
curl localhost:6060/debug/pprof/heap > heap.prof
go tool pprof heap.prof
```

**Solutions**:
```go
// Clean up subscriptions properly
OnAssignmentChanged: func(ctx context.Context, added, removed []parti.Partition) error {
    // ALWAYS unsubscribe removed first
    for _, p := range removed {
        if sub, ok := subscriptions[p.Keys[0]]; ok {
            sub.Unsubscribe()  // Critical
            delete(subscriptions, p.Keys[0])
        }
    }
    // Then subscribe to added
    for _, p := range added {
        // ...
    }
    return nil
}
```

---

## Next Steps

This operations guide provides the foundation for running Parti in production. For comprehensive operational coverage, see:

- **Full Deployment Guide** (Priority 3): Detailed deployment patterns, security, HA setup
- **Monitoring Guide** (Priority 3): Complete metrics, Grafana dashboards, alerts
- **Runbook** (Priority 3): Incident response procedures, emergency operations
- **Performance Tuning** (Priority 3): Advanced tuning, optimization techniques

For development and API details:
- **User Guide**: [USER_GUIDE.md](USER_GUIDE.md) - Getting started and core concepts
- **API Reference**: [API_REFERENCE.md](API_REFERENCE.md) - Complete API documentation
