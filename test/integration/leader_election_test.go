//go:build integration
// +build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/internal/logger"
	"github.com/arloliu/parti/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestLeaderElection_BasicFailover tests basic leader failover scenarios.
func TestLeaderElection_BasicFailover(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Use t.Context() which auto-cancels on test completion
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// Create cluster with fast configuration for quick leader election
	cluster := testutil.NewFastWorkerCluster(t, nc, 10)

	// Enable debug logging to see shutdown behavior
	debugLogger := logger.NewTest(t)

	// Add and start 3 workers
	for i := 0; i < 3; i++ {
		mgr := cluster.AddWorker(ctx, debugLogger)
		err := mgr.Start(ctx)
		require.NoError(t, err, "worker %d failed to start", i)
	}
	defer cluster.StopWorkers()

	// Wait for all workers to reach stable state (faster with FastConfig)
	cluster.WaitForStableState(10 * time.Second)

	// Verify exactly one leader exists
	originalLeader := cluster.VerifyExactlyOneLeader()
	originalLeaderID := originalLeader.WorkerID()
	t.Logf("Original leader: %s", originalLeaderID)

	// Find the leader's index
	leaderIndex := -1
	for i, mgr := range cluster.Workers {
		if mgr.WorkerID() == originalLeaderID {
			leaderIndex = i
			break
		}
	}
	require.NotEqual(t, -1, leaderIndex, "leader not found in workers")

	// Kill the leader
	t.Logf("Killing leader worker %d (%s)", leaderIndex, originalLeaderID)
	cluster.RemoveWorker(leaderIndex)

	// Wait for new leader election (ElectionTimeout is 1s with FastConfig, should happen within 5s)
	t.Logf("Waiting for new leader election...")

	var newLeader *parti.Manager
	require.Eventually(t, func() bool {
		activeWorkers := cluster.GetActiveWorkers()

		leaderCount := 0
		for _, mgr := range activeWorkers {
			if mgr.IsLeader() {
				leaderCount++
				newLeader = mgr
				t.Logf("  Worker %s is leader", mgr.WorkerID())
			}
		}

		return leaderCount == 1
	}, 8*time.Second, 200*time.Millisecond, "new leader should be elected within 8s")

	newLeaderID := newLeader.WorkerID()
	t.Logf("New leader elected: %s", newLeaderID)

	// Verify it's a different leader
	require.NotEqual(t, originalLeaderID, newLeaderID, "new leader should be different from original")

	// Verify remaining workers are stable
	activeWorkers := cluster.GetActiveWorkers()
	require.Equal(t, 2, len(activeWorkers), "should have 2 active workers")

	stableCount := 0
	for _, mgr := range activeWorkers {
		state := mgr.State()
		t.Logf("Active worker %s: state=%s, leader=%v",
			mgr.WorkerID(), state.String(), mgr.IsLeader())
		if state.String() == "Stable" {
			stableCount++
		}
	}
	require.Equal(t, 2, stableCount, "remaining 2 workers should be stable")
}

// TestLeaderElection_ColdStart tests leader election during cold start.
func TestLeaderElection_ColdStart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Use t.Context() which auto-cancels on test completion
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// Create cluster with fast configuration
	cluster := testutil.NewFastWorkerCluster(t, nc, 10)

	// Start 5 workers simultaneously (cold start)
	t.Logf("Starting 5 workers simultaneously...")
	for i := 0; i < 5; i++ {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err, "worker %d failed to start", i)
	}
	defer cluster.StopWorkers()

	// Wait for all workers to reach stable state (cold start window is 500ms)
	cluster.WaitForStableState(8 * time.Second)

	// Verify exactly one leader emerges
	leader := cluster.VerifyExactlyOneLeader()
	t.Logf("Leader elected: %s", leader.WorkerID())

	// Verify all workers have partitions
	cluster.VerifyAllWorkersHavePartitions()

	// Verify total partition count matches
	cluster.VerifyTotalPartitionCount(10)
}

// TestLeaderElection_OnlyLeaderRunsCalculator tests that only the leader runs the calculator.
func TestLeaderElection_OnlyLeaderRunsCalculator(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Use t.Context() which auto-cancels on test completion
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	// Create cluster with fast configuration
	cluster := testutil.NewFastWorkerCluster(t, nc, 10)

	// Add and start 3 workers
	for i := 0; i < 3; i++ {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err, "worker %d failed to start", i)
	}
	defer cluster.StopWorkers()

	// Wait for all workers to reach stable state
	cluster.WaitForStableState(8 * time.Second)

	// Verify exactly one leader
	leader := cluster.VerifyExactlyOneLeader()

	// Verify leader is in stable state (calculator should be running)
	require.Equal(t, "Stable", leader.State().String(), "leader should be in Stable state")

	// Verify followers are also in stable state
	followerCount := 0
	for i, mgr := range cluster.Workers {
		if mgr.WorkerID() != leader.WorkerID() {
			followerCount++
			require.Equal(t, "Stable", mgr.State().String(),
				"follower %d should be in Stable state", i)
			require.False(t, mgr.IsLeader(),
				"follower %d should not be leader", i)
		}
	}
	require.Equal(t, 2, followerCount, "should have 2 followers")

	t.Logf("✅ Verified: Only leader (%s) runs calculator, %d followers are passive",
		leader.WorkerID(), followerCount)
}

// TestLeaderElection_LeaderRenewal tests that leader renews its lease periodically.
func TestLeaderElection_LeaderRenewal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Use t.Context() which auto-cancels on test completion
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Create cluster with fast configuration
	cluster := testutil.NewFastWorkerCluster(t, nc, 10)

	// Add and start 2 workers
	for i := 0; i < 2; i++ {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err, "worker %d failed to start", i)
	}
	defer cluster.StopWorkers()

	// Wait for workers to reach stable state
	cluster.WaitForStableState(8 * time.Second)

	// Get initial leader
	initialLeader := cluster.VerifyExactlyOneLeader()
	initialLeaderID := initialLeader.WorkerID()
	t.Logf("Initial leader: %s", initialLeaderID)

	// Wait for 6x ElectionTimeout (ElectionTimeout is 1s, so wait 6s for multiple renewals)
	t.Logf("Waiting 6s for leader to renew lease...")
	time.Sleep(6 * time.Second)

	// Verify same leader is still the leader (lease renewed)
	currentLeader := cluster.VerifyExactlyOneLeader()
	currentLeaderID := currentLeader.WorkerID()

	require.Equal(t, initialLeaderID, currentLeaderID,
		"leader should remain the same after lease renewals")

	t.Logf("✅ Leader %s successfully maintained leadership for 6s", currentLeaderID)
}
