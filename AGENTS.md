# Parti Agent Configuration

All agent rules and skills live in the [`.agents/`](.agents/) directory.

## Rules

Rules are loaded in numeric order before any work begins. See [`.agents/rules/AGENTS.md`](.agents/rules/AGENTS.md) for the full index.

| File | Topic |
|------|-------|
| [`100-overview.md`](.agents/rules/100-overview.md) | Project identity, structure, architecture, dependencies, prime directives |
| [`200-coding-style.md`](.agents/rules/200-coding-style.md) | Go idioms, error handling, file layout, naming, loop patterns |
| [`300-testing.md`](.agents/rules/300-testing.md) | Unit/integration/stress organization, async testing rules, make targets |
| [`400-documentation.md`](.agents/rules/400-documentation.md) | Mandatory Godoc format with Parti-specific examples |
| [`500-workflow.md`](.agents/rules/500-workflow.md) | Git conventions, pre-commit checks, make targets reference |
| [`600-perf-sec.md`](.agents/rules/600-perf-sec.md) | Performance optimizations (xxh3, allocations) and security boundaries |
| [`700-lint-after-write.md`](.agents/rules/700-lint-after-write.md) | Automated linting workflow and common fixes |

## Skills

Skills are invocable agent capabilities in [`.agents/skills/`](.agents/skills/).

| Skill | Description |
|-------|-------------|
| [`go-api-review`](.agents/skills/go-api-review/SKILL.md) | Reviews a Go package's exported API (Godoc) and README for discoverability, clarity, and developer experience — without reading internal source code |
| [`qa-review`](.agents/skills/qa-review/SKILL.md) | QA-focused review for correctness, fault tolerance, and performance from the perspective of external users |
| [`doc-sync`](.agents/skills/doc-sync/SKILL.md) | Audits and updates `docs/` files and public-package Godoc to match the current API — fixes stale signatures, phantom symbols, and missing entries |
