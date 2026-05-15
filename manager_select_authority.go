package parti

import (
	"github.com/arloliu/parti/v2/types"
)

// AuthorityChoice is the result of the dual-read source-of-truth selection
// rule (§3.6). It tells the manager which channel — the commit watcher or
// the legacy alias watcher — should drive the next applyAssignment call for
// the observation in hand.
type AuthorityChoice int

const (
	// AuthorityNone indicates no usable authority is available; the manager
	// should wait for a fresher observation.
	AuthorityNone AuthorityChoice = iota
	// AuthorityCommit indicates the commit watcher's payload should drive
	// the apply.
	AuthorityCommit
	// AuthorityLegacyAlias indicates the legacy alias watcher's payload
	// should drive the apply (rolling-upgrade or new-leader→old-leader
	// handoff window).
	AuthorityLegacyAlias
)

// selectAuthority implements §3.6 source-of-truth selection. The rule:
//
//	Case 1: commit wins when commit exists AND (no legacy OR
//	        commit.LeaderRevision >= legacyEntry.LeaderRevision).
//	Case 2: legacy alias wins when legacy exists AND legacy.LeaderRevision
//	        >= lastSeen AND (no commit OR legacy.LeaderRevision >
//	        commit.LeaderRevision). This is the rolling-upgrade window where
//	        an old leader has just written a higher-rev alias and the new
//	        commit (from a different leader term) is older.
//	Case 3: otherwise — no usable authority.
//
// Pure function; testable without a Manager.
//
// Parameters:
//   - commit: Most recently observed AssignmentCommit (nil if none).
//   - legacyEntry: Most recently observed legacy alias Assignment (nil if none).
//   - lastSeen: Highest LeaderRevision the worker has previously accepted.
//
// Returns:
//   - AuthorityChoice: AuthorityCommit, AuthorityLegacyAlias, or AuthorityNone.
func selectAuthority(commit *types.AssignmentCommit, legacyEntry *types.Assignment, lastSeen uint64) AuthorityChoice {
	hasCommit := commit != nil
	hasLegacy := legacyEntry != nil

	if hasCommit && (!hasLegacy || commit.LeaderRevision >= legacyEntry.LeaderRevision) {
		return AuthorityCommit
	}
	if hasLegacy && legacyEntry.LeaderRevision >= lastSeen &&
		(!hasCommit || legacyEntry.LeaderRevision > commit.LeaderRevision) {
		return AuthorityLegacyAlias
	}

	return AuthorityNone
}
