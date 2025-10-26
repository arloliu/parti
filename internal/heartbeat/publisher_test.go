package heartbeat

import (
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	partitest "github.com/arloliu/parti/testing"
)

func TestPublisher_SetWorkerID(t *testing.T) {
	t.Run("sets worker ID successfully", func(t *testing.T) {
		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-set-id")

		publisher := New(kv, "worker-hb", 2*time.Second)
		publisher.SetWorkerID("worker-1")

		require.Equal(t, "worker-1", publisher.WorkerID())
	})
}

func TestPublisher_Start(t *testing.T) {
	t.Run("starts successfully and publishes heartbeat", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-start-1")

		publisher := New(kv, "worker-hb", 100*time.Millisecond)
		publisher.SetWorkerID("worker-1")

		err := publisher.Start(ctx)
		require.NoError(t, err)
		require.True(t, publisher.IsStarted())

		// Verify heartbeat was published
		entry, err := kv.Get(ctx, "worker-hb.worker-1")
		require.NoError(t, err)
		require.NotNil(t, entry)

		// Cleanup
		err = publisher.Stop()
		require.NoError(t, err)
	})

	t.Run("returns error if worker ID not set", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-start-2")

		publisher := New(kv, "worker-hb", 2*time.Second)

		err := publisher.Start(ctx)
		require.ErrorIs(t, err, ErrNoWorkerID)
		require.False(t, publisher.IsStarted())
	})

	t.Run("returns error if already started", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-start-3")

		publisher := New(kv, "worker-hb", 2*time.Second)
		publisher.SetWorkerID("worker-1")

		err := publisher.Start(ctx)
		require.NoError(t, err)

		// Try to start again
		err = publisher.Start(ctx)
		require.ErrorIs(t, err, ErrAlreadyStarted)

		// Cleanup
		err = publisher.Stop()
		require.NoError(t, err)
	})
}

func TestPublisher_Stop(t *testing.T) {
	t.Run("stops successfully", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-stop-1")

		publisher := New(kv, "worker-hb", 2*time.Second)
		publisher.SetWorkerID("worker-1")

		err := publisher.Start(ctx)
		require.NoError(t, err)

		err = publisher.Stop()
		require.NoError(t, err)
		require.False(t, publisher.IsStarted())
	})

	t.Run("returns error if not started", func(t *testing.T) {
		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-stop-2")

		publisher := New(kv, "worker-hb", 2*time.Second)

		err := publisher.Stop()
		require.ErrorIs(t, err, ErrNotStarted)
	})
}

func TestPublisher_PeriodicHeartbeats(t *testing.T) {
	t.Run("publishes heartbeats at regular intervals", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-periodic")

		// Use short interval for testing
		publisher := New(kv, "worker-hb", 100*time.Millisecond)
		publisher.SetWorkerID("worker-1")

		err := publisher.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = publisher.Stop() }()

		// Wait for a few heartbeats
		time.Sleep(350 * time.Millisecond)

		// Get the heartbeat
		entry, err := kv.Get(ctx, "worker-hb.worker-1")
		require.NoError(t, err)
		require.NotNil(t, entry)

		// Parse the timestamp
		firstTimestamp, err := time.Parse(time.RFC3339Nano, string(entry.Value()))
		require.NoError(t, err)

		// Wait for another heartbeat
		time.Sleep(150 * time.Millisecond)

		// Get updated heartbeat
		entry, err = kv.Get(ctx, "worker-hb.worker-1")
		require.NoError(t, err)

		secondTimestamp, err := time.Parse(time.RFC3339Nano, string(entry.Value()))
		require.NoError(t, err)

		// Second timestamp should be after first
		require.True(t, secondTimestamp.After(firstTimestamp))
	})
}

func TestPublisher_TTLExpiry(t *testing.T) {
	t.Run("heartbeat expires after TTL", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)

		// Create KV with short TTL for testing
		js, err := jetstream.New(nc)
		require.NoError(t, err)

		kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  "test-hb-ttl",
			TTL:     1 * time.Second, // Very short TTL
			Storage: jetstream.MemoryStorage,
		})
		require.NoError(t, err)

		publisher := New(kv, "worker-hb", 2*time.Second) // Longer than TTL
		publisher.SetWorkerID("worker-1")

		err = publisher.Start(ctx)
		require.NoError(t, err)

		// Verify initial heartbeat exists
		entry, err := kv.Get(ctx, "worker-hb.worker-1")
		require.NoError(t, err)
		require.NotNil(t, entry)

		// Stop publisher (no more heartbeats)
		err = publisher.Stop()
		require.NoError(t, err)

		// Wait for TTL to expire
		time.Sleep(2 * time.Second)

		// Heartbeat should be gone
		_, err = kv.Get(ctx, "worker-hb.worker-1")
		require.Error(t, err)
		require.ErrorIs(t, err, jetstream.ErrKeyNotFound)
	})
}

func TestPublisher_MultipleWorkers(t *testing.T) {
	t.Run("multiple workers publish independently", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-multiple")

		// Create three publishers
		publishers := make([]*Publisher, 3)
		for i := range publishers {
			publishers[i] = New(kv, "worker-hb", 100*time.Millisecond)
			publishers[i].SetWorkerID(fmt.Sprintf("worker-%d", i+1))

			err := publishers[i].Start(ctx)
			require.NoError(t, err)
		}

		// Wait for heartbeats
		time.Sleep(200 * time.Millisecond)

		// Verify all heartbeats exist
		for i := range publishers {
			key := fmt.Sprintf("worker-hb.worker-%d", i+1)
			entry, err := kv.Get(ctx, key)
			require.NoError(t, err)
			require.NotNil(t, entry)
		}

		// Cleanup
		for _, publisher := range publishers {
			err := publisher.Stop()
			require.NoError(t, err)
		}
	})
}

func TestPublisher_KeyFormat(t *testing.T) {
	t.Run("generates correct key format", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		kv := partitest.CreateJetStreamKV(t, nc, "test-hb-keyformat")

		publisher := New(kv, "worker-hb", 2*time.Second)
		publisher.SetWorkerID("worker-123")

		err := publisher.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = publisher.Stop() }()

		// Verify key format
		entry, err := kv.Get(ctx, "worker-hb.worker-123")
		require.NoError(t, err)
		require.NotNil(t, entry)
	})
}
