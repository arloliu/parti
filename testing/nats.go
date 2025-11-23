package testing

import (
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// StartEmbeddedNATS starts an embedded NATS server with JetStream enabled for testing.
//
// The server runs in-process with JetStream enabled and stores data in a temporary
// directory that is automatically cleaned up when the test completes. This provides
// a fast, reliable way to test NATS-dependent code without external dependencies.
//
// Benefits over testcontainers:
//   - Zero external dependencies (no Docker required)
//   - Fast startup (milliseconds vs seconds)
//   - Works everywhere Go works (CI/CD friendly)
//   - Perfect for parallel test execution
//   - Automatic cleanup via t.Cleanup()
//
// The server uses a random available port to avoid conflicts in parallel tests.
//
// Parameters:
//   - t: Testing context for logging and cleanup
//
// Returns:
//   - *server.Server: The embedded NATS server instance
//   - *nats.Conn: Connected NATS client (closed automatically on test completion)
//
// Example:
//
//	func TestMyComponent(t *testing.T) {
//	    _, nc := testutil.StartEmbeddedNATS(t)
//	    // Use nc for your tests
//	    // Server and connection are automatically cleaned up
//	}
func StartEmbeddedNATS(t *testing.T) (*server.Server, *nats.Conn) {
	t.Helper()

	// Create server with random port and JetStream enabled
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,          // Use random available port
		JetStream: true,        // Enable JetStream for KV stores
		StoreDir:  t.TempDir(), // Use test temp dir (auto-cleanup)
		LogFile:   "",          // Disable file logging
		Debug:     false,       // Disable debug output
		Trace:     false,       // Disable trace output
		NoLog:     true,        // Suppress all server logs in tests
	}

	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("Failed to create embedded NATS server: %v", err)
	}

	// Start server in background goroutine
	go ns.Start()

	// Wait for server to be ready (with timeout)
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("Embedded NATS server not ready within timeout")
	}

	// Connect client to the server
	nc, err := nats.Connect(ns.ClientURL(),
		nats.Timeout(2*time.Second),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(3),
	)
	if err != nil {
		ns.Shutdown()
		t.Fatalf("Failed to connect to embedded NATS server: %v", err)
	}

	// Register cleanup handlers (executed in reverse order)
	t.Cleanup(func() {
		nc.Close()
		ns.Shutdown()
		ns.WaitForShutdown()
	})

	return ns, nc
}

// StartEmbeddedNATSCluster starts a 3-node NATS cluster with JetStream for HA testing.
//
// This is useful for testing leader failover, network partitions, and other
// high-availability scenarios. Each server in the cluster runs in-process.
//
// The cluster uses the NATS gossip protocol for automatic node discovery and
// maintains a quorum for JetStream operations.
//
// Parameters:
//   - t: Testing context for logging and cleanup
//
// Returns:
//   - []*server.Server: Slice of 3 NATS server instances
//   - *nats.Conn: Connected NATS client (connected to all servers)
//
// Example:
//
//	func TestLeaderFailover(t *testing.T) {
//	    servers, nc := testutil.StartEmbeddedNATSCluster(t)
//	    // Simulate leader failure
//	    servers[0].Shutdown()
//	    // Test failover behavior
//	}
func StartEmbeddedNATSCluster(t *testing.T) ([]*server.Server, *nats.Conn) {
	t.Helper()

	const clusterSize = 3
	servers := make([]*server.Server, clusterSize)
	clientURLs := make([]string, clusterSize)
	clusterPorts := make([]int, clusterSize)

	// Start each server in the cluster
	for i := range clusterSize {
		ns := startClusterNode(t, i, clusterPorts, servers)
		servers[i] = ns
		clientURLs[i] = ns.ClientURL()
		clusterPorts[i] = getClusterPort(t, ns, i, servers)
	}

	// Wait for cluster formation
	waitForClusterFormation(t, servers, clusterSize)

	// Connect client and register cleanup
	nc := connectToCluster(t, clientURLs, servers)

	return servers, nc
}

// startClusterNode creates and starts a single NATS server node in the cluster.
func startClusterNode(t *testing.T, index int, clusterPorts []int, servers []*server.Server) *server.Server {
	t.Helper()

	opts := &server.Options{
		ServerName: fmt.Sprintf("test-server-%d", index),
		Host:       "127.0.0.1",
		Port:       -1,
		Cluster: server.ClusterOpts{
			Name: "test-cluster",
			Host: "127.0.0.1",
			Port: -1,
		},
		JetStream: true,
		StoreDir:  t.TempDir(),
		LogFile:   "",
		Debug:     false,
		Trace:     false,
		NoLog:     true,
	}

	// Add routes to previously started servers
	if index > 0 {
		opts.Routes = buildClusterRoutes(index, clusterPorts)
	}

	ns, err := server.NewServer(opts)
	if err != nil {
		shutdownServers(servers[:index])
		t.Fatalf("Failed to create NATS server %d: %v", index, err)
	}

	go ns.Start()

	if !ns.ReadyForConnections(10 * time.Second) {
		shutdownServers(servers[:index+1])
		t.Fatalf("NATS server %d not ready", index)
	}

	return ns
}

// buildClusterRoutes builds the route URLs for cluster formation.
func buildClusterRoutes(count int, clusterPorts []int) []*url.URL {
	routes := make([]*url.URL, count)
	for j := 0; j < count; j++ {
		routeURL, _ := url.Parse(fmt.Sprintf("nats://127.0.0.1:%d", clusterPorts[j]))
		routes[j] = routeURL
	}

	return routes
}

// getClusterPort extracts the cluster port from a server.
func getClusterPort(t *testing.T, ns *server.Server, index int, servers []*server.Server) int {
	t.Helper()

	addr := ns.ClusterAddr()
	if addr == nil {
		shutdownServers(servers[:index+1])
		t.Fatalf("NATS server %d cluster address not available", index)
	}

	return addr.Port
}

// waitForClusterFormation waits for all servers to connect to each other.
func waitForClusterFormation(t *testing.T, servers []*server.Server, clusterSize int) {
	t.Helper()

	timeout := time.After(10 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			shutdownServers(servers)
			t.Fatal("Cluster failed to form within timeout")
		case <-ticker.C:
			if isClusterReady(servers, clusterSize) {
				return
			}
		}
	}
}

// isClusterReady checks if all servers are connected to each other.
func isClusterReady(servers []*server.Server, clusterSize int) bool {
	for _, s := range servers {
		if s.NumRoutes() < clusterSize-1 {
			return false
		}
	}

	return true
}

// connectToCluster creates a client connection to the cluster.
func connectToCluster(t *testing.T, clientURLs []string, servers []*server.Server) *nats.Conn {
	t.Helper()

	nc, err := nats.Connect(
		clientURLs[0],
		nats.UserInfo("", ""),
		nats.Timeout(2*time.Second),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		shutdownServers(servers)
		t.Fatalf("Failed to connect to cluster: %v", err)
	}

	t.Cleanup(func() {
		nc.Close()
		for i, s := range servers {
			s.Shutdown()
			s.WaitForShutdown()
			t.Logf("Shut down cluster node %d", i)
		}
	})

	return nc
}

// shutdownServers gracefully shuts down all non-nil servers.
func shutdownServers(servers []*server.Server) {
	for _, s := range servers {
		if s != nil {
			s.Shutdown()
		}
	}
}

// CreateJetStreamKV creates a JetStream KV bucket for testing using the new JetStream API.
//
// This is a convenience wrapper for creating KV buckets with sensible defaults
// for testing purposes. Uses the new jetstream.KeyValue interface.
//
// Parameters:
//   - t: Testing context
//   - nc: NATS connection (from StartEmbeddedNATS)
//   - bucketName: Name of the KV bucket to create
//
// Returns:
//   - jetstream.KeyValue: The created KV bucket interface
//
// Example:
//
//	func TestStableID(t *testing.T) {
//	    _, nc := testutil.StartEmbeddedNATS(t)
//	    kv := testutil.CreateJetStreamKV(t, nc, "worker-ids")
//	    // Use kv for testing
//	}
func CreateJetStreamKV(t *testing.T, nc *nats.Conn, bucketName string) jetstream.KeyValue {
	t.Helper()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("Failed to get JetStream context: %v", err)
	}

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      bucketName,
		Description: fmt.Sprintf("Test KV bucket: %s", bucketName),
		TTL:         1 * time.Minute, // Short TTL for testing
		Storage:     jetstream.MemoryStorage,
		Replicas:    1,
	})
	if err != nil {
		t.Fatalf("Failed to create KV bucket %s: %v", bucketName, err)
	}

	return kv
}
