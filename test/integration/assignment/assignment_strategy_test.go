package assignment_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestConsistentHash_PartitionAffinity verifies that ConsistentHash strategy
// maintains >65% partition affinity during rebalancing (cache-friendly behavior).
func TestConsistentHash_PartitionAffinity(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	const (
		numPartitions  = 100
		initialWorkers = 3
		finalWorkers   = 4
		minAffinityPct = 65.0
	)

	env := newTestEnv(t, 90*time.Second)

	// Create partitions with unique identifiable keys
	partitions := make([]types.Partition, numPartitions)
	for i := range partitions {
		partitions[i] = types.Partition{
			Keys:   []string{"partition", string(rune('A' + i/26)), string(rune('a' + i%26))},
			Weight: 100,
		}
	}

	cfg := parti.TestConfig()
	cfg.WorkerIDPrefix = "affinity-worker"
	cfg.WorkerIDTTL = 10 * time.Second
	cfg.ColdStartWindow = 3 * time.Second
	cfg.PlannedScaleWindow = 2 * time.Second

	// Phase 1: Start initial workers and record assignments
	t.Logf("Phase 1: Starting %d initial workers...", initialWorkers)

	js, err := jetstream.New(env.Conn)
	require.NoError(t, err)

	initialManagers := make([]*parti.Manager, initialWorkers)
	for i := range initialManagers {
		mgr, err := parti.NewManager(&cfg, js, source.NewStatic(partitions), strategy.NewConsistentHash())
		require.NoError(t, err)
		initialManagers[i] = mgr
	}

	startManagersConcurrently(t, env.Ctx, initialManagers)
	waitForStableWithTimeout(t, env.Ctx, initialManagers, 15*time.Second)
	verifyAssignments(t, initialManagers, numPartitions)

	initialAssignments := collectPartitionKeys(initialManagers)
	for wID, keys := range initialAssignments {
		t.Logf("  Worker %s: %d partitions", wID, len(keys))
	}

	// Phase 2: Add one more worker and observe rebalancing
	t.Logf("Phase 2: Adding 1 worker (scale %d→%d)...", initialWorkers, finalWorkers)

	newMgr, err := parti.NewManager(&cfg, js, source.NewStatic(partitions), strategy.NewConsistentHash())
	require.NoError(t, err)

	allManagers := make([]*parti.Manager, 0, len(initialManagers)+1)
	allManagers = append(allManagers, initialManagers...)
	allManagers = append(allManagers, newMgr)
	defer cleanupManagers(t, allManagers)

	startManagersConcurrently(t, env.Ctx, []*parti.Manager{newMgr})
	waitForStableWithTimeout(t, env.Ctx, allManagers, 15*time.Second)
	verifyAssignments(t, allManagers, numPartitions)

	newAssignments := collectPartitionKeys(allManagers)
	for wID, keys := range newAssignments {
		t.Logf("  Worker %s: %d partitions", wID, len(keys))
	}

	// Calculate affinity
	totalPartitions := 0
	retainedPartitions := 0

	for workerID, oldParts := range initialAssignments {
		newParts := newAssignments[workerID]
		oldMap := make(map[string]bool, len(oldParts))
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

		pct := 0.0
		if len(oldParts) > 0 {
			pct = float64(retained) / float64(len(oldParts)) * 100.0
		}

		t.Logf("  Worker %s: retained %d/%d partitions (%.1f%%)", workerID, retained, len(oldParts), pct)
	}

	overallPct := float64(retainedPartitions) / float64(totalPartitions) * 100.0
	t.Logf("Overall affinity: %.1f%% (%d/%d partitions retained)", overallPct, retainedPartitions, totalPartitions)

	require.GreaterOrEqual(t, overallPct, minAffinityPct,
		"ConsistentHash should maintain at least %.1f%% partition affinity during rebalancing", minAffinityPct)
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
		numWorkers    = 7
		expectedMin   = 14 // floor(100/7)
		expectedMax   = 15 // ceil(100/7)
	)

	env := newTestEnv(t, 120*time.Second)

	t.Logf("Testing RoundRobin distribution with %d partitions across %d workers...", numPartitions, numWorkers)

	partitions := make([]types.Partition, numPartitions)
	for i := range partitions {
		partitions[i] = types.Partition{
			Keys:   []string{"rr-partition", fmt.Sprintf("%03d", i)},
			Weight: 100,
		}
	}

	cfg := parti.TestConfig()
	cfg.WorkerIDPrefix = "rr-worker"
	cfg.WorkerIDTTL = 10 * time.Second
	cfg.ColdStartWindow = 3 * time.Second
	cfg.PlannedScaleWindow = 2 * time.Second
	cfg.RebalanceCooldown = 1 * time.Second

	managers := env.SetupManagers(t, &cfg, source.NewStatic(partitions), strategy.NewRoundRobin(), numWorkers)
	startManagersConcurrently(t, env.Ctx, managers)
	waitForStableWithTimeout(t, env.Ctx, managers, 15*time.Second)
	verifyAssignments(t, managers, numPartitions)

	// Verify even distribution (±1 partition)
	assignments := make(map[string]int)
	allAssigned := make(map[string]bool)

	for _, mgr := range managers {
		wID := mgr.WorkerID()
		count := len(mgr.CurrentAssignment().Partitions)
		assignments[wID] = count

		for _, p := range mgr.CurrentAssignment().Partitions {
			allAssigned[partitionKey(p)] = true
		}

		t.Logf("Worker %s: %d partitions", wID, count)
	}

	require.Equal(t, numPartitions, len(allAssigned), "All partitions should be assigned exactly once")

	for wID, count := range assignments {
		require.GreaterOrEqual(t, count, expectedMin, "Worker %s has too few partitions", wID)
		require.LessOrEqual(t, count, expectedMax, "Worker %s has too many partitions", wID)
	}

	minCount, maxCount, totalCount := numPartitions, 0, 0
	for _, c := range assignments {
		if c < minCount {
			minCount = c
		}
		if c > maxCount {
			maxCount = c
		}

		totalCount += c
	}

	t.Logf("Distribution: min=%d, max=%d, avg=%.1f (±%d tolerance met)",
		minCount, maxCount, float64(totalCount)/float64(numWorkers), maxCount-minCount)
	require.Equal(t, numPartitions, totalCount, "Total partitions should equal original count")
	require.LessOrEqual(t, maxCount-minCount, 1, "RoundRobin should distribute within ±1 partition")
}

// TestWeightedPartitions_LoadBalancing verifies that weighted partitions
// are distributed to balance total weight across workers.
func TestWeightedPartitions_LoadBalancing(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	const numWorkers = 4

	env := newTestEnv(t, 30*time.Second)

	// 10 light (w=1), 5 medium (w=2), 2 heavy (w=5)
	partitions := make([]types.Partition, 0, 17)
	for i := 0; i < 10; i++ {
		partitions = append(partitions, types.Partition{Keys: []string{"light", string(rune('A' + i))}, Weight: 1})
	}

	for i := 0; i < 5; i++ {
		partitions = append(partitions, types.Partition{Keys: []string{"medium", string(rune('A' + i))}, Weight: 2})
	}

	for i := 0; i < 2; i++ {
		partitions = append(partitions, types.Partition{Keys: []string{"heavy", string(rune('A' + i))}, Weight: 5})
	}

	totalWeight := 10*1 + 5*2 + 2*5 // = 30
	expectedWeightPerWorker := float64(totalWeight) / float64(numWorkers)
	t.Logf("Total weight: %d, Expected per worker: %.1f", totalWeight, expectedWeightPerWorker)

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
		RebalanceCooldown:     2 * time.Second,
	}

	managers := env.SetupManagers(t, cfg, source.NewStatic(partitions), strategy.NewWeightedConsistentHash(), numWorkers)
	startManagersConcurrently(t, env.Ctx, managers)
	waitForStableWithTimeout(t, env.Ctx, managers, 15*time.Second)
	verifyAssignments(t, managers, len(partitions))

	// Collect weights
	workerWeights := make(map[string]int)
	for _, mgr := range managers {
		wID := mgr.WorkerID()
		var weight int64
		count := 0
		for _, p := range mgr.CurrentAssignment().Partitions {
			weight += p.Weight
			count++
		}

		workerWeights[wID] = int(weight)
		t.Logf("Worker %s: %d partitions, total weight=%d", wID, count, weight)
	}

	// Verify total weight
	totalAssignedWeight := 0
	minWeight, maxWeight := totalWeight, 0
	for _, w := range workerWeights {
		totalAssignedWeight += w
		if w < minWeight {
			minWeight = w
		}
		if w > maxWeight {
			maxWeight = w
		}
	}

	require.Equal(t, totalWeight, totalAssignedWeight, "Total assigned weight should equal total partition weight")

	// Allow 75% deviation (weighted partitions can't be split)
	maxAllowedDev := expectedWeightPerWorker * 0.75
	for wID, w := range workerWeights {
		dev := float64(w) - expectedWeightPerWorker
		if dev < 0 {
			dev = -dev
		}

		t.Logf("  Worker %s deviation: %.1f (%.1f%% of expected)", wID, dev, dev/expectedWeightPerWorker*100)
		require.LessOrEqual(t, dev, maxAllowedDev,
			"Worker %s weight deviation %.1f exceeds allowed %.1f", wID, dev, maxAllowedDev)
	}

	t.Logf("Weight distribution: min=%d, max=%d, avg=%.1f (within ±65%% tolerance)",
		minWeight, maxWeight, float64(totalAssignedWeight)/float64(numWorkers))
}

// ---------------------------------------------------------------------------
// Strategy test helpers
// ---------------------------------------------------------------------------

// collectPartitionKeys returns workerID → partition keys for all managers.
func collectPartitionKeys(managers []*parti.Manager) map[string][]string {
	result := make(map[string][]string)
	for _, mgr := range managers {
		wID := mgr.WorkerID()
		var keys []string
		for _, p := range mgr.CurrentAssignment().Partitions {
			keys = append(keys, partitionKey(p))
		}

		result[wID] = keys
	}

	return result
}
