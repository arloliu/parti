package assignment

import (
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// TestCalculator_RecoveryGrace_BlocksNormalRebalance verifies that non-emergency
// rebalancing is deferred while the StateProvider reports IsInRecoveryGrace == true.
func TestCalculator_RecoveryGrace_BlocksNormalRebalance(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-recovery-grace-blocked-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-recovery-grace-blocked-heartbeat")

	// Register initial worker
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
		},
	}
	strategy := &mockStrategy{}

	stateProvider := &mockStateProvider{}
	stateProvider.SetGrace(true) // activate recovery grace from the start

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         2 * time.Second,
		EmergencyGracePeriod: 5 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             1 * time.Millisecond,
		StateProvider:        stateProvider,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for any initial activity to settle
	time.Sleep(150 * time.Millisecond)
	initialVersion := calc.CurrentVersion()

	// Add a second worker — this would normally trigger a scaling rebalance
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Wait long enough for the watcher to fire and attempt checkForChanges
	time.Sleep(300 * time.Millisecond)

	// Version must NOT change — recovery grace suppresses non-emergency rebalancing
	blockedVersion := calc.CurrentVersion()
	require.Equal(t, initialVersion, blockedVersion,
		"recovery grace must block non-emergency rebalancing")
}

// TestCalculator_RecoveryGrace_AllowsRebalanceAfterGraceExpires verifies that normal
// rebalancing resumes once IsInRecoveryGrace returns false.
func TestCalculator_RecoveryGrace_AllowsRebalanceAfterGraceExpires(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-recovery-grace-allowed-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-recovery-grace-allowed-heartbeat")

	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
		},
	}
	strategy := &mockStrategy{}

	stateProvider := &mockStateProvider{}
	stateProvider.SetGrace(true) // start in recovery grace

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         2 * time.Second, // long TTL so heartbeats outlive the test
		EmergencyGracePeriod: 5 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             1 * time.Millisecond,
		StateProvider:        stateProvider,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for the initial cold_start stabilization to complete before measuring
	time.Sleep(500 * time.Millisecond)
	initialVersion := calc.CurrentVersion()

	// Add worker-2 while grace is active — should be blocked
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)

	blockedVersion := calc.CurrentVersion()
	require.Equal(t, initialVersion, blockedVersion, "rebalance must be blocked during grace period")

	// Exit recovery grace; re-send heartbeat for worker-2 so the watcher fires again
	stateProvider.SetGrace(false)
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// The watcher fires within ~100ms (debounce), then scaling window is 50ms.
	// Use Eventually with a generous timeout to avoid flakiness.
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > initialVersion
	}, 5*time.Second, 50*time.Millisecond,
		"rebalance must proceed after recovery grace expires (initial version: %d, current: %d)",
		initialVersion, calc.CurrentVersion(),
	)
}
