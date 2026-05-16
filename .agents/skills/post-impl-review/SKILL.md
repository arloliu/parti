---
name: post-impl-review
description: Dispatch GitHub Copilot CLI (gpt-5.5 at xhigh effort) to perform a post-implementation review of delivered code against a plan / phase spec. Verifies the implementation faithfully realizes the spec, surfaces latent bugs, audits test coverage, and runs lint/build/test validation. Writes a versioned review report under tmp/. Designed for iterative fix-review loops until merge-clean.
---

# Post-Implementation Review Skill

This skill automates **code-against-plan review** after an implementing agent (or human) has delivered an implementation phase. The reviewer (Copilot CLI, `gpt-5.5` at `xhigh`) reads the spec, the delivered code, and project conventions, then writes a structured report with `file:line` evidence and a merge-readiness verdict.

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

The report is written to `tmp/<plan-stem>_<phase>_post_implementation_review_<vN>.md`.

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

Write the prompt to a temp file under `/tmp/claude/` (or `$TMPDIR/`) first. Use a per-invocation filename (e.g., `copilot_<phase>_review_<vN>_prompt.md`) so iterations don't clobber each other and can be inspected after the fact.

## Prompt template

Replace `<PHASE>`, `<VERSION>`, `<PLAN_PATH>`, `<SPEC_SECTIONS>`, `<IN_SCOPE_FILES>`, `<OUT_OF_SCOPE>`, `<PRIOR_REVIEW>`, `<REPORT_PATH>`, and `<VALIDATION_COMMANDS>` before sending.

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
> 4. Project conventions in `.agents/rules/` (coding style, testing,
>    documentation, lint-after-write rules).
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
> ## Run validation
>
> Run these commands in the working directory and paste the tails:
>
> <VALIDATION_COMMANDS>
>
> Typical for a Go project:
> ```
> make lint
> go test ./... -race -count=1
> go vet ./...
> ```
>
> Note any flakes or failures.
>
> ## Produce a review report
>
> Path: `<REPORT_PATH>`.
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
> (Paste tails. Note any failures or flakes.)
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

Each version's report should reference the prior version's findings explicitly. Use a versioned filename so the history is preserved on disk: `tmp/<plan-stem>_<phase>_post_implementation_review_v1.md`, `..._v2.md`, etc.

Stop the loop when the reviewer's verdict is **merge** with zero P0 and zero P1 findings (P2 polish items are not merge-blockers).

When dispatching v2+, include the prior review file in the "Read in this order" section so the reviewer audits the resolution of prior findings explicitly.

## Cost notes

Same as the other review skills: non-trivial Copilot run. Plan for ~2–5 minutes per pass. Don't dispatch v2 until fixes for v1 are actually applied — running back-to-back without changes wastes tokens.
