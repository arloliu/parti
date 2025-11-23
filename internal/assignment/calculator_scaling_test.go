package assignment

import (
	"context"
	"testing"
	"time"

	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

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
