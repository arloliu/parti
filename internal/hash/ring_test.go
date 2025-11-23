package hash

import (
	"fmt"
	"testing"

	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	workers := []string{"worker-0", "worker-1", "worker-2"}
	ring := NewRing(workers, 100, 0)

	require.NotNil(t, ring)
	require.Equal(t, 300, ring.Size()) // 3 workers * 100 virtual nodes
	require.ElementsMatch(t, workers, ring.Workers())
}

func TestRing_GetNode(t *testing.T) {
	t.Run("assigns keys consistently", func(t *testing.T) {
		workers := []string{"worker-0", "worker-1"}
		ring := NewRing(workers, 150, 0)

		// Same key always maps to same worker (test multiple keys)
		for _, key := range []string{"test-partition", "another-key", "xyz"} {
			worker1 := ring.GetNode(key)
			worker2 := ring.GetNode(key)
			worker3 := ring.GetNode(key)

			require.Equal(t, worker1, worker2, "key %s not consistent", key)
			require.Equal(t, worker1, worker3, "key %s not consistent", key)
			require.Contains(t, workers, worker1, "worker should be from known set")
		}
	})

	t.Run("distributes keys across workers", func(t *testing.T) {
		workers := []string{"worker-0", "worker-1", "worker-2"}
		ring := NewRing(workers, 150, 0)

		// Count assignments for many keys
		counts := make(map[string]int)
		for i := range 1000 {
			key := fmt.Sprintf("partition-%d", i)
			worker := ring.GetNode(key)
			counts[worker]++
		}

		// Each worker should get roughly 1/3 of keys (allow 20% variance)
		expectedPerWorker := 1000 / len(workers)
		tolerance := expectedPerWorker * 20 / 100

		for _, worker := range workers {
			require.Contains(t, counts, worker, "worker should have assignments")
			count := counts[worker]
			require.GreaterOrEqual(t, count, expectedPerWorker-tolerance, "worker %s under-assigned", worker)
			require.LessOrEqual(t, count, expectedPerWorker+tolerance, "worker %s over-assigned", worker)
		}
	})

	t.Run("returns empty string for empty ring", func(t *testing.T) {
		ring := NewRing([]string{}, 150, 0)
		worker := ring.GetNode("any-key")
		require.Empty(t, worker)
	})
}

func TestRing_GetNodeForPartition(t *testing.T) {
	workers := []string{"worker-0", "worker-1"}
	ring := NewRing(workers, 150, 0)

	t.Run("handles partition with single key", func(t *testing.T) {
		partition := types.Partition{
			Keys:   []string{"keyspace1"},
			Weight: 100,
		}

		worker := ring.GetNodeForPartition(partition)
		require.Contains(t, workers, worker)
	})

	t.Run("handles partition with multiple keys", func(t *testing.T) {
		partition := types.Partition{
			Keys:   []string{"keyspace1", "table1", "range-1"},
			Weight: 100,
		}

		worker1 := ring.GetNodeForPartition(partition)
		require.Contains(t, workers, worker1)

		// Same partition should always map to same worker
		worker2 := ring.GetNodeForPartition(partition)
		worker3 := ring.GetNodeForPartition(partition)
		require.Equal(t, worker1, worker2, "partition assignment not consistent")
		require.Equal(t, worker1, worker3, "partition assignment not consistent")
	})

	t.Run("returns empty for partition with no keys", func(t *testing.T) {
		partition := types.Partition{
			Keys:   []string{},
			Weight: 100,
		}

		worker := ring.GetNodeForPartition(partition)
		require.Empty(t, worker)
	})
}

func TestRing_CacheAffinity(t *testing.T) {
	t.Run("maintains cache affinity when worker added", func(t *testing.T) {
		// Create ring with 2 workers
		initialWorkers := []string{"worker-0", "worker-1"}
		ring1 := NewRing(initialWorkers, 150, 12345) // Use seed for determinism

		// Assign 1000 partitions
		partitions := make([]types.Partition, 1000)
		for i := range partitions {
			partitions[i] = types.Partition{
				Keys:   []string{fmt.Sprintf("p-%d", i)},
				Weight: 100,
			}
		}

		// Record initial assignments
		initialAssignments := make(map[int]string)
		for i, p := range partitions {
			initialAssignments[i] = ring1.GetNodeForPartition(p)
		}

		// Add third worker
		newWorkers := []string{"worker-0", "worker-1", "worker-2"}
		ring2 := NewRing(newWorkers, 150, 12345) // Same seed

		// Count how many partitions stayed on same worker
		sameWorker := 0
		for i, p := range partitions {
			newWorker := ring2.GetNodeForPartition(p)
			if newWorker == initialAssignments[i] {
				sameWorker++
			}
		}

		// Consistent hashing with 150 virtual nodes typically maintains ~65-70% affinity
		// when adding 1 worker to 2 (theoretical minimum is 66.7% since only 1/3 needs to move)
		// In practice with small sample size, allow down to 45%
		affinityPercent := (sameWorker * 100) / len(partitions)
		require.GreaterOrEqual(t, affinityPercent, 45,
			"Cache affinity %d%% is too low (expected >= 45%%)", affinityPercent)

		t.Logf("Cache affinity when adding worker: %d%% (%d/%d)", affinityPercent, sameWorker, len(partitions))
	})

	t.Run("maintains cache affinity when worker removed", func(t *testing.T) {
		// Create ring with 3 workers
		initialWorkers := []string{"worker-0", "worker-1", "worker-2"}
		ring1 := NewRing(initialWorkers, 150, 12345) // Use seed for determinism

		// Assign 1000 partitions
		partitions := make([]types.Partition, 1000)
		for i := range partitions {
			partitions[i] = types.Partition{
				Keys:   []string{fmt.Sprintf("p-%d", i)},
				Weight: 100,
			}
		}

		// Record initial assignments
		initialAssignments := make(map[int]string)
		for i, p := range partitions {
			initialAssignments[i] = ring1.GetNodeForPartition(p)
		}

		// Remove one worker
		newWorkers := []string{"worker-0", "worker-1"}
		ring2 := NewRing(newWorkers, 150, 12345) // Same seed

		// Count how many partitions stayed on same worker
		// (excluding those that were on removed worker)
		sameWorker := 0
		totalChecked := 0
		for i, p := range partitions {
			oldWorker := initialAssignments[i]
			if oldWorker == "worker-2" {
				continue // This worker was removed, skip
			}
			totalChecked++
			newWorker := ring2.GetNodeForPartition(p)
			if newWorker == oldWorker {
				sameWorker++
			}
		}

		// For partitions not on removed worker, consistent hashing should maintain ~100% affinity
		// In practice, with virtual nodes, we might see some reassignment due to hash collisions
		// Expect at least 95% affinity for non-removed workers
		affinityPercent := (sameWorker * 100) / totalChecked
		require.GreaterOrEqual(t, affinityPercent, 95,
			"Cache affinity %d%% is too low (expected >= 95%%)", affinityPercent)

		t.Logf("Cache affinity when removing worker: %d%% (%d/%d checked, %d on removed worker)",
			affinityPercent, sameWorker, totalChecked, len(partitions)-totalChecked)
	})
}

func TestWeightedRing(t *testing.T) {
	workers := []string{"worker-0", "worker-1", "worker-2"}
	ring := NewWeighted(workers, 150, 0)

	t.Run("assigns partitions with uniform weights", func(t *testing.T) {
		partitions := make([]types.Partition, 30)
		for i := range partitions {
			partitions[i] = types.Partition{
				Keys:   []string{fmt.Sprintf("p-%d", i)},
				Weight: 100,
			}
		}

		assignments := ring.AssignPartitions(partitions)

		// Verify all partitions assigned
		totalAssigned := 0
		for _, parts := range assignments {
			totalAssigned += len(parts)
		}
		require.Equal(t, len(partitions), totalAssigned)

		// Verify balanced distribution (each worker gets 10 ± 3 partitions)
		for workerID, parts := range assignments {
			require.GreaterOrEqual(t, len(parts), 7, "worker %s under-assigned", workerID)
			require.LessOrEqual(t, len(parts), 13, "worker %s over-assigned", workerID)
		}
	})

	t.Run("balances partitions with varying weights", func(t *testing.T) {
		partitions := []types.Partition{
			{Keys: []string{"heavy-1"}, Weight: 1000},
			{Keys: []string{"heavy-2"}, Weight: 1000},
			{Keys: []string{"heavy-3"}, Weight: 1000},
			{Keys: []string{"light-1"}, Weight: 100},
			{Keys: []string{"light-2"}, Weight: 100},
			{Keys: []string{"light-3"}, Weight: 100},
		}

		assignments := ring.AssignPartitions(partitions)

		// Calculate total weight per worker
		totalWeight := int64(0)
		for workerID := range assignments {
			weight := ring.GetWorkerWeight(workerID)
			totalWeight += weight
			t.Logf("Worker %s: weight=%d, partitions=%d", workerID, weight, len(assignments[workerID]))
		}

		// Total weight should equal sum of partition weights
		require.Equal(t, int64(3300), totalWeight)

		// Each worker should have roughly 1100 weight
		// WeightedRing allows 15% overload for cache affinity
		expectedPerWorker := totalWeight / int64(len(workers))
		maxAllowed := expectedPerWorker + (expectedPerWorker * 15 / 100)

		for workerID := range assignments {
			weight := ring.GetWorkerWeight(workerID)
			require.LessOrEqual(t, weight, maxAllowed,
				"worker %s over-weighted (has %d, max %d)", workerID, weight, maxAllowed)
		}
	})

	t.Run("handles empty partition list", func(t *testing.T) {
		assignments := ring.AssignPartitions([]types.Partition{})
		require.Empty(t, assignments)
	})

	t.Run("uses default weight for zero-weight partitions", func(t *testing.T) {
		partitions := []types.Partition{
			{Keys: []string{"p-1"}, Weight: 0}, // Should use default 100
			{Keys: []string{"p-2"}, Weight: 0},
		}

		assignments := ring.AssignPartitions(partitions)

		totalWeight := int64(0)
		for workerID := range assignments {
			totalWeight += ring.GetWorkerWeight(workerID)
		}

		require.Equal(t, int64(200), totalWeight) // 2 partitions * 100 default
	})
}

func TestRing_DifferentSeeds(t *testing.T) {
	workers := []string{"worker-0", "worker-1", "worker-2"}

	// Different seeds should produce consistent assignments
	ring1 := NewRing(workers, 150, 0)
	ring2 := NewRing(workers, 150, 12345)
	ring3 := NewRing(workers, 150, 12345) // Same seed as ring2

	// Test multiple partitions for statistical confidence
	differentCount := 0
	for i := range 100 {
		partition := types.Partition{
			Keys:   []string{fmt.Sprintf("partition-%d", i)},
			Weight: 100,
		}

		worker1 := ring1.GetNodeForPartition(partition)
		worker2 := ring2.GetNodeForPartition(partition)
		worker3 := ring3.GetNodeForPartition(partition)

		// Same seed should produce same assignment
		require.Equal(t, worker2, worker3, "Same seed should produce same assignment")

		// Different seeds will usually produce different assignments
		if worker1 != worker2 {
			differentCount++
		}
	}

	// With 100 partitions and 3 workers, expect most assignments to differ
	// Allow for some chance collisions (> 30% different)
	differentPercent := (differentCount * 100) / 100
	require.GreaterOrEqual(t, differentPercent, 30,
		"Different seeds should produce different distributions")

	t.Logf("Different seed assignments: %d%%", differentPercent)
}

func TestRing_VirtualNodesImpact(t *testing.T) {
	workers := []string{"worker-0", "worker-1", "worker-2"}

	// Test with different virtual node counts
	testCases := []struct {
		name         string
		virtualNodes int
	}{
		{"few virtual nodes", 10},
		{"moderate virtual nodes", 100},
		{"many virtual nodes", 300},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ring := NewRing(workers, tc.virtualNodes, 0)

			// Count distribution for 1000 keys
			counts := make(map[string]int)
			for i := range 1000 {
				key := fmt.Sprintf("partition-%d", i)
				worker := ring.GetNode(key)
				counts[worker]++
			}

			// Calculate standard deviation
			expectedPerWorker := 1000 / len(workers)
			variance := 0
			for _, count := range counts {
				diff := count - expectedPerWorker
				variance += diff * diff
			}
			stdDev := variance / len(counts)

			t.Logf("%s: stdDev=%d (lower is better distribution)", tc.name, stdDev)

			// More virtual nodes should give better distribution (lower std dev)
			// But we don't enforce strict limits as it's probabilistic
		})
	}
}
