# Practical Implementation Plan

**Last Updated**: October 26, 2025
**Status**: In Progress - Phase 1
**See Also**: [STATUS.md](STATUS.md) for detailed component status

---

## Overview

This is the **actionable execution plan** for completing Parti. It prioritizes implementation and testing before documentation.

**Current Status**: Foundation works (~70% complete), state machine missing (~30% remaining)

**Timeline**: 6 weeks to production-ready

**Philosophy**: Test first, document later. Be honest about status.

---

## Phase 0: Foundation Audit ✅ COMPLETE

**Goal**: Honest assessment of what works vs what doesn't

**Deliverables**:
- ✅ STATUS.md created with component-by-component assessment
- ✅ Identified 3 missing states (Scaling, Rebalancing, Emergency)
- ✅ Identified minimal integration test coverage (~10%)
- ✅ Documented that state machine is critical missing piece

**Outcome**: We now have honest baseline - foundation works, state machine doesn't

**Outcome**: We now have honest baseline - foundation works, state machine doesn't

---

## Phase 1: Complete State Machine (CURRENT) 🔴

**Status**: ⏳ Not Started
**Priority**: Critical - Everything depends on this
**Timeline**: 2 weeks (10 working days)
**See**: [State Machine Details](#appendix-state-machine-implementation) for technical approach

### Why First?
State machine is foundational architecture. Rebalancing, scaling, emergency response all depend on it.

### Goals
1. Wire calculator state machine to Manager states
2. Implement adaptive stabilization windows (30s cold start, 10s planned scale)
3. Implement emergency rebalancing (no window)
4. Test all state transitions with integration tests

### Tasks

#### 1.1 Calculator Internal State (3 days)
- [ ] Add `calculatorState` enum (Idle, Scaling, Rebalancing, Emergency)
- [ ] Add state fields to Calculator struct (`calcState`, `scalingStart`, `scalingReason`)
- [ ] Implement `detectRebalanceType()`:
  - Cold start: 0→N workers = 30s window
  - Planned scale: Gradual changes = 10s window
  - Emergency: Worker disappeared = 0s window
  - Restart: >50% workers gone = 30s window
- [ ] Implement state transition methods:
  - `enterScalingState(reason, window)` - start stabilization timer
  - `enterRebalancingState()` - trigger assignment calculation
  - `enterEmergencyState()` - immediate rebalance
  - `returnToIdleState()` - back to stable
- [ ] Wire `monitorWorkerHealth()` to call `detectRebalanceType()`
- [ ] Add `GetState()` method for Manager to observe calculator state

**Tests**:
- [ ] Unit test `detectRebalanceType()` logic with various worker count changes
- [ ] Unit test each state transition method
- [ ] Unit test stabilization window timers

#### 1.2 Manager State Integration (3 days)
- [ ] Add `StateScaling`, `StateRebalancing`, `StateEmergency` to Manager state transitions
- [ ] Implement `isValidTransition(from, to State)` with validation rules
- [ ] Update `transitionState()` to enforce validation
- [ ] Implement `monitorCalculatorState()` goroutine (leader only):
  - Watch calculator state changes
  - Trigger Manager state transitions
  - Call state change hooks
- [ ] Wire calculator state to Manager state:
  - `calcStateScaling` → `StateScaling`
  - `calcStateRebalancing` → `StateRebalancing`
  - `calcStateEmergency` → `StateEmergency`
  - Assignment published → `StateStable`

**Tests**:
- [ ] Test Manager reflects calculator state correctly
- [ ] Test invalid state transitions are rejected
- [ ] Test `OnStateChanged` hook fires for all states
- [ ] Test leader-only state monitoring (followers don't monitor calculator)

#### 1.3 Integration Tests (4 days)
- [ ] **Cold start scenario** (0 → 3 workers):
  - Verify `StateScaling` entered
  - Verify 30s stabilization window honored
  - Verify transitions: Init → ClaimingID → Election → Scaling → Rebalancing → Stable
  - Verify assignments published after window

- [ ] **Planned scale up** (3 → 5 workers):
  - Verify `StateScaling` entered
  - Verify 10s stabilization window
  - Verify assignments recalculated after window
  - Verify ConsistentHash maintains >80% affinity

- [ ] **Emergency scenario** (kill 1 of 3 workers):
  - Verify `StateEmergency` entered immediately
  - Verify NO stabilization window
  - Verify assignments redistributed immediately
  - Verify remaining workers receive new partitions

- [ ] **Restart detection** (10 → 0 → 10 workers rapidly):
  - Verify detected as cold start (not emergency)
  - Verify 30s window applied
  - Verify assignments stable after restart

- [ ] **State transition validation**:
  - Test invalid transitions rejected
  - Test hook callbacks fire in correct order
  - Test concurrent state changes handled safely

**Deliverable**: State machine fully functional with comprehensive tests proving it works

---

## Phase 2: Leader Election Robustness (HIGH PRIORITY) 🟠

**Status**: ⏳ Not Started
**Priority**: High - Core reliability
**Timeline**: 1 week (5 working days)

### Why Second?
If leader election is broken, the entire system fails. Must verify it's bulletproof.

### Goals
1. Verify leader failover works correctly
2. Verify split-brain prevention
3. Verify assignments survive leader transitions

### Tasks

#### 2.1 Leader Failover Tests (3 days)
- [ ] Test leader crashes, new leader elected within TTL
- [ ] Test leader election during cold start (no previous leader)
- [ ] Test exactly one leader exists at any time
- [ ] Test assignments preserved during leader transition
- [ ] Test calculator starts on new leader
- [ ] Test followers don't start calculator
- [ ] Test KV key expiry and renewal

**Tests**:
- [ ] Kill leader, verify new leader elected within 15s
- [ ] Start 5 workers simultaneously, verify single leader emerges
- [ ] Verify assignments don't duplicate during leader transition
- [ ] Verify workers receive assignments from new leader

#### 2.2 Leadership Edge Cases (2 days)
- [ ] Test rapid leader churn (leader keeps dying repeatedly)
- [ ] Test leader loses NATS connection temporarily
- [ ] Test leader shutdown during scaling window
- [ ] Test leader shutdown during rebalancing
- [ ] Test leader shutdown during emergency rebalance
- [ ] Test followers promoted to leader mid-operation

**Tests**:
- [ ] Kill leader every 5s for 60s, verify system stabilizes
- [ ] Disconnect leader NATS, verify leadership transferred
- [ ] Kill leader during each state, verify new leader resumes operation

**Deliverable**: Confidence that leader election is production-ready

---

## Phase 3: Assignment Correctness (HIGH PRIORITY) 🟠

**Status**: ⏳ Not Started
**Priority**: High - Core functionality
**Timeline**: 1 week (5 working days)

### Why Third?
Correct partition assignment is the whole point of the library.

### Goals
1. Verify all partitions assigned (no orphans)
2. Verify no duplicate assignments
3. Verify strategy behavior (affinity, distribution)

### Tasks

#### 3.1 Assignment Distribution Tests (3 days)
- [ ] Test all partitions assigned (count matches)
- [ ] Test no partition assigned to multiple workers
- [ ] Test partition weights respected
- [ ] Test assignments stable when no topology changes
- [ ] Test `OnAssignmentChanged` hook provides correct partition list
- [ ] Test Assignment() returns consistent results

**Tests**:
- [ ] 100 partitions, 5 workers: verify all assigned, no duplicates
- [ ] Weighted partitions: verify heavier partitions distributed properly
- [ ] No worker changes for 5 minutes: verify assignments unchanged

#### 3.2 Strategy-Specific Tests (2 days)
- [ ] **ConsistentHash**:
  - Verify >80% partition affinity during rebalancing
  - Verify hash ring distribution
  - Verify partition migration minimized

- [ ] **RoundRobin**:
  - Verify even distribution (±1 partition)
  - Verify deterministic assignment
  - Verify partition IDs respected

**Tests**:
- [ ] ConsistentHash: 3→4 workers, verify <20% partitions moved
- [ ] RoundRobin: 100 partitions, 7 workers, verify each gets 14-15 partitions

**Deliverable**: Verified assignment correctness for production use

---

## Phase 4: Reliability & Error Handling (MEDIUM PRIORITY) 🟡

**Status**: ⏳ Not Started
**Priority**: Medium - Production hardening
**Timeline**: 1 week (5 working days)

### Goals
1. Handle NATS failures gracefully
2. Handle concurrent operations safely
3. Graceful shutdown with in-flight work

### Tasks

#### 4.1 Error Scenarios (3 days)
- [ ] NATS connection lost and recovered
- [ ] NATS KV unavailable
- [ ] Heartbeat publish failures
- [ ] Assignment publish failures
- [ ] Concurrent Start() calls
- [ ] Concurrent Stop() calls
- [ ] Stop() during state transitions

**Tests**:
- [ ] Disconnect NATS, verify workers retry
- [ ] KV unavailable, verify graceful degradation
- [ ] Multiple goroutines call Start(), verify safe

#### 4.2 Graceful Shutdown (2 days)
- [ ] Context cancellation propagates correctly
- [ ] All goroutines exit on Stop()
- [ ] No goroutine leaks
- [ ] In-flight operations complete
- [ ] Hooks called during shutdown

**Tests**:
- [ ] Call Stop(), verify all goroutines exit within 5s
- [ ] Stop during rebalancing, verify clean shutdown
- [ ] Check goroutine count before/after Stop()

**Deliverable**: Reliable operation under adverse conditions

---

## Phase 5: Performance Verification (MEDIUM PRIORITY) 🟡

**Status**: ⏳ Not Started
**Priority**: Medium - Performance validation
**Timeline**: 1 week (5 working days)

### Goals
1. Verify assignment calculation performance
2. Verify heartbeat overhead acceptable
3. Verify KV operation latency

### Tasks

#### 5.1 Benchmarks (3 days)
- [ ] Benchmark ConsistentHash assignment (1K, 10K, 100K partitions)
- [ ] Benchmark RoundRobin assignment
- [ ] Benchmark heartbeat publishing
- [ ] Benchmark KV read/write operations
- [ ] Benchmark state transitions

**Targets**:
- 10K partitions, 200 workers: <100ms assignment calculation
- Heartbeat publish: <10ms
- KV operations: <50ms p99

#### 5.2 Load Testing (2 days)
- [ ] 200 workers, 10K partitions
- [ ] Rapid scale up/down (10 workers → 50 → 10 every 30s)
- [ ] Long-running stability (24 hours)

**Deliverable**: Performance characteristics documented

---

## Phase 6: Dynamic Partition Discovery (OPTIONAL) ⚪

**Status**: ⏳ Not Started
**Priority**: Low - Nice to have
**Timeline**: 3-5 days (optional)

### Tasks
- [ ] Test RefreshPartitions() integration
- [ ] Test partition list changes trigger rebalancing
- [ ] Test PartitionSource implementations
- [ ] Document partition source contract

**Deliverable**: Dynamic partition discovery verified (if time permits)

---

## Phase 7: Documentation (LAST) 📚

**Status**: ⏳ Not Started
**Priority**: Low - Only after implementation complete
**Timeline**: 1 week (5 working days)

### Why Last?
**Document what works, not what we hope will work.** Code first, docs later.

### Tasks
- [ ] README enhancement (architecture diagram, quick start)
- [ ] API documentation (godoc examples for all workflows)
- [ ] Deployment guide (NATS setup, Kubernetes examples)
- [ ] Configuration tuning guide (based on benchmark results)
- [ ] Troubleshooting guide (based on integration test scenarios)
- [ ] Migration guide (Kafka → Parti)
- [ ] Examples:
  - [ ] Custom strategy example
  - [ ] Metrics integration example
  - [ ] Kafka consumer replacement example

**Deliverable**: Comprehensive documentation for users

---

## Timeline Summary

| Phase | Focus | Duration | Start | End |
|-------|-------|----------|-------|-----|
| 0 | Foundation Audit | 1 day | ✅ Complete | ✅ Complete |
| 1 | State Machine | 2 weeks | TBD | TBD |
| 2 | Leader Election | 1 week | TBD | TBD |
| 3 | Assignment Correctness | 1 week | TBD | TBD |
| 4 | Reliability | 1 week | TBD | TBD |
| 5 | Performance | 1 week | TBD | TBD |
| 6 | Dynamic Partitions | 3-5 days | Optional | Optional |
| 7 | Documentation | 1 week | TBD | TBD |

**Total**: 6-7 weeks to production-ready

---

## Success Criteria

### Phase 1 Complete When:
- [ ] All 9 states reachable and tested
- [ ] State transitions validated
- [ ] Integration tests prove state machine works
- [ ] No manual testing required

### Phase 2 Complete When:
- [ ] Leader failover tested and working
- [ ] Split-brain prevention verified
- [ ] Assignments survive leader transitions
- [ ] Edge cases handled

### Phase 3 Complete When:
- [ ] No orphaned partitions possible
- [ ] No duplicate assignments possible
- [ ] Strategy behavior verified
- [ ] Assignment correctness proven

### Production Ready When:
- [ ] All integration tests passing
- [ ] All error scenarios handled
- [ ] Performance benchmarks meet targets
- [ ] Documentation complete
- [ ] Ready to replace Kafka consumer groups

---

## Appendix: State Machine Implementation

### Architecture Overview

```
manager.go (Manager) - ALL WORKERS
  ├─ State: atomic.Int32 (9 states)
  ├─ transitionState(newState) - enforces valid transitions
  ├─ isValidTransition(from, to) - validation rules
  ├─ monitorLeadership() - watches for leader changes
  ├─ monitorAssignmentChanges() - watches for assignments
  └─ monitorCalculatorState() - watches calculator (LEADER ONLY)

internal/assignment/calculator.go (Calculator - LEADER ONLY)
  ├─ calcState: atomic.Int32 (Idle, Scaling, Rebalancing, Emergency)
  ├─ monitorWorkerHealth() - detects topology changes
  ├─ detectRebalanceType() - determines cold start / planned / emergency
  ├─ enterScalingState(reason, window) - starts stabilization timer
  ├─ enterRebalancingState() - triggers assignment calculation
  ├─ enterEmergencyState() - immediate rebalance
  ├─ returnToIdleState() - back to stable
  └─ GetState() - exposes calculator state to Manager
```

### State Transitions

#### Manager States (9 total)
1. **StateInit** → StateClaimingID (on Start())
2. **StateClaimingID** → StateElection (after ID claimed)
3. **StateElection** → StateWaitingAssignment (after election complete)
4. **StateWaitingAssignment** → StateStable (after initial assignment)
5. **StateStable** → StateScaling (topology change detected)
6. **StateScaling** → StateRebalancing (stabilization window expired)
7. **StateRebalancing** → StateStable (assignments published)
8. **StateStable** → StateEmergency (worker crashed)
9. **StateEmergency** → StateStable (emergency rebalance complete)
10. **Any** → StateShutdown (on Stop())

#### Calculator States (4 total)
1. **Idle** → Scaling (topology change, cold start/planned scale)
2. **Scaling** → Rebalancing (stabilization window expired)
3. **Rebalancing** → Idle (assignments published)
4. **Idle** → Emergency (worker disappeared)
5. **Emergency** → Idle (emergency rebalance complete)

### Rebalance Type Detection Logic

```go
func (c *Calculator) detectRebalanceType() (reason string, window time.Duration) {
    prevCount := len(c.lastWorkers)
    currCount := len(c.currentWorkers())

    // Emergency: Worker disappeared
    if currCount < prevCount {
        return "emergency", 0 // No window
    }

    // Cold start: Starting from 0
    if prevCount == 0 && currCount >= 1 {
        return "cold_start", 30 * time.Second
    }

    // Restart: >50% workers disappeared and rejoined
    if (prevCount - currCount) > prevCount/2 {
        return "restart", 30 * time.Second
    }

    // Planned scale: Gradual changes
    return "planned_scale", 10 * time.Second
}
```

### State Validation Rules

```go
func (m *Manager) isValidTransition(from, to State) bool {
    validTransitions := map[State][]State{
        StateInit:              {StateClaimingID, StateShutdown},
        StateClaimingID:        {StateElection, StateShutdown},
        StateElection:          {StateWaitingAssignment, StateShutdown},
        StateWaitingAssignment: {StateStable, StateShutdown},
        StateStable:            {StateScaling, StateEmergency, StateShutdown},
        StateScaling:           {StateRebalancing, StateShutdown},
        StateRebalancing:       {StateStable, StateShutdown},
        StateEmergency:         {StateStable, StateShutdown},
        StateShutdown:          {}, // Terminal state
    }

    for _, valid := range validTransitions[from] {
        if valid == to {
            return true
        }
    }
    return false
}

#### 3.3 Scale Tests (1 day)
- [ ] Test scale from 1 → 10 workers (one at a time)
- [ ] Test scale from 10 → 1 workers (one at a time)
- [ ] Test scale from 0 → 10 workers (simultaneous)
- [ ] Measure rebalancing frequency
- [ ] Measure cache affinity maintenance

**Phase 3 Total**: 5 days

**Deliverable**: Proof that assignment logic works correctly

---

### Phase 4: Verify Reliability (HIGH PRIORITY) 🟠

**Why Fourth**: Production reliability is non-negotiable

#### 4.1 Graceful Shutdown Tests (1 day)
- [ ] Test shutdown with active partitions
- [ ] Test shutdown timeout handling
- [ ] Test all goroutines exit
- [ ] Test shutdown prevents new assignments
- [ ] Test context cancellation propagates

#### 4.2 Error Handling Tests (2 days)
- [ ] Test NATS connection loss and recovery
- [ ] Test KV bucket unavailable
- [ ] Test partition source fails
- [ ] Test assignment strategy returns error
- [ ] Test heartbeat publication fails
- [ ] Test worker ID claim fails

#### 4.3 Concurrent Operations Tests (2 days)
- [ ] Test rapid worker join/leave cycles
- [ ] Test multiple workers starting simultaneously
- [ ] Test concurrent shutdown
- [ ] Test leader election during high churn
- [ ] Stress test with 50+ workers

**Phase 4 Total**: 5 days

**Deliverable**: Confidence in error handling and edge cases

---

### Phase 5: Performance Verification (MEDIUM PRIORITY) 🟡

**Why Fifth**: Need to know if performance is acceptable

#### 5.1 Benchmarks (2 days)
- [ ] Assignment calculation latency:
  - 10 workers, 100 partitions
  - 50 workers, 1000 partitions
  - 100 workers, 10000 partitions
- [ ] Leader election latency
- [ ] Rebalancing time vs worker count
- [ ] Memory usage profiling
- [ ] CPU usage profiling

#### 5.2 Load Testing (1 day)
- [ ] Test with 100 workers
- [ ] Test with 10,000 partitions
- [ ] Test with partition churn
- [ ] Test with worker churn
- [ ] Identify bottlenecks

**Phase 5 Total**: 3 days

**Deliverable**: Performance characteristics documented

---

### Phase 6: Dynamic Partition Discovery (MEDIUM PRIORITY) 🟡

**Why Sixth**: Nice to have but not critical for initial use

#### 6.1 RefreshPartitions Implementation (1 day)
- [ ] Implement `RefreshPartitions()` properly
- [ ] Trigger rebalancing on partition changes
- [ ] Add `TriggerRebalance()` to calculator
- [ ] Test partition addition
- [ ] Test partition removal

#### 6.2 Subscription Helper Integration (1 day)
- [ ] Integration test with real NATS subscriptions
- [ ] Test subscription updates during rebalancing
- [ ] Test retry logic
- [ ] Test cleanup on shutdown

**Phase 6 Total**: 2 days

**Deliverable**: Dynamic partition discovery works

---

### Phase 7: Documentation (AFTER IMPLEMENTATION) 📚

**Why Last**: Document what actually works, not what we wish worked

#### 7.1 Core Documentation (2 days)
- [ ] Honest README with actual capabilities
- [ ] API documentation with real examples
- [ ] Architecture diagram reflecting reality
- [ ] Configuration guide with tested values
- [ ] Limitations and known issues

#### 7.2 Examples (2 days)
- [ ] Basic example (already exists, verify it works)
- [ ] Kafka consumer example (if tested)
- [ ] Custom strategy example (if tested)
- [ ] Metrics integration example (if tested)

**Phase 7 Total**: 4 days

**Deliverable**: Accurate documentation

---

## Realistic Timeline

### Sprint 1 (2 weeks): State Machine + Leader Election
- Phase 1: Complete State Machine (5-8 days)
- Phase 2: Leader Election Verification (3 days)
- Buffer: 2 days for unexpected issues

### Sprint 2 (2 weeks): Assignment + Reliability
- Phase 3: Assignment Verification (5 days)
- Phase 4: Reliability Testing (5 days)
- Buffer: 2 days for bug fixes

### Sprint 3 (1 week): Performance + Polish
- Phase 5: Performance Verification (3 days)
- Phase 6: Dynamic Partitions (2 days)

### Sprint 4 (1 week): Documentation
- Phase 7: Documentation (4 days)
- Final review and polish (3 days)

**Total Estimated Time**: 6 weeks

---

## Success Criteria (Be Honest)

### After Sprint 1
- [ ] State machine enters all 9 states correctly
- [ ] State transitions validated and tested
- [ ] Leader election survives leader crashes
- [ ] No split-brain scenarios observed

### After Sprint 2
- [ ] All partitions assigned with no duplicates
- [ ] Rebalancing works without data loss
- [ ] Graceful shutdown completes successfully
- [ ] Error scenarios don't cause panics

### After Sprint 3
- [ ] Performance acceptable for target scale (100 workers)
- [ ] Dynamic partition discovery works
- [ ] Subscription helper integrated and tested

### After Sprint 4
- [ ] Documentation matches reality
- [ ] Examples all work
- [ ] Ready for v0.1.0 release

---

## What We're NOT Doing (Yet)

These are nice-to-haves for future versions:

- ❌ Weighted round-robin strategy
- ❌ Locality-aware strategies
- ❌ Health check HTTP endpoints
- ❌ Grafana dashboards
- ❌ Migration guides from other systems
- ❌ Documentation website
- ❌ Community building
- ❌ Supporting 1000+ workers

Focus on making the core rock-solid first.

---

## Key Principles

1. **Test FIRST, document LATER**
2. **Verify assumptions don't just assume**
3. **Integration tests > unit tests** (for distributed systems)
4. **Be honest about what works**
5. **Focus on correctness before performance**
6. **One feature at a time**
7. **Don't move to next phase until current phase proven**

---

## Current Status (October 26, 2025)

**Completed Today**:
- ✅ NopHooks implementation (eliminated nil checks)
- ✅ State machine implementation plan created
- ✅ Practical plan created (this document)

**Next Action**:
- Start Phase 1.1: Calculator Internal State implementation
- Create tracking issue for state machine implementation
- Set up test infrastructure for state machine tests

**Estimated Completion**: ~6 weeks from now (early December 2025)

---

**This is the honest plan. Now let's execute it.**
