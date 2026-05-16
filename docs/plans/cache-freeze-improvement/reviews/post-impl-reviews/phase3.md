# Phase 3 Post-Implementation Review (v2)

## Summary

The v1 P0/P1 fixes are correct: publisher leadership fences now call a live election verifier, GC is protected against in-flight payload refs, GC lifecycle is wired into the calculator, and the previously degenerate tests were replaced with meaningful coverage. I found no new P0/P1 issues introduced by commit `8418d96`. The `live revision >= claimed term revision + value-prefix` leadership check is a justified spec refinement because renewals update the KV key and advance its live revision while the term revision remains stable. Ready to merge; note one unrelated `source` race-test flake occurred on the first full run and passed on targeted and full rerun.

## Prior Finding Resolution Audit

| Finding | Status | Evidence |
| --- | --- | --- |
| P0-1 live leadership fence | Resolved | Publisher fences call `checkLeadership(ctx, in.LeaderRevision, ...)` before aliases and after aliases (`internal/assignment/assignment_publisher.go:316`, `internal/assignment/assignment_publisher.go:329`). The callback invokes `leaderCheckFn` and preserves both phase sentinel and underlying mismatch (`internal/assignment/assignment_publisher.go:498`). Production manager wires `LeaderCheck: m.checkElectionLeadership` (`manager_assignment.go:105`), which calls election `CheckLeadership` when available (`manager_election.go:260`). `NATSElection.CheckLeadership` performs a live `kv.Get` (`internal/election/nats_election.go:351`), rejects absent keys (`internal/election/nats_election.go:352`), rejects live revision below claim (`internal/election/nats_election.go:358`), and rejects values not prefixed by the local worker ID (`internal/election/nats_election.go:364`). Renewals use `kv.Update` and store the returned live revision (`internal/election/nats_election.go:175`, `internal/election/nats_election.go:182`, `internal/election/nats_election.go:192`), while `Revision()` returns stable `termRevision` (`internal/election/nats_election.go:285`), so `live >= claimed` is necessary and correct for same-term renewals. |
| P0-2 GC vs in-flight publish race | Resolved | Publisher registers all refs immediately after payload create/verify and before step 5 (`internal/assignment/assignment_publisher.go:299`), and clears them with `defer` on every return path after step 9 success/failure or earlier abort (`internal/assignment/assignment_publisher.go:310`). `LiveRefs` snapshots `sync.Map` safely (`internal/assignment/assignment_publisher.go:922`). GC unions `LiveRefs` into the live set before listing/deleting (`internal/assignment/commit_gc.go:255`) and re-checks `LiveRefs` immediately before each delete (`internal/assignment/commit_gc.go:295`), closing the listed race window. |
| P1-1 GC lifecycle wiring | Resolved | Calculator constructs `CommitGC` and wires publisher trigger (`internal/assignment/calculator.go:142`, `internal/assignment/calculator.go:147`), starts GC during `Calculator.Start` (`internal/assignment/calculator.go:263`), and stops/drains it during `Calculator.Stop` (`internal/assignment/calculator.go:332`). `CommitGC.Stop` is repeat-safe and waits for `doneCh` (`internal/assignment/commit_gc.go:183`); `Trigger` is buffered/non-blocking and drops when full (`internal/assignment/commit_gc.go:200`). `Start` rejects duplicate starts (`internal/assignment/commit_gc.go:166`), and calculator-level `Start` is already CAS-guarded (`internal/assignment/calculator.go:199`). |
| P1-2 alias-barrier failure test | Resolved | `putFailingKV.Put` fails only the exact legacy alias key and leaves payload `Create` unaffected (`internal/assignment/assignment_publisher_v1_review_test.go:245`). The replacement test asserts `ErrAliasBarrierFailed` (`internal/assignment/assignment_publisher_v1_review_test.go:310`), alias-barrier metrics (`internal/assignment/assignment_publisher_v1_review_test.go:316`), no `assignment._commit` (`internal/assignment/assignment_publisher_v1_review_test.go:321`), and `commit_aborts == 0` (`internal/assignment/assignment_publisher_v1_review_test.go:318`). The old cancelled-context half is removed and documented as moved (`internal/assignment/assignment_publisher_test.go:691`). |
| P1-3 heartbeat default-classification tests | Resolved | Missing heartbeat test publishes successfully then requires `assignment.no-hb-w` alias (`internal/assignment/assignment_publisher_v1_review_test.go:338`). Malformed heartbeat test writes non-JSON/non-RFC3339 bytes and requires `assignment.bad-hb-w` alias (`internal/assignment/assignment_publisher_v1_review_test.go:359`). Both exercise `classifyLegacyWorkers`, whose read/decode failures append workers to legacy (`internal/assignment/assignment_publisher.go:605`). |
| P1-4 #61 legacy heartbeat invariant | Resolved | New test fetches `heartbeat.old-w`, decodes via `types.DecodeHeartbeat`, and asserts `Capabilities == 0` and `AppliedVersion != proposedV` (`internal/assignment/assignment_publisher_v1_review_test.go:414`). |
| P2 alias_visible_uncommitted overcount | Resolved | `runAliasBarrier` returns `aliasWritten`, sets it only after `writeLegacyAliasWithRetry` succeeds (`internal/assignment/assignment_publisher.go:650`, `internal/assignment/assignment_publisher.go:671`), and all exposure increments are gated on it for barrier failure, post-alias leadership loss, and commit CAS failure (`internal/assignment/assignment_publisher.go:665`, `internal/assignment/assignment_publisher.go:336`, `internal/assignment/assignment_publisher.go:380`). |

## Spec Compliance (delta from v1)

| Spec section | v2 status | Evidence |
| --- | --- | --- |
| §3.5 step 5 / step 7 leadership fences | Compliant after v2 | The plan now documents the renewal-aware `live >= R` + worker-name check (`docs/plans/cache-freeze-improvement/00-original-plan.md:1130`). Production call chain is publisher -> `LeaderCheckFn` -> manager -> `NATSElection.CheckLeadership` -> live `kv.Get` (`internal/assignment/assignment_publisher.go:498`, `manager_election.go:260`, `internal/election/nats_election.go:351`). |
| §3.5 step 12 GC | Compliant after v2 | Publisher triggers GC after successful commit-side hygiene (`internal/assignment/assignment_publisher.go:415`), and calculator starts/stops the background loop (`internal/assignment/calculator.go:263`, `internal/assignment/calculator.go:332`). |
| §3.9 GC under concurrent publish | Compliant after v2 | `inflightRefs` registration happens before step 5 and clears via defer (`internal/assignment/assignment_publisher.go:299`); GC consults live refs in both the initial live union and the immediate pre-delete re-check (`internal/assignment/commit_gc.go:255`, `internal/assignment/commit_gc.go:295`). |
| §3.10 exposure metrics | Compliant after v2 | `alias_visible_uncommitted` now fires only after at least one alias write has succeeded (`internal/assignment/assignment_publisher.go:637`). |
| Unchanged rows from v1 | Still compliant per v1 | §3.1, §3.2, §3.3, §3.5 steps 1-4/6/8-11, §3.6 publisher-scope commit shape, §3.7, §3.8, and §3.10 core metrics remain as reviewed in v1. |

## Findings

None.

## Test Coverage Audit

| Test | Status | Evidence |
| --- | --- | --- |
| `TestPublisher_LeadershipFence_LiveElectionKV_AbortsWhenLiveRevisionMismatches` | Present-and-meaningful | Wires real `NATSElection.CheckLeadership`, simulates takeover in KV, and asserts pre-alias abort preserves both sentinels (`internal/assignment/assignment_publisher_v1_review_test.go:42`). |
| `TestNATSElection_CheckLeadership_LiveKVRevision` | Present-and-meaningful | Verifies happy path, wrong revision, zero claim, absent leader key, and new leader rejection (`internal/election/nats_election_test.go:423`). |
| `TestCommitGC_DoesNotDeletePayloadAdoptedByInFlightPublish` | Present-and-meaningful | Stages an old orphan payload in `LiveRefs`, confirms GC preserves it, then drops the ref and confirms GC reaps it (`internal/assignment/assignment_publisher_v1_review_test.go:121`). |
| `TestCommitGC_PublisherAdoptedPayloadVisibleViaLiveRefs` | Present-and-meaningful | Confirms production publisher `LiveRefs` snapshots `inflightRefs` (`internal/assignment/assignment_publisher_v1_review_test.go:172`). |
| `TestCommitGC_LifecycleStartStop` | Present-and-meaningful | Starts GC, sends repeated triggers, stops with a bounded wait, and calls `Stop` again (`internal/assignment/assignment_publisher_v1_review_test.go:206`). |
| `TestRollingUpgrade_AliasBarrier_FailureAbortsBeforeCommit_NotAtPayloadCreate` | Present-and-meaningful | Fails only legacy alias `Put` and asserts sentinel, metrics, no commit, no commit abort, and no exposure metric (`internal/assignment/assignment_publisher_v1_review_test.go:268`). |
| `TestPublisher_LegacyAliasBarrier_MissingHeartbeatTreatedAsLegacy` | Present-and-meaningful | Missing heartbeat produces mandatory legacy alias (`internal/assignment/assignment_publisher_v1_review_test.go:338`). |
| `TestPublisher_LegacyAliasBarrier_MalformedHeartbeatTreatedAsLegacy` | Present-and-meaningful | Malformed heartbeat produces mandatory legacy alias (`internal/assignment/assignment_publisher_v1_review_test.go:359`). |
| `TestPublisher_AliasBarrier_CASFailure_LegacyHeartbeatHasNoAppliedVersion` | Present-and-meaningful | Decodes legacy heartbeat and asserts no capability / no applied version for failed V (`internal/assignment/assignment_publisher_v1_review_test.go:387`). |
| Modified old #48 test | Present-and-meaningful | The degenerate cancelled-context failure half is gone; the remaining test covers only successful mandatory alias write (`internal/assignment/assignment_publisher_test.go:687`). |

## Interactions Outside Phase Scope

Phase 4/5 consumers should rely on the new `AssignmentCommit.LeaderRevision` as a stable term revision, not the renewal-advanced live KV revision. Custom election implementations used with production managers should implement `CheckLeadership(ctx, claimed)` with live storage verification; the manager falls back to no-op for election agents that do not expose that method (`manager_election.go:241`), while the built-in `NATSElection` and `NopElection` both now expose the method (`internal/election/nats_election.go:334`, `internal/election/nop.go:45`).

## Lint / Build / Test Status

`make lint` passed:

```text
Checking golangci-lint version...
✓ golangci-lint 2.11.4 is installed
Running linters...
0 issues.
```

`go vet ./...` passed:

```text
<no output>
```

First `go test ./... -race -count=1 -timeout 600s` run failed once in out-of-scope `source`:

```text
--- FAIL: TestNatsKV_Watch_Deduplication (0.55s)
    nats_kv_dedup_test.go:103: received signal for reordered update
FAIL
FAIL	github.com/arloliu/parti/v2/source	2.795s
...
FAIL
```

Targeted rerun of the failing test passed:

```text
ok  	github.com/arloliu/parti/v2/source	2.054s
```

Full race-suite rerun passed:

```text
ok  	github.com/arloliu/parti/v2/test/integration/manager	87.050s
ok  	github.com/arloliu/parti/v2/test/integration/misc	1.044s
ok  	github.com/arloliu/parti/v2/test/integration/partition	9.799s
ok  	github.com/arloliu/parti/v2/test/integration/stableid	5.610s
?   	github.com/arloliu/parti/v2/test/simulation/cmd/simulation	[no test files]
?   	github.com/arloliu/parti/v2/test/simulation/internal/config	[no test files]
ok  	github.com/arloliu/parti/v2/test/simulation/internal/coordinator	8.528s
?   	github.com/arloliu/parti/v2/test/simulation/internal/logging	[no test files]
ok  	github.com/arloliu/parti/v2/test/simulation/internal/metrics	1.010s
?   	github.com/arloliu/parti/v2/test/simulation/internal/natsutil	[no test files]
?   	github.com/arloliu/parti/v2/test/simulation/internal/producer	[no test files]
ok  	github.com/arloliu/parti/v2/test/simulation/internal/worker	6.498s
ok  	github.com/arloliu/parti/v2/test/stress	7.168s
ok  	github.com/arloliu/parti/v2/types	1.007s
```

## Verdict

**merge**. All prior P0/P1 findings are resolved, no new P0/P1 findings were found, and validation is clean on rerun with one unrelated source-package flake noted.
