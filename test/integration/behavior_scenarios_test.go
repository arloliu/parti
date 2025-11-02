package integration_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/test/testutil"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// TestIntegration_K8sRollingUpdate_PreservesAssignments verifies the system
// handles Kubernetes rolling updates gracefully.
//
// Scenario:
//   - Start 10 workers with stable assignments
//   - Perform rolling update: replace workers one by one
//   - Each replacement: stop old worker, wait 2s, start new worker
//   - Verify partition coverage maintained throughout
//   - Verify >80% cache affinity preserved
//
// Expected Behavior:
//   - Consistent hashing preserves locality during replacements
//   - No assignment gaps during rolling update
//   - Cache affinity >80% after completion
//   - System remains stable throughout
//
// This simulates the most common production deployment pattern.
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
	cluster := testutil.NewWorkerCluster(t, nc, 100)
	defer cluster.StopWorkers()

	// Start 10 workers
	t.Log("Starting 10 workers...")
	for i := 0; i < 10; i++ {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err)
	}

	// Wait for cluster to stabilize
	t.Log("Waiting for initial cluster stabilization...")
	cluster.WaitForStableState(25 * time.Second)
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

		// Wait for remaining workers to rebalance and stabilize
		// Use exponential backoff to wait for all partitions to be covered
		maxWaitTime := 15 * time.Second
		startWait := time.Now()
		stabilized := false
		for time.Since(startWait) < maxWaitTime {
			time.Sleep(1 * time.Second)
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

		// Wait for stabilization after this replacement
		time.Sleep(5 * time.Second)
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

// TestIntegration_NetworkPartition_HealsGracefully verifies system recovery
// after network partition.
//
// Scenario:
//   - Start 6 workers with stable assignments
//   - Simulate partition: stop 3 workers (group A), keep 3 running (group B)
//   - Verify group B continues processing with reassigned partitions
//   - Restart group A workers (simulating network healing)
//   - Verify system rebalances and all partitions are covered
//
// Expected Behavior:
//   - Group B workers take over group A's partitions
//   - No partition gaps during partition
//   - System rebalances after healing
//   - All 50 partitions remain assigned throughout
//
// NOTE: This is a simplified partition simulation since we use embedded NATS.
// In production, network partitions would be more complex (split-brain scenarios).
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
		// Each worker should have roughly 50/6 ≈ 8 partitions (allow ±4 variance)
		require.InDelta(t, 8, count, 4,
			"Worker %s has unbalanced assignment: %d partitions", mgr.WorkerID(), count)
	}

	t.Log("Test passed - system healed gracefully after network partition")
}

// TestIntegration_HighChurn_SystemStable verifies system stability under
// continuous worker churn.
//
// Scenario:
//   - Start with 5 workers
//   - For 60 seconds: randomly add/remove workers every 5-10 seconds
//   - Maintain 3-7 workers at all times
//   - Verify system remains stable throughout
//   - Verify no partition gaps or panics
//
// Expected Behavior:
//   - System handles continuous churn without crashing
//   - All 50 partitions remain assigned at all times
//   - No resource leaks or goroutine leaks
//   - State machine remains healthy
//
// This tests the system's resilience under chaotic conditions.
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

// TestIntegration_MassWorkerFailure_Recovers verifies system recovery from
// sudden loss of majority of workers.
//
// Scenario:
//   - Start 10 workers with stable assignments
//   - Suddenly stop 8 workers (80% failure)
//   - Verify remaining 2 workers take over all partitions
//   - Gradually restart failed workers
//   - Verify system rebalances
//
// Expected Behavior:
//   - Remaining workers detect failures quickly
//   - Emergency rebalancing triggered
//   - All 50 partitions reassigned to survivors
//   - System recovers as workers rejoin
//
// This tests the system's resilience to catastrophic failures.
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

// TestIntegration_LeaderFailover_DuringRebalance verifies correct behavior
// when leader fails during active rebalancing.
//
// Scenario:
//   - Start 5 workers, wait for stable state with one leader
//   - Add 3 new workers to trigger rebalancing
//   - Stop the leader while rebalancing is in progress
//   - Verify new leader takes over and completes rebalancing
//
// Expected Behavior:
//   - New leader elected quickly (within 5 seconds)
//   - Rebalancing completes successfully
//   - All 50 partitions remain assigned
//   - No state machine deadlocks
//
// This tests leader failover during critical operations.
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

// TestIntegration_CascadingFailures_Contained verifies that worker failures
// don't trigger cascading failures in other workers.
//
// Scenario:
//   - Start 8 workers with stable assignments
//   - Stop 2 workers simultaneously (initial failure)
//   - Monitor remaining workers for stability
//   - Verify remaining 6 workers don't fail due to increased load
//
// Expected Behavior:
//   - Initial failures detected and handled
//   - Remaining workers remain stable
//   - No cascading failures or panics
//   - All 50 partitions redistributed cleanly
//
// This tests system isolation and fault containment.
func TestIntegration_CascadingFailures_Contained(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	t.Log("Test: Cascading failures contained")

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	// Create cluster with 8 workers and 50 partitions
	cluster := testutil.NewWorkerCluster(t, nc, 50)
	defer cluster.StopWorkers()

	// Start 8 workers
	t.Log("Starting 8 workers...")
	for i := 0; i < 8; i++ {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err)
	}

	// Wait for cluster to stabilize
	t.Log("Waiting for initial stabilization...")
	cluster.WaitForStableState(25 * time.Second)
	t.Log("Initial cluster stable")

	// Record initial state
	totalInitial := 0
	for _, mgr := range cluster.Workers {
		assignment := mgr.CurrentAssignment()
		totalInitial += len(assignment.Partitions)
		t.Logf("Worker %s: %d partitions initially", mgr.WorkerID(), len(assignment.Partitions))
	}
	require.Equal(t, 50, totalInitial, "expected all 50 partitions assigned initially")

	// INITIAL FAILURE: Stop 2 workers simultaneously
	t.Log("Triggering initial failure: stopping 2 workers...")
	failedIDs := make([]string, 2)
	for i := 0; i < 2; i++ {
		failedIDs[i] = cluster.Workers[i].WorkerID()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := cluster.Workers[i].Stop(stopCtx)
		stopCancel()
		require.NoError(t, err, "failed to stop worker %d", i)
	}
	t.Logf("Stopped workers: %v", failedIDs)

	// Wait for initial rebalancing after failures
	t.Log("Waiting for initial rebalancing after failures...")
	time.Sleep(15 * time.Second)

	// Monitor remaining workers for stability
	t.Log("Monitoring remaining 6 workers for cascading failures...")
	monitorCtx, monitorCancel := context.WithTimeout(ctx, 30*time.Second)
	defer monitorCancel()

	stable := true
	monitorTicker := time.NewTicker(3 * time.Second)
	defer monitorTicker.Stop()

	for {
		select {
		case <-monitorCtx.Done():
			t.Log("Monitoring complete - no cascading failures detected")
			goto MonitoringDone

		case <-monitorTicker.C:
			// Check that all remaining workers are healthy
			total := 0
			for i := 2; i < 8; i++ {
				mgr := cluster.Workers[i]
				assignment := mgr.CurrentAssignment()
				count := len(assignment.Partitions)
				total += count

				// Check if worker is still responsive
				state := mgr.State()
				t.Logf("Monitor: Worker %s - state=%s, partitions=%d",
					mgr.WorkerID(), state.String(), count)

				if state == types.StateShutdown {
					t.Errorf("CASCADING FAILURE: Worker %s failed (state=%s)",
						mgr.WorkerID(), state.String())
					stable = false
				}
			}

			// Verify partition coverage
			if total != 50 {
				t.Errorf("COVERAGE ISSUE: Only %d/50 partitions assigned", total)
				stable = false
			}
		}
	}

MonitoringDone:
	require.True(t, stable, "cascading failures detected")

	// Final verification
	totalFinal := 0
	for i := 2; i < 8; i++ {
		assignment := cluster.Workers[i].CurrentAssignment()
		totalFinal += len(assignment.Partitions)
		t.Logf("Worker %s: %d partitions after monitoring",
			cluster.Workers[i].WorkerID(), len(assignment.Partitions))
	}
	require.Equal(t, 50, totalFinal, "expected all 50 partitions assigned after monitoring")

	t.Log("Test passed - cascading failures contained successfully")
}
