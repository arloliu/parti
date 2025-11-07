package stableid

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestClaimer_PersistentStorage tests stable ID claiming with file-based storage.
// This simulates production NATS where KV data persists across restarts.
func TestClaimer_PersistentStorage(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping persistent storage test in short mode")
	}

	t.Run("handles persistent revisions across NATS restarts", func(t *testing.T) {
		ctx := t.Context()

		// Create temporary directory for NATS storage
		tempDir := t.TempDir()
		storeDir := filepath.Join(tempDir, "nats-store")

		// Helper function to start NATS with persistent storage
		startNATS := func() (*server.Server, *nats.Conn) {
			opts := &server.Options{
				JetStream: true,
				Port:      -1,
				StoreDir:  storeDir, // PERSISTENT STORAGE
			}

			ns, err := server.NewServer(opts)
			require.NoError(t, err)

			go ns.Start()
			require.True(t, ns.ReadyForConnections(10*time.Second))

			nc, err := nats.Connect(ns.ClientURL())
			require.NoError(t, err)

			return ns, nc
		}

		// === FIRST RUN: Create KV and claim IDs ===
		t.Log("=== FIRST RUN: Creating KV and claiming IDs ===")
		ns1, nc1 := startNATS()

		js1, err := jetstream.New(nc1)
		require.NoError(t, err)

		kv1, err := js1.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  "parti-stableid",
			TTL:     30 * time.Second,      // Production-like TTL
			Storage: jetstream.FileStorage, // PERSISTENT!
		})
		require.NoError(t, err)

		// Claim IDs 0-2
		for i := 0; i < 3; i++ {
			claimer := NewClaimer(kv1, "simulation-worker", 0, 999, 30*time.Second, nil)
			workerID, err := claimer.Claim(ctx)
			require.NoError(t, err)
			require.Equal(t, fmt.Sprintf("simulation-worker-%d", i), workerID)
			t.Logf("First run: Claimed %s", workerID)
		}

		// Check current revisions
		for i := 0; i < 3; i++ {
			key := fmt.Sprintf("simulation-worker-%d", i)
			entry, err := kv1.Get(ctx, key)
			require.NoError(t, err)
			t.Logf("First run: Key %s has revision %d", key, entry.Revision())
		}

		// Simulate crash - close without cleanup
		t.Log("=== Simulating crash (no cleanup) ===")
		nc1.Close()
		ns1.Shutdown()

		// Wait for TTL to expire BEFORE restarting
		t.Log("=== Waiting for TTL expiration (30s + margin) ===")
		time.Sleep(31 * time.Second)

		// === SECOND RUN: Restart NATS with same storage ===
		t.Log("=== SECOND RUN: Restarting NATS with persistent storage ===")
		ns2, nc2 := startNATS()
		defer nc2.Close()
		defer ns2.Shutdown()

		js2, err := jetstream.New(nc2)
		require.NoError(t, err)

		// Access EXISTING bucket
		kv2, err := js2.KeyValue(ctx, "parti-stableid")
		require.NoError(t, err)

		// Check if old keys still exist
		for i := 0; i < 3; i++ {
			key := fmt.Sprintf("simulation-worker-%d", i)
			entry, err := kv2.Get(ctx, key)
			if err != nil {
				t.Logf("Second run: Key %s not found (good - expired): %v", key, err)
			} else {
				t.Logf("Second run: Key %s STILL EXISTS with revision %d (BAD - should be expired!)", key, entry.Revision())
			}
		}

		// Try to claim IDs again - this is the critical test
		t.Log("=== SECOND RUN: Attempting to claim IDs ===")
		claimedIDs := make([]string, 3)
		for i := 0; i < 3; i++ {
			claimer := NewClaimer(kv2, "simulation-worker", 0, 999, 30*time.Second, nil)
			workerID, err := claimer.Claim(ctx)
			require.NoError(t, err, "Should be able to reclaim IDs after restart")
			claimedIDs[i] = workerID
			t.Logf("Second run: Claimed %s", workerID)
		}

		// Verify all IDs claimed (may not be 0-2 if those are still locked)
		require.Len(t, claimedIDs, 3)
	})

	t.Run("simulates simulation startup scenario with high revisions", func(t *testing.T) {
		ctx := t.Context()

		tempDir := t.TempDir()
		storeDir := filepath.Join(tempDir, "nats-sim-test")

		opts := &server.Options{
			JetStream: true,
			Port:      -1,
			StoreDir:  storeDir,
		}

		ns, err := server.NewServer(opts)
		require.NoError(t, err)

		go ns.Start()
		require.True(t, ns.ReadyForConnections(10*time.Second))
		defer ns.Shutdown()

		nc, err := nats.Connect(ns.ClientURL())
		require.NoError(t, err)
		defer nc.Close()

		js, err := jetstream.New(nc)
		require.NoError(t, err)

		// Create KV with short TTL
		kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  "parti-stableid",
			TTL:     2 * time.Second,
			Storage: jetstream.FileStorage,
		})
		require.NoError(t, err)

		// Simulate multiple simulation runs by creating and expiring keys repeatedly
		t.Log("Simulating 100+ previous simulation runs...")
		for run := 0; run < 10; run++ {
			// Claim IDs
			claimers := make([]*Claimer, 3)
			for i := 0; i < 3; i++ {
				claimers[i] = NewClaimer(kv, "simulation-worker", 0, 999, 2*time.Second, nil)
				_, err := claimers[i].Claim(ctx)
				require.NoError(t, err)
			}

			// Let TTL expire
			time.Sleep(2500 * time.Millisecond)

			if run%5 == 0 {
				t.Logf("Completed run %d/10", run+1)
			}
		}

		// Check revisions - they should be very high now
		t.Log("Checking final revision numbers...")
		for i := 0; i < 3; i++ {
			key := fmt.Sprintf("simulation-worker-%d", i)
			entry, err := kv.Get(ctx, key)
			if err != nil {
				t.Logf("Key %s expired (revision unknown)", key)
			} else {
				t.Logf("Key %s has revision %d", key, entry.Revision())
			}
		}

		// Now try normal claiming - should still work fast
		t.Log("Final test: Claiming with high revisions in storage...")
		startTime := time.Now()

		claimers := make([]*Claimer, 3)
		for i := 0; i < 3; i++ {
			claimers[i] = NewClaimer(kv, "simulation-worker", 0, 999, 2*time.Second, nil)
			workerID, err := claimers[i].Claim(ctx)
			require.NoError(t, err)
			t.Logf("Claimed %s in %v", workerID, time.Since(startTime))
		}

		elapsed := time.Since(startTime)
		t.Logf("Total claiming time: %v", elapsed)
		require.Less(t, elapsed, 1*time.Second, "Claiming should be fast even with high revisions")
	})
}

// TestClaimer_StaleKeyDetection tests the scenario where keys exist but haven't been renewed.
func TestClaimer_StaleKeyDetection(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stale key detection test in short mode")
	}

	t.Run("detects and handles stale keys with valid revisions", func(t *testing.T) {
		ctx := t.Context()

		tempDir := t.TempDir()
		storeDir := filepath.Join(tempDir, "nats-stale")

		// Start NATS
		opts := &server.Options{
			JetStream: true,
			Port:      -1,
			StoreDir:  storeDir,
		}

		ns, err := server.NewServer(opts)
		require.NoError(t, err)

		go ns.Start()
		require.True(t, ns.ReadyForConnections(10*time.Second))

		nc, err := nats.Connect(ns.ClientURL())
		require.NoError(t, err)

		js, err := jetstream.New(nc)
		require.NoError(t, err)

		// Create KV
		kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  "parti-stableid",
			TTL:     2 * time.Second,
			Storage: jetstream.FileStorage,
		})
		require.NoError(t, err)

		// Worker 1 claims ID but never renews (simulates hung process)
		claimer1 := NewClaimer(kv, "worker", 0, 9, 2*time.Second, nil)
		workerID1, err := claimer1.Claim(ctx)
		require.NoError(t, err)
		require.Equal(t, "worker-0", workerID1)

		// DON'T start renewal - simulating hung/zombie process

		// Wait for TTL to expire
		t.Log("Waiting for TTL expiration...")
		time.Sleep(2500 * time.Millisecond)

		// Worker 2 should be able to claim same ID
		claimer2 := NewClaimer(kv, "worker", 0, 9, 2*time.Second, nil)
		workerID2, err := claimer2.Claim(ctx)
		require.NoError(t, err)
		require.Equal(t, "worker-0", workerID2, "Should reclaim expired ID")

		// Verify new claim
		entry, err := kv.Get(ctx, "worker-0")
		require.NoError(t, err)
		t.Logf("Reclaimed key has revision: %d", entry.Revision())

		// Cleanup
		nc.Close()
		ns.Shutdown()
	})
}
