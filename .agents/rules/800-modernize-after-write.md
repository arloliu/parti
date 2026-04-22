---
trigger: always_on
glob: "**/*.go"
description: Run go fix to modernize Go code after writing
---

# Modernize After Write

After modifying any `.go` file, run `go fix` on the **affected packages only** (not `./...`) to apply modernization rewrites (`max`/`min`, `range N`, `any` alias, etc.).

## Workflow

1. **Run:** `go fix ./path/to/pkg/...` — scope to the packages you touched.
2. **Review the diff:** `git diff` — confirm it only modernizes your changes.
3. **Commit:** the modernization result together with your change.

## Scope Discipline

**Do NOT run `go fix ./...`** as part of a feature commit. It will modernize unrelated files across the repo and balloon your diff with drive-by cleanup that belongs in its own commit.

Repo-wide modernization is a separate, dedicated change:

```bash
go fix ./...
git add -A
git commit -m "chore: modernize with go fix"
```

Keep feature commits focused on the feature.

## Why

Parti targets modern Go; the modernize analyzer catches equivalents that are easier to read and have fewer failure modes. Running it per-change prevents a slow accumulation of non-idiomatic code, without inflating individual PRs.

## Lint After

`go fix` can change formatting. Always re-run `make lint` after — see [700-lint-after-write.md](700-lint-after-write.md).
