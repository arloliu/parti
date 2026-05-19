# Simulation — Phase 3a: Ownership-Violation Detection (H1 + M1)

Single-PR plan to give the simulation oracle a way to distinguish a
**legitimate JetStream redelivery** from a **real ownership violation** (two
workers processing the same `(partition, seq)`). Items **H1** and **M1** in
the audit summary.

## Why

The current tracker treats every `partitionSeq <= lastReceived` as
`duplicateCount++` (`test/simulation/internal/coordinator/tracker.go:222-240`).
JetStream's at-least-once contract makes plain redelivery common (worker
crashed before ACK, NAK delays, etc.), so `DuplicateCount > 0` is uninformative —
the simulation cannot tell a Processing-Gate regression that briefly lets two
workers process the same seq apart from ordinary redelivery noise.

Splitting the counter is the **strongest invariant the oracle can express
today** that maps directly to the parti library's core exclusivity contract.
With this, `OwnershipViolationCount > 0` becomes a hard test failure signal,
distinct from informational `RedeliveryCount`.

## Out of scope

- **H2 (heartbeat-watcher invariants)**: Phase 3b. Requires a new goroutine
  decoding KV heartbeats; bigger scope.
- **H7 (checkpoint completeness)**: Phase 3c. Only matters on the crash-resume
  path.
- **R5 (FailureReport enrichment with attribution)** beyond what the new
  ownership-violation events add naturally. The reviewer-promised WorkerID +
  ProducerID + timeline overhaul is a separate PR.
- **DupTracer rework**: the existing `coordinator/duptrace.go` records
  per-duplicate sliding-window samples. It stays as-is for now; the new
  ownership-violation path is independent and additive.

## Design

### Tracker signature change

```go
// Old:
func (t *MessageTracker) RecordReceived(partitionID int, partitionSeq int64) ([]time.Duration, error)
// New:
func (t *MessageTracker) RecordReceived(partitionID int, partitionSeq int64, workerID string) ([]time.Duration, error)
```

The simulation already has `WorkerID` at the call site
(`coordinator.go:928` reads `msg.WorkerID`), so threading it through is
trivial.

### Tracker state additions

```go
// lastWorkerPerSeq is a bounded per-partition map of (seq → worker that
// first processed it). Used to classify a partitionSeq<=lastReceived
// observation as either redelivery (same worker) or ownership violation
// (different worker). When the per-partition map exceeds workerCacheMax,
// the smallest-seq entry is evicted.
lastWorkerPerSeq map[int]map[int64]string
workerCacheMax   int  // default 4096; configurable for stress tests

// New counters:
redeliveryCount         int64 // partitionSeq<=lastReceived AND originalWorker==currentWorker
ownershipViolationCount int64 // partitionSeq<=lastReceived AND originalWorker!=currentWorker
// duplicateCount is retained for fallback when the original worker is
// pruned (no data). Existing callers continue to see it.
```

### Classification logic

Replace the existing duplicate branch (`tracker.go:222-240`) with:

```go
if partitionSeq <= lastReceived {
    // Gap-healed path first (unchanged).
    if escalatedSet, ok := t.gapEscalated[partitionID]; ok {
        if _, wasGap := escalatedSet[partitionSeq]; wasGap {
            // Late arrival heals an escalated gap.
            delete(escalatedSet, partitionSeq)
            t.gapsHealedCount++
            t.physicalReceivedCount++
            // Record the worker that actually healed it.
            t.recordWorkerForSeq(partitionID, partitionSeq, workerID)
            return healedDurations, nil
        }
    }

    // Classify the duplicate. Empty workerID is unclassifiable (e.g.,
    // checkpoint restore replay); fall back to legacy duplicate counter
    // to avoid manufacturing false-positive ownership violations.
    var origWorker string
    var known bool
    if pmap, ok := t.lastWorkerPerSeq[partitionID]; ok {
        origWorker, known = pmap[partitionSeq]
    }
    switch {
    case !known, origWorker == "", workerID == "":
        // Pruned out, or one side has no worker info — legacy duplicate.
        t.duplicateCount++
        return healedDurations, &MessageDuplicateError{PartitionID: partitionID, Sequence: partitionSeq}
    case origWorker == workerID:
        // Same worker reprocessing — JetStream redelivery, expected.
        // Returns a typed event-as-error so the coordinator can dispatch
        // to RecordRedelivery() via errors.Is(err, ErrMessageRedelivery).
        // Not treated as a failure; not subject to stop-on-failure.
        t.redeliveryCount++
        return healedDurations, &MessageRedeliveryEvent{PartitionID: partitionID, Sequence: partitionSeq, WorkerID: workerID}
    default:
        // Different worker — exclusivity contract violated.
        t.ownershipViolationCount++
        return healedDurations, &MessageOwnershipViolationError{
            PartitionID:    partitionID,
            Sequence:       partitionSeq,
            OriginalWorker: origWorker,
            CurrentWorker:  workerID,
        }
    }
}
```

`recordWorkerForSeq` is a no-op when `workerID == ""` — restore replay
populates seqs without worker attribution, so future duplicates of those
seqs fall through the `origWorker == ""` arm above and remain plain
duplicates. This preserves the "detection horizon" semantic the
pruning strategy already establishes.

On the contiguous-advance path (`tracker.go:242-263`) and the out-of-order
arrival path (`tracker.go:187-211`), call `t.recordWorkerForSeq(...)` once
per physical receipt to populate the map.

### Pruning

When `len(t.lastWorkerPerSeq[pid]) > workerCacheMax`, evict the lowest-seq
entry. O(N) walk; only fires after the cache fills, so amortized cost is low.

For the default (4096), per partition memory ≈ 4096 × (8 byte int64 + ~16
byte string header) ≈ 100 KB. Total across 1500 partitions ≈ 150 MB. For
shorter configs, the cap is rarely reached.

The cap is exposed as `cfg.Coordinator.WorkerCacheMaxPerPartition` (default
4096) so stress runs can tune it.

**Config changes required:**

- `test/simulation/internal/config/config.go` — add
  `WorkerCacheMaxPerPartition int yaml:"worker_cache_max_per_partition"`
  to `CoordinatorConfig`.
- `test/simulation/internal/config/defaults.go` — apply default of 4096
  when zero (analogous to the existing `GapAging` / `ValidationWindow`
  defaulting blocks).
- `test/simulation/internal/config/validation.go` — reject values ≤ 0 with a
  clear message; zero would mean "evict immediately" which is meaningless.

**Detection horizon (acknowledged limit):** beyond the cache window,
duplicate-of-very-old-seq falls through to the legacy `DuplicateCount`
counter and is **not** classified as a violation even if the workers
differ. This is an explicit trade-off — bounded memory in exchange for a
finite detection horizon. Operators of long-running stress configs should
raise `worker_cache_max_per_partition` accordingly. Treat the value as a
guaranteed-detection lookback, not full-history ownership proof.

**Same logical worker-ID, different instance:** if chaos restarts worker-X
with the same `workerID`, a JetStream redelivery to the restarted instance
is classified as redelivery (not violation), because the simulation
identifies workers by logical ID, not process instance. This matches
parti's ownership semantics (stable worker IDs across restarts) and is
intentional. Documented here for completeness.

### New error/event types

```go
var ErrMessageOwnershipViolation = errors.New("ownership violation: same sequence processed by different workers")
var ErrMessageRedelivery         = errors.New("at-least-once redelivery") // informational

type MessageOwnershipViolationError struct {
    PartitionID    int    `json:"partition_id"`
    Sequence       int64  `json:"sequence"`
    OriginalWorker string `json:"original_worker"`
    CurrentWorker  string `json:"current_worker"`
}

func (e *MessageOwnershipViolationError) Error() string { ... }
func (e *MessageOwnershipViolationError) Unwrap() error { return ErrMessageOwnershipViolation }

// MessageRedeliveryEvent is returned as the error value when the same worker
// reprocesses a sequence. It is *informational* — not a failure. The
// coordinator dispatches it via errors.Is to record a Prometheus metric
// and does NOT propagate it to the stop-on-failure path or the failure
// report. Using the error channel for this keeps the dispatch shape
// consistent with gaps/duplicates/violations and avoids growing the
// RecordReceived return signature.
type MessageRedeliveryEvent struct {
    PartitionID int    `json:"partition_id"`
    Sequence    int64  `json:"sequence"`
    WorkerID    string `json:"worker_id"`
}

func (e *MessageRedeliveryEvent) Error() string { ... }
func (e *MessageRedeliveryEvent) Unwrap() error { return ErrMessageRedelivery }
```

### TrackerStats / GetStats additions

```go
type TrackerStats struct {
    // ... existing fields ...
    DuplicateCount          int   `json:"duplicate_count"`           // unchanged; pruned fallback
    RedeliveryCount         int64 `json:"redelivery_count"`          // NEW
    OwnershipViolationCount int64 `json:"ownership_violation_count"` // NEW
}
```

`GetStats()` populates the new fields. A new method `GetOwnershipViolationCount()`
exposed for the main-loop invariants check.

### Coordinator wiring

`coordinator.go:928` changes from:
```go
healed, err := c.tracker.RecordReceived(msg.PartitionID, msg.PartitionSequence)
```
to:
```go
healed, err := c.tracker.RecordReceived(msg.PartitionID, msg.PartitionSequence, msg.WorkerID)
```

The error-branch dispatch grows two new arms (in addition to the existing
`ErrMessageGap` / `ErrMessageDuplicate` ones):

```go
// Redelivery — informational; record metric and continue. NOT a failure.
if errors.Is(err, ErrMessageRedelivery) {
    if c.metricsCollector != nil {
        c.metricsCollector.RecordRedelivery()
    }
    // do not propagate to dup tracer or stop-on-failure
    continue receive loop
}
// Ownership violation — a real exclusivity failure.
if errors.Is(err, ErrMessageOwnershipViolation) {
    var ove *MessageOwnershipViolationError
    if errors.As(err, &ove) {
        if c.metricsCollector != nil {
            c.metricsCollector.RecordOwnershipViolation()
        }
        c.appendOwnershipViolation(*ove)  // bounded slice, cap 1000
        log.Printf("[Coordinator] OWNERSHIP VIOLATION: partition=%d seq=%d original=%s current=%s",
            ove.PartitionID, ove.Sequence, ove.OriginalWorker, ove.CurrentWorker)
        if c.stopOnFailure {
            c.internalTriggerFailure("Ownership violation", err)
            return
        }
    }
}
```

Order matters: the redelivery check must happen before the generic `err != nil`
gate so the existing gap/duplicate dispatch doesn't re-fire on a redelivery
event. Place the new arms at the top of the `if err != nil { ... }` block.

**DupTracer remains as-is.** It only records `ErrMessageDuplicate` —
i.e., the pruned-fallback case. Same-worker redeliveries (return
`ErrMessageRedelivery`) and ownership violations (return
`ErrMessageOwnershipViolation`) bypass it. This is intentional: the new
ownership-violation list in `FailureReport` is the structured replacement,
and dup-tracer's sliding-window dump remains useful for the legacy
pruned-duplicate path that has no per-event attribution.

### Failure report enrichment

`FailureReport` (`coordinator.go:863`) gets one new field:
```go
type FailureReport struct {
    // ... existing fields ...
    OwnershipViolations []MessageOwnershipViolationError `json:"ownership_violations,omitempty"`
}
```

Populated from a coordinator-side slice that the receive loop appends to on
each violation event. Bounded at N=1000 to cap report size.

### Main-loop invariants

`cmd/simulation/main.go:663-666` and `:711-714` currently check
`failures > 0 || late > 0 || lost > 0`. Add `ownershipViolations > 0` (new
condition):
```go
violations := coord.GetTracker().GetOwnershipViolationCount()
if failures > 0 || late > 0 || lost > 0 || initialExc > 0 || takeoverExc > 0 || violations > 0 {
    return fmt.Errorf("stability invariants failed: ... ownership_violations=%d", ..., violations)
}
```

`RedeliveryCount` is **not** part of the invariants — it's informational and
expected to be > 0 under chaos.

### Prometheus metrics

Add two Counter metrics in `metrics/collector.go`:
- `simulation_redeliveries_total` — informational.
- `simulation_ownership_violations_total` — alertable.

Plus a `RecordOwnershipViolation()` and `RecordRedelivery()` method on the
Collector. Coordinator calls these when classifying.

## TDD plan

1. **TestRecordReceived_SameWorkerDuplicate_IsRedelivery**: receive seq=1
   from worker-A. Receive seq=1 again from worker-A. Assert
   `RedeliveryCount == 1`, `OwnershipViolationCount == 0`, and the
   returned error satisfies `errors.Is(err, ErrMessageRedelivery)` and
   `errors.As(&MessageRedeliveryEvent{})` with WorkerID="worker-A".
   (Redelivery is signaled as an event-as-error per the design above.)

2. **TestRecordReceived_DifferentWorkerDuplicate_IsOwnershipViolation**:
   receive seq=1 from worker-A. Receive seq=1 from worker-B. Assert
   `OwnershipViolationCount == 1`, `RedeliveryCount == 0`, error is
   `*MessageOwnershipViolationError` with `OriginalWorker="worker-A"`,
   `CurrentWorker="worker-B"`.

3. **TestRecordReceived_PrunedWorker_FallsBackToLegacyDuplicate**: set
   `workerCacheMax=2`, receive seqs 1,2,3 from worker-A. Receive seq=1 again
   (now pruned). Assert `DuplicateCount == 1`, no
   `OwnershipViolationCount` increment, error is `*MessageDuplicateError`.

4. **TestRecordReceived_GapHealRecordsWorker**: build a gap, escalate via
   AgeOut, then late-arrival heals it from worker-A. Assert the worker is
   stored for that seq. Receive same seq from worker-B → ownership violation.

5. **TestRecordReceived_OutOfOrderRecordsWorker**: receive seq=2 from
   worker-A (exercises **out-of-order branch** worker recording — this is
   what the test is targeting). Receive seq=1 from worker-B (closes the
   window via the contiguous-advance loop; window advances to 2). Now
   `lastReceived=2`. Receive seq=2 again from worker-C: classification
   path lookup finds `origWorker = "worker-A"` (set by the out-of-order
   branch); assert `*MessageOwnershipViolationError` with
   `OriginalWorker="worker-A"`, `CurrentWorker="worker-C"`. A broken
   implementation that omits worker-recording in the out-of-order branch
   would have `origWorker = ""` and the test would see a plain
   `*MessageDuplicateError` instead, failing the assertion. This is the
   targeted regression guard for the out-of-order branch.

6. **TestRecordReceived_OwnershipViolationFlowsToFailureReport**: end-to-end
   via Coordinator — inject a ReceivedMessage that triggers the violation
   path; assert the failure report (on TriggerFailure) contains the
   violation entry with both worker IDs.

7. **TestCheckpointRestore_EmptyWorkerID_FallsBackToLegacyDuplicate**:
   exercise the restore replay path (`checkpoint.go:170-183`) which
   passes `""` as workerID. Assert that a subsequent duplicate of a
   restored seq classifies as `*MessageDuplicateError` (legacy fallback),
   NOT as `*MessageOwnershipViolationError`. Pre-fix-of-P0 the
   pseudocode would have generated a false-positive violation; this
   test guards against that regression.

**Parent-fail expectation table** (verify-first per project memory):

| Test | Fails on parent commit? | Why |
|---|---|---|
| #1 same-worker redelivery | yes | parent has no RedeliveryCount; counts as DuplicateCount instead |
| #2 different-worker violation | **yes (critical)** | parent has no violation detection at all |
| #3 pruned fallback | n/a (constructive) | exercises new field; no parent equivalent |
| #4 gap-heal worker recording | yes | parent doesn't track worker on heal |
| #5 out-of-order worker recording | **yes (critical)** | parent's out-of-order branch never recorded workers |
| #6 violation in FailureReport | yes | FailureReport.OwnershipViolations field doesn't exist on parent |
| #7 empty-workerID fallback | n/a (constructive) | exercises new field |

## Risks

- **Risk 1 (signature change)**: every call site of `RecordReceived` must
  pass a workerID. Touch points: `coordinator.go:928`, `checkpoint.go:170-183`
  (restore loop), all tracker tests. The restore loop currently has no
  worker info — passes `""` (empty) which exercises the pruned-fallback
  path. Document that ownership-violation detection does not span restart;
  checkpoint completeness (Phase 3c) addresses this.

- **Risk 2 (gap-heal classification)**: the existing `gapEscalated` heal
  path is independent of the new worker-tracking. The plan inserts the
  worker recording **after** the gap-heal early return so heals are
  unaffected. Verified in test #4.

- **Risk 3 (memory growth on long stress runs)**: pruning caps
  per-partition map size. The `WorkerCacheMaxPerPartition` knob lets
  operators raise it for very long runs that should retain history.

- **Risk 4 (existing tests using seq=0)**: not relevant — Phase 1 already
  shifted them to seq=1. The signature change requires updating every
  existing tracker test to pass a workerID though; trivial sed.

## Verification gates

1. `make lint` clean.
2. `make test` clean.
3. `go test ./test/simulation/...` clean.
4. All 6 new TDD tests pass; the bug-exposing ones (#2, #5) fail on the
   parent commit (verify-first per project memory).
5. No regressions in existing tracker tests after the signature change.

## Commit message (draft)

```
feat(simulation): classify duplicates as redelivery vs ownership violation

The simulation tracker treated every late-arrival sequence as a generic
"duplicate". Under at-least-once delivery, JetStream legitimately
redelivers messages (worker crashed before ACK, NAK delays, etc.), so
DuplicateCount > 0 told the oracle nothing — masking the failure mode
the simulation is supposed to catch: two workers concurrently processing
the same (partition, seq), which is a Processing-Gate / handoff
regression.

Tracker now records the WorkerID that first processed each sequence
(bounded LRU per partition, cap 4096 by default) and on a later same-seq
receipt classifies:
  - same worker  → RedeliveryCount++  (informational, expected)
  - other worker → OwnershipViolationCount++ + ErrMessageOwnershipViolation
  - pruned       → DuplicateCount++   (legacy fallback)

OwnershipViolationCount > 0 is now a hard stability-invariant failure
in the main loop; the FailureReport lists each violation with both
worker IDs. RedeliveryCount is exposed but not gated on, since it
correlates with chaos intensity rather than parti correctness.

Adds Prometheus counters simulation_redeliveries_total and
simulation_ownership_violations_total.

Plan and review history: docs/plans/sim-oracle-phase3/
```
