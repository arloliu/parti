package assignment

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
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

	reason, window := calc.detectRebalanceType(lastWorkers, currentWorkers, false)

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

	reason, window := calc.detectRebalanceType(lastWorkers, currentWorkers, false)

	require.Equal(t, "planned_scale", reason)
	require.Equal(t, calc.PlannedScaleWindow, window)
	require.Equal(t, 10*time.Second, window)
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

	reason, window := calc.detectRebalanceType(lastWorkers, currentWorkers, false)

	require.Equal(t, "planned_scale", reason)
	require.Equal(t, calc.PlannedScaleWindow, window)
	require.Equal(t, 10*time.Second, window)
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

// newV9Calculator returns a calculator wired to embedded NATS for the C-tests.
// The detector clock is injected so emergency timing is deterministic.
func newV9Calculator(t *testing.T, bucketSuffix string) (*Calculator, *time.Time) {
	t.Helper()
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "v9-asn-"+bucketSuffix)
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "v9-hb-"+bucketSuffix)

	cfg := &Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "asn",
		Source:               &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}}},
		Strategy:             &mockStrategy{},
		HeartbeatPrefix:      "v9-hb",
		HeartbeatTTL:         10 * time.Second,
		EmergencyGracePeriod: 200 * time.Millisecond,
		Cooldown:             1 * time.Millisecond,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
	}
	calc, err := NewCalculator(cfg)
	require.NoError(t, err)

	t0 := time.Unix(0, 0)
	now := t0
	calc.emergencyDetector.now = func() time.Time { return now }

	return calc, &now
}

// putHeartbeat publishes a heartbeat for a worker so monitor.GetActiveWorkers sees it.
func putHeartbeat(t *testing.T, calc *Calculator, ctx context.Context, workerID string) {
	t.Helper()
	_, err := calc.HeartbeatKV.Put(ctx, "v9-hb."+workerID, []byte("alive"))
	require.NoError(t, err)
}

// C1: pollForChanges in degraded mode must not mutate the emergency detector.
func TestPollForChanges_DegradedFetch_SkipsDetector(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "v9-c1-asn")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "v9-c1-hb")

	cfg := &Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "asn",
		Source:               &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}}},
		Strategy:             &mockStrategy{},
		HeartbeatPrefix:      "v9-hb",
		HeartbeatTTL:         10 * time.Second,
		EmergencyGracePeriod: 100 * time.Millisecond,
		Cooldown:             1 * time.Millisecond,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
	}
	calc, err := NewCalculator(cfg)
	require.NoError(t, err)

	// Pre-populate cache with two workers so degraded fetch returns them.
	calc.updateCachedWorkers([]string{"A", "B"})

	// Pre-populate detector with an in-flight disappearance for A.
	calc.emergencyDetector.mu.Lock()
	calc.emergencyDetector.disappearedWorkers["A"] = time.Now()
	calc.emergencyDetector.mu.Unlock()

	// Drop the NATS connection to force degraded fetch on next call.
	nc.Close()

	// poll triggers the degraded path. With pre-populated cache, getActiveWorkers
	// returns the cached list with fresh=false.
	err = calc.pollForChanges(ctx)
	require.NoError(t, err)

	// Detector must be unchanged: degraded observation does not mutate detector state.
	calc.emergencyDetector.mu.Lock()
	_, stillTracked := calc.emergencyDetector.disappearedWorkers["A"]
	calc.emergencyDetector.mu.Unlock()
	require.True(t, stillTracked, "detector must NOT mutate from a degraded (cached) observation")
}

// C2: emergency claim path releases pollMu before invoking the long-running rebalance,
// so a concurrent pollForChanges call can proceed past pollMu without deadlock.
//
// The rebalance is held inside the strategy callback so the calculator is
// "in" the emergency rebalance when the second poll fires. If the emergency
// path held pollMu across the rebalance, the second poll would deadlock.
func TestPollForChanges_EmergencyPath_ReleasesPollMuBeforeRebalance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx := t.Context()
	calc, clock := newV9Calculator(t, "c2")

	release := make(chan struct{})
	gate := make(chan struct{}, 1)
	calc.Strategy = &blockingStrategy{block: release, entered: gate}

	// One worker in KV; lastWorkers has a different worker (B) so a change is detected.
	putHeartbeat(t, calc, ctx, "A")
	calc.mu.Lock()
	calc.lastWorkers = map[string]bool{"B": true}
	calc.mu.Unlock()

	// Pre-mark B as past-grace, using the injected clock.
	calc.emergencyDetector.mu.Lock()
	calc.emergencyDetector.disappearedWorkers["B"] = clock.Add(-time.Hour)
	calc.emergencyDetector.mu.Unlock()

	// First poll: emergency fires; strategy callback blocks → pollMu must already be released.
	rebalanceDone := make(chan error, 1)
	go func() {
		rebalanceDone <- calc.pollForChanges(ctx)
	}()

	// Wait until the strategy callback has been entered (= rebalance is in-flight).
	select {
	case <-gate:
	case <-time.After(2 * time.Second):
		close(release)
		<-rebalanceDone
		t.Fatal("strategy callback was never entered — rebalance did not run")
	}

	// Second poll must not block on pollMu.
	second := make(chan struct{})
	go func() {
		_ = calc.pollForChanges(ctx)
		close(second)
	}()
	select {
	case <-second:
		// pollMu was released before rebalance — good.
	case <-time.After(500 * time.Millisecond):
		close(release)
		<-rebalanceDone
		t.Fatal("second pollForChanges blocked on pollMu — emergency path did not release it before rebalance")
	}

	close(release)
	<-rebalanceDone
}

// blockingStrategy blocks Assign() the first time it's called; subsequent calls succeed.
type blockingStrategy struct {
	block   chan struct{}
	entered chan struct{}
	called  atomic.Bool
}

func (b *blockingStrategy) Assign(workers []string, partitions []types.Partition) (map[string][]types.Partition, error) {
	if b.called.CompareAndSwap(false, true) {
		select {
		case b.entered <- struct{}{}:
		default:
		}
		<-b.block
	}
	result := make(map[string][]types.Partition)
	for i, part := range partitions {
		w := workers[i%len(workers)]
		result[w] = append(result[w], part)
	}

	return result, nil
}

// C3: same-count replacement (worker A leaves, B arrives) suppresses planned_scale
// while A is mid-grace, then fires emergency once the grace expires.
func TestPollForChanges_SameCountReplacement_SuppressesPlannedScale_FiresEmergencyAfterGrace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx := t.Context()
	calc, clock := newV9Calculator(t, "c3")

	// lastWorkers={A,B}.
	calc.mu.Lock()
	calc.lastWorkers = map[string]bool{"A": true, "B": true}
	calc.currentWorkers = map[string]bool{"A": true, "B": true}
	calc.mu.Unlock()

	// Heartbeats: B and C (A left, C joined — same count).
	putHeartbeat(t, calc, ctx, "B")
	putHeartbeat(t, calc, ctx, "C")

	// Initial poll: A is mid-grace; planned_scale suppressed; state stays Idle.
	require.NoError(t, calc.pollForChanges(ctx))
	require.Equal(t, types.CalcStateIdle, calc.GetState(),
		"planned_scale must be suppressed while a disappearance is mid-grace")

	// Advance time past grace.
	*clock = clock.Add(300 * time.Millisecond)

	// Next poll: A's grace expired; emergency fires.
	require.NoError(t, calc.pollForChanges(ctx))
	require.Eventually(t, func() bool {
		return calc.GetState() == types.CalcStateIdle && calc.publisher.CurrentVersion() > 0
	}, 2*time.Second, 10*time.Millisecond, "emergency rebalance should fire and return to Idle")
}

// C4: after a recovery clears pending, the detector tracking for the
// recovered worker is gone, so a subsequent observation does not inherit
// stale state. This variant exercises recovery combined with another
// topology change (worker D joins), so the change-detection path also fires.
// See TestPollForChanges_PureRecoveryNoTopologyChange_ClearsDetector for the
// pure-recovery shape where only the recovered worker reappears.
func TestPollForChanges_RecoveryClearsPending_NormalRebalanceResumes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx := t.Context()
	calc, _ := newV9Calculator(t, "c4")

	calc.mu.Lock()
	calc.lastWorkers = map[string]bool{"A": true, "B": true, "C": true}
	calc.currentWorkers = map[string]bool{"A": true, "B": true, "C": true}
	calc.mu.Unlock()

	// A disappears (B, C alive). pending starts.
	putHeartbeat(t, calc, ctx, "B")
	putHeartbeat(t, calc, ctx, "C")
	require.NoError(t, calc.pollForChanges(ctx))
	calc.emergencyDetector.mu.Lock()
	_, tracked := calc.emergencyDetector.disappearedWorkers["A"]
	calc.emergencyDetector.mu.Unlock()
	require.True(t, tracked, "A should be tracked after disappearance observation")

	// A recovers AND a new worker D joins (so the change check triggers re-evaluation).
	putHeartbeat(t, calc, ctx, "A")
	putHeartbeat(t, calc, ctx, "D")
	require.NoError(t, calc.pollForChanges(ctx))

	calc.emergencyDetector.mu.Lock()
	_, stillTracked := calc.emergencyDetector.disappearedWorkers["A"]
	calc.emergencyDetector.mu.Unlock()
	require.False(t, stillTracked, "recovery must clear pending via Phase 1")
}

// TestPollForChanges_PureRecoveryNoTopologyChange_ClearsDetector exercises the
// pure-recovery shape: A disappears, A reappears with no other topology
// change (workers == lastWorkers), then A disappears again. The detector
// invariant requires firstSeen[A] to be cleared by the recovery poll so a
// subsequent disappearance starts a fresh grace period.
//
// Without the detector reconciliation running on every fresh observation
// (before the !changed short-circuit), the recovery poll would skip
// CheckEmergency, A's stranded firstSeen would survive, and the next
// disappearance would inherit the stale timestamp — firing immediate
// emergency once enough wall-clock time had elapsed since the original
// disappearance.
func TestPollForChanges_PureRecoveryNoTopologyChange_ClearsDetector(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx := t.Context()
	calc, clock := newV9Calculator(t, "c-pure-recovery")

	calc.mu.Lock()
	calc.lastWorkers = map[string]bool{"A": true, "B": true}
	calc.currentWorkers = map[string]bool{"A": true, "B": true}
	calc.mu.Unlock()

	// Step 1: A disappears. Only B heartbeats.
	putHeartbeat(t, calc, ctx, "B")
	require.NoError(t, calc.pollForChanges(ctx))
	calc.emergencyDetector.mu.Lock()
	_, tracked := calc.emergencyDetector.disappearedWorkers["A"]
	calc.emergencyDetector.mu.Unlock()
	require.True(t, tracked, "A must be tracked after first observation")

	// Step 2: A recovers. workers == lastWorkers → !changed.
	// With the fix, CheckEmergency still runs and Phase 1 clears A.
	putHeartbeat(t, calc, ctx, "A")
	*clock = clock.Add(50 * time.Millisecond)
	require.NoError(t, calc.pollForChanges(ctx))

	calc.emergencyDetector.mu.Lock()
	_, stillTracked := calc.emergencyDetector.disappearedWorkers["A"]
	calc.emergencyDetector.mu.Unlock()
	require.False(t, stillTracked,
		"pure-recovery poll (workers == lastWorkers) must clear detector tracking for A")

	// Step 3: advance past the original grace period, then A disappears
	// again. Without the fix, the stale firstSeen from step 1 would make
	// this an immediate confirmed emergency; with the fix, the recovery
	// poll cleared A so the disappearance starts a fresh grace.
	*clock = clock.Add(300 * time.Millisecond) // > grace (200ms)
	require.NoError(t, calc.HeartbeatKV.Delete(ctx, "v9-hb.A"))
	require.NoError(t, calc.pollForChanges(ctx))

	require.Equal(t, types.CalcStateIdle, calc.GetState(),
		"second disappearance must observe a fresh grace period, not fire immediate emergency")
}

// C5 (FP-3): rebalance with a non-emergency lifecycle must NOT consume
// c.disappearedWorkers — that buffer is reserved for the emergency rebalance.
func TestRebalance_NonEmergencyLifecycle_DoesNotConsumeDisappearedWorkers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx := t.Context()
	calc, _ := newV9Calculator(t, "c5")

	// Pre-populate disappearedWorkers.
	calc.mu.Lock()
	calc.disappearedWorkers = []string{"A", "B"}
	calc.mu.Unlock()

	putHeartbeat(t, calc, ctx, "C")

	// Non-emergency lifecycle: must not consume the buffer.
	require.NoError(t, calc.rebalance(ctx, "planned_scale"))

	calc.mu.Lock()
	remaining := calc.disappearedWorkers
	calc.mu.Unlock()
	require.ElementsMatch(t, []string{"A", "B"}, remaining,
		"disappearedWorkers buffer must survive a non-emergency rebalance")
}

// C6: when two workers are co-pending, the one whose grace expires first fires
// emergency; the other's tracking is preserved.
func TestPollForChanges_CoPending_AFiresEmergencyEPreserved(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx := t.Context()
	calc, clock := newV9Calculator(t, "c6")

	calc.mu.Lock()
	calc.lastWorkers = map[string]bool{"A": true, "B": true, "C": true}
	calc.currentWorkers = map[string]bool{"A": true, "B": true, "C": true}
	calc.mu.Unlock()

	// Only C heartbeats — both A and B are missing, but A disappeared first.
	putHeartbeat(t, calc, ctx, "C")

	// First observation: track A and B at t=0 (both observed missing at the same time).
	// To get separate firstSeen, we observe A as missing first (B still alive), then both missing.
	// Use direct CheckEmergency calls on the detector to set up the staggered firstSeen.
	calc.emergencyDetector.mu.Lock()
	calc.emergencyDetector.disappearedWorkers["A"] = *clock
	calc.emergencyDetector.disappearedWorkers["B"] = clock.Add(100 * time.Millisecond)
	calc.emergencyDetector.mu.Unlock()

	// Advance past A's grace (200ms) but before B's grace would expire (B firstSeen=100ms,
	// so grace expires at 300ms). At t=210ms, A is confirmed, B is still pending.
	*clock = clock.Add(210 * time.Millisecond)
	require.NoError(t, calc.pollForChanges(ctx))

	// Wait for emergency to complete and return to Idle.
	require.Eventually(t, func() bool {
		return calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 10*time.Millisecond)

	// After the rebalance, B's tracking should be preserved. But because the rebalance
	// called ObserveAlive with the live worker set (C only), B is NOT in that set, so
	// B's tracking is preserved (ObserveAlive only deletes workers in the alive set).
	calc.emergencyDetector.mu.Lock()
	_, bTracked := calc.emergencyDetector.disappearedWorkers["B"]
	calc.emergencyDetector.mu.Unlock()
	require.True(t, bTracked, "B's tracking must survive the emergency rebalance for A")
}

// C7: scaling timer racing emergency claim — only one wins via strict-source CAS.
func TestPollForChanges_ScalingTimerVsEmergencyClaim_AtomicTransition(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	calc, _ := newV9Calculator(t, "c7")

	// Move state to Scaling manually.
	require.True(t, calc.stateMach.compareAndSwapState(types.CalcStateIdle, types.CalcStateScaling))

	// Simulate scaling timer winning: advance Scaling -> Rebalancing.
	require.True(t, calc.stateMach.compareAndSwapState(types.CalcStateScaling, types.CalcStateRebalancing))

	// Emergency claim must fail because neither Idle nor Scaling matches.
	require.False(t, calc.stateMach.TryClaimEmergency(context.Background()))
	require.Equal(t, types.CalcStateRebalancing, calc.stateMach.GetState())
}

// C8: scaling timer wins; subsequent emergency tracking is preserved across the
// rebalance, becomes stranded; on the next planned rebalance picking up the
// recovered/absorbed worker, ObserveAlive clears the entry.
func TestPollForChanges_ScalingTimerWins_ThenRebalanceAbsorbsWorker(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx := t.Context()
	calc, clock := newV9Calculator(t, "c8")

	// lastWorkers={A,B}. A disappears; B still alive.
	calc.mu.Lock()
	calc.lastWorkers = map[string]bool{"A": true, "B": true}
	calc.currentWorkers = map[string]bool{"A": true, "B": true}
	calc.mu.Unlock()

	putHeartbeat(t, calc, ctx, "B")

	// Track A.
	require.NoError(t, calc.pollForChanges(ctx))

	// "Scaling timer wins" — simulate a planned rebalance running that absorbs the change.
	// Drive the rebalance directly with a non-emergency lifecycle.
	require.NoError(t, calc.rebalance(ctx, "planned_scale"))

	// A's tracking should now be stranded (A removed from lastWorkers by handleRebalance —
	// but handleRebalance isn't called by direct rebalance(); we update manually).
	calc.mu.Lock()
	calc.lastWorkers = map[string]bool{"B": true}
	calc.mu.Unlock()

	*clock = clock.Add(time.Second)

	// A reappears; planned rebalance picks A up. ObserveAlive must clear A.
	putHeartbeat(t, calc, ctx, "A")
	require.NoError(t, calc.rebalance(ctx, "planned_scale"))

	calc.emergencyDetector.mu.Lock()
	_, aTracked := calc.emergencyDetector.disappearedWorkers["A"]
	calc.emergencyDetector.mu.Unlock()
	require.False(t, aTracked, "ObserveAlive in rebalance must clear A when A becomes visible again")
}

// C9 (v9 fix verification): audit_repair path triggers a rebalance with a fresh
// KV scan. If a stranded detector entry for A exists and A's heartbeat is now in
// KV, ObserveAlive must clear A's entry before a subsequent disappearance starts
// a fresh grace period.
func TestRebalance_AuditRepairPath_ObserveAliveClearsStaleEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx := t.Context()
	calc, clock := newV9Calculator(t, "c9")

	// Pre-populate a stranded detector entry for A.
	calc.emergencyDetector.mu.Lock()
	calc.emergencyDetector.disappearedWorkers["A"] = *clock
	calc.emergencyDetector.mu.Unlock()

	// Put A's heartbeat into KV so the audit_repair rebalance's fresh scan sees A.
	putHeartbeat(t, calc, ctx, "A")

	// audit_repair triggers rebalance with a non-emergency lifecycle.
	require.NoError(t, calc.rebalance(ctx, "audit_repair"))

	// A's entry must be cleared by ObserveAlive.
	calc.emergencyDetector.mu.Lock()
	_, stillTracked := calc.emergencyDetector.disappearedWorkers["A"]
	calc.emergencyDetector.mu.Unlock()
	require.False(t, stillTracked, "ObserveAlive in rebalance must clear A's stranded tracking")

	// Subsequent disappearance starts a fresh grace.
	*clock = clock.Add(time.Second)
	// Set lastWorkers so A enters prev.
	calc.mu.Lock()
	calc.lastWorkers = map[string]bool{"A": true}
	calc.mu.Unlock()

	emergency, _, _ := calc.emergencyDetector.CheckEmergency(
		map[string]bool{"A": true},
		map[string]bool{},
	)
	require.False(t, emergency, "fresh grace period must not be voided by the prior stranded entry")
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
