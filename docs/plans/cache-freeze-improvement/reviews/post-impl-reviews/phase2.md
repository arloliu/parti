# Phase 2 Post-Implementation Review (v3)

## Summary

All v1 and v2 P0/P1 findings are resolved. The v2 initial-heartbeat `CapAckV1` bug is fixed by making the publisher OR `types.CapAckV1` into every emitted v1 heartbeat, including the synchronous `Start` publish. I found no new P0 or P1 issues; there is one P2 documentation/API-clarity issue around the now-intentional divergence between the manager's raw capability bitmask and the on-wire heartbeat bitmask. Ready to merge.

## v2 Finding Resolution Audit

| v2 finding | Status | Evidence |
|---|---|---|
| P0 — Initial v1 heartbeat omits `CapAckV1` | resolved | `build()` reads `capsFn` and then unconditionally ORs `types.CapAckV1` before returning the heartbeat at `internal/heartbeat/publisher.go:336-345`; `Start` still publishes synchronously at `internal/heartbeat/publisher.go:247-252`, so the first wire heartbeat now gets the intrinsic bit. |
| Regression test for `capsFn == 0` initial publish | resolved | `TestHeartbeat_InitialStartPublish_IncludesCapAckV1` wires `SetCapabilitiesFn(func() uint32 { return 0 })`, starts the publisher, reads the initial KV value, decodes it, and asserts `SchemaVersion==1` plus `Capabilities&types.CapAckV1 != 0` at `internal/heartbeat/publisher_test.go:487-509`. |
| `TestHeartbeat_CapabilitiesReflectWiredComponents` update | resolved | The test now expects `types.CapAckV1` when the external `capsFn` is zero, expects all bits when `capsFn` returns all bits, and expects `types.CapAckV1` to remain after clearing the external value at `internal/heartbeat/publisher_test.go:435-477`. |

## Spec Compliance

| §4.1 section | Status | Evidence |
|---|---|---|
| Heartbeat payload type | compliant | `types.Heartbeat` includes the planned v1 fields and JSON tags at `types/heartbeat.go:22-32`. |
| Capability constants | compliant | `CapAckV1`, `CapTwoPhaseHandoff`, and `CapProcessingGate` are `1 << 0`, `1 << 1`, and `1 << 2` at `types/heartbeat.go:40-46`. |
| Dual decoder | compliant | JSON payloads beginning with `{` decode as v1 and return JSON errors without legacy fallback at `types/heartbeat.go:79-85`; legacy timestamp fallback returns `SchemaVersion=0` and `Capabilities=0` at `types/heartbeat.go:88-101`. |
| v1 wire format: JSON, `SchemaVersion=1`, `CapAckV1` set | compliant | `publish()` always marshals JSON at `internal/heartbeat/publisher.go:356-366`; `build()` emits `SchemaVersion: 1` and `Capabilities: caps|types.CapAckV1` at `internal/heartbeat/publisher.go:336-345`. |
| Runtime capability reporting API | compliant, with noted divergence | `Manager.SetCapability` and `Capabilities` remain atomic set/clear/load APIs at `manager.go:557-590`; `startHeartbeat` still sets `CapAckV1` after successful publisher start for external `Manager.Capabilities()` observers at `manager_election.go:248-256`. On-wire heartbeats are now `Manager.Capabilities() | CapAckV1`, so a raw manager snapshot can temporarily or manually differ from the wire view for `CapAckV1`. |
| `CapTwoPhaseHandoff` runtime reporting | compliant | Startup calls `setupHandoff` only when enabled, starts the coordinator, and sets `CapTwoPhaseHandoff` only after that successful path at `manager.go:339-352`; disabled mode skips the bit. |
| Writer-side applied snapshot / DTO | compliant | `AppliedAssignment` carries the planned ack fields at `internal/heartbeat/publisher.go:26-36`; `SetAppliedAssignment` copies them into the publisher snapshot at `internal/heartbeat/publisher.go:178-195`. |
| Monotone `SetAppliedAssignment` | compliant | Lower versions return without mutation and equal/higher versions overwrite at `internal/heartbeat/publisher.go:178-195`; equal-version overwrite is asserted at `internal/heartbeat/publisher_test.go:321-360`. |
| `PublishNow` | compliant | `PublishNow` rejects stopped publishers and otherwise calls the shared publish path at `internal/heartbeat/publisher.go:208-216`; immediate reader visibility is covered at `internal/heartbeat/publisher_test.go:393-433`. |
| Reader-side `WorkerMonitor.GetHeartbeats` | not assessed | `internal/assignment/*` is explicitly out of scope for this review. |

## New Findings

### P2 — Document `CapAckV1` manager/wire divergence

The fix makes the wire heartbeat bitmask equal to `capsFn() | types.CapAckV1` at `internal/heartbeat/publisher.go:336-345`, while `Manager.Capabilities()` remains the raw atomic manager bitmask at `manager.go:572-590`. That is correct for the v2 P0: `CapAckV1` is intrinsic to this v1 JSON publisher, so the wire view must not depend on manager startup ordering.

The remaining polish issue is documentation clarity. `manager.go` still says the publisher embeds the current runtime bitmask via `Capabilities()` at `manager.go:580-584` and that `CapAckV1` is set by `startHeartbeat` after successful start at `manager.go:566-567`; both are true for external `Manager.Capabilities()` callers, but incomplete for the on-wire view because clearing `CapAckV1` through `Manager.SetCapability(types.CapAckV1, false)` will not clear the published heartbeat bit. This does not block merge because `Manager.SetCapability`/`Capabilities()` still behave correctly for external observers and `startHeartbeat` sets the manager bit after success at `manager_election.go:253-256`.

No P0 or P1 findings.

## Test Coverage Delta

| Test | Delta |
|---|---|
| `TestHeartbeat_CapabilitiesReflectWiredComponents` | Updated to assert the intrinsic `CapAckV1` bit is present when `capsFn` returns zero and remains present after external caps are cleared at `internal/heartbeat/publisher_test.go:435-477`. |
| `TestHeartbeat_InitialStartPublish_IncludesCapAckV1` | New regression test for v2 P0; proves the synchronous initial `Start` heartbeat includes `CapAckV1` even when the external callback returns zero at `internal/heartbeat/publisher_test.go:487-509`. |

## Lint / Build / Test Status

`make lint` passed:

```text
Checking golangci-lint version...
✓ golangci-lint 2.11.4 is installed
Running linters...
0 issues.
```

`go test ./... -race -count=1` passed:

```text
?   	github.com/arloliu/parti/v2/scripts/inspect_consumers	[no test files]
?   	github.com/arloliu/parti/v2/scripts/trace_visualizer	[no test files]
ok  	github.com/arloliu/parti/v2/source	4.280s
ok  	github.com/arloliu/parti/v2/strategy	1.053s
?   	github.com/arloliu/parti/v2/test/cmd/nats-server	[no test files]
ok  	github.com/arloliu/parti/v2/test/integration/assignment	8.762s
ok  	github.com/arloliu/parti/v2/test/integration/consumer	76.771s
ok  	github.com/arloliu/parti/v2/test/integration/durable	27.966s
ok  	github.com/arloliu/parti/v2/test/integration/failure	51.904s
ok  	github.com/arloliu/parti/v2/test/integration/handoff	12.472s
ok  	github.com/arloliu/parti/v2/test/integration/jsutil	1.303s
ok  	github.com/arloliu/parti/v2/test/integration/manager	87.064s
ok  	github.com/arloliu/parti/v2/test/integration/misc	1.045s
ok  	github.com/arloliu/parti/v2/test/integration/partition	9.915s
ok  	github.com/arloliu/parti/v2/test/integration/stableid	5.608s
?   	github.com/arloliu/parti/v2/test/simulation/cmd/simulation	[no test files]
?   	github.com/arloliu/parti/v2/test/simulation/internal/config	[no test files]
ok  	github.com/arloliu/parti/v2/test/simulation/internal/coordinator	9.308s
?   	github.com/arloliu/parti/v2/test/simulation/internal/logging	[no test files]
ok  	github.com/arloliu/parti/v2/test/simulation/internal/metrics	1.009s
?   	github.com/arloliu/parti/v2/test/simulation/internal/natsutil	[no test files]
?   	github.com/arloliu/parti/v2/test/simulation/internal/producer	[no test files]
ok  	github.com/arloliu/parti/v2/test/simulation/internal/worker	7.008s
ok  	github.com/arloliu/parti/v2/test/stress	7.065s
ok  	github.com/arloliu/parti/v2/types	1.007s
```

`go vet ./...` passed:

```text
<no output>
```

## Verdict

merge. The v2 P0 is fixed, all v1/v2 blocking findings are resolved, and the only new item is P2 documentation clarity around the intentional `CapAckV1` manager/wire divergence.
