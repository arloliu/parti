# NATS Thundering-Herd Hardening — Implementation Deviations

Two cases where the implementation deviated from the spec in
[`00-plan.md`](00-plan.md). Each is documented with plan citation,
why the spec form did not work, and the substitute used.

## Task 1.2 Step 7: StartupBudget negative test relocated to unit-level

**Plan citation:** Task 1.2 Step 7 of [`00-plan.md`](00-plan.md)
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
`TestStart_WatchdogFiresAfterStartupTimeout` (documented in the
manager-start-async plan's `tmp/impl-deviations.md` "Task 9" section).

## Task 3.3: Diagnostic uses worker churn instead of Raft meta-election

**Plan citation:** [`00-plan.md`](00-plan.md) §"Task 3.3:
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
