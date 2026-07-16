package handoff

// Live-NATS proof of §4.10: no-leader healing within the I2 bound, and
// §4.12: mixed-concurrency stress (leader full cadence + follower
// backstops + concurrent Apply-origin claim mutation).
//
// The 5-minute followerBackstopTarget is a const, so real-time waiting is
// not viable in a unit test; both proofs drive injected SweepTicks +
// fake Now against a REAL NATS-backed claim Store, exactly like
// sweep_gate_reap_live_test.go's convention (testing.Short() guard, no
// build tag — this file runs as part of `make test`, not
// `make test-integration`).

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestSweepAuthorityLive_NoLeaderHealingWithinI2Bound pins §4.10:
// N coordinators share one real NATS-backed claim bucket, ALL reporting
// SweepAuthority=false (no leader anywhere in the fleet). A planted
// expired-TTL prepare still heals within the I2 either-origin bound —
// (backstopEvery+1) x SweepInterval — via the ticker's backstop cadence,
// and the healing pass is observed as ticker-origin, UNAUTHORIZED,
// admitted via SweepObserver — proving it was NOT healed by an
// accidental leader running at full cadence.
func TestSweepAuthorityLive_NoLeaderHealingWithinI2Bound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	// partitest (not internal/testutil): an in-package handoff test
	// cannot import internal/testutil, which pulls in the root parti
	// package that itself imports this package (cycle).
	srv, nc := partitest.StartEmbeddedNATS(t)
	defer func() {
		nc.Close()
		srv.Shutdown()
		srv.WaitForShutdown()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const bucket = "sweep-authority-live-i2"
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)
	store := NewNATSClaimStore(kv, "claims/")

	// A sanity handle dedicated to the test's own reads — never a handle
	// a live component owns (ListKeys spins up a throwaway ordered
	// consumer that mutates its handle's stream state).
	sanityKV, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)
	sanityStore := NewNATSClaimStore(sanityKV, "claims/")

	// A stuck, TTL-expired prepare: the reconcile arm's expired-reset
	// path (independent of LivePartitions/leadership) is the healing
	// this test observes — exactly the shape a stalled two-worker
	// handoff leaves behind.
	seedNow := time.Now().UTC()
	stuck := Claim{
		PartitionID:  "p-stuck",
		Owner:        "worker-A",
		PendingOwner: "worker-B",
		State:        ClaimStatePrepare,
		Epoch:        1,
		LastUpdated:  seedNow.Add(-time.Hour),
		TTLSeconds:   1, // long since expired relative to LastUpdated
	}
	_, err = store.PutIfEpoch(ctx, "p-stuck", 0, stuck)
	require.NoError(t, err)

	const (
		numWorkers    = 3
		interval      = 30 * time.Second
		backstopEvery = 10 // default: followerBackstopTarget(5m) / interval(30s)
	)

	// A single mutex-guarded fake clock shared by every coordinator: the
	// I2 bound is expressed in tick counts, not wall-clock time, so
	// there is no need for per-worker clock skew. Guarded (not a plain
	// closure over a bare variable) so -race is clean regardless of
	// exactly when each coordinator's ticker goroutine reads it.
	clock := struct {
		mu  sync.Mutex
		now time.Time
	}{now: seedNow}
	readNow := func() time.Time {
		clock.mu.Lock()
		defer clock.mu.Unlock()

		return clock.now
	}
	advanceNow := func(d time.Duration) time.Time {
		clock.mu.Lock()
		defer clock.mu.Unlock()
		clock.now = clock.now.Add(d)

		return clock.now
	}

	type worker struct {
		coord  *twoPhaseCoordinator
		ticks  chan time.Time
		events chan sweepAuthEvent
	}

	workers := make([]*worker, numWorkers)
	for i := range numWorkers {
		w := &worker{ticks: make(chan time.Time), events: make(chan sweepAuthEvent, 64)}
		coord, ok := New(Config{
			Store:          store,
			Now:            readNow,
			SweepInterval:  interval,
			SweepPhaseSeed: uint64(i), // stagger backstop ticks across the fleet
			SweepTicks:     w.ticks,
			SweepAuthority: func() bool { return false }, // no leader, ever
			SweepObserver: func(origin sweepOrigin, authorized, admitted bool) {
				w.events <- sweepAuthEvent{origin: origin, authorized: authorized, admitted: admitted}
			},
			MaxRetries:  3,
			BaseBackoff: time.Millisecond,
			MaxBackoff:  5 * time.Millisecond,
		}, true).(*twoPhaseCoordinator)
		require.True(t, ok)
		w.coord = coord
		w.coord.Start(ctx)
		workers[i] = w
	}

	// Drive ticks in lockstep across all N coordinators, up to the I2
	// bound (backstopEvery+1 rounds), checking for healing after each
	// round and capturing whichever pass actually admitted.
	var healingEvent *sweepAuthEvent
	healed := false
	for round := 1; round <= backstopEvery+1 && !healed; round++ {
		advanceNow(interval)
		for _, w := range workers {
			select {
			case w.ticks <- readNow():
			case <-time.After(5 * time.Second):
				t.Fatalf("worker did not consume round %d's tick", round)
			}
		}

		for _, w := range workers {
		drain:
			for {
				select {
				case ev := <-w.events:
					if ev.admitted {
						e := ev
						healingEvent = &e
					}
				default:
					break drain
				}
			}
		}

		got, rev, gerr := sanityStore.Get(ctx, "p-stuck")
		require.NoError(t, gerr)
		if rev != 0 && got.State == ClaimStateStable {
			healed = true
		}
	}

	require.True(t, healed,
		"the stuck prepare must heal within the I2 bound ((backstopEvery+1) x SweepInterval) with no leader present")
	require.NotNil(t, healingEvent, "the healing pass must have been observed via SweepObserver")
	require.Equal(t, sweepOriginTicker, healingEvent.origin, "healed by a TICKER pass, not an accidental leader")
	require.False(t, healingEvent.authorized, "healed while UNAUTHORIZED — no accidental leader ran at full cadence")
	require.True(t, healingEvent.admitted)
}

// countTickerAdmissions returns a SweepObserver that increments counter
// for every ticker-origin ADMITTED event whose authorized value matches
// wantAuthorized — factored into its own named function (rather than an
// inline closure at each call site) so its branch counts against ITS
// OWN cyclomatic complexity budget, not
// TestSweepAuthorityLive_MixedConcurrencyStress's.
func countTickerAdmissions(counter *atomic.Int64, wantAuthorized bool) func(origin sweepOrigin, authorized, admitted bool) {
	return func(origin sweepOrigin, authorized, admitted bool) {
		if origin == sweepOriginTicker && authorized == wantAuthorized && admitted {
			counter.Add(1)
		}
	}
}

// liveStressCoordinatorOpts configures newLiveStressCoordinator.
type liveStressCoordinatorOpts struct {
	store     ClaimStore
	now       func() time.Time
	interval  time.Duration
	ticks     chan time.Time
	authority bool
	observer  func(origin sweepOrigin, authorized, admitted bool)
}

// newLiveStressCoordinator builds and starts a two-phase coordinator for
// TestSweepAuthorityLive_MixedConcurrencyStress — factored out to keep
// that test's own cyclomatic complexity under the repo's lint budget.
func newLiveStressCoordinator(t *testing.T, ctx context.Context, opts liveStressCoordinatorOpts) *twoPhaseCoordinator {
	t.Helper()
	authority := opts.authority
	coord, ok := New(Config{
		Store:          opts.store,
		Now:            opts.now,
		SweepInterval:  opts.interval,
		SweepPhaseSeed: 0,
		SweepTicks:     opts.ticks,
		SweepAuthority: func() bool { return authority },
		SweepObserver:  opts.observer,
		MaxRetries:     3,
		BaseBackoff:    time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
	}, true).(*twoPhaseCoordinator)
	require.True(t, ok)
	coord.Start(ctx)

	return coord
}

// TestSweepAuthorityLive_MixedConcurrencyStress pins §4.12 (review
// finding P1-4): the minimal sufficient shape the review specified —
// two coordinators against one embedded-NATS bucket, each with its OWN
// production and probe KV handles (NewNATSClaimStoreWithProbe, mirroring
// how the manager wires a dedicated probe handle in production — never
// sharing a handle across coordinators, since kv.Status mutates shared
// *stream state under concurrent Get/Put: the epoch-monitor race class
// rule 300-testing.md:44 requires live-NATS stress to catch). One
// constant-authority leader and one false-authority follower, the
// follower's phase deterministically reaching a backstop tick; injected
// ticks and fake time (no real 5-minute waits); ~5s of concurrent
// Apply-origin claim mutation from both coordinators under -race;
// SweepObserver assertions proving at least one authorized ticker
// admission, at least one unauthorized backstop admission, and
// overlapping Apply attempts; finishing with no deadlock and stable
// claim convergence.
func TestSweepAuthorityLive_MixedConcurrencyStress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	srv, nc := partitest.StartEmbeddedNATS(t)
	defer func() {
		nc.Close()
		srv.Shutdown()
		srv.WaitForShutdown()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const bucket = "sweep-authority-live-mixed-concurrency"
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	// Two coordinators, ONE shared bucket, each with its own dedicated
	// production + probe handle pair — four handles total, never shared
	// across coordinators.
	kvA, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)
	probeA, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)
	kvB, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)
	probeB, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)

	storeA := NewNATSClaimStoreWithProbe(kvA, probeA, "claims/")
	storeB := NewNATSClaimStoreWithProbe(kvB, probeB, "claims/")

	// A sanity handle dedicated to the test's own final reads — never a
	// handle a live component owns.
	sanityKV, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)
	sanityStore := NewNATSClaimStore(sanityKV, "claims/")

	// A single mutex-guarded fake clock shared by both coordinators —
	// the I2/backstop bound is expressed in tick counts, not wall-clock
	// time.
	clock := struct {
		mu  sync.Mutex
		now time.Time
	}{now: time.Now().UTC()}
	readNow := func() time.Time {
		clock.mu.Lock()
		defer clock.mu.Unlock()

		return clock.now
	}
	advanceNow := func(d time.Duration) time.Time {
		clock.mu.Lock()
		defer clock.mu.Unlock()
		clock.now = clock.now.Add(d)

		return clock.now
	}

	const (
		interval      = 30 * time.Second
		backstopEvery = 10 // default: followerBackstopTarget(5m) / interval(30s)
	)

	ticksA := make(chan time.Time)
	ticksB := make(chan time.Time)

	// Aggregate counters, not a buffered event-history channel: under
	// the Apply-origin stress below, SweepObserver fires on EVERY single
	// Apply call (hundreds+ in a 5s window) — a bounded channel with
	// nothing draining it concurrently deadlocks (the apply-loop
	// goroutines block sending to a full channel while the only drain
	// point waits for them to finish first). This test only needs
	// aggregate admission counts, so atomics sidestep the problem
	// entirely.
	var (
		aTickerAuthorizedAdmitted   atomic.Int64
		bTickerUnauthorizedAdmitted atomic.Int64
	)

	coordA := newLiveStressCoordinator(t, ctx, liveStressCoordinatorOpts{
		store: storeA, now: readNow, interval: interval, ticks: ticksA,
		authority: true, // constant-authority leader
		observer:  countTickerAdmissions(&aTickerAuthorizedAdmitted, true),
	})
	coordB := newLiveStressCoordinator(t, ctx, liveStressCoordinatorOpts{
		store: storeB, now: readNow, interval: interval, ticks: ticksB,
		authority: false, // false-authority follower; phase 0: deterministic backstop at tick 10
		observer:  countTickerAdmissions(&bTickerUnauthorizedAdmitted, false),
	})

	partitions := []string{"p0", "p1", "p2", "p3"}
	makeAssignment := func(pids ...string) types.Assignment {
		parts := make([]types.Partition, len(pids))
		for i, pid := range pids {
			parts[i] = types.Partition{Keys: []string{pid}}
		}

		return types.Assignment{Version: 1, Partitions: parts}
	}

	stressCtx, stressCancel := context.WithTimeout(ctx, 5*time.Second)
	defer stressCancel()

	var (
		wg              sync.WaitGroup
		inFlight        atomic.Int32
		overlapObserved atomic.Bool
	)

	// Concurrent Apply-origin claim mutation: both coordinators toggle
	// the SAME partition set between "acquire" and "release" as fast as
	// they can for ~5s, contending over the shared bucket under -race —
	// the overlapping-attempts proof the review requires.
	applyLoop := func(coord *twoPhaseCoordinator, worker string) {
		defer wg.Done()
		prev := types.Assignment{}
		acquire := false
		for stressCtx.Err() == nil {
			next := types.Assignment{}
			if acquire {
				next = makeAssignment(partitions...)
			}
			acquire = !acquire

			if inFlight.Add(1) > 1 {
				overlapObserved.Store(true)
			}
			_ = coord.Apply(stressCtx, worker, prev, next)
			inFlight.Add(-1)

			prev = next
		}
	}

	wg.Add(2)
	go applyLoop(coordA, "worker-A")
	go applyLoop(coordB, "worker-B")

	// Concurrently — overlapping the Apply stress above — drive ticks
	// for both coordinators, enough rounds to guarantee coordB's
	// deterministic backstop (tick backstopEvery, phase 0) fires at
	// least once.
	pushBothTicks := func() {
		now := advanceNow(interval)
		var tickWg sync.WaitGroup
		tickWg.Add(2)
		go func() {
			defer tickWg.Done()
			select {
			case ticksA <- now:
			case <-ctx.Done():
			}
		}()
		go func() {
			defer tickWg.Done()
			select {
			case ticksB <- now:
			case <-ctx.Done():
			}
		}()
		tickWg.Wait()
	}

	const rounds = backstopEvery + 2
	for range rounds {
		pushBothTicks()
	}

	wg.Wait() // the ~5s Apply stress window completes

	// A few more rounds, past the Apply stress window, so the fake
	// clock advances enough (well past the default 1-minute claim TTL)
	// for the reconcile arm to settle any claim left mid-handoff by the
	// contention above — the "stable claim convergence" this test must
	// finish by asserting.
	for range 6 {
		pushBothTicks()
	}

	require.GreaterOrEqual(t, aTickerAuthorizedAdmitted.Load(), int64(1),
		"the constant-authority leader must record at least one authorized ticker admission")
	require.GreaterOrEqual(t, bTickerUnauthorizedAdmitted.Load(), int64(1),
		"the false-authority follower must record at least one unauthorized backstop admission")
	require.True(t, overlapObserved.Load(),
		"the concurrent Apply-origin stress must have produced overlapping attempts")

	// No deadlock: reaching this point within the outer 60s ctx already
	// proves it. Stable claim convergence: every listed claim must have
	// settled to stable.
	require.Eventually(t, func() bool {
		keys, lerr := sanityStore.ListKeys(ctx)
		if lerr != nil {
			return false
		}
		for _, k := range keys {
			c, rev, gerr := sanityStore.Get(ctx, k)
			if gerr != nil || rev == 0 {
				continue
			}
			if c.State != ClaimStateStable {
				return false
			}
		}

		return true
	}, 10*time.Second, 100*time.Millisecond, "claims must converge to stable after the stress window")
}
