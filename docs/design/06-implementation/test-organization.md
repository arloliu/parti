# Test Organization

This document describes the testing strategy and organization for the `parti` library.

## Overview

The project uses a **hybrid testing approach**:
- **Unit tests**: Co-located with implementation files
- **Integration tests**: Dedicated `test/integration/` directory for cross-component scenarios

This approach provides:
- ✅ Fast unit tests for component-level verification
- ✅ Comprehensive integration tests for end-to-end scenarios
- ✅ Clear separation for CI/CD pipelines
- ✅ Easy to maintain and extend

## Directory Structure

```
parti/
├── manager.go
├── manager_test.go              # Unit tests (mocks, fast)
├── manager_debug_test.go        # Debug utilities
├── config_test.go               # Unit tests
├── test/                        # Integration and E2E tests
│   ├── README.md                # Test documentation
│   ├── integration/             # Integration tests
│   │   ├── manager_lifecycle_test.go
│   │   ├── leader_failover_test.go
│   │   ├── scale_up_test.go
│   │   └── ...
│   └── testutil/                # Shared test utilities
│       ├── fixtures.go
│       └── helpers.go
├── internal/
│   ├── election/
│   │   ├── nats_election.go
│   │   └── nats_election_test.go    # Mixed unit + integration
│   ├── heartbeat/
│   │   ├── publisher.go
│   │   └── publisher_test.go        # Mixed unit + integration
│   └── ...
├── strategy/
│   ├── consistent_hash.go
│   └── consistent_hash_test.go      # Unit tests
├── source/
│   ├── static.go
│   └── static_test.go               # Unit tests
└── ...
```

## Test Types

### 1. Unit Tests (`*_test.go`)

**Location**: Co-located with implementation files
**Package**: Same package or `_test` package
**Characteristics**:
- Fast execution (< 1 second)
- Use mocks and fakes
- Test single components in isolation
- May use embedded NATS for internal packages
- Protected with `testing.Short()` guard

**Example**:
```go
// strategy/consistent_hash_test.go
package strategy

import "testing"

func TestConsistentHash_Assign(t *testing.T) {
    // No external dependencies
    // Tests logic in isolation
}
```

**Internal Package Tests** (special case):
```go
// internal/election/nats_election_test.go
package election

func TestElection_CampaignForLeader(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test in short mode")
    }

    // Uses embedded NATS but still considered "unit test"
    // Tests single component (election) in isolation
}
```

### 2. Integration Tests (`test/integration/`)

**Location**: `test/integration/` directory
**Package**: `integration_test`
**Build Tags**: `//go:build integration`
**Characteristics**:
- Slower execution (1-10 seconds)
- Test multiple components working together
- Use embedded NATS server
- Test real-world scenarios
- Black-box testing (external package)

**Example**:
```go
//go:build integration
// +build integration

package integration_test

import "testing"

func TestLeaderFailover(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test in short mode")
    }

    // Start multiple managers
    // Kill leader
    // Verify new leader elected
    // Verify assignments remain stable
}
```

## Running Tests

### Quick Reference

| Command | What It Does | Duration | Use Case |
|---------|--------------|----------|----------|
| `make test-unit` | Unit tests only | ~5s | Fast feedback during development |
| `make test-integration` | Integration tests only | ~30s | Test cross-component scenarios |
| `make test-all` | Unit + Integration | ~35s | Pre-commit verification |
| `make test` | Unit tests with race detector | ~10s | Race condition detection |
| `make ci` | Full CI suite | ~45s | CI/CD pipeline |

### Detailed Commands

**Fast unit tests** (during development):
```bash
make test-unit
# or
go test -short ./...
```

**Integration tests only**:
```bash
make test-integration
# or
go test -tags=integration ./test/integration/... -v
```

**All tests**:
```bash
make test-all
# or run separately:
go test -short ./...
go test -tags=integration ./test/integration/...
```

**With race detector** (unit tests):
```bash
make test-race
# or
go test -race ./...
```

**Specific integration test**:
```bash
go test -tags=integration ./test/integration/... -run TestLeaderFailover -v
```

**With coverage**:
```bash
make coverage
make coverage-html  # Opens in browser
```

## Writing Tests

### Unit Test Guidelines

1. **Co-locate with implementation**:
   ```
   manager.go
   manager_test.go
   ```

2. **Use same package for white-box testing**:
   ```go
   package parti  // Can access private fields/methods
   ```

3. **Use `_test` package for black-box testing**:
   ```go
   package parti_test  // Only access exported API
   ```

4. **Mock external dependencies**:
   ```go
   type mockSource struct{}
   func (m *mockSource) ListPartitions(ctx context.Context) ([]Partition, error) {
       return []Partition{{Keys: []string{"p0"}}}, nil
   }
   ```

5. **Keep tests fast**:
   - No network calls (except embedded NATS in internal packages)
   - No file I/O unless necessary
   - Use `testing.Short()` guard for slower tests

### Integration Test Guidelines

1. **Place in `test/integration/`**:
   ```
   test/integration/leader_failover_test.go
   ```

2. **Always use build tags**:
   ```go
   //go:build integration
   // +build integration

   package integration_test
   ```

3. **Always use `testing.Short()` guard**:
   ```go
   func TestScenario(t *testing.T) {
       if testing.Short() {
           t.Skip("skipping integration test in short mode")
       }
       // test code...
   }
   ```

4. **Use embedded NATS**:
   ```go
   srv, conn := testutil.StartEmbeddedNATS(t)
   defer srv.Shutdown()
   defer conn.Close()
   ```

5. **Test realistic scenarios**:
   - Multiple workers
   - Leader failover
   - Network partitions
   - Concurrent operations
   - Rebalancing

6. **Clean up resources**:
   ```go
   defer mgr.Stop(context.Background())
   defer conn.Close()
   defer srv.Shutdown()
   ```

### Test Naming Conventions

**Unit tests**:
- `TestTypeName_MethodName` - Testing specific method
- `TestTypeName_MethodName_Scenario` - Testing specific scenario

**Integration tests**:
- `TestManager_*` - Manager-level scenarios
- `TestLeader_*` - Leader election/failover
- `TestScale_*` - Scaling operations
- `TestAssignment_*` - Partition assignment
- `TestNetwork_*` - Network partition scenarios

## Internal Package Test Strategy

Internal packages (`internal/*`) use a **mixed approach**:

### Why Mixed?

1. **Component isolation**: Each internal package tests its own component
2. **Fast enough**: Even with embedded NATS, tests are relatively fast
3. **Convenience**: Tests live next to implementation
4. **White-box testing**: Can test internal details

### Pattern

```go
// internal/election/nats_election_test.go
package election

func TestElection_FastLogic(t *testing.T) {
    // Pure unit test - no NATS
    // Tests internal logic
}

func TestElection_CampaignForLeader(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test in short mode")
    }

    // Uses embedded NATS
    // Still tests only election component
    srv, conn := testutil.StartEmbeddedNATS(t)
    defer srv.Shutdown()
    defer conn.Close()

    // test election logic...
}
```

### What Goes in `test/integration/`?

**Cross-component scenarios** that test multiple parts working together:
- Manager + Election + Heartbeat + Assignment
- Multiple workers coordinating
- Leader failover affecting assignments
- Scale events triggering rebalancing
- Network partitions and recovery

## CI/CD Integration

### GitHub Actions Example

```yaml
name: CI

on: [push, pull_request]

jobs:
  unit-tests:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - uses: actions/setup-go@v4
        with:
          go-version: '1.25'

      - name: Run unit tests
        run: make test-unit

      - name: Run with race detector
        run: make test-race

  integration-tests:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - uses: actions/setup-go@v4
        with:
          go-version: '1.25'

      - name: Run integration tests
        run: make test-integration

  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - uses: actions/setup-go@v4
        with:
          go-version: '1.25'

      - name: Run linters
        run: make lint
```

### Makefile Targets

The Makefile provides these test targets:

```makefile
make test-unit          # Fast unit tests
make test-integration   # Integration tests
make test-all          # Both unit + integration
make test              # Unit tests with race detector
make test-race         # Race detector only
make coverage          # Coverage report
make ci                # Full CI suite
```

## Coverage

### Generating Coverage

```bash
# All tests (unit + integration)
make coverage

# HTML report
make coverage-html
```

### Coverage Goals

- **Unit tests**: >80% code coverage
- **Integration tests**: Focus on scenarios, not line coverage
- **Critical paths**: 100% coverage (leader election, assignment logic)

## Performance Benchmarks

Benchmarks are optional and should be in separate files:

```
component_bench_test.go
```

Example:
```go
func BenchmarkConsistentHash_Assign(b *testing.B) {
    strategy := NewConsistentHash()
    workers := []string{"w0", "w1", "w2"}
    partitions := make([]Partition, 100)
    // ... setup

    for b.Loop() {
        strategy.Assign(workers, partitions)
    }
}
```

## Debugging Tests

### Verbose Output

```bash
go test -v ./...
go test -tags=integration ./test/integration/... -v
```

### Run Specific Test

```bash
go test ./manager_test.go -run TestNewManager_NilSafety -v
go test -tags=integration ./test/integration/... -run TestLeaderFailover -v
```

### Disable Test Cache

```bash
go test -count=1 ./...
```

### With Logging

```go
import "github.com/arloliu/parti/internal/logging"

cfg.Logger = logger.NewStdLogger(logger.LevelDebug)
```

## Best Practices

### DO ✅

- Write unit tests for all public functions
- Use table-driven tests when you have multiple test cases
- Keep tests independent (no shared state)
- Use descriptive test names
- Clean up resources with `defer`
- Use `testing.Short()` for slower tests
- Use build tags for integration tests
- Test error cases and edge cases

### DON'T ❌

- Mix test types in the same file
- Create test interdependencies
- Use real external services (use embedded NATS)
- Skip error checking in tests
- Use `time.Sleep()` for synchronization (use channels or sync primitives)
- Hardcode timeouts (make them configurable)
- Forget to clean up resources

## Future Enhancements

### Planned Test Categories

1. **E2E Tests** (`test/e2e/`):
   - Test with real NATS cluster
   - Multi-node scenarios
   - Production-like configurations

2. **Load Tests** (`test/load/`):
   - Performance under load
   - Stress testing
   - Resource usage

3. **Chaos Tests** (`test/chaos/`):
   - Random failures
   - Network delays
   - Resource exhaustion

## Summary

| Test Type | Location | Speed | Coverage Goal | Purpose |
|-----------|----------|-------|---------------|---------|
| **Unit** | Next to code | Fast (< 1s) | >80% | Component verification |
| **Integration** | `test/integration/` | Medium (1-10s) | Scenarios | Multi-component interaction |
| **Internal Mixed** | `internal/*/` | Fast-Medium | >80% | Component + NATS integration |

**Key Principle**: Keep unit tests fast and co-located, use dedicated integration directory for complex scenarios.
