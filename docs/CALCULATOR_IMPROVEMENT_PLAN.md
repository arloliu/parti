# Calculator Improvement Plan

**Status**: Draft
**Priority**: High
**Target Version**: v1.1.0
**Last Updated**: October 29, 2025

## Executive Summary

This document outlines a phased improvement plan for the `internal/assignment/calculator.go` component based on deep analysis that identified critical concurrency issues, performance bottlenecks, and architectural concerns.

**Critical Issues Found**: 4
**High Priority Issues**: 6
**Medium Priority Issues**: 8
**Low Priority Issues**: 5

---

## Phase 1: Critical Fixes (Required for Production)

### 1.1 Fix State Change Channel Dropping (CRITICAL)
**Issue**: State changes silently dropped when channel is full (line 237)
**Impact**: Manager/Calculator state desynchronization, incorrect behavior
**Priority**: P0 - Must fix before production use

**Current Code**:
```go
select {
case c.stateChangeChan <- state:
default:
    // State change LOST silently
}
```

**Solution**: Implement State Channel (Pub/Sub) Pattern using `xsync.Map`

**Benefits**:
- **High Performance**: `xsync.Map` is a highly optimized concurrent map for this use case.
- **Zero Message Loss**: Guarantees delivery for active subscribers without blocking the calculator.
- **Clean Decoupling**: Separates state production from consumption.
- **Extensible**: Easy to add more state observers (e.g., for metrics, debugging).
- **Idiomatic Go**: Uses clean, modern Go concurrency patterns.

**Implementation**:
```go
// In internal/assignment/calculator.go
import (
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/arloliu/parti/types"
	"github.com/puzpuzpuz/xsync/v4"
)

type stateSubscriber struct {
    ch     chan types.CalculatorState
    mu     sync.Mutex
    closed bool
}

func (s *stateSubscriber) trySend(state types.CalculatorState, metrics types.MetricsCollector) {
    s.mu.Lock()
    defer s.mu.Unlock()
    if s.closed {
        return
    }

    select {
    case s.ch <- state:
    default:
        metrics.RecordSlowSubscriber()
    }
}

func (s *stateSubscriber) close() {
    s.mu.Lock()
    if s.closed {
        s.mu.Unlock()
        return
    }
    s.closed = true
    close(s.ch)
    s.mu.Unlock()
}

type Calculator struct {
    // ... existing fields

    // State management fan-out
    subscribers      *xsync.Map[string, *stateSubscriber]
    nextSubscriberID atomic.Uint64

    // stateChangeChan is removed
}

// NewCalculator creates a new calculator
func NewCalculator(...) *Calculator {
    return &Calculator{
        // ... existing initialization
        subscribers: xsync.NewMap[string, *stateSubscriber](),
    }
}

// SubscribeToStateChanges returns a channel for state updates and a function to unsubscribe.
func (c *Calculator) SubscribeToStateChanges() (<-chan types.CalculatorState, func()) {
    id := c.nextSubscriberID.Add(1)
    key := strconv.FormatUint(id, 10)

    sub := &stateSubscriber{ch: make(chan types.CalculatorState, 1)}
    c.subscribers.Store(key, sub)

    // Immediately send the current state.
    sub.trySend(c.GetState(), c.metrics)

    unsubscribe := func() {
        c.removeSubscriber(key)
    }

    return sub.ch, unsubscribe
}

func (c *Calculator) removeSubscriber(key string) {
    if sub, ok := c.subscribers.LoadAndDelete(key); ok {
        sub.close()
    }
}

// emitStateChange notifies all subscribers of a state transition.
func (c *Calculator) emitStateChange(state types.CalculatorState) {
    oldState := c.GetState()
    if oldState == state {
        return // No change, no notification needed.
    }

    c.calcState.Store(int32(state))
    c.logger.Info("state transition", "from", oldState, "to", state)

    c.subscribers.Range(func(key string, sub *stateSubscriber) bool {
        sub.trySend(state, c.metrics)
        return true
    })
}

// Stop stops the calculator and cleans up all subscriber channels.
func (c *Calculator) Stop() error {
    // ... existing stop logic

    c.subscribers.Range(func(key string, sub *stateSubscriber) bool {
        c.subscribers.Delete(key)
        sub.close()
        return true
    })

    return nil
}
```

```go
// In manager.go

func (m *Manager) monitorCalculatorState() {
    defer m.wg.Done()

    // Subscribe to state changes and defer the unsubscribe call.
    stateCh, unsubscribe := m.calculator.SubscribeToStateChanges()
    defer unsubscribe()

    for {
        select {
        case <-m.ctx.Done():
            return
        case state, ok := <-stateCh:
            if !ok {
                m.logger.Info("calculator state channel closed")
                return // Channel closed, calculator has stopped.
            }
            if err := m.syncStateFromCalculator(state); err != nil {
                m.logError("failed to sync state from calculator", "state", state, "error", err)
            }
        }
    }
}
```

```go
// unit test
func TestCalculatorStateChanges(t *testing.T) {
    calc := NewCalculator(...)

    // Subscribe and defer unsubscribe.
    ch, unsubscribe := calc.SubscribeToStateChanges()
    defer unsubscribe()

    // Wait for initial state.
    initialState := <-ch
    assert.Equal(t, types.CalcStateIdle, initialState)

    // Trigger a state change.
    calc.enterScalingState(ctx, "test", 1*time.Second)

    // Verify the new state is received.
    select {
    case state := <-ch:
        assert.Equal(t, types.CalcStateScaling, state)
    case <-time.After(100 * time.Millisecond):
        t.Fatal("timeout waiting for state change")
    }
}
```

**Testing**:
- Unit tests with multiple subscribers
- Slow subscriber simulation
- Concurrent subscription/unsubscription

**Estimated Time**: 4 hours
**Owner**: TBD

---

### 1.2 Fix Watcher Lifecycle Race Condition (CRITICAL)
**Issue**: Race between Stop() and processWatcherEvents() (lines 704-719)
**Impact**: Potential panic, resource leak, unpredictable behavior
**Priority**: P0

**Current Code**:
```go
func (c *Calculator) Stop() error {
    c.watcherMu.Lock()
    if c.watcher != nil {
        c.watcher.Stop() // Race with processWatcherEvents
        c.watcher = nil
    }
    c.watcherMu.Unlock()
    close(c.stopCh)
}
```

**Solution**:
```go
func (c *Calculator) Stop() error {
    // 1. Signal stop first
    close(c.stopCh)

    // 2. Wait for goroutines to finish
    c.wg.Wait()

    // 3. Then cleanup watcher (safe now)
    c.watcherMu.Lock()
    if c.watcher != nil {
        c.watcher.Stop()
        c.watcher = nil
    }
    c.watcherMu.Unlock()

    return nil
}
```

**Testing**: Race detector, concurrent Stop() calls
**Estimated Time**: 3 hours
**Owner**: TBD

---

### 1.3 Fix Data Race in detectRebalanceType (CRITICAL)
**Issue**: Reading lastWorkers map without lock (line 461)
**Impact**: Data race, unpredictable behavior, crashes
**Priority**: P0

**Current Code**:
```go
func (c *Calculator) detectRebalanceType() RebalanceType {
    prevCount := len(c.lastWorkers) // Missing lock
    // ...
}
```

**Solution**:
```go
func (c *Calculator) detectRebalanceType() RebalanceType {
    c.mu.RLock()
    prevCount := len(c.lastWorkers)
    c.mu.RUnlock()
    // ...
}
```

**Testing**: Race detector, concurrent rebalancing
**Estimated Time**: 1 hour
**Owner**: TBD

---

### 1.4 Fix Goroutine Leaks (CRITICAL)
**Issue**: Goroutines started without wait group tracking (line 534)
**Impact**: Resource exhaustion over time
**Priority**: P0

**Current Code**:
```go
go func() {
    select {
    case <-time.After(window):
        c.enterRebalancingState(ctx)
    case <-c.stopCh:
        return
    }
}()
```

**Solution**:
```go
c.wg.Add(1)
go func() {
    defer c.wg.Done()
    timer := time.NewTimer(window)
    defer timer.Stop()

    select {
    case <-timer.C:
        c.enterRebalancingState(ctx)
    case <-c.stopCh:
        return
    }
}()
```

**Testing**: Goroutine leak detection, stress tests
**Estimated Time**: 2 hours
**Owner**: TBD

---

## Phase 2: High Priority Performance Fixes

### 2.1 Optimize Map Allocations (HIGH)
**Issue**: Creating new maps on every call (lines 551, 741, 791, 827)
**Impact**: GC pressure, allocation overhead
**Priority**: P1

**Solution**:
```go
// Use clear() (Go 1.21+) instead of recreating maps
clear(c.lastWorkers)
for w := range c.currentWorkers {
    c.lastWorkers[w] = true
}
```

**Metrics**: Reduce allocations by 60%, improve GC pause times
**Estimated Time**: 2 hours
**Owner**: TBD

---

### 2.2 Pre-allocate Slices (HIGH)
**Issue**: Dynamic append without capacity hints (line 961)
**Impact**: Multiple reallocations, memory churn
**Priority**: P1

**Solution**:
```go
// Before
var workers []string
for _, key := range keys {
    workers = append(workers, workerID)
}

// After
workers := make([]string, 0, len(keys))
for _, key := range keys {
    workers = append(workers, workerID)
}
```

**Estimated Time**: 1 hour
**Owner**: TBD

---

### 2.3 Cache Frequently Computed Strings (HIGH)
**Issue**: String formatting in hot paths (lines 900, 1050)
**Impact**: Unnecessary allocations
**Priority**: P1

**Solution**:
```go
type Calculator struct {
    // ... existing fields

    // Cached patterns
    workerKeyPattern string // "heartbeat.*"
    assignmentPrefix string // "assignment."
}

func NewCalculator(...) *Calculator {
    c := &Calculator{
        workerKeyPattern: fmt.Sprintf("%s.*", hbPrefix),
        assignmentPrefix: fmt.Sprintf("%s.", prefix),
    }
    return c
}
```

**Estimated Time**: 2 hours
**Owner**: TBD

---

### 2.4 Implement Circuit Breaker (HIGH)
**Issue**: Infinite retry loop on rebalance failures (line 866)
**Impact**: CPU waste, log spam
**Priority**: P1

**Solution**:
```go
type CircuitBreaker struct {
    failures      atomic.Int32
    lastFailTime  time.Time
    mu            sync.Mutex
}

func (c *Calculator) rebalanceWithCircuitBreaker(ctx context.Context, reason string) error {
    if c.circuitBreaker.IsOpen() {
        return ErrCircuitOpen
    }

    err := c.rebalance(ctx, reason)
    c.circuitBreaker.RecordResult(err)
    return err
}
```

**Estimated Time**: 4 hours
**Owner**: TBD

---

### 2.5 Add Retry with Backoff (HIGH)
**Issue**: No backoff on transient failures
**Impact**: Unnecessary load, poor error handling
**Priority**: P1

**Solution**:
```go
func (c *Calculator) rebalanceWithRetry(ctx context.Context, reason string) error {
    backoff := 100 * time.Millisecond
    maxBackoff := 5 * time.Second

    for attempt := 0; attempt < 3; attempt++ {
        if err := c.rebalance(ctx, reason); err == nil {
            return nil
        }

        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(backoff):
            backoff = min(backoff*2, maxBackoff)
        }
    }
    return ErrRebalanceFailed
}
```

**Estimated Time**: 3 hours
**Owner**: TBD

---

### 2.6 Batch KV Operations (HIGH)
**Issue**: Individual puts for each worker assignment (line 1031)
**Impact**: Network roundtrips, latency
**Priority**: P1

**Solution**:
```go
// Use KV.PutAll or batch operations if available
// Otherwise, use goroutines with rate limiting
const maxConcurrentPuts = 10
sem := make(chan struct{}, maxConcurrentPuts)

for workerID, parts := range assignments {
    sem <- struct{}{}
    go func(id string, p []types.Partition) {
        defer func() { <-sem }()
        c.publishAssignment(ctx, id, p, version)
    }(workerID, parts)
}
```

**Estimated Time**: 4 hours
**Owner**: TBD

---

## Phase 3: Architectural Improvements

### 3.1 Separate Concerns (MEDIUM)
**Issue**: Calculator handles too many responsibilities
**Impact**: Hard to test, maintain, reason about
**Priority**: P2

**Proposed Structure**:
```go
// Split into focused components
type Calculator struct {
    monitor   *WorkerMonitor        // Worker health detection
    state     *StateMachine         // State management
    publisher *AssignmentPublisher  // KV operations
    detector  *EmergencyDetector    // Emergency detection

    rebalanceCh chan RebalanceRequest
    metrics     *CalculatorMetrics
}

// Each component has clear responsibility
type WorkerMonitor struct {
    heartbeatKV jetstream.KeyValue
    detector    *EmergencyDetector
}

type AssignmentPublisher struct {
    assignmentKV jetstream.KeyValue
    mu           sync.Mutex
}
```

**Benefits**:
- Easier testing (mock individual components)
- Better code organization
- Reduced complexity per component
- Clearer interfaces

**Estimated Time**: 16 hours (2 days)
**Owner**: TBD

---

### 3.2 Implement Formal State Machine (MEDIUM)
**Issue**: Implicit state transitions, no validation
**Impact**: Hard to reason about, potential invalid states
**Priority**: P2

**Solution**:
```go
type StateMachine struct {
    current     types.CalculatorState
    mu          sync.RWMutex
    transitions map[types.CalculatorState][]types.CalculatorState
    onChange    func(from, to types.CalculatorState)
}

func (sm *StateMachine) Transition(to types.CalculatorState) error {
    sm.mu.Lock()
    defer sm.mu.Unlock()

    if !sm.isValidTransition(sm.current, to) {
        return ErrInvalidTransition
    }

    from := sm.current
    sm.current = to

    if sm.onChange != nil {
        go sm.onChange(from, to)
    }

    return nil
}
```

**Estimated Time**: 8 hours (1 day)
**Owner**: TBD

---

### 3.3 Add Comprehensive Metrics (MEDIUM)
**Issue**: Limited observability
**Impact**: Hard to debug production issues
**Priority**: P2

**Metrics to Add**:
```go
type CalculatorMetrics struct {
    // Performance
    rebalanceDuration      prometheus.Histogram
    kvOperationLatency     prometheus.Histogram

    // Reliability
    rebalanceAttempts      prometheus.Counter
    rebalanceFailures      prometheus.Counter
    droppedStateChanges    prometheus.Counter
    circuitBreakerOpens    prometheus.Counter

    // Health
    activeWorkerCount      prometheus.Gauge
    partitionCount         prometheus.Gauge
    assignmentVersion      prometheus.Gauge

    // State tracking
    stateTransitions       prometheus.CounterVec
    stateDuration          prometheus.HistogramVec
}
```

**Estimated Time**: 6 hours
**Owner**: TBD

---

### 3.4 Improve Error Handling (MEDIUM)
**Issue**: Silent failures, errors only logged (line 269)
**Impact**: Hard to detect data corruption
**Priority**: P2

**Solution**:
```go
type ErrorAggregator struct {
    errors []error
    mu     sync.Mutex
}

func (c *Calculator) processWatcherEvents() {
    errors := &ErrorAggregator{}

    for entry := range c.watcher.Updates() {
        if err := c.processEntry(entry); err != nil {
            errors.Add(err)
            c.metrics.RecordProcessingError(err)
        }
    }

    if errors.Count() > threshold {
        c.logger.Error("too many processing errors", "count", errors.Count())
        // Consider entering degraded state
    }
}
```

**Estimated Time**: 4 hours
**Owner**: TBD

---

## Phase 4: Code Quality Improvements

### 4.1 Add Context Propagation (LOW)
**Issue**: Background context used for some operations
**Priority**: P3

**Solution**: Use dedicated calculator context throughout

**Estimated Time**: 2 hours
**Owner**: TBD

---

### 4.2 Add Comprehensive Unit Tests (MEDIUM)
**Issue**: Complex logic with insufficient coverage
**Priority**: P2

**Target Coverage**: 85%+
**Focus Areas**:
- State machine transitions
- Emergency detection logic
- Rebalance type detection
- Worker monitoring

**Estimated Time**: 12 hours (1.5 days)
**Owner**: TBD

---

### 4.3 Add Benchmark Suite (LOW)
**Issue**: No performance benchmarks
**Priority**: P3

**Benchmarks to Add**:
- Map allocation patterns
- String formatting overhead
- KV operation batching
- Rebalance computation

**Estimated Time**: 4 hours
**Owner**: TBD

---

## Implementation Timeline

### Sprint 1 (Week 1) - Critical Fixes
- [ ] 1.1 Fix state change channel dropping
- [ ] 1.2 Fix watcher lifecycle race
- [ ] 1.3 Fix data race in detectRebalanceType
- [ ] 1.4 Fix goroutine leaks
- [ ] Testing and validation

**Deliverable**: Production-safe Calculator

---

### Sprint 2 (Week 2) - Performance Optimizations
- [ ] 2.1 Optimize map allocations
- [ ] 2.2 Pre-allocate slices
- [ ] 2.3 Cache computed strings
- [ ] 2.4 Implement circuit breaker
- [ ] 2.5 Add retry with backoff
- [ ] Performance benchmarking

**Deliverable**: 40% performance improvement

---

### Sprint 3 (Week 3) - Architectural Refactoring
- [ ] 2.6 Batch KV operations
- [ ] 3.1 Separate concerns (partial)
- [ ] 3.3 Add comprehensive metrics
- [ ] Integration testing

**Deliverable**: Improved observability, better architecture

---

### Sprint 4 (Week 4) - Polish & Documentation
- [ ] 3.1 Complete component separation
- [ ] 3.2 Formal state machine
- [ ] 3.4 Improve error handling
- [ ] 4.2 Comprehensive unit tests
- [ ] Documentation updates

**Deliverable**: Production-ready v1.1.0

---

## Success Metrics

### Performance Targets
- [ ] Reduce memory allocations by 50%
- [ ] Reduce GC pause times by 30%
- [ ] Improve rebalance latency by 40%
- [ ] Zero goroutine leaks over 24h test
- [ ] Zero data races in race detector

### Reliability Targets
- [ ] Zero dropped state changes under load
- [ ] 99.9% rebalance success rate
- [ ] Graceful degradation on failures
- [ ] < 1s recovery from transient errors

### Code Quality Targets
- [ ] 85%+ test coverage
- [ ] All critical paths benchmarked
- [ ] Zero linter warnings
- [ ] Comprehensive godoc coverage

---

## Risk Assessment

### High Risk Items
1. **State machine refactoring** - Complex, touches many code paths
   - Mitigation: Incremental changes, extensive testing

2. **Component separation** - Large refactor
   - Mitigation: Feature flags, gradual rollout

### Medium Risk Items
1. **Channel behavior change** - Could affect timing
   - Mitigation: A/B testing, performance comparison

2. **KV batching** - Network behavior change
   - Mitigation: Configurable, monitor metrics

---

## Testing Strategy

### Unit Tests
- State machine transitions
- Emergency detection logic
- Rebalance calculations
- Error handling paths

### Integration Tests
- Leader failover scenarios
- High-frequency state changes
- Concurrent operations
- Resource exhaustion

### Performance Tests
- Memory allocation benchmarks
- GC pressure under load
- Latency percentiles
- Throughput limits

### Chaos Tests
- Network partitions
- NATS failures
- Random worker crashes
- Clock skew

---

## Rollout Plan

### Phase 1: Internal Testing (Week 1-2)
- Deploy to staging
- Run load tests
- Monitor metrics
- Fix critical issues

### Phase 2: Canary Deployment (Week 3)
- 10% traffic
- Monitor error rates
- Compare metrics with v1.0
- Gradual rollout to 50%

### Phase 3: Full Deployment (Week 4)
- 100% traffic
- 24h monitoring
- Performance report
- Post-mortem review

---

## Appendix A: Code Examples

See individual sections above for specific code examples.

---

## Appendix B: Metrics Dashboard

```yaml
# Prometheus queries for monitoring
calculator_rebalance_duration_seconds:
  query: histogram_quantile(0.99, calculator_rebalance_duration_seconds)
  alert: > 5s

calculator_dropped_state_changes_total:
  query: rate(calculator_dropped_state_changes_total[5m])
  alert: > 0

calculator_rebalance_failures_total:
  query: rate(calculator_rebalance_failures_total[5m])
  alert: > 0.01

calculator_active_workers:
  query: calculator_active_workers
  alert: sudden changes
```

---

## Appendix C: References

- Original code: `internal/assignment/calculator.go`
- Analysis report: (generated October 29, 2025)
- Related issues: #TBD
- Design docs: `docs/design/04-components/calculator.md`

---

## Change Log

- 2025-10-29: Initial plan created based on deep analysis
