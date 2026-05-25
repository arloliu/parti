# Implementation Deviations

Per-plan deviations from spec, kept in tree to document
not-fully-faithful implementations and the reasons.

> Thundering-herd hardening deviations moved to
> [`docs/plans/thundering-herd-hardening/deviations.md`](../docs/plans/thundering-herd-hardening/deviations.md).

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
