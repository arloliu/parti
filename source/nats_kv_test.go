package source

import (
	"context"
	"testing"
	"time"

	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
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
