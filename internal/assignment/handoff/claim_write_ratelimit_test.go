package handoff

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/ratelimit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
