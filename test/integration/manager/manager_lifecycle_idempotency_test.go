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

// TestManager_StartAfterStop locks in the documented lifecycle contract:
// Manager instances are single-use. After Stop, a subsequent Start returns
// ErrAlreadyStarted because the manager deliberately keeps m.ctx non-nil
// after shutdown so background goroutines can still observe the cancellation
// via their select statements (see manager.go Stop comment).
//
// Callers that need to "restart" a worker must construct a new Manager.
func TestManager_StartAfterStop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	src := source.NewStatic(testutil.CreateTestPartitions(3))
	assignStrat := strategy.NewConsistentHash()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	mgr, err := parti.NewManager(&cfg, js, src, assignStrat)
	require.NoError(t, err)

	// First Start: success. Manager.Start now returns at
	// WaitingAssignment; wait for the background runner to reach Stable.
	require.NoError(t, mgr.Start(ctx))
	require.NoError(t, <-mgr.WaitState(parti.StateStable, 10*time.Second))
	require.Equal(t, parti.StateStable, mgr.State())

	// Stop succeeds.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	require.NoError(t, mgr.Stop(stopCtx))
	require.Equal(t, parti.StateShutdown, mgr.State())

	// Second Start: must fail deterministically. Manager is single-use.
	err = mgr.Start(ctx)
	require.ErrorIs(t, err, types.ErrAlreadyStarted,
		"Start after Stop must return ErrAlreadyStarted — managers are single-use")

	// Second Stop: documented behavior is ErrNotStarted (already in shutdown).
	err = mgr.Stop(stopCtx)
	require.ErrorIs(t, err, types.ErrNotStarted)
}
