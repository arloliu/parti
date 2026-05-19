# PR-3 Implementation Spec — `Manager.State()` Reconcile + Partition-Lifecycle FSM Routing (W1+W5)

Implements **W1** (merged W1 + W5) from [`00-fix-plan.md`](./00-fix-plan.md).

`Manager.State()` lies during real leader-side work, via **two independent causes** that together produce one observable symptom (dashboards / hooks / readiness probes seeing `Stable` while a rebalance is in flight). The audit (`tmp/worker_state_analysis/00-report.md:275-291`) and consolidated review (`tmp/worker_state_analysis/02-consolidated.md`) explicitly require a single PR that closes BOTH — half-fixing leaves half the symptom.

- **Cause A — subscriber-drop / no reconcile.** `stateSubscriber.trySend` drops on a full 4-slot buffer (`internal/assignment/state_subscriber.go:24-29`, `state_machine.go:119`); the manager-side consumer `monitorCalculatorState` is purely event-driven with no fallback (`manager_assignment.go:166-193`). A burst of `Idle→Scaling→Rebalancing→Idle` transitions can saturate the buffer; any drop is silent.
- **Cause B — `monitorPartitions` bypasses the FSM.** Partition-source change events drive `triggerPartitionRebalance` → `rebalance` directly (`internal/assignment/calculator.go:645-656, 675-728`) without entering the calculator state machine. The public contract — `Rebalancing` means "active partition rebalance in progress" (`types/state.go:35-36`, `types/calculator_state.go:24-26`) and `Stable, Scaling, Rebalancing` are the documented readiness-OK states (`docs/OPERATIONS.md:324`, `docs/LIFECYCLE.md:66`) — is therefore violated whenever a partition lifecycle drives a rebalance.

**Resolution: PR-3 fixes both causes in one bundle.**

1. **Cause A.** Add a periodic reconcile tick to `monitorCalculatorState` that reads `calc.GetState()` and re-drives `syncStateFromCalculator` if it differs from the last value the manager has applied. Subscriber stays drop-tolerant; recovery cadence becomes deterministic. Mirrors PR-1's alias-watcher reconcile shape (`manager_assignment.go:282-319`). Invariant is narrowed: reconcile guarantees **eventual projection of the current calculator state**, NOT replay of every missed transition (see §2.1, §4, §10.2).
2. **Cause B.** Route partition-lifecycle rebalances through the calculator state machine via a new strict-source claim primitive (`TryClaimRebalancing(from=Idle)`) plus a partition-specific runner (`RunClaimedRebalanceErr`) that preserves the callback's error so `restorePendingOnGraceBail` keeps working. After the partition rebalance returns to Idle, the path performs a single in-line tail-check via `observeAndDecide` so an emergency claim that lost the race during the rebalance window is retried without waiting for the next poll. On claim failure (something else is already running a rebalance), the existing `pendingPartitionUpdate` / drain-ticker path re-triggers naturally; duplicate-rebalance bound is explicitly characterized in §4.

**Revision history:** this spec has cleared five revision rounds (v1 → v5). The summary bullets below cover v1 → v3 (the rounds that closed all P0 findings); the full row-per-revision table including v4 (Test 5.3 signal + Test 5.7 timeout override + 15 s derivation) and v5 (Test 5.7 `partitionRebalanceBlocker` determinism seam — final) is at the bottom of this file. **Current revision: v5 (final, ready to implement).**

- v1 — initial draft against `00-fix-plan.md` and `tmp/worker_state_analysis/00-report.md` v3. Plan-review (`tmp/03-pr3-spec_pr3-w1-w5_review.md`) returned 2 P0 / 3 P1 / 1 P2:
  - P0-A (`errShuttingDown` restore lost): the v1 design routed partition lifecycle through `RunClaimedRebalance` whose callback (`handleRebalance`) swallows `errShuttingDown` to nil; the original cause never reaches `restorePendingOnGraceBail`. **v2 fix:** add a new partition-specific runner `RunClaimedRebalanceErr` that returns the callback's error to the caller while still returning the FSM to Idle. `restorePendingOnGraceBail` stays load-bearing.
  - P0-B (emergency drop with no marker): under Option X the partition path claims `Rebalancing`, so concurrent `TryClaimEmergency` rejects from non-Idle; no marker exists to retry. **v2 fix:** after `RunClaimedRebalanceErr` completes and FSM is back to Idle, the partition-lifecycle path performs an in-line tail-check via `c.observeAndDecide(ctx, nil)` — a single-goroutine handoff that immediately re-detects any emergency that arrived during the rebalance window.
  - P1-A (duplicate partition rebalance on claim-failure retry): made explicit in §4 behavior table. The existing `snapshotSource` re-read inside every `rebalance` (`calculator.go:1111-1124`) means at most one extra `Stable → Rebalancing → Stable` cycle whose body is a no-op publish; bounded by one drain-tick interval. Test 5.3 is **required** (not optional) and asserts the duplicate is bounded.
  - P1-B (reconcile cannot replay completed transitions): §1 framing, §2.1 decision, §4 behavior table, and Test 5.1 narrowed to "eventual projection of current state, not replay of missed transitions." A stronger invariant would require a lossless event path — out of PR-3 scope (§10.3).
  - P1-C (hook firing order): §4 reworded to distinguish "calculator notification enqueue order" (deterministic, FSM-side) from "user hook execution order" (asynchronous, dispatched via `invokeHook` at `manager.go:827-839`). Test 5.2 asserts eventual presence of both transitions in the recorded stream without requiring order vs assignment hooks.
  - P2 (public glossary worker-count-only): one-line glossary edit added to PR-3 scope (`docs/REFERENCE.md:358`).
- v2 (2026-05-19) — closes all P0/P1/P2 from `tmp/03-pr3-spec_pr3-w1-w5_review.md`. Design Call 1 unchanged (Option A). Design Call 2 still Option X but with two additions: (i) partition-specific `RunClaimedRebalanceErr` runner that propagates the callback error, (ii) in-line emergency tail-check after the partition rebalance returns to Idle. Tests: 4 required (was 2 required + 1 optional), one optional. LOC: ~88 (was ~70). Plan-review v2 (`tmp/03-pr3-spec_pr3-w1-w5_v2_review.md`) returned 1 residual P0 + 2 P1: P0-B tail-check could reuse an exhausted rebalance context; Test 5.5 used the wrong manager state for Emergency and required a forbidden cross-tuple hook order; Test 5.3 measured an async/drop-tolerant signal that could not prove the duplicate bound.
- v3 — closes the v2 plan-review findings. Residual P0-B: §3.5 allocates a **fresh stop-aware context** for the tail-check (`ctxFromStopCh(c.stopCh, partitionTailCheckTimeout)` with `partitionTailCheckTimeout = 15 * time.Second`) so it cannot be starved by an exhausted `reqCtx`. Test 5.7 added as a dedicated context-exhaustion regression (kept separate from Test 5.5 to keep the existing emergency-contention test focused on the FSM ordering rather than on context-exhaustion timing). P1 — Test 5.5 corrected to assert manager `Stable → Emergency → Stable` (per `manager_state.go:230-234`, `types/state.go:38-39`) and to assert **eventual presence** of all four `(from,to)` tuples without ordering between the two cycles (per async `invokeHook` at `manager.go:827-839`); a residual ordering parenthetical in Test 5.2 was also removed. P1 — Test 5.3 retargeted to count `TryClaimRebalancing` invocations via a test-only `export_test.go` counter on the calculator (lowest-surface deterministic producer-side signal vs. publisher / callback wrappers); fails if more than one extra `TryClaimRebalancing` runs per dropped partition update. §10.1 updated: "tail-check context exhaustion" is now closed; "second emergency after tail-check completes" remains the only documented residual. LOC: ~90 (was ~88; fresh-context allocation + named timeout const adds ~3 LOC of production code; test corrections add no production LOC). Ceiling raised to ~92.

---

## 1. Anchors (verified 2026-05-19 against HEAD `96359fe`)

| Anchor | File:line | Status |
|---|---|---|
| `stateSubscriber.trySend` (drops on full buffer) | `internal/assignment/state_subscriber.go:17-30` | **reused** — drop-tolerant by design; not changed by PR-3 |
| State subscriber buffer size (`make(chan, 4)`) | `internal/assignment/state_machine.go:119` | **reused** |
| `monitorCalculatorState` (manager subscriber consumer) | `manager_assignment.go:166-193` | **rewritten** — add periodic reconcile arm |
| `monitorCalculatorState` startup wiring (readyCh after subscribe) | `manager_assignment.go:135-137, 169-171` | **reused** — preserve subscribe-before-Start order |
| `syncStateFromCalculator` (calc state → manager state mapping) | `manager_state.go:181-246` | **reused** — already idempotent under "skip if same target"; reconcile re-call is safe |
| `transitionState` + `isValidTransition` | `manager_state.go:90-179` | **reused** — `isValidTransition` already permits the loops PR-3 introduces (see §2.3) |
| `Manager.invokeHook` (async hook dispatch via goroutine) | `manager.go:827-839` | **reused** — confirms hook execution order is NOT FSM-deterministic; informs §4 wording |
| `assignmentCalculator` interface | `manager.go:182-189` | **modified** — adds `GetState() types.CalculatorState` so `monitorCalculatorState` can run a reconcile read without depending on the concrete type |
| `Calculator.GetState` (concrete impl exists) | `internal/assignment/calculator.go:378-384` | **reused** — already returns `c.stateMach.GetState()` |
| `NopCalculator.GetState` (must exist after interface widening) | `internal/assignment/nop_calculator.go` (location confirmed via grep before impl) | **added** if not present — returns `types.CalcStateIdle` |
| `monitorPartitions` select loop | `internal/assignment/calculator.go:675-728` | **modified** — both arms (immediate + drain-ticker deferred) route through FSM claim |
| `triggerPartitionRebalance` (direct rebalance call) | `internal/assignment/calculator.go:645-656` | **modified** — replaced by FSM-claim path; calls the new runner after successful FSM claim and propagates the callback error |
| `restorePendingOnGraceBail` | `internal/assignment/calculator.go:658-672` | **reused** — kept load-bearing by the new `RunClaimedRebalanceErr` runner that propagates `errShuttingDown` |
| `rebalance` in-rebalance grace re-check | `internal/assignment/calculator.go:1127-1138` | **reused** — origin of the `errShuttingDown` the restore helper handles |
| `handleRebalance` (swallows `errShuttingDown` to nil) | `internal/assignment/calculator.go:1057-1075` | **reused** — used by `RunClaimedRebalance` for emergency/scaling paths where the error is not load-bearing; partition lifecycle uses a separate callback path (see §3.4) |
| `StateMachine.EnterRebalancing` (existing strict-source CAS from Scaling) | `internal/assignment/state_machine.go:233-254` | **reused as template** — PR-3 adds a sibling claim primitive |
| `StateMachine.TryClaimEmergency` / `RunClaimedRebalance` (claim+run pattern) | `internal/assignment/state_machine.go:256-309` | **reused as template** — PR-3 adds `TryClaimRebalancing` mirroring this shape and a sibling `RunClaimedRebalanceErr` that returns the callback error |
| `StateMachine.compareAndSwapState` (strict-source CAS primitive) | `internal/assignment/state_machine.go:359-361` | **reused** — new claim primitive uses it directly |
| `StateMachine.notifyStateChange` (fanout) | `internal/assignment/state_machine.go:369-378` | **reused** |
| `StateMachine.ReturnToIdle` | `internal/assignment/state_machine.go` — `RunClaimedRebalance` calls it at `:302, :308` | **reused** — `RunClaimedRebalanceErr` calls the same path |
| Manager FSM contract enum + Godoc | `types/state.go:35-36, 11` | **reused** — Option X preserves the contract literally; no edit |
| Calculator FSM contract enum + Godoc | `types/calculator_state.go:24-26, 7` | **reused** — same |
| Readiness probe docs (Stable / Scaling / Rebalancing / Degraded ready) | `docs/OPERATIONS.md:323-324`, `docs/LIFECYCLE.md:66` | **reused** — Option X keeps these honest; no doc edit needed |
| Public glossary entry for `Rebalancing` | `docs/REFERENCE.md:358` | **modified** — broadened wording from "after worker count changes" to "after worker or partition-source changes" (P2 fix; see §3.6) |
| Calculator → manager state mapping (CalcStateRebalancing → StateRebalancing) | `manager_state.go:230-231` | **reused** — partition-lifecycle path will now flow through this mapping naturally |
| `Manager.State` reader (returns `m.state`) | searched via grep; backed by `m.state` atomic | **reused** — no API surface change |
| Hook contract `OnStateChanged` (every state transition) | `docs/REFERENCE.md:33-34, 55`, `docs/API_REFERENCE.md:752, 777-779` | **reused** — Option X makes the hook fire on partition-lifecycle rebalances as well, which is the contractual behavior users already expect |
| `m.mu` guarding `m.calculator` | `manager_assignment.go:78-80, 249-251` | **reused** — `monitorCalculatorState` receives `calc` by parameter (see `:136`) so no new lock acquisition is needed |
| `observeAndDecide` (poll-driven emergency detection) | `internal/assignment/calculator.go:769-861`; emergency claim at `:819-826` | **reused** — partition-lifecycle path calls this in-line as a tail-check after `RunClaimedRebalanceErr` returns (see §3.5) |

Verified against current branch `main` @ `96359fe`. Spec author MUST re-verify line numbers immediately before implementing if HEAD has advanced (recent commits to this area: `6f0e6b2`, `3594c97`, `b0aff80`).

---

## 2. Design

PR-3 must resolve two design calls. Both are listed below with options, evidence, and a single named **Decision**. Plan-review v1 findings reshape Design Call 2's implementation (the P0/P1 fixes are integrated, NOT switched to Option Y).

### 2.1 Design call 1 — subscriber-drop recovery shape

**Options:**

- **A. Add a periodic reconcile tick to `monitorCalculatorState`.** Pulls `calc.GetState()` on a fixed cadence and re-drives `syncStateFromCalculator` if the result differs from the manager's last-observed mapping. Subscriber stays drop-tolerant; recovery cadence is deterministic and bounded by the tick. Mirrors PR-1's alias-watcher reconcile (`manager_assignment.go:292-318`).
- **B. Replace the subscriber's drop semantics with coalescing latest-value.** Inside `stateSubscriber.trySend`, drain-then-overwrite a 1-slot channel (or convert to `atomic.Int32` + a notify channel) so the latest state always wins. Changes the drop semantics on the subscriber side; affects every consumer of `SubscribeToStateChanges`.
- **C. Both.** Coalescing + periodic reconcile.
- **D. Lossless event path (transition queue / sequence numbers).** Subscriber buffer becomes unbounded or per-subscriber sequence numbers expose missing transitions to consumers. Would replay every missed transition, not just project current state.

**Evidence informing the choice:**

- The subscriber is a `chan types.CalculatorState` buffered 4 (`state_machine.go:119`). Drop semantics are deliberate — `trySend` MUST NOT block the state machine (`state_subscriber.go:24-29`). Correct coalescing requires either reading from the producer side (lock-and-replace via a slot field — not a chan) or switching the wire type. Either change widens blast radius beyond a state-projection fix.
- PR-1 used the same shape (Option A) for the legacy alias watcher and that pattern is now correctness-tested via the silent-stall test (`01-pr1-spec.md` §5.2). Using the same primitive here keeps the codebase's recovery model consistent.
- The drop is rare in steady state (buffer=4 absorbs `Idle→Scaling→Rebalancing→Idle`). Reconcile only needs to be load-bearing under burst churn.
- Option D requires a redesign of the producer side and is out of scope for a Tier-P1 ~50-LOC fix.

**Decision: Option A.** Periodic reconcile tick at a package-private `calculatorStateReconcileInterval = 1 * time.Second` (test-overridable). Subscriber drop semantics unchanged.

**Invariant narrowing (plan-review v1 P1-B fix):** reconcile guarantees **eventual projection of the current calculator state**, NOT replay of every missed transition or recovery of every missed `OnStateChanged` hook. If a burst `Scaling → Rebalancing → Idle` completes entirely between two reconcile ticks AND the subscriber dropped every event in that burst, reconcile sees only the final `Idle` and projects accordingly. The intermediate `Rebalancing` hook is not replayed; this is the deliberate boundary of Option A. The dominant operator-visible symptom (Cause A as framed in §1) is the *current* state lying — not the *history* lying — and the reconcile arm closes that. Stronger invariants are deferred to Option C/D as a future PR (§10.3).

Fallback: if implementation reveals a corner case in which the reconcile read races subscribe initialization, escalate to Option C. No such case is visible from the audit or the current code.

### 2.2 Design call 2 — partition-lifecycle FSM contract

**Options:**

- **X. Route partition lifecycle through the FSM.** `monitorPartitions` enters `Rebalancing` via a new strict-source FSM claim, runs the rebalance, returns to `Idle`. Public contract (`types/state.go:35-36`, `types/calculator_state.go:24-26`) holds literally. `OnStateChanged` fires on partition rebalances. Readiness probes correctly see `Rebalancing` during partition lifecycle.
- **Y. Narrow the public contract.** Edit `types/state.go`, `types/calculator_state.go`, and the readiness-probe doc snippets so `Rebalancing` only covers "leader-driven membership rebalance." Cheaper code change; user-visible semantic shift.

**Evidence informing the choice:**

- `00-fix-plan.md:22-23`: "Operator-pervasive. Dashboards, hooks, readiness probes all read `Manager.State()`."
- `docs/OPERATIONS.md:323-324`: readiness probe is documented to be OK in `Stable, Scaling, Rebalancing, Degraded`. Option Y diverges from doc.
- `docs/REFERENCE.md:33-34, 55` and `docs/API_REFERENCE.md:777-779`: `OnStateChanged` is contracted to fire on "every state transition." Under Option Y, users keying on this hook silently miss partition-rebalance events with no upgrade signal.
- The FSM already has the necessary primitives: `compareAndSwapState`, `notifyStateChange`, `RunClaimedRebalance`. The emergency path establishes a clean precedent: `TryClaimEmergency` CASes from Idle OR Scaling and invokes `RunClaimedRebalance` (`state_machine.go:272-287`). A sibling primitive `TryClaimRebalancing(from=Idle)` is a direct mirror.
- `isValidTransition` already permits `Stable → Rebalancing → Stable` (`manager_state.go:165, 167`).
- The audit (`tmp/worker_state_analysis/00-report.md:289-291`) ranks the symptom as "Operator-pervasive."

**Decision: Option X — Route partition lifecycle through the FSM.**

Plan-review v1 surfaced two concrete failure modes in v1's pseudocode that did NOT exist in the pre-PR-3 code; closing them is mandatory before implementation. The corrections do not change the option chosen, only how it is implemented.

#### 2.2.1 P0-A fix — preserve `errShuttingDown` propagation

**Problem (plan-review v1 P0-A).** v1 routed partition lifecycle through `RunClaimedRebalance` (`state_machine.go:298-309`), which calls `c.onRebalanceCb` (= `handleRebalance` at `calculator.go:1057-1075`). `handleRebalance` swallows `errShuttingDown` to nil (`:1062-1064`). `RunClaimedRebalance` has no error return. Result: the in-rebalance grace re-check at `calculator.go:1127-1138` returns `errShuttingDown`, `handleRebalance` converts it to nil, `RunClaimedRebalance` returns void, and `restorePendingOnGraceBail` at the call site sees nil — `pendingPartitionUpdate` is NOT restored. The deferred partition update is lost until the next watcher event.

**Options considered:**

1. **Add a partition-specific runner `RunClaimedRebalanceErr` on `StateMachine` that returns the callback's error to the caller while still returning the FSM to Idle.** Symmetric to existing `RunClaimedRebalance`. Caller decides whether to interpret the error.
2. Capture the original cause from `rebalance` before `handleRebalance` swallows it (closure-side channel or shared variable). Adds shared state across goroutines (the watcher goroutine vs the rebalance-runner) for a clean signal — racy without a lock; with a lock, more surface than Option 1.
3. Make `handleRebalance` distinguish "partition-lifecycle call site" via a lifecycle-string check so it stops swallowing `errShuttingDown` for those reasons. Couples error-swallowing policy to magic-string lifecycle values; the existing emergency/scaling paths intentionally swallow `errShuttingDown` and we'd have to preserve that — adds conditional behavior on a hot path.

**Decision (P0-A): Option 1.** Add `StateMachine.RunClaimedRebalanceErr(ctx, reason) error` adjacent to `RunClaimedRebalance` (`state_machine.go:289-309`). Bypasses `handleRebalance` by calling a new partition-specific callback (`handlePartitionRebalance`) that does NOT swallow `errShuttingDown`. Both runners share `ReturnToIdle` for the post-callback FSM transition.

Why not Option 2: shared state across goroutines for one error is a regression in clarity. Why not Option 3: error-swallowing policy belongs in the caller, not in the callback gated by a string. Option 1 keeps the swallow policy where it is (in the existing `handleRebalance` for non-partition callers) and gives partition lifecycle its own callback with the policy it actually needs.

Evidence: `state_machine.go:298-309` (current `RunClaimedRebalance`), `calculator.go:1057-1075` (`handleRebalance` swallows), `calculator.go:1127-1138` (origin of `errShuttingDown`), `calculator.go:663-672` (`restorePendingOnGraceBail` is the consumer).

#### 2.2.2 P0-B fix — emergency-after-partition tail-check

**Problem (plan-review v1 P0-B).** Today (pre-PR-3) the partition path calls `rebalance` directly, leaving FSM `Idle`. A concurrent emergency observation can claim `Idle → Emergency` via `TryClaimEmergency` and queue behind `rebalanceMu` (`state_machine.go:272-286`, `calculator.go:819-824`). Under Option X the partition path claims `Rebalancing`; `TryClaimEmergency` rejects from `Rebalancing` (existing behavior, `:272-286`). `observeAndDecide` returns nil without a marker (`calculator.go:819-826`). If no further watcher event arrives, the emergency rebalance waits for the next poll cycle.

**Options considered:**

1. Add a `pendingEmergencyClaim` marker analogous to `pendingPartitionUpdate`, drained by the same drain ticker. New state, new race, new test surface; extends the marker invariant to a second concern.
2. **Allow the partition-lifecycle runner's caller to chain-check for queued emergency after rebalance completes (single-goroutine handoff while the FSM is already back to Idle).** After `RunClaimedRebalanceErr` returns to Idle, the partition-lifecycle goroutine calls `c.observeAndDecide(ctx, nil)` once. If an emergency observation arrived during the rebalance window, `observeAndDecide` re-detects it through the existing emergency-detector path and claims `Idle → Emergency`.
3. Stronger transition: allow `Rebalancing → Emergency` preemption with `EnterRebalancing` / `TryClaimRebalancing` cancellation. Preempting a mid-publish partition rebalance is dangerous (assignment partial-publish; loses the just-snapshotted source revision); the operational story is bad.

**Decision (P0-B): Option 2.** After `c.stateMach.RunClaimedRebalanceErr(reqCtx, lifecycle)` returns (FSM has called `ReturnToIdle`), the partition-lifecycle path performs a single in-line tail-check: `_ = c.observeAndDecide(reqCtx, nil)`. The tail-check is bounded (one call, no retry loop) and runs on the same goroutine, so it executes during the time window in which the partition lifecycle would otherwise return control to the watcher select loop. Emergency detection arrived during the rebalance window is picked up immediately rather than waiting for the next `pollTicker` tick (`Calculator.run` poll loop).

Why not Option 1: a marker requires a drain mechanism, a CAS, a test, and a contract for how it interacts with `pendingPartitionUpdate`. Option 2 reuses `observeAndDecide` whose emergency-detection contract is already exercised by the existing emergency tests. Why not Option 3: pre-empting an in-flight rebalance midway through publish is a correctness hazard well beyond PR-3's scope.

Caveat: the tail-check only closes the window when the partition lifecycle is the runner that completed. If a worker-set Scaling rebalance was in flight instead, the existing scaling-path `RunClaimedRebalance` (called from `EnterRebalancing` at `state_machine.go:233-254` or from `claimEmergency` at `calculator.go:819-826`) is unchanged. Those paths already either claim emergency themselves or run on the poll goroutine that polls again on the next tick. The tail-check is targeted at exactly the new gap that Option X introduces.

Evidence: `state_machine.go:272-286` (`TryClaimEmergency` rejects from `Rebalancing`), `calculator.go:819-826` (no marker on failed emergency claim), `calculator.go:769-861` (`observeAndDecide` is the existing emergency-detection entry point and is reentrant for back-to-back calls).

#### 2.2.3 P1-A — claim-failure retry duplicate bound

**Problem (plan-review v1 P1-A).** v1 set `pendingPartitionUpdate=true` on any `TryClaimRebalancing` failure. The drain ticker later calls `triggerPartitionRebalance` again. If the in-flight Scaling/Emergency rebalance already re-read the source via `snapshotSource` (`calculator.go:1111-1124, 1168-1172`), the deferred drain produces a duplicate `Stable → Rebalancing → Stable` cycle whose publish body finds nothing new.

**Decision (P1-A): accept the duplicate, bound it, and test it.** §4 behavior table is amended:

> When a partition update fires while FSM is in Scaling/Emergency: the in-flight rebalance picks up the fresh source via `snapshotSource`, AND the drain-ticker arm retriggers `triggerPartitionRebalance` after the in-flight rebalance completes. The retry's `rebalance` body re-snapshots and finds no source delta (assignment unchanged) so the publish is a no-op for assignment contents — but `OnStateChanged(Stable→Rebalancing→Stable)` still fires for the retry cycle. **Duplicate bound: at most one extra `Stable → Rebalancing → Stable` cycle, bounded in time by one `RebalanceGraceDrainInterval` after the original lifecycle returns to Idle.**

Test 5.3 (was optional in v1) is now **required** and asserts the tight bound directly: exactly one `handlePartitionRebalance` body entry per dropped partition update (`n1 - n0 == 1`), via the producer-side counter described in §5.3. See §5.3 for the signal choice and the rejected alternatives (CAS-attempt counting, async hook-recorder counting).

Why not "clear the pending bit when an in-flight rebalance observes a fresh source": adding generation/sequence tracking to `pendingPartitionUpdate` adds state that's not load-bearing for correctness (only for hook-firing economy). The duplicate cycle is operationally cheap and visibly bounded; the explicit characterization is the lower-risk fix.

#### 2.2.4 P1-C — hook firing order

**Problem (plan-review v1 P1-C).** v1 claimed a specific user-observable order between `OnStateChanged(Stable→Rebalancing)`, assignment hooks, and `OnStateChanged(Rebalancing→Stable)`. Hooks are dispatched via `invokeHook` which spawns a goroutine (`manager.go:827-839`). User-visible order is not deterministic.

**Decision (P1-C):** §4 distinguishes "calculator notification enqueue order" (deterministic, FSM-side) from "user hook execution order" (eventual presence, not order). Test 5.2 asserts eventual presence of both transitions in the recorded stream without requiring order vs assignment hooks.

#### 2.2.5 P2 — public glossary

**Decision (P2):** §3.6 broadens `docs/REFERENCE.md:358` glossary entry from "after worker count changes" to "after worker or partition-source changes." One-line edit.

Fallback for the design call as a whole: if `TryClaimRebalancing` introduces an unexpected interaction with `EnterScaling`'s scaling-timer goroutine or with `TryClaimEmergency`'s preemption, Option Y is the fallback. Not anticipated.

### 2.3 Why no `isValidTransition` edge additions are needed

The partition-lifecycle path produces `Idle → Rebalancing → Idle` on the calculator side and `Stable → Rebalancing → Stable` on the manager side (or `WaitingAssignment → Rebalancing` if a partition change fires before initial assignment lands). Both edges exist today in `isValidTransition`:

- `StateStable: {..., StateRebalancing, ...}` — `manager_state.go:165`
- `StateRebalancing: {StateStable, StateWaitingAssignment, ...}` — `manager_state.go:167`
- `StateWaitingAssignment: {..., StateRebalancing, ...}` — `manager_state.go:164`

`syncStateFromCalculator`'s mapping (`manager_state.go:194-246`) already maps `CalcStateRebalancing → StateRebalancing` (`:230-231`).

---

## 3. Implementation

### 3.1 Widen `assignmentCalculator` to expose `GetState`

**Current shape** (`manager.go:182-189`):

```go
type assignmentCalculator interface {
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    SubscribeToStateChanges() (<-chan types.CalculatorState, func())
    TriggerRebalance(ctx context.Context) error
}
```

**Target shape:**

```go
type assignmentCalculator interface {
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    SubscribeToStateChanges() (<-chan types.CalculatorState, func())
    TriggerRebalance(ctx context.Context) error
    // GetState returns the calculator's current state. Used by
    // monitorCalculatorState's periodic reconcile to recover from
    // dropped subscriber events (the buffer is fixed-size and trySend
    // is drop-on-full; see internal/assignment/state_subscriber.go:24-29).
    GetState() types.CalculatorState
}
```

**Concrete implementations:**

1. `internal/assignment/calculator.go:378-384` — `Calculator.GetState` already exists. **No change.**
2. `internal/assignment/nop_calculator.go` — confirm via `grep -n "NopCalculator" internal/assignment/*.go`; add `GetState() types.CalculatorState` returning `types.CalcStateIdle` if missing.
3. Test doubles (`stubCalculator` in `manager_assignment_test.go:35+`, `monitorTestCalculator` in `manager_assignment_fixes_test.go:249+`) — add a controllable `GetState`.

### 3.2 Rewrite `monitorCalculatorState` with a reconcile arm

**Current shape** (`manager_assignment.go:166-193`):

```go
func (m *Manager) monitorCalculatorState(calc assignmentCalculator, readyCh chan struct{}) {
    m.logger.Info("starting calculator state monitor")
    stateCh, unsubscribe := calc.SubscribeToStateChanges()
    close(readyCh)
    defer unsubscribe()

    for {
        select {
        case <-m.ctx.Done():
            m.logger.Info("calculator state monitor stopped")
            return
        case calcState, ok := <-stateCh:
            if !ok {
                m.logger.Info("calculator state channel closed, stopping monitor")
                return
            }
            if err := m.syncStateFromCalculator(calcState); err != nil {
                m.logError("failed to sync state from calculator",
                    "calc_state", calcState, "error", err)
            }
        }
    }
}
```

**Target shape:**

```go
// calculatorStateReconcileInterval is the period between idempotent reads
// of calc.GetState() in monitorCalculatorState. Recovers from dropped
// subscriber events (state_subscriber.go:24-29: trySend drops on full
// buffer; buffer size 4 at state_machine.go:119). Cadence is fast enough
// that a missed transition is corrected within ~1s under any realistic
// burst.
//
// IMPORTANT: reconcile guarantees eventual projection of the CURRENT
// calculator state. If a transient state (e.g., Rebalancing) is dropped
// AND completes between two reconcile ticks, the intermediate transition
// is NOT replayed — only the current state is projected. See PR-3 §2.1
// and §10.2.
//
// Declared as a package-level var (not a const) so reconcile-timing tests
// can override it. Production callers MUST NOT mutate this value.
var calculatorStateReconcileInterval = 1 * time.Second

func (m *Manager) monitorCalculatorState(calc assignmentCalculator, readyCh chan struct{}) {
    m.logger.Info("starting calculator state monitor")

    stateCh, unsubscribe := calc.SubscribeToStateChanges()
    close(readyCh)
    defer unsubscribe()

    reconcileTicker := time.NewTicker(calculatorStateReconcileInterval)
    defer reconcileTicker.Stop()

    var (
        lastApplied    types.CalculatorState
        lastAppliedSet bool
    )

    apply := func(calcState types.CalculatorState) {
        if err := m.syncStateFromCalculator(calcState); err != nil {
            m.logError("failed to sync state from calculator",
                "calc_state", calcState, "error", err)
            return
        }
        lastApplied = calcState
        lastAppliedSet = true
    }

    for {
        select {
        case <-m.ctx.Done():
            m.logger.Info("calculator state monitor stopped")
            return

        case calcState, ok := <-stateCh:
            if !ok {
                m.logger.Info("calculator state channel closed, stopping monitor")
                return
            }
            apply(calcState)

        case <-reconcileTicker.C:
            current := calc.GetState()
            if lastAppliedSet && current == lastApplied {
                continue
            }
            apply(current)
        }
    }
}
```

Three changes from current: new package-private interval var, new ticker with `defer Stop()`, new reconcile arm.

### 3.3 Add `StateMachine.TryClaimRebalancing`

**Location:** `internal/assignment/state_machine.go`, immediately below `TryClaimEmergency` (current end at `:287`).

**Target shape:**

```go
// TryClaimRebalancing attempts to atomically claim the rebalancing
// lifecycle from Idle. Strict-source CAS from Idle to Rebalancing.
// Returns true if the claim succeeded; the caller is then responsible
// for invoking RunClaimedRebalanceErr to execute the rebalance and
// return to Idle.
//
// Designed for partition-lifecycle callers (monitorPartitions in
// calculator.go) that need to drive a rebalance but did NOT first
// enter Scaling. Without this primitive, monitorPartitions bypasses
// the FSM entirely, violating the public contract that says Rebalancing
// means "active partition rebalance in progress" (types/state.go:35-36,
// types/calculator_state.go:24-26).
//
// Reason is recorded BEFORE the state-change notification fans out,
// mirroring tryClaimEmergencyFrom so subscribers see the reason on
// their first read of the Rebalancing state.
func (sm *StateMachine) TryClaimRebalancing(_ context.Context, reason string) bool {
    if !sm.compareAndSwapState(types.CalcStateIdle, types.CalcStateRebalancing) {
        currentState := types.CalculatorState(sm.current.Load())
        sm.logger.Debug("partition rebalance claim deferred: state machine not idle",
            "current_state", currentState.String(),
            "reason", reason)
        return false
    }

    sm.mu.Lock()
    sm.scalingReason = reason
    sm.mu.Unlock()

    sm.logger.Info("entering rebalancing state via partition-lifecycle claim",
        "reason", reason)

    sm.notifyStateChange(types.CalcStateRebalancing)

    return true
}
```

**Why CAS from Idle only (not Idle OR Scaling):** the scaling timer at `state_machine.go:201-218` will transition Scaling → Rebalancing once it fires; pre-empting Scaling invalidates the stabilization window. `rebalance` always re-reads via `snapshotSource` (`calculator.go:1116-1124, 1168-1172`), so the scaling-timer's eventual Rebalancing picks up the partition delta. The drain-ticker arm + the §3.5 tail-check together ensure no emergency is stranded.

### 3.4 Add `StateMachine.RunClaimedRebalanceErr` (P0-A fix)

**Location:** `internal/assignment/state_machine.go`, immediately after `RunClaimedRebalance` (current `:298-309`).

**Target shape:**

```go
// RunClaimedRebalanceErr runs a rebalance callback for a previously-claimed
// lifecycle and returns the callback's error to the caller. The FSM is
// returned to Idle whether the callback succeeded, failed, or returned
// errShuttingDown.
//
// Distinct from RunClaimedRebalance: this variant gives the caller the
// callback error so the partition-lifecycle path can run
// restorePendingOnGraceBail when rebalance bails on a recovery-grace
// re-check (calculator.go:1127-1138, 1057-1065 swallows errShuttingDown
// for legacy callers). The general RunClaimedRebalance preserves the
// existing void contract for emergency/scaling callers that do not need
// the error.
//
// Must be called after a successful TryClaimRebalancing (or another
// strict-source CAS into Rebalancing/Emergency).
//
// Parameters:
//   - ctx: Context for the rebalance operation
//   - reason: Lifecycle reason passed to the callback
//
// Returns:
//   - error: the partition-rebalance callback's error (nil on success).
//     The FSM is back to Idle regardless of the returned value.
func (sm *StateMachine) RunClaimedRebalanceErr(ctx context.Context, reason string) error {
    var cbErr error
    if sm.onPartitionRebalanceCb != nil {
        cbErr = sm.onPartitionRebalanceCb(ctx, reason)
        if cbErr != nil {
            sm.logger.Error("partition rebalance failed", "reason", reason, "error", cbErr)
        }
    }
    sm.ReturnToIdle()
    return cbErr
}
```

**StateMachine field addition:** alongside the existing `onRebalanceCb` (wired in `NewCalculator` at `calculator.go:167`), add a sibling `onPartitionRebalanceCb`. Wire it from `NewCalculator` to a new `Calculator.handlePartitionRebalance` (see §3.5) that does NOT swallow `errShuttingDown`. This keeps the existing `handleRebalance` swallow policy intact for emergency/scaling callers.

LOC accounting: the new callback field is one struct line + one constructor wire + `RunClaimedRebalanceErr` body (~12 LOC) + `handlePartitionRebalance` (~12 LOC). See §3.7.

### 3.5 Route `monitorPartitions` through the FSM claim with tail-check

**Current shape** (`calculator.go:645-728`): `triggerPartitionRebalance` calls `rebalance` directly; both `monitorPartitions` arms call it and pass the result to `restorePendingOnGraceBail`.

**Target shape:**

Add `handlePartitionRebalance` (sibling to `handleRebalance`) and rewrite `triggerPartitionRebalance` to claim the FSM, run via `RunClaimedRebalanceErr`, run a tail-check on a fresh stop-aware context, and propagate the error.

```go
// partitionRebalanceRequestTimeout bounds the request context allocated for
// the partition-lifecycle rebalance itself (the call into
// RunClaimedRebalanceErr). Production default matches the pre-PR-3 hard-coded
// 30 s used by the legacy triggerPartitionRebalance (calculator.go:649).
// Declared as a package-level var (not a const) so Test 5.7 can shorten it
// to force a deterministic context-exhaustion regression for the §3.5
// fresh-context fix. Production callers MUST NOT mutate this value.
var partitionRebalanceRequestTimeout = 30 * time.Second

// partitionTailCheckTimeout bounds the in-line observeAndDecide tail-check
// that runs after a partition-lifecycle rebalance returns to Idle. It MUST
// be allocated on a fresh ctxFromStopCh derived context — NOT on the
// rebalance's reqCtx — because reqCtx may already be cancelled or near
// its partitionRebalanceRequestTimeout deadline (30 s default) by the time
// RunClaimedRebalanceErr returns (PR-3 §2.2.2, plan-review v2 P0-B).
//
// Derivation of the 15 s value (plan-review v3 P2). The tail-check performs
// exactly two operations: (i) one worker-set KV read via
// `collectWorkerObservation` → `fetchWorkersWithKnown` (calculator.go:769-775),
// which is one JetStream KV ranged read over the workers bucket; under load
// this completes in well under 1 s in steady state and is bounded by NATS
// round-trip + decode (single-digit seconds even under transient reconnect
// jitter); and (ii) `TryClaimEmergency`, which is a single in-memory CAS on
// the state machine (`state_machine.go:272-286`) — sub-microsecond. The
// budget is therefore dominated by the KV read plus reconnect headroom. 15 s
// gives roughly 3× headroom over the KV-read p99 we expect under a single
// NATS reconnect storm, while staying well below the 30 s
// `partitionRebalanceRequestTimeout` (so the tail-check cannot dominate the
// upstream rebalance timeout) and below the default `EmergencyGracePeriod`
// of 5 s + typical emergency rebalance duration (so a real shutdown via
// `stopCh` aborts the tail-check long before its deadline matters). It is
// an operational policy bound, not an empirically proven threshold, and
// shutdown still cancels via `stopCh` regardless of the value.
//
// Declared as a package-level var (not a const) so tests asserting the
// fresh-context behavior can override it. Production callers MUST NOT
// mutate this value.
var partitionTailCheckTimeout = 15 * time.Second

// partitionRebalanceBlocker is a test-only synchronization hook consumed by
// handlePartitionRebalance AFTER the partitionRebalanceEntries increment and
// BEFORE the call into c.rebalance. Production callers leave it nil and the
// guard is a single nil-check on the partition-lifecycle path; tests
// (Test 5.7) install a non-nil channel via export_test.go and release it
// after they have observed callback entry and slept past the
// partitionRebalanceRequestTimeout deadline, which deterministically holds
// the partition callback open long enough for reqCtx to expire before
// RunClaimedRebalanceErr returns. The hook is scoped to this callback only —
// scaling and emergency callbacks (handleRebalance) DO NOT consult it, so
// production rebalance latency is unaffected on those paths.
//
// Declared as a package-level var (not a const, not a build-tagged symbol)
// so the test can swap it from export_test.go and the production code path
// retains a single `if h := …; h != nil { <-h }` line. Production callers
// MUST NOT set this value.
var partitionRebalanceBlocker chan struct{} // nil in production

// handlePartitionRebalance is the partition-lifecycle rebalance callback
// invoked by StateMachine.RunClaimedRebalanceErr. Unlike handleRebalance
// (calculator.go:1057), it does NOT swallow errShuttingDown — the caller
// needs that error so restorePendingOnGraceBail (calculator.go:663-672)
// can restore pendingPartitionUpdate when grace flipped between the
// pre-check and rebalanceMu acquisition (calculator.go:1127-1138).
func (c *Calculator) handlePartitionRebalance(ctx context.Context, lifecycle string) error {
    // Test 5.3 signal (v4): increment BEFORE the rebalance body so each
    // successful CAS-and-run pair is counted exactly once, regardless of
    // whether the body returns an error. Exposed via export_test.go.
    c.partitionRebalanceEntries.Add(1)
    // Test 5.7 determinism hook (v5): nil in production. When set by a test,
    // block here until the test releases the channel — this lets the test
    // hold the callback open until reqCtx (allocated in
    // triggerPartitionRebalance with partitionRebalanceRequestTimeout = 10 ms
    // in Test 5.7) has provably expired, so the assertion
    // `reqCtx.Err() != nil at return of RunClaimedRebalanceErr` is structurally
    // guaranteed rather than probabilistic. The check is a single nil-load on
    // the production hot path.
    if h := partitionRebalanceBlocker; h != nil {
        <-h
    }
    if err := c.rebalance(ctx, lifecycle); err != nil {
        // Surface errShuttingDown verbatim; surface other errors with a
        // descriptive wrapper.
        if errors.Is(err, errShuttingDown) {
            c.Logger.Info("partition rebalance skipped during shutdown / grace flip",
                "lifecycle", lifecycle)
            return err
        }
        return fmt.Errorf("partition rebalance failed for %s: %w", lifecycle, err)
    }
    // Match handleRebalance: keep lastWorkers consistent so the next
    // poll doesn't immediately re-enter scaling.
    c.mu.Lock()
    c.setLastWorkersLocked(c.currentWorkers)
    c.mu.Unlock()
    return nil
}

// triggerPartitionRebalance runs a partition-lifecycle rebalance via the
// state-machine claim path. On claim success it drives the rebalance via
// RunClaimedRebalanceErr, then runs a single tail-check observeAndDecide
// to give any emergency that arrived during the rebalance window an
// immediate claim opportunity (PR-3 §2.2.2 P0-B fix). On claim failure
// it restores pendingPartitionUpdate so the drain ticker retries.
//
// Returns the rebalance callback's error so the caller can run
// restorePendingOnGraceBail (PR-3 §2.2.1 P0-A fix).
func (c *Calculator) triggerPartitionRebalance(lifecycle string) error {
    if !c.stateMach.TryClaimRebalancing(context.Background(), lifecycle) {
        // Another lifecycle is mid-flight. Restore the deferred bit so
        // the drain ticker retries. See §2.2.3: this may produce one
        // extra Rebalancing cycle once the in-flight lifecycle finishes,
        // bounded by RebalanceGraceDrainInterval. The retry's publish
        // body is a no-op for assignment contents (the in-flight
        // lifecycle re-snapshots via snapshotSource).
        c.pendingPartitionUpdate.Store(true)
        return nil
    }

    reqCtx, cancel := ctxFromStopCh(context.Background(), c.stopCh, partitionRebalanceRequestTimeout)
    defer cancel()

    err := c.stateMach.RunClaimedRebalanceErr(reqCtx, lifecycle)

    // Tail-check (PR-3 §2.2.2): FSM is back to Idle. Run one in-line
    // observeAndDecide so an emergency that lost the TryClaimEmergency
    // race during the rebalance window gets re-detected immediately
    // (rather than waiting for the next poll tick).
    //
    // IMPORTANT (v3, plan-review v2 P0-B fix): the tail-check MUST use
    // a FRESH stop-aware context. reqCtx above carries a
    // partitionRebalanceRequestTimeout deadline (30 s default) that the
    // partition rebalance may have already consumed (worker reads, partition
    // snapshots, publish in c.rebalance at calculator.go:1140-1272).
    // observeAndDecide collects workers as
    // its first step (calculator.go:769-775) and exits fast on a
    // cancelled context (calculator.go:863-870); reusing reqCtx would
    // strand any emergency loser until the next poll tick.
    // partitionTailCheckTimeout is a small bound (15 s) — long enough
    // for a single worker-set read + emergency claim, short enough to
    // not delay shutdown.
    tailCtx, tailCancel := ctxFromStopCh(context.Background(), c.stopCh, partitionTailCheckTimeout)
    defer tailCancel()
    if tailErr := c.observeAndDecide(tailCtx, nil); tailErr != nil &&
        !errors.Is(tailErr, errShuttingDown) {
        // observeAndDecide errors are non-fatal (the poll path already
        // swallows them and continues). Logged but NOT returned — the
        // partition-lifecycle caller only cares about the rebalance
        // error so restorePendingOnGraceBail still receives
        // errShuttingDown when applicable (PR-3 §2.2.1, Test 5.4).
        c.Logger.Debug("post-partition-rebalance tail-check observeAndDecide returned error",
            "lifecycle", lifecycle, "error", tailErr)
    }

    return err
}
```

**Callers of `triggerPartitionRebalance`** (`calculator.go:693-694, 724-725`): unchanged in shape — both already call `restorePendingOnGraceBail(err)` on the returned error. Under the new design that helper is once again load-bearing for `errShuttingDown` (PR-3 §2.2.1 fix).

**Wiring `onPartitionRebalanceCb`:** in `NewCalculator` (`calculator.go:~167` where `onRebalanceCb` is wired), add a parallel wire to `c.handlePartitionRebalance`. Both callbacks share the same `c.rebalance` body; only the error-policy wrapper differs.

### 3.6 Public-glossary edit (P2)

Edit `docs/REFERENCE.md:358`:

```diff
- | **Rebalancing**         | Redistributing partitions after worker count changes                                                 |
+ | **Rebalancing**         | Redistributing partitions after worker or partition-source changes                                   |
```

Surface change: a single table row. No other doc edits required — `types/state.go:35-36`, `types/calculator_state.go:24-26`, `docs/OPERATIONS.md:323-324`, and `docs/LIFECYCLE.md:66` are already worded broadly enough to cover partition-source-driven rebalancing.

### 3.7 LOC budget

| Site | LOC |
|---|---|
| `assignmentCalculator` interface extension + Nop impl | ~5 |
| `monitorCalculatorState` rewrite + var | ~28 |
| `StateMachine.TryClaimRebalancing` | ~15 |
| `StateMachine.RunClaimedRebalanceErr` + new callback field + constructor wire | ~14 |
| `Calculator.handlePartitionRebalance` | ~12 |
| `triggerPartitionRebalance` rewrite (FSM claim + fresh-ctx tail-check) | ~21 |
| `partitionTailCheckTimeout` var + comment | ~2 |
| `partitionRebalanceRequestTimeout` var + comment (v4, Test 5.7 override seam) | ~1 |
| `Calculator.partitionRebalanceEntries atomic.Int64` field + increment in `handlePartitionRebalance` (v4, Test 5.3 signal) | ~2 |
| `partitionRebalanceBlocker chan struct{}` package var + nil-guarded `<-h` line in `handlePartitionRebalance` (v5, Test 5.7 determinism seam) | ~2 |
| Test-double `GetState` additions (2 sites) | ~4 |
| `docs/REFERENCE.md` glossary edit | ~1 |
| **Total** | **~95 LOC** |

Ceiling: **~97 LOC + 5 required tests + 1 optional**. Justification for the overrun vs `00-fix-plan.md`'s 50-LOC Tier P1 target: the two P0 fixes (preserving `errShuttingDown` propagation and the emergency tail-check) require ~26 LOC of new callback + runner + tail-check that did not exist in v1; v3 added ~3 LOC for the fresh-context allocation + named timeout var (plan-review v2 P0-B); v4 adds ~3 LOC total for the `partitionRebalanceRequestTimeout` override seam (1 line declaration + comment), the `partitionRebalanceEntries` counter field + its atomic increment in `handlePartitionRebalance` (2 lines) — the increment lives inside the already-counted `handlePartitionRebalance` body, so it is not double-counted. v5 adds ~2 LOC for the test-only `partitionRebalanceBlocker` package var (its declaration plus the `if h := partitionRebalanceBlocker; h != nil { <-h }` guard inside `handlePartitionRebalance`); the var itself is test-only state but the nil-guarded receive line lives on the production code path (Test 5.7 closure for plan-review v4 P1-B). Without these the implementation regresses operationally-load-bearing paths surfaced by plan-review v1, v2, v3, and v4. The structural primitives (`TryClaimRebalancing`, `RunClaimedRebalanceErr`) are reusable; the partition-specific callback `handlePartitionRebalance` is the minimum-additional-surface way to preserve `errShuttingDown` without changing the existing emergency/scaling swallow policy.

### 3.8 Required test updates (existing tests)

Search by grep before commit:

```sh
grep -rn "monitorCalculatorState\|monitorPartitions\|triggerPartitionRebalance" *_test.go internal/assignment/*_test.go
```

Two patterns to watch for:

1. Tests asserting `Manager.State() == StateStable` immediately after a partition-source change must switch to `WaitState(StateStable, …)`.
2. Tests counting `OnStateChanged` invocations during a partition-source change must account for the new `(Stable, Rebalancing)` + `(Rebalancing, Stable)` pair.

---

## 4. Behavior summary

Visible to operators / hook consumers before vs after PR-3:

| Scenario | Before | After |
|---|---|---|
| Subscriber buffer overflow during burst transitions, transient state still current at next reconcile tick | `Manager.State()` sticks on stale state until next CAS path | Reconcile tick (≤ 1 s) re-drives correct state; `OnStateChanged` fires from `transitionState` CAS path; no duplicate (the `from==to` short-circuit at `manager_state.go:106-107` covers redelivery) |
| Subscriber drops every event in a `Scaling→Rebalancing→Idle` burst that completes between reconcile ticks | `Manager.State()` stays in pre-burst state | Reconcile picks up `Idle` and projects to `Stable`. **Intermediate `StateRebalancing` is NOT replayed**; `OnStateChanged(Stable→Rebalancing)` and `(Rebalancing→Stable)` do NOT fire for the dropped burst. This is the deliberate boundary of Option A (§2.1, §10.2) |
| Partition-source watch fires (worker count unchanged) | `Manager.State()` stays in `Stable`; `OnStateChanged` does not fire; calculator runs rebalance "invisibly" | Manager observes `Stable → Rebalancing → Stable`; readiness probe correctly reports `Rebalancing` during the work. `OnStateChanged` enqueue order (FSM side) is `(Stable→Rebalancing)` BEFORE `(Rebalancing→Stable)`. User-hook execution order is NOT guaranteed against assignment hooks (`OnAssignmentChanged` / `OnPartitionsAssigned`) because `invokeHook` dispatches via goroutines (`manager.go:827-839`); the test asserts **eventual presence**, not order, of all hook invocations |
| Partition-source watch fires WHILE FSM is in Scaling | Direct rebalance from `monitorPartitions` runs concurrently with scaling timer; two rebalances possible | `TryClaimRebalancing` returns false; `pendingPartitionUpdate` set; drain ticker retries on next tick. Two outcomes possible per §2.2.3: (a) scaling timer's `EnterRebalancing` picks up the fresh source via `snapshotSource` and the drain-tick retry's `rebalance` body finds no delta (no-op publish), producing **one extra `Stable→Rebalancing→Stable` cycle** with empty assignment effects; OR (b) drain-tick retry occurs after Scaling already completed and produces a normal partition-lifecycle cycle. **Duplicate bound: at most one extra cycle.** Test 5.3 enforces |
| Partition-source watch fires WHILE FSM is in Emergency | Concurrent emergency + partition rebalance possible | Claim fails; `pendingPartitionUpdate` set; drain ticker retries after emergency completes. Same bound as Scaling case |
| Partition-source watch fires; recovery grace flips between pre-check and `rebalanceMu` acquisition | `triggerPartitionRebalance` returns `errShuttingDown`; `restorePendingOnGraceBail` restores `pendingPartitionUpdate`; drain ticker retries | **Same as before.** PR-3 preserves this path via the new `RunClaimedRebalanceErr` (PR-3 §2.2.1). Test 5.4 is the regression guard |
| Emergency observation arrives DURING a partition-lifecycle rebalance | Concurrent emergency claim allowed (FSM was `Idle`) | Partition-lifecycle owns `Rebalancing`; emergency `TryClaimEmergency` rejects; AFTER partition rebalance completes and FSM returns to Idle, the tail-check `observeAndDecide` immediately re-detects the emergency and claims `Idle → Emergency` (PR-3 §2.2.2). **No marker required; no wait for next poll.** Test 5.5 enforces |
| `WaitState(StateRebalancing, …)` keyed on partition rebalance | Times out | Resolves within window |

**Calculator state notification enqueue order (deterministic, FSM side):**

1. `TryClaimRebalancing` calls `notifyStateChange(CalcStateRebalancing)` BEFORE returning true.
2. `RunClaimedRebalanceErr` invokes `onPartitionRebalanceCb` (= `handlePartitionRebalance` = `rebalance`).
3. On callback return, `RunClaimedRebalanceErr` calls `ReturnToIdle` which notifies `CalcStateIdle`.

**User hook execution order (NOT deterministic):** `OnStateChanged(Stable, Rebalancing)`, assignment hooks fired from the apply path, and `OnStateChanged(Rebalancing, Stable)` are all dispatched via `Manager.invokeHook` which spawns a goroutine per invocation (`manager.go:829-838`). Users MUST NOT rely on cross-hook order. Tests assert **eventual presence** of all relevant invocations within a bounded window.

---

## 5. Tests

Five required tests, plus one optional. Each test states intent, setup, action, assertion, and file:line target. Test 5.7 (new in v3) is a dedicated context-exhaustion regression for the tail-check fresh-context fix; kept separate from Test 5.5 so the emergency-contention test remains focused on FSM ordering rather than mixing context-deadline timing into its assertions.

### Test 5.1 — Reconcile recovers a still-current dropped state (Cause A)

**Intent:** prove reconcile closes Cause A for the case it is designed for: a calculator state change that was dropped by the subscriber's full buffer AND remains current at the next reconcile tick is projected within `2 * calculatorStateReconcileInterval`. Per §2.1 invariant, this test does NOT assert recovery of completed bursts (the boundary is documented in §10.2).

**Setup:**
- `newTestManager(t)` with a controllable `assignmentCalculator` double (extend `stubCalculator` or `monitorTestCalculator`).
- Double exposes: a `subscribers` slice with selective drop, `setState(types.CalculatorState)`, `GetState() types.CalculatorState`.
- Override `calculatorStateReconcileInterval = 50 * time.Millisecond` for the test.
- Bring manager to `StateStable`. Subscribe to `OnStateChanged` and record `(from, to)` tuples.

**Action:**
1. `double.setState(CalcStateScaling)` BUT drop the subscriber send (simulate `trySend` buffer-full).
2. Wait for `2 * calculatorStateReconcileInterval`.

**Assertion:**
- `m.State() == StateScaling`.
- `OnStateChanged` recorder contains exactly one `(StateStable, StateScaling)` tuple.

**File:line target:** new file `manager_calculator_state_monitor_test.go` (or appended to `manager_assignment_test.go`).

### Test 5.2 — Partition lifecycle routes through the FSM (Cause B)

**Intent:** prove Cause B — a partition-source change drives the manager through `Stable → Rebalancing → Stable`. Asserts eventual presence of both transitions, NOT order against assignment hooks (per §4 P1-C wording).

**Setup:**
- Real `assignment.Calculator` wired to a controllable `WatchablePartitionSource` (locate via `grep -rn "WatchablePartitionSource" internal/assignment/*_test.go partitest/`).
- Real `StateMachine`; real `handlePartitionRebalance` callback wired.
- Manager subscribed to `OnStateChanged`, recording tuples.
- Cold-bootstrap to `StateStable` with an initial partition set.

**Action:**
1. Push a partition change through the watchable source.
2. `WaitState(StateStable, 5*time.Second)`.

**Assertion:**
- `OnStateChanged` recorder contains BOTH `(StateStable, StateRebalancing)` AND `(StateRebalancing, StateStable)` (eventual presence within a bounded window). **No ordering assertion across the two tuples** — `OnStateChanged` is dispatched via `Manager.invokeHook` which spawns a goroutine per invocation (`manager.go:827-839`), so user-hook callback execution order is NOT guaranteed even though the FSM-side notification enqueue order is deterministic. If a future test needs to verify strict enqueue order, it MUST subscribe to the calculator via `SubscribeToStateChanges` rather than read the manager hook recorder.
- Calculator state machine transitioned `Idle → Rebalancing → Idle`.
- Final partition assignment reflects the new partition set.
- **NO ordering assertion** between `OnStateChanged` invocations and assignment hooks (e.g., `OnPartitionsAssigned`). The test may record both streams but only asserts presence + counts, not interleaving.

**File:line target:** new file `internal/assignment/calculator_partition_lifecycle_fsm_test.go` (or appended to existing partition-lifecycle test file if one exists — search via `grep -rn "monitorPartitions\|partitionUpdate" internal/assignment/*_test.go`).

### Test 5.3 — Partition claim fails during Scaling; duplicate-rebalance bound (P1-A)

**Intent:** prove the claim-failure retry duplicate bound from §2.2.3. When `monitorPartitions` fires while `StateMachine` is in `Scaling`, the claim returns false, `pendingPartitionUpdate` is set, the in-flight scaling rebalance picks up the source delta via `snapshotSource`, and the drain ticker produces AT MOST one extra partition-lifecycle attempt.

**Signal choice (v4, plan-review v3 P1-A fix):** the assertion counts **`handlePartitionRebalance` body entries** for the controlled partition update, exposed via a test-only `atomic.Int64` counter on the `Calculator` and read via `export_test.go`. The counter is incremented at the very top of `handlePartitionRebalance` (BEFORE the call into `c.rebalance`), so every successful CAS that drives a partition-lifecycle rebalance increments it exactly once. This signal maps directly onto the §2.2.3 / §10.3 invariant — "at most one extra `Stable → Rebalancing → Stable` cycle" — because each `handlePartitionRebalance` body entry corresponds 1:1 to one such cycle (the CAS-win is the entry guard, and `ReturnToIdle` runs unconditionally after the callback returns).

v3 selected `TryClaimRebalancing` invocations (claim attempts including CAS-loss). Plan-review v3 P1-A correctly observed that this signal conflates "claim attempted but lost" with "claim won and ran" — the drain ticker can fire multiple times while Scaling/Emergency holds the FSM, each clearing `pendingPartitionUpdate`, calling `triggerPartitionRebalance` → `TryClaimRebalancing`, losing the CAS, restoring the bit, and re-attempting on the next tick. The number of failed attempts therefore depends on the test-controlled relationship between `PlannedScaleWindow` and `RebalanceGraceDrainInterval`, not on the spec's "at most one extra cycle" invariant. Counting successful callback entries restores the direct mapping. Alternatives considered and rejected:

- `OnStateChanged` hook recorder (v2): drop-tolerant + async via `state_subscriber.go:16-29` and `manager.go:827-839`; can read "0 cycles" while the producer ran an unbounded retry storm.
- `RunClaimedRebalanceErr` invocations: equivalent to counting partition callback entries (the runner is what drives the callback) but adds surface inside `StateMachine`; the partition path's `handlePartitionRebalance` is the dedicated callback and is the cleaner attachment point.
- `TryClaimRebalancing` invocation count (v3): conflates failed CAS attempts with successful cycles; bound depends on test timing, not the spec invariant.
- Assignment-publisher publish count: indirect — also bumped by the scaling-timer rebalance and by other lifecycles, requires source-generation filtering to disambiguate.

`handlePartitionRebalance` body entry is the smallest-surface signal whose count is exactly the §2.2.3 duplicate bound.

**Setup:**
- Real calculator + state machine + watchable source.
- Override `PlannedScaleWindow` (~200 ms) and `RebalanceGraceDrainInterval` (~50 ms) for test speed.
- Test-only counter wired via `export_test.go`: add an unexported `atomic.Int64` field `partitionRebalanceEntries` on `Calculator`; increment unconditionally as the FIRST line of `handlePartitionRebalance` (before `c.rebalance`); expose a getter `PartitionRebalanceEntries(c *Calculator) int64` from `export_test.go`. Zero production cost beyond one field, one atomic increment, and no exported surface.

**Action:**
1. Trigger a worker-set change to put FSM into `Scaling`.
2. Immediately push a partition-source change. Record the counter value `n0` right before pushing.
3. Wait for `PlannedScaleWindow + 4 * RebalanceGraceDrainInterval + assignment-settle` (i.e., let Scaling complete, allow the drain-tick retry to win the now-Idle CAS, allow at least two further drain ticks during which a buggy implementation could double-fire).
4. `WaitState(StateStable, …)`.

**Assertion:**
- During the Scaling window: `m.State() == StateScaling` (NOT `Rebalancing`).
- Final `m.State() == StateStable`.
- **`partitionRebalanceEntries` counter delta `n1 - n0 == 1`** for the single dropped partition update. This is exactly the §2.2.3 "at most one extra cycle" invariant: the immediate-arm `TryClaimRebalancing` lost CAS against Scaling (no callback ran, no increment); the drain ticker eventually wins the CAS once Scaling has returned the FSM to Idle (one callback runs, one increment). A wrong implementation that re-fires the partition callback under `pendingPartitionUpdate=true` after the first successful run will produce delta `≥ 2` and the test FAILS.
- Final partition assignment reflects the new partition + worker set.

**Failure model:** if `n1 - n0 ≥ 2`, the implementation has not bounded the duplicate cycle and the test fails. Critically, this bound is independent of test timing: any number of drain-tick CAS-loss attempts can fire during the Scaling window without incrementing the counter, because the counter is gated on successful claim + callback entry. The OnStateChanged hook recorder is NOT used as the primary assertion here because it cannot distinguish "0 hook cycles observed because producer ran 0 retries" from "0 hook cycles observed because the async drop-tolerant hook pipeline ate them."

**Cross-check against §2.2.3 / §10.3 invariant:** §2.2.3 reads "at most one extra `Stable → Rebalancing → Stable` cycle." Each `handlePartitionRebalance` body entry corresponds to exactly one such cycle on the partition path (the callback is invoked synchronously after the CAS-win and before `ReturnToIdle`). Counting body entries therefore measures the invariant directly with the tight bound `≤ 1` rather than the looser `≤ 2` that v3 derived from a CAS-attempt counter.

**File:line target:** same file as Test 5.2.

### Test 5.4 — Grace-flip regression (P0-A)

**Intent:** prove `restorePendingOnGraceBail` is still load-bearing under PR-3. When recovery grace flips to true between the pre-check at `calculator.go:715-718` and `rebalanceMu` acquisition at `:1127-1138`, `rebalance` returns `errShuttingDown`, `handlePartitionRebalance` surfaces it, `RunClaimedRebalanceErr` returns it, `triggerPartitionRebalance` returns it to the watcher arm, `restorePendingOnGraceBail` restores `pendingPartitionUpdate`, and the drain ticker retries after grace lifts.

**Setup:**
- Real calculator + state machine + watchable source.
- Test hook to force `inRecoveryGrace()` to return false at the partition-watch arm pre-check (`calculator.go:715`) but TRUE at the `rebalance`-internal re-check (`shouldDeferForRecoveryGrace` at `calculator.go:1136`). One way: inject a `grace gate` test double that flips between two reads; equivalent: use a `sync.Once`-style mock that returns false on first invocation and true thereafter.
- Override `RebalanceGraceDrainInterval` ~50 ms.

**Action:**
1. Push a partition-source change (grace pre-check sees false; lifecycle proceeds).
2. Inside `rebalance` the re-check sees true → `errShuttingDown`.
3. Lift grace (test hook flips back to false).
4. Wait for ≤ 2 drain ticks.

**Assertion:**
- After step 2: `pendingPartitionUpdate.Load() == true` (restored).
- After step 4: a partition rebalance completes; `OnStateChanged` recorder contains the `(Stable, Rebalancing) → (Rebalancing, Stable)` pair from the drain-tick retry.
- Final partition assignment reflects the change.

**File:line target:** same file as Test 5.2.

### Test 5.5 — Emergency contention regression (P0-B)

**Intent:** prove the tail-check from §2.2.2 closes the emergency-during-partition-rebalance gap. When partition lifecycle owns `Rebalancing` and an emergency observation arrives, the emergency rebalance runs immediately after partition rebalance completes, WITHOUT waiting for an unrelated future poll tick.

**Setup:**
- Real calculator + state machine + watchable source + controllable worker heartbeat (allowing test-driven disappearance).
- Override `PollInterval` to a large value (~10 s) so the test cannot accidentally pass via a normal poll re-detection.
- Override `RebalanceGraceDrainInterval` ~50 ms.
- Manager subscribed to `OnStateChanged`.
- Optional: a synchronization hook inside the partition `handlePartitionRebalance` to block until a test-driven worker-disappearance event is observable (so the test deterministically forces the emergency observation while the FSM is `Rebalancing`).

**Action:**
1. Begin a partition rebalance (push a source change).
2. While `m.State() == StateRebalancing`, simulate a worker disappearance that should drive emergency (e.g., remove its heartbeat KV and wait past the emergency-detector window — the detector's grace must be tuned to fire quickly under test).
3. Allow the partition rebalance to complete.

**Assertion:**
- After the partition rebalance completes, an **emergency rebalance runs within `2 * RebalanceGraceDrainInterval`** (well under `PollInterval`).
- The calculator state machine transitions `Idle → Emergency → Idle` after the `Idle → Rebalancing → Idle` partition cycle. Use `SubscribeToStateChanges` on the calculator to record FSM-side transitions if strict ordering is needed (see "Ordering" below).
- The manager-side `OnStateChanged` recorder contains, within a bounded window (e.g., 5 s), **eventual presence** of all four tuples: `(StateStable, StateRebalancing)`, `(StateRebalancing, StateStable)`, `(StateStable, StateEmergency)`, and `(StateEmergency, StateStable)`. Note: Emergency maps to `StateEmergency` on the manager side via `syncStateFromCalculator` at `manager_state.go:230-234` (`CalcStateRebalancing → StateRebalancing`; `CalcStateEmergency → StateEmergency` — distinct public states per `types/state.go:35-39`).
- **No ordering assertion between the two cycles** on the manager hook recorder. `Manager.invokeHook` dispatches each `OnStateChanged` via a fresh goroutine (`manager.go:827-839`), so callback execution order is NOT guaranteed even when FSM-side enqueue order is. The test asserts presence within the window; it does NOT assert `(Stable→Rebalancing)` callback completes before `(Stable→Emergency)` callback.
- The final assignment reflects the disappeared worker.

**Ordering (if needed):** strict ordering of `Idle → Rebalancing → Idle → Emergency → Idle` MUST be verified via the calculator's `SubscribeToStateChanges` channel (single-goroutine FSM enqueue), NOT the manager hook recorder. The test SHOULD subscribe and assert the recorded calculator-state sequence equals `[Rebalancing, Idle, Emergency, Idle]` (starting from the post-bootstrap Idle baseline).

**File:line target:** same file as Test 5.2.

### Test 5.6 (optional) — Completed-burst subscriber drop is NOT replayed

**Intent:** pin the §2.1 invariant boundary. Documents that if a `Scaling → Rebalancing → Idle` burst completes entirely between two reconcile ticks AND every event was dropped, reconcile projects only the final `Idle` state. NO replay of `Rebalancing`. This is the deliberate boundary of Option A.

**Setup:**
- Same as Test 5.1 but with selective drop of all three transitions in a burst.

**Action:**
1. `setState(Scaling)`, `setState(Rebalancing)`, `setState(Idle)` — all dropped by the subscriber double.
2. Wait `2 * calculatorStateReconcileInterval`.

**Assertion:**
- `m.State() == StateStable` (manager mapping of `CalcStateIdle`).
- `OnStateChanged` recorder does **NOT** contain `(StateStable, StateRebalancing)` for the dropped burst — only whatever pre-existed.
- Test docstring explicitly notes this is the deliberate boundary, not a bug.

**File:line target:** same file as Test 5.1.

Test 5.6 is optional because it asserts a non-fix (deliberately scoped-out behavior). Recommended as a regression guard against accidental future strengthening of the invariant without the requisite producer-side redesign.

### Test 5.7 — Tail-check uses a fresh context, not an exhausted rebalance context (v3, plan-review v2 P0-B)

**Intent:** prove the §3.5 fix — the tail-check `observeAndDecide` runs on a fresh stop-aware context (`partitionTailCheckTimeout`) and is NOT starved when the partition rebalance has consumed or exceeded its 30 s `reqCtx` deadline. A wrong implementation that reuses `reqCtx` for the tail-check would skip emergency re-detection in this scenario and strand the emergency loser until the next poll.

**Determinism mechanism (v5, plan-review v4 P1-B fix):** the v4 design overrode `partitionRebalanceRequestTimeout = 10 * time.Millisecond` and relied on the `rebalance` body taking longer than 10 ms to guarantee `reqCtx` was cancelled by the time `RunClaimedRebalanceErr` returned. Plan-review v4 surfaced that the rebalance work can in principle complete in well under 10 ms (in-memory fakes, no real KV round-trip on some test wirings), in which case the test would pass without checking the invariant under test. v5 fixes this by ADDING a second test-only seam — `partitionRebalanceBlocker chan struct{}` (introduced in §3.5) — that gates the partition callback open until the test releases it. Combined with the 10 ms `partitionRebalanceRequestTimeout` override and an explicit post-entry sleep past that deadline, this gives a deterministic ordering: callback enters → counter increments → callback blocks → test confirms entry → test sleeps past `reqCtx`'s 10 ms deadline → test releases the blocker → `rebalance` body runs against an already-cancelled `reqCtx` → `RunClaimedRebalanceErr` returns. The `reqCtx.Err() != nil` assertion at return is then structurally guaranteed, not probabilistic. Other rebalance paths (scaling, emergency via `handleRebalance`) DO NOT consult `partitionRebalanceBlocker`, so the determinism seam is scoped strictly to the partition-lifecycle callback.

**Setup:**
- Real calculator + state machine + watchable source + controllable worker heartbeat.
- Override `PollInterval` to a large value (~10 s) so the test cannot pass via a normal poll re-detection.
- Override `partitionRebalanceRequestTimeout` to `10 * time.Millisecond` via `export_test.go` (e.g., `SetPartitionRebalanceRequestTimeout(10 * time.Millisecond)` with a `defer` to restore). Test 5.7 lives in `package assignment` (same file as Test 5.5, see §5.5 / §5.2 file target), so helper calls are unqualified.
- Override `partitionTailCheckTimeout` to ~500 ms via `export_test.go` so the tail-check completes quickly and any failure mode (reusing `reqCtx`) is observable within ~1 s rather than 15 s.
- Install the v5 determinism blocker: `blocker := make(chan struct{})`; `SetPartitionRebalanceBlocker(blocker)` (exposed from `export_test.go` — co-located with `PartitionRebalanceEntries(c)` from Test 5.3). `defer SetPartitionRebalanceBlocker(nil)` to clear after the test.
- Manager subscribed to `OnStateChanged`; calculator subscribed via `SubscribeToStateChanges` for FSM-side assertions.

**Action (pseudocode — implementer should be able to write the test without re-deriving the ordering):**

```go
// 1. Setup applied above.
// Test lives in `package assignment` (same file as Test 5.5). All helper
// calls are unqualified.
blocker := make(chan struct{})
SetPartitionRebalanceBlocker(blocker)
defer SetPartitionRebalanceBlocker(nil)
SetPartitionRebalanceRequestTimeout(10 * time.Millisecond)
defer SetPartitionRebalanceRequestTimeout(30 * time.Second)
SetPartitionTailCheckTimeout(500 * time.Millisecond)
defer SetPartitionTailCheckTimeout(15 * time.Second)

// 2. Bring the cluster up; wait for WaitState(StateStable, …).
require.NoError(t, mgr.WaitState(ctx, types.StateStable, 5*time.Second))

n0 := PartitionRebalanceEntries(calc)

// 3. Push a partition-source change to start the partition lifecycle.
src.UpdatePartitions(newPartitionSet)

// 4. Wait for the partition callback to ENTER (counter increments) — proves
//    the FSM has claimed Rebalancing and the goroutine is parked on
//    `<-partitionRebalanceBlocker` BEFORE c.rebalance runs.
require.Eventually(t, func() bool {
    return PartitionRebalanceEntries(calc) == n0+1
}, time.Second, 5*time.Millisecond, "partition callback never entered")

// 5. Inject the emergency observation while the FSM is Rebalancing.
//    Because the FSM is `Rebalancing` (from step 3), TryClaimEmergency
//    rejects; the emergency observation is the "loser" the tail-check is
//    designed to recover.
hb.Drop(workerB)

// 6. Sleep past the reqCtx deadline. The callback is blocked on `<-blocker`
//    inside handlePartitionRebalance; reqCtx (allocated with a 10 ms budget
//    in triggerPartitionRebalance) expires while we sleep. Any margin >
//    partitionRebalanceRequestTimeout suffices; 50 ms gives 5× headroom.
time.Sleep(50 * time.Millisecond)

// 7. Release the blocker. handlePartitionRebalance proceeds into c.rebalance
//    with an already-cancelled reqCtx; rebalance returns (likely with a
//    context error wrap or errShuttingDown if stopCh raced), then
//    RunClaimedRebalanceErr returns to triggerPartitionRebalance.
close(blocker)

// 8. Wait at most 2 * partitionTailCheckTimeout (~1 s) for the calculator-side
//    state stream to surface Emergency via the tail-check observeAndDecide.
```

**Assertion:**
- `reqCtx.Err() != nil` at the moment `RunClaimedRebalanceErr` returns — guaranteed by construction: the callback is parked on `<-partitionRebalanceBlocker` from step 4 onward, and step 6 sleeps for 50 ms which is 5× the 10 ms `partitionRebalanceRequestTimeout` budget, so `reqCtx` is provably expired before step 7 unblocks the callback. Verified concretely either by asserting (a) the partition callback's returned error is non-nil (its `ctx.Err()` propagates through `c.rebalance`), or (b) by recording `reqCtx.Err()` at return via a test-only capture hook installed alongside the blocker.
- The calculator-side state-change stream contains `Emergency` **within `2 * partitionTailCheckTimeout`** after step 7, i.e., well before the next `PollInterval` tick (~10 s).
- A wrong implementation that reuses `reqCtx` for `observeAndDecide` will see `reqCtx` already cancelled, exit fast at `collectWorkerObservation` (`calculator.go:769-775, 863-870`), skip emergency claim, and the calculator-state stream will NOT contain `Emergency` within the window — the test FAILS.
- Final assignment reflects the disappeared worker.

**Why this is deterministic across CI runners:** the test no longer races the wall-clock duration of `c.rebalance`. The only timing involved is the 50 ms sleep in step 6 versus the 10 ms `partitionRebalanceRequestTimeout` — a 5× margin that is robust to any realistic scheduler jitter. A slow CI host makes `reqCtx` more cancelled by step 7, not less; a fast host is irrelevant because the callback is parked on the blocker until the test explicitly releases it. No assertion conditions on the duration of `c.rebalance`.

**File:line target:** same file as Test 5.5.

---

## 6. Migration / backwards-compat

**No code-level migration required.** PR-3 does not change any public API surface. The contract being repaired (`Rebalancing` means active partition rebalance) was already the documented behavior. Existing users observing `Manager.State()` or `OnStateChanged` will see partition-rebalance events surface where today they are silently suppressed — a strict improvement.

**`CHANGELOG.md` entry** under `## Unreleased` (or next release header):

> ### Fixed
> - `Manager.State()` and `OnStateChanged` now correctly reflect partition-lifecycle rebalances (previously, partition-source changes ran a rebalance without entering `StateRebalancing`). Additionally, a low-frequency reconcile in `monitorCalculatorState` ensures the manager's projected state recovers within ~1 s of a dropped calculator state-machine subscriber event. Note: the reconcile path guarantees eventual projection of the current calculator state, not replay of every missed transient transition.
>
> ### Documentation
> - `docs/REFERENCE.md` glossary: broaden "Rebalancing" to cover partition-source changes as well as worker-count changes.

---

## 7. Risks and edge cases

### 7.1 Reconcile race with subscribe initialization

Reconcile ticker arms after `close(readyCh)`. First tick (≤ 1 s) reads `calc.GetState()`. All cases (calculator not yet started → `CalcStateIdle`; calculator started in Idle → `CalcStateIdle`; calculator mid-Scaling/Rebalancing/Emergency → reconcile drives the right transition) are safe by the `lastApplied` gate and `transitionState`'s `from==to` short-circuit.

### 7.2 Reconcile during manager shutdown

`m.ctx.Done()` is first arm of select; reconcile is third. `defer reconcileTicker.Stop()` releases. No leak.

### 7.3 Claim contention with `TryClaimEmergency`

Both `TryClaimEmergency` and the new `TryClaimRebalancing` race for the same FSM. CAS guarantees one wins:

- Emergency wins: partition claim returns false; `pendingPartitionUpdate` set; drain ticker retries after emergency completes.
- Partition wins: emergency claim's `TryClaimEmergency` returns false; emergency-deferral logging fires (`state_machine.go:283-285`); the tail-check `observeAndDecide` at the end of `triggerPartitionRebalance` re-detects emergency immediately (§2.2.2).

### 7.4 `restorePendingOnGraceBail` propagation under partition lifecycle

PR-3's `RunClaimedRebalanceErr` propagates the callback error. `handlePartitionRebalance` surfaces `errShuttingDown` verbatim. `triggerPartitionRebalance` returns the error to the watcher arm. `restorePendingOnGraceBail` retains its load-bearing role. Test 5.4 is the regression guard.

### 7.5 Tail-check fires during real shutdown

If the partition rebalance returned `errShuttingDown` because `stopCh` is closed (real shutdown, not grace flip), the tail-check's **fresh** `tailCtx` is also derived from `ctxFromStopCh(c.stopCh, partitionTailCheckTimeout)`, so it cancels immediately when `stopCh` is closed. `observeAndDecide` is ctx-aware via `c.collectWorkerObservation` → `getActiveWorkersFiltered` → KV reads. It returns an error or no-ops in that path. Logged at Debug; non-fatal. Crucially the tail-check is NOT starved by an exhausted `reqCtx`: the only thing that can cancel `tailCtx` is `stopCh` (real shutdown) or the `partitionTailCheckTimeout` itself.

### 7.6 Tail-check is single-shot, not a retry loop

Intentional. If a second emergency arrives in the narrow window AFTER the tail-check's `observeAndDecide` finished but before the watcher select-loop is re-entered, the next `pollTicker` tick picks it up. A retry loop would re-enter the partition watcher's responsibility and risk starvation against other arms; one tail-check restores parity with pre-PR-3's "emergency claim was Idle→Emergency without delay" guarantee for the dominant scenario.

### 7.7 Reconcile interval vs ticker drift on slow hosts

Idempotent; `lastApplied` guard makes a coalesced double-tick a no-op.

### 7.8 Test-double interface widening churn

Every implementation of `assignmentCalculator` must add `GetState`. Grep first:

```sh
grep -rn "assignmentCalculator\b\|SubscribeToStateChanges\b" --include="*.go" .
```

Known implementations as of HEAD `96359fe`: `internal/assignment.Calculator` (already has `GetState`), `internal/assignment.NopCalculator` (confirm), `stubCalculator` and `monitorTestCalculator` test doubles.

---

## 8. Acceptance criteria

Before this PR can be merged:

1. **All new tests pass** under `go test ./... -count=1 -race`:
   - Test 5.1 (reconcile recovers still-current dropped state).
   - Test 5.2 (partition lifecycle routes through FSM).
   - Test 5.3 (claim-failure duplicate-rebalance bound — required, was optional in v1; v4 retargets to a calculator-side `handlePartitionRebalance` body-entry counter with bound `== 1`).
   - Test 5.4 (grace-flip regression — new in v2).
   - Test 5.5 (emergency contention regression — new in v2; v3 corrected to assert `StateEmergency` and drop cross-tuple hook ordering).
   - Test 5.7 (tail-check uses a fresh context — new in v3).
   - Test 5.6 if included (optional invariant-boundary guard).
2. **Existing tests pass.** No regression in `manager_assignment_test.go`, `manager_assignment_fixes_test.go`, `internal/assignment/calculator_test.go`. Any test asserting on `OnStateChanged` count for a partition-source-change scenario MUST be updated per §3.8.
3. **Full test suite passes** under `-race`. No new flakes.
4. `go vet ./...` and the configured linter pass without new warnings.
5. `Manager.State()` observably enters `StateRebalancing` during a partition rebalance — verified by Test 5.2.
6. `OnStateChanged` fires for `(Stable → Rebalancing)` and `(Rebalancing → Stable)` during partition rebalance — verified by Test 5.2.
7. A dropped subscriber event whose state remains current is recovered within `2 * calculatorStateReconcileInterval` — verified by Test 5.1.
8. `restorePendingOnGraceBail` is exercised under partition lifecycle — verified by Test 5.4.
9. Emergency-during-partition-rebalance is recovered without waiting for a poll — verified by Test 5.5.
10. Emergency-during-partition-rebalance is recovered EVEN WHEN the original rebalance `reqCtx` has been exhausted (fresh tail-check context) — verified by Test 5.7.
11. `docs/REFERENCE.md:358` glossary edit lands — verified by `grep -n "Rebalancing.*worker or partition-source" docs/REFERENCE.md`.
12. `/post-impl-review` (Codex `xhigh` for v1) returns a MERGE verdict.

---

## 9. Out of scope (explicitly NOT in this PR)

| Item | Why deferred |
|---|---|
| Replace subscriber buffer-drop with coalescing semantics (Option B/C from §2.1) | Not needed under Option A's narrowed invariant. Future PR if reconcile cadence proves operationally too slow or if a stronger invariant (Option D — lossless event path) is required. |
| Stronger reconcile invariant (replay of every missed transition) | Requires producer-side redesign — see §10.3. Out of PR-3's Tier-P1 budget. |
| Add `pendingWorkerUpdate` flag (W6) | PR-4 (W2+W13) addresses heartbeat-watcher recovery latency; W6 becomes redundant per `00-fix-plan.md` Tier P3. |
| Edit `types/state.go` / `types/calculator_state.go` Godoc | Option Y was rejected (§2.2). Existing Godoc remains correct after PR-3. |
| Add metric for "reconcile detected divergence" | Useful diagnostic but not required for correctness. |
| Address W2/W13 (heartbeat watcher rewatch) | PR-4 (next in sequence). |
| Generalize the tail-check to other claim-failure paths (e.g., emergency-during-scaling) | The tail-check is targeted at the specific gap Option X introduces. Other claim-failure paths retain their pre-PR-3 behavior (poll-driven retry). |

---

## 10. Known limitations NOT addressed by PR-3

### 10.1 Tail-check is single-shot

§7.6 details. A second emergency arriving in the narrow window AFTER the tail-check completes but BEFORE the watcher select-loop re-runs falls back to the next `pollTicker` tick. Operationally equivalent to a poll-driven re-detection at the rate the existing emergency-detector grace allows. **This is the only remaining residual** for the tail-check after v3.

**Closed in v3 (plan-review v2 P0-B):** "tail-check uses an already-exhausted rebalance context" is no longer a residual. §3.5 allocates a fresh stop-aware context (`ctxFromStopCh(c.stopCh, partitionTailCheckTimeout)`, 15 s) for `observeAndDecide`. Test 5.7 is the regression guard.

**Suggested follow-up:** none required for the single-shot residual. If empirical poll latency under load proves problematic, a future PR could move emergency detection off the poll tick onto a dedicated watcher (separate scope, separate spec).

### 10.2 Reconcile does not replay completed transitions

If a `Scaling → Rebalancing → Idle` burst completes entirely between two reconcile ticks AND the subscriber dropped every event, the `Rebalancing` state is NOT re-projected: reconcile sees only the current `Idle`. `OnStateChanged` hooks for the missed transitions do NOT fire. Test 5.6 (optional) documents this boundary.

**Suggested follow-up:** Option D from §2.1 — a lossless event path (sequence numbers / transition queue) or Option C (subscriber-side coalescing in addition to consumer-side reconcile). Either is a producer-side redesign — separate PR.

### 10.3 Duplicate partition rebalance on claim-failure retry

§2.2.3 details. Test 5.3 enforces the bound (at most one extra cycle within one `RebalanceGraceDrainInterval`). The retry cycle's `rebalance` body re-snapshots and finds no source delta; publish is a no-op for assignment contents but `OnStateChanged` still fires.

**Suggested follow-up:** add a source-revision generation check to `pendingPartitionUpdate` so an in-flight rebalance that observed a fresh source generation can clear the bit. ~15 LOC + 1 test. Separate PR if the duplicate cycle proves operationally noisy in production.

### 10.4 Subscriber buffer remains drop-tolerant

The 4-slot buffer at `state_machine.go:119` and the `trySend` drop-on-full at `state_subscriber.go:24-29` are unchanged. PR-3 adds a reconcile arm on the consumer side; producer-side drop semantics are preserved. Any future consumer of `SubscribeToStateChanges` that depends on lossless delivery (no such consumer exists today) MUST add its own reconcile, OR PR-3's reconcile pattern MUST be promoted to a coalescing primitive (Option C). Documented here as a contract for future callers.

### 10.5 No reconcile metric

The reconcile arm does not increment a counter for "reconcile drove a corrective transition." Such a metric would expose buffer-overflow rate operationally. Not in PR-3's scope.

---

## 11. Verification checklist for the implementer

Run all of the following before requesting `/post-impl-review`:

1. `gofmt -l . | grep -v vendor` — no formatting diff.
2. `go vet ./...` — clean.
3. The configured linter (per `AGENTS.md` workflow): `make lint` or `golangci-lint run`. Resolve any new warnings.
4. `go build ./...` — clean.
5. `go test ./... -count=1` — pass.
6. `go test ./... -count=1 -race` — pass.
7. `grep -rn "monitorCalculatorState\|TryClaimRebalancing\|RunClaimedRebalanceErr\|handlePartitionRebalance\|calculatorStateReconcileInterval" --include="*.go" .` — verify call sites match §3.
8. `grep -n "Rebalancing.*worker or partition-source" docs/REFERENCE.md` — verify glossary edit.
9. Manual smoke test: bring up a 3-worker cluster, add a partition to the source, observe `OnStateChanged` recorder via a test hook. Expected: `Stable → Rebalancing → Stable` for each worker on the leader-side.
10. Re-verify all `file:line` anchors in §1 against HEAD before commit; if HEAD has advanced beyond `96359fe`, refresh.

---

## 12. Model & effort recommendations (from `00-fix-plan.md` §"Per-PR matrix")

| Phase | Tool | Model / effort |
|---|---|---|
| Planning (this spec, v5 — final) | Claude Code | **Opus 4.7** — done |
| Implementation | Claude Code | **Opus 4.7** — touches state machine, adds new callback / runner, propagates errors across goroutines |
| Plan review (pre-impl, v1/v2) | `/plan-review` | Codex **xhigh** |
| Plan review (pre-impl, v3/v4) | `/plan-review` | Codex **high** (precision pass over earlier deltas) |
| Final plan review (v5) | `/final-plan-review` | Codex **high** (precision sweep — stale text, numbering drift) |
| Post-impl review (v1) | `/post-impl-review` | Codex **xhigh** |
| Post-impl review (v2+) | `/post-impl-review` | Codex **high** |

Rationale: PR-3 v5 includes the v1 design call PLUS the v2 P0 fixes (error propagation through a new FSM runner, emergency tail-check) PLUS the v3 fresh-context fix and corrected test signals PLUS the v4 retargeted `handlePartitionRebalance` body-entry counter + `partitionRebalanceRequestTimeout` override seam PLUS the v5 `partitionRebalanceBlocker` determinism seam. The implementation surface is moderate-large (~95 LOC + 5 required tests + 1 optional, ceiling ~97) but the cost of a subtle wrong assumption (incorrect claim ordering, missing notify, broken `restorePendingOnGraceBail` propagation, tail-check starved by exhausted ctx, test 5.3 measuring a drop-tolerant signal, Test 5.7 timing race) is high. Opus 4.7 for implementation; xhigh on the first review pass.

Estimated reviewer wall-time budget: ~14 min across one `/plan-review` + 1–2 `/post-impl-review` rounds.

---

## 13. Revision history

| Version | Date | Summary |
|---|---|---|
| v1 | 2026-05-19 | Initial draft. Design Call 1: Option A (periodic reconcile). Design Call 2: Option X (route partition lifecycle through FSM via new `TryClaimRebalancing` claim primitive). ~70 LOC + 2 tests. Plan-review returned 2 P0 / 3 P1 / 1 P2. |
| v2 | 2026-05-19 | Closes all v1 plan-review findings. P0-A: added `StateMachine.RunClaimedRebalanceErr` + `Calculator.handlePartitionRebalance` so `errShuttingDown` propagates to `restorePendingOnGraceBail`. P0-B: added in-line tail-check via `observeAndDecide` after partition rebalance completes. P1-A: explicit duplicate-rebalance bound in §4; Test 5.3 promoted to required. P1-B: §2.1 invariant narrowed to eventual projection; §4 / §10.2 updated; Test 5.1 narrowed; optional Test 5.6 added. P1-C: §4 hook-order wording reworked to distinguish FSM enqueue order from user-hook execution order. P2: glossary edit at `docs/REFERENCE.md:358`. Tests: 2 required → 4 required + 1 optional. LOC: ~70 → ~88 (ceiling ~90). Plan-review v2 returned 1 residual P0 + 2 P1. |
| v3 | 2026-05-19 | Closes plan-review v2 findings. **Residual P0-B (tail-check context exhaustion):** §3.5 now allocates a fresh stop-aware context for the tail-check via `ctxFromStopCh(c.stopCh, partitionTailCheckTimeout)` with `partitionTailCheckTimeout = 15 * time.Second`; tail-check is no longer starved by an exhausted `reqCtx`. New Test 5.7 added as a dedicated context-exhaustion regression (kept separate from Test 5.5 so the emergency-contention test stays focused on FSM ordering rather than context-deadline timing). §7.5 and §10.1 updated. **P1 (Test 5.5 wrong manager state + forbidden ordering):** Test 5.5 corrected to assert `StateEmergency` (per `manager_state.go:230-234`, `types/state.go:38-39`) and eventual presence of all four `(from,to)` tuples without ordering between the two cycles (per async `invokeHook` at `manager.go:827-839`); strict ordering deferred to the calculator-side `SubscribeToStateChanges` stream. Residual stale parenthetical in Test 5.2 removed. **P1 (Test 5.3 wrong signal):** Test 5.3 retargeted from the drop-tolerant async hook recorder to a deterministic producer-side `TryClaimRebalancing` invocation counter (lowest test-only surface: an `atomic.Int64` on `StateMachine` exposed via `export_test.go`); test fails if more than one extra `TryClaimRebalancing` runs per dropped partition update. LOC: ~88 → ~90 (ceiling ~92; +3 production LOC for fresh-context allocation + named timeout var, +0 test production LOC for the two test corrections). Tests: 4 required → 5 required + 1 optional. |
| v4 | 2026-05-19 | Closes plan-review v3 residuals (0 P0, 2 P1, 1 P2). **P1-A (Test 5.3 bound):** Test 5.3 signal changed from `TryClaimRebalancing` invocation count (timing-dependent under the drain ticker) to `handlePartitionRebalance` body-entry count on the `Calculator`; bound tightened from `≤ 2` to `== 1` per dropped partition update, matching the §2.2.3 / §10.3 "at most one extra cycle" invariant directly. Counter is an `atomic.Int64` on `Calculator`, incremented as the first line of the partition-rebalance callback, exposed via `export_test.go`. **P1-B (Test 5.7 non-determinism, attempt 1):** §3.5 introduces package-level var `partitionRebalanceRequestTimeout` (default 30 s, matching legacy literal) replacing the inline `30*time.Second` in `triggerPartitionRebalance`. Test 5.7 overrides it to 10 ms via `export_test.go` so `reqCtx` is provably cancelled by the time `RunClaimedRebalanceErr` returns, removing the wall-clock race; v3's "delay-hook" option dropped. **P2 (15 s justification):** §3.5 comment for `partitionTailCheckTimeout` now derives the 15 s value from one worker-set KV read p99 + reconnect headroom, the negligible `TryClaimEmergency` CAS cost, and ratios to `partitionRebalanceRequestTimeout` (30 s) and `EmergencyGracePeriod` (5 s default); framed as an operational policy bound, not an empirically proven threshold. LOC: ~90 → ~93 (ceiling ~95). Tests: 5 required + 1 optional — unchanged in count. Plan-review v4 returned 0 P0 + 1 residual P1 (P1-B not fully closed). |
| v5 | 2026-05-19 | Closes the v4 residual P1-B (Test 5.7 race). v4's 10 ms `partitionRebalanceRequestTimeout` override alone did not guarantee `reqCtx.Err() != nil` by the time `RunClaimedRebalanceErr` returned — a sufficiently fast `c.rebalance` body could complete in under 10 ms and let the test pass without checking the invariant. v5 adds `partitionRebalanceBlocker chan struct{}`, a nil-by-default package-level test-only seam in `internal/assignment`, consulted by `handlePartitionRebalance` AFTER the `partitionRebalanceEntries` increment and BEFORE the call into `c.rebalance`. Test 5.7 installs a non-nil channel, waits for the entry counter to bump (proving the callback is parked), sleeps past the 10 ms `reqCtx` deadline, then closes the blocker — making `reqCtx.Err() != nil` at `RunClaimedRebalanceErr` return structurally guaranteed rather than probabilistic. Other rebalance paths (scaling, emergency via `handleRebalance`) do NOT consult the blocker; production cost is one nil-load per partition-lifecycle callback invocation. v4 closures (P1-A Test 5.3 bound, P2 15 s derivation) and v3 closures (P0-A error propagation, P0-B fresh tail-check context, P1 Test 5.5/5.3 signal corrections) remain intact and unchanged. LOC: ~93 → ~95 (ceiling raised to ~97; +2 production LOC for the test-only `partitionRebalanceBlocker` var declaration and its nil-guarded `<-h` line inside `handlePartitionRebalance`). Tests: 5 required + 1 optional — unchanged in count. |
