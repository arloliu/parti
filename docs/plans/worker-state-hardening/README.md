# Worker-State Mechanism Hardening

Priority plan derived from the worker-state-transition audit (`tmp/worker_state_analysis/00-report.md` v3) and consolidated reviews (Codex `/plan-review`, in-conversation self-review). The audit initially surfaced 14 weak points (W1–W14); the Codex review of PR-1 v2 surfaced two additional pre-existing latent races (W15, W16). This plan ships all 16 weak points as **7 focused PRs plus a documentation pass**.

## Why this exists

The audit's focus was *"can the leader miss a transition and leave workers orphaned or in a falsely stable state?"* Two findings carry actual authority impact:

- **W12** — the legacy assignment-alias watcher (the v2.3.0→v2.x rolling-upgrade path) exits permanently on channel close with no rewatch and no reconcile (`manager_assignment.go:287-289, 336-340`). Mid-upgrade workers can miss a fresher alias and stay on a stale assignment. **The only S2 finding that affects authority, not just observability.**
- **W1+W5** — `Manager.State()` lies during real leader-side work, via two causes (subscriber drops + partition-lifecycle FSM bypass). Observable from dashboards, hooks, readiness probes.

The rest are S3/S4 — latency, churn, metric noise, footguns — that ship as smaller PRs.

## Status

| Phase | State |
|---|---|
| Audit (v3, post-Codex review) | Done — `tmp/worker_state_analysis/00-report.md` |
| Consolidated review | Done — `tmp/worker_state_analysis/02-consolidated.md` |
| PR-1 (W12 — legacy alias watcher) | Pending — spec in `01-pr1-spec.md` v3 |
| PR-2 (W15+W16 — cross-path stale-store + apply-retry race) | Pending — spec TODO; new since v1 (surfaced by Codex during PR-1 review) |
| PR-3 (W1+W5 — `Manager.State()` reconcile + FSM contract) | Pending — spec TODO |
| PR-4 (W2+W13 — heartbeat watcher rewatch + publisher backpressure) | Pending — spec TODO |
| PR-5 (W3 — `lastWorkers` symmetry) | Pending — spec TODO |
| PR-6 (W10 — Stop ordering) | Pending — spec TODO |
| PR-7 (W7 — config validation) | Pending — spec TODO |
| Doc (W11, W14) | Pending — bundle with PR-7 or standalone |
| Open verification (capability bits, `LeaderRevision` consistency, source-revision rollback) | Pending — separate investigation; not part of this plan |

## Layout

| File | Content |
|---|---|
| `README.md` | This file. Entry point and status. |
| `00-fix-plan.md` | Priority fix plan: ranked findings, PR sequencing, model/effort recommendations per phase, what NOT to fix. |
| `01-pr1-spec.md` | PR-1 implementation spec (W12 — legacy alias watcher rewatch + reconcile). |
| `02-pr2-spec.md` (TODO) | PR-2 implementation spec (W15+W16 — cross-path stale-store + apply-retry race; pre-`Apply` revalidation OR shared apply-interlock). |
| `03-pr3-spec.md` (TODO) | PR-3 implementation spec (W1+W5 — `Manager.State()` reconcile + partition-lifecycle FSM contract). |
| ... | Later PRs (PR-4 through PR-7 + Doc) written after the prior one merges (avoids speculative spec drift). |

## Supporting artifacts

Under `tmp/worker_state_analysis/` (not checked in):

- `00-report.md` — the v3 audit
- `00-report_worker-state-v2_review.md` — Codex external review
- `01-self-review.md` — in-conversation self-review
- `02-consolidated.md` — merged corrections

## Cross-references

- `docs/plans/assignment-correctness-fixes/00-fix-plan.md` — prior plan in the same area, used as structural template for this plan
- `tmp/worker-state-transition-investigation.md` — GPT-5.5 reviewer's prior pass (incorporated into the audit)

## PR sequencing (from `00-fix-plan.md`)

| PR | Bundle | Effort | Gating |
|---|---|---|---|
| **PR-1** | W12 — legacy alias watcher rewatch + reconcile | ~30 LOC + 5 tests | None — pattern-mirror of `monitorCommitChanges` |
| **PR-2** | W15+W16 — cross-path stale-store + apply-retry race | ~30 LOC + 3 tests | Design call: post-`Apply` revalidation vs shared apply-interlock |
| **PR-3** | W1+W5 — `Manager.State()` reconcile + partition-lifecycle FSM contract | ~50 LOC + 2 tests + 1 design decision | Design call: route partition lifecycle through FSM vs narrow contract |
| **PR-4** | W2+W13 — heartbeat watcher rewatch + bounded poll + publisher backpressure | ~50 LOC + 3 tests | None |
| **PR-5** | W3 — `lastWorkers` symmetry across direct rebalance paths | ~10 LOC + 1 test | None — single-line move |
| **PR-6** | W10 — Stop ordering / election release timing | ~20 LOC + 1 test | None |
| **PR-7** | W7 — config validation warning for `EmergencyGracePeriod` spread | ~15 LOC + 1 test | None |
| Doc | W11, W14, open-verification items | ~50 LOC doc | None |

Recommended start: **PR-1** (highest authority impact; mechanical pattern; smallest spec).
