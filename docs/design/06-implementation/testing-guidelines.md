# Testing Guidelines

This document outlines the testing strategy and best practices for the parti project.

## Quick Links

- **[Concurrent Testing with synctest](./synctest-usage.md)** - Using Go 1.25's testing/synctest for deterministic concurrent testing
- **Unit Tests** - Testing individual components in isolation
- **Integration Tests** - Testing with embedded NATS server
- **End-to-End Tests** - Testing complete workflows

## Overview

The Parti library uses a comprehensive testing approach with three levels:
1. **Unit Tests** - Test individual components in isolation with mocks
2. **Integration Tests** - Test components with real NATS using embedded server
3. **End-to-End Tests** - Test full scenarios with multiple workers

## Embedded NATS Server for Testing

### Why Embedded NATS?

We use an **embedded NATS server** for integration testing instead of external containers or services.

**Advantages:**
- ✅ **Zero External Dependencies** - No Docker, no containers, just Go
- ✅ **Fast Startup** - Milliseconds vs seconds for containers
- ✅ **Easy CI/CD** - Works everywhere Go works (GitHub Actions, GitLab CI, etc.)
- ✅ **Simple Code** - Import `github.com/nats-io/nats-server/v2/server`
- ✅ **JetStream Support** - Full KV store functionality for testing
- ✅ **No Port Conflicts** - Uses random ports, perfect for parallel tests
- ✅ **Automatic Cleanup** - `t.Cleanup()` handles all cleanup

### Basic Usage

```go
package mypackage

import (
    "testing"
    partitest "github.com/arloliu/parti/testing"
)

func TestMyComponent(t *testing.T) {
    // Start embedded NATS server with JetStream
    _, nc := partitest.StartEmbeddedNATS(t)
    // nc is a connected NATS client
    // Server and connection are automatically cleaned up after test

    // Use nc for your tests
    js, _ := nc.JetStream()
    kv, _ := js.CreateKeyValue(&nats.KeyValueConfig{...})
}
```

### Parallel Tests

The embedded NATS server supports parallel test execution:

```go
func TestParallel(t *testing.T) {
    for i := range 10 {
        t.Run(fmt.Sprintf("worker-%d", i), func(t *testing.T) {
            t.Parallel() // Run tests concurrently

            // Each test gets its own server on a unique port
            _, nc := testutil.StartEmbeddedNATS(t)

            // No port conflicts!
        })
    }
}
```

### Cluster Testing (Advanced)

For testing high-availability scenarios:

```go
func TestLeaderFailover(t *testing.T) {
    // Start 3-node NATS cluster
    servers, nc := testutil.StartEmbeddedNATSCluster(t)

    // Simulate leader failure
    servers[0].Shutdown()

    // Test failover behavior
    // ...
}
```

## Testing Patterns by Component Type

### 1. Pure Logic Components (No NATS)

Test with simple unit tests and table-driven tests:

```go
// internal/hash/ring_test.go
func TestHashRing_AddNode(t *testing.T) {
    ring := NewHashRing(100)
    ring.AddNode("worker-0", 100)

    // Test pure logic
    node := ring.GetNode("partition-key")
    require.Equal(t, "worker-0", node)
}
```

### 2. NATS-Dependent Components

Use embedded NATS for integration tests:

```go
// internal/election/nats_test.go
func TestNATSElection_RequestLeadership(t *testing.T) {
    _, nc := testutil.StartEmbeddedNATS(t)

    agent := NewNATSElection(nc, "test-bucket")

    ctx := context.Background()
    isLeader, err := agent.RequestLeadership(ctx, "worker-0", 30)

    require.NoError(t, err)
    require.True(t, isLeader)
}

func TestNATSElection_MultipleWorkers(t *testing.T) {
    _, nc := testutil.StartEmbeddedNATS(t)

    // Create multiple agents sharing the same server
    agent1 := NewNATSElection(nc, "test-bucket")
    agent2 := NewNATSElection(nc, "test-bucket")

    ctx := context.Background()

    // First worker becomes leader
    isLeader1, _ := agent1.RequestLeadership(ctx, "worker-0", 30)
    require.True(t, isLeader1)

    // Second worker fails to become leader
    isLeader2, _ := agent2.RequestLeadership(ctx, "worker-1", 30)
    require.False(t, isLeader2)
}
```

### 3. Manager End-to-End Tests

Test complete scenarios with multiple workers:

```go
// manager_integration_test.go
func TestManager_ColdStart(t *testing.T) {
    _, nc := testutil.StartEmbeddedNATS(t)

    // Define test partitions
    partitions := []parti.Partition{
        {Keys: []string{"p0"}, Weight: 100},
        {Keys: []string{"p1"}, Weight: 100},
        {Keys: []string{"p2"}, Weight: 100},
    }

    // Start 3 workers
    managers := make([]*parti.Manager, 3)
    for i := range 3 {
        cfg := &parti.Config{
            WorkerIDPrefix: "worker",
            WorkerIDMin:    0,
            WorkerIDMax:    10,
        }
        src := source.NewStatic(partitions)
        strat := strategy.NewConsistentHash()

        mgr, err := parti.NewManager(cfg, nc, src, strat)
        require.NoError(t, err)

        err = mgr.Start(context.Background())
        require.NoError(t, err)

        managers[i] = mgr
        t.Cleanup(func() { mgr.Stop(context.Background()) })
    }

    // Wait for stabilization
    time.Sleep(2 * time.Second)

    // Verify all partitions assigned
    assignedCount := 0
    for _, mgr := range managers {
        assignment := mgr.CurrentAssignment()
        assignedCount += len(assignment.Partitions)
    }
    require.Equal(t, len(partitions), assignedCount)

    // Verify exactly one leader
    leaderCount := 0
    for _, mgr := range managers {
        if mgr.IsLeader() {
            leaderCount++
        }
    }
    require.Equal(t, 1, leaderCount)
}
```

## Test Organization

### Directory Structure

```
internal/
├── testutil/              # Shared test utilities
│   ├── nats.go            # Embedded NATS helpers
│   ├── nats_test.go       # Test the helpers themselves
│   └── fixtures.go        # Common test fixtures
│
├── election/
│   ├── nats.go            # Implementation
│   ├── nats_test.go       # Unit + integration tests
│   └── mock.go            # Mocks for other tests
│
├── stableid/
│   ├── claimer.go
│   ├── claimer_test.go    # Unit + integration tests
│   └── mock.go
│
└── ... (other packages)
```

### File Naming Conventions

- `*_test.go` - All test files (unit + integration)
- `*_bench_test.go` - Benchmark files (optional, use sparingly)
- `mock.go` - Mock implementations for testing other packages
- `fixtures.go` - Common test data and helpers

### Test Function Naming

```go
// Unit tests
func TestComponentName_Method(t *testing.T)
func TestComponentName_Method_EdgeCase(t *testing.T)

// Integration tests (same pattern, just uses real NATS)
func TestComponentName_Integration_Scenario(t *testing.T)

// Table-driven tests
func TestComponentName_Method(t *testing.T) {
    tests := []struct {
        name string
        // ...
    }{
        {name: "valid input"},
        {name: "empty input"},
    }
    // ...
}
```

## Best Practices

### 1. Use t.Helper() in Test Utilities

```go
func setupTestEnvironment(t *testing.T) *Environment {
    t.Helper() // Stack traces point to caller, not this function
    // ...
}
```

### 2. Use t.Cleanup() for Resource Cleanup

```go
func TestWithCleanup(t *testing.T) {
    resource := acquireResource()
    t.Cleanup(func() {
        resource.Release() // Always runs, even on panic
    })
    // ...
}
```

### 3. Use Subtests for Related Scenarios

```go
func TestManager(t *testing.T) {
    t.Run("cold start", func(t *testing.T) { /* ... */ })
    t.Run("rolling update", func(t *testing.T) { /* ... */ })
    t.Run("scale up", func(t *testing.T) { /* ... */ })
}
```

### 4. Skip Expensive Tests in Short Mode

```go
func TestExpensiveOperation(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping expensive test in short mode")
    }
    // ...
}
```

Run with: `go test -short`

### 5. Use Table-Driven Tests for Multiple Inputs

```go
func TestHashRing_GetNode(t *testing.T) {
    tests := []struct {
        name     string
        key      string
        expected string
    }{
        {"first partition", "p0", "worker-0"},
        {"second partition", "p1", "worker-1"},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // Test with tt.key, tt.expected
        })
    }
}
```

### 6. Use testify/require for Assertions

```go
require.NoError(t, err)         // Fails immediately
require.Equal(t, expected, actual)
require.True(t, condition)
require.NotNil(t, value)
```

## Running Tests

```bash
# Run all tests
go test ./...

# Run specific package
go test ./internal/election

# Run with verbose output
go test -v ./...

# Run in short mode (skip slow tests)
go test -short ./...

# Run with coverage
go test -cover ./...

# Run specific test
go test -run TestNATSElection_RequestLeadership ./internal/election

# Run parallel tests
go test -parallel 4 ./...
```

## Continuous Integration

The embedded NATS approach works perfectly in CI environments:

```yaml
# GitHub Actions example
- name: Run tests
  run: go test -v -race -cover ./...
```

No need for Docker, service containers, or environment setup!

## Future Testing Scenarios

As implementation progresses, add tests for:

1. **Cold Start** - 3 workers start simultaneously
2. **Rolling Update** - Replace workers one at a time
3. **Scale Up** - Add workers dynamically (3 → 5)
4. **Scale Down** - Remove workers gracefully (5 → 3)
5. **Leader Crash** - Leader fails, another takes over
6. **Network Partition** - Simulate network issues
7. **Assignment Churn** - Rapid worker join/leave events
8. **Cache Affinity** - Measure partition stability during rebalancing

## Summary

- ✅ Use **embedded NATS server** for all integration tests
- ✅ Write tests alongside implementation
- ✅ Use `testutil.StartEmbeddedNATS(t)` for single server
- ✅ Use `testutil.StartEmbeddedNATSCluster(t)` for HA testing
- ✅ Leverage `t.Cleanup()` for automatic resource management
- ✅ Support parallel test execution with `-parallel`
- ✅ Works seamlessly in CI/CD without external dependencies
