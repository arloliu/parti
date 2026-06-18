package assignment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pmetrics "github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

func TestCalculator_Start(t *testing.T) {
	t.Run("starts successfully with initial assignment", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-start-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-start-heartbeat")

		// Create a heartbeat for worker-1
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
			HeartbeatTTL:         6 * time.Second,
			EmergencyGracePeriod: 3 * time.Second,
			ColdStartWindow:      50 * time.Millisecond,
			PlannedScaleWindow:   50 * time.Millisecond,
		})
		require.NoError(t, err)

		err = calc.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = calc.Stop(ctx) }()

		require.True(t, calc.IsStarted())

		// Wait for initial assignment to complete (happens in background goroutine)
		// ColdStartWindow is 50ms, so wait up to 500ms for completion
		require.Eventually(t, func() bool {
			return calc.CurrentVersion() > 0
		}, 500*time.Millisecond, 10*time.Millisecond, "initial assignment should complete")

		// Verify assignment was published
		entry, err := assignmentKV.Get(ctx, "assignment.worker-1")
		require.NoError(t, err)

		var assignment types.Assignment
		err = json.Unmarshal(entry.Value(), &assignment)
		require.NoError(t, err)
		require.Equal(t, calc.CurrentVersion(), assignment.Version)
		require.Len(t, assignment.Partitions, 2)
	})

	t.Run("returns error if already started", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-started-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-started-heartbeat")

		// Create a heartbeat
		_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
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
			ColdStartWindow:      50 * time.Millisecond,
			PlannedScaleWindow:   50 * time.Millisecond,
		})
		require.NoError(t, err)

		err = calc.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = calc.Stop(ctx) }()

		err = calc.Start(ctx)
		require.Error(t, err)
		require.ErrorIs(t, err, types.ErrCalculatorAlreadyStarted)
	})
}

func TestCalculator_Stop(t *testing.T) {
	t.Run("stops successfully", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-stop-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-stop-heartbeat")

		// Create a heartbeat
		_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
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
			ColdStartWindow:      50 * time.Millisecond,
			PlannedScaleWindow:   50 * time.Millisecond,
		})
		require.NoError(t, err)

		err = calc.Start(ctx)
		require.NoError(t, err)

		err = calc.Stop(ctx)
		require.NoError(t, err)
		require.False(t, calc.IsStarted())
	})

	t.Run("returns error if not started", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-not-started-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-not-started-heartbeat")

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
		})
		require.NoError(t, err)

		err = calc.Stop(ctx)
		require.Error(t, err)
		require.ErrorIs(t, err, types.ErrCalculatorNotStarted)
	})
}

func TestCalculator_WorkerMonitoring(t *testing.T) {
	t.Run("detects new worker and triggers rebalance", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-monitoring-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-monitoring-heartbeat")

		// Create initial heartbeat for worker-1
		_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)

		source := &mockSource{
			partitions: []types.Partition{
				{Keys: []string{"p1"}},
				{Keys: []string{"p2"}},
			},
		}
		strategy := &mockStrategy{}

		// Reduce HeartbeatTTL from 6s to 2s for faster test (poll interval = TTL/2 = 1s)
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
			Cooldown:             100 * time.Millisecond,
		})
		require.NoError(t, err)

		err = calc.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = calc.Stop(ctx) }()

		initialVersion := calc.CurrentVersion()

		// Add worker-2 heartbeat
		time.Sleep(150 * time.Millisecond) // Wait for cooldown
		_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)

		// Version should increase due to rebalance and both workers should receive assignments.
		require.Eventually(t, func() bool {
			if calc.CurrentVersion() <= initialVersion {
				return false
			}

			entry1, err1 := assignmentKV.Get(ctx, "assignment.worker-1")
			entry2, err2 := assignmentKV.Get(ctx, "assignment.worker-2")

			return err1 == nil && entry1 != nil && err2 == nil && entry2 != nil
		}, 1500*time.Millisecond, 25*time.Millisecond, "rebalance should publish assignments for both workers")
	})
}

func TestCalculator_CooldownPreventsRebalancing(t *testing.T) {
	t.Run("respects cooldown period", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-cooldown-prevent-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-cooldown-prevent-heartbeat")

		// Create initial heartbeat
		_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)

		source := &mockSource{
			partitions: []types.Partition{{Keys: []string{"p1"}}},
		}
		strategy := &mockStrategy{}

		// Reduce HeartbeatTTL from 6s to 2s for faster test
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
			Cooldown:             2 * time.Second,
		})
		require.NoError(t, err)

		err = calc.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = calc.Stop(ctx) }()

		// Wait for initial assignment to complete (happens in background goroutine)
		// Two-phase assignment: immediate (v1) + final (v2)
		// ColdStartWindow is 50ms, so wait up to 500ms for both phases
		require.Eventually(t, func() bool {
			return calc.CurrentVersion() >= 2 // Wait for final assignment (second phase)
		}, 500*time.Millisecond, 10*time.Millisecond, "initial assignment should complete")

		initialVersion := calc.CurrentVersion()
		t.Logf("Initial two-phase assignment completed at version %d", initialVersion)

		// Add worker-2 immediately (should be blocked by cooldown)
		_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)

		// Version should NOT change during the first monitoring cycle due to cooldown.
		require.Never(t, func() bool {
			return calc.CurrentVersion() > initialVersion
		}, 1200*time.Millisecond, 25*time.Millisecond, "version should not change while cooldown is active")
	})
}

func TestCalculator_GetActiveWorkers(t *testing.T) {
	t.Run("retrieves active workers from heartbeats", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-workers-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-workers-heartbeat")

		// Create heartbeats for multiple workers
		_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)
		_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)
		_, err = heartbeatKV.Put(ctx, "worker-hb.worker-3", []byte(time.Now().Format(time.RFC3339Nano)))
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
		})
		require.NoError(t, err)

		workers, _, err := calc.getActiveWorkers(ctx)
		require.NoError(t, err)
		require.Len(t, workers, 3)
		require.Contains(t, workers, "worker-1")
		require.Contains(t, workers, "worker-2")
		require.Contains(t, workers, "worker-3")
	})

	t.Run("returns empty list when no workers", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-no-workers-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-no-workers-heartbeat")

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
		})
		require.NoError(t, err)

		workers, _, err := calc.getActiveWorkers(ctx)
		require.NoError(t, err)
		require.Empty(t, workers)
	})
}

func TestCalculatorStateChanges(t *testing.T) {
	t.Run("receives initial and subsequent state changes", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-state-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-state-heartbeat")

		// Create a heartbeat for worker-1
		_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
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
			Logger:               partitest.NewTestLogger(t),
		})
		require.NoError(t, err) // Subscribe and defer unsubscribe.
		ch, unsubscribe := calc.SubscribeToStateChanges()
		defer unsubscribe()

		// 1. Wait for initial state.
		var initialState types.CalculatorState
		select {
		case initialState = <-ch:
			// received
		case <-ctx.Done():
			t.Fatal("timed out waiting for initial state")
		}
		require.Equal(t, types.CalcStateIdle, initialState)

		// 2. Trigger a state change by entering scaling state and verify it's received.
		calc.enterScalingState(ctx, "test", 500*time.Millisecond)

		var scalingState types.CalculatorState
		select {
		case scalingState = <-ch:
			// received
		case <-ctx.Done():
			t.Fatal("timed out waiting for scaling state")
		}
		require.Equal(t, types.CalcStateScaling, scalingState)

		// 3. Wait for automatic transition to rebalancing (after timer fires).
		var rebalancingState types.CalculatorState
		select {
		case rebalancingState = <-ch:
			// received
		case <-ctx.Done():
			t.Fatal("timed out waiting for rebalancing state")
		}
		require.Equal(t, types.CalcStateRebalancing, rebalancingState)

		// 4. Wait for automatic return to idle (after rebalance completes).
		var idleState types.CalculatorState
		select {
		case idleState = <-ch:
			// received
		case <-ctx.Done():
			t.Fatal("timed out waiting for return to idle state")
		}
		require.Equal(t, types.CalcStateIdle, idleState)

		// 5. Unsubscribe and ensure channel is closed.
		unsubscribe()

		// After unsubscribe, the channel should be closed.
		var finalState types.CalculatorState
		var ok bool
		select {
		case finalState, ok = <-ch:
			// received
		case <-time.After(100 * time.Millisecond):
			t.Fatal("channel was not closed after unsubscribe")
		}
		require.False(t, ok, "channel should be closed")
		require.Equal(t, types.CalculatorState(0), finalState, "zero value should be received from closed channel")
	})
}

func TestCalculator_Stop_PreserveAssignments(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-stop-cleanup-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-stop-cleanup-heartbeat")

	ctx := t.Context()

	// Create 3 partitions
	partitions := []types.Partition{
		{Keys: []string{"p1"}},
		{Keys: []string{"p2"}},
		{Keys: []string{"p3"}},
	}
	source := &mockSource{partitions: partitions}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker",
		HeartbeatTTL:         5 * time.Second,
		EmergencyGracePeriod: 2 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
	})
	require.NoError(t, err)

	// Publish heartbeats for 3 workers
	for _, workerID := range []string{"w1", "w2", "w3"} {
		_, err := heartbeatKV.Put(ctx, fmt.Sprintf("worker.%s", workerID), []byte("heartbeat"))
		require.NoError(t, err)
	}

	// Start calculator - it will perform initial assignment
	err = calc.Start(ctx)
	require.NoError(t, err)

	// Verify assignments exist in KV.
	require.Eventually(t, func() bool {
		keys, err := assignmentKV.Keys(ctx)
		if err != nil {
			return false
		}

		assignmentKeysCount := 0
		for _, key := range keys {
			if key == "assignment.w1" || key == "assignment.w2" || key == "assignment.w3" {
				assignmentKeysCount++
			}
		}

		return assignmentKeysCount == 3
	}, 1*time.Second, 25*time.Millisecond, "expected 3 workers to have assignments")

	t.Log("Verified: Assignments exist before Stop()")

	// Stop calculator - assignments should remain for new leader
	err = calc.Stop(ctx)
	require.NoError(t, err)

	// Verify assignments are PRESERVED in KV (for version continuity across leader changes)
	for _, workerID := range []string{"w1", "w2", "w3"} {
		key := fmt.Sprintf("assignment.%s", workerID)
		entry, err := assignmentKV.Get(ctx, key)
		require.NoError(t, err, "expected assignment for %s to be preserved", workerID)
		require.NotNil(t, entry, "expected assignment for %s to exist", workerID)
	}

	t.Log("Verified: Calculator.Stop() preserves assignments for new leader (version continuity)")
}

type mockWatchableSource struct {
	mu         sync.Mutex
	partitions []types.Partition
	listeners  []chan struct{}
}

func (m *mockWatchableSource) Start(ctx context.Context) error { return nil }
func (m *mockWatchableSource) Stop(ctx context.Context) error  { return nil }

func (m *mockWatchableSource) List(ctx context.Context) ([]types.Partition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.partitions, nil
}

func (m *mockWatchableSource) Watch(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{}, 1)
	m.mu.Lock()
	m.listeners = append(m.listeners, ch)
	m.mu.Unlock()
	return ch
}

func (m *mockWatchableSource) Update(partitions []types.Partition) {
	m.mu.Lock()
	m.partitions = partitions
	listeners := m.listeners
	m.mu.Unlock()

	for _, ch := range listeners {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func TestCalculator_WatchableSource(t *testing.T) {
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-watch-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-watch-heartbeat")

	// Create a heartbeat for worker-1
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockWatchableSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}},
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
		HeartbeatTTL:         6 * time.Second,
		EmergencyGracePeriod: 3 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial assignment
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > 0
	}, 1*time.Second, 10*time.Millisecond)

	initialVersion := calc.CurrentVersion()

	// Update partitions
	source.Update([]types.Partition{
		{Keys: []string{"p1"}},
		{Keys: []string{"p2"}},
	})

	// Verify rebalance triggered and version incremented
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > initialVersion
	}, 1*time.Second, 10*time.Millisecond, "calculator should rebalance on partition update")
}

// --- merged from calculator_getactiveworkers_ordering_test.go ---

// TestGetActiveWorkers_EnumRecoverySignalsBeforeCredibilityGate locks the stage
// ORDER of the getActiveWorkers funnel: the enumeration-failure reset (which
// fires OnEnumerationSuccess) runs on a successful Keys scan BEFORE the F10-A
// suspicious-shrink credibility gate (calculator.go: resetEnumerationFailures at
// ~1233, the suspicious block at ~1246). So a scan that RECOVERS the enumeration
// stall but is ALSO sharply shrunk must still signal enumeration recovery, even
// though the credibility gate degrades it to the cached worker set.
//
// The observable signal is the OnEnumerationSuccess callback. A refactor that
// linearizes the funnel with the suspicious gate's early return ahead of the
// reset would swallow the recovery signal on a recovered-but-suspicious scan;
// this test catches that via the success counter.
func TestGetActiveWorkers_EnumRecoverySignalsBeforeCredibilityGate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const seedWorkers = 5
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "ordering-assign-"+t.Name())
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "ordering-hb-"+t.Name())
	for i := range seedWorkers {
		key := fmt.Sprintf("worker-hb.worker-%03d", i)
		_, err := heartbeatKV.Put(ctx, key, []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)
	}

	fault := &keysTimeoutKV{KeyValue: heartbeatKV}
	var enumErr, enumOK atomic.Int64
	calc, err := NewCalculator(&Config{
		AssignmentKV:                         assignmentKV,
		HeartbeatKV:                          fault,
		AssignmentPrefix:                     "assignment",
		Source:                               &mutableSource{partitions: makePartitions(seedWorkers)},
		Strategy:                             &mockStrategy{},
		HeartbeatPrefix:                      "worker-hb",
		HeartbeatTTL:                         5 * time.Second,
		EmergencyGracePeriod:                 1 * time.Second,
		ColdStartWindow:                      10 * time.Millisecond,
		PlannedScaleWindow:                   10 * time.Millisecond,
		Cooldown:                             0,
		EnumerationFailureThreshold:          2,
		WorkerShrinkConfirmationCount:        2,
		WorkerShrinkConfirmationThresholdPct: 50,
		OnEnumerationError:                   func(error) { enumErr.Add(1) },
		OnEnumerationSuccess:                 func() { enumOK.Add(1) },
	})
	require.NoError(t, err)

	// Seed rebalance (fault disarmed) establishes lastKnownWorkerCount=seedWorkers
	// and primes the worker cache, so a later shrink reads as suspicious.
	require.NoError(t, calc.rebalance(ctx, "test-seed"))
	require.Equal(t, seedWorkers, calc.lastKnownWorkerCount)

	// Sustained enumeration stall: EnumerationFailureThreshold consecutive Keys
	// timeouts cross the threshold and fire OnEnumerationError.
	fault.armed.Store(true)
	for range 2 {
		_, _, err = calc.getActiveWorkers(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
	require.GreaterOrEqual(t, enumErr.Load(), int64(1), "sustained stall must fire OnEnumerationError")

	// Recover enumeration AND shrink the set in the same scan: Keys succeeds (stall
	// recovered) but observes 1 of 5 workers (a >50% shrink), so the credibility
	// gate marks it suspicious-and-unconfirmed and degrades to the cached set.
	fault.armed.Store(false)
	for i := 1; i < seedWorkers; i++ {
		require.NoError(t, heartbeatKV.Delete(ctx, fmt.Sprintf("worker-hb.worker-%03d", i)))
	}

	okBefore := enumOK.Load()
	workers, fresh, err := calc.getActiveWorkers(ctx)
	require.NoError(t, err, "the enumeration scan itself succeeded")
	require.False(t, fresh, "a suspicious shrink must degrade to the cached set (fresh=false)")
	require.Len(t, workers, seedWorkers, "the cached pre-shrink worker set is returned, not the shrunk view")

	require.Equal(t, okBefore+1, enumOK.Load(),
		"OnEnumerationSuccess must fire on the recovered-but-suspicious scan: the enumeration "+
			"reset runs BEFORE the credibility gate's early return")
}

// --- merged from calculator_metrics_test.go ---

// WorkerChange captures added/removed counts for verification.
type WorkerChange struct {
	added   int
	removed int
}

// MockMetricsCollector captures metric calls for verification.
type MockMetricsCollector struct {
	*pmetrics.NopMetrics // Pick up future MetricsCollector methods automatically.
	mu                   sync.Mutex

	// Captured data
	workerChanges []WorkerChange
	activeWorkers []int
}

// Implement only the methods we need to verify
func (m *MockMetricsCollector) RecordWorkerChange(added, removed int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workerChanges = append(m.workerChanges, WorkerChange{added: added, removed: removed})
}

func (m *MockMetricsCollector) RecordActiveWorkers(count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeWorkers = append(m.activeWorkers, count)
}

// Stub out other methods to avoid panics
func (m *MockMetricsCollector) RecordEmergencyRebalance(disappearedWorkers int)              {}
func (m *MockMetricsCollector) RecordRebalanceAttempt(reason string, success bool)           {}
func (m *MockMetricsCollector) RecordRebalanceDuration(duration float64, reason string)      {}
func (m *MockMetricsCollector) RecordPartitionCount(count int)                               {}
func (m *MockMetricsCollector) RecordOrphanedPartitions(count int)                           {}
func (m *MockMetricsCollector) RecordAssignmentChange(added, removed int, version int64)     {}
func (m *MockMetricsCollector) RecordKVOperationDuration(operation string, duration float64) {}
func (m *MockMetricsCollector) RecordStateChangeDropped()                                    {}
func (m *MockMetricsCollector) RecordCacheUsage(cacheType string, age float64)               {}
func (m *MockMetricsCollector) IncrementCacheFallback(reason string)                         {}

// TestCalculator_EmergencyMetrics verifies that worker topology metrics
// are recorded even during emergency rebalancing.
func TestCalculator_EmergencyMetrics(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-emergency-metrics-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-emergency-metrics-heartbeat")

	// Setup: 2 workers initially
	workers := []string{"worker-1", "worker-2"}
	for _, w := range workers {
		_, err := heartbeatKV.Put(ctx, "worker-hb."+w, []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)
	}

	source := &mockSource{
		partitions: []types.Partition{{Keys: []string{"p1"}}},
	}
	strategy := &mockStrategy{}
	metrics := &MockMetricsCollector{NopMetrics: pmetrics.NewNop()}

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
		Cooldown:             1 * time.Second,
		Metrics:              metrics, // Inject mock metrics
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial assignment
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > 0
	}, 2*time.Second, 50*time.Millisecond)

	// Clear initial metrics
	metrics.mu.Lock()
	metrics.workerChanges = nil
	metrics.activeWorkers = nil
	metrics.mu.Unlock()

	// Trigger Emergency: Delete worker-2
	err = heartbeatKV.Delete(ctx, "worker-hb.worker-2")
	require.NoError(t, err)

	// Wait for emergency rebalance and metrics recording
	require.Eventually(t, func() bool {
		metrics.mu.Lock()
		defer metrics.mu.Unlock()
		return len(metrics.workerChanges) > 0
	}, 3*time.Second, 50*time.Millisecond)

	// Verify metrics were recorded
	metrics.mu.Lock()
	defer metrics.mu.Unlock()

	require.NotEmpty(t, metrics.workerChanges, "should record worker changes")
	require.NotEmpty(t, metrics.activeWorkers, "should record active workers")

	foundChange := false
	for _, change := range metrics.workerChanges {
		if change.added == 0 && change.removed == 1 {
			foundChange = true
			break
		}
	}
	require.True(t, foundChange, "should record removal of 1 worker")

	foundActive := slices.Contains(metrics.activeWorkers, 1)
	require.True(t, foundActive, "should record 1 active worker")
}

// --- merged from calculator_cache_test.go ---

// TestCalculator_CacheFallback_ConnectivityError tests that Calculator falls back to cache
// when connectivity errors occur.
func TestCalculator_CacheFallback_ConnectivityError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx := t.Context()
	_, ncConn := partitest.StartEmbeddedNATS(t)
	heartbeatKV := partitest.CreateJetStreamKV(t, ncConn, "test-cache-fallback-heartbeat")
	assignmentKV := partitest.CreateJetStreamKV(t, ncConn, "test-cache-fallback-assignment")

	// Create initial worker heartbeats
	initialWorkers := []string{"worker-1", "worker-2", "worker-3"}
	for _, workerID := range initialWorkers {
		_, err := heartbeatKV.Put(ctx, "worker-hb."+workerID, []byte("active"))
		require.NoError(t, err)
	}

	// Create calculator
	cfg := &Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}},
		Strategy:             &mockStrategy{},
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         6 * time.Second,
		EmergencyGracePeriod: 100 * time.Millisecond,
		Cooldown:             100 * time.Millisecond,
		ColdStartWindow:      200 * time.Millisecond,
		PlannedScaleWindow:   100 * time.Millisecond,
	}

	calc, err := NewCalculator(cfg)
	require.NoError(t, err)
	require.NotNil(t, calc)

	// Start calculator
	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() {
		_ = calc.Stop(ctx)
	}()

	// Verify cache is populated.
	require.Eventually(t, func() bool {
		cached, age, ok := calc.getCachedWorkers()
		return ok && len(cached) == 3 && age < 1*time.Second
	}, 1*time.Second, 25*time.Millisecond, "cache should be populated after successful fetch")

	// Close NATS connection to simulate connectivity error
	ncConn.Close()

	// Try to fetch workers - should fall back to cache
	var workers []string
	require.Eventually(t, func() bool {
		fetched, _, err := calc.getActiveWorkers(ctx)
		if err != nil {
			return false
		}
		if len(fetched) != 3 {
			return false
		}

		workers = fetched

		return true
	}, 1*time.Second, 25*time.Millisecond, "should use cache when connectivity error occurs")
	require.Len(t, workers, 3, "should return cached workers")
	require.ElementsMatch(t, initialWorkers, workers)
}

// TestCalculator_CacheFallback_NoCacheAvailable tests that Calculator returns ErrDegraded
// when no cache is available during connectivity errors.
func TestCalculator_CacheFallback_NoCacheAvailable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx := t.Context()
	_, ncConn := partitest.StartEmbeddedNATS(t)
	heartbeatKV := partitest.CreateJetStreamKV(t, ncConn, "test-no-cache-heartbeat")
	assignmentKV := partitest.CreateJetStreamKV(t, ncConn, "test-no-cache-assignment")

	// Create calculator
	cfg := &Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}}},
		Strategy:             &mockStrategy{},
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         6 * time.Second,
		EmergencyGracePeriod: 100 * time.Millisecond,
		Cooldown:             100 * time.Millisecond,
		ColdStartWindow:      200 * time.Millisecond,
		PlannedScaleWindow:   100 * time.Millisecond,
	}

	calc, err := NewCalculator(cfg)
	require.NoError(t, err)
	require.NotNil(t, calc)

	// DON'T start calculator - no cache will be populated

	// Close NATS connection to simulate connectivity error
	ncConn.Close()

	// Try to fetch workers - should return ErrDegraded
	var workers []string
	var gotErr error
	require.Eventually(t, func() bool {
		workers, _, gotErr = calc.getActiveWorkers(ctx)
		return errors.Is(gotErr, types.ErrDegraded)
	}, 1*time.Second, 25*time.Millisecond, "should return ErrDegraded when no cache is available")
	require.Error(t, gotErr, "should return error when no cache available")
	require.True(t, errors.Is(gotErr, types.ErrDegraded), "should return ErrDegraded")
	require.Nil(t, workers)
}

// TestCalculator_CacheUpdate_OnSuccess tests that cache is updated on successful fetches.
func TestCalculator_CacheUpdate_OnSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx := t.Context()
	_, ncConn := partitest.StartEmbeddedNATS(t)
	heartbeatKV := partitest.CreateJetStreamKV(t, ncConn, "test-cache-update-heartbeat")
	assignmentKV := partitest.CreateJetStreamKV(t, ncConn, "test-cache-update-assignment")

	// Create initial worker heartbeats
	initialWorkers := []string{"worker-1", "worker-2"}
	for _, workerID := range initialWorkers {
		_, err := heartbeatKV.Put(ctx, "worker-hb."+workerID, []byte("active"))
		require.NoError(t, err)
	}

	// Create calculator
	cfg := &Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}}},
		Strategy:             &mockStrategy{},
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         6 * time.Second,
		EmergencyGracePeriod: 100 * time.Millisecond,
		Cooldown:             100 * time.Millisecond,
		ColdStartWindow:      200 * time.Millisecond,
		PlannedScaleWindow:   100 * time.Millisecond,
	}

	calc, err := NewCalculator(cfg)
	require.NoError(t, err)
	require.NotNil(t, calc)

	// Fetch workers - should populate cache
	workers, _, err := calc.getActiveWorkers(ctx)
	require.NoError(t, err)
	require.Len(t, workers, 2)

	// Verify initial cache
	cached1, age1, ok1 := calc.getCachedWorkers()
	require.True(t, ok1)
	require.Len(t, cached1, 2)
	require.Less(t, age1, 1*time.Second)

	// Add a new worker
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-3", []byte("active"))
	require.NoError(t, err)

	// Fetch workers again - should update cache
	require.Eventually(t, func() bool {
		workers, _, err = calc.getActiveWorkers(ctx)
		if err != nil || len(workers) != 3 {
			return false
		}

		cached2, age2, ok2 := calc.getCachedWorkers()
		return ok2 && len(cached2) == 3 && age2 < 1*time.Second
	}, 1*time.Second, 25*time.Millisecond, "cache should update after a successful fetch")

	// Verify cache was updated with new data
	cached2, age2, ok2 := calc.getCachedWorkers()
	require.True(t, ok2)
	require.Len(t, cached2, 3, "cache should contain 3 workers after update")
	require.Less(t, age2, 1*time.Second, "cache should be fresh")
	require.ElementsMatch(t, []string{"worker-1", "worker-2", "worker-3"}, cached2, "cache should contain all workers")
}
