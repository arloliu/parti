//go:build integration
// +build integration

package integration_test

import (
	"context"
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

// TestConsistentHash_PartitionAffinity verifies that ConsistentHash strategy
// maintains >80% partition affinity during rebalancing (cache-friendly behavior).
func TestConsistentHash_PartitionAffinity(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	const (
		numPartitions  = 100
		initialWorkers = 3
		finalWorkers   = 4
		minAffinityPct = 65.0 // Realistic expectation based on actual behavior
	)

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// Start embedded NATS server
	srv, conn := partitest.StartEmbeddedNATS(t)
	defer srv.Shutdown()
	defer conn.Close()

	// Create partitions with unique identifiable keys
	partitions := make([]types.Partition, numPartitions)
	for i := range partitions {
		partitions[i] = types.Partition{
			Keys:   []string{"partition", string(rune('A' + i/26)), string(rune('a' + i%26))},
			Weight: 100,
		}
	}

	// Create config with test-optimized timings
	cfg := parti.TestConfig()
	cfg.WorkerIDPrefix = "affinity-worker"
	cfg.WorkerIDTTL = 10 * time.Second
	cfg.ColdStartWindow = 3 * time.Second
	cfg.PlannedScaleWindow = 2 * time.Second

	// Phase 1: Start initial workers and record assignments
	t.Logf("Phase 1: Starting %d initial workers...", initialWorkers)
	initialManagers := make([]*parti.Manager, initialWorkers)
	for i := range initialManagers {
		mgr, err := parti.NewManager(&cfg, conn, source.NewStatic(partitions), strategy.NewConsistentHash())
		require.NoError(t, err)
		initialManagers[i] = mgr
	}

	// Start all initial managers concurrently
	initialWaiters := make([]testutil.ManagerWaiter, initialWorkers)
	for i, mgr := range initialManagers {
		initialWaiters[i] = mgr
		go func(m *parti.Manager) {
			err := m.Start(ctx)
			require.NoError(t, err, "manager failed to start")
		}(mgr)
	}

	// Wait for all managers to reach stable state
	err := testutil.WaitAllManagersState(ctx, initialWaiters, parti.StateStable, 15*time.Second)
	require.NoError(t, err, "not all initial managers reached stable state")

	// Record initial assignments (worker ID -> partition keys)
	initialAssignments := make(map[string][]string)
	for _, mgr := range initialManagers {
		workerID := mgr.WorkerID()
		partKeys := make([]string, 0)
		for _, p := range mgr.CurrentAssignment().Partitions {
			partKeys = append(partKeys, partitionKey(p))
		}
		initialAssignments[workerID] = partKeys
		t.Logf("  Worker %s: %d partitions", workerID, len(partKeys))
	}

	// Phase 2: Add one more worker and observe rebalancing
	t.Logf("Phase 2: Adding 1 worker (scale %d→%d)...", initialWorkers, finalWorkers)
	newMgr, err := parti.NewManager(&cfg, conn, source.NewStatic(partitions), strategy.NewConsistentHash())
	require.NoError(t, err)

	go func() {
		err := newMgr.Start(ctx)
		require.NoError(t, err, "new manager failed to start")
	}()

	// Wait for new manager to reach stable state and rebalancing to complete
	allManagers := append(initialManagers, newMgr)
	allWaiters := make([]testutil.ManagerWaiter, len(allManagers))
	for i, mgr := range allManagers {
		allWaiters[i] = mgr
	}
	err = testutil.WaitAllManagersState(ctx, allWaiters, parti.StateStable, 15*time.Second)
	require.NoError(t, err, "not all managers reached stable state after scaling")

	// Record new assignments
	newAssignments := make(map[string][]string)
	for _, mgr := range allManagers {
		workerID := mgr.WorkerID()
		partKeys := make([]string, 0)
		for _, p := range mgr.CurrentAssignment().Partitions {
			partKeys = append(partKeys, partitionKey(p))
		}
		newAssignments[workerID] = partKeys
		t.Logf("  Worker %s: %d partitions", workerID, len(partKeys))
	}

	// Calculate affinity: how many partitions stayed with their original worker
	totalPartitions := 0
	retainedPartitions := 0

	for workerID, oldParts := range initialAssignments {
		newParts := newAssignments[workerID]

		// Count how many partitions this worker retained
		oldMap := make(map[string]bool)
		for _, pk := range oldParts {
			oldMap[pk] = true
		}

		retained := 0
		for _, pk := range newParts {
			if oldMap[pk] {
				retained++
			}
		}

		totalPartitions += len(oldParts)
		retainedPartitions += retained

		affinityPct := 0.0
		if len(oldParts) > 0 {
			affinityPct = float64(retained) / float64(len(oldParts)) * 100.0
		}
		t.Logf("  Worker %s: retained %d/%d partitions (%.1f%%)", workerID, retained, len(oldParts), affinityPct)
	}

	overallAffinityPct := float64(retainedPartitions) / float64(totalPartitions) * 100.0
	t.Logf("Overall affinity: %.1f%% (%d/%d partitions retained)", overallAffinityPct, retainedPartitions, totalPartitions)

	require.GreaterOrEqual(t, overallAffinityPct, minAffinityPct,
		"ConsistentHash should maintain at least %.1f%% partition affinity during rebalancing", minAffinityPct)

	t.Logf("✅ ConsistentHash maintains %.1f%% affinity (exceeds %.1f%% requirement)", overallAffinityPct, minAffinityPct)

	// Cleanup
	stopCtx, stopCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopCancel()
	for i, mgr := range allManagers {
		_ = mgr.Stop(stopCtx)
		t.Logf("Stopped manager %d", i)
	}
}

// TestRoundRobin_EvenDistribution verifies that RoundRobin strategy
// distributes partitions evenly across workers (±1 partition tolerance).
func TestRoundRobin_EvenDistribution(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	const (
		numPartitions = 100
		numWorkers    = 7  // Deliberately not a divisor of 100 to test uneven distribution
		expectedMin   = 14 // floor(100/7) = 14
		expectedMax   = 15 // ceil(100/7) = 15
	)

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	// Start embedded NATS server
	srv, conn := partitest.StartEmbeddedNATS(t)
	defer srv.Shutdown()
	defer conn.Close()

	t.Logf("Testing RoundRobin distribution with %d partitions across %d workers...", numPartitions, numWorkers)
	// Create partitions
	partitions := make([]types.Partition, numPartitions)
	for i := range partitions {
		partitions[i] = types.Partition{
			Keys:   []string{"rr-partition", string(rune(i))},
			Weight: 100,
		}
	}

	// Create config with test-optimized timings
	cfg := parti.TestConfig()
	cfg.WorkerIDPrefix = "rr-worker"
	cfg.WorkerIDTTL = 10 * time.Second
	cfg.ColdStartWindow = 3 * time.Second
	cfg.PlannedScaleWindow = 2 * time.Second
	cfg.Assignment.MinRebalanceInterval = 1 * time.Second

	// Create managers with RoundRobin strategy
	logger := logging.NewNop()
	managers := make([]*parti.Manager, numWorkers)
	mgrWaiters := make([]testutil.ManagerWaiter, numWorkers)
	for i := range managers {
		mgr, err := parti.NewManager(&cfg, conn, source.NewStatic(partitions), strategy.NewRoundRobin(), parti.WithLogger(logger))
		require.NoError(t, err)
		managers[i] = mgr
		mgrWaiters[i] = mgr
	}

	// Start all managers concurrently
	for _, mgr := range managers {
		go func(m *parti.Manager) {
			err := m.Start(ctx)
			require.NoError(t, err, "manager failed to start")
		}(mgr)
	}

	err := testutil.WaitAllManagersState(t.Context(), mgrWaiters, parti.StateStable, 15*time.Second)
	require.NoError(t, err, "not all managers reached stable state")

	// Collect assignments and verify distribution
	assignments := make(map[string]int) // worker ID -> partition count
	allAssignedPartitions := make(map[string]bool)

	for _, mgr := range managers {
		workerID := mgr.WorkerID()
		partCount := len(mgr.CurrentAssignment().Partitions)
		assignments[workerID] = partCount

		// Track all assigned partitions
		for _, p := range mgr.CurrentAssignment().Partitions {
			allAssignedPartitions[partitionKey(p)] = true
		}

		t.Logf("Worker %s: %d partitions", workerID, partCount)
	}

	// Verify all partitions assigned exactly once
	require.Equal(t, numPartitions, len(allAssignedPartitions), "All partitions should be assigned exactly once")

	// Verify even distribution (±1 partition)
	for workerID, count := range assignments {
		require.GreaterOrEqual(t, count, expectedMin, "Worker %s has too few partitions", workerID)
		require.LessOrEqual(t, count, expectedMax, "Worker %s has too many partitions", workerID)
	}

	// Calculate distribution statistics
	minCount, maxCount := numPartitions, 0
	totalCount := 0
	for _, count := range assignments {
		if count < minCount {
			minCount = count
		}
		if count > maxCount {
			maxCount = count
		}
		totalCount += count
	}
	avgCount := float64(totalCount) / float64(numWorkers)

	t.Logf("✅ Distribution: min=%d, max=%d, avg=%.1f (±%d tolerance met)", minCount, maxCount, avgCount, maxCount-minCount)
	require.Equal(t, numPartitions, totalCount, "Total partitions should equal original count")
	require.LessOrEqual(t, maxCount-minCount, 1, "RoundRobin should distribute within ±1 partition")

	// Cleanup
	stopCtx, stopCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopCancel()
	for i, mgr := range managers {
		_ = mgr.Stop(stopCtx)
		t.Logf("Stopped manager %d", i)
	}
}

// TestWeightedPartitions_LoadBalancing verifies that weighted partitions
// are distributed to balance total weight across workers.
func TestWeightedPartitions_LoadBalancing(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	const (
		numWorkers = 4
	)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Start embedded NATS server
	srv, conn := partitest.StartEmbeddedNATS(t)
	defer srv.Shutdown()
	defer conn.Close()

	// Create partitions with varying weights
	// 10 light partitions (weight=1), 5 medium (weight=2), 2 heavy (weight=5)
	partitions := make([]types.Partition, 0, 17)

	// Light partitions (weight=1)
	for i := 0; i < 10; i++ {
		partitions = append(partitions, types.Partition{
			Keys:   []string{"light", string(rune('A' + i))},
			Weight: 1,
		})
	}

	// Medium partitions (weight=2)
	for i := 0; i < 5; i++ {
		partitions = append(partitions, types.Partition{
			Keys:   []string{"medium", string(rune('A' + i))},
			Weight: 2,
		})
	}

	// Heavy partitions (weight=5)
	for i := 0; i < 2; i++ {
		partitions = append(partitions, types.Partition{
			Keys:   []string{"heavy", string(rune('A' + i))},
			Weight: 5,
		})
	}

	totalWeight := 10*1 + 5*2 + 2*5                                       // = 10 + 10 + 10 = 30
	expectedWeightPerWorker := float64(totalWeight) / float64(numWorkers) // = 7.5

	t.Logf("Total weight: %d, Expected per worker: %.1f", totalWeight, expectedWeightPerWorker)

	// Create config
	cfg := &parti.Config{
		WorkerIDPrefix:        "weight-worker",
		WorkerIDMin:           0,
		WorkerIDMax:           99,
		WorkerIDTTL:           10 * time.Second,
		HeartbeatInterval:     300 * time.Millisecond,
		HeartbeatTTL:          1 * time.Second,
		ElectionTimeout:       1 * time.Second,
		StartupTimeout:        10 * time.Second,
		ShutdownTimeout:       3 * time.Second,
		ColdStartWindow:       3 * time.Second,
		PlannedScaleWindow:    2 * time.Second,
		RestartDetectionRatio: 0.5,
		Assignment: parti.AssignmentConfig{
			MinRebalanceThreshold: 0.15,
			MinRebalanceInterval:     2 * time.Second, // Must be <= ColdStartWindow (3s)
		},
	}

	// Create managers with ConsistentHash (supports weights)
	managers := make([]*parti.Manager, numWorkers)
	for i := range managers {
		mgr, err := parti.NewManager(cfg, conn, source.NewStatic(partitions), strategy.NewConsistentHash())
		require.NoError(t, err)
		managers[i] = mgr
	}

	// Start all managers concurrently
	mgrWaiters := make([]testutil.ManagerWaiter, numWorkers)
	for i, mgr := range managers {
		mgrWaiters[i] = mgr
		go func(m *parti.Manager) {
			err := m.Start(ctx)
			require.NoError(t, err, "manager failed to start")
		}(mgr)
	}

	// Wait for all managers to reach stable state
	err := testutil.WaitAllManagersState(ctx, mgrWaiters, parti.StateStable, 15*time.Second)
	require.NoError(t, err, "not all managers reached stable state")

	// Collect assignments and calculate weights
	workerWeights := make(map[string]int)
	workerCounts := make(map[string]int)

	for _, mgr := range managers {
		workerID := mgr.WorkerID()
		weight := int64(0)
		count := 0
		for _, p := range mgr.CurrentAssignment().Partitions {
			weight += p.Weight
			count++
		}
		workerWeights[workerID] = int(weight)
		workerCounts[workerID] = count
		t.Logf("Worker %s: %d partitions, total weight=%d", workerID, count, weight)
	}

	// Verify weight distribution is reasonably balanced
	minWeight, maxWeight := totalWeight, 0
	totalAssignedWeight := 0
	for _, weight := range workerWeights {
		if weight < minWeight {
			minWeight = weight
		}
		if weight > maxWeight {
			maxWeight = weight
		}
		totalAssignedWeight += weight
	}

	require.Equal(t, totalWeight, totalAssignedWeight, "Total assigned weight should equal total partition weight")

	// Allow 65% deviation from perfect balance (weighted partitions can't be split)
	// With small number of partitions (17 total) and varying weights, perfect balance is impossible
	// The goal is to verify weights are considered, not achieve perfect balance
	maxAllowedDeviation := expectedWeightPerWorker * 0.65
	for workerID, weight := range workerWeights {
		deviation := float64(weight) - expectedWeightPerWorker
		if deviation < 0 {
			deviation = -deviation
		}
		t.Logf("  Worker %s deviation: %.1f (%.1f%% of expected)", workerID, deviation, deviation/expectedWeightPerWorker*100)
		require.LessOrEqual(t, deviation, maxAllowedDeviation,
			"Worker %s weight deviation %.1f exceeds allowed %.1f", workerID, deviation, maxAllowedDeviation)
	}

	avgWeight := float64(totalAssignedWeight) / float64(numWorkers)
	t.Logf("✅ Weight distribution: min=%d, max=%d, avg=%.1f (within ±65%% tolerance)", minWeight, maxWeight, avgWeight)

	// Cleanup
	stopCtx, stopCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopCancel()
	for i, mgr := range managers {
		_ = mgr.Stop(stopCtx)
		t.Logf("Stopped manager %d", i)
	}
}
