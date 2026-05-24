---
name: post-impl-review
description: Dispatch an external reviewer (Codex plugin preferred, GitHub Copilot CLI fallback) to perform a post-implementation review of delivered code against a plan / phase spec. Verifies the implementation faithfully realizes the spec, surfaces latent bugs, audits test coverage, and runs lint/build/test validation. Writes a versioned review report under tmp/. Designed for iterative fix-review loops until merge-clean.
---

# Post-Implementation Review Skill

This skill automates **code-against-plan review** after an implementing agent (or human) has delivered an implementation phase. The reviewer reads the spec, the delivered code, and project conventions, then writes a structured report with `file:line` evidence and a merge-readiness verdict.

The reviewer is routed through the **Codex plugin** (`/codex:rescue` → `codex:codex-rescue` subagent → Codex CLI) when available, with **GitHub Copilot CLI** (`gpt-5.5` at `xhigh`) as a fallback. See "Quick-review alternatives" under Invocation for when to use `/codex:review` or `/codex:adversarial-review` directly instead of running this skill.

## When to invoke

- An implementing agent has just delivered a phase or self-contained chunk of work and you want an independent pass.
- A prior post-impl review (v1/v2/…) flagged findings, fixes were applied, and you want to verify the fixes plus surface any new issues.
- You are about to commit / merge a phase and want a final sanity gate.

Do **not** use this skill for:
- Reviewing the plan itself — use `plan-review` or `final-plan-review`.
- Style-only or DX reviews — use `qa-review` or `go-api-review`.

## Arguments

Caller provides:
- **Phase identifier**: e.g., `phase1`, `phase2`, or `auth-rewrite`. Used in the report filename.
- **Plan / spec file**: the authoritative section the implementation realizes.
- **Round / version**: `v1`, `v2`, `v3` — increment each time fixes are applied and re-review is needed.
- **Scope** (files and packages in scope, files explicitly out of scope).
- **Pre-validation block** (strongly recommended for this repo): a structured record of the lint/build/test commands the caller already ran in the host workspace, with outcomes and tail output. The reviewer is instructed to consume this and **not** re-run the commands. See "Pre-flight: validate locally first" below.

The report is written to `tmp/<plan-stem>_<phase>_post_implementation_review_<vN>.md`.

## Pre-flight: validate locally first

Before dispatching the reviewer, run the project's standard validation suite in the host workspace and capture the result. For Parti this is the pre-PR gate:

```bash
make lint
make test            # unit suite with -race
make test-integration  # live-NATS scenarios with -race
```

Then build a **pre-validation block** the dispatch prompt embeds verbatim. Two acceptable forms — caller's choice based on output size:

- **Inline** (short / all-pass): a fenced block per command with the last ~20–40 lines of stdout/stderr.
- **By reference**: write the full captured output to `tmp/<phase>_prevalidation_<vN>.md` (or any path under `tmp/`) and have the prompt name that file plus a one-line outcome per command.

Why this matters:

1. **Re-running is slow.** The full suite is 5–10+ minutes; the reviewer pass would double it.
2. **Embedded clusters stall in sandboxes.** `make test-integration` boots an embedded NATS cluster. When the reviewer (codex / copilot CLI) executes it inside its sandbox, it has historically hung indefinitely and required manual `kill` of the dispatched agent. Never delegate that command — run it in the host workspace yourself and pass the result.
3. **The reviewer's strength is reading code, not running commands.** External-reviewer budget is best spent on spec-compliance and bug-hunting, not waiting on `go test`.

If a pre-validation command **failed**, fix it before dispatching the reviewer — the reviewer is not a fix loop. Exception: if the failure is the very thing you want the reviewer's opinion on (e.g., "this race condition reproduces under -race; here's the trace, is my proposed fix right?"), include the failing tail and an explicit question in the prompt.

## Invocation

Reviewer routing: **Codex plugin first, Copilot CLI as fallback.**

### Reviewer detection

The Codex plugin is available when this file exists:

```bash
test -f "$HOME/.claude/plugins/marketplaces/openai-codex/plugins/codex/scripts/codex-companion.mjs"
```

If present, use the Codex path. If absent (plugin uninstalled) or the codex run fails (errors, timeout, refusal), fall back to Copilot CLI.

### Primary: Codex plugin

Dispatch via the `Agent` tool using the codex rescue subagent. Pass `--wait`, `--effort xhigh`, and `--write` as routing flags in the prompt prefix. `--write` must be explicit — the rescue subagent's default is read-only for "review" tasks, but this skill needs Codex to write the versioned report file.

The codex-rescue subagent parses these as routing controls and strips them from the task text before invoking `codex-companion.mjs` (see its agent definition: "Preserve the user's task text as-is apart from stripping routing flags"). They are intent signals to the subagent, not literal CLI args appended to the prompt.

```
Agent({
  subagent_type: "codex:codex-rescue",
  description: "Post-impl review <phase> v<VERSION>",
  prompt: "--wait --effort xhigh --write\n\n<full structured prompt from template below>"
})
```

The structured prompt directs Codex to write the report to `<REPORT_PATH>`; the subagent returns Codex's stdout verbatim.

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

Write the prompt to a temp file under `/tmp/claude/` (or `$TMPDIR/`) first. Use a per-invocation filename (e.g., `copilot_<phase>_review_<vN>_prompt.md`) so iterations don't clobber each other and can be inspected after the fact. Both reviewers receive the same structured prompt below.

### Quick-review alternatives

When you do **not** need the full structured spec-compliance audit, prior-finding audit, or a versioned report on disk — just a fresh outside-model pass over the working-tree diff — use one of the codex commands directly instead of this skill:

- `/codex:review --wait` — fixed-prompt review of working-tree (or `--base <ref>` for a branch diff). Output returned verbatim; no report file written. Good for lightweight sanity passes.
- `/codex:adversarial-review --wait [focus text]` — same target selection as `/codex:review` but with adversarial framing that challenges design choices and assumptions. Accepts trailing focus text. Good for v1 rounds when you want the reviewer to question approach rather than just hunt for defects.

These are not substitutes for `post-impl-review` when the spec-vs-impl audit and versioned report are needed — that's what justifies routing through `/codex:rescue` with this skill's structured prompt.

## Prompt template

Replace `<PHASE>`, `<VERSION>`, `<PLAN_PATH>`, `<SPEC_SECTIONS>`, `<IN_SCOPE_FILES>`, `<OUT_OF_SCOPE>`, `<PRIOR_REVIEW>`, `<REPORT_PATH>`, `<PRE_VALIDATION_BLOCK>`, and `<VALIDATION_COMMANDS>` before sending.

`<PRE_VALIDATION_BLOCK>` is the caller's pre-validated lint/test output (inline tails or `tmp/` file reference, per "Pre-flight: validate locally first"). If the caller did not pre-validate, set it literally to `None — caller did not pre-validate; run the commands listed below.` and populate `<VALIDATION_COMMANDS>` with the full validation suite. When pre-validation IS provided, `<VALIDATION_COMMANDS>` should be either empty or limited to lightweight static checks the caller deliberately deferred (e.g., `go vet ./<changed-pkg>` for a specific package).

> You are doing a **v<VERSION> post-implementation review of <PHASE>** of
> the project's implementation plan. The implementation is uncommitted in
> the working tree. Your job is to verify it faithfully realizes the spec,
> surface latent bugs, audit test coverage, and run the validation commands.
>
> ## Working directory
>
> - Working directory: <REPO_ROOT>.
> - Inspect changes via `git diff --stat HEAD` and `git status`.
>
> ## Read in this order
>
> 1. <PRIOR_REVIEW> if provided — the most recent prior post-impl review
>    for this phase, so you can verify whether its findings have been
>    addressed. The prior review's `file:line` citations are your bar.
> 2. `<PLAN_PATH>` — read the spec sections this phase implements:
>    <SPEC_SECTIONS>.
> 3. The implemented code (in-scope files):
>    <IN_SCOPE_FILES>.
> 4. Project conventions in `.agents/rules/` (Go style, testing,
>    documentation, validation, and go-after-write rules).
>
> ## Out of scope
>
> <OUT_OF_SCOPE>. If you spot issues there, note briefly under
> "Interactions Outside Phase Scope" but do **not** file them as findings.
>
> ## Evaluate against this bar
>
> The delivered code must:
>
> 1. **Faithfully implement the spec.** Every behavior described in the
>    spec sections must be present with semantics matching exactly. Each
>    public API in the spec's "API surface summary" (if any) must exist
>    with the correct signature.
> 2. **Preserve invariants.** No regression on existing public API.
>    Earlier-phase contracts still hold.
> 3. **Be free of latent bugs.** Lock ordering, goroutine lifecycle,
>    atomic operations, channel send/close races, error-handling at
>    boundaries.
> 4. **Have tests that actually encode the invariants.** Each listed
>    test must exist by name and verify what it claims (not a degenerate
>    setup that passes vacuously).
> 5. **Match project conventions.** Godoc on all exports, no unjustified
>    `nolint`, test layout matches existing patterns.
>
> ## If reviewing v2+: verify prior fixes
>
> For each prior-round finding, confirm resolution with `file:line`
> evidence and judge whether the fix is correct and complete (not a
> workaround). Then look for NEW issues introduced by the fixes —
> common categories:
> - Locking changes that introduce new contention or deadlock.
> - Lifecycle changes (WaitGroup, context-cancel) that introduce new
>   leak or ordering hazards.
> - Test seams that leak into the production API surface.
> - Tests that became degenerate after refactoring.
>
> ## Validation
>
> ### Pre-validated by the caller
>
> The caller has already executed the validation suite below in their host
> workspace on the same working tree you are reviewing. Treat these results
> as authoritative; **do not re-run** these commands.
>
> <PRE_VALIDATION_BLOCK>
>
> If you believe a tail is stale or does not match the working tree (e.g.,
> the caller edited files after capturing it), raise a P1 finding
> ("pre-validation tail appears stale; request a fresh run from the caller")
> instead of executing the command yourself.
>
> ### What you MAY run
>
> Static checks scoped to the in-scope files: `gofmt -l`, `grep`,
> `go vet ./<specific-pkg>`, and any commands explicitly listed in
> `<VALIDATION_COMMANDS>` below (which the caller deferred to you).
>
> <VALIDATION_COMMANDS>
>
> ### What you MUST NOT run
>
> - Any command already executed in the pre-validation block above —
>   re-running wastes 5–10+ minutes of wall time the caller has already paid.
> - `make test-integration` (or any equivalent target that boots an embedded
>   NATS cluster / spawns network services). It has historically hung
>   indefinitely in sandboxed reviewer environments and required the caller
>   to manually kill the dispatched agent. Never start network services or
>   long-running clusters, even if you suspect the pre-validation result is
>   incomplete — surface the gap as a finding instead.
> - Any unbounded `go test ./...` across the whole module — scope to the
>   smallest package set if you genuinely need an extra static check.
>
> Note any flakes or failures from the pre-validation block in the report.
>
> ## Produce a review report
>
> Path: `<REPORT_PATH>`.
>
> Do not modify source files, tests, or any tracked file in the repository.
> `<REPORT_PATH>` is your only deliverable. Running the validation commands
> above is expected — the build/test toolchain managing its own caches under
> `~/.cache/go-build`, `vendor/`, etc. does not count as a modification.
>
> Format:
>
> ```markdown
> # <Phase> Post-Implementation Review (v<VERSION>)
>
> ## Summary
> (3-5 sentences: did the implementation deliver the phase to spec?
>  Where are the sharp edges? Ready to merge yes/no.)
>
> ## Spec Compliance
> (per-section table: spec section -> compliant / minor deviation / missing,
>  with file:line evidence)
>
> ## <If v2+> Prior Finding Resolution Audit
> (table: prior finding -> resolved / partially resolved / regressed,
>  with file:line evidence)
>
> ## Findings
>
> ### P0 — <title>
> (failure case; file:line evidence; recommended fix; suggested test)
>
> ### P1 — <title>
> ...
>
> ### P2 — <title>
> ...
>
> (If none, write "None.")
>
> ## Test Coverage Audit
> (For each test in scope: present-and-meaningful / present-but-degenerate /
>  missing. Cite file:line for each test body.)
>
> ## Interactions Outside Phase Scope
> (Things downstream phases will need to know.)
>
> ## Lint / Build / Test Status
> (Reference the pre-validated tails — do not re-paste them in full. Note any
>  failures or flakes you spotted. If you ran any additional scoped static
>  checks, paste those tails here.)
>
> ## Verdict
> (merge / fix-then-merge / re-do — and the specific gates if fix-then-merge)
> ```
>
> Severity bar:
> - **P0** = correctness bug, spec violation, or missing safety property.
>   Must be fixed before merge.
> - **P1** = serious gap (missing test, unclear Godoc, locking smell,
>   plausible-but-unproven correctness). Fix before merge.
> - **P2** = polish (naming, comment clarity, redundant code).
>   Nice-to-have.
>
> Be code-grounded: cite `file:line` for every claim. The spec is
> authoritative — when in doubt, the plan wins, not the delivered code.
> Be terse — bias toward verdict clarity.
>
> **Important:** if there are no P0 or P1 findings, explicitly recommend
> **merge** in the verdict.

## After the review returns

1. Read the report.
2. Surface a short summary to the user: verdict, P0/P1/P2 counts, top 1-2 findings.
3. Do **not** auto-apply fixes. Hand the report to a fix agent or to the user for review.

## Iterative loop (the common case)

The post-implementation review is designed to run multiple times within one phase:

```
implement → post-impl-review v1 → fix findings → post-impl-review v2 → … → merge
```

**Step 0 before dispatching v2+:** confirm the working tree actually changed since the prior review. Spot-check with `git diff --stat` and verify the diff touches the files cited in the prior round's P0/P1 findings. If the diff is empty or doesn't address the prior findings, do not dispatch — re-reviewing unchanged code reproduces the previous verdict and wastes the budget called out in "Cost notes". Surface the gap to the user instead.

Each version's report should reference the prior version's findings explicitly. Use a versioned filename so the history is preserved on disk: `tmp/<plan-stem>_<phase>_post_implementation_review_v1.md`, `..._v2.md`, etc.

Stop the loop when the reviewer's verdict is **merge** with zero P0 and zero P1 findings (P2 polish items are not merge-blockers).

When dispatching v2+, include the prior review file in the "Read in this order" section so the reviewer audits the resolution of prior findings explicitly.

**Effort step-down for v3+ rounds.** v1 and v2 default to `xhigh` — first-pass bug hunting and fix verification are where deep reasoning earns its cost. By v3+, the prior findings were narrow scope, the search surface has shrunk, and the audit is mostly "did these specific fixes land cleanly." Step down to `--effort high` for v3+ rounds; stay at `xhigh` only if a v2+ round introduced material new code (e.g., a refactor in response to a prior P0) that hasn't been reviewed at depth yet.

## Cost notes

Non-trivial external reviewer run. v1/v2 dispatch at Codex effort `xhigh` (or Copilot `gpt-5.5 xhigh` on fallback); v3+ rounds drop to `high` per the step-down note above. Plan for ~2–5 minutes per pass at `xhigh`, ~2–4 minutes at `high`. Don't dispatch v2 until fixes for v1 are actually applied — running back-to-back without changes wastes tokens.
