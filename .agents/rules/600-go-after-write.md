# 600 - Go After Write

After modifying any `.go` file:

1. **Run `go fix` on affected packages only:** `go fix ./path/to/pkg/...`
2. **Review the diff:** confirm `go fix` only modernized touched code.
3. **Run `make lint`:** fix all reported issues.
4. **Re-run validation:** repeat until clean.

Do not run `go fix ./...` as part of a feature commit. Repo-wide modernization
belongs in its own dedicated change.

## Common Fixes
| Lint Error | Fix |
|------------|-----|
| `goimports` | Run `goimports -w file.go` |
| `errcheck` | Handle or explicitly ignore with `_ =` |
| `unused` | Remove dead code |
| `govet` | Fix type/format mismatches |

## Stale Cache Caveat

If lint output looks stale or inconsistent after unrelated edits, clear the
cache and rerun lint:

```bash
make clean-linter-cache && make lint
```

Do not preserve a `//nolint` directive just because a cold-cache run appears to
need it. `nolintlint` is disabled in this repo because its unused-directive
check has been flaky. To verify whether a suppression is still needed, remove
the directive and rerun the targeted lint/test command. Keep the directive only
when the underlying linter still reports a real issue that must be suppressed.
