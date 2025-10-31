package assignment

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// mockSource implements PartitionSource for testing
type mockSource struct {
	partitions []types.Partition
	err        error
}

func (m *mockSource) ListPartitions(ctx context.Context) ([]types.Partition, error) {
	if m.err != nil {
		return nil, m.err
	}

	return m.partitions, nil
}

// mockStrategy implements AssignmentStrategy for testing
type mockStrategy struct {
	assignments map[string][]types.Partition
	err         error
}

func (m *mockStrategy) Assign(workers []string, partitions []types.Partition) (map[string][]types.Partition, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.assignments != nil {
		return m.assignments, nil
	}

	// Simple round-robin assignment
	result := make(map[string][]types.Partition)
	for i, part := range partitions {
		workerIdx := i % len(workers)
		worker := workers[workerIdx]
		result[worker] = append(result[worker], part)
	}

	return result, nil
}

func TestCalculator_SetMethods(t *testing.T) {
	t.Run("sets cooldown successfully", func(t *testing.T) {
		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-cooldown-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-cooldown-heartbeat")

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
		calc.SetCooldown(5 * time.Second)

		require.Equal(t, 5*time.Second, calc.Cooldown)
	})

	t.Run("sets min threshold successfully", func(t *testing.T) {
		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-threshold-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-threshold-heartbeat")

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
		calc.SetMinThreshold(0.3)

		require.Equal(t, 0.3, calc.MinThreshold)
	})

	t.Run("sets restart detection ratio successfully", func(t *testing.T) {
		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-ratio-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-ratio-heartbeat")

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
		calc.SetRestartDetectionRatio(0.7)

		require.Equal(t, 0.7, calc.RestartRatio)
	})

	t.Run("sets stabilization windows successfully", func(t *testing.T) {
		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-windows-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-windows-heartbeat")

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
		calc.SetStabilizationWindows(20*time.Second, 5*time.Second)

		require.Equal(t, 20*time.Second, calc.ColdStartWindow)
		require.Equal(t, 5*time.Second, calc.PlannedScaleWindow)
	})
}

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
		})
		require.NoError(t, err)
		// Use very short stabilization windows for testing
		calc.SetStabilizationWindows(50*time.Millisecond, 50*time.Millisecond)

		err = calc.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = calc.Stop(ctx) }()

		require.True(t, calc.IsStarted())
		require.Greater(t, calc.CurrentVersion(), int64(0))

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
		})
		require.NoError(t, err)
		calc.SetStabilizationWindows(50*time.Millisecond, 50*time.Millisecond)

		err = calc.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = calc.Stop(ctx) }()

		err = calc.Start(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "already started")
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
		})
		require.NoError(t, err)
		calc.SetStabilizationWindows(50*time.Millisecond, 50*time.Millisecond)

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
		require.Contains(t, err.Error(), "not started")
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
		})
		require.NoError(t, err)
		calc.SetStabilizationWindows(50*time.Millisecond, 50*time.Millisecond)
		calc.SetCooldown(100 * time.Millisecond) // Short cooldown for testing

		err = calc.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = calc.Stop(ctx) }()

		initialVersion := calc.CurrentVersion()

		// Add worker-2 heartbeat
		time.Sleep(150 * time.Millisecond) // Wait for cooldown
		_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)

		// Wait for monitoring cycle to detect change (TTL/2 + processing = 1s + margin)
		time.Sleep(1200 * time.Millisecond)

		// Version should increase due to rebalance
		require.Greater(t, calc.CurrentVersion(), initialVersion)

		// Verify both workers got assignments
		entry1, err := assignmentKV.Get(ctx, "assignment.worker-1")
		require.NoError(t, err)
		require.NotNil(t, entry1)

		entry2, err := assignmentKV.Get(ctx, "assignment.worker-2")
		require.NoError(t, err)
		require.NotNil(t, entry2)
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
		})
		require.NoError(t, err)
		calc.SetStabilizationWindows(50*time.Millisecond, 50*time.Millisecond)
		calc.SetCooldown(2 * time.Second) // Reduced from 5s to 2s

		err = calc.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = calc.Stop(ctx) }()

		initialVersion := calc.CurrentVersion()

		// Add worker-2 immediately (should be blocked by cooldown)
		_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)

		// Wait for monitoring cycle (TTL/2 + margin = 1s + 200ms)
		time.Sleep(1200 * time.Millisecond)

		// Version should NOT change due to cooldown
		require.Equal(t, initialVersion, calc.CurrentVersion())
	})
}

func TestCalculator_StabilizationWindow(t *testing.T) {
	t.Run("selects cold start window for many workers", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-coldstart-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-coldstart-heartbeat")

		// Create heartbeats for many workers
		for i := 1; i <= 5; i++ {
			key := fmt.Sprintf("worker-hb.worker-%d", i)
			_, err := heartbeatKV.Put(ctx, key, []byte(time.Now().Format(time.RFC3339Nano)))
			require.NoError(t, err)
		}

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
		})
		require.NoError(t, err)
		calc.SetRestartDetectionRatio(0.5) // 5 workers / 2 expected = 2.5 ratio > 0.5

		window := calc.selectStabilizationWindow(ctx)
		require.Equal(t, calc.ColdStartWindow, window)
	})

	t.Run("selects planned scale window for few workers", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-scale-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-scale-heartbeat")

		// Create heartbeat for one worker
		_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)

		// Create many partitions so expected workers is high
		var partitions []types.Partition
		for i := 0; i < 50; i++ {
			partitions = append(partitions, types.Partition{Keys: []string{fmt.Sprintf("p%d", i)}})
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
		})
		require.NoError(t, err)
		calc.SetRestartDetectionRatio(0.5) // 1 worker / 5 expected = 0.2 ratio < 0.5

		window := calc.selectStabilizationWindow(ctx)
		require.Equal(t, calc.PlannedScaleWindow, window)
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

		workers, err := calc.getActiveWorkers(ctx)
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

		workers, err := calc.getActiveWorkers(ctx)
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
		})
		require.NoError(t, err)
		calc.SetLogger(partitest.NewTestLogger(t))

		// Subscribe and defer unsubscribe.
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

func TestCalculator_Stop_CleansUpAssignments(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-calc-stop-cleanup-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-calc-stop-cleanup-heartbeat")

	ctx := context.Background()

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

	// Wait a bit for initial assignment to complete
	time.Sleep(200 * time.Millisecond)

	// Verify assignments exist in KV
	keys, err := assignmentKV.Keys(ctx)
	require.NoError(t, err)
	assignmentKeysCount := 0
	for _, key := range keys {
		if key == "assignment.w1" || key == "assignment.w2" || key == "assignment.w3" {
			assignmentKeysCount++
		}
	}
	require.Equal(t, 3, assignmentKeysCount, "expected 3 workers to have assignments")

	t.Log("Verified: Assignments exist before Stop()")

	// Stop calculator - should clean up assignments
	err = calc.Stop(ctx)
	require.NoError(t, err)

	// Verify assignments are deleted from KV
	// Note: Keys() returns error when KV is empty, so we check each key individually
	for _, workerID := range []string{"w1", "w2", "w3"} {
		key := fmt.Sprintf("assignment.%s", workerID)
		_, err := assignmentKV.Get(ctx, key)
		require.Error(t, err, "expected assignment for %s to be deleted", workerID)
	}

	t.Log("Verified: Calculator.Stop() cleaned up all assignments from KV")
}
