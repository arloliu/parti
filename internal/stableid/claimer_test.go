package stableid

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	partitest "github.com/arloliu/parti/testing"
)

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
