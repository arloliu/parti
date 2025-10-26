package stableid

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	partitest "github.com/arloliu/parti/testing"
)

func TestClaimer_Claim(t *testing.T) {
	t.Run("claims first available ID", func(t *testing.T) {
		ctx := t.Context()

		// Setup embedded NATS
		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-stable-ids")

		claimer := NewClaimer(kv, "worker", 0, 9, 30*time.Second)

		workerID, err := claimer.Claim(ctx)
		require.NoError(t, err)
		require.Equal(t, "worker-0", workerID)
		require.Equal(t, "worker-0", claimer.WorkerID())
	})

	t.Run("claims next available ID when first is taken", func(t *testing.T) {
		ctx := t.Context()

		// Setup embedded NATS
		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-stable-ids-2")

		// Claim worker-0 first
		claimer1 := NewClaimer(kv, "worker", 0, 9, 30*time.Second)
		workerID1, err := claimer1.Claim(ctx)
		require.NoError(t, err)
		require.Equal(t, "worker-0", workerID1)

		// Second claimer should get worker-1
		claimer2 := NewClaimer(kv, "worker", 0, 9, 30*time.Second)
		workerID2, err := claimer2.Claim(ctx)
		require.NoError(t, err)
		require.Equal(t, "worker-1", workerID2)
	})

	t.Run("returns error when pool exhausted", func(t *testing.T) {
		ctx := t.Context()

		// Setup embedded NATS
		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-stable-ids-3")

		// Create small pool (0-1) and claim both IDs
		claimer1 := NewClaimer(kv, "worker", 0, 1, 30*time.Second)
		_, err := claimer1.Claim(ctx)
		require.NoError(t, err)

		claimer2 := NewClaimer(kv, "worker", 0, 1, 30*time.Second)
		_, err = claimer2.Claim(ctx)
		require.NoError(t, err)

		// Third claimer should fail
		claimer3 := NewClaimer(kv, "worker", 0, 1, 30*time.Second)
		_, err = claimer3.Claim(ctx)
		require.ErrorIs(t, err, ErrNoAvailableID)
	})

	t.Run("respects context cancellation", func(t *testing.T) {
		ctx := t.Context()

		// Setup embedded NATS
		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-stable-ids-4")

		claimer := NewClaimer(kv, "worker", 0, 100, 30*time.Second)

		// Cancel context immediately
		ctx, cancel := context.WithCancel(ctx)
		cancel()

		_, err := claimer.Claim(ctx)
		require.Error(t, err)
	})
}

func TestClaimer_Renewal(t *testing.T) {
	t.Run("renews ID periodically", func(t *testing.T) {
		ctx := t.Context()

		// Setup embedded NATS with short TTL
		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-stable-ids-renewal")

		claimer := NewClaimer(kv, "worker", 0, 9, 2*time.Second)
		workerID, err := claimer.Claim(ctx)
		require.NoError(t, err)

		// Start renewal
		err = claimer.StartRenewal(ctx)
		require.NoError(t, err)

		// Wait for at least one renewal cycle
		time.Sleep(1 * time.Second)

		// Verify key still exists (should be renewed)
		entry, err := kv.Get(ctx, workerID)
		require.NoError(t, err)
		require.NotNil(t, entry)

		// Cleanup
		err = claimer.Release(ctx)
		require.NoError(t, err)
	})

	t.Run("returns error if called before claim", func(t *testing.T) {
		ctx := t.Context()

		// Setup embedded NATS
		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-stable-ids-renewal-2")

		claimer := NewClaimer(kv, "worker", 0, 9, 30*time.Second)

		err := claimer.StartRenewal(ctx)
		require.ErrorIs(t, err, ErrNotClaimed)
	})
}

func TestClaimer_Release(t *testing.T) {
	t.Run("releases claimed ID", func(t *testing.T) {
		ctx := t.Context()

		// Setup embedded NATS
		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-stable-ids-release")

		claimer := NewClaimer(kv, "worker", 0, 9, 30*time.Second)
		workerID, err := claimer.Claim(ctx)
		require.NoError(t, err)

		// Start renewal
		err = claimer.StartRenewal(ctx)
		require.NoError(t, err)

		// Release
		err = claimer.Release(ctx)
		require.NoError(t, err)
		require.Empty(t, claimer.WorkerID())

		// Verify key is deleted
		_, err = kv.Get(ctx, workerID)
		require.Error(t, err)
	})

	t.Run("allows ID to be reclaimed after release", func(t *testing.T) {
		ctx := t.Context()

		// Setup embedded NATS
		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-stable-ids-release-2")

		// First claimer
		claimer1 := NewClaimer(kv, "worker", 0, 9, 30*time.Second)
		workerID1, err := claimer1.Claim(ctx)
		require.NoError(t, err)
		require.Equal(t, "worker-0", workerID1)

		// Release it
		err = claimer1.Release(ctx)
		require.NoError(t, err)

		// Second claimer should be able to claim the same ID
		claimer2 := NewClaimer(kv, "worker", 0, 9, 30*time.Second)
		workerID2, err := claimer2.Claim(ctx)
		require.NoError(t, err)
		require.Equal(t, "worker-0", workerID2)
	})

	t.Run("returns error if not claimed", func(t *testing.T) {
		ctx := t.Context()

		// Setup embedded NATS
		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-stable-ids-release-3")

		claimer := NewClaimer(kv, "worker", 0, 9, 30*time.Second)

		err := claimer.Release(ctx)
		require.ErrorIs(t, err, ErrNotClaimed)
	})
}

func TestClaimer_ConcurrentClaiming(t *testing.T) {
	t.Run("multiple claimers get different IDs", func(t *testing.T) {
		ctx := t.Context()

		// Setup embedded NATS
		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-stable-ids-concurrent")

		// Create 5 claimers concurrently
		numClaimers := 5
		results := make(chan string, numClaimers)
		errs := make(chan error, numClaimers)

		for range numClaimers {
			go func() {
				claimer := NewClaimer(kv, "worker", 0, 9, 30*time.Second)
				workerID, err := claimer.Claim(ctx)
				if err != nil {
					errs <- err
					return
				}
				results <- workerID
			}()
		}

		// Collect results
		claimedIDs := make(map[string]bool)
		for range numClaimers {
			select {
			case id := <-results:
				require.False(t, claimedIDs[id], "ID %s claimed twice", id)
				claimedIDs[id] = true
			case err := <-errs:
				t.Fatalf("Claim failed: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("Timeout waiting for claims")
			}
		}

		require.Len(t, claimedIDs, numClaimers, "Should have claimed %d unique IDs", numClaimers)
	})
}
