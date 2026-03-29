package partitest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStartEmbeddedNATS(t *testing.T) {
	ns, nc := StartEmbeddedNATS(t)

	require.NotNil(t, ns)
	require.NotNil(t, nc)
	require.True(t, nc.IsConnected())

	// Verify server is running
	require.True(t, ns.ReadyForConnections(1*time.Second))

	// Verify JetStream is enabled
	js, err := nc.JetStream()
	require.NoError(t, err)
	require.NotNil(t, js)
}

// TestStartEmbeddedNATS_ParallelTests verifies parallel test execution.
func TestStartEmbeddedNATS_ParallelTests(t *testing.T) {
	t.Parallel()

	// Run multiple tests in parallel to verify no port conflicts
	for range 5 {
		t.Run("parallel", func(t *testing.T) {
			t.Parallel()

			_, nc := StartEmbeddedNATS(t)
			require.NotNil(t, nc)
			require.True(t, nc.IsConnected())

			// Note: In real concurrent tests, use testing/synctest instead of time.Sleep
			// See docs/design/06-implementation/synctest-usage.md for examples
		})
	}
}

func TestStartEmbeddedNATSCluster(t *testing.T) {
	// NATS cluster testing is useful for verifying HA scenarios.
	// We use embedded NATS servers with pre-allocated ports to ensure
	// reliable cluster formation.

	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}

	servers, nc := StartEmbeddedNATSCluster(t)

	require.Len(t, servers, 3)
	require.NotNil(t, nc)
	require.True(t, nc.IsConnected())

	// Verify cluster formation (use generous timeout — race detector slows things down)
	for i, s := range servers {
		require.True(t, s.ReadyForConnections(5*time.Second), "server %d not ready", i)
		// NumRoutes returns the number of registered routes.
		// Since we provide full mesh routes to all servers, and they might establish
		// multiple connections during formation, we just verify we have at least
		// the minimum required connections (clusterSize - 1).
		require.GreaterOrEqual(t, s.NumRoutes(), 2, "server %d should have at least 2 routes", i)
	}

	// Verify JetStream works across cluster
	js, err := nc.JetStream()
	require.NoError(t, err)
	require.NotNil(t, js)
}

func TestCreateJetStreamKV(t *testing.T) {
	ctx := t.Context()
	_, nc := StartEmbeddedNATS(t)

	kv := CreateJetStreamKV(t, nc, "test-bucket")
	require.NotNil(t, kv)

	// Verify KV operations work
	_, err := kv.Put(ctx, "test-key", []byte("test-value"))
	require.NoError(t, err)

	entry, err := kv.Get(ctx, "test-key")
	require.NoError(t, err)
	require.Equal(t, []byte("test-value"), entry.Value())
}

func TestCreateJetStreamKV_MultipleTests(t *testing.T) {
	ctx := t.Context()
	_, nc := StartEmbeddedNATS(t)

	// Create multiple buckets to verify isolation
	kv1 := CreateJetStreamKV(t, nc, "bucket-1")
	kv2 := CreateJetStreamKV(t, nc, "bucket-2")

	// Write to first bucket
	_, err := kv1.Put(ctx, "key", []byte("value1"))
	require.NoError(t, err)

	// Write to second bucket
	_, err = kv2.Put(ctx, "key", []byte("value2"))
	require.NoError(t, err)

	// Verify isolation
	entry1, err := kv1.Get(ctx, "key")
	require.NoError(t, err)
	require.Equal(t, []byte("value1"), entry1.Value())

	entry2, err := kv2.Get(ctx, "key")
	require.NoError(t, err)
	require.Equal(t, []byte("value2"), entry2.Value())
}
