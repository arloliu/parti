package handoff

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// --- Helpers specific to two-phase tests ---

// nopUpdater is a no-op ConsumerUpdater used to drive two-phase flow without side effects.
type nopUpdater struct{}

func (nopUpdater) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []types.Partition) error {
	return nil
}

// mockUpdater counts invocations and records last inputs for coordination tests.
type mockUpdater struct {
	calls    atomic.Int64
	lastID   atomic.Value // string
	lastPart atomic.Value // []types.Partition
}

func (m *mockUpdater) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []types.Partition) error {
	m.calls.Add(1)
	m.lastID.Store(workerID)
	m.lastPart.Store(partitions)
	return nil
}

// flakyStore simulates CAS conflicts for a configurable number of attempts.
type flakyStore struct {
	failUntil int
	calls     int
}

func (f *flakyStore) Get(ctx context.Context, partitionID string) (Claim, uint64, error) {
	// Simulate no existing claim to exercise create path first.
	// For updateClaim retry logic, we need to return a valid claim if we want to test updates.
	// But for the current test cases (create path), returning empty is fine.
	return Claim{}, 0, nil
}

func (f *flakyStore) PutIfEpoch(ctx context.Context, partitionID string, expectedEpoch int64, next Claim) (uint64, error) {
	f.calls++
	if f.calls <= f.failUntil {
		return 0, ErrEpochMismatch
	}
	return uint64(f.calls), nil
}

func (f *flakyStore) ListKeys(ctx context.Context) ([]string, error) { return nil, nil }

// consumerUpdaterFunc is a test helper to satisfy ConsumerUpdater.
type consumerUpdaterFunc func(ctx context.Context, workerID string, partitions []types.Partition) error

func (f consumerUpdaterFunc) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []types.Partition) error {
	return f(ctx, workerID, partitions)
}

// --- Two-phase tests ---

// Verifies that configurable delays expose prepare -> commit -> stable transitions externally.
func TestTwoPhase_DelaysExposeIntermediateStates(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	worker := "w1"
	pid := "p1"

	// Seed an existing stable claim owned by a different worker so the
	// incoming worker will go through prepare -> commit -> stable.
	seed := Claim{
		PartitionID: pid,
		Owner:       "w0",
		State:       ClaimStateStable,
		Epoch:       1,
		LastUpdated: time.Now().UTC(),
		TTLSeconds:  int64((30 * time.Second).Seconds()),
	}
	store.data[pid] = seed
	store.rev[pid] = 1

	cfg := Config{
		ConsumerUpdater:   nopUpdater{},
		Store:             store,
		Now:               time.Now,
		TTL:               30 * time.Second,
		SweepInterval:     0,
		MaxRetries:        2,
		BaseBackoff:       1 * time.Millisecond,
		MaxBackoff:        2 * time.Millisecond,
		DelayAfterPrepare: 30 * time.Millisecond,
		DelayBeforeStable: 30 * time.Millisecond,
	}
	coord := New(cfg, true)

	prev := types.Assignment{Version: 1, Lifecycle: "stable", Partitions: nil}
	next := types.Assignment{Version: 2, Lifecycle: "stable", Partitions: []types.Partition{{Keys: []string{pid}}}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = coord.Apply(ctx, worker, prev, next)
		close(done)
	}()

	// Shortly after Apply starts, we should observe prepare.
	time.Sleep(10 * time.Millisecond)
	cur, _, err := store.Get(ctx, pid)
	require.NoError(t, err)
	require.Equal(t, ClaimStatePrepare, cur.State, "should enter prepare state")
	require.Equal(t, worker, cur.PendingOwner)
	require.Equal(t, "w0", cur.Owner) // owner unchanged until commit

	// After DelayAfterPrepare, commit should be visible with new owner.
	time.Sleep(cfg.DelayAfterPrepare + 10*time.Millisecond)
	cur, _, err = store.Get(ctx, pid)
	require.NoError(t, err)
	require.Equal(t, ClaimStateCommit, cur.State, "should enter commit state")
	require.Equal(t, worker, cur.Owner, "owner should switch on commit")
	require.Empty(t, cur.PendingOwner)

	// After DelayBeforeStable, final stable should be visible.
	time.Sleep(cfg.DelayBeforeStable + 10*time.Millisecond)
	cur, _, err = store.Get(ctx, pid)
	require.NoError(t, err)
	require.Equal(t, ClaimStateStable, cur.State, "should finalize to stable state")
	require.Equal(t, worker, cur.Owner)

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("apply did not finish: %v", ctx.Err())
	}
}

func TestTwoPhaseCoordinator_ApplyDelegates(t *testing.T) {
	up := &mockUpdater{}
	coord := New(Config{ConsumerUpdater: up}, true)

	oldA := types.Assignment{Version: 3}
	newA := types.Assignment{Version: 4, Partitions: []types.Partition{{Keys: []string{"p"}, Weight: 1}}}

	require.NoError(t, coord.Apply(context.Background(), "worker-2", oldA, newA))
	require.Equal(t, int64(1), up.calls.Load())
	stored, ok := up.lastPart.Load().([]types.Partition)
	require.True(t, ok)
	require.Equal(t, "p", stored[0].Keys[0])
}

func TestTwoPhaseRetryCAS_SucceedsAfterConflicts(t *testing.T) {
	store := &flakyStore{failUntil: 2}
	cfg := Config{Store: store, MaxRetries: 5, BaseBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond, Jitter: 0}
	cfg.ConsumerUpdater = consumerUpdaterFunc(func(ctx context.Context, workerID string, partitions []types.Partition) error { return nil })
	cfg.Metrics = NopMetrics{}
	cfg.Now = time.Now
	coord := New(cfg, true)

	err := coord.Apply(context.Background(), "worker-1", types.Assignment{}, types.Assignment{Partitions: []types.Partition{{Keys: []string{"p1"}}}})
	require.NoError(t, err)
	// Expect initial create + conflicts + eventual success
	require.GreaterOrEqual(t, store.calls, 3)
}

func TestTwoPhaseRetryCAS_ExhaustsRetries(t *testing.T) {
	store := &flakyStore{failUntil: 10} // always fail within retry budget
	cfg := Config{Store: store, MaxRetries: 2, BaseBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond, Jitter: 0}
	cfg.ConsumerUpdater = consumerUpdaterFunc(func(ctx context.Context, workerID string, partitions []types.Partition) error { return nil })
	cfg.Metrics = NopMetrics{}
	cfg.Now = time.Now
	coord := New(cfg, true)

	err := coord.Apply(context.Background(), "worker-1", types.Assignment{}, types.Assignment{Partitions: []types.Partition{{Keys: []string{"p1"}}}})
	require.Error(t, err)
	require.GreaterOrEqual(t, store.calls, 3) // initial + 2 retries
}

// TestTwoPhase_RemovalGuardBlocksConsumerRemoval verifies that a non-nil RemovalGuard
// is invoked before the consumer-updater phase and that a guard error prevents the
// consumer updater from being called.
func TestTwoPhase_RemovalGuardBlocksConsumerRemoval(t *testing.T) {
	t.Parallel()

	up := &mockUpdater{}
	guardCalls := atomic.Int64{}
	coord := New(Config{
		Store:           newMemStore(),
		ConsumerUpdater: up,
		RemovalGuard: func(ctx context.Context, workerID string, previous, next types.Assignment) error {
			guardCalls.Add(1)
			require.Equal(t, "w1", workerID)
			require.Len(t, previous.Partitions, 1)
			require.Empty(t, next.Partitions)
			return ErrRemovalPending
		},
		TTL: time.Minute,
	}, true)

	prev := types.Assignment{Version: 1, Partitions: []types.Partition{{Keys: []string{"p0"}}}}
	next := types.Assignment{Version: 2, Partitions: nil}

	err := coord.Apply(context.Background(), "w1", prev, next)
	require.ErrorIs(t, err, ErrRemovalPending)
	require.Equal(t, int64(1), guardCalls.Load())
	require.Equal(t, int64(0), up.calls.Load(), "consumer updater must not remove subjects while guard blocks")
}

// TestTwoPhase_MultiKeyPartition_ClaimKeyedBySubjectKey verifies the coordinator
// keys ownership claims by Partition.SubjectKey() (dot-joined) — the identity
// the consumer's pull gating and processing gate resolve ownership by — and not
// Partition.ID() (dash-joined), which would not match for a multi-key partition.
func TestTwoPhase_MultiKeyPartition_ClaimKeyedBySubjectKey(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	coord := New(Config{Store: store, ConsumerUpdater: nopUpdater{}, TTL: time.Minute}, true)

	p := types.Partition{Keys: []string{"region", "us-east"}}
	require.NotEqual(t, p.ID(), p.SubjectKey(),
		"test requires a partition whose ID() differs from SubjectKey()")

	err := coord.Apply(context.Background(), "w1",
		types.Assignment{}, types.Assignment{Version: 1, Partitions: []types.Partition{p}})
	require.NoError(t, err)

	// The claim must exist keyed by SubjectKey(), owned and stable.
	claim, rev, err := store.Get(context.Background(), p.SubjectKey())
	require.NoError(t, err)
	require.NotZero(t, rev, "claim must exist keyed by SubjectKey() %q", p.SubjectKey())
	require.Equal(t, p.SubjectKey(), claim.PartitionID)
	require.Equal(t, "w1", claim.Owner)
	require.Equal(t, ClaimStateStable, claim.State)

	// It must NOT be keyed by ID() — that is the bug this guards against.
	_, revByID, err := store.Get(context.Background(), p.ID())
	require.NoError(t, err)
	require.Zero(t, revByID, "claim must not be keyed by ID() %q", p.ID())
}
