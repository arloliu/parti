package parti

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fullBootstrapAssignment is the pre-advanced full set a worker's
// waitForAssignment stores into the snapshot before any claim is written.
func fullBootstrapAssignment() Assignment {
	return Assignment{
		Version:        1,
		LeaderRevision: 5,
		Partitions:     []Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}},
	}
}

// TestScheduleApplyRetry_BeforeBootstrap_UsesEmptyPrev is the verify-first
// reproducer for the startup empty-diff retry self-exit (root cause RC3 / F-D3).
//
// Sequence reproduced: waitForAssignment pre-advances the in-memory snapshot to
// the full partition set BEFORE any claim is written (manager_election.go's
// m.assignment.Store with no apply hook); the initial apply fails under a
// KV-write fault and schedules a retry. On the parent the retry — and every
// other apply path — reads prev = m.CurrentAssignment() = the pre-advanced full
// set, so the handoff coordinator's prepare diff is EMPTY (preparePhase only
// writes partitions in next absent from prev): the apply "succeeds" writing ZERO
// claims and the retry self-exits. Claims never land until a process restart.
//
// The fix: while no claims have ever been committed, applyAssignmentWithPrevCore
// overrides prev to empty so the FULL claim set is (re)written. This test pins
// that the retry path benefits from the override by capturing the prev the
// coordinator receives. It references only the pre-existing API, so it compiles
// and FAILS on the parent base (the coordinator gets the full pre-advanced prev).
func TestScheduleApplyRetry_BeforeBootstrap_UsesEmptyPrev(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)

	// Simulate waitForAssignment: pre-advance the snapshot to the full set with
	// NO claims written (no apply hook ran — a direct, non-apply store).
	full := fullBootstrapAssignment()
	m.assignment.Store(full)

	// Schedule a retry for the same full set, exactly as the failed initial
	// apply does on its apply-failure arm.
	m.scheduleApplyRetry(full)

	// Wait past the ~1s+jitter retry backoff for the retry's Apply to fire.
	require.Eventually(t, func() bool { return rh.applyCount.Load() >= 1 },
		5*time.Second, 50*time.Millisecond, "retry's Apply must fire within 5s")

	rh.mu.Lock()
	defer rh.mu.Unlock()
	require.NotEmpty(t, rh.applyPrevs, "retry must have called Apply at least once")
	prev := rh.applyPrevs[len(rh.applyPrevs)-1]
	require.Empty(t, prev.Partitions,
		"before the first claim commit the coordinator MUST receive an empty prev so the "+
			"prepare diff is the FULL partition set; the pre-advanced snapshot yields an "+
			"empty diff and a zero-claim self-exit (the startup empty-diff bug)")
}

// TestApplyCore_BootstrapOverridesPrevAndLatches pins both halves of the fix on
// the shared apply pipeline: while no claims have been committed, core overrides
// the caller's (pre-advanced) prev to empty so the coordinator writes the full
// claim set, and a successful such write latches initialClaimsCommitted.
func TestApplyCore_BootstrapOverridesPrevAndLatches(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)

	full := fullBootstrapAssignment()
	m.assignment.Store(full) // waitForAssignment pre-advance
	require.False(t, m.initialClaimsCommitted.Load(), "precondition: flag starts false")

	// A same-version apply with the pre-advanced full prev (what the
	// commit/assignment watcher computes after the pre-advance). Core must
	// override prev to empty so claims are actually written.
	require.NoError(t, m.applyAssignmentWithPrevCore(full, full))

	require.True(t, m.initialClaimsCommitted.Load(),
		"a full-set bootstrap write must latch initialClaimsCommitted")

	rh.mu.Lock()
	defer rh.mu.Unlock()
	require.Len(t, rh.applyPrevs, 1)
	require.Empty(t, rh.applyPrevs[0].Partitions,
		"during bootstrap core MUST hand the coordinator an empty prev so the full claim "+
			"set is written, not an empty diff against the pre-advanced snapshot")
}

// TestApplyCore_BootstrapVersionAdvanceOverridesPrev pins the exact scenario
// that REQUIRES the override to live in the shared apply pipeline rather than
// only in scheduleApplyRetry: a higher-version apply (V2) arriving over the
// pre-advanced V1 snapshot while no claims are committed (a startup-window
// rebalance). Without the core override, that V2 apply reads prev =
// CurrentAssignment() = V1, computes an empty prepare diff, and writes zero
// claims — and it stale-gate-drops the V1 retry. The core override forces an
// empty prev so the full set is written. A retry-only fix leaves THIS path
// (commit-/assignment-watcher applies) empty-diffing, so this test goes RED
// under retry-only placement (verified by mutation).
func TestApplyCore_BootstrapVersionAdvanceOverridesPrev(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)

	v1 := fullBootstrapAssignment()
	v2 := Assignment{Version: 2, LeaderRevision: 6, Partitions: v1.Partitions}
	m.assignment.Store(v1) // waitForAssignment pre-advanced to V1, no claims written
	require.False(t, m.initialClaimsCommitted.Load())

	// Apply the NEWER version against the pre-advanced V1 snapshot.
	require.NoError(t, m.applyAssignmentWithPrevCore(v1, v2))

	require.True(t, m.initialClaimsCommitted.Load(), "the V2 bootstrap write must latch the flag")

	rh.mu.Lock()
	defer rh.mu.Unlock()
	require.Len(t, rh.applyPrevs, 1)
	require.Empty(t, rh.applyPrevs[0].Partitions,
		"a higher-version apply over the pre-advanced snapshot during bootstrap MUST receive an "+
			"empty prev (full claim write); retry-only placement leaves this watcher path empty-diffing")
}

// TestApplyCore_AfterBootstrapKeepsPrev is the other direction of the boundary
// (feedback_test_both_directions_of_boundary): once claims ARE committed, core
// must NOT override prev, so steady-state applies stay incremental and do not
// re-issue a full-set write over already-committed claims.
func TestApplyCore_AfterBootstrapKeepsPrev(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)

	full := fullBootstrapAssignment()
	next := Assignment{
		Version:        2,
		LeaderRevision: 6,
		Partitions:     append(append([]Partition(nil), full.Partitions...), Partition{Keys: []string{"p3"}}),
	}
	m.assignment.Store(full)
	m.initialClaimsCommitted.Store(true) // claims already committed at least once

	require.NoError(t, m.applyAssignmentWithPrevCore(full, next))

	rh.mu.Lock()
	defer rh.mu.Unlock()
	require.Len(t, rh.applyPrevs, 1)
	require.Equal(t, full.Partitions, rh.applyPrevs[0].Partitions,
		"after the first claim commit core MUST use the real prev (incremental semantics), "+
			"not a forced empty prev")
}

// TestApplyCore_ConcurrentBootstrapApplies_LatchOnceNoRace pins the
// concurrent-apply safety the override relies on (AGENTS.md concurrency
// discipline). The retry goroutine and the commit/assignment watchers can all
// issue an apply for the same bootstrap version at once. applyStoreMu serializes
// them and the latch is read+written under that lock, so EXACTLY ONE apply takes
// the empty-prev bootstrap path; the rest observe the latched flag and use the
// real prev. Run under -race by the suite.
func TestApplyCore_ConcurrentBootstrapApplies_LatchOnceNoRace(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)

	full := fullBootstrapAssignment()
	m.assignment.Store(full) // pre-advance

	const racers = 8
	var wg sync.WaitGroup
	wg.Add(racers)
	for range racers {
		go func() {
			defer wg.Done()
			_ = m.applyAssignmentWithPrevCore(full, full)
		}()
	}
	wg.Wait()

	require.True(t, m.initialClaimsCommitted.Load(), "the bootstrap write must latch the flag")

	rh.mu.Lock()
	defer rh.mu.Unlock()
	require.Len(t, rh.applyPrevs, racers, "all serialized applies ran")
	empties := 0
	for _, p := range rh.applyPrevs {
		if len(p.Partitions) == 0 {
			empties++
		}
	}
	require.Equal(t, 1, empties,
		"exactly one apply may take the empty-prev bootstrap path; once it latches the flag "+
			"the rest must use the real prev (no redundant full-set re-write)")
}
