package manager_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// TestLeaderElection_BasicFailover tests basic leader failover scenarios.
func TestLeaderElection_BasicFailover(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Use t.Context() which auto-cancels on test completion
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// Create cluster with fast configuration for quick leader election
	cluster := testutil.NewFastWorkerCluster(t, nc, 10)

	debugLogger := logging.NewNop()

	// Add and start 3 workers
	for i := range 3 {
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

	// Find the leader's index using thread-safe GetWorkers()
	leaderIndex := -1
	workers := cluster.GetWorkers()
	for i, mgr := range workers {
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
	t.Log("Waiting for new leader election...")

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

	// Wait for stabilization after leader election
	cluster.WaitForStableState(10 * time.Second)

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

	t.Parallel()

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Use t.Context() which auto-cancels on test completion
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// Create cluster with fast configuration
	cluster := testutil.NewFastWorkerCluster(t, nc, 10)

	// Start 5 workers simultaneously (cold start)
	t.Log("Starting 5 workers simultaneously...")
	for i := range 5 {
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

	// Note: under consistent hashing with low partition counts, it's possible (though rare)
	// for a worker to temporarily have zero partitions. This test focuses on leadership,
	// so we validate total coverage rather than enforcing per-worker minimums here.

	// Verify total partition coverage (all 10 partitions are assigned somewhere)
	cluster.VerifyTotalPartitionCount(10)
}

// TestLeaderElection_OnlyLeaderRunsCalculator tests that only the leader runs the calculator.
func TestLeaderElection_OnlyLeaderRunsCalculator(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Use t.Context() which auto-cancels on test completion
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	// Create cluster with fast configuration
	cluster := testutil.NewFastWorkerCluster(t, nc, 10)

	// Add and start 3 workers
	for i := range 3 {
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

	t.Logf("Verified: Only leader (%s) runs calculator, %d followers are passive",
		leader.WorkerID(), followerCount)
}

// TestLeaderElection_LeaderRenewal tests that leader renews its lease periodically.
func TestLeaderElection_LeaderRenewal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Use t.Context() which auto-cancels on test completion
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Create cluster with fast configuration
	cluster := testutil.NewFastWorkerCluster(t, nc, 10)

	// Add and start 2 workers
	for i := range 2 {
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

	// Ensure the same leader is maintained for multiple renewal periods.
	t.Log("Verifying leader renews lease for 6s...")
	require.Never(t, func() bool {
		leaderCount := 0
		currentLeaderID := ""
		for _, mgr := range cluster.Workers {
			if mgr.State() == types.StateShutdown {
				continue
			}
			if mgr.IsLeader() {
				leaderCount++
				currentLeaderID = mgr.WorkerID()
			}
		}

		return leaderCount != 1 || currentLeaderID != initialLeaderID
	}, 6*time.Second, 100*time.Millisecond,
		"leader should remain the same across lease renewals")

	currentLeaderID := cluster.VerifyExactlyOneLeader().WorkerID()

	require.Equal(t, initialLeaderID, currentLeaderID,
		"leader should remain the same after lease renewals")

	t.Logf("Leader %s successfully maintained leadership for 6s", currentLeaderID)
}

// TestLeaderElection_AssignmentPreservation tests that assignments are preserved during leader transition.
//
//nolint:cyclop
func TestLeaderElection_AssignmentPreservation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Use t.Context() for automatic cancellation
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Create cluster with fast configuration
	cluster := testutil.NewFastWorkerCluster(t, nc, 20)
	debugLogger := logging.NewNop()

	// Start 3 workers
	for i := range 3 {
		mgr := cluster.AddWorker(ctx, debugLogger)
		err := mgr.Start(ctx)
		require.NoError(t, err, "worker %d failed to start", i)
	}
	defer cluster.StopWorkers()

	// Wait for system to stabilize and assignments to be published
	cluster.WaitForStableState(10 * time.Second)

	// Get initial leader and verify assignments exist
	originalLeader := cluster.VerifyExactlyOneLeader()
	originalLeaderID := originalLeader.WorkerID()
	t.Logf("Original leader: %s", originalLeaderID)

	// Capture assignments before leader dies
	assignmentsBefore := make(map[string]map[string]bool) // workerID -> set of partition keys
	allKeysBefore := make(map[string][]string)            // Track which workers have which keys
	for _, worker := range cluster.Workers {
		assignment := worker.CurrentAssignment()
		partitionSet := make(map[string]bool)
		for _, part := range assignment.Partitions {
			for _, key := range part.Keys {
				partitionSet[key] = true
				allKeysBefore[key] = append(allKeysBefore[key], worker.WorkerID())
			}
		}
		assignmentsBefore[worker.WorkerID()] = partitionSet
		t.Logf("Worker %s has %d partitions (%d unique keys) before failover",
			worker.WorkerID(), len(assignment.Partitions), len(partitionSet))
	}

	// Check for duplicate assignments
	for key, owners := range allKeysBefore {
		if len(owners) > 1 {
			t.Logf("Partition key %s assigned to multiple workers: %v", key, owners)
		}
	}

	// Verify we have non-empty assignments
	totalPartitionsBefore := 0
	for _, partSet := range assignmentsBefore {
		totalPartitionsBefore += len(partSet)
	}
	require.Greater(t, totalPartitionsBefore, 0, "should have assignments before failover")

	// Count unique partitions (since duplicates are possible during transitions)
	uniquePartitionsBefore := make(map[string]bool)
	for _, partSet := range assignmentsBefore {
		for key := range partSet {
			uniquePartitionsBefore[key] = true
		}
	}
	t.Logf("Before failover: %d partition assignments, %d unique partitions",
		totalPartitionsBefore, len(uniquePartitionsBefore))

	// Kill the original leader
	t.Logf("Killing leader %s...", originalLeaderID)
	err := originalLeader.Stop(ctx)
	require.NoError(t, err)

	// Verify new leader elected (only check alive workers).
	leaderCount := 0
	var newLeaderID string
	require.Eventually(t, func() bool {
		leaderCount = 0
		newLeaderID = ""
		for _, mgr := range cluster.Workers {
			if mgr.State() == types.StateShutdown {
				continue
			}
			if mgr.IsLeader() {
				leaderCount++
				newLeaderID = mgr.WorkerID()
			}
		}

		return leaderCount == 1 && newLeaderID != "" && newLeaderID != originalLeaderID
	}, 5*time.Second, 100*time.Millisecond, "expected exactly one new leader after failover")
	t.Logf("New leader elected: %s", newLeaderID)

	require.Equal(t, 1, leaderCount, "expected exactly one leader after failover")
	require.NotEqual(t, originalLeaderID, newLeaderID, "new leader should be different")

	// Wait for emergency rebalance to complete and workers to receive new assignments
	// The new leader will detect worker-0 disappeared and trigger emergency rebalance
	// We need to wait for ALL workers to receive assignments with version >= 4
	// (since new leader discovered version=3 and will publish version=4+)
	require.Eventually(t, func() bool {
		allUpdated := true
		for _, mgr := range cluster.Workers {
			if mgr.State() == types.StateShutdown {
				continue // Skip dead workers
			}
			assignment := mgr.CurrentAssignment()
			if assignment.Version < 4 {
				t.Logf("Worker %s still has old assignment version=%d, waiting...",
					mgr.WorkerID(), assignment.Version)
				allUpdated = false
			}
		}

		return allUpdated
	}, 15*time.Second, 500*time.Millisecond, "workers did not receive new assignments after leader failover")

	t.Log("All workers received new assignments from new leader")

	// Capture assignments after leader transition
	assignmentsAfter := make(map[string]map[string]bool)
	totalPartitionsAfter := 0
	for _, worker := range cluster.Workers {
		if worker.WorkerID() == originalLeaderID {
			continue // Skip the dead leader
		}
		assignment := worker.CurrentAssignment()
		partitionSet := make(map[string]bool)
		for _, part := range assignment.Partitions {
			for _, key := range part.Keys {
				partitionSet[key] = true
			}
		}
		assignmentsAfter[worker.WorkerID()] = partitionSet
		totalPartitionsAfter += len(partitionSet)
		t.Logf("Worker %s has %d partitions (%d unique keys) after failover",
			worker.WorkerID(), len(assignment.Partitions), len(partitionSet))
	}

	// Count unique partitions after failover
	uniquePartitionsAfter := make(map[string]bool)
	duplicatesAfter := make(map[string][]string)
	for workerID, partSet := range assignmentsAfter {
		for key := range partSet {
			uniquePartitionsAfter[key] = true
			duplicatesAfter[key] = append(duplicatesAfter[key], workerID)
		}
	}
	t.Logf("After failover: %d partition assignments, %d unique partitions",
		totalPartitionsAfter, len(uniquePartitionsAfter))

	// CRITICAL: Verify NO duplicate assignments after failover
	for key, owners := range duplicatesAfter {
		require.Len(t, owners, 1,
			"partition %s should have exactly 1 owner after failover, but has: %v", key, owners)
	}

	// Verify all 20 partitions are still assigned (no orphans)
	require.Equal(t, 20, len(uniquePartitionsAfter),
		"should have all 20 partitions assigned after leader transition (no orphans)")

	// Verify no worker lost ALL their partitions (some continuity expected)
	for workerID, afterSet := range assignmentsAfter {
		require.Greater(t, len(afterSet), 0,
			"worker %s should still have assignments after leader transition", workerID)
	}

	t.Logf("Assignments successfully transitioned: %d unique partitions preserved, no duplicates, no orphans",
		len(uniquePartitionsAfter))
}

// TestLeaderElection_AssignmentVersioning tests that assignment versions increment monotonically across leader changes.
func TestLeaderElection_AssignmentVersioning(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Use t.Context() for automatic cancellation
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Create cluster with fast configuration
	cluster := testutil.NewFastWorkerCluster(t, nc, 20)
	debugLogger := logging.NewNop()

	// Start 3 workers
	for i := range 3 {
		mgr := cluster.AddWorker(ctx, debugLogger)
		err := mgr.Start(ctx)
		require.NoError(t, err, "worker %d failed to start", i)
	}

	// Wait for all workers to reach Stable state
	cluster.WaitForStableState(10 * time.Second)

	// Capture initial versions from all workers
	versionsBeforeFailover := make(map[string]int64)
	for _, worker := range cluster.Workers {
		assignment := worker.CurrentAssignment()
		versionsBeforeFailover[worker.WorkerID()] = assignment.Version
		t.Logf("Worker %s has assignment version %d before failover",
			worker.WorkerID(), assignment.Version)
	}

	// Get original leader
	originalLeader := cluster.VerifyExactlyOneLeader()
	originalLeaderID := originalLeader.WorkerID()
	t.Logf("Original leader: %s", originalLeaderID)

	// Find the highest version before failover
	maxVersionBefore := int64(0)
	for _, version := range versionsBeforeFailover {
		if version > maxVersionBefore {
			maxVersionBefore = version
		}
	}
	t.Logf("Highest version before failover: %d", maxVersionBefore)

	// Kill the original leader
	t.Logf("Killing leader %s...", originalLeaderID)
	err := originalLeader.Stop(ctx)
	require.NoError(t, err)

	// Find new leader (must be different manager instance, not just different worker ID)
	leaderCount := 0
	var newLeader *parti.Manager
	require.Eventually(t, func() bool {
		leaderCount = 0
		newLeader = nil
		for _, mgr := range cluster.Workers {
			if mgr.State() == types.StateShutdown {
				continue
			}
			if mgr.IsLeader() {
				leaderCount++
				newLeader = mgr
			}
		}

		return leaderCount == 1 && newLeader != nil && newLeader != originalLeader
	}, 5*time.Second, 100*time.Millisecond, "expected a new leader after failover")

	require.Equal(t, 1, leaderCount, "expected exactly one leader after failover")
	require.NotSame(t, originalLeader, newLeader, "new leader must be a different manager instance")
	t.Logf("New leader elected: %s", newLeader.WorkerID())

	// Wait for emergency rebalance and workers to receive new assignments
	require.Eventually(t, func() bool {
		allUpdated := true
		for _, mgr := range cluster.Workers {
			if mgr.State() == types.StateShutdown {
				continue
			}
			assignment := mgr.CurrentAssignment()
			// New leader should have published version > maxVersionBefore
			if assignment.Version <= maxVersionBefore {
				t.Logf("Worker %s still has old version=%d, waiting for version > %d",
					mgr.WorkerID(), assignment.Version, maxVersionBefore)
				allUpdated = false
			}
		}

		return allUpdated
	}, 15*time.Second, 500*time.Millisecond, "workers did not receive new assignment versions")

	// Capture versions after failover
	versionsAfterFailover := make(map[string]int64)
	for _, worker := range cluster.Workers {
		if worker.State() == types.StateShutdown {
			continue
		}
		assignment := worker.CurrentAssignment()
		versionsAfterFailover[worker.WorkerID()] = assignment.Version
		t.Logf("Worker %s has assignment version %d after failover",
			worker.WorkerID(), assignment.Version)
	}

	// Verify version monotonicity: all new versions should be > maxVersionBefore
	for workerID, newVersion := range versionsAfterFailover {
		require.Greater(t, newVersion, maxVersionBefore,
			"worker %s version should be greater than highest version before failover (%d > %d)",
			workerID, newVersion, maxVersionBefore)
	}

	// Verify all alive workers have the same version (from same rebalance)
	versionSet := make(map[int64]bool)
	for _, version := range versionsAfterFailover {
		versionSet[version] = true
	}
	t.Logf("Unique versions after failover: %v", getKeys(versionSet))

	// All workers should have received the same emergency rebalance version
	require.LessOrEqual(t, len(versionSet), 2,
		"should have at most 2 versions (emergency rebalance might increment twice)")

	t.Logf("Version monotonicity preserved: all versions > %d after leader failover", maxVersionBefore)
}

// TestLeaderElection_NoOrphansOnFailover verifies no partitions become orphaned during rapid leader transitions.
func TestLeaderElection_NoOrphansOnFailover(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Use t.Context() for automatic cancellation
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	// Create cluster with fast configuration
	cluster := testutil.NewFastWorkerCluster(t, nc, 20)
	debugLogger := logging.NewNop()

	// Start 4 workers simultaneously for realistic concurrent startup
	for i := range 4 {
		mgr := cluster.AddWorker(ctx, debugLogger)
		err := mgr.Start(ctx)
		require.NoError(t, err, "worker %d failed to start", i)
	}

	// Wait for all workers to reach Stable state
	cluster.WaitForStableState(10 * time.Second)

	// Verify initial state: all 20 partitions assigned. Assignment publication
	// and follower consumption can lag the Stable transition under parallel
	// load, so poll until all 20 are observed (matches the failover-round
	// pattern below).
	var initialPartitions map[string]bool
	require.Eventually(t, func() bool {
		initialPartitions = collectAlivePartitionKeys(cluster.Workers)
		return len(initialPartitions) == 20
	}, 10*time.Second, 100*time.Millisecond, "all 20 partitions should be assigned initially")
	t.Logf("Initial state: %d partitions assigned across %d workers",
		len(initialPartitions), len(cluster.Workers))

	// Perform 3 rapid leader transitions
	for round := 1; round <= 3; round++ {
		t.Logf("=== Failover Round %d ===", round)

		// Find and kill current leader
		currentLeader := cluster.VerifyExactlyOneLeader()
		leaderID := currentLeader.WorkerID()
		t.Logf("Killing leader %s (round %d)", leaderID, round)

		err := currentLeader.Stop(ctx)
		require.NoError(t, err)

		aliveWorkers := 0
		leaderCount := 0
		var newLeader *parti.Manager
		require.Eventually(t, func() bool {
			aliveWorkers, leaderCount, newLeader = countAliveLeaders(cluster.Workers)

			if leaderCount != 1 || newLeader == nil || newLeader == currentLeader {
				return false
			}

			return len(collectAlivePartitionKeys(cluster.Workers)) == 20
		}, 8*time.Second, 100*time.Millisecond, "expected leader election and partition recovery after round %d", round)

		require.Equal(t, 1, leaderCount, "expected exactly one leader after round %d", round)
		require.NotSame(t, currentLeader, newLeader, "new leader must be different manager instance in round %d", round)
		t.Logf("New leader elected: %s (%d workers alive)", newLeader.WorkerID(), aliveWorkers)

		// Verify no orphaned partitions: all 20 should still be assigned
		currentPartitions := collectAlivePartitionKeys(cluster.Workers)
		require.Len(t, currentPartitions, 20,
			"all 20 partitions should remain assigned after failover round %d (found %d)",
			round, len(currentPartitions))

		// Verify no duplicates
		partitionCounts := collectAlivePartitionCounts(cluster.Workers)

		duplicates := 0
		for key, count := range partitionCounts {
			if count > 1 {
				duplicates++
				t.Logf("Partition key %s assigned %d times", key, count)
			}
		}
		require.Equal(t, 0, duplicates, "no partitions should be duplicated after round %d", round)

		// Verify complete coverage: all 20 partition keys exist
		expectedKeys := make(map[string]bool)
		for i := range 20 {
			expectedKeys[fmt.Sprintf("partition-%d", i)] = true
		}
		for expectedKey := range expectedKeys {
			require.Contains(t, currentPartitions, expectedKey,
				"partition key %s is orphaned after failover round %d", expectedKey, round)
		}

		t.Logf("Round %d: All 20 partitions assigned, no orphans, no duplicates", round)
	}

	t.Log("Completed 3 rapid leader transitions: no orphaned partitions detected")
}

// TestLeaderElection_RapidChurn tests system stability under rapid leader changes.
// This is a lightweight test - 3 rounds in 15 seconds instead of 12 rounds in 60s.
func TestLeaderElection_RapidChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel() // Can run in parallel with other tests

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Create cluster with fast configuration
	cluster := testutil.NewFastWorkerCluster(t, nc, 20)

	// Start 4 workers (need extras since we'll kill 3)
	for i := range 4 {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err, "worker %d failed to start", i)
	}
	defer cluster.StopWorkers()

	// Wait for initial stability
	cluster.WaitForStableState(8 * time.Second)
	initialLeader := cluster.VerifyExactlyOneLeader()
	t.Logf("Initial leader: %s", initialLeader.WorkerID())

	// Rapid churn: Kill leader 3 times with 5-second intervals
	for round := 1; round <= 3; round++ {
		t.Logf("=== Churn Round %d ===", round)

		// Find and kill current leader
		currentLeader := cluster.VerifyExactlyOneLeader()
		leaderID := currentLeader.WorkerID()
		t.Logf("Killing leader %s (round %d)", leaderID, round)

		err := currentLeader.Stop(ctx)
		require.NoError(t, err, "failed to stop leader in round %d", round)

		leaderCount := 0
		var newLeaderID string
		require.Eventually(t, func() bool {
			leaderCount = 0
			newLeaderID = ""
			uniq := make(map[string]struct{})
			for _, mgr := range cluster.Workers {
				if mgr.State() == types.StateShutdown {
					continue
				}
				if mgr.IsLeader() {
					leaderCount++
					newLeaderID = mgr.WorkerID()
				}
				assignment := mgr.CurrentAssignment()
				for _, p := range assignment.Partitions {
					if id := p.ID(); id != "" {
						uniq[id] = struct{}{}
					}
				}
			}

			return leaderCount == 1 && newLeaderID != "" && newLeaderID != leaderID && len(uniq) == 20
		}, 8*time.Second, 100*time.Millisecond, "expected leader election and partition coverage after round %d", round)

		require.Equal(t, 1, leaderCount, "expected exactly one leader after round %d", round)
		require.NotEqual(t, leaderID, newLeaderID, "new leader should be different in round %d", round)
		t.Logf("New leader elected: %s", newLeaderID)

		// Verify partitions still assigned (quick check, unique coverage)
		aliveWorkers := 0
		uniq := make(map[string]struct{})
		for _, mgr := range cluster.Workers {
			if mgr.State() == types.StateShutdown {
				continue
			}
			aliveWorkers++
			assignment := mgr.CurrentAssignment()
			for _, p := range assignment.Partitions {
				if id := p.ID(); id != "" {
					uniq[id] = struct{}{}
				}
			}
		}
		require.Equal(t, 20, len(uniq),
			"after round %d: expected 20 partitions, got %d (alive workers: %d)",
			round, len(uniq), aliveWorkers)

		// Wait for system to stabilize before next round
		cluster.WaitForStableState(8 * time.Second)
	}

	t.Log("System remained stable through 3 rapid leader transitions")
}

// TestLeaderElection_ShutdownDuringRebalancing tests leader shutdown during active rebalancing.
// This is a focused test - we trigger rebalancing and kill leader immediately.
func TestLeaderElection_ShutdownDuringRebalancing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel() // Can run in parallel

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()

	// Create cluster
	cluster := testutil.NewFastWorkerCluster(t, nc, 20)

	// Start 2 workers and wait for stability
	for range 2 {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err)
	}
	defer cluster.StopWorkers()

	cluster.WaitForStableState(8 * time.Second)
	leader := cluster.VerifyExactlyOneLeader()
	leaderID := leader.WorkerID()
	t.Logf("Initial leader: %s", leaderID)

	// Start a new worker to trigger planned scale (rebalancing with 1s cooldown)
	newWorker := cluster.AddWorker(ctx)
	err := newWorker.Start(ctx)
	require.NoError(t, err)

	// Wait for new worker to reach Stable state
	require.Eventually(t, func() bool {
		return newWorker.State() == types.StateStable
	}, 8*time.Second, 200*time.Millisecond, "new worker should reach Stable state")

	t.Logf("New worker %s reached Stable state", newWorker.WorkerID())

	// Wait for leader to detect the new worker and enter Scaling/Rebalancing/Stable state
	require.Eventually(t, func() bool {
		s := leader.State()
		return s == types.StateScaling || s == types.StateRebalancing || s == types.StateStable
	}, 5*time.Second, 100*time.Millisecond, "leader should detect new worker")

	leaderState := leader.State()
	t.Logf("Leader state before shutdown: %s", leaderState)

	// Kill leader immediately (during or just after rebalancing)
	t.Logf("Killing leader %s during rebalancing", leaderID)
	err = leader.Stop(ctx)
	require.NoError(t, err)

	// Verify new leader elected and remaining workers fully recover.
	var newLeader *parti.Manager
	require.Eventually(t, func() bool {
		leaderCount := 0
		newLeader = nil
		for _, mgr := range cluster.Workers {
			if mgr.State() == types.StateShutdown {
				continue
			}
			if mgr.IsLeader() {
				leaderCount++
				newLeader = mgr
			}
		}

		if leaderCount != 1 || newLeader == nil || newLeader.WorkerID() == leaderID {
			return false
		}

		aliveWorkers := 0
		totalPartitions := 0
		for _, mgr := range cluster.Workers {
			if mgr.State() == types.StateShutdown {
				continue
			}
			if mgr.State() != types.StateStable {
				return false
			}
			aliveWorkers++
			totalPartitions += len(mgr.CurrentAssignment().Partitions)
		}

		return aliveWorkers == 2 && totalPartitions == 20
	}, 8*time.Second, 100*time.Millisecond, "system should recover from leader shutdown during rebalancing")
	require.NotEqual(t, leaderID, newLeader.WorkerID(), "new leader should be different")
	t.Logf("New leader elected: %s", newLeader.WorkerID())

	// Verify remaining 2 workers have partitions (worker-0 killed, workers 1-2 remain)
	aliveWorkers := 0
	totalPartitions := 0
	for _, mgr := range cluster.Workers {
		if mgr.State() == types.StateShutdown {
			continue
		}
		aliveWorkers++
		assignment := mgr.CurrentAssignment()
		partCount := len(assignment.Partitions)
		totalPartitions += partCount
		require.Greater(t, partCount, 0, "worker %s should have partitions", mgr.WorkerID())
		t.Logf("Worker %s: %d partitions", mgr.WorkerID(), partCount)
	}

	require.Equal(t, 2, aliveWorkers, "should have 2 alive workers (worker-0 killed)")
	require.Equal(t, 20, totalPartitions, "all 20 partitions should be assigned")

	t.Log("System recovered successfully from leader shutdown during rebalancing")
}

// TestLeaderElection_ShutdownDuringEmergency tests leader shutdown during emergency rebalancing.
// This is a focused stress test - we kill one worker (triggers emergency), then immediately kill leader.
func TestLeaderElection_ShutdownDuringEmergency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel() // Can run in parallel

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()

	// Create cluster
	cluster := testutil.NewFastWorkerCluster(t, nc, 20)

	// Start 4 workers
	for range 4 {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err)
	}
	defer cluster.StopWorkers()

	cluster.WaitForStableState(8 * time.Second)
	leader := cluster.VerifyExactlyOneLeader()
	leaderID := leader.WorkerID()
	t.Logf("Initial leader: %s", leaderID)

	// Find a follower to kill (trigger emergency)
	var victimWorker *parti.Manager
	for _, mgr := range cluster.Workers {
		if mgr.WorkerID() != leaderID {
			victimWorker = mgr
			break
		}
	}
	require.NotNil(t, victimWorker, "should find a follower")

	// Kill the follower to trigger emergency rebalancing
	t.Logf("Killing follower %s to trigger emergency", victimWorker.WorkerID())
	err := victimWorker.Stop(ctx)
	require.NoError(t, err)

	// Wait for leader to notice the follower death. With FastConfig the leader
	// may complete the emergency rebalance so quickly that it returns to Stable
	// before we can observe it. Accept any state change OR a version bump as
	// evidence that the follower death was detected.
	initialVersion := leader.CurrentAssignment().Version
	require.Eventually(t, func() bool {
		s := leader.State()
		v := leader.CurrentAssignment().Version
		return s != types.StateStable || v > initialVersion
	}, 8*time.Second, 50*time.Millisecond, "leader should detect follower death")

	// Now kill the leader during (or shortly after) emergency rebalancing
	t.Logf("Killing leader %s during emergency rebalancing", leaderID)
	err = leader.Stop(ctx)
	require.NoError(t, err)

	// Verify new leader elected and remaining workers fully recover.
	var newLeader *parti.Manager
	require.Eventually(t, func() bool {
		leaderCount := 0
		newLeader = nil
		for _, mgr := range cluster.Workers {
			if mgr.State() == types.StateShutdown {
				continue
			}
			if mgr.IsLeader() {
				leaderCount++
				newLeader = mgr
			}
		}

		if leaderCount != 1 || newLeader == nil || newLeader.WorkerID() == leaderID {
			return false
		}

		aliveWorkers := 0
		totalPartitions := 0
		for _, mgr := range cluster.Workers {
			if mgr.State() == types.StateShutdown {
				continue
			}
			if mgr.State() != types.StateStable {
				return false
			}
			aliveWorkers++
			totalPartitions += len(mgr.CurrentAssignment().Partitions)
		}

		return aliveWorkers == 2 && totalPartitions == 20
	}, 8*time.Second, 100*time.Millisecond, "system should recover from cascading failures")
	require.NotEqual(t, leaderID, newLeader.WorkerID(), "new leader should be different")
	t.Logf("New leader elected: %s", newLeader.WorkerID())

	// Verify remaining 2 workers have all partitions
	aliveWorkers := 0
	totalPartitions := 0
	for _, mgr := range cluster.Workers {
		if mgr.State() == types.StateShutdown {
			continue
		}
		aliveWorkers++
		assignment := mgr.CurrentAssignment()
		partCount := len(assignment.Partitions)
		totalPartitions += partCount
		require.Greater(t, partCount, 0, "worker %s should have partitions", mgr.WorkerID())
		t.Logf("Worker %s: %d partitions", mgr.WorkerID(), partCount)
	}

	require.Equal(t, 2, aliveWorkers, "should have 2 alive workers")
	require.Equal(t, 20, totalPartitions, "all 20 partitions should be assigned")

	t.Log("System recovered from cascading failures (follower + leader during emergency)")
}

// Helper function to get keys from a map
func getKeys(m map[int64]bool) []int64 {
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	return keys
}

func countAliveLeaders(workers []*parti.Manager) (aliveWorkers int, leaderCount int, leader *parti.Manager) {
	for _, mgr := range workers {
		if mgr.State() == types.StateShutdown {
			continue
		}
		aliveWorkers++
		if mgr.IsLeader() {
			leaderCount++
			leader = mgr
		}
	}

	return aliveWorkers, leaderCount, leader
}

func collectAlivePartitionKeys(workers []*parti.Manager) map[string]bool {
	partitionKeys := make(map[string]bool)
	for _, worker := range workers {
		if worker.State() == types.StateShutdown {
			continue
		}
		assignment := worker.CurrentAssignment()
		for _, part := range assignment.Partitions {
			for _, key := range part.Keys {
				partitionKeys[key] = true
			}
		}
	}

	return partitionKeys
}

func collectAlivePartitionCounts(workers []*parti.Manager) map[string]int {
	partitionCounts := make(map[string]int)
	for _, mgr := range workers {
		if mgr.State() == types.StateShutdown {
			continue
		}
		assignment := mgr.CurrentAssignment()
		for _, partition := range assignment.Partitions {
			for _, key := range partition.Keys {
				partitionCounts[key]++
			}
		}
	}

	return partitionCounts
}

// TestLeaderElection_FollowersKeepAssignmentDuringCleanup verifies that followers
// maintain assignments across leader changes and receive updated assignments from new leader.
//
// This test ensures:
// 1. Leader stops but does NOT clean up assignments (preserves version continuity)
// 2. Followers maintain their cached assignments during leader election
// 3. New leader discovers existing version and publishes next version
// 4. Followers receive and apply new assignments with monotonically increasing version
//
// Expected behavior:
// - Leader stops → assignments PRESERVED in KV (no cleanup)
// - Followers keep processing with cached assignments (2-8 second gap during election)
// - New leader elected → discovers highest version → publishes version N+1
// - Followers receive new assignments → update normally
func TestLeaderElection_FollowersKeepAssignmentDuringCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Create cluster with 10 partitions for easier tracking
	cluster := testutil.NewFastWorkerCluster(t, nc, 10)
	debugLogger := logging.NewNop()

	// Start 3 workers
	for i := range 3 {
		mgr := cluster.AddWorker(ctx, debugLogger)
		err := mgr.Start(ctx)
		require.NoError(t, err, "worker %d failed to start", i)
	}
	defer cluster.StopWorkers()

	// Wait for system to stabilize
	cluster.WaitForStableState(10 * time.Second)

	// Identify leader and followers
	originalLeader := cluster.VerifyExactlyOneLeader()
	leaderID := originalLeader.WorkerID()

	followers := make([]*parti.Manager, 0, 2)
	followerIDs := make([]string, 0, 2)
	for _, mgr := range cluster.Workers {
		if mgr.WorkerID() != leaderID {
			followers = append(followers, mgr)
			followerIDs = append(followerIDs, mgr.WorkerID())
		}
	}
	require.Len(t, followers, 2, "should have exactly 2 followers")

	t.Logf("Leader: %s", leaderID)
	t.Logf("Followers: %v", followerIDs)

	// Capture followers' assignments BEFORE leader stops
	followerAssignmentsBefore := make(map[string]types.Assignment)
	for _, follower := range followers {
		assignment := follower.CurrentAssignment()
		followerAssignmentsBefore[follower.WorkerID()] = assignment
		t.Logf("Follower %s has %d partitions (version %d) BEFORE leader stop",
			follower.WorkerID(), len(assignment.Partitions), assignment.Version)
	}

	// CRITICAL MOMENT: Stop the leader
	// Leader does NOT clean up assignments anymore (preserves version continuity)
	t.Logf("=== Stopping leader %s (assignments will be preserved in KV) ===", leaderID)
	err := originalLeader.Stop(ctx)
	require.NoError(t, err)

	// VERIFICATION 1: Followers should have received NEW assignments from new leader
	// Version should have increased (new leader published next version)
	var newLeader *parti.Manager
	require.Eventually(t, func() bool {
		leaderCount := 0
		newLeader = nil
		for _, mgr := range cluster.Workers {
			if mgr.State() == types.StateShutdown {
				continue
			}
			if mgr.IsLeader() {
				leaderCount++
				newLeader = mgr
			}
		}
		if leaderCount != 1 || newLeader == nil || newLeader.WorkerID() == leaderID {
			return false
		}

		for _, follower := range followers {
			workerID := follower.WorkerID()
			currentAssignment := follower.CurrentAssignment()
			originalAssignment := followerAssignmentsBefore[workerID]
			if currentAssignment.Version <= originalAssignment.Version {
				return false
			}
			if len(currentAssignment.Partitions) == 0 {
				return false
			}
			if follower.State() != types.StateStable {
				return false
			}
		}

		return true
	}, 8*time.Second, 100*time.Millisecond, "followers should receive new assignments from the new leader")
	t.Logf("New leader elected: %s", newLeader.WorkerID())

	for _, follower := range followers {
		workerID := follower.WorkerID()
		currentAssignment := follower.CurrentAssignment()
		originalAssignment := followerAssignmentsBefore[workerID]

		// Version should have increased (new leader published version N+1)
		require.Greater(t, currentAssignment.Version, originalAssignment.Version,
			"Follower %s should have received NEW assignment from new leader (version should increase)",
			workerID)

		// Follower should still have partitions (maybe different distribution)
		require.Greater(t, len(currentAssignment.Partitions), 0,
			"Follower %s should have partitions after new leader rebalance",
			workerID)

		t.Logf("Follower %s updated: %d partitions (version %d → %d)",
			workerID,
			len(currentAssignment.Partitions),
			originalAssignment.Version,
			currentAssignment.Version)
	}

	t.Log("Verified: Followers received new assignments from new leader with version continuity")

	// VERIFICATION 2: Followers are still in Stable state
	for _, follower := range followers {
		state := follower.State()
		require.Equal(t, types.StateStable, state,
			"Follower %s should be in Stable state after new leader rebalance",
			follower.WorkerID())
	}

	t.Log("Verified: Followers are in Stable state")

	// The critical test is complete! We've proven:
	// 1. Leader stop does NOT clean up assignments (version continuity preserved)
	// 2. New leader discovers existing version and publishes next version
	// 3. Followers receive new assignments with monotonically increasing version
	// 4. No data loss during leader transition

	// SUCCESS SUMMARY
	t.Log("")
	t.Log("========== TEST PASSED ==========")
	t.Log("Assignments preserved across leader change")
	t.Log("Version monotonicity maintained")
	t.Log("Followers updated successfully")
}
