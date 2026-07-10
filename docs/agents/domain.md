# Domain Docs

How the engineering skills should consume this repo's domain documentation when exploring the codebase.

This repo is **single-context**: one `CONTEXT.md` and one `docs/adr/` at the repo root.

## Before exploring, read these

- **`CONTEXT.md`** at the repo root.
- **`docs/adr/`** — read ADRs that touch the area you're about to work in.

If any of these files don't exist, **proceed silently**. Don't flag their absence; don't suggest creating them upfront. The `/domain-modeling` skill (reached via `/grill-with-docs` and `/improve-codebase-architecture`) creates them lazily when terms or decisions actually get resolved.

Note: this repo already carries hand-authored architecture docs under `docs/` (e.g. `ARCHITECTURE.md`, `LIFECYCLE.md`, `CONSUMERS.md`, `STRATEGIES.md`). Those remain the authoritative deep references; `CONTEXT.md` and `docs/adr/` are the lightweight, skill-maintained glossary + decision log layered on top.

## File structure

Single-context repo (this repo — Go packages live at the module root, not under `src/`):

```
/
├── CONTEXT.md
├── docs/
│   └── adr/
│       ├── 0001-example-decision.md
│       └── 0002-another-decision.md
├── manager/
├── source/
├── consumer/
└── ...
```

For reference, a multi-context repo (presence of `CONTEXT-MAP.md` at the root) instead points at one `CONTEXT.md` per context, with optional context-scoped `docs/adr/` directories. Parti is not laid out this way.

## Use the glossary's vocabulary

When your output names a domain concept (in an issue title, a refactor proposal, a hypothesis, a test name), use the term as defined in `CONTEXT.md`. Don't drift to synonyms the glossary explicitly avoids.

If the concept you need isn't in the glossary yet, that's a signal — either you're inventing language the project doesn't use (reconsider) or there's a real gap (note it for `/domain-modeling`).

## Flag ADR conflicts

If your output contradicts an existing ADR, surface it explicitly rather than silently overriding:

> _Contradicts ADR-0007 (some past decision) — but worth reopening because…_
