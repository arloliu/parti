package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestEmergencyHysteresis_TransientDisappearance tests that transient worker
// disappearance (less than grace period) does not trigger emergency rebalance.
func TestEmergencyHysteresis_TransientDisappearance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	cfg.HeartbeatInterval = 500 * time.Millisecond
	cfg.HeartbeatTTL = 2 * time.Second
	cfg.EmergencyGracePeriod = 1500 * time.Millisecond
	cfg.Assignment.MinRebalanceInterval = 500 * time.Millisecond

	cluster := testutil.NewWorkerCluster(t, nc, 9)
	cluster.Config = cfg
	defer cluster.StopWorkers()

	for i := 0; i < 3; i++ {
		cluster.AddWorker(ctx)
	}
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(10 * time.Second)

	t.Log("Initial cluster stable with 3 workers")

	// Stop worker 0
	workerToStop := cluster.Workers[0]
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := workerToStop.Stop(stopCtx)
	stopCancel()
	require.NoError(t, err)

	t.Log("Waiting 1 second (less than 1.5s grace period)")
	time.Sleep(1 * time.Second)

	// Restart worker
	t.Log("Restarting worker - simulating recovery")
	newWorker, err := parti.NewManager(&cluster.Config, cluster.NC, cluster.Source, cluster.Strategy)
	require.NoError(t, err)
	cluster.Workers[0] = newWorker
	err = newWorker.Start(ctx)
	require.NoError(t, err)

	time.Sleep(3 * time.Second)

	// Verify all workers are still active
	activeCount := 0
	for _, mgr := range cluster.Workers {
		if len(mgr.CurrentAssignment().Partitions) > 0 {
			activeCount++
		}
	}

	require.Equal(t, 3, activeCount, "All 3 workers should remain active")
	t.Log("Verified: Transient disappearance did not trigger emergency rebalance")
}

// TestEmergencyHysteresis_ConfirmedDisappearance tests confirmed worker failure.
func TestEmergencyHysteresis_ConfirmedDisappearance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	cfg.HeartbeatInterval = 500 * time.Millisecond
	cfg.HeartbeatTTL = 2 * time.Second
	cfg.EmergencyGracePeriod = 1 * time.Second
	cfg.Assignment.MinRebalanceInterval = 500 * time.Millisecond

	cluster := testutil.NewWorkerCluster(t, nc, 9)
	cluster.Config = cfg
	defer cluster.StopWorkers()

	for i := 0; i < 3; i++ {
		cluster.AddWorker(ctx)
	}
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(10 * time.Second)

	t.Log("Initial cluster stable with 3 workers")
	cluster.VerifyTotalPartitionCount(9)

	// Stop worker 0
	workerToStop := cluster.Workers[0]
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := workerToStop.Stop(stopCtx)
	stopCancel()
	require.NoError(t, err)

	t.Log("Waiting for grace period and TTL to expire (4 seconds)")
	time.Sleep(4 * time.Second)

	t.Log("Waiting for emergency rebalance to complete")
	time.Sleep(2 * time.Second)

	// Verify emergency rebalance occurred
	activeWorkers := 0
	totalPartitions := 0
	for _, mgr := range cluster.Workers {
		partCount := len(mgr.CurrentAssignment().Partitions)
		totalPartitions += partCount
		if partCount > 0 {
			activeWorkers++
		}
	}

	require.Equal(t, 2, activeWorkers, "Should have exactly 2 active workers")
	require.Equal(t, 9, totalPartitions, "All 9 partitions should still be assigned")
	t.Log("Verified: Confirmed disappearance triggered emergency rebalance")
}

// TestEmergencyHysteresis_MultipleWorkerFailures tests multiple worker failures.
func TestEmergencyHysteresis_MultipleWorkerFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	cfg.HeartbeatInterval = 500 * time.Millisecond
	cfg.HeartbeatTTL = 2 * time.Second
	cfg.EmergencyGracePeriod = 1 * time.Second
	cfg.Assignment.MinRebalanceInterval = 500 * time.Millisecond

	cluster := testutil.NewWorkerCluster(t, nc, 12)
	cluster.Config = cfg
	defer cluster.StopWorkers()

	for i := 0; i < 4; i++ {
		cluster.AddWorker(ctx)
	}
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(10 * time.Second)

	t.Log("Initial cluster stable with 4 workers")
	cluster.VerifyTotalPartitionCount(12)

	// Stop two workers
	t.Log("Stopping workers 0 and 1")
	for i := 0; i < 2; i++ {
		workerToStop := cluster.Workers[i]
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := workerToStop.Stop(stopCtx)
		stopCancel()
		require.NoError(t, err)
	}

	t.Log("Waiting for grace period and TTL to expire")
	time.Sleep(4 * time.Second)
	time.Sleep(2 * time.Second)

	// Verify only 2 workers remain active
	activeWorkers := 0
	totalPartitions := 0
	for _, mgr := range cluster.Workers {
		partCount := len(mgr.CurrentAssignment().Partitions)
		totalPartitions += partCount
		if partCount > 0 {
			activeWorkers++
		}
	}

	require.Equal(t, 2, activeWorkers, "Should have exactly 2 active workers")
	require.Equal(t, 12, totalPartitions, "All 12 partitions should still be assigned")
	t.Log("Verified: Multiple worker failures handled correctly")
}
