package handoff

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// Orphan-claim reap contract. A claim whose partition has left the
// authoritative partition set (the source no longer lists it, so no
// assignment can ever reference it again) is dead weight in the handoff
// bucket: nothing reads it, but it is walked by every resolver warm and
// every sweep, forever. The sweep deletes such claims — but only under
// conditions that make a wrong delete impossible to sustain:
//
//   - only when LivePartitions vouches for the set (ok=true; the manager
//     wires this leader-only so a config-skewed follower can never reap);
//   - only stable claims with no pending owner (in-flight handoffs are
//     reconciled by the existing sweep arms, never reaped);
//   - only after the claim has been continuously absent from the vouched
//     set for OrphanGrace (transient source churn never qualifies);
//   - via a revision-CAS delete, so a concurrent re-add that recreates or
//     transitions the claim wins over the reaper unconditionally.

const orphanTestGrace = 10 * time.Minute

// bigTTL keeps claims out of IsExpired territory for every time advance a
// test makes, so the expired-reset sweep arm cannot interfere with what
// these tests pin.
const bigTTL = int64(24 * 60 * 60)

func stableClaim(pid string, now time.Time) Claim {
	return Claim{
		PartitionID: pid,
		Owner:       "w1",
		State:       ClaimStateStable,
		Epoch:       1,
		LastUpdated: now.UTC(),
		TTLSeconds:  bigTTL,
	}
}

// reapHarness bundles the mutable clock and live-set the tests steer.
type reapHarness struct {
	store *memStore
	coord Coordinator

	mu   sync.Mutex
	now  time.Time
	live map[string]struct{}
	ok   bool
}

func newReapHarness(t *testing.T, grace time.Duration) *reapHarness {
	t.Helper()
	h := &reapHarness{
		store: newMemStore(),
		now:   time.Now().UTC(),
		ok:    true,
		live:  map[string]struct{}{},
	}
	h.coord = New(Config{
		Store: h.store,
		Now: func() time.Time {
			h.mu.Lock()
			defer h.mu.Unlock()

			return h.now
		},
		LivePartitions: func(_ context.Context) (map[string]struct{}, bool) {
			h.mu.Lock()
			defer h.mu.Unlock()

			return h.live, h.ok
		},
		OrphanGrace:   grace,
		SweepInterval: -1, // sweep on every Apply (0 would default to 30s in New)
		MaxRetries:    1,
		BaseBackoff:   time.Millisecond,
		MaxBackoff:    2 * time.Millisecond,
	}, true)

	return h
}

func (h *reapHarness) seed(c Claim) {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.store.data[c.PartitionID] = c
	h.store.rev[c.PartitionID] = 1
}

func (h *reapHarness) setLive(pids ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.live = make(map[string]struct{}, len(pids))
	for _, p := range pids {
		h.live[p] = struct{}{}
	}
}

func (h *reapHarness) setOK(ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ok = ok
}

func (h *reapHarness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

// sweep triggers one opportunistic sweep through the public Apply path.
func (h *reapHarness) sweep(t *testing.T) {
	t.Helper()
	require.NoError(t, h.coord.Apply(context.Background(), "worker-1", types.Assignment{}, types.Assignment{}))
}

func (h *reapHarness) claimExists(t *testing.T, pid string) bool {
	t.Helper()
	_, rev, err := h.store.Get(context.Background(), pid)
	require.NoError(t, err)

	return rev != 0
}

func TestOrphanReap_DeletesOrphanAfterGrace(t *testing.T) {
	t.Parallel()

	h := newReapHarness(t, orphanTestGrace)
	now := time.Now().UTC()
	h.seed(stableClaim("p-live", now))
	h.seed(stableClaim("p-gone", now))
	h.setLive("p-live")

	// First observation starts the absence clock; nothing is deleted yet.
	h.sweep(t)
	require.True(t, h.claimExists(t, "p-gone"), "first absent observation must not delete")
	require.True(t, h.claimExists(t, "p-live"))

	h.advance(orphanTestGrace + time.Second)
	h.sweep(t)
	require.False(t, h.claimExists(t, "p-gone"), "orphan past grace must be reaped")
	require.True(t, h.claimExists(t, "p-live"), "in-set claim must never be reaped")
}

func TestOrphanReap_WithinGraceKept(t *testing.T) {
	t.Parallel()

	h := newReapHarness(t, orphanTestGrace)
	h.seed(stableClaim("p-gone", time.Now().UTC()))
	h.setLive() // empty set: p-gone is absent

	h.sweep(t)
	h.advance(orphanTestGrace - time.Second)
	h.sweep(t)
	require.True(t, h.claimExists(t, "p-gone"), "claim within grace must be kept")
}

func TestOrphanReap_SupplierNotOKSkipsAndDoesNotStartClock(t *testing.T) {
	t.Parallel()

	h := newReapHarness(t, orphanTestGrace)
	h.seed(stableClaim("p-gone", time.Now().UTC()))
	h.setLive()
	h.setOK(false)

	// Unvouched passes must neither delete nor start the absence clock.
	h.sweep(t)
	h.advance(2 * orphanTestGrace)
	h.sweep(t)
	require.True(t, h.claimExists(t, "p-gone"), "unvouched set must never reap")

	// The clock starts at the first vouched absence, not at the not-ok pass.
	h.setOK(true)
	h.sweep(t)
	require.True(t, h.claimExists(t, "p-gone"),
		"first vouched observation starts the clock; deleting here would credit unvouched time")

	h.advance(orphanTestGrace + time.Second)
	h.sweep(t)
	require.False(t, h.claimExists(t, "p-gone"))
}

func TestOrphanReap_UnvouchedGapResetsExistingClock(t *testing.T) {
	t.Parallel()

	h := newReapHarness(t, orphanTestGrace)
	h.seed(stableClaim("p-gone", time.Now().UTC()))
	h.setLive()

	// Vouched absence starts the clock.
	h.sweep(t)

	// A long unvouched gap (lost leadership / source down) must RESET the
	// clock, not merely pause it: time spent unvouched is time the worker
	// could not verify continuous absence, so it must not count toward grace.
	h.setOK(false)
	h.advance(2 * orphanTestGrace)
	h.sweep(t)

	h.setOK(true)
	h.sweep(t)
	require.True(t, h.claimExists(t, "p-gone"),
		"first vouched pass after an unvouched gap must restart grace, not reap on the stale clock")

	h.advance(orphanTestGrace + time.Second)
	h.sweep(t)
	require.False(t, h.claimExists(t, "p-gone"), "a full vouched grace after the gap reaps")
}

func TestOrphanReap_NonStableOrphanKept(t *testing.T) {
	t.Parallel()

	h := newReapHarness(t, orphanTestGrace)
	now := time.Now().UTC()
	prep := Claim{
		PartitionID:  "p-gone",
		Owner:        "w2",
		PendingOwner: "w1",
		State:        ClaimStatePrepare,
		Epoch:        2,
		LastUpdated:  now,
		TTLSeconds:   bigTTL, // not expired: the reset arm must not normalize it mid-test
	}
	h.seed(prep)
	h.setLive()

	h.sweep(t)
	h.advance(orphanTestGrace + time.Second)
	h.sweep(t)

	got, rev, err := h.store.Get(context.Background(), "p-gone")
	require.NoError(t, err)
	require.NotZero(t, rev, "in-flight (non-stable) claim must never be reaped")
	require.Equal(t, ClaimStatePrepare, got.State)
}

func TestOrphanReap_ReturnToSetResetsGrace(t *testing.T) {
	t.Parallel()

	h := newReapHarness(t, orphanTestGrace)
	h.seed(stableClaim("p-flap", time.Now().UTC()))
	h.setLive()

	// Absent: clock starts.
	h.sweep(t)

	// Returns to the set past the original grace: candidate must clear.
	h.advance(orphanTestGrace + time.Second)
	h.setLive("p-flap")
	h.sweep(t)
	require.True(t, h.claimExists(t, "p-flap"))

	// Absent again: the clock must restart, not resume.
	h.setLive()
	h.advance(time.Second)
	h.sweep(t)
	require.True(t, h.claimExists(t, "p-flap"),
		"re-absence must restart the grace clock, not inherit the earlier absence")

	h.advance(orphanTestGrace + time.Second)
	h.sweep(t)
	require.False(t, h.claimExists(t, "p-flap"))
}

func TestOrphanReap_RevisionConflictKeepsClaim(t *testing.T) {
	t.Parallel()

	h := newReapHarness(t, orphanTestGrace)
	h.seed(stableClaim("p-gone", time.Now().UTC()))
	h.setLive()

	// Simulate a concurrent transition between the sweep's read and its
	// delete: every delete attempt fails the revision CAS.
	h.swapStore(t, &conflictingDeleteStore{ClaimStore: h.store})

	h.sweep(t)
	h.advance(orphanTestGrace + time.Second)
	h.sweep(t)
	require.True(t, h.claimExists(t, "p-gone"),
		"a lost delete CAS means the claim moved; the reaper must yield")
}

func TestOrphanReap_ZeroGraceDisables(t *testing.T) {
	t.Parallel()

	h := newReapHarness(t, 0)
	h.seed(stableClaim("p-gone", time.Now().UTC()))
	h.setLive()

	h.sweep(t)
	h.advance(365 * 24 * time.Hour)
	h.sweep(t)
	require.True(t, h.claimExists(t, "p-gone"), "OrphanGrace<=0 must disable reaping entirely")
}

// swapStore replaces the coordinator's store mid-test. Reaches into the
// concrete type; tests live in-package by design (see claim_test.go).
func (h *reapHarness) swapStore(t *testing.T, s ClaimStore) {
	t.Helper()
	tp, ok := h.coord.(*twoPhaseCoordinator)
	require.True(t, ok, "harness coordinator must be the two-phase implementation")
	tp.cfg.Store = s
}

// conflictingDeleteStore fails every Delete with a revision conflict while
// delegating everything else to the wrapped store.
type conflictingDeleteStore struct {
	ClaimStore
}

var errRevConflict = errors.New("revision conflict")

func (c *conflictingDeleteStore) Delete(context.Context, string, uint64) error {
	return errRevConflict
}

// gatedListStore blocks ListKeys until released, counting calls. Used to
// hold one sweep body open while another pass arrives.
type gatedListStore struct {
	ClaimStore
	entered chan struct{} // signaled once per ListKeys entry
	release chan struct{} // close to let ListKeys proceed
	calls   atomic.Int32
}

func (g *gatedListStore) ListKeys(ctx context.Context) ([]string, error) {
	g.calls.Add(1)
	g.entered <- struct{}{}
	<-g.release

	return g.ClaimStore.ListKeys(ctx)
}

// TestSweep_SingleFlight pins that sweep bodies never run concurrently: a
// pass arriving while another sweep is mid-body is skipped outright. This
// is what makes the orphan absence clock single-writer — without it, an
// unvouched pass's clock-clear could interleave between a concurrent
// vouched pass's reap decision and its delete, reaping on a clock the
// clear should have invalidated.
func TestSweep_SingleFlight(t *testing.T) {
	t.Parallel()

	h := newReapHarness(t, orphanTestGrace)
	h.seed(stableClaim("p-0", time.Now().UTC()))
	gated := &gatedListStore{
		ClaimStore: h.store,
		entered:    make(chan struct{}, 2),
		release:    make(chan struct{}),
	}
	h.swapStore(t, gated)

	// First sweep: parks inside ListKeys, holding the sweep body open.
	var wg sync.WaitGroup
	wg.Go(func() { h.sweep(t) })
	<-gated.entered

	// Second sweep while the first is mid-body: must skip without listing.
	wg.Go(func() { h.sweep(t) })
	select {
	case <-gated.entered:
		// Pre-fix behavior: the second pass entered the body concurrently.
		// Fall through; the assertion below reports it.
	case <-time.After(100 * time.Millisecond):
		// Post-fix behavior: the second pass skipped without listing.
	}

	close(gated.release)
	wg.Wait()
	require.EqualValues(t, 1, gated.calls.Load(),
		"a sweep arriving mid-body must be skipped, not run concurrently")
}
