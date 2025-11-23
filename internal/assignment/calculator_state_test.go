package assignment

import (
	"context"
	"sync"
	"testing"
	"time"

	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// stateTransitionCollector is a helper for collecting state transitions in tests.
//
// This helper solves the common problem of observing intermediate states in
// asynchronous state machines. Instead of polling for a specific state (which
// might miss fast transitions), it subscribes to ALL state changes and collects
// them for verification.
//
// Usage:
//
//	collector := newStateTransitionCollector(t, stateMachine)
//	defer collector.Stop()
//
//	// Trigger state transitions
//	stateMachine.EnterScaling(ctx, "test", 50*time.Millisecond)
//
//	// Wait for expected final state
//	collector.WaitForState(types.CalcStateIdle, 2*time.Second)
//
//	// Verify the complete transition sequence
//	collector.RequireContains(types.CalcStateScaling)
//	collector.RequireContains(types.CalcStateRebalancing)
//	collector.RequireLastState(types.CalcStateIdle)
type stateTransitionCollector struct {
	t       *testing.T
	states  []types.CalculatorState
	mu      sync.Mutex
	done    chan struct{}
	stateCh <-chan types.CalculatorState
	unsub   func()
}

// newStateTransitionCollector creates a new state transition collector.
//
// It subscribes to the state machine's notifications and starts collecting
// all state changes in a background goroutine.
//
// Parameters:
//   - t: Test instance
//   - sm: StateMachine to observe
//
// Returns:
//   - *stateTransitionCollector: Collector instance (call Stop() when done)
func newStateTransitionCollector(t *testing.T, sm *StateMachine) *stateTransitionCollector {
	t.Helper()

	stateCh, unsub := sm.Subscribe()

	collector := &stateTransitionCollector{
		t:       t,
		states:  make([]types.CalculatorState, 0, 8),
		done:    make(chan struct{}),
		stateCh: stateCh,
		unsub:   unsub,
	}

	// Start collecting states in background
	go collector.collect()

	return collector
}

// collect runs in a goroutine to gather all state transitions.
func (c *stateTransitionCollector) collect() {
	for {
		select {
		case state, ok := <-c.stateCh:
			if !ok {
				return
			}
			c.mu.Lock()
			c.states = append(c.states, state)
			c.mu.Unlock()
		case <-c.done:
			return
		}
	}
}

// Stop stops the collector and unsubscribes from state changes.
func (c *stateTransitionCollector) Stop() {
	c.t.Helper()
	close(c.done)
	c.unsub()
}

// WaitForState waits for the state machine to reach a specific state.
//
// This is useful for waiting until a state transition sequence completes.
//
// Parameters:
//   - targetState: State to wait for
//   - timeout: Maximum time to wait
//
// Returns:
//   - bool: true if state was reached, false if timeout
func (c *stateTransitionCollector) WaitForState(targetState types.CalculatorState, timeout time.Duration) bool {
	c.t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.states) > 0 && c.states[len(c.states)-1] == targetState {
			c.mu.Unlock()
			return true
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}

	return false
}

// GetStates returns a copy of all collected states.
func (c *stateTransitionCollector) GetStates() []types.CalculatorState {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	result := make([]types.CalculatorState, len(c.states))
	copy(result, c.states)

	return result
}

// RequireContains asserts that the given state appears in the transition history.
func (c *stateTransitionCollector) RequireContains(state types.CalculatorState) {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	require.Contains(c.t, c.states, state,
		"State %s not found in transition history: %v",
		state, c.states)
}

// RequireLastState asserts that the last observed state matches the expected state.
func (c *stateTransitionCollector) RequireLastState(expected types.CalculatorState) {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	require.NotEmpty(c.t, c.states, "No states collected")
	require.Equal(c.t, expected, c.states[len(c.states)-1],
		"Last state mismatch. Full history: %v", c.states)
}

// RequireMinimumStates asserts that at least N states were observed.
func (c *stateTransitionCollector) RequireMinimumStates(count int) {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	require.GreaterOrEqual(c.t, len(c.states), count,
		"Expected at least %d states, got %d: %v",
		count, len(c.states), c.states)
}

func TestCalculator_detectRebalanceType_ColdStart(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-coldstart-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-coldstart-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "test-assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "test-hb",
		HeartbeatTTL:         10 * time.Second,
		EmergencyGracePeriod: 5 * time.Second,
	})
	require.NoError(t, err)

	// Cold start: 0 → 3 workers
	lastWorkers := map[string]bool{} // Empty - cold start
	currentWorkers := map[string]bool{
		"worker-0": true,
		"worker-1": true,
		"worker-2": true,
	}

	reason, window := calc.detectRebalanceType(lastWorkers, currentWorkers)

	require.Equal(t, "cold_start", reason)
	require.Equal(t, calc.ColdStartWindow, window)
	require.Equal(t, 30*time.Second, window)
}

func TestCalculator_detectRebalanceType_PlannedScale(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-plannedscale-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-plannedscale-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "test-assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "test-hb",
		HeartbeatTTL:         10 * time.Second,
		EmergencyGracePeriod: 5 * time.Second,
	})
	require.NoError(t, err)

	// Set up previous workers
	lastWorkers := map[string]bool{
		"worker-0": true,
		"worker-1": true,
		"worker-2": true,
	}

	// Planned scale: 3 → 4 workers (gradual addition)
	currentWorkers := map[string]bool{
		"worker-0": true,
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
	}

	reason, window := calc.detectRebalanceType(lastWorkers, currentWorkers)

	require.Equal(t, "planned_scale", reason)
	require.Equal(t, calc.PlannedScaleWindow, window)
	require.Equal(t, 10*time.Second, window)
}

func TestCalculator_detectRebalanceType_Emergency(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-emergency-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-emergency-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	// Use shorter intervals for faster test: HeartbeatInterval=1s, GracePeriod=1s (instead of default 0.75*interval)
	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "test-assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "test-hb",
		HeartbeatTTL:         1 * time.Second,
		EmergencyGracePeriod: 1 * time.Second,
	})
	require.NoError(t, err)

	// Set up previous workers
	lastWorkers := map[string]bool{
		"worker-0": true,
		"worker-1": true,
		"worker-2": true,
	}

	// Emergency: 3 → 2 workers (worker-2 crashed)
	currentWorkers := map[string]bool{
		"worker-0": true,
		"worker-1": true,
	}

	// First check - should return empty (grace period)
	reason, window := calc.detectRebalanceType(lastWorkers, currentWorkers)
	require.Equal(t, "", reason, "Should be in grace period initially")
	require.Equal(t, time.Duration(0), window)

	// Wait for grace period to expire (grace period = 1s, wait 1.1s to be safe)
	time.Sleep(1100 * time.Millisecond)

	// Second check - should now be emergency
	reason, window = calc.detectRebalanceType(lastWorkers, currentWorkers)
	require.Equal(t, "emergency", reason)
	require.Equal(t, time.Duration(0), window)
}

func TestCalculator_detectRebalanceType_Restart(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-restart-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-restart-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "test-assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "test-hb",
		HeartbeatTTL:         10 * time.Second,
		EmergencyGracePeriod: 5 * time.Second,
		RestartRatio:         0.5,
	})
	require.NoError(t, err)

	// Set up previous workers (10 workers)
	lastWorkers := map[string]bool{
		"worker-0": true,
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
		"worker-4": true,
		"worker-5": true,
		"worker-6": true,
		"worker-7": true,
		"worker-8": true,
		"worker-9": true,
	}

	// Worker set changed: 10 → 10 workers but 6 workers different (60% turnover)
	// Since worker count stayed same, treated as planned_scale
	currentWorkers := map[string]bool{
		"worker-0":  true,
		"worker-1":  true,
		"worker-2":  true,
		"worker-3":  true,
		"worker-10": true, // New
		"worker-11": true, // New
		"worker-12": true, // New
		"worker-13": true, // New
		"worker-14": true, // New
		"worker-15": true, // New
	}

	reason, window := calc.detectRebalanceType(lastWorkers, currentWorkers)

	require.Equal(t, "planned_scale", reason)
	require.Equal(t, calc.PlannedScaleWindow, window)
	require.Equal(t, 10*time.Second, window)
}

func TestCalculator_detectRebalanceType_MultipleWorkersCrashed(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-multicrash-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-multicrash-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	// Use shorter intervals for faster test: HeartbeatInterval=1s, GracePeriod=1s (instead of default 0.75*interval)
	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "test-assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "test-hb",
		HeartbeatTTL:         1 * time.Second,
		EmergencyGracePeriod: 1 * time.Second,
	})
	require.NoError(t, err)

	// Set up previous workers
	lastWorkers := map[string]bool{
		"worker-0": true,
		"worker-1": true,
		"worker-2": true,
		"worker-3": true,
		"worker-4": true,
	}

	// Emergency: 5 → 2 workers (3 workers crashed)
	currentWorkers := map[string]bool{
		"worker-0": true,
		"worker-1": true,
	}

	// First check - should return empty (grace period)
	reason, window := calc.detectRebalanceType(lastWorkers, currentWorkers)
	require.Equal(t, "", reason, "Should be in grace period initially")
	require.Equal(t, time.Duration(0), window)

	// Wait for grace period to expire (grace period = 1s, wait 1.1s to be safe)
	time.Sleep(1100 * time.Millisecond)

	// Second check - should now be emergency
	reason, window = calc.detectRebalanceType(lastWorkers, currentWorkers)
	require.Equal(t, "emergency", reason)
	require.Equal(t, time.Duration(0), window)
}

func TestCalculator_StateTransitions_GetState(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-getstate-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-getstate-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "test-assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "test-hb",
		HeartbeatTTL:         10 * time.Second,
		EmergencyGracePeriod: 5 * time.Second,
	})
	require.NoError(t, err)

	// Initial state should be Idle
	require.Equal(t, types.CalcStateIdle, calc.GetState())

	// Use collector to observe all transitions
	collector := newStateTransitionCollector(t, calc.stateMach)
	defer collector.Stop()

	// Transition to Scaling
	ctx := context.Background()
	calc.enterScalingState(ctx, "cold_start", 50*time.Millisecond)

	// First wait until we actually observe Scaling (to avoid matching the initial Idle)
	require.True(t, collector.WaitForState(types.CalcStateScaling, 1*time.Second),
		"Timeout waiting to observe Scaling state")
	// Then wait for the state machine to complete transitions back to Idle
	require.True(t, collector.WaitForState(types.CalcStateIdle, 2*time.Second),
		"Timeout waiting for return to Idle state")

	// Verify all states were observed
	collector.RequireContains(types.CalcStateScaling)
	collector.RequireContains(types.CalcStateRebalancing)
	collector.RequireLastState(types.CalcStateIdle)
}

func TestCalculator_StateTransitions_Scaling(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-scaling-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-scaling-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "test-assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "test-hb",
		HeartbeatTTL:         10 * time.Second,
		EmergencyGracePeriod: 5 * time.Second,
	})
	require.NoError(t, err)

	ctx := context.Background()

	// Use collector to observe all transitions
	collector := newStateTransitionCollector(t, calc.stateMach)
	defer collector.Stop()

	// Enter scaling state
	calc.enterScalingState(ctx, "cold_start", 50*time.Millisecond)

	// Initial checks
	require.Equal(t, types.CalcStateScaling, calc.GetState())
	require.Equal(t, "cold_start", calc.GetScalingReason())

	// Wait to observe Scaling first (avoid matching initial Idle), then final Idle
	require.True(t, collector.WaitForState(types.CalcStateScaling, 1*time.Second),
		"Timeout waiting to observe Scaling state")
	require.True(t, collector.WaitForState(types.CalcStateIdle, 2*time.Second),
		"Timeout waiting for return to Idle state")

	// Verify all transitions occurred
	collector.RequireContains(types.CalcStateScaling)
	collector.RequireContains(types.CalcStateRebalancing)
	collector.RequireLastState(types.CalcStateIdle)
}

func TestCalculator_StateTransitions_Emergency(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-emergency-state-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-emergency-state-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "test-assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "test-hb",
		HeartbeatTTL:         10 * time.Second,
		EmergencyGracePeriod: 5 * time.Second,
	})
	require.NoError(t, err)

	ctx := context.Background()

	// Use collector to observe all transitions
	collector := newStateTransitionCollector(t, calc.stateMach)
	defer collector.Stop()

	// Enter emergency state
	calc.enterEmergencyState(ctx)

	// Wait for complete transition back to Idle
	require.True(t, collector.WaitForState(types.CalcStateIdle, 2*time.Second),
		"Timeout waiting for return to Idle state")

	// Verify emergency rebalancing happened
	// Note: Emergency does NOT go through Rebalancing state - it performs
	// rebalancing while IN Emergency state, then transitions directly to Idle
	collector.RequireContains(types.CalcStateEmergency)
	collector.RequireLastState(types.CalcStateIdle)
	collector.RequireMinimumStates(3) // Idle (initial), Emergency, Idle (final)

	// Verify scaling reason was cleared
	require.Equal(t, "", calc.GetScalingReason())
}

func TestCalculator_StateTransitions_ReturnToIdle(t *testing.T) {
	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-returnidle-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-returnidle-heartbeat")

	// Create a heartbeat for worker-1
	_, err := heartbeatKV.Put(ctx, "test-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "test-assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "test-hb",
		HeartbeatTTL:         10 * time.Second,
		EmergencyGracePeriod: 5 * time.Second,
	})
	require.NoError(t, err)

	// Subscribe to state changes BEFORE triggering transition to avoid missing states
	stateCh, unsubscribe := calc.stateMach.Subscribe()
	defer unsubscribe()

	// Collect all state transitions in a goroutine
	var statesMu sync.Mutex
	states := []types.CalculatorState{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for state := range stateCh {
			statesMu.Lock()
			states = append(states, state)
			currentLen := len(states)
			statesMu.Unlock()

			// Exit after we've seen: Idle -> Scaling -> Rebalancing -> Idle
			// (minimum 4 states including the initial Idle from subscription)
			if state == types.CalcStateIdle && currentLen >= 4 {
				return
			}
		}
	}()

	// Enter scaling state (this will transition through Scaling -> Rebalancing -> Idle)
	// Use 50ms window to ensure we have time to observe states
	calc.enterScalingState(ctx, "test_reason", 50*time.Millisecond)

	// Wait for the state machine to complete the full cycle
	select {
	case <-done:
		// Transition complete
	case <-time.After(2 * time.Second):
		statesMu.Lock()
		t.Fatalf("timeout waiting for state transitions, saw states: %v", states)
		statesMu.Unlock()
	}

	// Verify we observed the complete transition sequence
	statesMu.Lock()
	defer statesMu.Unlock()

	require.GreaterOrEqual(t, len(states), 4, "should have seen at least 4 states (Idle, Scaling, Rebalancing, Idle)")
	require.Contains(t, states, types.CalcStateScaling, "should have entered Scaling state")
	require.Contains(t, states, types.CalcStateRebalancing, "should have entered Rebalancing state")
	require.Equal(t, types.CalcStateIdle, states[len(states)-1], "should have returned to Idle as final state")

	// Verify scaling reason was cleared after returning to idle
	require.Equal(t, "", calc.GetScalingReason())
}

func TestCalculator_StateTransitions_PreventsConcurrentRebalance(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-preventconcurrent-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-preventconcurrent-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "test-assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "test-hb",
		HeartbeatTTL:         1 * time.Second,
		EmergencyGracePeriod: 500 * time.Millisecond,
		Cooldown:             100 * time.Millisecond,
		ColdStartWindow:      500 * time.Millisecond,
		PlannedScaleWindow:   300 * time.Millisecond,
	})
	require.NoError(t, err)

	ctx := context.Background()

	// Start calculator to set up KV buckets
	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Publish some heartbeats
	hbKV := partitest.CreateJetStreamKV(t, nc, "test-hb")

	_, err = hbKV.Put(ctx, "worker-0", []byte("alive"))
	require.NoError(t, err)

	// Wait for initial assignment
	time.Sleep(200 * time.Millisecond)

	// Manually enter scaling state
	calc.enterScalingState(ctx, "test", 200*time.Millisecond)

	// Set up for checkForChanges
	calc.mu.Lock()
	calc.lastWorkers = map[string]bool{"worker-0": true}
	calc.mu.Unlock()

	// Try to trigger another rebalance while scaling
	_, err = hbKV.Put(ctx, "worker-1", []byte("alive"))
	require.NoError(t, err)

	err = calc.checkForChanges(ctx)
	require.NoError(t, err)

	// Should still be in Scaling state (not started new rebalance)
	require.Equal(t, types.CalcStateScaling, calc.GetState())
}

func TestCalculator_StateString(t *testing.T) {
	tests := []struct {
		name  string
		state types.CalculatorState
		want  string
	}{
		{"idle", types.CalcStateIdle, "Idle"},
		{"scaling", types.CalcStateScaling, "Scaling"},
		{"rebalancing", types.CalcStateRebalancing, "Rebalancing"},
		{"emergency", types.CalcStateEmergency, "Emergency"},
		{"unknown", types.CalculatorState(999), "Unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.state.String())
		})
	}
}
