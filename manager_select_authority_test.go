package parti

import (
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

func TestSelectAuthority_TableDriven(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		commit   *types.AssignmentCommit
		legacy   *types.Assignment
		lastSeen uint64
		want     AuthorityChoice
	}{
		{
			name:     "Case1_CommitOnly",
			commit:   &types.AssignmentCommit{Version: 1, LeaderRevision: 10},
			legacy:   nil,
			lastSeen: 0,
			want:     AuthorityCommit,
		},
		{
			name:     "Case1_CommitFresherThanLegacy",
			commit:   &types.AssignmentCommit{LeaderRevision: 20},
			legacy:   &types.Assignment{LeaderRevision: 15},
			lastSeen: 10,
			want:     AuthorityCommit,
		},
		{
			name:     "Case1_CommitEqualLegacy_TieGoesToCommit",
			commit:   &types.AssignmentCommit{LeaderRevision: 20},
			legacy:   &types.Assignment{LeaderRevision: 20},
			lastSeen: 10,
			want:     AuthorityCommit,
		},
		{
			name:     "Case2_LegacyFresherThanCommit_HandoffWindow",
			commit:   &types.AssignmentCommit{LeaderRevision: 10},
			legacy:   &types.Assignment{LeaderRevision: 20},
			lastSeen: 10,
			want:     AuthorityLegacyAlias,
		},
		{
			name:     "Case2_LegacyOnly_NoCommit",
			commit:   nil,
			legacy:   &types.Assignment{LeaderRevision: 10},
			lastSeen: 10,
			want:     AuthorityLegacyAlias,
		},
		{
			name:     "Case3_LegacyBelowLastSeen",
			commit:   nil,
			legacy:   &types.Assignment{LeaderRevision: 5},
			lastSeen: 10,
			want:     AuthorityNone,
		},
		{
			name:     "Case3_NoCommitNoLegacy",
			commit:   nil,
			legacy:   nil,
			lastSeen: 0,
			want:     AuthorityNone,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := selectAuthority(tc.commit, tc.legacy, tc.lastSeen)
			require.Equal(t, tc.want, got)
		})
	}
}
