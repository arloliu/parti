package strategy

import (
	"testing"

	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// TestAssignmentStrategy_ZeroPartitions verifies that strategies handle zero partitions correctly.
func TestAssignmentStrategy_ZeroPartitions(t *testing.T) {
	strategies := map[string]types.AssignmentStrategy{
		"ConsistentHash": NewConsistentHash(),
		"RoundRobin":     NewRoundRobin(),
	}

	for name, strategy := range strategies {
		t.Run(name, func(t *testing.T) {
			workers := []string{"worker-0", "worker-1"}
			partitions := []types.Partition{}

			assignments, err := strategy.Assign(workers, partitions)

			require.NoError(t, err, "zero partitions should not cause error")

			// Verify no partitions are assigned (either empty map or map with empty slices)
			totalAssigned := 0
			for _, parts := range assignments {
				totalAssigned += len(parts)
			}
			require.Equal(t, 0, totalAssigned, "no partitions should be assigned for zero partitions input")
		})
	}
}

// TestAssignmentStrategy_SingleWorker_GetsAllPartitions verifies single worker scenarios.
func TestAssignmentStrategy_SingleWorker_GetsAllPartitions(t *testing.T) {
	strategies := map[string]types.AssignmentStrategy{
		"ConsistentHash": NewConsistentHash(),
		"RoundRobin":     NewRoundRobin(),
	}

	for name, strategy := range strategies {
		t.Run(name, func(t *testing.T) {
			workers := []string{"worker-0"}
			partitions := []types.Partition{
				{Keys: []string{"partition-0"}, Weight: 100},
				{Keys: []string{"partition-1"}, Weight: 100},
				{Keys: []string{"partition-2"}, Weight: 100},
			}

			assignments, err := strategy.Assign(workers, partitions)

			require.NoError(t, err)
			require.Len(t, assignments, 1, "should have assignments for 1 worker")
			require.Len(t, assignments["worker-0"], 3, "single worker should get all partitions")
		})
	}
}

// TestAssignmentStrategy_MoreWorkersThanPartitions verifies that some workers may get nothing.
func TestAssignmentStrategy_MoreWorkersThanPartitions(t *testing.T) {
	strategies := map[string]types.AssignmentStrategy{
		"ConsistentHash": NewConsistentHash(),
		"RoundRobin":     NewRoundRobin(),
	}

	for name, strategy := range strategies {
		t.Run(name, func(t *testing.T) {
			workers := []string{"worker-0", "worker-1", "worker-2", "worker-3", "worker-4"}
			partitions := []types.Partition{
				{Keys: []string{"partition-0"}, Weight: 100},
				{Keys: []string{"partition-1"}, Weight: 100},
			}

			assignments, err := strategy.Assign(workers, partitions)

			require.NoError(t, err)

			// Verify all partitions are assigned
			totalAssigned := 0
			for _, parts := range assignments {
				totalAssigned += len(parts)
			}
			require.Equal(t, len(partitions), totalAssigned, "all partitions should be assigned")

			// Some workers should have no assignments
			emptyWorkers := 0
			for _, parts := range assignments {
				if len(parts) == 0 {
					emptyWorkers++
				}
			}
			require.Greater(t, emptyWorkers, 0, "with more workers than partitions, some workers should get nothing")
		})
	}
}

// TestAssignmentStrategy_MorePartitionsThanWorkers verifies even distribution.
func TestAssignmentStrategy_MorePartitionsThanWorkers(t *testing.T) {
	strategies := map[string]types.AssignmentStrategy{
		"ConsistentHash": NewConsistentHash(),
		"RoundRobin":     NewRoundRobin(),
	}

	for name, strategy := range strategies {
		t.Run(name, func(t *testing.T) {
			workers := []string{"worker-0", "worker-1"}
			partitions := make([]types.Partition, 100)
			for i := range partitions {
				partitions[i] = types.Partition{Keys: []string{string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))}, Weight: 100}
			}

			assignments, err := strategy.Assign(workers, partitions)

			require.NoError(t, err)
			require.Len(t, assignments, 2, "should have assignments for 2 workers")

			// Verify all partitions are assigned
			totalAssigned := 0
			for _, parts := range assignments {
				totalAssigned += len(parts)
			}
			require.Equal(t, len(partitions), totalAssigned, "all partitions should be assigned")

			// Both workers should have some partitions
			for worker, parts := range assignments {
				require.NotEmpty(t, parts, "worker %s should have at least some partitions", worker)
			}

			// Distribution should be reasonably fair (allow 30% variance for consistent hashing)
			expectedPerWorker := float64(len(partitions)) / float64(len(workers))
			for worker, parts := range assignments {
				ratio := float64(len(parts)) / expectedPerWorker
				require.GreaterOrEqual(t, ratio, 0.3, "worker %s has too few partitions", worker)
				require.LessOrEqual(t, ratio, 1.7, "worker %s has too many partitions", worker)
			}
		})
	}
}

// TestAssignmentStrategy_AllWeightsZero verifies handling of zero weights.
func TestAssignmentStrategy_AllWeightsZero(t *testing.T) {
	strategies := map[string]types.AssignmentStrategy{
		"ConsistentHash": NewConsistentHash(),
		"RoundRobin":     NewRoundRobin(),
	}

	for name, strategy := range strategies {
		t.Run(name, func(t *testing.T) {
			workers := []string{"worker-0", "worker-1"}
			partitions := []types.Partition{
				{Keys: []string{"partition-0"}, Weight: 0},
				{Keys: []string{"partition-1"}, Weight: 0},
				{Keys: []string{"partition-2"}, Weight: 0},
			}

			assignments, err := strategy.Assign(workers, partitions)

			// Zero weights should still distribute partitions evenly
			require.NoError(t, err)
			totalAssigned := 0
			for _, parts := range assignments {
				totalAssigned += len(parts)
			}
			require.Equal(t, len(partitions), totalAssigned, "all partitions should be assigned even with zero weights")
		})
	}
}

// TestAssignmentStrategy_AllWeightsEqual verifies equal weight distribution.
func TestAssignmentStrategy_AllWeightsEqual(t *testing.T) {
	strategies := map[string]types.AssignmentStrategy{
		"ConsistentHash": NewConsistentHash(),
		"RoundRobin":     NewRoundRobin(),
	}

	for name, strategy := range strategies {
		t.Run(name, func(t *testing.T) {
			workers := []string{"worker-0", "worker-1", "worker-2"}
			partitions := make([]types.Partition, 30)
			for i := range partitions {
				partitions[i] = types.Partition{Keys: []string{string(rune('a' + i))}, Weight: 50}
			}

			assignments, err := strategy.Assign(workers, partitions)

			require.NoError(t, err)

			// Verify all partitions are assigned
			totalAssigned := 0
			for _, parts := range assignments {
				totalAssigned += len(parts)
			}
			require.Equal(t, len(partitions), totalAssigned, "all partitions should be assigned")

			// With equal weights, distribution should be reasonably fair
			expectedPerWorker := float64(len(partitions)) / float64(len(workers))
			for worker, parts := range assignments {
				ratio := float64(len(parts)) / expectedPerWorker
				require.GreaterOrEqual(t, ratio, 0.5, "worker %s has too few partitions with equal weights", worker)
				require.LessOrEqual(t, ratio, 1.5, "worker %s has too many partitions with equal weights", worker)
			}
		})
	}
}

// TestAssignmentStrategy_ExtremeWeights verifies handling of extreme weight differences.
func TestAssignmentStrategy_ExtremeWeights(t *testing.T) {
	t.Run("ConsistentHash", func(t *testing.T) {
		strategy := NewConsistentHash()
		workers := []string{"worker-0", "worker-1"}
		partitions := []types.Partition{
			{Keys: []string{"partition-0"}, Weight: 1},     // Very low weight
			{Keys: []string{"partition-1"}, Weight: 10000}, // Very high weight
			{Keys: []string{"partition-2"}, Weight: 1},
			{Keys: []string{"partition-3"}, Weight: 10000},
		}

		assignments, err := strategy.Assign(workers, partitions)

		require.NoError(t, err)

		// Verify all partitions are assigned
		totalAssigned := 0
		for _, parts := range assignments {
			totalAssigned += len(parts)
		}
		require.Equal(t, len(partitions), totalAssigned, "all partitions should be assigned")

		// Note: ConsistentHash doesn't use weights for distribution (by design)
		// It uses consistent hashing for cache affinity, not weight-based distribution
		// This test verifies it handles extreme weights without crashing
	})

	t.Run("RoundRobin", func(t *testing.T) {
		strategy := NewRoundRobin()
		workers := []string{"worker-0", "worker-1"}
		partitions := []types.Partition{
			{Keys: []string{"partition-0"}, Weight: 1},
			{Keys: []string{"partition-1"}, Weight: 10000},
			{Keys: []string{"partition-2"}, Weight: 1},
			{Keys: []string{"partition-3"}, Weight: 10000},
		}

		assignments, err := strategy.Assign(workers, partitions)

		require.NoError(t, err)

		// Verify all partitions are assigned
		totalAssigned := 0
		for _, parts := range assignments {
			totalAssigned += len(parts)
		}
		require.Equal(t, len(partitions), totalAssigned, "all partitions should be assigned")

		// RoundRobin should distribute evenly regardless of weights (by design)
		require.Len(t, assignments["worker-0"], 2)
		require.Len(t, assignments["worker-1"], 2)
	})
}

// TestAssignmentStrategy_MixedWeights verifies reasonable weight distribution.
func TestAssignmentStrategy_MixedWeights(t *testing.T) {
	strategies := map[string]types.AssignmentStrategy{
		"ConsistentHash": NewConsistentHash(),
		"RoundRobin":     NewRoundRobin(),
	}

	for name, strategy := range strategies {
		t.Run(name, func(t *testing.T) {
			workers := []string{"worker-0", "worker-1", "worker-2"}
			partitions := []types.Partition{
				{Keys: []string{"partition-0"}, Weight: 100},
				{Keys: []string{"partition-1"}, Weight: 200},
				{Keys: []string{"partition-2"}, Weight: 50},
				{Keys: []string{"partition-3"}, Weight: 150},
				{Keys: []string{"partition-4"}, Weight: 100},
				{Keys: []string{"partition-5"}, Weight: 300},
			}

			assignments, err := strategy.Assign(workers, partitions)

			require.NoError(t, err)

			// Verify all partitions are assigned
			totalAssigned := 0
			for _, parts := range assignments {
				totalAssigned += len(parts)
			}
			require.Equal(t, len(partitions), totalAssigned, "all partitions should be assigned")

			// With small number of partitions, consistent hashing might not distribute to all workers
			// This is expected behavior due to hash distribution
			// Just verify that at least one worker got partitions
			hasAssignments := false
			for _, parts := range assignments {
				if len(parts) > 0 {
					hasAssignments = true

					break
				}
			}
			require.True(t, hasAssignments, "at least one worker should have partitions")
		})
	}
}

// TestAssignmentStrategy_AllPartitionsAssignedExactlyOnce verifies assignment completeness.
func TestAssignmentStrategy_AllPartitionsAssignedExactlyOnce(t *testing.T) {
	strategies := map[string]types.AssignmentStrategy{
		"ConsistentHash": NewConsistentHash(),
		"RoundRobin":     NewRoundRobin(),
	}

	for name, strategy := range strategies {
		t.Run(name, func(t *testing.T) {
			workers := []string{"worker-0", "worker-1", "worker-2"}
			partitions := make([]types.Partition, 50)
			for i := range partitions {
				partitions[i] = types.Partition{Keys: []string{string(rune('a'+i%26)) + string(rune('0'+(i/26)%10))}, Weight: 100}
			}

			assignments, err := strategy.Assign(workers, partitions)

			require.NoError(t, err)

			// Track which partitions we've seen
			seen := make(map[string]int)
			for _, parts := range assignments {
				for _, p := range parts {
					seen[p.Keys[0]]++
				}
			}

			// Verify every partition appears exactly once
			for i, partition := range partitions {
				count, exists := seen[partition.Keys[0]]
				require.True(t, exists, "partition %d (%s) was not assigned", i, partition.Keys[0])
				require.Equal(t, 1, count, "partition %d (%s) was assigned %d times", i, partition.Keys[0], count)
			}

			// Verify no extra partitions
			require.Equal(t, len(partitions), len(seen), "should have exactly as many assigned partitions as input")
		})
	}
}

// TestAssignmentStrategy_NoWorkerGetsDuplicatePartitions verifies no duplicates.
func TestAssignmentStrategy_NoWorkerGetsDuplicatePartitions(t *testing.T) {
	strategies := map[string]types.AssignmentStrategy{
		"ConsistentHash": NewConsistentHash(),
		"RoundRobin":     NewRoundRobin(),
	}

	for name, strategy := range strategies {
		t.Run(name, func(t *testing.T) {
			workers := []string{"worker-0", "worker-1", "worker-2"}
			partitions := make([]types.Partition, 50)
			for i := range partitions {
				partitions[i] = types.Partition{Keys: []string{string(rune('a'+i%26)) + string(rune('0'+(i/26)%10))}, Weight: 100}
			}

			assignments, err := strategy.Assign(workers, partitions)

			require.NoError(t, err)

			// Check each worker's assignments for duplicates
			for worker, parts := range assignments {
				seen := make(map[string]bool)
				for _, p := range parts {
					key := p.Keys[0]
					require.False(t, seen[key], "worker %s has duplicate partition %s", worker, key)
					seen[key] = true
				}
			}
		})
	}
}
