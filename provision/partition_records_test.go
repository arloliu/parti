package provision

import (
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

func part(weight int64, keys ...string) types.Partition {
	return types.Partition{Keys: keys, Weight: weight}
}

func TestDiffPartitions_AddOnly(t *testing.T) {
	t.Parallel()

	added, removed, changed := diffPartitions(nil, []types.Partition{part(1, "a"), part(2, "b")})

	require.Len(t, added, 2)
	require.Empty(t, removed)
	require.Empty(t, changed)
	require.Equal(t, []string{"a"}, added[0].Keys)
	require.Equal(t, []string{"b"}, added[1].Keys)
}

func TestDiffPartitions_RemoveOnly(t *testing.T) {
	t.Parallel()

	live := []types.Partition{part(1, "a"), part(1, "b"), part(1, "c")}
	desired := []types.Partition{part(1, "b")}

	added, removed, changed := diffPartitions(live, desired)

	require.Empty(t, added)
	require.Len(t, removed, 2)
	require.Empty(t, changed)
	require.Equal(t, []string{"a"}, removed[0].Keys)
	require.Equal(t, []string{"c"}, removed[1].Keys)
}

func TestDiffPartitions_WeightChangeOnly(t *testing.T) {
	t.Parallel()

	live := []types.Partition{part(1, "a"), part(5, "b")}
	desired := []types.Partition{part(9, "a"), part(5, "b")}

	added, removed, changed := diffPartitions(live, desired)

	require.Empty(t, added)
	require.Empty(t, removed)
	require.Len(t, changed, 1)
	require.Equal(t, []string{"a"}, changed[0].Keys)
	require.Equal(t, int64(1), changed[0].OldWeight)
	require.Equal(t, int64(9), changed[0].NewWeight)
}

func TestDiffPartitions_Mixed(t *testing.T) {
	t.Parallel()

	live := []types.Partition{part(1, "keep"), part(1, "drop"), part(2, "reweight")}
	desired := []types.Partition{part(1, "keep"), part(7, "reweight"), part(1, "new")}

	added, removed, changed := diffPartitions(live, desired)

	require.Len(t, added, 1)
	require.Equal(t, []string{"new"}, added[0].Keys)
	require.Len(t, removed, 1)
	require.Equal(t, []string{"drop"}, removed[0].Keys)
	require.Len(t, changed, 1)
	require.Equal(t, []string{"reweight"}, changed[0].Keys)
	require.Equal(t, int64(2), changed[0].OldWeight)
	require.Equal(t, int64(7), changed[0].NewWeight)
}

func TestDiffPartitions_NoDiff(t *testing.T) {
	t.Parallel()

	set := []types.Partition{part(1, "a"), part(2, "b")}

	added, removed, changed := diffPartitions(set, set)

	// All three are non-nil empty slices so plan JSON renders [] not null.
	require.NotNil(t, added)
	require.NotNil(t, removed)
	require.NotNil(t, changed)
	require.Empty(t, added)
	require.Empty(t, removed)
	require.Empty(t, changed)
}

// TestDiffPartitions_DifferentKeysIsAddPlusRemove verifies a record whose key
// set differs is a distinct partition, not a weight change.
func TestDiffPartitions_DifferentKeysIsAddPlusRemove(t *testing.T) {
	t.Parallel()

	added, removed, changed := diffPartitions(
		[]types.Partition{part(1, "topic", "0")},
		[]types.Partition{part(1, "topic", "1")},
	)

	require.Len(t, added, 1)
	require.Len(t, removed, 1)
	require.Empty(t, changed)
}

// TestDiffPartitions_DeterministicOrdering feeds unsorted inputs and asserts
// every output list is sorted by CanonicalID.
func TestDiffPartitions_DeterministicOrdering(t *testing.T) {
	t.Parallel()

	live := []types.Partition{part(1, "d"), part(1, "b"), part(1, "z"), part(1, "w1"), part(1, "w2")}
	desired := []types.Partition{
		part(1, "c"), part(1, "a"), part(1, "e"), // adds, unsorted
		part(9, "w2"), part(9, "w1"), // weight changes, unsorted
	}

	added, removed, changed := diffPartitions(live, desired)

	require.Equal(t, []string{"a"}, added[0].Keys)
	require.Equal(t, []string{"c"}, added[1].Keys)
	require.Equal(t, []string{"e"}, added[2].Keys)
	require.Equal(t, []string{"b"}, removed[0].Keys)
	require.Equal(t, []string{"d"}, removed[1].Keys)
	require.Equal(t, []string{"z"}, removed[2].Keys)
	require.Equal(t, []string{"w1"}, changed[0].Keys)
	require.Equal(t, []string{"w2"}, changed[1].Keys)
}

// TestDiffPartitions_OutputIsDeepCopied verifies the diff does not alias the
// caller's Keys slices.
func TestDiffPartitions_OutputIsDeepCopied(t *testing.T) {
	t.Parallel()

	desired := []types.Partition{{Keys: []string{"orig"}, Weight: 1}}
	added, _, _ := diffPartitions(nil, desired)
	require.Len(t, added, 1)

	desired[0].Keys[0] = "mutated"
	require.Equal(t, "orig", added[0].Keys[0], "diff output must not alias input Keys")
}
