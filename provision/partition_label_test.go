package provision

import (
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

func TestClonePartition_PreservesLabel(t *testing.T) {
	t.Parallel()

	p := types.Partition{Keys: []string{"a"}, Weight: 2, Label: "vip"}
	got := clonePartition(p)
	require.Equal(t, p, got)
}

func TestDiffPartitions_LabelOnlyChangeIsVisible(t *testing.T) {
	t.Parallel()

	live := []types.Partition{{Keys: []string{"a"}, Weight: 1}}
	desired := []types.Partition{{Keys: []string{"a"}, Weight: 1, Label: "vip"}}

	added, removed, changed := diffPartitions(live, desired)
	require.Empty(t, added)
	require.Empty(t, removed)
	require.Len(t, changed, 1, "label-only edit must surface as a change in plan output")
	require.Equal(t, "", changed[0].OldLabel)
	require.Equal(t, "vip", changed[0].NewLabel)
	require.Equal(t, int64(1), changed[0].OldWeight)
	require.Equal(t, int64(1), changed[0].NewWeight)
}

func TestPartitionTablesEqual_LabelAware(t *testing.T) {
	t.Parallel()

	a := []types.Partition{{Keys: []string{"a"}, Weight: 1}}
	b := []types.Partition{{Keys: []string{"a"}, Weight: 1, Label: "vip"}}

	require.True(t, partitionTablesEqual(a, a))
	require.False(t, partitionTablesEqual(a, b),
		"label-only apply must not be skipped as a no-op")
}
