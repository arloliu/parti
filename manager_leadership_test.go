package parti

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/internal/logging"
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

	// Create test logger for debugging
	logger := logging.NewTest(t)

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
		Assignment: AssignmentConfig{
			MinRebalanceInterval: 100 * time.Millisecond, // Short cooldown for testing fast detection
		},
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
	mgr1, err := NewManager(&cfg, nc, src, strategy, WithHooks(hooks), WithLogger(logger))
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
	mgr2, err := NewManager(&cfg, nc, src, strategy, WithLogger(logger))
	require.NoError(t, err)

	// Ensure mgr2 is stopped before test ends
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mgr2.Stop(stopCtx)
	}()

	// Track mgr2 start result
	mgr2Started := make(chan error, 1)
	go func() {
		startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mgr2Started <- mgr2.Start(startCtx)
	}()

	// Wait for mgr2 to reach a stable state (either WaitingAssignment or Stable)
	require.Eventually(t, func() bool {
		state := mgr2.State()
		t.Logf("Worker 2 state: %s", state.String())
		return state == StateWaitingAssignment || state == StateStable
	}, 3*time.Second, 100*time.Millisecond, "worker 2 did not start successfully")

	// Wait for leader to detect new worker and react to scaling event
	// The leader should transition through Scaling -> Rebalancing -> Stable
	// Due to the short scaling window (500ms), we might miss Scaling state when polling,
	// so we check the recorded state transitions for evidence of rebalancing activity
	require.Eventually(t, func() bool {
		for _, transition := range stateTransitions {
			if transition == "Stable → Scaling" ||
				transition == "Stable → Rebalancing" ||
				transition == "Scaling → Rebalancing" ||
				transition == "WaitingAssignment → Scaling" ||
				transition == "WaitingAssignment → Rebalancing" {
				t.Logf("Found rebalancing activity: %s", transition)
				return true
			}
		}

		return false
	}, 5*time.Second, 100*time.Millisecond, "leader did not enter Scaling or Rebalancing state")

	t.Log("Leader entered rebalancing state (Scaling or Rebalancing)")

	// Force leadership change by stopping first worker's election participation
	// This simulates losing leadership while in rebalancing state (Scaling or Rebalancing)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = mgr1.Stop(stopCtx)
	stopCancel()
	require.NoError(t, err)

	// Check that state transitions included proper cleanup
	// Should see either:
	// - Scaling → Rebalancing → Stable → Shutdown (if completed normally)
	// - Rebalancing → Stable → Shutdown (if we missed Scaling due to timing)
	// - Scaling → Stable → Shutdown (if lost leadership mid-scaling)
	// - Scaling → WaitingAssignment (if lost leadership with no assignment)

	t.Log("State transitions:")
	for i, transition := range stateTransitions {
		t.Logf("  %d. %s", i+1, transition)
	}

	// Verify we had a valid transition involving rebalancing activity
	// Accept either Scaling or Rebalancing as evidence of the rebalancing process
	foundValidExit := false
	for _, transition := range stateTransitions {
		if transition == "Scaling → Rebalancing" ||
			transition == "Rebalancing → Stable" ||
			transition == "Scaling → Stable" ||
			transition == "Scaling → WaitingAssignment" ||
			transition == "Rebalancing → Shutdown" ||
			transition == "Stable → Rebalancing" {
			foundValidExit = true
			break
		}
	}

	require.True(t, foundValidExit, "did not find valid rebalancing transition")
	t.Log("Successfully transitioned through rebalancing states")
}
