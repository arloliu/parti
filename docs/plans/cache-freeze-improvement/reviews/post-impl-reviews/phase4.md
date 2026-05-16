# Phase 4 Post-Implementation Review (v4)

## Summary

The v3 P0 is resolved: `applyAssignmentWithPrev` now runs `Apply -> LSR-advance -> Store -> Ack -> Hooks`, so a concurrent reader can no longer observe the dangerous `(new snapshot, old LSR)` pair. The LSR-before-Store move still satisfies the phase invariant because `handoffCoordinator.Apply` returning nil is the successful state-machine action after which LSR may advance. I found no new P0/P1/P2 issues introduced by commit `9c727d8`; the new old-snapshot/new-LSR interleaving is safe under `handleCommitValueOnce` cases (a)-(d). Validation passed, including `TestHandoffConflictStress` as part of the full race run, so this is merge-ready.

## Prior Finding Resolution Audit

| Finding | Status | Evidence |
|---|---|---|
| v3 P0 — centralized LSR update leaves a Store->LSR stale-fence race | resolved | The pipeline now applies first (`manager_assignment.go:750-756`), advances LSR before publication (`manager_assignment.go:758-771`), stores the snapshot only after LSR is advanced (`manager_assignment.go:773-779`), publishes the heartbeat ack (`manager_assignment.go:789-803`), then invokes production metrics/hooks (`manager_assignment.go:805-807`). This is exactly `Apply -> LSR -> Store -> Ack -> Hooks` for production behavior; the only extra step is the nil-default test hook immediately after Store. |
| v3 test bar — deterministic post-Store hook proves LSR is already advanced | resolved | `testHookAfterApplyStore` is unexported and nil-default on `Manager` (`manager.go:146-151`). `applyAssignmentWithPrev` invokes it synchronously immediately after `m.assignment.Store(newAssignment)` (`manager_assignment.go:773-779`). The test installs the hook in the in-package `parti` test file (`manager_commit_state_machine_test.go:1`, `manager_commit_state_machine_test.go:814-824`) and asserts the hook captured `lastSeenLeaderRevision >= newAssignment.LeaderRevision` plus the just-stored snapshot (`manager_commit_state_machine_test.go:831-841`). A repo search found the only assignment to this field in `manager_commit_state_machine_test.go:814`; the only production-code touch is the nil-guarded invocation at `manager_assignment.go:777-779`, so it does not leak to public API. |
| v3 test bar — concurrent reader does not see `(Version=new, LSR<new.LR)` | resolved | The second subtest starts a concurrent tight-loop reader before applying (`manager_commit_state_machine_test.go:854-878`), runs 50 `applyAssignment` calls (`manager_commit_state_machine_test.go:879-889`), and asserts zero bad observations where `cur.Version == newAsgn.Version && lsr < newAsgn.LeaderRevision` (`manager_commit_state_machine_test.go:896-897`). |
| LSR-before-Store invariant vs phase plan | resolved | The phase plan requires LSR to advance only after a successful state-machine action (`partition_assignment_phase4_implementation_plan.md:11-14`). `applyAssignmentWithPrev` advances LSR only after `handoffCoordinator.Apply` returns nil (`manager_assignment.go:750-771`), and its comment explicitly records that this remains within the plan invariant (`manager_assignment.go:700-708`). |
| New old-snapshot/new-LSR interleaving | safe | `handleCommitValueOnce` reads the current snapshot before LSR (`manager_assignment.go:516-534`). With old snapshot and new LSR, case (a) does not no-op unless `commit.Version <= cur.Version`; if it does, it only publishes a no-op observation and max-advances LSR (`manager_assignment.go:519-527`). Case (b) rejects only `commit.LeaderRevision < lastSeen` (`manager_assignment.go:530-536`), which is correct once LSR has advanced to a successfully applied leader revision. Cases (c)/(d) continue through unconditional apply construction once not rejected (`manager_assignment.go:539-581`), so the old snapshot does not create an incorrect apply/skip decision. |
| Retry coalescing with concurrent fresh watcher | safe | Retry success calls `m.applyAssignment(*pending)` (`manager_assignment.go:883-899`), so it reaches the same centralized LSR-before-Store pipeline. Concurrent LSR advances are monotone because `updateLastSeenLeaderRevision` is a CAS-loop max that ignores lower/equal revisions (`manager_assignment.go:958-974`). |
| Cold-empty bootstrap bypass | unchanged and correct | The cold-empty path intentionally bypasses `applyAssignmentWithPrev`, stores `Assignment{}`, publishes an explicit empty ack, and leaves `LeaderRevision`/LSR at zero because no leader revision has been accepted (`manager.go:520-549`). That is outside the v3 P0 ordering because there is no successful leader state-machine action to fence. |
| v2 P0 — apply-retry success does not advance LSR | remains resolved | The retry goroutine calls `m.applyAssignment(*pending)` on each pending retry (`manager_assignment.go:883-899`), and `applyAssignment` centralizes LSR advancement before Store (`manager_assignment.go:681-723`, `manager_assignment.go:758-771`). `TestApplyRetry_SuccessAdvancesLSR` waits for retry success, asserts LSR is 100, then proves a stale V=8/LR=50 commit is rejected (`manager_commit_state_machine_test.go:730-776`). |
| v2 P1/P2 — reconcile pre-tick assertion and global mutation | remain resolved | `TestMonitorCommitChanges_PeriodicReconcile_RecoversDivergence` uses a 1s interval, does not run in parallel while mutating `commitReconcileInterval`, asserts V=5 before the first tick, then waits for reconcile recovery to V=15 (`manager_commit_watcher_test.go:238-300`). |
| v1 P0 #1 — `EnableTwoPhaseHandoff` wiring | remains resolved | `startCalculator` copies `m.cfg.EnableTwoPhaseHandoff` into `assignment.Config` (`manager_assignment.go:102-121`), and tests assert both true propagation and false default on a real manager-created calculator (`manager_audit_wireup_test.go:18-98`). |
| v1 P0 #2 — cold empty bootstrap ack before stable | remains resolved | Startup runs `applyInitialAssignment` before `StateStable` (`manager.go:444-460`). The cold-empty branch stores empty, calls `SetAppliedAssignment`, and calls `PublishNow` before returning (`manager.go:520-549`), with direct unit coverage (`manager_apply_assignment_test.go:64-103`). |
| v1 P0 #3 — LSR advances before successful apply | remains resolved | Commit and alias paths document that LSR advances only inside `applyAssignmentWithPrev` after success (`manager_assignment.go:389-397`, `manager_assignment.go:575-581`); initial commit bootstrap calls `applyAssignmentWithPrev` before publishing `lastObservedCommit` (`manager.go:500-512`). Failure tests assert LSR and snapshot remain unchanged after commit-path and alias-path Apply failures (`manager_commit_state_machine_test.go:535-611`). |
| v1 P1 #4 — strict gzip | remains resolved | `FetchAndVerifyCommitPayload` returns `ErrCommitPayloadDecompress` immediately on gzip failure (`internal/assignment/commit_payload_fetch.go:73-83`), and the decompression regression test proves uncompressed bytes with a matching raw hash do not apply or advance LSR (`manager_commit_state_machine_test.go:627-675`). |

## New Findings

None.

## Lint / Build / Test Status

```text
### make lint
Checking golangci-lint version...
✓ golangci-lint 2.11.4 is installed
Running linters...
0 issues.

### go test ./... -race -count=1
ok  	github.com/arloliu/parti/v2	13.459s
ok  	github.com/arloliu/parti/v2/internal/assignment	14.537s
ok  	github.com/arloliu/parti/v2/test/integration/consumer	76.508s
ok  	github.com/arloliu/parti/v2/test/integration/failure	52.237s
ok  	github.com/arloliu/parti/v2/test/integration/handoff	12.632s
ok  	github.com/arloliu/parti/v2/test/integration/manager	87.740s
ok  	github.com/arloliu/parti/v2/test/stress	7.074s
ok  	github.com/arloliu/parti/v2/types	1.007s

### go vet ./...
(no output)
```

All requested commands exited 0. The prior `TestHandoffConflictStress` flake did not reproduce in this run; the full `test/integration/handoff` package passed under race.

## Verdict

**merge**. There are no P0 or P1 findings, no new issues from the v4 fix, and the v3 P0 ordering race is closed.
