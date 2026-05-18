# Phase 5 — Plan Review v2 Response

Codex re-review at `tmp/sim-oracle-phase5_v2_review.md`. Verdict: REVISE.
All three P0s closed. Two P1s remain:

## P1-R1 — Outcome A cannot be verified from the planned failure report — **ACCEPT**

Right. "OwnershipUnobservedCount == 0 after the first ChaosEvent fired"
is not auditable from a single integer count.

**Resolution (plan §6, §10 updated):**

- Split the unobserved counter into
  `OwnershipUnobservedPreChaosCount` and
  `OwnershipUnobservedPostChaosCount`.
- Outcome A criterion 4 is now `OwnershipUnobservedPostChaosCount == 0`.
- `FailureReport.FirstChaosEventAt time.Time` is captured for
  post-hoc inspection.
- Implementation: coordinator gets a new method
  `MarkChaosStarted()` invoked by the chaos dispatch loop on the first
  event. Internal `chaosStarted bool` and `firstChaosAt time.Time`
  fields; classifier reads `chaosStarted` (atomic.Bool) when
  incrementing the unobserved counter, routing to the pre- or
  post-chaos bucket.

Acceptance criterion 5 updated accordingly.

## P1-R2 — Risk 5 overclaims H2 cannot land in row 4 — **ACCEPT**

Codex's scenario is sound: under H2 + stopped-worker prune timing, an
H2-induced gap can present as `currentOwners == [receivingWorker]`,
which row 4 classifies as redelivery. That's a real false-negative
class.

**Resolution (plan Risk 5 + §6 Outcome A wording updated):**

- Risk 5 rewritten with the concrete H2-induced row-4 scenario.
- Outcome A wording softened from "classifier was the root cause /
  bug fixed" to "no exclusivity-violation signal detectable by this
  classifier under this workload". An H2 fix remains a Phase 6+
  prerequisite for a stronger library-level exclusivity claim.
- Note: the CI gate-on enablement at `chaos_gate.yaml` is still
  appropriate. It catches violations, ConcurrentOwners, and
  Inconclusive — three useful classes — and the H2-induced row-4
  miss is one well-defined class of false negative documented in
  Risk 5.

## Plan changes applied

- §6 Outcome A criteria 4 → split counters.
- §6 Outcome A interpretation softened.
- Risk 5 fully rewritten with concrete row-4 scenario.
- §10 acceptance criterion 5 → split counters + FirstChaosEventAt.

The plan additions are minimal — Risk 5 explicitly documents the H2
blind spot but does not block this phase's structural improvements.
