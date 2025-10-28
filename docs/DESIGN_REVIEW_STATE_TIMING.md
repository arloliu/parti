# Design Review: State Transitions, Stabilization, Cooldown & TTL

**Date**: 2025-10-28
**Status**: Analysis for Discussion
**Goal**: Identify error-prone, ambiguous areas and propose refactoring for maintainability

---

## Executive Summary

**Current Issues Identified**:
1. ⚠️ **Dual State Machines** - Manager and Calculator states are loosely coupled via polling
2. ⚠️ **Cooldown vs Stabilization Window** - Two similar but different timing mechanisms
3. ⚠️ **TTL Confusion** - Three different TTLs with unclear relationships
4. ⚠️ **State Transition Ambiguity** - Manager polls calculator every 200ms for state sync
5. ⚠️ **Emergency Detection Logic** - Complex worker disappearance detection
6. ⚠️ **Test-Only Configuration** - Default cooldown breaks fast tests

---

## 1. Dual State Machine Architecture

### Current Design

```
Manager State (manager.go)           Calculator State (calculator.go)
├─ Init                             ├─ Idle
├─ ClaimingID                       ├─ Scaling (with timer)
├─ Election                         ├─ Rebalancing
├─ WaitingAssignment                └─ Emergency
├─ Stable ◄─────────────────────────┼─ (loosely coupled via polling)
├─ Scaling
├─ Rebalancing
├─ Emergency
└─ Shutdown
```

**Communication**: `Manager.monitorCalculatorState()` polls calculator every 200ms

### Problem Areas

#### 1.1 State Synchronization Lag
```go
// manager.go:771
func (m *Manager) monitorCalculatorState() {
    ticker := time.NewTicker(200 * time.Millisecond) // Poll every 200ms
    // ...
    if calcState == lastCalcState {
        continue // Skip if unchanged
    }
```

**Issues**:
- 200ms polling creates potential lag in state reflection
- What if calculator transitions Scaling→Rebalancing→Idle in <200ms?
- Manager might miss intermediate states entirely
- Race condition: Calculator state can change between read and Manager state update

#### 1.2 Asymmetric State Sets
```
Manager has 9 states
Calculator has 4 states

Mapping ambiguities:
- Manager.Stable can correspond to Calculator.Idle OR Calculator.Scaling (early check)
- Manager.Scaling corresponds to Calculator.Scaling
- Fast rebalancing: Calculator.Scaling→Idle before Manager.monitorCalculatorState() detects it
```

#### 1.3 No Event-Driven Communication
- Manager doesn't "subscribe" to calculator state changes
- No channel-based notification
- Relies on time-based polling which is error-prone

### Error-Prone Scenarios

**Scenario 1: Fast Rebalancing**
```
Time  Calculator State      Manager State      Manager Poll?
0ms   Idle                 Stable             -
10ms  Scaling              Stable             -
110ms Rebalancing          Stable             -
150ms Idle                 Stable             -
200ms Idle                 Stable             ✓ Poll → No change seen!
```
Result: Manager never sees Scaling/Rebalancing, stays in Stable forever

**Scenario 2: State Desynchronization**
```
Calculator: Idle → Scaling(100ms window) → Rebalancing → Idle
Manager:    Stable → (200ms) → Scaling → (200ms) → Stable

Manager transitions lag behind calculator by up to 400ms
```

---

## 2. Cooldown vs Stabilization Window

### Current Implementation

#### 2.1 Rebalance Cooldown
```go
// config.go:132
RebalanceCooldown: 10 * time.Second  // Default: 10s

// calculator.go:821
cooldownActive := time.Since(c.lastRebalance) < c.cooldown

// Blocks ALL rebalancing if cooldown active
if cooldownActive {
    c.logger.Info("rebalance needed but cooldown active")
    return nil
}
```

**Purpose**: Prevent thrashing by enforcing minimum time between rebalances
**Scope**: Blocks checkForChanges() from triggering ANY rebalance
**Updated**: On every `publishAssignments()` completion

#### 2.2 Stabilization Windows
```go
// config.go:90-95
ColdStartWindow:    30 * time.Second  // Default: 30s
PlannedScaleWindow: 10 * time.Second  // Default: 10s

// calculator.go:417-479
func detectRebalanceType(currentWorkers) (reason string, window time.Duration) {
    if currCount < prevCount { return "emergency", 0 }  // No window
    if prevCount == 0       { return "cold_start", coldStartWindow }
    if missingRatio > 0.5   { return "restart", coldStartWindow }
    return "planned_scale", plannedScaleWindow
}
```

**Purpose**: Wait for worker topology to stabilize before calculating assignments
**Scope**: Used ONLY when entering Scaling state
**Applied**: One-time delay before transition to Rebalancing

### Problem Areas

#### 2.3 Semantic Confusion
```
Q: What's the difference between cooldown and stabilization window?
A: Cooldown = "how soon can we rebalance AGAIN?"
   Stabilization = "how long to WAIT before rebalancing?"

But both are timing delays that affect rebalancing!
```

**Confusion Matrix**:
| Scenario | Cooldown Applies? | Stabilization Applies? | Result |
|----------|-------------------|------------------------|--------|
| Cold start (0→3 workers) | ❌ No (lastRebalance=zero) | ✅ Yes (30s window) | Wait 30s, then rebalance |
| Worker join (3→4 workers) | ✅ Yes (if <10s since last) | ✅ Yes (10s window) | BOTH apply! Which wins? |
| Worker crash (4→3 workers) | ❌ Bypassed (emergency) | ❌ No (emergency=0s) | Immediate rebalance |

#### 2.4 Interaction Ambiguity
```go
// What happens if worker-2 joins 5s after initial rebalance?
//
// Timeline:
// T+0s:  Initial rebalance completes (lastRebalance = now)
// T+5s:  Worker-2 joins
//        - Cooldown check: 5s < 10s → BLOCKED
//        - Stabilization: Would use 10s window if allowed
//
// Result: Cooldown blocks state transition, stabilization never evaluated!
```

**Current Behavior**: Cooldown takes precedence, blocking stabilization logic entirely

#### 2.5 Test Configuration Nightmare
```go
// manager_leadership_test.go
// Before fix: Tests timed out at 7-8 seconds
// Root cause: 10s default cooldown blocked state transitions

// Solution: Test-specific override
Assignment: AssignmentConfig{
    RebalanceCooldown: 100 * time.Millisecond,  // 100x faster than default!
}
```

**Problem**: Production defaults break tests, tests use unrealistic timings

---

## 3. TTL Configuration Chaos

### Three Different TTLs

#### 3.1 WorkerIDTTL
```go
WorkerIDTTL: 30 * time.Second  // Default

// Purpose: How long a worker ID claim is valid
// Usage: Stable ID lease in NATS KV
// Renewal: Every TTL/3 (10s)
```

#### 3.2 HeartbeatTTL
```go
HeartbeatTTL: 6 * time.Second  // Default

// Purpose: How long before a heartbeat is considered stale
// Usage: Worker liveness detection
// Renewal: Every HeartbeatInterval (2s)
```

#### 3.3 AssignmentTTL
```go
AssignmentTTL: 0  // Default = no expiration

// Purpose: How long assignments persist in KV
// Usage: Assignment versioning across leader changes
// Renewal: Never (infinite)
```

### Problem Areas

#### 3.4 TTL Relationship Constraints
```
Required relationships (from config comments):
- WorkerIDTTL > HeartbeatInterval (must be 3-5x)
- HeartbeatTTL > HeartbeatInterval (must be 3x)
- But what if WorkerIDTTL < HeartbeatTTL?
```

**Example of Bad Configuration**:
```go
Config{
    WorkerIDTTL:       5 * time.Second,  // Short lease
    HeartbeatInterval: 2 * time.Second,
    HeartbeatTTL:      6 * time.Second,  // Longer than ID lease!
}

// Worker can appear "alive" (heartbeat valid) but lose its ID claim
// Result: Duplicate IDs or assignment confusion
```

#### 3.5 No Validation
```go
// config.go has NO validation that enforces:
// - WorkerIDTTL >= HeartbeatTTL
// - HeartbeatTTL >= 2 * HeartbeatInterval
// - RebalanceCooldown <= PlannedScaleWindow (reasonable constraint)
```

#### 3.6 Polling Interval Derivation
```go
// internal/assignment/calculator.go:643
pollInterval := heartbeatTTL / 2  // 3s if TTL=6s

// But worker detection happens via watcher + polling hybrid
// What if watcher is broken? Polling is fallback with TTL/2 delay
```

**Question**: Should polling interval be configurable separately?

---

## 4. State Transition Validation

### Current Validation

```go
// manager.go:492-527
func (m *Manager) isValidTransition(from, to State) bool {
    validTransitions := map[State][]State{
        StateInit:              {StateClaimingID},
        StateClaimingID:        {StateElection},
        StateElection:          {StateWaitingAssignment},
        StateWaitingAssignment: {StateStable},
        StateStable:            {StateScaling, StateRebalancing, StateEmergency, StateShutdown},
        StateScaling:           {StateRebalancing, StateStable, StateEmergency, StateShutdown},
        StateRebalancing:       {StateStable, StateEmergency, StateShutdown},
        StateEmergency:         {StateStable, StateShutdown},
        StateShutdown:          {},
    }
    // ...
}
```

### Problem Areas

#### 4.1 Scaling→Stable Direct Transition
```go
StateScaling: {StateRebalancing, StateStable, StateEmergency, StateShutdown},
//                                ^^^^^^^^^^^ Added to handle fast rebalancing
```

**Why Needed**: If rebalancing completes in <200ms, Manager might miss Rebalancing state

**Problem**: This is a band-aid fix for polling lag, not proper design

#### 4.2 No Reason Tracking
```go
// When StateStable → StateScaling, WHY?
// - Cold start?
// - Planned scale?
// - Restart detection?
//
// Current: Stored in Calculator.scalingReason (string)
// Manager: Has no visibility into this!
```

#### 4.3 Invalid Transition Logging
```go
// manager.go:460
if !m.isValidTransition(from, to) {
    m.logError("invalid state transition attempted", ...)
    return  // Silently fails!
}
```

**Problem**: Invalid transitions fail silently, no error returned to caller

---

## 5. Emergency Detection Logic

### Current Implementation

```go
// calculator.go:417-436
func detectRebalanceType(currentWorkers map[string]bool) (reason string, window time.Duration) {
    prevCount := len(c.lastWorkers)
    currCount := len(currentWorkers)

    // Emergency: Worker(s) disappeared
    if currCount < prevCount {
        disappeared := make([]string, 0)
        for workerID := range c.lastWorkers {
            if !currentWorkers[workerID] {
                disappeared = append(disappeared, workerID)
            }
        }
        return "emergency", 0  // No stabilization window
    }
    // ...
}
```

### Problem Areas

#### 5.1 Flapping Worker Scenario
```
T+0s:  Workers: [A, B, C] → Stable
T+5s:  Worker C crashes
       detectRebalanceType() sees [A, B]
       → Emergency rebalance (no window)
T+6s:  Worker C rejoins (transient network issue)
       detectRebalanceType() sees [A, B, C]
       → Planned scale (10s window)
T+8s:  Worker C crashes again
       → Emergency rebalance

Result: Thrashing between emergency and planned scale
```

#### 5.2 Partial Heartbeat Visibility
```go
// What if heartbeat watcher sees Worker C's heartbeat, but getActiveWorkers() doesn't?
//
// Cause: NATS KV eventual consistency, network delay, or watch lag
// Effect: False positive "worker disappeared" detection
```

#### 5.3 Restart vs Emergency Ambiguity
```go
// Restart detection (calculator.go:451-467):
missingRatio := float64(missingCount) / float64(prevCount)
if missingRatio > c.restartRatio {  // Default: 0.5
    return "restart", c.coldStartWindow
}

// But what if:
// - 2 of 3 workers crash simultaneously (66% > 50%)
//   → Treated as "restart" with 30s window
//   → Should be emergency! 33% capacity loss!
```

---

## 6. Watcher + Polling Hybrid

### Current Design

```go
// calculator.go:638-786
func (c *Calculator) monitorWorkers() {
    // Start watcher for fast detection
    c.startWatcher(ctx)  // ~50-200ms detection

    // Fallback polling
    pollInterval := heartbeatTTL / 2
    ticker := time.NewTicker(pollInterval)
}
```

**Idea**: Watcher for speed, polling for reliability

### Problem Areas

#### 6.1 Debounce Interaction
```go
// calculator.go:735-751 (watcher event processing)
case entry := <-watcher.Updates():
    debounceTimer.Reset(100 * time.Millisecond)  // 100ms debounce

// Problem: Rapid events reset timer, delaying check indefinitely
//
// Scenario:
// T+0ms:   Worker A heartbeat → Reset timer to 100ms
// T+50ms:  Worker B heartbeat → Reset timer to 100ms (from 50ms)
// T+100ms: Worker C heartbeat → Reset timer to 100ms (from 50ms)
// ...
// If heartbeats every 50ms, debounce never fires!
```

#### 6.2 Dual Triggering Risk
```go
// What if watcher and polling both detect the same change?
//
// T+0ms:  Worker D joins
// T+10ms: Watcher detects, calls checkForChanges()
// T+50ms: Polling detects, calls checkForChanges()
//
// Protection: cooldown mechanism
// But adds 10s delay to any subsequent change!
```

---

## Proposed Refactoring Plan

### Phase 1: Unify State Communication

**Goal**: Replace polling with event-driven state updates

#### Changes:
1. Add state change channel to Calculator
   ```go
   type Calculator struct {
       stateChangeChan chan types.CalculatorState
   }
   ```

2. Emit state changes instead of polling
   ```go
   func (c *Calculator) enterScalingState(...) {
       c.calcState.Store(int32(types.CalcStateScaling))
       select {
       case c.stateChangeChan <- types.CalcStateScaling:
       default: // Non-blocking
       }
   }
   ```

3. Manager subscribes to channel
   ```go
   func (m *Manager) monitorCalculatorState() {
       for {
           select {
           case calcState := <-m.calculator.StateChanges():
               m.syncStateFromCalculator(calcState)
           case <-m.ctx.Done():
               return
           }
       }
   }
   ```

**Benefits**:
- ✅ Zero-lag state synchronization
- ✅ No missed intermediate states
- ✅ No race conditions from polling

**Risks**:
- Channel buffering strategy needed
- What if Manager can't keep up?

---

### Phase 2: Consolidate Timing Controls

**Goal**: Clarify cooldown vs stabilization, reduce configuration complexity

#### Proposed: Three-Tier Timing Model

```go
type TimingConfig struct {
    // Tier 1: Detection Speed (how fast we notice changes)
    WatcherDebounce   time.Duration  // 100ms - batch rapid events
    PollingInterval   time.Duration  // 3s - fallback detection

    // Tier 2: Stabilization (how long to wait before acting)
    ColdStartWindow   time.Duration  // 30s - all workers joining
    PlannedScaleWindow time.Duration  // 10s - gradual additions
    EmergencyWindow   time.Duration  // 0s - immediate action

    // Tier 3: Rate Limiting (how often we can rebalance)
    MinRebalanceInterval time.Duration  // 10s - renamed from RebalanceCooldown
}
```

**Semantic Clarity**:
- **Detection**: How fast do we notice?
- **Stabilization**: How long do we wait?
- **Rate Limiting**: How often can we act?

#### Rule Consolidation:
```go
// New rule: MinRebalanceInterval applies AFTER stabilization completes
//
// Old behavior:
//   Worker joins → Cooldown check (blocks) OR Stabilization (waits)
//
// New behavior:
//   Worker joins → Stabilization window (always applies)
//              → Then: MinRebalanceInterval since last rebalance
```

**Example Timeline**:
```
T+0s:  Initial rebalance completes
       lastRebalance = now

T+5s:  Worker joins
       - Check: 5s < 10s MinRebalanceInterval? YES
       - Action: Defer to T+10s (cooldown expires)
       - NO stabilization window yet!

T+10s: Cooldown expires
       - Enter Scaling state
       - Start 10s PlannedScaleWindow

T+20s: Stabilization complete
       - Transition to Rebalancing
       - Calculate and publish assignments

T+25s: Another worker joins
       - Check: 5s < 10s MinRebalanceInterval? YES
       - Action: Defer to T+30s
```

---

### Phase 3: TTL Validation & Documentation

**Goal**: Enforce TTL relationships, clarify purposes

#### Add Configuration Validation:
```go
func (cfg *Config) Validate() error {
    // Rule 1: WorkerIDTTL must be significantly longer than HeartbeatInterval
    if cfg.WorkerIDTTL < 3 * cfg.HeartbeatInterval {
        return fmt.Errorf("WorkerIDTTL (%s) must be at least 3x HeartbeatInterval (%s)",
            cfg.WorkerIDTTL, cfg.HeartbeatInterval)
    }

    // Rule 2: HeartbeatTTL must be longer than 2x HeartbeatInterval
    if cfg.HeartbeatTTL < 2 * cfg.HeartbeatInterval {
        return fmt.Errorf("HeartbeatTTL (%s) must be at least 2x HeartbeatInterval (%s)",
            cfg.HeartbeatTTL, cfg.HeartbeatInterval)
    }

    // Rule 3: WorkerIDTTL should be >= HeartbeatTTL
    if cfg.WorkerIDTTL < cfg.HeartbeatTTL {
        return fmt.Errorf("WorkerIDTTL (%s) should be >= HeartbeatTTL (%s)",
            cfg.WorkerIDTTL, cfg.HeartbeatTTL)
    }

    // Rule 4: MinRebalanceInterval should be reasonable
    if cfg.Assignment.MinRebalanceInterval > cfg.ColdStartWindow {
        return fmt.Errorf("MinRebalanceInterval (%s) should not exceed ColdStartWindow (%s)",
            cfg.Assignment.MinRebalanceInterval, cfg.ColdStartWindow)
    }

    return nil
}
```

#### TTL Documentation Matrix:
```go
// TTL Purpose Guide
// ================
//
// 1. WorkerIDTTL (Default: 30s)
//    Purpose: Stable worker identity lease
//    Renewal: Every TTL/3 (~10s)
//    Expiry Impact: Worker loses ID, must re-claim (disruption)
//    Recommendation: 3-5x HeartbeatInterval
//
// 2. HeartbeatTTL (Default: 6s)
//    Purpose: Worker liveness detection
//    Renewal: Every HeartbeatInterval (2s)
//    Expiry Impact: Worker considered dead → Emergency rebalance
//    Recommendation: 3x HeartbeatInterval
//
// 3. AssignmentTTL (Default: 0 = infinite)
//    Purpose: Assignment persistence across leader changes
//    Renewal: Never
//    Expiry Impact: Lost assignment history → Version reset
//    Recommendation: 0 (infinite) or very long (1h+)
//
// Constraint Hierarchy:
//   WorkerIDTTL >= HeartbeatTTL >= 2 * HeartbeatInterval
```

---

### Phase 4: Emergency Detection Improvements

**Goal**: Reduce false positives, handle edge cases

#### Proposed: Hysteresis-Based Detection

```go
type EmergencyDetector struct {
    disappearedWorkers map[string]time.Time  // When each worker disappeared
    gracePeriod        time.Duration          // Wait before emergency (e.g., 2s)
}

func (d *EmergencyDetector) DetectEmergency(prev, curr map[string]bool) (emergency bool, workers []string) {
    now := time.Now()

    // Find disappeared workers
    for workerID := range prev {
        if !curr[workerID] {
            // Track when first seen as disappeared
            if _, exists := d.disappearedWorkers[workerID]; !exists {
                d.disappearedWorkers[workerID] = now
            }
        } else {
            // Worker reappeared, clear tracking
            delete(d.disappearedWorkers, workerID)
        }
    }

    // Check if any workers exceeded grace period
    confirmed := make([]string, 0)
    for workerID, firstSeen := range d.disappearedWorkers {
        if now.Sub(firstSeen) >= d.gracePeriod {
            confirmed = append(confirmed, workerID)
        }
    }

    return len(confirmed) > 0, confirmed
}
```

**Benefits**:
- ✅ Prevents flapping detection (2s grace period)
- ✅ Distinguishes transient issues from real crashes
- ✅ Tracks per-worker disappearance

**Trade-off**: Emergency rebalance delayed by grace period (2s)

---

### Phase 5: Test Configuration Separation

**Goal**: Separate test timings from production defaults

#### Proposed: Test-Friendly Defaults

```go
// config.go
func DefaultConfig() Config {
    return productionDefaults()
}

func TestConfig() Config {
    cfg := productionDefaults()
    // Test-specific overrides
    cfg.Assignment.MinRebalanceInterval = 100 * time.Millisecond
    cfg.ColdStartWindow = 1 * time.Second
    cfg.PlannedScaleWindow = 500 * time.Millisecond
    cfg.HeartbeatInterval = 500 * time.Millisecond
    cfg.HeartbeatTTL = 1500 * time.Millisecond
    return cfg
}

func productionDefaults() Config {
    // Current defaults remain for production
    // ...
}
```

**Usage**:
```go
// test files
cfg := parti.TestConfig()  // Fast timings

// production
cfg := parti.DefaultConfig()  // Safe timings
```

---

## Discussion Questions

### 1. State Communication
**Q1**: Should we replace polling with channels, or is polling "good enough"?
**Trade-off**: Channels = complexity vs Polling = simplicity but lag

Absolutely replace polling with channels. Polling is not "good enough" because it's fundamentally unreliable for state synchronization in a distributed system. It can miss fast state changes, introduces significant lag, and forces the implementation of brittle workarounds. The complexity of implementing channels is a small price to pay for the immense gain in correctness and reliability.

**Q2**: If using channels, should they be buffered? How large?
**Risk**: Unbuffered = blocking, Buffered = potential loss if full

Use a small buffer (e.g., size 1 or 2).
- **Unbuffered (size 0)**: This would cause the `Calculator` to block until the `Manager` is ready to receive. This could be problematic if the `Manager` is busy, potentially stalling the `Calculator`.

- **Buffered (size 1)**: This is a good compromise. The `Calculator` can emit a state change without blocking, and if another state change happens before the `Manager` has processed the first one, the `Manager` will simply pick up the latest state on its next read. This provides a good balance of responsiveness and decoupling.

- **Large Buffer**: A large buffer is unnecessary and could hide problems where the `Manager` is falling behind, while also consuming more memory.

### 2. Timing Consolidation
**Q3**: Is the three-tier model (Detection/Stabilization/RateLimit) clearer?
**Alternative**: Keep current cooldown + stabilization as-is

Yes, the three-tier model is significantly clearer. It separates concerns logically:
1.  **Detection**: How fast we *see* a change.
2.  **Stabilization**: How long we *wait* before acting on the change.
3.  **Rate Limiting**: How often we are *allowed* to act.
This mental model is much easier to reason about than the current ambiguous interaction between cooldown and stabilization.

**Q4**: Should MinRebalanceInterval apply BEFORE or AFTER stabilization?
**Current**: BEFORE (blocks state transition)
**Proposed**: AFTER (allows stabilization, then checks interval)

It should apply **BEFORE** the stabilization window begins, but in a non-blocking way. The proposed timeline in the review document is slightly off. A better flow would be:
1.  **Worker change detected.**
2.  **Check `MinRebalanceInterval`**.
    - If the interval is active, **schedule a check** to run after the interval expires. Do not block. Simply ignore the trigger for now. This prevents the system from even entering a `Scaling` state, which is correct.
    - If the interval is not active, proceed to step 3.
3.  **Enter `Scaling` state** and start the appropriate stabilization window (`PlannedScaleWindow`, etc.).
4.  After the window expires, transition to `Rebalancing`.

This ensures rate-limiting takes precedence, preventing system thrashing, while stabilization is only used for valid, spaced-out scaling events.

### 3. TTL Validation
**Q5**: Should Config.Validate() be called automatically in NewManager()?
**Pro**: Fail-fast on bad config
**Con**: Breaking change for existing users

Yes, absolutely.

**Q6**: Should we add a "recommended" vs "valid" validation level?
**Example**: WorkerIDTTL < HeartbeatTTL is valid but not recommended

Start with strict "valid" checks first. For example, `HeartbeatTTL < HeartbeatInterval` is fundamentally invalid and should always be an error.

- You can add "recommended" checks as warnings logged by the logger. For example, `WorkerIDTTL < HeartbeatTTL` might be a valid but risky configuration. This could be logged as a `WARN` level message without returning an error, giving the operator a chance to correct it without breaking the application.

```go
// In Validate()
if cfg.WorkerIDTTL < cfg.HeartbeatTTL {
    // This is a hard failure
    return fmt.Errorf("WorkerIDTTL (%v) must be >= HeartbeatTTL (%v)", ...)
}
if cfg.WorkerIDTTL < 2 * cfg.HeartbeatTTL {
    // This is a soft failure (warning)
    logger.Warn("Configuration warning: WorkerIDTTL is less than the recommended 2x HeartbeatTTL")
}
```

### 4. Emergency Detection
**Q7**: Is 2s grace period reasonable for hysteresis?
**Too short**: False positives from network blips
**Too long**: Delayed response to real crashes

 A 2-second grace period seems like a reasonable starting point, especially since the default `HeartbeatInterval` is 2 seconds. This grace period should be configurable. A good default would be `1.5 * HeartbeatInterval`. This allows for one missed heartbeat without triggering an immediate emergency, which is a common pattern for handling transient network issues.

**Q8**: Should restart detection (missingRatio > 0.5) be removed?
**Argument**: If >50% workers disappear, it's emergency regardless of "restart"

Yes, the current `restart` detection logic is ambiguous and potentially dangerous. If >50% of your capacity disappears, that is an emergency that requires immediate attention, not a 30-second stabilization window.
- **Simplify the logic**:
    - `currCount < prevCount` -> **Emergency**. Use the new hysteresis detector to confirm.
    - `prevCount == 0` -> **Cold Start**.
    - `currCount > prevCount` -> **Planned Scale**.

This is much clearer and safer.


### 5. Implementation Strategy
**Q9**: Should we do big-bang refactor or incremental phases?
**Big-bang**: Clean slate but high risk
**Incremental**: Safe but prolonged migration

Incremental phases are strongly recommended. The proposed 5-phase plan is perfect for this. It allows for delivering value and stability improvements incrementally while minimizing risk. A big-bang refactor would be complex and difficult to test.

**Q10**: Which phase should we tackle first?
**Vote**: Phase 1 (State) | Phase 2 (Timing) | Phase 3 (TTL) | Phase 4 (Emergency) | Phase 5 (Test)

1.  **Phase 3 (TTL Validation) & Phase 5 (Test Config)**: These are the "low-hanging fruit". They are easy to implement, have low risk, and provide immediate value by making the system safer to configure and easier to test. They can likely be done in a single step.
2.  **Phase 1 (State Communication)**: This is the highest impact change. It fixes the core architectural flaw and will simplify subsequent refactoring. This should be the first major task after the quick wins.
3.  **Phase 4 (Emergency Detection)**: Improving emergency detection provides a significant boost to stability.
4.  **Phase 2 (Timing Consolidation)**: This is a larger refactoring that benefits from the unified state model of Phase 1. It should be done after the state communication is solid.


---

## Summary of Error-Prone Areas

### Critical Issues (Fix Soon)
1. 🔴 **State Synchronization Lag** - Polling every 200ms can miss fast transitions
2. 🔴 **Cooldown Blocks Stabilization** - Incorrect precedence order
3. 🔴 **No TTL Validation** - Bad configs accepted silently
4. 🔴 **Emergency Flapping** - No hysteresis for transient failures

### Medium Issues (Fix Later)
5. 🟡 **Test Config Nightmare** - Tests need 100ms cooldown vs 10s production
6. 🟡 **Semantic Confusion** - "Cooldown" vs "Stabilization" unclear
7. 🟡 **Invalid Transition Failures** - Silent failures on bad transitions

### Low Priority (Nice to Have)
8. 🟢 **State Reason Tracking** - Manager can't see "why" Scaling state entered
9. 🟢 **Dual Triggering Risk** - Watcher + Polling can both trigger (mitigated by cooldown)

---

## Recommended Action Items

### Immediate (This Week)
1. ✅ Add Config.Validate() method (Phase 3)
2. ✅ Create TestConfig() helper (Phase 5)
3. ✅ Document TTL constraints clearly (Phase 3)

### Short-term (Next Sprint)
4. ⏳ Replace state polling with channels (Phase 1)
5. ⏳ Implement emergency hysteresis (Phase 4)
6. ⏳ Rename RebalanceCooldown → MinRebalanceInterval (Phase 2)

### Long-term (Next Quarter)
7. 📅 Full timing model consolidation (Phase 2)
8. 📅 State transition audit trail (track reasons)
9. 📅 Performance benchmarking of new state model

---

**Next Steps**: Discuss questions, prioritize phases, create implementation PRs
