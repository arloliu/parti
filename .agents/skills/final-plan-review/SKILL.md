---
name: final-plan-review
description: Dispatch GitHub Copilot CLI (gpt-5.5 at xhigh effort) to perform a precision pass / pre-implementation sanity check on a plan that has already cleared multiple rounds of architectural review. Catches residual stale text, internal contradictions, ambiguous pseudocode, and numbering drift — does not redesign. Writes a structured review report under tmp/.
---

# Final Plan Review Skill

This skill automates the **last review pass before implementation starts**. The plan has already been through one or more rounds of architectural review (`plan-review`); the goal here is to catch the precision issues that would cause an implementing agent to either ship a bug or waste cycles asking clarifying questions.

This is the automated equivalent of "Prompt 2 — Precision Pass / Pre-Implementation Sanity Check" in `tmp/partition_assignment_reviewer_prompts.md`.

## When to invoke

- Architecture is settled across prior `plan-review` rounds.
- You are about to hand the plan to implementing agents.
- You want one focused sweep for stale text, contradictions, ambiguous pseudocode, and wire-format mismatches with current code.

Do **not** use this skill for:
- Redesigning the plan — that's `plan-review`.
- Reviewing code against the plan after implementation — that's `post-impl-review`.

## Arguments

Caller provides:
- **Plan path**: the authoritative spec file.
- **Optional code references**: directories or files the plan cites for "current behavior" claims; the reviewer cross-checks the plan against these.

The report is written to `tmp/<plan-stem>_precision_pass_review.md` (or `tmp/<plan-stem>_final_review.md` if a precision-pass file already exists from a prior cycle — in that case use a v2/v3 suffix).

## Invocation

```bash
copilot \
  --model gpt-5.5 \
  --effort xhigh \
  --allow-all-tools \
  --add-dir <REPO_ROOT> \
  --add-dir /tmp/claude \
  -p "$(cat /tmp/claude/<prompt-file>.md)"
```

Write the prompt to a temp file under `/tmp/claude/` (or `$TMPDIR/`) before invoking.

## Prompt template

Replace `<PLAN_PATH>`, `<STRATEGY_DOC_PATH>`, `<CODE_REFS>`, and `<REPORT_PATH>`.

> You are doing a final precision-pass review on `<PLAN_PATH>` before
> implementation begins.
>
> The architecture has been settled across multiple prior review rounds.
> Your job is **not** to redesign anything; it is to catch precision issues
> that would cause an implementing agent to either ship a bug or waste
> cycles asking clarifying questions.
>
> **Read:**
>
> 1. `<PLAN_PATH>` — the spec.
> 2. `<STRATEGY_DOC_PATH>` if provided — the phasing / dispatch doc. Useful
>    for identifying what each phase's implementer will see.
> 3. The most recent two `*_review.md` files under `tmp/` to understand
>    what's already been resolved.
> 4. Cross-reference plan claims against actual code in: <CODE_REFS>.
>
> **Look specifically for:**
>
> - **Contradictions between sections.** E.g., §X says A, §Y says not-A.
> - **Stale references** to design choices that have been superseded
>   (e.g., a paragraph still talks about inline format when the plan
>   has moved to refs-always).
> - **Wire-format / schema mismatches.** Does the plan's "current code
>   does Y" match what the source actually does today? Cite `file:line`.
> - **Ambiguous pseudocode.** Would a competent implementer reach a
>   deterministic interpretation? Or could two implementers reasonably
>   ship different behaviour?
> - **Missing edge cases at boundaries.** E.g., what if write step N
>   fails? What if the payload is empty bytes?
> - **Tests that no longer match the spec** (e.g., test name references
>   a removed primitive).
> - **Numbering drift** in step lists.
> - **Cross-references to nonexistent sections.**
>
> **Do not redo architecture.** If you find yourself proposing a
> fundamental design change, write it up as an explicit "out of scope
> for this review" note instead of folding it into findings.
>
> **Produce a review report** at `<REPORT_PATH>`.
>
> Format same as a standard plan review (Summary / Findings by severity /
> Verdict), but bias toward P1 and P2 severity. A P0 here would be a
> serious surprise; if you find one, flag it prominently in the Summary.
>
> Severity bar:
> - **P0** = correctness bug discovered late; rare but possible. Must be
>   fixed before implementation starts.
> - **P1** = precision issue that will cost the implementer time
>   (ambiguity, stale text, contradicted detail). Fix before implementation.
> - **P2** = polish (naming, documentation clarity, restructuring).
>   Nice-to-have.
>
> Cite `file:line` for every claim about source code. Paraphrasing from
> memory is not acceptable.

## After the review returns

1. Read the report.
2. Summarize verdict + counts (P0/P1/P2) to the user.
3. If P0 or P1 findings exist, propose plan edits to address them. Do not auto-apply edits to the plan without user confirmation — precision-pass findings sometimes reveal genuine architectural ambiguities the user wants to discuss.

## Loop guidance

Typically one pass of `final-plan-review` is enough. If the plan still has open P0/P1 findings after fixes, that suggests reopening `plan-review` to address architecture, not running another precision pass.

## Cost notes

Same as `plan-review`: non-trivial Copilot run. ~2–5 minutes wall time. Single-shot, not parallel.
