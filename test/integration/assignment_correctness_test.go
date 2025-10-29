package integration_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	"github.com/arloliu/parti/test/testutil"
	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// TestAssignmentCorrectness_AllPartitionsAssigned verifies that all partitions
// are assigned exactly once across workers with no orphans or duplicates.
func TestAssignmentCorrectness_AllPartitionsAssigned(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	const (
		numPartitions = 100
		numWorkers    = 5
	)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Start embedded NATS server
	srv, conn := partitest.StartEmbeddedNATS(t)
	defer srv.Shutdown()
	defer conn.Close()

	debugLogger := logging.NewNop()

	// Create partitions with unique IDs
	partitions := make([]types.Partition, numPartitions)
	for i := range partitions {
		partitions[i] = types.Partition{
			Keys:   []string{"partition", fmt.Sprintf("%03d", i)},
			Weight: 100,
		}
	}

	// Create config with test-optimized timings
	cfg := parti.TestConfig()
	cfg.WorkerIDPrefix = "test-worker"
	cfg.WorkerIDTTL = 10 * time.Second // Longer TTL to prevent expiration during test
	cfg.ColdStartWindow = 2 * time.Second
	cfg.PlannedScaleWindow = 1 * time.Second

	// Create managers with SHARED NATS connection and debug logger
	managers := make([]*parti.Manager, numWorkers)
	for i := range managers {
		mgr, err := parti.NewManager(&cfg, conn, source.NewStatic(partitions), strategy.NewConsistentHash(), parti.WithLogger(debugLogger))
		require.NoError(t, err)
		managers[i] = mgr
	}

	// Start all managers CONCURRENTLY (simulates real deployment)
	// This prevents stable ID TTL expiration during sequential startup
	startTime := time.Now()
	var wg sync.WaitGroup
	startErrors := make([]error, numWorkers)

	for i, mgr := range managers {
		idx := i
		m := mgr
		wg.Go(func() {
			t.Logf("Starting manager %d at T+%v", idx, time.Since(startTime))
			if err := m.Start(ctx); err != nil {
				startErrors[idx] = err
				t.Logf("Manager %d failed to start: %v", idx, err)
			} else {
				t.Logf("Manager %d started successfully", idx)
			}
		})
	}

	wg.Wait()

	// Check for start errors
	for i, err := range startErrors {
		require.NoError(t, err, "manager %d failed to start", i)
	}

	// Wait for all managers to reach stable state
	mgrWaiters := make([]testutil.ManagerWaiter, numWorkers)
	for i, mgr := range managers {
		mgrWaiters[i] = mgr
	}
	err := testutil.WaitAllManagersState(ctx, mgrWaiters, parti.StateStable, 15*time.Second)
	require.NoError(t, err, "not all managers reached stable state")

	// Collect assignments from all workers
	assignmentMap := make(map[string][]string) // partition key -> worker IDs
	totalAssigned := 0

	for i, mgr := range managers {
		assignments := mgr.CurrentAssignment()
		require.NotNil(t, assignments, "manager %d has nil assignment", i)

		workerID := mgr.WorkerID()
		t.Logf("Worker %d (%s): %d partitions assigned", i, workerID, len(assignments.Partitions))

		for _, partition := range assignments.Partitions {
			partKey := partitionKey(partition)
			assignmentMap[partKey] = append(assignmentMap[partKey], workerID)
			totalAssigned++
		}
	}

	// Verify all partitions assigned
	require.Equal(t, numPartitions, len(assignmentMap),
		"Not all partitions were assigned. Expected %d, got %d", numPartitions, len(assignmentMap))

	// Verify no duplicates
	duplicates := make([]string, 0)
	orphans := make([]string, 0)

	for _, partition := range partitions {
		partKey := partitionKey(partition)
		workers := assignmentMap[partKey]
		if len(workers) == 0 {
			orphans = append(orphans, partKey)
		} else if len(workers) > 1 {
			duplicates = append(duplicates, partKey)
			t.Logf("Partition %s assigned to multiple workers: %v", partKey, workers)
		}
	}

	require.Empty(t, orphans, "Orphaned partitions (not assigned to any worker): %v", orphans)
	require.Empty(t, duplicates, "Duplicate assignments (assigned to multiple workers): %v", duplicates)

	// Verify distribution is reasonable (no worker has 0 or too many)
	for i, mgr := range managers {
		assignments := mgr.CurrentAssignment()
		count := len(assignments.Partitions)
		expectedMin := numPartitions / (numWorkers * 2) // At least 10 partitions per worker
		expectedMax := numPartitions / numWorkers * 2   // At most 40 partitions per worker

		require.GreaterOrEqual(t, count, expectedMin,
			"Worker %d has too few partitions (%d < %d)", i, count, expectedMin)
		require.LessOrEqual(t, count, expectedMax,
			"Worker %d has too many partitions (%d > %d)", i, count, expectedMax)
	}

	t.Log("All partitions assigned exactly once")
	t.Logf("Distribution: total=%d, workers=%d, avg=%.1f per worker",
		totalAssigned, numWorkers, float64(totalAssigned)/float64(numWorkers))

	// Cleanup
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	for i, mgr := range managers {
		_ = mgr.Stop(stopCtx)
		t.Logf("Stopped manager %d", i)
	}
}

// TestAssignmentCorrectness_StableAssignments verifies that assignments remain
// stable when there are no topology changes.
func TestAssignmentCorrectness_StableAssignments(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	const (
		numPartitions = 50
		numWorkers    = 3
		observePeriod = 10 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start embedded NATS server
	srv, conn := partitest.StartEmbeddedNATS(t)
	defer srv.Shutdown()
	defer conn.Close()

	// Create partitions
	partitions := make([]types.Partition, numPartitions)
	for i := range partitions {
		partitions[i] = types.Partition{
			Keys:   []string{"test", "partition", string(rune(i))},
			Weight: 100,
		}
	}

	// Create config with test-optimized timings
	cfg := parti.TestConfig()
	cfg.WorkerIDPrefix = "test-worker"
	cfg.ColdStartWindow = 2 * time.Second
	cfg.PlannedScaleWindow = 1 * time.Second

	// Create managers
	managers := make([]*parti.Manager, numWorkers)
	for i := range managers {
		mgr, err := parti.NewManager(&cfg, conn, source.NewStatic(partitions), strategy.NewConsistentHash())
		require.NoError(t, err)
		managers[i] = mgr
	}

	// Start all managers concurrently
	mgrWaiters := make([]testutil.ManagerWaiter, numWorkers)
	for i, mgr := range managers {
		mgrWaiters[i] = mgr
		go func(m *parti.Manager, idx int) {
			err := m.Start(ctx)
			require.NoError(t, err, "manager %d failed to start", idx)
		}(mgr, i)
	}

	// Wait for all managers to reach stable state
	err := testutil.WaitAllManagersState(ctx, mgrWaiters, parti.StateStable, 15*time.Second)
	require.NoError(t, err, "not all managers reached stable state")

	// Record initial assignments
	initialAssignments := make(map[int][]types.Partition) // worker index -> partitions
	for i, mgr := range managers {
		initialAssignments[i] = mgr.CurrentAssignment().Partitions
		t.Logf("Worker %d initial: %d partitions", i, len(initialAssignments[i]))
	}

	// Observe assignments over time
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	changeCount := 0
	observations := 0

	observeCtx, observeCancel := context.WithTimeout(ctx, observePeriod)
	defer observeCancel()

	for {
		select {
		case <-observeCtx.Done():
			goto done
		case <-ticker.C:
			observations++
			for i, mgr := range managers {
				current := mgr.CurrentAssignment().Partitions
				initial := initialAssignments[i]

				// Compare assignments
				if !partitionSetsEqual(initial, current) {
					changeCount++
					t.Logf("Worker %d assignment changed at observation %d: %d -> %d partitions",
						i, observations, len(initial), len(current))
					initialAssignments[i] = current // Update for next comparison
				}
			}
		}
	}

done:
	require.Equal(t, 0, changeCount,
		"Assignments changed %d times during %s with no topology changes", changeCount, observePeriod)

	t.Logf("Assignments remained stable for %s (%d observations)", observePeriod, observations)

	// Cleanup
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	for i, mgr := range managers {
		_ = mgr.Stop(stopCtx)
		t.Logf("Stopped manager %d", i)
	}
}

// partitionKey generates a unique key for a partition based on its Keys field.
func partitionKey(p types.Partition) string {
	if len(p.Keys) == 0 {
		return ""
	}
	// Join keys with a delimiter
	result := p.Keys[0]
	for i := 1; i < len(p.Keys); i++ {
		result += ":" + p.Keys[i]
	}

	return result
}

// partitionSetsEqual compares two partition slices for equality (ignoring order).
func partitionSetsEqual(a, b []types.Partition) bool {
	if len(a) != len(b) {
		return false
	}

	aMap := make(map[string]bool)
	for _, p := range a {
		aMap[partitionKey(p)] = true
	}

	for _, p := range b {
		if !aMap[partitionKey(p)] {
			return false
		}
	}

	return true
}
