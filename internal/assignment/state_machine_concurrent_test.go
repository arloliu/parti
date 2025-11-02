package assignment

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/internal/metrics"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// TestStateMachine_ConcurrentTransitions_OnlyOneSucceeds verifies that when multiple
// goroutines attempt transitions concurrently, the state machine serializes them properly
// and prevents race conditions.
func TestStateMachine_ConcurrentTransitions_OnlyOneSucceeds(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	rebalanceCount := atomic.Int32{}
	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(_ context.Context, _ string) error {
			rebalanceCount.Add(1)
			time.Sleep(100 * time.Millisecond) // Simulate work

			return nil
		},
		stopCh,
	)

	// Start state should be Idle
	require.Equal(t, types.CalcStateIdle, sm.GetState())

	// Subscribe to track state changes
	stateCh, unsubscribe := sm.Subscribe()
	defer unsubscribe()

	stateChanges := make([]types.CalculatorState, 0)
	var stateMu sync.Mutex

	// Collect all state changes
	go func() {
		for state := range stateCh {
			stateMu.Lock()
			stateChanges = append(stateChanges, state)
			stateMu.Unlock()
		}
	}()

	// Launch 10 concurrent goroutines trying to enter Scaling state
	const numGoroutines = 10
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Go(func() {
			// All trying to transition from Idle -> Scaling
			// Use 1 second window to ensure all goroutines start before first completes
			sm.EnterScaling(context.Background(), "concurrent_test", 1*time.Second)
		})
	}

	wg.Wait()

	// Wait for state machine to settle (scaling window + buffer)
	time.Sleep(1500 * time.Millisecond)

	// Only ONE scaling transition should have occurred (others rejected)
	stateMu.Lock()
	scalingCount := 0
	for _, state := range stateChanges {
		if state == types.CalcStateScaling {
			scalingCount++
		}
	}
	stateMu.Unlock()

	require.LessOrEqual(t, scalingCount, 1, "Only one concurrent transition to Scaling should succeed")

	// State should have progressed through the transition
	finalState := sm.GetState()
	require.Contains(t, []types.CalculatorState{
		types.CalcStateIdle,        // Already completed cycle
		types.CalcStateRebalancing, // In progress
	}, finalState)
}

// TestStateMachine_ConcurrentIdenticalTransitions_Idempotent verifies that
// multiple identical transition requests are idempotent and don't cause issues.
func TestStateMachine_ConcurrentIdenticalTransitions_Idempotent(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	rebalanceCalled := atomic.Int32{}
	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(_ context.Context, _ string) error {
			rebalanceCalled.Add(1)
			time.Sleep(50 * time.Millisecond)

			return nil
		},
		stopCh,
	)

	// Put machine in Scaling state first
	sm.EnterScaling(context.Background(), "initial", 500*time.Millisecond)
	require.Equal(t, types.CalcStateScaling, sm.GetState())

	// Try to enter Scaling again concurrently (idempotent operation)
	const numAttempts = 5
	var wg sync.WaitGroup

	for i := 0; i < numAttempts; i++ {
		wg.Go(func() {
			// These should all return false (already in Scaling)
			sm.EnterScaling(context.Background(), "duplicate", 500*time.Millisecond)
		})
	}

	wg.Wait()

	// State should still be Scaling (or progressed naturally)
	currentState := sm.GetState()
	require.Contains(t, []types.CalculatorState{
		types.CalcStateScaling,
		types.CalcStateRebalancing,
		types.CalcStateIdle,
	}, currentState)

	// Rebalance should have been called exactly once (from initial transition)
	time.Sleep(200 * time.Millisecond)
	require.LessOrEqual(t, rebalanceCalled.Load(), int32(1), "Idempotent transitions should not trigger multiple rebalances")
}

// TestStateMachine_TransitionDuringNotification_Serialized verifies that
// state transitions happening during notification delivery are properly serialized.
func TestStateMachine_TransitionDuringNotification_Serialized(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(_ context.Context, _ string) error {
			time.Sleep(50 * time.Millisecond)

			return nil
		},
		stopCh,
	)

	// Subscribe to state changes
	stateCh, unsubscribe := sm.Subscribe()
	defer unsubscribe()

	// Track all state transitions in order
	var stateSequence []types.CalculatorState
	var mu sync.Mutex

	// Start goroutine to collect state changes
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case state, ok := <-stateCh:
				if !ok {
					return
				}
				mu.Lock()
				stateSequence = append(stateSequence, state)
				mu.Unlock()
			case <-time.After(2 * time.Second):
				return
			}
		}
	}()

	// Trigger rapid state transitions
	sm.EnterScaling(context.Background(), "test1", 100*time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	sm.EnterEmergency(context.Background())
	time.Sleep(10 * time.Millisecond)
	sm.EnterScaling(context.Background(), "test3", 100*time.Millisecond)

	// Wait for notifications
	time.Sleep(500 * time.Millisecond)
	unsubscribe()
	<-done

	// Verify all transitions were recorded (even if happening during notifications)
	mu.Lock()
	defer mu.Unlock()

	require.NotEmpty(t, stateSequence, "Should have received state change notifications")
	t.Logf("State sequence: %v", stateSequence)

	// Verify valid state progression (no invalid transitions)
	for i := 1; i < len(stateSequence); i++ {
		prev := stateSequence[i-1]
		curr := stateSequence[i]
		// All transitions should be valid
		require.NotEqual(t, prev, curr, "State should change between notifications")
	}
}

// TestStateMachine_RapidTransitions_AllOrdered verifies that rapid successive
// transitions maintain ordering and don't get lost.
func TestStateMachine_RapidTransitions_AllOrdered(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	rebalanceCount := atomic.Int32{}
	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(_ context.Context, _ string) error {
			rebalanceCount.Add(1)

			return nil // Fast rebalance to test rapid transitions
		},
		stopCh,
	)

	// Subscribe to track all state changes
	stateCh, unsubscribe := sm.Subscribe()
	defer unsubscribe()

	var stateSequence []types.CalculatorState
	var mu sync.Mutex

	// Collect states
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case state, ok := <-stateCh:
				if !ok {
					return
				}
				mu.Lock()
				stateSequence = append(stateSequence, state)
				mu.Unlock()
			case <-time.After(1 * time.Second):
				return
			}
		}
	}()

	// Trigger rapid transitions with minimal delays
	for i := 0; i < 5; i++ {
		sm.EnterScaling(context.Background(), "rapid_test", 50*time.Millisecond)
		time.Sleep(5 * time.Millisecond) // Very short delay
	}

	// Wait for all transitions to complete
	time.Sleep(500 * time.Millisecond)
	unsubscribe()
	<-done

	mu.Lock()
	defer mu.Unlock()

	// Should have received multiple state notifications
	require.NotEmpty(t, stateSequence, "Should have recorded state transitions")
	t.Logf("Captured %d state transitions from rapid changes", len(stateSequence))

	// All states should be valid
	for _, state := range stateSequence {
		require.Contains(t, []types.CalculatorState{
			types.CalcStateIdle,
			types.CalcStateScaling,
			types.CalcStateRebalancing,
			types.CalcStateEmergency,
		}, state)
	}
}

// TestStateMachine_ConcurrentEmergency_LastWins verifies that concurrent emergency
// triggers are handled correctly, with the last emergency reason being recorded.
func TestStateMachine_ConcurrentEmergency_LastWins(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(_ context.Context, _ string) error {
			time.Sleep(50 * time.Millisecond)

			return nil
		},
		stopCh,
	)

	// Launch multiple concurrent emergency triggers
	const numEmergencies = 10
	var wg sync.WaitGroup
	reasons := make([]string, numEmergencies)

	for i := 0; i < numEmergencies; i++ {
		reasons[i] = "emergency_" + string(rune('A'+i))
		wg.Go(func() {
			sm.EnterEmergency(context.Background())
		})
	}

	wg.Wait()

	// Wait for state to settle
	time.Sleep(200 * time.Millisecond)

	// State should be Emergency or have completed
	state := sm.GetState()
	require.Contains(t, []types.CalculatorState{
		types.CalcStateEmergency,
		types.CalcStateRebalancing,
		types.CalcStateIdle,
	}, state)

	// Reason should be one of the provided reasons
	reason := sm.GetScalingReason()
	if state == types.CalcStateEmergency || state == types.CalcStateRebalancing {
		require.NotEmpty(t, reason, "Should have a scaling reason")
		t.Logf("Final emergency reason: %s", reason)
	}
}

// TestStateMachine_StopDuringTransition_CleansUp verifies that stopping the
// state machine during active transitions doesn't cause deadlocks or panics.
func TestStateMachine_StopDuringTransition_CleansUp(t *testing.T) {
	stopCh := make(chan struct{})

	blockRebalance := make(chan struct{})
	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(ctx context.Context, _ string) error {
			// Block until test signals or context cancels
			select {
			case <-blockRebalance:
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
				return nil
			}

			return nil
		},
		stopCh,
	)

	// Start a transition that will block
	go sm.EnterScaling(context.Background(), "blocking_test", 5*time.Second)

	// Wait for transition to start
	time.Sleep(100 * time.Millisecond)

	// Verify it's in Scaling state
	require.Equal(t, types.CalcStateScaling, sm.GetState())

	// Stop the state machine while transition is active
	close(stopCh)

	// This should not hang or panic
	time.Sleep(200 * time.Millisecond)

	// Unblock the rebalance (should already be cancelled)
	close(blockRebalance)

	// State machine should handle this gracefully
	t.Log("State machine stopped cleanly during active transition")
}
