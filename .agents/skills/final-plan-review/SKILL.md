---
name: final-plan-review
description: Dispatch an external reviewer (Codex plugin preferred, GitHub Copilot CLI fallback) to perform a precision pass / pre-implementation sanity check on a plan that has already cleared multiple rounds of architectural review. Catches residual stale text, internal contradictions, ambiguous pseudocode, and numbering drift — does not redesign. Writes a structured review report under tmp/.
---

# Final Plan Review Skill

This skill automates the **last review pass before implementation starts**. The plan has already been through one or more rounds of architectural review (`plan-review`); the goal here is to catch the precision issues that would cause an implementing agent to either ship a bug or waste cycles asking clarifying questions. The reviewer is routed through the **Codex plugin** (`/codex:rescue` → `codex:codex-rescue` subagent → Codex CLI) when available, with **GitHub Copilot CLI** (`gpt-5.5` at `high`) as a fallback.

**Effort level: `high`** (not `xhigh`). The task is a precision sweep over already-settled architecture — pattern-matching for stale text, contradictions, ambiguous pseudocode, numbering drift, and wire-format mismatches against current code. These are mostly mechanical findings; the deeper chain-of-thought that `xhigh` provides isn't earned here. Bump to `xhigh` only if you suspect residual architectural ambiguity (in which case you should probably reopen `plan-review` instead).

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

Reviewer routing: **Codex plugin first, Copilot CLI as fallback.**

### Reviewer detection

The Codex plugin is available when this file exists:

```bash
test -f "$HOME/.claude/plugins/marketplaces/openai-codex/plugins/codex/scripts/codex-companion.mjs"
```

If present, use the Codex path. If absent (plugin uninstalled) or the codex run fails (errors, timeout, refusal), fall back to Copilot CLI.

### Primary: Codex plugin

Dispatch via the `Agent` tool using the codex rescue subagent. Pass `--wait`, `--effort high`, and `--write` as routing flags in the prompt prefix. `--write` must be explicit — the rescue subagent's default is read-only for "review" tasks, but this skill needs Codex to write the report file.

```
Agent({
  subagent_type: "codex:codex-rescue",
  description: "Final plan review",
  prompt: "--wait --effort high --write\n\n<full structured prompt from template below>"
})
```

### Fallback: Copilot CLI

```bash
copilot \
  --model gpt-5.5 \
  --effort high \
  --allow-all-tools \
  --add-dir <REPO_ROOT> \
  --add-dir /tmp/claude \
  -p "$(cat /tmp/claude/<prompt-file>.md)"
```

Write the prompt to a temp file under `/tmp/claude/` (or `$TMPDIR/`) before invoking. Both reviewers receive the same structured prompt below.

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

Non-trivial external reviewer run (Codex effort `high`, or Copilot `gpt-5.5 high` on fallback) — a step down from `plan-review`'s `xhigh` since this is a precision sweep, not an architectural depth pass. ~2–4 minutes wall time. Single-shot, not parallel.
