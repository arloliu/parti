package partitest

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
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

func TestStartEmbeddedNATSCluster_Size5(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}

	servers, nc := StartEmbeddedNATSCluster(t, WithClusterSize(5))

	require.Len(t, servers, 5)
	require.True(t, nc.IsConnected())

	// Every node should see at least the other 4 as routes once formed.
	for i, s := range servers {
		require.True(t, s.ReadyForConnections(5*time.Second), "server %d not ready", i)
		require.GreaterOrEqual(t, s.NumRoutes(), 4, "server %d should have >=4 routes", i)
	}

	// A Replicas=5 stream must be creatable against a 5-node cluster.
	si := CreateStream(t, nc, StreamSpec{
		Name:     "RF5_STREAM",
		Subjects: []string{"rf5.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 5,
	})
	require.Equal(t, 5, si.Config.Replicas)
	require.Equal(t, jetstream.FileStorage, si.Config.Storage)
}

func TestStartEmbeddedNATSCluster_DefaultIsThree(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}

	servers, nc := StartEmbeddedNATSCluster(t)
	require.Len(t, servers, 3)
	require.True(t, nc.IsConnected())
}

func TestCluster_RestartNode_FileStateSurvives(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}

	ctx := t.Context()
	c := StartCluster(t, WithClusterSize(3))
	require.Len(t, c.Servers, 3)

	// Write a value into an R3 FileStorage KV bucket, then restart a node and
	// confirm the value survives (file state persisted across the restart).
	kv := CreateJetStreamKV(t, c.Conn, "persist",
		WithKVStorage(jetstream.FileStorage),
		WithKVReplicas(3),
		WithKVTTL(0),
	)
	_, err := kv.Put(ctx, "k", []byte("v"))
	require.NoError(t, err)

	// Restart a non-meta-leader node so the bucket keeps quorum throughout.
	target := -1
	for i, s := range c.Servers {
		if !s.JetStreamIsLeader() {
			target = i
			break
		}
	}
	require.GreaterOrEqual(t, target, 0)

	c.Servers[target].Shutdown()
	c.Servers[target].WaitForShutdown()
	c.RestartNode(target)
	require.True(t, c.Servers[target].ReadyForConnections(5*time.Second))

	entry, err := kv.Get(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, []byte("v"), entry.Value())
}

func TestCluster_RestartNodeWiped_ReplicatesFromPeers(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}

	ctx := t.Context()
	c := StartCluster(t, WithClusterSize(3))

	kv := CreateJetStreamKV(t, c.Conn, "persist",
		WithKVStorage(jetstream.FileStorage),
		WithKVReplicas(3),
		WithKVTTL(0),
	)
	_, err := kv.Put(ctx, "k", []byte("v"))
	require.NoError(t, err)

	// Wipe-restart a non-leader node (PVC loss). Quorum (2 of 3) holds, so the
	// value remains readable and the wiped node re-replicates from peers.
	target := -1
	for i, s := range c.Servers {
		if !s.JetStreamIsLeader() {
			target = i
			break
		}
	}
	require.GreaterOrEqual(t, target, 0)

	c.Servers[target].Shutdown()
	c.Servers[target].WaitForShutdown()
	c.RestartNodeWiped(target)
	require.True(t, c.Servers[target].ReadyForConnections(5*time.Second))

	entry, err := kv.Get(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, []byte("v"), entry.Value())
}

func TestCreateStream_FileStorageReplicas(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}

	_, nc := StartEmbeddedNATSCluster(t, WithClusterSize(3))

	si := CreateStream(t, nc, StreamSpec{
		Name:     "PARTI_TEST",
		Subjects: []string{"parti.test.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 3,
	})
	require.Equal(t, 3, si.Config.Replicas)
	require.Equal(t, jetstream.FileStorage, si.Config.Storage)
}

func TestCreateJetStreamKV_OptionsOverride(t *testing.T) {
	ctx := t.Context()
	_, nc := StartEmbeddedNATS(t)

	kv := CreateJetStreamKV(t, nc, "opt-bucket",
		WithKVStorage(jetstream.FileStorage),
		WithKVTTL(0),
	)
	require.NotNil(t, kv)

	// KV buckets are backed by a stream named KV_<bucket>; inspect it to
	// confirm the storage option was applied.
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	stream, err := js.Stream(ctx, "KV_opt-bucket")
	require.NoError(t, err)
	require.Equal(t, jetstream.FileStorage, stream.CachedInfo().Config.Storage)
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
