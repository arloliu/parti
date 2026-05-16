# Parti — Claude Code Configuration

## What This Project Is

**Parti** (`github.com/arloliu/parti/v2`) is a Go library for dynamically partitioning work across multiple worker instances using NATS JetStream. It provides stable worker IDs, leader-based partition assignment, and cache-affinity-aware rebalancing.

Key public packages:
- **Root (`parti`)** — `Manager`, `Config`, `Hooks`, `Options`; entry point for all users
- **`consumer/`** — Unified JetStream consumer API: `Queue`, `Static`, `Dynamic`, `Broadcast`
- **`partition/`** — Static partition routing (publish + core-NATS subscribe)
- **`strategy/`** — Assignment strategies: `ConsistentHash`, `WeightedConsistentHash`, `RoundRobin`
- **`source/`** — Partition sources: `Static`, `NatsKV`
- **`types/`** — Leaf package: shared interfaces, sentinel errors, state constants

Internal packages under `internal/` are private implementation details — do not reference them in public API or docs.

## Working Principles

- **Surface uncertainty before coding.** If multiple interpretations exist, present them; if unclear, ask.
- **Minimum change that solves the problem.** No speculative features or unasked-for flexibility.
- **Don't guess — verify.** Write a small test or benchmark; don't refactor on intuition.
- **Define verifiable success criteria.** Transform vague tasks into concrete checks.

## Git Conventions

**Never add `Co-Authored-By` or any other attribution trailers to git commit messages.**

## Quick Commands

- `make test` — unit tests (race + CGO disabled)
- `make lint` — golangci-lint (pinned 2.11.4)
- `make clean-linter-cache` — clear golangci-lint cache when stale results produce false `nolintlint` reports
- `make test-integration` / `test-all` / `test-stress` — broader test scopes
- `make ci` — full CI gate (lint + test + coverage)

## How to Work in This Codebase

All coding rules, testing conventions, documentation standards, workflow steps, and performance/security guidelines are in numbered rule files. **Read them before making changes.**

All agent skills are invocable capabilities for structured reviews — use them when asked.

@AGENTS.md

## Invoking Skills

To run a skill, ask Claude to use it by name:

- `/go-api-review [package]` — Review exported API and README for DX, discoverability, and clarity. Does not read internal source.
- `/qa-review [package]` — Review for correctness, fault tolerance, error propagation, and concurrency safety from a user perspective.
- `/doc-sync [scope]` — Audit and fix `docs/` files and Godoc to match the current API: corrects stale signatures, removes phantom symbols, adds missing entries.
- `/plan-review <plan-path> <short-name>` — Dispatches Copilot CLI (gpt-5.5 xhigh) for full architectural review of a design plan. Writes a versioned report under `tmp/`. Use after material plan rewrites.
- `/final-plan-review <plan-path>` — Dispatches Copilot CLI for a precision pass / pre-implementation sanity check on an architecturally-settled plan. Catches stale text, ambiguous pseudocode, numbering drift — does not redesign.
- `/post-impl-review <phase> <plan-path> <vN>` — Dispatches Copilot CLI to review delivered code against a spec; runs lint/build/test validation. Designed for iterative fix-review loops until merge-clean.

All skills scope to Parti's public packages by default; you can specify a subset (e.g., `consumer/`, `docs/CONSUMERS.md`).

The three Copilot-dispatching skills (`plan-review`, `final-plan-review`, `post-impl-review`) run an external `gpt-5.5 xhigh` pass that costs real tokens and ~2–8 min wall time per invocation. Don't dispatch speculatively.
