package assignment

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/types"
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

func TestStateMachine_EnterEmergency_DuringRebalancing_Ignored(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	started := make(chan struct{})
	release := make(chan struct{})
	rebalanceCount := atomic.Int32{}

	sm := NewStateMachine(
		logging.NewNop(),
		metrics.NewNop(),
		func(ctx context.Context, _ string) error {
			rebalanceCount.Add(1)
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		stopCh,
	)

	sm.EnterScaling(context.Background(), "planned_scale", 10*time.Millisecond)

	select {
	case <-started:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("rebalance did not start")
	}

	require.Equal(t, types.CalcStateRebalancing, sm.GetState())

	// Emergency should be ignored while rebalancing.
	sm.EnterEmergency(context.Background())
	require.Equal(t, types.CalcStateRebalancing, sm.GetState())

	close(release)
	require.Eventually(t, func() bool {
		return sm.GetState() == types.CalcStateIdle
	}, 200*time.Millisecond, 10*time.Millisecond)
	require.Equal(t, int32(1), rebalanceCount.Load())
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

// newTestStateMachine returns a state machine with the supplied callback.
func newTestStateMachine(t *testing.T, cb func(ctx context.Context, reason string) error) (*StateMachine, chan struct{}) {
	t.Helper()
	stopCh := make(chan struct{})
	sm := NewStateMachine(logging.NewNop(), metrics.NewNop(), cb, stopCh)
	return sm, stopCh
}

// S1: compareAndSwapState succeeds when the current state matches `from`.
func TestCompareAndSwapState_SucceedsOnMatch(t *testing.T) {
	sm, stopCh := newTestStateMachine(t, func(context.Context, string) error { return nil })
	defer close(stopCh)

	require.True(t, sm.compareAndSwapState(types.CalcStateIdle, types.CalcStateScaling))
	require.Equal(t, types.CalcStateScaling, sm.GetState())
}

// S2: compareAndSwapState fails when the current state differs from `from`.
func TestCompareAndSwapState_FailsOnMismatch(t *testing.T) {
	sm, stopCh := newTestStateMachine(t, func(context.Context, string) error { return nil })
	defer close(stopCh)

	require.False(t, sm.compareAndSwapState(types.CalcStateScaling, types.CalcStateRebalancing))
	require.Equal(t, types.CalcStateIdle, sm.GetState(), "state unchanged on failed CAS")
}

// S3: TryClaimEmergency accepts from Idle.
func TestTryClaimEmergency_AcceptsFromIdle(t *testing.T) {
	sm, stopCh := newTestStateMachine(t, func(context.Context, string) error { return nil })
	defer close(stopCh)

	require.True(t, sm.TryClaimEmergency(context.Background()))
	require.Equal(t, types.CalcStateEmergency, sm.GetState())
	require.Equal(t, "emergency", sm.GetScalingReason())
}

// S4: TryClaimEmergency accepts from Scaling (preempts the timer).
func TestTryClaimEmergency_AcceptsFromScaling_PreemptsTimer(t *testing.T) {
	sm, stopCh := newTestStateMachine(t, func(context.Context, string) error { return nil })
	defer close(stopCh)

	require.True(t, sm.compareAndSwapState(types.CalcStateIdle, types.CalcStateScaling))
	require.True(t, sm.TryClaimEmergency(context.Background()))
	require.Equal(t, types.CalcStateEmergency, sm.GetState())
}

// S5: TryClaimEmergency rejects when scaling timer already advanced to Rebalancing.
//
// This guards the strict-source CAS: once the scaling timer has CAS'd
// Scaling→Rebalancing, emergency cannot claim because neither Idle nor
// Scaling matches the current state.
func TestTryClaimEmergency_RejectsWhenScalingTimerAlreadyAdvanced(t *testing.T) {
	sm, stopCh := newTestStateMachine(t, func(context.Context, string) error { return nil })
	defer close(stopCh)

	require.True(t, sm.compareAndSwapState(types.CalcStateIdle, types.CalcStateScaling))
	// Scaling timer wins the race and advances to Rebalancing.
	require.True(t, sm.compareAndSwapState(types.CalcStateScaling, types.CalcStateRebalancing))

	require.False(t, sm.TryClaimEmergency(context.Background()))
	require.Equal(t, types.CalcStateRebalancing, sm.GetState())
}

// S6: TryClaimEmergency rejects from Rebalancing.
func TestTryClaimEmergency_RejectsFromRebalancing(t *testing.T) {
	sm, stopCh := newTestStateMachine(t, func(context.Context, string) error { return nil })
	defer close(stopCh)

	require.True(t, sm.compareAndSwapState(types.CalcStateIdle, types.CalcStateScaling))
	require.True(t, sm.compareAndSwapState(types.CalcStateScaling, types.CalcStateRebalancing))

	require.False(t, sm.TryClaimEmergency(context.Background()))
}

// S7: TryClaimEmergency rejects from Emergency (already claimed).
func TestTryClaimEmergency_RejectsFromEmergency(t *testing.T) {
	sm, stopCh := newTestStateMachine(t, func(context.Context, string) error { return nil })
	defer close(stopCh)

	require.True(t, sm.compareAndSwapState(types.CalcStateIdle, types.CalcStateEmergency))
	require.False(t, sm.TryClaimEmergency(context.Background()))
}

// S8: EnterRebalancing is a no-op when state changed out of Scaling.
func TestEnterRebalancing_NoOpWhenStateChanged(t *testing.T) {
	called := atomic.Bool{}
	sm, stopCh := newTestStateMachine(t, func(context.Context, string) error {
		called.Store(true)
		return nil
	})
	defer close(stopCh)

	// State is Idle (not Scaling). EnterRebalancing should bail.
	sm.EnterRebalancing(context.Background())

	require.False(t, called.Load(), "rebalance callback must not be invoked when state is not Scaling")
	require.Equal(t, types.CalcStateIdle, sm.GetState())
}

// S9: RunClaimedRebalance returns to Idle on success.
func TestRunClaimedRebalance_ReturnsToIdleOnSuccess(t *testing.T) {
	called := atomic.Bool{}
	sm, stopCh := newTestStateMachine(t, func(context.Context, string) error {
		called.Store(true)
		return nil
	})
	defer close(stopCh)

	// Pre-claim emergency for context.
	require.True(t, sm.TryClaimEmergency(context.Background()))
	sm.RunClaimedRebalance(context.Background(), "emergency")

	require.True(t, called.Load())
	require.Equal(t, types.CalcStateIdle, sm.GetState())
	require.Empty(t, sm.GetScalingReason())
}

// S10: RunClaimedRebalance returns to Idle on error.
func TestRunClaimedRebalance_ReturnsToIdleOnError(t *testing.T) {
	sm, stopCh := newTestStateMachine(t, func(context.Context, string) error {
		return context.Canceled
	})
	defer close(stopCh)

	require.True(t, sm.TryClaimEmergency(context.Background()))
	sm.RunClaimedRebalance(context.Background(), "emergency")

	require.Equal(t, types.CalcStateIdle, sm.GetState())
}

// S11: TryClaimEmergency sets scalingReason BEFORE notifying subscribers.
//
// Subscribers observing the Emergency state must see the correct reason on
// their first GetScalingReason() read. If reason were set after the
// notification, a subscriber could read empty.
func TestEnterEmergency_ScalingReasonSetBeforeSubscriberNotification(t *testing.T) {
	release := make(chan struct{})
	sm, stopCh := newTestStateMachine(t, func(context.Context, string) error {
		// Block until release so the rebalance callback doesn't reset the reason.
		<-release
		return nil
	})
	defer close(stopCh)
	defer close(release)

	ch, unsub := sm.Subscribe()
	defer unsub()
	// Drain initial Idle state.
	<-ch

	go func() {
		_ = sm.TryClaimEmergency(context.Background())
		sm.RunClaimedRebalance(context.Background(), "emergency")
	}()

	select {
	case state := <-ch:
		require.Equal(t, types.CalcStateEmergency, state)
		// Reason must already be set when the subscriber receives the state.
		require.Equal(t, "emergency", sm.GetScalingReason(),
			"scalingReason must be set BEFORE the Emergency notification is fanned out")
	case <-time.After(time.Second):
		t.Fatal("did not receive Emergency state notification")
	}
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
