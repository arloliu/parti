package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/internal/stableid"
	partitest "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// Lifecycle-focused tests for Claimer's renewal behavior.

func TestStableID_Renewal_IdempotentStart(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "itest-stable-ids-lifecycle",
		TTL:     1 * time.Second,
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	c := stableid.NewClaimer(kv, "worker", 0, 9, 1*time.Second, nil)
	_, err = c.Claim(ctx)
	require.NoError(t, err)

	require.NoError(t, c.StartRenewal())
	// Second start should return ErrRenewalAlreadyStarted
	err = c.StartRenewal()
	require.ErrorIs(t, err, stableid.ErrRenewalAlreadyStarted)
}

func TestStableID_Release_WithoutStartRenewal(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "itest-stable-ids-lifecycle-2",
		TTL:     1 * time.Second,
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	c := stableid.NewClaimer(kv, "worker", 0, 9, 1*time.Second, nil)
	wid, err := c.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", wid)

	// Release should not block even if StartRenewal wasn't called.
	require.NoError(t, c.Release(ctx))
}

func TestStableID_StartRenewal_ContextCancelDoesNotStopLoop(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "itest-stable-ids-lifecycle-3",
		TTL:     600 * time.Millisecond,
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	c := stableid.NewClaimer(kv, "worker", 0, 9, 600*time.Millisecond, nil)
	wid, err := c.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", wid)

	// startCtx is retained to mirror legacy pattern; not passed anymore.
	_, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	require.NoError(t, c.StartRenewal())
	// Cancel the start context; loop should keep running.
	cancel()

	// Wait beyond TTL; renewal should keep the key alive.
	time.Sleep(800 * time.Millisecond)
	_, err = kv.Get(ctx, "worker-0")
	require.NoError(t, err, "key should be renewed and not expired")

	require.NoError(t, c.Release(ctx))
}

func TestStableID_Close_PreservesKey(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "itest-stable-ids-lifecycle-4",
		TTL:     1 * time.Second,
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	c := stableid.NewClaimer(kv, "worker", 0, 9, 1*time.Second, nil)
	wid, err := c.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", wid)

	require.NoError(t, c.StartRenewal())
	c.Close()

	// After Close, key should still exist (not deleted)
	_, err = kv.Get(ctx, "worker-0")
	require.NoError(t, err)
}
