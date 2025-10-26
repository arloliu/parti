package strategy

import (
	"testing"

	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

func TestRoundRobin_Assign(t *testing.T) {
	t.Run("distributes partitions evenly across workers", func(t *testing.T) {
		strategy := NewRoundRobin()
		workers := []string{"worker-0", "worker-1", "worker-2"}
		partitions := make([]types.Partition, 9)
		for i := range partitions {
			partitions[i] = types.Partition{Keys: []string{string(rune('a' + i))}, Weight: 100}
		}

		assignments, err := strategy.Assign(workers, partitions)

		require.NoError(t, err)
		require.Len(t, assignments, 3)
		require.Len(t, assignments["worker-0"], 3)
		require.Len(t, assignments["worker-1"], 3)
		require.Len(t, assignments["worker-2"], 3)
	})

	t.Run("handles uneven distribution", func(t *testing.T) {
		strategy := NewRoundRobin()
		workers := []string{"worker-0", "worker-1"}
		partitions := make([]types.Partition, 5)
		for i := range partitions {
			partitions[i] = types.Partition{Keys: []string{string(rune('a' + i))}, Weight: 100}
		}

		assignments, err := strategy.Assign(workers, partitions)

		require.NoError(t, err)
		require.Len(t, assignments, 2)
		require.Len(t, assignments["worker-0"], 3)
		require.Len(t, assignments["worker-1"], 2)
	})

	t.Run("returns error when no workers available", func(t *testing.T) {
		strategy := NewRoundRobin()
		workers := []string{}
		partitions := []types.Partition{{Keys: []string{"p0"}, Weight: 100}}

		_, err := strategy.Assign(workers, partitions)

		require.Error(t, err)
		require.Contains(t, err.Error(), "no workers")
	})
}
