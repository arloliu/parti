package assignment

import (
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// TestCalculator_EmergencyBypassesCooldown verifies that emergency rebalancing
// ignores the rate-limiting cooldown period.
func TestCalculator_EmergencyBypassesCooldown(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-emergency-priority-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-emergency-priority-heartbeat")

	// Setup: 3 workers initially
	workers := []string{"worker-1", "worker-2", "worker-3"}
	for _, w := range workers {
		_, err := heartbeatKV.Put(ctx, "worker-hb."+w, []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)
	}

	source := &mockSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
		},
	}
	strategy := &mockStrategy{}

	// Config with LONG cooldown but SHORT grace period
	cooldown := 5 * time.Second
	gracePeriod := 200 * time.Millisecond

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         200 * time.Millisecond,
		EmergencyGracePeriod: gracePeriod,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             cooldown, // Long cooldown!
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial assignment
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > 0
	}, 2*time.Second, 50*time.Millisecond, "initial assignment failed")

	initialVersion := calc.CurrentVersion()
	t.Logf("Initial version: %d", initialVersion)

	// Trigger Emergency: Delete worker-3
	// This should trigger emergency rebalance after grace period (200ms)
	// ignoring the 5s cooldown.
	err = heartbeatKV.Delete(ctx, "worker-hb.worker-3")
	require.NoError(t, err)

	// Wait for grace period + monitoring cycle
	// Should be much faster than cooldown (5s)
	start := time.Now()
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > initialVersion
	}, 2*time.Second, 50*time.Millisecond, "emergency rebalance failed to bypass cooldown")

	duration := time.Since(start)
	t.Logf("Emergency rebalance took %v (cooldown was %v)", duration, cooldown)
	require.Less(t, duration, cooldown, "rebalance should have happened faster than cooldown")
}

// TestCalculator_PlannedScaleRespectsCooldown verifies that normal scaling
// still respects the rate-limiting cooldown period.
func TestCalculator_PlannedScaleRespectsCooldown(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-planned-cooldown-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-planned-cooldown-heartbeat")

	// Setup: 1 worker initially
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{{Keys: []string{"p1"}}},
	}
	strategy := &mockStrategy{}

	cooldown := 1 * time.Second

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         200 * time.Millisecond,
		EmergencyGracePeriod: 100 * time.Millisecond,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             cooldown,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial assignment
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > 0
	}, 2*time.Second, 50*time.Millisecond)

	// Wait for ColdStartWindow (50ms) to complete and trigger its rebalance
	// This ensures we are in a stable state before testing planned scale
	time.Sleep(100 * time.Millisecond)

	initialVersion := calc.CurrentVersion()
	t.Logf("Initial version (after cold start): %d", initialVersion)

	// Trigger Planned Scale: Add worker-2
	// This should be BLOCKED by cooldown initially
	// Last rebalance was ~50ms ago (ColdStart). Cooldown is 1s.
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Check shortly after (within cooldown)
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, initialVersion, calc.CurrentVersion(), "planned scale should be blocked by cooldown")

	// Wait for cooldown to expire
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > initialVersion
	}, 2*time.Second, 100*time.Millisecond, "planned scale should proceed after cooldown")
}

// TestCalculator_EmergencyDuringScaling verifies that emergency rebalancing
// interrupts an active scaling window and doesn't cause duplicate rebalances.
func TestCalculator_EmergencyDuringScaling(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-emergency-scaling-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-emergency-scaling-heartbeat")

	// Setup: 2 workers initially
	workers := []string{"worker-1", "worker-2"}
	for _, w := range workers {
		_, err := heartbeatKV.Put(ctx, "worker-hb."+w, []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)
	}

	source := &mockSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
		},
	}
	strategy := &mockStrategy{}

	// Long scaling window so we can crash a worker during it
	scalingWindow := 2 * time.Second
	gracePeriod := 100 * time.Millisecond

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         200 * time.Millisecond,
		EmergencyGracePeriod: gracePeriod,
		ColdStartWindow:      scalingWindow, // Long window
		PlannedScaleWindow:   scalingWindow,
		Cooldown:             100 * time.Millisecond,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial assignment and scaling state
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > 0 && calc.GetState() == types.CalcStateScaling
	}, 1*time.Second, 50*time.Millisecond, "should be in scaling state")

	versionAfterImmediate := calc.CurrentVersion()
	t.Logf("Version after immediate assignment: %d, state: %s", versionAfterImmediate, calc.GetState())

	// Now crash a worker while in Scaling state
	err = heartbeatKV.Delete(ctx, "worker-hb.worker-2")
	require.NoError(t, err)

	// Wait for emergency rebalance (should happen within grace period + monitoring cycle)
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > versionAfterImmediate
	}, 1*time.Second, 50*time.Millisecond, "emergency should trigger during scaling")

	versionAfterEmergency := calc.CurrentVersion()
	t.Logf("Version after emergency: %d", versionAfterEmergency)

	// Wait a bit longer than the scaling window to ensure no extra rebalance
	// from the orphaned scaling timer
	time.Sleep(scalingWindow + 500*time.Millisecond)

	finalVersion := calc.CurrentVersion()
	t.Logf("Final version after scaling window expired: %d", finalVersion)

	// The version should NOT have increased again from the orphaned timer
	require.Equal(t, versionAfterEmergency, finalVersion,
		"orphaned scaling timer should NOT trigger extra rebalance")
}
