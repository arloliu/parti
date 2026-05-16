# CapProcessingGate Wiring Plan Review (v3)

## Summary

The v3 plan closes all v2 findings: Test #7 now proves post-updater sampling order, the `CapabilityReporter` Godoc is accurate about caller/race surfaces and OR-only scope, the exact change set names the correct `sync/atomic` import, and the contradictory out-of-scope docstring bullet is gone. Ready to implement: **yes**; no new blocking or polish findings were introduced in this revision.

## v2 Finding Resolution

| v2 finding | Status | Resolution |
| --- | --- | --- |
| P1 — Error-path test can pass without proving post-update sampling | CLOSED | Test #7 now requires a stub updater whose `Capabilities()` returns an initially-zero `atomic.Uint32`, and whose `UpdateWorkerConsumer` stores `CapProcessingGate` before returning an error (`docs/plans/cache-freeze-improvement/plans/cap-processing-gate-wiring.md:365-375`). The assertion explicitly depends on `reported` being 0 before updater execution, so a manager implementation that samples before the updater fails (`docs/plans/cache-freeze-improvement/plans/cap-processing-gate-wiring.md:377-382`). This matches the real source ordering requirement: updater work occurs inside `handoffCoordinator.Apply` (`manager_assignment.go:745-751`; `internal/assignment/handoff/direct.go:28-40`; `internal/assignment/handoff/twophase.go:80-89`), and the planned sample point is after every Apply return (`docs/plans/cache-freeze-improvement/plans/cap-processing-gate-wiring.md:246-258`). |
| P2 — `CapabilityReporter` Godoc misstates who calls `Capabilities` | CLOSED | The Godoc now says only the manager-apply goroutine calls `reportConsumerCapabilities` after each `handoffCoordinator.Apply` attempt, and explicitly says the heartbeat publisher does **not** call `Capabilities()` directly (`docs/plans/cache-freeze-improvement/plans/cap-processing-gate-wiring.md:76-84`). That matches current heartbeat wiring: the publisher is given `m.Capabilities` (`manager_election.go:278-280`), and heartbeat composition calls only `p.capsFn()` (`internal/heartbeat/publisher.go:336-340`). |
| P2 — Godoc says the manager bitmask is OR-only, but `SetCapability` can clear bits | CLOSED | The Godoc now scopes OR-only behavior to the reporter integration and explicitly notes that `Manager.SetCapability` can clear bits via `active=false` (`docs/plans/cache-freeze-improvement/plans/cap-processing-gate-wiring.md:96-102`). This matches source, where `SetCapability` uses `Or` when active and `And(^capBit)` when inactive (`manager.go:740-745`). |
| P2 — Exact change set omits the needed `sync/atomic` import | CLOSED | The internal change set now says to add `sync/atomic` and says `types` is already imported (`docs/plans/cache-freeze-improvement/plans/cap-processing-gate-wiring.md:315-323`). That matches the current import block, which has `sync` but not `sync/atomic`, and already imports `github.com/arloliu/parti/v2/types` (`internal/durable/worker_consumer.go:3-20`). |
| P2 — `manager.go` docstring update is both in-scope and out-of-scope | CLOSED | The exact change set keeps the `manager.go:732` docstring fix in scope (`docs/plans/cache-freeze-improvement/plans/cap-processing-gate-wiring.md:325-329`), and the out-of-scope section no longer lists it (`docs/plans/cache-freeze-improvement/plans/cap-processing-gate-wiring.md:430-435`). The source comment remains stale today and is correctly included in the planned edit (`manager.go:725-735`). |

## New Findings

None.

## Verdict

**merge-ready** — all v2 findings are closed, and no new P0/P1/P2 issues were found.
