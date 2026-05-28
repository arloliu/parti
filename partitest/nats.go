package partitest

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
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
func StartEmbeddedNATS(t testing.TB) (*server.Server, *nats.Conn) {
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

	// Connect client to the server with the recommended Parti posture:
	// MaxReconnects(-1) lets the client ride through transient outages
	// instead of giving up and going CLOSED. Mirrors the guidance in
	// docs/OPERATIONS.md "NATS Client Connection".
	nc, err := nats.Connect(ns.ClientURL(),
		nats.Timeout(2*time.Second),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
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

// clusterConfig holds resolved options for StartEmbeddedNATSCluster.
type clusterConfig struct {
	size int
}

// ClusterOption configures StartEmbeddedNATSCluster.
type ClusterOption func(*clusterConfig)

// WithClusterSize sets the number of in-process NATS nodes in the cluster.
//
// The default is 3. The value bounds the maximum replication factor (RF) any
// stream, KV bucket, or consumer created against the cluster can use: a stream
// with Replicas=5 requires at least 5 nodes. A value < 1 fails the test.
//
// Storage type is NOT a cluster-level setting in NATS — it is configured per
// stream / KV bucket / consumer. Use CreateStream and CreateJetStreamKV's
// storage options to control it.
func WithClusterSize(n int) ClusterOption {
	return func(c *clusterConfig) { c.size = n }
}

// StartEmbeddedNATSCluster starts an N-node NATS cluster with JetStream for HA testing.
//
// The node count defaults to 3 and is configurable via WithClusterSize. This is
// useful for testing leader failover, network partitions, quorum loss, and other
// high-availability scenarios. Each server in the cluster runs in-process.
//
// The cluster uses the NATS gossip protocol for automatic node discovery and
// maintains a quorum for JetStream operations.
//
// Parameters:
//   - t: Testing context for logging and cleanup
//   - opts: Optional cluster options (e.g. WithClusterSize(5))
//
// Returns:
//   - []*server.Server: Slice of cluster-size NATS server instances
//   - *nats.Conn: Connected NATS client (connected to all servers)
//
// Example:
//
//	func TestLeaderFailover(t *testing.T) {
//	    servers, nc := testutil.StartEmbeddedNATSCluster(t, WithClusterSize(5))
//	    // Simulate leader failure
//	    servers[0].Shutdown()
//	    // Test failover behavior
//	}
func StartEmbeddedNATSCluster(t *testing.T, opts ...ClusterOption) ([]*server.Server, *nats.Conn) {
	t.Helper()
	c := StartCluster(t, opts...)

	return c.Servers, c.Conn
}

// Cluster is a handle to an embedded NATS cluster that supports restarting
// individual nodes in place (same ports + StoreDir) for outage/recovery tests.
//
// Servers and Conn mirror the StartEmbeddedNATSCluster return values. Tests may
// shut down a node via Servers[i].Shutdown() and later restart it with
// RestartNode(i): because the node keeps its original StoreDir, FileStorage
// state survives the restart while MemoryStorage assets are wiped — exactly the
// production restart semantics.
// nodeReadyTimeout bounds how long a single embedded node may take to accept
// connections after Start. Generous because back-to-back multi-node clusters
// under -race CPU contention can stall a node's startup well past a few seconds.
const nodeReadyTimeout = 30 * time.Second

type Cluster struct {
	Servers []*server.Server
	Conn    *nats.Conn

	t        *testing.T
	nodeOpts []*server.Options // per-node options, reused verbatim on restart
}

// StartCluster starts an N-node embedded NATS cluster and returns a Cluster
// handle. The node count defaults to 3 and is configurable via WithClusterSize.
//
// Unlike StartEmbeddedNATSCluster (which returns bare slices for backward
// compatibility), the handle retains each node's options so RestartNode can
// bring a downed node back identically.
func StartCluster(t *testing.T, opts ...ClusterOption) *Cluster {
	t.Helper()

	cfg := clusterConfig{size: 3}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.size < 1 {
		t.Fatalf("WithClusterSize: size must be >= 1, got %d", cfg.size)
	}
	clusterSize := cfg.size

	// Pre-allocate ports to avoid chicken-and-egg problem with routes
	clusterPorts := make([]int, clusterSize)
	clientPorts := make([]int, clusterSize)
	var err error
	for i := range clusterSize {
		clusterPorts[i], err = getFreePort()
		if err != nil {
			t.Fatalf("Failed to get free port: %v", err)
		}
		clientPorts[i], err = getFreePort()
		if err != nil {
			t.Fatalf("Failed to get free port: %v", err)
		}
	}

	// Build full mesh routes
	routes := buildClusterRoutes(clusterSize, clusterPorts)

	// Build per-node options once. StoreDir is allocated here (not inside the
	// node starter) so a restart reuses the same directory — FileStorage state
	// then survives the restart.
	c := &Cluster{
		Servers:  make([]*server.Server, clusterSize),
		t:        t,
		nodeOpts: make([]*server.Options, clusterSize),
	}
	for i := range clusterSize {
		c.nodeOpts[i] = &server.Options{
			ServerName: fmt.Sprintf("test-server-%d", i),
			Host:       "127.0.0.1",
			Port:       clientPorts[i],
			Cluster: server.ClusterOpts{
				Name: "test-cluster",
				Host: "127.0.0.1",
				Port: clusterPorts[i],
			},
			JetStream: true,
			StoreDir:  t.TempDir(),
			LogFile:   "",
			Debug:     false,
			Trace:     false,
			NoLog:     true,
			Routes:    routes,
		}
	}

	// Start all nodes
	for i := range clusterSize {
		c.Servers[i] = startClusterNode(t, i, c.nodeOpts[i], c.Servers)
	}

	// Wait for cluster formation (routes connected to all peers).
	waitForClusterFormation(t, c.Servers, clusterSize)

	// Wait for the JetStream meta-leader to be elected. Route formation
	// is a prerequisite but not sufficient; the Raft meta-group needs an
	// additional round-trip to elect a leader before R>1 bucket creation
	// will succeed.
	waitForJetStreamMetaLeader(t, c.Servers)

	// Wait for the meta-leader to register all N JetStream peers. Leader
	// election precedes peer-stats propagation; creating an R=N stream/KV
	// before the leader knows all N peers fails with "no suitable peers for
	// placement". Gate on the leader reporting the full peer set.
	waitForJetStreamPeers(t, c.Servers, clusterSize)

	// Connect client.
	clientURLs := make([]string, clusterSize)
	for i, s := range c.Servers {
		clientURLs[i] = s.ClientURL()
	}
	c.Conn = connectToCluster(t, clientURLs, c.Servers)

	// Register cleanup over the current server set (RestartNode replaces
	// entries in c.Servers, so iterate the field, not a captured slice).
	t.Cleanup(func() {
		c.Conn.Close()
		for i, s := range c.Servers {
			if s == nil {
				continue
			}
			s.Shutdown()
			s.WaitForShutdown()
			t.Logf("Shut down cluster node %d", i)
		}
	})

	return c
}

// RestartNode restarts cluster node i in place using its original options
// (same client/cluster ports, StoreDir, routes, and server name).
//
// The caller is responsible for having shut the node down first (e.g.
// c.Servers[i].Shutdown()); RestartNode shuts it down defensively if it is
// still running. The new server is stored back into c.Servers[i] and the
// method blocks until it is ready for connections.
func (c *Cluster) RestartNode(i int) {
	c.t.Helper()
	c.restart(i, false)
}

// RestartNodeWiped restarts cluster node i with a FRESH (empty) StoreDir,
// simulating a lost persistent volume (PVC) or wiped file storage: the node
// keeps its identity (ports, routes, name) but comes back with no on-disk
// JetStream state and must re-replicate from surviving peers.
//
// The fresh StoreDir is retained, so subsequent RestartNode calls keep the new
// (now-populated) directory — matching a replaced volume.
func (c *Cluster) RestartNodeWiped(i int) {
	c.t.Helper()
	c.restart(i, true)
}

func (c *Cluster) restart(i int, wipeStore bool) {
	c.t.Helper()
	if i < 0 || i >= len(c.Servers) {
		c.t.Fatalf("restartNode: index %d out of range [0,%d)", i, len(c.Servers))
	}

	if old := c.Servers[i]; old != nil && old.Running() {
		old.Shutdown()
		old.WaitForShutdown()
	}

	if wipeStore {
		c.nodeOpts[i].StoreDir = c.t.TempDir()
	}

	ns, err := server.NewServer(c.nodeOpts[i])
	if err != nil {
		c.t.Fatalf("restart %d: NewServer: %v", i, err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(nodeReadyTimeout) {
		ns.Shutdown()
		c.t.Fatalf("restart %d: not ready within timeout", i)
	}
	c.Servers[i] = ns
}

// startClusterNode creates and starts a single NATS server node from opts.
func startClusterNode(t *testing.T, index int, opts *server.Options, servers []*server.Server) *server.Server {
	t.Helper()

	ns, err := server.NewServer(opts)
	if err != nil {
		shutdownServers(servers[:index])
		t.Fatalf("Failed to create NATS server %d: %v", index, err)
	}

	go ns.Start()

	if !ns.ReadyForConnections(nodeReadyTimeout) {
		shutdownServers(servers[:index+1])
		t.Fatalf("NATS server %d not ready", index)
	}

	return ns
}

// buildClusterRoutes builds the route URLs for cluster formation.
func buildClusterRoutes(count int, clusterPorts []int) []*url.URL {
	routes := make([]*url.URL, count)
	for j := range count {
		routeURL, _ := url.Parse(fmt.Sprintf("nats://127.0.0.1:%d", clusterPorts[j]))
		routes[j] = routeURL
	}

	return routes
}

// getFreePort returns a free TCP port.
func getFreePort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}

	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}

	tcpAddr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		_ = l.Close()
		return 0, errors.New("failed to obtain TCP address")
	}
	port := tcpAddr.Port

	if err := l.Close(); err != nil {
		return 0, err
	}

	return port, nil
}

// waitForClusterFormation waits for all servers to connect to each other.
func waitForClusterFormation(t *testing.T, servers []*server.Server, clusterSize int) {
	t.Helper()

	// Scale the timeout with cluster size: larger clusters need more
	// round-trips before every node sees all its peers. 3 nodes → ~14s,
	// 5 nodes → ~20s.
	timeout := time.After(time.Duration(5+3*clusterSize) * time.Second)
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

// waitForJetStreamMetaLeader waits until exactly one server in the cluster
// reports JetStreamIsLeader()==true. Must be called after waitForClusterFormation.
func waitForJetStreamMetaLeader(t *testing.T, servers []*server.Server) {
	t.Helper()

	// Scale with cluster size for the same reason as cluster formation:
	// the meta Raft group needs more time to elect a leader as nodes grow.
	timeout := time.After(time.Duration(10+2*len(servers)) * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			shutdownServers(servers)
			t.Fatal("JetStream meta-leader not elected within 15 s")
		case <-ticker.C:
			for _, s := range servers {
				if s.JetStreamIsLeader() {
					return
				}
			}
		}
	}
}

// waitForJetStreamPeers waits until the meta-leader reports all clusterSize
// JetStream peers as online and stats-reporting. Must be called after
// waitForJetStreamMetaLeader. JetStreamClusterPeers returns a non-nil list only
// on the leader, and only counts peers that are online, JS-enabled, and have
// reported stats — exactly the set eligible for R=N placement.
func waitForJetStreamPeers(t *testing.T, servers []*server.Server, clusterSize int) {
	t.Helper()

	timeout := time.After(time.Duration(10+2*len(servers)) * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			shutdownServers(servers)
			t.Fatal("JetStream peers not all registered with meta-leader within timeout")
		case <-ticker.C:
			for _, s := range servers {
				if len(s.JetStreamClusterPeers()) >= clusterSize {
					return
				}
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
		strings.Join(clientURLs, ","),
		nats.UserInfo("", ""),
		nats.Timeout(2*time.Second),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		shutdownServers(servers)
		t.Fatalf("Failed to connect to cluster: %v", err)
	}

	// Cleanup (conn close + node shutdown) is registered by StartCluster so it
	// can iterate the current server set after RestartNode replacements.
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
func CreateJetStreamKV(t testing.TB, nc *nats.Conn, bucketName string, opts ...KVOption) jetstream.KeyValue {
	t.Helper()

	cfg := kvConfig{
		storage:  jetstream.MemoryStorage,
		replicas: 1,
		ttl:      1 * time.Minute, // Short TTL for testing
	}
	for _, o := range opts {
		o(&cfg)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("Failed to get JetStream context: %v", err)
	}

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      bucketName,
		Description: fmt.Sprintf("Test KV bucket: %s", bucketName),
		TTL:         cfg.ttl,
		Storage:     cfg.storage,
		Replicas:    cfg.replicas,
	})
	if err != nil {
		t.Fatalf("Failed to create KV bucket %s: %v", bucketName, err)
	}

	return kv
}

// kvConfig holds resolved options for CreateJetStreamKV.
type kvConfig struct {
	storage  jetstream.StorageType
	replicas int
	ttl      time.Duration
}

// KVOption configures a KV bucket created by CreateJetStreamKV.
type KVOption func(*kvConfig)

// WithKVStorage sets the KV bucket storage type (default jetstream.MemoryStorage).
func WithKVStorage(s jetstream.StorageType) KVOption {
	return func(c *kvConfig) { c.storage = s }
}

// WithKVReplicas sets the KV bucket replication factor (default 1). The value
// must not exceed the cluster node count or bucket creation fails.
func WithKVReplicas(n int) KVOption {
	return func(c *kvConfig) { c.replicas = n }
}

// WithKVTTL sets the KV bucket entry TTL (default 1 minute). Zero disables TTL.
func WithKVTTL(d time.Duration) KVOption {
	return func(c *kvConfig) { c.ttl = d }
}

// StreamSpec configures a JetStream stream created by CreateStream.
type StreamSpec struct {
	// Name is the stream name (required).
	Name string
	// Subjects is the list of subjects the stream binds (required).
	Subjects []string
	// Storage is the stream storage type. Defaults to jetstream.FileStorage
	// (the zero value of StorageType) so RF>1 streams survive node restarts.
	Storage jetstream.StorageType
	// Replicas is the stream replication factor. Zero is normalized to 1.
	// Must not exceed the cluster node count or stream creation fails.
	Replicas int
}

// CreateStream creates a JetStream stream from spec and returns its info.
//
// Unlike CreateJetStreamKV (which defaults to memory storage for fast, isolated
// unit tests), CreateStream defaults to FileStorage because it is intended for
// multi-node HA scenarios where the stream must survive node restarts. Set
// StreamSpec.Storage explicitly to override.
//
// Example:
//
//	si := CreateStream(t, nc, StreamSpec{
//	    Name:     "PARTI_TEST",
//	    Subjects: []string{"parti.test.>"},
//	    Storage:  jetstream.FileStorage,
//	    Replicas: 5,
//	})
func CreateStream(t testing.TB, nc *nats.Conn, spec StreamSpec) *jetstream.StreamInfo {
	t.Helper()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("Failed to get JetStream context: %v", err)
	}

	replicas := spec.Replicas
	if replicas == 0 {
		replicas = 1
	}

	ctx := t.Context()
	s, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     spec.Name,
		Subjects: spec.Subjects,
		Storage:  spec.Storage,
		Replicas: replicas,
	})
	if err != nil {
		t.Fatalf("Failed to create stream %s (Replicas=%d storage=%v): %v",
			spec.Name, replicas, spec.Storage, err)
	}

	si, err := s.Info(ctx)
	if err != nil {
		t.Fatalf("Failed to get stream info %s: %v", spec.Name, err)
	}

	return si
}
