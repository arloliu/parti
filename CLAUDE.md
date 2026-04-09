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

## How to Work in This Codebase

All coding rules, testing conventions, documentation standards, workflow steps, and performance/security guidelines are in numbered rule files. **Read them before making changes.**

All agent skills are invocable capabilities for structured reviews — use them when asked.

@AGENTS.md

## Invoking Skills

To run a skill, ask Claude to use it by name:

- `/go-api-review [package]` — Review exported API and README for DX, discoverability, and clarity. Does not read internal source.
- `/qa-review [package]` — Review for correctness, fault tolerance, error propagation, and concurrency safety from a user perspective.

Both skills scope to Parti's public packages by default; you can specify a subset (e.g., `consumer/`, `types/`).
