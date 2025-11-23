package parti

import (
	"testing"

	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

func TestManager_clonePartitions(t *testing.T) {
	m := &Manager{}

	original := []types.Partition{
		{Keys: []string{"p1"}, Weight: 10},
		{Keys: []string{"p2"}, Weight: 20},
	}

	cloned := m.clonePartitions(original)

	require.Equal(t, original, cloned)
	require.NotSame(t, &original[0], &cloned[0])
	require.NotSame(t, &original[0].Keys[0], &cloned[0].Keys[0])

	// Modify clone should not affect original
	cloned[0].Weight = 99
	cloned[0].Keys[0] = "modified"

	require.Equal(t, int64(10), original[0].Weight)
	require.Equal(t, "p1", original[0].Keys[0])
}
