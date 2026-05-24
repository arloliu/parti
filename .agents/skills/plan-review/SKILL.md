---
name: plan-review
description: Dispatch an external reviewer (Codex plugin preferred, GitHub Copilot CLI fallback) to perform a full architectural / recurring review of a design or implementation plan. Use when a plan has changed substantially since the last review (new pillar, new design direction, new scope) and you want a fresh outside-model pass against the invariants and failure modes. Writes a structured review report under tmp/.
---

# Plan Review Skill

This skill automates **architectural plan review** by handing the plan to a stronger reviewer model running outside this conversation. The reviewer is routed through the **Codex plugin** (`/codex:rescue` → `codex:codex-rescue` subagent → Codex CLI) when available, with **GitHub Copilot CLI** (`gpt-5.5` at `xhigh`) as a fallback. Use it when the plan you're working with has materially changed and you need a recurring/full pass — not a precision sweep (use `final-plan-review` for that) and not a code-level audit (use `post-impl-review`).

## When to invoke

- The plan was just rewritten or had a new pillar / phase added.
- A prior review's findings have been addressed and you want a fresh independent pass.
- You are about to commit to an implementation strategy and want an architectural sanity check first.

Do **not** use this skill for:
- Final pre-implementation precision pass — use `final-plan-review`.
- Reviewing already-implemented code against a plan — use `post-impl-review`.
- One-off questions about a plan ("does pillar 3 work?") — answer directly without spawning the reviewer.

## Arguments

Caller provides:
- **Plan path**: the authoritative spec file (any markdown file with the design / implementation plan).
- **Short name**: a brief identifier for this round (e.g., `refactored_plan`, `followup_plan`, `pre_implementation`). The report is written to `tmp/<plan-stem>_<short-name>_review.md` (or `tmp/<short-name>_review.md` if the plan stem would be redundant).
- **Optional**: companion docs to read (strategy, prior reviews, code references the plan cites). Default to reading the most recent prior review(s) in the same `tmp/` neighborhood plus any strategy or feedback files the plan itself references.

If the caller did not supply the short name, derive one from the call context (e.g., "post-refactor", "v3") and tell the user what you chose.

## Invocation

Reviewer routing: **Codex plugin first, Copilot CLI as fallback.**

### Reviewer detection

The Codex plugin is available when this file exists:

```bash
test -f "$HOME/.claude/plugins/marketplaces/openai-codex/plugins/codex/scripts/codex-companion.mjs"
```

If present, use the Codex path. If absent (plugin uninstalled) or the codex run fails (errors, timeout, refusal), fall back to Copilot CLI.

### Primary: Codex plugin

Dispatch via the `Agent` tool using the codex rescue subagent. The subagent is a thin forwarder that runs the Codex CLI with your prompt; pass `--wait`, `--effort xhigh`, and `--write` as routing flags in the prompt prefix. `--write` must be explicit — the rescue subagent's default is read-only for "review" tasks, but this skill needs Codex to write the report file.

The codex-rescue subagent parses these as routing controls and strips them from the task text before invoking `codex-companion.mjs` (see its agent definition: "Preserve the user's task text as-is apart from stripping routing flags"). They are intent signals to the subagent, not literal CLI args appended to the prompt.

```
Agent({
  subagent_type: "codex:codex-rescue",
  description: "Plan review: <short-name>",
  prompt: "--wait --effort xhigh --write\n\n<full structured prompt from template below>"
})
```

The subagent returns Codex's stdout verbatim; the structured prompt instructs Codex to write the report directly to `<REPORT_PATH>`.

### Fallback: Copilot CLI

```bash
copilot \
  --model gpt-5.5 \
  --effort xhigh \
  --allow-all-tools \
  --add-dir <REPO_ROOT> \
  --add-dir /tmp/claude \
  -p "$(cat /tmp/claude/<prompt-file>.md)"
```

Write the prompt to a temp file under `/tmp/claude/` (or `$TMPDIR/`) first — this avoids shell escaping and keeps the prompt inspectable.

Both reviewers receive the same structured prompt below; only the dispatch wrapper differs.

## Prompt template

Replace `<PLAN_PATH>`, `<SHORT_NAME>`, `<COMPANION_DOCS>`, `<CODE_REFS>`, and `<REPORT_PATH>` before sending. The report path convention is `tmp/<plan-stem>_<short-name>_review.md`.

> You are reviewing a P0 design / robustness plan for a Go library. The plan
> addresses end-to-end invariants of a distributed system; correctness across
> failure modes is the bar.
>
> **Reviewer constraints:** This is a documentation / design review. Do not
> run `make`, `go test`, `go build`, lint, or any command that boots a
> network service. Verify the plan's "current code does X" claims by reading
> source files and citing `file:line`. If a claim is genuinely unverifiable
> without execution, surface it as a finding ("claim X cannot be verified
> statically; recommend caller validate empirically before implementation")
> rather than running anything. `make test-integration` in particular has
> historically hung in sandboxed reviewer environments — never run it.
>
> **Read in order:**
>
> 1. `<PLAN_PATH>` — the authoritative spec. Read every section.
> 2. <COMPANION_DOCS> — strategy / phasing / migration docs the plan references.
> 3. The most recent prior reviews under `tmp/` if any (look for `*_review.md`
>    and `*_review_response.md` files in date order). This tells you what the
>    prior round flagged and how the plan has evolved since.
> 4. For ground-truth code references the plan cites, consult the actual
>    source files: <CODE_REFS>.
>
> **Evaluate against this bar:**
>
> The plan must be implementation-ready. The end-to-end correctness property
> the plan claims must hold in:
> - Steady state.
> - Mixed-version / rolling upgrade (old + new, leader-role bouncing).
> - Split-brain (concurrent writers, CAS chains intact).
> - Failure modes: write race, CAS loss, watcher close, source delete/purge,
>   apply failure, partial batch crash, optional-safety-disabled.
>
> **Look specifically for:**
> - Failure scenarios the plan does not address.
> - Contradictions between sections (one section says X, another assumes not-X).
> - Reliance on optional/off-by-default machinery that isn't explicitly required
>   by the safety story.
> - Wire-format / schema mismatches between current code and the proposed design.
> - Race windows the design assumes are closed by CAS or fencing but aren't.
> - Audit / escalation logic that could create new failure modes (e.g.,
>   reassigning a heartbeating-but-stuck worker without a processing gate).
> - Coverage proofs that rely on operations that are not safe (e.g., unioning
>   non-homomorphic digests).
> - Test plan completeness — does each invariant have an encoding in tests?
>
> **Produce a review report** at `<REPORT_PATH>`.
>
> Write ONLY to `<REPORT_PATH>`. Do not create, modify, or delete any other
> file.
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
> - **P0** = correctness bug or invariant violation in the plan as written.
>   Must be fixed before implementation starts.
> - **P1** = serious gap (ambiguity, stale text, contradicted detail,
>   missing edge case). Fix before implementation.
> - **P2** = polish (unnecessary restriction, suboptimal naming,
>   documentation clarity). Nice-to-have.
>
> Be direct. If the plan is ready to implement, say so. If it isn't, name
> the specific gates that need closing. Cite `file:line` for every claim
> grounded in the source — paraphrasing from memory is not acceptable.

## After the review returns

1. Read the written report.
2. Surface the verdict and top findings to the user as a short summary (don't dump the whole report inline).
3. Do **not** silently apply fixes — review reports inform the next round of plan editing, not unconditional changes.

## Loop guidance

If the caller asks to "loop until clean":
- Step 0: before dispatching, confirm the plan has materially changed since the
  last `tmp/<plan-stem>_*_review.md` for this plan. If no prior review exists,
  proceed. If a prior review exists and the plan looks unchanged at a glance
  (no new sections, no rewritten pillars), surface that to the user and ask
  whether to dispatch anyway — a re-review on an unchanged plan typically
  reproduces the previous findings and wastes the budget called out in
  "Cost notes".
- Pass 1: dispatch this skill, then summarize.
- If verdict is "fix-then-merge" or equivalent and findings are addressable in the plan (not in code), edit the plan to address each finding, then dispatch again.
- Stop when the verdict is "ready" / "merge" / no P0 or P1 findings remain.
- One reviewer pass per loop iteration. Do not parallelize — overlapping reviews on the same plan produce contradictory output.

## Cost notes

This dispatches a non-trivial external reviewer run — Codex CLI (effort `xhigh`) by default, or Copilot `gpt-5.5 xhigh` on fallback. Each invocation costs real tokens and ~2–5 minutes of wall time. Don't dispatch speculatively. Confirm the plan path and short name before invoking.
