package source

import (
	"context"
	"testing"

	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

func TestStatic_ListPartitions(t *testing.T) {
	t.Run("returns all partitions", func(t *testing.T) {
		partitions := []types.Partition{
			{Keys: []string{"tool001", "chamber1"}, Weight: 100},
			{Keys: []string{"tool001", "chamber2"}, Weight: 150},
			{Keys: []string{"tool002", "chamber1"}, Weight: 200},
		}
		src := NewStatic(partitions)

		result, err := src.ListPartitions(context.Background())

		require.NoError(t, err)
		require.Len(t, result, 3)
		require.Equal(t, partitions, result)
	})

	t.Run("returns empty list when no partitions", func(t *testing.T) {
		src := NewStatic([]types.Partition{})

		result, err := src.ListPartitions(context.Background())

		require.NoError(t, err)
		require.Empty(t, result)
	})

	t.Run("does not modify original slice", func(t *testing.T) {
		partitions := []types.Partition{
			{Keys: []string{"p1"}, Weight: 100},
		}
		src := NewStatic(partitions)

		result, err := src.ListPartitions(context.Background())
		require.NoError(t, err)

		// Modify returned slice
		result[0].Weight = 999

		// Original should be unchanged
		result2, _ := src.ListPartitions(context.Background())
		require.Equal(t, int64(100), result2[0].Weight)
	})
}
