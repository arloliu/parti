# Phase 6 E2E Tests #68-70 Post-Implementation Review (v2)

## Summary

The v1 P0 is genuinely closed: `invariantHolds` now builds the active worker set, requires it to match `commit.Workers`, and checks heartbeat digest/version convergence for every active worker. Tests #68-70 remain present and meaningful against the plan's end-to-end invariant, including the leader-failover and partial-apply recovery paths. I did not find new P0/P1 issues introduced by the stricter helper; the eventual waits cover legitimate bootstrap, failover, and audit-repair windows. Ready to merge; the prior P2 timeout/comment cleanup remains deferred and non-blocking.

## Spec Compliance

| Spec section | Status | Evidence |
|---|---|---|
| #68 `TestE2E_AddPartitionsConvergesToInvariant`: start 3 workers, add 10 partitions via `Modify`, assert bounded ownership + digest convergence | Compliant | The test starts three workers at `test/integration/manager/manager_e2e_invariant_test.go:397-403`, modifies the source to the 11-partition set (seed + 10 added) at `test/integration/manager/manager_e2e_invariant_test.go:405-409`, and waits for the invariant at `test/integration/manager/manager_e2e_invariant_test.go:411-412`. The helper enforces exact ownership at `test/integration/manager/manager_e2e_invariant_test.go:139-166` and active-worker digest/version convergence at `test/integration/manager/manager_e2e_invariant_test.go:196-214`. |
| #69 `TestE2E_PartitionAdditionDuringLeaderChange`: same scenario, stop leader mid-add, assert eventual invariant | Compliant | The test modifies the source at `test/integration/manager/manager_e2e_invariant_test.go:457-460`, stops the old leader immediately afterward at `test/integration/manager/manager_e2e_invariant_test.go:462-468`, waits for a different active leader at `test/integration/manager/manager_e2e_invariant_test.go:470-473` and `internal/testutil/cluster_helpers.go:43-69`, then asserts the full invariant within 60s at `test/integration/manager/manager_e2e_invariant_test.go:475-476`. The stricter active-fleet/commit symmetry check at `test/integration/manager/manager_e2e_invariant_test.go:178-194` prevents passing on a stale pre-failover commit that still lists the stopped leader. |
| #70 `TestE2E_PartialApplyFailureRecovery`: inject handoff failure on one worker; assert invariant after audit-driven reassignment with full capability chain | Compliant | The test starts all workers with `capReportingUpdater` at `test/integration/manager/manager_e2e_invariant_test.go:516-522`, selects a non-leader victim at `test/integration/manager/manager_e2e_invariant_test.go:524-537`, arms persistent apply failure before `Modify` at `test/integration/manager/manager_e2e_invariant_test.go:548-561`, proves an audit-driven version advance and nonzero failure calls at `test/integration/manager/manager_e2e_invariant_test.go:563-578`, heals the victim, then waits for the invariant at `test/integration/manager/manager_e2e_invariant_test.go:580-584`. The final helper checks active workers, so the recovered victim is allowed and required to converge if still active at `test/integration/manager/manager_e2e_invariant_test.go:196-214`. |
| Shared invariant helper proves the plan invariant for the live fleet | Compliant | The helper documents the three required properties at `test/integration/manager/manager_e2e_invariant_test.go:114-128`, collects active IDs from `cluster.GetActiveWorkers()` at `test/integration/manager/manager_e2e_invariant_test.go:139-149`, enforces active fleet `<-> commit.Workers` equality at `test/integration/manager/manager_e2e_invariant_test.go:178-194`, and checks each active worker's heartbeat against `commit.Payloads[W].SetDigest` and `commit.Version` at `test/integration/manager/manager_e2e_invariant_test.go:196-214`. |
| Test harness seams preserve existing cluster behavior | Compliant | `NewWorkerCluster` and `NewFastWorkerCluster` continue to build static-source clusters through the shared constructor at `internal/testutil/nats.go:173-203`; `NewWorkerClusterWithSource` adds the source/config seam needed by these E2E tests at `internal/testutil/nats.go:205-223`; `AddWorker` still maps the optional logger to `parti.WithLogger` before delegating at `internal/testutil/nats.go:268-290`. |

## Prior Finding Resolution Audit

| Prior finding | Status | Evidence |
|---|---|---|
| P0 - Shared invariant helper did not prove digest convergence for every active worker | Resolved | The prior gap was looping only over `commit.Workers`. The v2 helper now builds `activeIDs` from `cluster.GetActiveWorkers()` at `test/integration/manager/manager_e2e_invariant_test.go:139-149`, rejects active workers missing from `commit.Workers` and commit workers missing from the active fleet at `test/integration/manager/manager_e2e_invariant_test.go:178-194`, then loops over `activeIDs` for payload, heartbeat, version, and digest checks at `test/integration/manager/manager_e2e_invariant_test.go:196-214`. This is a direct fix, not a workaround. |
| P2 - Timeout rationale/comment cleanup incomplete | Deferred / non-blocking | The main correctness-sensitive audit wait is now explicitly tied to `5 * 5s = 25s` plus slack at `test/integration/manager/manager_e2e_invariant_test.go:563-566`. The top-level `e2eTestConfig` comment still says the HeartbeatTTL is shorter than the standard integration config at `test/integration/manager/manager_e2e_invariant_test.go:25-29`, while the config uses `testutil.IntegrationTestConfig()` at `test/integration/manager/manager_e2e_invariant_test.go:30-43` whose HeartbeatTTL is 5s at `internal/testutil/nats.go:36-45`. Per review scope, this remains polish only and does not invalidate #68-70. |

## Findings

None.

## Test Coverage Audit

| Test | Status | Evidence |
|---|---|---|
| `TestE2E_AddPartitionsConvergesToInvariant` | Present-and-meaningful | Present at `test/integration/manager/manager_e2e_invariant_test.go:366-413`; it seeds one partition, starts three workers, verifies bootstrap, modifies to 11 partitions, and asserts the full helper invariant. |
| `TestE2E_PartitionAdditionDuringLeaderChange` | Present-and-meaningful | Present at `test/integration/manager/manager_e2e_invariant_test.go:415-477`; it waits past bootstrap, modifies the source, stops the old leader, waits for a different leader, and the shared helper now rejects stale commits that do not match the two-worker active fleet. |
| `TestE2E_PartialApplyFailureRecovery` | Present-and-meaningful | Present at `test/integration/manager/manager_e2e_invariant_test.go:479-585`; it injects persistent victim apply failure, proves the audit-repair version bump and failure-path execution, heals the updater, and asserts the full active-fleet invariant. |

## Interactions Outside Phase Scope

`internal/testutil/nats.go` is modified to add `NewWorkerClusterWithSource` and `AddWorkerWithOptions`, which are reasonable test-harness seams for these E2E tests and keep existing `AddWorker` behavior intact at `internal/testutil/nats.go:268-290`. The new E2E test file is currently untracked according to `git status`, so it must be added before commit.

## Lint / Build / Test Status

`make clean-linter-cache && make lint`:

```text
Cleaning golangci-lint cache...
Checking golangci-lint version...
✓ golangci-lint 2.11.4 is installed
Running linters...
0 issues.
```

`go test -race ./test/integration/manager/... -count=1 -timeout=300s -run TestE2E`:

```text
ok  	github.com/arloliu/parti/v2/test/integration/manager	64.225s
```

`go vet ./...`:

```text
(no output; exit 0)
```

## Verdict

merge. There are no P0 or P1 findings; only the explicitly deferred, non-blocking P2 comment/timeout polish remains.
