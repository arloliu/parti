# Review Response — Round 1

Source: `tmp/sim-oracle-phase1_review.md` (2026-05-18, codex xhigh).

## P0 — C1 phantom holes (accepted, plan updated)

The reviewer's failure case is correct: receive seq=3, 4, 5 before seq=1, 2.
With the proposed `lastReceived=0` anchor alone, the seq=4 receipt's
range-fill loop iterates `[1, 4)` and adds seq=3 to `missingPerPartition`
(it's not yet in missing). Seq=3 then blocks the window-advance loop at
`tracker.go:265-276` even after seqs 1, 2 heal.

**Fix applied** in plan §C1:
- Added Change 2: range-fill skips `s` when `s <= oldHWM && !already-missing`.
- Hardened test `TestRecordReceived_FirstObservationIsGap` to assert the
  missing set is **exactly** {1, 2} and final `lastReceived == 5`.
- Added `TestRecordReceived_FirstObsGap_AgedOut` to exercise AgeOut and
  confirm no phantom-hole gap escalation.

## P1 — C4 main.go pre-create path (accepted, plan rewritten)

The reviewer is right that `cmd/simulation/main.go:227` pre-creates the
handoff bucket with `pc.KVBuckets.HandoffTTL = 0`, and `kvutil/bucket.go:48-53`
does not reconcile config for existing buckets. So the JetStream bucket
really *is* created with TTL=0, persisting until superseded — which is the
simulation's documented intent.

**Fix applied** in plan §C4:
- Rewrote the section to distinguish the two paths.
- `worker.go` deletion (dead code) confirmed as the only Phase 1 action.
- `main.go` left as-is, but the comment is tightened to make it clear that
  this is the load-bearing line.

## P1 — H6 test deadlock (accepted, tests rewritten)

The naive test (zero-buffer channel + blocking send + single goroutine)
deadlocks. Replaced with two concurrent tests:

1. `TestHandleAssignmentChanged_BlockingSendDelivers` — receive concurrently,
   assert delivery via `time.After(1s)` guard.
2. `TestHandleAssignmentChanged_CancellationDoesNotWedge` — cancel ctx,
   assert handler returns within 100ms.

Together these prove no-drop + no-wedge.

## Reviewer's additional tests (accepted)

- C2 yaml-validation test added.
- C2 defaulting-regression test added (omitted `ack_wait`, low `gap_aging`,
  must still fail validation).
- C1 AgeOut phantom-hole test added.
