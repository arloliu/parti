package parti

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// casClaimStore is a real (non-no-op) in-memory handoff.ClaimStore. Unlike the
// stub fakeClaimStore in manager_handoff_guard_test.go (whose PutIfEpoch is a
// no-op), this one actually persists writes, so a REAL twoPhaseCoordinator sweep
// mutates the stored claim instead of leaving the seeded value untouched — the
// stub would make the sweep-reset assertions below vacuous. It mirrors the
// handoff package's test memStore semantics
// (internal/assignment/handoff/claim_test.go). The epoch-gated CAS branches
// (create, epoch-mismatch) are implemented for fidelity but are NOT contended in
// this single-writer test; production contention is covered by the handoff
// package's own CAS tests.
type casClaimStore struct {
	mu   sync.Mutex
	data map[string]handoff.Claim
	rev  map[string]uint64
}

var _ handoff.ClaimStore = (*casClaimStore)(nil)

func newCASClaimStore() *casClaimStore {
	return &casClaimStore{data: make(map[string]handoff.Claim), rev: make(map[string]uint64)}
}

func (s *casClaimStore) seed(pid string, claim handoff.Claim, rev uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[pid] = claim
	s.rev[pid] = rev
}

func (s *casClaimStore) Get(_ context.Context, partitionID string) (handoff.Claim, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.data[partitionID]
	if !ok {
		return handoff.Claim{}, 0, nil
	}

	return c, s.rev[partitionID], nil
}

var errCASEpochMismatch = errors.New("cas claim store: epoch mismatch")

func (s *casClaimStore) PutIfEpoch(_ context.Context, partitionID string, expectedEpoch int64, next handoff.Claim) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.data[partitionID]
	if !ok {
		if expectedEpoch != 0 {
			return 0, errCASEpochMismatch
		}
		s.data[partitionID] = next
		s.rev[partitionID] = 1

		return s.rev[partitionID], nil
	}
	if cur.Epoch != expectedEpoch {
		return 0, errCASEpochMismatch
	}
	s.data[partitionID] = next
	s.rev[partitionID]++

	return s.rev[partitionID], nil
}

func (s *casClaimStore) ListKeys(_ context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.data))
	for k := range s.data {
		out = append(out, k)
	}

	return out, nil
}

func (s *casClaimStore) Delete(_ context.Context, partitionID string, revision uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.rev[partitionID]
	if !ok || cur != revision {
		return errors.New("revision mismatch")
	}
	delete(s.data, partitionID)
	delete(s.rev, partitionID)

	return nil
}

// TestGuardHandoffRemoval_GainingWorkerDeath_Liveness pins the safety/liveness
// boundary documented in Task 4 of the G4 fix plan
// (docs/plans/auto-healing-gap-closure/02-g4-handoff-rebalance-fix-plan.md).
//
// Unlike TestGuardHandoffRemoval — which seeds a hand-built claim shape into a
// stub store and only proves the guard's per-state decision — this test wires
// the REAL components on both sides of the seam: a real twoPhaseCoordinator
// runs its real opportunistic sweep, and the resulting claim it actually writes
// is fed to the real Manager.guardHandoffRemoval. That seam (sweep output →
// guard input) is exactly what isolated unit tests cannot exercise.
//
// Scenario (gaining worker dies mid-handoff):
//  1. A transfer left the claim at {owner: OLD, pendingOwner: NEW, prepare} and
//     the gaining worker NEW then died, so the claim expired.
//  2. The real sweep resets the expired non-stable claim to
//     {owner: OLD, state: stable, pendingOwner: ""} (it never deletes a stable
//     claim for a still-owned partition — twophase.go maybeSweepClaims).
//  3. The current commit still assigns the partition AWAY from OLD.
//
// The boundary this asserts (both directions, per the test-both-directions
// discipline):
//   - SAFETY: the guard BLOCKS OLD's removal — OLD must not silently drop a
//     partition no live worker has committed.
//   - LIVENESS (negative space): scheduleApplyRetry replays the SAME failed
//     assignment; against a dead gaining worker the guard must keep blocking and
//     must NOT spuriously converge on its own.
//   - CONVERGENCE only on a NEW signal: a later assignment that returns the
//     partition to OLD converges (no removal), and an assignment whose claim
//     proves a DIFFERENT live worker committed releases OLD's removal.
func TestGuardHandoffRemoval_GainingWorkerDeath_Liveness(t *testing.T) {
	t.Parallel()

	const (
		oldOwner = "worker-old"
		newOwner = "worker-new"
		pid      = "p1"
		ver      = int64(7)
	)
	ttl := time.Second
	now := time.Now().UTC()
	ctx := context.Background()

	store := newCASClaimStore()

	// (1) A transfer ran preparePhase (owner stays OLD, pendingOwner=NEW) but
	// NEW's commit never landed because NEW died; the claim then expired.
	store.seed(pid, handoff.Claim{
		PartitionID:  pid,
		Owner:        oldOwner,
		PendingOwner: newOwner,
		State:        handoff.ClaimStatePrepare,
		Epoch:        2,
		LastUpdated:  now.Add(-2 * ttl), // expired
		TTLSeconds:   int64(ttl.Seconds()),
	}, 1)

	// (2) Run the REAL sweep via a real two-phase coordinator. SweepInterval=0
	// forces a sweep on every Apply; an empty→empty assignment performs no
	// prepare/commit work, so only the opportunistic sweep touches the store.
	coord := handoff.New(handoff.Config{
		Store:         store,
		Now:           func() time.Time { return now },
		SweepInterval: 0,
		TTL:           ttl,
		MaxRetries:    1,
		BaseBackoff:   time.Millisecond,
		MaxBackoff:    2 * time.Millisecond,
	}, true)
	require.NoError(t, coord.Apply(ctx, oldOwner, types.Assignment{}, types.Assignment{}))

	// The sweep must have produced the gaining-worker-death shape.
	swept, _, err := store.Get(ctx, pid)
	require.NoError(t, err)
	require.Equal(t, handoff.ClaimStateStable, swept.State,
		"sweep must reset the expired prepare claim to stable")
	require.Equal(t, oldOwner, swept.Owner,
		"sweep must leave the still-owning OLD worker as owner (no deletion)")
	require.Empty(t, swept.PendingOwner,
		"sweep must clear the dead gaining worker as pending owner")

	// (3) The current commit still assigns pid away from OLD: prev owns pid,
	// next removes it, and pid is still present in the current commit batch (a
	// transfer, not a source deletion).
	m, rm := newGuardManager(store, ver, pid)
	prev := Assignment{Version: ver, Partitions: []types.Partition{part(pid)}}
	next := Assignment{Version: ver, Partitions: nil}

	// SAFETY: the guard reads the REAL swept claim ({owner: OLD, stable}) and
	// blocks — OLD keeps serving rather than dropping an uncommitted transfer.
	require.ErrorIs(t, m.guardHandoffRemoval(ctx, oldOwner, prev, next), handoff.ErrRemovalPending,
		"guard must block removal of a partition whose transfer the dead gaining worker never committed")
	require.Equal(t, 1, rm.count())

	// LIVENESS (negative space): the guard re-reads the LIVE claim on every apply
	// attempt and keeps blocking while the transfer stays uncommitted. Mutate the
	// claim between calls to a DIFFERENT still-uncommitted shape — a fresh
	// different-owner prepare, as if the gaining worker briefly retried prepare
	// before dying again — and require removal to STILL block, recording the
	// deferral again (rm.count()==2). This block-on-a-changed-claim, together
	// with the committed-claim ALLOW in convergence path 2 below, proves the
	// guard evaluates the CURRENT claim per call rather than caching its first
	// verdict (a cached block could never later flip to ALLOW). That per-call
	// re-evaluation is why scheduleApplyRetry — which replays the apply,
	// re-reading the live claim each tick (manager_assignment.go) — cannot
	// self-converge against an uncommitted gaining worker.
	store.seed(pid, handoff.Claim{
		PartitionID:  pid,
		Owner:        newOwner,
		PendingOwner: oldOwner,
		State:        handoff.ClaimStatePrepare,
		Epoch:        3,
	}, 10)
	require.ErrorIs(t, m.guardHandoffRemoval(ctx, oldOwner, prev, next), handoff.ErrRemovalPending,
		"a retry observing a fresh uncommitted (different-owner prepare) claim must keep blocking "+
			"and record the deferral again")
	require.Equal(t, 2, rm.count())

	// CONVERGENCE path 1 — a later assignment returns pid to OLD: nothing is
	// removed, so the guard short-circuits (no removal) and the worker converges.
	require.NoError(t, m.guardHandoffRemoval(ctx, oldOwner, prev, prev),
		"a later assignment returning the partition to OLD must converge (no removal to guard)")

	// CONVERGENCE path 2 — reassignment to a LIVE worker: once a different
	// worker's claim proves a committed transfer, OLD's removal is released.
	store.seed(pid, handoff.Claim{Owner: newOwner, State: handoff.ClaimStateCommit, Epoch: 4}, 11)
	require.NoError(t, m.guardHandoffRemoval(ctx, oldOwner, prev, next),
		"once a live worker has committed the claim, OLD's removal must be allowed")
}
