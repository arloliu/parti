# Test Directory

This directory contains all tests for the `parti` library, organized by test type and execution characteristics.

## Structure

```
test/
├── integration/                        # Integration tests (functional correctness)
│   ├── assignment_correctness_test.go # All partitions assigned, no duplicates
│   ├── claimer_context_test.go        # Context lifecycle for stable IDs
│   ├── emergency_hysteresis_test.go   # Emergency detection with grace period
│   ├── emergency_scenarios_test.go    # Worker crash, cascading failures, K8s updates
│   ├── error_handling_test.go         # Concurrent start/stop, error conditions
│   ├── graceful_shutdown_test.go      # Shutdown behavior and cleanup
│   ├── leader_election_test.go        # Election, failover, assignment preservation
│   ├── manager_lifecycle_test.go      # Basic start/stop, multiple workers
│   ├── nats_failure_test.go          # NATS disconnection and reconnection
│   ├── partition_source_test.go       # Partition source implementations
│   ├── refresh_partitions_test.go     # Dynamic partition changes
│   ├── state_machine_test.go          # State transitions (cold start, scaling, emergency)
│   ├── strategy_test.go               # Assignment strategy verification
│   ├── subscription_helper_test.go    # Subscription helpers
│   ├── timing_scenarios_test.go       # Timing and coordination scenarios
│   └── watcher_test.go                # KV watcher behavior
├── stress/                             # Stress & performance tests
│   ├── memory_benchmark_test.go       # Isolated memory consumption measurements
│   ├── scale_workers_test.go          # Worker scaling (1-50+ workers)
│   └── README.md                      # Stress test documentation
├── testutil/                           # Shared test utilities
│   ├── external_nats.go               # External NATS server (process isolation)
│   ├── external_nats_test.go          # External NATS infrastructure tests
│   └── nats.go                        # Embedded NATS server utilities
└── cmd/
    └── nats-server/                    # Standalone NATS server binary
        └── main.go                     # For memory isolation benchmarks
```

## Running Tests

### Quick Reference

```bash
# Fast unit tests (~30s)
make test-unit

# Integration tests (~1m 45s, functional correctness)
make test-integration

# Stress tests (~15-20 minutes, performance/memory benchmarks)
make test-stress

# All tests (unit + integration + stress)
make test-all

# Specific integration test
go test -tags=integration ./test/integration -run TestLeaderElection

# Specific stress test
go test -tags=integration ./test/stress -run TestMemoryBenchmark_IsolatedParti/1w-100p
```

### Run Unit Tests (fast, <30s)
```bash
make test-unit
# OR
go test ./... -short
```

### Run Integration Tests (~1m 45s)
```bash
make test-integration
# OR
go test -tags=integration ./test/integration
```

### Run Stress Tests (~15-20 minutes)
```bash
make test-stress
# OR
go test -tags=integration -timeout=20m ./test/stress
```

### Run All Tests (complete suite)
```bash
make test-all
# OR
go test -tags=integration ./...
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
- **Location:** Alongside implementation files (`*_test.go`)
- **Execution:** Fast (<30 seconds total)
- **Purpose:** Test individual functions and components in isolation
- **Run:** `go test ./...` (no build tags required)
- **Coverage:** All public functions and edge cases

### Integration Tests (`test/integration/`)
- **Location:** `test/integration/` directory
- **Build Tag:** Requires `-tags=integration`
- **Duration:** ~1m 45s with parallel execution
- **Purpose:** Validate distributed system behavior and cross-component interactions
- **Infrastructure:** Uses embedded NATS server (no external dependencies)
- **Key Features:**
  - Real distributed coordination (leader election, rebalancing)
  - TTL-based stable ID claiming
  - State machine transitions (cold start, scaling, emergency)
  - NATS failure scenarios (disconnection/reconnection)
  - Partition refresh and assignment strategies
- **Run:** `make test-integration` or `go test -tags=integration ./test/integration`

### Stress Tests (`test/stress/`)
- **Location:** `test/stress/` directory
- **Build Tag:** Requires `-tags=integration`
- **Duration:** ~15-20 minutes (long-running performance tests)
- **Purpose:** Performance benchmarking, memory profiling, scalability validation
- **Infrastructure:** Uses external NATS server in separate process for memory isolation
- **Key Features:**
  - **Memory benchmarks:** Accurate measurements without NATS overhead
  - **Scalability tests:** 1-50+ workers with various partition counts
  - **Performance baselines:** Reference metrics for regression detection
  - **Binary caching:** 250,000x speedup (first compile: 628ms, cached: 2.51µs)
- **Key Results:**
  - Memory plateaus at ~5.5 MB for 10+ workers (excellent horizontal scaling)
  - Embedded NATS adds ~2.4 MB overhead (~38% contamination)
  - Startup time: ~4s per worker (linear scaling)
- **Run:** `make test-stress` or `go test -tags=integration -timeout=20m ./test/stress`
- **Documentation:** See `test/stress/README.md` for complete details

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
- Run with: `go test ./test/integration/...`

## Writing Integration Tests

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
js, _ := jetstream.New(conn)
mgr, err := parti.NewManager(&cfg, js, src, strategy)
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

### Flake Detector (Non-Blocking)

We provide a long-run harness to detect flaky tests. It’s wired as an optional, non-blocking CI job that you can trigger manually or on schedule.

Run locally (unit-only, race, quick sample):

```bash
MAX_RUNS=10 TEST_SET=unit RACE=1 bash scripts/flake_detector.sh
```

CI workflow: `.github/workflows/flake-detector.yml` runs daily with `MAX_RUNS=10`, `TEST_SET=unit`, `-race`, and `-failfast`.

Baseline (Nov 2025):

- Unit (race): 0 flakes over 2 local iterations (quick sample)
- Integration (race): data race in upstream nats-server during `TestWorkerConsumerConcurrentUpdatesConverges` when run via harness; integration runs are excluded from the CI flake job. Track upstream and revisit enabling.

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
