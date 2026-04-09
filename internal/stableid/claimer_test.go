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
	createIdx   int
	getIdx      int
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
