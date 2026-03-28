package source

import (
	"context"
	"testing"
	"time"

	partitest "github.com/arloliu/parti/v2/testing"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestNatsKV_Watch_Deduplication(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := context.Background()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_dedup"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil)
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	ch := src.Watch(ctx)

	// 1. Initial Update -> Should trigger signal
	p1 := []types.Partition{{Keys: []string{"p1"}, Weight: 100}}
	err = src.Update(ctx, p1)
	require.NoError(t, err)

	select {
	case <-ch:
		// Success
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for initial signal")
	}

	// 2. Identical Update -> Should NOT trigger signal
	err = src.Update(ctx, p1)
	require.NoError(t, err)

	select {
	case <-ch:
		t.Fatal("received signal for identical update")
	case <-time.After(500 * time.Millisecond):
		// Success (timeout expected)
	}

	// 3. Different Update (Weight change) -> Should trigger signal
	p2 := []types.Partition{{Keys: []string{"p1"}, Weight: 200}}
	err = src.Update(ctx, p2)
	require.NoError(t, err)

	select {
	case <-ch:
		// Success
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for weight change signal")
	}

	// 4. Different Update (Key change) -> Should trigger signal
	p3 := []types.Partition{{Keys: []string{"p1", "sub"}, Weight: 200}}
	err = src.Update(ctx, p3)
	require.NoError(t, err)

	select {
	case <-ch:
		// Success
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for key change signal")
	}

	// 5. Multiple partitions
	pMulti := []types.Partition{
		{Keys: []string{"a"}, Weight: 100},
		{Keys: []string{"b"}, Weight: 100},
	}
	err = src.Update(ctx, pMulti)
	require.NoError(t, err)

	select {
	case <-ch:
		// Success
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for multi partition signal")
	}

	// 6. Reordered Multiple partitions -> Should NOT trigger signal (canonical sorting)
	pMultiReordered := []types.Partition{
		{Keys: []string{"b"}, Weight: 100},
		{Keys: []string{"a"}, Weight: 100},
	}
	err = src.Update(ctx, pMultiReordered)
	require.NoError(t, err)

	select {
	case <-ch:
		t.Fatal("received signal for reordered update")
	case <-time.After(500 * time.Millisecond):
		// Success
	}
}
