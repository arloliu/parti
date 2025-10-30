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
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-coldstart-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-coldstart-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(assignmentKV, heartbeatKV, "test-assignment", source, strategy, "test-hb", 10*time.Second, 5*time.Second)

	// Cold start: 0 → 3 workers
	lastWorkers := map[string]bool{} // Empty - cold start
	currentWorkers := map[string]bool{
		"worker-0": true,
		"worker-1": true,
		"worker-2": true,
	}

	reason, window := calc.detectRebalanceType(lastWorkers, currentWorkers)

	require.Equal(t, "cold_start", reason)
	require.Equal(t, calc.coldStartWindow, window)
	require.Equal(t, 30*time.Second, window)
}

func TestCalculator_detectRebalanceType_PlannedScale(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-plannedscale-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-plannedscale-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(assignmentKV, heartbeatKV, "test-assignment", source, strategy, "test-hb", 10*time.Second, 5*time.Second)

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
	require.Equal(t, calc.plannedScaleWin, window)
	require.Equal(t, 10*time.Second, window)
}

func TestCalculator_detectRebalanceType_Emergency(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-emergency-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-emergency-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	// Use shorter intervals for faster test: HeartbeatInterval=1s, GracePeriod=1s (instead of default 0.75*interval)
	calc := NewCalculator(assignmentKV, heartbeatKV, "test-assignment", source, strategy, "test-hb", 1*time.Second, 1*time.Second)

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

	calc := NewCalculator(assignmentKV, heartbeatKV, "test-assignment", source, strategy, "test-hb", 10*time.Second, 5*time.Second)
	calc.SetRestartDetectionRatio(0.5) // 50% threshold (not used anymore - removed restart detection)

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
	require.Equal(t, calc.plannedScaleWin, window)
	require.Equal(t, 10*time.Second, window)
}

func TestCalculator_detectRebalanceType_MultipleWorkersCrashed(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-multicrash-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-multicrash-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	// Use shorter intervals for faster test: HeartbeatInterval=1s, GracePeriod=1s (instead of default 0.75*interval)
	calc := NewCalculator(assignmentKV, heartbeatKV, "test-assignment", source, strategy, "test-hb", 1*time.Second, 1*time.Second)

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

	calc := NewCalculator(assignmentKV, heartbeatKV, "test-assignment", source, strategy, "test-hb", 10*time.Second, 5*time.Second)

	// Initial state should be Idle
	require.Equal(t, types.CalcStateIdle, calc.GetState())

	// Transition to Scaling
	ctx := context.Background()
	calc.enterScalingState(ctx, "cold_start", 100*time.Millisecond)
	require.Equal(t, types.CalcStateScaling, calc.GetState())

	// Wait for automatic transition to Rebalancing
	time.Sleep(150 * time.Millisecond)
	// Note: State might be Idle if rebalancing completed quickly
	state := calc.GetState()
	require.Contains(t, []types.CalculatorState{types.CalcStateRebalancing, types.CalcStateIdle}, state)
}

func TestCalculator_StateTransitions_Scaling(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-scaling-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-scaling-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(assignmentKV, heartbeatKV, "test-assignment", source, strategy, "test-hb", 10*time.Second, 5*time.Second)

	ctx := context.Background()

	// Enter scaling state
	calc.enterScalingState(ctx, "cold_start", 50*time.Millisecond)

	require.Equal(t, types.CalcStateScaling, calc.GetState())
	require.Equal(t, "cold_start", calc.GetScalingReason())

	// Wait for window to expire and transition to rebalancing
	time.Sleep(100 * time.Millisecond)

	// Should have transitioned to Idle after rebalancing
	require.Eventually(t, func() bool {
		return calc.GetState() == types.CalcStateIdle
	}, 1*time.Second, 50*time.Millisecond)
}

func TestCalculator_StateTransitions_Emergency(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-emergency-state-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-emergency-state-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(assignmentKV, heartbeatKV, "test-assignment", source, strategy, "test-hb", 10*time.Second, 5*time.Second)

	ctx := context.Background()

	// Enter emergency state
	calc.enterEmergencyState(ctx)

	// Emergency state should quickly transition through Rebalancing to Idle
	require.Eventually(t, func() bool {
		return calc.GetState() == types.CalcStateIdle
	}, 1*time.Second, 50*time.Millisecond)

	// Verify scaling reason was cleared
	require.Equal(t, "", calc.GetScalingReason()) // Should be cleared after returning to idle
}

func TestCalculator_StateTransitions_ReturnToIdle(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-returnidle-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-returnidle-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(assignmentKV, heartbeatKV, "test-assignment", source, strategy, "test-hb", 10*time.Second, 5*time.Second)

	// Manually set state to Rebalancing
	calc.calcState.Store(int32(types.CalcStateRebalancing))
	calc.mu.Lock()
	calc.scalingReason = "test_reason"
	calc.mu.Unlock()

	require.Equal(t, types.CalcStateRebalancing, calc.GetState())

	// Return to idle
	calc.returnToIdleState()

	require.Equal(t, types.CalcStateIdle, calc.GetState())
	require.Equal(t, "", calc.GetScalingReason())
}

func TestCalculator_StateTransitions_PreventsConcurrentRebalance(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-preventconcurrent-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-preventconcurrent-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(assignmentKV, heartbeatKV, "test-assignment", source, strategy, "test-hb", 1*time.Second, 500*time.Millisecond)
	calc.SetCooldown(100 * time.Millisecond)
	calc.SetStabilizationWindows(500*time.Millisecond, 300*time.Millisecond) // Fast windows for test

	ctx := context.Background()

	// Start calculator to set up KV buckets
	err := calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop() }()

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

func TestCalculator_StateTransitions_CooldownPreventsRebalance(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-cooldown-state-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-cooldown-state-heartbeat")

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}}
	strategy := &mockStrategy{}

	calc := NewCalculator(assignmentKV, heartbeatKV, "test-assignment", source, strategy, "test-hb", 1*time.Second, 500*time.Millisecond)
	calc.SetCooldown(500 * time.Millisecond)
	calc.SetStabilizationWindows(500*time.Millisecond, 300*time.Millisecond) // Fast windows for test

	ctx := context.Background()

	// Start calculator
	err := calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop() }()

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
	require.Equal(t, types.CalcStateIdle, calc.GetState())
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
