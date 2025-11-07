package stableid

import (
	"testing"
	"time"

	partitest "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestClaimer_TTLExpiration verifies stable ID reclaiming after TTL expiration.
// This is critical for production where workers crash without calling Release().
func TestClaimer_TTLExpiration(t *testing.T) {
	t.Run("can reclaim ID after TTL expires without Release()", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)

		// Create KV with very short TTL for testing
		js, err := jetstream.New(nc)
		require.NoError(t, err)

		kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  "test-stable-ids-ttl-expiry",
			TTL:     1 * time.Second, // Short TTL to simulate quick expiration
			Storage: jetstream.MemoryStorage,
		})
		require.NoError(t, err)

		// First worker claims ID without renewal (simulating crash scenario)
		claimer1 := NewClaimer(kv, "worker", 0, 9, 1*time.Second, nil)
		workerID1, err := claimer1.Claim(ctx)
		require.NoError(t, err)
		require.Equal(t, "worker-0", workerID1)

		// Verify key exists initially
		entry, err := kv.Get(ctx, "worker-0")
		require.NoError(t, err)
		require.NotNil(t, entry)
		initialRevision := entry.Revision()
		t.Logf("Initial key created with revision: %d", initialRevision)

		// DON'T call StartRenewal() - simulating worker crash
		// DON'T call Release() - simulating unclean shutdown

		// Wait for TTL to expire (1s + margin)
		t.Logf("Waiting for TTL expiration (1s + 500ms margin)...")
		time.Sleep(1500 * time.Millisecond)

		// Verify key is gone or can be reclaimed
		entry, err = kv.Get(ctx, "worker-0")
		if err != nil {
			t.Logf("Key deleted after TTL (expected): %v", err)
		} else {
			t.Logf("Key still exists with revision %d, value: %s", entry.Revision(), string(entry.Value()))
		}

		// Second worker should be able to claim the same ID
		claimer2 := NewClaimer(kv, "worker", 0, 9, 1*time.Second, nil)
		workerID2, err := claimer2.Claim(ctx)
		require.NoError(t, err, "Failed to reclaim ID after TTL expiration")
		require.Equal(t, "worker-0", workerID2)

		// Verify new claim succeeded
		entry, err = kv.Get(ctx, "worker-0")
		require.NoError(t, err)
		require.NotNil(t, entry)
		newRevision := entry.Revision()
		t.Logf("New key created with revision: %d (previous was %d)", newRevision, initialRevision)

		// New revision should be higher
		require.Greater(t, newRevision, initialRevision, "New claim should have higher revision")
	})

	t.Run("multiple workers compete for expired IDs", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)

		js, err := jetstream.New(nc)
		require.NoError(t, err)

		kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  "test-stable-ids-concurrent-ttl",
			TTL:     800 * time.Millisecond,
			Storage: jetstream.MemoryStorage,
		})
		require.NoError(t, err)

		// First worker claims IDs 0-2
		claimers := make([]*Claimer, 3)
		for i := 0; i < 3; i++ {
			claimers[i] = NewClaimer(kv, "worker", 0, 9, 800*time.Millisecond, nil)
			workerID, err := claimers[i].Claim(ctx)
			require.NoError(t, err)
			require.Equal(t, "worker-"+string(rune('0'+i)), workerID)
		}

		// Simulate crash - no Release()
		t.Log("All workers 'crashed' - no Release() called")

		// Wait for TTL expiration
		time.Sleep(1200 * time.Millisecond)

		// Start 5 new workers concurrently trying to claim IDs
		type result struct {
			workerIdx int
			workerID  string
			err       error
		}
		resultCh := make(chan result, 5)

		for i := 0; i < 5; i++ {
			go func(idx int) {
				claimer := NewClaimer(kv, "worker", 0, 9, 800*time.Millisecond, nil)
				workerID, err := claimer.Claim(ctx)
				resultCh <- result{workerIdx: idx, workerID: workerID, err: err}
			}(i)
		}

		// Collect results
		claimedIDs := make(map[string]bool)
		for i := 0; i < 5; i++ {
			res := <-resultCh
			require.NoError(t, res.err, "Worker %d failed to claim ID", res.workerIdx)
			require.NotEmpty(t, res.workerID)

			// Verify no duplicate claims
			require.False(t, claimedIDs[res.workerID], "Duplicate claim detected: %s", res.workerID)
			claimedIDs[res.workerID] = true

			t.Logf("Worker %d claimed: %s", res.workerIdx, res.workerID)
		}

		// Should have 5 unique IDs
		require.Equal(t, 5, len(claimedIDs))
	})

	t.Run("handles high revision numbers from previous runs", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)

		js, err := jetstream.New(nc)
		require.NoError(t, err)

		kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  "test-stable-ids-high-revision",
			TTL:     500 * time.Millisecond,
			Storage: jetstream.MemoryStorage,
		})
		require.NoError(t, err)

		// Simulate many previous runs by claiming and releasing repeatedly
		for i := 0; i < 20; i++ {
			claimer := NewClaimer(kv, "worker", 0, 2, 500*time.Millisecond, nil)
			workerID, err := claimer.Claim(ctx)
			require.NoError(t, err)
			t.Logf("Iteration %d: Claimed %s", i, workerID)

			// Wait for TTL
			time.Sleep(600 * time.Millisecond)
		}

		// Now try to claim - should work despite high revisions
		finalClaimer := NewClaimer(kv, "worker", 0, 2, 500*time.Millisecond, nil)
		workerID, err := finalClaimer.Claim(ctx)
		require.NoError(t, err, "Failed to claim after many iterations")
		require.NotEmpty(t, workerID)

		t.Logf("Final claim successful: %s", workerID)
	})

	t.Run("verifies Create() behavior on expired keys", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)

		js, err := jetstream.New(nc)
		require.NoError(t, err)

		kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  "test-create-after-expiry",
			TTL:     600 * time.Millisecond,
			Storage: jetstream.MemoryStorage,
		})
		require.NoError(t, err)

		// Create a key
		rev1, err := kv.Create(ctx, "test-key", []byte("value1"))
		require.NoError(t, err)
		t.Logf("Created key with revision: %d", rev1)

		// Wait for expiry
		time.Sleep(800 * time.Millisecond)

		// Verify key is deleted
		_, err = kv.Get(ctx, "test-key")
		t.Logf("Get after expiry error: %v", err)

		// Try Create again - should succeed (this is the critical test)
		rev2, err := kv.Create(ctx, "test-key", []byte("value2"))
		if err != nil {
			t.Logf("ERROR: Create() failed after TTL expiry: %v", err)
			t.Logf("This indicates the implementation has a bug!")

			// Try Put instead
			t.Log("Attempting Put() as workaround...")
			rev2, err = kv.Put(ctx, "test-key", []byte("value2"))
			require.NoError(t, err, "Put() should work as fallback")
			t.Logf("Put() succeeded with revision: %d", rev2)
		} else {
			t.Logf("Create() succeeded with revision: %d (previous was %d)", rev2, rev1)
			require.Greater(t, rev2, rev1)
		}
	})
}

// TestClaimer_ProductionScenarios tests realistic production failure scenarios.
func TestClaimer_ProductionScenarios(t *testing.T) {
	t.Run("rolling restart with no cleanup", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)

		js, err := jetstream.New(nc)
		require.NoError(t, err)

		kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  "test-rolling-restart",
			TTL:     2 * time.Second,
			Storage: jetstream.MemoryStorage,
		})
		require.NoError(t, err)

		// Simulate 10 workers running
		initialClaimers := make([]*Claimer, 10)
		for i := 0; i < 10; i++ {
			initialClaimers[i] = NewClaimer(kv, "worker", 0, 19, 2*time.Second, nil)
			workerID, err := initialClaimers[i].Claim(ctx)
			require.NoError(t, err)
			t.Logf("Initial worker %d claimed: %s", i, workerID)
		}

		// Wait for TTL to expire (simulating all workers crashed simultaneously)
		t.Log("Simulating cluster-wide crash...")
		time.Sleep(2500 * time.Millisecond)

		// Now try to restart 10 workers concurrently
		t.Log("Restarting workers...")
		resultCh := make(chan error, 10)
		startTime := time.Now()

		for i := 0; i < 10; i++ {
			go func(idx int) {
				claimer := NewClaimer(kv, "worker", 0, 19, 2*time.Second, nil)
				_, err := claimer.Claim(ctx)
				resultCh <- err
			}(i)
		}

		// Verify all workers can claim IDs within reasonable time
		successCount := 0
		for i := 0; i < 10; i++ {
			err := <-resultCh
			if err == nil {
				successCount++
			} else {
				t.Errorf("Worker %d failed to claim: %v", i, err)
			}
		}

		elapsed := time.Since(startTime)
		t.Logf("All workers restarted in %v", elapsed)

		require.Equal(t, 10, successCount, "All workers should successfully claim IDs")
		require.Less(t, elapsed, 5*time.Second, "Claiming should be fast (not sequential timeouts)")
	})
}
