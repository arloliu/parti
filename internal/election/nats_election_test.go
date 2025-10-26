package election

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	partitest "github.com/arloliu/parti/testing"
)

func TestNATSElection_RequestLeadership(t *testing.T) {
	t.Run("acquires leadership when no leader exists", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-1")

		election := NewNATSElection(kv, "leader")

		isLeader, err := election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)
		require.Equal(t, "worker-1", election.WorkerID())
	})

	t.Run("fails when another worker is leader", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-2")

		// First worker becomes leader
		election1 := NewNATSElection(kv, "leader")
		isLeader, err := election1.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Second worker tries to become leader
		election2 := NewNATSElection(kv, "leader")
		isLeader, err = election2.RequestLeadership(ctx, "worker-2", 30)
		require.NoError(t, err)
		require.False(t, isLeader)
	})

	t.Run("renews leadership if already leader", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-3")

		election := NewNATSElection(kv, "leader")

		// Acquire leadership
		isLeader, err := election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Request again (should renew)
		isLeader, err = election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)
	})

	t.Run("returns error for invalid lease duration", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-4")

		election := NewNATSElection(kv, "leader")

		isLeader, err := election.RequestLeadership(ctx, "worker-1", 0)
		require.ErrorIs(t, err, ErrInvalidDuration)
		require.False(t, isLeader)
	})
}

func TestNATSElection_RenewLeadership(t *testing.T) {
	t.Run("renews leadership successfully", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-renew-1")

		election := NewNATSElection(kv, "leader")

		// Acquire leadership
		isLeader, err := election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Renew leadership
		err = election.RenewLeadership(ctx)
		require.NoError(t, err)
	})

	t.Run("fails if not the leader", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-renew-2")

		election := NewNATSElection(kv, "leader")

		err := election.RenewLeadership(ctx)
		require.ErrorIs(t, err, ErrNotLeader)
	})

	t.Run("fails if leadership was lost", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-renew-3")

		election := NewNATSElection(kv, "leader")

		// Acquire leadership
		isLeader, err := election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Another process takes over (simulate by deleting and recreating)
		err = kv.Delete(ctx, "leader")
		require.NoError(t, err)

		// Try to renew - should fail
		err = election.RenewLeadership(ctx)
		require.ErrorIs(t, err, ErrLeadershipLost)
	})
}

func TestNATSElection_ReleaseLeadership(t *testing.T) {
	t.Run("releases leadership successfully", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-release-1")

		election := NewNATSElection(kv, "leader")

		// Acquire leadership
		isLeader, err := election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Release leadership
		err = election.ReleaseLeadership(ctx)
		require.NoError(t, err)
		require.Empty(t, election.WorkerID())

		// Verify key is deleted
		_, err = kv.Get(ctx, "leader")
		require.Error(t, err)
		require.ErrorIs(t, err, jetstream.ErrKeyNotFound)
	})

	t.Run("fails if not the leader", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-release-2")

		election := NewNATSElection(kv, "leader")

		err := election.ReleaseLeadership(ctx)
		require.ErrorIs(t, err, ErrNotLeader)
	})

	t.Run("allows another worker to become leader", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-release-3")

		// First worker becomes leader
		election1 := NewNATSElection(kv, "leader")
		isLeader, err := election1.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Release leadership
		err = election1.ReleaseLeadership(ctx)
		require.NoError(t, err)

		// Second worker can now become leader
		election2 := NewNATSElection(kv, "leader")
		isLeader, err = election2.RequestLeadership(ctx, "worker-2", 30)
		require.NoError(t, err)
		require.True(t, isLeader)
	})
}

func TestNATSElection_IsLeader(t *testing.T) {
	t.Run("returns true when leader", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-isleader-1")

		election := NewNATSElection(kv, "leader")

		// Acquire leadership
		isLeader, err := election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Check leadership
		isLeader, err = election.IsLeader(ctx)
		require.NoError(t, err)
		require.True(t, isLeader)
	})

	t.Run("returns false when not leader", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-isleader-2")

		election := NewNATSElection(kv, "leader")

		isLeader, err := election.IsLeader(ctx)
		require.NoError(t, err)
		require.False(t, isLeader)
	})

	t.Run("returns false when key was deleted", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-isleader-3")

		election := NewNATSElection(kv, "leader")

		// Acquire leadership
		isLeader, err := election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Delete the key (simulate another process taking over)
		err = kv.Delete(ctx, "leader")
		require.NoError(t, err)

		// Check leadership - should detect we lost it
		isLeader, err = election.IsLeader(ctx)
		require.NoError(t, err)
		require.False(t, isLeader)
	})

	t.Run("returns false when revision changed", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-isleader-4")

		election := NewNATSElection(kv, "leader")

		// Acquire leadership
		isLeader, err := election.RequestLeadership(ctx, "worker-1", 30)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Another process takes over
		err = kv.Delete(ctx, "leader")
		require.NoError(t, err)
		_, err = kv.Create(ctx, "leader", []byte("worker-2"))
		require.NoError(t, err)

		// Check leadership - should detect revision changed
		isLeader, err = election.IsLeader(ctx)
		require.NoError(t, err)
		require.False(t, isLeader)
	})
}

func TestNATSElection_LeadershipFailover(t *testing.T) {
	t.Run("automatic failover on TTL expiry", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)

		// Create KV with short TTL for testing
		js, err := jetstream.New(nc)
		require.NoError(t, err)

		kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  "test-election-failover",
			TTL:     2 * time.Second, // Short TTL for testing
			Storage: jetstream.MemoryStorage,
		})
		require.NoError(t, err)

		// Worker 1 becomes leader
		election1 := NewNATSElection(kv, "leader")
		isLeader, err := election1.RequestLeadership(ctx, "worker-1", 2)
		require.NoError(t, err)
		require.True(t, isLeader)

		// Wait for TTL to expire
		time.Sleep(3 * time.Second)

		// Worker 2 can now become leader
		election2 := NewNATSElection(kv, "leader")
		isLeader, err = election2.RequestLeadership(ctx, "worker-2", 2)
		require.NoError(t, err)
		require.True(t, isLeader)
	})
}

func TestNATSElection_ConcurrentLeadership(t *testing.T) {
	t.Run("only one worker becomes leader", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-election-concurrent")

		numWorkers := 5
		results := make(chan bool, numWorkers)
		errs := make(chan error, numWorkers)

		// Start multiple workers trying to become leader
		for i := range numWorkers {
			go func(workerNum int) {
				election := NewNATSElection(kv, "leader")
				workerID := "worker-" + string(rune('0'+workerNum))
				isLeader, err := election.RequestLeadership(ctx, workerID, 30)
				if err != nil {
					errs <- err
					return
				}
				results <- isLeader
			}(i)
		}

		// Collect results
		leaderCount := 0
		for range numWorkers {
			select {
			case isLeader := <-results:
				if isLeader {
					leaderCount++
				}
			case err := <-errs:
				t.Fatalf("Request leadership failed: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("Timeout waiting for leadership requests")
			}
		}

		// Exactly one worker should be leader
		require.Equal(t, 1, leaderCount, "Expected exactly one leader")
	})
}
