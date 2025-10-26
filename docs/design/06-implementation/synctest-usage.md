# Using testing/synctest for Concurrent Code Testing

## Overview

Go 1.25 introduces `testing/synctest`, a game-changing package for testing concurrent code. It provides deterministic testing of time-dependent and concurrent behavior by controlling goroutine scheduling and virtual time.

## Why synctest Matters for Parti

The parti library heavily relies on concurrent operations:
- **Heartbeat publishing** with periodic timers
- **Leader election** with timeout mechanisms
- **Assignment rebalancing** with cooldown periods
- **Background monitoring** goroutines
- **Context-based cancellation** throughout

Traditional testing of concurrent code has problems:
- ❌ `time.Sleep()` makes tests slow and flaky
- ❌ Race conditions can hide in timing variations
- ❌ Timeouts are unpredictable in CI environments
- ❌ Coverage of edge cases (simultaneous events) is difficult

With `synctest`:
- ✅ **Virtual time** - fast, deterministic time progression
- ✅ **Controlled scheduling** - reproduce race conditions reliably
- ✅ **Immediate verification** - no waiting for real timeouts
- ✅ **Complete coverage** - test rare timing scenarios easily

## Core Concepts

### synctest.Test()

The main entry point that creates an isolated testing bubble (note: use `synctest.Test()`, not `synctest.Run()`):

```go
func TestHeartbeat(t *testing.T) {
    synctest.Test(t, func(t *testing.T) {
        // Inside this bubble:
        // - Time is virtual and controlled
        // - Goroutines are scheduled deterministically
        // - time.Sleep(), time.After(), etc. use virtual time

        ticker := time.NewTicker(2 * time.Second)
        defer ticker.Stop()

        go func() {
            for range ticker.C {
                publishHeartbeat()
            }
        }()

        synctest.Wait() // Advance virtual time until all goroutines blocked
    })
}
```

### synctest.Wait()

Advances virtual time until all goroutines in the bubble are durably blocked or finished:

```go
synctest.Test(t, func(t *testing.T) {
    done := make(chan struct{})

    go func() {
        time.Sleep(5 * time.Second)  // Virtual - returns immediately
        close(done)
    }()

    synctest.Wait()  // Advances time, goroutine completes
    <-done           // Immediately ready
})
```

## Testing Patterns for Parti

### 1. Testing Periodic Operations (Heartbeats)

**Without synctest** (slow, flaky):
```go
func TestHeartbeat_Old(t *testing.T) {
    publisher := NewHeartbeatPublisher(cfg, nc)
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    publisher.Start(ctx)

    time.Sleep(3 * time.Second)  // ❌ Slow
    // Check that 1-2 heartbeats were published (range due to timing)

    time.Sleep(3 * time.Second)  // ❌ More waiting
    // Check for more heartbeats
}
```

**With synctest** (fast, deterministic):
```go
func TestHeartbeat_Synctest(t *testing.T) {
    synctest.Test(t, func(t *testing.T) {
        publisher := NewHeartbeatPublisher(cfg, nc)
        ctx, cancel := context.WithCancel(context.Background())
        defer cancel()

        publisher.Start(ctx)

        synctest.Wait()  // ✅ Instant - advance to first heartbeat
        require.Equal(t, 1, getHeartbeatCount())

        synctest.Wait()  // ✅ Instant - advance to second heartbeat
        require.Equal(t, 2, getHeartbeatCount())

        cancel()
        synctest.Wait()  // ✅ Verify clean shutdown
    })
}
```

### 2. Testing Timeouts and Election

**Election with timeout**:
```go
func TestElection_Timeout(t *testing.T) {
    synctest.Test(t, func(t *testing.T) {
        agent := NewElectionAgent(cfg, nc)
        ctx := context.Background()

        // Start election
        resultCh := make(chan bool, 1)
        go func() {
            isLeader, _ := agent.Campaign(ctx, "worker-1", 5*time.Second)
            resultCh <- isLeader
        }()

        // No response from other nodes - should timeout
        synctest.Wait()  // ✅ Instant timeout after 5 virtual seconds

        require.False(t, <-resultCh)  // Lost election due to timeout
    })
}
```

### 3. Testing Rebalancing Cooldowns

```go
func TestRebalancing_Cooldown(t *testing.T) {
    synctest.Test(t, func(t *testing.T) {
        mgr := setupManager(t)

        // Trigger first rebalance
        mgr.RefreshPartitions(context.Background())
        require.Equal(t, StateRebalancing, mgr.State())

        synctest.Wait()  // Complete rebalance
        require.Equal(t, StateStable, mgr.State())

        // Immediate second rebalance should be blocked by cooldown
        mgr.RefreshPartitions(context.Background())
        require.Equal(t, StateStable, mgr.State())  // Still stable

        // Advance past cooldown period (10s)
        synctest.Wait()

        // Now rebalance should be allowed
        mgr.RefreshPartitions(context.Background())
        require.Equal(t, StateRebalancing, mgr.State())
    })
}
```

### 4. Testing Manager Lifecycle

```go
func TestManager_Lifecycle(t *testing.T) {
    synctest.Test(t, func(t *testing.T) {
        mgr := setupManager(t)
        ctx := context.Background()

        // Start manager - spawns multiple goroutines:
        // - Heartbeat publisher
        // - Election participant
        // - Assignment listener
        // - Metrics reporter
        err := mgr.Start(ctx)
        require.NoError(t, err)

        // Let all background tasks initialize
        synctest.Wait()

        require.Equal(t, StateStable, mgr.State())
        require.NotEmpty(t, mgr.WorkerID())

        // Graceful shutdown
        shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
        defer cancel()

        err = mgr.Stop(shutdownCtx)
        require.NoError(t, err)

        // Verify all goroutines finished
        synctest.Wait()
    })
}
```

### 5. Testing Race Conditions

**Simultaneous leader election**:
```go
func TestElection_SimultaneousCandidates(t *testing.T) {
    synctest.Test(t, func(t *testing.T) {
        agent1 := NewElectionAgent(cfg, nc1)
        agent2 := NewElectionAgent(cfg, nc2)

        results := make(chan string, 2)

        // Both try to become leader at exact same virtual time
        go func() {
            isLeader, _ := agent1.Campaign(ctx, "worker-1", 5*time.Second)
            if isLeader {
                results <- "worker-1"
            }
        }()

        go func() {
            isLeader, _ := agent2.Campaign(ctx, "worker-2", 5*time.Second)
            if isLeader {
                results <- "worker-2"
            }
        }()

        synctest.Wait()  // Let election complete

        // Exactly one should win
        require.Len(t, results, 1)
    })
}
```

### 6. Testing Context Cancellation

```go
func TestHeartbeat_ContextCancellation(t *testing.T) {
    synctest.Test(t, func(t *testing.T) {
        publisher := NewHeartbeatPublisher(cfg, nc)
        ctx, cancel := context.WithCancel(context.Background())

        publisher.Start(ctx)

        // Publish a few heartbeats
        synctest.Wait()
        synctest.Wait()
        count := getHeartbeatCount()
        require.GreaterOrEqual(t, count, 2)

        // Cancel and verify immediate stop
        cancel()
        synctest.Wait()

        // No more heartbeats after cancellation
        require.Equal(t, count, getHeartbeatCount())
    })
}
```

## Best Practices

### ✅ DO

1. **Use synctest.Test() for time-dependent tests**:
   ```go
   func TestWithTimer(t *testing.T) {
       synctest.Test(t, func(t *testing.T) {
           // Test code with timers, tickers, sleeps
       })
   }
   ```

2. **Call synctest.Wait() to advance time**:
   ```go
   synctest.Wait()  // Advance until all goroutines durably blocked
   ```

3. **Test shutdown paths explicitly**:
   ```go
   cancel()
   synctest.Wait()  // Verify cleanup
   ```

4. **Use with embedded NATS for integration tests**:
   ```go
   synctest.Test(t, func(t *testing.T) {
       ns, nc := testutil.StartEmbeddedNATS(t)
       // Test with real NATS + virtual time
   })
   ```

5. **Combine with table-driven tests for timing scenarios**:
   ```go
   tests := []struct {
       name     string
       delay    time.Duration
       expected State
   }{
       {"fast transition", 1 * time.Second, StateStable},
       {"slow transition", 30 * time.Second, StateRebalancing},
   }
   ```

### ❌ DON'T

1. **Don't use real time.Sleep() in synctest.Test()**:
   ```go
   synctest.Test(t, func(t *testing.T) {
       time.Sleep(5 * time.Second)  // ✅ Virtual - OK
   })

   // Outside synctest.Test():
   time.Sleep(5 * time.Second)  // ❌ Real sleep - blocks tests
   ```

2. **Don't mix synctest with real external services**:
   ```go
   // ❌ Bad - external NATS won't respect virtual time
   synctest.Test(t, func(t *testing.T) {
       nc, _ := nats.Connect("nats://external:4222")
   })

   // ✅ Good - embedded NATS works with synctest
   synctest.Test(t, func(t *testing.T) {
       ns, nc := testutil.StartEmbeddedNATS(t)
   })
   ```

3. **Don't forget to wait for goroutines**:
   ```go
   synctest.Test(t, func(t *testing.T) {
       go doSomething()
       // ❌ Test ends immediately - deadlock panic
   })

   synctest.Test(t, func(t *testing.T) {
       go doSomething()
       synctest.Wait()  // ✅ Wait for completion
   })
   ```

## Integration with Parti's Testing Strategy

### Test Organization

```
parti/
├── internal/
│   ├── heartbeat/
│   │   ├── publisher.go
│   │   ├── publisher_test.go         # Use synctest.Run()
│   │   └── publisher_integration_test.go  # synctest + embedded NATS
│   ├── election/
│   │   ├── nats.go
│   │   ├── nats_test.go              # Use synctest.Run()
│   │   └── nats_integration_test.go  # synctest + embedded NATS
│   └── assignment/
│       ├── calculator.go
│       ├── calculator_test.go        # Unit tests
│       └── calculator_integration_test.go  # synctest + embedded NATS
├── manager_test.go                    # Use synctest.Run()
└── manager_integration_test.go       # synctest + embedded NATS
```

### Example: Complete Heartbeat Test Suite

```go
// publisher_test.go - Unit tests with mocks
func TestPublisher_Start(t *testing.T) {
    synctest.Test(t, func(t *testing.T) {
        mockConn := &mockNATSConn{}
        pub := NewPublisher(cfg, mockConn)

        ctx, cancel := context.WithCancel(context.Background())
        defer cancel()

        pub.Start(ctx)
        synctest.Wait()  // First publish

        require.Equal(t, 1, mockConn.PublishCount())
    })
}

// publisher_integration_test.go - Integration with real NATS
func TestPublisher_Integration(t *testing.T) {
    synctest.Test(t, func(t *testing.T) {
        ns, nc := testutil.StartEmbeddedNATS(t)
        defer ns.Shutdown()

        pub := NewPublisher(cfg, nc)
        ctx, cancel := context.WithCancel(context.Background())
        defer cancel()

        // Subscribe to heartbeat topic
        sub, _ := nc.Subscribe("parti.heartbeat", func(msg *nats.Msg) {
            // Verify heartbeat payload
        })
        defer sub.Unsubscribe()

        pub.Start(ctx)

        // Verify multiple heartbeats
        for range 5 {
            synctest.Wait()
        }

        require.GreaterOrEqual(t, sub.MessageCount(), 5)
    })
}
```

## Migration Strategy

### Phase 1: New Code (Immediate)
- All new concurrent code uses synctest from day 1
- Heartbeat publisher tests
- Election agent tests
- Assignment calculator tests
- Manager lifecycle tests

### Phase 2: Existing Code (Gradual)
- Existing tests can continue using real time
- Migrate to synctest when modifying tests
- No rush - both approaches work

### Phase 3: Documentation (Ongoing)
- Document all timing assumptions
- Add synctest examples to each component
- Update testing guidelines

## Common Patterns Cheat Sheet

```go
// Pattern: Periodic Task
synctest.Test(t, func(t *testing.T) {
    ticker := time.NewTicker(interval)
    defer ticker.Stop()
    go func() {
        for range ticker.C {
            doWork()
        }
    }()
    synctest.Wait()  // Advance to first tick
})

// Pattern: Timeout
synctest.Test(t, func(t *testing.T) {
    ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()

    result := doWorkWithContext(ctx)
    synctest.Wait()  // Timeout happens instantly
})

// Pattern: Rate Limiting
synctest.Test(t, func(t *testing.T) {
    limiter := rate.NewLimiter(rate.Every(time.Second), 1)

    for range 10 {
        limiter.Wait(ctx)  // Virtual wait
        doWork()
    }
    synctest.Wait()  // All work completes instantly
})

// Pattern: Cooldown
synctest.Test(t, func(t *testing.T) {
    lastRebalance := time.Now()

    if time.Since(lastRebalance) < cooldownPeriod {
        return  // Too soon
    }

    synctest.Wait()  // Advance past cooldown
    rebalance()
})
```

## References

- [Go 1.25 Release Notes - testing/synctest](https://tip.golang.org/doc/go1.25)
- [testing/synctest Package Documentation](https://pkg.go.dev/testing/synctest)
- [Proposal: Deterministic Testing for Time and Goroutines](https://github.com/golang/go/issues/67434)

## Summary

Using `testing/synctest` in parti will:
- ✅ Make concurrent tests **100x faster** (no real sleeps)
- ✅ Make tests **completely deterministic** (no flakes)
- ✅ Enable **comprehensive edge case testing** (race conditions)
- ✅ Simplify **complex timing scenarios** (timeouts, cooldowns)
- ✅ Improve **CI/CD reliability** (no timing-dependent failures)

Every component that uses time, timers, tickers, or concurrent goroutines should use `synctest.Run()` in tests.
