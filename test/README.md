# Test Directory

This directory contains integration and end-to-end tests for the `parti` library.

## Structure

```
test/
├── integration/          # Integration tests for multi-component scenarios
│   ├── manager_lifecycle_test.go      # Basic start/stop, single worker
│   ├── leader_failover_test.go        # Leader election and failover
│   ├── scale_up_test.go               # Adding workers dynamically
│   ├── scale_down_test.go             # Removing workers gracefully
│   ├── rolling_update_test.go         # Simulated rolling updates
│   ├── network_partition_test.go      # Network partition recovery
│   ├── assignment_stability_test.go   # Cache affinity verification
│   └── concurrent_startup_test.go     # Concurrent worker startup
└── testutil/             # Shared test utilities and fixtures
    ├── fixtures.go       # Test data and configurations
    └── helpers.go        # Common setup and teardown functions
```

## Running Tests

### Run All Tests (including integration)
```bash
go test ./... -v
```

### Run Only Unit Tests (fast)
```bash
go test -short ./...
```

### Run Only Integration Tests
```bash
go test -tags=integration ./test/integration/... -v
```

### Run Specific Integration Test
```bash
go test -tags=integration ./test/integration/... -run TestManager_StartStop -v
```

## Test Categories

### Unit Tests
- Located alongside implementation files (`*_test.go`)
- Use mocks and fakes
- Fast execution (< 1 second per test)
- No external dependencies (NATS embedded in memory when needed)
- Run with: `go test -short ./...`

### Integration Tests (this directory)
- Located in `test/integration/`
- Test multiple components working together
- Use embedded NATS server
- Slower execution (1-10 seconds per test)
- Tagged with `//go:build integration`
- Run with: `go test -tags=integration ./test/integration/...`

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
  run: go test -tags=integration ./test/integration/... -v -timeout=5m
```

### Makefile Targets
```makefile
.PHONY: test test-unit test-integration

# Fast unit tests
test-unit:
	go test -short ./... -v

# Integration tests
test-integration:
	go test -tags=integration ./test/integration/... -v -timeout=5m

# All tests
test:
	go test ./... -v
	go test -tags=integration ./test/integration/... -v
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
go test -tags=integration ./test/integration/... -v -count=1
```

### Run Specific Test with Logging
```bash
go test -tags=integration ./test/integration/... -run TestManager_StartStop -v -count=1 2>&1 | tee test.log
```

## Performance Considerations

- Integration tests are expected to take 1-10 seconds each
- Use shorter timeouts than production (for faster feedback)
- Multiple concurrent tests may compete for NATS ports
- Consider using `t.Parallel()` carefully (NATS server per test)
