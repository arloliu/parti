package handoff

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// staleHandoffResetSpy wraps NopMetrics and counts IncClaimStaleHandoffReset
// invocations for assertion in tests. Embedding NopMetrics keeps the spy a
// drop-in replacement without re-implementing every method.
type staleHandoffResetSpy struct {
	NopMetrics
	resets atomic.Int64
}

func (s *staleHandoffResetSpy) IncClaimStaleHandoffReset() {
	s.resets.Add(1)
}

// TestTwoPhase_PreparePhase_RecoversStuckPrepareOnReacquire reproduces a
// latent bug in preparePhase: when a partition's claim is stuck at
// (owner=A, pending=B, state=prepare) — which can happen if a A->B
// reassignment ran preparePhase but the new owner's commitPhase did not
// complete (audit-driven revert, worker crash, context cancellation, slow
// consumer setup, etc.) — and worker A then re-acquires the partition,
// preparePhase short-circuits because cur.Owner == workerID and returns
// nil without resetting the stale handoff fields. commitPhase and
// stabilizePhase also no-op because neither matches the (owner=self,
// state=prepare, pending=other) shape. The claim is then permanently
// stuck at state=prepare, and the processing gate suppresses pulls on
// the partition with state_not_allowed(prepare) until something else
// writes the claim.
//
// This test seeds the stuck state, runs a full Apply as the re-acquiring
// owner, and asserts the claim converges to a clean (owner=A, state=stable)
// terminal state. On the unfixed code this test FAILS: post-Apply the
// claim is still state=prepare with pending=B. On the fixed code it
// PASSES because preparePhase (or a peer phase) recognises the stale
// handoff and resets the claim.
func TestTwoPhase_PreparePhase_RecoversStuckPrepareOnReacquire(t *testing.T) {
	t.Parallel()

	const (
		workerA = "worker-A"
		workerB = "worker-B"
		pid     = "p-stuck"
	)

	store := newMemStore()

	// Seed the stuck state directly. This models a partial A->B handoff
	// whose new-owner commit never landed. LastUpdated is "now" so the
	// opportunistic sweep does NOT mask the bug — only the preparePhase
	// transition path can recover the claim.
	stuck := Claim{
		PartitionID:  pid,
		Owner:        workerA,
		PendingOwner: workerB,
		State:        ClaimStatePrepare,
		Epoch:        2,
		LastUpdated:  time.Now().UTC(),
		TTLSeconds:   int64((30 * time.Second).Seconds()),
	}
	store.data[pid] = stuck
	store.rev[pid] = 2

	cfg := Config{
		ConsumerUpdater: nopUpdater{},
		Store:           store,
		Now:             time.Now,
		TTL:             30 * time.Second,
		SweepInterval:   0,
		MaxRetries:      3,
		BaseBackoff:     1 * time.Millisecond,
		MaxBackoff:      5 * time.Millisecond,
	}
	coord := New(cfg, true)

	// A is re-acquiring the partition. Previous assignment did NOT include
	// it (modelling the typical A -> B -> A revert path where A briefly
	// lost it in V=N+1 and gets it back in V=N+2).
	prev := types.Assignment{Version: 5, Lifecycle: "stable", Partitions: nil}
	next := types.Assignment{Version: 6, Lifecycle: "stable", Partitions: []types.Partition{{Keys: []string{pid}}}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, coord.Apply(ctx, workerA, prev, next))

	got, _, err := store.Get(ctx, pid)
	require.NoError(t, err)

	require.Equalf(t, ClaimStateStable, got.State,
		"claim must converge to stable after re-acquire, got state=%s pending=%q. "+
			"This is the production bug: preparePhase returns nil when cur.Owner==workerID "+
			"regardless of cur.State / cur.PendingOwner, leaving the claim stuck at "+
			"prepare with a stale pending owner. The processing gate then suppresses "+
			"pulls indefinitely with state_not_allowed(prepare).",
		got.State, got.PendingOwner)
	require.Equal(t, workerA, got.Owner,
		"owner must be the re-acquiring worker after Apply")
	require.Empty(t, got.PendingOwner,
		"pending owner must be cleared after re-acquire converges to stable")
	require.Greater(t, got.Epoch, stuck.Epoch,
		"epoch must advance on the recovery transition")
}

// TestTwoPhase_PreparePhase_ReacquireIdempotentOnAlreadyStable guards
// the fix from regressing the idempotent-re-Apply path: if the claim is
// already (owner=workerID, state=stable, pending=""), preparePhase must
// not gratuitously bump the epoch or transition the claim. This is the
// existing behaviour and must be preserved by the fix.
func TestTwoPhase_PreparePhase_ReacquireIdempotentOnAlreadyStable(t *testing.T) {
	t.Parallel()

	const (
		workerA = "worker-A"
		pid     = "p-idem"
	)

	store := newMemStore()

	clean := Claim{
		PartitionID: pid,
		Owner:       workerA,
		State:       ClaimStateStable,
		Epoch:       7,
		LastUpdated: time.Now().UTC(),
		TTLSeconds:  int64((30 * time.Second).Seconds()),
	}
	store.data[pid] = clean
	store.rev[pid] = 7

	cfg := Config{
		ConsumerUpdater: nopUpdater{},
		Store:           store,
		Now:             time.Now,
		TTL:             30 * time.Second,
		SweepInterval:   0,
		MaxRetries:      3,
		BaseBackoff:     1 * time.Millisecond,
		MaxBackoff:      5 * time.Millisecond,
	}
	coord := New(cfg, true)

	// prev=empty so preparePhase iterates the partition (re-acquire path).
	prev := types.Assignment{Version: 5, Partitions: nil}
	next := types.Assignment{Version: 6, Partitions: []types.Partition{{Keys: []string{pid}}}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, coord.Apply(ctx, workerA, prev, next))

	got, _, err := store.Get(ctx, pid)
	require.NoError(t, err)
	require.Equal(t, ClaimStateStable, got.State)
	require.Equal(t, workerA, got.Owner)
	require.Empty(t, got.PendingOwner)
	require.Equal(t, clean.Epoch, got.Epoch,
		"epoch must not advance when the claim is already clean stable")
}

// TestTwoPhase_PreparePhase_RecoversFromStaleCommit covers a stuck-non-stable
// shape where a previous Apply transitioned the claim past prepare (state=commit)
// but never stabilized. On re-acquire by the same owner, preparePhase must reset
// the claim to clean stable and advance the epoch.
func TestTwoPhase_PreparePhase_RecoversFromStaleCommit(t *testing.T) {
	t.Parallel()

	const (
		workerA = "worker-A"
		pid     = "p-stuck-commit"
	)

	store := newMemStore()

	stuck := Claim{
		PartitionID: pid,
		Owner:       workerA,
		State:       ClaimStateCommit,
		Epoch:       3,
		LastUpdated: time.Now().UTC(),
		TTLSeconds:  int64((30 * time.Second).Seconds()),
	}
	store.data[pid] = stuck
	store.rev[pid] = 3

	cfg := Config{
		ConsumerUpdater: nopUpdater{},
		Store:           store,
		Now:             time.Now,
		TTL:             30 * time.Second,
		SweepInterval:   0,
		MaxRetries:      3,
		BaseBackoff:     1 * time.Millisecond,
		MaxBackoff:      5 * time.Millisecond,
	}
	coord := New(cfg, true)

	prev := types.Assignment{Version: 5, Partitions: nil}
	next := types.Assignment{Version: 6, Partitions: []types.Partition{{Keys: []string{pid}}}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, coord.Apply(ctx, workerA, prev, next))

	got, _, err := store.Get(ctx, pid)
	require.NoError(t, err)
	require.Equal(t, ClaimStateStable, got.State,
		"stuck commit-state claim must converge to stable on re-acquire")
	require.Equal(t, workerA, got.Owner)
	require.Empty(t, got.PendingOwner)
	require.GreaterOrEqual(t, got.Epoch, int64(4),
		"epoch must advance past the stuck commit epoch")
}

// TestTwoPhase_PreparePhase_RecoversFromStaleAbort documents that the fix
// is defensive for any non-stable state when the owner is the re-acquiring
// worker — including the abort state.
func TestTwoPhase_PreparePhase_RecoversFromStaleAbort(t *testing.T) {
	t.Parallel()

	const (
		workerA = "worker-A"
		pid     = "p-stuck-abort"
	)

	store := newMemStore()

	stuck := Claim{
		PartitionID: pid,
		Owner:       workerA,
		State:       ClaimStateAbort,
		Epoch:       3,
		LastUpdated: time.Now().UTC(),
		TTLSeconds:  int64((30 * time.Second).Seconds()),
	}
	store.data[pid] = stuck
	store.rev[pid] = 3

	cfg := Config{
		ConsumerUpdater: nopUpdater{},
		Store:           store,
		Now:             time.Now,
		TTL:             30 * time.Second,
		SweepInterval:   0,
		MaxRetries:      3,
		BaseBackoff:     1 * time.Millisecond,
		MaxBackoff:      5 * time.Millisecond,
	}
	coord := New(cfg, true)

	prev := types.Assignment{Version: 5, Partitions: nil}
	next := types.Assignment{Version: 6, Partitions: []types.Partition{{Keys: []string{pid}}}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, coord.Apply(ctx, workerA, prev, next))

	got, _, err := store.Get(ctx, pid)
	require.NoError(t, err)
	require.Equal(t, ClaimStateStable, got.State,
		"stuck abort-state claim must converge to stable on re-acquire")
	require.Equal(t, workerA, got.Owner)
	require.Empty(t, got.PendingOwner)
	require.GreaterOrEqual(t, got.Epoch, int64(4),
		"epoch must advance past the stuck abort epoch")
}

// TestTwoPhase_StaleHandoffResetMetric asserts the new
// IncClaimStaleHandoffReset metric is emitted exactly once on the recovery
// path so operators can observe how often the race fires.
func TestTwoPhase_StaleHandoffResetMetric(t *testing.T) {
	t.Parallel()

	const (
		workerA = "worker-A"
		workerB = "worker-B"
		pid     = "p-metric"
	)

	store := newMemStore()

	stuck := Claim{
		PartitionID:  pid,
		Owner:        workerA,
		PendingOwner: workerB,
		State:        ClaimStatePrepare,
		Epoch:        2,
		LastUpdated:  time.Now().UTC(),
		TTLSeconds:   int64((30 * time.Second).Seconds()),
	}
	store.data[pid] = stuck
	store.rev[pid] = 2

	spy := &staleHandoffResetSpy{}

	cfg := Config{
		ConsumerUpdater: nopUpdater{},
		Store:           store,
		Metrics:         spy,
		Now:             time.Now,
		TTL:             30 * time.Second,
		SweepInterval:   0,
		MaxRetries:      3,
		BaseBackoff:     1 * time.Millisecond,
		MaxBackoff:      5 * time.Millisecond,
	}
	coord := New(cfg, true)

	prev := types.Assignment{Version: 5, Partitions: nil}
	next := types.Assignment{Version: 6, Partitions: []types.Partition{{Keys: []string{pid}}}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, coord.Apply(ctx, workerA, prev, next))

	require.Equal(t, int64(1), spy.resets.Load(),
		"IncClaimStaleHandoffReset must be called exactly once on the recovery path")
}

// TestTwoPhase_PreparePhase_OwnedByOtherUnchangedSemantics is a regression
// guard that ensures the fix does not redirect the stable-other-owner path:
// when the existing claim is owned by a different worker and stable, the
// re-acquiring worker must still go through NextPrepare (i.e. the existing
// cur.Owner != workerID branch is untouched).
func TestTwoPhase_PreparePhase_OwnedByOtherUnchangedSemantics(t *testing.T) {
	t.Parallel()

	const (
		workerA = "worker-A"
		workerB = "worker-B"
		pid     = "p-stable-other"
	)

	store := newMemStore()

	existing := Claim{
		PartitionID: pid,
		Owner:       workerB,
		State:       ClaimStateStable,
		Epoch:       4,
		LastUpdated: time.Now().UTC(),
		TTLSeconds:  int64((30 * time.Second).Seconds()),
	}
	store.data[pid] = existing
	store.rev[pid] = 4

	// DelayAfterPrepare is wide enough for require.Eventually to observe
	// the intermediate prepare write before commit advances the claim.
	cfg := Config{
		ConsumerUpdater:   nopUpdater{},
		Store:             store,
		Now:               time.Now,
		TTL:               30 * time.Second,
		SweepInterval:     0,
		MaxRetries:        3,
		BaseBackoff:       1 * time.Millisecond,
		MaxBackoff:        5 * time.Millisecond,
		DelayAfterPrepare: 200 * time.Millisecond,
	}
	coord := New(cfg, true)

	prev := types.Assignment{Version: 5, Partitions: nil}
	next := types.Assignment{Version: 6, Partitions: []types.Partition{{Keys: []string{pid}}}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = coord.Apply(ctx, workerA, prev, next)
		close(done)
	}()

	// During DelayAfterPrepare we should observe the prepare state with
	// the new owner recorded as pendingOwner — proves NextPrepare fired
	// on the cur.Owner != workerID branch. Poll the store rather than
	// sleeping (.agents/rules/300-testing.md forbids time.Sleep for
	// async synchronization).
	var mid Claim
	require.Eventually(t, func() bool {
		c, _, gerr := store.Get(ctx, pid)
		if gerr != nil {
			return false
		}
		if c.State != ClaimStatePrepare {
			return false
		}
		if c.Owner != workerB || c.PendingOwner != workerA {
			return false
		}
		mid = c

		return true
	}, 1*time.Second, 1*time.Millisecond,
		"expected intermediate prepare state with owner=B pending=A during DelayAfterPrepare")

	require.Equal(t, ClaimStatePrepare, mid.State,
		"the existing other-owner branch must fire NextPrepare; fix must not redirect this path")
	require.Equal(t, workerB, mid.Owner, "owner unchanged until commit")
	require.Equal(t, workerA, mid.PendingOwner, "pendingOwner must be the re-acquiring worker")

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("apply did not finish: %v", ctx.Err())
	}

	got, _, err := store.Get(ctx, pid)
	require.NoError(t, err)
	require.Equal(t, ClaimStateStable, got.State)
	require.Equal(t, workerA, got.Owner)
	require.Empty(t, got.PendingOwner)
}

// casConflictOnceStore wraps a memStore and forces the first PutIfEpoch
// call for a target partition to return ErrEpochMismatch, after which it
// delegates to the underlying store. This simulates a single CAS conflict
// observed by the preparePhase reset path so we can assert that
// IncClaimStaleHandoffReset is emitted exactly once even when the
// updateClaim CAS loop re-invokes the transform.
type casConflictOnceStore struct {
	inner    *memStore
	targetID string
	failed   atomic.Bool // set after the one synthetic conflict is delivered
}

var _ ClaimStore = (*casConflictOnceStore)(nil)

func (s *casConflictOnceStore) Get(ctx context.Context, partitionID string) (Claim, uint64, error) {
	return s.inner.Get(ctx, partitionID)
}

func (s *casConflictOnceStore) PutIfEpoch(ctx context.Context, partitionID string, expectedEpoch int64, next Claim) (uint64, error) {
	if partitionID == s.targetID && s.failed.CompareAndSwap(false, true) {
		return 0, ErrEpochMismatch
	}

	return s.inner.PutIfEpoch(ctx, partitionID, expectedEpoch, next)
}

func (s *casConflictOnceStore) ListKeys(ctx context.Context) ([]string, error) {
	return s.inner.ListKeys(ctx)
}

func (s *casConflictOnceStore) Delete(ctx context.Context, partitionID string, revision uint64) error {
	return s.inner.Delete(ctx, partitionID, revision)
}

// TestTwoPhase_StaleHandoffResetMetric_NoOvercountOnCASRetry guards the
// post-impl-review finding that
// when preparePhase resets a stuck handoff and the underlying CAS conflicts
// once before succeeding, IncClaimStaleHandoffReset must be emitted exactly
// once for the single durable reset — not once per transform invocation.
//
// Pre-fix (defd9ac) the metric is incremented inside the updateClaim
// transform closure, so each retry would bump the counter. With the
// after-success emission in place the counter reflects exactly the number
// of CAS-successful resets.
func TestTwoPhase_StaleHandoffResetMetric_NoOvercountOnCASRetry(t *testing.T) {
	t.Parallel()

	const (
		workerA = "worker-A"
		workerB = "worker-B"
		pid     = "p-metric-cas"
	)

	inner := newMemStore()
	stuck := Claim{
		PartitionID:  pid,
		Owner:        workerA,
		PendingOwner: workerB,
		State:        ClaimStatePrepare,
		Epoch:        2,
		LastUpdated:  time.Now().UTC(),
		TTLSeconds:   int64((30 * time.Second).Seconds()),
	}
	inner.data[pid] = stuck
	inner.rev[pid] = 2

	store := &casConflictOnceStore{inner: inner, targetID: pid}
	spy := &staleHandoffResetSpy{}

	cfg := Config{
		ConsumerUpdater: nopUpdater{},
		Store:           store,
		Metrics:         spy,
		Now:             time.Now,
		TTL:             30 * time.Second,
		SweepInterval:   0,
		MaxRetries:      3,
		BaseBackoff:     1 * time.Millisecond,
		MaxBackoff:      5 * time.Millisecond,
	}
	coord := New(cfg, true)

	prev := types.Assignment{Version: 5, Partitions: nil}
	next := types.Assignment{Version: 6, Partitions: []types.Partition{{Keys: []string{pid}}}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, coord.Apply(ctx, workerA, prev, next))

	// Verify the synthetic conflict actually fired (so the updateClaim
	// transform was invoked at least twice). Without this we'd just be
	// repeating TestTwoPhase_StaleHandoffResetMetric.
	require.True(t, store.failed.Load(),
		"expected the casConflictOnceStore to inject one CAS conflict for the reset write")

	// The single durable reset must produce exactly one metric increment
	// regardless of how many times the transform closure ran.
	require.Equal(t, int64(1), spy.resets.Load(),
		"IncClaimStaleHandoffReset must be emitted exactly once per durable reset, "+
			"even when updateClaim retries on CAS conflict")

	// And the reset must have actually landed: claim is now clean stable.
	got, _, err := store.Get(ctx, pid)
	require.NoError(t, err)
	require.Equal(t, ClaimStateStable, got.State,
		"the reset write must succeed on retry after the CAS conflict")
	require.Equal(t, workerA, got.Owner)
	require.Empty(t, got.PendingOwner)
	require.Greater(t, got.Epoch, stuck.Epoch,
		"epoch must advance on the recovery transition")
}
