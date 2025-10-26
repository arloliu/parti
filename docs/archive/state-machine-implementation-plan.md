# State Machine Implementation Plan

## Current Status

### Implemented States ✅
1. **StateInit** - Initial state before operations
2. **StateClaimingID** - Worker claiming stable ID
3. **StateElection** - Leader election participation
4. **StateWaitingAssignment** - Waiting for initial partition assignment
5. **StateStable** - Normal operation with stable assignment
6. **StateShutdown** - Graceful shutdown in progress

### Missing States ❌
1. **StateScaling** - Dynamic scaling/stabilization window
2. **StateRebalancing** - Partition rebalancing in progress
3. **StateEmergency** - Emergency rebalancing (worker crash)

## Architecture Overview

### Current Implementation
```
manager.go (Manager)
  ├─ State transitions (transitionState)
  ├─ monitorLeadership() - watches for leader changes
  ├─ monitorAssignmentChanges() - watches for assignment updates
  └─ calculator (leader only)

internal/assignment/calculator.go (Calculator - leader only)
  ├─ monitorWorkerHealth() - detects worker joins/leaves
  ├─ shouldTriggerRebalance() - determines if rebalance needed
  └─ calculateAndPublish() - runs assignment strategy
```

### Required Architecture
```
manager.go (Manager) - ALL WORKERS
  ├─ State management with full state machine
  ├─ State transition validation
  ├─ monitorLeadership() ✅ EXISTS
  ├─ monitorAssignmentChanges() ✅ EXISTS
  └─ NEW: monitorCalculatorState() - watch calculator state changes (leader only)

internal/assignment/calculator.go (Calculator - LEADER ONLY)
  ├─ Internal state tracking (scaling, rebalancing, emergency)
  ├─ monitorWorkerHealth() ✅ EXISTS
  ├─ detectRebalanceType() - NEW: cold start / planned / emergency
  ├─ enterScalingState() - NEW: adaptive stabilization window
  ├─ enterRebalancingState() - NEW: calculate assignments
  ├─ enterEmergencyState() - NEW: immediate rebalance
  └─ publishCalculatorState() - NEW: notify manager of calculator state
```

## Implementation Phases

### Phase 1: Calculator State Tracking (Internal) 🔵

**Goal**: Add internal state tracking to Calculator without changing Manager API

**Changes to `internal/assignment/calculator.go`**:

1. **Add Calculator internal states**:
```go
type calculatorState int

const (
    calcStateIdle calculatorState = iota
    calcStateScaling      // Waiting in stabilization window
    calcStateRebalancing  // Actively calculating assignments
    calcStateEmergency    // Emergency rebalancing
)

type Calculator struct {
    // ... existing fields ...

    // NEW: Internal state tracking
    calcState     atomic.Int32  // calculatorState
    scalingStart  time.Time      // When scaling window started
    scalingReason string          // "cold_start", "planned_scale", "restart"
}
```

2. **Add state detection logic**:
```go
// detectRebalanceType determines what type of rebalance is needed
func (c *Calculator) detectRebalanceType(ctx context.Context) (rebalanceType string, window time.Duration) {
    c.mu.RLock()
    prevWorkerCount := len(c.lastWorkers)
    c.mu.RUnlock()

    currentWorkers := c.getActiveWorkers(ctx)
    currentCount := len(currentWorkers)

    // Emergency: Worker disappeared (missed heartbeats)
    if currentCount < prevWorkerCount {
        for workerID := range c.lastWorkers {
            if !currentWorkers[workerID] {
                c.logger.Warn("worker disappeared", "workerID", workerID)
                return "emergency", 0 // No window
            }
        }
    }

    // Cold start: Starting from 0 workers
    if prevWorkerCount == 0 && currentCount >= 1 {
        c.logger.Info("cold start detected", "workers", currentCount)
        return "cold_start", c.coldStartWindow // 30s
    }

    // Restart detection: Many workers disappeared then rejoined quickly
    if float64(prevWorkerCount-currentCount)/float64(prevWorkerCount) > c.restartRatio {
        c.logger.Info("restart detected", "prev", prevWorkerCount, "current", currentCount)
        return "restart", c.coldStartWindow // 30s
    }

    // Planned scale: Gradual changes
    c.logger.Info("planned scale detected", "prev", prevWorkerCount, "current", currentCount)
    return "planned_scale", c.plannedScaleWin // 10s
}
```

3. **Add state transition methods**:
```go
// enterScalingState initiates stabilization window
func (c *Calculator) enterScalingState(reason string, window time.Duration) {
    c.calcState.Store(int32(calcStateScaling))
    c.scalingStart = time.Now()
    c.scalingReason = reason

    c.logger.Info("entering scaling state",
        "reason", reason,
        "window", window,
    )

    // Start timer for scaling window
    go func() {
        select {
        case <-time.After(window):
            c.enterRebalancingState()
        case <-c.stopCh:
            return
        }
    }()
}

// enterRebalancingState triggers actual rebalancing
func (c *Calculator) enterRebalancingState() {
    c.calcState.Store(int32(calcStateRebalancing))

    c.logger.Info("entering rebalancing state")

    // Trigger actual calculation
    c.triggerCalculation()
}

// enterEmergencyState triggers immediate rebalancing
func (c *Calculator) enterEmergencyState() {
    c.calcState.Store(int32(calcStateEmergency))

    c.logger.Warn("entering emergency state - immediate rebalance")

    // Cancel any pending scaling window
    // Trigger immediate calculation
    c.triggerCalculation()
}

// returnToIdleState after successful rebalance
func (c *Calculator) returnToIdleState() {
    c.calcState.Store(int32(calcStateIdle))
    c.lastRebalance = time.Now()

    c.logger.Info("returned to idle state")
}
```

4. **Modify `monitorWorkerHealth()` to use state machine**:
```go
func (c *Calculator) monitorWorkerHealth() {
    // ... existing setup ...

    for {
        select {
        case <-c.stopCh:
            return
        case <-ticker.C:
            ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

            // Check if rebalance needed
            if c.shouldTriggerRebalance(ctx) {
                // Detect rebalance type
                rebalanceType, window := c.detectRebalanceType(ctx)

                switch rebalanceType {
                case "emergency":
                    c.enterEmergencyState()
                case "cold_start", "restart", "planned_scale":
                    c.enterScalingState(rebalanceType, window)
                }
            }

            cancel()
        }
    }
}
```

**Benefits**:
- Calculator now tracks its own state
- Adaptive window selection works correctly
- No Manager API changes needed yet
- Tests can verify state transitions within Calculator

**Testing**:
- Unit tests for `detectRebalanceType()`
- Unit tests for state transitions
- Integration tests for scaling window timing

---

### Phase 2: Manager State Integration 🟡

**Goal**: Connect Manager states to Calculator internal states

**Changes to `manager.go`**:

1. **Add state transition validation**:
```go
// isValidTransition checks if state transition is allowed
func (m *Manager) isValidTransition(from, to State) bool {
    // Define valid transitions
    validTransitions := map[State][]State{
        StateInit:              {StateClaimingID},
        StateClaimingID:        {StateElection},
        StateElection:          {StateWaitingAssignment},
        StateWaitingAssignment: {StateStable},
        StateStable:            {StateScaling, StateRebalancing, StateEmergency, StateShutdown},
        StateScaling:           {StateRebalancing, StateEmergency, StateShutdown},
        StateRebalancing:       {StateStable, StateEmergency, StateShutdown},
        StateEmergency:         {StateStable, StateShutdown},
        StateShutdown:          {}, // Terminal state
    }

    allowedStates := validTransitions[from]
    for _, allowed := range allowedStates {
        if allowed == to {
            return true
        }
    }

    return false
}

// transitionState with validation
func (m *Manager) transitionState(from, to State) error {
    if !m.isValidTransition(from, to) {
        return fmt.Errorf("invalid state transition: %s -> %s", from, to)
    }

    m.state.Store(int32(to))

    // Trigger state change hook
    if m.hooks != nil && m.hooks.OnStateChanged != nil {
        go func() {
            if err := m.hooks.OnStateChanged(m.ctx, from, to); err != nil {
                m.logError("state change hook error", "from", from, "to", to, "error", err)
            }
        }()
    }

    m.logger.Info("state transition", "from", from, "to", to)
    return nil
}
```

2. **Add calculator state monitoring (leader only)**:
```go
// monitorCalculatorState watches calculator state and updates manager state
func (m *Manager) monitorCalculatorState() {
    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-m.ctx.Done():
            return
        case <-ticker.C:
            if m.calculator == nil {
                continue
            }

            calcState := m.calculator.GetState() // NEW method needed
            managerState := m.State()

            // Map calculator state to manager state
            switch calcState {
            case calcStateScaling:
                if managerState == StateStable {
                    m.transitionState(managerState, StateScaling)
                }
            case calcStateRebalancing:
                if managerState == StateStable || managerState == StateScaling {
                    m.transitionState(managerState, StateRebalancing)
                }
            case calcStateEmergency:
                if managerState != StateEmergency {
                    m.transitionState(managerState, StateEmergency)
                }
            case calcStateIdle:
                if managerState == StateRebalancing || managerState == StateEmergency || managerState == StateScaling {
                    m.transitionState(managerState, StateStable)
                }
            }
        }
    }
}

// Start calculator monitoring in startCalculator()
func (m *Manager) startCalculator(assignmentKV, heartbeatKV jetstream.KeyValue) error {
    // ... existing calculator setup ...

    // Start calculator state monitoring
    m.wg.Add(1)
    go func() {
        defer m.wg.Done()
        m.monitorCalculatorState()
    }()

    return nil
}
```

3. **Add Calculator.GetState() method**:
```go
// In internal/assignment/calculator.go

// GetState returns the current calculator state
func (c *Calculator) GetState() calculatorState {
    return calculatorState(c.calcState.Load())
}

// GetStateString returns human-readable state
func (c *Calculator) GetStateString() string {
    state := c.GetState()
    switch state {
    case calcStateIdle:
        return "Idle"
    case calcStateScaling:
        return fmt.Sprintf("Scaling (%s, %s remaining)",
            c.scalingReason,
            time.Until(c.scalingStart.Add(c.getScalingWindow())))
    case calcStateRebalancing:
        return "Rebalancing"
    case calcStateEmergency:
        return "Emergency"
    default:
        return "Unknown"
    }
}
```

**Benefits**:
- Manager state now reflects actual operational state
- State transitions are validated
- Leader accurately reports StateScaling/StateRebalancing/StateEmergency
- Followers remain in StateStable (they don't run calculator)

**Testing**:
- Integration tests for state transitions during scaling
- Tests for invalid state transitions
- Tests for emergency state handling

---

### Phase 3: Follower State Synchronization 🟢

**Goal**: Ensure followers can observe scaling/rebalancing states

**Problem**: Followers don't run Calculator, so they can't detect StateScaling/StateRebalancing

**Solution Options**:

#### Option A: State Broadcasting (Recommended)
Leader publishes its state to NATS KV, followers watch and mirror it.

```go
// Leader publishes state changes
func (m *Manager) publishManagerState(state State) error {
    stateKey := fmt.Sprintf("%s-state", m.cfg.WorkerIDPrefix)
    stateData := map[string]interface{}{
        "state":     state.String(),
        "workerID":  m.workerID,
        "timestamp": time.Now().Unix(),
    }

    data, _ := json.Marshal(stateData)
    _, err := m.assignmentKV.Put(m.ctx, stateKey, data)
    return err
}

// Followers watch leader state
func (m *Manager) monitorLeaderState() {
    // Watch leader state key
    stateKey := fmt.Sprintf("%s-state", m.cfg.WorkerIDPrefix)
    watcher, _ := m.assignmentKV.Watch(m.ctx, stateKey)
    defer watcher.Stop()

    for {
        select {
        case <-m.ctx.Done():
            return
        case entry := <-watcher.Updates():
            if entry == nil {
                continue
            }

            var stateData map[string]interface{}
            json.Unmarshal(entry.Value(), &stateData)

            leaderState := stateData["state"].(string)
            m.syncToLeaderState(leaderState)
        }
    }
}
```

#### Option B: Inferred State (Simpler)
Followers stay in StateStable but can infer activity through assignment version changes.

**Recommended**: Option A for full state awareness

---

### Phase 4: Testing & Validation ✅

**Unit Tests**:
1. Calculator state transitions
2. Rebalance type detection (cold start, planned, emergency, restart)
3. Stabilization window timing
4. Manager state validation

**Integration Tests**:
1. Cold start: 0 → 3 workers → StateScaling(30s) → StateRebalancing → StateStable
2. Planned scale: 3 → 5 workers → StateScaling(10s) → StateRebalancing → StateStable
3. Emergency: Worker crash → StateEmergency → StateRebalancing → StateStable
4. Restart detection: 10 → 0 → 10 workers → StateScaling(30s) → StateRebalancing → StateStable

**Chaos Testing**:
1. Multiple rapid worker joins/leaves
2. Leader failover during StateScaling
3. Worker crash during StateRebalancing
4. Network partition during emergency

---

## Implementation Checklist

### Phase 1: Calculator Internal State ⏳
- [ ] Add `calculatorState` enum and fields
- [ ] Implement `detectRebalanceType()`
- [ ] Implement `enterScalingState()`
- [ ] Implement `enterRebalancingState()`
- [ ] Implement `enterEmergencyState()`
- [ ] Implement `returnToIdleState()`
- [ ] Modify `monitorWorkerHealth()` to use state machine
- [ ] Add unit tests for state detection
- [ ] Add unit tests for state transitions

### Phase 2: Manager Integration ⏳
- [ ] Add `isValidTransition()`
- [ ] Update `transitionState()` with validation
- [ ] Add `Calculator.GetState()` method
- [ ] Add `Calculator.GetStateString()` method
- [ ] Implement `monitorCalculatorState()`
- [ ] Update `startCalculator()` to start monitoring
- [ ] Add integration tests for manager state sync
- [ ] Add tests for invalid transitions

### Phase 3: Follower Sync ⏳
- [ ] Design state broadcasting protocol
- [ ] Implement `publishManagerState()`
- [ ] Implement `monitorLeaderState()` (followers)
- [ ] Add tests for follower state synchronization
- [ ] Add tests for leader failover during state transitions

### Phase 4: Testing ⏳
- [ ] Write cold start integration test
- [ ] Write planned scale integration test
- [ ] Write emergency integration test
- [ ] Write restart detection test
- [ ] Add chaos tests for edge cases
- [ ] Performance testing for state transitions
- [ ] Documentation updates

---

## Timeline Estimate

- **Phase 1**: 2-3 days (calculator state tracking)
- **Phase 2**: 2-3 days (manager integration)
- **Phase 3**: 1-2 days (follower sync)
- **Phase 4**: 2-3 days (comprehensive testing)

**Total**: 7-11 days for full implementation

---

## Benefits of Full Implementation

1. **Observability**: Users can see exactly what the cluster is doing
2. **Debugging**: State history helps diagnose issues
3. **Monitoring**: Prometheus metrics for each state
4. **Testing**: Easier to write deterministic tests
5. **Documentation**: State machine becomes self-documenting
6. **Hooks**: Users can react to state changes (OnStateChanged)

---

## Risk Mitigation

1. **Backward Compatibility**: New states are additive, existing code still works
2. **Graceful Degradation**: If state sync fails, workers still function
3. **Testing**: Extensive integration tests before release
4. **Documentation**: Clear migration guide for users
5. **Metrics**: Add state transition metrics for production monitoring

---

## Example Usage After Implementation

```go
// User code can now observe full state machine
manager.OnStateChanged(func(ctx context.Context, from, to parti.State) error {
    switch to {
    case parti.StateScaling:
        log.Info("cluster scaling in progress, delaying batch jobs")
    case parti.StateRebalancing:
        log.Info("rebalancing partitions")
    case parti.StateEmergency:
        log.Warn("emergency rebalancing due to worker crash")
        metrics.Inc("parti_emergency_rebalance")
    case parti.StateStable:
        log.Info("cluster stable, resuming normal operations")
    }
    return nil
})
```

---

**Status**: Ready for implementation
**Priority**: Medium (library is functional without this, but adds significant value)
**Complexity**: Medium (requires careful state machine design and testing)
