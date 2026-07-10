package types_test

import (
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

func TestMergeLabels_SetClearAbsent(t *testing.T) {
	t.Parallel()

	current := []types.Partition{
		{Keys: []string{"a"}, Weight: 1, Label: "gold"},
		{Keys: []string{"b"}, Weight: 2},
		{Keys: []string{"c"}, Weight: 3, Label: "vip"},
	}

	got, unmatched, err := types.MergeLabels(current, map[string]*string{
		"a": new("vip"), // set (overwrite existing)
		"c": nil,        // clear
		// "b" absent ⇒ unchanged
	})
	require.NoError(t, err)
	require.Empty(t, unmatched)

	require.Equal(t, "vip", got[0].Label, "a set to vip")
	require.Equal(t, "", got[1].Label, "b unchanged (empty)")
	require.Equal(t, "", got[2].Label, "c cleared")
	// Non-label fields preserved.
	require.Equal(t, int64(1), got[0].Weight)
	require.Equal(t, []string{"c"}, got[2].Keys)
}

func TestMergeLabels_UnknownIDUnmatched(t *testing.T) {
	t.Parallel()

	current := []types.Partition{{Keys: []string{"a"}}}

	got, unmatched, err := types.MergeLabels(current, map[string]*string{
		"zzz": new("vip"),
		"aaa": nil,
	})
	require.NoError(t, err)
	require.Len(t, got, 1, "no partition added for unknown ids")
	require.Equal(t, "", got[0].Label)
	require.Equal(t, []string{"aaa", "zzz"}, unmatched, "unmatched ids returned sorted")
}

func TestMergeLabels_EmptyAndNilIntents(t *testing.T) {
	t.Parallel()

	current := []types.Partition{
		{Keys: []string{"a"}, Label: "gold"},
		{Keys: []string{"b", "sub"}, Weight: 5},
	}

	for _, name := range []string{"nil", "empty"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var intents map[string]*string
			if name == "empty" {
				intents = map[string]*string{}
			}
			got, unmatched, err := types.MergeLabels(current, intents)
			require.NoError(t, err)
			require.Empty(t, unmatched)
			require.Equal(t, current, got, "deep-equal copy of current")

			// Even with no intents applied, the result must deep-copy Keys so a
			// caller cannot mutate current through the returned slice.
			require.NotSame(t, &current[0].Keys[0], &got[0].Keys[0],
				"Keys must not share a backing array with current")
			got[0].Keys[0] = "mutated"
			require.Equal(t, "a", current[0].Keys[0], "mutating result must not touch current")
		})
	}
}

func TestMergeLabels_CollisionOutranksUnmatched(t *testing.T) {
	t.Parallel()

	// current holds the ID() collision pair plus a normal partition.
	current := []types.Partition{
		{Keys: []string{"a-b", "c"}},
		{Keys: []string{"a", "b-c"}},
		{Keys: []string{"solo"}},
	}

	// Intents carry BOTH a collided-id target and an unmatched id. The
	// fail-closed collision must win: error + nil slices, regardless of the
	// unmatched id also present.
	got, unmatched, err := types.MergeLabels(current, map[string]*string{
		"a-b-c": new("vip"), // ambiguous collision
		"typo":  new("vip"), // unmatched
	})
	require.Error(t, err)
	require.Nil(t, got, "collision returns nil partitions")
	require.Nil(t, unmatched, "collision returns nil unmatched, not the typo slice")
}

func TestMergeLabels_DoesNotValidateLabelValues(t *testing.T) {
	t.Parallel()

	// MergeLabels is mechanical: it does not run ValidateLabel. An invalid
	// label value is applied as-is; the caller validates at its boundary and the
	// write path backstops. This pins that contract.
	current := []types.Partition{{Keys: []string{"a"}}}
	got, unmatched, err := types.MergeLabels(current, map[string]*string{"a": new("has space")})
	require.NoError(t, err)
	require.Empty(t, unmatched)
	require.Equal(t, "has space", got[0].Label, "invalid label value applied verbatim, not rejected")
	require.Error(t, types.ValidateLabel(got[0].Label), "the applied value is indeed invalid")
}

func TestMergeLabels_NoAliasing(t *testing.T) {
	t.Parallel()

	current := []types.Partition{{Keys: []string{"a", "b"}, Label: "gold"}}

	got, _, err := types.MergeLabels(current, map[string]*string{"a-b": new("vip")})
	require.NoError(t, err)

	// Mutating the result must not touch current — no shared Keys backing array,
	// and the label change stayed on the copy.
	got[0].Keys[0] = "mutated"
	require.Equal(t, "a", current[0].Keys[0], "current Keys must not alias result")
	require.Equal(t, "gold", current[0].Label, "current Label must be unchanged")

	// And mutating current after the call must not touch the result.
	current[0].Label = "changed"
	require.Equal(t, "vip", got[0].Label, "result must not alias current")
}

func TestMergeLabels_CollisionFailsClosed(t *testing.T) {
	t.Parallel()

	// The repo's pinned ID() collision pair: both produce ID() "a-b-c".
	collided := []types.Partition{
		{Keys: []string{"a-b", "c"}},
		{Keys: []string{"a", "b-c"}},
	}
	require.Equal(t, collided[0].ID(), collided[1].ID())

	t.Run("intent targets collided id ⇒ error, nil slices", func(t *testing.T) {
		t.Parallel()
		got, unmatched, err := types.MergeLabels(collided, map[string]*string{"a-b-c": new("vip")})
		require.Error(t, err)
		require.Nil(t, got)
		require.Nil(t, unmatched)
		require.Contains(t, err.Error(), "a-b-c")
	})

	t.Run("intent does not target collided id ⇒ no error, collided pass through", func(t *testing.T) {
		t.Parallel()
		current := append([]types.Partition{{Keys: []string{"other"}}}, collided...)
		got, unmatched, err := types.MergeLabels(current, map[string]*string{"other": new("vip")})
		require.NoError(t, err)
		require.Empty(t, unmatched)
		require.Equal(t, "vip", got[0].Label, "targeted partition relabeled")
		// Both collided partitions copied through untouched.
		require.Equal(t, "", got[1].Label)
		require.Equal(t, "", got[2].Label)
		require.Equal(t, []string{"a-b", "c"}, got[1].Keys)
		require.Equal(t, []string{"a", "b-c"}, got[2].Keys)
	})
}
