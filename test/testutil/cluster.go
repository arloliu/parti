package testutil

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// StartEmbeddedNATS starts an embedded NATS server for integration tests.
// It wraps the parti/testing package function for convenience.
func StartEmbeddedNATS(t *testing.T) (*nats.Conn, func()) {
	t.Helper()
	srv, nc := partitest.StartEmbeddedNATS(t)
	cleanup := func() {
		nc.Close()
		srv.Shutdown()
		srv.WaitForShutdown()
	}

	return nc, cleanup
}

// IntegrationTestConfig provides default configuration for integration tests.
func IntegrationTestConfig() parti.Config {
	cfg := parti.Config{
		WorkerIDPrefix:        "worker",
		WorkerIDMin:           0,
		WorkerIDMax:           10,
		WorkerIDTTL:           5 * time.Second,        // Reduced from 10s
		HeartbeatInterval:     500 * time.Millisecond, // Reduced from 1s
		HeartbeatTTL:          2 * time.Second,        // Reduced from 3s
		ElectionTimeout:       2 * time.Second,        // Reduced from 5s - faster leader election
		StartupTimeout:        10 * time.Second,       // Reduced from 25s
		ShutdownTimeout:       3 * time.Second,        // Reduced from 5s
		ColdStartWindow:       1 * time.Second,        // Reduced from 2s - faster cold start detection
		PlannedScaleWindow:    500 * time.Millisecond, // Reduced from 1s
		RestartDetectionRatio: 0.5,
		Assignment: parti.AssignmentConfig{
			MinRebalanceInterval: 2 * time.Second, // Reduced from default 10s - faster rebalancing in tests
		},
	}
	parti.SetDefaults(&cfg) // Apply default KV bucket names

	return cfg
}

// FastTestConfig provides aggressive timeouts for faster leader election tests.
// Use this for tests that focus on leader election and don't need long stabilization.
func FastTestConfig() parti.Config {
	cfg := parti.Config{
		WorkerIDPrefix:        "worker",
		WorkerIDMin:           0,
		WorkerIDMax:           20,                     // Support up to 20 workers
		WorkerIDTTL:           30 * time.Second,       // Long TTL to prevent expiration during concurrent startup
		HeartbeatInterval:     300 * time.Millisecond, // Very fast heartbeats
		HeartbeatTTL:          3 * time.Second,        // Balance: long enough for assignments, short enough for dead worker detection
		ElectionTimeout:       1 * time.Second,        // Very fast election - failover in 1-2s
		StartupTimeout:        5 * time.Second,
		ShutdownTimeout:       2 * time.Second,
		ColdStartWindow:       500 * time.Millisecond, // Very fast cold start
		PlannedScaleWindow:    300 * time.Millisecond,
		RestartDetectionRatio: 0.5,
		Assignment: parti.AssignmentConfig{
			MinRebalanceInterval: 400 * time.Millisecond, // Must be <= ColdStartWindow (500ms)
		},
	}
	parti.SetDefaults(&cfg) // Apply default KV bucket names

	return cfg
}

// CreateTestPartitions creates n partitions for testing.
func CreateTestPartitions(n int) []types.Partition {
	partitions := make([]types.Partition, n)
	for i := range partitions {
		partitions[i] = types.Partition{
			Keys:   []string{fmt.Sprintf("partition-%d", i)},
			Weight: 100,
		}
	}

	return partitions
}

// StateTracker tracks state transitions for a worker.
type StateTracker struct {
	WorkerIndex int
	States      []types.State
	T           *testing.T
}

// CreateStateTracker creates a new state tracker.
func CreateStateTracker(t *testing.T, workerIndex int) *StateTracker {
	return &StateTracker{
		WorkerIndex: workerIndex,
		States:      make([]types.State, 0),
		T:           t,
	}
}

// Hook returns a hook function for tracking state changes.
func (st *StateTracker) Hook() func(context.Context, types.State, types.State) error {
	return func(ctx context.Context, from, to types.State) error {
		st.T.Logf("Worker %d: %s → %s", st.WorkerIndex, from.String(), to.String())
		st.States = append(st.States, to)
		return nil
	}
}

// HasState checks if the worker went through a specific state.
func (st *StateTracker) HasState(state types.State) bool {
	for _, s := range st.States {
		if s == state {
			return true
		}
	}

	return false
}

// WorkerCluster manages a cluster of workers for testing.
type WorkerCluster struct {
	Workers       []*parti.Manager
	StateTrackers []*StateTracker
	Config        parti.Config
	Source        types.PartitionSource
	Strategy      types.AssignmentStrategy
	NC            *nats.Conn
	T             *testing.T
}

// NewWorkerCluster creates a new worker cluster for testing.
func NewWorkerCluster(t *testing.T, nc *nats.Conn, numPartitions int) *WorkerCluster {
	cfg := IntegrationTestConfig()
	partitions := CreateTestPartitions(numPartitions)
	src := source.NewStatic(partitions)
	assignmentStrategy := strategy.NewConsistentHash()

	return &WorkerCluster{
		Workers:       make([]*parti.Manager, 0),
		StateTrackers: make([]*StateTracker, 0),
		Config:        cfg,
		Source:        src,
		Strategy:      assignmentStrategy,
		NC:            nc,
		T:             t,
	}
}

// NewFastWorkerCluster creates a worker cluster with aggressive timeouts for fast leader election tests.
// Use this for tests that focus on leader election failover and don't need long stabilization windows.
func NewFastWorkerCluster(t *testing.T, nc *nats.Conn, numPartitions int) *WorkerCluster {
	cfg := FastTestConfig()
	partitions := CreateTestPartitions(numPartitions)
	src := source.NewStatic(partitions)
	assignmentStrategy := strategy.NewConsistentHash()

	return &WorkerCluster{
		Workers:       make([]*parti.Manager, 0),
		StateTrackers: make([]*StateTracker, 0),
		Config:        cfg,
		Source:        src,
		Strategy:      assignmentStrategy,
		NC:            nc,
		T:             t,
	}
}

// AddWorker adds a worker to the cluster with state tracking.
//
// Optional logger can be passed to enable debug logging for troubleshooting:
//
//	debugLogger := logger.NewTest(t)
//	cluster.AddWorker(ctx, debugLogger)
//
// Without a logger, the worker will use the default NopLogger (no output).
//
// Parameters:
//   - ctx: Context for the worker lifecycle
//   - opts: Optional logger for debug output
//
// Returns:
//   - *parti.Manager: The created worker manager
func (wc *WorkerCluster) AddWorker(ctx context.Context, opts ...types.Logger) *parti.Manager {
	workerIdx := len(wc.Workers)
	tracker := CreateStateTracker(wc.T, workerIdx)
	wc.StateTrackers = append(wc.StateTrackers, tracker)

	hooks := &parti.Hooks{
		OnStateChanged: tracker.Hook(),
	}

	// Build manager options
	managerOpts := []parti.Option{parti.WithHooks(hooks)}

	// Add logger if provided
	if len(opts) > 0 && opts[0] != nil {
		managerOpts = append(managerOpts, parti.WithLogger(opts[0]))
	}

	mgr, err := parti.NewManager(
		&wc.Config, wc.NC, wc.Source, wc.Strategy,
		managerOpts...,
	)
	require.NoError(wc.T, err, "failed to create worker %d", workerIdx)

	wc.Workers = append(wc.Workers, mgr)

	return mgr
}

// AddWorkerWithoutTracking adds a worker without state tracking.
func (wc *WorkerCluster) AddWorkerWithoutTracking(ctx context.Context) *parti.Manager {
	mgr, err := parti.NewManager(&wc.Config, wc.NC, wc.Source, wc.Strategy)
	require.NoError(wc.T, err, "failed to create worker")

	wc.Workers = append(wc.Workers, mgr)

	return mgr
}

// StartWorkers starts all workers in the cluster.
func (wc *WorkerCluster) StartWorkers(ctx context.Context) {
	for i, mgr := range wc.Workers {
		err := mgr.Start(ctx)
		require.NoError(wc.T, err, "worker %d failed to start", i)
	}
}

// StopWorkers stops all workers gracefully.
// Skips workers that are already in Shutdown state.
func (wc *WorkerCluster) StopWorkers() {
	for i, mgr := range wc.Workers {
		// Skip if already shutdown
		if mgr.State() == types.StateShutdown {
			wc.T.Logf("Worker %d already shutdown, skipping", i)
			continue
		}

		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		// Stop in goroutine with timeout to prevent hanging
		done := make(chan error, 1)
		go func() {
			done <- mgr.Stop(stopCtx)
		}()

		select {
		case err := <-done:
			if err != nil {
				wc.T.Logf("Worker %d stop error (non-fatal): %v", i, err)
			}
		case <-time.After(5 * time.Second):
			wc.T.Logf("Worker %d stop timeout after 5s (non-fatal)", i)
		}

		cancel()
	}
}

// WaitForStableState waits for all workers to reach Stable state.
func (wc *WorkerCluster) WaitForStableState(timeout time.Duration) {
	require.Eventually(wc.T, func() bool {
		stableCount := 0
		for i, mgr := range wc.Workers {
			state := mgr.State()
			if state != types.StateStable {
				wc.T.Logf("Worker %d (%s) in state: %s (leader: %v)",
					i, mgr.WorkerID(), state.String(), mgr.IsLeader())
			} else {
				stableCount++
			}
		}
		wc.T.Logf("Stable workers: %d/%d", stableCount, len(wc.Workers))

		return stableCount == len(wc.Workers)
	}, timeout, 500*time.Millisecond, "workers did not reach Stable state")
}

// VerifyExactlyOneLeader verifies that exactly one worker is the leader.
func (wc *WorkerCluster) VerifyExactlyOneLeader() *parti.Manager {
	leaderCount := 0
	var leader *parti.Manager
	for i, mgr := range wc.Workers {
		// Skip shutdown workers
		if mgr.State() == types.StateShutdown {
			continue
		}
		if mgr.IsLeader() {
			leaderCount++
			leader = mgr
			wc.T.Logf("Worker %d (%s) is the leader", i, mgr.WorkerID())
		}
	}
	require.Equal(wc.T, 1, leaderCount, "expected exactly one leader")

	return leader
}

// VerifyAllWorkersHavePartitions verifies all workers have at least one partition.
func (wc *WorkerCluster) VerifyAllWorkersHavePartitions() {
	for i, mgr := range wc.Workers {
		assignment := mgr.CurrentAssignment()
		require.Greater(wc.T, len(assignment.Partitions), 0,
			"worker %d has no partitions", i)
		wc.T.Logf("Worker %d (%s): %d partitions",
			i, mgr.WorkerID(), len(assignment.Partitions))
	}
}

// VerifyTotalPartitionCount verifies the total number of unique partitions assigned.
func (wc *WorkerCluster) VerifyTotalPartitionCount(expected int) {
	// Use a map to track unique partitions across all workers
	uniquePartitions := make(map[string]bool)

	for i, mgr := range wc.Workers {
		assignment := mgr.CurrentAssignment()
		wc.T.Logf("Worker %d (%s): %d partitions",
			i, mgr.WorkerID(), len(assignment.Partitions))

		// Add partitions to unique set
		for _, p := range assignment.Partitions {
			// Use the first key as partition identifier
			if len(p.Keys) > 0 {
				uniquePartitions[p.Keys[0]] = true
			}
		}
	}

	totalUnique := len(uniquePartitions)
	require.Equal(wc.T, expected, totalUnique,
		"unique partition count mismatch (expected %d, got %d unique partitions)", expected, totalUnique)
}

// VerifyStateTransition verifies that at least one worker went through the specified state.
func (wc *WorkerCluster) VerifyStateTransition(state types.State) {
	found := false
	for i, tracker := range wc.StateTrackers {
		if tracker.HasState(state) {
			wc.T.Logf("Worker %d went through %s state", i, state.String())
			found = true
		}
	}
	require.True(wc.T, found,
		"no worker entered %s state", state.String())
}

// GetLeader returns the current leader or nil.
func (wc *WorkerCluster) GetLeader() *parti.Manager {
	for _, mgr := range wc.Workers {
		if mgr.IsLeader() {
			return mgr
		}
	}

	return nil
}

// RemoveWorker stops and removes a worker from the cluster (simulates crash).
// It calls Stop() but doesn't remove from the Workers slice to avoid index issues.
// Tests should account for stopped workers when counting leaders/stable workers.
func (wc *WorkerCluster) RemoveWorker(index int) {
	require.Less(wc.T, index, len(wc.Workers), "invalid worker index")

	mgr := wc.Workers[index]
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := mgr.Stop(stopCtx)
	require.NoError(wc.T, err, "failed to stop worker %d", index)

	wc.T.Logf("Removed worker %d (%s)", index, mgr.WorkerID())
}

// GetActiveWorkers returns only the workers that are not in Shutdown state.
func (wc *WorkerCluster) GetActiveWorkers() []*parti.Manager {
	active := make([]*parti.Manager, 0, len(wc.Workers))
	for _, mgr := range wc.Workers {
		if mgr.State() != types.StateShutdown {
			active = append(active, mgr)
		}
	}

	return active
}
