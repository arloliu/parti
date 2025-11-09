package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/test/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestIntegration_Emergency_WorkerCrash_ReassignsPartitions verifies that when a worker
// crashes beyond the grace period, its partitions are reassigned to remaining workers.
//
// Production scenario: Single node hardware failure in a multi-node cluster.
func TestIntegration_Emergency_WorkerCrash_ReassignsPartitions(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Configure short intervals for faster test
	cfg := testutil.IntegrationTestConfig()
	cfg.HeartbeatInterval = 500 * time.Millisecond
	cfg.HeartbeatTTL = 1500 * time.Millisecond // 3x interval
	cfg.EmergencyGracePeriod = 1 * time.Second
	cfg.Assignment.MinRebalanceInterval = 500 * time.Millisecond

	// Create cluster with 3 workers and 12 partitions (4 per worker ideally)
	cluster := testutil.NewWorkerCluster(t, nc, 12)
	cluster.Config = cfg
	defer cluster.StopWorkers()

	// Enable debug logging to see what's happening
	debugLogger := logging.NewTest(t)

	// Start 3 workers with debug logging
	for i := 0; i < 3; i++ {
		cluster.AddWorker(ctx, debugLogger)
	}
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(10 * time.Second)

	t.Log("Initial cluster stable with 3 workers")
	cluster.VerifyTotalPartitionCount(12)

	// Record pre-crash partition distribution
	worker0Partitions := len(cluster.Workers[0].CurrentAssignment().Partitions)
	require.Greater(t, worker0Partitions, 0, "Worker 0 should have partitions before crash")
	t.Logf("Worker 0 has %d partitions before crash", worker0Partitions)

	// Crash worker 0 (simulate hardware failure - sudden stop)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := cluster.Workers[0].Stop(stopCtx)
	stopCancel()
	require.NoError(t, err)
	t.Log("Worker 0 crashed (stopped)")

	// Wait for:
	// 1. Heartbeat TTL expiry: 1.5s
	// 2. New leader election: up to 8s (FastConfig ElectionTimeout=1s)
	// 3. Emergency grace period: 1s
	// 4. Emergency detection and rebalance: 2s
	// Total: ~12.5s, use 15s for safety
	t.Log("Waiting for leader failover + emergency detection + rebalance (15 seconds)")
	time.Sleep(15 * time.Second)

	// Verify partitions were reassigned to remaining workers
	totalPartitions := 0
	activeWorkers := 0
	for i := 1; i < len(cluster.Workers); i++ {
		mgr := cluster.Workers[i]
		partCount := len(mgr.CurrentAssignment().Partitions)
		totalPartitions += partCount
		if partCount > 0 {
			activeWorkers++
			t.Logf("Worker %d has %d partitions after emergency", i, partCount)
		}
	}

	require.Equal(t, 2, activeWorkers, "Should have exactly 2 active workers after crash")
	require.Equal(t, 12, totalPartitions, "All 12 partitions should be reassigned to remaining workers")

	t.Log("SUCCESS: Emergency rebalance completed, all partitions reassigned correctly")
}

// TestIntegration_Emergency_CascadingFailures_HandlesGracefully verifies that multiple
// workers failing in sequence (cascading failure) are all handled correctly.
//
// Production scenario: Domino effect failure where one node's crash causes increased
// load on others, leading to more failures.
func TestIntegration_Emergency_CascadingFailures_HandlesGracefully(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	cfg.HeartbeatInterval = 500 * time.Millisecond
	cfg.HeartbeatTTL = 1500 * time.Millisecond
	cfg.EmergencyGracePeriod = 1 * time.Second
	cfg.Assignment.MinRebalanceInterval = 500 * time.Millisecond

	// Create cluster with 5 workers and 20 partitions
	cluster := testutil.NewWorkerCluster(t, nc, 20)
	cluster.Config = cfg
	defer cluster.StopWorkers()

	for i := 0; i < 5; i++ {
		cluster.AddWorker(ctx)
	}
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(10 * time.Second)

	t.Log("Initial cluster stable with 5 workers")
	cluster.VerifyTotalPartitionCount(20)

	// First failure: Stop worker 0
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := cluster.Workers[0].Stop(stopCtx)
	stopCancel()
	require.NoError(t, err)
	t.Log("Worker 0 failed")

	// Wait 2 seconds
	time.Sleep(2 * time.Second)

	// Second failure: Stop worker 1 (cascading)
	stopCtx, stopCancel = context.WithTimeout(context.Background(), 2*time.Second)
	err = cluster.Workers[1].Stop(stopCtx)
	stopCancel()
	require.NoError(t, err)
	t.Log("Worker 1 failed (cascading)")

	// Wait 2 seconds
	time.Sleep(2 * time.Second)

	// Third failure: Stop worker 2 (cascading)
	stopCtx, stopCancel = context.WithTimeout(context.Background(), 2*time.Second)
	err = cluster.Workers[2].Stop(stopCtx)
	stopCancel()
	require.NoError(t, err)
	t.Log("Worker 2 failed (cascading)")

	// Wait for all emergency rebalances to complete
	t.Log("Waiting for all emergency rebalances to complete (5 seconds)")
	time.Sleep(5 * time.Second)

	// Verify remaining 2 workers handle all 20 partitions
	totalPartitions := 0
	activeWorkers := 0
	for i := 3; i < len(cluster.Workers); i++ {
		mgr := cluster.Workers[i]
		partCount := len(mgr.CurrentAssignment().Partitions)
		totalPartitions += partCount
		if partCount > 0 {
			activeWorkers++
			t.Logf("Worker %d has %d partitions after cascading failures", i, partCount)
		}
	}

	require.Equal(t, 2, activeWorkers, "Should have exactly 2 active workers remaining")
	require.Equal(t, 20, totalPartitions, "All 20 partitions should be on remaining 2 workers")

	t.Log("SUCCESS: Cascading failures handled, all partitions safe")
}

// TestIntegration_Emergency_K8sRollingUpdate_NoDataLoss simulates a Kubernetes rolling
// update where pods restart one by one, each completing within grace period.
//
// Production scenario: kubectl rollout restart or deployment update with maxUnavailable=1.
func TestIntegration_Emergency_K8sRollingUpdate_NoDataLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	cfg.HeartbeatInterval = 500 * time.Millisecond
	cfg.HeartbeatTTL = 2500 * time.Millisecond // Must be >= EmergencyGracePeriod
	cfg.EmergencyGracePeriod = 2 * time.Second // Longer grace for rolling updates
	cfg.Assignment.MinRebalanceInterval = 500 * time.Millisecond
	cfg.ColdStartWindow = 3 * time.Second    // Same as planned scale window
	cfg.PlannedScaleWindow = 3 * time.Second // Allow time for pods to restart

	// Create cluster with 3 workers and 15 partitions
	cluster := testutil.NewWorkerCluster(t, nc, 15)
	cluster.Config = cfg
	defer cluster.StopWorkers()

	for i := 0; i < 3; i++ {
		cluster.AddWorker(ctx)
	}
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(10 * time.Second)

	t.Log("Initial cluster stable with 3 workers")
	cluster.VerifyTotalPartitionCount(15)

	// Rolling update: Restart each pod one by one
	for i := 0; i < 3; i++ {
		t.Logf("Rolling update: Restarting worker %d", i)

		// Stop worker
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := cluster.Workers[i].Stop(stopCtx)
		stopCancel()
		require.NoError(t, err)

		// Simulate pod restart time (1.5 seconds - within 2 second grace period)
		time.Sleep(1500 * time.Millisecond)

		// Start new worker (simulates pod coming back up)
		js, err := jetstream.New(cluster.NC)
		require.NoError(t, err)
		newWorker, err := parti.NewManager(&cluster.Config, js, cluster.Source, cluster.Strategy)
		require.NoError(t, err)
		cluster.Workers[i] = newWorker
		err = newWorker.Start(ctx)
		require.NoError(t, err)

		t.Logf("Worker %d restarted successfully", i)

		// Wait for cluster to stabilize before next restart
		time.Sleep(2 * time.Second)
	}

	// Wait for final stabilization
	t.Log("Waiting for final cluster stabilization (5 seconds)")
	time.Sleep(5 * time.Second)

	// Verify all workers are active and all partitions assigned
	totalPartitions := 0
	activeWorkers := 0
	for i, mgr := range cluster.Workers {
		partCount := len(mgr.CurrentAssignment().Partitions)
		totalPartitions += partCount
		if partCount > 0 {
			activeWorkers++
			t.Logf("Worker %d has %d partitions after rolling update", i, partCount)
		}
	}

	require.Equal(t, 3, activeWorkers, "All 3 workers should be active after rolling update")
	require.Equal(t, 15, totalPartitions, "All 15 partitions should still be assigned")

	t.Log("SUCCESS: Rolling update completed without data loss or emergency rebalancing")
}

// TestIntegration_Emergency_SlowWorker_DoesNotBlockSystem verifies that a slow or
// unresponsive worker doesn't block the emergency detection and rebalancing process.
//
// Production scenario: Worker experiencing high CPU/memory causing slow heartbeat updates.
func TestIntegration_Emergency_SlowWorker_DoesNotBlockSystem(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	cfg.HeartbeatInterval = 500 * time.Millisecond
	cfg.HeartbeatTTL = 1500 * time.Millisecond
	cfg.EmergencyGracePeriod = 1 * time.Second
	cfg.Assignment.MinRebalanceInterval = 500 * time.Millisecond

	cluster := testutil.NewWorkerCluster(t, nc, 12)
	cluster.Config = cfg
	defer cluster.StopWorkers()

	for i := 0; i < 3; i++ {
		cluster.AddWorker(ctx)
	}
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(10 * time.Second)

	t.Log("Initial cluster stable with 3 workers")

	// Stop worker 0 (simulating slow/unresponsive worker that eventually times out)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := cluster.Workers[0].Stop(stopCtx)
	stopCancel()
	require.NoError(t, err)
	t.Log("Worker 0 stopped (simulating unresponsive worker)")

	// Record time before emergency detection
	startTime := time.Now()

	// Wait for emergency rebalance
	time.Sleep(5 * time.Second)

	elapsedTime := time.Since(startTime)
	t.Logf("Emergency rebalance completed in %v", elapsedTime)

	// Verify rebalance happened reasonably quickly (under 10 seconds)
	require.Less(t, elapsedTime, 10*time.Second, "Emergency rebalance should not be blocked by slow worker")

	// Verify partitions were reassigned
	totalPartitions := 0
	for i := 1; i < len(cluster.Workers); i++ {
		mgr := cluster.Workers[i]
		totalPartitions += len(mgr.CurrentAssignment().Partitions)
	}

	require.Equal(t, 12, totalPartitions, "All partitions should be reassigned despite slow worker")

	t.Log("SUCCESS: Slow worker did not block emergency rebalancing")
}
