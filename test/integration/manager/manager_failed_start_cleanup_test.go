package manager_test

import (
	"context"
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

// TestFailedStart_AutoCleanupContract verifies the public Start() contract
// documented at manager.go:
//
//	"If Start returns an error, all partially-acquired resources (KV leases,
//	background goroutines, election state) are automatically cleaned up.
//	The caller does NOT need to call Stop after a failed Start."
//
// Concretely: after a failed Start, the manager must be left in a terminal
// state (StateShutdown), and calling Stop a second time must be a benign
// no-op returning ErrNotStarted rather than causing a panic or hang.
//
// The failure we inject is stable-ID-pool exhaustion: a worker pool of size 1
// is already claimed, so a second manager's claimWorkerID call fails.
func TestFailedStart_AutoCleanupContract(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	// Pool of exactly 1 ID — the second manager will fail claim.
	// Use a non-zero range because SetDefaults (invoked by NewManager)
	// treats zero values as "unset" and restores the default WorkerIDMax.
	cfg.WorkerIDMin = 1
	cfg.WorkerIDMax = 1

	src := source.NewStatic(testutil.CreateTestPartitions(3))
	assignStrat := strategy.NewConsistentHash()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	// First manager grabs the only ID.
	first, err := parti.NewManager(&cfg, js, src, assignStrat)
	require.NoError(t, err)
	require.NoError(t, first.Start(ctx))
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = first.Stop(stopCtx)
	}()

	// Second manager must fail to claim a stable ID and return an error
	// from Start. Use a short-ish ctx so the claim loop doesn't iterate long.
	second, err := parti.NewManager(&cfg, js, src, assignStrat)
	require.NoError(t, err)

	startCtx, startCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer startCancel()
	err = second.Start(startCtx)
	require.Error(t, err, "second manager should fail to claim stable ID")

	// Contract: auto-cleanup ran, so manager is in a terminal state.
	require.Equal(t, parti.StateShutdown, second.State(),
		"failed Start should leave manager in StateShutdown after auto-cleanup")

	// Contract: caller does NOT need to call Stop. If they do anyway, it must
	// be benign — ErrNotStarted signals "already cleaned up", not a new fault.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	err = second.Stop(stopCtx)
	require.ErrorIs(t, err, types.ErrNotStarted,
		"Stop after auto-cleanup must be a benign no-op returning ErrNotStarted")
}
