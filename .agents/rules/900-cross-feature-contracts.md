# 900 - Cross-Feature Contracts

Read before changing how any error is wrapped, classified, or routed, and before
touching `Manager.Start`. These contracts live on `main`, each has a regression
test in tree, and any failure-classification or error-routing change MUST preserve
them. Run the named tests when you touch the relevant path.

## 1. Whole-bucket-missing → every worker enters `StateDegraded`
Bucket-missing errors from stableid / heartbeat / election / assignment-watcher
flow through `m.recordKVError` → accumulate against `KVErrorThreshold` → trip
`m.enterDegraded`, so every worker degrades within a bounded window. A classifier
that routes a bucket-missing error elsewhere (e.g. a self-stop path) regresses this.
- Pinned by `TestManager_LiveNATSBucketLoss`,
  `TestManager_LiveNATSBucketLoss_OnDegradedHook` (`test/integration/manager/`).

## 2. Peer claim takeover → only the losing worker shuts down
`onClaimerError` (`manager_election.go`) routes `ErrClaimLost` through
`claimLostShutdown` **only** when the wrapped cause is neither connectivity nor
degrading-JetStream — distinguished via `natsutil.IsConnectivityError ||
natsutil.IsDegradingJetStreamError`. Whole-bucket loss goes to `recordKVError`
(contract 1) instead; only a genuine peer takeover shuts one worker down while the
rest stay healthy. Any classifier change must keep this distinction.

The trap: widening the stableid error classifier once regressed contract 1 by
routing whole-bucket loss down this claim-lost path. The bucket-loss vs
peer-takeover split *is* the guard — keep it.
- Pinned by `TestStableID_StaleKeyTakeover_Reclaim` (`test/integration/stableid/`).

## 3. OnDegraded hook fires exactly once per Degraded entry per worker
- Pinned by `TestManager_LiveNATSBucketLoss_OnDegradedHook`.

## 4. `Manager.Start` returns after the sanity-check phase, not after `StateStable`
Start transitions to `WaitingAssignment` and spawns a background runner. The runner
does one initial wait + apply and starts the post-Stable monitors; on apply success
it CAS-transitions `WaitingAssignment → Stable` (CAS-guarded so calculator ownership
wins on conflict). Callers needing a ready manager call `WaitState(StateStable,
timeout)`. A soft watchdog enters Degraded (reason `startup-timeout`) once if state
is still `WaitingAssignment` after `StartupTimeout`.

Apply is unbounded per attempt (identical to pre-refactor Start): a stuck updater can
block the runner inside `handoffCoordinator.Apply(m.ctx, ...)` until Stop; the
watchdog still fires for probe rotation.
- Pinned by `TestStart_ReturnsBeforeStable`, `TestCasToStableFromWaitingAssignment_*`,
  `TestStartupAsync_CalculatorStateNotClobbered`,
  `TestStart_StopDuringBackground_NoDegraded`,
  `TestStart_WatchdogFiresAfterStartupTimeout`.
