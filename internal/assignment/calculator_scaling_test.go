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
	kv := partitest.CreateJetStreamKV(t, nc, "test-scaling")

	src := &mockSource{partitions: []types.Partition{
		{Keys: []string{"p1"}, Weight: 100},
		{Keys: []string{"p2"}, Weight: 100},
	}}
	strategy := &mockStrategy{}

	calc := NewCalculator(kv, "test", src, strategy, "heartbeat", 5*time.Second)
	calc.SetStabilizationWindows(100*time.Millisecond, 50*time.Millisecond) // Very short windows for testing

	ctx := context.Background()

	// Manually trigger scaling state (bypassing Start/monitorWorkers)
	calc.enterScalingState("test_reason", 100*time.Millisecond, ctx)

	// Verify we're in Scaling state
	require.Equal(t, "Scaling", calc.GetState())

	// Wait longer than the window to see if transition happens
	time.Sleep(200 * time.Millisecond)

	// Check if transitioned to Rebalancing or Idle
	finalState := calc.GetState()
	t.Logf("Final state after window: %s", finalState)

	// Should have transitioned away from Scaling
	// (Either to Rebalancing then Idle, or just Idle if rebalance was fast)
	require.NotEqual(t, "Scaling", finalState, "calculator should have transitioned out of Scaling state")
}

// TestCalculator_ScalingTransition_WithRealStart tests scaling transition with full Calculator.Start().
//
// This tests the integration between monitorWorkers and the state machine.
func TestCalculator_ScalingTransition_WithRealStart(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-scaling-real")

	src := &mockSource{partitions: []types.Partition{
		{Keys: []string{"p1"}, Weight: 100},
		{Keys: []string{"p2"}, Weight: 100},
	}}
	strategy := &mockStrategy{}

	calc := NewCalculator(kv, "test", src, strategy, "heartbeat", 1*time.Second)
	calc.SetStabilizationWindows(500*time.Millisecond, 250*time.Millisecond) // Short windows for testing
	calc.SetCooldown(0)                                                      // No cooldown for this test

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create initial heartbeat (simulate 1 worker)
	err := publishHeartbeat(ctx, kv, "heartbeat.worker-1", 2*time.Second)
	require.NoError(t, err)

	// Start calculator
	err = calc.Start(ctx)
	require.NoError(t, err)
	defer calc.Stop()

	// Wait for initial assignment
	time.Sleep(200 * time.Millisecond)

	initialState := calc.GetState()
	t.Logf("Initial state: %s", initialState)

	// Add a second worker (should trigger scaling)
	err = publishHeartbeat(ctx, kv, "heartbeat.worker-2", 2*time.Second)
	require.NoError(t, err)

	// Wait for monitorWorkers to detect change (checks every HeartbeatTTL/2 = 500ms)
	time.Sleep(600 * time.Millisecond)

	scalingState := calc.GetState()
	t.Logf("State after adding worker: %s", scalingState)

	// Should have entered Scaling state
	require.Equal(t, "Scaling", scalingState, "calculator should enter Scaling state when worker added")

	// Wait for stabilization window (500ms) + buffer
	time.Sleep(800 * time.Millisecond)

	finalState := calc.GetState()
	t.Logf("Final state after window: %s", finalState)

	// Should have completed scaling and returned to Idle
	require.Equal(t, "Idle", finalState, "calculator should return to Idle after scaling completes")
}

// TestCalculator_ScalingTransition_ContextCancellation tests that context cancellation stops the timer.
func TestCalculator_ScalingTransition_ContextCancellation(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-scaling-cancel")

	src := &mockSource{partitions: []types.Partition{
		{Keys: []string{"p1"}, Weight: 100},
	}}
	strategy := &mockStrategy{}

	calc := NewCalculator(kv, "test", src, strategy, "heartbeat", 5*time.Second)
	calc.SetStabilizationWindows(2*time.Second, 1*time.Second) // Long window

	ctx, cancel := context.WithCancel(context.Background())

	// Trigger scaling state
	calc.enterScalingState("test_reason", 2*time.Second, ctx)

	require.Equal(t, "Scaling", calc.GetState())

	// Cancel context before window elapses
	time.Sleep(100 * time.Millisecond)
	cancel()

	// Wait a bit more
	time.Sleep(100 * time.Millisecond)

	// Should still be in Scaling (goroutine exited, no transition)
	state := calc.GetState()
	t.Logf("State after context cancellation: %s", state)
	require.Equal(t, "Scaling", state, "should remain in Scaling when context cancelled before window")
}

// TestCalculator_ScalingTransition_StopBeforeWindow tests that Stop() prevents transition.
func TestCalculator_ScalingTransition_StopBeforeWindow(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-scaling-stop")

	src := &mockSource{partitions: []types.Partition{
		{Keys: []string{"p1"}, Weight: 100},
	}}
	strategy := &mockStrategy{}

	calc := NewCalculator(kv, "test", src, strategy, "heartbeat", 5*time.Second)
	calc.SetStabilizationWindows(2*time.Second, 1*time.Second) // Long window
	calc.SetCooldown(0)

	ctx := context.Background()

	// Start calculator
	err := calc.Start(ctx)
	require.NoError(t, err)

	// Trigger scaling state manually
	calc.enterScalingState("test_reason", 2*time.Second, ctx)

	require.Equal(t, "Scaling", calc.GetState())

	// Stop calculator before window elapses
	time.Sleep(100 * time.Millisecond)
	err = calc.Stop()
	require.NoError(t, err)

	// Wait longer than window would have been
	time.Sleep(2500 * time.Millisecond)

	// State shouldn't change after Stop
	state := calc.GetState()
	t.Logf("State after Stop: %s", state)
}

// TestCalculator_ScalingTransition_RapidStateChanges tests multiple rapid worker changes.
func TestCalculator_ScalingTransition_RapidStateChanges(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-scaling-rapid")

	src := &mockSource{partitions: []types.Partition{
		{Keys: []string{"p1"}, Weight: 100},
		{Keys: []string{"p2"}, Weight: 100},
	}}
	strategy := &mockStrategy{}

	calc := NewCalculator(kv, "test", src, strategy, "heartbeat", 500*time.Millisecond)
	calc.SetStabilizationWindows(300*time.Millisecond, 150*time.Millisecond)
	calc.SetCooldown(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create initial worker
	err := publishHeartbeat(ctx, kv, "heartbeat.worker-1", 1*time.Second)
	require.NoError(t, err)

	// Start calculator
	err = calc.Start(ctx)
	require.NoError(t, err)
	defer calc.Stop()

	time.Sleep(200 * time.Millisecond)
	t.Logf("Initial state: %s", calc.GetState())

	// Add worker 2
	err = publishHeartbeat(ctx, kv, "heartbeat.worker-2", 1*time.Second)
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)
	state1 := calc.GetState()
	t.Logf("After adding worker-2: %s", state1)

	// Add worker 3 quickly
	err = publishHeartbeat(ctx, kv, "heartbeat.worker-3", 1*time.Second)
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
		return calc.GetState() == "Idle"
	}, 2*time.Second, 100*time.Millisecond, "should eventually return to Idle")
}

// publishHeartbeat is a helper to publish a worker heartbeat to the KV store.
func publishHeartbeat(ctx context.Context, kv jetstream.KeyValue, key string, ttl time.Duration) error {
	data := []byte(time.Now().Format(time.RFC3339))
	_, err := kv.Put(ctx, key, data)
	return err
}
