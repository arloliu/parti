package assignment

import (
	"context"
	"testing"
	"time"

	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

func TestCalculator_detectRebalanceType_ColdStart(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-calc-coldstart")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(kv, "test-assignment", source, strategy, "test-hb", 10*time.Second)

	// Cold start: 0 → 3 workers
	currentWorkers := map[string]bool{
		"worker-0": true,
		"worker-1": true,
		"worker-2": true,
	}

	reason, window := calc.detectRebalanceType(currentWorkers)

	require.Equal(t, "cold_start", reason)
	require.Equal(t, calc.coldStartWindow, window)
	require.Equal(t, 30*time.Second, window)
}

func TestCalculator_detectRebalanceType_PlannedScale(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-calc-plannedscale")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(kv, "test-assignment", source, strategy, "test-hb", 10*time.Second)

	// Set up previous workers
	calc.lastWorkers = map[string]bool{
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

	reason, window := calc.detectRebalanceType(currentWorkers)

	require.Equal(t, "planned_scale", reason)
	require.Equal(t, calc.plannedScaleWin, window)
	require.Equal(t, 10*time.Second, window)
}

func TestCalculator_detectRebalanceType_Emergency(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-calc-emergency")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(kv, "test-assignment", source, strategy, "test-hb", 10*time.Second)

	// Set up previous workers
	calc.lastWorkers = map[string]bool{
		"worker-0": true,
		"worker-1": true,
		"worker-2": true,
	}

	// Emergency: 3 → 2 workers (worker-2 crashed)
	currentWorkers := map[string]bool{
		"worker-0": true,
		"worker-1": true,
	}

	reason, window := calc.detectRebalanceType(currentWorkers)

	require.Equal(t, "emergency", reason)
	require.Equal(t, time.Duration(0), window)
}

func TestCalculator_detectRebalanceType_Restart(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-calc-restart")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(kv, "test-assignment", source, strategy, "test-hb", 10*time.Second)
	calc.SetRestartDetectionRatio(0.5) // 50% threshold

	// Set up previous workers (10 workers)
	calc.lastWorkers = map[string]bool{
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

	// Restart: 10 → 10 workers, but 6 workers changed (60% > 50% threshold)
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

	reason, window := calc.detectRebalanceType(currentWorkers)

	require.Equal(t, "restart", reason)
	require.Equal(t, calc.coldStartWindow, window)
	require.Equal(t, 30*time.Second, window)
}

func TestCalculator_detectRebalanceType_MultipleWorkersCrashed(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-calc-multicrash")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(kv, "test-assignment", source, strategy, "test-hb", 10*time.Second)

	// Set up previous workers
	calc.lastWorkers = map[string]bool{
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

	reason, window := calc.detectRebalanceType(currentWorkers)

	require.Equal(t, "emergency", reason)
	require.Equal(t, time.Duration(0), window)
}

func TestCalculator_StateTransitions_GetState(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-calc-getstate")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(kv, "test-assignment", source, strategy, "test-hb", 10*time.Second)

	// Initial state should be Idle
	require.Equal(t, "Idle", calc.GetState())

	// Transition to Scaling
	ctx := context.Background()
	calc.enterScalingState("cold_start", 100*time.Millisecond, ctx)
	require.Equal(t, "Scaling", calc.GetState())

	// Wait for automatic transition to Rebalancing
	time.Sleep(150 * time.Millisecond)
	// Note: State might be Idle if rebalancing completed quickly
	state := calc.GetState()
	require.Contains(t, []string{"Rebalancing", "Idle"}, state)
}

func TestCalculator_StateTransitions_Scaling(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-calc-scaling")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(kv, "test-assignment", source, strategy, "test-hb", 10*time.Second)

	ctx := context.Background()

	// Enter scaling state
	calc.enterScalingState("cold_start", 50*time.Millisecond, ctx)

	require.Equal(t, "Scaling", calc.GetState())
	require.Equal(t, "cold_start", calc.scalingReason)

	// Wait for window to expire and transition to rebalancing
	time.Sleep(100 * time.Millisecond)

	// Should have transitioned to Idle after rebalancing
	require.Eventually(t, func() bool {
		return calc.GetState() == "Idle"
	}, 1*time.Second, 50*time.Millisecond)
}

func TestCalculator_StateTransitions_Emergency(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-calc-emergency-state")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(kv, "test-assignment", source, strategy, "test-hb", 10*time.Second)

	ctx := context.Background()

	// Enter emergency state
	calc.enterEmergencyState(ctx)

	// Emergency state should quickly transition through Rebalancing to Idle
	require.Eventually(t, func() bool {
		return calc.GetState() == "Idle"
	}, 1*time.Second, 50*time.Millisecond)

	// Verify scaling reason was cleared
	calc.mu.RLock()
	reason := calc.scalingReason
	calc.mu.RUnlock()
	require.Equal(t, "", reason) // Should be cleared after returning to idle
}

func TestCalculator_StateTransitions_ReturnToIdle(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-calc-returnidle")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(kv, "test-assignment", source, strategy, "test-hb", 10*time.Second)

	// Manually set state to Rebalancing
	calc.calcState.Store(int32(calcStateRebalancing))
	calc.scalingReason = "test_reason"

	require.Equal(t, "Rebalancing", calc.GetState())

	// Return to idle
	calc.returnToIdleState()

	require.Equal(t, "Idle", calc.GetState())
	require.Equal(t, "", calc.scalingReason)
}

func TestCalculator_StateTransitions_PreventsConcurrentRebalance(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-calc-preventconcurrent")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(kv, "test-assignment", source, strategy, "test-hb", 10*time.Second)
	calc.SetCooldown(100 * time.Millisecond)

	ctx := context.Background()

	// Start calculator to set up KV buckets
	err := calc.Start(ctx)
	require.NoError(t, err)
	defer calc.Stop()

	// Publish some heartbeats
	hbKV := partitest.CreateJetStreamKV(t, nc, "test-hb")

	_, err = hbKV.Put(ctx, "worker-0", []byte("alive"))
	require.NoError(t, err)

	// Wait for initial assignment
	time.Sleep(200 * time.Millisecond)

	// Manually enter scaling state
	calc.enterScalingState("test", 200*time.Millisecond, ctx)

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
	require.Equal(t, "Scaling", calc.GetState())
}

func TestCalculator_StateTransitions_CooldownPreventsRebalance(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "test-calc-cooldown-state")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(kv, "test-assignment", source, strategy, "test-hb", 10*time.Second)
	calc.SetCooldown(500 * time.Millisecond)

	ctx := context.Background()

	// Start calculator
	err := calc.Start(ctx)
	require.NoError(t, err)
	defer calc.Stop()

	// Publish heartbeats
	hbKV := partitest.CreateJetStreamKV(t, nc, "test-hb")

	_, err = hbKV.Put(ctx, "worker-0", []byte("alive"))
	require.NoError(t, err)

	// Wait for initial assignment
	time.Sleep(200 * time.Millisecond)

	// Set up lastWorkers for comparison
	calc.mu.Lock()
	calc.lastWorkers = map[string]bool{"worker-0": true}
	calc.lastRebalance = time.Now() // Set recent rebalance
	calc.mu.Unlock()

	// Try to trigger rebalance during cooldown
	_, err = hbKV.Put(ctx, "worker-1", []byte("alive"))
	require.NoError(t, err)

	err = calc.checkForChanges(ctx)
	require.NoError(t, err)

	// Should remain in Idle (cooldown prevented rebalance)
	require.Equal(t, "Idle", calc.GetState())
}

func TestCalculator_StateString(t *testing.T) {
	tests := []struct {
		name  string
		state calculatorState
		want  string
	}{
		{"idle", calcStateIdle, "Idle"},
		{"scaling", calcStateScaling, "Scaling"},
		{"rebalancing", calcStateRebalancing, "Rebalancing"},
		{"emergency", calcStateEmergency, "Emergency"},
		{"unknown", calculatorState(999), "Unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.state.String())
		})
	}
}
