# 02 — degraded-record-pointer (consolidation Phase 4.6)

**Status:** DRAFT (plan written; codex plan-review pending; not started)
**Branch / worktree:** `degraded-record-pointer` off `main` (`4ab9d43`).
**Parent plan:** `00-investigation-and-consolidation-plan.md` (this is the value-add version of
the declined step 3.1 `degraded-state-struct`).

## 1. Goal and framing

Collapse the two degraded-state atomics

```go
degradedSince      atomic.Int64  // UnixNano, 0 = not degraded; CAS(0,now) is the entry gate
lastDegradedReason atomic.Value  // string; stored AFTER the CAS, cleared BEFORE since
```

into one

```go
type degradedRecord struct {
    since  int64  // UnixNano when degraded entered
    reason string // active degrade reason
}
degraded atomic.Pointer[degradedRecord] // nil = not degraded
```

so `{since, reason}` are published in a **single atomic swap**. This is the structural enforcement
of **T4** (the store-after-CAS / clear-before-since ordering): with one pointer the ordering is no
longer a hand-maintained invariant across two atomics — it is impossible to express an inconsistent
pair.

**This is a behaviour-preserving PURE REFACTOR, not a bug fix.** It is framed that way deliberately
(see §3): the goroutine model means there is no reachable torn read to "fix." The one *observable*
delta is the deletion of a now-dead defensive branch (the empty-reason recovery gate, §4), which the
single swap makes unreachable.

`atomic.Pointer[T]` already has five precedents on the `Manager` struct (`lastSeenAlias`,
`lastObservedCommit`, `stashedCommit`, `stashedApplyRetry`, `committedAssignment`), so this is an
idiomatic, in-house pattern.

**In scope:** exactly the two fields above. **Out of scope (was the declined 3.1):** the other
degraded atomics — `connDownSince`, `connUpSince`, `recoveryGraceStart`, `inRecoveryGrace`,
`kvErrorCount`/`kvErrorWindow`. They are a different concern (connectivity / recovery-grace / KV-error
window) and folding them is not justified here.

## 2. Current design (what we are replacing)

`enterDegraded(reason)` (manager_degraded.go ~291):
1. reject if `State()==StateShutdown`;
2. `now := time.Now()`; **CAS** `degradedSince 0 → now.UnixNano()` — the sole-winner entry gate; a
   failed CAS (already degraded) returns immediately (a true **no-op**: it does not touch `since` or
   `reason`);
3. `lastDegradedReason.Store(reason)` — only the CAS winner reaches here (no loser-clobber);
4. `transitionState(StateDegraded)`; on failure roll back **reason then since** (`Store("")` then
   `Store(0)`), preserving the happens-before;
5. log + `OnDegraded` hook + `SetDegradedMode(1)` + start the alert monitor.

`exitDegraded()` (~339): load `since` (return if 0) → `transitionState(StateStable)` (return if it
fails) → **clear reason then since** (`Store("")` then `Store(0)`) → log + metrics + (leader) enter
recovery grace.

`recoverySignalStalled(reason, scopedReason, signalAt, leaderOnly)` (~412) loads `since =
degradedSince.Load()` **independently** of the `reason` read in `attemptRecoveryFromDegraded`.

`attemptRecoveryFromDegraded()` (~426), connection-monitor goroutine: guard `degradedSince==0`;
refresh; record KV success; commitment guard (`currentAssignmentApplied`); **empty-reason gate**
(§4); Family B kv-unavailable gate; Family A epoch-mismatch; heartbeat-bucket backstop; NP-10
enumeration gate; else `exitDegraded()`.

Other readers: alert monitor (~685), the two `degradedSince!=0` early-returns (~150, ~246).
`emitDegradedAlert`/`calculateAlertLevel` take a `degradedSince time.Time` **parameter** (not the
field) — unaffected except the call site that reads the field to build that argument.

## 3. Goroutine model (verified — this is the load-bearing fact)

- **`exitDegraded` is called only from `attemptRecoveryFromDegraded`, which is called only from the
  connection-monitor goroutine (manager_degraded.go:119).** Exit and the entire recovery cascade run
  on a **single goroutine**.
- **`enterDegraded` fires from many goroutines** (startup watchdog, setup, the connection monitor,
  the calculator's enumeration-stall callback, the assignment watcher, and `recordKVError` from the
  election/assignment paths) — **but enter-while-degraded is a no-op CAS** (fails on a non-nil
  record), so a concurrent enter cannot mutate `since`/`reason` while already degraded.

**Consequence (corrects an earlier hypothesis):** within a recovery tick, `since` and `reason` are
stable — exit is same-goroutine, and concurrent enter is a no-op. So the separate loads of `reason`
(line 468) and `since` (line 420) **cannot** straddle an exit+re-enter. **There is no reachable torn
read**, hence no bug to fix. A `-race` "reproducer" for it would pass on the parent — the verify-first
STOP signal and the documented vacuous-proof trap. The refactor is justified on simplicity +
structural-T4 grounds, not correctness.

## 4. The one behavioural delta: deleting the empty-reason recovery gate

manager_degraded.go:464-471:
```go
reason, _ := m.lastDegradedReason.Load().(string)
if reason == "" {
    return // stay degraded this tick
}
```
This guards the **enter-side cross-goroutine window**: goroutine A's `enterDegraded` has won the CAS
(`since` set) but not yet stored `reason` (step 2→3); goroutine B (monitor) reads `since!=0` but
`reason==""`. The gate keeps B degraded that tick.

With one pointer, A publishes `since` and `reason` in a **single swap**, so B can never observe
`since`-without-`reason`. The branch becomes **structurally dead** and is deleted. Its replacement is
a *construction argument* (the record is built with both fields before it is ever visible) plus a
`-race` stress proof (§7).

## 5. The widened enter/transition window — a PRE-EXISTING race this refactor does NOT solve

`enterDegraded` keeps the **asymmetric ordering**: set the record **then** `transitionState`
(StateDegraded); `exitDegraded` does `transitionState`(StateStable) **then** clear the record. This
ordering is correct for the *pair* invariant (§7.1), but it does **not** make "Degraded state ⟹
record present" an absolute guarantee — and the parent's two-atomic version does not either. Earlier
drafts overstated this; the honest analysis follows (codex plan-review P1×2):

The transient: "record present but `state` not yet `StateDegraded`." Today this window is steps 3→4
(`reason` stored at :307, transition at :313). The pointer moves the record-visible point back to the
CAS, **widening the window from `:307→:313` to `:300→:313`** — 7 lines, all *before* the same
`transitionState` call.

**Why "Degraded ⟹ record present" is NOT absolute (pre-existing):** a monitor tick (B) can observe
`record != nil` while enter-goroutine A is mid-enter (between record-publish and
`transitionState(StateDegraded)`). For a **non-reason-scoped** reason whose recovery is satisfied by
the commitment guard alone, B reaches `exitDegraded` → `transitionState(StateStable)`, which treats
"already Stable" as success (manager_state.go:105-107) — a **vacuous** transition — then clears the
record. If A's `transitionState(StateDegraded)` then runs, the final state is **Degraded with a nil
record** (parent: Degraded with `degradedSince==0` — the *identical* pre-existing outcome, since the
parent's exit also clears before A's transition completes). Closing this would require guarding
`exitDegraded` to clear only on an actual `Degraded→Stable` transition — a **distinct behaviour
change, out of scope for 4.6** (noted as a potential follow-up, "4.7 exit-on-confirmed-degraded").

**Reason-scoped self-protection is only partial (codex P1):** the `signal <= since` argument holds
only for recovery signals stamped *before or at* the record's `since`. But `recordKVHealthyOp`
(:240-244) and `recordEnumerationSuccess` (:391-395) stamp `time.Now()`, so a success landing in the
post-record/pre-transition window can make `signalAt > since` and **open** a reason-scoped gate. So
kv-unavailable / enumeration are NOT structurally immune in the window either — only immune to
stale/pre-degrade signals.

**Plan position:** preserve the asymmetric ordering exactly; document this window as a PRE-EXISTING
race the refactor neither introduces nor solves; widen the existing window by 7 lines only. The §7.3
proof is therefore **parent-relative**: the same concurrency harness (incl. post-record signal
advancement) must show the child exhibits **no failure mode the parent does not also exhibit** — it is
NOT an absolute "no Degraded+nil-record" assertion (that state is pre-existing-reachable on both).

## 6. Blast radius and concrete edits

**Production — all in manager_degraded.go unless noted:**
- manager.go: replace the two field decls (and their ~20-line comment block) with the `degradedRecord`
  type + `degraded atomic.Pointer[degradedRecord]`, with a tightened comment.
- `enterDegraded`: build `rec := &degradedRecord{since: now.UnixNano(), reason: reason}`;
  `CompareAndSwap(nil, rec)` as the entry gate; rollback `Store(nil)` on transition failure.
- `exitDegraded`: `rec := m.degraded.Load()` (return if nil) for `since`; after a successful
  transition, `m.degraded.Store(nil)`.
- `recoverySignalStalled`: take `since` from the record the caller already loaded (pass it in, or load
  once) so reason and since are read from the **same** record — keeps the gates consistent and avoids
  reintroducing two independent loads. Likely signature change: accept `since int64` instead of
  loading it. Confirm both call sites (:482 kv-unavailable, :517 enumeration) pass the record's since.
- `attemptRecoveryFromDegraded`: `rec := m.degraded.Load(); if rec == nil { return }`; use
  `rec.reason` / `rec.since`; **delete** the empty-reason gate (§4).
- the two `degradedSince.Load()!=0` early-returns (:150, :246) → `m.degraded.Load()!=nil`.
- alert monitor (:685) → read `rec := m.degraded.Load()`; guard nil; use `rec.since`.
- Consider a small `m.isDegraded() bool` / `m.degradedSinceNanos() int64` helper to avoid repeating
  `Load()!=nil` / nil-checks (optional; only if it reduces, not adds, surface).

**Tests (14 files reference the fields):** mechanical port of field accesses. Two need real work:
- **manager_recovery_emptyreason_test.go** — arms an **unrepresentable** state
  (`lastDegradedReason.Store("")` with `since` set). It cannot be ported. **Delete it, but only after
  its replacement (§7) is green**, and state in the commit that the window it guarded is now
  structurally impossible.
- **manager_reason_ownership_test.go** — white-box pins the store-after-CAS / clear-before-since field
  ordering. That ordering no longer exists as two stores. **Re-express** it to pin the new invariant:
  a reader of `m.degraded` sees either nil or a fully-populated `{since!=0, reason!=""}` record — never
  a partial pair.

## 7. Verify-first / proof obligation

Not a bug reproducer (no reachable bug — §3). The proof set, written/confirmed **before** the impl
where it can fail on the parent, and asserted green after:

1. **Record-atomicity `-race` stress test (the replacement for emptyreason):** spawn N goroutines
   hammering `enterDegraded`/`exitDegraded`/re-enter (including a non-reason-scoped reason) while a
   reader goroutine spins `m.degraded.Load()` and asserts the loaded record is **either nil or fully
   populated** — never a partial pair (`since!=0 && reason==""` or `since==0 && reason!=""`). Must be
   **RED under a split-publish perturbation** (a deliberately two-step set-since-then-set-reason
   variant) and GREEN with the single swap — i.e. non-vacuous. **Do NOT** assert "never StateStable
   with a non-nil record": the asymmetric ordering (§5) deliberately exposes Stable+record transiently
   on both enter and exit, so that assertion would (correctly) fail the design — it is not a valid
   invariant (codex P1).
2. **Existing recovery proofs stay green** unchanged in behaviour: `manager_recovery_conjuncts_test.go`,
   `manager_kv_read_unavailable_test.go`, `manager_enumeration_stall_recovery_test.go`,
   `manager_degraded_recovery_selfheal_test.go`, `manager_alert_level_test.go`,
   `manager_np5_blocked_apply_recovery_test.go`, `manager_epoch_fence_test.go`,
   `manager_stream_missing_observer_test.go`, and the np2 integration test — these are the
   behaviour-preservation gate.
3. **The widened-window check (§5) — PARENT-RELATIVE:** a focused `-race` test driving
   `enterDegraded("startup-timeout")` concurrently with `attemptRecoveryFromDegraded`, **including a
   `recordKVHealthyOp`/`recordEnumerationSuccess` stamp during the window** (to exercise the
   signal-advancement opening in §5). Run the SAME harness against the parent (two-atomic) and the
   child (pointer); assert the child exhibits **no failure mode the parent does not** (e.g.
   stuck-degraded after a Degraded+nil-record / Degraded+`since==0` outcome). This is the guard that
   the 7-line-wider window introduces nothing new — NOT an absolute-invariant assertion. If it surfaces
   the pre-existing race on BOTH, that is a documented pre-existing issue (the §5 "4.7" follow-up), not
   something to silently fix here.

## 8. Sequencing and gate

`plan → codex plan-review until clean → write/confirm §7 proofs → implement → make lint (0) → go test
-race ./... (unit) → go test -race ./test/integration/... → /simplify → /post-impl-review (codex
≠ implementer) to MERGE → squash by scope → PR → CI`. Keep ≤1 step unverified.

## 9. Invariants ledger

- **T4** — the `{since, reason}` **pair** atomicity (no partial pair ever observable) becomes
  structurally impossible to violate via the single swap. This is the change's purpose. Note the
  scope: T4-as-pair-atomicity is enforced; it does NOT make *state-vs-record* consistency absolute —
  that is a separate, pre-existing-racy property (§5) left out of scope.
- **T1** (recovery-gate dual nature) — unchanged: the two reason-scoped gates + the global backstops
  keep their logic and order; only their `since`/`reason` source changes to one record.
- **T5** (whole-bucket-loss is the only path to "KV error threshold exceeded") — untouched.
- **No new race class** — §5 widens an existing pre-transition window by 7 lines, all before the same
  serializing `transitionState`; the parent-relative §7.3 test is the guard that it introduces nothing
  the parent does not already exhibit.
