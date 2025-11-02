# NATS Cluster Configuration Reference

This document provides detailed NATS configuration for the permanent dual cluster (Blue/Green) architecture in the tFDC deployment strategy.

## Table of Contents

- [Cluster Architecture](#cluster-architecture)
- [Blue Cluster Configuration](#blue-cluster-configuration)
- [Green Cluster Configuration](#green-cluster-configuration)
- [Stream Configuration](#stream-configuration)
- [KV Bucket Configuration](#kv-bucket-configuration)
- [Monitoring and Observability](#monitoring-and-observability)
- [Operational Procedures](#operational-procedures)

## Cluster Architecture

### Permanent Dual Cluster Model

```
┌────────────────────────────────────────────────────────────┐
│                     NATS Blue Cluster                       │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐    │
│  │  nats-blue-0 │  │  nats-blue-1 │  │  nats-blue-2 │    │
│  └──────────────┘  └──────────────┘  └──────────────┘    │
│         │                  │                  │            │
│         └──────────────────┴──────────────────┘            │
│                    Cluster Mesh                            │
│                                                             │
│  Streams: metrics.v1, alerts.v1, events.v1                │
│  KV Buckets: assignments_v1, heartbeats, workers_v1       │
└────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────┐
│                    NATS Green Cluster                       │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐    │
│  │ nats-green-0 │  │ nats-green-1 │  │ nats-green-2 │    │
│  └──────────────┘  └──────────────┘  └──────────────┘    │
│         │                  │                  │            │
│         └──────────────────┴──────────────────┘            │
│                    Cluster Mesh                            │
│                                                             │
│  Streams: metrics.v1, alerts.v1, events.v1                │
│  KV Buckets: assignments_v1, heartbeats, workers_v1       │
└────────────────────────────────────────────────────────────┘
```

### Resource Allocation

| Resource | Blue Cluster | Green Cluster |
|----------|-------------|---------------|
| Nodes | 3 | 3 |
| CPU (per node) | 2 cores | 2 cores |
| Memory (per node) | 4GB | 4GB |
| Storage (per node) | 100GB SSD | 100GB SSD |
| Total Storage | 300GB | 300GB |
| Retention | 7 days | 7 days |

## Blue Cluster Configuration

### StatefulSet Definition

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: nats-blue
  namespace: tfdc
  labels:
    app: nats
    cluster: blue
spec:
  serviceName: nats-blue
  replicas: 3
  selector:
    matchLabels:
      app: nats
      cluster: blue
  template:
    metadata:
      labels:
        app: nats
        cluster: blue
    spec:
      terminationGracePeriodSeconds: 60
      containers:
      - name: nats
        image: nats:2.10.22
        ports:
        - name: client
          containerPort: 4222
        - name: cluster
          containerPort: 6222
        - name: monitor
          containerPort: 8222
        - name: metrics
          containerPort: 7777
        args:
        - -c
        - /etc/nats-config/nats.conf
        - --http_port
        - "8222"
        volumeMounts:
        - name: config-volume
          mountPath: /etc/nats-config
        - name: data
          mountPath: /data
        resources:
          requests:
            cpu: 1000m
            memory: 2Gi
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
      - name: config-volume
        configMap:
          name: nats-blue-config
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: ["ReadWriteOnce"]
      storageClassName: fast-ssd
      resources:
        requests:
          storage: 100Gi
```

### Configuration File

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: nats-blue-config
  namespace: tfdc
data:
  nats.conf: |
    # Server name (unique per pod)
    server_name: $POD_NAME

    # Client port
    port: 4222

    # HTTP monitoring port
    http_port: 8222

    # Cluster configuration
    cluster {
      name: blue
      port: 6222

      # Cluster routes for mesh topology
      routes = [
        nats://nats-blue-0.nats-blue.tfdc.svc.cluster.local:6222
        nats://nats-blue-1.nats-blue.tfdc.svc.cluster.local:6222
        nats://nats-blue-2.nats-blue.tfdc.svc.cluster.local:6222
      ]

      # Cluster authentication (use secrets in production)
      authorization {
        user: cluster_user
        password: $CLUSTER_PASSWORD
      }
    }

    # JetStream configuration
    jetstream {
      store_dir: /data/jetstream

      # Maximum memory and storage
      max_memory_store: 2GB
      max_file_store: 80GB

      # Domain for cluster isolation
      domain: blue
    }

    # Logging
    log_file: "/data/nats.log"
    logtime: true
    debug: false
    trace: false

    # Limits
    max_connections: 10000
    max_subscriptions: 10000
    max_payload: 10MB
    max_pending: 256MB

    # Write deadline for slow consumers
    write_deadline: "10s"

    # Client authentication (use accounts in production)
    authorization {
      users = [
        {
          user: "tfdc_client"
          password: $CLIENT_PASSWORD
          permissions {
            publish {
              allow: [">"]
            }
            subscribe {
              allow: [">"]
            }
          }
        }
      ]
    }

    # System account for monitoring
    system_account: $SYS
```

### Services

```yaml
---
# Headless service for StatefulSet
apiVersion: v1
kind: Service
metadata:
  name: nats-blue
  namespace: tfdc
  labels:
    app: nats
    cluster: blue
spec:
  clusterIP: None
  selector:
    app: nats
    cluster: blue
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
---
# Client service for external access
apiVersion: v1
kind: Service
metadata:
  name: nats-blue-client
  namespace: tfdc
  labels:
    app: nats
    cluster: blue
spec:
  type: ClusterIP
  selector:
    app: nats
    cluster: blue
  ports:
  - name: client
    port: 4222
    targetPort: 4222
```

## Green Cluster Configuration

### StatefulSet Definition

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: nats-green
  namespace: tfdc
  labels:
    app: nats
    cluster: green
spec:
  serviceName: nats-green
  replicas: 3
  selector:
    matchLabels:
      app: nats
      cluster: green
  template:
    metadata:
      labels:
        app: nats
        cluster: green
    spec:
      terminationGracePeriodSeconds: 60
      containers:
      - name: nats
        image: nats:2.10.22
        ports:
        - name: client
          containerPort: 4222
        - name: cluster
          containerPort: 6222
        - name: monitor
          containerPort: 8222
        - name: metrics
          containerPort: 7777
        args:
        - -c
        - /etc/nats-config/nats.conf
        - --http_port
        - "8222"
        volumeMounts:
        - name: config-volume
          mountPath: /etc/nats-config
        - name: data
          mountPath: /data
        resources:
          requests:
            cpu: 1000m
            memory: 2Gi
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
      - name: config-volume
        configMap:
          name: nats-green-config
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: ["ReadWriteOnce"]
      storageClassName: fast-ssd
      resources:
        requests:
          storage: 100Gi
```

### Configuration File

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: nats-green-config
  namespace: tfdc
data:
  nats.conf: |
    # Server name (unique per pod)
    server_name: $POD_NAME

    # Client port
    port: 4222

    # HTTP monitoring port
    http_port: 8222

    # Cluster configuration
    cluster {
      name: green
      port: 6222

      # Cluster routes for mesh topology
      routes = [
        nats://nats-green-0.nats-green.tfdc.svc.cluster.local:6222
        nats://nats-green-1.nats-green.tfdc.svc.cluster.local:6222
        nats://nats-green-2.nats-green.tfdc.svc.cluster.local:6222
      ]

      # Cluster authentication (use secrets in production)
      authorization {
        user: cluster_user
        password: $CLUSTER_PASSWORD
      }
    }

    # JetStream configuration
    jetstream {
      store_dir: /data/jetstream

      # Maximum memory and storage
      max_memory_store: 2GB
      max_file_store: 80GB

      # Domain for cluster isolation
      domain: green
    }

    # Logging
    log_file: "/data/nats.log"
    logtime: true
    debug: false
    trace: false

    # Limits
    max_connections: 10000
    max_subscriptions: 10000
    max_payload: 10MB
    max_pending: 256MB

    # Write deadline for slow consumers
    write_deadline: "10s"

    # Client authentication (use accounts in production)
    authorization {
      users = [
        {
          user: "tfdc_client"
          password: $CLIENT_PASSWORD
          permissions {
            publish {
              allow: [">"]
            }
            subscribe {
              allow: [">"]
            }
          }
        }
      ]
    }

    # System account for monitoring
    system_account: $SYS
```

### Services

```yaml
---
# Headless service for StatefulSet
apiVersion: v1
kind: Service
metadata:
  name: nats-green
  namespace: tfdc
  labels:
    app: nats
    cluster: green
spec:
  clusterIP: None
  selector:
    app: nats
    cluster: green
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
---
# Client service for external access
apiVersion: v1
kind: Service
metadata:
  name: nats-green-client
  namespace: tfdc
  labels:
    app: nats
    cluster: green
spec:
  type: ClusterIP
  selector:
    app: nats
    cluster: green
  ports:
  - name: client
    port: 4222
    targetPort: 4222
```

## Stream Configuration

### Metrics Stream

```yaml
# Create stream using NATS CLI
nats stream add metrics.v1 \
  --subjects "metrics.>" \
  --storage file \
  --retention limits \
  --max-age 7d \
  --max-msgs-per-subject 100000 \
  --max-bytes 50GB \
  --replicas 3 \
  --discard old \
  --dupe-window 2m
```

**Go Code Example:**
```go
package main

import (
    "context"
    "time"

    "github.com/nats-io/nats.go"
    "github.com/nats-io/nats.go/jetstream"
)

func createMetricsStream(nc *nats.Conn) error {
    js, err := jetstream.New(nc)
    if err != nil {
        return err
    }

    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    _, err = js.CreateStream(ctx, jetstream.StreamConfig{
        Name:        "metrics.v1",
        Subjects:    []string{"metrics.>"},
        Storage:     jetstream.FileStorage,
        Retention:   jetstream.LimitsPolicy,
        MaxAge:      7 * 24 * time.Hour,
        MaxMsgsPer:  100000,
        MaxBytes:    50 * 1024 * 1024 * 1024, // 50GB
        Replicas:    3,
        Discard:     jetstream.DiscardOld,
        Duplicates:  2 * time.Minute,
    })

    return err
}
```

### Alerts Stream

```yaml
# Create stream using NATS CLI
nats stream add alerts.v1 \
  --subjects "alerts.>" \
  --storage file \
  --retention limits \
  --max-age 30d \
  --max-msgs 1000000 \
  --max-bytes 10GB \
  --replicas 3 \
  --discard old \
  --dupe-window 5m
```

**Go Code Example:**
```go
func createAlertsStream(nc *nats.Conn) error {
    js, err := jetstream.New(nc)
    if err != nil {
        return err
    }

    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    _, err = js.CreateStream(ctx, jetstream.StreamConfig{
        Name:        "alerts.v1",
        Subjects:    []string{"alerts.>"},
        Storage:     jetstream.FileStorage,
        Retention:   jetstream.LimitsPolicy,
        MaxAge:      30 * 24 * time.Hour,
        MaxMsgs:     1000000,
        MaxBytes:    10 * 1024 * 1024 * 1024, // 10GB
        Replicas:    3,
        Discard:     jetstream.DiscardOld,
        Duplicates:  5 * time.Minute,
    })

    return err
}
```

### Events Stream

```yaml
# Create stream using NATS CLI
nats stream add events.v1 \
  --subjects "events.>" \
  --storage file \
  --retention limits \
  --max-age 14d \
  --max-msgs 5000000 \
  --max-bytes 100GB \
  --replicas 3 \
  --discard old \
  --dupe-window 1m
```

**Go Code Example:**
```go
func createEventsStream(nc *nats.Conn) error {
    js, err := jetstream.New(nc)
    if err != nil {
        return err
    }

    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    _, err = js.CreateStream(ctx, jetstream.StreamConfig{
        Name:        "events.v1",
        Subjects:    []string{"events.>"},
        Storage:     jetstream.FileStorage,
        Retention:   jetstream.LimitsPolicy,
        MaxAge:      14 * 24 * time.Hour,
        MaxMsgs:     5000000,
        MaxBytes:    100 * 1024 * 1024 * 1024, // 100GB
        Replicas:    3,
        Discard:     jetstream.DiscardOld,
        Duplicates:  1 * time.Minute,
    })

    return err
}
```

## KV Bucket Configuration

### Assignments Bucket

```yaml
# Create KV bucket using NATS CLI
nats kv add assignments_v1 \
  --history 10 \
  --ttl 0 \
  --replicas 3 \
  --max-value-size 1MB
```

**Go Code Example:**
```go
func createAssignmentsKV(nc *nats.Conn) error {
    js, err := jetstream.New(nc)
    if err != nil {
        return err
    }

    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    _, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
        Bucket:       "assignments_v1",
        History:      10,
        TTL:          0, // No expiration
        Replicas:     3,
        MaxValueSize: 1024 * 1024, // 1MB
    })

    return err
}
```

### Heartbeats Bucket

```yaml
# Create KV bucket using NATS CLI
nats kv add heartbeats \
  --history 5 \
  --ttl 60s \
  --replicas 3 \
  --max-value-size 10KB
```

**Go Code Example:**
```go
func createHeartbeatsKV(nc *nats.Conn) error {
    js, err := jetstream.New(nc)
    if err != nil {
        return err
    }

    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    _, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
        Bucket:       "heartbeats",
        History:      5,
        TTL:          60 * time.Second,
        Replicas:     3,
        MaxValueSize: 10 * 1024, // 10KB
    })

    return err
}
```

### Workers Bucket

```yaml
# Create KV bucket using NATS CLI
nats kv add workers_v1 \
  --history 20 \
  --ttl 0 \
  --replicas 3 \
  --max-value-size 100KB
```

**Go Code Example:**
```go
func createWorkersKV(nc *nats.Conn) error {
    js, err := jetstream.New(nc)
    if err != nil {
        return err
    }

    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    _, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
        Bucket:       "workers_v1",
        History:      20,
        TTL:          0, // No expiration
        Replicas:     3,
        MaxValueSize: 100 * 1024, // 100KB
    })

    return err
}
```

## Monitoring and Observability

### Prometheus Metrics Configuration

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: prometheus-nats-exporter
  namespace: monitoring
data:
  prometheus.yml: |
    scrape_configs:
    - job_name: 'nats-blue'
      static_configs:
      - targets:
        - nats-blue-0.nats-blue.tfdc.svc.cluster.local:8222
        - nats-blue-1.nats-blue.tfdc.svc.cluster.local:8222
        - nats-blue-2.nats-blue.tfdc.svc.cluster.local:8222
      metrics_path: /metrics

    - job_name: 'nats-green'
      static_configs:
      - targets:
        - nats-green-0.nats-green.tfdc.svc.cluster.local:8222
        - nats-green-1.nats-green.tfdc.svc.cluster.local:8222
        - nats-green-2.nats-green.tfdc.svc.cluster.local:8222
      metrics_path: /metrics
```

### Key Metrics to Monitor

```
# Connection metrics
nats_core_total_connections{cluster="blue"} 150
nats_core_connection_count{cluster="blue"} 150

# Message metrics
nats_core_msg_in_count{cluster="blue"} 1234567
nats_core_msg_out_count{cluster="blue"} 1234567
nats_core_msg_bytes_in{cluster="blue"} 9876543210
nats_core_msg_bytes_out{cluster="blue"} 9876543210

# JetStream metrics
nats_jetstream_streams{cluster="blue"} 3
nats_jetstream_consumers{cluster="blue"} 15
nats_jetstream_storage_bytes{cluster="blue",stream="metrics.v1"} 45000000000

# Cluster metrics
nats_core_cluster_size{cluster="blue"} 3
nats_core_routes{cluster="blue"} 2
```

### Grafana Dashboard

```json
{
  "dashboard": {
    "title": "NATS - Blue/Green Clusters",
    "panels": [
      {
        "title": "Message Rate by Cluster",
        "targets": [
          {
            "expr": "rate(nats_core_msg_in_count[5m])"
          }
        ]
      },
      {
        "title": "Storage Usage by Stream",
        "targets": [
          {
            "expr": "nats_jetstream_storage_bytes"
          }
        ]
      },
      {
        "title": "Cluster Health",
        "targets": [
          {
            "expr": "nats_core_cluster_size == 3"
          }
        ]
      },
      {
        "title": "Consumer Lag",
        "targets": [
          {
            "expr": "nats_jetstream_consumer_num_pending"
          }
        ]
      }
    ]
  }
}
```

## Operational Procedures

### Cluster Initialization

```bash
#!/bin/bash
# Initialize both Blue and Green NATS clusters

set -e

echo "Initializing Blue NATS cluster..."
kubectl apply -f nats-blue-statefulset.yaml
kubectl apply -f nats-blue-service.yaml

# Wait for Blue cluster to be ready
kubectl wait --for=condition=ready pod -l cluster=blue -n tfdc --timeout=5m

echo "Creating Blue cluster streams and KV buckets..."
NATS_URL="nats://nats-blue-client.tfdc.svc.cluster.local:4222"

nats stream add metrics.v1 --server=$NATS_URL \
  --subjects "metrics.>" --storage file --retention limits \
  --max-age 7d --max-bytes 50GB --replicas 3

nats stream add alerts.v1 --server=$NATS_URL \
  --subjects "alerts.>" --storage file --retention limits \
  --max-age 30d --max-bytes 10GB --replicas 3

nats stream add events.v1 --server=$NATS_URL \
  --subjects "events.>" --storage file --retention limits \
  --max-age 14d --max-bytes 100GB --replicas 3

nats kv add assignments_v1 --server=$NATS_URL \
  --history 10 --ttl 0 --replicas 3

nats kv add heartbeats --server=$NATS_URL \
  --history 5 --ttl 60s --replicas 3

nats kv add workers_v1 --server=$NATS_URL \
  --history 20 --ttl 0 --replicas 3

echo "Blue cluster initialized successfully!"

echo "Initializing Green NATS cluster..."
kubectl apply -f nats-green-statefulset.yaml
kubectl apply -f nats-green-service.yaml

# Wait for Green cluster to be ready
kubectl wait --for=condition=ready pod -l cluster=green -n tfdc --timeout=5m

echo "Creating Green cluster streams and KV buckets..."
NATS_URL="nats://nats-green-client.tfdc.svc.cluster.local:4222"

nats stream add metrics.v1 --server=$NATS_URL \
  --subjects "metrics.>" --storage file --retention limits \
  --max-age 7d --max-bytes 50GB --replicas 3

nats stream add alerts.v1 --server=$NATS_URL \
  --subjects "alerts.>" --storage file --retention limits \
  --max-age 30d --max-bytes 10GB --replicas 3

nats stream add events.v1 --server=$NATS_URL \
  --subjects "events.>" --storage file --retention limits \
  --max-age 14d --max-bytes 100GB --replicas 3

nats kv add assignments_v1 --server=$NATS_URL \
  --history 10 --ttl 0 --replicas 3

nats kv add heartbeats --server=$NATS_URL \
  --history 5 --ttl 60s --replicas 3

nats kv add workers_v1 --server=$NATS_URL \
  --history 20 --ttl 0 --replicas 3

echo "Green cluster initialized successfully!"
echo "Both NATS clusters are ready for deployment!"
```

### Cluster Health Check

```bash
#!/bin/bash
# Check health of both NATS clusters

function check_cluster() {
    local cluster=$1
    local url="nats://${cluster}-client.tfdc.svc.cluster.local:4222"

    echo "Checking $cluster cluster..."

    # Check connectivity
    if ! nats server ping --server=$url; then
        echo "✗ $cluster cluster is not responding"
        return 1
    fi
    echo "✓ $cluster cluster is responding"

    # Check cluster size
    local size=$(nats server list --server=$url | grep -c "nats-$cluster")
    if [ "$size" -ne 3 ]; then
        echo "✗ $cluster cluster size is $size (expected 3)"
        return 1
    fi
    echo "✓ $cluster cluster has 3 nodes"

    # Check streams
    local streams=$(nats stream list --server=$url | grep -c "metrics.v1\|alerts.v1\|events.v1")
    if [ "$streams" -ne 3 ]; then
        echo "✗ $cluster cluster has $streams streams (expected 3)"
        return 1
    fi
    echo "✓ $cluster cluster has all 3 streams"

    # Check KV buckets
    local kvs=$(nats kv list --server=$url | grep -c "assignments_v1\|heartbeats\|workers_v1")
    if [ "$kvs" -ne 3 ]; then
        echo "✗ $cluster cluster has $kvs KV buckets (expected 3)"
        return 1
    fi
    echo "✓ $cluster cluster has all 3 KV buckets"

    echo "$cluster cluster is healthy!"
    return 0
}

check_cluster "nats-blue"
blue_status=$?

check_cluster "nats-green"
green_status=$?

if [ $blue_status -eq 0 ] && [ $green_status -eq 0 ]; then
    echo "✓ Both clusters are healthy!"
    exit 0
else
    echo "✗ One or more clusters are unhealthy"
    exit 1
fi
```

### Backup Procedure

```bash
#!/bin/bash
# Backup NATS cluster data

CLUSTER=${1:-blue}
BACKUP_DIR="/backup/nats-${CLUSTER}-$(date +%Y%m%d-%H%M%S)"
NATS_URL="nats://nats-${CLUSTER}-client.tfdc.svc.cluster.local:4222"

mkdir -p "$BACKUP_DIR"

echo "Backing up $CLUSTER cluster to $BACKUP_DIR..."

# Backup streams
for stream in metrics.v1 alerts.v1 events.v1; do
    echo "Backing up stream: $stream"
    nats stream backup --server=$NATS_URL $stream "$BACKUP_DIR/$stream"
done

# Backup KV buckets (extract from underlying streams)
for kv in assignments_v1 heartbeats workers_v1; do
    echo "Backing up KV bucket: $kv"
    # KV buckets are backed up via their underlying stream
    nats stream backup --server=$NATS_URL "KV_$kv" "$BACKUP_DIR/KV_$kv"
done

echo "Backup complete: $BACKUP_DIR"
```

### Restore Procedure

```bash
#!/bin/bash
# Restore NATS cluster from backup

CLUSTER=${1:-blue}
BACKUP_DIR=$2
NATS_URL="nats://nats-${CLUSTER}-client.tfdc.svc.cluster.local:4222"

if [ -z "$BACKUP_DIR" ]; then
    echo "Usage: $0 <cluster> <backup_dir>"
    exit 1
fi

echo "Restoring $CLUSTER cluster from $BACKUP_DIR..."

# Restore streams
for stream in metrics.v1 alerts.v1 events.v1; do
    echo "Restoring stream: $stream"
    nats stream restore --server=$NATS_URL $stream "$BACKUP_DIR/$stream"
done

# Restore KV buckets
for kv in assignments_v1 heartbeats workers_v1; do
    echo "Restoring KV bucket: $kv"
    nats stream restore --server=$NATS_URL "KV_$kv" "$BACKUP_DIR/KV_$kv"
done

echo "Restore complete!"
```

### Cluster Maintenance

```bash
#!/bin/bash
# Perform rolling maintenance on NATS cluster

CLUSTER=${1:-blue}

echo "Starting rolling maintenance for $CLUSTER cluster..."

for i in 0 1 2; do
    POD="nats-${CLUSTER}-$i"

    echo "Draining $POD..."
    kubectl exec -n tfdc $POD -- nats-server --signal ldm

    echo "Waiting for connections to drain..."
    sleep 30

    echo "Restarting $POD..."
    kubectl delete pod -n tfdc $POD

    echo "Waiting for $POD to be ready..."
    kubectl wait --for=condition=ready pod/$POD -n tfdc --timeout=5m

    echo "$POD maintenance complete"
    sleep 10
done

echo "Cluster maintenance complete!"
```

## Troubleshooting

### Common Issues

1. **Cluster split-brain**
   ```bash
   # Check cluster connectivity
   nats server list --server=nats://nats-blue-client.tfdc.svc.cluster.local:4222

   # Verify all 3 nodes are connected
   # If not, check network policies and pod logs
   kubectl logs -n tfdc nats-blue-0
   ```

2. **Storage exhaustion**
   ```bash
   # Check storage usage
   kubectl exec -n tfdc nats-blue-0 -- df -h /data

   # Check stream storage
   nats stream info metrics.v1 --server=nats://nats-blue-client.tfdc.svc.cluster.local:4222

   # Purge old messages if needed
   nats stream purge metrics.v1 --keep 1000000 --server=nats://nats-blue-client.tfdc.svc.cluster.local:4222
   ```

3. **Consumer lag**
   ```bash
   # Check consumer lag
   nats consumer info metrics.v1 defender-consumer --server=nats://nats-blue-client.tfdc.svc.cluster.local:4222

   # Increase consumer replicas if needed
   kubectl scale deployment defender-blue --replicas=5
   ```

## References

- [NATS Documentation](https://docs.nats.io/)
- [JetStream Guide](https://docs.nats.io/nats-concepts/jetstream)
- [NATS Clustering](https://docs.nats.io/running-a-nats-service/configuration/clustering)
- [NATS Kubernetes](https://docs.nats.io/running-a-nats-service/nats-kubernetes)
