package assignment

import (
	"fmt"
	"slices"

	"github.com/arloliu/parti/v2/types"
)

// topologyInput is the input to buildLabelTopology: the active worker set,
// their label reads, the partition snapshot, and the unlabeled-partition
// policy.
type topologyInput struct {
	Workers    []string            // active set, post-filter
	Labels     map[string][]string // workerID → sorted label set (successful reads)
	Unknown    map[string]bool     // workerID → labels unreadable (§6); disjoint from Labels
	Partitions []types.Partition   // source snapshot
	Policy     string              // "dedicated" | "shared" ("" = dedicated)
}

// labelTopology is the output of buildLabelTopology: workers partitioned
// into label pools, partitions partitioned into label groups, plus the
// derived pools computeLabelAssignments needs to run the strategy per
// group and merge.
type labelTopology struct {
	Pools        map[string][]string          // label → matching workers (sorted)
	Groups       map[string][]types.Partition // label → partitions; "" key = unlabeled group
	SortedLabels []string                     // sorted keys of Groups, excluding ""
	EmptyLabels  []string                     // labels whose pool is empty (sorted)
	GeneralPool  []string                     // per policy; empty ⇒ caller uses AllWorkers
	FallbackPool []string                     // unlabeled workers; empty ⇒ caller uses AllWorkers
	AllWorkers   []string                     // Workers minus Unknown (sorted)
	UnknownSet   map[string]bool              // pass-through of in.Unknown
}

// emptyPoolAction is the caller-supplied decision for a labeled group whose
// pool is empty: park the group's partitions (leave them unassigned) or
// spill them onto the fallback/general pool.
type emptyPoolAction int

const (
	emptyPoolPark emptyPoolAction = iota
	emptyPoolSpill
)

// buildLabelTopology partitions workers and partitions into label pools
// and groups (spec §7). Pure and deterministic: all slices sorted.
func buildLabelTopology(in topologyInput) labelTopology {
	topo := labelTopology{
		Pools:      map[string][]string{},
		Groups:     map[string][]types.Partition{},
		UnknownSet: in.Unknown,
	}

	// Active workers minus unknown-label workers, sorted.
	for _, w := range in.Workers {
		if in.Unknown[w] {
			continue
		}
		topo.AllWorkers = append(topo.AllWorkers, w)
		labels := in.Labels[w]
		if len(labels) == 0 {
			topo.FallbackPool = append(topo.FallbackPool, w)
		}
		for _, l := range labels {
			topo.Pools[l] = append(topo.Pools[l], w)
		}
	}
	slices.Sort(topo.AllWorkers)
	slices.Sort(topo.FallbackPool)
	for l := range topo.Pools {
		slices.Sort(topo.Pools[l])
	}

	if in.Policy == "shared" {
		topo.GeneralPool = topo.AllWorkers
	} else { // "dedicated" and "" default
		topo.GeneralPool = topo.FallbackPool
	}

	for _, p := range in.Partitions {
		topo.Groups[p.Label] = append(topo.Groups[p.Label], p)
	}
	for l := range topo.Groups {
		if l == "" {
			continue
		}
		topo.SortedLabels = append(topo.SortedLabels, l)
		if len(topo.Pools[l]) == 0 {
			topo.EmptyLabels = append(topo.EmptyLabels, l)
		}
	}
	slices.Sort(topo.SortedLabels)
	slices.Sort(topo.EmptyLabels)

	return topo
}

// computeLabelAssignments runs the configured strategy once per pool and
// merges (spec §7). actions supplies the park/spill decision for each
// empty-pool label (Task 8 computes them from grace state); a missing
// entry defaults to park — the conservative choice, and unreachable in
// production because the calculator always populates every EmptyLabels
// entry.
func computeLabelAssignments(
	strategy types.AssignmentStrategy,
	topo labelTopology,
	actions map[string]emptyPoolAction,
) (map[string][]types.Partition, []types.Partition, error) {
	merged := make(map[string][]types.Partition, len(topo.AllWorkers)+len(topo.UnknownSet))
	var parked []types.Partition

	assignGroup := func(pool []string, group []types.Partition, what string) error {
		if len(group) == 0 {
			return nil
		}
		if len(pool) == 0 {
			pool = topo.AllWorkers // guarded final rung; callers pre-check
		}
		if len(pool) == 0 {
			return fmt.Errorf("label pipeline: no workers available for %s", what)
		}
		out, err := strategy.Assign(pool, group)
		if err != nil {
			return fmt.Errorf("label pipeline: %s: %w", what, err)
		}
		for w, ps := range out {
			// First write stores the strategy's slice untouched — the I1
			// golden contract is nil-vs-empty sensitive, and appending zero
			// elements to a nil slice would turn a strategy's non-nil empty
			// slice into nil. Storing the slice is safe: each Assign call
			// returns a freshly allocated map owned solely by this function.
			if cur, ok := merged[w]; ok {
				merged[w] = append(cur, ps...)
			} else {
				merged[w] = ps
			}
		}

		return nil
	}

	// Unlabeled group → general pool (ladder: general → all).
	if err := assignGroup(topo.GeneralPool, topo.Groups[""], "unlabeled group"); err != nil {
		return nil, nil, err
	}

	// Labeled groups in sorted order.
	for _, l := range topo.SortedLabels {
		pool := topo.Pools[l]
		if len(pool) > 0 {
			if err := assignGroup(pool, topo.Groups[l], "label "+l); err != nil {
				return nil, nil, err
			}
			continue
		}
		//nolint:exhaustive // default arm handles emptyPoolPark AND any out-of-range value: park is the safety net that keeps I9 (no partition vanishes) on enum drift.
		switch actions[l] {
		case emptyPoolSpill:
			// Ladder: unlabeled workers first (never another label's
			// dedicated pool), then all workers.
			if err := assignGroup(topo.FallbackPool, topo.Groups[l], "spilled label "+l); err != nil {
				return nil, nil, err
			}
		default:
			// Park — covers emptyPoolPark, missing actions[l] entries (the
			// zero value is emptyPoolPark, the conservative default), and
			// any future/invalid enum value. Missing entries are unreachable
			// in production: the calculator populates every EmptyLabels
			// entry.
			parked = append(parked, topo.Groups[l]...)
		}
	}

	// I8: every active worker appears exactly once — explicit empty
	// entries for workers in no pool and for unknown-label workers.
	for _, w := range topo.AllWorkers {
		if _, ok := merged[w]; !ok {
			merged[w] = []types.Partition{}
		}
	}
	for w := range topo.UnknownSet {
		merged[w] = []types.Partition{}
	}

	return merged, parked, nil
}
