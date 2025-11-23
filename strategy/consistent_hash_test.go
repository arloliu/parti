package strategy

import (
	"testing"

	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

func TestConsistentHash_Assign(t *testing.T) {
	t.Run("assigns partitions to single worker", func(t *testing.T) {
		strategy := NewConsistentHash()
		workers := []string{"worker-0"}
		partitions := []types.Partition{
			{Keys: []string{"partition-0"}, Weight: 100},
			{Keys: []string{"partition-1"}, Weight: 100},
		}

		assignments, err := strategy.Assign(workers, partitions)

		require.NoError(t, err)
		require.Len(t, assignments, 1)
		require.Len(t, assignments["worker-0"], 2)
	})

	t.Run("distributes partitions across multiple workers", func(t *testing.T) {
		strategy := NewConsistentHash()
		workers := []string{"worker-0", "worker-1", "worker-2"}
		partitions := make([]types.Partition, 30)
		for i := range partitions {
			partitions[i] = types.Partition{Keys: []string{string(rune('a' + i))}, Weight: 100}
		}

		assignments, err := strategy.Assign(workers, partitions)

		require.NoError(t, err)
		require.Len(t, assignments, 3)

		// Verify each worker gets at least some partitions
		// With consistent hashing, distribution won't be perfectly even
		for worker, assigned := range assignments {
			require.NotEmpty(t, assigned, "worker %s should have at least some partitions", worker)
		}

		// Verify all partitions are assigned
		totalAssigned := 0
		for _, assigned := range assignments {
			totalAssigned += len(assigned)
		}
		require.Equal(t, len(partitions), totalAssigned, "all partitions should be assigned")
	})

	t.Run("assignment is deterministic", func(t *testing.T) {
		strategy := NewConsistentHash()
		workers := []string{"worker-0", "worker-1", "worker-2"}
		partitions := make([]types.Partition, 20)
		for i := range partitions {
			partitions[i] = types.Partition{Keys: []string{string(rune('a' + i))}, Weight: 100}
		}

		// Assign twice and verify results are identical
		assignments1, err1 := strategy.Assign(workers, partitions)
		assignments2, err2 := strategy.Assign(workers, partitions)

		require.NoError(t, err1)
		require.NoError(t, err2)
		require.Equal(t, assignments1, assignments2, "assignments should be deterministic")
	})

	t.Run("preserves cache affinity when adding worker", func(t *testing.T) {
		strategy := NewConsistentHash()
		workers := []string{"worker-0", "worker-1"}
		partitions := make([]types.Partition, 100)
		for i := range partitions {
			partitions[i] = types.Partition{Keys: []string{string(rune('a' + i))}, Weight: 100}
		}

		// Initial assignment with 2 workers
		initialAssignments, err := strategy.Assign(workers, partitions)
		require.NoError(t, err)

		// Add a third worker
		workers = append(workers, "worker-2")
		newAssignments, err := strategy.Assign(workers, partitions)
		require.NoError(t, err)

		// Count how many partitions stayed with their original worker
		stablePartitions := 0
		for worker, initialPartitions := range initialAssignments {
			newPartitions := newAssignments[worker]
			for _, partition := range initialPartitions {
				for _, newPartition := range newPartitions {
					if partition.Keys[0] == newPartition.Keys[0] {
						stablePartitions++

						break
					}
				}
			}
		} // Consistent hashing should preserve >60% of assignments (typical threshold)
		stabilityRate := float64(stablePartitions) / float64(len(partitions))
		require.Greater(t, stabilityRate, 0.6, "should preserve >60%% cache affinity, got %.2f%%", stabilityRate*100)
	})

	t.Run("returns error when no workers available", func(t *testing.T) {
		strategy := NewConsistentHash()
		workers := []string{}
		partitions := []types.Partition{{Keys: []string{"p0"}, Weight: 100}}

		_, err := strategy.Assign(workers, partitions)

		require.Error(t, err)
		require.Contains(t, err.Error(), "no workers")
	})

	t.Run("custom virtual nodes", func(t *testing.T) {
		strategy := NewConsistentHash(WithVirtualNodes(300))
		workers := []string{"worker-0"}
		partitions := []types.Partition{{Keys: []string{"p0"}, Weight: 100}}

		assignments, err := strategy.Assign(workers, partitions)

		require.NoError(t, err)
		require.Len(t, assignments, 1)
	})
}
