package parti

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/ratelimit"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// countingClaimWriteLimiter records Wait calls and optionally returns an error.
type countingClaimWriteLimiter struct {
	mu      sync.Mutex
	calls   int
	waitErr error
}

func (c *countingClaimWriteLimiter) Wait(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.waitErr
}

func (c *countingClaimWriteLimiter) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

var _ ratelimit.Limiter = (*countingClaimWriteLimiter)(nil)

func expiredClaim(epoch int64) handoff.Claim {
	return handoff.Claim{
		State:       handoff.ClaimStatePrepare,
		LastUpdated: time.Now().UTC().Add(-20 * time.Second), // expired vs TTL 10
		TTLSeconds:  10,
		Epoch:       epoch,
	}
}

// TestManager_handoffStartupHygiene_PacesEveryReset is the site-2 reproducer:
// the startup hygiene loop must consult the claim-write limiter before every
// physical reset write. Three expired claims → three limiter consultations.
func TestManager_handoffStartupHygiene_PacesEveryReset(t *testing.T) {
	limiter := &countingClaimWriteLimiter{}
	m := &Manager{logger: logging.NewNop(), claimWriteLimiter: limiter}

	store := new(mockClaimStore)
	store.On("ListKeys", mock.Anything).Return([]string{"p1", "p2", "p3"}, nil)
	for i, k := range []string{"p1", "p2", "p3"} {
		epoch := int64(i + 1)
		store.On("Get", mock.Anything, k).Return(expiredClaim(epoch), uint64(1), nil)
		store.On("PutIfEpoch", mock.Anything, k, epoch, mock.MatchedBy(func(c handoff.Claim) bool {
			return c.State == handoff.ClaimStateStable
		})).Return(uint64(2), nil)
	}

	resumable := m.handoffStartupHygiene(context.Background(), store)
	require.False(t, resumable)
	assert.Equal(t, 3, limiter.Calls(), "limiter must gate every physical reset write")
	store.AssertExpectations(t)
}

// TestManager_handoffStartupHygiene_CtxCancelStops proves a limiter error
// (e.g. the OperationTimeout firing) stops the best-effort pass before any
// write: no PutIfEpoch is issued.
func TestManager_handoffStartupHygiene_CtxCancelStops(t *testing.T) {
	limiter := &countingClaimWriteLimiter{waitErr: context.Canceled}
	m := &Manager{logger: logging.NewNop(), claimWriteLimiter: limiter}

	store := new(mockClaimStore)
	store.On("ListKeys", mock.Anything).Return([]string{"p1"}, nil)
	store.On("Get", mock.Anything, "p1").Return(expiredClaim(1), uint64(1), nil)
	// No PutIfEpoch expectation: the limiter aborts before the write.

	resumable := m.handoffStartupHygiene(context.Background(), store)
	require.True(t, resumable,
		"a gate-cancelled hygiene pass conservatively flags resumable so the bounded resume pass still runs")
	assert.Equal(t, 1, limiter.Calls())
	store.AssertExpectations(t) // asserts PutIfEpoch was NOT called
}

// TestManager_handoffStartupHygiene_NilLimiterUnchanged proves a nil limiter
// leaves the reset path behaving exactly as before the feature.
func TestManager_handoffStartupHygiene_NilLimiterUnchanged(t *testing.T) {
	m := &Manager{logger: logging.NewNop(), claimWriteLimiter: nil}

	store := new(mockClaimStore)
	store.On("ListKeys", mock.Anything).Return([]string{"p1"}, nil)
	store.On("Get", mock.Anything, "p1").Return(expiredClaim(7), uint64(1), nil)
	store.On("PutIfEpoch", mock.Anything, "p1", int64(7), mock.MatchedBy(func(c handoff.Claim) bool {
		return c.State == handoff.ClaimStateStable
	})).Return(uint64(2), nil)

	resumable := m.handoffStartupHygiene(context.Background(), store)
	require.False(t, resumable)
	store.AssertExpectations(t)
}

// --- runHandoffResume core (site 3): finalizeResumeClaims ---

func commitClaim(owner string, epoch int64) handoff.Claim {
	return handoff.Claim{
		Owner:       owner,
		State:       handoff.ClaimStateCommit,
		Epoch:       epoch,
		LastUpdated: time.Now().UTC(),
	}
}

// TestManager_finalizeResumeClaims_PacesEveryFinalize is the site-3 reproducer:
// every physical commit->stable finalize write must consult the limiter.
func TestManager_finalizeResumeClaims_PacesEveryFinalize(t *testing.T) {
	limiter := &countingClaimWriteLimiter{}
	m := &Manager{logger: logging.NewNop(), claimWriteLimiter: limiter}

	store := new(mockClaimStore)
	store.On("ListKeys", mock.Anything).Return([]string{"p1", "p2", "p3"}, nil)
	for i, k := range []string{"p1", "p2", "p3"} {
		epoch := int64(i + 1)
		store.On("Get", mock.Anything, k).Return(commitClaim("w1", epoch), uint64(1), nil)
		store.On("PutIfEpoch", mock.Anything, k, epoch, mock.MatchedBy(func(c handoff.Claim) bool {
			return c.State == handoff.ClaimStateStable
		})).Return(uint64(2), nil)
	}

	finalized, completed := m.finalizeResumeClaims(context.Background(), store, "w1")
	require.True(t, completed)
	assert.Equal(t, 3, finalized)
	assert.Equal(t, 3, limiter.Calls(), "limiter must gate every physical finalize write")
	store.AssertExpectations(t)
}

// TestManager_finalizeResumeClaims_CtxCancelLeavesIncomplete proves a cancelled
// paced wait aborts before the write and reports incomplete (so the caller keeps
// pendingHandoffResume set for a later retry).
func TestManager_finalizeResumeClaims_CtxCancelLeavesIncomplete(t *testing.T) {
	limiter := &countingClaimWriteLimiter{waitErr: context.Canceled}
	m := &Manager{logger: logging.NewNop(), claimWriteLimiter: limiter}

	store := new(mockClaimStore)
	store.On("ListKeys", mock.Anything).Return([]string{"p1"}, nil)
	store.On("Get", mock.Anything, "p1").Return(commitClaim("w1", 1), uint64(1), nil)
	// No PutIfEpoch expectation: the limiter aborts before the write.

	finalized, completed := m.finalizeResumeClaims(context.Background(), store, "w1")
	assert.False(t, completed, "a cancelled pass must report incomplete so the resume flag stays set")
	assert.Zero(t, finalized)
	assert.Equal(t, 1, limiter.Calls())
	store.AssertExpectations(t)
}

// TestManager_finalizeResumeClaims_NilLimiterUnchanged proves a nil limiter
// leaves the finalize path behaving as before the feature.
func TestManager_finalizeResumeClaims_NilLimiterUnchanged(t *testing.T) {
	m := &Manager{logger: logging.NewNop(), claimWriteLimiter: nil}

	store := new(mockClaimStore)
	store.On("ListKeys", mock.Anything).Return([]string{"p1"}, nil)
	store.On("Get", mock.Anything, "p1").Return(commitClaim("w1", 5), uint64(1), nil)
	store.On("PutIfEpoch", mock.Anything, "p1", int64(5), mock.MatchedBy(func(c handoff.Claim) bool {
		return c.State == handoff.ClaimStateStable
	})).Return(uint64(2), nil)

	finalized, completed := m.finalizeResumeClaims(context.Background(), store, "w1")
	require.True(t, completed)
	assert.Equal(t, 1, finalized)
	store.AssertExpectations(t)
}

// TestManager_finalizeResumeClaims_SkipsForeignAndNonCommit proves the gate
// fires only for this worker's commit-state claims; foreign-owner and
// non-commit claims are skipped without consuming a token.
func TestManager_finalizeResumeClaims_SkipsForeignAndNonCommit(t *testing.T) {
	limiter := &countingClaimWriteLimiter{}
	m := &Manager{logger: logging.NewNop(), claimWriteLimiter: limiter}

	prepare := commitClaim("w1", 1)
	prepare.State = handoff.ClaimStatePrepare

	store := new(mockClaimStore)
	store.On("ListKeys", mock.Anything).Return([]string{"foreign", "prepare", "mine"}, nil)
	store.On("Get", mock.Anything, "foreign").Return(commitClaim("w2", 1), uint64(1), nil)
	store.On("Get", mock.Anything, "prepare").Return(prepare, uint64(1), nil)
	store.On("Get", mock.Anything, "mine").Return(commitClaim("w1", 9), uint64(1), nil)
	store.On("PutIfEpoch", mock.Anything, "mine", int64(9), mock.MatchedBy(func(c handoff.Claim) bool {
		return c.State == handoff.ClaimStateStable
	})).Return(uint64(2), nil)

	finalized, completed := m.finalizeResumeClaims(context.Background(), store, "w1")
	require.True(t, completed)
	assert.Equal(t, 1, finalized)
	assert.Equal(t, 1, limiter.Calls(), "gate fires only for the one owned commit claim")
	store.AssertExpectations(t)
}

// --- Config validation ---

func newTwoPhaseConfig() Config {
	cfg := TestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = "parti-handoff"
	cfg.KVBuckets.HandoffTTL = 2 * time.Minute
	return cfg
}

func TestConfig_ClaimWriteRate_Validation(t *testing.T) {
	t.Run("disabled is valid", func(t *testing.T) {
		cfg := newTwoPhaseConfig()
		cfg.Handoff.ClaimWritePerSec = 0
		cfg.Handoff.ClaimWriteBurst = 0
		require.NoError(t, cfg.Validate())
	})

	t.Run("positive rate with burst is valid", func(t *testing.T) {
		cfg := newTwoPhaseConfig()
		cfg.Handoff.ClaimWritePerSec = 100
		cfg.Handoff.ClaimWriteBurst = 32
		require.NoError(t, cfg.Validate())
	})

	t.Run("positive rate with zero burst is rejected", func(t *testing.T) {
		cfg := newTwoPhaseConfig()
		cfg.Handoff.ClaimWritePerSec = 100
		cfg.Handoff.ClaimWriteBurst = 0
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ClaimWriteBurst")
	})

	t.Run("negative rate is rejected", func(t *testing.T) {
		cfg := newTwoPhaseConfig()
		cfg.Handoff.ClaimWritePerSec = -1
		cfg.Handoff.ClaimWriteBurst = 1
		require.Error(t, cfg.Validate())
	})

	t.Run("burst contract enforced even when two-phase handoff is off", func(t *testing.T) {
		// The (perSec>0, burst<1) invariant is validated unconditionally so the
		// same pair is accepted/rejected consistently regardless of the flag.
		cfg := TestConfig() // EnableTwoPhaseHandoff defaults to false
		require.False(t, cfg.EnableTwoPhaseHandoff)
		cfg.Handoff.ClaimWritePerSec = 100
		cfg.Handoff.ClaimWriteBurst = 0
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ClaimWriteBurst")
	})
}

// --- buildClaimWriteLimiter ---

func TestManager_buildClaimWriteLimiter(t *testing.T) {
	t.Run("disabled returns nil", func(t *testing.T) {
		cfg := newTwoPhaseConfig()
		cfg.Handoff.ClaimWritePerSec = 0
		m := &Manager{cfg: cfg, handoffMetrics: types.NopHandoffMetricsRecorder{}}
		require.Nil(t, m.buildClaimWriteLimiter())
	})

	t.Run("configured returns a limiter", func(t *testing.T) {
		cfg := newTwoPhaseConfig()
		cfg.Handoff.ClaimWritePerSec = 100
		cfg.Handoff.ClaimWriteBurst = 8
		m := &Manager{cfg: cfg, handoffMetrics: types.NopHandoffMetricsRecorder{}}
		require.NotNil(t, m.buildClaimWriteLimiter())
	})

	t.Run("observer wired when metrics implements sidecar", func(t *testing.T) {
		cfg := newTwoPhaseConfig()
		cfg.Handoff.ClaimWritePerSec = 1000 // ~1ms spacing
		cfg.Handoff.ClaimWriteBurst = 1
		obs := &fakeClaimWriteMetrics{}
		m := &Manager{cfg: cfg, handoffMetrics: obs}

		lim := m.buildClaimWriteLimiter()
		require.NotNil(t, lim)

		// First Wait draws the single burst token immediately (no delay → no
		// metric). The second must wait → observer fires.
		require.NoError(t, lim.Wait(t.Context()))
		require.NoError(t, lim.Wait(t.Context()))

		assert.GreaterOrEqual(t, obs.Throttled(), 1, "a delayed claim-write must increment the throttle metric")
	})
}

// --- adapter ---

func TestClaimWriteThrottleAdapter_Bridges(t *testing.T) {
	obs := &fakeClaimWriteMetrics{}
	a := claimWriteThrottleAdapter{obs: obs}

	a.IncrementThrottled()
	a.ObserveWait(0.25)

	assert.Equal(t, 1, obs.Throttled())
	assert.InDelta(t, 0.25, obs.LastWait(), 0.0001)
}

// fakeClaimWriteMetrics implements HandoffMetricsRecorder and the optional
// claimWriteThrottleObserver sidecar.
type fakeClaimWriteMetrics struct {
	types.NopHandoffMetricsRecorder
	mu        sync.Mutex
	throttled int
	lastWait  float64
}

func (f *fakeClaimWriteMetrics) IncClaimWriteThrottled() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.throttled++
}

func (f *fakeClaimWriteMetrics) ObserveClaimWriteThrottleWait(seconds float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastWait = seconds
}

func (f *fakeClaimWriteMetrics) Throttled() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.throttled
}

func (f *fakeClaimWriteMetrics) LastWait() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastWait
}

var _ claimWriteThrottleObserver = (*fakeClaimWriteMetrics)(nil)
