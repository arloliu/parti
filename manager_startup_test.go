package parti_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestStart_ReturnsBeforeStable pins the new contract: Start returns once
// sanity checks (worker ID, buckets, election, heartbeat, calculator) are
// wired. State at return may be WaitingAssignment OR any later valid
// state (the background runner may have completed; the calculator may
// have projected Scaling/Rebalancing/Emergency). The post-Stable monitors
// drive recovery from here.
func TestStart_ReturnsBeforeStable(t *testing.T) {
	nc, cleanupNATS := testutil.StartEmbeddedNATS(t)
	defer cleanupNATS()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	src := source.NewStatic(testutil.CreateTestPartitions(3))

	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, mgr.Start(ctx))

	require.NotEmpty(t, mgr.WorkerID(), "worker ID must be claimed synchronously in Start")

	s := mgr.State()
	require.Truef(t,
		s == types.StateWaitingAssignment ||
			s == types.StateStable ||
			s == types.StateScaling ||
			s == types.StateRebalancing ||
			s == types.StateEmergency,
		"state after Start must be WaitingAssignment or a later active state; got %v", s)

	require.NoError(t, <-mgr.WaitState(types.StateStable, 5*time.Second))
	require.NotEmpty(t, mgr.CurrentAssignment().Partitions)
}

// TestStart_StopDuringBackground_NoDegraded asserts that calling Stop
// while the background runner is mid-flight transitions cleanly to
// Shutdown without leaving Degraded residue. The runner's only blocking
// operations are waitForAssignment and applyInitialAssignment, both of
// which honor m.ctx via the standard JetStream/NATS plumbing.
func TestStart_StopDuringBackground_NoDegraded(t *testing.T) {
	nc, cleanupNATS := testutil.StartEmbeddedNATS(t)
	defer cleanupNATS()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	cfg.StartupTimeout = 30 * time.Second // long, so Stop wins the race

	// A static source so the sync phase completes; the leader publishes
	// the initial assignment immediately, so the runner reaches its
	// apply step quickly. We want Stop to land somewhere in the
	// run+monitor-start sequence; a few millisecond gap suffices.
	src := source.NewStatic(testutil.CreateTestPartitions(2))

	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, mgr.Start(ctx))

	require.NoError(t, mgr.Stop(context.Background()))
	require.Equal(t, types.StateShutdown, mgr.State())
}

// Note: TestStart_WatchdogFiresAfterStartupTimeout is in
// manager_startup_async_cas_test.go as a unit-level test driving
// startStartupTimeoutWatchdog directly. See tmp/impl-deviations.md
// for the reason — the plan's integration form does not work
// because StartupTimeout also bounds the synchronous sanity ctx in
// prepareStart, so setting it to 1ms kills bucket creation before
// Start can return.

// TestStart_EmptySource_ReachesStable asserts that when the partition
// source is empty at startup (leader has nothing to publish), the
// worker still reaches Stable and OnPartitionsAssigned/Revoked are not
// fired (empty diff). OnAssignmentChanged fires AT LEAST ONCE — the
// leader publishes a Version=1 empty assignment that
// applyAssignmentWithPrev's hook code reports as a change from initial
// state (Version=0 empty → Version=1 empty), and the calculator may
// re-publish during settling, producing additional empty fires. Every
// fire must carry empty old + empty new (asserted via max-length
// atomics below). The plan called this "cold-start-empty bypass" but
// the bypass at manager.go:603-633 only triggers when
// initial.Version == 0 (no leader has published) — which does not
// arise for a single-worker cluster that is its own leader.
// See tmp/impl-deviations.md.
func TestStart_EmptySource_ReachesStable(t *testing.T) {
	nc, cleanupNATS := testutil.StartEmbeddedNATS(t)
	defer cleanupNATS()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	src := source.NewStatic(nil) // empty source

	var (
		assignmentChanged, partitionsAssigned, partitionsRevoked atomic.Int32
		// Track the largest old/new partition length ever observed by
		// OnAssignmentChanged — empty-source path must NEVER see
		// non-empty partition slices.
		maxOldLen, maxNewLen atomic.Int32
	)
	hooks := &parti.Hooks{
		OnAssignmentChanged: func(_ context.Context, oldPartitions, newPartitions []parti.Partition) error {
			assignmentChanged.Add(1)
			if n := int32(len(oldPartitions)); n > maxOldLen.Load() {
				maxOldLen.Store(n)
			}
			if n := int32(len(newPartitions)); n > maxNewLen.Load() {
				maxNewLen.Store(n)
			}

			return nil
		},
		OnPartitionsAssigned: func(_ context.Context, _ []parti.Partition) error {
			partitionsAssigned.Add(1)
			return nil
		},
		OnPartitionsRevoked: func(_ context.Context, _ []parti.Partition) error {
			partitionsRevoked.Add(1)
			return nil
		},
	}

	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(), parti.WithHooks(hooks))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, mgr.Start(ctx))
	require.NoError(t, <-mgr.WaitState(parti.StateStable, 5*time.Second))

	// OnAssignmentChanged must fire at least once for the Version=1
	// empty assignment. Empty-source path: every fire must carry empty
	// old + empty new — if any fire ever observed non-empty partitions,
	// the runner is delivering wrong content to hooks.
	require.Eventually(t, func() bool {
		return assignmentChanged.Load() >= 1
	}, 2*time.Second, 20*time.Millisecond,
		"empty source: OnAssignmentChanged must fire at least once for the Version=1 empty assignment")
	require.Equal(t, int32(0), maxOldLen.Load(),
		"empty source: OnAssignmentChanged must never see non-empty old partitions")
	require.Equal(t, int32(0), maxNewLen.Load(),
		"empty source: OnAssignmentChanged must never see non-empty new partitions")

	// Empty-source path always yields zero partitions, so derived hooks
	// must not fire (added/removed are both empty).
	require.Equal(t, int32(0), partitionsAssigned.Load(),
		"empty source: no partitions to assign")
	require.Equal(t, int32(0), partitionsRevoked.Load(),
		"empty source: no partitions to revoke")

	require.Empty(t, mgr.CurrentAssignment().Partitions)
}

// TestStart_EmptySliceFromNonEmptyCluster_HookFiresEmptyEmpty asserts the
// Path B case: leader publishes a non-empty assignment (Version > 0) but
// this worker's slice is empty (more workers than partitions, or strategy
// distribution leaves the worker with nothing). Because Version > 0, the
// cold-start-empty branch does not match — control flows through
// applyAssignmentWithPrev which fires OnAssignmentChanged([], []) and
// then UpdateWorkerConsumer(ctx, workerID, []). UpdateWorkerConsumer's
// idempotency contract requires this to be safe.
//
// 2 workers + 1 partition under consistent hash: exactly one worker gets
// the partition; the other draws empty. Asserts:
//   - Both workers reach Stable.
//   - The worker with empty slice fires OnAssignmentChanged with empty new.
//   - OnPartitionsAssigned/Revoked do NOT fire on the empty-slice worker.
func TestStart_EmptySliceFromNonEmptyCluster_HookFiresEmptyEmpty(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	partitions := testutil.CreateTestPartitions(1) // single partition
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()

	type hookCounts struct {
		assignmentChanged, partitionsAssigned, partitionsRevoked atomic.Int32
		lastOldLen, lastNewLen                                   atomic.Int32
	}
	counts := make([]*hookCounts, 2)
	makeHooks := func(idx int) *parti.Hooks {
		counts[idx] = &hookCounts{}
		return &parti.Hooks{
			OnAssignmentChanged: func(_ context.Context, oldP, newP []parti.Partition) error {
				counts[idx].assignmentChanged.Add(1)
				counts[idx].lastOldLen.Store(int32(len(oldP))) //nolint:gosec // test sizes bounded
				counts[idx].lastNewLen.Store(int32(len(newP))) //nolint:gosec // test sizes bounded
				return nil
			},
			OnPartitionsAssigned: func(_ context.Context, _ []parti.Partition) error {
				counts[idx].partitionsAssigned.Add(1)
				return nil
			},
			OnPartitionsRevoked: func(_ context.Context, _ []parti.Partition) error {
				counts[idx].partitionsRevoked.Add(1)
				return nil
			},
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	mgrs := make([]*parti.Manager, 2)
	for i := range 2 {
		m, err := parti.NewManager(&cfg, js, src, assignStrat, parti.WithHooks(makeHooks(i)))
		require.NoError(t, err)
		require.NoError(t, m.Start(ctx))
		mgrs[i] = m
		time.Sleep(150 * time.Millisecond)
	}
	t.Cleanup(func() {
		for _, m := range mgrs {
			_ = m.Stop(context.Background())
		}
	})

	for i, m := range mgrs {
		require.NoErrorf(t, <-m.WaitState(parti.StateStable, 10*time.Second),
			"worker %d did not reach StateStable", i)
	}
	time.Sleep(300 * time.Millisecond) // let async hook dispatch settle

	// Identify which worker drew empty vs the partition.
	var emptyIdx, ownerIdx int
	for i, m := range mgrs {
		if len(m.CurrentAssignment().Partitions) == 0 {
			emptyIdx = i
		} else {
			ownerIdx = i
		}
	}
	require.NotEqual(t, emptyIdx, ownerIdx, "exactly one worker should hold the partition")

	// The empty-slice worker fired OnAssignmentChanged with ([], []).
	require.GreaterOrEqual(t, counts[emptyIdx].assignmentChanged.Load(), int32(1),
		"empty-slice worker must fire OnAssignmentChanged at least once")
	require.Equal(t, int32(0), counts[emptyIdx].lastNewLen.Load(),
		"empty-slice worker's last OnAssignmentChanged new should be []")

	// Empty diff: derived hooks must not fire on the empty-slice worker.
	require.Equal(t, int32(0), counts[emptyIdx].partitionsAssigned.Load(),
		"empty-slice worker: no partitions to assign")
	require.Equal(t, int32(0), counts[emptyIdx].partitionsRevoked.Load(),
		"empty-slice worker: no partitions to revoke")

	// The owning worker received its partition cleanly.
	require.GreaterOrEqual(t, counts[ownerIdx].partitionsAssigned.Load(), int32(1),
		"owner worker must see OnPartitionsAssigned at least once")
}
