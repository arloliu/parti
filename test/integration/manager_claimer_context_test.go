package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/internal/stableid"
	partitest "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// The previous tests that verified renewal behavior against startup vs manager contexts
// are no longer necessary: StartRenewal() no longer accepts a context, and its lifecycle
// is controlled via Release()/Close(). Remaining tests focus on renewal timing and
// multi-worker behavior.

// TestClaimerContextLifecycle_RenewalInterval verifies renewal happens at expected intervals.
func TestClaimerContextLifecycle_RenewalInterval(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create KV bucket with SHORT TTL for fast testing
	ttl := 3 * time.Second
	expectedInterval := ttl / 3 // Should renew at 1/3 of TTL = 1 second
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{
		Bucket:  "test-stable-id-renewal-interval",
		History: 10, // Keep history to count renewals
		TTL:     ttl,
	})
	require.NoError(t, err)

	ctx := context.Background()
	claimer := stableid.NewClaimer(kv, "worker", 0, 10, ttl, nil)

	workerID, err := claimer.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", workerID)

	// Start renewal
	err = claimer.StartRenewal()
	require.NoError(t, err)

	t.Logf("Claimed: %s, expecting renewal every ~%v", workerID, expectedInterval)

	// Watch the key for updates
	key := workerID // Use the actual worker ID as the key
	initialEntry, err := kv.Get(ctx, key)
	require.NoError(t, err)
	initialRevision := initialEntry.Revision()

	t.Logf("Initial revision: %d", initialRevision)

	// Wait for 2 renewal intervals (should see 2 updates)
	time.Sleep(2*expectedInterval + 500*time.Millisecond)

	// Check revision increased (renewals happened)
	currentEntry, err := kv.Get(ctx, key)
	require.NoError(t, err)
	currentRevision := currentEntry.Revision()

	t.Logf("Current revision after 2 intervals: %d", currentRevision)

	// Should have at least 2 renewals (revision increased by 2+)
	renewalCount := currentRevision - initialRevision
	require.GreaterOrEqual(t, renewalCount, uint64(2), "Should have at least 2 renewals")

	t.Logf("VERIFIED: %d renewals occurred in 2 intervals", renewalCount)

	// Cleanup
	require.NoError(t, claimer.Release(ctx))
}

// TestClaimerContextLifecycle_MultipleWorkers simulates real-world scenario:
// Multiple workers starting concurrently, each with their own startup and manager contexts.
func TestClaimerContextLifecycle_MultipleWorkers(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create KV bucket
	ttl := 5 * time.Second
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{
		Bucket:  "test-stable-id-multiple-workers",
		History: 1,
		TTL:     ttl,
	})
	require.NoError(t, err)

	numWorkers := 5
	claimers := make([]*stableid.Claimer, numWorkers)
	managerContexts := make([]context.Context, numWorkers)
	cancelFuncs := make([]context.CancelFunc, numWorkers)
	workerIDs := make([]string, numWorkers)

	// Start all workers concurrently (simulating real deployment)
	for i := 0; i < numWorkers; i++ {
		// Each worker has its own startup and manager context
		startupCtx, cancelStartup := context.WithTimeout(context.Background(), 10*time.Second)

		managerCtx, cancel := context.WithCancel(context.Background())
		managerContexts[i] = managerCtx //nolint:fatcontext
		cancelFuncs[i] = cancel

		// Create claimer
		claimer := stableid.NewClaimer(kv, "worker", 0, 10, ttl, nil)
		claimers[i] = claimer

		// Claim ID with startup context
		workerID, err := claimer.Claim(startupCtx)
		require.NoError(t, err)
		cancelStartup()
		workerIDs[i] = workerID

		// Using startup context for renewal is acceptable; loop lifecycle is now independent of ctx
		err = claimer.StartRenewal()
		require.NoError(t, err)

		t.Logf("Worker %d claimed: %s", i, workerID)

		// Cancel startup context immediately (simulating fast startup)
		cancelStartup()
	}

	// Verify all workers got unique IDs
	seen := make(map[string]bool)
	for _, id := range workerIDs {
		require.False(t, seen[id], "Duplicate worker ID: %s", id)
		seen[id] = true
	}
	t.Logf("All %d workers have unique IDs", numWorkers)

	// Wait for TTL to confirm IDs are kept alive by renewal
	t.Logf("Waiting %v to confirm IDs remain renewed...", ttl)
	time.Sleep(ttl + time.Second)

	// Assert that previously claimed IDs cannot be reclaimed: attempt to claim exact same IDs by constraining range
	for i := range numWorkers {
		// Parse numeric suffix from workerIDs[i]
		var idx int
		_, _ = fmt.Sscanf(workerIDs[i], "worker-%d", &idx)
		newClaimer := stableid.NewClaimer(kv, "worker", idx, idx, ttl, nil)
		_, err := newClaimer.Claim(context.Background())
		require.ErrorIs(t, err, stableid.ErrNoAvailableID, "ID %s should still be held by renewal", workerIDs[i])
	}

	// Cleanup
	for i, cancel := range cancelFuncs {
		if claimers[i] != nil {
			_ = claimers[i].Release(context.Background())
		}
		cancel()
	}
}
