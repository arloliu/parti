//go:build integration
// +build integration

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

// TestClaimerContextLifecycle_StartupContextCancellation proves the bug:
// If StartRenewal() receives a startup context that gets cancelled after startup,
// the renewal loop stops and the stable ID expires, allowing other workers to claim it.
func TestClaimerContextLifecycle_StartupContextCancellation(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create KV bucket with SHORT TTL for fast testing
	ttl := 3 * time.Second
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{
		Bucket:  "test-stable-id-context-bug",
		History: 1,
		TTL:     ttl,
	})
	require.NoError(t, err)

	// Simulate Manager.Start() behavior with startup context
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStartup()

	// Manager's lifecycle context (should be used for renewal)
	managerCtx, cancelManager := context.WithCancel(context.Background())
	defer cancelManager()

	// Worker 1: Claims ID using startup context (BUG)
	claimer1 := stableid.NewClaimer(kv, "worker", 0, 10, ttl, nil)

	workerID1, err := claimer1.Claim(startupCtx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", workerID1)

	// Start renewal with WRONG context (startup context) - THIS IS THE BUG
	err = claimer1.StartRenewal(startupCtx)
	require.NoError(t, err)

	t.Logf("✅ Worker 1 claimed: %s at T+0s", workerID1)

	// Wait for "startup" to complete, then cancel startup context
	time.Sleep(500 * time.Millisecond)
	cancelStartup() // This simulates startup context being cancelled after Manager.Start() returns
	t.Logf("🔴 Startup context cancelled at T+500ms")

	// Wait for TTL to expire (3 seconds) - if renewal stopped, ID will expire
	t.Logf("⏳ Waiting %v for stable ID to expire (if renewal stopped)...", ttl)
	time.Sleep(ttl + 500*time.Millisecond) // TTL + buffer

	// Worker 2: Try to claim the same ID - should succeed because Worker 1's ID expired
	claimer2 := stableid.NewClaimer(kv, "worker", 0, 10, ttl, nil)

	workerID2, err := claimer2.Claim(managerCtx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", workerID2, "BUG CONFIRMED: Worker 2 claimed same ID as Worker 1!")

	t.Logf("🚨 BUG VERIFIED: Worker 2 claimed same ID '%s' after Worker 1's renewal stopped!", workerID2)

	// Cleanup
	require.NoError(t, claimer2.Release(managerCtx))
}

// TestClaimerContextLifecycle_ManagerContextCorrect proves the fix:
// If StartRenewal() receives the manager's lifecycle context,
// renewal continues even after startup completes, preventing ID expiration.
func TestClaimerContextLifecycle_ManagerContextCorrect(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create KV bucket with SHORT TTL for fast testing
	ttl := 3 * time.Second
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{
		Bucket:  "test-stable-id-context-fix",
		History: 1,
		TTL:     ttl,
	})
	require.NoError(t, err)

	// Simulate Manager.Start() behavior with startup context
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStartup()

	// Manager's lifecycle context (should be used for renewal)
	managerCtx, cancelManager := context.WithCancel(context.Background())
	defer cancelManager()

	// Worker 1: Claims ID using startup context for Claim()
	claimer1 := stableid.NewClaimer(kv, "worker", 0, 10, ttl, nil)

	workerID1, err := claimer1.Claim(startupCtx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", workerID1)

	// Start renewal with CORRECT context (manager context) - THIS IS THE FIX
	err = claimer1.StartRenewal(managerCtx)
	require.NoError(t, err)

	t.Logf("✅ Worker 1 claimed: %s at T+0s", workerID1)

	// Wait for "startup" to complete, then cancel startup context
	time.Sleep(500 * time.Millisecond)
	cancelStartup() // Startup context cancelled, but renewal should continue
	t.Logf("✅ Startup context cancelled at T+500ms (renewal should continue)")

	// Wait longer than TTL to verify renewal is working
	t.Logf("⏳ Waiting %v (longer than TTL) to verify renewal keeps ID alive...", ttl+time.Second)
	time.Sleep(ttl + time.Second)

	// Worker 2: Try to claim the same ID - should FAIL because Worker 1's renewal is still working
	claimer2 := stableid.NewClaimer(kv, "worker", 0, 0, ttl, nil) // Only try ID 0

	workerID2, err := claimer2.Claim(managerCtx)
	require.Error(t, err, "Worker 2 should fail to claim worker-0 because Worker 1 is still renewing it")
	require.Equal(t, stableid.ErrNoAvailableID, err)
	require.Equal(t, "", workerID2)

	t.Logf("✅ FIX VERIFIED: Worker 2 cannot claim worker-0 because renewal kept it alive!")

	// Cleanup
	require.NoError(t, claimer1.Release(managerCtx))
}

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
	err = claimer.StartRenewal(ctx)
	require.NoError(t, err)

	t.Logf("✅ Claimed: %s, expecting renewal every ~%v", workerID, expectedInterval)

	// Watch the key for updates
	key := "worker.0"
	initialEntry, err := kv.Get(ctx, key)
	require.NoError(t, err)
	initialRevision := initialEntry.Revision()

	t.Logf("📊 Initial revision: %d", initialRevision)

	// Wait for 2 renewal intervals (should see 2 updates)
	time.Sleep(2*expectedInterval + 500*time.Millisecond)

	// Check revision increased (renewals happened)
	currentEntry, err := kv.Get(ctx, key)
	require.NoError(t, err)
	currentRevision := currentEntry.Revision()

	t.Logf("📊 Current revision after 2 intervals: %d", currentRevision)

	// Should have at least 2 renewals (revision increased by 2+)
	renewalCount := currentRevision - initialRevision
	require.GreaterOrEqual(t, renewalCount, uint64(2), "Should have at least 2 renewals")

	t.Logf("✅ VERIFIED: %d renewals occurred in 2 intervals", renewalCount)

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
		defer cancelStartup()

		managerCtx, cancel := context.WithCancel(context.Background())
		managerContexts[i] = managerCtx
		cancelFuncs[i] = cancel

		// Create claimer
		claimer := stableid.NewClaimer(kv, "worker", 0, 10, ttl, nil)
		claimers[i] = claimer

		// Claim ID with startup context
		workerID, err := claimer.Claim(startupCtx)
		require.NoError(t, err)
		workerIDs[i] = workerID

		// BUG: Using startup context for renewal (will fail)
		// FIX: Should use managerCtx instead
		err = claimer.StartRenewal(startupCtx) // BUG - using wrong context
		require.NoError(t, err)

		t.Logf("✅ Worker %d claimed: %s", i, workerID)

		// Cancel startup context immediately (simulating fast startup)
		cancelStartup()
	}

	// Verify all workers got unique IDs
	seen := make(map[string]bool)
	for _, id := range workerIDs {
		require.False(t, seen[id], "Duplicate worker ID: %s", id)
		seen[id] = true
	}
	t.Logf("✅ All %d workers have unique IDs", numWorkers)

	// Wait for TTL to expire - if renewal stopped, IDs will become available
	t.Logf("⏳ Waiting %v for stable IDs to expire (if renewal stopped)...", ttl)
	time.Sleep(ttl + time.Second)

	// Try to claim IDs again - if bug exists, should be able to claim previously claimed IDs
	for i := 0; i < numWorkers; i++ {
		newClaimer := stableid.NewClaimer(kv, "worker", 0, 10, ttl, nil)
		newID, err := newClaimer.Claim(context.Background())
		if err == nil {
			t.Logf("🚨 BUG VERIFIED: New claimer claimed previously used ID: %s", newID)
			// Check if this ID was previously claimed
			for j, oldID := range workerIDs {
				if oldID == newID {
					t.Logf("   ⚠️  This ID belonged to Worker %d!", j)
					break
				}
			}
			require.NoError(t, newClaimer.Release(context.Background()))
		}
	}

	// Cleanup
	for i, cancel := range cancelFuncs {
		if claimers[i] != nil {
			_ = claimers[i].Release(context.Background())
		}
		cancel()
	}
}
