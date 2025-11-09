package integration_test

// Integration tests: behavior and timing scenarios around manager lifecycle, scaling, elections, and fault tolerance.
//
// This file consolidates cases previously in behavior_scenarios_test.go and timing_scenarios_test.go
// to reduce dispersion. Scenarios here focus on:
//   - Rolling updates and upgrade patterns
//   - Network partitions and healing
//   - High churn stability
//   - Mass failures and recovery
//   - Leader failover during rebalancing
//   - Rapid scaling stabilization windows
//   - Slow heartbeat boundaries without false emergencies
//   - Concurrent leader election races

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/test/testutil"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// TestIntegration_K8sRollingUpdate_PreservesAssignments verifies the system handles Kubernetes rolling updates gracefully.
func TestIntegration_K8sRollingUpdate_PreservesAssignments(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	t.Log("Test: Kubernetes rolling update - preserve assignments")

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Increased timeout to 4 minutes to accommodate rolling update with rebalancing waits
	ctx, cancel := context.WithTimeout(t.Context(), 240*time.Second)
	defer cancel()

	// Create cluster with 10 workers and 100 partitions (more partitions = better affinity test)
	// Use fast-tuned config to accelerate stabilization without changing correctness.
	cluster := testutil.NewFastWorkerCluster(t, nc, 100)
	defer cluster.StopWorkers()

	// Start 10 workers
	t.Log("Starting 10 workers...")
	for i := 0; i < 10; i++ {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err)
	}

	// Wait for cluster to stabilize (fast config should converge quicker)
	t.Log("Waiting for initial cluster stabilization...")
	cluster.WaitForStableState(15 * time.Second)
	t.Log("Initial cluster stable")

	// Record initial assignments for affinity calculation
	initialAssignments := make(map[string]map[string]bool) // workerID -> set of partition keys
	totalInitial := 0
	for _, mgr := range cluster.Workers {
		assignment := mgr.CurrentAssignment()
		partitionKeys := make(map[string]bool)
		for _, p := range assignment.Partitions {
			for _, key := range p.Keys {
				partitionKeys[key] = true
			}
		}
		initialAssignments[mgr.WorkerID()] = partitionKeys
		totalInitial += len(assignment.Partitions)
		t.Logf("Worker %s: %d partitions initially", mgr.WorkerID(), len(assignment.Partitions))
	}
	require.Equal(t, 100, totalInitial, "expected all 100 partitions assigned initially")

	// Perform rolling update: replace each worker one by one
	t.Log("Starting rolling update...")
	originalWorkerCount := len(cluster.Workers)
	for i := 0; i < 10; i++ {
		workerToReplace := cluster.Workers[i]
		oldWorkerID := workerToReplace.WorkerID()
		t.Logf("Rolling update step %d/10: Replacing worker %s", i+1, oldWorkerID)

		// Stop old worker
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := workerToReplace.Stop(stopCtx)
		stopCancel()
		require.NoError(t, err, "failed to stop worker %s", oldWorkerID)

		// Wait for remaining workers to rebalance and stabilize.
		// With fast config, convergence should be quick; poll more frequently for earlier exit.
		maxWaitTime := 10 * time.Second
		startWait := time.Now()
		stabilized := false
		for time.Since(startWait) < maxWaitTime {
			time.Sleep(250 * time.Millisecond)
			totalDuring := 0
			// Only count workers from original slice (0 to originalWorkerCount-1)
			for j := 0; j < originalWorkerCount; j++ {
				if j == i {
					continue // Skip the stopped worker
				}
				assignment := cluster.Workers[j].CurrentAssignment()
				totalDuring += len(assignment.Partitions)
			}
			t.Logf("Partitions during replacement (after %v): %d", time.Since(startWait).Round(time.Second), totalDuring)
			if totalDuring == 100 {
				stabilized = true
				break
			}
		}
		require.True(t, stabilized, "expected all partitions covered during replacement %d", i+1)

		// Start new worker (with same stable ID due to worker ID claiming)
		newWorker := cluster.AddWorker(ctx)
		err = newWorker.Start(ctx)
		require.NoError(t, err, "failed to start replacement worker")
		t.Logf("Started replacement worker %s", newWorker.WorkerID())

		// Update cluster workers list - replace the stopped worker with the new one
		// AddWorker appended the new worker, so copy it to the correct position
		cluster.Workers[i] = cluster.Workers[len(cluster.Workers)-1]
		// Truncate to remove the duplicate
		cluster.Workers = cluster.Workers[:originalWorkerCount]

		// Brief settle time after replacement
		time.Sleep(2 * time.Second)
	}

	t.Log("Rolling update complete, waiting for final stabilization...")
	cluster.WaitForStableState(20 * time.Second)
	t.Log("System stabilized after rolling update")

	// Verify all partitions are still assigned
	// Use GetActiveWorkers to ensure only active workers are counted
	activeWorkers := cluster.GetActiveWorkers()
	finalAssignments := make(map[string]map[string]bool) // workerID -> set of partition keys
	totalFinal := 0
	for _, mgr := range activeWorkers {
		assignment := mgr.CurrentAssignment()
		partitionKeys := make(map[string]bool)
		for _, p := range assignment.Partitions {
			for _, key := range p.Keys {
				partitionKeys[key] = true
			}
		}
		finalAssignments[mgr.WorkerID()] = partitionKeys
		totalFinal += len(assignment.Partitions)
		t.Logf("Worker %s: %d partitions after rolling update", mgr.WorkerID(), len(assignment.Partitions))
	}
	require.Equal(t, 100, totalFinal, "expected all 100 partitions assigned after rolling update")

	// Calculate cache affinity (how many partitions stayed with same worker)
	matchedPartitions := 0
	for workerID, initialKeys := range initialAssignments {
		finalKeys, exists := finalAssignments[workerID]
		if !exists {
			continue // Worker ID might have changed (shouldn't happen with stable IDs)
		}

		// Count how many partitions stayed with this worker
		for key := range initialKeys {
			if finalKeys[key] {
				matchedPartitions++
			}
		}
	}

	affinityPercent := float64(matchedPartitions) / float64(totalInitial) * 100
	t.Logf("Cache affinity: %.1f%% (%d/%d partitions stayed with same worker)",
		affinityPercent, matchedPartitions, totalInitial)

	// Verify cache affinity >80% (consistent hashing should preserve locality)
	require.GreaterOrEqual(t, affinityPercent, 80.0,
		"expected >80%% cache affinity after rolling update, got %.1f%%", affinityPercent)

	t.Log("Test passed - rolling update preserved assignments with good cache affinity")
}

// TestIntegration_NetworkPartition_HealsGracefully verifies system recovery after network partition.
func TestIntegration_NetworkPartition_HealsGracefully(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	t.Log("Test: Network partition healing")

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	// Create cluster with 6 workers and 50 partitions
	cluster := testutil.NewWorkerCluster(t, nc, 50)
	defer cluster.StopWorkers()

	// Start 6 workers
	t.Log("Starting 6 workers...")
	for i := 0; i < 6; i++ {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err)
	}

	// Wait for cluster to stabilize
	t.Log("Waiting for initial cluster stabilization...")
	cluster.WaitForStableState(25 * time.Second)
	t.Log("Initial cluster stable")

	// Verify initial state
	totalInitial := 0
	for _, mgr := range cluster.Workers {
		assignment := mgr.CurrentAssignment()
		totalInitial += len(assignment.Partitions)
		t.Logf("Worker %s: %d partitions initially", mgr.WorkerID(), len(assignment.Partitions))
	}
	require.Equal(t, 50, totalInitial, "expected all 50 partitions assigned initially")

	// SIMULATE NETWORK PARTITION: Stop first 3 workers (group A)
	t.Log("Simulating network partition: stopping group A (3 workers)...")
	groupAIDs := make([]string, 3)
	for i := 0; i < 3; i++ {
		groupAIDs[i] = cluster.Workers[i].WorkerID()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := cluster.Workers[i].Stop(stopCtx)
		stopCancel()
		require.NoError(t, err, "failed to stop worker %d", i)
	}
	t.Logf("Group A stopped: %v", groupAIDs)

	// Wait for system to detect failures and rebalance
	t.Log("Waiting for system to detect partition and rebalance...")
	time.Sleep(12 * time.Second)

	// Verify group B (workers 3-5) now handle all partitions
	totalDuringPartition := 0
	for i := 3; i < 6; i++ {
		assignment := cluster.Workers[i].CurrentAssignment()
		totalDuringPartition += len(assignment.Partitions)
		t.Logf("Worker %s (group B): %d partitions during partition",
			cluster.Workers[i].WorkerID(), len(assignment.Partitions))
	}
	t.Logf("Total partitions during partition: %d", totalDuringPartition)
	require.Equal(t, 50, totalDuringPartition,
		"expected group B to handle all 50 partitions during partition")

	// HEAL NETWORK PARTITION: Restart group A workers
	t.Log("Healing network partition: restarting group A...")
	newGroupAWorkers := make([]*parti.Manager, 3)
	for i := 0; i < 3; i++ {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err, "failed to restart worker %d", i)
		newGroupAWorkers[i] = mgr
		t.Logf("Restarted worker %s", mgr.WorkerID())
	}

	// Update cluster workers list with restarted workers
	// AddWorker appended 3 new workers to the slice (now at indices 6-8)
	// Copy them to positions 0-2 and truncate to remove duplicates
	copy(cluster.Workers[:3], cluster.Workers[6:9])
	cluster.Workers = cluster.Workers[:6] // Keep only 6 workers total

	// Wait for system to rebalance after healing
	t.Log("Waiting for system to rebalance after healing...")
	cluster.WaitForStableState(25 * time.Second)
	// After stability, wait briefly for assignments to propagate and balance
	require.True(t, cluster.WaitForAllWorkersHavePartitions(8*time.Second),
		"timed out waiting for workers to receive partitions after healing")
	require.True(t, cluster.WaitForBalancedAssignments(50, 6, 12*time.Second),
		"assignments did not balance within tolerance in time")
	t.Log("System stabilized after healing")

	// Verify all partitions are assigned across all 6 workers
	// Use GetActiveWorkers to exclude stopped workers
	activeWorkers := cluster.GetActiveWorkers()
	totalAfterHealing := 0
	for _, mgr := range activeWorkers {
		assignment := mgr.CurrentAssignment()
		totalAfterHealing += len(assignment.Partitions)
		t.Logf("Worker %s: %d partitions after healing", mgr.WorkerID(), len(assignment.Partitions))
	}
	require.Equal(t, 50, totalAfterHealing, "expected all 50 partitions assigned after healing")

	// Verify each worker has a reasonable share (should be balanced)
	for _, mgr := range activeWorkers {
		assignment := mgr.CurrentAssignment()
		count := len(assignment.Partitions)
		// Each worker should have roughly 50/6 ≈ 8 partitions (allow ±6 variance)
		require.InDelta(t, 8, count, 6,
			"Worker %s has unbalanced assignment: %d partitions", mgr.WorkerID(), count)
	}

	t.Log("Test passed - system healed gracefully after network partition")
}

// TestIntegration_HighChurn_SystemStable verifies system stability under continuous worker churn.
func TestIntegration_HighChurn_SystemStable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	t.Log("Test: High churn - system stability")

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()

	// Create cluster with 50 partitions
	cluster := testutil.NewWorkerCluster(t, nc, 50)
	defer cluster.StopWorkers()

	// Start initial 5 workers
	t.Log("Starting initial 5 workers...")
	for i := 0; i < 5; i++ {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err)
	}

	// Wait for initial stabilization
	t.Log("Waiting for initial stabilization...")
	cluster.WaitForStableState(20 * time.Second)
	t.Log("Initial cluster stable")

	// Run churn for 60 seconds
	t.Log("Starting high churn period (60 seconds)...")
	churnCtx, churnCancel := context.WithTimeout(ctx, 60*time.Second)
	defer churnCancel()

	var churnWg sync.WaitGroup
	churnWg.Add(1)
	go func() {
		defer churnWg.Done()

		churnCount := 0
		for {
			select {
			case <-churnCtx.Done():
				t.Log("Churn period complete")
				return
			case <-time.After(5 * time.Second):
				// Randomly add or remove a worker
				currentCount := cluster.WorkerCount()

				switch {
				case currentCount <= 3:
					// Too few workers, must add
					t.Logf("Churn %d: Adding worker (current: %d)", churnCount, currentCount)
					mgr := cluster.AddWorker(ctx)
					err := mgr.Start(ctx)
					if err != nil {
						t.Logf("Warning: Failed to start worker during churn: %v", err)
						continue
					}
					churnCount++

				case currentCount >= 7:
					// Too many workers, must remove
					t.Logf("Churn %d: Removing worker (current: %d)", churnCount, currentCount)
					// Remove last worker using RemoveWorker (which stops it)
					workers := cluster.GetWorkers()
					lastIdx := len(workers) - 1
					cluster.RemoveWorker(lastIdx)
					churnCount++

				default:
					// Moderate count, randomly add or remove
					if churnCount%2 == 0 {
						// Add worker
						t.Logf("Churn %d: Adding worker (current: %d)", churnCount, currentCount)
						mgr := cluster.AddWorker(ctx)
						err := mgr.Start(ctx)
						if err != nil {
							t.Logf("Warning: Failed to start worker during churn: %v", err)
							continue
						}
					} else {
						// Remove worker using RemoveWorker (which stops it)
						t.Logf("Churn %d: Removing worker (current: %d)", churnCount, currentCount)
						workers := cluster.GetWorkers()
						lastIdx := len(workers) - 1
						cluster.RemoveWorker(lastIdx)
					}
					churnCount++
				}
			}
		}
	}()

	// Monitor partition coverage during churn
	monitorWg := sync.WaitGroup{}
	monitorWg.Add(1)
	go func() {
		defer monitorWg.Done()

		monitorTicker := time.NewTicker(3 * time.Second)
		defer monitorTicker.Stop()

		for {
			select {
			case <-churnCtx.Done():
				return
			case <-monitorTicker.C:
				// Check partition coverage
				workers := cluster.GetWorkers()
				total := 0
				for _, mgr := range workers {
					assignment := mgr.CurrentAssignment()
					total += len(assignment.Partitions)
				}
				t.Logf("Monitor: %d workers, %d partitions assigned", len(workers), total)

				// Verify all 50 partitions are covered
				if total != 50 {
					t.Errorf("CRITICAL: Only %d/50 partitions assigned during churn", total)
				}
			}
		}
	}()

	// Wait for churn period to complete
	churnWg.Wait()
	monitorWg.Wait()

	// Wait for final stabilization
	t.Log("Churn complete, waiting for final stabilization...")
	time.Sleep(10 * time.Second)

	// Final verification
	totalFinal := 0
	for _, mgr := range cluster.Workers {
		assignment := mgr.CurrentAssignment()
		totalFinal += len(assignment.Partitions)
		t.Logf("Worker %s: %d partitions after churn", mgr.WorkerID(), len(assignment.Partitions))
	}
	require.Equal(t, 50, totalFinal, "expected all 50 partitions assigned after churn")

	t.Log("Test passed - system remained stable under high churn")
}

// TestIntegration_MassWorkerFailure_Recovers verifies system recovery from sudden loss of majority of workers.
func TestIntegration_MassWorkerFailure_Recovers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	t.Log("Test: Mass worker failure recovery")

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	// Create cluster with 10 workers and 50 partitions
	cluster := testutil.NewWorkerCluster(t, nc, 50)
	defer cluster.StopWorkers()

	// Start 10 workers
	t.Log("Starting 10 workers...")
	for i := 0; i < 10; i++ {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err)
	}

	// Wait for cluster to stabilize
	t.Log("Waiting for initial stabilization...")
	cluster.WaitForStableState(25 * time.Second)
	t.Log("Initial cluster stable")

	// Verify initial state
	totalInitial := 0
	for _, mgr := range cluster.Workers {
		assignment := mgr.CurrentAssignment()
		totalInitial += len(assignment.Partitions)
		t.Logf("Worker %s: %d partitions initially", mgr.WorkerID(), len(assignment.Partitions))
	}
	require.Equal(t, 50, totalInitial, "expected all 50 partitions assigned initially")

	// MASS FAILURE: Stop 8 out of 10 workers
	t.Log("MASS FAILURE: Stopping 8 workers simultaneously...")
	var failWg sync.WaitGroup
	for i := 0; i < 8; i++ {
		failWg.Add(1)
		go func(idx int) {
			defer failWg.Done()
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer stopCancel()
			err := cluster.Workers[idx].Stop(stopCtx)
			if err != nil {
				t.Logf("Warning: Failed to stop worker %d: %v", idx, err)
			}
		}(i)
	}
	failWg.Wait()
	t.Log("8 workers stopped")

	// Wait for survivors to detect failures and rebalance
	t.Log("Waiting for survivors to detect failures and rebalance...")
	time.Sleep(15 * time.Second)

	// Verify remaining 2 workers took over all partitions
	totalAfterFailure := 0
	for i := 8; i < 10; i++ {
		assignment := cluster.Workers[i].CurrentAssignment()
		count := len(assignment.Partitions)
		totalAfterFailure += count
		t.Logf("Survivor worker %s: %d partitions", cluster.Workers[i].WorkerID(), count)
	}
	require.Equal(t, 50, totalAfterFailure,
		"expected survivors to handle all 50 partitions after mass failure")

	// Gradually restart failed workers
	t.Log("Restarting failed workers...")
	newWorkers := make([]*parti.Manager, 8)
	for i := 0; i < 8; i++ {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err, "failed to restart worker %d", i)
		newWorkers[i] = mgr
		t.Logf("Restarted worker %s", mgr.WorkerID())

		// Wait a bit between restarts (simulate gradual recovery)
		time.Sleep(2 * time.Second)
	}

	// Update cluster workers list - replace stopped workers with new ones
	// AddWorker appended 8 new workers to the slice, so we need to:
	// 1. Copy new workers (indices 10-17) to positions 0-7
	// 2. Truncate the slice to remove duplicates
	copy(cluster.Workers[:8], cluster.Workers[10:18])
	cluster.Workers = cluster.Workers[:10] // Keep only workers 0-9 (8 restarted + 2 survivors)

	// Wait for final rebalancing
	t.Log("Waiting for final rebalancing...")
	cluster.WaitForStableState(25 * time.Second)
	t.Log("System stabilized after recovery")

	// Verify all partitions are assigned
	// Use GetActiveWorkers to exclude stopped workers
	activeWorkers := cluster.GetActiveWorkers()
	totalFinal := 0
	for _, mgr := range activeWorkers {
		assignment := mgr.CurrentAssignment()
		totalFinal += len(assignment.Partitions)
		t.Logf("Worker %s: %d partitions after recovery", mgr.WorkerID(), len(assignment.Partitions))
	}
	require.Equal(t, 50, totalFinal, "expected all 50 partitions assigned after recovery")

	t.Log("Test passed - system recovered from mass worker failure")
}

// TestIntegration_LeaderFailover_DuringRebalance verifies correct behavior when leader fails during active rebalancing.
func TestIntegration_LeaderFailover_DuringRebalance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	t.Log("Test: Leader failover during rebalancing")

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	// Create cluster with 50 partitions
	cluster := testutil.NewFastWorkerCluster(t, nc, 50) // Use fast config for quicker failover
	defer cluster.StopWorkers()

	// Start initial 5 workers
	t.Log("Starting initial 5 workers...")
	for i := 0; i < 5; i++ {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err)
	}

	// Wait for initial stabilization
	t.Log("Waiting for initial stabilization...")
	cluster.WaitForStableState(20 * time.Second)
	t.Log("Initial cluster stable")

	// Identify the leader
	var leaderMgr *parti.Manager
	var leaderIdx int
	for i, mgr := range cluster.Workers {
		if mgr.IsLeader() {
			leaderMgr = mgr
			leaderIdx = i
			break
		}
	}
	require.NotNil(t, leaderMgr, "expected to find leader")
	leaderID := leaderMgr.WorkerID()
	t.Logf("Current leader: %s", leaderID)

	// Add 3 new workers to trigger rebalancing
	t.Log("Adding 3 new workers to trigger rebalancing...")
	for i := 0; i < 3; i++ {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err)
	}

	// Wait briefly for rebalancing to start
	time.Sleep(3 * time.Second)

	// Stop the leader during rebalancing
	t.Logf("Stopping leader %s during rebalancing...", leaderID)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := leaderMgr.Stop(stopCtx)
	stopCancel()
	require.NoError(t, err)

	// Remove stopped leader from cluster
	cluster.Workers = append(cluster.Workers[:leaderIdx], cluster.Workers[leaderIdx+1:]...)
	cluster.StateTrackers = append(cluster.StateTrackers[:leaderIdx], cluster.StateTrackers[leaderIdx+1:]...)

	t.Log("Leader stopped, waiting for new leader election...")

	// Wait for new leader election and rebalancing completion
	time.Sleep(15 * time.Second)

	// Verify new leader exists
	newLeaderCount := 0
	var newLeaderID string
	for _, mgr := range cluster.Workers {
		if mgr.IsLeader() {
			newLeaderID = mgr.WorkerID()
			newLeaderCount++
		}
	}
	require.Equal(t, 1, newLeaderCount, "expected exactly one new leader")
	require.NotEmpty(t, newLeaderID, "expected new leader to be identified")
	require.NotEqual(t, leaderID, newLeaderID, "expected different leader")
	t.Logf("New leader elected: %s", newLeaderID)

	// Wait for system to fully stabilize
	t.Log("Waiting for final stabilization...")
	cluster.WaitForStableState(20 * time.Second)
	t.Log("System stabilized")

	// Verify all partitions are assigned
	totalFinal := 0
	for _, mgr := range cluster.Workers {
		assignment := mgr.CurrentAssignment()
		totalFinal += len(assignment.Partitions)
		t.Logf("Worker %s: %d partitions", mgr.WorkerID(), len(assignment.Partitions))
	}
	require.Equal(t, 50, totalFinal, "expected all 50 partitions assigned after failover")

	t.Log("Test passed - new leader completed rebalancing after failover")
}

// TestScenario_RapidScaling_StabilizationWindows verifies that rapid scaling results in a single batched rebalance.
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
	// Use TimingProfile for consistent rapid scaling semantics.
	cfg := testutil.NewConfigFromProfile(testutil.TimingProfile{
		HeartbeatInterval:    500 * time.Millisecond,
		TTLMultiplier:        3.0, // 1.5s TTL
		ColdStartWindow:      5 * time.Second,
		PlannedScaleWindow:   3 * time.Second,
		MinRebalanceInterval: 3 * time.Second,
	})

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

	// With proper batching, we expect at most ~3 versions (setup + 1 batched rebalance)
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

// TestScenario_SlowHeartbeats_NearExpiryBoundary verifies that slow-but-valid heartbeats don't trigger emergencies.
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
	// Timing profile representing slow-but-valid heartbeat scenario.
	cfg := testutil.NewConfigFromProfile(testutil.TimingProfile{
		HeartbeatInterval:    1 * time.Second,
		TTLMultiplier:        3.0, // 3s TTL
		GraceMultiplier:      2.0, // 2s grace
		ColdStartWindow:      5 * time.Second,
		PlannedScaleWindow:   3 * time.Second,
		MinRebalanceInterval: 2 * time.Second,
	})

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

// TestScenario_ConcurrentLeaderElection_RaceCondition verifies only one leader wins under simultaneous claims.
func TestScenario_ConcurrentLeaderElection_RaceCondition(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Profile emphasizing election contention while preserving batching invariants.
	cfg := testutil.NewConfigFromProfile(testutil.TimingProfile{
		ElectionTimeout:      2 * time.Second,
		ColdStartWindow:      5 * time.Second,
		PlannedScaleWindow:   3 * time.Second,
		MinRebalanceInterval: 2 * time.Second,
	})

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
