package assignment_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Partition helpers
// ---------------------------------------------------------------------------

// makePartitions creates n partitions with keys ["partition", "000"], …
// and the given weight. If weight is 0, it defaults to 100.
func makePartitions(n int, weight int64) []types.Partition {
	if weight == 0 {
		weight = 100
	}

	partitions := make([]types.Partition, n)
	for i := range partitions {
		partitions[i] = types.Partition{
			Keys:   []string{"partition", fmt.Sprintf("%03d", i)},
			Weight: weight,
		}
	}

	return partitions
}

// partitionKey generates a colon-joined unique key for a partition.
func partitionKey(p types.Partition) string {
	if len(p.Keys) == 0 {
		return ""
	}

	result := p.Keys[0]
	for i := 1; i < len(p.Keys); i++ {
		result += ":" + p.Keys[i]
	}

	return result
}

// partitionSetsEqual compares two partition slices for equality (ignoring order).
func partitionSetsEqual(a, b []types.Partition) bool {
	if len(a) != len(b) {
		return false
	}

	aMap := make(map[string]bool, len(a))
	for _, p := range a {
		aMap[partitionKey(p)] = true
	}

	for _, p := range b {
		if !aMap[partitionKey(p)] {
			return false
		}
	}

	return true
}

// ---------------------------------------------------------------------------
// Config helpers
// ---------------------------------------------------------------------------

// createTestConfig returns a *parti.Config with fast timeouts suitable for
// integration tests.
func createTestConfig() *parti.Config {
	return &parti.Config{
		WorkerIDPrefix:        "test-worker",
		WorkerIDMin:           0,
		WorkerIDMax:           99,
		WorkerIDTTL:           10 * time.Second,
		HeartbeatInterval:     300 * time.Millisecond,
		HeartbeatTTL:          1 * time.Second,
		ElectionTimeout:       1 * time.Second,
		StartupTimeout:        5 * time.Second,
		ShutdownTimeout:       2 * time.Second,
		ColdStartWindow:       2 * time.Second,
		PlannedScaleWindow:    2 * time.Second,
		RestartDetectionRatio: 0.5,
		RebalanceCooldown:     1 * time.Second,
	}
}

// ---------------------------------------------------------------------------
// Manager lifecycle helpers
// ---------------------------------------------------------------------------

// setupManagers creates numManagers Manager instances sharing the given NATS
// connection, config, partition source, strategy and logger.
func setupManagers(
	t *testing.T,
	conn *nats.Conn,
	cfg *parti.Config,
	src parti.PartitionSource,
	assignStrategy parti.AssignmentStrategy,
	numManagers int,
	logger parti.Logger,
) []*parti.Manager {
	t.Helper()

	managers := make([]*parti.Manager, numManagers)
	for i := 0; i < numManagers; i++ {
		js, err := jetstream.New(conn)
		require.NoError(t, err)

		mgr, err := parti.NewManager(cfg, js, src, assignStrategy, parti.WithLogger(logger))
		require.NoError(t, err, "Failed to create manager %d", i)

		managers[i] = mgr
	}

	return managers
}

// startManagers starts all managers sequentially.
func startManagers(t *testing.T, ctx context.Context, managers []*parti.Manager) {
	t.Helper()
	t.Logf("Starting %d managers...", len(managers))

	for i, mgr := range managers {
		err := mgr.Start(ctx)
		require.NoError(t, err, "Failed to start manager %d", i)
	}
}

// startManagersConcurrently starts all managers in goroutines and returns
// immediately. Errors are reported through t.
func startManagersConcurrently(t *testing.T, ctx context.Context, managers []*parti.Manager) {
	t.Helper()

	for i, mgr := range managers {
		go func(m *parti.Manager, idx int) {
			err := m.Start(ctx)
			require.NoError(t, err, "manager %d failed to start", idx)
		}(mgr, i)
	}
}

// waitForStable waits for all managers to reach StateStable within 20 s.
func waitForStable(t *testing.T, ctx context.Context, managers []*parti.Manager) {
	t.Helper()
	t.Log("Waiting for all managers to reach Stable state...")

	waiters := toManagerWaiters(managers)
	err := testutil.WaitAllManagersState(ctx, waiters, types.StateStable, 20*time.Second)
	require.NoError(t, err, "Managers failed to reach Stable state")
}

// waitForStableWithTimeout is like waitForStable but with a custom timeout.
func waitForStableWithTimeout(t *testing.T, ctx context.Context, managers []*parti.Manager, timeout time.Duration) {
	t.Helper()
	t.Log("Waiting for all managers to reach Stable state...")

	waiters := toManagerWaiters(managers)
	err := testutil.WaitAllManagersState(ctx, waiters, types.StateStable, timeout)
	require.NoError(t, err, "Managers failed to reach Stable state")
}

// cleanupManagers stops all non-nil managers with a 5 s timeout.
func cleanupManagers(t *testing.T, managers []*parti.Manager) {
	t.Helper()

	for i, mgr := range managers {
		if mgr != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := mgr.Stop(stopCtx); err != nil {
				t.Logf("Failed to stop manager %d: %v", i, err)
			}
			stopCancel()
		}
	}
}

// ---------------------------------------------------------------------------
// Assignment verification helpers
// ---------------------------------------------------------------------------

// verifyAssignments polls until all managers collectively report expectedCount
// unique partitions with no duplicates (up to 15 s). It then logs the final
// stable state per manager.
func verifyAssignments(t *testing.T, managers []*parti.Manager, expectedCount int) {
	t.Helper()
	t.Logf("Verifying assignments (expected %d partitions)...", expectedCount)

	require.Eventually(t, func() bool {
		seen := make(map[string]int)
		for _, mgr := range managers {
			for _, p := range mgr.CurrentAssignment().Partitions {
				seen[fmt.Sprintf("%v", p.Keys)]++
			}
		}

		if len(seen) != expectedCount {
			return false
		}

		for _, count := range seen {
			if count > 1 {
				return false
			}
		}

		return true
	}, 15*time.Second, 200*time.Millisecond,
		"Assignments did not stabilize to %d unique partitions with no duplicates", expectedCount)

	for i, mgr := range managers {
		a := mgr.CurrentAssignment()
		t.Logf("Manager %d: %d partitions assigned (version %d)", i, len(a.Partitions), a.Version)
	}
}

// findLeader returns the first manager that reports IsLeader() == true.
func findLeader(t *testing.T, managers []*parti.Manager) *parti.Manager {
	t.Helper()

	for _, mgr := range managers {
		if mgr.IsLeader() {
			return mgr
		}
	}

	require.Fail(t, "No leader found")

	return nil
}

// refreshPartitions calls RefreshPartitions on the given leader with a 15 s
// timeout.
func refreshPartitions(t *testing.T, leader *parti.Manager) {
	t.Helper()
	t.Log("Calling RefreshPartitions() on leader...")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err := leader.RefreshPartitions(ctx)
	require.NoError(t, err, "RefreshPartitions failed")
}

// maxVersion returns the highest assignment version across all managers.
func maxVersion(managers []*parti.Manager) int64 {
	var v int64
	for _, m := range managers {
		if cv := m.CurrentAssignment().Version; cv > v {
			v = cv
		}
	}

	return v
}

// waitForVersionChange polls until every manager is StateStable with a version
// higher than initialVersion.
func waitForVersionChange(t *testing.T, managers []*parti.Manager, initialVersion int64) {
	t.Helper()

	require.Eventually(t, func() bool {
		for _, m := range managers {
			if m.State() != types.StateStable {
				return false
			}
			if m.CurrentAssignment().Version <= initialVersion {
				return false
			}
		}

		return true
	}, 20*time.Second, 100*time.Millisecond,
		"Managers failed to reach Stable state with version > %d", initialVersion)
}

// refreshAndWait captures the current max version, calls RefreshPartitions on
// the leader, and waits for all managers to converge to a new version in
// Stable state.
func refreshAndWait(t *testing.T, managers []*parti.Manager) {
	t.Helper()

	v := maxVersion(managers)
	leader := findLeader(t, managers)
	refreshPartitions(t, leader)
	t.Log("Waiting for all managers to stabilize after refresh...")
	waitForVersionChange(t, managers, v)
}

// ---------------------------------------------------------------------------
// testEnv provides a reusable test environment (NATS, context, logger).
// ---------------------------------------------------------------------------

// testEnv bundles the common test infrastructure every assignment integration
// test needs: an embedded NATS server, a root context, and a logger.
type testEnv struct {
	Conn   *nats.Conn
	Ctx    context.Context
	Cancel context.CancelFunc
	Logger parti.Logger
}

// newTestEnv starts an embedded NATS server, creates a root context with the
// given timeout, and returns a testEnv. NATS and the context are cleaned up
// automatically when the test finishes.
func newTestEnv(t *testing.T, timeout time.Duration) *testEnv {
	t.Helper()

	srv, conn := partitest.StartEmbeddedNATS(t)
	t.Cleanup(func() {
		conn.Close()
		srv.Shutdown()
	})

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)

	return &testEnv{
		Conn:   conn,
		Ctx:    ctx,
		Cancel: cancel,
		Logger: logging.NewNop(),
	}
}

// SetupManagers creates n Manager instances using the testEnv's NATS connection
// and registers cleanup to stop them all when the test finishes.
func (e *testEnv) SetupManagers(
	t *testing.T,
	cfg *parti.Config,
	src parti.PartitionSource,
	assignStrategy parti.AssignmentStrategy,
	n int,
) []*parti.Manager {
	t.Helper()

	managers := setupManagers(t, e.Conn, cfg, src, assignStrategy, n, e.Logger)
	t.Cleanup(func() { cleanupManagers(t, managers) })

	return managers
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func toManagerWaiters(managers []*parti.Manager) []testutil.ManagerWaiter {
	waiters := make([]testutil.ManagerWaiter, len(managers))
	for i, m := range managers {
		waiters[i] = m
	}

	return waiters
}
