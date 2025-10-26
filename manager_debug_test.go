package parti_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// TestManager_Debug is a simplified test for debugging.
func TestManager_Debug(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping debug test in short mode")
	}

	// Start NATS server
	srv, conn := partitest.StartEmbeddedNATS(t)
	defer srv.Shutdown()
	defer conn.Close()

	// Create config with very short windows
	cfg := parti.Config{
		WorkerIDPrefix:        "debug-worker",
		WorkerIDMin:           0,
		WorkerIDMax:           99,
		WorkerIDTTL:           10 * time.Second,
		HeartbeatInterval:     500 * time.Millisecond,
		HeartbeatTTL:          2 * time.Second,
		ElectionTimeout:       2 * time.Second,
		StartupTimeout:        30 * time.Second,
		ShutdownTimeout:       5 * time.Second,
		ColdStartWindow:       1 * time.Second,
		PlannedScaleWindow:    500 * time.Millisecond,
		RestartDetectionRatio: 0.5,
	}

	// Create partition source
	partitions := []types.Partition{
		{Keys: []string{"partition-1"}, Weight: 100},
	}
	src := source.NewStatic(partitions)

	// Create assignment strategy
	strategy := strategy.NewConsistentHash()

	// Create manager
	mgr, err := parti.NewManager(&cfg, conn, src, strategy)
	require.NoError(t, err)
	require.NotNil(t, mgr)

	// Start manager
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	t.Log("Starting manager...")
	err = mgr.Start(ctx)
	if err != nil {
		t.Logf("Start error: %v", err)
	}
	require.NoError(t, err)

	t.Logf("Manager started. WorkerID: %s, IsLeader: %v, State: %v",
		mgr.WorkerID(), mgr.IsLeader(), mgr.State())

	assignment := mgr.CurrentAssignment()
	t.Logf("Assignment: %d partitions, version %d", len(assignment.Partitions), assignment.Version)

	// Stop manager
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()

	err = mgr.Stop(stopCtx)
	require.NoError(t, err)
}
