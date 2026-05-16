---
trigger: always_on
glob: "**/*.go"
description: Run linter after modifying Go files
---

# Lint After Write

After modifying any `.go` file:

1. **Run:** `make lint`
2. **Fix:** All reported issues before committing.
3. **Re-run:** Until clean.

## Common Fixes
| Lint Error | Fix |
|------------|-----|
| `goimports` | Run `goimports -w file.go` |
| `errcheck` | Handle or explicitly ignore with `_ =` |
| `unused` | Remove dead code |
| `govet` | Fix type/format mismatches |

## Stale-cache caveat

`golangci-lint` caches results per-file. After unrelated changes you may
see `nolintlint` flag a `//nolint:<linter>` directive as "unused" when
the underlying rule actually fires on a fresh analysis. Before deleting
any suppression that looks unused:

```
make clean-linter-cache && make lint
```

If the directive is still reported unused with a cold cache, it's safe
to remove. If a new issue from the suppressed linter appears in its
place, the directive was correctly suppressing a real rule — keep it.
