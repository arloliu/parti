package stableid

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	partitest "github.com/arloliu/parti/testing"
)

// TestNATSKV_PutResetsTTL verifies that Put() resets the TTL just like Update() does.
// This is critical for stable ID renewal - we need to ensure Put() keeps the ID alive.
func TestNATSKV_PutResetsTTL(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)

	ctx := context.Background()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create KV bucket with 500ms TTL for faster test
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "test-ttl",
		TTL:     500 * time.Millisecond,
		History: 1,
	})
	require.NoError(t, err)

	t.Run("Put resets TTL", func(t *testing.T) {
		// Create initial entry
		_, err := kv.Create(ctx, "test-key", []byte("initial"))
		require.NoError(t, err)

		// Wait 250ms (half of TTL)
		time.Sleep(250 * time.Millisecond)

		// Use Put() to update the value
		_, err = kv.Put(ctx, "test-key", []byte("updated-1"))
		require.NoError(t, err)

		// Wait another 400ms (would exceed original TTL)
		time.Sleep(400 * time.Millisecond)

		// Entry should still exist because Put() reset the TTL
		entry, err := kv.Get(ctx, "test-key")
		require.NoError(t, err, "Put() should have reset TTL, entry should still exist")
		require.Equal(t, "updated-1", string(entry.Value()))

		t.Log("Put() successfully reset TTL")
	})

	t.Run("Update resets TTL", func(t *testing.T) {
		// Create initial entry
		rev, err := kv.Create(ctx, "test-key-2", []byte("initial"))
		require.NoError(t, err)

		// Wait 250ms (half of TTL)
		time.Sleep(250 * time.Millisecond)

		// Use Update() to update the value
		_, err = kv.Update(ctx, "test-key-2", []byte("updated-2"), rev)
		require.NoError(t, err)

		// Wait another 400ms (would exceed original TTL)
		time.Sleep(400 * time.Millisecond)

		// Entry should still exist because Update() reset the TTL
		entry, err := kv.Get(ctx, "test-key-2")
		require.NoError(t, err, "Update() should have reset TTL, entry should still exist")
		require.Equal(t, "updated-2", string(entry.Value()))

		t.Log("Update() successfully reset TTL")
	})

	t.Run("Without Put/Update - entry expires", func(t *testing.T) {
		// Create initial entry
		_, err := kv.Create(ctx, "test-key-3", []byte("initial"))
		require.NoError(t, err)

		// Wait for TTL to expire (700ms to be safe, TTL is 500ms)
		time.Sleep(700 * time.Millisecond)

		// Entry should be gone
		_, err = kv.Get(ctx, "test-key-3")
		require.Error(t, err, "Entry should have expired after TTL")
		require.ErrorIs(t, err, jetstream.ErrKeyNotFound)

		t.Log("Entry correctly expired after TTL without renewal")
	})
}

// TestNATSKV_PutVsUpdateRevision verifies the revision behavior difference.
func TestNATSKV_PutVsUpdateRevision(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)

	ctx := context.Background()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "test-revision",
		History: 5,
	})
	require.NoError(t, err)

	t.Run("Put always succeeds - ignores revision", func(t *testing.T) {
		// Create initial entry
		rev1, err := kv.Create(ctx, "key-put", []byte("v1"))
		require.NoError(t, err)

		// Put with any value succeeds
		rev2, err := kv.Put(ctx, "key-put", []byte("v2"))
		require.NoError(t, err)
		require.Greater(t, rev2, rev1, "Put should increment revision")

		// Put again - still succeeds
		rev3, err := kv.Put(ctx, "key-put", []byte("v3"))
		require.NoError(t, err)
		require.Greater(t, rev3, rev2, "Put should increment revision again")

		t.Log("Put() always succeeds regardless of current revision")
	})

	t.Run("Update requires correct revision", func(t *testing.T) {
		// Create initial entry
		rev1, err := kv.Create(ctx, "key-update", []byte("v1"))
		require.NoError(t, err)

		// Update with correct revision succeeds
		rev2, err := kv.Update(ctx, "key-update", []byte("v2"), rev1)
		require.NoError(t, err)
		require.Greater(t, rev2, rev1)

		// Update with old revision fails
		_, err = kv.Update(ctx, "key-update", []byte("v3"), rev1)
		require.Error(t, err, "Update with old revision should fail")

		// Update with correct revision succeeds
		rev3, err := kv.Update(ctx, "key-update", []byte("v3"), rev2)
		require.NoError(t, err)
		require.Greater(t, rev3, rev2)

		t.Log("Update() requires correct revision")
	})
}
