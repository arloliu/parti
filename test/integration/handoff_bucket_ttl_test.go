package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/internal/testutil"
	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestHandoffBucketExistsAndTTLEnforced verifies that when two-phase handoff is enabled,
// the manager creates the handoff KV bucket and that entries expire according to HandoffTTL.
func TestHandoffBucketExistsAndTTLEnforced(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}

	ctx := context.Background()
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Configure a very short TTL to make the test fast.
	cfg := parti.TestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = "itest-handoff-ttl"
	cfg.KVBuckets.HandoffTTL = 800 * time.Millisecond

	parts := []parti.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}}
	src := source.NewStatic(parts)
	curStrategy := strategy.NewConsistentHash()

	mgr, err := parti.NewManager(&cfg, js, src, curStrategy)
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	// Wait briefly to ensure Start has created buckets.
	time.Sleep(100 * time.Millisecond)

	// Open the handoff bucket and write a key; it should exist.
	kv, err := js.KeyValue(context.Background(), cfg.KVBuckets.HandoffBucket)
	require.NoError(t, err, "handoff bucket should exist")

	_, err = kv.Put(ctx, "claims/test", []byte("1"))
	require.NoError(t, err)

	// Immediately the key should be retrievable.
	_, err = kv.Get(ctx, "claims/test")
	require.NoError(t, err)

	// After TTL, it should have expired. Add a small buffer over TTL.
	time.Sleep(cfg.KVBuckets.HandoffTTL + 300*time.Millisecond)
	_, err = kv.Get(ctx, "claims/test")
	require.Error(t, err, "expected key to expire after TTL")
}
