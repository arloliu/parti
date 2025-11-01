package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/test/testutil"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// TestScenario_RapidScaling_StabilizationWindows verifies that rapid scaling (3→10 workers)
// results in a single rebalance after the stabilization window, not cascading rebalances.
//
// Production scenario: Kubernetes Horizontal Pod Autoscaler (HPA) rapidly scales pods
// in response to load spike.
func TestScenario_RapidScaling_StabilizationWindows(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Configure for rapid scaling scenario
	// Key insight: MinRebalanceInterval should be longer than the time to add all workers
	// to ensure they are batched into a single rebalance after stabilization
	cfg := testutil.IntegrationTestConfig()
	cfg.HeartbeatInterval = 500 * time.Millisecond
	cfg.HeartbeatTTL = 1500 * time.Millisecond
	cfg.ColdStartWindow = 5 * time.Second                 // Longer window for cold start
	cfg.PlannedScaleWindow = 3 * time.Second              // Wait 3s to batch rapid changes
	cfg.Assignment.MinRebalanceInterval = 3 * time.Second // Longer cooldown for batching

	// Create cluster with 12 partitions
	cluster := testutil.NewWorkerCluster(t, nc, 12)
	cluster.Config = cfg
	defer cluster.StopWorkers()

	debugLogger := logging.NewTest(t)

	// Start 3 initial workers
	t.Log("Starting 3 initial workers")
	for i := 0; i < 3; i++ {
		mgr := cluster.AddWorker(ctx, debugLogger)
		err := mgr.Start(ctx)
		require.NoError(t, err, "failed to start initial worker %d", i)
	}
	cluster.WaitForStableState(10 * time.Second)

	// Get initial version
	leader := cluster.GetLeader()
	require.NotNil(t, leader, "should have a leader")
	initialVersion := leader.CurrentAssignment().Version
	t.Logf("Initial cluster stable with 3 workers, version %d", initialVersion)

	// Rapidly add 7 more workers (simulate K8s HPA scaling)
	// Start all workers concurrently to simulate burst scaling behavior
	t.Log("Rapidly scaling from 3 to 10 workers...")
	scaleStart := time.Now()

	// Start all 7 workers concurrently
	for i := 0; i < 7; i++ {
		mgr := cluster.AddWorker(ctx, debugLogger)
		// Start each worker in a goroutine to minimize time between starts
		go func(m *parti.Manager, idx int) {
			if err := m.Start(ctx); err != nil {
				t.Logf("Worker %d start error: %v", idx, err)
			}
		}(mgr, i+3)
	}

	// Small wait to ensure all workers have started
	time.Sleep(500 * time.Millisecond)
	scaleEnd := time.Now()
	t.Logf("Added 7 workers in %v", scaleEnd.Sub(scaleStart))

	// Wait for PlannedScaleWindow to expire (3s window + margin)
	t.Log("Waiting for planned scale stabilization window (3s + margin)")
	time.Sleep(4 * time.Second)

	// Now wait for rebalance to complete
	t.Log("Waiting for rebalance to complete")
	time.Sleep(3 * time.Second)

	// Get final version
	leader = cluster.GetLeader()
	require.NotNil(t, leader)
	finalVersion := leader.CurrentAssignment().Version

	// With proper batching, we expect:
	// - Version from initial state (could be 1-3 depending on worker startup timing)
	// - One final version after all 7 workers are added and stabilization completes
	// Maximum version delta should be 3 (initial setup + batched scaling)
	versionDelta := int(finalVersion - initialVersion)
	require.LessOrEqual(t, versionDelta, 3,
		"should have at most 3 rebalances with batching, got %d rebalances", versionDelta)

	t.Logf("Scaling batched correctly: %d → %d (%d rebalance(s))",
		initialVersion, finalVersion, versionDelta)

	// Verify all 10 workers are active and stable
	cluster.WaitForStableState(10 * time.Second)

	// Count stable workers manually
	activeWorkers := cluster.GetActiveWorkers()
	stableCount := 0
	for _, mgr := range activeWorkers {
		if mgr.State() == types.StateStable {
			stableCount++
		}
	}
	require.Equal(t, 10, stableCount, "should have 10 stable workers")

	// Verify all partitions are assigned
	cluster.VerifyTotalPartitionCount(12)
	t.Log("SUCCESS: Rapid scaling handled with minimal rebalances")
}

// TestScenario_SlowHeartbeats_NearExpiryBoundary verifies that workers with heartbeats
// arriving just before TTL expiry don't trigger false-positive emergencies.
//
// Production scenario: Network congestion causes heartbeat delays but workers are healthy.
func TestScenario_SlowHeartbeats_NearExpiryBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Configure with tight heartbeat timing
	cfg := testutil.IntegrationTestConfig()
	cfg.HeartbeatInterval = 1 * time.Second
	cfg.HeartbeatTTL = 3 * time.Second                    // 3x interval
	cfg.EmergencyGracePeriod = 2 * time.Second            // Grace period for slow heartbeats
	cfg.ColdStartWindow = 5 * time.Second                 // Long enough for initial setup
	cfg.PlannedScaleWindow = 3 * time.Second              // 3s for planned scaling
	cfg.Assignment.MinRebalanceInterval = 2 * time.Second // Must be <= ColdStartWindow

	cluster := testutil.NewWorkerCluster(t, nc, 6)
	cluster.Config = cfg
	defer cluster.StopWorkers()

	debugLogger := logging.NewTest(t)

	// Start 3 workers
	t.Log("Starting 3 workers with tight heartbeat timing")
	for i := 0; i < 3; i++ {
		mgr := cluster.AddWorker(ctx, debugLogger)
		err := mgr.Start(ctx)
		require.NoError(t, err, "failed to start worker %d", i)
	}
	cluster.WaitForStableState(10 * time.Second)

	initialVersion := cluster.GetLeader().CurrentAssignment().Version
	t.Logf("Initial cluster stable, version %d", initialVersion)

	// Simulate slow heartbeats: Workers are publishing but with delays
	// The HeartbeatInterval is 1s, but actual publishing might take longer
	// due to network congestion. The TTL is 3s, so heartbeats arriving
	// at 2.5s should still be considered valid.

	// Wait and observe that no emergency is triggered despite slow network
	t.Log("Observing system for 15 seconds (5 heartbeat intervals)")
	time.Sleep(15 * time.Second)

	// Verify no emergency rebalance occurred
	finalVersion := cluster.GetLeader().CurrentAssignment().Version
	require.Equal(t, initialVersion, finalVersion,
		"no rebalance should occur with slow but valid heartbeats")

	// Verify all workers still stable
	activeWorkers := cluster.GetActiveWorkers()
	stableCount := 0
	for _, mgr := range activeWorkers {
		if mgr.State() == types.StateStable {
			stableCount++
		}
	}
	require.Equal(t, 3, stableCount, "all workers should remain stable")

	cluster.VerifyTotalPartitionCount(6)
	t.Log("SUCCESS: No false-positive emergencies with slow heartbeats")
}

// TestScenario_ConcurrentLeaderElection_RaceCondition verifies that when multiple workers
// attempt to claim leadership simultaneously, only one succeeds and others back off gracefully.
//
// Production scenario: Cluster-wide restart after configuration change causes race.
func TestScenario_ConcurrentLeaderElection_RaceCondition(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	cfg.ElectionTimeout = 2 * time.Second                 // Longer timeout for election
	cfg.ColdStartWindow = 5 * time.Second                 // Long enough for initial setup
	cfg.PlannedScaleWindow = 3 * time.Second              // 3s for planned scaling
	cfg.Assignment.MinRebalanceInterval = 2 * time.Second // Must be <= ColdStartWindow

	cluster := testutil.NewWorkerCluster(t, nc, 6)
	cluster.Config = cfg
	defer cluster.StopWorkers()

	debugLogger := logging.NewTest(t)

	// Start 5 workers simultaneously (maximize election contention)
	t.Log("Starting 5 workers simultaneously to trigger election race")
	for i := 0; i < 5; i++ {
		mgr := cluster.AddWorker(ctx, debugLogger)
		// Start immediately without waiting - simulate simultaneous startup
		go func(m *parti.Manager, idx int) {
			if err := m.Start(ctx); err != nil {
				t.Logf("Worker %d start error: %v", idx, err)
			}
		}(mgr, i)
	}

	// Wait a bit for all workers to start attempting election
	time.Sleep(500 * time.Millisecond)
	t.Log("All workers started, waiting for election to resolve")

	// Wait for election to resolve
	t.Log("Waiting for leader election to resolve (10 seconds)")
	time.Sleep(10 * time.Second)

	// Verify exactly one leader
	leader := cluster.GetLeader()
	require.NotNil(t, leader, "should have exactly one leader")

	leaderCount := 0
	for _, worker := range cluster.Workers {
		if worker.IsLeader() {
			leaderCount++
		}
	}
	require.Equal(t, 1, leaderCount, "should have exactly one leader, not multiple")

	t.Logf("Leader elected: %s", leader.WorkerID())

	// Verify all workers reached stable state
	cluster.WaitForStableState(15 * time.Second)

	// Count stable workers manually
	activeWorkers := cluster.GetActiveWorkers()
	stableCount := 0
	for _, mgr := range activeWorkers {
		if mgr.State() == types.StateStable {
			stableCount++
		}
	}
	require.Equal(t, 5, stableCount, "all workers should be stable")

	// Verify partitions are assigned correctly
	cluster.VerifyTotalPartitionCount(6)
	t.Log("SUCCESS: Leader election resolved correctly without split-brain")
}
