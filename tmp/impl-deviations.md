# Implementation Deviations

## Task 9: Watchdog test relocated to unit-level

**Plan citation:** Tasks 9 Step 1 (`docs/plans/manager-start-async/2026-05-24-manager-start-async.md`)
sketches `TestStart_WatchdogFiresAfterStartupTimeout` as an integration
test that sets `cfg.StartupTimeout = 1 * time.Millisecond` and calls
`mgr.Start(ctx)` against live NATS.

**Why the plan's form does not work:** `prepareStart`
(`manager_setup.go:27-30`) uses `StartupTimeout` to bound the synchronous
sanity-phase ctx (`startupCtx, _ = context.WithTimeout(ctx,
m.cfg.StartupTimeout)`). With `StartupTimeout = 1ms` the bucket-creation
RPC (`ensureStableIDKV`) is killed by context-deadline-exceeded *before*
Start can return — so the test never reaches the watchdog-fired state.
Verified empirically:

```
manager_startup_async_test.go:117:
    failed to create stable ID KV: failed to open KV bucket
    parti-stableid: context deadline exceeded
```

**Deviation:** moved the test to `manager_startup_async_cas_test.go`
(same-package `parti`) and drive it via `newTestManager` +
`startStartupTimeoutWatchdog` directly. Test pins the same wiring
contract (StartupTimeout elapses → watchdog enters Degraded) without
fighting `StartupTimeout`'s dual role as both sync-ctx bound and
watchdog anchor.

The integration test file keeps the other Task 5/8 cases that exercise
the runner against live NATS.

## Task 1.2 Step 7: StartupBudget negative test relocated to unit-level

**Plan citation:** Task 1.2 Step 7 of `tmp/nats-thundering-herd-hardening-plan.md`
proposes `TestApplyStartJitter_StartupBudget_Negative` as an integration test
that sets `cfg.StartupTimeout = 200ms` (later revised to 1s in the task brief)
and forces jitter = 3s. Expected: watchdog fires "startup-timeout".

**Why the integration form does not work reliably:** When the single
worker is also the leader, the leader calculator starts running during
`prepareStart`. The calculator's state machine can transition the manager
from `StateWaitingAssignment` to `StateScaling` (or similar) within the
first second. The startup watchdog checks `m.State() != StateWaitingAssignment`
before firing; if the calculator's state propagation wins the race, the
watchdog returns without entering Degraded — and the test condition
"startup-timeout" is never satisfied.

Empirically observed: `TestApplyStartJitter_StartupBudget_Negative` with
`StartupTimeout=1s` + live NATS consistently produced "Condition never
satisfied" in the CI-load environment.

**Deviation:** moved the test to `manager_apply_jitter_startup_test.go` at
unit level using `newTestManager(t)`. The test:
1. Drives the state machine to `WaitingAssignment` manually
2. Sets `cfg.StartupTimeout = 100ms`, `cfg.ApplyStartJitter = 5s`,
   forced jitter sampler = 3s
3. Calls `startStartupTimeoutWatchdog()` directly
4. Launches a goroutine calling `mgr.applyAssignment(Assignment{Version: 1})`
   — this goroutine sleeps 3s of forced jitter while the watchdog fires at 100ms
5. Asserts "startup-timeout" appears in OnDegraded within 2s

This approach pins the same wiring contract as the integration form without
the leader-calculator race, and mirrors the precedent in
`TestStart_WatchdogFiresAfterStartupTimeout` documented in the previous
section.

## Task 3.3: Diagnostic uses worker churn instead of Raft meta-election

**Plan citation:** `tmp/nats-thundering-herd-hardening-plan.md` §"Task 3.3:
Multi-version-burst diagnostic + window-sizing measurement", Step 1 ("Boot
3-node embedded NATS cluster... identify the JetStream meta-leader and
forcibly stop that NATS node. The cluster will hold an election; assignment-
bucket leadership flips; the new leader re-publishes the active assignment,
which the workers' assignment-watchers see.").

**What the plan asks for:** A 3-node embedded NATS cluster where the
JetStream meta-leader is killed mid-test. The resulting Raft election causes
the new leader to re-publish the active assignment, producing a rapid burst of
identical assignment versions hitting every worker's watcher simultaneously.

**Why that doesn't work here:** No 3-node embedded-NATS cluster helper exists
in this repo. The only available helper is `testutil.StartEmbeddedNATS(t)`,
which is single-node. A single-node JetStream server has no peer to elect
against; "killing the leader" stops the only node and disconnects every
worker — a completely different failure mode from the leadership-flip burst the
diagnostic is designed to measure.

**Substitute trigger used:** Worker-churn rebalancing via
`WorkerCluster.AddWorkerWithOptions`. The test starts 20 workers and waits
for `StateStable`, then adds one extra worker in each of 3 waves
(`soakAfter/numWaves = 3.3 s` apart). Each addition causes the leader
calculator to re-publish a new assignment version, which hits every existing
worker's assignment-watcher and calls `RecordApplyAttempt`. This is the same
apply-pipeline path that Raft re-election triggers; the measurement
infrastructure is identical.

**End-to-end result (single run, single-node NATS):**

```
AGGREGATE max_burst_size=1 max_burst_duration=0s recommended_debounce_window=50ms
```

`max_burst_size=1` reflects that under single-node embedded NATS each
rebalance produces exactly one assignment notification per worker with no
rapid-fire duplicates — no thundering herd is observable at this scale. This
is the expected baseline; the debounce guards against Raft-election bursts in
production multi-node clusters where the new leader and the old in-flight
messages can arrive within milliseconds of each other.

**Adapting to a multi-node cluster:** Operators or CI with access to a
production-grade NATS endpoint can replace `testutil.StartEmbeddedNATS(t)`
with any `*nats.Conn` pointing at a multi-node cluster and add a
meta-leader kill step between Phase 1 and Phase 2. The
`recordingBurstCollector` and all burst-analysis helpers remain unchanged.

## Task 9a: Cold-start empty test relaxed

**Plan citation:** Task 9a Step 1 sketches
`TestStart_ColdStartEmpty_NoAssignmentHooks` asserting zero
OnAssignmentChanged calls when `source.NewStatic(nil)` is used.

**Why the assertion is wrong:** The leader publishes a Version=1
assignment even with an empty source (calculator runs through
`assignment_publisher.go:329` proposedVersion = currentVersion + 1).
The waiting worker fetches that Version=1 assignment from KV. The
cold-empty bypass at `manager.go:603-633` only triggers when
`initial.Version == 0 && len(initial.Partitions) == 0` — i.e., the
assignment KV is genuinely empty (no leader has published yet). That
condition does not arise in single-worker startup because the worker
is the leader and publishes before checking. So Path B (Version>0,
empty slice) is what executes, and OnAssignmentChanged fires at
least once with empty old + empty new — the leader may re-publish
during settling, producing additional empty fires.

**Deviation:** the test was rewritten to assert that
OnAssignmentChanged fires at least once (the leader may re-publish
during settling, producing additional empty fires), and every
observed fire carries empty old + empty new; no Assigned/Revoked
hooks; worker reaches Stable. This still validates the runner
refactor's correctness on empty assignments without depending on a
code path that the live flow does not enter.
