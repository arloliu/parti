package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/test/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestEmergencyHysteresis_TransientDisappearance tests that transient worker
// disappearance (less than grace period) does not trigger emergency rebalance.
func TestEmergencyHysteresis_TransientDisappearance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Use centralized emergency fast timing profile.
	cfg := testutil.NewConfigFromProfile(testutil.MakeEmergencyFast())
	// Override grace slightly higher for transient disappearance scenario.
	cfg.EmergencyGracePeriod = 700 * time.Millisecond

	cluster := testutil.NewFastWorkerCluster(t, nc, 9)
	cluster.Config = cfg
	defer cluster.StopWorkers()

	for i := 0; i < 3; i++ {
		cluster.AddWorker(ctx)
	}
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(6 * time.Second)

	t.Log("Initial cluster stable with 3 workers")

	// Stop worker 0
	workerToStop := cluster.Workers[0]
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := workerToStop.Stop(stopCtx)
	stopCancel()
	require.NoError(t, err)

	t.Log("Waiting 450ms (< grace period 700ms)")
	time.Sleep(450 * time.Millisecond)

	// Restart worker
	t.Log("Restarting worker - simulating recovery")
	js, err := jetstream.New(cluster.NC)
	require.NoError(t, err)
	newWorker, err := parti.NewManager(&cluster.Config, js, cluster.Source, cluster.Strategy)
	require.NoError(t, err)
	cluster.Workers[0] = newWorker
	err = newWorker.Start(ctx)
	require.NoError(t, err)

	time.Sleep(1 * time.Second)

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

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.NewConfigFromProfile(testutil.MakeEmergencyFast())
	// Grace lower than transient test to force confirmed disappearance.
	cfg.EmergencyGracePeriod = 500 * time.Millisecond

	cluster := testutil.NewFastWorkerCluster(t, nc, 9)
	cluster.Config = cfg
	defer cluster.StopWorkers()

	for i := 0; i < 3; i++ {
		cluster.AddWorker(ctx)
	}
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(6 * time.Second)

	t.Log("Initial cluster stable with 3 workers")
	cluster.VerifyTotalPartitionCount(9)

	// Stop worker 0
	workerToStop := cluster.Workers[0]
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := workerToStop.Stop(stopCtx)
	stopCancel()
	require.NoError(t, err)

	// Wait for grace + TTL expiry (~1.4s) + small buffer for detection & rebalance
	t.Log("Waiting for grace period + TTL expiry (~1.4s) plus buffer")
	time.Sleep(1*time.Second + 400*time.Millisecond)
	t.Log("Waiting briefly for emergency rebalance completion")
	time.Sleep(600 * time.Millisecond)

	// Verify emergency rebalance occurred
	// Skip worker 0 since it's stopped - only check workers 1 and 2
	activeWorkers := 0
	totalPartitions := 0
	for i := 1; i < len(cluster.Workers); i++ {
		mgr := cluster.Workers[i]
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

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.NewConfigFromProfile(testutil.MakeEmergencyFast())
	cfg.EmergencyGracePeriod = 500 * time.Millisecond

	cluster := testutil.NewFastWorkerCluster(t, nc, 12)
	cluster.Config = cfg
	defer cluster.StopWorkers()

	for i := 0; i < 4; i++ {
		cluster.AddWorker(ctx)
	}
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(6 * time.Second)

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

	t.Log("Waiting for grace + TTL expiry (~1.4s) plus buffer")
	time.Sleep(1*time.Second + 400*time.Millisecond)
	t.Log("Waiting briefly for emergency rebalance completion")
	time.Sleep(600 * time.Millisecond)

	// Verify only 2 workers remain active (skip stopped workers 0 and 1)
	activeWorkers := 0
	totalPartitions := 0
	for i := 2; i < len(cluster.Workers); i++ {
		mgr := cluster.Workers[i]
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
