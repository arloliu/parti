# NATS Thundering-Herd Hardening — Implementation Deviations

One case where the implementation deviated from the spec in
[`00-plan.md`](00-plan.md). Documented with plan citation,
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

## Task 3.3: lifted (3-node cluster diagnostic landed)

Original deviation substituted single-node worker-churn rebalancing for
the plan's intended 3-node-cluster + leader-kill trigger, citing absence
of a cluster helper. That premise was wrong — `partitest.StartEmbeddedNATSCluster`
existed all along. Empirically, killing the JetStream meta-leader does not
trigger a Parti calculator re-publish (separate election layers, multi-URL
reconnect keeps the Parti leader alive, version gate filters watcher
replays). The implemented diagnostic uses a 3-node cluster + R=3 KV for
realistic apply-pipeline semantics, and kills the **Parti calculator
leader** to force peer takeover + Version=N+1 publish. `Config.AssignmentWatcherDebounce`
is left at 0 (its default) deliberately — the diagnostic measures the raw
burst size to inform what the debounce default should be; running it with
debounce enabled would mute the signal. See
`test/integration/manager/apply_coalescing_test.go` and `make herd-diagnostic`.

**Diagnostic finding:** under Parti-leader kill with `RebalanceCooldown=2s`,
the per-worker burst observable by this diagnostic is `max_burst_size=1`.
The existing controls (version gate at `manager_assignment.go:584` + 2s
rebalance cooldown) space consecutive assignment versions far enough apart
that no two arrive within the 50 ms `idleGap`. This confirms the pipeline
is already herd-free under the tested conditions: each worker sees exactly
one `RecordApplyAttempt` per assignment version. The `recommended_debounce_window`
output is therefore a conservative 50 ms safety floor. A per-worker burst
> 1 would require JetStream delivering backed-up messages on reconnect
(V=N through V=N+k in rapid succession) — not reproduced by either the
meta-leader-kill or Parti-leader-kill triggers under multi-URL seeding.
