package parti

import (
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// TestRollingUpgrade_NewWorker_AppliesLegacyAliasWhenNoCommit verifies §3.6
// case 2 / case 3 in the alias-only world: a new (CapAckV1-capable) worker
// applies the legacy alias when no commit key exists.
func TestRollingUpgrade_NewWorker_AppliesLegacyAliasWhenNoCommit(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)
	// No commit observed.
	m.lastObservedCommit.Store(nil)

	// Mimic the watcher firing on a legacy alias entry. We invoke
	// handleAssignmentEntry by directly poking the post-decode path so we
	// can stage the Assignment value precisely.
	legacy := Assignment{
		Version:        4,
		LeaderRevision: 15,
		Partitions:     []Partition{{Keys: []string{"alpha"}}, {Keys: []string{"beta"}}},
	}
	m.lastSeenAlias.Store(&legacy)
	choice := selectAuthority(nil, &legacy, 0)
	require.Equal(t, AuthorityLegacyAlias, choice)
	require.NoError(t, m.applyAssignment(legacy))

	cur := m.CurrentAssignment()
	require.Equal(t, int64(4), cur.Version)
	require.Equal(t, uint64(15), cur.LeaderRevision)
	require.Equal(t, int64(1), rh.applyCount.Load())
}

// TestRollingUpgrade_NewLeaderToOldLeaderHandoff_AliasOverridesStaleCommit
// validates the §3.6 "previously-fragile handoff" case: a new leader
// produces a higher-LeaderRevision alias than the existing commit; the
// dual-read selector picks the alias.
func TestRollingUpgrade_NewLeaderToOldLeaderHandoff_AliasOverridesStaleCommit(t *testing.T) {
	t.Parallel()
	commit := &types.AssignmentCommit{Version: 10, LeaderRevision: 20}
	legacy := &Assignment{Version: 11, LeaderRevision: 25}
	choice := selectAuthority(commit, legacy, 0)
	require.Equal(t, AuthorityLegacyAlias, choice, "legacy.LR=25 > commit.LR=20 ⇒ legacy wins")
}

// TestRollingUpgrade_AliasFresherThanCommit_NextCommitTakesOver continues
// the previous scenario: a new commit at an even higher LeaderRevision
// arrives and once again wins the selector.
func TestRollingUpgrade_AliasFresherThanCommit_NextCommitTakesOver(t *testing.T) {
	t.Parallel()
	commit := &types.AssignmentCommit{Version: 12, LeaderRevision: 30}
	legacy := &Assignment{Version: 11, LeaderRevision: 25}
	choice := selectAuthority(commit, legacy, 25)
	require.Equal(t, AuthorityCommit, choice, "commit.LR=30 > legacy.LR=25 ⇒ commit wins")
}
