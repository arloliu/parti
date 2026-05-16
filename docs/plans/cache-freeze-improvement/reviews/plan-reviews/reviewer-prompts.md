# Reviewer Prompts — Partition Assignment Robustness Plan

> **Archival note.** This is the working prompt sheet used during the
> cache-freeze improvement; it is preserved verbatim as a process record.
> References below to `tmp/partition_assignment_<short-name>_review.md` and
> similar globs describe the scratch area used at the time. The archived
> finals of those reviews now live in this directory
> (`docs/plans/cache-freeze-improvement/reviews/plan-reviews/`) and in
> `../post-impl-reviews/`. Template placeholders (`<short-name>`, `<N>`) and
> `*` globs are left as written.

This file contains ready-to-send prompts for asking a reviewer
(senior architect / staff engineer / external auditor) to evaluate
the plan and produce a structured review report under `tmp/`.

Each prompt is self-contained: it tells the reviewer what to
read, what to evaluate, what bar to apply, and where to write
the output. Pick the one matching the round of review you want.

---

## Prompt 1 — Initial / Recurring Architectural Review

Use this for a full review pass when the plan has changed
substantially since the last review (new pillar, new design
direction, new scope).

> You are reviewing a P0 robustness plan for parti
> (`github.com/arloliu/parti/v2`), a Go library for dynamically
> partitioning work across worker instances using NATS JetStream.
> The plan addresses an end-to-end "no orphan partitions"
> invariant covering the partition source, leader assignment
> publishing, worker assignment watching, and consumer/handoff
> application.
>
> **Read in order:**
>
> 1. `docs/plans/cache-freeze-improvement/00-original-plan.md` — the
>    authoritative spec. Pillars 1-4 plus migration section.
> 2. `docs/plans/cache-freeze-improvement/02-implementation-strategy.md` —
>    phasing and model/effort guidance (mostly operational, but
>    useful for understanding scope boundaries).
> 3. The most recent prior review under `tmp/` if any (look for
>    `*_review.md` and `*_review_response.md` files in date order)
>    so you know what the prior round flagged and how the plan
>    has evolved.
> 4. For ground-truth code references the plan cites, consult the
>    actual source files (e.g., `internal/assignment/assignment_publisher.go`,
>    `internal/heartbeat/publisher.go`, `manager_assignment.go`,
>    `source/nats_kv.go`, `types/partition.go`,
>    `internal/assignment/handoff/twophase.go`,
>    `internal/assignment/handoff/claims.go`).
>
> **Evaluate against this bar:**
>
> The plan must be implementation-ready for a P0 invariant. That
> means the end-to-end coverage property — "for every source
> revision R and every commit batch V, the union of *applied*
> assignments across capable workers equals the source partition
> ID set at R exactly once" — must hold in:
>
> - Steady state (all workers capable).
> - Mixed-version rolling upgrade (old leader + new workers, new
>   leader + old workers, leader-role bouncing between
>   versions).
> - Split-brain (two leaders writing concurrently; CAS chains
>   intact).
> - Failure modes: payload write race, commit CAS loss, watcher
>   close, source delete/purge, handoff apply failure, partial
>   batch crash, processing-gate disabled.
>
> **Look specifically for:**
>
> - Failure scenarios the plan does not address.
> - Contradictions between sections (e.g., one section says new
>   workers ignore X while another assumes they read it).
> - Reliance on optional/off-by-default machinery that isn't
>   explicitly required by the safety story.
> - Wire-format mismatches between current code and proposed
>   schema.
> - Race windows the design assumes are closed by CAS or fencing
>   but actually aren't.
> - Audit/escalation logic that could create new failure modes
>   (e.g., reassigning a heartbeating-but-stuck worker without a
>   processing gate).
> - Coverage proofs that rely on operations that are not safe
>   (e.g., unioning non-homomorphic digests).
> - Test plan completeness — does each invariant have an
>   encoding in tests?
>
> **Produce a review report** at
> `tmp/partition_assignment_<short-name>_review.md`. Replace
> `<short-name>` with a brief identifier of the round (e.g.,
> `refactored_plan`, `followup_plan`, `pre_implementation`).
>
> **Report format:**
>
> ```markdown
> # <Title> Review
>
> ## Summary
> (3-5 sentences: what's strong, what's the remaining sharp edge,
>  ready-to-implement yes/no)
>
> ## Findings
>
> ### P0 — <short title>
> (full description; failure case; recommended fix; suggested
>  tests if applicable)
>
> ### P0 — <short title>
> ...
>
> ### P1 — <short title>
> ...
>
> ### P2 — <short title>
> ...
>
> ## Additional Tests To Add
> (optional, if not folded into individual findings)
>
> ## Verdict
> (one paragraph: what's required before implementation can start)
> ```
>
> Severity bar:
> - **P0** = correctness bug or invariant violation in the plan
>   as written. Must be fixed before implementation starts.
> - **P1** = serious gap (ambiguity, stale text, contradicted
>   detail, missing edge case). Fix before implementation.
> - **P2** = polish (unnecessary restriction, suboptimal naming,
>   documentation clarity). Nice-to-have.
>
> Be direct. If the plan is ready to implement, say so. If it
> isn't, name the specific gates that need closing.

---

## Prompt 2 — Precision Pass / Pre-Implementation Sanity Check

Use this when the plan has gone through 1+ rounds of architectural
review and the goal is to catch *residual* issues — stale text,
spec ambiguities, internal contradictions — before handing it to
implementing agents.

> You are doing a final precision-pass review on
> `docs/plans/cache-freeze-improvement/00-original-plan.md` before
> implementation begins.
>
> The architecture has been settled across multiple prior review
> rounds. Your job is **not** to redesign anything; it is to
> catch precision issues that would cause an implementing agent
> to either ship a bug or waste cycles asking clarifying
> questions.
>
> **Read:**
>
> 1. `docs/plans/cache-freeze-improvement/00-original-plan.md` (the spec).
> 2. `docs/plans/cache-freeze-improvement/02-implementation-strategy.md` (the
>    phasing — useful for identifying what each phase's
>    implementer will see).
> 3. The most recent two `*_review.md` files under `tmp/` to
>    understand what's already been resolved.
> 4. Cross-reference plan claims against actual code in:
>    - `internal/heartbeat/publisher.go` (current heartbeat
>      writer behavior)
>    - `internal/assignment/handoff/` (two-phase machinery the
>      plan depends on)
>    - `manager_assignment.go` (current assignment watcher
>      behavior)
>    - `source/nats_kv.go` (current source behavior)
>    - `types/partition.go` (current Partition methods)
>
> **Look specifically for:**
>
> - **Contradictions between sections.** E.g., §3.6 says X, but
>   §3.7 or the KV schema table says not-X.
> - **Stale references** to design choices that have been
>   superseded (e.g., a paragraph still talks about inline
>   commit when the plan has moved to refs-always).
> - **Wire-format mismatches.** Does the plan's "old code does
>   Y" match what the actual code does?
> - **Ambiguous pseudocode.** Would a competent implementer
>   reach a deterministic interpretation? Or could two
>   implementers reasonably ship different behaviour?
> - **Missing edge cases at boundaries.** E.g., what if the
>   commit log write at step N fails? What if the heartbeat
>   payload is empty bytes?
> - **Tests that no longer match the spec** (e.g., test name
>   references a removed primitive).
> - **Numbering drift** in step lists.
> - **Cross-references to nonexistent sections.**
>
> **Do not redo architecture.** If you find yourself proposing a
> fundamental design change, write it up as an explicit "out of
> scope for this review" note instead of folding it into
> findings.
>
> **Produce a review report** at
> `docs/plans/cache-freeze-improvement/reviews/plan-reviews/precision-pass-review.md`.
>
> Format same as Prompt 1, but bias toward P1 and P2 severity.
> A P0 here would be a serious surprise; if you find one, flag
> prominently in the summary.

---

## Prompt 3 — Implementation-Phase Plan Review

Use this when starting a specific implementation phase from
`docs/plans/cache-freeze-improvement/02-implementation-strategy.md` and you want
the reviewer to do a focused sanity check on just that phase's
spec section before the implementing agent starts.

> You are reviewing **Phase N** of the partition assignment
> robustness implementation, where N is one of {1: Source-layer,
> 2: Types+heartbeat, 3: Publisher rewrite, 4: Calculator+SM, 5:
> Manager wiring, 6: Tests, 7: Docs}.
>
> **Read:**
>
> 1. `docs/plans/cache-freeze-improvement/02-implementation-strategy.md` —
>    specifically the row for Phase N. Note the scope, files,
>    model/effort, and review gates.
> 2. `docs/plans/cache-freeze-improvement/00-original-plan.md` — the
>    sections this phase implements. The strategy doc lists them.
> 3. The current state of the files this phase will modify.
>
> **Evaluate:**
>
> - Is the spec section detailed enough for a competent
>   implementer to ship without asking clarifying questions?
> - Are there interactions with code outside this phase's
>   scope that the implementer will need to know about?
> - Are the phase's tests well-specified? Each test should have
>   a clear setup, action, and assertion.
> - Are the metrics this phase introduces fully named and
>   documented?
> - Are public-API additions correctly scoped (additive only,
>   no breaking changes unless explicitly called out)?
>
> **Produce a phase-specific report** at
> `tmp/partition_assignment_phase<N>_pre_implementation_review.md`.
>
> Format:
>
> ```markdown
> # Phase <N> Pre-Implementation Review
>
> ## Spec Readiness
> (yes/no, with reasoning)
>
> ## Gaps Needing Clarification
> (each gap: location in plan, the ambiguity, suggested resolution)
>
> ## Interactions Outside Phase Scope
> (things the implementer needs to know about other packages)
>
> ## Test Plan Adequacy
> (per-test review: clear or unclear; missing tests if any)
>
> ## Recommendation
> (proceed / fix-gaps-first / re-scope / defer)
> ```
>
> Be terse. The bar is whether implementation can start, not
> whether the design is perfect.

---

## Operational notes

- **One reviewer per round.** Don't dispatch Prompts 1 and 2 in
  parallel; they overlap and you'll get contradictory output.
  Use Prompt 1 for architectural review, then Prompt 2 once for
  the precision pass before implementation.
- **Severity discipline.** Reviewers tend to grade-inflate.
  Re-check P0 findings against the actual invariant — if the
  failure case requires a chain of "and then also" steps, it
  may really be P1.
- **Code-grounded only.** Reviewers should cite actual file:line
  references when claiming "current code does X", not paraphrase
  from memory. The plan itself does this; the review should
  match the bar.
- **Companion docs are not the spec.** The strategy doc is
  operational; the architect feedback files are history. Only
  the robustness plan is authoritative. Reviewers should call
  out plan-vs-companion-doc contradictions when they spot them
  (the companion docs should be updated, not the plan).

---

## Where each artifact lives

| Artifact | Path |
|---|---|
| Authoritative plan | `docs/plans/cache-freeze-improvement/00-original-plan.md` |
| Implementation strategy | `docs/plans/cache-freeze-improvement/02-implementation-strategy.md` |
| Architect feedback files | `tmp/partition_assignment_*_feedback.md`, `tmp/partition_assignment_*_review.md` |
| My responses to reviews | `tmp/partition_assignment_*_review_response.md` |
| Counter-proposals (history) | `tmp/partition_assignment_counter_proposal_*.md` |
| Reviewer prompts (this file) | `docs/plans/cache-freeze-improvement/reviews/plan-reviews/reviewer-prompts.md` |
