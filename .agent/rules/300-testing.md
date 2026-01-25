# 300 - Testing Guidelines

## Organization
- **Unit:** Co-located in `*_test.go`. Package `foo` or `foo_test`.
- **Integration:** `tests/` directory (root level). Use `testing.Short()` guard.

## Rules
- **No Emojis:** Do not use emojis in test logs.
- **Context:** Use `t.Context()`.
- **Env:** Use `t.Setenv()` (not `os.Setenv`).
- **Benchmarks:** Use `for b.Loop()` (Go 1.24+).
- **Assertions:** Use `testify` (`require`, `assert`).

## Async Testing (CRITICAL)
- ❌ **NEVER** use `time.Sleep()` to wait for state.
- ✅ **ALWAYS** use event-driven collectors that:
    1. Subscribe BEFORE triggering action.
    2. Collect all state transitions.
    3. Assert on complete history.

## Test Patterns
**Table-Driven** — Use ONLY for multiple cases:
```go
tests := []struct { name string; input X; want Y }{ ... }
for _, tt := range tests { t.Run(tt.name, func(t *testing.T) { ... }) }
```

**Simple** — For single cases:
```go
func TestOneThing(t *testing.T) {
    got := Do()
    require.Equal(t, want, got)
}
```
