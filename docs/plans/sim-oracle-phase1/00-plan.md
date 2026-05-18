# Simulation Oracle — Phase 1 Bug Fixes

Single-PR plan covering five small, independent fixes to the simulation's
oracle correctness and observability. Derived from `tmp/sim_audit/SUMMARY.md`
(specifically items **C1**, **C2**, **C3**, **C4**, and **H6**).

## Why bundle these

All five are:
- Scoped to `test/simulation/` (one exception: a single deletion in the same
  area in `test/simulation/internal/worker/worker.go`).
- Independent — no shared state, no ordering between fixes.
- Small (each ≤ ~30 LOC).
- High-confidence — each has a concrete reproducer or a deterministic test.

Bundling them avoids 5 separate review cycles for fixes that are all in the
"oracle correctness" theme.

## Out of scope

- Migration of `worker.go` / `metrics/collector.go` from `internal/durable` to
  the public `consumer/` package. This is Phase 2 (separate PR).
- Stronger oracle invariants (per-message ownership audit, heartbeat watcher).
  Phase 3.
- CI expansion. Phase 4.
- Any feature work beyond the five listed fixes.

## Fixes

### C1 — Tracker first-observation init swallows pre-seq losses + misclassifies out-of-order seq=1 as duplicate

**File:** `test/simulation/internal/coordinator/tracker.go:164-178`

**Current behavior:** On the first `RecordReceived` for a partition, the code
unconditionally seeds `lastReceivedPerPartition[partitionID] = partitionSeq`
without checking that `partitionSeq == 1`. Two failure modes:

1. **False negative**: if producer's seq=1..N-1 are lost (e.g. worker crashed
   before the reporting buffer existed), and the first message the tracker ever
   sees for that partition is seq=N (N>1), seqs 1..N-1 are never recorded as
   missing → never aged out → never counted as gaps. **The oracle cannot
   detect loss of the first message(s) of any partition.**
2. **False positive**: if seq=2 arrives before seq=1 at startup (legitimate
   out-of-order delivery — JetStream does not guarantee ordering across
   consumer subscription boundaries), `lastReceived=2` is seeded. When seq=1
   then arrives, it hits the `partitionSeq <= lastReceived` branch and is
   reported as `MessageDuplicateError`.

**Fix:** Seed `lastReceivedPerPartition = 0` on first observation and fall
through to the regular gap/duplicate detection logic. The regular logic:
- If `partitionSeq == 1` (expected): advance contiguous window by one. ✓
- If `partitionSeq > 1`: enter out-of-order branch, add `[1..partSeq-1]` to
  `missingPerPartition`. ✓
- If `partitionSeq <= 0`: impossible — producer starts at 1 (`producer.go:60-63,
  136-137`).

**Implementation sketch — two coordinated changes:**

**Change 1: first-observation init.** Seed `lastReceived=0` and fall through.

```go
lastReceived, exists := t.lastReceivedPerPartition[partitionID]
if !exists {
    // Anchor at 0; let the regular path classify partitionSeq.
    lastReceived = 0
    t.lastReceivedPerPartition[partitionID] = 0
    if _, ok := t.missingPerPartition[partitionID]; !ok {
        t.missingPerPartition[partitionID] = make(map[int64]time.Time)
    }
    // No early return — fall through.
}
```

**Change 2: range-fill must not re-add already-observed sequences.**
Reviewer P0: without this, receiving seq=3, 4, 5 (in that order) with
`lastReceived=0` would re-enter the out-of-order branch for each, and the
range-fill loop at `tracker.go:198-203` would add seq=3 to `missingPerPartition`
on the seq=4 receipt (because it iterates `[1, 4)` and seq=3 is not yet in
missing). Seq=3 is then a phantom hole that blocks the window-advance loop at
`tracker.go:265-276` (which stops at the first missing key) and is escalatable
to a false gap by AgeOut.

Fix the range-fill (`tracker.go:198-203`):

```go
miss := t.missingPerPartition[partitionID]
now := time.Now()
hwm := t.highWatermarkPerPartition[partitionID]
for s := expectedSeq; s < partitionSeq; s++ {
    if _, present := miss[s]; present {
        continue // already tracked as missing
    }
    if s <= hwm {
        continue // already physically observed out-of-order
    }
    miss[s] = now
}
```

The `s <= hwm` guard is the load-bearing addition. The invariant: a sequence
`s` such that `s <= hwm` and `s` is not in `missing` must have been physically
observed out-of-order (the window-advance loop relies on the same invariant
in the opposite direction).

This guard is a no-op for the pre-fix scenario where the first observation
seeded `lastReceived = firstSeq` — because then `expectedSeq = firstSeq + 1
> hwm` and the loop body was previously unreachable for `s <= hwm`. So the
guard only changes behavior in the new `lastReceived=0` path.

**One subtle correctness point:** the existing massive-gap fast-forward branch
(`gapSize > 10000`) at `tracker.go:213-220` will fire for any first observation
with `partitionSeq > 10001`. That preserves current behavior (avoid OOM). Not
changed in this fix.

**Tests to add** in `tracker_test.go`:

1. `TestRecordReceived_FirstSeqOutOfOrder`: send seq=2 then seq=1; assert seq=1
   is **not** classified as duplicate; assert `lastReceivedPerPartition == 2`,
   `holesHealedCount == 1`, no `MessageDuplicateError`.

2. `TestRecordReceived_FirstObservationIsGap`: receive seq=3 then 4 then 5 (in
   that order; before any of 1, 2 arrive). Assert:
   - `missingPerPartition[pid]` is **exactly** `{1, 2}` (not `{1, 2, 3}` or
     `{1, 2, 3, 4}`).
   - `highWatermarkPerPartition[pid] == 5`.
   - `physicalReceivedCount == 3`.
   - `GapCount == 0` (still in aging window).
   Then receive seq=1 and seq=2; assert:
   - `lastReceivedPerPartition[pid] == 5` (full contiguous advance through 3,
     4, 5 via window-advance loop).
   - `holesHealedCount == 2`.
   - `GapCount == 0`, `DuplicateCount == 0`.

3. `TestRecordReceived_FirstSeqIsOne` (regression): send seq=1 first; assert
   `lastReceivedPerPartition[partitionID] == 1`, `physicalReceivedCount == 1`,
   no error.

4. `TestRecordReceived_FirstObsGap_AgedOut`: receive seq=3, 4, 5 (as above),
   call `AgeOut(now + GapAging)`, assert exactly seqs 1 and 2 are escalated to
   gaps (seqs 3, 4, 5 not escalated, despite never having been "contiguously
   advanced past" when AgeOut fires). This proves the phantom-hole regression
   from the reviewer.

### C2 — Add `AckWait < GapAging` invariant check at config validation

**Files:**
- `test/simulation/internal/worker/worker.go:258` (AckWait currently hardcoded)
- `test/simulation/internal/config/config.go` (new field)
- `test/simulation/internal/config/defaults.go` (default 30s)
- `test/simulation/internal/config/validation.go` (cross-check)
- `test/simulation/configs/stress-short-aggressive.yaml:51`
- `test/simulation/configs/stress-5m-aggressive.yaml:51`
- `test/simulation/configs/stress-short-quick.yaml:51`

**Current behavior:** Worker hardcodes `AckWait: 30s`. Three configs set
`gap_aging` ≤ 30s:
- `stress-5m-aggressive.yaml`: gap_aging=20s ← violation
- `stress-short-aggressive.yaml`: gap_aging=20s ← violation
- `stress-short-quick.yaml`: gap_aging=30s ← racy (equal)

When `AckWait >= GapAging`, JetStream's redelivery window for a worker-crashed
message exceeds the coordinator's hole-escalation window. The coordinator
escalates the hole to a confirmed gap BEFORE JetStream redelivers it. With
`--stop-on-failure` enabled, this kills the simulation on a hole that would
have healed.

**Fix design (two parts):**

1. **Make AckWait configurable** on the simulation side. Add
   `Workers.AckWait time.Duration` with default 30s. Replace the hardcoded
   `AckWait: 30 * time.Second` in `worker.go:258` with `cfg.AckWait`. Plumb
   through `worker.Config`.

2. **Add validation** in `validation.go`:
   ```go
   if cfg.Workers.AckWait >= cfg.Coordinator.GapAging {
       return fmt.Errorf("workers.ack_wait (%s) must be < coordinator.gap_aging (%s) "+
           "to avoid false-positive gap escalations during JetStream redelivery",
           cfg.Workers.AckWait, cfg.Coordinator.GapAging)
   }
   ```

3. **Fix the three offending configs**: lower their `workers.ack_wait` to
   preserve their fast-detection intent rather than relaxing gap_aging.
   - stress-5m-aggressive: ack_wait=10s (gap_aging stays 20s)
   - stress-short-aggressive: ack_wait=10s (gap_aging stays 20s)
   - stress-short-quick: ack_wait=15s (gap_aging stays 30s)

**Tests to add (expanded per reviewer):**

1. In `config/validation_test.go` (create if missing):
   - `TestValidate_AckWaitGteGapAging_Rejects`: build a minimal valid config,
     set `Workers.AckWait = 30s` and `Coordinator.GapAging = 30s`; assert
     validation fails with a message naming both fields. Repeat for
     `AckWait = 30s, GapAging = 20s` (strict inequality).
   - `TestValidate_AckWaitLtGapAging_Passes`: `AckWait = 10s,
     GapAging = 30s`; assert no validation error.
   - `TestValidate_AckWaitOmitted_AppliesDefault`: leave `Workers.AckWait`
     unset (zero), call `applyDefaults` then `validateConfig`. The defaulted
     value (30s) must be visible to validation. If `GapAging` is set to 20s
     in this test, validation must fail — proving B2 cannot be reintroduced
     by an omitted field.

2. `TestAllShippedConfigsValidate`: iterate every file in
   `test/simulation/configs/*.yaml`, load + apply defaults + validate. Assert
   no error for any file. This catches yaml drift introduced by the AckWait
   addition and verifies `chaos_comprehensive.yaml` (the only CI config) is
   not affected — its `gap_aging: 240s` is well above the default 30s
   AckWait.

### C3 — Trace visualizer JSON tags don't match coordinator's output

**File:** `scripts/trace_visualizer/main.go:23-28`

**Current behavior:** Visualizer's `MessageGapError` uses UpperCamelCase JSON
tags (`PartitionID`, `ExpectedSeq`, …). The coordinator writes snake_case
(`tracker.go:18-23`). Every field decodes to zero; partition filter targets
partition 0; output is misleading or empty.

**Fix:** Change the visualizer's JSON tags to snake_case to match
`coordinator.MessageGapError`. Single-file change.

```go
type MessageGapError struct {
    PartitionID int   `json:"partition_id"`
    ExpectedSeq int64 `json:"expected_seq"`
    ReceivedSeq int64 `json:"received_seq"`
    LastSent    int64 `json:"last_sent"`
}
```

**Tests to add:** A small `scripts/trace_visualizer/main_test.go` (or co-located
package test) that JSON-decodes a minimal coordinator-shape report and asserts
the fields are populated. Coordinator's `FailureReport` is already snake_case
(written via `json.MarshalIndent` to `failure_report.json`), so the test fixture
can be a small JSON blob inline.

### C4 — `worker.go` `HandoffTTL = 0` is dead code; load-bearing path is in `main.go`

**Files:**
- `test/simulation/internal/worker/worker.go:198-206` (dead code — delete)
- `test/simulation/cmd/simulation/main.go:222-227` (load-bearing — clarify
  comment, do not delete)

**The full picture** (post-reviewer correction):

The simulation has two places that touch `KVBuckets.HandoffTTL`:

1. **`worker.go:198-206`** — sets `partiCfg.KVBuckets.HandoffTTL = 0` on the
   `parti.Config` passed to `parti.NewManager`. This is a **silent no-op**:
   `NewManager` calls `parti.SetDefaults` which re-applies the struct-tag
   default `2m` over the zero value (`config.go:421-426`, empirically
   verified). The manager runs with `HandoffTTL = 2m` in memory, and
   `Config.Validate()` (`config.go:515-522`) sees `2m` and passes. This
   line of code does nothing.

2. **`main.go:222-227`** — pre-creates the `parti-handoff` JetStream KV bucket
   with TTL=0 via `kvutil.EnsureKVBucket(... b.ttl)` at line 241. **This is the
   load-bearing path.** `kvutil/bucket.go:48-53` opens an existing bucket
   without reconciling its config, so the bucket's storage-level TTL stays at
   what main.go created (= 0, i.e. never expires). Manager startup later calls
   `setupHandoff` with the defaulted-to-2m in-memory `cfg.KVBuckets.HandoffTTL`,
   but the existing bucket is opened as-is — the 2m hint is never written to
   JetStream.

Net runtime state: the bucket really *does* have TTL=0 (claims persist until
superseded), driven entirely by `main.go`. `worker.go`'s assignment is dead.

**Fix:** Delete the `worker.go` `HandoffTTL = 0` line and the misleading
multi-line comment that suggests it controls anything. Keep
`EnableTwoPhaseHandoff = cfg.EnforceExclusiveConsumption`. In `main.go`, leave
the pre-create behavior untouched (it's the actual intent) but tighten the
comment to make it clear that **this is the line that disables JetStream KV
TTL**, and that the manager-level `HandoffTTL` config field is purely advisory
once the bucket exists.

**Why we are not "fixing" main.go to align with manager config:** the
simulation's documented intent (per the comment author and per the audit
itself) is to have non-expiring handoff claims for correctness under long
runs. Aligning the simulation manager config to 2m would alter behavior; that
is a Phase 4 question, not Phase 1.

**Tests to add:**
None. C4 is a code-clarity fix only. Adding an integration test that asserts
the bucket's effective TTL would require constructing the simulation's
embedded NATS server and calling `js.KeyValueStatus(ctx, "parti-handoff")` —
disproportionate for a comment cleanup. If a future fix changes the
runtime behavior, the test can be added then.

### H6 — `startLatencyCh` non-blocking send silently drops samples

**File:** `test/simulation/internal/worker/worker.go:427-434`

**Current behavior:**
```go
select {
case w.startLatencyCh <- coordinator.StartLatencyReport{...}:
default:
    log.Printf("[%s] startLatencyCh full, dropping first-assignment latency=%s", ...)
}
```

The channel has buffer 256 (`coordinator.go:180`). `SlowStartExceedances()`
gates pass/fail in the final report (`cmd/simulation/main.go:706-709`). Under
chaos bursts where many workers fire `OnAssignmentChanged` simultaneously and
the assignment-processing goroutine is slow, samples can be dropped — silently
hiding regressions of the slow-start invariant the channel exists to enforce.

**Fix:** Replace the non-blocking select with a blocking send that respects
`ctx.Done()`:
```go
select {
case <-ctx.Done():
case w.startLatencyCh <- coordinator.StartLatencyReport{
    WorkerID: w.id,
    Latency:  latency,
}:
}
```

This matches the symmetric pattern already in use for `assignmentReportCh`
at `worker.go:450-453`. The blocking send is safe because the hook itself runs
in the manager's goroutine and is allowed to be slow; back-pressure is
acceptable.

**Tests to add (revised per reviewer P1 — the naive test deadlocks):**

The proposed blocking send with a zero-buffer channel and a single-threaded
test will deadlock by construction. Specify two concurrent tests with
timeouts.

1. `TestHandleAssignmentChanged_BlockingSendDelivers`: zero-buffer
   `startLatencyCh`. Receive in the test goroutine; call
   `handleAssignmentChanged` in a separate goroutine. Use a `time.After(1s)`
   guard to fail if the receive does not complete. Assert exactly one
   `StartLatencyReport` arrives with the expected `WorkerID`.

2. `TestHandleAssignmentChanged_CancellationDoesNotWedge`: zero-buffer
   `startLatencyCh`, no receiver. Cancel `w.ctx` *before* calling
   `handleAssignmentChanged`. Assert the call returns within a short timeout
   (e.g. 100ms) — proving the new blocking send respects `ctx.Done()` and
   does not wedge shutdown.

Together these prove (1) no-drop on slow drainer, and (2) no wedge on
shutdown.

## Implementation order

Independent fixes; any order works. Suggested for minimal cognitive load:

1. **C4** — pure deletion, 5 lines.
2. **C3** — pure JSON tag rename, 4 lines + test fixture.
3. **H6** — replace `default:` with `case <-ctx.Done():`, 3 lines + test.
4. **C2** — config plumbing + validation + 3 yaml updates, ~30 LOC + test.
5. **C1** — tracker first-observation fix + 3 tracker tests, ~10 LOC.

## Verification

Before declaring complete:
- `make lint` clean.
- `make test` clean (full unit suite).
- `go test ./test/simulation/...` clean.
- New tests fail on the parent commit and pass on the fix (verify-first per
  memory `feedback_verify_first_with_reproducer`).
- `go build ./test/simulation/...` clean.

## Risks and non-risks

- **Risk (C1):** The fall-through path now executes `physicalReceivedCount++`
  in different branches depending on whether partitionSeq is 1 (contiguous)
  or > 1 (out-of-order). Current code only counted once on first observation.
  Audit confirms both new paths increment physicalReceivedCount exactly once
  per call, so the count remains correct.
- **Risk (C2):** Lowering AckWait in aggressive configs trades JetStream
  redelivery latency for faster oracle escalation. Configs are aggressive by
  design; this is the intent.
- **Non-risk (H6):** The blocking send back-pressures only the
  OnAssignmentChanged hook. The manager's hook callsite already tolerates
  arbitrary hook duration; nothing else in the worker depends on the hook
  returning quickly.

## Commit message (draft)

```
fix(simulation): correct oracle false positives/negatives and clean up dead config code

Five small bugs in test/simulation/ surfaced by an oracle-correctness audit:

- tracker: first-observation init swallowed pre-seq losses and misclassified
  out-of-order seq=1 as a duplicate. Seed lastReceived=0 so the regular
  out-of-order path classifies the first observation correctly.

- config: add AckWait < GapAging cross-validation. Three aggressive configs
  violated the invariant by setting gap_aging < hardcoded AckWait=30s,
  causing false-positive gap escalations under chaos. Make AckWait
  configurable; fix the offending configs to preserve their aggressive intent.

- scripts/trace_visualizer: JSON tags were UpperCamelCase but coordinator
  writes snake_case. All fields decoded to zero; partition filter always
  targeted partition 0. Visualizer is now functional.

- worker: HandoffTTL=0 was a silent no-op (parti.SetDefaults re-applies the
  2m default). Delete the misleading code; the simulation has always run with
  the 2m default.

- worker: startLatencyCh non-blocking send silently dropped samples under
  chaos bursts, blinding the SlowStartExceedances invariant. Replace with
  a blocking send that respects ctx.Done(), matching the pattern already
  used for assignmentReportCh.
```
