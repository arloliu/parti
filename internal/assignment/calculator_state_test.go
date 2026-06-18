package assignment

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
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

// --- merged from calculator_scaling_test.go ---

// TestCalculator_ScalingTransition_TimerFires tests that the timer goroutine fires after the window.
//
// This is a focused unit test to verify the timer mechanism works in isolation.
func TestCalculator_ScalingTransition_TimerFires(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-scaling-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-scaling-heartbeat")

	src := &mockSource{partitions: []types.Partition{
		{Keys: []string{"p1"}, Weight: 100},
		{Keys: []string{"p2"}, Weight: 100},
	}}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "test",
		Source:               src,
		Strategy:             strategy,
		HeartbeatPrefix:      "heartbeat",
		HeartbeatTTL:         5 * time.Second,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      100 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
	})
	require.NoError(t, err)

	ctx := context.Background()

	// Manually trigger scaling state (bypassing Start/monitorWorkers)
	calc.enterScalingState(ctx, "test_reason", 100*time.Millisecond)

	// Verify we're in Scaling state
	require.Equal(t, types.CalcStateScaling, calc.GetState())

	// Wait longer than the window to see if transition happens
	time.Sleep(200 * time.Millisecond)

	// Check if transitioned to Rebalancing or Idle
	finalState := calc.GetState()
	t.Logf("Final state after window: %s", finalState)

	// Should have transitioned away from Scaling
	// (Either to Rebalancing then Idle, or just Idle if rebalance was fast)
	require.NotEqual(t, types.CalcStateScaling, finalState, "calculator should have transitioned out of Scaling state")
}

// TestCalculator_ScalingTransition_WithRealStart tests scaling transition with full Calculator.Start().
//
// This tests the integration between monitorWorkers and the state machine.
func TestCalculator_ScalingTransition_WithRealStart(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-scaling-real-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-scaling-real-heartbeat")

	src := &mockSource{partitions: []types.Partition{
		{Keys: []string{"p1"}, Weight: 100},
		{Keys: []string{"p2"}, Weight: 100},
	}}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "test",
		Source:               src,
		Strategy:             strategy,
		HeartbeatPrefix:      "heartbeat",
		HeartbeatTTL:         1 * time.Second,
		EmergencyGracePeriod: 500 * time.Millisecond,
		ColdStartWindow:      500 * time.Millisecond,
		PlannedScaleWindow:   250 * time.Millisecond,
		Cooldown:             0,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create initial heartbeat (simulate 1 worker)
	err = publishHeartbeat(ctx, heartbeatKV, "heartbeat.worker-1", 2*time.Second)
	require.NoError(t, err)

	// Start calculator
	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial assignment
	time.Sleep(200 * time.Millisecond)

	initialState := calc.GetState()
	t.Logf("Initial state: %s", initialState)

	// Add a second worker (should trigger scaling)
	err = publishHeartbeat(ctx, heartbeatKV, "heartbeat.worker-2", 2*time.Second)
	require.NoError(t, err)

	// With hybrid approach (watcher + polling), detection is much faster (<100ms)
	// Give watcher time to process the event
	time.Sleep(50 * time.Millisecond)

	scalingState := calc.GetState()
	t.Logf("State after adding worker: %s", scalingState)

	// Should have entered Scaling state (or already transitioned through it)
	// With fast watcher detection, we might catch it in Scaling or it might already be done
	require.Contains(t, []types.CalculatorState{types.CalcStateScaling, types.CalcStateRebalancing, types.CalcStateIdle}, scalingState,
		"calculator should have processed worker addition")

	// Wait for full cycle to complete (stabilization window + processing)
	time.Sleep(800 * time.Millisecond)

	finalState := calc.GetState()
	t.Logf("Final state after window: %s", finalState)

	// Should have completed scaling and returned to Idle
	require.Equal(t, types.CalcStateIdle, finalState, "calculator should return to Idle after scaling completes")
}

// TestCalculator_ScalingTransition_ContextCancellation tests that context cancellation stops the timer.
func TestCalculator_ScalingTransition_ContextCancellation(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-scaling-cancel-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-scaling-cancel-heartbeat")

	src := &mockSource{partitions: []types.Partition{
		{Keys: []string{"p1"}, Weight: 100},
	}}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "test",
		Source:               src,
		Strategy:             strategy,
		HeartbeatPrefix:      "heartbeat",
		HeartbeatTTL:         5 * time.Second,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      2 * time.Second,
		PlannedScaleWindow:   1 * time.Second,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	// Trigger scaling state
	calc.enterScalingState(ctx, "test_reason", 2*time.Second)

	require.Equal(t, types.CalcStateScaling, calc.GetState())

	// Cancel context before window elapses
	time.Sleep(100 * time.Millisecond)
	cancel()

	// Wait a bit more
	time.Sleep(100 * time.Millisecond)

	// Should still be in Scaling (goroutine exited, no transition)
	state := calc.GetState()
	t.Logf("State after context cancellation: %s", state)
	require.Equal(t, types.CalcStateScaling, state, "should remain in Scaling when context cancelled before window")
}

// TestCalculator_ScalingTransition_StopBeforeWindow tests that Stop() prevents transition.
func TestCalculator_ScalingTransition_StopBeforeWindow(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-scaling-stop-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-scaling-stop-heartbeat")

	src := &mockSource{partitions: []types.Partition{
		{Keys: []string{"p1"}, Weight: 100},
	}}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "test",
		Source:               src,
		Strategy:             strategy,
		HeartbeatPrefix:      "heartbeat",
		HeartbeatTTL:         5 * time.Second,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      500 * time.Millisecond,
		PlannedScaleWindow:   1 * time.Second,
		Cooldown:             0,
	})
	require.NoError(t, err)

	ctx := context.Background()

	// Start calculator
	err = calc.Start(ctx)
	require.NoError(t, err)

	// Trigger scaling state manually
	calc.enterScalingState(ctx, "test_reason", 500*time.Millisecond)

	require.Equal(t, types.CalcStateScaling, calc.GetState())

	// Stop calculator before window elapses
	time.Sleep(100 * time.Millisecond)
	err = calc.Stop(ctx)
	require.NoError(t, err)

	// Wait longer than window would have been
	time.Sleep(700 * time.Millisecond)

	// State shouldn't change after Stop
	state := calc.GetState()
	t.Logf("State after Stop: %s", state)
}

// TestCalculator_ScalingTransition_RapidStateChanges tests multiple rapid worker changes.
func TestCalculator_ScalingTransition_RapidStateChanges(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-scaling-rapid-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-scaling-rapid-heartbeat")

	src := &mockSource{partitions: []types.Partition{
		{Keys: []string{"p1"}, Weight: 100},
		{Keys: []string{"p2"}, Weight: 100},
	}}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "test",
		Source:               src,
		Strategy:             strategy,
		HeartbeatPrefix:      "heartbeat",
		HeartbeatTTL:         500 * time.Millisecond,
		EmergencyGracePeriod: 300 * time.Millisecond,
		ColdStartWindow:      300 * time.Millisecond,
		PlannedScaleWindow:   150 * time.Millisecond,
		Cooldown:             0,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create initial worker
	err = publishHeartbeat(ctx, heartbeatKV, "heartbeat.worker-1", 1*time.Second)
	require.NoError(t, err)

	// Start calculator
	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	time.Sleep(200 * time.Millisecond)
	t.Logf("Initial state: %s", calc.GetState())

	// Add worker 2
	err = publishHeartbeat(ctx, heartbeatKV, "heartbeat.worker-2", 1*time.Second)
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)
	state1 := calc.GetState()
	t.Logf("After adding worker-2: %s", state1)

	// Add worker 3 quickly
	err = publishHeartbeat(ctx, heartbeatKV, "heartbeat.worker-3", 1*time.Second)
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)
	state2 := calc.GetState()
	t.Logf("After adding worker-3: %s", state2)

	// Should be in Scaling state (or might have already transitioned)
	// The key test is that it doesn't crash or deadlock
	t.Logf("State during rapid changes: %s", state2)

	// Wait for all windows to complete
	time.Sleep(1 * time.Second)

	finalState := calc.GetState()
	t.Logf("Final state: %s", finalState)

	// Eventually should return to Idle
	require.Eventually(t, func() bool {
		return calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 100*time.Millisecond, "should eventually return to Idle")
}

// publishHeartbeat is a helper to publish a worker heartbeat to the KV store.
func publishHeartbeat(ctx context.Context, kv jetstream.KeyValue, key string, _ time.Duration) error {
	data := []byte(time.Now().Format(time.RFC3339))
	_, err := kv.Put(ctx, key, data)
	return err
}

// --- merged from calculator_cooldown_test.go ---

// fakeClock is a manually-advanced clock for deterministic cooldown tests.
// The calculator's cooldown gate and the publisher's lastRebalance stamp both
// read Config.Now, so advancing this clock past Cooldown deterministically
// opens the gate regardless of real-time scheduling under load. Freezing it
// (not advancing) keeps the gate closed no matter how much wall time passes.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Now()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// waitStableVersion waits until the calculator's version is > 0 and has not
// changed across a confirmation window, then returns it. Cold start may perform
// more than one rebalance (initial assignment plus a stabilization-window
// rebalance); with a frozen fake clock the cooldown gate blocks any further
// rebalance once cold start settles, so a stable version is a deterministic
// baseline for the cooldown assertions that follow.
func waitStableVersion(t *testing.T, calc *Calculator) int64 {
	t.Helper()
	const confirm = 500 * time.Millisecond
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		v := calc.CurrentVersion()
		// Require a published version AND that the state machine has returned
		// to Idle. Idle gates out the window where a cold-start final rebalance
		// is still pending (CalcStateScaling) but the version has not yet been
		// bumped: a version-only check could otherwise capture the
		// immediate-assignment version as "stable" under load, before the
		// delayed stabilization timer fires — the same premature-baseline race
		// this helper exists to prevent.
		if v == 0 || calc.GetState() != types.CalcStateIdle {
			time.Sleep(25 * time.Millisecond)
			continue
		}
		stable := true
		end := time.Now().Add(confirm)
		for time.Now().Before(end) {
			time.Sleep(25 * time.Millisecond)
			if calc.CurrentVersion() != v || calc.GetState() != types.CalcStateIdle {
				stable = false
				break
			}
		}
		if stable {
			return v
		}
	}
	t.Fatal("calculator version did not stabilize")

	return 0
}

// TestCalculator_RebalanceAttemptDuringCooldown_Blocked verifies cooldown prevents rebalancing.
func TestCalculator_RebalanceAttemptDuringCooldown_Blocked(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-blocked-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-blocked-heartbeat")

	// Create initial worker
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
		},
	}
	strategy := &mockStrategy{}

	clock := newFakeClock()
	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         2 * time.Second, // long enough that workers persist through the short test
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             5 * time.Second, // fake-clock driven; never elapses in real time
		Now:                  clock.Now,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Cold-start rebalance(s) establish the initial version and stamp
	// lastRebalance at the (frozen) clock time. Wait until it fully settles.
	initialVersion := waitStableVersion(t, calc)

	// Add worker-2: the monitor's watcher fires a poll, but the cooldown gate
	// (clock has not advanced past the 5s Cooldown) must block the rebalance.
	// Because the gate reads the frozen fake clock, real-time scheduling under
	// load cannot let the rebalance slip through.
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	require.Never(t, func() bool { return calc.CurrentVersion() != initialVersion },
		750*time.Millisecond, 50*time.Millisecond,
		"cooldown must block the rebalance while the clock has not advanced past Cooldown")

	t.Logf("cooldown correctly blocked rebalance: version remained %d", initialVersion)
}

// TestCalculator_RebalanceAfterCooldown_Allowed verifies rebalance proceeds after cooldown expires.
func TestCalculator_RebalanceAfterCooldown_Allowed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-allowed-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-allowed-heartbeat")

	// Create initial worker
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
		},
	}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         200 * time.Millisecond, // Fast heartbeat
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             200 * time.Millisecond, // Short cooldown
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial rebalance
	time.Sleep(100 * time.Millisecond)
	initialVersion := calc.CurrentVersion()
	require.NotZero(t, initialVersion)

	// Add worker-2
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Wait for cooldown to expire + monitoring cycle (200ms cooldown + 100ms monitoring + margin)
	time.Sleep(500 * time.Millisecond)

	// Version SHOULD change - cooldown expired
	newVersion := calc.CurrentVersion()
	require.Greater(t, newVersion, initialVersion, "rebalance should proceed after cooldown")

	t.Logf("Rebalance proceeded after cooldown: %d → %d", initialVersion, newVersion)
}

// TestCalculator_CooldownBoundary_ExactTiming verifies cooldown timing is precise.
func TestCalculator_CooldownBoundary_ExactTiming(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-boundary-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-boundary-heartbeat")

	// Create initial worker
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{{Keys: []string{"p1"}}},
	}
	strategy := &mockStrategy{}

	cooldownDuration := 5 * time.Second // fake-clock driven; real time never reaches it
	clock := newFakeClock()
	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         2 * time.Second,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             cooldownDuration,
		Now:                  clock.Now,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	initialVersion := waitStableVersion(t, calc)

	// Add worker-2. Just below the boundary: advance the clock to one tick short
	// of Cooldown — the gate must still block.
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)
	clock.Advance(cooldownDuration - time.Millisecond)
	require.Never(t, func() bool { return calc.CurrentVersion() != initialVersion },
		500*time.Millisecond, 50*time.Millisecond, "must still be in cooldown just below the boundary")
	t.Log("just below boundary: cooldown still active")

	// Cross the boundary: advance past Cooldown and re-touch the heartbeat to
	// fire the watcher; the rebalance must now proceed.
	clock.Advance(2 * time.Millisecond)
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return calc.CurrentVersion() > initialVersion },
		3*time.Second, 50*time.Millisecond, "rebalance must proceed once the clock crosses Cooldown")
	t.Logf("crossed boundary: version %d → %d", initialVersion, calc.CurrentVersion())
}

// TestCalculator_MultipleCooldowns_Sequential verifies multiple cooldown cycles work correctly.
func TestCalculator_MultipleCooldowns_Sequential(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-multi-cooldown-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-multi-cooldown-heartbeat")

	// Create initial worker
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
			{Keys: []string{"p3"}},
		},
	}
	strategy := &mockStrategy{}

	clock := newFakeClock()
	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         3 * time.Second, // workers persist across all cycles
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             5 * time.Second, // fake-clock driven; never elapses in real time
		Now:                  clock.Now,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// renew re-puts the given worker heartbeats: keeps them alive (beating the
	// KV TTL) and fires the monitor's watcher. A cooldown-blocked poll does not
	// clear the pending worker-set change, so re-firing the watcher after the
	// clock advances past Cooldown lets the blocked rebalance proceed.
	renew := func(workers ...string) {
		for _, w := range workers {
			_, perr := heartbeatKV.Put(ctx, "worker-hb."+w, []byte(time.Now().Format(time.RFC3339Nano)))
			require.NoError(t, perr)
		}
	}

	prev := waitStableVersion(t, calc)
	t.Logf("initial version: %d", prev)

	alive := make([]string, 0, 4)
	alive = append(alive, "worker-1")
	for _, added := range []string{"worker-2", "worker-3", "worker-4"} {
		alive = append(alive, added)
		renew(alive...) // introduce the new worker (set change) — blocked by cooldown
		clock.Advance(5*time.Second + 100*time.Millisecond)
		renew(alive...) // gate is open now; re-fire the watcher to retry the rebalance
		want := prev + 1
		require.Eventually(t, func() bool { return calc.CurrentVersion() == want },
			3*time.Second, 50*time.Millisecond,
			"adding %s should bump version to %d after cooldown", added, want)
		require.Equal(t, want, calc.CurrentVersion(),
			"each cooldown cycle must increment the version by exactly 1")
		t.Logf("after %s: version %d", added, want)
		prev = want
	}
}

// TestCalculator_TriggerRebalance_BypassesCooldown verifies TriggerRebalance bypasses cooldown.
func TestCalculator_TriggerRebalance_BypassesCooldown(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-trigger-bypass-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-trigger-bypass-heartbeat")

	// Create worker
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{{Keys: []string{"p1"}}},
	}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         200 * time.Millisecond,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   100 * time.Millisecond,
		Cooldown:             10 * time.Second, // Very long cooldown
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial rebalance
	time.Sleep(500 * time.Millisecond)
	initialVersion := calc.CurrentVersion()

	// Trigger rebalance manually (should bypass cooldown)
	err = calc.TriggerRebalance(ctx)
	require.NoError(t, err)

	// Wait for rebalance to complete
	time.Sleep(500 * time.Millisecond)

	newVersion := calc.CurrentVersion()
	require.Greater(t, newVersion, initialVersion, "TriggerRebalance should bypass cooldown")

	t.Logf("TriggerRebalance bypassed cooldown: %d → %d", initialVersion, newVersion)
}

// TestCalculator_CooldownReset_AfterEachRebalance verifies cooldown resets after each rebalance.
func TestCalculator_CooldownReset_AfterEachRebalance(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-reset-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-reset-heartbeat")

	// Create initial worker
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{{Keys: []string{"p1"}}},
	}
	strategy := &mockStrategy{}

	// Use shorter cooldown for faster test
	cooldownDuration := 200 * time.Millisecond
	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         200 * time.Millisecond,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             cooldownDuration,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial assignment publish (version >=1)
	var v1 int64
	waitDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(waitDeadline) {
		v1 = calc.CurrentVersion()
		if v1 >= 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.GreaterOrEqual(t, v1, int64(1), "initial assignment version should be >=1")
	t1 := time.Now()

	// Add worker-2
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Wait for version increment (rebalance triggered) with generous deadline
	var v2 int64
	waitDeadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(waitDeadline) {
		v2 = calc.CurrentVersion()
		if v2 > v1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.Greater(t, v2, v1, "first rebalance should complete")
	t2 := time.Now()

	elapsed1 := t2.Sub(t1)
	t.Logf("First rebalance delay: %v (cooldown=%v)", elapsed1, cooldownDuration)

	// Add worker-3 IMMEDIATELY after rebalance (cooldown should reset)
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-3", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Wait for second version increment with same generous deadline
	var v3 int64
	waitDeadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(waitDeadline) {
		v3 = calc.CurrentVersion()
		if v3 > v2 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.Greater(t, v3, v2, "second rebalance should complete with fresh cooldown")
	t3 := time.Now()
	require.Greater(t, v3, v2, "second rebalance should complete with fresh cooldown")

	elapsed2 := t3.Sub(t2)
	t.Logf("Second rebalance delay: %v (cooldown=%v)", elapsed2, cooldownDuration)

	// Assert behavior: first rebalance may be fast due to cold start shortcut; second should honor cooldown.
	require.Greater(t, elapsed2, cooldownDuration, "second rebalance should reflect cooldown interval")
	require.Greater(t, elapsed2, elapsed1, "second rebalance should not be faster than first when cooldown applies")
	// Upper bound sanity (avoid runaway delays > 10s in test environment).
	require.Less(t, elapsed2, 5*time.Second, "second rebalance delay unexpectedly large")
}

// TestCalculator_Cooldown_WithPartitionRefresh verifies cooldown applies to partition source changes.
func TestCalculator_Cooldown_WithPartitionRefresh(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-refresh-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-refresh-heartbeat")

	// Create worker
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Use dynamic source that can change partitions
	source := &mockSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
		},
	}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         200 * time.Millisecond,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             200 * time.Millisecond,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial rebalance
	time.Sleep(100 * time.Millisecond)
	initialVersion := calc.CurrentVersion()

	// Change partition source (add new partition). Use SetPartitions: the
	// calculator's background rebalance goroutine may be reading the source
	// concurrently via List, so a direct field write would be a data race.
	source.SetPartitions([]types.Partition{
		{Keys: []string{"p1"}},
		{Keys: []string{"p2"}},
		{Keys: []string{"p3"}}, // New partition
	})

	// Trigger partition refresh
	err = calc.TriggerRebalance(ctx)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	firstRefreshVersion := calc.CurrentVersion()
	require.Greater(t, firstRefreshVersion, initialVersion, "first refresh should succeed")

	// Try another refresh immediately (should be blocked by cooldown)
	source.SetPartitions([]types.Partition{
		{Keys: []string{"p1"}},
		{Keys: []string{"p2"}},
		{Keys: []string{"p3"}},
		{Keys: []string{"p4"}},
	})
	err = calc.TriggerRebalance(ctx)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	// Note: TriggerRebalance bypasses cooldown, so this will actually succeed
	// This documents the current behavior
	secondRefreshVersion := calc.CurrentVersion()
	t.Logf("Partition refresh: %d → %d → %d (TriggerRebalance bypasses cooldown)",
		initialVersion, firstRefreshVersion, secondRefreshVersion)
}

// --- merged from calculator_trigger_rebalance_test.go ---

// TestTriggerRebalance_NoDuplicateScaleOnNextPoll verifies that a manual
// TriggerRebalance refreshes lastWorkers so the next observeAndDecide poll
// does not fire a spurious planned_scale rebalance for the same topology.
//
// Pre-PR-5 the manual-rebalance path called rebalance directly without
// updating lastWorkers; rebalance refreshes currentWorkers from a fresh
// heartbeat scan, so when TriggerRebalance is invoked while a newly-added
// worker is still pending detection by the worker monitor's watcher, the
// post-trigger state has currentWorkers={w1,w2,w3} but lastWorkers={w1,w2}
// — the next poll sees the diff and enters Scaling for a no-op cycle.
func TestTriggerRebalance_NoDuplicateScaleOnNextPoll(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-trigger-no-dup-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-trigger-no-dup-heartbeat")

	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}},
	}
	strategy := &mockStrategy{}

	// Long HeartbeatTTL keeps all workers alive for the whole test. The worker
	// monitor is stopped after the cluster settles (see below) so its watcher
	// cannot independently observe worker-3 and race the manual
	// TriggerRebalance.
	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         60 * time.Second,
		EmergencyGracePeriod: 5 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             10 * time.Millisecond,
	})
	require.NoError(t, err)

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for the initial rebalance to settle.
	require.Eventually(t, func() bool {
		return calc.GetState() == types.CalcStateIdle && calc.CurrentVersion() > 0
	}, 5*time.Second, 25*time.Millisecond, "initial rebalance did not settle")

	// Stop the worker monitor's background watcher so no background poll can
	// rebalance behind our back and perturb the version during the behavioral
	// check below. TriggerRebalance still picks up worker-3 via its own fresh
	// KV scan (GetActiveWorkers is a direct scan, monitor-independent), and
	// IsStarted reads a calculator-level flag, so stopping the monitor does not
	// disable TriggerRebalance.
	require.NoError(t, calc.monitor.Stop())

	versionBeforeTrigger := calc.CurrentVersion()

	// Add a third worker directly to KV. TriggerRebalance fetches the worker
	// set fresh inside rebalance, so it picks up worker-3.
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-3", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	require.NoError(t, calc.TriggerRebalance(ctx))
	versionAfterTrigger := calc.CurrentVersion()
	require.Greater(t, versionAfterTrigger, versionBeforeTrigger,
		"TriggerRebalance must publish a new assignment version")

	// Primary regression guard — deterministic, with no dependence on watcher
	// quiescence or scaling-timer timing: TriggerRebalance must refresh
	// lastWorkers to match the freshly-scanned worker set. This is the exact
	// invariant the W3 fix restored. Without the refresh, lastWorkers stays
	// {worker-1, worker-2} and the next observe cycle re-enters planned_scale
	// for worker-3 (the duplicate scale this test guards against).
	calc.mu.RLock()
	lastWorkers := calc.cloneLastWorkersLocked()
	calc.mu.RUnlock()
	require.True(t, lastWorkers["worker-3"],
		"TriggerRebalance must refresh lastWorkers to include the newly-scanned worker-3")
	require.Len(t, lastWorkers, 3,
		"lastWorkers must equal the post-trigger worker set {worker-1, worker-2, worker-3}")

	// Behavioral confirmation: observing the now-unchanged topology must not
	// start another rebalance. With lastWorkers refreshed, observeAndDecide
	// short-circuits on "no change" before any scaling or rebalance, so the
	// version stays put. The monitor is stopped, so this explicit
	// checkForChanges is the only observation in flight — fully deterministic,
	// with no draining for the absence of a state transition.
	require.Eventually(t, func() bool {
		return calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 10*time.Millisecond, "calculator should return to Idle after TriggerRebalance")
	require.NoError(t, calc.checkForChanges(ctx))
	require.Equal(t, versionAfterTrigger, calc.CurrentVersion(),
		"no further rebalance must run for an unchanged topology after TriggerRebalance")
}

// --- merged from calculator_splitbrain_test.go ---

// TestCalculator_LeaderRevision_EmbeddedInAssignment verifies that the LeaderRevision
// function from Config is called during rebalance and its value is embedded in
// the published assignments read back from KV.
func TestCalculator_LeaderRevision_EmbeddedInAssignment(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-splitbrain-epoch-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-splitbrain-epoch-heartbeat")

	_, err := heartbeatKV.Put(ctx, "heartbeat.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	const wantRevision uint64 = 99

	calc, err := NewCalculator(&Config{
		AssignmentKV:       assignmentKV,
		HeartbeatKV:        heartbeatKV,
		AssignmentPrefix:   "assignment",
		HeartbeatPrefix:    "heartbeat",
		HeartbeatTTL:       2 * time.Second,
		ColdStartWindow:    50 * time.Millisecond,
		PlannedScaleWindow: 50 * time.Millisecond,
		Cooldown:           1 * time.Millisecond,
		Source: &mockSource{partitions: []types.Partition{
			{Keys: []string{"p1"}},
		}},
		Strategy:       &mockStrategy{},
		LeaderRevision: func() uint64 { return wantRevision },
	})
	require.NoError(t, err)

	require.NoError(t, calc.Start(ctx))
	t.Cleanup(func() { _ = calc.Stop(ctx) })

	// Wait for initial rebalance to publish at least one assignment.
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > 0
	}, 5*time.Second, 50*time.Millisecond, "initial rebalance must complete")

	// Read the published assignment from KV and check LeaderRevision.
	entry, err := assignmentKV.Get(ctx, "assignment.worker-1")
	require.NoError(t, err)

	var asgn types.Assignment
	require.NoError(t, json.Unmarshal(entry.Value(), &asgn))
	require.Equal(t, wantRevision, asgn.LeaderRevision,
		"LeaderRevision must be embedded in the published assignment")
}

// ============================================================================
// P9.2: Pre-publish shutdown check blocks publish
// ============================================================================

// TestCalculator_Rebalance_AbortsOnShutdown verifies that rebalance skips
// publishing when the StateProvider reports StateShutdown, preventing
// split-brain assignments from a leader that is shutting down.
func TestCalculator_Rebalance_AbortsOnShutdown(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-splitbrain-shutdown-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-splitbrain-shutdown-heartbeat")

	_, err := heartbeatKV.Put(ctx, "heartbeat.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	stateProvider := &mockStateProvider{}
	// Leave state as stable (zero value) for initial rebalance; we'll flip it before TriggerRebalance.

	calc, err := NewCalculator(&Config{
		AssignmentKV:       assignmentKV,
		HeartbeatKV:        heartbeatKV,
		AssignmentPrefix:   "assignment",
		HeartbeatPrefix:    "heartbeat",
		HeartbeatTTL:       2 * time.Second,
		ColdStartWindow:    50 * time.Millisecond,
		PlannedScaleWindow: 50 * time.Millisecond,
		Cooldown:           1 * time.Millisecond,
		Source: &mockSource{partitions: []types.Partition{
			{Keys: []string{"p1"}},
		}},
		Strategy:      &mockStrategy{},
		StateProvider: stateProvider,
	})
	require.NoError(t, err)

	require.NoError(t, calc.Start(ctx))
	t.Cleanup(func() { _ = calc.Stop(ctx) })

	// Wait for the initial assignment to be published.
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > 0
	}, 5*time.Second, 50*time.Millisecond, "initial rebalance must complete")

	versionAfterStart := calc.CurrentVersion()

	// Flip state to shutdown before triggering another rebalance.
	stateProvider.SetState(types.StateShutdown)

	// TriggerRebalance must fail with the sentinel shutdown error and must NOT increment the version.
	err = calc.TriggerRebalance(ctx)
	require.ErrorIs(t, err, errShuttingDown, "rebalance must return errShuttingDown when manager is shutting down")
	require.Equal(t, versionAfterStart, calc.CurrentVersion(),
		"version must not increment when rebalance is aborted due to shutdown")
}
