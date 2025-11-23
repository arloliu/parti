package integration_test

import (
	"testing"
	"time"

	"github.com/arloliu/parti/internal/stableid"
	partitest "github.com/arloliu/parti/testing"
	"github.com/stretchr/testify/require"
)

// These tests validate stable ID claiming behavior against a real embedded NATS+JetStream.
// They are integration tests by design (external dependency, timing/TTL interactions).
func TestStableID_Claim_Renew_Release(t *testing.T) {
	t.Parallel()

	t.Run("claims first available ID then renews and releases", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()

		// Embedded NATS with JetStream
		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "itest-stable-ids-basic")

		// Claim
		claimer := stableid.NewClaimer(kv, "worker", 0, 9, 2*time.Second, nil)
		workerID, err := claimer.Claim(ctx)
		require.NoError(t, err)
		require.Equal(t, "worker-0", workerID)

		// Start renewal
		require.NoError(t, claimer.StartRenewal())
		time.Sleep(200 * time.Millisecond)

		// Release
		require.NoError(t, claimer.Release(ctx))
		require.Empty(t, claimer.WorkerID())
	})
}

func TestStableID_ConcurrentClaiming(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "itest-stable-ids-concurrent")

	num := 5
	results := make(chan string, num)
	errs := make(chan error, num)

	for i := 0; i < num; i++ {
		go func() {
			c := stableid.NewClaimer(kv, "worker", 0, 9, 2*time.Second, nil)
			id, err := c.Claim(ctx)
			if err != nil {
				errs <- err
				return
			}
			results <- id
		}()
	}

	got := map[string]bool{}
	for i := 0; i < num; i++ {
		select {
		case id := <-results:
			require.False(t, got[id], "duplicate id %s", id)
			got[id] = true
		case err := <-errs:
			t.Fatalf("claim failed: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for claims")
		}
	}
	require.Len(t, got, num)
}
