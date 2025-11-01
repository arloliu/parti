# WeightedConsistentHash Strategy — Design Document

**Status**: ✅ Design Approved – Ready for Implementation
**Created**: November 1, 2025
**Approved**: November 1, 2025
**Target**: v1.1.0 (Phase 4 – Assignment Edge Cases)
**Estimated Effort**: 3–4 hours implementation + 1 hour testing

---

## 1. Problem Statement

### Current Landscape
- ✅ `ConsistentHash` strategy provides stable partition placement but ignores weights.
- ✅ `RoundRobin` strategy exists for simple unweighted distribution.
- ✅ `internal/hash/ring.go` exposes a `WeightedRing` capable of weight-aware placement.
- ❌ No exported strategy leverages partition weights, leaving heavy partitions clustered and workers overloaded.

### Why It Matters
Production workloads process partitions whose batch sizes vary by 100×–500×. Without weighting:
- A small subset of workers routinely exceeds CPU and memory limits.
- Rebalancing causes cascading failures (OOM, long GC pauses, uneven latency).
- Operational teams must intervene manually, nullifying the value of automated partition management.

The library specification already promises `WeightedConsistentHashStrategy(150)` as the default. Missing functionality blocks Phase 4 edge-case testing and prevents users from adopting Parti in heterogeneous workloads.

---

## 2. Goals & Success Metrics

### Primary Goals
1. **Weighted Load Balancing** – respect partition weights during assignment to avoid overload.
2. **Partition Stability** – keep >90 % of partitions on the same worker during scale events.
3. **Extreme Weight Handling** – gracefully distribute partitions 100×–500× heavier than the median.
4. **Operational Simplicity** – expose behaviour through a single strategy mirroring existing API ergonomics.

### Success Metrics
- **Load Variance**: Per-worker total weight stays within 30 % of the cluster average post assignment.
- **Stability**: <10 % of partitions move when the worker set changes from 100 → 110 and back.
- **Extreme Distribution**: Each worker receives at most ⌈E / W⌉ + 1 extreme partitions (E = extreme partitions, W = workers).
- **Performance**: Equal-weight workloads match `ConsistentHash` latency (≤5 % overhead in benchmarks).

### Non-Goals
- Dynamic weight recalibration from runtime metrics (weights remain static per evaluation cycle).
- Worker capacity awareness (assume homogeneous workers for now).
- Multi-dimensional resource constraints (CPU vs memory vs bandwidth) within this release.

---

## 3. Requirements & Constraints

### Functional Requirements
- Accept `[]parti.Partition` where `Partition.Weight()` may return zero. Treat zero as the configured default weight.
- Reject assignments when `len(workers) == 0` with a deterministic `ErrNoWorkers`.
- Allow `len(partitions) == 0`; return each worker with an empty slice to preserve determinism.
- Maintain deterministic output given identical inputs and hash seed.
- Provide configuration knobs for virtual nodes, hash seed, overload threshold, extreme threshold, default weight, and logger.

### Non-Functional Requirements
- Safe for concurrent use: the strategy must not retain mutable state across calls. All per-call data structures live on the stack or heap local to `Assign`.
- Honour Go style and `golangci-lint` constraints (function length, cyclomatic complexity ≤22, etc.).
- Avoid quadratic behaviour; algorithm should be O(N + E log E + W).
- Integrate with project logging (via `parti.Logger`) for validation warnings and debugging messages.

### Environmental Constraints
- Go ≥ 1.25.0.
- No additional third-party dependencies.

---

## 4. Real-World Reference Scenario

- **Workers**: 100 nodes (rapid scale-up to 110, gradual scale-down over ~24 h).
- **Partitions**: 3 000 total. 2 850 “normal” (weight ≈ 100), 150 “extreme” (weight 10 000–50 000).
- **Cache**: Optional (DB fallback acceptable). Partition movement, not cache misses, drives operational cost.

Design must keep extreme partitions evenly distributed while ensuring normal partitions remain largely stationary between rebalances.

---

## 5. Component Responsibilities

| Component | Responsibility |
|-----------|----------------|
| `strategy/WeightedConsistentHash` | Public strategy that classifies partitions, manages overload enforcement, and orchestrates assignments using the ring. |
| `internal/hash/WeightedRing` | Generic ring providing consistent hashing with optional weights. Remains agnostic of “extreme” heuristics so other consumers can reuse it. |
| `strategy/options.go` | Hosts shared option builders (`WithVirtualNodes`, `WithHashSeed`) and weighted-only options; wires logger dependency. |
| `parti.Logger` | Optional logger for configuration warnings (WARN) and placement diagnostics (DEBUG). Defaults to `logging.Nop`. |

This separation avoids duplicating specialised behaviour inside the reusable ring while keeping the strategy cohesive.

---

## 6. Algorithm Overview

### Definitions
- `effectiveWeight(p) = max(p.Weight(), defaultWeight)`.
- `avgPartitionWeight = totalWeight / len(partitions)` when `len(partitions) > 0`, else 0.
- `extremeThreshold = max(extremeThresholdConfig, 1.5) * avgPartitionWeight`.
- `avgWorkerWeight = totalWeight / len(workers)` when `len(workers) > 0`, else 0.
- `maxWorkerWeight = max(overloadThresholdConfig, 1.15) * avgWorkerWeight`.

### Guard Conditions
1. **No Workers** – return `ErrNoWorkers` before touching the ring.
2. **Zero Partitions** – return `map[string][]Partition{worker: nil}` for each worker. Guarantees deterministic output and simplifies consumers.
3. **Pre-seeded Loads** – initialise `workerLoad := map[string]int64{}` with every worker key so fallback properly considers idle workers.

### Placement Flow
```
1. Fast Path – Equal Weights
   If all effective weights match, delegate to ConsistentHash.Assign to avoid extra work.

2. Weight Accumulation
   totalWeight = Σ effectiveWeight(p)
   If totalWeight == 0, treat all partitions as normal (no extremes) and maxWorkerWeight == 0.

3. Classification
   extremes = partitions where effectiveWeight(p) > extremeThreshold
   normals  = remaining partitions

4. Assign Extremes First
   Sort extremes descending by effective weight.
   For each index i, partition p:
       worker = workers[i % len(workers)]
       assignments[worker] append p
       workerLoad[worker] += effectiveWeight(p)

5. Assign Normal Partitions
   For each normal partition p:
       candidate = ring.GetNodeForPartition(p.ID())
       if workerLoad[candidate] + effectiveWeight(p) <= maxWorkerWeight:
           place on candidate
       else:
           fallback = findLightestWorker(workers, workerLoad)
           place on fallback (even if this exceeds maxWorkerWeight)
           if overflow occurs, emit single DEBUG log per rebalance summarising overflow count

6. Return assignments map keyed by worker identifier.
```

### Helper Behaviour
- `findLightestWorker` scans the worker slice to preserve deterministic ordering and considers idle workers first.
- Ties break lexicographically to guarantee repeatable distribution.
- Logging is rate-limited per call: overflow events aggregate counters emitted once before returning.

---

## 7. Configuration & Validation

| Option | Default | Minimum | Notes |
|--------|---------|---------|-------|
| `WithVirtualNodes(int)` | 150 | 1 | Shared with `ConsistentHash`. |
| `WithHashSeed(uint64)` | 0 | — | Seed for deterministic hashing. |
| `WithOverloadThreshold(float64)` | 1.3 | 1.15 | Soft cap for worker load variance. Values below minimum auto-corrected with WARN log. |
| `WithExtremeThreshold(float64)` | 2.0 | 1.5 | Multiplier over average partition weight. |
| `WithDefaultWeight(int64)` | 1 | 1 | Applied when `Partition.Weight()` returns 0. |
| `WithLogger(parti.Logger)` | `logging.Nop` | — | Used for WARN/DEBUG output; optional. |

Validation occurs during construction. Each coerced value triggers exactly one WARN (`logger.Warn("overloadThreshold too low", "provided", v, "using", 1.15)`). When the caller supplies no logger, warnings are silently discarded (matching existing patterns).

---

## 8. Concurrency & Complexity

- **Thread Safety**: The strategy holds only immutable configuration; all per-assignment state is local. Safe for concurrent `Assign` calls without extra locking.
- **Time Complexity**: O(N + E log E + W) where N = normal partitions, E = extreme partitions, W = workers (fallback scan). In the reference case (N≈2 850, E=150, W≈110) the dominant cost remains linear in N.
- **Space Complexity**: O(N + W) for temporary slices and load maps.

---

## 9. Testing & Verification Plan

### Unit Tests (`strategy/weighted_consistent_hash_test.go`)
- **Equal Weights** – confirm fast path matches `ConsistentHash` output bit-for-bit.
- **Zero Partitions** – verify deterministic empty assignments per worker.
- **No Workers** – expect `ErrNoWorkers`.
- **Unequal Weights** – synthetic workload (weights 1…100) stays within 30 % variance.
- **Extreme Scenario** – model the 3 000 partition production case; assert round-robin extreme distribution and variance bound.
- **Fallback Overflow** – create a partition heavier than `maxWorkerWeight`; ensure placement succeeds and overflow counter logged once.
- **Configuration Validation** – provide illegal thresholds/default weight and exercise WARN logging via a test logger.
- **Concurrency Smoke** – run `Assign` from multiple goroutines under `-race` to guard against data races.

### Assignment Edge Tests (`strategy/assignment_edge_test.go`)
- Add `WeightedConsistentHash` to every existing scenario (zero partitions, single worker, more workers than partitions, mixed weights).
- Include explicit assertion that fallback selects an idle worker when candidate is overloaded.

### Stability Metric Harness (`test/integration/assignment_correctness_test.go`)
- Capture baseline assignment for 100 workers.
- Re-run after adding 10 workers; compute fraction of partitions that changed owners (<10 %).
- Scale back to 100 and repeat measurement to ensure hysteresis does not accumulate churn.

### Benchmarks (`strategy/weighted_consistent_hash_bench_test.go`)
- Equal weights vs `ConsistentHash`.
- Unequal weights.
- Extreme-heavy workloads.
- Scaling study with workers = 10, 100, 1 000.

---

## 10. Implementation Plan

### Phase 1 – Strategy Skeleton (≈1.5 h)
- Implement `WeightedConsistentHash` struct + constructor with validation and logger wiring.
- Add helper functions (`effectiveWeight`, `allWeightsEqual`, `findLightestWorker`).
- Write `Assign` with guards, extreme-first placement, fallback logging, and deterministic tie-breaking.

### Phase 2 – Options & Ring Utilities (≈1 h)
- Extend `strategy/options.go` with shared and weighted-specific builders.
- Update `internal/hash/ring.go` defaults (`defaultWeight = 1`) and expose necessary helper methods without embedding extreme logic.
- Ensure ring overload threshold remains configurable for other internal consumers.

### Phase 3 – Testing (≈1 h)
- Author unit tests enumerated above.
- Update edge-case suites.
- Add integration harness for stability metric (or extend existing one if present).

### Phase 4 – Benchmarks & Telemetry (≈0.5 h)
- Implement benchmarks.
- Emit DEBUG summary logs for overflow/extreme counts with logger (if provided).

### Phase 5 – Documentation & Release Notes (≈0.5 h)
- Update `strategy/doc.go`, `docs/library-specification.md`, NEXT_STEPS/project status, and provide a GoDoc example demonstrating configuration options.

---

## 11. Risks & Mitigations

| Risk | Mitigation |
|------|------------|
| Partitions far exceed `maxWorkerWeight`, causing frequent soft-cap overflow. | Document expectation; future work may introduce hard caps or partition splitting. |
| Sorting cost grows if >20 % of partitions are extreme. | Highlight as limitation; benchmark and consider chunked scheduling in later iterations. |
| Callers mutate returned assignment slices. | Document that slices are owned by caller but should be treated as read-only snapshots. |

---

## 12. Future Enhancements

- **Dynamic Weight Adjustment** – feed runtime metrics back into weights and trigger targeted rebalances.
- **Worker Capacity Awareness** – incorporate per-worker capacity hints (CPU, memory) into load calculations.
- **Multi-Dimensional Weights** – support weighting across multiple metrics via heuristic scoring or linear programming.

---

## Appendix – Helper Pseudocode

```go
func effectiveWeight(p Partition, defaultWeight int64) int64 {
    if w := p.Weight(); w > 0 {
        return w
    }
    return defaultWeight
}

func allWeightsEqual(partitions []Partition, defaultWeight int64) bool {
    if len(partitions) == 0 {
        return true
    }
    first := effectiveWeight(partitions[0], defaultWeight)
    for _, p := range partitions[1:] {
        if effectiveWeight(p, defaultWeight) != first {
            return false
        }
    }
    return true
}

func findLightestWorker(workers []string, workerLoad map[string]int64) string {
    lightest := workers[0]
    minLoad := workerLoad[lightest]
    for _, w := range workers[1:] {
        load := workerLoad[w]
        if load < minLoad || (load == minLoad && w < lightest) {
            lightest = w
            minLoad = load
        }
    }
    return lightest
}
```

---

**End of Document**
