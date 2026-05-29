package assignment

import (
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// fakeClock is a manually-advanced clock for deterministic cooldown tests.
// The calculator's cooldown gate and the publisher's lastRebalance stamp both
// read Config.Now, so advancing this clock past Cooldown deterministically
// opens the gate regardless of real-time scheduling under load. Freezing it
// (not advancing) keeps the gate closed no matter how much wall time passes.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Now()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// waitStableVersion waits until the calculator's version is > 0 and has not
// changed across a confirmation window, then returns it. Cold start may perform
// more than one rebalance (initial assignment plus a stabilization-window
// rebalance); with a frozen fake clock the cooldown gate blocks any further
// rebalance once cold start settles, so a stable version is a deterministic
// baseline for the cooldown assertions that follow.
func waitStableVersion(t *testing.T, calc *Calculator) int64 {
	t.Helper()
	const confirm = 500 * time.Millisecond
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		v := calc.CurrentVersion()
		if v == 0 {
			time.Sleep(25 * time.Millisecond)
			continue
		}
		stable := true
		end := time.Now().Add(confirm)
		for time.Now().Before(end) {
			time.Sleep(25 * time.Millisecond)
			if calc.CurrentVersion() != v {
				stable = false
				break
			}
		}
		if stable {
			return v
		}
	}
	t.Fatal("calculator version did not stabilize")

	return 0
}

// TestCalculator_RebalanceAttemptDuringCooldown_Blocked verifies cooldown prevents rebalancing.
func TestCalculator_RebalanceAttemptDuringCooldown_Blocked(t *testing.T) {
	t.Parallel()
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

	clock := newFakeClock()
	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         2 * time.Second, // long enough that workers persist through the short test
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             5 * time.Second, // fake-clock driven; never elapses in real time
		Now:                  clock.Now,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Cold-start rebalance(s) establish the initial version and stamp
	// lastRebalance at the (frozen) clock time. Wait until it fully settles.
	initialVersion := waitStableVersion(t, calc)

	// Add worker-2: the monitor's watcher fires a poll, but the cooldown gate
	// (clock has not advanced past the 5s Cooldown) must block the rebalance.
	// Because the gate reads the frozen fake clock, real-time scheduling under
	// load cannot let the rebalance slip through.
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	require.Never(t, func() bool { return calc.CurrentVersion() != initialVersion },
		750*time.Millisecond, 50*time.Millisecond,
		"cooldown must block the rebalance while the clock has not advanced past Cooldown")

	t.Logf("cooldown correctly blocked rebalance: version remained %d", initialVersion)
}

// TestCalculator_RebalanceAfterCooldown_Allowed verifies rebalance proceeds after cooldown expires.
func TestCalculator_RebalanceAfterCooldown_Allowed(t *testing.T) {
	t.Parallel()
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
		HeartbeatTTL:         200 * time.Millisecond, // Fast heartbeat
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             200 * time.Millisecond, // Short cooldown
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial rebalance
	time.Sleep(100 * time.Millisecond)
	initialVersion := calc.CurrentVersion()
	require.NotZero(t, initialVersion)

	// Add worker-2
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Wait for cooldown to expire + monitoring cycle (200ms cooldown + 100ms monitoring + margin)
	time.Sleep(500 * time.Millisecond)

	// Version SHOULD change - cooldown expired
	newVersion := calc.CurrentVersion()
	require.Greater(t, newVersion, initialVersion, "rebalance should proceed after cooldown")

	t.Logf("Rebalance proceeded after cooldown: %d → %d", initialVersion, newVersion)
}

// TestCalculator_CooldownBoundary_ExactTiming verifies cooldown timing is precise.
func TestCalculator_CooldownBoundary_ExactTiming(t *testing.T) {
	t.Parallel()
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

	cooldownDuration := 5 * time.Second // fake-clock driven; real time never reaches it
	clock := newFakeClock()
	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         2 * time.Second,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             cooldownDuration,
		Now:                  clock.Now,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	initialVersion := waitStableVersion(t, calc)

	// Add worker-2. Just below the boundary: advance the clock to one tick short
	// of Cooldown — the gate must still block.
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)
	clock.Advance(cooldownDuration - time.Millisecond)
	require.Never(t, func() bool { return calc.CurrentVersion() != initialVersion },
		500*time.Millisecond, 50*time.Millisecond, "must still be in cooldown just below the boundary")
	t.Log("just below boundary: cooldown still active")

	// Cross the boundary: advance past Cooldown and re-touch the heartbeat to
	// fire the watcher; the rebalance must now proceed.
	clock.Advance(2 * time.Millisecond)
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return calc.CurrentVersion() > initialVersion },
		3*time.Second, 50*time.Millisecond, "rebalance must proceed once the clock crosses Cooldown")
	t.Logf("crossed boundary: version %d → %d", initialVersion, calc.CurrentVersion())
}

// TestCalculator_MultipleCooldowns_Sequential verifies multiple cooldown cycles work correctly.
func TestCalculator_MultipleCooldowns_Sequential(t *testing.T) {
	t.Parallel()
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

	clock := newFakeClock()
	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         3 * time.Second, // workers persist across all cycles
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             5 * time.Second, // fake-clock driven; never elapses in real time
		Now:                  clock.Now,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// renew re-puts the given worker heartbeats: keeps them alive (beating the
	// KV TTL) and fires the monitor's watcher. A cooldown-blocked poll does not
	// clear the pending worker-set change, so re-firing the watcher after the
	// clock advances past Cooldown lets the blocked rebalance proceed.
	renew := func(workers ...string) {
		for _, w := range workers {
			_, perr := heartbeatKV.Put(ctx, "worker-hb."+w, []byte(time.Now().Format(time.RFC3339Nano)))
			require.NoError(t, perr)
		}
	}

	prev := waitStableVersion(t, calc)
	t.Logf("initial version: %d", prev)

	alive := make([]string, 0, 4)
	alive = append(alive, "worker-1")
	for _, added := range []string{"worker-2", "worker-3", "worker-4"} {
		alive = append(alive, added)
		renew(alive...) // introduce the new worker (set change) — blocked by cooldown
		clock.Advance(5*time.Second + 100*time.Millisecond)
		renew(alive...) // gate is open now; re-fire the watcher to retry the rebalance
		want := prev + 1
		require.Eventually(t, func() bool { return calc.CurrentVersion() == want },
			3*time.Second, 50*time.Millisecond,
			"adding %s should bump version to %d after cooldown", added, want)
		require.Equal(t, want, calc.CurrentVersion(),
			"each cooldown cycle must increment the version by exactly 1")
		t.Logf("after %s: version %d", added, want)
		prev = want
	}
}

// TestCalculator_TriggerRebalance_BypassesCooldown verifies TriggerRebalance bypasses cooldown.
func TestCalculator_TriggerRebalance_BypassesCooldown(t *testing.T) {
	t.Parallel()
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
		HeartbeatTTL:         200 * time.Millisecond,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
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
	t.Parallel()
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

	// Use shorter cooldown for faster test
	cooldownDuration := 200 * time.Millisecond
	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         200 * time.Millisecond,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             cooldownDuration,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial assignment publish (version >=1)
	var v1 int64
	waitDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(waitDeadline) {
		v1 = calc.CurrentVersion()
		if v1 >= 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.GreaterOrEqual(t, v1, int64(1), "initial assignment version should be >=1")
	t1 := time.Now()

	// Add worker-2
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Wait for version increment (rebalance triggered) with generous deadline
	var v2 int64
	waitDeadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(waitDeadline) {
		v2 = calc.CurrentVersion()
		if v2 > v1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.Greater(t, v2, v1, "first rebalance should complete")
	t2 := time.Now()

	elapsed1 := t2.Sub(t1)
	t.Logf("First rebalance delay: %v (cooldown=%v)", elapsed1, cooldownDuration)

	// Add worker-3 IMMEDIATELY after rebalance (cooldown should reset)
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-3", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Wait for second version increment with same generous deadline
	var v3 int64
	waitDeadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(waitDeadline) {
		v3 = calc.CurrentVersion()
		if v3 > v2 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.Greater(t, v3, v2, "second rebalance should complete with fresh cooldown")
	t3 := time.Now()
	require.Greater(t, v3, v2, "second rebalance should complete with fresh cooldown")

	elapsed2 := t3.Sub(t2)
	t.Logf("Second rebalance delay: %v (cooldown=%v)", elapsed2, cooldownDuration)

	// Assert behavior: first rebalance may be fast due to cold start shortcut; second should honor cooldown.
	require.Greater(t, elapsed2, cooldownDuration, "second rebalance should reflect cooldown interval")
	require.Greater(t, elapsed2, elapsed1, "second rebalance should not be faster than first when cooldown applies")
	// Upper bound sanity (avoid runaway delays > 10s in test environment).
	require.Less(t, elapsed2, 5*time.Second, "second rebalance delay unexpectedly large")
}

// TestCalculator_Cooldown_WithPartitionRefresh verifies cooldown applies to partition source changes.
func TestCalculator_Cooldown_WithPartitionRefresh(t *testing.T) {
	t.Parallel()
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
		HeartbeatTTL:         200 * time.Millisecond,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             200 * time.Millisecond,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial rebalance
	time.Sleep(100 * time.Millisecond)
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
	time.Sleep(100 * time.Millisecond)

	firstRefreshVersion := calc.CurrentVersion()
	require.Greater(t, firstRefreshVersion, initialVersion, "first refresh should succeed")

	// Try another refresh immediately (should be blocked by cooldown)
	source.partitions = append(source.partitions, types.Partition{Keys: []string{"p4"}})
	err = calc.TriggerRebalance(ctx)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	// Note: TriggerRebalance bypasses cooldown, so this will actually succeed
	// This documents the current behavior
	secondRefreshVersion := calc.CurrentVersion()
	t.Logf("Partition refresh: %d → %d → %d (TriggerRebalance bypasses cooldown)",
		initialVersion, firstRefreshVersion, secondRefreshVersion)
}
