# Phase 1 Post-Implementation Review (v3)

## Summary

All v1 and v2 P1/P2 source-layer findings are resolved. The Watch/Stop lifecycle fix closes the late `wg.Add` race, the leadership-cadence test now exercises a live reconcile loop with a toggleable probe, and the stale-cache `Modify` test now freezes the watcher deterministically. I found no new P0/P1/P2 issues introduced by the fixes. Ready to merge.

## v2 Finding Resolution Audit

| v2 finding | Status | Evidence |
|---|---|---|
| P1 #1 - `Watch` can add a WaitGroup task after `Stop` started | resolved | `Watch` takes `s.mu`, checks `!s.running`, and returns a pre-closed channel before appending listeners or calling `wg.Add` at `source/nats_kv.go:319-331`; accepted watchers append/capture `srcCtx` and call `s.wg.Add(1)` while still holding `s.mu` at `source/nats_kv.go:333-341`. `Stop` sets `s.running=false`, closes listeners, unlocks, then waits at `source/nats_kv.go:270-303`, so `Wait` cannot race a post-stop `Watch` add. Regression coverage exists in `TestNatsKV_Watch_AfterStop_ReturnsClosedChannel` at `source/nats_kv_test.go:1188-1223`. |
| P1 #2 - leadership-cadence test does not prove live per-tick recomputation | resolved | Production recomputes cadence after every live tick and resets the timer with that value at `source/nats_kv.go:815-827`. `TestNatsKV_ReconcileLoop_LiveProbeRecomputesPerTick` starts a real source with `WithLeadershipProbe(isLeader.Load)` at `source/nats_kv_test.go:1097-1120`, overrides private test cadences at `source/nats_kv_test.go:1102-1105`, observes live ticks through `onReconcileTick` at `source/nats_kv_test.go:1107-1116`, then verifies follower -> leader -> follower interval transitions at `source/nats_kv_test.go:1137-1185`. |
| P2 #3 - production struct carries test-observability fields | resolved | Bare reconcile counters are gone from the production struct; the only observability seam is a private nil-by-default callback `onReconcileTick` at `source/nats_kv.go:148-151`. `NewNatsKV` initializes production options/defaults without setting the hook at `source/nats_kv.go:181-198`; tests assign it explicitly at `source/nats_kv_test.go:1112-1116`. The hook is called outside `s.mu` with the next scheduled interval at `source/nats_kv.go:817-827`, avoiding re-entrancy deadlocks. |
| Residual v1 P2 #9 - `TestNatsKV_Modify_SeesFreshKVNotCache` was racy | resolved | The test seeds KV before `Start`, injects a frozen watcher that never delivers updates, and starts with cache `[A,B,C]` at `source/nats_kv_test.go:333-356`. It raw-writes `[A,B,C,D]`, confirms the local cache is still stale at length 3, then asserts `Modify` sees 4 entries from KV at `source/nats_kv_test.go:363-387`. |

## Spec Compliance

| Section | Status | Evidence |
|---|---|---|
| section 1.1 - Track current revision | compliant | `NatsKV` carries `revision`/`known` at `source/nats_kv.go:133-140`; `Start` seeds from initial `kv.Get` or `ErrKeyNotFound` and applies through the shared locked path at `source/nats_kv.go:221-244`; delete/purge watcher events preserve the event revision and `known=true` at `source/nats_kv.go:731-735`; `Snapshot` returns partitions/revision/known at `source/nats_kv.go:401-415`. |
| section 1.2 - CAS-safe `Update` | compliant | `Update` validates/dedupes before encoding at `source/nats_kv.go:441-449`, uses `Create` for unknown state and `Update` with local revision otherwise at `source/nats_kv.go:453-465`, refreshes from KV on CAS conflicts at `source/nats_kv.go:483-485`, and returns `ErrUpdateRetryExhausted` at `source/nats_kv.go:489`. |
| section 1.3 - `Modify` helper | compliant | `Modify` reads fresh from KV each attempt via `fetchFromKV` at `source/nats_kv.go:516-524` and `source/nats_kv.go:906-921`, validates/dedupes the proposal at `source/nats_kv.go:526-534`, CAS-writes with retry at `source/nats_kv.go:536-556`, and documents re-invocation/side-effect constraints at `source/nats_kv.go:492-502`. |
| section 1.4 - `applyLocal` helper | compliant | `Start` uses `applyLocalLocked` for initial state at `source/nats_kv.go:243-244`; `applyLocal` handles locking, canonical diff, revision/known update, and listener fan-out at `source/nats_kv.go:641-655`; `applyLocalLocked` deep-copies/sorts/diffs at `source/nats_kv.go:661-674`. Watcher, reconcile, `Update`, and `Modify` route through it at `source/nats_kv.go:746`, `source/nats_kv.go:872`, `source/nats_kv.go:470`, and `source/nats_kv.go:545`. |
| section 2.1 - Reconcile ticker | compliant | `Start` launches watcher and reconcile goroutines under the WaitGroup at `source/nats_kv.go:257-259`; `reconcileLoop` schedules periodic reads at `source/nats_kv.go:797-829`; `reconcileOnce` fetches KV, handles `ErrKeyNotFound`, decodes, and applies locally at `source/nats_kv.go:849-873`. |
| section 2.2 - Idempotency | compliant | `applyLocalLocked` computes `changed` with `partitionsEqual` at `source/nats_kv.go:667`; listener fan-out only runs when `notify && changed` at `source/nats_kv.go:641-655`. |
| section 2.3 - Watcher close handling | compliant | `watchLoop` uses the `entry, ok := <-watcher.Updates()` form and calls `restartWatcher` on `!ok` at `source/nats_kv.go:718-724`; `restartWatcher` retries with backoff and spawns the replacement watch loop under the WaitGroup at `source/nats_kv.go:756-769`. |
| section 2.4 - Configuration | compliant | `NatsKVOption`, `WithReconcileInterval`, `WithLeadershipProbe`, and `WithUpdateRetries` are implemented at `source/nats_kv.go:50-100`; `NewNatsKV` remains variadic and initializes default, leader, and follower intervals at `source/nats_kv.go:181-198`. |
| section 2.5 - Delete/purge fan-out | compliant | Delete/purge events go through `applyLocal(nil, entry.Revision(), true)`, preserving delete revision and notifying on state change at `source/nats_kv.go:731-735`; fan-out is centralized in `applyLocal` at `source/nats_kv.go:641-655`. |
| section 3.4 - `CanonicalID` | compliant | `Partition.CanonicalID` length-prefixes each key and returns empty for no keys at `types/partition.go:81-113`; collision/digest regression coverage is at `types/partition_test.go:142-181`. |
| section 4.6 - validation/dedupe | compliant | Read-side decode rejects invalid partitions and duplicate canonical IDs at `source/nats_kv.go:1006-1022`; write-side `validateAndDedupe` validates, dedupes by `CanonicalID`, and deep-copies keys at `source/nats_kv.go:1033-1052`; `Update`/`Modify` both use it before CAS at `source/nats_kv.go:443` and `source/nats_kv.go:526`. |
| polling-cost | compliant | Leader/follower constants are 30s/5min at `source/nats_kv.go:25-31`; `WithLeadershipProbe` documents leader/follower cadence at `source/nats_kv.go:69-72`; `nextReconcileInterval` selects leader/follower intervals per probe call at `source/nats_kv.go:834-843`. |

## New Findings

None.

## Test Coverage Delta

| Test | Status | Evidence |
|---|---|---|
| `TestNatsKV_Watch_AfterStop_ReturnsClosedChannel` | present-and-meaningful | Calls `Stop`, then `Watch`, asserts the returned channel is closed, and verifies `wg.Wait` is drained at `source/nats_kv_test.go:1188-1223`. |
| `TestNatsKV_ReconcileLoop_LiveProbeRecomputesPerTick` | present-and-meaningful | Starts a live source with a toggleable leadership probe, private fast cadences, and a tick hook, then verifies follower/leader/follower transitions at `source/nats_kv_test.go:1079-1185`. |
| `TestNatsKV_Modify_SeesFreshKVNotCache` | present-and-meaningful | Uses a frozen watcher, raw KV write, stale-cache assertion, and callback count assertion at `source/nats_kv_test.go:333-387`. |

## Lint / Build / Test Status

`make lint` passed:

```text
Checking golangci-lint version...
✓ golangci-lint 2.11.4 is installed
Running linters...
0 issues.
__MAKE_LINT_STATUS=0
```

`go test ./types/... ./source/... -race -count=1` passed:

```text
ok  	github.com/arloliu/parti/v2/types	1.007s
ok  	github.com/arloliu/parti/v2/source	4.324s
__GO_TEST_STATUS=0
```

`go vet ./...` passed:

```text
__GO_VET_STATUS=0
```

No flakes observed.

## Verdict

merge. No P0 or P1 findings remain, and no new issues were introduced by the v2 fixes.
