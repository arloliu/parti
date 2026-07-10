package types

import (
	"fmt"
	"slices"
)

// MergeLabels returns a deep copy of current with each partition's Label
// adjusted by intents, keyed on Partition.ID():
//
//   - intents[id] == nil  ⇒ clear the label ("")
//   - intents[id] == &s   ⇒ set the label to s
//   - id absent           ⇒ label unchanged
//
// It is the label-preserving primitive for a writer that rebuilds its partition
// list from a source that does not carry labels: instead of hand-writing a CAS
// Modify closure that re-derives inherit/set/clear semantics (and risks silently
// stripping every label), the writer computes the new list with MergeLabels and
// hands the result straight to source.Modify.
//
// The second return is the sorted list of ids that appear in intents but not in
// current (unmatched). Callers reject these at their own boundary — for example
// returning a not-found for a typo'd id — instead of silently no-op'ing.
//
// MergeLabels fails closed on an ID() collision. Partition.ID() dash-joins Keys
// and is NOT collision-safe: Keys ["a-b","c"] and ["a","b-c"] both yield
// "a-b-c" (the write path dedupes on CanonicalID precisely because of this). If
// an intent's id matches MORE THAN ONE partition in current, MergeLabels returns
// a non-nil error and nil slices — it never guesses which partition to relabel.
// A collided partition that no intent targets is copied through untouched (no
// error), so a collision only matters when a caller actually tries to relabel
// through the ambiguous id.
//
// current is not mutated; the returned partitions deep-copy each Keys slice, so
// the result shares no backing array with current and is safe to mutate or hand
// to source.Modify. MergeLabels does NOT validate label values — pair it with
// ValidateLabel at the caller's boundary; source.validateAndDedupe backstops at
// write time.
func MergeLabels(current []Partition, intents map[string]*string) ([]Partition, []string, error) {
	// Index current by ID(); keep every matching index so a collision (an id
	// mapping to >1 partition) is detectable rather than silently resolved.
	idx := make(map[string][]int, len(current))
	for i := range current {
		id := current[i].ID()
		idx[id] = append(idx[id], i)
	}

	// First pass: classify each intent's id. Collision on a TARGETED id fails
	// closed before any result is produced; an id absent from current is
	// unmatched. This runs before the copy so an error returns nil slices.
	var unmatched []string
	for id := range intents {
		switch n := len(idx[id]); {
		case n == 0:
			unmatched = append(unmatched, id)
		case n > 1:
			return nil, nil, fmt.Errorf(
				"MergeLabels: intent id %q matches %d partitions whose key tuples collide on ID(); refusing to guess which to relabel",
				id, n)
		}
	}

	// Deep-copy current so neither the caller's slice nor its Keys backing
	// arrays are mutated, and the result is safe to hand to source.Modify.
	result := make([]Partition, len(current))
	for i := range current {
		cp := current[i]
		cp.Keys = make([]string, len(current[i].Keys))
		copy(cp.Keys, current[i].Keys)
		result[i] = cp
	}

	// Second pass: apply each unambiguously matched intent to its one partition.
	for id, val := range intents {
		is := idx[id]
		if len(is) != 1 {
			continue // unmatched (0) recorded above; collision (>1) already errored
		}
		if val == nil {
			result[is[0]].Label = "" // clear
		} else {
			result[is[0]].Label = *val // set
		}
	}

	slices.Sort(unmatched)

	return result, unmatched, nil
}
