# 300 - Testing Guidelines

Apply these rules before adding or changing tests.

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
- **DO NOT** use `time.Sleep()` to wait for state.
- **DO** use event-driven collectors that:
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

## Concurrency Stress Tests for Monitor Goroutines
When adding a monitor goroutine on a ticker (e.g. `monitorBucketEpochs`,
`monitorAssignmentChanges`, source `reconciler`, envelope retry loops), add a
focused concurrency stress test under `test/integration/<package>/`:

- Start a small real cluster (embedded NATS, 2-3 worker managers).
- Configure the monitor at aggressive cadence (e.g. `OperationTimeout=10ms`).
- Drive concurrent KV traffic against the buckets the monitor probes for ~5s.
- Assert no race-detector triggers (`go test -race ...`).

Rationale: unit tests exercise the monitor in isolation and cannot surface races
between it and production paths that share nats.go's cached `*stream` state — a
green unit suite has passed while the live-cluster suite tripped `-race`. Use
`test/integration/manager/manager_epoch_monitor_concurrency_test.go` as the
template.

## Running Tests
Use the `Makefile` targets listed in
[500-validation-and-workflow.md](500-validation-and-workflow.md). Common gates
are `make test`, `make test-integration`, and `make test-all`.
