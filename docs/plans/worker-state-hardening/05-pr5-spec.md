# PR-5 Implementation Spec — `lastWorkers` Symmetry for `TriggerRebalance` (W3)

Implements **W3** from [`00-fix-plan.md`](./00-fix-plan.md).

`TriggerRebalance` calls `Calculator.rebalance` directly without updating `lastWorkers` on success. The next `observeAndDecide` poll sees `lastWorkers ≠ currentWorkers` and fires a duplicate `planned_scale` rebalance after `cooldown`. `handleRebalance` (the FSM-driven scaling/emergency/restart path) already updates `lastWorkers` on success (`calculator.go:1172-1176`); this PR mirrors that pattern for the manual-trigger path.

## Scope reduction since `00-fix-plan.md`

The original W3 evidence cited three direct-`rebalance` call sites: `TriggerRebalance`, the legacy partition-lifecycle path (`monitorPartitions` → direct `rebalance`), and the rebalance code itself. After PR-3 (commit `89d7fa5`) the partition-lifecycle path goes through the FSM (`triggerPartitionRebalance` → `RunClaimedRebalanceErr` → `handlePartitionRebalance`) and intentionally does NOT refresh `lastWorkers` (so the post-rebalance emergency tail-check sees the pre-rebalance `prev`). Only `TriggerRebalance` remains as the unaddressed direct-`rebalance` caller. The fix is ~3 LOC + 1 test.

---

## 1. Anchors (verified 2026-05-19 against HEAD `35e3287`)

| Anchor | File:line | Status |
|---|---|---|
| `TriggerRebalance` | `internal/assignment/calculator.go:420-428` | **modified** — add `setLastWorkersLocked` after success |
| `handleRebalance` (precedent) | `internal/assignment/calculator.go:1160-1179` | reference only — same shape as the fix |
| `setLastWorkersLocked` | `internal/assignment/calculator.go:1012` | reused |
| `handlePartitionRebalance` (intentionally diverges) | `internal/assignment/calculator.go:699-721` | reference only — confirms why partition lifecycle does NOT mirror this |

## 2. Design

`TriggerRebalance`'s success path needs to call `setLastWorkersLocked(c.currentWorkers)` under `c.mu`, identical to `handleRebalance` lines 1174-1176. No refactor / no helper extraction — the duplication is 3 lines and matches an established pattern.

**Rejected: extract a `finalizeRebalanceSuccess()` helper.** Two call sites, 3 lines each, and the partition-lifecycle path intentionally diverges — a helper would add an abstraction with one knob (whether to update `lastWorkers`) that flips per call site. Lower clarity for the same LOC.

## 3. Implementation

```go
// internal/assignment/calculator.go:420-428 (TriggerRebalance)
func (c *Calculator) TriggerRebalance(ctx context.Context) error {
    if !c.IsStarted() {
        return types.ErrCalculatorNotStarted
    }

    c.Logger.Info("manual rebalance triggered")

    if err := c.rebalance(ctx, "manual-refresh"); err != nil {
        return err
    }

    // Mirror handleRebalance's lastWorkers refresh so the next poll does
    // not re-enter a duplicate planned_scale for the same topology.
    c.mu.Lock()
    c.setLastWorkersLocked(c.currentWorkers)
    c.mu.Unlock()

    return nil
}
```

`errShuttingDown` is NOT special-cased here — `TriggerRebalance` is user-driven, so the caller observes the shutdown error directly (matches the existing return-error-to-caller contract).

## 4. Behavior summary

### Before PR-5

1. Operator calls `TriggerRebalance`.
2. `rebalance` runs, publishes a new assignment, returns nil.
3. `lastWorkers` is unchanged from its pre-trigger value.
4. Next `observeAndDecide` poll sees `lastWorkers ≠ currentWorkers`; `detectRebalanceType` returns `planned_scale`; the calculator enters `Scaling` and rebalances again with no work to do.

### After PR-5

1. Operator calls `TriggerRebalance`.
2. `rebalance` runs, publishes, returns nil.
3. `lastWorkers` is refreshed.
4. Next poll sees `lastWorkers == currentWorkers`; no spurious scaling cycle.

## 5. Tests

**Test 5.1 — `TestTriggerRebalance_NoDuplicateScaleOnNextPoll`** (new test)

- **Intent:** prove that a manual `TriggerRebalance` does not leave a stale `lastWorkers` that would fire a duplicate `planned_scale` on the next poll.
- **Setup:** real calculator + state machine + controllable workers; bring up a stable cluster with 2 workers; wait for `CalcStateIdle`. Capture initial `partitionRebalanceEntries` counter for FSM-driven rebalances OR a different deterministic counter for `rebalance` invocations. Since `TriggerRebalance` bypasses the FSM, the cleanest signal is to count `handleRebalance` invocations via the existing FSM hook (the FSM does NOT call `handleRebalance` for `TriggerRebalance`'s direct path), so use a state subscriber + count of `CalcStateScaling` enter events instead.
- **Action:**
  1. Subscribe to `CalcStateScaling` via `SubscribeToStateChanges`.
  2. Call `calc.TriggerRebalance(ctx)`; wait for return.
  3. Force a poll tick (or wait one `PollInterval`).
  4. Wait a short bounded window (e.g., 2 × `PollInterval`).
- **Assertion:** the `CalcStateScaling` channel receives zero entries during the wait window (no duplicate scaling). A regression where `lastWorkers` stays stale will fire a `Scaling` enter event.
- **File:line target:** new file `internal/assignment/calculator_trigger_rebalance_test.go` (or appended to `calculator_test.go` if a `TriggerRebalance`-related test already lives there — `grep -n "TriggerRebalance" internal/assignment/*_test.go` first).

## 6. Migration / backwards compatibility

No public API change. No metric change. Existing call sites of `TriggerRebalance` (both user code via `Manager.TriggerRebalance` and internal-test usage) gain the symmetric `lastWorkers` refresh; this is a strict improvement over the prior duplicate-cycle behavior.

## 10. Known pre-existing issues NOT addressed by PR-5

- W3 originally also cited the partition-lifecycle path. After PR-3 that path no longer calls `rebalance` directly — it goes through `handlePartitionRebalance` which intentionally does NOT update `lastWorkers` (so the post-rebalance emergency tail-check sees the pre-rebalance `prev`). This divergence is documented in PR-3 spec §3.5 / §10.

## 11. Verification checklist

1. `go build ./...`
2. `go vet ./...`
3. `golangci-lint run ./...` — no new warnings.
4. `go test -count=1 -race -run 'TestTriggerRebalance_' ./internal/assignment/...`
5. `go test ./... -race -count=1 -timeout 10m`

## 12. Model & effort

| Phase | Model / effort |
|---|---|
| Planning (this spec) | Opus 4.7 (direct orchestrator-authored — Sonnet/Haiku tier work, no design call) |
| Implementation | Sonnet 4.6 or direct |
| Plan review | Skipped per `00-fix-plan.md` ("Skip `/plan-review` — too small; LOC is ~10") |
| Post-impl review | Codex **high** v1 |

LOC: ~6 production + ~50 test = ~56 total.
