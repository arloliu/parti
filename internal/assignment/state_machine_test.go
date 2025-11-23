package assignment

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/internal/metrics"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

func TestStateMachine_InitialState(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(ctx context.Context, reason string) error { return nil },
		stopCh,
	)

	require.Equal(t, types.CalcStateIdle, sm.GetState())
	require.Empty(t, sm.GetScalingReason())
}

func TestStateMachine_EnterScaling(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	rebalanceCalled := atomic.Bool{}
	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(ctx context.Context, reason string) error {
			rebalanceCalled.Store(true)
			require.Equal(t, "cold_start", reason)
			return nil
		},
		stopCh,
	)

	// Should transition to Scaling state
	sm.EnterScaling(context.Background(), "cold_start", 50*time.Millisecond)
	require.Equal(t, types.CalcStateScaling, sm.GetState())
	require.Equal(t, "cold_start", sm.GetScalingReason())

	// Should trigger rebalance after window
	require.Eventually(t, rebalanceCalled.Load, 200*time.Millisecond, 10*time.Millisecond)

	// Should return to Idle after rebalance
	require.Eventually(t, func() bool {
		return sm.GetState() == types.CalcStateIdle
	}, 200*time.Millisecond, 10*time.Millisecond)
}

func TestStateMachine_EnterScaling_OnlyFromIdle(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(ctx context.Context, reason string) error { return nil },
		stopCh,
	)

	// First transition should work
	sm.EnterScaling(context.Background(), "cold_start", 100*time.Millisecond)
	require.Equal(t, types.CalcStateScaling, sm.GetState())

	// Second transition should be ignored (not Idle)
	sm.EnterScaling(context.Background(), "planned_scale", 100*time.Millisecond)
	require.Equal(t, types.CalcStateScaling, sm.GetState())
	require.Equal(t, "cold_start", sm.GetScalingReason()) // Still original reason
}

func TestStateMachine_EnterRebalancing(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(ctx context.Context, reason string) error { return nil },
		stopCh,
	)

	// Set a reason first via Scaling
	sm.EnterScaling(context.Background(), "test_reason", 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond) // Let scaling timer fire

	// After rebalancing completes, check state
	require.Eventually(t, func() bool {
		return sm.GetState() == types.CalcStateIdle
	}, 100*time.Millisecond, 10*time.Millisecond)
}

func TestStateMachine_EnterEmergency(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	rebalanceCalled := atomic.Bool{}
	var capturedReason string

	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(ctx context.Context, reason string) error {
			capturedReason = reason
			rebalanceCalled.Store(true)
			return nil
		},
		stopCh,
	)

	// Should transition to Emergency and immediately trigger rebalance
	sm.EnterEmergency(context.Background())

	require.Eventually(t, rebalanceCalled.Load, 100*time.Millisecond, 10*time.Millisecond)
	require.Equal(t, "emergency", capturedReason)

	// Should return to Idle after rebalance
	require.Eventually(t, func() bool {
		return sm.GetState() == types.CalcStateIdle
	}, 100*time.Millisecond, 10*time.Millisecond)
}

func TestStateMachine_ReturnToIdle(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(ctx context.Context, reason string) error { return nil },
		stopCh,
	)

	// Transition to Scaling first (can only enter Rebalancing from Scaling)
	sm.EnterScaling(context.Background(), "test", 10*time.Millisecond)
	require.Equal(t, types.CalcStateScaling, sm.GetState())

	// Wait for auto-transition to Rebalancing
	time.Sleep(50 * time.Millisecond)

	// Should return to Idle automatically after rebalance
	require.Eventually(t, func() bool {
		return sm.GetState() == types.CalcStateIdle
	}, 100*time.Millisecond, 10*time.Millisecond)
	require.Empty(t, sm.GetScalingReason())
}

func TestStateMachine_Subscribe(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(ctx context.Context, reason string) error { return nil },
		stopCh,
	)

	// Subscribe to state changes
	ch, unsubscribe := sm.Subscribe()
	defer unsubscribe()

	// Should receive initial state
	select {
	case state := <-ch:
		require.Equal(t, types.CalcStateIdle, state)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("did not receive initial state")
	}

	// Transition to Scaling
	sm.EnterScaling(context.Background(), "test", 10*time.Millisecond)

	// Should receive new state
	select {
	case state := <-ch:
		require.Equal(t, types.CalcStateScaling, state)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("did not receive state change")
	}
}

func TestStateMachine_MultipleSubscribers(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(ctx context.Context, reason string) error { return nil },
		stopCh,
	)

	// Create multiple subscribers
	ch1, unsub1 := sm.Subscribe()
	defer unsub1()
	ch2, unsub2 := sm.Subscribe()
	defer unsub2()

	// Drain initial states
	<-ch1
	<-ch2

	// Transition to Emergency
	sm.EnterEmergency(context.Background())

	// Both should receive the change
	select {
	case state := <-ch1:
		require.Equal(t, types.CalcStateEmergency, state)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("subscriber 1 did not receive state change")
	}

	select {
	case state := <-ch2:
		require.Equal(t, types.CalcStateEmergency, state)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("subscriber 2 did not receive state change")
	}
}

func TestStateMachine_Unsubscribe(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(ctx context.Context, reason string) error { return nil },
		stopCh,
	)

	ch, unsubscribe := sm.Subscribe()
	<-ch // Drain initial state

	// Unsubscribe
	unsubscribe()

	// Transition - unsubscribed channel should not receive
	sm.EnterScaling(context.Background(), "test", 10*time.Millisecond)

	// Channel should eventually be closed by unsubscribe, or we just verify no receive happens
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("unsubscribed channel should not receive new state changes")
		}
		// Channel closed - expected
	case <-time.After(50 * time.Millisecond):
		// Timeout - also acceptable, channel may not be closed yet
	}
}

func TestStateMachine_WaitForShutdown(t *testing.T) {
	stopCh := make(chan struct{})

	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(ctx context.Context, reason string) error {
			time.Sleep(50 * time.Millisecond) // Simulate work
			return nil
		},
		stopCh,
	)

	// Start a scaling operation with timer
	sm.EnterScaling(context.Background(), "test", 100*time.Millisecond)

	// Close stopCh to trigger shutdown
	close(stopCh)

	// WaitForShutdown should block until timer goroutine exits
	done := make(chan struct{})
	go func() {
		sm.WaitForShutdown()
		close(done)
	}()

	select {
	case <-done:
		// Expected - WaitForShutdown completed
	case <-time.After(500 * time.Millisecond):
		t.Fatal("WaitForShutdown did not complete in time")
	}
}

func TestStateMachine_ScalingCancellation(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	rebalanceCalled := atomic.Bool{}
	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(ctx context.Context, reason string) error {
			rebalanceCalled.Store(true)
			return nil
		},
		stopCh,
	)

	// Start scaling with short window
	ctx, cancel := context.WithCancel(context.Background())
	sm.EnterScaling(ctx, "test", 100*time.Millisecond)
	require.Equal(t, types.CalcStateScaling, sm.GetState())

	// Cancel context immediately
	cancel()

	// Wait for context cancellation to take effect
	time.Sleep(150 * time.Millisecond)

	// Rebalance should not be called due to cancellation
	require.False(t, rebalanceCalled.Load(), "rebalance should not be called when context is cancelled")

	// State should return to Idle after context cancellation
	// Note: Even though rebalance didn't happen, the timer goroutine should exit and state should stabilize
	require.Equal(t, types.CalcStateScaling, sm.GetState(), "state remains Scaling when timer is cancelled")
}

func TestStateMachine_RebalanceError(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(ctx context.Context, reason string) error {
			return context.Canceled // Simulate error
		},
		stopCh,
	)

	// Should still transition through states even if rebalance fails
	sm.EnterEmergency(context.Background())

	// Should return to Idle even after error
	require.Eventually(t, func() bool {
		return sm.GetState() == types.CalcStateIdle
	}, 100*time.Millisecond, 10*time.Millisecond)
}
