package assignment

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// TestCalculator_LeaderRevision_EmbeddedInAssignment verifies that the LeaderRevision
// function from Config is called during rebalance and its value is embedded in
// the published assignments read back from KV.
func TestCalculator_LeaderRevision_EmbeddedInAssignment(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-splitbrain-epoch-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-splitbrain-epoch-heartbeat")

	_, err := heartbeatKV.Put(ctx, "heartbeat.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	const wantRevision uint64 = 99

	calc, err := NewCalculator(&Config{
		AssignmentKV:       assignmentKV,
		HeartbeatKV:        heartbeatKV,
		AssignmentPrefix:   "assignment",
		HeartbeatPrefix:    "heartbeat",
		HeartbeatTTL:       2 * time.Second,
		ColdStartWindow:    50 * time.Millisecond,
		PlannedScaleWindow: 50 * time.Millisecond,
		Cooldown:           1 * time.Millisecond,
		Source: &mockSource{partitions: []types.Partition{
			{Keys: []string{"p1"}},
		}},
		Strategy:       &mockStrategy{},
		LeaderRevision: func() uint64 { return wantRevision },
	})
	require.NoError(t, err)

	require.NoError(t, calc.Start(ctx))
	t.Cleanup(func() { _ = calc.Stop(ctx) })

	// Wait for initial rebalance to publish at least one assignment.
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > 0
	}, 5*time.Second, 50*time.Millisecond, "initial rebalance must complete")

	// Read the published assignment from KV and check LeaderRevision.
	entry, err := assignmentKV.Get(ctx, "assignment.worker-1")
	require.NoError(t, err)

	var asgn types.Assignment
	require.NoError(t, json.Unmarshal(entry.Value(), &asgn))
	require.Equal(t, wantRevision, asgn.LeaderRevision,
		"LeaderRevision must be embedded in the published assignment")
}

// ============================================================================
// P9.2: Pre-publish shutdown check blocks publish
// ============================================================================

// TestCalculator_Rebalance_AbortsOnShutdown verifies that rebalance skips
// publishing when the StateProvider reports StateShutdown, preventing
// split-brain assignments from a leader that is shutting down.
func TestCalculator_Rebalance_AbortsOnShutdown(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-splitbrain-shutdown-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-splitbrain-shutdown-heartbeat")

	_, err := heartbeatKV.Put(ctx, "heartbeat.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	stateProvider := &mockStateProvider{}
	// Leave state as stable (zero value) for initial rebalance; we'll flip it before TriggerRebalance.

	calc, err := NewCalculator(&Config{
		AssignmentKV:       assignmentKV,
		HeartbeatKV:        heartbeatKV,
		AssignmentPrefix:   "assignment",
		HeartbeatPrefix:    "heartbeat",
		HeartbeatTTL:       2 * time.Second,
		ColdStartWindow:    50 * time.Millisecond,
		PlannedScaleWindow: 50 * time.Millisecond,
		Cooldown:           1 * time.Millisecond,
		Source: &mockSource{partitions: []types.Partition{
			{Keys: []string{"p1"}},
		}},
		Strategy:      &mockStrategy{},
		StateProvider: stateProvider,
	})
	require.NoError(t, err)

	require.NoError(t, calc.Start(ctx))
	t.Cleanup(func() { _ = calc.Stop(ctx) })

	// Wait for the initial assignment to be published.
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > 0
	}, 5*time.Second, 50*time.Millisecond, "initial rebalance must complete")

	versionAfterStart := calc.CurrentVersion()

	// Flip state to shutdown before triggering another rebalance.
	stateProvider.SetState(types.StateShutdown)

	// TriggerRebalance must fail with the sentinel shutdown error and must NOT increment the version.
	err = calc.TriggerRebalance(ctx)
	require.ErrorIs(t, err, errShuttingDown, "rebalance must return errShuttingDown when manager is shutting down")
	require.Equal(t, versionAfterStart, calc.CurrentVersion(),
		"version must not increment when rebalance is aborted due to shutdown")
}
