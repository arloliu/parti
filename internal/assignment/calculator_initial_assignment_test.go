package assignment

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// TestCalculator_InitialAssignment_TwoPhase verifies the two-phase initial assignment.
// This is the critical test that ensures zero coverage gap during cold start.
func TestCalculator_InitialAssignment_TwoPhase(t *testing.T) {
	t.Run("immediate assignment covers partitions before stabilization window", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-twophase-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-twophase-heartbeat")

		// Create ONE worker heartbeat initially (simulating leader only)
		_, err := heartbeatKV.Put(ctx, "worker-hb.worker-0", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)

		partitions := []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
			{Keys: []string{"p3"}},
			{Keys: []string{"p4"}},
		}
		source := &mockSource{partitions: partitions}
		strategy := &mockStrategy{}

		calc, err := NewCalculator(&Config{
			AssignmentKV:         assignmentKV,
			HeartbeatKV:          heartbeatKV,
			AssignmentPrefix:     "assignment",
			Source:               source,
			Strategy:             strategy,
			HeartbeatPrefix:      "worker-hb",
			HeartbeatTTL:         6 * time.Second,
			EmergencyGracePeriod: 3 * time.Second,
			ColdStartWindow:      500 * time.Millisecond, // Longer window to test behavior
			PlannedScaleWindow:   50 * time.Millisecond,
		})
		require.NoError(t, err)

		// Start calculator (initialAssignment runs in background)
		err = calc.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = calc.Stop(ctx) }()

		// CRITICAL TEST: Verify assignment happens IMMEDIATELY (within 100ms)
		// This proves zero coverage gap - partitions assigned before stabilization window
		var immediateAssignment types.Assignment
		require.Eventually(t, func() bool {
			entry, err := assignmentKV.Get(ctx, "assignment.worker-0")
			if err != nil {
				return false
			}
			err = json.Unmarshal(entry.Value(), &immediateAssignment)

			return err == nil && len(immediateAssignment.Partitions) > 0
		}, 200*time.Millisecond, 10*time.Millisecond, "immediate assignment must happen within 200ms")

		// Verify worker-0 got ALL partitions (it's the only worker)
		require.Len(t, immediateAssignment.Partitions, 4, "worker-0 should get all partitions immediately")
		require.Equal(t, int64(1), immediateAssignment.Version, "first assignment should be version 1")

		t.Logf("IMMEDIATE ASSIGNMENT: worker-0 received %d partitions in <200ms (version %d)",
			len(immediateAssignment.Partitions), immediateAssignment.Version)

		// Now add 2 more workers DURING the stabilization window
		time.Sleep(100 * time.Millisecond) // Let immediate assignment settle
		_, err = heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)
		_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)

		t.Log("Added worker-1 and worker-2 during stabilization window")

		// Wait for stabilization window to complete + some margin
		time.Sleep(600 * time.Millisecond)

		// Verify FINAL assignment redistributed partitions to all 3 workers
		var finalAssignments [3]types.Assignment
		for i := 0; i < 3; i++ {
			entry, err := assignmentKV.Get(ctx, "assignment.worker-"+string(rune('0'+i)))
			require.NoError(t, err, "worker-%d should have assignment", i)

			err = json.Unmarshal(entry.Value(), &finalAssignments[i])
			require.NoError(t, err)
		}

		// Verify version increased (final assignment)
		require.Greater(t, finalAssignments[0].Version, immediateAssignment.Version,
			"final assignment should have higher version")

		// Verify total partition coverage
		totalPartitions := 0
		for i := 0; i < 3; i++ {
			totalPartitions += len(finalAssignments[i].Partitions)
			t.Logf("worker-%d: %d partitions (version %d)", i,
				len(finalAssignments[i].Partitions), finalAssignments[i].Version)
		}
		require.Equal(t, 4, totalPartitions, "all 4 partitions should be assigned")

		t.Logf("✅ FINAL ASSIGNMENT: Partitions redistributed across 3 workers (version %d)",
			finalAssignments[0].Version)
	})

	t.Run("handles context cancellation during stabilization wait", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-cancel-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-cancel-heartbeat")

		_, err := heartbeatKV.Put(ctx, "worker-hb.worker-0", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)

		source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}}}
		strategy := &mockStrategy{}

		calc, err := NewCalculator(&Config{
			AssignmentKV:         assignmentKV,
			HeartbeatKV:          heartbeatKV,
			AssignmentPrefix:     "assignment",
			Source:               source,
			Strategy:             strategy,
			HeartbeatPrefix:      "worker-hb",
			HeartbeatTTL:         6 * time.Second,
			EmergencyGracePeriod: 3 * time.Second,
			ColdStartWindow:      5 * time.Second, // Long window
			PlannedScaleWindow:   50 * time.Millisecond,
		})
		require.NoError(t, err)

		// Create cancellable context
		cancelCtx, cancel := context.WithCancel(ctx)

		err = calc.Start(cancelCtx)
		require.NoError(t, err)
		defer func() { _ = calc.Stop(ctx) }()

		// Wait for immediate assignment
		require.Eventually(t, func() bool {
			return calc.CurrentVersion() > 0
		}, 500*time.Millisecond, 10*time.Millisecond, "immediate assignment should complete")

		immediateVersion := calc.CurrentVersion()
		t.Logf("Immediate assignment completed at version %d", immediateVersion)

		// Cancel context during stabilization window
		cancel()

		// Give some time for cancellation to propagate
		time.Sleep(200 * time.Millisecond)

		// Final assignment should NOT happen due to cancellation
		finalVersion := calc.CurrentVersion()
		require.Equal(t, immediateVersion, finalVersion,
			"version should not change after context cancellation")

		t.Logf("✅ Context cancellation prevented final assignment (stayed at version %d)", finalVersion)
	})

	t.Run("single worker scenario - no rebalancing needed", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-single-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-single-heartbeat")

		// Single worker
		_, err := heartbeatKV.Put(ctx, "worker-hb.worker-0", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)

		source := &mockSource{partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
		}}
		strategy := &mockStrategy{}

		calc, err := NewCalculator(&Config{
			AssignmentKV:         assignmentKV,
			HeartbeatKV:          heartbeatKV,
			AssignmentPrefix:     "assignment",
			Source:               source,
			Strategy:             strategy,
			HeartbeatPrefix:      "worker-hb",
			HeartbeatTTL:         6 * time.Second,
			EmergencyGracePeriod: 3 * time.Second,
			ColdStartWindow:      300 * time.Millisecond,
			PlannedScaleWindow:   50 * time.Millisecond,
		})
		require.NoError(t, err)

		err = calc.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = calc.Stop(ctx) }()

		// Wait for both phases to complete
		time.Sleep(500 * time.Millisecond)

		// Should have 2 versions: immediate + final
		require.GreaterOrEqual(t, calc.CurrentVersion(), int64(2),
			"should have at least 2 versions (immediate + final)")

		// Verify worker-0 still has all partitions
		entry, err := assignmentKV.Get(ctx, "assignment.worker-0")
		require.NoError(t, err)

		var assignment types.Assignment
		err = json.Unmarshal(entry.Value(), &assignment)
		require.NoError(t, err)
		require.Len(t, assignment.Partitions, 2, "single worker should keep all partitions")

		t.Logf("✅ Single worker kept all partitions through both phases (version %d)", assignment.Version)
	})

	t.Run("multiple workers present at start", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-multi-start-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-multi-start-heartbeat")

		// Create 3 workers BEFORE calculator starts
		for i := 0; i < 3; i++ {
			_, err := heartbeatKV.Put(ctx, "worker-hb.worker-"+string(rune('0'+i)),
				[]byte(time.Now().Format(time.RFC3339Nano)))
			require.NoError(t, err)
		}

		source := &mockSource{partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
			{Keys: []string{"p3"}},
			{Keys: []string{"p4"}},
			{Keys: []string{"p5"}},
			{Keys: []string{"p6"}},
		}}
		strategy := &mockStrategy{}

		calc, err := NewCalculator(&Config{
			AssignmentKV:         assignmentKV,
			HeartbeatKV:          heartbeatKV,
			AssignmentPrefix:     "assignment",
			Source:               source,
			Strategy:             strategy,
			HeartbeatPrefix:      "worker-hb",
			HeartbeatTTL:         6 * time.Second,
			EmergencyGracePeriod: 3 * time.Second,
			ColdStartWindow:      300 * time.Millisecond,
			PlannedScaleWindow:   50 * time.Millisecond,
		})
		require.NoError(t, err)

		err = calc.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = calc.Stop(ctx) }()

		// Immediate assignment should distribute across all 3 workers
		require.Eventually(t, func() bool {
			return calc.CurrentVersion() > 0
		}, 200*time.Millisecond, 10*time.Millisecond, "immediate assignment should complete")

		// Verify all 3 workers got assignments immediately
		immediatePartitionCount := 0
		for i := 0; i < 3; i++ {
			entry, err := assignmentKV.Get(ctx, "assignment.worker-"+string(rune('0'+i)))
			if err == nil {
				var assignment types.Assignment
				if json.Unmarshal(entry.Value(), &assignment) == nil {
					immediatePartitionCount += len(assignment.Partitions)
					t.Logf("Immediate: worker-%d got %d partitions", i, len(assignment.Partitions))
				}
			}
		}
		require.Equal(t, 6, immediatePartitionCount, "all partitions should be assigned immediately")

		// Wait for final assignment
		time.Sleep(500 * time.Millisecond)

		// Verify final distribution
		finalPartitionCount := 0
		for i := 0; i < 3; i++ {
			entry, err := assignmentKV.Get(ctx, "assignment.worker-"+string(rune('0'+i)))
			require.NoError(t, err)

			var assignment types.Assignment
			err = json.Unmarshal(entry.Value(), &assignment)
			require.NoError(t, err)
			finalPartitionCount += len(assignment.Partitions)
			t.Logf("Final: worker-%d has %d partitions", i, len(assignment.Partitions))
		}
		require.Equal(t, 6, finalPartitionCount, "all partitions should remain assigned")

		t.Log("All workers received immediate assignments, maintained coverage through final phase")
	})
}

// TestCalculator_InitialAssignment_ZeroCoverageGap is the critical test proving
// there is NO time window where partitions are unassigned.
func TestCalculator_InitialAssignment_ZeroCoverageGap(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-zero-gap-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-zero-gap-heartbeat")

	// Single worker (leader)
	_, err := heartbeatKV.Put(ctx, "worker-hb.leader", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	partitions := []types.Partition{
		{Keys: []string{"critical-partition-1"}},
		{Keys: []string{"critical-partition-2"}},
	}
	source := &mockSource{partitions: partitions}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         6 * time.Second,
		EmergencyGracePeriod: 3 * time.Second,
		ColdStartWindow:      1 * time.Second, // Long window to make gap obvious if it exists
		PlannedScaleWindow:   50 * time.Millisecond,
	})
	require.NoError(t, err)

	// Record start time
	startTime := time.Now()

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Monitor assignment continuously to detect any gap
	gapDetected := false
	assignmentCheckDone := make(chan bool)

	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				entry, err := assignmentKV.Get(ctx, "assignment.leader")
				if err != nil {
					// No assignment yet - this is OK only in the first ~200ms
					elapsed := time.Since(startTime)
					if elapsed > 200*time.Millisecond {
						gapDetected = true
						t.Errorf("COVERAGE GAP DETECTED: No assignment after %v", elapsed)
						return
					}

					continue
				}

				var assignment types.Assignment
				if err := json.Unmarshal(entry.Value(), &assignment); err != nil {
					continue
				}

				if len(assignment.Partitions) == 0 {
					gapDetected = true
					t.Error("COVERAGE GAP DETECTED: Empty partition list in assignment")
					return
				}

				// Check if we've passed the stabilization window
				if time.Since(startTime) > 1200*time.Millisecond {
					return
				}

			case <-assignmentCheckDone:
				return
			}
		}
	}()

	// Wait for full cycle (immediate + stabilization + final)
	time.Sleep(1500 * time.Millisecond)
	close(assignmentCheckDone)

	require.False(t, gapDetected, "No coverage gap should be detected at any point")

	// Verify final state
	entry, err := assignmentKV.Get(ctx, "assignment.leader")
	require.NoError(t, err)

	var finalAssignment types.Assignment
	err = json.Unmarshal(entry.Value(), &finalAssignment)
	require.NoError(t, err)
	require.Len(t, finalAssignment.Partitions, 2, "leader should have both partitions")

	t.Logf("✅ ZERO COVERAGE GAP VERIFIED: Partitions were assigned continuously throughout %v window",
		time.Since(startTime))
}

// TestCalculator_InitialAssignment_Metrics verifies metrics are recorded correctly.
func TestCalculator_InitialAssignment_Metrics(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-metrics-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-metrics-heartbeat")

	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-0", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}}}
	strategy := &mockStrategy{}

	// Use mock metrics collector
	metrics := &mockMetricsCollector{
		rebalanceAttempts: make(map[string]int),
	}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         6 * time.Second,
		EmergencyGracePeriod: 3 * time.Second,
		ColdStartWindow:      200 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Metrics:              metrics,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for both phases
	time.Sleep(500 * time.Millisecond)

	// Verify two rebalance attempts recorded
	require.GreaterOrEqual(t, metrics.GetRebalanceAttempt("cold_start_immediate"), 1,
		"immediate assignment should be recorded")
	require.GreaterOrEqual(t, metrics.GetRebalanceAttempt("cold_start_final"), 1,
		"final assignment should be recorded")

	t.Logf("✅ Metrics correctly recorded: immediate=%d, final=%d",
		metrics.GetRebalanceAttempt("cold_start_immediate"),
		metrics.GetRebalanceAttempt("cold_start_final"))
}

// mockMetricsCollector for testing
type mockMetricsCollector struct {
	mu                sync.RWMutex
	rebalanceAttempts map[string]int
}

func (m *mockMetricsCollector) RecordRebalanceAttempt(lifecycle string, success bool) {
	if success {
		m.mu.Lock()
		m.rebalanceAttempts[lifecycle]++
		m.mu.Unlock()
	}
}

func (m *mockMetricsCollector) GetRebalanceAttempt(key string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rebalanceAttempts[key]
}

func (m *mockMetricsCollector) RecordRebalanceDuration(duration float64, lifecycle string)   {}
func (m *mockMetricsCollector) RecordWorkerChange(added, removed int)                        {}
func (m *mockMetricsCollector) RecordActiveWorkers(count int)                                {}
func (m *mockMetricsCollector) RecordPartitionCount(count int)                               {}
func (m *mockMetricsCollector) RecordEmergencyRebalance(disappearedWorkers int)              {}
func (m *mockMetricsCollector) RecordCacheUsage(cacheType string, age float64)               {}
func (m *mockMetricsCollector) IncrementCacheFallback(reason string)                         {}
func (m *mockMetricsCollector) RecordStateChangeDropped()                                    {}
func (m *mockMetricsCollector) RecordKVOperationDuration(operation string, duration float64) {}
func (m *mockMetricsCollector) RecordStateTransition(from, to types.State, duration float64) {}
func (m *mockMetricsCollector) RecordLeadershipChange(newLeader string)                      {}
func (m *mockMetricsCollector) RecordDegradedDuration(duration float64)                      {}
func (m *mockMetricsCollector) SetDegradedMode(degraded float64)                             {}
func (m *mockMetricsCollector) SetCacheAge(age float64)                                      {}
func (m *mockMetricsCollector) SetAlertLevel(level int)                                      {}
func (m *mockMetricsCollector) IncrementAlertEmitted(level string)                           {}
func (m *mockMetricsCollector) RecordHeartbeat(workerID string, success bool)                {}
func (m *mockMetricsCollector) RecordAssignmentChange(added, removed int, version int64)     {}
