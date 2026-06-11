package parti

import (
	"testing"

	"github.com/arloliu/parti/v2/types"
)

// TestAssignmentSupersedesForStash pins the full applied-identity ordering used
// by the retry/coalesce stash. The ordering mirrors the applied-ack identity
// the core apply gate trusts (isApplyResultStale / currentAssignmentApplied):
// (Version, LeaderRevision) lex, then PartitionSetDigest, then source-revision
// known/value. At identical full identity the candidate is an idempotent
// duplicate and cur is kept.
func TestAssignmentSupersedesForStash(t *testing.T) {
	partsA := []types.Partition{{Keys: []string{"alpha"}}}
	partsB := []types.Partition{{Keys: []string{"beta"}}, {Keys: []string{"gamma"}}}

	base := func() Assignment {
		return Assignment{Version: 7, LeaderRevision: 10, Partitions: partsA}
	}

	tests := []struct {
		name      string
		cur       Assignment
		candidate Assignment
		want      bool // candidate supersedes cur (replace)
	}{
		{
			name:      "higher version replaces",
			cur:       base(),
			candidate: Assignment{Version: 8, LeaderRevision: 1, Partitions: partsA},
			want:      true,
		},
		{
			name:      "lower version is dropped",
			cur:       base(),
			candidate: Assignment{Version: 6, LeaderRevision: 99, Partitions: partsB},
			want:      false,
		},
		{
			name:      "same version higher leader revision replaces",
			cur:       base(),
			candidate: Assignment{Version: 7, LeaderRevision: 20, Partitions: partsA},
			want:      true,
		},
		{
			name:      "same version lower leader revision is dropped",
			cur:       Assignment{Version: 7, LeaderRevision: 20, Partitions: partsA},
			candidate: Assignment{Version: 7, LeaderRevision: 10, Partitions: partsB},
			want:      false,
		},
		{
			name:      "same (V,LR) different digest replaces (last arrival wins)",
			cur:       base(),
			candidate: Assignment{Version: 7, LeaderRevision: 10, Partitions: partsB},
			want:      true,
		},
		{
			name: "same (V,LR) same digest but candidate adds known source replaces",
			cur:  base(),
			candidate: Assignment{
				Version: 7, LeaderRevision: 10, Partitions: partsA,
				SourceRevisionKnown: true, SourceRevision: 5,
			},
			want: true,
		},
		{
			name: "same (V,LR) same digest different known source value replaces",
			cur: Assignment{
				Version: 7, LeaderRevision: 10, Partitions: partsA,
				SourceRevisionKnown: true, SourceRevision: 5,
			},
			candidate: Assignment{
				Version: 7, LeaderRevision: 10, Partitions: partsA,
				SourceRevisionKnown: true, SourceRevision: 6,
			},
			want: true,
		},
		{
			name:      "identical full identity keeps cur (idempotent duplicate)",
			cur:       base(),
			candidate: base(),
			want:      false,
		},
		{
			name: "identical full identity with source keeps cur",
			cur: Assignment{
				Version: 7, LeaderRevision: 10, Partitions: partsA,
				SourceRevisionKnown: true, SourceRevision: 5,
			},
			candidate: Assignment{
				Version: 7, LeaderRevision: 10, Partitions: partsA,
				SourceRevisionKnown: true, SourceRevision: 5,
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := assignmentSupersedesForStash(tc.candidate, tc.cur)
			if got != tc.want {
				t.Fatalf("assignmentSupersedesForStash(candidate, cur) = %v, want %v", got, tc.want)
			}
		})
	}
}
