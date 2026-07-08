package assignment

import (
	"testing"

	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

func parts(labels ...string) []types.Partition {
	out := make([]types.Partition, len(labels))
	for i, l := range labels {
		out[i] = types.Partition{Keys: []string{string(rune('a' + i))}, Label: l}
	}
	return out
}

func topo(workers []string, labels map[string][]string, partitions []types.Partition, policy string) labelTopology {
	return buildLabelTopology(topologyInput{
		Workers: workers, Labels: labels, Partitions: partitions, Policy: policy,
	})
}

func TestBuildLabelTopology_PoolsAndGroups(t *testing.T) {
	t.Parallel()

	tp := topo(
		[]string{"w0", "w1", "w2"},
		map[string][]string{"w0": {"vip"}, "w1": {"batch", "vip"}, "w2": nil},
		parts("vip", "", "batch", "ghost"),
		"dedicated",
	)

	require.Equal(t, []string{"w0", "w1"}, tp.Pools["vip"])
	require.Equal(t, []string{"w1"}, tp.Pools["batch"])
	require.Equal(t, []string{"w2"}, tp.GeneralPool, "dedicated: unlabeled workers only")
	require.Equal(t, []string{"w2"}, tp.FallbackPool)
	require.Equal(t, []string{"ghost"}, tp.EmptyLabels, "label with no matching worker")
	require.Len(t, tp.Groups[""], 1)
	require.Len(t, tp.Groups["vip"], 1)
}

func TestBuildLabelTopology_SharedPolicy(t *testing.T) {
	t.Parallel()

	tp := topo(
		[]string{"w0", "w1"},
		map[string][]string{"w0": {"vip"}, "w1": nil},
		parts(""),
		"shared",
	)
	require.Equal(t, []string{"w0", "w1"}, tp.GeneralPool, "shared: all workers")
}

func TestComputeLabelAssignments_MergeContract(t *testing.T) {
	t.Parallel()

	// w0 vip-only; w1 unlabeled; w2 unknown labels; one vip partition,
	// one unlabeled partition. Expect: w0 gets vip, w1 gets unlabeled,
	// w2 present with EMPTY slice (I8 — no stale-assignment leak).
	in := topologyInput{
		Workers:    []string{"w0", "w1", "w2"},
		Labels:     map[string][]string{"w0": {"vip"}, "w1": nil},
		Unknown:    map[string]bool{"w2": true},
		Partitions: parts("vip", ""),
		Policy:     "dedicated",
	}
	tp := buildLabelTopology(in)
	merged, parked, err := computeLabelAssignments(strategy.NewRoundRobin(), tp, nil)
	require.NoError(t, err)
	require.Empty(t, parked)

	require.Len(t, merged, 3, "every active worker gets an entry")
	require.NotNil(t, merged["w2"])
	require.Empty(t, merged["w2"], "unknown-label worker: explicit empty entry")
	require.Len(t, merged["w0"], 1)
	require.Equal(t, "vip", merged["w0"][0].Label)
	require.Len(t, merged["w1"], 1)

	total := 0
	for _, ps := range merged {
		total += len(ps)
	}
	require.Equal(t, 2, total, "each partition exactly once (I9)")
}

func TestComputeLabelAssignments_ParkAndSpill(t *testing.T) {
	t.Parallel()

	in := topologyInput{
		Workers:    []string{"w0"},
		Labels:     map[string][]string{"w0": nil},
		Partitions: parts("vip", ""),
		Policy:     "dedicated",
	}
	tp := buildLabelTopology(in)
	require.Equal(t, []string{"vip"}, tp.EmptyLabels)

	// Park:
	merged, parked, err := computeLabelAssignments(strategy.NewRoundRobin(), tp,
		map[string]emptyPoolAction{"vip": emptyPoolPark})
	require.NoError(t, err)
	require.Len(t, parked, 1)
	require.Equal(t, "vip", parked[0].Label)
	require.Len(t, merged["w0"], 1, "unlabeled partition still assigned")

	// Spill:
	merged, parked, err = computeLabelAssignments(strategy.NewRoundRobin(), tp,
		map[string]emptyPoolAction{"vip": emptyPoolSpill})
	require.NoError(t, err)
	require.Empty(t, parked)
	require.Len(t, merged["w0"], 2, "spilled onto the fallback pool")
}

func TestComputeLabelAssignments_SpillPrefersUnlabeledWorkers(t *testing.T) {
	t.Parallel()

	// vip pool empty; batch pool exists; one unlabeled worker. Spilled
	// vip partitions must land on the unlabeled worker, never invade the
	// batch pool (spec I5).
	in := topologyInput{
		Workers:    []string{"batchw", "plainw"},
		Labels:     map[string][]string{"batchw": {"batch"}, "plainw": nil},
		Partitions: parts("vip"),
		Policy:     "dedicated",
	}
	tp := buildLabelTopology(in)
	merged, _, err := computeLabelAssignments(strategy.NewRoundRobin(), tp,
		map[string]emptyPoolAction{"vip": emptyPoolSpill})
	require.NoError(t, err)
	require.Empty(t, merged["batchw"], "spill must not invade another label's pool")
	require.Len(t, merged["plainw"], 1)
}

// TestComputeLabelAssignments_I1Golden: with zero labels anywhere the
// pipeline output must be identical to a direct Strategy.Assign call.
// Two geometries: more partitions than workers (every worker gets some),
// and more workers than partitions (some workers get zero — pins that the
// merge preserves the strategy's non-nil empty slices untouched).
func TestComputeLabelAssignments_I1Golden(t *testing.T) {
	t.Parallel()

	geometries := []struct {
		name       string
		workers    []string
		partitions int
	}{
		{"more partitions than workers", []string{"w0", "w1", "w2"}, 10},
		{"more workers than partitions", []string{"w0", "w1", "w2", "w3", "w4"}, 3},
	}

	for _, g := range geometries {
		partitions := make([]types.Partition, g.partitions)
		for i := range partitions {
			partitions[i] = types.Partition{Keys: []string{"p", string(rune('0' + i))}, Weight: int64(i%3 + 1)}
		}
		labels := make(map[string][]string, len(g.workers))
		for _, w := range g.workers {
			labels[w] = nil
		}

		for _, st := range []types.AssignmentStrategy{
			strategy.NewRoundRobin(),
			strategy.NewConsistentHash(),
		} {
			direct, err := st.Assign(g.workers, partitions)
			require.NoError(t, err)

			tp := buildLabelTopology(topologyInput{
				Workers:    g.workers,
				Labels:     labels,
				Partitions: partitions,
				Policy:     "dedicated",
			})
			merged, parked, err := computeLabelAssignments(st, tp, nil)
			require.NoError(t, err)
			require.Empty(t, parked)
			require.Equal(t, direct, merged, "label-free pipeline must equal direct Assign (%s)", g.name)
		}
	}
}

func TestComputeLabelAssignments_Deterministic(t *testing.T) {
	t.Parallel()

	in := topologyInput{
		Workers:    []string{"w2", "w0", "w1"},
		Labels:     map[string][]string{"w0": {"vip"}, "w1": {"batch"}, "w2": nil},
		Partitions: parts("vip", "batch", "", "vip"),
		Policy:     "dedicated",
	}
	tp := buildLabelTopology(in)
	first, _, err := computeLabelAssignments(strategy.NewConsistentHash(), tp, nil)
	require.NoError(t, err)
	for range 20 {
		tp2 := buildLabelTopology(in)
		again, _, err := computeLabelAssignments(strategy.NewConsistentHash(), tp2, nil)
		require.NoError(t, err)
		require.Equal(t, first, again)
	}
}
