# Design Decisions

## Overview

This document provides detailed rationale for key architectural decisions, explaining the trade-offs considered and why specific approaches were chosen.

## Assignment Strategy: Leader-Controlled vs Distributed Consensus

### Decision
Use **embedded leader-controlled assignment** instead of distributed consensus (Raft, Paxos).

### Problem Statement
Need to assign 2,400~5000 chambers to 30-150 defenders with:
- Minimal reassignment during scaling
- Single source of truth
- Automatic failover
- No external dependencies

### Options

#### Option A: Distributed Consensus (Raft/Paxos)
**Pros**:
- No single point of failure
- Formally verified correctness
- Multiple nodes participate in decision

**Cons**:
- Super complex implementation
- Requires 3+ coordinator nodes (operational overhead)
- Network partitions can cause split-brain
- Slower decision-making (multiple round-trips)

#### Option B: Leader-Controlled Assignment (Selected)
**Pros**:
- Simple implementation (~200 lines)
- Fast decision-making (single node calculates)
- Embedded in defender process (no separate service)
- Automatic failover (election agent handles)
- Single source of truth (leader publishes to NATS KV)

**Cons**:
- Brief pause during leader failover (~5-10s, acceptable, no inline impact)
- Leader has slightly higher resource usage (+100 MB, +50m CPU, acceptable)

## Assignment Granularity: Tool-Level vs Chamber-Level

### Decision
Assign at **chamber level** (not tool level).

### Problem Statement
Each tool has 1-4 chambers:
- Entire tool (1,500-2000 assignment units)
- Individual chambers (1,500-8.000 assignment units)

### Analysis

**Tool-Level Assignment**:
- Pros: Fewer assignment units (simpler)
- Cons: Coarse-grained load balancing (tool with 4 chambers = 4x load of tool with 1 chamber)

**Chamber-Level Assignment** (Selected):
- Pros: Fine-grained load balancing
- Pros: Better weight distribution
- Cons: More assignment units (but still manageable)

**Example**:
```
Tool-001 has 4 chambers:
- Tool-level: All 4 go to defender-5 (high load)
- Chamber-level: Chambers distributed to defender-5, 7, 12, 18 (balanced load)
```

## Consistent Hashing: Virtual Nodes and Hash Function

### Decision
Use **150-200 virtual nodes per defender** with **xxHash 64-bit** hash function.

### Problem Statement
Need a consistent hashing implementation that:
- Distributes 2,400-5,000 chambers evenly across 30-150 defenders
- Minimizes chamber reassignment during scaling
- Avoids hash collisions
- Performs fast enough for real-time rebalancing

### Virtual Node Concept

A **virtual node** (also called virtual replica) is a technique where each physical defender is represented by multiple points on the hash ring.

**Without Virtual Nodes** (1 defender = 1 hash point):
```
Hash Ring: [0 ----------------------- 2^64-1]
            ↑                         ↑
        defender-0               defender-1

Problem: Uneven distribution
- defender-0 gets 40% of chambers
- defender-1 gets 60% of chambers
```

**With Virtual Nodes** (1 defender = 150 hash points):
```
Hash Ring: [0 ----------------------- 2^64-1]
            ↑  ↑    ↑  ↑    ↑   ↑    ↑  ↑
            d0 d1   d0 d1   d0  d1   d0 d1
            v0 v0   v1 v1   v2  v2  v74 v74

Each defender has multiple "virtual" positions:
- defender-0: hash(defender-0-vnode-0), ..., hash(defender-0-vnode-149)
- defender-1: hash(defender-1-vnode-0), ..., hash(defender-1-vnode-149)

Result: Even distribution (~50% each)
```

**Benefits**:
1. **Balanced load**: More hash points = better distribution
2. **Reduced variance**: Standard deviation of chamber counts decreases
3. **Smoother rebalancing**: Chambers spread to multiple neighbors, not just one

### Virtual Node Count Analysis

| Virtual Nodes | Chamber Movement (Scale Up) | Distribution Quality |
|--------------|----------------------------|---------------------|
| 10 | ~25-30% | Poor (high variance) |
| 50 | ~15-20% | Fair |
| **150** | **~15-20%** | **Good** |
| **200** | **~15-20%** | **Good** |
| 500 | ~15-20% | Good (diminishing returns) |

**Trade-off**:
- More virtual nodes = better distribution, but more hash calculations
- **150-200 is optimal**: good distribution, reasonable compute cost

### Hash Function: xxHash 64-bit

**Why 64-bit hash space?**

At maximum scale (150 defenders, 200 vnodes, 5,000 chambers):
```
Total hash points: 150 × 200 + 5,000 = 35,000 points

32-bit hash space (e.g., MurmurHash3):
  Hash space: 2^32 = 4.3 billion
  Collision risk: sqrt(35,000) / sqrt(2^32) ≈ 0.003 (0.3%)
  Expected collisions: ~105 (unacceptable!)

64-bit hash space (xxHash):
  Hash space: 2^64 = 18.4 quintillion
  Collision risk: sqrt(35,000) / sqrt(2^64) ≈ 10^-11 (negligible)
  Expected collisions: ~0 (zero)
```

**Why xxHash specifically?**
- **Extremely fast**: 10-15 GB/s throughput
- **Zero collision risk**: Massive 2^64 hash space
- **Excellent distribution**: Passes SMHasher test suite
- **CPU-friendly**: SIMD optimizations

### Performance

**Assignment calculation with 30 defenders, 5,000 chambers, 200 vnodes**:
```
Virtual node hashing: 30 × 200 = 6,000 hashes × 0.1 μs = 0.6 ms
Chamber hashing:      5,000 hashes × 0.1 μs = 0.5 ms
Ring sorting:         O(N log N) = 6,000 × log(6,000) = 0.1 ms
Chamber lookup:       5,000 × O(log 6,000) = 5 ms
Total: ~6 ms (fast enough for real-time rebalancing)
```

### Implementation Example

```go
import (
    "slices"
    "github.com/cespare/xxhash/v2"
)

type Ring struct {
    nodes      []RingNode  // Sorted by hash
    vnodeCount int         // 150-200 vnodes per defender
}

type RingNode struct {
    hash       uint64  // 64-bit hash value
    defenderID int32   // Physical defender ID
}

// Create consistent hashing ring
func NewRing(defenderIDs []int, vnodeCount int) *Ring {
    ring := &Ring{
        nodes:      make([]RingNode, 0, len(defenderIDs)*vnodeCount),
        vnodeCount: vnodeCount,
    }

    // Create virtual nodes for each defender
    for _, defenderID := range defenderIDs {
        for vnode := 0; vnode < vnodeCount; vnode++ {
            key := fmt.Sprintf("defender-%d-vnode-%d", defenderID, vnode)
            hash := xxhash.Sum64String(key)
            ring.nodes = append(ring.nodes, RingNode{
                hash:       hash,
                defenderID: int32(defenderID),
            })
        }
    }

    slices.SortFunc(ring.nodes, func(a, b RingNode) int {
        if a.hash < b.hash {
            return -1
        } else if a.hash > b.hash {
            return 1
        }
        return 0
    })

    return ring
}

// Lookup: Find defender for chamber (O(log N) binary search)
func (r *Ring) Lookup(chamberID int) int32 {
    key := fmt.Sprintf("chamber-%d", chamberID)
    hash := xxhash.Sum64String(key)

    idx, found := slices.BinarySearchFunc(r.nodes, hash, func(node RingNode, target uint64) int {
        if node.hash < target {
            return -1
        } else if node.hash > target {
            return 1
        }
        return 0
    })

    if !found {
        // Hash not found exactly, idx points to insertion position
        // This is the next node clockwise on the ring
        if idx == len(r.nodes) {
            idx = 0  // Wrap around to first node
        }
    }

    return r.nodes[idx].defenderID
}
```

### Memory Impact

**In-memory ring structure**:
```
30 defenders × 200 vnodes = 6,000 nodes
Each node: 8 bytes (uint64 hash) + 4 bytes (int32 defender ID) = 12 bytes
Total: 6,000 × 12 = 72 KB

Impact: Negligible (< 0.01% of defender's 8-16 GB memory)
```

## Weight-Aware Load Balancing

### Decision
Use **weighted consistent hashing with post-assignment rebalancing** to account for chamber workload differences.

### Problem Statement
Chambers have vastly different workloads, but basic consistent hashing treats all chambers equally.

**Chamber Weight Formula**:
```
Weight = SVID_count × Data_Collection_Freq × Context_Duration_Seconds
```

**Real-World Weight Variance**:
```
Lightweight chamber:  50 SVIDs × 1 Hz × 600s = 30,000 weight
Medium chamber:      100 SVIDs × 1 Hz × 1,200s = 120,000 weight
Heavy chamber:       200 SVIDs × 10 Hz × 1,800s = 3,600,000 weight

Weight ratio: 1 : 4 : 120 (massive difference!)
```

**Why This Matters**:
- Consistent hashing distributes chambers evenly **by count**, not by **workload**
- Defender-0 might get 80 lightweight chambers (total weight: 2.4M)
- Defender-1 might get 80 heavy chambers (total weight: 288M)
- **Result**: 120× load imbalance despite equal chamber counts

### Approach: Two-Phase Assignment

#### Phase 1: Consistent Hashing (Initial Assignment)

Use standard consistent hashing to get initial chamber distribution:

```go
// Phase 1: Initial assignment (ignores weight)
assignments := make(map[string]string) // chamber_key → defender_id

for _, chamber := range chambers {
    chamberKey := fmt.Sprintf("%s:%s", chamber.ToolID, chamber.ChamberID)
    hash := xxhash.Sum64String(chamberKey)
    defenderID := ring.Lookup(hash)
    assignments[chamberKey] = defenderID
}
```

**Result**: Chambers distributed evenly by count (~80 chambers per defender)

#### Phase 2: Weight-Aware Rebalancing (Correction)

Calculate total weight per defender and rebalance if imbalance exceeds threshold:

```go
// Phase 2: Weight-aware rebalancing
type DefenderLoad struct {
    DefenderID    string
    TotalWeight   int64
    Chambers      []Chamber
}

func RebalanceByWeight(assignments map[string]string, chambers []Chamber, threshold float64) map[string]string {
    // 1. Calculate weight per defender
    defenderLoads := make(map[string]*DefenderLoad)
    totalWeight := int64(0)

    for _, chamber := range chambers {
        chamberKey := fmt.Sprintf("%s:%s", chamber.ToolID, chamber.ChamberID)
        defenderID := assignments[chamberKey]

        if defenderLoads[defenderID] == nil {
            defenderLoads[defenderID] = &DefenderLoad{
                DefenderID: defenderID,
                Chambers:   make([]Chamber, 0),
            }
        }

        defenderLoads[defenderID].TotalWeight += chamber.Weight
        defenderLoads[defenderID].Chambers = append(defenderLoads[defenderID].Chambers, chamber)
        totalWeight += chamber.Weight
    }

    avgWeight := totalWeight / int64(len(defenderLoads))
    maxAllowedWeight := int64(float64(avgWeight) * (1.0 + threshold))
    minAllowedWeight := int64(float64(avgWeight) * (1.0 - threshold))

    // 2. Sort defenders by load
    overloadedDefenders := make([]*DefenderLoad, 0)
    underloadedDefenders := make([]*DefenderLoad, 0)

    for _, load := range defenderLoads {
        if load.TotalWeight > maxAllowedWeight {
            overloadedDefenders = append(overloadedDefenders, load)
        } else if load.TotalWeight < minAllowedWeight {
            underloadedDefenders = append(underloadedDefenders, load)
        }
    }

    // 3. Rebalance: Move chambers from overloaded to underloaded defenders
    for len(overloadedDefenders) > 0 && len(underloadedDefenders) > 0 {
        overloaded := overloadedDefenders[0]
        underloaded := underloadedDefenders[0]

        // Sort overloaded defender's chambers by weight (heaviest first)
        slices.SortFunc(overloaded.Chambers, func(a, b Chamber) int {
            return int(b.Weight - a.Weight)
        })

        // Move heaviest chamber that fits
        for i, chamber := range overloaded.Chambers {
            if underloaded.TotalWeight + chamber.Weight <= maxAllowedWeight {
                // Move chamber
                chamberKey := fmt.Sprintf("%s:%s", chamber.ToolID, chamber.ChamberID)
                assignments[chamberKey] = underloaded.DefenderID

                // Update loads
                overloaded.TotalWeight -= chamber.Weight
                underloaded.TotalWeight += chamber.Weight
                overloaded.Chambers = append(overloaded.Chambers[:i], overloaded.Chambers[i+1:]...)
                underloaded.Chambers = append(underloaded.Chambers, chamber)

                break
            }
        }

        // Remove from lists if within threshold
        if overloaded.TotalWeight <= maxAllowedWeight {
            overloadedDefenders = overloadedDefenders[1:]
        }
        if underloaded.TotalWeight >= minAllowedWeight {
            underloadedDefenders = underloadedDefenders[1:]
        }
    }

    return assignments
}
```

### Rebalancing Threshold

**Threshold Selection**: 20% (configurable)

```
Average weight per defender: 10,000,000
Allowed range: 8,000,000 - 12,000,000 (±20%)

If defender outside this range: trigger rebalancing
If within range: accept (preserve cache locality)
```

**Rationale**:
- **Too strict (5-10%)**: Excessive chamber movement, destroys cache locality
- **Too loose (40-50%)**: Significant load imbalance, some defenders overloaded
- **20% is balanced**: Tolerable imbalance, minimal chamber movement

### Weight Calculation and Caching

**Weight Metadata Storage** (Cassandra):

```sql
CREATE TABLE chamber_metadata (
    tool_id text,
    chamber_id text,
    svid_count int,
    collection_freq_hz int,
    avg_context_duration_sec int,
    calculated_weight bigint,
    last_updated timestamp,
    PRIMARY KEY ((tool_id), chamber_id)
);
```

**Weight Update Strategy**:
1. **Initial weight**: Estimated from equipment configuration
2. **Runtime update**: Every 24 hours, recalculate based on actual metrics
3. **Assignment uses cached weights**: Leader reads from Cassandra before calculation

```go
// Leader reads weights before assignment calculation
func (ac *AssignmentController) GetChamberWeights() map[string]int64 {
    weights := make(map[string]int64)

    query := "SELECT tool_id, chamber_id, calculated_weight FROM chamber_metadata"
    iter := ac.cassandra.Query(query).Iter()

    var toolID, chamberID string
    var weight int64

    for iter.Scan(&toolID, &chamberID, &weight) {
        key := fmt.Sprintf("%s:%s", toolID, chamberID)
        weights[key] = weight
    }

    return weights
}
```

### Performance Impact

**Weight-aware rebalancing overhead**:
```
Phase 1 (Consistent Hashing): ~6 ms (5,000 chambers)
Phase 2 (Weight Rebalancing):
  - Weight calculation: 5,000 chambers × 0.001 ms = 5 ms
  - Sorting defenders by load: O(D log D) = 30 × log(30) < 0.1 ms
  - Greedy reassignment: 100-500 chambers × 0.01 ms = 1-5 ms

Total: ~12-16 ms (still acceptable for real-time rebalancing)
```

### Chamber Movement Comparison

**Without Weight Rebalancing**:
```
Scale 30 → 45 defenders:
- Consistent hashing: ~20% chambers move (1,000 chambers)
- Load imbalance: Some defenders 3-5× overloaded
```

**With Weight Rebalancing**:
```
Scale 30 → 45 defenders:
- Consistent hashing: ~20% chambers move (1,000 chambers)
- Weight rebalancing: +5-10% additional movement (250-500 chambers)
- Total movement: ~25-30% chambers
- Load balance: All defenders within ±20% of average
```

**Trade-off**: Slightly more movement (5-10%) for significantly better load distribution

### Example: Weight-Aware Assignment

**Scenario**: 30 defenders, 2,400 chambers with varying weights

**Before Weight Rebalancing**:
```
Defender-0: 80 chambers, total weight = 15,000,000 (50% above average)
Defender-5: 80 chambers, total weight = 6,000,000 (40% below average)
Defender-12: 80 chambers, total weight = 11,000,000 (10% above average)
...

Average weight: 10,000,000
Standard deviation: 3,500,000 (35% variance!)
```

**After Weight Rebalancing (20% threshold)**:
```
Defender-0: 72 chambers, total weight = 11,500,000 (15% above average)
Defender-5: 88 chambers, total weight = 9,200,000 (8% below average)
Defender-12: 80 chambers, total weight = 10,500,000 (5% above average)
...

Average weight: 10,000,000
Standard deviation: 1,800,000 (18% variance)

Movement: 8 chambers moved from defender-0 to defender-5 (0.3% of total chambers)
```

### Configuration

```yaml
# Assignment Controller Configuration
assignment_controller:
  weight_rebalancing:
    enabled: true
    threshold: 0.20  # ±20% from average
    max_iterations: 5  # Prevent infinite loops
    preserve_locality: true  # Minimize chamber movement

  weight_metadata:
    cassandra_table: "chamber_metadata"
    cache_ttl: 3600  # 1 hour
    update_interval: 86400  # 24 hours
```

### Operational Benefits

1. **Balanced CPU/Memory Usage**: All defenders within 20% of average load
2. **Predictable Scaling**: Know exact capacity needed (avg_weight × defender_count)
3. **Fair Resource Allocation**: No "unlucky" defenders with all heavy chambers
4. **Cache Efficiency**: Minimal movement (5-10% additional) preserves most caches
5. **SLO Compliance**: Uniform processing times across all defenders

## Kubernetes Workload: Deployment vs StatefulSet

### Decision
Use **Deployment with self-assigned stable IDs** (hybrid approach).

### Problem Statement
````
```

## Kubernetes Workload: Deployment vs StatefulSet

### Decision
Use **Deployment with self-assigned stable IDs** (hybrid approach).

### Problem Statement
Need stable defender IDs for zero-reassignment rolling updates, but also need advanced deployment strategies (blue-green, canary).

### Options Considered

#### Option A: StatefulSet
**Pros**:
- Built-in stable network identity (defender-0, defender-1, etc.)
- Ordered pod creation/deletion
- Automatic PVC management (but we don't need PVC)

**Cons**:
- **Blue-green deployment is complex (name collisions)**
- **Canary deployment limited (partition-based only)**
- **Cannot run two versions simultaneously with same IDs**
- Less flexible for progressive rollout

#### Option B: Deployment with Self-Assigned Stable IDs (Selected)
**Pros**:
- Full Deployment flexibility (blue-green, canary, progressive)
- Works with Argo Rollouts, Flagger
- Can run 60 pods (30 blue + 30 green)
- Kubernetes-native deployment strategies
- Stable IDs via NATS KV pool (defender-0, defender-1, etc.)

**Cons**:
- Additional complexity (~200 lines for ID claiming logic, but the claiming logic is pretty simple)
- Dependency on NATS KV for startup (but NATS needs to be alive anyway)

### Implementation Example
```go
// Defender claims stable ID from NATS KV pool
id, err := d.claimStableID(ctx, maxDefenders)
// Sequential search: defender-0, defender-1, ..., until finds available
```

## Cache Storage: PVC vs In-Memory Only

### Decision
Use **in-memory SQLite cache WITHOUT PVC persistence**.

### Problem Statement
Should defenders persist cache to PVC for faster restarts?

### Analysis

**With PVC**:
- Pros: Cache survives pod restarts (faster warm-up)
- Cons:
  - PVC attachment delays pod rescheduling (30-60s), our TKS sucks at this part
  - PVC management complexity (storage classes, quotas)
  - Node affinity constraints (PVC tied to node)

**Without PVC** (Selected):
- Pros:
  - Fast pod rescheduling (no PVC attachment)
  - Truly stateless design
  - No storage management
  - Consistent hashing ensures same chambers = fast rebuild
- Cons:
  - Cold cache after restart (but only ~100 MB U-chart history)

**Key Insight**: Cache is small (~100 MB) and only stores U-chart history. Rebuilding from Cassandra takes ~5-10 seconds, which is acceptable.

## Stable ID Pool Size

### Decision
Pool size = **200 IDs** (defender-0 to defender-199), configured via environment variable.

### Rationale

**Why 200 as default?**
- Current workload: 30-50 defenders (typical), 100-150 defenders (peak)
- **Safety margin**: 200 = 2× peak capacity (handles unexpected growth)
- Room for temporary over-provisioning during deployments (e.g., 150 old + 150 new pods during rolling update)

**Why configurable?**
- Different environments have different scales
- Can be set via `MAX_DEFENDER_COUNT` environment variable or configurateion field
- Prevents hard-coded limits that block scaling

**Pool exhaustion behavior**:
- New pod fails to claim ID → enters CrashLoopBackoff
- This is **intentional** (prevents overload beyond designed capacity)
- Operator must increase pool size + redeploy to scale beyond limit

**Algorithm**:
```
Pool: defender-0, defender-1, ..., defender-{MAX_DEFENDER_COUNT-1}
Claiming: Sequential search (try 0, 1, 2, ... until available)
Heartbeat: Every 2s (keep claim alive)
TTL: 30s (3× missed heartbeats = auto-release)
```

**Example configurations**:
```yaml
# Development environment (small scale)
env:
  - name: MAX_DEFENDER_COUNT
    value: "50"

# Production environment (standard)
env:
  - name: MAX_DEFENDER_COUNT
    value: "200"

# Large-scale production (high volume)
env:
  - name: MAX_DEFENDER_COUNT
    value: "500"
```

## Heartbeat Interval

### Decision
**2-second heartbeat interval** for both assignments and stable IDs.

### Analysis

| Interval | Crash Detection | Network Overhead | False Positives |
|----------|----------------|------------------|-----------------|
| 1s | 3s (fast) | High | More |
| **2s** | **6s** | **Moderate** | **Few** |
| 5s | 15s (slow) | Low | Rare |

**Trade-off**:
- Faster heartbeat = quicker crash detection, but more network traffic
- 2s with 3× missed = 6s detection (acceptable for this workload)

**Verdict**: **2 seconds** - good balance

## Assignment Calculation Trigger (CRITICAL PART)

### Decision
**State-aware adaptive triggering** with different windows based on operational context.

### Problem Statement
Different scenarios have different urgency requirements:
- **Cold start**: Many defenders join simultaneously → batch to prevent thrashing
- **Scale up/down**: Planned capacity change → fast but not instant
- **Crash**: Defender dies unexpectedly → emergency response needed

A single fixed window (e.g., 30s) cannot optimize for all three scenarios.

### State-Aware Trigger Strategy

| State | Detection | Trigger Window | Rationale |
|-------|-----------|---------------|-----------|
| **Cold Start** | 0 → N defenders in <60s | **30 seconds** | Batch all joins, prevent thrashing (30 calculations → 1) |
| **Planned Scale** | Gradual joins over >60s | **10 seconds** | Fast response, still batches small bursts |
| **Crash Recovery** | Defender misses 3 heartbeats | **Immediate (0s)** | Emergency: chambers need reassignment ASAP |
| **Steady State** | No changes | **No calculation** | Save CPU, no work needed |

### Implementation Strategy

```go
type TriggerState int

const (
    StateUnknown      TriggerState = iota
    StateColdStart    // Initial deployment (0 → many defenders)
    StatePlannedScale // Gradual scaling (controlled)
    StateCrash        // Unexpected defender failure
    StateSteady       // No changes
)

type AssignmentTrigger struct {
    defenderCount     int
    lastChangeTime    time.Time
    pendingChanges    []DefenderEvent
    currentState      TriggerState
}

// State detection logic
func (t *AssignmentTrigger) DetectState() TriggerState {
    now := time.Now()
    timeSinceChange := now.Sub(t.lastChangeTime)

    // Cold start: 0 → many defenders within 60s
    if t.defenderCount == 0 && len(t.pendingChanges) > 10 && timeSinceChange < 60*time.Second {
        return StateColdStart
    }

    // Crash: defender missed heartbeats (marked as FAILED)
    for _, event := range t.pendingChanges {
        if event.Type == DefenderCrashed {
            return StateCrash
        }
    }

    // Planned scale: gradual changes
    if len(t.pendingChanges) > 0 {
        return StatePlannedScale
    }

    return StateSteady
}

// Get trigger window based on state
func (t *AssignmentTrigger) GetTriggerWindow() time.Duration {
    switch t.DetectState() {
    case StateColdStart:
        return 30 * time.Second  // Batch all initial joins
    case StateCrash:
        return 0 * time.Second   // Immediate recalculation
    case StatePlannedScale:
        return 10 * time.Second  // Fast but batched
    case StateSteady:
        return time.Duration(0)  // No calculation needed
    default:
        return 10 * time.Second  // Default: conservative
    }
}
```

### Decision Logic Examples

**Example 1: Cold Start** (30s window)
```
T+0s:  defender-0 joins  → pending (count: 1)
T+5s:  defender-1 joins  → pending (count: 2)
T+10s: defender-2 joins  → pending (count: 3)
...
T+25s: defender-29 joins → pending (count: 30)
T+30s: State = COLD_START → Calculate ONCE for all 30
       Result: 1 calculation instead of 30
```

**Example 2: Planned Scale Up** (10s window)
```
T+0s:  30 defenders running (steady state)
T+100s: defender-30 joins → pending (count: 1)
T+105s: defender-31 joins → pending (count: 2)
T+110s: State = PLANNED_SCALE → Calculate for 32 defenders
        Result: Fast response (10s delay, acceptable for scale-up)
```

**Example 3: Crash Recovery** (immediate)
```
T+0s:  30 defenders running
T+100s: defender-15 misses heartbeat #1
T+102s: defender-15 misses heartbeat #2
T+104s: defender-15 misses heartbeat #3 → CRASH detected
T+104s: State = CRASH → Calculate IMMEDIATELY
        Result: Chambers reassigned within <1s
```

**Example 4: Multiple Crashes** (immediate, batched)
```
T+0s:   30 defenders running
T+100s: defender-10 crashes
T+100s: State = CRASH → Calculate immediately (29 defenders)
T+105s: defender-11 crashes (within same window)
T+105s: State = CRASH → Calculate immediately (28 defenders)
        Result: Each crash triggers recalculation (can't batch crashes)
```

### Trade-offs

**Why not always immediate?**
- Cold start: 30 immediate calculations = wasted CPU (30× overhead)
- Planned scale: Multiple pods in rolling update = thrashing

**Why immediate for crashes?**
- Chambers are orphaned (no defender processing them)
- Business impact: tFDC out-of-spec for orhpan chambers
- Can't wait 10-30s when data is not being processed

**Why 10s for planned scale?**
- Fast enough for operational changes
- Still batches small bursts (2-3 pods in quick succession)
- Avoids thrashing from rolling updates

### Verdict
**State-aware adaptive triggering**:
- ✅ Cold start: 30s (prevents thrashing)
- ✅ Planned scale: 10s (responsive)
- ✅ Crash: immediate (emergency response)
- ✅ Steady state: no calculation (efficient)

## Leader Election TTL

### Decision
**10-second TTL** with 5-second renewal interval.

### Rationale
- Leader renews every 5s
- If leader crashes, lock expires after 10s
- New leader elected within 5-10s total

**Trade-off**: Faster TTL (5s) would detect failures quicker, but increase network overhead.

## Summary Table

| Decision | Selected Approach | Key Benefit |
|----------|------------------|-------------|
| Message Broker | NATS JetStream | 10-20x faster operations |
| Assignment Strategy | Leader-controlled | Simplicity, speed |
| Assignment Granularity | Chamber-level | Better load balancing |
| Consistent Hashing | 150-200 vnodes + xxHash 64-bit | Zero collisions, optimal distribution |
| Kubernetes Workload | Deployment + stable IDs | Blue-green/canary support |
| Cache Storage | In-memory only | Truly stateless |
| ID Pool Size | 200 IDs (configurable) | Handles 2× peak + growth |
| Heartbeat Interval | 2 seconds | 6s crash detection |
| Assignment Trigger | State-aware (0s/10s/30s) | Crash=immediate, scale=fast, cold-start=batched |
| Leader Election TTL | 10 seconds | Fast failover |

## Related Documents

- [High-Level Design](./high-level-design.md) - System architecture overview
- [Data Flow](./data-flow.md) - Detailed message flows
- [State Machine](./state-machine.md) - Cluster state transitions
- [Kafka Limitations](../02-problem-analysis/kafka-limitations.md) - Why not Kafka
