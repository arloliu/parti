# Parti Agent Configuration

This is the authoritative entrypoint for coding agents working in this
repository. Claude Code imports this file from [`CLAUDE.md`](CLAUDE.md); other
agents should read `AGENTS.md` directly.

Parti (`github.com/arloliu/parti/v2`) is a Go library for dynamically
partitioning work across worker instances using NATS JetStream. Detailed project
structure, coding rules, testing rules, documentation standards, workflow, and
review discipline live under [`.agents/rules/`](.agents/rules/).

## Detailed Rules

Read [`.agents/rules/AGENTS.md`](.agents/rules/AGENTS.md) first. It maps task
triggers to the rule files that apply.

Always follow [`.agents/rules/000-agent-contract.md`](.agents/rules/000-agent-contract.md).
It includes the explicit rule: do not guess when source evidence, tests,
benchmarks, docs, or grep can answer.

## Skills

Skills are invocable agent capabilities in [`.agents/skills/`](.agents/skills/).

Available skills:

- `/go-api-review [package]` — Review exported API and README for DX, discoverability, and clarity. Does not read internal source.
- `/qa-review [package]` — Review for correctness, fault tolerance, error propagation, and concurrency safety from a user perspective.
- `/doc-sync [scope]` — Audit and fix `docs/` files and Godoc to match the current API: corrects stale signatures, removes phantom symbols, adds missing entries.
- `/plan-review <plan-path> <short-name>` — Full architectural review of a design plan. Writes a versioned report under `tmp/`. Use after material plan rewrites.
- `/final-plan-review <plan-path>` — Precision pass / pre-implementation sanity check on an architecturally-settled plan. Catches stale text, ambiguous pseudocode, numbering drift — does not redesign.
- `/post-impl-review <phase> <plan-path> <vN>` — Review delivered code against a spec; runs lint/build/test validation. Designed for iterative fix-review loops until merge-clean. For lightweight passes without spec-compliance audit, use `/codex:review` or `/codex:adversarial-review` directly instead.

All skills scope to Parti's public packages by default; specify a subset when
needed (for example, `consumer/` or `docs/CONSUMERS.md`).

The three external-reviewer skills (`plan-review`, `final-plan-review`,
`post-impl-review`) dispatch an outside reviewer through the local skill
workflow, with Copilot `gpt-5.5` as a fallback. Effort defaults vary by task:
`plan-review` and `post-impl-review` (v1/v2) at `xhigh`;
`final-plan-review` and `post-impl-review` v3+ at `high`. Each invocation costs
real tokens and about 2–8 minutes wall time. Do not dispatch speculatively.
