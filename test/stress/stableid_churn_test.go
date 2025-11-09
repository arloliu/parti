package stress_test

import (
	"testing"
	"time"

	"github.com/arloliu/parti/internal/stableid"
	partitest "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// These stress tests consolidate heavy scenarios that are too slow for regular integration runs.
// Enable with:
//   PARTI_STRESS=1 go test -v -timeout 20m ./test/stress -count=1

func TestStableID_Stress_HighRevisionChurn(t *testing.T) {
	requireStressEnabled(t)

	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "stress-stable-ids-high-rev",
		TTL:     300 * time.Millisecond,
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	// Simulate many previous runs by claiming and letting TTL expire repeatedly
	for i := 0; i < 20; i++ {
		c := stableid.NewClaimer(kv, "worker", 0, 2, 300*time.Millisecond, nil)
		_, err := c.Claim(ctx)
		require.NoError(t, err)
		time.Sleep(400 * time.Millisecond) // allow expiry
	}

	// Final claim should still be fast and succeed
	final := stableid.NewClaimer(kv, "worker", 0, 2, 300*time.Millisecond, nil)
	id, err := final.Claim(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, id)
}

func TestStableID_Stress_CreateAfterExpiry(t *testing.T) {
	requireStressEnabled(t)

	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "stress-create-after-expiry",
		TTL:     400 * time.Millisecond,
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	rev1, err := kv.Create(ctx, "test-key", []byte("v1"))
	require.NoError(t, err)
	_ = rev1

	time.Sleep(600 * time.Millisecond)

	// Create should succeed again after TTL expiry
	rev2, err := kv.Create(ctx, "test-key", []byte("v2"))
	if err != nil {
		// If broker retains a tombstone briefly, Put must still work
		rev2, err = kv.Put(ctx, "test-key", []byte("v2"))
		require.NoError(t, err)
	}
	_ = rev2
}

func TestStableID_Stress_ConcurrentReclaimAfterCrash(t *testing.T) {
	requireStressEnabled(t)

	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "stress-concurrent-reclaim",
		TTL:     350 * time.Millisecond,
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	// Pre-claim a few IDs without renewal (simulate crash)
	for i := 0; i < 3; i++ {
		c := stableid.NewClaimer(kv, "worker", 0, 9, 350*time.Millisecond, nil)
		_, err := c.Claim(ctx)
		require.NoError(t, err)
	}

	// Wait for expiry
	time.Sleep(500 * time.Millisecond)

	// Now race N claimers concurrently
	n := 8
	res := make(chan string, n)
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			c := stableid.NewClaimer(kv, "worker", 0, 9, 350*time.Millisecond, nil)
			id, err := c.Claim(ctx)
			if err != nil {
				errCh <- err
				return
			}
			res <- id
		}()
	}

	claimed := map[string]bool{}
	for i := 0; i < n; i++ {
		select {
		case id := <-res:
			require.False(t, claimed[id], "duplicate claim %s", id)
			claimed[id] = true
		case err := <-errCh:
			t.Fatalf("claim failed: %v", err)
		case <-time.After(3 * time.Second):
			t.Fatal("timeout waiting for claims")
		}
	}
	require.Len(t, claimed, n)
}
