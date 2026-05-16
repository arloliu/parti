# phase4_followup_claim_resolver Post-Implementation Review (v3 - integration test addition)

## Summary

The integration test is meaningful and bug-sensitive: it restarts a real embedded NATS server with the same client port and JetStream StoreDir, drives the full manager + handoff + processing gate + partition consumer stack, and fails on parent `5a9102f` at the cache-convergence assertion. The empirical finding is accurate for the pinned NATS client: server restart/reconnect does not close the KV watcher `Updates()` channel, so this integration test exercises recovery through periodic reconcile, not the watcher supervisor. The new commit documents that distinction, but older fix comments and the `5bc46cc` body still overstate "NATS reconnect" as a watcher-channel-close trigger. No P0/P1 findings; verdict is merge with P2 documentation/test-cleanup polish.

## Empirical finding audit

Accurate. `nats.go` v1.50.0 documents that a `KeyWatcher` "will not close the channel until Stop is called or connection is closed" (`.../github.com/nats-io/nats.go@v1.50.0/jetstream/kv.go:342-347`), `Stop()` only unsubscribes (`kv.go:1205-1210`), and the internal channel is closed from the subscription closed handler (`kv.go:1335-1339`). A NATS server restart with reconnect enabled leaves the `nats.Conn` alive, so the channel staying open is consistent with both source and the observed test behavior.

Implication: the production recovery mechanism for silent watcher stalls is the periodic reconciler. The watcher supervisor is still useful for actual subscription/channel closure (`watcher.Stop()`, connection close, or any server-side event that really closes the subscription), but it should be framed as defensive belt-and-suspenders, not as the production path proven by the restart integration test. A heartbeat/event-drought detector is not required for correctness while reconcile is enabled, but would be the right mechanism if operators need watcher-restart metrics to reflect silent stalls rather than only channel-close events.

## Integration test audit

### Coverage

The test covers a real NATS restart by shutting down and restarting an embedded server on the same port and StoreDir (`test/integration/failure/claim_resolver_nats_restart_test.go:170-176`, `:463-535`). JetStream state is intended to survive via FileStorage for both the stream (`:81-90`) and handoff KV (`:124-129`), and the helper preserves the StoreDir (`:478-486`, `:517-518`). It then writes a fresh claim for one partition and requires every worker's resolver cache to converge (`:185-198`, `:308-337`), plus verifies post-restart message consumption on unchanged-owner partitions (`:199-211`).

What it does not cover: the watcher supervisor `!ok` branch on real restart; that branch is covered only by cooperative `r.watcher.Stop()` unit tests (`internal/durable/claim_resolver_watcher_freeze_test.go:69-75`, `internal/durable/claim_resolver_restart_test.go:52-75`). Server-side consumer GC is not directly covered; if it closes the subscription, the unit reproducer covers the same `Updates()`-closed path, and if it silently stalls, reconcile covers convergence but not restart metrics. Long network partition is also not covered; that is a broader reconnect/manager failure mode and should be a separate chaos/integration test only if production evidence warrants it.

### Sensitivity (parent-base verification)

Confirmed. In a temporary worktree at `5a9102f`, I copied the integration test, removed the `durable.WithReconcileInterval(1*time.Second)` option reference, and ran:

```text
go test ./test/integration/failure/ -run TestClaimResolver_RecoversAfterNATSRestart -race -count=1 -timeout 90s
```

Failure tail:

```text
--- FAIL: TestClaimResolver_RecoversAfterNATSRestart (14.11s)
    claim_resolver_nats_restart_test.go:176: NATS restart + client reconnect completed in 179.273648ms
    claim_resolver_nats_restart_test.go:197:
        	Error Trace:	/tmp/parti-prefix/test/integration/failure/claim_resolver_nats_restart_test.go:321
        	            				/tmp/parti-prefix/test/integration/failure/claim_resolver_nats_restart_test.go:197
        	Error:      	Condition never satisfied
        	Test:       	TestClaimResolver_RecoversAfterNATSRestart
        	Messages:   	resolver cache did not converge to mutated owner after NATS restart — this is the production bug: the watcher silently stops delivering events on server restart and, without the periodic-reconcile + watcher-restart fix in commit 30e93ce, the cache stays frozen at the pre-restart state forever. pre-restart snapshot=map[worker-0:map[p0:worker-0 p1:worker-0 p2:worker-0 p3:worker-1] worker-1:map[p0:worker-0 p1:worker-0 p2:worker-0 p3:worker-1] worker-2:map[p0:worker-0 p1:worker-0 p2:worker-0 p3:worker-1]]
FAIL
FAIL	github.com/arloliu/parti/v2/test/integration/failure	14.123s
FAIL
```

Because real restart does not close the watcher channel, this failure is due to the parent lacking periodic reconcile; it does not prove the watcher-restart branch.

### Stability

`-race -count=3` passed twice. The verbose run showed stable reconnect windows:

```text
=== RUN   TestClaimResolver_RecoversAfterNATSRestart
    claim_resolver_nats_restart_test.go:176: NATS restart + client reconnect completed in 229.964696ms
--- PASS: TestClaimResolver_RecoversAfterNATSRestart (5.63s)
=== RUN   TestClaimResolver_RecoversAfterNATSRestart
    claim_resolver_nats_restart_test.go:176: NATS restart + client reconnect completed in 228.828903ms
--- PASS: TestClaimResolver_RecoversAfterNATSRestart (5.61s)
=== RUN   TestClaimResolver_RecoversAfterNATSRestart
    claim_resolver_nats_restart_test.go:176: NATS restart + client reconnect completed in 229.173849ms
--- PASS: TestClaimResolver_RecoversAfterNATSRestart (5.62s)
PASS
ok  	github.com/arloliu/parti/v2/test/integration/failure	17.882s
```

The waits are bounded (`20s` initial convergence, `5s` reconnect, `10s` cache convergence, `15s` message flow) and poll with `require.Eventually`, not unbounded sleeps.

### Scope and tightness

Scope is tight for the stated gap. The test validates cache convergence after restart by mutating one claim to a synthetic owner (`test/integration/failure/claim_resolver_nats_restart_test.go:185-198`) and separately validates message flow only on other partitions, avoiding the expected suppression for the synthetic non-worker owner (`:199-211`). The `1s` reconcile interval is test-friendly but uses the production code path; operators on the default `30s` interval should expect recovery latency of up to one reconcile period after a silent watcher stall (`internal/durable/claim_resolver.go:26`, `test/integration/failure/claim_resolver_nats_restart_test.go:57-60`, `:367-372`).

## Findings

### P2 - Resolver comments and original fix commit overclaim reconnect as a channel-close trigger

`5bc46cc` says the root cause is `Updates()` channel close on "NATS reconnect" and says the supervisor restarts on channel close for that case. Current code comments still say the watcher supervisor re-establishes the watcher if `Updates()` closes for "NATS reconnect, server-side consumer GC, etc." (`internal/durable/claim_resolver.go:76-82`) and the `!ok` branch repeats "NATS reconnect" as the channel-close cause (`internal/durable/claim_resolver.go:555-561`). The new integration commit and test comment correct this for real server restart (`test/integration/failure/claim_resolver_nats_restart_test.go:34-41`), but the production-code comments should be narrowed to "channel/subscription close" and separately say silent stalls/reconnects are recovered by reconcile.

### P2 - Restart helper does not shut down the replacement server

The helper swaps the live server inside `nsPtr.srv` on restart (`test/integration/failure/claim_resolver_nats_restart_test.go:513-519`) but returns the initial `*server.Server` value (`:533-535`). The caller cleanup shuts down the captured initial `ns` (`:71-76`), not the replacement server. This did not flake in `-count=3` because each run uses a fresh free port and the test process exits, but it leaks the restarted embedded server until process exit and should be fixed by returning/cleaning through a closure that reads `nsPtr.srv`.

## Documentation accuracy

`4916736` properly records the empirical finding: the commit body states the client does not surface NATS restart as a watcher channel close, recovery comes from periodic reconcile, the test interval is `1s`, production default is `30s`, and watcher restart remains covered only by the cooperative unit reproducer. The integration test file repeats the same distinction in its top-level comment (`test/integration/failure/claim_resolver_nats_restart_test.go:25-60`) and in the resolver construction comment (`:367-372`).

The stale/overbroad documentation is in the pre-existing fix framing: `5bc46cc` and `internal/durable/claim_resolver.go:76-82`, `:555-561` still imply NATS reconnect/server restart reaches the supervisor's `!ok` branch. That should be corrected, but it is documentation/operability precision rather than a correctness blocker because the reconciler covers the real restart path.

## Lint / Build / Test Status

`make lint`:

```text
===== make lint =====
Checking golangci-lint version...
✓ golangci-lint 2.11.4 is installed
Running linters...
0 issues.
===== make lint exit=0 =====
```

`go test ./test/integration/failure/ -run TestClaimResolver_RecoversAfterNATSRestart -race -count=3 -timeout 180s`:

```text
===== integration restart race count=3 =====
ok  	github.com/arloliu/parti/v2/test/integration/failure	17.821s
===== integration restart race count=3 exit=0 =====
```

`go test ./... -race -count=1 -short -timeout 300s` tail:

```text
ok  	github.com/arloliu/parti/v2/test/integration/manager	9.587s
ok  	github.com/arloliu/parti/v2/test/integration/misc	1.006s
ok  	github.com/arloliu/parti/v2/test/integration/partition	1.013s
ok  	github.com/arloliu/parti/v2/test/integration/stableid	2.544s
?   	github.com/arloliu/parti/v2/test/simulation/cmd/simulation	[no test files]
?   	github.com/arloliu/parti/v2/test/simulation/internal/config	[no test files]
ok  	github.com/arloliu/parti/v2/test/simulation/internal/coordinator	8.922s
?   	github.com/arloliu/parti/v2/test/simulation/internal/logging	[no test files]
ok  	github.com/arloliu/parti/v2/test/simulation/internal/metrics	1.010s
?   	github.com/arloliu/parti/v2/test/simulation/internal/natsutil	[no test files]
?   	github.com/arloliu/parti/v2/test/simulation/internal/producer	[no test files]
ok  	github.com/arloliu/parti/v2/test/simulation/internal/worker	1.010s
ok  	github.com/arloliu/parti/v2/test/stress	1.010s
ok  	github.com/arloliu/parti/v2/types	1.007s
===== all short race exit=0 =====
```

`go vet ./...` and `go build ./...`:

```text
===== go vet =====
===== go vet exit=0 =====

===== go build =====
===== go build exit=0 =====
```

## Verdict

merge. The integration test is sensitive to the pre-fix bug, stable under repeated race runs, and correctly records that real NATS restart recovery is delivered by periodic reconcile rather than the watcher supervisor. Remaining issues are P2 documentation/test-cleanup polish only.
