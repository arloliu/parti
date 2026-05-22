package stableid

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/partitest"
)

// mockKV wraps jetstream.KeyValue embedding so that only Create and Get
// need to be implemented; all other method calls panic.
type mockKV struct {
	jetstream.KeyValue
	createCalls []func(ctx context.Context, key string, value []byte) (uint64, error)
	getCalls    []func(ctx context.Context, key string) (jetstream.KeyValueEntry, error)
	updateCalls []func(ctx context.Context, key string, value []byte, revision uint64) (uint64, error)
	createIdx   int
	getIdx      int
	updateIdx   int
}

func (m *mockKV) Create(ctx context.Context, key string, value []byte, _ ...jetstream.KVCreateOpt) (uint64, error) {
	idx := m.createIdx
	m.createIdx++
	if idx < len(m.createCalls) {
		return m.createCalls[idx](ctx, key, value)
	}
	panic("unexpected Create call")
}

func (m *mockKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	idx := m.getIdx
	m.getIdx++
	if idx < len(m.getCalls) {
		return m.getCalls[idx](ctx, key)
	}
	panic("unexpected Get call")
}

func (m *mockKV) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	idx := m.updateIdx
	m.updateIdx++
	if idx < len(m.updateCalls) {
		return m.updateCalls[idx](ctx, key, value, revision)
	}
	panic("unexpected Update call")
}

// Unit tests that do not require a real KV backend.

func TestClaimer_StartRenewal_WithoutClaim(t *testing.T) {
	t.Parallel()

	c := NewClaimer(nil, "worker", 0, 9, 0, nil) // kv nil is fine for this path
	err := c.StartRenewal()
	require.ErrorIs(t, err, ErrNotClaimed)
}

func TestClaimer_Release_WithoutClaim(t *testing.T) {
	t.Parallel()

	c := NewClaimer(nil, "worker", 0, 9, 0, nil)
	err := c.Release(context.Background())
	require.ErrorIs(t, err, ErrNotClaimed)
}

func TestClaimer_WorkerID_DefaultEmpty(t *testing.T) {
	t.Parallel()

	c := NewClaimer(nil, "worker", 0, 9, 0, nil)
	require.Equal(t, "", c.WorkerID())
}

func TestClaimer_DoubleRelease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "unit-stableid-double-release", TTL: 500 * time.Millisecond, Storage: jetstream.MemoryStorage})
	require.NoError(t, err)

	c := NewClaimer(kv, "worker", 0, 0, 500*time.Millisecond, nil)
	wid, err := c.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", wid)

	require.NoError(t, c.StartRenewal())

	// First release succeeds
	require.NoError(t, c.Release(ctx))

	// Second release returns ErrNotClaimed
	err = c.Release(ctx)
	require.ErrorIs(t, err, ErrNotClaimed)
}

// TestClaimer_SetOnError_Concurrent exercises the atomic.Pointer hot path:
// the renewal goroutine reads onError while SetOnError swaps it from another
// goroutine. Run with -race; no assertion failure here is the pass condition.
func TestClaimer_SetOnError_Concurrent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "unit-stableid-onerror-race",
		TTL:     500 * time.Millisecond,
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	c := NewClaimer(kv, "worker", 0, 0, 300*time.Millisecond, nil)
	_, err = c.Claim(ctx)
	require.NoError(t, err)
	c.SetOnError(func(error) {})
	require.NoError(t, c.StartRenewal())
	t.Cleanup(func() { _ = c.Release(ctx) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			if i%3 == 0 {
				c.SetOnError(nil)
			} else {
				c.SetOnError(func(error) {})
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SetOnError swap loop did not complete in 5s")
	}
}

func TestClaimer_StartRenewal_AfterClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "unit-stableid-renew-after-close", TTL: 500 * time.Millisecond, Storage: jetstream.MemoryStorage})
	require.NoError(t, err)

	c := NewClaimer(kv, "worker", 0, 0, 500*time.Millisecond, nil)
	wid, err := c.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", wid)

	c.Close()
	err = c.StartRenewal()
	require.ErrorIs(t, err, ErrAlreadyClosed)
	require.Equal(t, "worker-0", c.WorkerID(), "Close should not delete the key or clear workerID")
}

// TestClaimer_Claim_StaleKeyRace verifies that when kv.Create returns ErrKeyExists
// but the key disappears before kv.Get (stale TTL expiry race), the fallback
// uses kv.Create again — NOT kv.Put — so two concurrent workers cannot both
// claim the same ID.
// TestClaimer_renew_ClaimLost verifies that when the renewal Update fails with
// a revision mismatch (ErrKeyExists — nats.go's wrong-last-sequence sentinel),
// renew returns ErrClaimLost rather than a generic renewal error.
func TestClaimer_renew_ClaimLost(t *testing.T) {
	t.Parallel()

	kv := &mockKV{
		createCalls: []func(context.Context, string, []byte) (uint64, error){
			func(_ context.Context, _ string, _ []byte) (uint64, error) { return 7, nil },
		},
		updateCalls: []func(context.Context, string, []byte, uint64) (uint64, error){
			func(_ context.Context, _ string, _ []byte, _ uint64) (uint64, error) {
				return 0, jetstream.ErrKeyExists
			},
		},
	}

	c := NewClaimer(kv, "worker", 0, 0, time.Second, nil)
	_, err := c.Claim(context.Background())
	require.NoError(t, err)

	err = c.renew(context.Background())
	require.ErrorIs(t, err, ErrClaimLost)
}

// TestClaimer_renew_TransientErrorIsNotClaimLost verifies that a non-CAS
// renewal error (e.g. connectivity) is returned as a generic error, NOT
// ErrClaimLost — the loop must keep retrying transient failures.
func TestClaimer_renew_TransientErrorIsNotClaimLost(t *testing.T) {
	t.Parallel()

	kv := &mockKV{
		createCalls: []func(context.Context, string, []byte) (uint64, error){
			func(_ context.Context, _ string, _ []byte) (uint64, error) { return 7, nil },
		},
		updateCalls: []func(context.Context, string, []byte, uint64) (uint64, error){
			func(_ context.Context, _ string, _ []byte, _ uint64) (uint64, error) {
				return 0, errors.New("nats: connection closed")
			},
		},
	}

	c := NewClaimer(kv, "worker", 0, 0, time.Second, nil)
	_, err := c.Claim(context.Background())
	require.NoError(t, err)

	err = c.renew(context.Background())
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrClaimLost)
}

// TestClaimer_RenewalStopsOnClaimLost verifies that when the claimed key's
// revision is bumped out from under a renewing worker (simulating a takeover),
// the next renewal detects ErrClaimLost, fires onError, and stops the loop.
func TestClaimer_RenewalStopsOnClaimLost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Bucket TTL=0 so the key does not expire during the test window.
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "unit-stableid-claim-lost",
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	c := NewClaimer(kv, "worker", 0, 0, 300*time.Millisecond, nil)
	wid, err := c.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", wid)

	gotErr := make(chan error, 1)
	c.SetOnError(func(e error) {
		select {
		case gotErr <- e:
		default:
		}
	})
	require.NoError(t, c.StartRenewal())
	t.Cleanup(func() { _ = c.Release(context.Background()) })

	// Simulate another worker taking the ID over: bump the revision.
	_, err = kv.Put(ctx, "worker-0", []byte("taken-over"))
	require.NoError(t, err)

	select {
	case e := <-gotErr:
		require.ErrorIs(t, e, ErrClaimLost)
	case <-time.After(5 * time.Second):
		t.Fatal("renewal did not report ErrClaimLost within 5s")
	}
}

// TestClaimer_ReleaseDoesNotDeleteReclaimedKey verifies that Release performs a
// revision-checked delete: if the ID was taken over (revision moved), Release
// must NOT delete the new owner's key.
func TestClaimer_ReleaseDoesNotDeleteReclaimedKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "unit-stableid-release-reclaimed",
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	// StartRenewal is intentionally not called: this exercises the no-renewal
	// path of Release, where lastRevision is whatever Claim recorded.
	c := NewClaimer(kv, "worker", 0, 0, time.Second, nil)
	_, err = c.Claim(ctx)
	require.NoError(t, err)

	// Simulate a takeover: another writer bumps the revision.
	newRev, err := kv.Put(ctx, "worker-0", []byte("new-owner"))
	require.NoError(t, err)

	// Release must not error and must not delete the new owner's key.
	require.NoError(t, c.Release(ctx))

	entry, err := kv.Get(ctx, "worker-0")
	require.NoError(t, err, "Release must not delete a key it no longer owns")
	require.Equal(t, newRev, entry.Revision())
	require.Equal(t, []byte("new-owner"), entry.Value())
}

// fakeEntry is a jetstream.KeyValueEntry test double with a controllable
// creation time and revision; all other fields return zero values.
type fakeEntry struct {
	revision uint64
	created  time.Time
}

func (f fakeEntry) Bucket() string                  { return "fake" }
func (f fakeEntry) Key() string                     { return "worker-0" }
func (f fakeEntry) Value() []byte                   { return nil }
func (f fakeEntry) Revision() uint64                { return f.revision }
func (f fakeEntry) Created() time.Time              { return f.created }
func (f fakeEntry) Delta() uint64                   { return 0 }
func (f fakeEntry) Operation() jetstream.KeyValueOp { return jetstream.KeyValuePut }

func TestClaimer_Claim_StaleKeyRace(t *testing.T) {
	t.Parallel()

	t.Run("retries Create after key disappears, succeeds", func(t *testing.T) {
		t.Parallel()

		kv := &mockKV{
			createCalls: []func(context.Context, string, []byte) (uint64, error){
				// First attempt: key exists (another worker is claiming)
				func(_ context.Context, _ string, _ []byte) (uint64, error) {
					return 0, jetstream.ErrKeyExists
				},
				// Retry Create: we win the race
				func(_ context.Context, _ string, _ []byte) (uint64, error) {
					return 42, nil
				},
			},
			getCalls: []func(context.Context, string) (jetstream.KeyValueEntry, error){
				// Key disappeared between Create and Get (stale TTL expiry)
				func(_ context.Context, _ string) (jetstream.KeyValueEntry, error) {
					return nil, errors.New("key not found")
				},
			},
		}

		c := NewClaimer(kv, "worker", 0, 0, 0, nil)
		wid, err := c.Claim(context.Background())
		require.NoError(t, err)
		require.Equal(t, "worker-0", wid)
	})

	t.Run("retries Create after key disappears, loses race to another worker", func(t *testing.T) {
		t.Parallel()

		kv := &mockKV{
			createCalls: []func(context.Context, string, []byte) (uint64, error){
				// First attempt: key exists
				func(_ context.Context, _ string, _ []byte) (uint64, error) {
					return 0, jetstream.ErrKeyExists
				},
				// Retry Create: another worker won
				func(_ context.Context, _ string, _ []byte) (uint64, error) {
					return 0, jetstream.ErrKeyExists
				},
			},
			getCalls: []func(context.Context, string) (jetstream.KeyValueEntry, error){
				// Key disappeared between Create and Get
				func(_ context.Context, _ string) (jetstream.KeyValueEntry, error) {
					return nil, errors.New("key not found")
				},
			},
		}

		c := NewClaimer(kv, "worker", 0, 0, 0, nil)
		_, err := c.Claim(context.Background())
		// Pool exhausted after trying the only available ID (0)
		require.ErrorIs(t, err, ErrNoAvailableID)
	})
}

// TestClaimer_ReclaimsLeakedIDOnTTLZeroBucket is the verify-first reproducer
// for the worker-ID leak. With a bucket whose TTL is 0 (unlimited — an operator
// misconfiguration), a worker that exits ungracefully (no Release) leaves its
// key behind forever. Without the staleness takeover, the next Claim skips the
// leaked key and consumes the next ID, so repeated ungraceful restarts walk
// worker-0, worker-1, worker-2, ... until the pool is exhausted. With the
// takeover, the leaked-but-stale ID is reclaimed instead.
func TestClaimer_ReclaimsLeakedIDOnTTLZeroBucket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Bucket TTL=0: keys never expire.
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "unit-stableid-leak-repro",
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	const ttl = 400 * time.Millisecond

	// First worker claims worker-0, then exits ungracefully (no Release).
	first := NewClaimer(kv, "worker", 0, 9, ttl, nil)
	wid, err := first.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", wid)

	// Wait past the stale threshold so the abandoned key is reclaimable.
	time.Sleep(ttl + 200*time.Millisecond)

	// A restarted worker must reclaim worker-0, NOT leak to worker-1.
	second := NewClaimer(kv, "worker", 0, 9, ttl, nil)
	wid, err = second.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", wid,
		"a restarted worker must reclaim the leaked stable ID, not consume a fresh one")
}

// TestClaimer_isStale verifies the staleness boundary in both threshold
// regimes: with a normal ttl the threshold equals ttl, and with a small ttl it
// is floored at 3×minRenewInterval so a live holder is never judged stale.
func TestClaimer_isStale(t *testing.T) {
	t.Parallel()

	// Normal regime: ttl >= 3×minRenewInterval, so staleThreshold == ttl.
	c := NewClaimer(nil, "worker", 0, 0, time.Second, nil)
	require.True(t, c.isStale(fakeEntry{created: time.Now().Add(-2 * time.Second)}),
		"a key untouched for 2×ttl must be stale")
	require.False(t, c.isStale(fakeEntry{created: time.Now().Add(-200 * time.Millisecond)}),
		"a freshly renewed key must not be stale")

	// Floor regime: ttl below 3×minRenewInterval. The renewal cadence is pinned
	// to minRenewInterval (100ms) and the threshold to 300ms, so a key younger
	// than 300ms is not stale even though it is older than ttl itself.
	small := NewClaimer(nil, "worker", 0, 0, 150*time.Millisecond, nil)
	require.False(t, small.isStale(fakeEntry{created: time.Now().Add(-200 * time.Millisecond)}),
		"with a small ttl the threshold is floored at 3×minRenewInterval, protecting a live holder")
	require.True(t, small.isStale(fakeEntry{created: time.Now().Add(-400 * time.Millisecond)}),
		"a key older than the floored threshold is still stale")
}

// TestClaimer_StaleTakeover_CASRace verifies that when two workers race to take
// over the same stale ID, exactly one wins the Update CAS; the other is denied
// and reports the pool exhausted (single-ID pool).
func TestClaimer_StaleTakeover_CASRace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "unit-stableid-cas-race",
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	const ttl = 400 * time.Millisecond

	// Seed a single, already-stale leaked key.
	_, err = kv.Create(ctx, "worker-0", []byte("leaked"))
	require.NoError(t, err)
	time.Sleep(ttl + 200*time.Millisecond)

	type result struct {
		wid string
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			c := NewClaimer(kv, "worker", 0, 0, ttl, nil)
			wid, err := c.Claim(ctx)
			results <- result{wid, err}
		}()
	}

	r1 := <-results
	r2 := <-results

	wins := 0
	exhausted := 0
	for _, r := range []result{r1, r2} {
		switch {
		case r.err == nil && r.wid == "worker-0":
			wins++
		case errors.Is(r.err, ErrNoAvailableID):
			exhausted++
		default:
			t.Fatalf("unexpected claim result: wid=%q err=%v", r.wid, r.err)
		}
	}
	require.Equal(t, 1, wins, "exactly one worker may win the takeover CAS")
	require.Equal(t, 1, exhausted, "the loser must find the pool exhausted")
}

// TestClaimer_Claim_StaleTakeover covers the takeover decision branches when
// Create reports the key exists and Get returns a still-present entry.
func TestClaimer_Claim_StaleTakeover(t *testing.T) {
	t.Parallel()

	t.Run("fresh key is skipped, not taken over", func(t *testing.T) {
		t.Parallel()
		kv := &mockKV{
			createCalls: []func(context.Context, string, []byte) (uint64, error){
				func(_ context.Context, _ string, _ []byte) (uint64, error) {
					return 0, jetstream.ErrKeyExists
				},
			},
			getCalls: []func(context.Context, string) (jetstream.KeyValueEntry, error){
				func(_ context.Context, _ string) (jetstream.KeyValueEntry, error) {
					return fakeEntry{revision: 5, created: time.Now()}, nil
				},
			},
		}
		// Pool of one ID: a fresh key means no ID is available.
		c := NewClaimer(kv, "worker", 0, 0, time.Second, nil)
		_, err := c.Claim(context.Background())
		require.ErrorIs(t, err, ErrNoAvailableID)
	})

	t.Run("stale key is taken over via Update", func(t *testing.T) {
		t.Parallel()
		kv := &mockKV{
			createCalls: []func(context.Context, string, []byte) (uint64, error){
				func(_ context.Context, _ string, _ []byte) (uint64, error) {
					return 0, jetstream.ErrKeyExists
				},
			},
			getCalls: []func(context.Context, string) (jetstream.KeyValueEntry, error){
				func(_ context.Context, _ string) (jetstream.KeyValueEntry, error) {
					return fakeEntry{revision: 5, created: time.Now().Add(-2 * time.Second)}, nil
				},
			},
			updateCalls: []func(context.Context, string, []byte, uint64) (uint64, error){
				func(_ context.Context, _ string, _ []byte, rev uint64) (uint64, error) {
					require.Equal(t, uint64(5), rev, "takeover must CAS on the entry's revision")
					return 6, nil
				},
			},
		}
		c := NewClaimer(kv, "worker", 0, 0, time.Second, nil)
		wid, err := c.Claim(context.Background())
		require.NoError(t, err)
		require.Equal(t, "worker-0", wid)
		require.Equal(t, uint64(6), c.lastRevision.Load(),
			"takeover must store the Update return revision, not the entry's prior revision — "+
				"otherwise the next renewal CAS would fail and falsely report ErrClaimLost")
	})

	t.Run("stale key, takeover loses the CAS race", func(t *testing.T) {
		t.Parallel()
		kv := &mockKV{
			createCalls: []func(context.Context, string, []byte) (uint64, error){
				func(_ context.Context, _ string, _ []byte) (uint64, error) {
					return 0, jetstream.ErrKeyExists
				},
			},
			getCalls: []func(context.Context, string) (jetstream.KeyValueEntry, error){
				func(_ context.Context, _ string) (jetstream.KeyValueEntry, error) {
					return fakeEntry{revision: 5, created: time.Now().Add(-2 * time.Second)}, nil
				},
			},
			updateCalls: []func(context.Context, string, []byte, uint64) (uint64, error){
				func(_ context.Context, _ string, _ []byte, _ uint64) (uint64, error) {
					return 0, jetstream.ErrKeyExists // another worker won
				},
			},
		}
		// Pool of one ID: losing the CAS exhausts the pool.
		c := NewClaimer(kv, "worker", 0, 0, time.Second, nil)
		_, err := c.Claim(context.Background())
		require.ErrorIs(t, err, ErrNoAvailableID)
	})

	t.Run("stale key, takeover hits an unexpected error", func(t *testing.T) {
		t.Parallel()
		kv := &mockKV{
			createCalls: []func(context.Context, string, []byte) (uint64, error){
				func(_ context.Context, _ string, _ []byte) (uint64, error) {
					return 0, jetstream.ErrKeyExists
				},
			},
			getCalls: []func(context.Context, string) (jetstream.KeyValueEntry, error){
				func(_ context.Context, _ string) (jetstream.KeyValueEntry, error) {
					return fakeEntry{revision: 5, created: time.Now().Add(-2 * time.Second)}, nil
				},
			},
			updateCalls: []func(context.Context, string, []byte, uint64) (uint64, error){
				func(_ context.Context, _ string, _ []byte, _ uint64) (uint64, error) {
					return 0, errors.New("nats: connection closed")
				},
			},
		}
		c := NewClaimer(kv, "worker", 0, 0, time.Second, nil)
		_, err := c.Claim(context.Background())
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrNoAvailableID)
	})
}

// TestClaimer_LiveOldPutRenewerNotTakenOver simulates a rolling upgrade: an old
// binary renews its claim with unconditional Put (no revision check). A new
// claimant must NOT take that ID over while the old renewer is alive — each Put
// advances the key's Created() timestamp, keeping it inside the stale threshold.
func TestClaimer_LiveOldPutRenewerNotTakenOver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "unit-stableid-old-renewer",
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	const ttl = 400 * time.Millisecond // staleThreshold == 400ms

	// An old worker holds worker-0.
	_, err = kv.Create(ctx, "worker-0", []byte("old-owner"))
	require.NoError(t, err)

	// Old-style renewal: unconditional Put, well inside the stale threshold.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(ttl / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_, _ = kv.Put(ctx, "worker-0", []byte("old-owner-renewed"))
			}
		}
	}()
	defer func() { close(stop); <-done }()

	// Let several old-style renewals land, then a new claimant scans the pool.
	time.Sleep(ttl + 100*time.Millisecond)
	c := NewClaimer(kv, "worker", 0, 1, ttl, nil)
	wid, err := c.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-1", wid,
		"a live old Put-renewer must not be taken over; the new claimant takes the next ID")
}

// TestClaimer_WireContract_RevisionMismatchIsErrKeyExists pins the nats.go wire
// contract the renewal and Release hardening rely on: a revision mismatch from
// Update, and a LastRevision mismatch from Delete, both satisfy
// errors.Is(err, jetstream.ErrKeyExists). A future nats.go change here would
// silently break ErrClaimLost detection and the revision-checked Release.
func TestClaimer_WireContract_RevisionMismatchIsErrKeyExists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "unit-stableid-wire-contract",
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	rev, err := kv.Create(ctx, "k", []byte("v1"))
	require.NoError(t, err)
	// Advance the revision so rev is now stale.
	_, err = kv.Put(ctx, "k", []byte("v2"))
	require.NoError(t, err)

	_, updErr := kv.Update(ctx, "k", []byte("v3"), rev)
	require.ErrorIs(t, updErr, jetstream.ErrKeyExists,
		"Update with a stale revision must surface as ErrKeyExists")

	delErr := kv.Delete(ctx, "k", jetstream.LastRevision(rev))
	require.ErrorIs(t, delErr, jetstream.ErrKeyExists,
		"Delete with a stale LastRevision must surface as ErrKeyExists")
}
