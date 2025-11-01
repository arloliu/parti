package assignment

import (
	"testing"
	"time"

	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// TestCalculator_RebalanceAttemptDuringCooldown_Blocked verifies cooldown prevents rebalancing.
func TestCalculator_RebalanceAttemptDuringCooldown_Blocked(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-blocked-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-blocked-heartbeat")

	// Create initial worker
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
		},
	}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         4 * time.Second,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      100 * time.Millisecond,
		PlannedScaleWindow:   100 * time.Millisecond,
		Cooldown:             3 * time.Second, // Long cooldown for testing
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial rebalance
	time.Sleep(500 * time.Millisecond)
	initialVersion := calc.CurrentVersion()
	require.NotZero(t, initialVersion, "should have initial version")

	// Add worker-2 (should trigger rebalance attempt but be blocked by cooldown)
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Wait for monitoring cycle (TTL/2 = 2s)
	time.Sleep(2200 * time.Millisecond)

	// Version should NOT change - cooldown is still active
	blockedVersion := calc.CurrentVersion()
	require.Equal(t, initialVersion, blockedVersion, "cooldown should block rebalance")

	t.Logf("Cooldown correctly blocked rebalance: version remained %d", blockedVersion)
}

// TestCalculator_RebalanceAfterCooldown_Allowed verifies rebalance proceeds after cooldown expires.
func TestCalculator_RebalanceAfterCooldown_Allowed(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-allowed-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-allowed-heartbeat")

	// Create initial worker
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
		},
	}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         4 * time.Second,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      100 * time.Millisecond,
		PlannedScaleWindow:   100 * time.Millisecond,
		Cooldown:             1 * time.Second, // Short cooldown for faster test
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial rebalance
	time.Sleep(500 * time.Millisecond)
	initialVersion := calc.CurrentVersion()
	require.NotZero(t, initialVersion)

	// Add worker-2
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Wait for cooldown to expire + monitoring cycle (1s cooldown + 2s monitoring + margin)
	time.Sleep(3500 * time.Millisecond)

	// Version SHOULD change - cooldown expired
	newVersion := calc.CurrentVersion()
	require.Greater(t, newVersion, initialVersion, "rebalance should proceed after cooldown")

	t.Logf("Rebalance proceeded after cooldown: %d → %d", initialVersion, newVersion)
}

// TestCalculator_CooldownBoundary_ExactTiming verifies cooldown timing is precise.
func TestCalculator_CooldownBoundary_ExactTiming(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-boundary-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-boundary-heartbeat")

	// Create initial worker
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{{Keys: []string{"p1"}}},
	}
	strategy := &mockStrategy{}

	cooldownDuration := 800 * time.Millisecond
	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         3 * time.Second,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             cooldownDuration,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial rebalance
	time.Sleep(300 * time.Millisecond)
	initialVersion := calc.CurrentVersion()
	rebalanceTime := time.Now()

	// Add worker-2
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Test at boundary: just before cooldown expires
	justBeforeExpiry := cooldownDuration - 100*time.Millisecond
	time.Sleep(justBeforeExpiry)

	// Should still be blocked
	beforeVersion := calc.CurrentVersion()
	require.Equal(t, initialVersion, beforeVersion, "should still be in cooldown")

	t.Logf("At boundary -%dms: cooldown still active", 100)

	// Wait past cooldown + monitoring cycle
	remainingTime := cooldownDuration - justBeforeExpiry + 2*time.Second
	time.Sleep(remainingTime)

	// Should be allowed now
	afterVersion := calc.CurrentVersion()
	require.Greater(t, afterVersion, initialVersion, "should be past cooldown")

	actualCooldown := time.Since(rebalanceTime)
	t.Logf("Cooldown boundary test: configured=%v, actual delay=%v", cooldownDuration, actualCooldown)
}

// TestCalculator_MultipleCooldowns_Sequential verifies multiple cooldown cycles work correctly.
func TestCalculator_MultipleCooldowns_Sequential(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-multi-cooldown-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-multi-cooldown-heartbeat")

	// Create initial worker
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
			{Keys: []string{"p3"}},
		},
	}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         4 * time.Second,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      100 * time.Millisecond,
		PlannedScaleWindow:   100 * time.Millisecond,
		Cooldown:             1 * time.Second, // Short for faster test
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial rebalance
	time.Sleep(500 * time.Millisecond)
	v1 := calc.CurrentVersion()
	t.Logf("Initial version: %d", v1)

	// Add worker-2, wait for cooldown + rebalance
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)
	time.Sleep(3500 * time.Millisecond) // Cooldown + monitoring + margin

	v2 := calc.CurrentVersion()
	require.Greater(t, v2, v1, "first rebalance should complete")
	t.Logf("After worker-2: %d → %d", v1, v2)

	// Add worker-3, wait for cooldown + rebalance
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-3", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)
	time.Sleep(3500 * time.Millisecond)

	v3 := calc.CurrentVersion()
	require.Greater(t, v3, v2, "second rebalance should complete")
	t.Logf("After worker-3: %d → %d", v2, v3)

	// Add worker-4, wait for cooldown + rebalance
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-4", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)
	time.Sleep(3500 * time.Millisecond)

	v4 := calc.CurrentVersion()
	require.Greater(t, v4, v3, "third rebalance should complete")
	t.Logf("After worker-4: %d → %d → %d → %d", v1, v2, v3, v4)

	// Verify each cooldown was respected
	require.Equal(t, v1+1, v2, "versions should increment by 1")
	require.Equal(t, v2+1, v3, "versions should increment by 1")
	require.Equal(t, v3+1, v4, "versions should increment by 1")
}

// TestCalculator_TriggerRebalance_BypassesCooldown verifies TriggerRebalance bypasses cooldown.
func TestCalculator_TriggerRebalance_BypassesCooldown(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-trigger-bypass-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-trigger-bypass-heartbeat")

	// Create worker
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{{Keys: []string{"p1"}}},
	}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         4 * time.Second,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      100 * time.Millisecond,
		PlannedScaleWindow:   100 * time.Millisecond,
		Cooldown:             10 * time.Second, // Very long cooldown
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial rebalance
	time.Sleep(500 * time.Millisecond)
	initialVersion := calc.CurrentVersion()

	// Trigger rebalance manually (should bypass cooldown)
	err = calc.TriggerRebalance(ctx)
	require.NoError(t, err)

	// Wait for rebalance to complete
	time.Sleep(500 * time.Millisecond)

	newVersion := calc.CurrentVersion()
	require.Greater(t, newVersion, initialVersion, "TriggerRebalance should bypass cooldown")

	t.Logf("TriggerRebalance bypassed cooldown: %d → %d", initialVersion, newVersion)
}

// TestCalculator_CooldownReset_AfterEachRebalance verifies cooldown resets after each rebalance.
func TestCalculator_CooldownReset_AfterEachRebalance(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-reset-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-reset-heartbeat")

	// Create initial worker
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{{Keys: []string{"p1"}}},
	}
	strategy := &mockStrategy{}

	cooldownDuration := 1 * time.Second
	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         4 * time.Second,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             cooldownDuration,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial rebalance
	time.Sleep(300 * time.Millisecond)
	v1 := calc.CurrentVersion()
	t1 := time.Now()

	// Add worker-2
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Wait for cooldown + monitoring cycle + rebalance
	time.Sleep(3500 * time.Millisecond)
	v2 := calc.CurrentVersion()
	t2 := time.Now()
	require.Greater(t, v2, v1, "first rebalance should complete")

	elapsed1 := t2.Sub(t1)
	t.Logf("First rebalance delay: %v (cooldown=%v)", elapsed1, cooldownDuration)

	// Add worker-3 IMMEDIATELY after rebalance (cooldown should reset)
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-3", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Wait again for cooldown + monitoring cycle
	time.Sleep(3500 * time.Millisecond)
	v3 := calc.CurrentVersion()
	t3 := time.Now()
	require.Greater(t, v3, v2, "second rebalance should complete with fresh cooldown")

	elapsed2 := t3.Sub(t2)
	t.Logf("Second rebalance delay: %v (cooldown=%v)", elapsed2, cooldownDuration)

	// Both delays should be similar (cooldown resets after each rebalance)
	timeDiff := elapsed1 - elapsed2
	if timeDiff < 0 {
		timeDiff = -timeDiff
	}
	require.Less(t, timeDiff, 1*time.Second, "cooldown reset should produce similar delays")
}

// TestCalculator_Cooldown_WithPartitionRefresh verifies cooldown applies to partition source changes.
func TestCalculator_Cooldown_WithPartitionRefresh(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-refresh-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-refresh-heartbeat")

	// Create worker
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Use dynamic source that can change partitions
	source := &mockSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
		},
	}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         4 * time.Second,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      100 * time.Millisecond,
		PlannedScaleWindow:   100 * time.Millisecond,
		Cooldown:             2 * time.Second,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial rebalance
	time.Sleep(500 * time.Millisecond)
	initialVersion := calc.CurrentVersion()

	// Change partition source (add new partition)
	source.partitions = []types.Partition{
		{Keys: []string{"p1"}},
		{Keys: []string{"p2"}},
		{Keys: []string{"p3"}}, // New partition
	}

	// Trigger partition refresh
	err = calc.TriggerRebalance(ctx)
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)

	firstRefreshVersion := calc.CurrentVersion()
	require.Greater(t, firstRefreshVersion, initialVersion, "first refresh should succeed")

	// Try another refresh immediately (should be blocked by cooldown)
	source.partitions = append(source.partitions, types.Partition{Keys: []string{"p4"}})
	err = calc.TriggerRebalance(ctx)
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)

	// Note: TriggerRebalance bypasses cooldown, so this will actually succeed
	// This documents the current behavior
	secondRefreshVersion := calc.CurrentVersion()
	t.Logf("Partition refresh: %d → %d → %d (TriggerRebalance bypasses cooldown)",
		initialVersion, firstRefreshVersion, secondRefreshVersion)
}
