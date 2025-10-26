# Kubernetes Workload Comparison: StatefulSet vs Deployment vs Hybrid Approach

## Overview

This document provides a detailed comparison of three approaches for deploying the tFDC defender cluster in Kubernetes, analyzing their trade-offs for our specific use case of distributed stateless processing with stable identities.

## The Three Approaches

### Approach 1: StatefulSet

**Description**: Use Kubernetes StatefulSet with pod identity (defender-0, defender-1, ..., defender-29)

**Identity Mechanism**: Kubernetes-provided stable network identities and persistent pod names

### Approach 2: Pure Deployment

**Description**: Use standard Kubernetes Deployment with random pod names

**Identity Mechanism**: No stable identities, pods are ephemeral and interchangeable

### Approach 3: Hybrid (Our Choice)

**Description**: Use Kubernetes Deployment with self-assigned stable IDs via NATS KV

**Identity Mechanism**: Application-level stable IDs claimed from external key-value store

## Detailed Comparison

### 1. Identity Management

| Aspect | StatefulSet | Pure Deployment | Hybrid Approach |
|--------|-------------|-----------------|-----------------|
| **Pod Names** | Stable (`defender-0`) | Random (`defender-7f8d9c-xyzab`) | Random (`defender-7f8d9c-xyzab`) |
| **Network Identity** | Stable DNS (`defender-0.defender.svc`) | Load-balanced service | Load-balanced service |
| **Application Identity** | Derived from pod name | None | Self-assigned (`defender-0`) |
| **Identity Source** | Kubernetes | N/A | NATS KV (external) |
| **Identity Persistence** | Pod lifecycle | None | TTL-based (30s) |
| **Identity Reuse** | Only after pod deletion | N/A | Immediate (after TTL) |

**Winner**: **Hybrid** - Decouples identity from pod lifecycle, enabling faster recovery

---

### 2. Deployment Strategies

#### Rolling Update

| Aspect | StatefulSet | Pure Deployment | Hybrid Approach |
|--------|-------------|-----------------|-----------------|
| **Update Strategy** | Sequential (0→1→2...) | Parallel batches | Parallel batches |
| **maxSurge Support** | ❌ No | ✅ Yes | ✅ Yes |
| **Update Order** | Fixed (0 to N-1) | Random | Random |
| **Chamber Reassignment** | ⚠️ Yes (new pods = new IDs) | ⚠️ Yes (random pods) | ✅ **No** (stable IDs) |
| **Update Duration** | 60-90s (sequential) | 30-45s (parallel) | 30-45s (parallel) |
| **Zero-Downtime** | ⚠️ Partial | ✅ Yes (with maxSurge) | ✅ Yes (with maxSurge) |

**Example - Rolling Update (30 defenders)**:

```yaml
# StatefulSet - Sequential updates
apiVersion: apps/v1
kind: StatefulSet
spec:
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      partition: 0  # Update all pods
      maxUnavailable: 1  # Only 1 pod at a time
  # maxSurge NOT supported
  # Update order: defender-0 → defender-1 → ... → defender-29
  # Duration: ~90 seconds (3s × 30 pods)
  # Chamber reassignment: YES (if using surge workaround)
```

```yaml
# Hybrid Approach - Parallel batches with surge
apiVersion: apps/v1
kind: Deployment
spec:
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 10        # Create 10 extra pods
      maxUnavailable: 0   # Zero unavailability
  # Update order: Random batches of 10
  # Duration: ~30-40 seconds (3 batches × 10-15s)
  # Chamber reassignment: NO (stable IDs reused)
```

**Winner**: **Hybrid** - Parallel updates with zero chamber reassignment

---

#### Blue-Green Deployment

| Aspect | StatefulSet | Pure Deployment | Hybrid Approach |
|--------|-------------|-----------------|-----------------|
| **Parallel Clusters** | ⚠️ Complex (name conflicts) | ✅ Simple | ✅ Simple |
| **ID Range Planning** | Required (0-29 vs 30-59) | Not needed | Required (0-29 vs 30-59) |
| **Instant Switchover** | ❌ Sequential scale-down | ✅ Yes | ✅ Yes |
| **Resource Efficiency** | ⚠️ Need PVCs for both | ✅ No PVCs | ✅ No PVCs |
| **Rollback Speed** | Slow (recreate pods) | Fast (scale up) | Fast (scale up) |

**Example - Blue-Green with StatefulSet**:

```yaml
# Blue Cluster (StatefulSet - Problem: Name conflicts)
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: defender-blue
spec:
  serviceName: defender-blue
  replicas: 30
  # Pods: defender-blue-0, defender-blue-1, ..., defender-blue-29
  # Problem: Different pod names = different identities
  # Cannot reuse stable IDs without complex naming scheme
```

```yaml
# Blue Cluster (Hybrid - Clean separation)
apiVersion: apps/v1
kind: Deployment
metadata:
  name: defender-blue
spec:
  replicas: 30
  # Pods: defender-blue-xxx, defender-blue-yyy (random names)
  # Stable IDs: defender-0 to defender-29 (from NATS KV)
  # Clean separation, instant switchover
```

**Winner**: **Hybrid** - Clean blue-green separation with stable IDs

---

#### Canary Deployment

| Aspect | StatefulSet | Pure Deployment | Hybrid Approach |
|--------|-------------|-----------------|-----------------|
| **Gradual Rollout** | ⚠️ Complex (partition) | ✅ Simple (2 deployments) | ✅ Simple (2 deployments) |
| **Traffic Control** | Limited | ✅ Fine-grained | ✅ Fine-grained |
| **Chamber Distribution** | Fixed (based on pod index) | Random | Random |
| **Rollback** | Slow | Fast | Fast |

**Example - 10% Canary**:

```yaml
# StatefulSet - Using partition
apiVersion: apps/v1
kind: StatefulSet
spec:
  replicas: 30
  updateStrategy:
    rollingUpdate:
      partition: 27  # Update only pods 27-29 (last 3 pods, ~10%)
  # Problem: Canary pods are fixed (always last pods)
  # Cannot control which chambers go to canary
```

```yaml
# Hybrid - Two deployments
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: defender-stable
spec:
  replicas: 27  # 90% stable version
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: defender-canary
spec:
  replicas: 3   # 10% canary version
  # Canary pods claim IDs: defender-30, defender-31, defender-32
  # Chamber distribution via consistent hashing (random subset)
```

**Winner**: **Hybrid** - Flexible canary deployment with clean separation

---

### 3. Scaling Behavior

#### Scale-Up

| Aspect | StatefulSet | Pure Deployment | Hybrid Approach |
|--------|-------------|-----------------|-----------------|
| **Pod Creation** | Sequential (0→1→2...) | Parallel | Parallel |
| **Scale-Up Duration** | Slow (sequential) | Fast (parallel) | Fast (parallel) |
| **ID Assignment** | Automatic (pod index) | Random | Self-assigned (sequential) |
| **Chamber Rebalancing** | Required | Required | Required |

**Example - Scale 30 → 45**:

```
StatefulSet:
  T+0s:  Create defender-30
  T+3s:  Create defender-31 (after 30 is ready)
  T+6s:  Create defender-32 (after 31 is ready)
  ...
  T+45s: All 15 pods created (sequential)
  Duration: ~45-60 seconds

Hybrid:
  T+0s:  Create all 15 pods in parallel
  T+3s:  All pods starting simultaneously
  T+5s:  All pods claimed IDs (defender-30 to defender-44)
  Duration: ~5-10 seconds (parallel)
```

**Winner**: **Hybrid** - Parallel pod creation

---

#### Scale-Down

| Aspect | StatefulSet | Pure Deployment | Hybrid Approach |
|--------|-------------|-----------------|-----------------|
| **Pod Deletion** | Sequential (reverse order) | Parallel | Parallel |
| **Scale-Down Duration** | Slow (sequential) | Fast (parallel) | Fast (parallel) |
| **Graceful Shutdown** | ✅ Yes | ✅ Yes | ✅ Yes |
| **ID Release** | Automatic | N/A | Explicit (delete KV entry) |

**Example - Scale 30 → 20**:

```
StatefulSet:
  T+0s:  Delete defender-29 (wait for termination)
  T+30s: Delete defender-28 (after 29 terminates)
  T+60s: Delete defender-27 (after 28 terminates)
  ...
  Duration: ~300 seconds (10 pods × 30s each)

Hybrid:
  T+0s:  Delete all 10 pods in parallel (SIGTERM)
  T+0-30s: All pods draining messages
  T+30s: All pods terminated
  Duration: ~30-40 seconds (parallel)
```

**Winner**: **Hybrid** - Parallel pod deletion

---

### 4. Failure Recovery

#### Crash Recovery

| Aspect | StatefulSet | Pure Deployment | Hybrid Approach |
|--------|-------------|-----------------|-----------------|
| **Detection Time** | 30s (liveness probe) | 30s (liveness probe) | 30s (heartbeat TTL) |
| **Pod Recreation** | Automatic (ReplicaSet) | Automatic (ReplicaSet) | Automatic (ReplicaSet) |
| **Identity Recovery** | Same pod name | New random name | Same stable ID |
| **PVC Reattachment** | Automatic | N/A | N/A |
| **Chamber Reassignment** | Not required | Required | Not required |
| **Recovery Duration** | 40-60s (PVC attach + mount) | 10-20s (no PVC) | 10-20s (no PVC) |

**Example - Defender-5 Crashes**:

```
StatefulSet:
  T+0s:  Pod defender-5 crashes
  T+30s: Liveness probe fails, pod deleted
  T+35s: New pod defender-5 created
  T+40s: PVC attached to new pod
  T+50s: Container starts, mounts PVC
  T+60s: Defender-5 back online (same identity, same PVC)
  Chamber reassignment: NO (same pod name = same identity)
  Duration: ~60 seconds

Hybrid:
  T+0s:  Pod defender-abc123 crashes
  T+30s: Heartbeat TTL expires, leader detects crash
  T+33s: Emergency rebalancing (chambers redistributed)
  T+35s: Liveness probe fails, pod deleted
  T+38s: New pod defender-xyz789 created
  T+43s: New pod claims stable ID "defender-5"
  T+45s: Defender-5 back online (same stable ID)
  T+75s: Final rebalancing (restore original distribution)
  Chamber reassignment: YES (emergency) then restored
  Duration: ~45 seconds (full recovery)
```

**Trade-off**:
- **StatefulSet**: Slower recovery (PVC attachment), but no rebalancing
- **Hybrid**: Faster recovery (no PVC), but temporary rebalancing

**Winner**: **Hybrid** - Faster recovery, acceptable rebalancing trade-off

---

### 5. State Management

| Aspect | StatefulSet | Pure Deployment | Hybrid Approach |
|--------|-------------|-----------------|-----------------|
| **Persistent Storage** | ✅ PVC per pod | ❌ No PVCs | ❌ No PVCs |
| **Cache Storage** | Disk-backed (PVC) | In-memory only | In-memory only |
| **Cache Size** | Large (10s of GB) | Small (100 MB) | Small (100 MB) |
| **Cache Persistence** | Survives crashes | Lost on crash | Lost on crash |
| **State Recovery** | Read from PVC | Rebuild from Cassandra | Rebuild from Cassandra |
| **Storage Cost** | High (30 PVCs × 10 GB) | None | None |

**Our Use Case Analysis**:

```
Cache Requirements:
  - Purpose: U-chart history (recent calculations)
  - Size: ~100 MB per defender (small!)
  - Hit Rate: 95-98%
  - Rebuild Time: 5-10 minutes (acceptable)

Conclusion:
  PVCs are overkill for our small, rebuild-able cache.
  In-memory cache is sufficient.
```

**Winner**: **Hybrid** - No PVCs needed, cost-effective

---

### 6. Operational Complexity

| Aspect | StatefulSet | Pure Deployment | Hybrid Approach |
|--------|-------------|-----------------|-----------------|
| **Setup Complexity** | Medium | Low | Medium-High |
| **Manifest Lines** | ~80 lines | ~60 lines | ~60 lines (Deployment) + NATS KV |
| **Dependencies** | Kubernetes only | Kubernetes only | Kubernetes + NATS + application logic |
| **Debugging** | Medium | Easy | Medium |
| **Learning Curve** | Medium | Low | High |
| **Failure Modes** | PVC issues, pod stuck | Pod crashes | ID conflicts, KV unavailable |

**Complexity Breakdown**:

```
StatefulSet:
  ✅ Simple identity model (built-in)
  ❌ Complex scaling (sequential)
  ❌ Complex updates (limited strategies)
  ❌ PVC management overhead

Hybrid:
  ❌ Custom ID claiming logic (application code)
  ❌ External dependency (NATS KV)
  ✅ Simple scaling (parallel)
  ✅ Flexible updates (all strategies)
  ✅ No PVC management
```

**Trade-off**: Higher initial complexity, lower operational complexity

**Winner**: **Tie** - Depends on team expertise

---

### 7. Cost Analysis

#### Resource Costs

| Resource | StatefulSet | Pure Deployment | Hybrid Approach |
|----------|-------------|-----------------|-----------------|
| **Compute (30 pods)** | 240-480 vCores<br>240-480 GB RAM | 240-480 vCores<br>240-480 GB RAM | 240-480 vCores<br>240-480 GB RAM |
| **Storage (PVCs)** | 300 GB (30 × 10 GB) | 0 GB | 0 GB |
| **NATS KV Storage** | N/A | N/A | < 1 GB (minimal) |
| **Network** | Same | Same | Same |

**Monthly Cost Estimate (AWS, us-east-1)**:

```
StatefulSet:
  Compute: $2,500-5,000 (same for all)
  EBS volumes: 30 × 10 GB × $0.10/GB/month = $30/month
  Total: $2,530-5,030/month

Hybrid:
  Compute: $2,500-5,000 (same for all)
  EBS volumes: $0 (no PVCs)
  NATS overhead: Negligible
  Total: $2,500-5,000/month

Savings: $30/month (1% of total cost)
```

**Winner**: **Hybrid** - Slight cost savings, but not significant

---

### 8. Suitability for Different Deployment Patterns

#### Pattern 1: Single Long-Running Cluster

**Scenario**: Deploy once, run for months/years with occasional updates

| Approach | Suitability | Notes |
|----------|-------------|-------|
| **StatefulSet** | ⭐⭐⭐⭐ Good | Stable identities, persistent storage beneficial |
| **Hybrid** | ⭐⭐⭐⭐⭐ Excellent | Flexible updates, no PVC management |

---

#### Pattern 2: Frequent Rolling Updates

**Scenario**: Weekly/daily version updates with zero downtime

| Approach | Suitability | Notes |
|----------|-------------|-------|
| **StatefulSet** | ⭐⭐ Poor | Sequential updates slow, limited strategies |
| **Hybrid** | ⭐⭐⭐⭐⭐ Excellent | **Zero reassignment, fast parallel updates** |

**This is our primary use case!**

---

#### Pattern 3: Blue-Green / Canary Deployments

**Scenario**: Advanced deployment strategies for risk mitigation

| Approach | Suitability | Notes |
|----------|-------------|-------|
| **StatefulSet** | ⭐⭐ Poor | Complex setup, name conflicts, limited flexibility |
| **Hybrid** | ⭐⭐⭐⭐⭐ Excellent | Clean separation, flexible traffic control |

**This is a key requirement!**

---

#### Pattern 4: Elastic Scaling

**Scenario**: Frequent scale-up/down based on load (HPA)

| Approach | Suitability | Notes |
|----------|-------------|-------|
| **StatefulSet** | ⭐⭐⭐ Fair | Sequential scaling slow, but works |
| **Hybrid** | ⭐⭐⭐⭐⭐ Excellent | Fast parallel scaling, dynamic ID claiming |

---

#### Pattern 5: Multi-Cluster / Geo-Distributed

**Scenario**: Multiple clusters in different regions

| Approach | Suitability | Notes |
|----------|-------------|-------|
| **StatefulSet** | ⭐⭐⭐ Fair | Need separate namespaces/clusters for isolation |
| **Hybrid** | ⭐⭐⭐⭐⭐ Excellent | ID ranges per cluster (0-29, 30-59, 60-89, etc.) |

---

## Decision Matrix

### Requirements vs Approaches

| Requirement | Weight | StatefulSet | Pure Deployment | Hybrid Approach |
|-------------|--------|-------------|-----------------|-----------------|
| **Zero-reassignment rolling updates** | 🔴 Critical | ❌ No (1/5) | ❌ No (1/5) | ✅ **Yes (5/5)** |
| **Blue-green/canary support** | 🔴 Critical | ❌ Poor (2/5) | ✅ Good (4/5) | ✅ **Excellent (5/5)** |
| **Fast crash recovery** | 🟡 High | ❌ Slow (2/5) | ✅ Fast (4/5) | ✅ **Fast (5/5)** |
| **Elastic scaling (HPA)** | 🟡 High | ⚠️ Fair (3/5) | ✅ Good (4/5) | ✅ **Excellent (5/5)** |
| **No persistent storage** | 🟢 Medium | ❌ PVCs required (1/5) | ✅ No PVCs (5/5) | ✅ **No PVCs (5/5)** |
| **Operational simplicity** | 🟢 Medium | ⚠️ Medium (3/5) | ✅ Simple (5/5) | ⚠️ **Medium (3/5)** |

### Weighted Scores

```
StatefulSet:
  Zero-reassignment (×5): 1 × 5 = 5
  Blue-green (×5):        2 × 5 = 10
  Crash recovery (×3):    2 × 3 = 6
  Elastic scaling (×3):   3 × 3 = 9
  No PVCs (×2):           1 × 2 = 2
  Simplicity (×2):        3 × 2 = 6
  Total: 38/100

Pure Deployment:
  Zero-reassignment (×5): 1 × 5 = 5
  Blue-green (×5):        4 × 5 = 20
  Crash recovery (×3):    4 × 3 = 12
  Elastic scaling (×3):   4 × 3 = 12
  No PVCs (×2):           5 × 2 = 10
  Simplicity (×2):        5 × 2 = 10
  Total: 69/100

Hybrid Approach:
  Zero-reassignment (×5): 5 × 5 = 25  ⭐ Key differentiator
  Blue-green (×5):        5 × 5 = 25  ⭐ Key differentiator
  Crash recovery (×3):    5 × 3 = 15
  Elastic scaling (×3):   5 × 3 = 15
  No PVCs (×2):           5 × 2 = 10
  Simplicity (×2):        3 × 2 = 6
  Total: 96/100

Winner: Hybrid Approach (96/100)
```

---

## Key Trade-offs

### Hybrid Approach Advantages ✅

1. **Zero-reassignment rolling updates** (unique capability)
2. **Flexible deployment strategies** (rolling, blue-green, canary, progressive)
3. **Fast parallel scaling** (scale-up, scale-down)
4. **No persistent volumes** (cost savings, simpler operations)
5. **Decoupled identity** (survives pod deletion, faster recovery)
6. **Cloud-agnostic** (works on any Kubernetes)

### Hybrid Approach Disadvantages ❌

1. **Higher initial complexity** (custom ID claiming logic)
2. **External dependency** (NATS KV required)
3. **Application-level implementation** (not Kubernetes built-in)
4. **ID conflicts possible** (need robust claiming mechanism)
5. **Learning curve** (team needs to understand custom identity model)

### When to Use Each Approach

#### Use StatefulSet When:
- ✅ You need large persistent storage per pod (10+ GB)
- ✅ Sequential startup/shutdown is required
- ✅ Network identity is critical (stable DNS names)
- ✅ Team has no external coordination service
- ❌ **Not suitable for our use case**

#### Use Pure Deployment When:
- ✅ Truly stateless workload (no identity needed)
- ✅ Simple architecture, minimal dependencies
- ✅ No consistent hashing or deterministic routing
- ❌ **Not suitable for our use case** (need stable IDs for consistent hashing)

#### Use Hybrid Approach When:
- ✅ **Need stable identities without PVCs** ← Our use case!
- ✅ **Frequent rolling updates with zero reassignment** ← Our use case!
- ✅ **Advanced deployment strategies required** ← Our use case!
- ✅ Small cache size (< 1 GB), fast rebuild
- ✅ Have external coordination service (NATS, etcd, Redis, etc.)
- ✅ Team can handle moderate complexity

---

## Implementation Comparison

### StatefulSet Implementation

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: defender
spec:
  serviceName: defender
  replicas: 30
  selector:
    matchLabels:
      app: defender
  template:
    metadata:
      labels:
        app: defender
    spec:
      containers:
      - name: defender
        image: defender:v2.2.0
        env:
        - name: DEFENDER_ID
          valueFrom:
            fieldRef:
              fieldPath: metadata.name  # defender-0, defender-1, ...
  volumeClaimTemplates:  # PVC per pod
  - metadata:
      name: cache
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 10Gi
```

**Pod lifecycle**:
```
1. Pod created: defender-5
2. PVC created: cache-defender-5
3. PVC attached to defender-5
4. Container starts, reads DEFENDER_ID from metadata.name
5. Application uses "defender-5" as identity
```

---

### Hybrid Approach Implementation

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: defender
spec:
  replicas: 30
  selector:
    matchLabels:
      app: defender
  template:
    metadata:
      labels:
        app: defender
    spec:
      containers:
      - name: defender
        image: defender:v2.2.0
        env:
        - name: NATS_SERVERS
          value: "nats://nats:4222"
        - name: NATS_KV_STABLE_IDS
          value: "stable-ids"
        - name: MAX_DEFENDERS
          value: "100"
        # No DEFENDER_ID env var, claimed at runtime
```

**Pod lifecycle with ID claiming**:
```go
func claimStableID(natsKV nats.KeyValue, maxDefenders int) (string, error) {
    podName := os.Getenv("POD_NAME")
    podUID := os.Getenv("POD_UID")

    // Try to claim IDs sequentially
    for id := 0; id < maxDefenders; id++ {
        defenderID := fmt.Sprintf("defender-%d", id)

        claim := ClaimInfo{
            StableID:   defenderID,
            PodName:    podName,
            PodUID:     podUID,
            ClaimedAt:  time.Now(),
        }

        // Atomic create operation (fails if key exists)
        _, err := natsKV.Create(defenderID, marshal(claim))
        if err == nil {
            // Success! This ID is ours
            log.Infof("Claimed stable ID: %s", defenderID)

            // Start heartbeat goroutine to maintain claim (TTL 30s)
            go maintainIDClaim(natsKV, defenderID, claim)

            return defenderID, nil
        }

        // ID already claimed, try next one
        log.Debugf("ID %s already claimed, trying next", defenderID)
    }

    return "", fmt.Errorf("no available IDs (tried 0-%d)", maxDefenders-1)
}

func maintainIDClaim(kv nats.KeyValue, defenderID string, claim ClaimInfo) {
    ticker := time.NewTicker(2 * time.Second)  // Heartbeat every 2s
    defer ticker.Stop()

    for range ticker.C {
        claim.LastHeartbeat = time.Now()

        // Update claim (resets TTL to 30s)
        _, err := kv.Put(defenderID, marshal(claim))
        if err != nil {
            log.Errorf("Failed to maintain ID claim: %v", err)
        }
    }
}
```

---

## Migration Path

If you're considering moving from StatefulSet to Hybrid:

### Step 1: Deploy NATS JetStream
```bash
# Deploy 3-node NATS cluster
kubectl apply -f nats-statefulset.yaml

# Create KV buckets
nats kv add stable-ids --replicas=3 --ttl=30s
nats kv add defender-heartbeats --replicas=3 --ttl=30s
nats kv add defender-assignments --replicas=3 --ttl=0
```

### Step 2: Update Application Code
```go
// Add ID claiming logic to defender startup
func main() {
    // Connect to NATS
    nc, _ := nats.Connect(os.Getenv("NATS_SERVERS"))
    js, _ := nc.JetStream()
    kv, _ := js.KeyValue("stable-ids")

    // Claim stable ID
    defenderID, err := claimStableID(kv, 100)
    if err != nil {
        log.Fatalf("Failed to claim ID: %v", err)
    }

    // Use defenderID for consistent hashing
    log.Infof("Starting with stable ID: %s", defenderID)

    // ... rest of application logic
}
```

### Step 3: Blue-Green Cutover
```bash
# Deploy new hybrid deployment (claim IDs 30-59)
kubectl apply -f defender-deployment.yaml
kubectl scale deployment defender --replicas=30

# Wait for all 30 new defenders to be ready
kubectl wait --for=condition=ready pod -l app=defender,version=v2 --timeout=60s

# Scale down old StatefulSet (release IDs 0-29)
kubectl scale statefulset defender --replicas=0

# Verify chamber redistribution
# New defenders with IDs 30-59 now handle all chambers

# Optional: Scale down new deployment to 30, let them claim IDs 0-29
kubectl scale deployment defender --replicas=0
kubectl scale deployment defender --replicas=30
# New pods will claim IDs 0-29 (now available)
```

---

## Conclusion

### Why We Chose the Hybrid Approach

Our requirements prioritize:

1. **Regular version releases** → Need fast, zero-impact rolling updates
2. **Dynamic operations** → Blue-green, canary, progressive rollout
3. **Scalability** → Fast elastic scaling (HPA)
4. **Stateless design** → No PVCs, rebuild-able cache

The **Hybrid Approach (Deployment + Self-Assigned Stable IDs)** is the only approach that satisfies all these requirements, particularly the critical requirement of **zero-reassignment rolling updates**.

### Key Innovation

The hybrid approach decouples application identity (stable IDs) from pod identity (random names), enabling:

- **Kubernetes benefits**: Fast parallel scaling, flexible deployment strategies, no PVC overhead
- **Application benefits**: Stable identities for consistent hashing, zero-reassignment updates
- **Operational benefits**: Advanced deployment patterns (blue-green, canary), fast recovery

This is a **best-of-both-worlds** solution that combines Kubernetes Deployment flexibility with StatefulSet-like stable identities, implemented at the application level using an external coordination service (NATS KV).

### Final Recommendation

**Use the Hybrid Approach** for:
- Distributed stateless processing systems
- Consistent hashing / partition assignment workloads
- Frequent rolling updates with zero reassignment requirement
- Advanced deployment strategies (blue-green, canary)
- Small, rebuild-able caches (< 1 GB)

**Accept the trade-offs**:
- Higher initial implementation complexity
- External dependency (NATS KV or similar)
- Team learning curve

**Reject the trade-offs** only if:
- Team cannot handle moderate complexity
- No external coordination service available
- Truly need large persistent storage (10+ GB per pod)

---

## Related Documents

- [Design Decisions - Decision #6](./design-decisions.md#decision-6-kubernetes-workload-deployment-with-self-assigned-stable-ids-hybrid-approach)
- [High-Level Design - Layer 4: Kubernetes Deployment](./high-level-design.md#layer-4-kubernetes-deployment-with-self-assigned-stable-ids)
- [Data Flow - Stable ID Claiming Flow](./data-flow.md#flow-6-stable-id-claiming)
- [Rolling Update Scenario](../05-operational-scenarios/rolling-update.md)
- [Constraints - Deployment Strategy Requirements](../../01-requirements/constraints.md#deployment-strategy-requirements)
