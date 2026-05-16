# phase4_followup_gap2 Post-Implementation Review (v1)

## Summary

The implementation is faithful to the gap-2 spec: reconcile rescues are observable, drift-triggered watcher restarts are rate-limited, and successful supervisor restarts classify `drift_detected` through the pending CAS path. The main concurrency edges introduced by the simplify pass look sound: `w.Stop()` captures the current watcher under `watcherMu`, the fire-and-forget goroutine is not part of `doneCh`, and `lastDriftRestartNano` has a single-writer invariant. No P0/P1 findings. Merge is recommended, with one P2 test-convention cleanup for `time.Sleep` in the new drift tests.

## Spec Compliance

| Requirement | Status | Evidence |
|---|---|---|
| R1 `IncReconcileRescue()` on both ResolverMetrics interfaces with matching Godoc | compliant | Internal interface includes the spec Godoc and method at `internal/durable/resolver_metrics.go:33-42`; public consumer interface matches at `consumer/metrics.go:45-54`. Test spy and simulation adapter implement it at `internal/durable/claim_resolver_test.go:81-93` and `test/simulation/internal/metrics/collector.go:974-976`; Prometheus counter exists at `test/simulation/internal/metrics/collector.go:74-75`, `test/simulation/internal/metrics/collector.go:328-332`. |
| R2 emit `IncReconcileRescue` from `reconcileOnce` only when `len(pendingByPID) > 0`, before apply | compliant | `reconcileOnce` returns before metrics when no pending work at `internal/durable/claim_resolver.go:869-871`; emits before `applyPendingBatch` at `internal/durable/claim_resolver.go:872-877`; restart request follows apply at `internal/durable/claim_resolver.go:879-885`. |
| R3 drift-triggered watcher restart with cooldown and `drift_detected` classification | compliant | Pending flag and last-restart fields are defined at `internal/durable/claim_resolver.go:158-171`; request path stores timestamp, stores pending flag before capturing/stopping watcher at `internal/durable/claim_resolver.go:911-942`; supervisor consumes the flag with `CompareAndSwap(true, false)` at `internal/durable/claim_resolver.go:576-590`. |
| R4 `WithDriftRestartCooldown`; default `max(2*reconcileInterval, 60s)`; explicit zero disables | compliant | Option sets the cooldown and companion bool at `internal/durable/claim_resolver.go:64-85`; default selection respects explicit zero via `!driftRestartCooldownSet` and uses `max` at `internal/durable/claim_resolver.go:247-254`; request path returns early for `cooldown <= 0` at `internal/durable/claim_resolver.go:905-909`. |
| R5 six tests by name and behavior | minor deviation | All six required tests exist at `internal/durable/claim_resolver_drift_test.go:19`, `:72`, `:128`, `:208`, `:305`, `:364` and are behaviorally meaningful. Minor deviation: the spec required no `time.Sleep` for async synchronization, but this file uses sleeps for negative/probe windows at `internal/durable/claim_resolver_drift_test.go:117`, `:295`, `:354`. |

## Concurrency / correctness audit

- `w.Stop()` offload: `requestWatcherRestartFromReconcile` captures `r.currentWatcher` while holding `watcherMu` at `internal/durable/claim_resolver.go:936-938`, then starts `go func() { _ = w.Stop() }()` after unlock at `internal/durable/claim_resolver.go:939-942`. `Stop()`'s `doneCh` waits only for the supervisor and reconciler waitgroup at `internal/durable/claim_resolver.go:319-332`, while lifecycle `Stop()` synchronously stops the current watcher and then waits for `doneCh` at `internal/durable/claim_resolver.go:351-370`; the drift-stop goroutine is intentionally not part of that wait. This is acceptable because `KeyWatcher.Stop` is idempotent and errors are intentionally discarded.
- `lastDriftRestartNano`: zero means never fired and bypasses cooldown at `internal/durable/claim_resolver.go:911-915`; elapsed time is computed as `time.Since(time.Unix(0, last)) < cooldown` at `internal/durable/claim_resolver.go:911-913`; the only writer found is `Store(time.Now().UnixNano())` at `internal/durable/claim_resolver.go:916`, matching the single-reconciler-goroutine invariant at `internal/durable/claim_resolver.go:901-904`.
- CAS interleavings: normal drift path is store flag then stop watcher at `internal/durable/claim_resolver.go:918-942`, followed by supervisor CAS to emit `drift_detected` at `internal/durable/claim_resolver.go:584-590`. A real non-drift close sees the flag false and emits `channel_closed` at `internal/durable/claim_resolver.go:584-590`. If a non-drift close races after the flag store but before drift stop, it can be misclassified; if supervisor CAS happens before the store, the drift close can classify as `channel_closed`. The implementation documents both misclassifications as benign because recovery is identical at `internal/durable/claim_resolver.go:918-924`.
- Repeated `runWatcher` failures: failed establish attempts emit `establish_failed` at `internal/durable/claim_resolver.go:561-573` and do not touch `driftRestartPending`; the flag is only consumed after a successful watcher is established at `internal/durable/claim_resolver.go:576-590`. This matches the spec's "eventually drift_detected" behavior after retry success.
- `reconcileInterval == 0`: default cooldown remains zero because defaulting is gated by `r.reconcileInterval > 0` at `internal/durable/claim_resolver.go:247-254`; restart request is inert at `internal/durable/claim_resolver.go:905-909`. If `reconcileOnce` is called directly and finds drift, rescue still fires before the inert restart request at `internal/durable/claim_resolver.go:869-885`, matching the spec's "rescue still fires; restart inert" note.

No race smell found in the drift restart state. `currentWatcher` is consistently written under `watcherMu` in initial and restart paths at `internal/durable/claim_resolver.go:511-514`, `internal/durable/claim_resolver.go:603-605`, and read under the same mutex for drift stop at `internal/durable/claim_resolver.go:936-938`.

## Findings

### P2 - New drift tests still use `time.Sleep` for async negative/probe windows

The spec explicitly required all async waits in `claim_resolver_drift_test.go` to use `require.Eventually` and no `time.Sleep` synchronization. Three sleeps remain: steady-state no-rescue waits by sleeping a fixed six ticks at `internal/durable/claim_resolver_drift_test.go:113-119`, cooldown probing sleeps inside a loop at `internal/durable/claim_resolver_drift_test.go:288-296`, and disabled-restart negative assertion sleeps at `internal/durable/claim_resolver_drift_test.go:349-355`. These tests are meaningful and passed under race/count validation, so this is not merge-blocking; replacing them with `require.Never` or explicit event-driven tick hooks would better match the project testing rule.

## Test Coverage Audit

| Test | Status | Evidence |
|---|---|---|
| `TestClaimResolver_ReconcileRescueIncrementsMetric` | present-and-meaningful | Starts resolver with 50ms reconcile and zero drift restart, stops watcher, writes a claim, and waits for rescue metric at `internal/durable/claim_resolver_drift_test.go:34-66`. |
| `TestClaimResolver_ReconcileNoRescueWhenNoDrift` | present-and-meaningful, with P2 wait-style issue | Seeds KV, waits for watcher convergence, then asserts rescue stays zero over multiple reconcile ticks at `internal/durable/claim_resolver_drift_test.go:87-120`. |
| `TestClaimResolver_DriftTriggersWatcherRestart` | present-and-meaningful | Stops watcher, writes drift, asserts rescue and `drift_detected`, then verifies a subsequent write reaches cache at `internal/durable/claim_resolver_drift_test.go:143-200`. |
| `TestClaimResolver_DriftRestartRespectsCooldown` | present-and-meaningful, with P2 wait-style issue | Drives two stopped-watcher drift events under a 5s cooldown; verifies second rescue occurs but `drift_detected` remains exactly one at `internal/durable/claim_resolver_drift_test.go:223-299`. The second cooperative close may classify as `channel_closed`, which matches the spec intent that `drift_detected` fires once within cooldown. |
| `TestClaimResolver_DriftRestartDisabledByZeroCooldown` | present-and-meaningful, with P2 wait-style issue | Uses `WithDriftRestartCooldown(0)`, drives drift, asserts rescue fires and no `drift_detected` restart appears in the bounded window at `internal/durable/claim_resolver_drift_test.go:320-356`. |
| `TestClaimResolver_DriftRestartReasonClassifiedCorrectly` | present-and-meaningful | Drives a drift restart, snapshots counts, then stops the new watcher without KV drift and asserts the next restart is `channel_closed` with no additional `drift_detected` at `internal/durable/claim_resolver_drift_test.go:379-435`. |
| `TestClaimResolver_WatcherRestartOnChannelClose` migrated constant use | present-and-meaningful | Uses `watcherRestartReasonChannelClosed` and verifies cache convergence plus metric at `internal/durable/claim_resolver_restart_test.go:41-74`. |
| `TestClaimResolver_StopWithRestartingWatcher` migrated constant use | present-and-meaningful | Uses `watcherRestartReasonEstablishFailed` to wait for failed re-establish before asserting `Stop` is prompt during backoff at `internal/durable/claim_resolver_restart_test.go:287-318`. |

## Sensitivity verification

Command run on parent `0bbd124` with the new drift test copied into `internal/durable/`:

```text
# github.com/arloliu/parti/v2/internal/durable [github.com/arloliu/parti/v2/internal/durable.test]
internal/durable/claim_resolver_drift_test.go:40:3: undefined: WithDriftRestartCooldown
internal/durable/claim_resolver_drift_test.go:64:13: ms.reconcileRescueCount undefined (type *metricsSpy has no field or method reconcileRescueCount)
internal/durable/claim_resolver_drift_test.go:119:21: ms.reconcileRescueCount undefined (type *metricsSpy has no field or method reconcileRescueCount)
internal/durable/claim_resolver_drift_test.go:148:3: undefined: WithDriftRestartCooldown
internal/durable/claim_resolver_drift_test.go:172:13: ms.reconcileRescueCount undefined (type *metricsSpy has no field or method reconcileRescueCount)
internal/durable/claim_resolver_drift_test.go:177:33: undefined: watcherRestartReasonDriftDetected
internal/durable/claim_resolver_drift_test.go:229:3: undefined: WithDriftRestartCooldown
internal/durable/claim_resolver_drift_test.go:247:33: undefined: watcherRestartReasonDriftDetected
internal/durable/claim_resolver_drift_test.go:284:13: ms.reconcileRescueCount undefined (type *metricsSpy has no field or method reconcileRescueCount)
internal/durable/claim_resolver_drift_test.go:293:49: undefined: watcherRestartReasonDriftDetected
internal/durable/claim_resolver_drift_test.go:293:49: too many errors
FAIL	github.com/arloliu/parti/v2/internal/durable [build failed]
FAIL
```

## Lint / Build / Test Status

`make lint`:

```text
===== make lint =====
Checking golangci-lint version...
✓ golangci-lint 2.11.4 is installed
Running linters...
0 issues.
```

`go test ./internal/durable/... -race -count=3 -timeout 180s`:

```text
===== go test durable race count=3 =====
ok  	github.com/arloliu/parti/v2/internal/durable	70.979s
```

`go test ./... -race -count=1 -short -timeout 300s` tail:

```text
ok  	github.com/arloliu/parti/v2/test/integration/manager	9.589s
ok  	github.com/arloliu/parti/v2/test/integration/misc	1.007s
ok  	github.com/arloliu/parti/v2/test/integration/partition	1.009s
ok  	github.com/arloliu/parti/v2/test/integration/stableid	2.548s
?   	github.com/arloliu/parti/v2/test/simulation/cmd/simulation	[no test files]
?   	github.com/arloliu/parti/v2/test/simulation/internal/config	[no test files]
ok  	github.com/arloliu/parti/v2/test/simulation/internal/coordinator	8.415s
?   	github.com/arloliu/parti/v2/test/simulation/internal/logging	[no test files]
ok  	github.com/arloliu/parti/v2/test/simulation/internal/metrics	1.011s
?   	github.com/arloliu/parti/v2/test/simulation/internal/natsutil	[no test files]
?   	github.com/arloliu/parti/v2/test/simulation/internal/producer	[no test files]
ok  	github.com/arloliu/parti/v2/test/simulation/internal/worker	1.012s
ok  	github.com/arloliu/parti/v2/test/stress	1.011s
ok  	github.com/arloliu/parti/v2/types	1.009s
```

`go vet ./...` and `go build ./...`:

```text
===== go vet =====
===== go build =====
```

## Verdict

merge. The implementation satisfies R1-R4 and behaviorally covers R5; there are zero P0/P1 findings. The only issue is P2 test-style cleanup for fixed sleeps in the new drift tests.
