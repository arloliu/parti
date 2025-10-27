package subscription

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti"
	partitest "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func TestHelper_UpdateSubscriptions(t *testing.T) {
	t.Run("adds subscriptions for new partitions", func(t *testing.T) {
		ctx := t.Context()
		_, nc := partitest.StartEmbeddedNATS(t)

		helper := NewHelper(nc, Config{
			Stream:       "test-stream",
			MaxRetries:   3,
			RetryBackoff: 100 * time.Millisecond,
		})
		defer helper.Close()

		// Create handler that counts messages
		msgCount := 0
		handler := func(msg *nats.Msg) {
			msgCount++
		}

		// Add partitions
		added := []parti.Partition{
			{Keys: []string{"partition-0"}},
			{Keys: []string{"partition-1"}},
		}
		removed := []parti.Partition{}

		err := helper.UpdateSubscriptions(ctx, added, removed, handler)
		require.NoError(t, err)

		// Verify subscriptions were created
		active := helper.ActivePartitions()
		require.Len(t, active, 2)
		require.Contains(t, active, "partition-0")
		require.Contains(t, active, "partition-1")

		// Publish messages to test subscriptions
		_ = nc.Publish("test-stream.partition-0", []byte("msg1"))
		_ = nc.Publish("test-stream.partition-1", []byte("msg2"))
		nc.Flush()

		// Give time for messages to be received
		time.Sleep(100 * time.Millisecond)
		require.Equal(t, 2, msgCount)
	})

	t.Run("removes subscriptions for removed partitions", func(t *testing.T) {
		ctx := t.Context()
		_, nc := partitest.StartEmbeddedNATS(t)

		helper := NewHelper(nc, Config{
			Stream:       "test-stream",
			MaxRetries:   3,
			RetryBackoff: 100 * time.Millisecond,
		})
		defer helper.Close()

		handler := func(msg *nats.Msg) {}

		// Add initial partitions
		added := []parti.Partition{
			{Keys: []string{"partition-0"}},
			{Keys: []string{"partition-1"}},
		}
		err := helper.UpdateSubscriptions(ctx, added, []parti.Partition{}, handler)
		require.NoError(t, err)
		require.Len(t, helper.ActivePartitions(), 2)

		// Remove one partition
		removed := []parti.Partition{
			{Keys: []string{"partition-0"}},
		}
		err = helper.UpdateSubscriptions(ctx, []parti.Partition{}, removed, handler)
		require.NoError(t, err)

		// Verify only one subscription remains
		active := helper.ActivePartitions()
		require.Len(t, active, 1)
		require.Contains(t, active, "partition-1")
		require.NotContains(t, active, "partition-0")
	})

	t.Run("handles empty partition keys", func(t *testing.T) {
		ctx := t.Context()
		_, nc := partitest.StartEmbeddedNATS(t)

		helper := NewHelper(nc, Config{
			Stream:       "test-stream",
			MaxRetries:   3,
			RetryBackoff: 100 * time.Millisecond,
		})
		defer helper.Close()

		handler := func(msg *nats.Msg) {}

		// Add partition with empty keys - should be skipped
		added := []parti.Partition{
			{Keys: []string{}}, // Empty keys
			{Keys: []string{"partition-1"}},
		}
		err := helper.UpdateSubscriptions(ctx, added, []parti.Partition{}, handler)
		require.NoError(t, err)

		// Should only have one subscription (partition-1)
		active := helper.ActivePartitions()
		require.Len(t, active, 1)
		require.Contains(t, active, "partition-1")
	})

	t.Run("handles context cancellation", func(t *testing.T) {
		_, nc := partitest.StartEmbeddedNATS(t)

		helper := NewHelper(nc, Config{
			Stream:       "test-stream",
			MaxRetries:   10,
			RetryBackoff: 500 * time.Millisecond,
		})
		defer helper.Close()

		// Create context that will be cancelled
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()

		// Close NATS connection to force subscription failures
		nc.Close()

		handler := func(msg *nats.Msg) {}

		added := []parti.Partition{
			{Keys: []string{"partition-0"}},
		}

		// Should fail due to context cancellation during retry
		err := helper.UpdateSubscriptions(ctx, added, []parti.Partition{}, handler)
		require.Error(t, err)
	})
}

func TestHelper_Close(t *testing.T) {
	t.Run("closes all subscriptions", func(t *testing.T) {
		ctx := t.Context()
		_, nc := partitest.StartEmbeddedNATS(t)

		helper := NewHelper(nc, Config{
			Stream:       "test-stream",
			MaxRetries:   3,
			RetryBackoff: 100 * time.Millisecond,
		})

		handler := func(msg *nats.Msg) {}

		// Add subscriptions
		added := []parti.Partition{
			{Keys: []string{"partition-0"}},
			{Keys: []string{"partition-1"}},
		}
		err := helper.UpdateSubscriptions(ctx, added, []parti.Partition{}, handler)
		require.NoError(t, err)
		require.Len(t, helper.ActivePartitions(), 2)

		// Close should remove all subscriptions
		err = helper.Close()
		require.NoError(t, err)
		require.Len(t, helper.ActivePartitions(), 0)
	})
}

func TestHelper_ActivePartitions(t *testing.T) {
	t.Run("returns empty list initially", func(t *testing.T) {
		_, nc := partitest.StartEmbeddedNATS(t)

		helper := NewHelper(nc, Config{
			Stream: "test-stream",
		})
		defer helper.Close()

		active := helper.ActivePartitions()
		require.Empty(t, active)
	})
}
