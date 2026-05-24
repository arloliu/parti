package parti_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

type mockSource struct {
	partitions []types.Partition
}

func (s *mockSource) Start(context.Context) error { return nil }
func (s *mockSource) Stop(context.Context) error  { return nil }
func (s *mockSource) List(context.Context) ([]types.Partition, error) {
	return s.partitions, nil
}

func TestManager_PartitionHooks(t *testing.T) {
	t.Parallel()

	// Start embedded NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create manager with hooks
	var assignedCount atomic.Int32
	var revokedCount atomic.Int32
	var assignedParts atomic.Value
	var revokedParts atomic.Value

	hooks := &types.Hooks{
		OnPartitionsAssigned: func(ctx context.Context, partitions []types.Partition) error {
			assignedCount.Add(1)
			assignedParts.Store(partitions)
			return nil
		},
		OnPartitionsRevoked: func(ctx context.Context, partitions []types.Partition) error {
			revokedCount.Add(1)
			revokedParts.Store(partitions)
			return nil
		},
	}

	cfg := parti.DefaultConfig()
	// Speed up startup
	cfg.StartupTimeout = 5 * time.Second
	cfg.WorkerIDTTL = 2 * time.Second
	cfg.HeartbeatTTL = 1 * time.Second
	cfg.HeartbeatInterval = 500 * time.Millisecond
	cfg.EmergencyGracePeriod = 750 * time.Millisecond

	src := &mockSource{}
	assignStrategy := strategy.NewRoundRobin()

	mgr, err := parti.NewManager(
		&cfg,
		js,
		src,
		assignStrategy,
		parti.WithHooks(hooks),
	)
	require.NoError(t, err)
	defer func() { _ = mgr.Stop(context.Background()) }()

	// Start manager
	err = mgr.Start(context.Background())
	require.NoError(t, err)

	// Wait for manager to be ready. Manager.Start now returns at
	// StateWaitingAssignment (post-refactor) and the background runner
	// transitions to Stable once the initial apply completes. With an
	// empty source under parti.DefaultConfig the calculator may project
	// Scaling/Rebalancing before the apply lands — accept those active
	// states as "ready enough" for the test's churn-then-assert pattern.
	require.Eventually(t, func() bool {
		s := mgr.State()
		return s == parti.StateStable || s == parti.StateWaitingAssignment ||
			s == parti.StateScaling || s == parti.StateRebalancing
	}, 5*time.Second, 100*time.Millisecond)

	// Update source with partitions
	partitions := []types.Partition{
		{Keys: []string{"p1"}},
		{Keys: []string{"p2"}},
	}
	src.partitions = partitions

	// Trigger refresh
	err = mgr.RefreshPartitions(context.Background())
	require.NoError(t, err)

	// Verify OnPartitionsAssigned called
	require.Eventually(t, func() bool {
		return assignedCount.Load() > 0
	}, 5*time.Second, 100*time.Millisecond)

	val := assignedParts.Load()
	require.NotNil(t, val)
	p, ok := val.([]types.Partition)
	require.True(t, ok, "expected []types.Partition")
	require.Len(t, p, 2)
	require.Equal(t, "p1", p[0].ID())
	require.Equal(t, "p2", p[1].ID())

	// Now update source to remove one partition
	src.partitions = []types.Partition{{Keys: []string{"p1"}}} // Removed p2

	// Trigger refresh
	err = mgr.RefreshPartitions(context.Background())
	require.NoError(t, err)

	// Verify OnPartitionsRevoked called
	require.Eventually(t, func() bool {
		return revokedCount.Load() > 0
	}, 5*time.Second, 100*time.Millisecond)

	val = revokedParts.Load()
	require.NotNil(t, val)
	p, ok = val.([]types.Partition)
	require.True(t, ok, "expected []types.Partition")
	require.Len(t, p, 1)
	require.Equal(t, "p2", p[0].ID())
}

func TestManager_DegradedHook(t *testing.T) {
	t.Parallel()

	// Start embedded NATS using partitest to get access to server
	srv, nc := partitest.StartEmbeddedNATS(t)
	defer func() {
		nc.Close()
		srv.Shutdown()
		srv.WaitForShutdown()
	}()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	var degradedCount atomic.Int32
	var degradedReason atomic.Value

	hooks := &types.Hooks{
		OnDegraded: func(ctx context.Context, reason string) error {
			degradedCount.Add(1)
			degradedReason.Store(reason)
			return nil
		},
	}

	// Configure manager with short thresholds for faster testing
	cfg := parti.DefaultConfig()
	cfg.DegradedBehavior.EnterThreshold = 100 * time.Millisecond
	cfg.StartupTimeout = 5 * time.Second

	src := source.NewStatic(nil)
	assignStrategy := strategy.NewRoundRobin()

	mgr, err := parti.NewManager(
		&cfg,
		js,
		src,
		assignStrategy,
		parti.WithHooks(hooks),
	)
	require.NoError(t, err)
	defer func() { _ = mgr.Stop(context.Background()) }()

	err = mgr.Start(context.Background())
	require.NoError(t, err)

	// Wait for manager to be ready. Manager.Start now returns at
	// WaitingAssignment; the calculator may project Scaling/Rebalancing
	// during initial apply.
	require.Eventually(t, func() bool {
		s := mgr.State()
		return s == parti.StateStable || s == parti.StateWaitingAssignment ||
			s == parti.StateScaling || s == parti.StateRebalancing
	}, 5*time.Second, 100*time.Millisecond)

	// Wait for the background runner's apply step to finish + post-Stable
	// monitors to start. With parti.DefaultConfig the calculator may keep
	// state in Scaling for a while; what we need is for monitorNATSConnection
	// (started by startPostStableMonitors at the end of the runner) to
	// be live before NATS goes down, otherwise the connection-loss
	// detector can not run.
	require.Eventually(t, func() bool {
		// CurrentAssignment is set as part of the initial apply pipeline;
		// once non-nil the runner has reached startPostStableMonitors.
		return mgr.CurrentAssignment().Version > 0
	}, 5*time.Second, 50*time.Millisecond, "runner did not complete initial apply (monitors not started)")

	// Stop NATS to trigger degraded mode
	srv.Shutdown()

	// Verify OnDegraded called
	require.Eventually(t, func() bool {
		return degradedCount.Load() > 0
	}, 10*time.Second, 100*time.Millisecond)

	val := degradedReason.Load()
	require.NotNil(t, val)
	reason, ok := val.(string)
	require.True(t, ok, "expected string reason")
	require.Contains(t, reason, "NATS connection down")
}

// orderingUpdater tracks the order in which UpdateWorkerConsumer is called
// relative to other operations using an atomic sequence counter.
type orderingUpdater struct {
	updateSeq *atomic.Int64 // sequence number when UpdateWorkerConsumer completes
	seqCtr    *atomic.Int64 // shared counter
}

func (u *orderingUpdater) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []types.Partition) error {
	// Increment and record our sequence number
	seq := u.seqCtr.Add(1)
	u.updateSeq.Store(seq)
	return nil
}

// TestManager_HandoffCompletesBeforeHooks verifies that the handoff coordinator's
// Apply method (which updates the consumer) completes BEFORE user hooks are invoked.
//
// This prevents the race condition where user code in OnAssignmentChanged expects
// to receive messages for new partitions, but the consumer subscription isn't
// updated yet.
func TestManager_HandoffCompletesBeforeHooks(t *testing.T) {
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Shared sequence counter
	seqCtr := &atomic.Int64{}

	// Track when UpdateWorkerConsumer completes
	updateSeq := &atomic.Int64{}
	updater := &orderingUpdater{updateSeq: updateSeq, seqCtr: seqCtr}

	// Track when OnAssignmentChanged is called
	hookSeq := &atomic.Int64{}
	hooks := &types.Hooks{
		OnAssignmentChanged: func(ctx context.Context, old, newParts []types.Partition) error {
			// Record sequence number when hook starts
			seq := seqCtr.Add(1)
			hookSeq.Store(seq)
			return nil
		},
	}

	cfg := parti.DefaultConfig()
	cfg.StartupTimeout = 5 * time.Second
	cfg.WorkerIDTTL = 2 * time.Second
	cfg.HeartbeatTTL = 1 * time.Second
	cfg.HeartbeatInterval = 500 * time.Millisecond
	cfg.EmergencyGracePeriod = 750 * time.Millisecond

	src := &mockSource{}
	assignStrategy := strategy.NewRoundRobin()

	mgr, err := parti.NewManager(
		&cfg,
		js,
		src,
		assignStrategy,
		parti.WithHooks(hooks),
		parti.WithWorkerConsumerUpdater(updater),
	)
	require.NoError(t, err)
	defer func() { _ = mgr.Stop(context.Background()) }()

	err = mgr.Start(context.Background())
	require.NoError(t, err)

	// Wait for manager to be ready. Manager.Start now returns at
	// WaitingAssignment; the calculator may project Scaling/Rebalancing
	// during initial apply.
	require.Eventually(t, func() bool {
		s := mgr.State()
		return s == parti.StateStable || s == parti.StateWaitingAssignment ||
			s == parti.StateScaling || s == parti.StateRebalancing
	}, 5*time.Second, 100*time.Millisecond)

	// Capture the consumer-seq baseline AFTER startup has fully settled. The
	// Phase 4 initial-bootstrap apply (synchronous-before-StateStable) plus
	// any early stabilization-window rebalance may both fire before we get
	// here, so we wait for the snapshot to quiesce before driving the next
	// change.
	require.Eventually(t, func() bool {
		// Quiesce: consumer-seq stops changing.
		s1 := updateSeq.Load()
		time.Sleep(150 * time.Millisecond)
		s2 := updateSeq.Load()
		return s1 == s2
	}, 5*time.Second, 100*time.Millisecond, "expected initial bootstrap applies to settle before test churn")
	baselineConsumerSeq := updateSeq.Load()
	baselineHookSeq := hookSeq.Load()

	// Update source with partitions to trigger an assignment change.
	src.partitions = []types.Partition{
		{Keys: []string{"p1"}},
		{Keys: []string{"p2"}},
	}

	// Trigger refresh.
	err = mgr.RefreshPartitions(context.Background())
	require.NoError(t, err)

	// Wait for the SUBSEQUENT consumer + hook chain to complete.
	require.Eventually(t, func() bool {
		return updateSeq.Load() > baselineConsumerSeq && hookSeq.Load() > baselineHookSeq
	}, 5*time.Second, 100*time.Millisecond, "expected RefreshPartitions-driven apply + hook to fire")

	// Verify ordering on the post-RefreshPartitions apply: the latest
	// consumer-seq is for the just-fired UpdateWorkerConsumer; the latest
	// hook-seq must be greater (the hook fires AFTER the consumer call
	// completes per the §4.4 ordering invariant).
	consumerSeq := updateSeq.Load()
	changeHookSeq := hookSeq.Load()
	t.Logf("UpdateWorkerConsumer completed at seq %d, OnAssignmentChanged called at seq %d", consumerSeq, changeHookSeq)
	require.Greater(t, changeHookSeq, consumerSeq,
		"OnAssignmentChanged (seq %d) should be called AFTER UpdateWorkerConsumer completes (seq %d)",
		changeHookSeq, consumerSeq)
}
