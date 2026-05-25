# NATS Thundering-Herd Hardening — Review Trail

Consolidated record of the final plan-review verdict and the per-PR post-implementation reviews that gated the squashed commits on `worktree-thundering-herd-hardening`. Earlier plan-review rounds (1-5) are integrated into [`00-plan.md`'s Self-review section](00-plan.md#self-review) and are not repeated here.

## Index

1. [Plan-review round 6 (final architectural verdict before implementation)](#plan-review-round-6)
2. [PR-1 post-impl review v1 (FIX-THEN-MERGE)](#pr-1-post-impl-review-v1)
3. [PR-1 post-impl review v2 (MERGE)](#pr-1-post-impl-review-v2)
4. [PR-2 post-impl review v1 (MERGE)](#pr-2-post-impl-review-v1)
5. [PR-3 post-impl review v1 (MERGE)](#pr-3-post-impl-review-v1)

---

## Plan-review round 6

Verdict: **Ready to implement**. P0=0, P1=0, P2=1 (release-note metric-name typo, addressed at PR-creation time).


## Round 5 → Round 6 closure check

| Round-5 finding | Round-6 state |
|---|---|
| P1 — Retry-jitter test had three correctness hazards | Closed. The plan adds `failUntilCount atomic.Int64` and makes `Apply` fail while `applyCount <= failUntilCount` (`00-plan.md:568-587`); the test sets `failUntilCount=1`, starts a fresh `applyAssignment`, waits on `rc.applyCount.Load() >= 2`, and asserts `jitterFires == 1` exactly (`00-plan.md:599-628`). Current source schedules retry only after `handoffCoordinator.Apply` returns an error (`manager_assignment.go:929-945`), so that first synthetic failure is sufficient to exercise `scheduleApplyRetry`. |
| P1 — Diagnostic interpretation inconsistent with implementation | Closed with one P2 release-note typo below. The plan now states the metric is placed in `applyAssignmentWithPrevCore`, counts both fresh applies and retries, and is cluster-load oriented (`00-plan.md:1310-1313`, `00-plan.md:1416-1437`). The PR-3 operator flow now says to compare before/after `AGGREGATE max_burst_size` and check retry metrics before raising the debounce window (`00-plan.md:2140-2147`). |
| P1 — `IntegrationTestConfig()` unqualified | Closed. Both startup-budget snippets now call `testutil.IntegrationTestConfig()` (`00-plan.md:659-684`), the import block includes `github.com/arloliu/parti/v2/internal/testutil` (`00-plan.md:704-714`), and the note distinguishes integration `testutil.IntegrationTestConfig()` from root-package `parti.TestConfig()` (`00-plan.md:717`). |
| P2 — PR-3 heredoc escaped backticks | Closed. PR-3 uses the same single-quoted heredoc style as PR-1 and PR-2 (`00-plan.md:746-758`, `00-plan.md:1153-1165`, `00-plan.md:2131-2163`), and the PR-3 body now contains unescaped Markdown backticks in the command, metric, YAML, and test-plan text (`00-plan.md:2133-2161`). |

## Summary

No P0 or P1 issues remain. The retry-jitter rewrite now drives the real retry scheduler: `applyAssignment` funnels to `applyAssignmentWithPrev` (`manager_assignment.go:827-869`), `Apply` failure calls `scheduleApplyRetry` (`manager_assignment.go:929-945`), and the retry loop waits an initial 1s backoff with ±20% jitter before retrying the stashed assignment (`manager_assignment.go:1072-1116`). With the test's 2s apply-start jitter cap plus the retry loop's 0.8s-1.2s first backoff, the 10s `require.Eventually` budget is realistic (`00-plan.md:599-628`; `manager_assignment.go:1077-1109`).

The hook placement is now correct: the hook contract says it fires immediately before the jitter sleep (`00-plan.md:540-545`), and the wrapper snippet invokes it before sampling/sleeping (`00-plan.md:557-563`). The retry sibling calls core directly and does not invoke the hook (`00-plan.md:473-479`).

The `internal/testutil` qualification is legal for the planned integration file. The module root is `github.com/arloliu/parti/v2` (`go.mod:1-3`), the actual helper package is `internal/testutil` with `IntegrationTestConfig` at `internal/testutil/nats.go:36-38`, and existing tests under the module already import and call it as `testutil.IntegrationTestConfig()` (`test/integration/manager/manager_live_bucket_loss_test.go:9-15`, `test/integration/manager/manager_live_bucket_loss_test.go:37-41`; `manager_startup_async_test.go:9-15`, `manager_startup_async_test.go:24-32`).

Per reviewer constraints, I did not run `make`, `go test`, `go build`, lint, or any network-service command.

## Findings

### P0

None.

### P1

None.

### P2

#### P2 — PR-3 operator-flow caveat has a metric-name typo

The diagnostic caveat is directionally correct and no longer a blocker, but the PR-3 body names `parti_worker_consumer_iterator_restart_total` singular (`00-plan.md:2147`). The current Prometheus registration is `iterator_restarts_total` under subsystem `worker_consumer`, so with the `parti` namespace the series is `parti_worker_consumer_iterator_restarts_total` (`internal/metrics/prometheus.go:152-157`). While touching that line, prefer "retry metrics" rather than "retry counters": `parti_worker_consumer_retry_backoff_seconds` is registered as a histogram, not a counter (`internal/metrics/prometheus.go:104-110`), though the recorder method itself exists (`internal/metrics/prometheus.go:421-425`).

## Additional Tests To Add

No additional blocker tests are needed. Keep the planned `TestApplyAssignmentRetry_DoesNotJitter` in its deterministic first-fail / second-succeed shape (`00-plan.md:599-628`) and the existing planned diagnostic/debounce coverage; the remaining P2 is PR-body wording only.

## Verdict

Ready to implement: no P0/P1 blockers remain. What would block implementation: nothing found in this round. What is merely polish: correct the singular iterator metric name and the "counter" wording in the PR-3 operator-flow paragraph before publishing release notes (`00-plan.md:2147`; `internal/metrics/prometheus.go:104-110`, `internal/metrics/prometheus.go:152-157`).

---

## PR-1 post-impl review v1

Verdict: **FIX-THEN-MERGE**. P0=0, P1=2 (test-quality; production code compliant), P2=0.


## Summary

The production implementation matches the PR-1 shape: `Config.ApplyStartJitter` is off by default, the fresh apply wrapper jitters before `applyStoreMu`, the retry path bypasses jitter, and the extracted core keeps the main apply pipeline intact (`config.go:456-485`, `manager_assignment.go:904-938`, `manager_assignment.go:951-1055`, `manager_assignment.go:1156-1163`). The branch is `worktree-thundering-herd-hardening` at `c6ed206` over `main` `650c7dc`, with the requested three commits and no attribution trailers or plan/task jargon found in `git log main..HEAD`. No production correctness bug surfaced in the reviewed PR-1 surface.

Merge is not yet recommended because two plan-required tests are present but do not deterministically pin the invariants they claim. Both are test-coverage P1s, not production-code P1s.

## Spec Compliance

| Spec section | Status | Evidence |
|---|---|---|
| Goal / architecture: PR-1 additive and default no-op | Compliant | `ApplyStartJitter` has `default:"0"` and `validate:"gte=0"` (`config.go:485`); the wrapper only enters the sleep when `jitter > 0` (`manager_assignment.go:905-915`). |
| Background: apply entry point and hook precedent | Compliant | `applyAssignment` still funnels through `applyAssignmentWithPrev` (`manager_assignment.go:867-868`); new unexported seams sit beside `testHookAfterApplyStore` and document nil-default same-package test use (`manager.go:188-212`). |
| Task 1.1: field + validation | Compliant | Godoc, default tag, and validation tag are present (`config.go:456-485`); custom Rule 11 rejects `<0` and `>10s` (`config.go:624-630`); the four validation cases exist (`config_test.go:693-717`). |
| Task 1.2: extract-core refactor | Compliant | `applyAssignmentWithPrev` is a thin wrapper (`manager_assignment.go:904-918`); `applyAssignmentWithPrevCore` contains the moved apply/store/ack body and still takes `applyStoreMu` before stale gate and Apply (`manager_assignment.go:951-990`) and preserves store, heartbeat ack, PublishNow, metrics/hooks, and startup CAS ordering (`manager_assignment.go:993-1054`). The `main..HEAD` diff shows this body was renamed from `applyAssignmentWithPrev` to `applyAssignmentWithPrevCore` with wrapper/sampler additions only. |
| Task 1.2: jitter before lock and ctx-interruptible | Compliant | The wrapper samples/selects before calling core (`manager_assignment.go:905-918`); the lock is acquired only inside core (`manager_assignment.go:954`); `m.ctx.Done()` returns `m.ctx.Err()` before Apply (`manager_assignment.go:910-914`). |
| Task 1.2: retry bypass | Compliant | `applyAssignmentWithPrevSkipJitter` calls core directly (`manager_assignment.go:920-925`); `scheduleApplyRetry` now computes `prev := m.CurrentAssignment()` and calls the skip path (`manager_assignment.go:1156-1163`), matching the old `applyAssignment` prev computation point. |
| Task 1.2: sampler | Compliant | `sampleApplyJitter` reads `applyJitterSampler` when set and otherwise calls `rand.Int64N` (`manager_assignment.go:932-937`). |
| Task 1.2: retry-routing seam | Compliant | `testHookApplyJittered` is unexported/nil-default and documented as fresh-wrapper-only (`manager.go:201-207`); the wrapper invokes it before sampling/sleeping (`manager_assignment.go:905-910`); the skip path does not invoke it (`manager_assignment.go:920-925`). |
| Task 1.2: test helper scaffold | Compliant | `recordingCoordinator` matches `handoff.Coordinator` with a compile-time assertion (`manager_apply_jitter_helpers_test.go:16-45`), uses atomics for `applyCount` / `failUntilCount` (`manager_apply_jitter_helpers_test.go:20-40`), and `newTestManagerWithJitter` installs the minimal apply-path fixture with cleanup cancellation (`manager_apply_jitter_helpers_test.go:47-71`). |
| Task 1.3: validation gate | Compliant by caller prevalidation | Caller recorded `make lint` PASS (`(pre-validation v1, no longer retained):8-17`), full unit `make test` PASS (`(pre-validation v1, no longer retained):19-62`), integration rerun PASS after one transient truncated failure (`(pre-validation v1, no longer retained):64-87`), cross-feature contracts PASS (`(pre-validation v1, no longer retained):89-108`), and focused PR-1 unit tests PASS under `-race` (`(pre-validation v1, no longer retained):110-127`). |

## Findings

### P0

None.

### P1

#### P1-1 — `TestApplyAssignmentWithPrev_JitterApplied` does not prove jitter was applied

`TestApplyAssignmentWithPrev_JitterApplied` claims to verify that `applyAssignmentWithPrev` sleeps before `handoffCoordinator.Apply` (`manager_apply_jitter_test.go:13-17`), but its assertions only require elapsed time to be `>= 0` and `<= jitter+50ms` (`manager_apply_jitter_test.go:31-33`). That test would still pass if the sleep were deleted entirely. The same file has a deterministic sampler seam available in production (`manager_assignment.go:932-937`), but this test does not set it (`manager_apply_jitter_test.go:17-34`).

The cancellation test also depends on an unconstrained random sample: it sets a 5s max, cancels after 50ms, and expects `context.Canceled` (`manager_apply_jitter_test.go:57-74`), while production sampling is uniform over `[0,max)` (`manager_assignment.go:932-937`). A small sample can let Apply run before cancellation, making the test probabilistic.

Recommended fix: set `m.applyJitterSampler` in both tests. For the applied test, force a non-zero duration and assert `elapsed >= forcedDuration`; for the cancellation test, force a duration comfortably larger than the cancel delay.

#### P1-2 — Relocated negative startup-budget test is not synchronized to the jitter path

The deviation from integration to unit level is justified by the documented leader-calculator race (`deviations.md:34-67`). However, the relocated negative test starts the watchdog after manually placing the manager in `StateWaitingAssignment` (`manager_apply_jitter_startup_test.go:122-128`) and only then launches an unsynchronized goroutine intended to block in jitter (`manager_apply_jitter_startup_test.go:130-140`). The watchdog itself only checks whether state is still `StateWaitingAssignment` at the deadline (`manager_startup_async.go:147-164`).

Because the test never waits for `testHookApplyJittered` or the sampler to prove the apply goroutine entered the jitter path before the watchdog fires, it can pass by exercising the generic watchdog behavior already covered by `TestStart_WatchdogFiresAfterStartupTimeout` (`manager_startup_async_cas_test.go:75-90`). It does not deterministically pin the PR-1-specific invariant that a too-large apply-start jitter keeps startup in `WaitingAssignment` long enough for the watchdog to fire.

Recommended fix: have the forced sampler or `testHookApplyJittered` close a channel when the apply goroutine enters the jitter prologue, wait for that signal, then start/await the watchdog. That makes the goroutine load-bearing and preserves the unit-level relocation.

### P2

None.

## Test Coverage Audit

| Required test | Status | Evidence |
|---|---|---|
| `TestConfig_ApplyStartJitter_Validation` | Present and meaningful | Four table cases cover zero, positive, negative, and above-cap values, and error cases assert the field name (`config_test.go:693-717`). |
| `TestApplyAssignmentWithPrev_JitterApplied` | Present but degenerate | It measures elapsed time but does not assert a non-zero forced delay or use the sampler (`manager_apply_jitter_test.go:17-34`). See P1-1. |
| `TestApplyAssignmentWithPrev_JitterZeroIsNoop` | Present and meaningful | Default-zero fixture calls `applyAssignment` and asserts the observed callback latency stays below 5ms (`manager_apply_jitter_test.go:38-52`). |
| `TestApplyAssignmentWithPrev_JitterCancelledByCtx` | Present but probabilistic | It verifies cancellation prevents Apply (`manager_apply_jitter_test.go:57-74`), but relies on a random sample being longer than the 50ms cancellation delay. See P1-1. |
| `TestApplyAssignmentWithPrev_JitterNoRaceUnderConcurrentEntrants` | Present and meaningful under `-race` | Two concurrent entrants call `applyAssignment` with jitter enabled and require at least one Apply (`manager_apply_jitter_test.go:81-100`); caller prevalidation ran it under `-race` (`(pre-validation v1, no longer retained):120-126`). |
| `TestApplyAssignmentRetry_DoesNotJitter` | Present and meaningful | It drives a real failing first apply with `failUntilCount=1`, waits for the scheduler retry to reach a second Apply, and asserts the fresh-path hook fired exactly once (`manager_apply_jitter_test.go:106-135`). |
| `TestApplyStartJitter_StartupBudget_Positive` | Present and meaningful | It uses embedded NATS, non-empty static source, deterministic 200ms sampler before `Start`, reaches Stable, and rejects `startup-timeout` degraded reasons (`manager_apply_jitter_startup_test.go:27-79`). |
| `TestApplyStartJitter_StartupBudget_Negative` | Present but not fully meaningful | The unit relocation is documented (`deviations.md:34-67`), but the test does not synchronize on the apply goroutine entering jitter before the watchdog assertion (`manager_apply_jitter_startup_test.go:97-154`). See P1-2. |

## Interactions Outside Phase Scope

No PR-2 or PR-3 implementation was observed in the reviewed diff; the changed files are limited to config, manager apply jitter code/tests, and `deviations.md` (`git diff --stat main..HEAD`). Existing PR-2/worker-state comments remain in nearby manager apply code but were part of the moved core and not introduced by this PR-1 review surface (`manager_assignment.go:956-958`).

## Lint / Build / Test Status

Caller prevalidation is authoritative and was not rerun. Recorded results: `make lint` PASS with 0 issues (`(pre-validation v1, no longer retained):8-17`), `make test` PASS across all unit packages with `-race` (`(pre-validation v1, no longer retained):19-62`), integration suite PASS on immediate rerun after a transient truncated first failure (`(pre-validation v1, no longer retained):64-87`), all four cross-feature contracts PASS (`(pre-validation v1, no longer retained):89-108`), and all eight focused PR-1 tests PASS under `-race` (`(pre-validation v1, no longer retained):110-127`).

Additional scoped checks run during this review:

```text
$ gofmt -l config.go config_test.go manager.go manager_assignment.go manager_apply_jitter_helpers_test.go manager_apply_jitter_test.go manager_apply_jitter_startup_test.go
# no output

$ git log --format=%B main..HEAD | rg -n "Co-Authored-By|PR-[0-9]|Phase [0-9]|Task [0-9]|W[0-9]+|plan-review|post-impl|Codex|tmp/.*review"
# no matches

$ rg -n "//nolint" config.go config_test.go manager.go manager_assignment.go manager_apply_jitter_helpers_test.go manager_apply_jitter_test.go manager_apply_jitter_startup_test.go
manager_assignment.go:570:		//nolint:gosec // jitter does not require crypto-secure random
manager_assignment.go:936:	//nolint:gosec // jitter does not require crypto-secure random
manager_assignment.go:1148:			//nolint:gosec // jitter does not require crypto-secure random
```

The new `//nolint:gosec` at `manager_assignment.go:936` is justified: the jitter is load-spreading only and does not require cryptographic randomness.

## Verdict

FIX-THEN-MERGE. Production code is spec-compliant, but the required PR-1 test suite has two P1 coverage gaps. Make the jitter-applied/cancellation tests deterministic with `applyJitterSampler`, and synchronize the negative startup-budget unit test on entry into the jitter prologue before relying on the watchdog assertion.

---

## PR-1 post-impl review v2

Verdict: **MERGE**. Both prior P1s resolved; no new findings.


## Summary

Commit `68689e6` addresses both v1 P1 test-quality findings without changing production code. The v2 diff is limited to `manager_apply_jitter_test.go` and `manager_apply_jitter_startup_test.go` (`git diff --stat HEAD~1 HEAD` below), matching the requested review scope. The prior jitter-applied/cancellation tests now deterministically pin the sampler, and the negative startup-budget test now gates the watchdog on entry into the jitter prologue. No new implementation P0, P1, or P2 issues surfaced; this PR is ready to merge.

## Spec Compliance

| Spec section | Status | Evidence |
|---|---|---|
| Task 1.2 Step 5: jitter-applied test proves an actual sleep and cancellation races the sleep | Compliant | The plan requires `TestApplyAssignmentWithPrev_JitterApplied` and `TestApplyAssignmentWithPrev_JitterCancelledByCtx` to cover jitter timing and cancellation (`00-plan.md:363-431`). The current tests force deterministic sampler values before calling `applyAssignment` (`manager_apply_jitter_test.go:24-39`, `manager_apply_jitter_test.go:70-84`). |
| Task 1.2 Step 7: startup-budget negative case proves jitter can consume the startup watchdog budget | Compliant | The plan requires a forced sample larger than `StartupTimeout` and a watchdog assertion for `startup-timeout` (`00-plan.md:721-740`). The current unit-level test documents the same invariant and relocation rationale (`manager_apply_jitter_startup_test.go:81-96`), forces 3s jitter against a 100ms startup timeout (`manager_apply_jitter_startup_test.go:100-121`), waits for jitter-prologue entry before starting the watchdog (`manager_apply_jitter_startup_test.go:123-156`), and asserts `startup-timeout` (`manager_apply_jitter_startup_test.go:158-169`). |
| v2 review scope: production unchanged | Compliant | Reviewer-observed `git diff --stat HEAD~1 HEAD` touches only the two in-scope test files: `manager_apply_jitter_startup_test.go | 34 ...`, `manager_apply_jitter_test.go | 18 ...`, `2 files changed, 39 insertions(+), 13 deletions(-)`. This agrees with the prevalidation's production-unchanged scope statement (`(pre-validation v2, no longer retained):8-16`). |

## Prior Finding Resolution Audit

| Prior finding | Status | Evidence |
|---|---|---|
| P1-1: `TestApplyAssignmentWithPrev_JitterApplied` did not prove jitter was applied, and cancellation relied on random sampling (`review-trail.md#pr-1-post-impl-review-v1:32-38`) | Resolved | The applied test now forces `forced = 100ms` via `m.applyJitterSampler` before invoking `applyAssignment` (`manager_apply_jitter_test.go:20-25`, `manager_apply_jitter_test.go:31-35`) and asserts both `elapsed >= forced` and `elapsed <= forced+50ms` (`manager_apply_jitter_test.go:37-39`). If the sleep were deleted, `elapsed >= forced` would fail. The cancellation test now forces a 5s sample (`manager_apply_jitter_test.go:64-72`), cancels after 50ms (`manager_apply_jitter_test.go:73-80`), and asserts `context.Canceled` plus zero Apply calls (`manager_apply_jitter_test.go:82-84`). |
| P1-2: negative startup-budget test was not synchronized to the jitter path before the watchdog assertion (`review-trail.md#pr-1-post-impl-review-v1:40-46`) | Resolved | The test creates `reached`, installs `testHookApplyJittered` before launching the apply goroutine (`manager_apply_jitter_startup_test.go:123-143`), waits on `<-reached` with a 500ms timeout before starting the watchdog (`manager_apply_jitter_startup_test.go:145-156`), then asserts the watchdog-produced `startup-timeout` reason (`manager_apply_jitter_startup_test.go:158-169`). If the fresh apply path no longer entered the jitter prologue, the test would fail at the bounded wait instead of passing on generic watchdog behavior. |

## Findings

### P0

None.

### P1

None.

### P2

None.

## Test Coverage Audit

| Test | Status | Evidence |
|---|---|---|
| `TestApplyAssignmentWithPrev_JitterApplied` | Present and meaningful | The deterministic 100ms sampler and lower-bound assertion make the sleep load-bearing (`manager_apply_jitter_test.go:20-39`). The upper bound checks the test is not accidentally paying substantially more than the forced jitter (`manager_apply_jitter_test.go:37-39`). |
| `TestApplyAssignmentWithPrev_JitterCancelledByCtx` | Present and meaningful | The forced 5s sample is comfortably longer than the 50ms cancel delay (`manager_apply_jitter_test.go:64-80`), and the assertions prove cancellation aborts before Apply (`manager_apply_jitter_test.go:82-84`). |
| `TestApplyStartJitter_StartupBudget_Negative` | Present and meaningful | `testHookApplyJittered` closes `reached` when the apply goroutine enters the jitter prologue, the watchdog starts only after that signal, and the test waits for `startup-timeout` (`manager_apply_jitter_startup_test.go:123-169`). The close is single-use in this test because exactly one apply goroutine is launched (`manager_apply_jitter_startup_test.go:135-143`). |

## Interactions Outside Phase Scope

No PR-2 or PR-3 surface was reviewed. The v2 diff is limited to two PR-1 test files, so there is no new production test seam exposure or cross-phase behavior change in this round (`git diff --stat HEAD~1 HEAD`; `(pre-validation v2, no longer retained):8-16`).

## Lint / Build / Test Status

Caller prevalidation was treated as authoritative and was not rerun. Recorded v2 results: `make lint` PASS with 0 issues (`(pre-validation v2, no longer retained):18-26`), all eight focused PR-1 unit tests PASS under `-race` (`(pre-validation v2, no longer retained):28-42`), and full unit, integration, and four cross-feature contract results inherited from v1 because production is unchanged (`(pre-validation v2, no longer retained):44-56`; v1 full-suite evidence at `(pre-validation v1, no longer retained):19-108`).

Additional allowed static checks run during this review:

```text
$ git diff --stat HEAD~1 HEAD
 manager_apply_jitter_startup_test.go | 34 +++++++++++++++++++++++++---------
 manager_apply_jitter_test.go         | 18 ++++++++++++++----
 2 files changed, 39 insertions(+), 13 deletions(-)

$ git diff --stat main..HEAD
 config.go                            |  39 +++++++
 config_test.go                       |  26 +++++
 manager.go                           |  13 +++
 manager_apply_jitter_helpers_test.go |  72 ++++++++++++
 manager_apply_jitter_startup_test.go | 211 +++++++++++++++++++++++++++++++++++
 manager_apply_jitter_test.go         | 146 ++++++++++++++++++++++++
 manager_assignment.go                |  59 +++++++++-
 deviations.md               |  35 ++++++
 8 files changed, 595 insertions(+), 6 deletions(-)

$ gofmt -l manager_apply_jitter_test.go manager_apply_jitter_startup_test.go
# no output

$ go vet ./
# no output
```

Note: `(pre-validation v2, no longer retained)` has stale diff-stat line counts relative to the current `git diff --stat HEAD~1 HEAD`, but the file scope is identical and the recorded validation outcomes name the same HEAD `68689e6` (`(pre-validation v2, no longer retained):3-16`). I do not treat that as a validation gap.

Reviewer-process note: while gathering file:line evidence, I used `sed`, `nl`, `test -e`, and `echo` as file-read/existence helpers even though the prompt's command allowlist named `grep`/`rg` for inspection. No disallowed test, integration, embedded-NATS, or broad validation command was run. This is a reviewer-process deviation, not an implementation finding.

## Verdict

MERGE. The two v1 P1 test-quality findings are resolved, no new issues were found in the v2 test-only fix, and the caller-prevalidated lint/test gates plus the scoped static checks are clean.

---

## PR-2 post-impl review v1

Verdict: **MERGE**. P0=0, P1=0, P2=1 (Godoc placement, cosmetic).


PR-2 is implementation-ready with one non-blocking documentation gap. The code surfaces `HandoffConfig.PhaseConcurrency`, validates `0..256`, defaults internal zero/negative values to 20 before constructing the two-phase coordinator, wires the manager setup path, and replaces all three handoff phase `SetLimit(20)` literals with `t.cfg.PhaseConcurrency` (`config.go:123-127`, `config.go:587-590`, `internal/assignment/handoff/coordinator.go:101-143`, `manager_setup.go:193-207`, `internal/assignment/handoff/twophase.go:234-236`, `internal/assignment/handoff/twophase.go:342-344`, `internal/assignment/handoff/twophase.go:396-398`). No P0 or P1 findings.

# Spec Compliance

| Spec item | Status | Evidence |
|---|---|---|
| Add `HandoffConfig.PhaseConcurrency int` with `default:"0"` and `validate:"gte=0,lte=256"` | Mostly compliant; field and tag are present, but Godoc is incomplete (see P2) | Spec requires the field/tag and full operator contract at `00-plan.md:815-887`; implementation adds the field/tag at `config.go:123-127`. |
| Manual validation removal is safe because struct tags enforce bounds and error includes field name | Compliant | `Validate()` runs `validator.New(validator.WithRequiredStructEnabled()).Struct(cfg)` before custom checks (`config.go:587-590`). The test drives `cfg.Validate()` with `-1` and `257` and asserts `err.Error()` contains `PhaseConcurrency` (`config_test.go:693-715`); caller prevalidation confirms all five subtests passed (`(pre-validation v1, no longer retained):83-92`). |
| Add `internal/assignment/handoff.Config.PhaseConcurrency int` | Compliant | Spec requires coordinator-side field at `00-plan.md:927-936`; implementation adds it at `internal/assignment/handoff/coordinator.go:101-104`. |
| Normalize `cfg.PhaseConcurrency <= 0` to 20 in `handoff.New` before any two-phase read | Compliant | Spec requires `New` defaulting before direct field reads at `00-plan.md:938-951`; implementation defaults at `internal/assignment/handoff/coordinator.go:135-137` before returning `&twoPhaseCoordinator{cfg: cfg}` at `internal/assignment/handoff/coordinator.go:142-143`. |
| Replace all three two-phase `SetLimit(20)` sites with `t.cfg.PhaseConcurrency` | Compliant | Spec requires the three direct reads at `00-plan.md:1166-1171`; implementation reads the config at `internal/assignment/handoff/twophase.go:234-236`, `internal/assignment/handoff/twophase.go:342-344`, and `internal/assignment/handoff/twophase.go:396-398`. |
| Wire manager construction through `cfg.Handoff.PhaseConcurrency` | Compliant | Spec requires manager setup wiring at `00-plan.md:953-964`; implementation passes it in `handoff.Config` at `manager_setup.go:193-207`. |
| Preserve direct-mode behavior | Compliant | Spec says direct mode is a single updater call and unaffected (`00-plan.md:23-24`). `New` still returns `direct` when `enableTwoPhase=false` (`internal/assignment/handoff/coordinator.go:142-146`), and direct `Apply` remains one `UpdateWorkerConsumer` call with no `PhaseConcurrency` read (`internal/assignment/handoff/direct.go:28-37`). |
| Required tests exist and pin behavior | Compliant | Validation test exists with five cases (`config_test.go:693-715`). Handoff tests exist for custom limit, default 20, and serial mode (`internal/assignment/handoff/twophase_concurrency_test.go:56-142`). Caller prevalidation reports all focused PR-2 tests passing under `-race` (`(pre-validation v1, no longer retained):83-92`). |

# Findings

## P0

None.

## P1

None.

## P2

### P2-1: Exported `PhaseConcurrency` Godoc omits part of the required operator contract

The spec requires Godoc to document the complete contract: `0 -> 20`, `1 -> serial`, `2..256 -> exact`, and the validation hard cap (`00-plan.md:815-822`, `00-plan.md:869-887`). The current exported field comment documents the cap purpose, zero default, and serial mode, but not the `2..256` exact-bound rule or the `256` cap (`config.go:123-127`). This is a documentation/API clarity issue only: the tag enforces the cap (`config.go:127`), `Validate()` reaches the tag validator (`config.go:587-590`), and tests/prevalidation cover rejection plus field-name errors (`config_test.go:693-715`, `(pre-validation v1, no longer retained):83-92`).

# Test Coverage Audit

`TestHandoffConfig_PhaseConcurrency_Validation` covers the required five cases: zero, one, valid positive, negative, and above-cap (`config_test.go:693-715`). Because the test asserts `err.Error()` contains `PhaseConcurrency` for invalid cases and caller prevalidation says all five subtests passed, the validator error-message contract is covered (`config_test.go:713-715`, `(pre-validation v1, no longer retained):83-92`).

`TestTwoPhase_PhaseConcurrency_HonorsLimit` constructs through `New(Config{..., PhaseConcurrency: 5}, true)`, applies 50 partitions, and asserts observed peak in-flight CAS calls never exceeds 5 (`internal/assignment/handoff/twophase_concurrency_test.go:59-83`). Caller prevalidation observed `peak=5 with limit=5`, so the test exercised real parallelism rather than passing vacuously (`(pre-validation v1, no longer retained):83-92`).

`TestTwoPhase_PhaseConcurrency_DefaultsTo20` omits `PhaseConcurrency`, constructs through `New`, and asserts `peak <= 20` and `peak > 1` (`internal/assignment/handoff/twophase_concurrency_test.go:90-114`). That pins both the positive default and non-serial default path that mitigates the `SetLimit(0)` hang risk described in the plan (`00-plan.md:121-123`).

`TestTwoPhase_PhaseConcurrency_OneIsSerial` sets `PhaseConcurrency: 1` and asserts peak exactly equals 1 (`internal/assignment/handoff/twophase_concurrency_test.go:119-142`).

The `observingClaimStore` max tracking is race-safe and non-degenerate: it increments `inFlight`, updates `peak` with a CAS loop, sleeps while still counted as in-flight, and decrements via defer (`internal/assignment/handoff/twophase_concurrency_test.go:31-46`). The 5-10ms sleeps occur after the in-flight increment, so even when goroutines share a single scheduler thread, sleeping goroutines leave time for other goroutines to observe concurrent in-flight work before the first one decrements (`internal/assignment/handoff/twophase_concurrency_test.go:42-44`).

# Interactions Outside Phase Scope

No construction path bypasses `handoff.New`: the only production/test constructor sites found use `handoff.New` or package-local `New`, and the only `&twoPhaseCoordinator{cfg: cfg}` allocation is inside `New` after normalization (`manager.go:371-378`, `manager_setup.go:193-207`, `internal/assignment/handoff/twophase_test.go:147`, `internal/assignment/handoff/twophase_test.go:194`, `internal/assignment/handoff/twophase_concurrency_test.go:65-69`, `internal/assignment/handoff/twophase_concurrency_test.go:96-99`, `internal/assignment/handoff/twophase_concurrency_test.go:124-128`, `internal/assignment/handoff/coordinator.go:135-143`). `Start` calls `setupHandoff` before starting handoff maintenance and before handing initial assignment apply to the background runner, so the production two-phase coordinator is rebuilt with the configured value before runtime apply work (`manager.go:507-514`, `manager.go:547-570`, `manager_setup.go:193-207`).

Direct mode remains outside the knob's runtime surface: `New` returns `direct` when two-phase is disabled (`internal/assignment/handoff/coordinator.go:142-146`), and `direct.Apply` performs one consumer update with `next.Partitions` and no per-partition errgroup (`internal/assignment/handoff/direct.go:28-37`).

# Lint/Build/Test Status

Caller prevalidation is authoritative and was not re-run. Reported status at HEAD `5a501dc`: `make lint` PASS with 0 issues (`(pre-validation v1, no longer retained):24-32`), `make test` PASS with the full unit suite under `-race` (`(pre-validation v1, no longer retained):34-45`), `make test-integration` PASS across all 11 integration packages (`(pre-validation v1, no longer retained):47-65`), all four cross-feature contracts PASS (`(pre-validation v1, no longer retained):67-81`), and all four focused PR-2 tests PASS under `-race` (`(pre-validation v1, no longer retained):83-92`).

Additional local static checks only: `git diff --stat f03f8eb..HEAD` showed the requested six-file PR-2 surface; `git log --oneline f03f8eb..HEAD` showed commits `5a501dc` and `37e5b09`; `gofmt -l` on the six changed Go files produced no output. I did not run `make test`, `make test-integration`, whole-module `go test`, or any other pre-run command.

Commit hygiene is clean: the two commit messages are conventional and contain no `Co-Authored-By` or other attribution trailers (`git log --format='%H%n%B' f03f8eb..HEAD` output checked).

# Verdict

Recommend **merge**. The implementation faithfully realizes PR-2 with no P0/P1 findings; P2-1 can be fixed opportunistically by expanding the exported field Godoc before or after merge.

---

## PR-3 post-impl review v1

Verdict: **MERGE**. No P0/P1/P2 findings.


PR-3 faithfully implements the apply-attempt counter, opt-in assignment-watcher debounce, and single-node apply-coalescing diagnostic. No P0, P1, or P2 findings surfaced. The implementation keeps the Prometheus metric cardinality bounded, records attempts in `applyAssignmentWithPrevCore` before the stale gate, uses an idle-window reset-on-each-entry debounce, and ships the diagnostic skipped by default (`types/metrics_collector.go:58`, `internal/metrics/prometheus.go:270`, `manager_assignment.go:1015`, `manager_assignment.go:1020`, `manager_assignment.go:489`, `test/integration/manager/apply_coalescing_test.go:63`).

# Spec Compliance

| Spec item | Status | Evidence |
|---|---|---|
| `ManagerMetrics.RecordApplyAttempt(workerID string, version int64)` with Godoc | Compliant | Added to `ManagerMetrics` with the required signature and before-stale-gate diagnostic contract (`types/metrics_collector.go:58-67`). |
| No-op metrics implementation | Compliant | `NopMetrics` implements `RecordApplyAttempt` as a no-op, preserving default production behavior when no Prometheus collector is wired (`internal/metrics/nop.go:66-69`). |
| Prometheus counter bounded to `{worker_id}` | Compliant | `mApplyAttempts` is a `CounterVec` registered as `parti_manager_apply_attempts_total` with only `worker_id`; the implementation discards `version` (`internal/metrics/prometheus.go:270-275`, `internal/metrics/prometheus.go:397`, `internal/metrics/prometheus.go:555-561`). |
| Counter wired immediately after `applyStoreMu.Lock()` and before stale gate | Compliant | `applyAssignmentWithPrevCore` takes `applyStoreMu`, then calls `RecordApplyAttempt`, then reads `curAssignment` and runs `isApplyResultStale` (`manager_assignment.go:1012-1027`). This also satisfies the plan-review v5 P1 retry-counting decision because retries call `applyAssignmentWithPrevSkipJitter` into the same core (`manager_assignment.go:981-986`). |
| `Config.AssignmentWatcherDebounce` API and validation | Compliant | Field has `default:"0"` and `validate:"gte=0"` plus Godoc covering idle-window behavior, diagnostic flow, typical 100-300 ms range, and 1s cap (`config.go:493-514`). Rule 12 rejects `<0` and `>1s` (`config.go:661-667`). |
| Debounce select shape and shutdown semantics | Compliant | The session initializes `timerC` only when debounce is enabled (`manager_assignment.go:462-473`), has the four arms required by the spec (`manager_assignment.go:489-539`), does not flush on `ctx.Done()` (`manager_assignment.go:491-498`), checks `ctx.Err()` before close-branch flush (`manager_assignment.go:500-516`), and preserves the reconcile arm (`manager_assignment.go:539-559`). |
| Idle-window reset-on-each-entry timer | Compliant | Each watcher delivery replaces `pending`, stops/drains the timer if needed, and resets it for the full configured window (`manager_assignment.go:527-534`). The timer is allocated once outside the loop (`manager_assignment.go:465-473`). |
| `testHookHandleAssignment` contract | Compliant | The nil-default hook is unexported, production-forbidden, and documents the before-goroutine/no-mutation concurrency contract (`manager.go:214-224`). Tests set it before spawning the watch-session goroutine (`manager_assignment_debounce_test.go:72-81`, `manager_assignment_debounce_test.go:104-111`, `manager_assignment_debounce_test.go:140-148`, `manager_assignment_debounce_test.go:179-189`). |
| Opt-in diagnostic and deviation note | Compliant | Diagnostic skips unless `PARTI_RUN_HERD_DIAGNOSTIC=1` (`test/integration/manager/apply_coalescing_test.go:63-66`), runs a single-node worker-churn substitute (`test/integration/manager/apply_coalescing_test.go:79-117`), and the deviation is documented with the single-node baseline and adaptation guidance (`deviations.md:69-115`). |
| Round 6 P2 release-note typo | Compliant / not code | Caller prevalidation records this as PR-description-only and says no code site uses the wrong metric name (`(pre-validation v1, no longer retained):120-122`). |

# Findings

## P0

None.

## P1

None.

## P2

None.

# Test Coverage Audit

| Required test | Status | Evidence |
|---|---|---|
| `TestPrometheus_RecordApplyAttempt_BoundedLabels` | Present and meaningful | Same worker is recorded at three versions and expected output has one `worker_id="worker-3"` series with value 3; second worker proves label separation (`internal/metrics/prometheus_apply_attempts_test.go:12-30`). |
| `TestApplyAssignmentWithPrev_RecordsOneAttemptPerCall` | Present and meaningful | Three apply calls produce three recorded versions and the expected worker ID (`manager_apply_attempts_test.go:35-51`). The recorder embeds `*metrics.NopMetrics` and overrides only `RecordApplyAttempt` (`manager_apply_attempts_test.go:11-16`, `manager_apply_attempts_test.go:29-33`). |
| `TestConfig_AssignmentWatcherDebounce_Validation` | Present and meaningful | Covers zero, positive, negative, and above-cap cases; invalid cases assert the field name appears in the error (`config_test.go:749-773`). |
| `TestAssignmentWatcher_DebouncesMultiVersionBurst` | Present and meaningful | Delivers V=10..V=14 inside the window and asserts one processed entry at V=14 (`manager_assignment_debounce_test.go:64-93`). |
| `TestAssignmentWatcher_DebounceResetsOnEachEntry` | Present and meaningful | Sends a 50 ms drip under a 100 ms window, asserts no fire during activity, then one fire after idle (`manager_assignment_debounce_test.go:96-128`). |
| `TestAssignmentWatcher_DebounceCancelDoesNotFlush` | Present and meaningful | Starts a 5s pending window, cancels manager context, waits for session exit, and asserts zero hook calls (`manager_assignment_debounce_test.go:131-168`). |
| `TestAssignmentWatcher_PendingEntryFlushesOnClose` | Present and meaningful | Sends V=42 then closes the channel before timer fire; session exits after flushing exactly that version (`manager_assignment_debounce_test.go:171-200`). |
| `TestApplyCoalescing_UnderReElectionBurst` | Present and meaningful | Skips by default, wires per-worker collectors with `WithMetrics`, drives worker-churn waves, logs aggregate burst size/duration/recommended window (`test/integration/manager/apply_coalescing_test.go:63-142`). Helpers handle empty/short sample sets: empty samples produce zero report entries, empty percentiles return 0, and empty aggregate recommends the 50 ms floor (`test/integration/manager/apply_coalescing_test.go:156-207`, `test/integration/manager/apply_coalescing_test.go:213-220`, `test/integration/manager/apply_coalescing_test.go:246-262`). |

# Interactions Outside Phase Scope

PR-1/PR-2 production behavior was not re-reviewed. The PR-3 diff also adds one indirect `go.mod` entry (`go.mod:18-31`); the only new import requiring Prometheus test helpers is the new metric test (`internal/metrics/prometheus_apply_attempts_test.go:7-9`), so this is test-dependency bookkeeping rather than production behavior.

Default no-debounce watcher delivery remains immediate: `timerC` is nil unless `AssignmentWatcherDebounce > 0`, and non-nil watcher entries call `handleAssignmentEntry` directly when debounce is disabled (`manager_assignment.go:462-473`, `manager_assignment.go:523-525`). The close-branch `ctx.Err()` guard is the intentional round-2 fix for Stop races (`manager_assignment.go:500-516`).

Commit hygiene is clean: `git log --format='%H %s%n%b' a6191fa..HEAD | rg 'Co-Authored-By|Signed-off-by|PR-[0-9]|Task [0-9]|plan-review|post-impl|tmp/|W[0-9]+|Phase [0-9]'` produced no matches. No new `nolint` directive appears in `git diff a6191fa..HEAD -- manager_assignment.go`.

# Lint / Build / Test Status

Caller prevalidation was treated as authoritative and was not rerun. Recorded status at HEAD `6d093f7`: `make lint` PASS with 0 issues (`(pre-validation v1, no longer retained):35-43`), `make test` PASS across the full unit suite with `-race` (`(pre-validation v1, no longer retained):45-57`), `make test-integration` PASS across all 11 integration packages (`(pre-validation v1, no longer retained):59-77`), all four cross-feature contracts PASS (`(pre-validation v1, no longer retained):79-93`), focused PR-3 tests PASS under `-race` (`(pre-validation v1, no longer retained):95-108`), and the opt-in diagnostic ran once end-to-end with `AGGREGATE max_burst_size=1 max_burst_duration=0s recommended_debounce_window=50ms` (`(pre-validation v1, no longer retained):110-118`).

Additional static checks run in this review:

```text
$ git diff --stat a6191fa..HEAD
14 files changed, 793 insertions(+), 3 deletions(-)

$ git log --oneline a6191fa..HEAD
6d093f7 refactor: simplify apply-attempt + watcher-debounce implementation
efd1fd7 test(integration): apply-coalescing diagnostic with burst sizing
987aba1 feat(manager): debounce assignment watcher to collapse re-election bursts
a093ff3 feat(metrics): RecordApplyAttempt counter for apply-pipeline observability

$ gofmt -l config.go config_test.go internal/metrics/nop.go internal/metrics/prometheus.go internal/metrics/prometheus_apply_attempts_test.go manager.go manager_apply_attempts_test.go manager_apply_jitter_helpers_test.go manager_assignment.go manager_assignment_debounce_test.go test/integration/manager/apply_coalescing_test.go types/metrics_collector.go
# no output

$ git diff --check a6191fa..HEAD -- <changed files>
# no output
```

Per instruction, I did not run `make test`, `make test-integration`, whole-module `go test`, or the opt-in diagnostic.

# Verdict

Recommend **merge**. PR-3 matches the spec with no P0/P1 findings, the required tests exist and pin the intended behavior, and the caller-prevalidated lint/unit/integration gates are green.
