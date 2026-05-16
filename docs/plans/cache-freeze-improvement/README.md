# Cache-Freeze Improvement — Archived Plan & Review Records

This directory archives the design, planning, and review record for the
partition-assignment robustness work, the user-visible name of which is the
**cache-freeze improvement** (the production failure mode the work closes is
a silent cache stall in `internal/durable/claim_resolver.go` that left workers
stuck owning partitions they could not actually process).

The work was delivered to `main` across 2026 Q1–Q2; see the matching
`## [Unreleased]` section in [`/CHANGELOG.md`](../../../CHANGELOG.md) and
commits in the `feat(assignment): phase N — …` and
`feat(durable):` / `fix(durable):` series.

## Arc of the work

```
00 original plan ──► 01 counter-proposal (refs-always) ──► 02 implementation strategy
                                                             │
                                                             ├─ Phase 1: source
                                                             ├─ Phase 2: heartbeat
                                                             ├─ Phase 3: publisher
                                                             ├─ Phase 4: calc + worker SM ─┐
                                                             ├─ Phase 5: manager wiring    │
                                                             ├─ Phase 6: tests             │
                                                             └─ Phase 7: docs              │
                                                                                           │
                                          Phase 4 followups (gaps surfaced in production)──┘
                                          ├─ claim-resolver watcher (the literal cache-freeze fix)
                                          ├─ gap1: ReconcileInterval exposure
                                          ├─ gap2: drift detection + active recovery
                                          ├─ gap3: preparePhase recovery of stuck-prepare claims
                                          └─ cap-processing-gate wiring (consumer → manager)
```

## Layout

| Path | Content |
|---|---|
| [`00-original-plan.md`](00-original-plan.md) | Initial design proposing the per-worker assignment-key model. Superseded by 01. |
| [`01-counter-proposal-refs-always.md`](01-counter-proposal-refs-always.md) | The chosen direction: refs-always commit with content-addressable payloads. |
| [`02-implementation-strategy.md`](02-implementation-strategy.md) | Phasing strategy (Phases 1–7), model/effort recommendations, dependency graph. |
| [`plans/`](plans/) | Per-phase and followup implementation plans. |
| [`reviews/plan-reviews/`](reviews/plan-reviews/) | Architect / Copilot review rounds against the design before implementation. |
| [`reviews/post-impl-reviews/`](reviews/post-impl-reviews/) | Post-implementation review of each delivered phase (final verdict only). |

## What is intentionally not archived

- **Intermediate review versions (`*_v1.md`, `*_v2.md`).** Only the final
  signed-off version of each multi-round review is archived. The iteration
  churn was the `/post-impl-review` loop doing its job; the verdict is what
  matters.
- **Simulation logs, repro YAMLs, chaos test scratch.** Local-only artifacts
  produced during debugging; never tracked.
- **Sibling recovery analyses** (`auto_recovery_*`, `live_bucket_loss_*`,
  `deep-review-state-recovery-*`, `qa_review_*`). These belong to the v2.2 /
  v2.3 recovery-controller work, which sets the stage for cache-freeze but is
  a distinct line of investigation. They remain in `tmp/` (local-only).

## Cross-references

- Public API surface for the work: [`/docs/API_REFERENCE.md`](../../API_REFERENCE.md)
  (Capabilities section, Heartbeat section, ResolverConfig, NatsKV Modify/
  AddPartitions/RemovePartitions, RevisionedPartitionSource, CapabilityReporter,
  `Manager.SetCapability` / `Manager.Capabilities`).
- User-facing release notes: [`/CHANGELOG.md`](../../../CHANGELOG.md)
  (`[Unreleased]` section).
- Internal protocol prose: [`/internal/assignment/doc.go`](../../../internal/assignment/doc.go).
