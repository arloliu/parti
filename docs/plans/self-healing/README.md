# Self-Healing Hardening

Phased fix plan for the ten findings in [`findings.md`](./findings.md)
(reviewed clean across 9 rounds; trail in
[`review-trail.md`](./review-trail.md)). The findings span the connectivity
layer, KV-bucket state, JetStream stream / consumer recovery, manager
orchestration, and election / double-assignment correctness.

## Why this exists

Parti's self-healing is **strong for transient faults and consumer loss, has a
hard floor at server-side state loss, and has two latent correctness gaps
under *partial* NATS failure** (the F9 / F10 area). The review investigation
established that on this k8s deployment the readiness probe IS the recovery
mechanism — so the organising invariant is:

> **Every unrecoverable failure must trip the Kubernetes readiness probe.**

The plan turns each finding into one PR (F2 split into four), sequenced so
low-risk warm-ups land first and the dominant
correctness work lands against a stable baseline.

## Status

| Phase | State |
|---|---|
| Investigation | Done — [`findings.md`](./findings.md) |
| 9-round findings review | Done — [`review-trail.md`](./review-trail.md) |
| Phased plan (this doc + [`00-fix-plan.md`](./00-fix-plan.md)) | **Plan-review clean (3 rounds)** — ready to implement |
| Phase 0 — F7, F8, F10-B | Not started |
| Phase 1 — F6-A, F3, F1 | Not started |
| Phase 2 — F9-A, F6-B, F5, F2 (×4 PRs), F10-A | Not started |
| Phase 3 — F9-B, F4 | Deferred — gated on operational evidence |

## Layout

```
docs/plans/self-healing/
├── README.md            # This file: status, layout, PR sequencing
├── 00-fix-plan.md       # Phase-organised per-finding plan (scope, design, reproducers, gates, deps)
├── findings.md          # Authoritative findings doc (reviewed clean across 9 rounds)
└── review-trail.md      # Consolidated review history (9 rounds → 1 file)
```

Per-PR specs are written **lazily** — only when the prior PR is merge-clean,
mirroring `docs/plans/worker-state-hardening/README.md`'s explicit pattern:
*"avoids speculative spec drift."* Spec files will be added here as
`01-pr1-spec.md`, `02-pr2-spec.md`, … using the existing convention.

## PR sequence

Full table with anchors, design, reproducers, and gates lives in
`00-fix-plan.md`. Compact view:

Tier = version-neutral model recommendation (**strong** = use the
strongest available Claude model; **standard** = standard tier);
Review = `/codex:review` effort for the post-impl review. See
`00-fix-plan.md` discipline rule 7 for the selection heuristic.

| Order | ID | Finding | Risk | Reproducer | Tier | Review | Notes |
|---|---|---|---|---|---|---|---|
| **Phase 0 — warm-ups** ||||||||
| P0.1 | F7 | Connection-config docs + warning | **LOW** | no | standard | `high` | Docs + read-only warning |
| P0.2 | F8 | `source.WithReconcileInterval(0)` guard | **LOW** | no | standard | `high` | Godoc + warning |
| P0.3 | F10-B | Two-phase config diagnostic warning | **LOW** | yes | standard | `high` | Fires at first two-phase apply, not at `Start` |
| **Phase 1 — additive correctness** ||||||||
| P1.1 | F6-A | Source-bucket escalation hook | LOW–MED | yes | standard | `high` | `OnSourceUnavailable` |
| P1.2 | F3 | stableID NotFound classification | **MED** | yes | standard | `high` | Small surface |
| P1.3 | F1 | Epoch fence | **MED** | yes | **strong** | `xhigh` | **Prerequisite for F9-A** |
| **Phase 2 — dominant fixes** ||||||||
| P2.1 | F9-A | Election bucket → `FileStorage` R≥3 | **LOW** | yes | **strong** | `xhigh` | Depends on F1; operator runbook in spec |
| P2.2 | F6-B | Calculator-layer partition floor | **MED** | yes | **strong** | `xhigh` | Symmetric with F10-A |
| P2.4a | F2 | Envelope + `restartWatcher` wiring | **MED–HIGH** | yes | **strong** | `xhigh` | Envelope crystallises here |
| P2.4b | F2 | Envelope → handoff watcher | **MED–HIGH** | yes | standard | `high` | Reuses envelope from P2.4a |
| P2.4c | F2 | Envelope → assignment watcher | **MED–HIGH** | yes | standard | `high` | Reuses; trips degraded on exhaustion |
| P2.4d | F2 | Envelope → dynamic-consumer recovery | **MED–HIGH** | yes | **strong** | `xhigh` | Prerequisite for P2.3 (F5) |
| P2.3 | F5 | Stream-gone hook + checkpoint reset | **MED** | yes | **strong** | `xhigh` | Depends on P2.4d for envelope wiring |
| P2.5 | F10-A | Truncated-`Keys()` defense + worker-set floor | **MED** | **chaos test FIRST** | **strong** | `xhigh` | Hard gate |
| **Phase 3 — DEFERRED** ||||||||
| P3.1 | F9-B | Lease-aware leader | **HIGH** | yes | tbd | tbd | Re-evaluate at re-promotion |
| P3.2 | F4 | In-process re-provision (OPTIONAL) | **HIGH** | yes | tbd | tbd | Likely dropped; F9-A subsumes election-bucket case |

13 in-scope PRs (P0–P2). 2 deferred (P3). Sequencing rationale and per-finding
detail in `00-fix-plan.md`.

## Cross-references

- Authoritative source: [`findings.md`](./findings.md)
- Consolidated review trail: [`review-trail.md`](./review-trail.md)
- IOPS justification for F9-A storage switch:
  [`docs/plans/iops-investigation/findings.md`](../iops-investigation/findings.md)
  §2 cell M1.9 (−2 % / −1 % within noise → switch is effectively free)
- Plan-shape mirror:
  [`docs/plans/worker-state-hardening/README.md`](../worker-state-hardening/README.md)
  (lazy per-PR specs)
- Partition-fencing roadmap (F10-C scope cross-reference):
  [`docs/plans/partition-fencing/README.md`](../partition-fencing/README.md)
