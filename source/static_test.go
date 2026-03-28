package source

import (
	"context"
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

func TestStatic_List(t *testing.T) {
	t.Run("returns all partitions", func(t *testing.T) {
		partitions := []types.Partition{
			{Keys: []string{"tool001", "chamber1"}, Weight: 100},
			{Keys: []string{"tool001", "chamber2"}, Weight: 150},
			{Keys: []string{"tool002", "chamber1"}, Weight: 200},
		}
		src := NewStatic(partitions)

		result, err := src.List(context.Background())

		require.NoError(t, err)
		require.Len(t, result, 3)
		require.Equal(t, partitions, result)
	})

	t.Run("returns empty list when no partitions", func(t *testing.T) {
		src := NewStatic([]types.Partition{})

		result, err := src.List(context.Background())

		require.NoError(t, err)
		require.Empty(t, result)
	})

	t.Run("does not modify original slice", func(t *testing.T) {
		partitions := []types.Partition{
			{Keys: []string{"p1"}, Weight: 100},
		}
		src := NewStatic(partitions)

		result, err := src.List(context.Background())
		require.NoError(t, err)

		// Modify returned slice
		result[0].Weight = 999

		// Original should be unchanged
		result2, _ := src.List(context.Background())
		require.Equal(t, int64(100), result2[0].Weight)
	})
}

func TestStatic_Start(t *testing.T) {
	t.Run("valid partitions", func(t *testing.T) {
		partitions := []types.Partition{
			{Keys: []string{"valid", "one"}},
		}
		src := NewStatic(partitions)
		require.NoError(t, src.Start(context.Background()))
	})

	t.Run("invalid partitions", func(t *testing.T) {
		partitions := []types.Partition{
			{Keys: []string{"valid"}},
			{Keys: []string{"invalid.dot"}},
		}
		src := NewStatic(partitions)
		err := src.Start(context.Background())
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid partition at index 1")
		require.Contains(t, err.Error(), "invalid character '.'")
	})
}
