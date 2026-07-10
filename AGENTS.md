# Parti Agent Configuration

Authoritative entrypoint for coding agents in this repository. Claude Code imports
this file from [`CLAUDE.md`](CLAUDE.md); other agents read `AGENTS.md` directly.

Parti (`github.com/arloliu/parti/v2`) is a Go library for dynamically partitioning
work across worker instances using NATS JetStream.

## Rules

Read [`.agents/rules/AGENTS.md`](.agents/rules/AGENTS.md) first — it maps task
triggers to the rule files that apply. Detailed project structure, Go style,
testing, docs, validation, git conventions, review loops, and cross-feature
contracts all live under [`.agents/rules/`](.agents/rules/).

[`000-agent-contract.md`](.agents/rules/000-agent-contract.md) is always in force,
including its core rule: **do not guess when source, tests, benchmarks, docs, or
grep can answer.**

Two gates to know before you open a PR — both detailed in the rules:
- **Pre-PR gate** ([`500`](.agents/rules/500-validation-and-workflow.md)): PRs
  touching `manager/`, `source/`, `stableid/`, `recovery/`, `internal/assignment/`,
  or `internal/durable/` must pass `make pre-pr`.
- **Cross-feature contracts** ([`900`](.agents/rules/900-cross-feature-contracts.md)):
  any error-classification, error-routing, or `Manager.Start` change must preserve
  the pinned contracts.

## Review Skills

Invocable review capabilities in [`.agents/skills/`](.agents/skills/), by slash
name. They scope to Parti's public packages by default; pass a subset (e.g.
`consumer/`, `docs/CONSUMERS.md`) to narrow.

- `/go-api-review [package]` — Exported API + README for DX, discoverability, clarity. Does not read internal source.
- `/qa-review [package]` — Correctness, fault tolerance, error propagation, concurrency safety from a user's perspective.
- `/doc-sync [scope]` — Sync `docs/` and Godoc to the current API: fix stale signatures, drop phantom symbols, add missing entries.
- `/plan-review <plan-path> <short-name>` — Full architectural review of a design plan. Writes a versioned report under `tmp/`.
- `/final-plan-review <plan-path>` — Precision / pre-implementation sanity pass on a settled plan. Does not redesign.
- `/post-impl-review <phase> <plan-path> <vN>` — Review delivered code against a spec; runs lint/build/test. For lightweight passes use `/codex:review` directly.

The three external-reviewer skills (`plan-review`, `final-plan-review`,
`post-impl-review`) dispatch an outside model and cost real tokens (~2-8 min each) —
do not dispatch speculatively. Reviewer sequence and effort defaults live in
[`850-review-loop-workflow.md`](.agents/rules/850-review-loop-workflow.md).

## Agent Skills

Configuration for the engineering skills (triage, spec, domain modeling, etc.):

- **Issue tracker** — GitHub Issues via `gh`; external PRs are not a triage surface. See [`docs/agents/issue-tracker.md`](docs/agents/issue-tracker.md).
- **Triage labels** — Five canonical roles map 1:1 to their default label strings. See [`docs/agents/triage-labels.md`](docs/agents/triage-labels.md).
- **Domain docs** — Single-context: `CONTEXT.md` + `docs/adr/` at the repo root. See [`docs/agents/domain.md`](docs/agents/domain.md).
