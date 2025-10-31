// Package assignment provides partition assignment calculation.
package assignment

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/types"
	"github.com/puzpuzpuz/xsync/v4"
)

// StateMachine manages calculator state transitions.
//
// Implements a validated state machine with these states:
//   - Idle: Ready for rebalancing
//   - Scaling: Waiting for stabilization window
//   - Rebalancing: Computing/publishing assignments
//   - Emergency: Immediate rebalancing (no window)
//
// Valid transitions are enforced to prevent invalid states.
type StateMachine struct {
	current atomic.Int32 // types.CalculatorState
	mu      sync.RWMutex

	scalingStart  time.Time
	scalingReason string

	logger  types.Logger
	metrics types.CalculatorMetrics

	// Fan-out to subscribers
	subscribers      *xsync.Map[uint64, *stateSubscriber]
	nextSubscriberID atomic.Uint64

	// Callback invoked when rebalancing needs to occur
	onRebalanceCb func(ctx context.Context, reason string) error

	// For tracking scaling timer goroutine
	wg sync.WaitGroup

	stopCh chan struct{}
}

// NewStateMachine creates a new state machine.
//
// Parameters:
//   - logger: Logger for state transitions
//   - metrics: Metrics collector for calculator operations
//   - onRebalance: Callback invoked when rebalancing should occur
//   - stopCh: Channel to signal shutdown (for canceling scaling timers)
//
// Returns:
//   - *StateMachine: A new state machine instance starting in Idle state
func NewStateMachine(
	logger types.Logger,
	metrics types.CalculatorMetrics,
	onRebalance func(ctx context.Context, reason string) error,
	stopCh chan struct{},
) *StateMachine {
	sm := &StateMachine{
		logger:        logger,
		metrics:       metrics,
		onRebalanceCb: onRebalance,
		subscribers:   xsync.NewMap[uint64, *stateSubscriber](),
		stopCh:        stopCh,
	}
	sm.current.Store(int32(types.CalcStateIdle))

	return sm
}

// GetState returns the current calculator state.
//
// This method is thread-safe and can be called concurrently.
//
// Returns:
//   - types.CalculatorState: Current state (Idle, Scaling, Rebalancing, or Emergency)
func (sm *StateMachine) GetState() types.CalculatorState {
	return types.CalculatorState(sm.current.Load())
}

// GetScalingReason returns the reason for the current scaling operation.
//
// Returns:
//   - string: Scaling reason ("cold_start", "planned_scale", "emergency", "restart", or "")
func (sm *StateMachine) GetScalingReason() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	return sm.scalingReason
}

// Subscribe returns a channel that receives state change notifications.
//
// The returned channel is buffered (size 4) to allow for rapid state transitions
// without blocking the state machine. The subscriber receives the current state
// immediately upon subscription.
//
// Returns:
//   - <-chan types.CalculatorState: Channel that receives state updates
//   - func(): Unsubscribe function to clean up resources
//
// Example:
//
//	ch, unsubscribe := sm.Subscribe()
//	defer unsubscribe()
//	for state := range ch {
//	    fmt.Printf("State changed to: %s\n", state)
//	}
func (sm *StateMachine) Subscribe() (<-chan types.CalculatorState, func()) {
	id := sm.nextSubscriberID.Add(1)

	// Buffer size of 4 allows Idle -> Scaling -> Rebalancing -> Idle transitions
	// to be queued without dropping states when subscriber is slow to process
	sub := &stateSubscriber{ch: make(chan types.CalculatorState, 4)}
	sm.subscribers.Store(id, sub)

	// Immediately send the current state
	sub.trySend(sm.GetState(), sm.metrics)

	unsubscribe := func() {
		sm.removeSubscriber(id)
	}

	return sub.ch, unsubscribe
}

// removeSubscriber removes a subscriber and closes its channel.
func (sm *StateMachine) removeSubscriber(id uint64) {
	if sub, ok := sm.subscribers.LoadAndDelete(id); ok {
		sub.close()
	}
}

// EnterScaling transitions to scaling state and starts a stabilization timer.
//
// This method enforces that the transition only occurs from Idle state.
// If the current state is not Idle, the transition is rejected and the
// original state is preserved.
//
// Parameters:
//   - ctx: Context for the scaling timer goroutine
//   - reason: Reason for scaling ("cold_start", "planned_scale", "restart")
//   - window: Stabilization window duration before rebalancing
func (sm *StateMachine) EnterScaling(ctx context.Context, reason string, window time.Duration) {
	// Check current state (don't swap yet)
	currentState := types.CalculatorState(sm.current.Load())
	if currentState != types.CalcStateIdle {
		sm.logger.Warn("attempted to enter scaling state from non-idle state",
			"current_state", currentState.String(),
			"reason", reason)

		return
	}

	sm.mu.Lock()
	sm.scalingStart = time.Now()
	sm.scalingReason = reason
	sm.mu.Unlock()

	sm.logger.Info("entering scaling state",
		"reason", reason,
		"window", window,
	)

	// Transition to Scaling state and notify subscribers
	sm.emitStateChange(types.CalcStateScaling)

	// Start timer for scaling window with tracked goroutine
	sm.wg.Go(func() {
		sm.logger.Info("scaling timer goroutine started", "window", window)

		timer := time.NewTimer(window)
		defer timer.Stop()

		select {
		case <-timer.C:
			sm.logger.Info("scaling timer fired, entering rebalancing state")
			sm.EnterRebalancing(ctx)
		case <-sm.stopCh:
			sm.logger.Info("scaling timer cancelled by stopCh")
			return
		case <-ctx.Done():
			sm.logger.Info("scaling timer cancelled by context", "error", ctx.Err())
			return
		}
	})
}

// EnterRebalancing transitions to rebalancing state and triggers the rebalance callback.
//
// This method invokes the rebalance callback to perform the actual assignment calculation.
// On success, it automatically transitions back to Idle. On error, it also returns to Idle
// to allow retry on the next worker change detection.
//
// Parameters:
//   - ctx: Context for the rebalance operation
func (sm *StateMachine) EnterRebalancing(ctx context.Context) {
	sm.logger.Info("entering rebalancing state")

	// Notify subscribers of state change
	sm.emitStateChange(types.CalcStateRebalancing)

	// Get the scaling reason before rebalancing
	sm.mu.RLock()
	reason := sm.scalingReason
	sm.mu.RUnlock()

	// Perform rebalance via callback
	if sm.onRebalanceCb != nil {
		if err := sm.onRebalanceCb(ctx, reason); err != nil {
			sm.logger.Error("rebalancing failed", "error", err)
			// Return to idle even on error to allow retry
			sm.ReturnToIdle()

			return
		}
	}

	// Successfully rebalanced, return to idle
	sm.ReturnToIdle()
}

// EnterEmergency transitions to emergency state for immediate rebalancing.
//
// Emergency rebalancing has no stabilization window and happens immediately
// when a worker crash is detected.
//
// Parameters:
//   - ctx: Context for the rebalance operation
func (sm *StateMachine) EnterEmergency(ctx context.Context) {
	sm.mu.Lock()
	sm.scalingReason = "emergency"
	sm.mu.Unlock()

	sm.logger.Warn("entering emergency state - immediate rebalance")

	// Notify subscribers of state change
	sm.emitStateChange(types.CalcStateEmergency)

	// Perform immediate rebalance via callback
	if sm.onRebalanceCb != nil {
		if err := sm.onRebalanceCb(ctx, "emergency"); err != nil {
			sm.logger.Error("emergency rebalancing failed", "error", err)
			// Return to idle to allow retry
			sm.ReturnToIdle()

			return
		}
	}

	// Successfully rebalanced, return to idle
	sm.ReturnToIdle()
}

// ReturnToIdle transitions the state machine back to idle after rebalancing completes.
//
// This method clears the scaling reason and notifies all subscribers of the state change.
func (sm *StateMachine) ReturnToIdle() {
	sm.mu.Lock()
	sm.scalingReason = ""
	sm.mu.Unlock()

	sm.logger.Info("returned to idle state")

	// Notify subscribers of state change
	sm.emitStateChange(types.CalcStateIdle)
}

// WaitForShutdown waits for all scaling timer goroutines to complete.
//
// This should be called during shutdown after closing the stopCh to ensure
// all goroutines have exited cleanly.
func (sm *StateMachine) WaitForShutdown() {
	sm.wg.Wait()
}

// emitStateChange notifies all subscribers of a state transition.
func (sm *StateMachine) emitStateChange(state types.CalculatorState) {
	oldState := sm.GetState()
	if oldState == state {
		return // No change, no notification needed
	}

	sm.current.Store(int32(state)) //nolint:gosec // G115: state is bounded enum, safe conversion
	sm.logger.Info("state transition", "from", oldState, "to", state)

	sm.subscribers.Range(func(_ uint64, sub *stateSubscriber) bool {
		sub.trySend(state, sm.metrics)
		return true
	})
}
