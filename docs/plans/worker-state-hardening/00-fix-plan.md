# Worker-State Mechanism Hardening — Priority Fix Plan

Derived from `tmp/worker_state_analysis/00-report.md` v3 (Codex-reviewed) and the consolidated review at `tmp/worker_state_analysis/02-consolidated.md`. Sorted by **user impact**, not severity tier — see the audit's §6 for the explanation.

---

## Re-ranked findings

### Tier P1 — Ship before next release

**W12 (S2)** — **Legacy assignment-alias watcher exits permanently on channel close.** No rewatch, no reconcile. Mid-upgrade workers from v2.3.0→v2.x can miss a fresher legacy alias and stay on a stale assignment until restart.
- **Evidence:** `manager_assignment.go:287-289, 336-340` (watcher exits clean on `!ok`); contrast `monitorCommitChanges` (`manager_assignment.go:407-432`) which rewatches.
- **Effort:** ~30 LOC + 2 tests. Pattern-mirror of `monitorCommitChanges` is direct.
- **Why P1:** Only S2 with authority impact. Every other S2 lies about state; W12 can leave a worker holding the wrong partitions.
- **Spec:** [`01-pr1-spec.md`](./01-pr1-spec.md)

**W1 (S2)** (subsumes prior W5) — **`Manager.State()` lies during real leader-side work.** Two causes:
- (a) calculator-state subscriber drops on full buffer (`internal/assignment/state_subscriber.go:24-29`) and `monitorCalculatorState` has no fallback reconcile (`manager_assignment.go:164-191`).
- (b) `monitorPartitions` bypasses the FSM entirely (`calculator.go:648-656, 675-728`), violating the contract that says `Rebalancing` means "active partition rebalance in progress" (`types/state.go:35-36`, `types/calculator_state.go:24-25`).

Single PR must cover BOTH causes — half-fixing leaves half the symptom.
- **Effort:** ~50 LOC + 2 tests + 1 design call (route partition lifecycle through FSM vs narrow contract).
- **Why P1:** Operator-pervasive. Dashboards, hooks, readiness probes all read `Manager.State()`.

### Tier P2 — Bundle into PR-3/4/5

**W2 (S3)** — **Heartbeat watcher: no rewatch on close + no active-recovery on silent stall.** Polling preserves convergence at 7.5s default cadence; the gap with the watcher fast-path (~100ms) is 75× wider than v2 originally claimed.
- **Evidence:** `worker_monitor.go:329-332` (exit on close, no rewatch); `worker_monitor.go:235-247` (polling fallback).
- **Effort:** Bundle with W13 in PR-3 (~30 LOC + 1 test for the watcher portion).

**W13 (S3)** — **Heartbeat publisher synchronous `Put` blocks past short `HeartbeatTTL`.** 5s Put timeout (`internal/heartbeat/publisher.go:310-314`) can exceed `HeartbeatTTL` under aggressive tuning, causing the worker's own heartbeat to expire while it's healthy.
- **Effort:** Bundle with W2 in PR-3 (~20 LOC + validation + 2 tests for the publisher portion).

**W3 (S3)** — **Direct rebalances leave `lastWorkers` stale.** `TriggerRebalance` and `triggerPartitionRebalance` update `currentWorkers` but not `lastWorkers`; next poll fires a duplicate `planned_scale` after cooldown.
- **Evidence:** `calculator.go:399-417` (`TriggerRebalance`), `calculator.go:648-656` (partition lifecycle), `calculator.go:1278-1285` (`rebalance` updates `currentWorkers` only), `calculator.go:1057-1075` (`handleRebalance` is the only path that repairs `lastWorkers`).
- **Effort:** ~10 LOC (move `setLastWorkersLocked` into `rebalance` success path) + 1 test. Most visible to operators despite S3 severity — likely PR-4 because the fix is so small.

**W10 (S3)** — **Long Stop sequence can let election TTL expire while heartbeat is still in KV.** Real risk only when Stop body exceeds 10s election TTL; defensive reorder is cheap.
- **Effort:** ~20 LOC + 1 test in PR-5.

### Tier P2 (cont.) — Pre-existing concurrency bugs surfaced during PR-1 v2 review

**W15 (S2)** — **Cross-path stale-store: blocked commit can overwrite fresher alias.** A commit (LR=3, V=10) holds `pendingApplyInFlight` and is blocked inside `handoffCoordinator.Apply`. A fresher alias (LR=4, V=10) arrives, is picked by `selectAuthority`, and applies — `m.assignment` now stores LR=4. The blocked commit resumes and `m.assignment.Store`s LR=3 over the LR=4 snapshot. No post-`Apply` authority recheck (`manager_assignment.go:728-781`).
- **Evidence:** `manager_assignment.go:540-553, 582-596, 745-781`. Surfaced by Codex during the PR-1 v2 review (`tmp/01-pr1-spec_pr1-impl-spec_v2_review.md` P0 #1).
- **Effort:** ~30 LOC + 2 tests. Either (a) post-`Apply` pre-`Store` revalidation against `m.CurrentAssignment()`, or (b) extend `pendingApplyInFlight` into a shared apply-interlock covering both commit and alias paths.
- **Why P2:** correctness-class bug under rolling-upgrade + heavy churn. Same severity tier as W12, but PR-1's W12 fix doesn't widen the race window — both paths already drive concurrent apply attempts today through their independent watcher goroutines.

**W16 (S3)** — **`scheduleApplyRetry` concurrent with new applies.** Failed apply retries run in a separate goroutine (`manager_assignment.go:849-908`) that calls `applyAssignment` directly without participating in any in-flight gate. Stale-retry `Store` can regress the snapshot — same shape as W15, different trigger.
- **Effort:** Bundle with W15. The fix shape (pre-`Store` revalidation OR shared interlock) closes both at the same site.
- **Why P2/P3:** less reachable than W15 (requires a transient apply failure first), but should be closed by the same PR as W15.

### Tier P3 — Operational / cosmetic

**W7 (S3)** — **Tight `EmergencyGracePeriod` vs GC pauses.** Config validation only enforces `EmergencyGracePeriod ≤ HeartbeatTTL`; the actual hysteresis budget is the spread between TTL and grace.
- **Effort:** ~15 LOC validation warning in PR-6.

**W6 (S3)** — **No `pendingWorkerUpdate` flag for workers blocked by grace/cooldown.** Asymmetric with `pendingPartitionUpdate`. Next poll re-detects within 7.5s anyway.
- **Effort:** Optional; only if PR-3 (W2) is deferred and the latency matters.

**W4 (S3)** — **Resolver reconcile cadence dependency.** Already warned at startup (`manager_setup.go:213-230`). No code change required.

**W9 (S3)** — **Concurrent emergency + partition-lifecycle ordering untested.** Theoretical; add a targeted test in PR-2 or PR-5.

**W8 (S4)** — **Detector resets on every leader handoff.** Metric undercount only; partition coverage preserved. Document or accept.

**W11 (S4)** — **Watchers ignore delete events.** Documentation pass.

**W14 (S4)** — **Cold-start monitor-start gap.** Not a bug; documented for completeness.

### Tier P-open — Verify before scoping

Three scenarios neither the audit nor either reviewer closed. Need a separate investigation pass before they can be promoted to PRs:

- **Capability-bit propagation.** Does any code path advertise `CapAckV1` while the ack-publish wiring is broken? (Reference: `types/heartbeat.go:40-47` capability bit semantics.)
- **Rolling-upgrade `LeaderRevision` consistency.** Does `m.lastSeenLeaderRevision` initialize correctly across handoff under all bootstrap paths?
- **Source-revision rollback handling.** Custom `WatchablePartitionSource` implementations emitting a regressed `SourceRevision` — defensive guard or contract-only?

---

## Recommended PR sequence

| PR | Bundle | Effort |
|---|---|---|
| **PR-1** | W12 — legacy alias watcher rewatch + reconcile | ~30 LOC + 5 tests |
| **PR-2** | W15+W16 — cross-path stale-store + apply-retry race (pre-`Store` revalidation OR shared apply-interlock) | ~30 LOC + 3 tests |
| **PR-3** | W1+W5 — `Manager.State()` reconcile + partition-lifecycle FSM contract | ~50 LOC + 2 tests + design call |
| **PR-4** | W2+W13 — heartbeat watcher rewatch + publisher backpressure | ~50 LOC + 3 tests |
| **PR-5** | W3 — `lastWorkers` symmetry across direct rebalance paths | ~10 LOC + 1 test |
| **PR-6** | W10 — Stop ordering / election release timing | ~20 LOC + 1 test |
| **PR-7** | W7 — config validation warning + W6 if needed | ~15 LOC + 1 test |
| Doc | W11, W14, open-verification items | ~50 LOC doc |

Total in-scope correctness effort: **~205 LOC + 15 tests + 2 design calls + doc pass** (PR-2's revalidation-vs-interlock decision and PR-3's FSM-route-vs-narrow-contract decision), excluding the open-verification round.

**Why PR-2 (W15+W16) is sequenced right after PR-1:** they are the second-highest user impact after PR-1 (both S2 authority concerns), and PR-1's narrowing relies on §10 documenting these as deferred — closing them in PR-2 cleans up that deferral promptly.

---

## Model & effort recommendations per PR

The prior `assignment-correctness-fixes` plan didn't bake model recommendations into the spec — they were left to the reviewer skills' defaults. This plan makes them explicit so each phase can be dispatched with the right cost/effort tradeoff.

**Conventions used below:**

- **Planning** = drafting the per-PR implementation spec (e.g., `01-pr1-spec.md`).
- **Implementation** = writing the actual code changes against the spec.
- **Plan review** = `/plan-review` (or `/final-plan-review` for the precision pass after architectural review is settled).
- **Post-impl review** = `/post-impl-review` (spec-compliance + lint/build/test validation).
- Models: Opus 4.7 (deepest reasoning), Sonnet 4.6 (capable + fast), Haiku 4.5 (mechanical).
- Codex effort tiers: `xhigh` (correctness-critical first passes), `high` (subsequent rounds / smaller scope).

### Per-PR matrix

| PR | Planning | Implementation | Plan-review (skill / model) | Post-impl-review (skill / model) |
|---|---|---|---|---|
| **PR-1 (W12)** — legacy alias watcher | **Opus 4.7** — needs reasoning about mixed-version rolling-upgrade semantics, `LeaderRevision` fences, and idempotent re-application | **Opus 4.7** — same; subtle interleavings with `selectAuthority` | `/plan-review` Codex **xhigh** (high-stakes authority path) | `/post-impl-review` Codex **xhigh** v1; **high** v2+ |
| **PR-2 (W15+W16)** — cross-path stale-store + apply-retry race | **Opus 4.7** — design call between post-`Apply` revalidation vs shared apply-interlock; both options touch the load-bearing apply path | **Opus 4.7** — invariant-critical; must preserve commit case-(e) coalescing semantics | `/plan-review` Codex **xhigh** | `/post-impl-review` Codex **xhigh** v1; **high** v2+ |
| **PR-3 (W1+W5)** — `Manager.State()` reconcile + FSM contract | **Opus 4.7** — includes a design call (route partition lifecycle through FSM vs narrow public contract); needs to reason about subscriber semantics, hook-firing order, contract drift across `types/state.go` and `types/calculator_state.go` | **Opus 4.7** — touches the state machine; needs to preserve existing strict-source CAS semantics | `/plan-review` Codex **xhigh** | `/post-impl-review` Codex **xhigh** v1; **high** v2+ |
| **PR-4 (W2+W13)** — heartbeat watcher rewatch + publisher backpressure | **Sonnet 4.6** — pattern-mirror of `monitorCommitChanges`; little novel design | **Sonnet 4.6** — mechanical translation of the commit-watcher pattern + a non-blocking publish path | `/plan-review` Codex **high** (template is known) | `/post-impl-review` Codex **xhigh** v1 (touches a load-bearing detection path); **high** v2+ |
| **PR-5 (W3)** — `lastWorkers` symmetry | **Sonnet 4.6** — single-line move; spec is mostly justification | **Sonnet 4.6** (or **Haiku 4.5** if straightforward) — `setLastWorkersLocked` migration into `rebalance` success path | Skip `/plan-review` (too small; LOC is ~10) — go straight to `/final-plan-review` Codex **high** if any uncertainty | `/post-impl-review` Codex **high** v1 |
| **PR-6 (W10)** — Stop ordering | **Sonnet 4.6** — ordering analysis is bounded; election-TTL math is explicit | **Sonnet 4.6** — `manager.go` Stop reorder + bounded timeout on election release | `/plan-review` Codex **high** | `/post-impl-review` Codex **high** v1 |
| **PR-7 (W7)** — config validation | **Haiku 4.5** — pure config-validation addition | **Haiku 4.5** — single validation rule + test | Skip | `/post-impl-review` Codex **high** v1 |
| Doc (W11, W14, open list) | **Haiku 4.5** — doc-only | **Haiku 4.5** | Skip | Skip — manual review only |

### Justification for the model tier breaks

- **Opus 4.7** is reserved for the two PRs (PR-1, PR-2) where the cost of a subtle wrong assumption is high: PR-1 affects authority during rolling upgrades; PR-2 changes the state-machine contract and the hook-firing order. Both have non-trivial design space and benefit from the deepest reasoning model.
- **Sonnet 4.6** is the default for the mechanical-but-still-careful PRs (PR-3, PR-4, PR-5). The pattern exists (commit watcher) or the change is small enough that the reasoning surface is bounded.
- **Haiku 4.5** is fine for pure config / doc work where there's no reasoning to do.
- **Codex `xhigh`** for the first review pass on PR-1, PR-2, PR-3 — they touch correctness-critical paths and the first review catches the most. Subsequent rounds at `high` after the structural issues are closed.
- **Codex `high`** for the smaller PRs from the start — the surface area doesn't warrant the cost difference.

### Aggregate cost estimate

Assuming each `/plan-review` is ~2-5 min @ `xhigh` (~3 min @ `high`) and each `/post-impl-review` is ~3-8 min:

- PR-1: ~15 min reviewer wall-time (1 plan-review + 2 post-impl rounds).
- PR-2: ~20 min reviewer wall-time (1 plan-review + 2-3 post-impl rounds — design call may need a re-review).
- PR-3: ~12 min reviewer wall-time.
- PR-4: ~5 min reviewer wall-time.
- PR-5: ~8 min reviewer wall-time.
- PR-6 + Doc: ~5 min reviewer wall-time combined.

**Total reviewer wall-time: ~65 min** across the plan (not counting human review / iteration).

---

## What NOT to do, and why

| Item | Skip because |
|---|---|
| Remove the calculator state-machine subscriber pattern entirely | Hook contracts depend on it; PR-2 fixes it, doesn't replace it. |
| Persist emergency detector state to KV across leader handoff (W8 fix) | Net loss: persistence + race surface outweighs the metric undercount. Document instead. |
| Add a `pendingWorkerUpdate` flag (W6 standalone) | Only matters if PR-3 (W2) is deferred; otherwise redundant with the faster watcher recovery. |
| Optimize partition-source `Watch` contract | Out of scope for `internal/assignment` correctness. Source contract is a separate surface. |
| Touch the assignment/commit delete-event handling (W11) | "Ignore delete" is intentional for leader-transition tolerance. Document the constraint; don't change the behavior. |
| Capability-bit propagation, `LeaderRevision` consistency, source-revision rollback | Pre-scope: needs a verification round first (see Tier P-open). |

---

## Summary

- **PR-1 (W12, ~30 LOC + 5 tests):** legacy alias watcher rewatch + reconcile. Mechanical pattern-mirror; only S2 with watcher-level authority impact. Spec ready in `01-pr1-spec.md` (v3).
- **PR-2 (W15+W16, ~30 LOC + 3 tests):** cross-path stale-store + apply-retry race. Post-`Apply` revalidation OR shared apply-interlock. The other S2 with authority impact, surfaced by Codex during PR-1 review.
- **PR-3 (W1+W5, ~50 LOC + 2 tests):** `Manager.State()` reconcile + partition-lifecycle FSM contract decision. Pervasive observability fix.
- **PR-4 (W2+W13, ~50 LOC + 3 tests):** heartbeat watcher rewatch + publisher non-blocking. Reduces detection-latency degradation.
- **PR-5 (W3, ~10 LOC + 1 test):** `lastWorkers` symmetry. Most visible to operators.
- **PR-6 (W10, ~20 LOC + 1 test):** Stop ordering / election release timing.
- **PR-7 (W7, ~15 LOC + 1 test):** config validation warning.
- **Doc:** W11, W14, open-verification items.

Total: **~205 LOC + 15 tests + 2 design calls** (PR-2's revalidation-vs-interlock + PR-3's FSM-route-vs-narrow-contract), shippable in a focused 6–8 days. Reviewer wall-time budget ~80 min across all PRs.
