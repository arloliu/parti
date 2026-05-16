# phase4_followup_gap3 Post-Implementation Review (v2)

## Summary

Both v1 P1 findings are resolved correctly. `IncClaimStaleHandoffReset` is now emitted only after `updateClaim` returns success and only when the successful transform invocation selected the stale-reset branch; the new CAS-conflict regression proves the prior overcount fails on `defd9ac`. The owned-by-other regression test no longer uses `time.Sleep` and now polls with a bounded `require.Eventually`. I found no new P0/P1/P2 issues; verdict: **merge**.

## Prior Finding Resolution Audit

| Prior finding | Status | Evidence | Judgement |
|---|---|---|---|
| P1 — Stale-handoff reset metric overcounts on CAS conflict | Resolved | `didReset` is declared inside each per-partition goroutine at `internal/assignment/handoff/twophase.go:239-249`, reset at the top of every transform invocation at `internal/assignment/handoff/twophase.go:249-251`, set only on the reset branch at `internal/assignment/handoff/twophase.go:292-309`, and the metric is emitted only after `updateClaim` returns nil at `internal/assignment/handoff/twophase.go:315-320`. No `IncClaimStaleHandoffReset` call remains inside the transform closure. | Correct, not bandaged. The flag is per PID closure scope, so parallel `errgroup` workers do not share it; CAS retry re-runs the transform and can flip the flag back to false before the post-success emission. |
| P1 — New owned-by-other regression test uses sleep-based async synchronization | Resolved | `twophase_stuck_prepare_test.go` has no `time.Sleep(...)` call; the owned-by-other test uses `require.Eventually` at `internal/assignment/handoff/twophase_stuck_prepare_test.go:391-406` with a 1s budget and 1ms poll interval, while `DelayAfterPrepare` is 200ms at `internal/assignment/handoff/twophase_stuck_prepare_test.go:358-370`. | Correct. The bounded polling replaces sleep-based synchronization and leaves enough observation window without hiding hangs; the existing `twophase_test.go` sleep remains out of scope per the prompt. |

## New Findings

None.

## Test Coverage Audit

| Test | Status | Evidence |
|---|---|---|
| `TestTwoPhase_PreparePhase_RecoversStuckPrepareOnReacquire` | Present-and-meaningful | Seeds stuck `(owner=A,pending=B,state=prepare)` at `internal/assignment/handoff/twophase_stuck_prepare_test.go:60-70`, runs re-acquire Apply at `internal/assignment/handoff/twophase_stuck_prepare_test.go:87-93`, and asserts stable/owner/pending/epoch at `internal/assignment/handoff/twophase_stuck_prepare_test.go:98-110`. |
| `TestTwoPhase_PreparePhase_ReacquireIdempotentOnAlreadyStable` | Present-and-meaningful | Seeds clean stable self-owned claim at `internal/assignment/handoff/twophase_stuck_prepare_test.go:128-137` and asserts no epoch bump after Apply at `internal/assignment/handoff/twophase_stuck_prepare_test.go:160-166`. |
| `TestTwoPhase_PreparePhase_RecoversFromStaleCommit` | Present-and-meaningful | Seeds self-owned commit state at `internal/assignment/handoff/twophase_stuck_prepare_test.go:183-192` and asserts stable owner A with epoch >= 4 at `internal/assignment/handoff/twophase_stuck_prepare_test.go:214-221`. |
| `TestTwoPhase_PreparePhase_RecoversFromStaleAbort` | Present-and-meaningful | Seeds self-owned abort state at `internal/assignment/handoff/twophase_stuck_prepare_test.go:237-246` and asserts stable owner A with epoch >= 4 at `internal/assignment/handoff/twophase_stuck_prepare_test.go:268-275`. |
| `TestTwoPhase_StaleHandoffResetMetric` | Present-and-meaningful | Wires `staleHandoffResetSpy` at `internal/assignment/handoff/twophase_stuck_prepare_test.go:304-317` and asserts exactly one reset metric for one non-contentious durable reset at `internal/assignment/handoff/twophase_stuck_prepare_test.go:325-328`. |
| `TestTwoPhase_PreparePhase_OwnedByOtherUnchangedSemantics` | Present-and-meaningful | Seeds stable other-owner claim at `internal/assignment/handoff/twophase_stuck_prepare_test.go:347-356`, observes prepare state via `require.Eventually` at `internal/assignment/handoff/twophase_stuck_prepare_test.go:391-406`, then asserts final stable owner A at `internal/assignment/handoff/twophase_stuck_prepare_test.go:419-423`. |
| `TestTwoPhase_StaleHandoffResetMetric_NoOvercountOnCASRetry` | Present-and-meaningful | `casConflictOnceStore` forces the first target `PutIfEpoch` to return `ErrEpochMismatch` at `internal/assignment/handoff/twophase_stuck_prepare_test.go:426-449`; the test verifies the conflict fired at `internal/assignment/handoff/twophase_stuck_prepare_test.go:512-516`, asserts exactly one metric at `internal/assignment/handoff/twophase_stuck_prepare_test.go:518-522`, and confirms the reset landed at `internal/assignment/handoff/twophase_stuck_prepare_test.go:524-532`. |

## Regression-test sensitivity verification

Yes. After copying the current `twophase_stuck_prepare_test.go` into a `defd9ac` worktree, `TestTwoPhase_StaleHandoffResetMetric_NoOvercountOnCASRetry` fails with the original metric overcount:

```text
--- FAIL: TestTwoPhase_StaleHandoffResetMetric_NoOvercountOnCASRetry (0.00s)
    twophase_stuck_prepare_test.go:520:
        	Error Trace:	/tmp/parti-gap3-v1/internal/assignment/handoff/twophase_stuck_prepare_test.go:520
        	Error:      	Not equal:
        	            	expected: 1
        	            	actual  : 2
        	Test:       	TestTwoPhase_StaleHandoffResetMetric_NoOvercountOnCASRetry
        	Messages:   	IncClaimStaleHandoffReset must be emitted exactly once per durable reset, even when updateClaim retries on CAS conflict
FAIL
FAIL	github.com/arloliu/parti/v2/internal/assignment/handoff	0.002s
FAIL
STATUS=1
```

## Lint / Build / Test Status

```text
===== make_lint =====
+ make lint
Checking golangci-lint version...
✓ golangci-lint 2.11.4 is installed
Running linters...
0 issues.
STATUS=0
```

```text
===== handoff_race_count3 =====
+ go test ./internal/assignment/handoff/... -race -count=3 -timeout 120s
ok  	github.com/arloliu/parti/v2/internal/assignment/handoff	1.659s
STATUS=0
```

```text
===== all_short_race =====
+ go test ./... -race -count=1 -short -timeout 300s
ok  	github.com/arloliu/parti/v2/test/simulation/internal/metrics	1.010s
?   	github.com/arloliu/parti/v2/test/simulation/internal/natsutil	[no test files]
?   	github.com/arloliu/parti/v2/test/simulation/internal/producer	[no test files]
ok  	github.com/arloliu/parti/v2/test/simulation/internal/worker	1.011s
ok  	github.com/arloliu/parti/v2/test/stress	1.010s
ok  	github.com/arloliu/parti/v2/types	1.007s
STATUS=0
```

```text
===== go_vet =====
+ go vet ./...
STATUS=0
```

```text
===== go_build =====
+ go build ./...
STATUS=0
```

## Verdict

**merge**. Zero P0, zero P1, zero P2.
