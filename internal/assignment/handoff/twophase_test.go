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

// TestClaimWriteRateLimit verifies that updateClaim consults the claim-write
// limiter before EVERY physical PutIfEpoch — including each CAS retry across the
// full MaxRetries budget — and that a limiter error aborts before any write while
// a nil limiter leaves behaviour unchanged.
func TestClaimWriteRateLimit(t *testing.T) {
	// memStore the row supplied as the bare store (so a post-check can read it
	// back). When non-nil it is the *memStore the row's store wraps; the
	// extraCheck closure uses it to assert no/landed writes.
	tests := []struct {
		name string
		// newStore builds the ClaimStore for the row. base is a fresh memStore the
		// row may wrap and that extraCheck can read back from.
		newStore func(base *memStore) ClaimStore
		// newLimiter builds the row's limiter; return nil to exercise the unlimited
		// (nil-limiter) path. The same *countingLimiter is passed to extraCheck.
		newLimiter func() *countingLimiter
		wantErr    error
		// extraCheck runs after updateClaim returns; base is the wrapped memStore,
		// limiter is the row's limiter (nil if newLimiter returned nil).
		extraCheck func(t *testing.T, base *memStore, limiter *countingLimiter)
	}{
		{
			// First PutIfEpoch conflicts (forced), the retry succeeds → 2 physical
			// writes → limiter must be consulted exactly twice.
			name: "GatesEveryWrite",
			newStore: func(base *memStore) ClaimStore {
				return &casConflictOnceStore{inner: base, targetID: "p1"}
			},
			newLimiter: func() *countingLimiter { return &countingLimiter{} },
			extraCheck: func(t *testing.T, _ *memStore, limiter *countingLimiter) {
				t.Helper()
				assert.Equal(t, 2, limiter.Calls(),
					"limiter must gate every physical PutIfEpoch attempt including the CAS retry")
			},
		},
		{
			// Pins that the gate fires on EVERY physical attempt across the full
			// MaxRetries budget. With MaxRetries=3 and 3 forced conflicts then
			// success, updateClaim makes 4 physical PutIfEpoch attempts → 4 limiter
			// consultations. A regression that gated only the first attempt or two
			// would slip past the single-conflict case but fail this one.
			name: "GatesEveryRetryToExhaustion",
			newStore: func(base *memStore) ClaimStore {
				return &casConflictNStore{inner: base, remaining: 3}
			},
			newLimiter: func() *countingLimiter { return &countingLimiter{} },
			extraCheck: func(t *testing.T, _ *memStore, limiter *countingLimiter) {
				t.Helper()
				assert.Equal(t, 4, limiter.Calls(),
					"limiter must gate every physical PutIfEpoch across all CAS retries, not just the first")
			},
		},
		{
			// Common no-conflict path: one physical write → one limiter consultation.
			name:       "SingleWrite",
			newStore:   func(base *memStore) ClaimStore { return base },
			newLimiter: func() *countingLimiter { return &countingLimiter{} },
			extraCheck: func(t *testing.T, _ *memStore, limiter *countingLimiter) {
				t.Helper()
				assert.Equal(t, 1, limiter.Calls())
			},
		},
		{
			// A nil limiter is unlimited and the write still succeeds (behaviour
			// unchanged from before the feature).
			name:       "NilLimiterUnchanged",
			newStore:   func(base *memStore) ClaimStore { return base },
			newLimiter: func() *countingLimiter { return nil },
			extraCheck: func(t *testing.T, base *memStore, _ *countingLimiter) {
				t.Helper()
				got, rev, err := base.Get(t.Context(), "p1")
				require.NoError(t, err)
				require.NotZero(t, rev)
				assert.Equal(t, ClaimStateStable, got.State)
			},
		},
		{
			// A limiter error (e.g. ctx cancellation during a paced wait) aborts
			// updateClaim before the write and is propagated, so the apply fails
			// pre-commit rather than writing unthrottled.
			name:       "CtxCancelAborts",
			newStore:   func(base *memStore) ClaimStore { return base },
			newLimiter: func() *countingLimiter { return &countingLimiter{waitErr: context.Canceled} },
			wantErr:    context.Canceled,
			extraCheck: func(t *testing.T, base *memStore, limiter *countingLimiter) {
				t.Helper()
				// No write should have landed.
				_, rev, getErr := base.Get(t.Context(), "p1")
				require.NoError(t, getErr)
				assert.Zero(t, rev, "no claim should be written when the limiter aborts")
				assert.Equal(t, 1, limiter.Calls())
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const pid = "p1"
			base := newMemStore()
			limiter := tc.newLimiter()
			// Pass a typed-nil ratelimit.Limiter when the row wants the unlimited path.
			var lim ratelimit.Limiter
			if limiter != nil {
				lim = limiter
			}
			tp := newTwoPhaseForTest(t, tc.newStore(base), lim)

			err := tp.updateClaim(t.Context(), pid, stableTransform())
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}

			if tc.extraCheck != nil {
				tc.extraCheck(t, base, limiter)
			}
		})
	}
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

// TestTwoPhase_PhaseConcurrency verifies that PhaseConcurrency bounds the number
// of in-flight updateClaim calls preparePhase runs at any instant: an explicit
// limit caps the observed peak, a zero (omitted) value is normalized to 20 by
// handoff.New and runs in parallel, and 1 is strictly serial.
func TestTwoPhase_PhaseConcurrency(t *testing.T) {
	tests := []struct {
		name string
		// phaseConcurrency is written verbatim into Config; 0 means "omitted —
		// sentinel; New must normalize to 20".
		phaseConcurrency int
		numPartitions    int
		holdDuration     time.Duration
		// checkPeak asserts the observed-peak semantics specific to the row.
		checkPeak func(t *testing.T, peak int32)
	}{
		{
			// Setting PhaseConcurrency=N caps in-flight updateClaim calls at N.
			name:             "HonorsLimit",
			phaseConcurrency: 5,
			numPartitions:    50,
			holdDuration:     10 * time.Millisecond,
			checkPeak: func(t *testing.T, peak int32) {
				t.Helper()
				require.LessOrEqual(t, peak, int32(5), "peak in-flight exceeded limit")
			},
		},
		{
			// Zero PhaseConcurrency is normalized to 20 by handoff.New. If
			// normalization is bypassed, errgroup.SetLimit(0) prevents new
			// goroutines from being added and the Apply call would hang.
			name:             "DefaultsTo20",
			phaseConcurrency: 0,
			numPartitions:    50,
			holdDuration:     10 * time.Millisecond,
			checkPeak: func(t *testing.T, peak int32) {
				t.Helper()
				require.LessOrEqual(t, peak, int32(20), "peak in-flight exceeded default 20")
				require.Greater(t, peak, int32(1), "default must be parallel, not serial")
			},
		},
		{
			// Operator contract: PhaseConcurrency=1 means one in-flight per phase, ever.
			name:             "OneIsSerial",
			phaseConcurrency: 1,
			numPartitions:    20,
			holdDuration:     5 * time.Millisecond,
			checkPeak: func(t *testing.T, peak int32) {
				t.Helper()
				require.Equal(t, int32(1), peak, "PhaseConcurrency=1 must be strictly serial")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newObservingClaimStore(tc.holdDuration)

			coord := New(Config{
				Store:            store,
				TTL:              1 * time.Minute,
				PhaseConcurrency: tc.phaseConcurrency,
			}, true)

			parts := make([]types.Partition, tc.numPartitions)
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
			tc.checkPeak(t, store.peak.Load())
		})
	}
}
