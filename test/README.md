# Test Directory

This directory contains integration and end-to-end tests for the `parti` library.

## Structure

```
test/
├── integration/                        # Integration tests (58 tests, ~1m 45s)
│   ├── assignment_correctness_test.go # All partitions assigned, no duplicates
│   ├── claimer_context_test.go        # Context lifecycle for stable IDs
│   ├── emergency_hysteresis_test.go   # Emergency detection with grace period
│   ├── emergency_scenarios_test.go    # Worker crash, cascading failures, K8s updates
│   ├── error_handling_test.go         # Concurrent start/stop, error conditions
│   ├── leader_election_test.go        # Election, failover, assignment preservation
│   ├── manager_lifecycle_test.go      # Basic start/stop, multiple workers
│   ├── nats_failure_test.go          # NATS disconnection and reconnection
│   ├── partition_source_test.go       # Partition source implementations
│   ├── refresh_partitions_test.go     # Dynamic partition changes
│   ├── state_machine_test.go          # State transitions (cold start, scaling, emergency)
│   ├── strategy_test.go               # Assignment strategy verification
│   ├── subscription_helper_test.go    # Subscription helpers
│   └── watcher_test.go                # KV watcher behavior
└── testutil/                           # Shared test utilities
    └── nats.go                         # Embedded NATS server utilities
```

## Running Tests

### Quick Reference

```bash
# Fast unit tests (~30s)
make test-unit

# Comprehensive integration tests (~1m 45s with parallelism)
make test-integration

# All tests with race detector
make test-all

# Specific test file
go test -tags=integration ./test/integration -run TestLeaderElection

# Specific test function
go test -tags=integration ./test/integration -run TestLeaderElection_BasicFailover -v
```

### Run All Tests (including integration)
```bash
go test -tags=integration ./...
```

### Run Only Unit Tests (fast, <30s)
```bash
make test-unit
# OR
go test ./... -short
```

### Run Only Integration Tests (~1m 45s)
```bash
make test-integration
# OR
go test -tags=integration ./test/integration
```

### Run With Race Detector
```bash
# Unit tests with race detection
go test -race ./...

# Integration tests with race detection (~1m 50s)
go test -race -tags=integration ./test/integration
```

## Test Categories

### Unit Tests
- Located alongside implementation files (`*_test.go`)
- Fast execution (<30 seconds total)
- Use standard Go testing patterns
- Run without build tags: `go test ./...`

### Integration Tests
- Located in `test/integration/` directory
- Require build tag: `-tags=integration`
- Use embedded NATS server (no external dependencies)
- Run in parallel with `t.Parallel()` for optimal performance
- Execution time: ~1m 45s (58 tests)
- Each test creates isolated NATS server and workers
- Test real distributed system behavior (TTL, election, rebalancing)

### Performance Optimization
- All integration tests use `t.Parallel()` for concurrent execution
- Tests run in parallel within the single package (optimal for Go)
- Each test has isolated resources (NATS server, KV buckets, workers)
- Wall clock time reduced by 61% compared to sequential execution
  - Before: ~4m 27s (sequential)
  - After: ~1m 45s (parallel)
- No external dependencies (NATS embedded in memory when needed)
- Run with: `go test -short ./...`

### Integration Tests (this directory)
- Located in `test/integration/`
- Test multiple components working together
- Use embedded NATS server
- Slower execution (1-10 seconds per test)
- Tagged with `//go:build integration`
- Run with: `go test ./test/integration/...`

## Writing Integration Tests

### Build Tags
All integration tests must include build tags at the top:

```go
//go:build integration
// +build integration

package integration_test
```

### Test Naming Convention
- `TestManager_*` - Tests for Manager lifecycle and coordination
- `TestLeader_*` - Tests for leader election and failover
- `TestScale_*` - Tests for scaling up/down
- `TestAssignment_*` - Tests for partition assignment and rebalancing
- `TestNetwork_*` - Tests for network partition scenarios

### Common Patterns

#### Setup NATS Server
```go
func TestMyScenario(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test in short mode")
    }

    // Start embedded NATS
    srv, conn := testutil.StartEmbeddedNATS(t)
    defer srv.Shutdown()
    defer conn.Close()

    // Your test logic...
}
```

#### Create Test Manager
```go
cfg := parti.Config{
    WorkerIDPrefix:     "test-worker",
    WorkerIDMin:        0,
    WorkerIDMax:        99,
    HeartbeatInterval:  1 * time.Second,
    // ... other config
}

partitions := []types.Partition{
    {Keys: []string{"partition-1"}, Weight: 100},
}
src := source.NewStatic(partitions)
strategy := strategy.NewConsistentHash()

mgr, err := parti.NewManager(&cfg, conn, src, strategy)
require.NoError(t, err)
```

## Best Practices

1. **Always use `testing.Short()` guard** - Even though tests are in integration directory
2. **Clean up resources** - Use `defer` for cleanup (server shutdown, connection close)
3. **Use realistic timeouts** - Integration tests may be slower than unit tests
4. **Test one scenario per test** - Keep tests focused and easy to debug
5. **Add context to failures** - Use `require` with descriptive messages
6. **Avoid test interdependencies** - Each test should be independent

## CI/CD Integration

### GitHub Actions Example
```yaml
# Unit tests (fast, run on every commit)
- name: Run unit tests
  run: go test -short ./... -v

# Integration tests (slower, run on PR)
- name: Run integration tests
  run: go test ./test/integration/... -v -timeout=5m
```

### Makefile Targets
```makefile
.PHONY: test test-unit test-integration

# Fast unit tests
test-unit:
	go test -short ./... -v

# Integration tests
test-integration:
	go test ./test/integration/... -v -timeout=5m

# All tests
test:
	go test ./... -v
	go test ./test/integration/... -v
```

## Current Test Coverage

### Implemented ✅
- [x] Manager start/stop lifecycle
- [x] Multiple workers coordination
- [x] Leader election on startup

### TODO 📝
- [ ] Leader failover and re-election
- [ ] Scale up (add workers dynamically)
- [ ] Scale down (remove workers gracefully)
- [ ] Rolling update simulation
- [ ] Network partition recovery
- [ ] Assignment stability verification
- [ ] Concurrent startup/shutdown stress tests
- [ ] Partition rebalancing scenarios

## Debugging Integration Tests

### Enable Debug Output
```go
import "github.com/arloliu/parti/internal/logging"

// In your test
cfg.Logger = logging.NewTest(t)
```

### Run with Verbose Output
```bash
go test ./test/integration/... -v -count=1
```

### Run Specific Test with Logging
```bash
go test ./test/integration/... -run TestManager_StartStop -v -count=1 2>&1 | tee test.log
```

## Performance Considerations

- Integration tests are expected to take 1-10 seconds each
- Use shorter timeouts than production (for faster feedback)
- Multiple concurrent tests may compete for NATS ports
- Consider using `t.Parallel()` carefully (NATS server per test)
