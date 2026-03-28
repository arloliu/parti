package source

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestNatsKV(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := context.Background()

	// Create KV bucket
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: "partitions",
	})
	require.NoError(t, err)

	key := "config"
	src := NewNatsKV(kv, key, nil)

	// Start
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	// Initial list should be empty
	partitions, err := src.List(ctx)
	require.NoError(t, err)
	require.Empty(t, partitions)

	// Update
	newPartitions := []types.Partition{
		{Keys: []string{"p1"}, Weight: 100},
	}
	err = src.Update(ctx, newPartitions)
	require.NoError(t, err)

	// Wait for update
	require.Eventually(t, func() bool {
		p, err := src.List(ctx)
		return err == nil && len(p) == 1
	}, 2*time.Second, 10*time.Millisecond)

	// Verify content
	partitions, err = src.List(ctx)
	require.NoError(t, err)
	require.Equal(t, newPartitions, partitions)

	// Test backward compatibility (uncompressed JSON)
	uncompressedData := []byte(`[{"keys":["p2"],"weight":200}]`)
	_, err = kv.Put(ctx, key, uncompressedData)
	require.NoError(t, err)

	// Wait for update
	require.Eventually(t, func() bool {
		p, err := src.List(ctx)
		if err != nil {
			return false
		}
		if len(p) != 1 {
			return false
		}

		return p[0].Weight == 200
	}, 2*time.Second, 10*time.Millisecond)

	partitions, err = src.List(ctx)
	require.NoError(t, err)
	require.Equal(t, "p2", partitions[0].Keys[0])
}

func TestNatsKV_Lifecycle(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := context.Background()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_lifecycle"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil)

	// 1. Start
	err = src.Start(ctx)
	require.NoError(t, err)
	require.True(t, src.running)
	require.NotNil(t, src.ctx)
	require.NoError(t, src.ctx.Err(), "context should not be cancelled after Start")

	// 2. Stop
	err = src.Stop(ctx)
	require.NoError(t, err)
	require.False(t, src.running)
	require.ErrorIs(t, src.ctx.Err(), context.Canceled, "context should be cancelled after Stop")

	// 3. Restart (should be allowed)
	err = src.Start(ctx)
	require.NoError(t, err)
	require.True(t, src.running)
	require.NotNil(t, src.ctx)
	require.NoError(t, src.ctx.Err(), "new context should not be cancelled after Restart")

	// Cleanup
	_ = src.Stop(ctx)
}

func TestNatsKV_WatcherUpdate(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := context.Background()

	// Create KV bucket
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: "partitions_watcher",
	})
	require.NoError(t, err)

	key := "config"
	src := NewNatsKV(kv, key, nil)

	// Start source - key does not exist yet
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	// Verify initial state is empty
	partitions, err := src.List(ctx)
	require.NoError(t, err)
	require.Empty(t, partitions)

	// Update partitions via Update method
	expected := []types.Partition{
		{Keys: []string{"p1"}, Weight: 10},
		{Keys: []string{"p2"}, Weight: 20},
	}
	err = src.Update(ctx, expected)
	require.NoError(t, err)

	// Verify watcher picks up the change
	require.Eventually(t, func() bool {
		p, err := src.List(ctx)
		if err != nil {
			return false
		}
		return len(p) == 2
	}, 2*time.Second, 10*time.Millisecond)

	// Verify content matches
	partitions, err = src.List(ctx)
	require.NoError(t, err)
	require.Equal(t, expected, partitions)
}

func TestNatsKV_Watch(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := context.Background()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_watch"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil)
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	ch := src.Watch(ctx)

	// Update partitions
	err = src.Update(ctx, []types.Partition{{Keys: []string{"p1"}}})
	require.NoError(t, err)

	// Should receive signal
	select {
	case <-ch:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for watch signal")
	}

	// Verify list updated
	partitions, err := src.List(ctx)
	require.NoError(t, err)
	require.Len(t, partitions, 1)
}
