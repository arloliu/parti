package handoff

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/ratelimit"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/assert"
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

func (f *flakyStore) Delete(context.Context, string, uint64) error { return nil }

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

// --- merged from claim_write_ratelimit_test.go ---

// countingLimiter is a test double for ratelimit.Limiter that records Wait
// calls and optionally returns an error.
type countingLimiter struct {
	mu      sync.Mutex
	calls   int
	waitErr error
}

func (c *countingLimiter) Wait(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.waitErr
}

func (c *countingLimiter) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

var _ ratelimit.Limiter = (*countingLimiter)(nil)

// newTwoPhaseForTest builds a *twoPhaseCoordinator with the given store and
// limiter, retries low so the test runs fast.
func newTwoPhaseForTest(t *testing.T, store ClaimStore, limiter ratelimit.Limiter) *twoPhaseCoordinator {
	t.Helper()
	c := New(Config{
		Store:             store,
		ClaimWriteLimiter: limiter,
		MaxRetries:        3,
		BaseBackoff:       time.Millisecond,
		MaxBackoff:        2 * time.Millisecond,
	}, true)
	tp, ok := c.(*twoPhaseCoordinator)
	require.True(t, ok, "expected a *twoPhaseCoordinator")

	return tp
}

func stableTransform() func(*Claim) (*Claim, error) {
	return func(cur *Claim) (*Claim, error) {
		if cur != nil {
			next := cur.NextStable(time.Now())
			return &next, nil
		}
		next := NewInitialClaim("seed", "w1", time.Now(), time.Minute)

		return &next, nil
	}
}

// TestClaimWriteRateLimit_GatesEveryWrite is the reproducer: updateClaim must
// consult the claim-write limiter before EVERY physical PutIfEpoch, including
// the retry after a CAS conflict. Without the gate the limiter is never called.
func TestClaimWriteRateLimit_GatesEveryWrite(t *testing.T) {
	const pid = "p1"
	store := &casConflictOnceStore{inner: newMemStore(), targetID: pid}
	limiter := &countingLimiter{}
	tp := newTwoPhaseForTest(t, store, limiter)

	// First PutIfEpoch conflicts (forced), the retry succeeds → 2 physical
	// writes → limiter must be consulted exactly twice.
	err := tp.updateClaim(t.Context(), pid, stableTransform())
	require.NoError(t, err)

	assert.Equal(t, 2, limiter.Calls(),
		"limiter must gate every physical PutIfEpoch attempt including the CAS retry")
}

// casConflictNStore forces the first n PutIfEpoch calls to conflict, then
// delegates to the inner store. It lets a test pin that the gate fires on every
// physical attempt across the full retry budget, not just the first conflict.
type casConflictNStore struct {
	inner     *memStore
	remaining int
}

var _ ClaimStore = (*casConflictNStore)(nil)

func (s *casConflictNStore) Get(ctx context.Context, pid string) (Claim, uint64, error) {
	return s.inner.Get(ctx, pid)
}

func (s *casConflictNStore) PutIfEpoch(ctx context.Context, pid string, epoch int64, next Claim) (uint64, error) {
	if s.remaining > 0 {
		s.remaining--
		return 0, ErrEpochMismatch
	}

	return s.inner.PutIfEpoch(ctx, pid, epoch, next)
}

func (s *casConflictNStore) ListKeys(ctx context.Context) ([]string, error) {
	return s.inner.ListKeys(ctx)
}

func (s *casConflictNStore) Delete(ctx context.Context, pid string, rev uint64) error {
	return s.inner.Delete(ctx, pid, rev)
}

// TestClaimWriteRateLimit_GatesEveryRetryToExhaustion pins that the gate fires on
// EVERY physical attempt across the full MaxRetries budget. With MaxRetries=3 and
// 3 forced conflicts then success, updateClaim makes 4 physical PutIfEpoch
// attempts → 4 limiter consultations. A regression that gated only the first
// attempt or two would slip past the single-conflict test but fail this one.
func TestClaimWriteRateLimit_GatesEveryRetryToExhaustion(t *testing.T) {
	const pid = "p1"
	store := &casConflictNStore{inner: newMemStore(), remaining: 3}
	limiter := &countingLimiter{}
	tp := newTwoPhaseForTest(t, store, limiter)

	require.NoError(t, tp.updateClaim(t.Context(), pid, stableTransform()))
	assert.Equal(t, 4, limiter.Calls(),
		"limiter must gate every physical PutIfEpoch across all CAS retries, not just the first")
}

// TestClaimWriteRateLimit_SingleWrite covers the common no-conflict path: one
// physical write → one limiter consultation.
func TestClaimWriteRateLimit_SingleWrite(t *testing.T) {
	const pid = "p1"
	limiter := &countingLimiter{}
	tp := newTwoPhaseForTest(t, newMemStore(), limiter)

	require.NoError(t, tp.updateClaim(t.Context(), pid, stableTransform()))
	assert.Equal(t, 1, limiter.Calls())
}

// TestClaimWriteRateLimit_NilLimiterUnchanged proves a nil limiter is unlimited
// and the write still succeeds (behaviour unchanged from before the feature).
func TestClaimWriteRateLimit_NilLimiterUnchanged(t *testing.T) {
	const pid = "p1"
	store := newMemStore()
	tp := newTwoPhaseForTest(t, store, nil)

	require.NoError(t, tp.updateClaim(t.Context(), pid, stableTransform()))

	got, rev, err := store.Get(t.Context(), pid)
	require.NoError(t, err)
	require.NotZero(t, rev)
	assert.Equal(t, ClaimStateStable, got.State)
}

// TestClaimWriteRateLimit_CtxCancelAborts proves a limiter error (e.g. ctx
// cancellation during a paced wait) aborts updateClaim before the write and is
// propagated, so the apply fails pre-commit rather than writing unthrottled.
func TestClaimWriteRateLimit_CtxCancelAborts(t *testing.T) {
	const pid = "p1"
	store := newMemStore()
	limiter := &countingLimiter{waitErr: context.Canceled}
	tp := newTwoPhaseForTest(t, store, limiter)

	err := tp.updateClaim(t.Context(), pid, stableTransform())
	require.ErrorIs(t, err, context.Canceled)

	// No write should have landed.
	_, rev, getErr := store.Get(t.Context(), pid)
	require.NoError(t, getErr)
	assert.Zero(t, rev, "no claim should be written when the limiter aborts")
	assert.Equal(t, 1, limiter.Calls())
}

// --- merged from twophase_concurrency_test.go ---

// observingClaimStore wraps the package-local memStore and instruments
// PutIfEpoch so tests can observe in-flight concurrency.
type observingClaimStore struct {
	inner    *memStore
	inFlight atomic.Int32
	peak     atomic.Int32
	holdFor  time.Duration
}

func newObservingClaimStore(hold time.Duration) *observingClaimStore {
	return &observingClaimStore{inner: newMemStore(), holdFor: hold}
}

func (o *observingClaimStore) Get(ctx context.Context, partitionID string) (Claim, uint64, error) {
	return o.inner.Get(ctx, partitionID)
}

func (o *observingClaimStore) PutIfEpoch(
	ctx context.Context, partitionID string, expectedEpoch int64, next Claim,
) (uint64, error) {
	cur := o.inFlight.Add(1)
	defer o.inFlight.Add(-1)
	for {
		old := o.peak.Load()
		if cur <= old || o.peak.CompareAndSwap(old, cur) {
			break
		}
	}
	if o.holdFor > 0 {
		time.Sleep(o.holdFor)
	}

	return o.inner.PutIfEpoch(ctx, partitionID, expectedEpoch, next)
}

func (o *observingClaimStore) ListKeys(ctx context.Context) ([]string, error) {
	return o.inner.ListKeys(ctx)
}

func (o *observingClaimStore) Delete(ctx context.Context, partitionID string, revision uint64) error {
	return o.inner.Delete(ctx, partitionID, revision)
}

// compile-time assertion
var _ ClaimStore = (*observingClaimStore)(nil)

// TestTwoPhase_PhaseConcurrency_HonorsLimit verifies that setting
// PhaseConcurrency=N causes preparePhase to run at most N in-flight
// updateClaim calls at any instant.
func TestTwoPhase_PhaseConcurrency_HonorsLimit(t *testing.T) {
	const partitions = 50
	const limit = 5

	store := newObservingClaimStore(10 * time.Millisecond)

	coord := New(Config{
		Store:            store,
		TTL:              1 * time.Minute,
		PhaseConcurrency: limit,
	}, true)

	parts := make([]types.Partition, partitions)
	for i := range parts {
		parts[i] = types.Partition{Keys: []string{fmt.Sprintf("p%d", i)}}
	}

	err := coord.Apply(
		context.Background(),
		"worker-1",
		types.Assignment{},
		types.Assignment{Partitions: parts, Version: 1},
	)
	require.NoError(t, err)
	require.LessOrEqual(t, store.peak.Load(), int32(limit), "peak in-flight exceeded limit")
}

// TestTwoPhase_PhaseConcurrency_DefaultsTo20 proves that zero
// PhaseConcurrency is normalized to 20 by handoff.New. If normalization
// is bypassed, errgroup.SetLimit(0) prevents new goroutines from being
// added and the Apply call would hang.
func TestTwoPhase_PhaseConcurrency_DefaultsTo20(t *testing.T) {
	const partitions = 50

	store := newObservingClaimStore(10 * time.Millisecond)

	// PhaseConcurrency omitted — sentinel 0; New must normalize to 20.
	coord := New(Config{
		Store: store,
		TTL:   1 * time.Minute,
	}, true)

	parts := make([]types.Partition, partitions)
	for i := range parts {
		parts[i] = types.Partition{Keys: []string{fmt.Sprintf("p%d", i)}}
	}

	err := coord.Apply(
		context.Background(),
		"worker-1",
		types.Assignment{},
		types.Assignment{Partitions: parts, Version: 1},
	)
	require.NoError(t, err)
	require.LessOrEqual(t, store.peak.Load(), int32(20), "peak in-flight exceeded default 20")
	require.Greater(t, store.peak.Load(), int32(1), "default must be parallel, not serial")
}

// TestTwoPhase_PhaseConcurrency_OneIsSerial proves the operator contract:
// PhaseConcurrency=1 means one in-flight per phase, ever.
func TestTwoPhase_PhaseConcurrency_OneIsSerial(t *testing.T) {
	const partitions = 20

	store := newObservingClaimStore(5 * time.Millisecond)

	coord := New(Config{
		Store:            store,
		TTL:              1 * time.Minute,
		PhaseConcurrency: 1,
	}, true)

	parts := make([]types.Partition, partitions)
	for i := range parts {
		parts[i] = types.Partition{Keys: []string{fmt.Sprintf("p%d", i)}}
	}

	err := coord.Apply(
		context.Background(),
		"worker-1",
		types.Assignment{},
		types.Assignment{Partitions: parts, Version: 1},
	)
	require.NoError(t, err)
	require.Equal(t, int32(1), store.peak.Load(), "PhaseConcurrency=1 must be strictly serial")
}
