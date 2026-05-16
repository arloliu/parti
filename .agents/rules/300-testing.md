# 300 - Testing Guidelines

## Organization
- **Unit:** Co-located in `*_test.go`. Same package or `_test` suffix.
- **Integration:** `test/integration/` directory. Package `integration_test`. Use `testing.Short()` guard.
- **Simulation:** `test/simulation/` directory. Long-running simulation scenarios.
- **Stress:** `test/stress/` directory. Enabled with `PARTI_STRESS=1` env var.

## Rules
- **No Emojis:** Do not use emojis in test log messages.
- **Context:** Use `t.Context()`.
- **Env:** Use `t.Setenv()` (not `os.Setenv`).
- **Benchmarks:** Use `for b.Loop()` (Go 1.24+).
- **Assertions:** Use `testify` (`require`, `assert`).
- **Embedded NATS:** Use `partitest.StartEmbeddedNATS(t)` for integration tests.
- **Cleanup:** Always use `defer` for resource cleanup.
- **Test-helper package choice:** `partitest/` (public, leaf) for helpers that must be importable from `package parti` tests or from external `_test` packages; `internal/testutil/` (which imports parti) for anything else. Importing `internal/testutil` from a `package parti` test file causes an import cycle.

## Async Testing (CRITICAL)
- ❌ **NEVER** use `time.Sleep()` to wait for state.
- ✅ **ALWAYS** use event-driven collectors that:
    1. Subscribe BEFORE triggering action.
    2. Collect all state transitions.
    3. Assert on complete history.
- See `internal/assignment/calculator_state_test.go` for reference implementation.

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

## Running Tests
```bash
make test              # Unit tests with race detector
make test-unit         # Same as test
make test-quick        # Unit tests without race detector (fast)
make test-integration  # Integration tests with embedded NATS
make test-stress       # Stress tests (PARTI_STRESS=1)
make test-all          # Unit + integration + stress
make test-smoke        # Quick stress smoke test
make coverage          # Generate coverage report
```
