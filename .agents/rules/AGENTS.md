# Parti — Agent Rules Index

This is the trigger map for repository rules. Read `000-agent-contract.md` for
every task, then read the files whose triggers match the work.

## Default Load
- For most Go implementation tasks, read `000`, `100`, `200`, `500`, and `600`.
- Add `300` when adding or changing tests.
- Add `400` when editing docs, examples, README content, or exported API.
- Add `700` only for hot paths, external input, credentials, auth, or network-facing code.
- Add `800` only for non-trivial design, plan, or review-loop work.
- For tiny documentation-only edits, `000` plus the relevant docs or workflow rule is enough.

## Always
- **[000-agent-contract.md](000-agent-contract.md)** — Always-on behavior: do not guess, control scope, verify claims, test intent, match conventions, and fail loud.

## Before Code Changes
- **[100-project-map.md](100-project-map.md)** — Project identity, package map, architecture constraints, dependency policy.
- **[200-go-style.md](200-go-style.md)** — Go idioms, error handling, interface assertions, file layout, naming, loop patterns.

## Before Adding or Changing Tests
- **[300-testing.md](300-testing.md)** — Test organization, async test requirements, helper-package choice, test patterns.

## Before Documentation or Exported API Changes
- **[400-docs.md](400-docs.md)** — Exported symbol docs, README/docs sync, Godoc examples.

## Before Validation, Commit, or PR Work
- **[500-validation-and-workflow.md](500-validation-and-workflow.md)** — Git conventions, validation gates, Make targets.

## After Modifying Go Files
- **[600-go-after-write.md](600-go-after-write.md)** — `go fix` scope, lint workflow, stale linter cache handling.

## For Hot Paths, External Input, or Security-Sensitive Code
- **[700-performance-security.md](700-performance-security.md)** — Hot-path performance, allocation discipline, input validation, secrets, NATS auth.

## Before Plan, Design, or Review-Loop Work
- **[800-design-and-review-loops.md](800-design-and-review-loops.md)** — Invariants, path enumeration, atomicity, review-loop discipline.

For broad or ambiguous tasks, read all rule files before editing.
