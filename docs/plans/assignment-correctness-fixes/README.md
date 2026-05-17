# `internal/assignment` Correctness Fixes

Priority fix plan derived from a deep audit of `internal/assignment` and reconciled against the IOPS-investigation findings (which falsified the IOPS-side hypotheses and refocused the work on correctness).

## Why this exists

A deep review of `internal/assignment` surfaced 8 confirmed correctness findings. In parallel, the IOPS investigation (`docs/plans/iops-investigation/`) measured that 88% of the user-observed IOPS cost lives in JetStream's data stream — NOT in parti's coordination loops. This plan separates the two concerns:

- **Correctness work** (this plan): independently valuable; ship in 4 PRs.
- **IOPS work** (separate): operator-side mitigation (`Storage = memory`) + a future architectural investigation narrowed to `consumer.Dynamic`.

## Status

| Phase | State |
|---|---|
| Deep audit | Done — `tmp/assignment_review/00-workflow.md` and 01-07 phase outputs |
| IOPS investigation reconciliation | Done — confirmed CC-IOPS-1..6 are not priority IOPS targets |
| Codex review of priority plan | Done — three corrections applied (see `00-fix-plan.md` and audit-trail in `tmp/`) |
| PR-1 (lifecycle bundle) | Pending — ~30 LOC |
| PR-2 (audit clock-skew) | Pending — ~20 LOC |
| PR-3 (housekeeping) | Pending — ~25 LOC |
| PR-4 (CAS-loss recovery) | Pending, **gated on repro evidence** — ~50 LOC |
| FINDING-A (Dynamic consumer collapse) | Separate investigation, not yet scoped |

## Layout

| File | Content |
|---|---|
| `README.md` | This file. Entry point and status. |
| `00-fix-plan.md` | The full priority fix plan (Codex-reviewed). Ranked findings, PR sequencing, what NOT to fix. |

## Supporting artifacts (in `tmp/assignment_review/`)

Not checked in (under `tmp/`), but referenced by the plan:

- `00-workflow.md` — 8-phase audit workflow
- `00-index.md` — phase-by-phase summaries
- `01-architecture-inventory.md` — 13 component cards, file:line anchors
- `02-flow-trace.md` — 7 end-to-end flow traces
- `03-concurrency-audit.md` — lock-ordering audit + 22 Q3 triage
- `04-failure-mode-audit.md` — 12 failure-mode walks
- `05-issue-catalog.md` — 8 confirmed issues (ISSUE-001..008) + 6 IOPS cross-cut items
- `06-synthesis.md` — 5 cross-cutting themes
- `07-verification-plan.md` — 8 concrete test designs
- `_prior_findings.md` — 23 prior-review findings catalogued

## Cross-references

- `docs/plans/iops-investigation/findings.md` — IOPS root-cause evidence (data stream dominates)
- `docs/plans/iops-investigation/m17-findings.md` — M1.7 ablation that proved the conclusion
- Codex review of this plan: dispatched via `codex:codex-rescue` 2026-05-17; corrections applied inline

## PR sequencing (from `00-fix-plan.md`)

| PR | Bundle | Effort | Gating |
|---|---|---|---|
| **PR-1** | ISSUE-002 + ISSUE-003 + ISSUE-005 (partition-triggered rebalance lifecycle) | ~30 LOC + 3 tests | None |
| **PR-2** | ISSUE-007 (audit clock-skew → monotonic observed-at) | ~20 LOC + 1 test | None |
| **PR-3** | ISSUE-006 (comment) + ISSUE-008 (cold-start) + ISSUE-004 (docstring) | ~25 LOC + 1 benchmark | None |
| **PR-4** | ISSUE-001 (CAS-loss recovery) | ~50 LOC + 2 tests | **Repro evidence required first** |

Recommended start: **PR-1** (highest leverage from one small helper).
