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
