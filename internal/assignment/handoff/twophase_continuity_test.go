package handoff

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// --- B4: missed-version continuity fail-open ---
//
// previous is always sourced from the worker's own last COMMITTED
// assignment in production (manager_assignment.go's
// committedAssignmentOrEmpty), and retry/stash coalescing can advance
// `next` past more than one version relative to it. Both reproducers below
// (from tmp/diff-restricted-walks-design.md §2.1, round-1 P0) model that
// gap directly at the coordinator level: previous stays pinned at V1 while
// next jumps to V3, even though an intermediate V2 was actually observed
// and committed by (or against) another worker's claim in the same shared
// store.

// TestB4_Sequence1_AtoBtoA_AcrossCoalescedVersion reproduces round-1's
// first counterexample: A holds p at V1, B takes stable ownership at V2,
// then V3 hands p back to A — but A's local `previous` is still V1 (V2 was
// coalesced/missed by A, e.g. because A's own V2 removal attempt returned
// ErrRemovalPending and was superseded by V3 before it could retry). The
// V1->V3 diff excludes p (present in both), so a diff-restricted prepare
// never runs for it. Today's unconditional full commit/stabilize walk does
// not heal this either: NextCommit only promotes a non-empty PendingOwner,
// so commit produces state=commit, Owner=B, and A's stabilize skips
// because the claim's owner is still B.
func TestB4_Sequence1_AtoBtoA_AcrossCoalescedVersion(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	coord := New(Config{
		Store:           store,
		ConsumerUpdater: nopUpdater{},
		Now:             time.Now,
		TTL:             time.Minute,
		SweepInterval:   time.Hour, // keep the opportunistic sweep out of this reproducer
		MaxRetries:      3,
		BaseBackoff:     time.Millisecond,
		MaxBackoff:      2 * time.Millisecond,
	}, true)

	p := types.Partition{Keys: []string{"p"}}
	ctx := t.Context()

	// 1. A commits V1 containing p.
	v1 := types.Assignment{Version: 1, Partitions: []types.Partition{p}}
	require.NoError(t, coord.Apply(ctx, "A", types.Assignment{}, v1))

	// 2. B completes V2 -> stable claim Owner=B. B's own `previous` is not
	// what this reproducer pins; only the resulting claim shape is.
	v2 := types.Assignment{Version: 2, Partitions: []types.Partition{p}}
	require.NoError(t, coord.Apply(ctx, "B", types.Assignment{}, v2))

	setup, _, err := store.Get(ctx, p.SubjectKey())
	require.NoError(t, err)
	require.Equal(t, "B", setup.Owner, "setup: B must be stable owner before A's V3")
	require.Equal(t, ClaimStateStable, setup.State)

	// 3. A applies V3 with previous=V1 — a Version gap (A missed/coalesced
	// past V2), exactly the shape production's committedAssignment-based
	// diffing produces when a retry or stash coalesces to a newer target.
	v3 := types.Assignment{Version: 3, Partitions: []types.Partition{p}}
	require.NoError(t, coord.Apply(ctx, "A", v1, v3))

	got, _, err := store.Get(ctx, p.SubjectKey())
	require.NoError(t, err)
	require.Equal(t, ClaimStateStable, got.State,
		"p must converge to stable after A's V3 re-acquire across the coalesced V2")
	require.Equal(t, "A", got.Owner,
		"p must end owned by A: the V1->V3 diff excludes p, so only a fail-open full "+
			"prepare walk records A as PendingOwner for the untouched commit/stabilize "+
			"phases to promote")
}

// TestB4_Sequence2_ReapAndReAdd_AcrossMissedRemoval reproduces round-1's
// second counterexample: A holds p at V1; the claim is then deleted out of
// band (models the sweep reaper firing after A missed an intermediate
// removal version), and V3 re-adds p to A with `previous` still V1 (the
// removal/re-add pair was coalesced past). The V1->V3 diff again excludes p
// (present in both), so no prepare recreates the missing claim; today's
// commit/stabilize walk also no-ops on a nil claim.
func TestB4_Sequence2_ReapAndReAdd_AcrossMissedRemoval(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	coord := New(Config{
		Store:           store,
		ConsumerUpdater: nopUpdater{},
		Now:             time.Now,
		TTL:             time.Minute,
		SweepInterval:   time.Hour,
		MaxRetries:      3,
		BaseBackoff:     time.Millisecond,
		MaxBackoff:      2 * time.Millisecond,
	}, true)

	p := types.Partition{Keys: []string{"p"}}
	ctx := t.Context()

	// A commits V1 with p.
	v1 := types.Assignment{Version: 1, Partitions: []types.Partition{p}}
	require.NoError(t, coord.Apply(ctx, "A", types.Assignment{}, v1))

	setup, setupRev, err := store.Get(ctx, p.SubjectKey())
	require.NoError(t, err)
	require.NotZero(t, setupRev, "setup: claim must exist after V1")
	require.Equal(t, "A", setup.Owner)

	// The source removes p in a version A misses; the leader's sweep reaps
	// the now-orphaned stable claim. Modeled directly as an out-of-band
	// delete rather than driven through the sweep's TTL/grace machinery,
	// which is not what this reproducer is pinning.
	require.NoError(t, store.Delete(ctx, p.SubjectKey(), setupRev))
	_, revAfterDelete, err := store.Get(ctx, p.SubjectKey())
	require.NoError(t, err)
	require.Zero(t, revAfterDelete, "setup: claim must be gone before A's V3")

	// A applies V3 re-adding p, with previous=V1 — the removal was
	// coalesced past, exactly as production's committedAssignment-based
	// diffing would produce.
	v3 := types.Assignment{Version: 3, Partitions: []types.Partition{p}}
	require.NoError(t, coord.Apply(ctx, "A", v1, v3))

	got, rev, err := store.Get(ctx, p.SubjectKey())
	require.NoError(t, err)
	require.NotZero(t, rev,
		"p's claim must be recreated: the V1->V3 diff excludes p, so only a fail-open "+
			"full prepare walk's NewInitialClaim recreates the missing claim")
	require.Equal(t, ClaimStateStable, got.State)
	require.Equal(t, "A", got.Owner)
}

// --- transitionVersionAdjacent: pure-function table test ---

func TestTransitionVersionAdjacent_Table(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		previous types.Assignment
		next     types.Assignment
		want     bool
	}{
		{
			name:     "consecutive is continuous",
			previous: types.Assignment{Version: 5},
			next:     types.Assignment{Version: 6},
			want:     true,
		},
		{
			name:     "zero-value start is continuous",
			previous: types.Assignment{},
			next:     types.Assignment{Version: 1},
			want:     true,
		},
		{
			name:     "gap (coalesced version) fails open",
			previous: types.Assignment{Version: 1},
			next:     types.Assignment{Version: 3},
			want:     false,
		},
		{
			name:     "equality (same-version redelivery) fails open",
			previous: types.Assignment{Version: 4},
			next:     types.Assignment{Version: 4},
			want:     false,
		},
		{
			name:     "regression fails open",
			previous: types.Assignment{Version: 7},
			next:     types.Assignment{Version: 6},
			want:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, transitionVersionAdjacent(tc.previous, tc.next))
		})
	}
}

// getCountingStore wraps a *memStore and counts ClaimStore.Get calls per
// partition ID, so a test can distinguish "touched by prepare" (3 Gets:
// prepare+commit+stabilize) from "touched only by commit/stabilize" (2
// Gets) without the opportunistic Apply-origin sweep's own ListKeys+Get
// traffic conflating the count (see the row-warmup helper below).
type getCountingStore struct {
	inner *memStore

	mu   sync.Mutex
	gets map[string]int
}

var _ ClaimStore = (*getCountingStore)(nil)

func newGetCountingStore(inner *memStore) *getCountingStore {
	return &getCountingStore{inner: inner, gets: make(map[string]int)}
}

func (s *getCountingStore) Get(ctx context.Context, pid string) (Claim, uint64, error) {
	s.mu.Lock()
	s.gets[pid]++
	s.mu.Unlock()

	return s.inner.Get(ctx, pid)
}

func (s *getCountingStore) PutIfEpoch(ctx context.Context, pid string, expectedEpoch int64, next Claim) (uint64, error) {
	return s.inner.PutIfEpoch(ctx, pid, expectedEpoch, next)
}

func (s *getCountingStore) ListKeys(ctx context.Context) ([]string, error) {
	return s.inner.ListKeys(ctx)
}

func (s *getCountingStore) Delete(ctx context.Context, pid string, revision uint64) error {
	return s.inner.Delete(ctx, pid, revision)
}

func (s *getCountingStore) count(pid string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.gets[pid]
}

func (s *getCountingStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets = make(map[string]int)
}

// newContinuityTestCoordinator builds a *twoPhaseCoordinator over a fixed
// clock and an hour-long SweepInterval, then consumes the first-call
// opportunistic sweep with a throwaway empty Apply while the store is still
// empty (so it costs zero Gets). Because the clock never advances, every
// subsequent Apply's sweep throttle sees now-lastSweep==0 < SweepInterval
// and is skipped — isolating the returned getCountingStore's counts to
// prepare/commit/stabilize phase reads only, matching round-1/round-2's
// "suppress or separately account for the initial sweep" resolution.
func newContinuityTestCoordinator(t *testing.T) (*twoPhaseCoordinator, *getCountingStore) {
	t.Helper()

	fixed := time.Now()
	counting := newGetCountingStore(newMemStore())
	cfg := Config{
		Store:           counting,
		ConsumerUpdater: nopUpdater{},
		Now:             func() time.Time { return fixed },
		TTL:             time.Minute,
		SweepInterval:   time.Hour,
		MaxRetries:      3,
		BaseBackoff:     time.Millisecond,
		MaxBackoff:      2 * time.Millisecond,
	}
	c := New(cfg, true)
	tp, ok := c.(*twoPhaseCoordinator)
	require.True(t, ok, "expected a *twoPhaseCoordinator")

	require.NoError(t, tp.Apply(t.Context(), "warmup", types.Assignment{}, types.Assignment{}))
	counting.reset()

	return tp, counting
}

// TestTransitionVersionAdjacent_PrepareWalkSet pins the Apply-level consequence
// of transitionVersionAdjacent: a continuous Apply's prepare only touches gained
// keys (an already-stable, self-owned key sees no prepare Get — 2 total,
// commit+stabilize only), while a discontinuous Apply's prepare walks every
// key in next, including ones already present (byte-identical) in
// previous — 3 Gets for every key, gained or not.
func TestTransitionVersionAdjacent_PrepareWalkSet(t *testing.T) {
	t.Parallel()

	unchanged := types.Partition{Keys: []string{"unchanged"}}
	gained := types.Partition{Keys: []string{"gained"}}

	t.Run("continuous trusts the diff", func(t *testing.T) {
		t.Parallel()

		tp, counting := newContinuityTestCoordinator(t)
		ctx := t.Context()

		// Seed "unchanged" as already stable, owned by A.
		seed := NewInitialClaim(unchanged.SubjectKey(), "A", time.Now(), time.Minute)
		counting.inner.data[unchanged.SubjectKey()] = seed
		counting.inner.rev[unchanged.SubjectKey()] = 1

		previous := types.Assignment{Version: 1, Partitions: []types.Partition{unchanged}}
		next := types.Assignment{Version: 2, Partitions: []types.Partition{unchanged, gained}}
		require.True(t, transitionVersionAdjacent(previous, next), "test precondition: must be continuous")

		require.NoError(t, tp.Apply(ctx, "A", previous, next))

		require.Equal(t, 2, counting.count(unchanged.SubjectKey()),
			"unchanged key must be touched only by commit+stabilize, not prepare")
		require.Equal(t, 3, counting.count(gained.SubjectKey()),
			"gained key must be touched by prepare+commit+stabilize")
	})

	t.Run("gap fails open to a full walk", func(t *testing.T) {
		t.Parallel()

		tp, counting := newContinuityTestCoordinator(t)
		ctx := t.Context()

		seed := NewInitialClaim(unchanged.SubjectKey(), "A", time.Now(), time.Minute)
		counting.inner.data[unchanged.SubjectKey()] = seed
		counting.inner.rev[unchanged.SubjectKey()] = 1

		// previous carries "unchanged" byte-identically, but the Version
		// gap (1 -> 3) means the diff cannot be trusted.
		previous := types.Assignment{Version: 1, Partitions: []types.Partition{unchanged}}
		next := types.Assignment{Version: 3, Partitions: []types.Partition{unchanged, gained}}
		require.False(t, transitionVersionAdjacent(previous, next), "test precondition: must be discontinuous")

		require.NoError(t, tp.Apply(ctx, "A", previous, next))

		require.Equal(t, 3, counting.count(unchanged.SubjectKey()),
			"discontinuous Apply must full-walk prepare even for a key present in previous")
		require.Equal(t, 3, counting.count(gained.SubjectKey()),
			"gained key must still be touched by prepare+commit+stabilize")
	})
}

// TestTransitionVersionAdjacent_DebounceGapPin prices in the routine-cost case
// the design note calls out: a debounce-coalesced delivery whose content is
// completely unchanged (no gained/lost keys, next.Version jumps by more
// than one) still runs the full prepare walk — every key touched, every
// prepare write a no-op — rather than the zero-prepare-Get diffed path a
// continuous Apply would take.
func TestTransitionVersionAdjacent_DebounceGapPin(t *testing.T) {
	t.Parallel()

	tp, counting := newContinuityTestCoordinator(t)
	ctx := t.Context()

	p1 := types.Partition{Keys: []string{"p1"}}
	p2 := types.Partition{Keys: []string{"p2"}}
	for _, p := range []types.Partition{p1, p2} {
		seed := NewInitialClaim(p.SubjectKey(), "A", time.Now(), time.Minute)
		counting.inner.data[p.SubjectKey()] = seed
		counting.inner.rev[p.SubjectKey()] = 1
	}

	// Debounce coalesced V2..V4 away: content unchanged, Version jumps 1->4.
	previous := types.Assignment{Version: 1, Partitions: []types.Partition{p1, p2}}
	next := types.Assignment{Version: 4, Partitions: []types.Partition{p1, p2}}
	require.False(t, transitionVersionAdjacent(previous, next), "test precondition: debounce gap must be discontinuous")

	require.NoError(t, tp.Apply(ctx, "A", previous, next))

	for _, p := range []types.Partition{p1, p2} {
		require.Equal(t, 3, counting.count(p.SubjectKey()),
			"debounce-gap Apply must full-walk prepare for every key even though content is unchanged")

		got, _, err := counting.Get(ctx, p.SubjectKey())
		require.NoError(t, err)
		require.Equal(t, ClaimStateStable, got.State, "no-op prepare must leave the claim clean stable")
		require.Equal(t, "A", got.Owner)
	}
}

// --- known-open hazard: stale cross-worker retry mutates a claim ---

// TestStaleCrossWorkerRetry_StealsClaim pins a KNOWN-OPEN hazard (round-2
// P0-2, tmp/diff-restricted-walks_v2_review.md, "Adjacent worker
// assignments do not fence late claim mutations"): claims carry no
// assignment Version/commit identity, so an OLDER apply from a DIFFERENT
// worker is admitted by the coordinator regardless of
// transitionVersionAdjacent. The check cannot see this shape because it is
// evaluated per-worker: B's own transition (empty -> V1) is locally
// adjacent, even though a fresher global commit (A's V2) already moved the
// partition elsewhere before B's stale retry landed.
//
// Sequence:
//  1. A applies V2 for partition p (previous empty) -> stable, Owner=A.
//  2. B applies V1 (previous empty) containing p -- a STALE assignment from
//     BEFORE A's ownership (models a retry of an earlier failed/delayed
//     apply that B is only now completing).
//
// documented current behavior (round-2 P0-2); claims carry no commit
// identity to fence this; deferred to the fence project. When the fence
// lands this test must flip.
func TestStaleCrossWorkerRetry_StealsClaim(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	coord := New(Config{
		Store:           store,
		ConsumerUpdater: nopUpdater{},
		Now:             time.Now,
		TTL:             time.Minute,
		SweepInterval:   time.Hour, // keep the opportunistic sweep out of this reproducer
		MaxRetries:      3,
		BaseBackoff:     time.Millisecond,
		MaxBackoff:      2 * time.Millisecond,
	}, true)

	p := types.Partition{Keys: []string{"p"}}
	ctx := t.Context()

	// 1. A applies V2 for p; previous empty (A's first apply).
	v2 := types.Assignment{Version: 2, Partitions: []types.Partition{p}}
	require.NoError(t, coord.Apply(ctx, "A", types.Assignment{}, v2))

	setup, _, err := store.Get(ctx, p.SubjectKey())
	require.NoError(t, err)
	require.Equal(t, "A", setup.Owner, "setup: A must be stable owner before B's stale V1 retry")
	require.Equal(t, ClaimStateStable, setup.State)

	// 2. B applies V1 -- a STALE assignment from before A's ownership. B's
	// own previous is empty (B never locally stored anything before this
	// retry), so the coordinator sees a locally well-formed, adjacent
	// transition and has no way to know a fresher global commit (A's V2)
	// already moved p elsewhere.
	v1 := types.Assignment{Version: 1, Partitions: []types.Partition{p}}
	applyErr := coord.Apply(ctx, "B", types.Assignment{}, v1)

	// 3. Assert current behavior: B's Apply succeeds and the claim ends
	// stable Owner=B -- the stale retry STOLE the claim from A.
	require.NoError(t, applyErr, "documented current behavior: the coordinator has no "+
		"cross-worker commit-identity fence, so a stale apply is not rejected")

	got, _, err := store.Get(ctx, p.SubjectKey())
	require.NoError(t, err)
	require.Equal(t, ClaimStateStable, got.State)
	require.Equal(t, "B", got.Owner,
		"documented current behavior (round-2 P0-2); claims carry no commit identity to "+
			"fence this; deferred to the fence project. When the fence lands this test must flip.")
}
