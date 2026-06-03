# 03 — exit-on-confirmed-degraded (consolidation Phase 4.7)

**Status:** IMPLEMENTED + full gate GREEN (lint 0, unit -race, integration -race); codex plan-review
and post-impl review both clean (0 P0/P1).
**Branch / worktree:** `exit-on-confirmed-degraded` off `main` (`760f351`).
**Parent plan:** `02-degraded-record-pointer-plan.md` §5 documented this as a PRE-EXISTING race left
out of scope ("4.7 exit-on-confirmed-degraded"). This is that follow-up.

## 1. Goal and framing

`exitDegraded` must clear the degraded record ONLY on a genuine `Degraded → Stable` transition. Today
it calls `transitionState(StateStable)`, which returns `true` **vacuously** when the state is already
`StateStable` (`manager_state.go:106`, `from == to`), and then unconditionally `Store(nil)`s the
record. That vacuous-success path lets a recovery tick clear a record that a *concurrent* enterer is
mid-publishing — stranding the worker in `Degraded` with a `nil` record.

**This is a behaviour change, narrowly scoped:** it removes one reachable stuck-Degraded outcome. It
is NOT a pure refactor (unlike 4.6). The only externally-observable delta is that an `exitDegraded`
call which today vacuously "succeeds" against an already-`Stable` state will instead become a no-op
(record preserved), so the in-flight degrade episode completes and recovers normally on the next tick.

## 2. The race (pre-existing; widened 7 lines by 4.6, not introduced by it)

`enterDegraded` ordering: publish record (CAS) **then** `transitionState(StateDegraded)`.
`exitDegraded` ordering: `transitionState(StateStable)` **then** `Store(nil)`.

Stranding sequence (clean start: `record==nil`, `state==Stable`):

1. Goroutine **A** (NOT the connection monitor — e.g. startup watchdog, `onEnumerationStall`, the
   assignment watcher, or `recordKVError` off the election/assignment path) calls
   `enterDegraded(reason)`: `CompareAndSwap(nil, rec)` publishes the record, but A has not yet reached
   `transitionState(StateDegraded)`. State is still `Stable`.
2. The connection-monitor goroutine **B** runs `attemptRecoveryFromDegraded` on its 1 s tick, loads
   A's record (non-nil), passes the gates, and calls `exitDegraded`:
   `transitionState(StateStable)` is **vacuous** (state is `Stable`) → returns `true` →
   `Store(nil)` wipes A's record.
3. A resumes: `transitionState(StateDegraded)` → **final state = `Degraded` + record `nil`**.

That stuck state defeats recovery (`attemptRecoveryFromDegraded` returns early on a nil record) and
alerting (`monitorDegradedAlerts` returns on a nil record). It self-heals only when some *independent*
`enterDegraded` re-arms the record (CAS succeeds on nil → vacuous `Degraded→Degraded` → record set
again) — which, if A's degrade condition cleared immediately, may not happen until the next unrelated
degrade event.

**Scope of the trigger:** ANY degrade reason can reach the strand — including the reason-scoped ones.
The reason-scoped gates (Family B kv-unavailable, NP-10 enumeration-stall) are immune only to
*stale / pre-degrade* signals: `recoverySignalStalled` blocks when `signalAt == 0 || signalAt <= since`
(`manager_degraded.go:415`). But `recordKVHealthyOp` and `recordEnumerationSuccess` stamp their signals
**unconditionally — even while degraded** (`manager_degraded.go:239`, `:392`), so a success landing in
the post-record / pre-transition window has `signalAt > rec.since` and **opens** the gate, letting B
reach `exitDegraded`. This is exactly the "reason-scoped self-protection is only partial" caveat the
02 plan already documented (`02-degraded-record-pointer-plan.md:133`). Non-reason-scoped reasons
(startup-timeout / NP-5 / generic) reach the strand the most directly (commitment guard alone), but the
fix closes the window for **all** reasons uniformly. The realistic regime is startup churn / many
goroutines entering degraded under CPU pressure.

**Pre-existing-on-parent:** the two-atomic design reached the identical outcome (`Degraded` +
`since==0`). 4.6 only widened the record-visible point from reason-store→transition to CAS→transition
(~7 lines). `manager_degraded_window_race_test.go` is deliberately parent-relative for this reason and
explicitly documents this strand as out-of-scope-there / in-scope-here.

## 3. The fix

Replace the vacuous-tolerant transition in `exitDegraded` with a **confirmed-Degraded CAS**, mirroring
the existing `casToStableFromWaitingAssignment` lock-free idiom (referenced at `manager_state.go:140`):
commit the state with a from-specific CAS, then fire `emitTransitionEffects` so observers see an
identical transition regardless of path.

```go
func (m *Manager) exitDegraded() {
    rec := m.degraded.Load()
    if rec == nil {
        return
    }

    duration := time.Since(time.Unix(0, rec.since))

    // Only a genuine Degraded->Stable transition may clear the record. A vacuous
    // Stable->Stable "success" (the state a concurrent enterDegraded is in after it
    // published the record but before it transitioned to Degraded) must NOT clear
    // it — doing so strands that episode in Degraded with a nil record.
    if !m.state.CompareAndSwap(int32(StateDegraded), int32(StateStable)) {
        return
    }
    m.emitTransitionEffects(StateDegraded, StateStable)

    m.degraded.Store(nil)
    // ... unchanged: log, metrics, leader recovery-grace ...
}
```

`emitTransitionEffects(from, to)` is the shared side-effect emitter (`manager_state.go:145`): the
structured log line, the `OnStateChanged` hook (via `invokeHook`, so `Stop`'s WaitGroup waits), and
`RecordStateTransition`. Calling it with `(StateDegraded, StateStable)` reproduces exactly what
`transitionState(StateStable)` emitted on the real-transition path, so observers see no difference.

**Shutdown handling preserved:** today, if the state is `Shutdown`, `transitionState(StateStable)`
returns false (invalid transition) and `exitDegraded` skips cleanup. Under the CAS, a `Shutdown` state
also fails `CompareAndSwap(Degraded, Stable)` and skips cleanup — same behaviour. **Observability delta
(intentional):** on that skipped path the old `transitionState` logged an "invalid state transition
attempted" error on its first attempt (`manager_state.go:109`); the bare CAS does not. This is by
design — a recovery tick racing a concurrent Shutdown (or a pre-transition enter) is a benign,
expected non-event, not an operator-actionable error, so suppressing that log removes noise rather than
hiding a fault. The genuine-exit log ("exiting degraded mode") is unchanged.

## 4. Tightness argument (no legitimate exit is newly blocked)

A non-nil record is set ONLY by `enterDegraded` (the single CAS publish). `isValidTransition`
(`manager_state.go:180`) forbids `StateDegraded → {anything but Stable, Shutdown}`. Therefore, whenever
`exitDegraded` runs against a *real* degrade episode, the state is genuinely `Degraded` (A's enter has
completed its own `transitionState(StateDegraded)`), so `CompareAndSwap(Degraded, Stable)` succeeds and
behaviour is unchanged. The ONLY case the CAS newly refuses is `state != Degraded` while a record is
present — i.e. exactly the racy transient (record published, transition not yet run) or a `Shutdown`
that already skipped cleanup. So the fix cannot regress a real recovery; it removes precisely the
strand.

## 5. Verify-first / proof obligation

The fix's *contract* is deterministically testable without any production seam or scheduler stress —
this is what makes the change cheap and the proof non-vacuous. Confirmed empirically during
investigation (RED on `main` `760f351`, GREEN with the fix):

1. **Strand reproducer (deterministic, white-box, table-driven):** arm `{state = <S>, record != nil}`
   for **every** state an enterer can be in during the pre-transition window — `StateStable`,
   `StateWaitingAssignment`, `StateScaling`, `StateRebalancing`, `StateEmergency` (all valid
   `*→Degraded` sources, and all today valid `*→Stable` so the old vacuous/real `transitionState`
   would clear the record from any of them) — then call `exitDegraded()` directly and assert the record
   **survives** (`m.degraded.Load() != nil`), the state is **unchanged** (no spurious transition out of
   `<S>`), and **no `OnStateChanged` fired** (no spurious effect emission). On the parent this is RED
   (the record is cleared, and for the non-Stable states the state is also dragged to Stable); with the
   fix it is GREEN. Uses the existing `markDegraded` test helper + `m.state.Store(int32(<S>))` (the
   field-poke pattern `manager_epoch_fence_test.go` already uses). This is the load-bearing proof:
   "exitDegraded won't clear unless it genuinely transitioned from Degraded" IS the mechanism that
   prevents stranding.
2. **Positive path unchanged:** arm `{state = StateDegraded, record != nil}`, call `exitDegraded()`,
   assert the record is cleared, state is `StateStable`, and the `OnStateChanged` hook fired with
   `(Degraded, Stable)` — i.e. the genuine exit still works and still emits its effects.
3. **Behaviour-preservation gate (must stay green, unchanged):** the existing degraded/recovery
   suite — `manager_recovery_conjuncts_test.go`, `manager_kv_read_unavailable_test.go`,
   `manager_enumeration_stall_recovery_test.go`, `manager_degraded_recovery_selfheal_test.go`,
   `manager_degraded_test.go`, `manager_alert_level_test.go`, `manager_np5_blocked_apply_recovery_test.go`,
   `manager_epoch_fence_test.go`, `manager_stream_missing_observer_test.go`,
   `manager_degraded_window_race_test.go`, and the np2 integration test.
4. **Window stress, now strengthened (optional but cheap):** `manager_degraded_window_race_test.go` is
   currently parent-relative (asserts record atomicity + liveness, NOT state-vs-record consistency).
   With the fix the strand is closed, so this test MAY be tightened to also assert "never ends in
   `Degraded` + nil record" after the storm drains. Decide during impl whether to tighten it here or
   keep it parent-relative and rely on proofs 1–2 (leaning: add a post-drain assertion, since the
   stronger invariant now holds).

## 6. Blast radius and concrete edits

**Production — `manager_degraded.go` only:** the `exitDegraded` transition block (§3). No signature
changes, no new fields, no other call sites (`exitDegraded` is called only from
`attemptRecoveryFromDegraded`). `emitTransitionEffects` is already exported within the package and
already used by both transition paths, so no new surface.

**Tests:** add the two deterministic proofs (§5.1, §5.2) — likely one new file
`manager_exit_confirmed_test.go`. Optionally tighten `manager_degraded_window_race_test.go` (§5.4).

## 7. Sequencing and gate

`plan → codex plan-review until clean → write §5.1 reproducer + confirm RED on parent → implement →
confirm §5.1/§5.2 GREEN → make lint (0) → go test -race ./... (unit) → go test -race
./test/integration/... → /simplify → /post-impl-review (codex ≠ implementer) to MERGE → squash by
scope → PR → CI`. Keep ≤1 step unverified.

## 8. Invariants ledger

- **NEW (the change's purpose):** `exitDegraded` clears the record iff it performed a genuine
  `Degraded → Stable` transition. Consequence: a worker can no longer end in `Degraded` + nil record
  via the pre-transition exit window.
- **T1** (recovery-gate dual nature) — unchanged: the gates in `attemptRecoveryFromDegraded` are
  untouched; only the final `exitDegraded`'s transition mechanism changes.
- **T4** (record pair-atomicity, from 4.6) — unchanged: still one atomic swap.
- **Transition effects parity** — `emitTransitionEffects(Degraded, Stable)` reproduces exactly the
  log/hook/metric the prior `transitionState(StateStable)` emitted on its real-transition path, so
  no observer sees a difference on the genuine-exit path.
- **Shutdown** — a `Shutdown` state fails the CAS and skips cleanup, identical to today's
  `transitionState` returning false from `Shutdown`.
