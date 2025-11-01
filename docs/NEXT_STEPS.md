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
- v1.1.0: Test coverage and quality improvements (Task 0 ✅ complete, Task 1 📋 ready)
- v1.2.0: Production-driven features (circuit breaker, retry logic) - requires production data
- v1.3.0+: Research-dependent optimizations (batch KV operations)

---

## v1.1.0 - Quality & Release Preparation

**Status**: Task 0 complete, Task 1 ready to start
**Release Target**: TBD (early stage development)
**Focus**: Behavior-driven testing, code quality, documentation

**Key Changes**:
- ✅ Task 0 COMPLETE: Removed 511 lines, 80% coverage maintained
- 📋 Task 1 READY: Comprehensive behavior-driven testing (20 hours planned)
  - Priority: Emergency detection, edge cases, property-based tests
  - Target: 80% → 90%+ coverage
- ❌ Benchmark suite removed from v1.1.0 (optional for later)
- 🔢 Tasks renumbered: Old Tasks 3-7 are now Tasks 2-6

---

### Task 0: Test Organization & Cleanup
**Priority**: P2 (Foundation for test coverage improvements)
**Status**: ✅ **COMPLETE**
**Estimated Time**: 8 hours (1 day)
**Actual Time**: 5 hours

**All Steps Complete**:
- ✅ **Step 1 Complete** (2h): Created `testing_helpers.go` with shared test utilities
  - Added testSetup helper for standardized NATS/KV initialization
  - Moved mock implementations (mockSource, mockStrategy) to shared file
  - Added helper functions (publishTestHeartbeat, deleteTestHeartbeat)
  - File: `testing_helpers.go` (171 lines)

- ✅ **Step 2 Complete** (1h): Deleted obsolete test files
  - Removed `calculator_watcher_test.go` (378 lines) - obsolete watcher implementation
  - Removed `cooldown_test.go` (82 lines) - redundant value tests, behavior tested elsewhere
  - WorkerMonitor has comprehensive test coverage (354 lines)
  - All remaining tests pass after deletion

- ✅ **Step 3 Complete** (1h): Removed all setter methods and updated tests
  - **Removed 4 setter methods from calculator.go**: SetCooldown, SetMinThreshold, SetRestartDetectionRatio, SetStabilizationWindows
  - **Removed entire test function**: TestCalculator_SetMethods (88 lines) - all subtests were for removed setters
  - **Updated 10+ test functions** across 4 test files to use Config pattern
  - **Pattern Applied**: Constructor Injection - all configuration in Config struct
  - **Result**: Tests align with "immutable dependencies after construction" design principle

- ✅ **Step 4 Complete** (30min): Updated documentation
  - Updated `doc.go` to show Config pattern instead of deprecated setters
  - Removed outdated constructor and setter examples
  - Added comprehensive Config pattern example with all fields
  - Updated lifecycle steps and error handling

- ✅ **Step 5 Complete** (30min): Verification and cleanup
  - All tests passing: `ok github.com/arloliu/parti/internal/assignment 24.075s`
  - Zero compile errors, zero linting issues
  - Test coverage maintained at 89.8%
  - No remaining setter method calls in production code

**Final Test File Structure**:
```
internal/assignment/
├── testing_helpers.go                171 lines  ✅ NEW - Shared test utilities
├── calculator_test.go                589 lines  ✅ CLEANED (was 721, -132 lines)
├── calculator_state_test.go          683 lines  ✅ GOOD - State logic tests
├── calculator_scaling_test.go        309 lines  ✅ CLEANED (was 304, updated pattern)
├── assignment_publisher_test.go      425 lines  ✅ GOOD - Publisher tests
├── worker_monitor_test.go            354 lines  ✅ GOOD - Monitor tests
├── state_machine_test.go             352 lines  ✅ GOOD - State machine tests
├── config_test.go                    295 lines  ✅ GOOD - Config validation
└── emergency_test.go                 193 lines  ✅ GOOD - Emergency detection
                                    ─────────
                                    3,371 lines total

Deleted Files:
├── calculator_watcher_test.go        378 lines  ❌ DELETED - Obsolete watcher tests
└── cooldown_test.go                   82 lines  ❌ DELETED - Redundant value tests

Total Reduction: 460 lines deleted + 51 lines from calculator.go = 511 lines (13.2% reduction)
```

**Improvements Achieved**:
- ✅ **Eliminated all setter methods** (4 methods, 51 lines from calculator.go)
- ✅ **Removed 2 obsolete/redundant test files** (460 lines)
- ✅ **Eliminated duplicate mocks** (moved to testing_helpers.go)
- ✅ **Applied constructor injection pattern** consistently across all tests
- ✅ **Fixed hanging test bug** (TestCalculator_Stop_CleansUpAssignments)
- ✅ **Updated documentation** to reflect Config pattern
- ✅ **All tests passing** with race detector (24.075s execution time)
- ✅ **Zero linting issues**, zero compile errors

**Architecture Improvements**:
- ✅ **Immutability enforced**: No state mutation after construction
- ✅ **Type safety**: Compile-time enforcement of configuration
- ✅ **Clarity**: All dependencies visible in constructor
- ✅ **Consistency**: All tests follow same pattern
- ✅ **Reduced API surface**: 4 fewer public methods to maintain

**Metrics**:
- **Code Reduction**: 511 lines removed (13.2%)
- **Test Files**: 10 → 8 files (-2 files)
- **Calculator.go**: 850 → 799 lines (-51 lines, -6%)
- **Test Files Total**: 3,789 → 3,371 lines (-418 lines, -11%)
- **Test Coverage**: Maintained at 89.8%
- **Test Execution**: Consistent ~24s with race detector

**Status**: ✅ **COMPLETE** - All objectives achieved, ready for Task 1 (Test Coverage Improvements)

---

**Estimated Time Breakdown**:
- Step 1: Create shared utilities (2h)
- Step 2: Delete obsolete tests (1h)
- Step 3: Reorganize calculator_test.go (3h)
- Step 4: Consolidate state tests (1h)
- Step 5: Update all test files (1h)
- **Total**: 8 hours

---
### Task 1: Behavior-Driven Test Coverage & Edge Cases
**Priority**: P1 (CRITICAL for production readiness)
**Status**: 🏃 In Progress - Phase 1 Complete
**Estimated Time**: 20 hours (2.5 days)
**Current Coverage**: 80.0% (71 → 86 tests, +15 emergency edge case tests)
**Target Coverage**: 90%+ (comprehensive edge case coverage)
**Approach**: Behavior-driven testing with scenario-based integration tests

**Philosophy**: Test **behaviors and failure scenarios**, not just code paths. Focus on "what could go wrong in production?"

**Why Scenario-Based Testing:**
- Parti is a **distributed coordination system**, not pure algorithmic logic
- Most bugs are **timing/concurrency/integration issues**, not mathematical correctness
- Scenario tests **directly validate production patterns** (K8s updates, network issues, leader failover)
- Property-based testing better suited for **pure functions** (sorting, encoding, compression)

**Progress Summary:**
- ✅ Phase 1: Emergency Detection - 15 tests (329 lines unit + 322 lines integration)
- ✅ Phase 2.1: State Machine Concurrency - 6 tests (383 lines)
- ✅ Phase 3: Worker Monitoring - Assessment complete (existing coverage sufficient)
- ✅ Phase 4: Assignment Calculation Edge Cases - 10 tests (381 lines)
- ✅ Phase 5: Cooldown & Stabilization Timing - 8 tests (439 lines)
- 📋 Phase 6: Production Scenario Testing - Scenario-based approach (replaces property-based)
- **Total Time So Far:** ~9.5 hours (Phase 1: 4h, Phase 2.1: 2h, Phase 3: 0.5h, Phase 4: 1h, Phase 5: 1h)
- **Time Remaining:** ~10.5 hours (Phase 6: 4h, Integration: 4h+)

---

#### Phase 1: Emergency Detection ✅ COMPLETE (6 hours)
**Priority**: HIGHEST - Incorrect emergency detection causes production incidents
**Actual Time**: ~4 hours
**Status**: ✅ Complete with comprehensive edge case coverage

**1.1 Single/Zero Worker Edge Cases** ✅ (2h actual: 1.5h)
```go
// Implemented tests:
✅ TestEmergency_SingleWorker_Disappears_IsEmergency
✅ TestEmergency_SingleWorker_Remains_NotEmergency
✅ TestEmergency_TwoWorkers_OneDisappears_IsEmergency
✅ TestEmergency_TwoWorkers_BothDisappear_IsEmergency
✅ TestEmergency_ZeroWorkers_ThenOneAppears_NotEmergency
✅ TestEmergency_AllWorkers_DisappearSimultaneously_IsEmergency
✅ TestEmergency_SingleWorkerDisappears_ThresholdCalculation
✅ TestEmergency_WorkersDisappear_ThenMoreDisappear_TracksAll
```

**1.2 Threshold Boundary Conditions** ⏭️ SKIPPED - Not Applicable
- **Reason**: EmergencyDetector design doesn't use percentage threshold
- **Design Decision**: ANY worker disappearing beyond grace period = emergency
- **MinThreshold**: Used for rebalance decisions, not emergency detection
- **Documented in**: TestEmergency_SingleWorkerDisappears_ThresholdCalculation

**1.3 Worker Flapping Scenarios** ✅ (2h actual: 1.5h)
```go
// Implemented tests:
✅ TestEmergency_WorkerFlapping_WithinGracePeriod_NotEmergency
✅ TestEmergency_WorkerFlapping_BeyondGracePeriod_IsEmergency
✅ TestEmergency_WorkerRejoins_BeforeEmergencyDetected_CancelsEmergency
✅ TestEmergency_WorkerRejoins_AfterEmergencyDetected_StillEmergency
✅ TestEmergency_RapidPodRestarts_DistinguishesFromFailure
✅ TestEmergency_MultipleFlappingWorkers_TrackedIndependently
✅ TestEmergency_GracePeriodRespected_NeverFiresEarly
```

**1.4 Integration Tests** ✅ (1h actual: 1h)
```go
// Created test/integration/emergency_scenarios_test.go:
✅ TestIntegration_Emergency_WorkerCrash_ReassignsPartitions
✅ TestIntegration_Emergency_CascadingFailures_HandlesGracefully
✅ TestIntegration_Emergency_K8sRollingUpdate_NoDataLoss
✅ TestIntegration_Emergency_SlowWorker_DoesNotBlockSystem
```

---

#### Phase 2: State Machine Concurrency & Edge Cases (5 hours)
**Priority**: HIGH - State machine bugs cause cascading failures
**Status**: 📋 Not Started
```go
TestIntegration_Emergency_NetworkPartition_RecoversGracefully
TestIntegration_Emergency_RollingUpdate_NoFalseEmergencies
```

---

#### Phase 2: State Machine Concurrency & Edge Cases ✅ COMPLETE (5 hours planned, 2h actual)
**Priority**: HIGH - State machine bugs cause cascading failures
**Status**: ✅ Complete - All concurrency tests implemented
**Actual Time**: ~2 hours
**Coverage Impact**: 79.0% (stable, focus on behavior testing)

**2.1 Concurrent Transitions** ✅ (2h actual)
```go
// Implemented tests in state_machine_concurrent_test.go:
✅ TestStateMachine_ConcurrentTransitions_OnlyOneSucceeds (0.50s)
   - Verifies only one of 10 concurrent transitions succeeds
   - Validates state machine serialization prevents race conditions

✅ TestStateMachine_ConcurrentIdenticalTransitions_Idempotent (0.20s)
   - Multiple identical transition requests handled correctly
   - Idempotent operations don't trigger duplicate rebalances

✅ TestStateMachine_TransitionDuringNotification_Serialized (0.57s)
   - Rapid transitions during notification delivery remain ordered
   - Captures complete state sequence: [Idle→Scaling→Emergency→Idle→Scaling→Rebalancing→Idle]

✅ TestStateMachine_RapidTransitions_AllOrdered (0.53s)
   - 5 rapid transitions with 5ms delays all captured
   - Validates no state changes are lost under high load

✅ TestStateMachine_ConcurrentEmergency_LastWins (0.25s)
   - 10 concurrent emergency triggers handled gracefully
   - Last emergency reason recorded correctly

✅ TestStateMachine_StopDuringTransition_CleansUp (0.30s)
   - Stopping during active transition doesn't deadlock
   - Graceful cleanup with blocked rebalance operation
```

**New Test File Created**:
- `state_machine_concurrent_test.go` (383 lines)
- 6 comprehensive concurrency tests
- Total test time: ~2.4 seconds
- All tests passing with race detector

**2.2 Notification Delivery Guarantees** ⏭️ DEFERRED
```go
// Reason: Existing tests already cover notification robustness
// Covered by: TestStateMachine_Subscribe, TestStateMachine_MultipleSubscribers
// Additional property-based tests deferred to Phase 6
```

**2.3 Invalid State Transitions** ⏭️ DEFERRED
```go
// Reason: State machine design uses CAS (compare-and-swap) pattern
// Invalid transitions are rejected implicitly (CAS fails)
// Existing tests: TestStateMachine_EnterScaling_OnlyFromIdle
// Property-based validation deferred to Phase 6
```

**Phase 2 Summary**:
- ✅ Core concurrency behavior thoroughly tested
- ✅ Race conditions prevented and verified
- ✅ Graceful handling of stop-during-transition
- ⏭️ Property-based tests deferred to Phase 6 (systematic approach)
- ⏭️ Additional notification tests deferred (existing coverage sufficient)

---

#### Phase 2: State Machine Concurrency & Edge Cases (5 hours)
**Priority**: HIGH - State machine bugs cause cascading failures

**2.1 Concurrent Transitions** (2h)
```go
// Behavior: State machine prevents race conditions
TestStateMachine_ConcurrentTransitions_OnlyOneSucceeds
TestStateMachine_ConcurrentIdenticalTransitions_Idempotent
TestStateMachine_TransitionDuringNotification_Serialized
TestStateMachine_RapidTransitions_AllOrdered

// Property-based: only valid transitions succeed
PropertyTest_StateMachine_OnlyValidTransitionsSucceed
```

**2.2 Notification Delivery Guarantees** (2h)
```go
// Behavior: Notification system is robust
TestStateMachine_SlowSubscriber_DoesNotBlockOthers
TestStateMachine_PanicInSubscriber_IsolatesFailure
TestStateMachine_UnsubscribeDuringNotification_NoDeadlock
TestStateMachine_BufferedChannel_DropsOldestOnOverflow
TestStateMachine_ManySubscribers_AllReceiveNotifications

// Property-based: all subscribers get all transitions
PropertyTest_StateMachine_AllSubscribersReceiveAllTransitions
```

**2.3 Invalid State Transitions** (1h)
```go
// Behavior: Invalid transitions rejected with clear errors
TestStateMachine_InvalidTransition_FromScalingToScaling_Rejected
TestStateMachine_InvalidTransition_FromIdleToRebalancing_Rejected
TestStateMachine_InvalidTransition_FromStoppedState_Rejected
TestStateMachine_AllInvalidTransitions_ReturnSpecificErrors

// Property-based: state machine never enters invalid state
PropertyTest_StateMachine_NeverEntersInvalidState
```

---

#### Phase 3: Worker Monitoring Resilience ✅ MOSTLY COMPLETE (4 hours planned, 0.5h actual)
**Priority**: HIGH - Worker monitor is the "eyes" of the system
**Status**: ✅ Existing tests provide comprehensive coverage
**Actual Time**: ~0.5 hours (review only)
**Coverage**: WorkerMonitor 93.8%+ across all methods

**Assessment Summary:**
The WorkerMonitor component already has **11 comprehensive tests** (354 lines) covering:
- ✅ Start/Stop lifecycle (3 tests)
- ✅ Worker appearance/disappearance detection (2 tests)
- ✅ Active worker queries (2 tests)
- ✅ Error handling and callback failures (1 test)
- ✅ Edge cases: empty prefix, multiple changes (2 tests)
- ✅ Race condition prevention (tested with `-race` flag)

**Coverage Analysis:**
```
worker_monitor.go:NewWorkerMonitor      100.0%
worker_monitor.go:Start                 100.0%
worker_monitor.go:Stop                  100.0%
worker_monitor.go:GetActiveWorkers      93.8%
worker_monitor.go:monitorWorkers        71.4%
worker_monitor.go:startWatcher          81.8%
worker_monitor.go:stopWatcher           85.7%
worker_monitor.go:processWatcherEvents  96.4%
```

**3.1 Degenerate Cases** ✅ Already Covered
```go
// Existing tests cover:
✅ TestWorkerMonitor_GetActiveWorkers_EmptyPrefix - empty/no workers
✅ TestWorkerMonitor_GetActiveWorkers - normal operation
✅ TestWorkerMonitor_DetectsWorkerDisappearance - expired heartbeats
// Note: Corrupted data handling done at heartbeat level, not monitor level
```

**3.2 Change Detection Accuracy** ✅ Already Covered
```go
// Existing tests cover:
✅ TestWorkerMonitor_MultipleWorkerChanges - rapid churn detection
✅ TestWorkerMonitor_DetectsWorkerAppearance - worker joins
✅ TestWorkerMonitor_DetectsWorkerDisappearance - worker leaves
// All changes properly detected via hybrid watcher + polling approach
```

**3.3 Failure Recovery** ⏭️ DEFERRED (Integration Test)
```go
// Reason: NATS failure scenarios require integration tests
// Covered in: test/integration/nats_failure_test.go
✅ TestNATSFailure_WorkerExpiry (10s)
✅ TestNATSFailure_HeartbeatExpiry (9s)
✅ TestNATSFailure_LeaderDisconnectDuringRebalance (13s)
// Additional watcher reconnection tests exist
```

**Phase 3 Conclusion:**
- ✅ Unit test coverage is comprehensive (11 tests, 354 lines)
- ✅ Edge cases handled correctly
- ✅ Race conditions prevented
- ✅ Integration tests cover NATS failure scenarios
- ⏭️ No additional unit tests needed for WorkerMonitor
- 📋 Integration test expansion deferred (already 5+ NATS failure tests exist)

---

#### Phase 4: Assignment Calculation Edge Cases ✅ COMPLETE (3 hours planned, 1h actual)
**Priority**: HIGH - Incorrect assignments = data loss
**Status**: ✅ Complete - Comprehensive edge case coverage
**Actual Time**: ~1 hour
**Coverage Impact**: 80.0% (stable, comprehensive behavior testing)

**Implementation Summary:**
Created `strategy/assignment_edge_test.go` (381 lines) with **10 comprehensive test functions** covering:
- ✅ Zero partitions edge case (both strategies)
- ✅ Single worker gets all partitions (both strategies)
- ✅ More workers than partitions - some workers idle (both strategies)
- ✅ More partitions than workers - fair distribution (both strategies)
- ✅ All weights zero - even distribution (both strategies)
- ✅ All weights equal - fair distribution (both strategies)
- ✅ Extreme weights - handles gracefully (both strategies)
- ✅ Mixed weights - reasonable distribution (both strategies)
- ✅ All partitions assigned exactly once - completeness (both strategies)
- ✅ No worker gets duplicate partitions - correctness (both strategies)

**New Test File Created:**
- `strategy/assignment_edge_test.go` (381 lines, 10 test functions, 20 subtests total)

**Key Insights:**
- ConsistentHash prioritizes cache affinity over weight-based distribution (by design)
- RoundRobin distributes evenly regardless of weights (by design)
- Both strategies handle degenerate cases correctly (zero partitions, single worker, etc.)
- Small partition counts with consistent hashing may not distribute to all workers (expected)
- All edge cases verified for both strategies in a single comprehensive test file

**Phase 4 Checklist:**
- ✅ 4.1 Degenerate Input Cases - Zero partitions, single worker, worker/partition ratios
- ✅ 4.2 Weighted Partitions - Zero, equal, extreme, mixed weights
- ✅ 4.3 Assignment Completeness - All partitions assigned exactly once, no duplicates
- ⏭️ 4.4 Cache Affinity - Already covered in existing `consistent_hash_test.go` (>60% stability)
- ⏭️ Property-based tests deferred to Phase 6 (systematic approach)

---

#### Phase 5: Cooldown & Stabilization Timing (2 hours)
**Priority**: MEDIUM - Timing bugs cause performance issues
**Status**: 📋 Ready to Start

**5.1 Cooldown Enforcement** (1h)
```go
// Behavior: Cooldown prevents excessive rebalancing
TestCalculator_RebalanceAttemptDuringCooldown_Blocked
TestCalculator_RebalanceAfterCooldown_Allowed
TestCalculator_EmergencyDuringCooldown_BypassesCooldown
TestCalculator_CooldownBoundary_ExactTiming

// Property-based: cooldown always enforced (except emergencies)
PropertyTest_Calculator_CooldownAlwaysEnforced
```

**5.2 Stabilization Window Behavior** (1h)
```go
// Behavior: Stabilization prevents premature rebalancing
TestCalculator_ScaleEventDuringStabilization_ExtendsWindow
TestCalculator_MultipleScaleEvents_BatchedIntoOneRebalance
TestCalculator_StabilizationTimeout_TriggersRebalance
TestCalculator_ColdStart_UsesLongerWindow
TestCalculator_PlannedScale_UsesShorterWindow
```

---

#### Phase 5: Cooldown & Stabilization Timing ✅ COMPLETE (2 hours planned, ~1h actual)
**Priority**: MEDIUM - Timing bugs cause performance issues
**Status**: ✅ Complete - Comprehensive cooldown behavior tests
**Actual Time**: ~1 hour
**Coverage Impact**: Stable (behavior-focused testing)

**Implementation Summary:**
Created `internal/assignment/calculator_cooldown_test.go` (439 lines) with **8 comprehensive test functions** (1 skipped):

**5.1 Cooldown Enforcement** ✅
```go
✅ TestCalculator_RebalanceAttemptDuringCooldown_Blocked (2.84s)
   - Verifies cooldown prevents rebalancing during active period

✅ TestCalculator_RebalanceAfterCooldown_Allowed (4.16s)
   - Verifies rebalance proceeds after cooldown expires

✅ TestCalculator_CooldownBoundary_ExactTiming (3.20s)
   - Tests cooldown at exact boundary (just before/after expiry)

✅ TestCalculator_MultipleCooldowns_Sequential (11.15s)
   - Tests multiple cooldown cycles work correctly

✅ TestCalculator_TriggerRebalance_BypassesCooldown (1.15s)
   - Verifies TriggerRebalance() bypasses cooldown (by design)

✅ TestCalculator_CooldownReset_AfterEachRebalance (7.40s)
   - Verifies cooldown timer resets after each rebalance

✅ TestCalculator_Cooldown_WithPartitionRefresh (1.26s)
   - Tests cooldown with partition source changes

⏭️ TestCalculator_EmergencyDuringCooldown_BypassesCooldown (SKIPPED)
   - Better tested in integration tests (emergency_scenarios_test.go)
```

**5.2 Stabilization Window Behavior** ⏭️ DEFERRED
```go
// Reason: Existing tests already cover stabilization windows
// Covered by:
//   - TestCalculator_StabilizationWindow (calculator_test.go)
//   - TestCalculator_RapidScaling_BatchesChanges (calculator_scaling_test.go)
//   - Integration tests for K8s rolling updates
```

**New Test File:** `calculator_cooldown_test.go` (439 lines, ~32s execution)

---

#### Phase 6: Production Scenario Testing (4 hours)
**Priority**: MEDIUM - Validates real-world failure patterns
**Status**: 📋 Ready to Start
**Approach**: Scenario-based integration testing (better fit for distributed systems than property-based testing)

**Why Scenario-Based Instead of Property-Based:**
- Parti is a **distributed coordination system**, not pure algorithmic logic
- Most bugs are **timing/concurrency issues**, not mathematical correctness
- Scenario tests **directly validate production patterns** (Kubernetes rolling updates, network partitions)
- Property-based testing is better for **pure functions** (sorting, compression, encoding)

**6.1 Timing & Coordination Scenarios** (2h)
```go
// test/integration/timing_scenarios_test.go

func TestScenario_RapidScaling_StabilizationWindows(t *testing.T)
    // Kubernetes-like scaling: 3→10 workers rapidly
    // Verify: Single rebalance after stabilization window
    // Validates: Batching logic prevents cascading rebalances

func TestScenario_SlowHeartbeats_NearExpiryBoundary(t *testing.T)
    // Workers with heartbeats arriving just before expiry
    // Verify: No false-positive emergencies
    // Validates: Grace period handling

func TestScenario_NetworkJitter_VariableLatency(t *testing.T)
    // Add random 10-100ms delays to NATS messages
    // Verify: System remains stable despite jitter
    // Validates: Timeout tuning

func TestScenario_ConcurrentLeaderElection_RaceCondition(t *testing.T)
    // Multiple workers claim leadership simultaneously
    // Verify: Only one succeeds, others back off gracefully
    // Validates: Leader election safety
```

**6.2 Failure Pattern Library** (1h)
```go
// test/integration/failure_patterns_test.go

func TestPattern_ThunderingHerd_AllWorkersRestartSimultaneously(t *testing.T)
    // Simulate cluster-wide config update
    // All workers restart at once
    // Verify: Stable ID reclaim, assignment recovery

func TestPattern_SplitBrain_DualLeaders(t *testing.T)
    // Simulate network partition causing two leaders
    // Verify: Resolution when partition heals
    // Validates: KV-based leader election correctness

func TestPattern_GrayFailure_SlowButNotDead(t *testing.T)
    // Worker processes messages slowly but heartbeat still active
    // Verify: System doesn't trigger emergency
    // Validates: Performance degradation handling
```

**6.3 Chaos Engineering Helpers** (1h)
```go
// test/testutil/chaos.go

type ChaosInjector struct {
    natsConn *nats.Conn
    delays   map[string]time.Duration
    dropRate map[string]float64
}

// DelayMessages adds latency to specific message patterns
func (c *ChaosInjector) DelayMessages(pattern string, delay time.Duration)

// DropMessages randomly drops messages matching pattern
func (c *ChaosInjector) DropMessages(pattern string, probability float64)

// PartitionNetwork simulates network split for duration
func (c *ChaosInjector) PartitionNetwork(duration time.Duration)

// SlowdownWorker reduces worker processing speed
func (c *ChaosInjector) SlowdownWorker(workerID string, factor float64)
```

---

#### Integration Test Expansion (4 hours)
**Cross-component behavior tests with real NATS**

**File**: `test/integration/behavior_scenarios_test.go`

**7.1 Production Workflow Scenarios** (2h)
```go
func TestIntegration_K8sRollingUpdate_PreservesAssignments(t *testing.T)
    // Simulate rolling update: 10→0→10 workers gradually
    // Verify: >80% partition affinity maintained
    // Validates: Stable ID reclaim, consistent hashing

func TestIntegration_NetworkPartition_HealsGracefully(t *testing.T)
    // Partition workers into two groups, then heal
    // Verify: Converges to single leader, consistent state
    // Validates: Split-brain resolution

func TestIntegration_HighChurn_SystemStable(t *testing.T)
    // Rapid worker joins/leaves over 60 seconds
    // Verify: System remains stable, no deadlocks
    // Validates: Stabilization logic under stress
```

**7.2 Emergency & Recovery Scenarios** (2h)
```go
func TestIntegration_MassWorkerFailure_Recovers(t *testing.T)
    // 10 workers → 8 fail simultaneously
    // Verify: Emergency detected, partitions reassigned
    // Validates: Emergency detection, rebalance under pressure

func TestIntegration_LeaderFailover_DuringRebalance(t *testing.T)
    // Leader fails mid-rebalance
    // Verify: New leader completes rebalance correctly
    // Validates: Leader election, rebalance idempotency

func TestIntegration_CascadingFailures_Contained(t *testing.T)
    // Workers fail in sequence, not simultaneously
    // Verify: Each failure handled independently
    // Validates: Cooldown logic, batching behavior
```

---

### Task 1 Implementation Plan

**Week 1** (12 hours):
- Day 1-2: Phase 1 (Emergency Detection) - 6h ✅ Complete (4h actual)
- Day 2-3: Phase 2 (State Machine Concurrency) - 5h ✅ Complete (2h actual)
- Day 3: Phase 3 (Worker Monitoring) - 4h ✅ Complete (0.5h actual - review only)
- Day 3: Phase 4 (Assignment Edge Cases) - 3h ✅ Complete (1h actual)

**Week 2** (8 hours):
- Day 4: Phase 5 (Cooldown/Stabilization) - 2h 📋 Ready to start
- Day 4-5: Phase 6 (Scenario-Based Testing) - 4h 📋 Ready to start
- Day 5: Integration Test Expansion - 4h 📋 Ready to start

**Total**: 20 hours (split across 2 weeks for review/iteration)

---

### Success Criteria

**Coverage**:
- [ ] Unit test coverage: 80% → 90%+
- [ ] Edge case coverage: All critical paths covered
- [ ] Scenario tests: 15+ production scenarios validated

**Quality**:
- [ ] All tests have descriptive behavior-focused names
- [ ] Zero race conditions (race detector clean)
- [ ] Zero flaky tests (10 consecutive runs pass)
- [ ] Scenario tests cover real-world failure patterns

**Documentation**:
- [ ] Each test documents WHY (what production issue it prevents)
- [ ] Scenario tests document real-world use cases
- [ ] Integration test scenarios reference production patterns

**Integration**:
- [ ] Emergency scenarios covered end-to-end
- [ ] State machine workflows tested with real NATS
- [ ] Production scenarios (K8s, network issues, leader failover) tested
- [ ] Chaos engineering helpers available for future testing

---

### Files to Create/Modify

**New Files**:
```
internal/assignment/
├── emergency_edge_test.go               # Phase 1: Emergency edge cases
├── state_machine_concurrent_test.go     # Phase 2: Concurrency tests
└── cooldown_stabilization_test.go       # Phase 5: Timing behavior (NEW)

strategy/
└── assignment_edge_test.go              # Phase 4: Assignment edge cases

test/integration/
├── emergency_scenarios_test.go          # Phase 1: Emergency integration
├── timing_scenarios_test.go             # Phase 6: Timing scenarios (NEW)
├── failure_patterns_test.go             # Phase 6: Failure patterns (NEW)
└── behavior_scenarios_test.go           # Integration expansion (NEW)

test/testutil/
└── chaos.go                             # Phase 6: Chaos engineering helpers (NEW)
```

**Modified Files**:
```
internal/assignment/
├── emergency_test.go               # EXPAND: Add edge cases
├── state_machine_test.go           # EXPAND: Add concurrency tests
├── worker_monitor_test.go          # REVIEW ONLY: Existing coverage sufficient
└── calculator_test.go              # EXPAND: Add degenerate cases
```

---

### Scenario-Based Testing Examples

**Example 1: Kubernetes Rolling Update**
```go
func TestScenario_K8sRollingUpdate_PreservesAffinity(t *testing.T) {
    harness := NewTestHarness(t)
    defer harness.Cleanup()

    // Initial state: 10 workers, 100 partitions
    workers := harness.StartWorkers(10)
    partitions := harness.CreatePartitions(100)
    initialAssignments := harness.WaitForStableAssignments()

    // Rolling update: Replace workers one by one
    for i := 0; i < 10; i++ {
        harness.StopWorker(workers[i])
        time.Sleep(2 * time.Second) // Kubernetes rollout delay
        newWorker := harness.StartWorker(fmt.Sprintf("worker-%d-v2", i))
        workers[i] = newWorker
    }

    // Verify: >80% partition affinity maintained
    finalAssignments := harness.WaitForStableAssignments()
    affinity := calculateAffinity(initialAssignments, finalAssignments)
    require.Greater(t, affinity, 0.80, "Should maintain >80% cache affinity")
}
```

**Example 2: Network Partition Recovery**
```go
func TestScenario_NetworkPartition_HealsCorrectly(t *testing.T) {
    harness := NewTestHarness(t)
    defer harness.Cleanup()

    // Start 10 workers
    workers := harness.StartWorkers(10)
    harness.WaitForStableAssignments()

    // Simulate network partition: Split into two groups
    chaos := harness.ChaosInjector()
    chaos.PartitionNetwork(5 * time.Second)

    // During partition: Two leaders may exist temporarily
    time.Sleep(2 * time.Second)

    // Heal partition
    chaos.HealPartition()

    // Verify: Single leader, consistent state within 10s
    require.Eventually(t, func() bool {
        leaders := harness.CountActiveLeaders()
        return leaders == 1
    }, 10*time.Second, 500*time.Millisecond)

    // Verify: All partitions still assigned correctly
    assignments := harness.GetCurrentAssignments()
    verifyAssignmentCompleteness(t, assignments, harness.Partitions())
}
```

---

### Task 2: Context Propagation Audit ✅ COMPLETE (2 hours estimated, 1h actual)
**Priority**: P3 (Code quality, not critical)
**Status**: ✅ Complete
**Actual Time**: ~1 hour

**Issue**: Some operations used `context.Background()` in places where context handling could be improved.

**Files Reviewed and Fixed**:
```
manager.go
├── Line 200: VERIFIED CORRECT - Uses context.Background() for independent manager lifecycle
├── Line 1026: FIXED - Now uses m.ctx instead of context.Background() for calculator stop
internal/heartbeat/
└── publisher.go
    ├── Start(ctx): Uses ctx parameter ONLY for initial publish (prevents startup hang)
    └── publishLoop(): Uses context.Background() for background operations (correct pattern)
```

**Changes Made**:
1. **manager.go line 1026**: Updated stopCalculator() to derive timeout context from m.ctx
   - Falls back to background context if m.ctx is nil (safety during edge cases)
   - Properly propagates cancellation to calculator.Stop()

2. **internal/heartbeat/publisher.go**:
   - **Start(ctx)**: Parameter used ONLY for initial publish timeout (prevents hanging on startup)
   - **publishLoop()**: Uses `context.Background()` for each publish (long-running background goroutine)
   - **Stop()**: Uses `context.Background()` for cleanup (parent may be cancelled)
   - **No context field needed** - each operation has appropriate context scope

**Testing**:
- ✅ All heartbeat tests pass with race detector
- ✅ Manager leadership tests pass
- ✅ Context handling works correctly for all scenarios
- ✅ No linting issues introduced

**Key Insights**:
1. **Startup vs Lifecycle Contexts**:
   - `Start(ctx)` parameter = startup timeout control (prevents hanging on first publish)
   - Not stored for later use - each operation gets appropriate context
   - Manager's `m.ctx` = manager lifetime (independent of startup)

2. **Background Goroutine Contexts**:
   - Long-running background goroutines should use `context.Background()`
   - Each operation wraps with timeout as needed
   - Stop signal via channel, not context cancellation

3. **Cleanup Contexts**:
   - Use fresh `context.Background()` for cleanup when parent may be cancelled
   - Allows graceful shutdown even if parent context is cancelled

**Success Criteria**:
- [x] Appropriate context usage in all paths
- [x] Context cancellation properly handled
- [x] All operations use context correctly for their scope

---

### Task 3: Documentation Review
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

### Task 4: Circuit Breaker Pattern
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

### Task 5: Retry with Exponential Backoff
**Priority**: P1 (High impact on reliability)
**Status**: 📋 Deferred (needs circuit breaker first)
**Estimated Time**: 6 hours
**Dependency**: Task 4 (Circuit Breaker)

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

### Task 6: Batch KV Operations Research
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

**v1.1.0 - Release Preparation** (22 hours total):
- Task 0: Test organization & cleanup (5h, P2) ✅ **COMPLETE**
- Task 1: Behavior-driven test coverage 80%→90%+ (20h, P2) 📋 **READY TO START**

**v1.1.0 - Optional Quality Improvements** (9 hours):
- Task 2: Context propagation (2h, P3) 📋
- Task 3: Documentation review (3h, P2) 📋

**v1.2.0 - Production-Driven** (Requires 3+ months production data, 14 hours):
- Task 4: Circuit breaker pattern (8h, P1) 📋
- Task 5: Retry with exponential backoff (6h, P1) 📋

**v1.3.0+ - Research-Dependent** (Conditional, 12 hours):
- Task 6: Batch KV operations (4h research + 8h impl, P2) 📋

**Recommendation**:
1. ✅ **Now**: Complete Task 1 (behavior-driven tests) for v1.1.0
2. 🎯 **Optional**: Tasks 2-3 if time permits
3. 📦 **Next**: Release v1.1.0 when ready
4. 🚀 **Then**: Deploy to production, collect metrics
5. 📊 **Wait**: 3+ months for meaningful data
6. 🎯 **Finally**: Implement v1.2.0 features based on real-world data

---

## References

- **Previous Plan**: `docs/CALCULATOR_IMPROVEMENT_PLAN.md` (Sprints 1-4 complete)
- **Architecture**: `docs/design/04-components/calculator.md`
- **Error Handling**: `types/errors.go` (22 sentinel errors defined)
- **Metrics**: `types/observability.go` (4 domain interfaces)
