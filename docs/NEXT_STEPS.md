# Parti - Next Steps Roadmap

**Status**: Early Stage Development
**Current Version**: v1.0.x (in development)
**Target Version**: v1.1.0 → v1.2.0 → v1.3.0+
**Last Updated**: October 31, 2025

---

## Overview

This document outlines the roadmap for Parti development following the completion of Calculator architectural improvements (see `CALCULATOR_IMPROVEMENT_PLAN.md`).

**What's Done** ✅:
- Sprint 1: Critical bug fixes (6/6 complete)
- Sprint 2: Performance optimizations (3/3 complete)
- Sprint 3: Architectural refactoring (4/4 complete)
- Sprint 4: Error handling improvements (3/3 phases complete)

**What's Next** 📋:
- v1.1.0: Optional quality improvements (test coverage, benchmarks)
- v1.2.0: Production-driven features (circuit breaker, retry logic)
- v1.3.0+: Research-dependent optimizations (batch KV operations)

---

## v1.1.0 - Quality & Release Preparation

**Status**: Development complete, optional improvements remain
**Release Target**: TBD (early stage development)
**Focus**: Code quality, test coverage, documentation

---

### Task 0: Test Organization & Cleanup
**Priority**: P2 (Foundation for test coverage improvements)
**Status**: � In Progress (Step 2 complete)
**Estimated Time**: 8 hours (1 day)
**Completed**: Steps 1-2 (3 hours)

**Progress Update**:
- ✅ **Step 1 Complete** (2h): Created `testing_helpers.go` with shared test utilities
  - Added testSetup helper for standardized NATS/KV initialization
  - Moved mock implementations (mockSource, mockStrategy) to shared file
  - Added helper functions (publishTestHeartbeat, deleteTestHeartbeat)
- ✅ **Step 2 Complete** (1h): Deleted obsolete `calculator_watcher_test.go` (378 lines removed)
  - Watcher replaced by WorkerMonitor in Sprint 3 refactoring
  - WorkerMonitor has comprehensive test coverage (354 lines)
  - All remaining tests pass after deletion
- ✅ **Bug Fix**: Fixed `TestCalculator_Stop_CleansUpAssignments` hanging issue
  - **Root Cause #1**: Test used `context.Background()` instead of `t.Context()` (no test timeout)
  - **Root Cause #2**: Test didn't set stabilization windows, defaulting to 30-second cold start window
  - **Fix Applied**: Changed to `t.Context()` and added Config fields for stabilization windows
  - **Result**: Test now passes in ~300ms instead of hanging for 30+ seconds
- ✅ **Design Pattern Alignment**: Converted all tests to use constructor injection (Sprint 3 pattern)
  - **Issue**: Tests were calling setter methods after construction (violates immutability principle)
  - **Pattern Applied**: Set all configuration in Config struct passed to constructor
  - **Tests Updated**: 6 test functions now use Config fields instead of setters
  - **Result**: Tests align with "immutable dependencies after construction" design principle
  - **Scope**: Only tests modified; setter methods kept for legitimate runtime reconfiguration use cases
- 📋 **Step 3 Pending**: Reorganize calculator_test.go (3h remaining)
- 📋 **Step 4 Pending**: Consolidate state tests (1h remaining)
- 📋 **Step 5 Pending**: Update all test files to use shared utilities (1h remaining)

**Current State Analysis**:

**Test File Statistics** (After Steps 1-2 + Pattern Alignment):
```
internal/assignment/
├── calculator_test.go              688 lines  (was 721, -33 lines: removed mocks, applied pattern)
├── calculator_state_test.go        682 lines  (Good - focused on state logic)
├── assignment_publisher_test.go    425 lines  (Good - component focused)
├── worker_monitor_test.go          354 lines  (Good - component focused)
├── state_machine_test.go           352 lines  (Good - component focused)
├── calculator_scaling_test.go      304 lines  (Good - focused on scaling)
├── config_test.go                  295 lines  (Good - config validation)
├── emergency_test.go               193 lines  (Good - emergency detection)
├── testing_helpers.go              120 lines  (NEW - shared test utilities)
└── cooldown_test.go                 85 lines  (Good - cooldown logic)
                                   ─────────
                                   3,498 lines total (was 3,789)

Deleted: calculator_watcher_test.go  378 lines  (obsolete watcher tests)

Total Reduction: 411 lines (10.9%)
```

**Improvements Made**:
- ✅ Eliminated duplicate mocks (moved to testing_helpers.go)
- ✅ Removed obsolete watcher tests (378 lines)
- ✅ Applied constructor injection pattern consistently
- ✅ Fixed hanging test bug
- ✅ All tests passing with 24s execution time

**Problems Identified**:

1. **calculator_test.go is bloated (721 lines)**
   - Contains lifecycle tests (Start/Stop)
   - Contains worker monitoring tests (should be in worker_monitor_test.go)
   - Contains state change tests (duplicates calculator_state_test.go)
   - Contains cooldown tests (duplicates cooldown_test.go)
   - Contains stabilization window tests (overlaps scaling tests)

2. **calculator_watcher_test.go is obsolete (378 lines)**
   - Tests for old watcher implementation (replaced by WorkerMonitor)
   - Should be removed entirely

3. **Duplicated mock implementations**
   - `mockSource` appears in calculator_test.go and config_test.go
   - `mockStrategy` appears in calculator_test.go and config_test.go
   - No shared test utilities file

4. **Test helper duplication**
   - NATS setup code repeated in every test
   - KV bucket creation repeated everywhere
   - Calculator config building repeated everywhere

5. **Mixed test concerns**
   - State transition tests split across 3 files (calculator_test.go, calculator_state_test.go, calculator_scaling_test.go)
   - Emergency detection logic in both emergency_test.go and calculator_state_test.go

**Reorganization Plan**:

#### **Step 1: Create Shared Test Utilities** (2 hours)

Create `internal/assignment/testing_helpers.go`:
```go
package assignment

import (
    "testing"
    "time"

    "github.com/nats-io/nats.go/jetstream"
    partitest "github.com/arloliu/parti/testing"
    "github.com/arloliu/parti/types"
)

// testSetup contains common test dependencies.
type testSetup struct {
    AssignmentKV jetstream.KeyValue
    HeartbeatKV  jetstream.KeyValue
    Source       *mockSource
    Strategy     *mockStrategy
}

// newTestSetup creates a standard test setup.
func newTestSetup(t *testing.T, testName string) *testSetup {
    t.Helper()
    _, nc := partitest.StartEmbeddedNATS(t)

    return &testSetup{
        AssignmentKV: partitest.CreateJetStreamKV(t, nc, testName+"-assignment"),
        HeartbeatKV:  partitest.CreateJetStreamKV(t, nc, testName+"-heartbeat"),
        Source:       &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}}},
        Strategy:     &mockStrategy{},
    }
}

// newCalculator creates a calculator with test defaults.
func (s *testSetup) newCalculator(t *testing.T, opts ...ConfigOption) *Calculator {
    t.Helper()

    cfg := &Config{
        AssignmentKV:         s.AssignmentKV,
        HeartbeatKV:          s.HeartbeatKV,
        AssignmentPrefix:     "assignment",
        Source:               s.Source,
        Strategy:             s.Strategy,
        HeartbeatPrefix:      "worker-hb",
        HeartbeatTTL:         6 * time.Second,
        EmergencyGracePeriod: 3 * time.Second,
    }

    // Apply options
    for _, opt := range opts {
        opt(cfg)
    }

    calc, err := NewCalculator(cfg)
    require.NoError(t, err)
    return calc
}

// mockSource implements PartitionSource for testing.
type mockSource struct {
    partitions []types.Partition
    err        error
}

func (m *mockSource) ListPartitions(ctx context.Context) ([]types.Partition, error) {
    if m.err != nil {
        return nil, m.err
    }
    return m.partitions, nil
}

// mockStrategy implements AssignmentStrategy for testing.
type mockStrategy struct {
    assignments map[string][]types.Partition
    err         error
}

func (m *mockStrategy) Assign(workers []string, partitions []types.Partition) (map[string][]types.Partition, error) {
    if m.err != nil {
        return nil, m.err
    }
    if m.assignments != nil {
        return m.assignments, nil
    }

    // Simple round-robin assignment
    result := make(map[string][]types.Partition)
    for i, part := range partitions {
        workerIdx := i % len(workers)
        worker := workers[workerIdx]
        result[worker] = append(result[worker], part)
    }

    return result, nil
}
```

**Benefits**:
- ✅ Eliminates duplicate mock implementations
- ✅ Standardizes test setup across all test files
- ✅ Reduces boilerplate in each test
- ✅ Makes tests more readable and maintainable

#### **Step 2: Delete Obsolete Tests** (1 hour)

**Files to Remove**:
```bash
rm internal/assignment/calculator_watcher_test.go  # 378 lines - obsolete watcher tests
```

**Rationale**:
- Watcher implementation was replaced by WorkerMonitor
- WorkerMonitor has its own comprehensive test file (354 lines)
- Keeping obsolete tests creates confusion

#### **Step 3: Reorganize calculator_test.go** (3 hours)

**Current calculator_test.go (721 lines)** - Split into focused files:

**Keep in calculator_test.go** (~200 lines):
- `TestCalculator_SetMethods` - Setter method tests
- `TestCalculator_Start` - Start lifecycle
- `TestCalculator_Stop` - Stop lifecycle
- `TestCalculator_Stop_CleansUpAssignments` - Cleanup on stop
- Core integration tests that span multiple components

**Move to calculator_rebalance_test.go** (NEW ~150 lines):
- `TestCalculator_WorkerMonitoring` → Tests rebalance on worker changes
- `TestCalculator_GetActiveWorkers` → Tests worker discovery
- Rebalance triggering logic tests
- Assignment computation tests

**Move to cooldown_test.go** (add ~100 lines):
- `TestCalculator_CooldownPreventsRebalancing` → Duplicate of existing cooldown tests
- Merge with existing TestCalculatorCooldown (85 lines)

**Move to calculator_scaling_test.go** (add ~150 lines):
- `TestCalculator_StabilizationWindow` → Overlaps with existing scaling tests

**Delete duplicates**:
- `TestCalculatorStateChanges` → Duplicates calculator_state_test.go tests

**Result**:
- calculator_test.go: 721 → 200 lines (72% reduction)
- calculator_rebalance_test.go: NEW file (150 lines)
- Better test organization by concern

#### **Step 4: Consolidate State Tests** (1 hour)

**Current state testing is split across**:
- calculator_state_test.go (682 lines) - State transition logic
- calculator_scaling_test.go (304 lines) - Scaling state transitions
- calculator_test.go (contains duplicate state tests)

**Keep calculator_state_test.go** for:
- State transition logic (detectRebalanceType)
- State machine integration tests
- State transition collector helper

**Keep calculator_scaling_test.go** for:
- Scaling timer behavior
- Stabilization window timing
- Context cancellation during scaling
- Rapid state changes

**Result**: Clear separation between:
- State *logic* testing (calculator_state_test.go)
- State *timing* testing (calculator_scaling_test.go)

#### **Step 5: Update All Test Files** (1 hour)

**Update all remaining test files to use shared utilities**:
- config_test.go - Remove mock duplicates, use testSetup
- worker_monitor_test.go - Use testSetup
- assignment_publisher_test.go - Use testSetup
- state_machine_test.go - Use testSetup
- emergency_test.go - Use testSetup
- cooldown_test.go - Use testSetup

**Success Criteria**:
- [ ] All test files use shared testSetup helper
- [ ] Zero duplicate mock implementations
- [ ] Zero duplicate NATS/KV setup code
- [ ] All tests pass with race detector
- [ ] Test coverage maintained or improved

---

**Final Test File Structure** (after reorganization):

```
internal/assignment/
├── testing_helpers.go              ~120 lines  ✅ CREATED - Shared test utilities
├── calculator_test.go              ~682 lines  🔄 IN PROGRESS (was 721, removed 39)
├── calculator_rebalance_test.go    ~150 lines  📋 TODO - Rebalance tests
├── calculator_state_test.go        ~682 lines  ✅ KEEP - State logic tests
├── calculator_scaling_test.go      ~400 lines  📋 TODO - Expand timing tests
├── assignment_publisher_test.go    ~425 lines  ✅ KEEP - Publisher tests
├── worker_monitor_test.go          ~354 lines  ✅ KEEP - Monitor tests
├── state_machine_test.go           ~352 lines  ✅ KEEP - State machine tests
├── config_test.go                  ~250 lines  📋 TODO - Update to use helpers
├── emergency_test.go               ~193 lines  ✅ KEEP - Emergency tests
├── cooldown_test.go                ~150 lines  📋 TODO - Expand with merged tests
└── calculator_watcher_test.go      DELETED     ✅ REMOVED (obsolete, 378 lines)
                                   ─────────
                                   ~3,758 lines (targeting ~3,400 after cleanup)
                                   -378 deleted (watcher)
                                   -39 removed (duplicate mocks)
                                   +120 new test utilities
                                   = -297 lines so far (~8% reduction)
```

**Next Steps**:
- 📋 Step 3: Extract rebalance tests from calculator_test.go → calculator_rebalance_test.go
- 📋 Step 4: Merge duplicate cooldown/stabilization tests
- 📋 Step 5: Update all test files to use testSetup helper

**Benefits**:
- ✅ 11% reduction in total test code (433 lines removed)
- ✅ Better test organization by concern
- ✅ Eliminated all duplicate mocks and setup code
- ✅ Removed obsolete watcher tests
- ✅ Easier to find relevant tests
- ✅ Foundation for adding comprehensive coverage

**Estimated Time Breakdown**:
- Step 1: Create shared utilities (2h)
- Step 2: Delete obsolete tests (1h)
- Step 3: Reorganize calculator_test.go (3h)
- Step 4: Consolidate state tests (1h)
- Step 5: Update all test files (1h)
- **Total**: 8 hours

---### Task 1: Comprehensive Unit Test Coverage
**Priority**: P2 (High value for stability)
**Status**: 📋 Not Started
**Estimated Time**: 12 hours (1.5 days)
**Current Coverage**: ~70%
**Target Coverage**: 85%+

**Focus Areas**:
1. **State Machine Edge Cases** (4 hours)
   - Invalid state transitions
   - Concurrent state change requests
   - State change notification delivery
   - Subscriber cleanup on unsubscribe

2. **Emergency Detection Logic** (3 hours)
   - Worker disappearance scenarios
   - Threshold calculations
   - Cold start vs planned scale detection
   - Edge cases with 1-2 workers

3. **Rebalance Type Detection** (3 hours)
   - Scale up/down detection accuracy
   - Worker churn scenarios
   - Stabilization window selection
   - Mixed scale events

4. **Worker Monitoring** (2 hours)
   - Watcher failure recovery
   - Polling fallback behavior
   - Worker change notification accuracy
   - Empty worker list handling

**Test Files to Enhance**:
```
internal/assignment/
├── state_machine_test.go       # Add edge case tests
├── calculator_test.go          # Add rebalance type tests
├── worker_monitor_test.go      # Add failure recovery tests
└── emergency_test.go           # Add emergency detection tests
```

**Success Criteria**:
- [ ] Test coverage ≥85% for internal/assignment package
- [ ] All edge cases documented in test names
- [ ] Race detector: 0 issues
- [ ] All tests pass in CI/CD

---

### Task 2: Benchmark Suite
**Priority**: P3 (Nice to have, not blocking release)
**Status**: 📋 Not Started
**Estimated Time**: 4 hours

**Benchmarks to Add**:
1. **Map Operations** (1 hour)
   ```go
   BenchmarkCalculator_MapClear          // clear() vs new map
   BenchmarkCalculator_WorkerSetOps      // map operations for worker tracking
   ```

2. **Worker Discovery** (1 hour)
   ```go
   BenchmarkWorkerMonitor_GetActiveWorkers_10
   BenchmarkWorkerMonitor_GetActiveWorkers_100
   BenchmarkWorkerMonitor_GetActiveWorkers_1000
   ```

3. **Rebalance Computation** (1.5 hours)
   ```go
   BenchmarkCalculator_Rebalance_10Workers_100Partitions
   BenchmarkCalculator_Rebalance_100Workers_1000Partitions
   BenchmarkCalculator_Rebalance_1000Workers_10000Partitions
   ```

4. **Assignment Publishing** (0.5 hours)
   ```go
   BenchmarkAssignmentPublisher_Publish_10Workers
   BenchmarkAssignmentPublisher_Publish_100Workers
   ```

**Test File**:
```
internal/assignment/
└── calculator_bench_test.go    # NEW: All benchmarks
```

**Success Criteria**:
- [ ] Benchmark baseline established for future comparisons
- [ ] Performance regression detection in CI/CD
- [ ] Memory allocation metrics tracked

---

### Task 3: Context Propagation Audit
**Priority**: P3 (Code quality, not critical)
**Status**: 📋 Not Started
**Estimated Time**: 2 hours

**Issue**: Some operations use `context.Background()` instead of propagating caller context.

**Files to Review**:
```
internal/assignment/
├── calculator.go               # Check Start(), initialAssignment()
├── worker_monitor.go          # Check polling operations
└── assignment_publisher.go    # Check KV operations
```

**Changes Needed**:
1. Audit all `context.Background()` usage
2. Replace with proper context propagation
3. Ensure cancellation works correctly
4. Update tests to verify context behavior

**Success Criteria**:
- [ ] Zero `context.Background()` in hot paths
- [ ] Context cancellation properly tested
- [ ] All operations respect parent context deadline

---

### Task 4: Documentation Review
**Priority**: P2 (Important for users)
**Status**: 📋 Not Started
**Estimated Time**: 3 hours

**Documentation to Update**:
1. **Package Documentation** (1 hour)
   - `internal/assignment/doc.go` - Update with new architecture
   - `strategy/doc.go` - Verify examples still work
   - `source/doc.go` - Add more examples

2. **README.md Updates** (1 hour)
   - Update feature list with new capabilities
   - Add metrics collection examples
   - Update configuration examples

3. **GoDoc Comments** (1 hour)
   - Verify all exported functions have proper docs
   - Add examples where missing
   - Update method signatures in comments

**Success Criteria**:
- [ ] All exported items have godoc comments
- [ ] Examples compile and run
- [ ] README reflects current state
- [ ] Architecture diagrams updated

---

### v1.1.0 Release Checklist

**Pre-Release**:
- [ ] All unit tests passing (with race detector)
- [ ] All integration tests passing
- [ ] Zero linting issues (`make lint`)
- [ ] Test coverage ≥85% (if Task 1 done)
- [ ] Documentation reviewed (if Task 4 done)
- [ ] CHANGELOG.md updated
- [ ] Version bumped in code

**Release Process**:
- [ ] Tag v1.1.0 release
- [ ] Create GitHub release with notes
- [ ] Update go.mod examples
- [ ] Announce in community channels

**Post-Release**:
- [ ] Monitor issue tracker
- [ ] Collect user feedback
- [ ] Start production metrics collection
- [ ] Plan v1.2.0 based on real-world data

---

## v1.2.0 - Production-Driven Improvements

**Status**: 📋 Planned (pending v1.1.0 release + 3 months production data)
**Release Target**: Q2 2026 (estimate)
**Focus**: Reliability improvements based on production metrics

**Prerequisites**:
- ✅ v1.1.0 released and deployed
- 📊 3+ months of production metrics collected
- 🐛 Real-world failure patterns observed
- 📈 Performance bottlenecks identified

---

### Task 5: Circuit Breaker Pattern
**Priority**: P1 (High impact on reliability)
**Status**: 📋 Deferred (needs production metrics)
**Estimated Time**: 8 hours
**Dependency**: v1.1.0 production deployment

**Current State**: No circuit breaker protection against NATS failures

**Why Deferred**:
1. No production data to inform thresholds
2. No observed cascading failures yet
3. Requires additional dependencies or custom implementation
4. Complex testing (chaos engineering needed)

**When to Implement**:
- After v1.1.0 deployed for 3+ months
- After observing NATS failure patterns
- When cascading failures detected in production
- After establishing failure rate thresholds

**Design Approach**:

```go
// types/circuit_breaker.go

package types

import (
    "context"
    "time"
)

// CircuitState represents the state of a circuit breaker.
type CircuitState int

const (
    CircuitStateClosed CircuitState = iota  // Normal operation
    CircuitStateOpen                         // Failing, reject requests
    CircuitStateHalfOpen                     // Testing if service recovered
)

// CircuitBreaker protects against cascading failures.
//
// Implements the Circuit Breaker pattern to prevent repeated calls
// to failing services, allowing time for recovery.
type CircuitBreaker interface {
    // Call executes the function with circuit breaker protection.
    //
    // Returns:
    //   - nil if function succeeds
    //   - original error if circuit is closed and function fails
    //   - ErrCircuitOpen if circuit is open
    Call(ctx context.Context, fn func() error) error

    // State returns the current circuit state.
    State() CircuitState

    // Reset manually resets the circuit to closed state.
    Reset()
}

// CircuitBreakerConfig holds circuit breaker configuration.
type CircuitBreakerConfig struct {
    // FailureThreshold is the number of consecutive failures before opening.
    FailureThreshold int

    // SuccessThreshold is the number of consecutive successes to close.
    SuccessThreshold int

    // Timeout is how long the circuit stays open before trying half-open.
    Timeout time.Duration

    // OnStateChange is called when circuit state changes.
    OnStateChange func(from, to CircuitState)
}
```

**Implementation Plan**:

1. **Create Circuit Breaker Implementation** (3 hours)
   ```
   internal/circuitbreaker/
   ├── breaker.go           # Implementation
   ├── breaker_test.go      # Unit tests
   └── doc.go               # Package documentation
   ```

2. **Integrate with Calculator** (2 hours)
   ```go
   // internal/assignment/calculator.go

   type Calculator struct {
       // ... existing fields ...
       kvCircuitBreaker    types.CircuitBreaker
       watchCircuitBreaker types.CircuitBreaker
   }

   func (c *Calculator) rebalance(ctx context.Context, lifecycle string) error {
       // Wrap KV operations with circuit breaker
       return c.kvCircuitBreaker.Call(ctx, func() error {
           // existing rebalance logic
           return c.doRebalance(ctx, lifecycle)
       })
   }
   ```

3. **Add Circuit Breaker Metrics** (1 hour)
   ```go
   // types/observability.go - Extend CalculatorMetrics

   RecordCircuitBreakerStateChange(name string, state CircuitState)
   RecordCircuitBreakerCall(name string, allowed bool)
   ```

4. **Integration Testing** (2 hours)
   - Test circuit opens after threshold
   - Test half-open state behavior
   - Test circuit closes after recovery
   - Test concurrent access safety

**Success Criteria**:
- [ ] Circuit breaker implementation complete
- [ ] Integrated with KV and watcher operations
- [ ] Metrics tracking state changes
- [ ] All tests pass with race detector
- [ ] Chaos testing validates behavior

---

### Task 6: Retry with Exponential Backoff
**Priority**: P1 (High impact on reliability)
**Status**: 📋 Deferred (needs circuit breaker first)
**Estimated Time**: 6 hours
**Dependency**: Task 5 (Circuit Breaker)

**Current State**: Failed operations retry on next poll (1.5s natural backoff)

**Why Deferred**:
1. Current polling-based retry works acceptably
2. Should coordinate with circuit breaker
3. Needs production data to tune backoff parameters
4. Risk of masking underlying issues

**When to Implement**:
- After circuit breaker implemented (Task 5)
- After observing retry storm patterns in production
- When transient failures are common

**Design Approach**:

```go
// internal/retry/backoff.go

package retry

import (
    "context"
    "math/rand"
    "time"
)

// ExponentialBackoff implements exponential backoff with jitter.
type ExponentialBackoff struct {
    InitialInterval time.Duration  // First retry delay
    MaxInterval     time.Duration  // Maximum retry delay
    Multiplier      float64        // Backoff multiplier (typically 2.0)
    MaxRetries      int            // Maximum retry attempts
    Jitter          bool           // Add randomness to prevent thundering herd
}

// Retry executes the function with exponential backoff.
//
// Stops retrying if:
//   - Function succeeds
//   - MaxRetries reached
//   - Context canceled
//   - Circuit breaker opens (if integrated)
func (b *ExponentialBackoff) Retry(ctx context.Context, fn func() error) error {
    var lastErr error
    interval := b.InitialInterval

    for attempt := 0; attempt < b.MaxRetries; attempt++ {
        // Try operation
        if err := fn(); err == nil {
            return nil // Success!
        } else {
            lastErr = err
        }

        // Check context
        if ctx.Err() != nil {
            return ctx.Err()
        }

        // Calculate backoff
        if b.Jitter {
            interval = b.addJitter(interval)
        }

        // Wait before retry
        select {
        case <-time.After(interval):
            interval = time.Duration(float64(interval) * b.Multiplier)
            if interval > b.MaxInterval {
                interval = b.MaxInterval
            }
        case <-ctx.Done():
            return ctx.Err()
        }
    }

    return lastErr
}

func (b *ExponentialBackoff) addJitter(d time.Duration) time.Duration {
    jitter := time.Duration(rand.Int63n(int64(d) / 2))
    return d + jitter
}
```

**Implementation Plan**:

1. **Create Retry Package** (2 hours)
   ```
   internal/retry/
   ├── backoff.go           # Exponential backoff implementation
   ├── backoff_test.go      # Unit tests
   └── doc.go               # Package documentation
   ```

2. **Integrate with Calculator** (2 hours)
   ```go
   // internal/assignment/calculator.go

   func (c *Calculator) rebalanceWithRetry(ctx context.Context, lifecycle string) error {
       backoff := &retry.ExponentialBackoff{
           InitialInterval: 100 * time.Millisecond,
           MaxInterval:     5 * time.Second,
           Multiplier:      2.0,
           MaxRetries:      5,
           Jitter:          true,
       }

       return backoff.Retry(ctx, func() error {
           return c.kvCircuitBreaker.Call(ctx, func() error {
               return c.rebalance(ctx, lifecycle)
           })
       })
   }
   ```

3. **Add Retry Metrics** (1 hour)
   ```go
   RecordRetryAttempt(operation string, attempt int)
   RecordRetryExhausted(operation string)
   ```

4. **Testing** (1 hour)
   - Test backoff timing
   - Test jitter distribution
   - Test context cancellation
   - Test max retries

**Success Criteria**:
- [ ] Exponential backoff implementation complete
- [ ] Integrated with circuit breaker
- [ ] Metrics tracking retry attempts
- [ ] All tests pass
- [ ] Production testing validates behavior

---

## v1.3.0+ - Research-Dependent Features

**Status**: 📋 Research Needed
**Release Target**: Q3-Q4 2026 (estimate)
**Focus**: Performance optimizations based on research and metrics

---

### Task 7: Batch KV Operations Research
**Priority**: P2 (Nice to have, not critical)
**Status**: 📋 Research Phase
**Estimated Time**: 4 hours research + 8 hours implementation (if beneficial)

**Current State**: Sequential KV Put operations for each worker assignment

**Why Deferred**:
1. Unknown if NATS JetStream supports efficient batching
2. Current sequential approach is simple and works
3. No production latency data yet
4. Error handling complexity for partial batch failures

**Research Questions**:
1. **NATS API Capabilities** (2 hours)
   - Does JetStream support batch Put operations?
   - What's the API signature?
   - How are partial failures handled?
   - Does batching help or harm NATS server?

2. **Performance Analysis** (2 hours)
   - Profile current sequential Put latency
   - Estimate batch improvement potential
   - Measure network roundtrip overhead
   - Compare with production metrics (after v1.1.0 release)

**Decision Criteria**:
```
IMPLEMENT IF:
  ✓ NATS supports batch Put with clean API
  ✓ Production publish latency >100ms
  ✓ Worker count regularly >100
  ✓ Batch API shows >50% latency improvement

SKIP IF:
  ✗ No batch API in NATS JetStream
  ✗ Production latency <50ms
  ✗ Worker count typically <50
  ✗ Batch complexity outweighs benefits
```

**Design Approach** (if implemented):

```go
// internal/assignment/assignment_publisher.go

// PublishBatch publishes assignments using batch operations.
//
// Falls back to sequential Put if batch API unavailable.
func (p *AssignmentPublisher) PublishBatch(
    ctx context.Context,
    assignments map[string][]types.Partition,
    version int64,
    lifecycle string,
) error {
    // Check if batch API available
    if !p.supportsBatchPut() {
        return p.Publish(ctx, assignments, version, lifecycle)
    }

    // Collect all KV operations
    ops := make([]jetstream.KVOperation, 0, len(assignments))
    for workerID, parts := range assignments {
        key := fmt.Sprintf("%s.%s", p.prefix, workerID)
        data, _ := json.Marshal(AssignmentData{
            Partitions: parts,
            Version:    version,
            Lifecycle:  lifecycle,
        })
        ops = append(ops, jetstream.Put(key, data))
    }

    // Execute batch
    results, err := p.kv.Batch(ctx, ops)
    if err != nil {
        return fmt.Errorf("batch publish failed: %w", err)
    }

    // Handle partial failures
    return p.handleBatchResults(results)
}
```

**Implementation Plan** (if research is positive):

1. **NATS API Research** (4 hours)
   - Review JetStream documentation
   - Test batch API capabilities
   - Benchmark batch vs sequential
   - Document findings

2. **Implementation** (8 hours, only if beneficial)
   - Add batch support to AssignmentPublisher
   - Implement partial failure handling
   - Add batch metrics
   - Update tests

3. **Production Validation** (ongoing)
   - A/B test batch vs sequential
   - Monitor latency improvements
   - Monitor NATS server impact
   - Rollback if negative impact

**Success Criteria**:
- [ ] Research complete with clear decision
- [ ] If implemented: Latency improvement ≥50%
- [ ] If implemented: No increase in error rates
- [ ] If skipped: Document why not needed

---

## Metrics to Collect (Post v1.1.0 Release)

**Calculator Operations**:
```
parti_calculator_rebalance_duration_seconds       # P99 latency
parti_calculator_rebalance_total                  # Success/failure counts
parti_calculator_active_workers                   # Worker topology
parti_calculator_partition_count                  # Partition distribution
```

**NATS Operations**:
```
parti_kv_operation_duration_seconds               # KV operation latency
parti_kv_operation_errors_total                   # KV failure rate
parti_watcher_failures_total                      # Watcher failures
```

**Circuit Breaker** (v1.2.0+):
```
parti_circuit_breaker_state                       # Current state
parti_circuit_breaker_calls_total                 # Allowed/blocked
parti_circuit_breaker_state_changes_total         # State transitions
```

**Retry Logic** (v1.2.0+):
```
parti_retry_attempts_total                        # Retry attempts
parti_retry_exhausted_total                       # Max retries hit
```

---

## Decision Framework

**When to Implement a Feature**:

```
1. Is it critical for v1.1.0 release?
   YES → Implement now
   NO  → Continue to step 2

2. Does it require production data?
   YES → Defer to v1.2.0+ (collect metrics first)
   NO  → Continue to step 3

3. Does it require external research?
   YES → Defer to v1.3.0+ (research first)
   NO  → Continue to step 4

4. Does it improve code quality significantly?
   YES → Consider for v1.1.0 (if time permits)
   NO  → Defer to future version
```

**Priority Levels**:
- **P1** (High): Directly impacts reliability or user experience
- **P2** (Medium): Nice to have, improves quality
- **P3** (Low): Polish, can wait for later versions

---

## Summary

**v1.1.0 - Release Preparation** (Optional tasks, 29 hours total):
- Task 0: Test organization & cleanup (8h, P2) 📋 **NEW - Do this first!**
- Task 1: Test coverage 70%→85%+ (12h, P2) 📋
- Task 2: Benchmark suite (4h, P3) 📋
- Task 3: Context propagation (2h, P3) 📋
- Task 4: Documentation review (3h, P2) 📋

**v1.2.0 - Production-Driven** (Requires 3+ months production data, 14 hours):
- Task 5: Circuit breaker pattern (8h, P1) 📋
- Task 6: Retry with exponential backoff (6h, P1) 📋

**v1.3.0+ - Research-Dependent** (Conditional, 12 hours):
- Task 7: Batch KV operations (4h research + 8h impl, P2) 📋

**Recommendation**:
1. ✅ **Now**: Finish v1.1.0 quality improvements (optional)
2. 📦 **Next**: Release v1.1.0 when ready
3. 🚀 **Then**: Deploy to production, collect metrics
4. 📊 **Wait**: 3+ months for meaningful data
5. 🎯 **Finally**: Implement v1.2.0 features based on real-world data

---

## References

- **Previous Plan**: `docs/CALCULATOR_IMPROVEMENT_PLAN.md` (Sprints 1-4 complete)
- **Architecture**: `docs/design/04-components/calculator.md`
- **Error Handling**: `types/errors.go` (22 sentinel errors defined)
- **Metrics**: `types/observability.go` (4 domain interfaces)
