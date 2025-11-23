package integration_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/internal/testutil"
	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// mockUpdater records UpdateWorkerConsumer invocations.
type mockUpdater struct{ calls atomic.Int32 }

func (m *mockUpdater) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []parti.Partition) error {
	m.calls.Add(1)
	return nil
}

func TestManager_TwoPhaseFlag_CoordinatorPathInvoked(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}

	cases := []struct {
		name   string
		enable bool
	}{
		{"flag_disabled_uses_direct", false},
		{"flag_enabled_uses_twophase_stub", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			nc, cleanup := testutil.StartEmbeddedNATS(t)
			defer cleanup()
			js, err := jetstream.New(nc)
			require.NoError(t, err)

			parts := []parti.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}}
			src := source.NewStatic(parts)
			curStrategy := strategy.NewConsistentHash()
			upd := &mockUpdater{}

			cfg := parti.TestConfig()
			cfg.EnableTwoPhaseHandoff = tc.enable

			mgr, err := parti.NewManager(&cfg, js, src, curStrategy, parti.WithWorkerConsumerUpdater(upd))
			require.NoError(t, err)
			require.NoError(t, mgr.Start(ctx))
			t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

			waitForAssignmentReady(t, mgr, len(parts), 5*time.Second)

			// Trigger a rebalance via RefreshPartitions (leader only) to cause a second Apply.
			if mgr.IsLeader() {
				refreshCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				_ = mgr.RefreshPartitions(refreshCtx) // ignore errors; we only want an attempt
				cancel()
				// Wait briefly for potential assignment change (non-strict; updater may be invoked again)
				time.Sleep(300 * time.Millisecond)
			}

			// Expect that the coordinator path invoked the updater at least once (initial) and possibly again.
			require.GreaterOrEqual(t, int(upd.calls.Load()), 1)
		})
	}
}

// waitForAssignment polls CurrentAssignment until version > 0 and partition count matches expected.
func waitForAssignmentReady(t *testing.T, mgr *parti.Manager, expectedCount int, timeout time.Duration) parti.Assignment {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		assign := mgr.CurrentAssignment()
		if assign.Version > 0 && len(assign.Partitions) == expectedCount {
			return assign
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("assignment not ready within %v", timeout)

	return parti.Assignment{}
}
