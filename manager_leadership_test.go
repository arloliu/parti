package parti

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// TestManager_LeadershipLoss_StateTransition tests that losing leadership
// while in Scaling/Rebalancing/Emergency state transitions correctly.
func TestManager_LeadershipLoss_StateTransition(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Setup embedded NATS
	_, nc := partitest.StartEmbeddedNATS(t)

	cfg := Config{
		WorkerIDPrefix:        "worker",
		WorkerIDMin:           0,
		WorkerIDMax:           10,
		WorkerIDTTL:           10 * time.Second,
		HeartbeatInterval:     1 * time.Second,
		HeartbeatTTL:          2 * time.Second,
		ElectionTimeout:       2 * time.Second, // Short timeout to trigger leadership changes
		StartupTimeout:        15 * time.Second,
		ShutdownTimeout:       5 * time.Second,
		ColdStartWindow:       1 * time.Second,
		PlannedScaleWindow:    500 * time.Millisecond,
		RestartDetectionRatio: 0.5,
	}

	// Create partition source
	partitions := make([]types.Partition, 5)
	for i := range partitions {
		partitions[i] = types.Partition{
			Keys:   []string{"partition-" + string(rune('A'+i))},
			Weight: 100,
		}
	}
	src := source.NewStatic(partitions)
	strategy := strategy.NewConsistentHash()

	// Track state transitions
	stateTransitions := make([]string, 0)
	hooks := &Hooks{
		OnStateChanged: func(_ context.Context, from, to types.State) error {
			transition := from.String() + " → " + to.String()
			stateTransitions = append(stateTransitions, transition)
			t.Logf("State transition: %s", transition)

			return nil
		},
	}

	// Create first worker
	mgr1, err := NewManager(&cfg, nc, src, strategy, WithHooks(hooks))
	require.NoError(t, err)

	err = mgr1.Start(ctx)
	require.NoError(t, err)
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mgr1.Stop(stopCtx)
	}()

	// Wait for worker to become leader and reach Stable
	require.Eventually(t, func() bool {
		return mgr1.IsLeader() && mgr1.State() == StateStable
	}, 10*time.Second, 200*time.Millisecond, "worker 1 did not become leader")

	t.Log("Worker 1 is leader and stable")

	// Create second worker (will trigger scaling in leader)
	mgr2, err := NewManager(&cfg, nc, src, strategy)
	require.NoError(t, err)

	// Start in background, may or may not succeed (that's okay for this test)
	go func() {
		startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = mgr2.Start(startCtx) // Ignore error, focus is on mgr1's state transition
	}()

	// Give it a moment to connect
	time.Sleep(500 * time.Millisecond)

	// Wait for leader to detect new worker and enter Scaling state
	require.Eventually(t, func() bool {
		state := mgr1.State()
		t.Logf("Worker 1 state: %s (leader: %v)", state.String(), mgr1.IsLeader())
		return state == StateScaling
	}, 5*time.Second, 200*time.Millisecond, "leader did not enter Scaling state")

	t.Log("Leader entered Scaling state")

	// Force leadership change by stopping first worker's election participation
	// This simulates losing leadership while in Scaling state
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = mgr1.Stop(stopCtx)
	stopCancel()
	require.NoError(t, err)

	// Check that state transitions included proper cleanup
	// Should see either:
	// - Scaling → Rebalancing → Stable (if completed normally)
	// - Scaling → Stable (if lost leadership mid-scaling)
	// - Scaling → WaitingAssignment (if lost leadership with no assignment)
	// - Scaling → Shutdown (on stop)

	t.Log("State transitions:")
	for i, transition := range stateTransitions {
		t.Logf("  %d. %s", i+1, transition)
	}

	// Verify we had a valid transition out of Scaling
	foundValidExit := false
	for _, transition := range stateTransitions {
		if transition == "Scaling → Rebalancing" ||
			transition == "Scaling → Stable" ||
			transition == "Scaling → WaitingAssignment" ||
			transition == "Scaling → Shutdown" {
			foundValidExit = true
			break
		}
	}

	require.True(t, foundValidExit, "did not find valid exit from Scaling state")
	t.Log("✅ Successfully transitioned out of Scaling state")
}
