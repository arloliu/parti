package assignment

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/v2/types"
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
	subscribers      sync.Map
	nextSubscriberID atomic.Uint64

	// Callback invoked when rebalancing needs to occur
	onRebalanceCb func(ctx context.Context, reason string) error

	// For tracking scaling timer goroutine
	wg sync.WaitGroup
	// Mutex to serialize Go() vs Wait() to avoid WaitGroup data race
	wgMu sync.Mutex

	stopCh chan struct{}

	// stopping indicates shutdown has begun to prevent scheduling new work
	stopping atomic.Bool
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
	if val, ok := sm.subscribers.LoadAndDelete(id); ok {
		if sub, ok := val.(*stateSubscriber); ok {
			sub.close()
		}
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
	if sm.stopping.Load() {
		sm.logger.Info("skipping scaling: shutdown flag set", "reason", reason)
		return
	}
	// If shutdown has been initiated, do not start scaling
	select {
	case <-sm.stopCh:
		sm.logger.Info("skipping scaling: shutdown in progress", "reason", reason)
		return
	default:
	}
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
	// Serialize with WaitForShutdown to prevent WaitGroup race
	sm.wgMu.Lock()
	defer sm.wgMu.Unlock()
	// Re-check shutdown just before scheduling the goroutine to avoid races with Wait()
	if sm.stopping.Load() {
		sm.logger.Info("skipping scaling timer: shutdown flag set")
		return
	}
	select {
	case <-sm.stopCh:
		sm.logger.Info("skipping scaling timer: shutdown in progress")
		return
	default:
	}
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
// This method should only be called from Scaling state (by the stabilization timer).
// If an emergency occurred during scaling, the state will have changed and this
// call becomes a no-op to prevent redundant rebalances.
//
// Parameters:
//   - ctx: Context for the rebalance operation
func (sm *StateMachine) EnterRebalancing(ctx context.Context) {
	// Guard: Only transition from Scaling state
	// If emergency occurred during scaling, we're no longer in Scaling
	// and should skip this (emergency already handled it)
	currentState := types.CalculatorState(sm.current.Load())
	if currentState != types.CalcStateScaling {
		sm.logger.Info("skipping rebalancing: not in scaling state",
			"current_state", currentState.String())
		return
	}

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
// This method can be called from Idle or Scaling states. If already in
// Rebalancing or Emergency state, the call is deferred to prevent cascading
// rebalances - the next poll cycle will detect the change.
//
// Parameters:
//   - ctx: Context for the rebalance operation
func (sm *StateMachine) EnterEmergency(ctx context.Context) {
	// Check if we can enter emergency state
	// Allow from Idle or Scaling (interrupts stabilization window)
	// Reject from Rebalancing or Emergency (already handling changes)
	currentState := types.CalculatorState(sm.current.Load())
	if currentState == types.CalcStateRebalancing || currentState == types.CalcStateEmergency {
		sm.logger.Warn("emergency detected but rebalance already in progress - deferring",
			"current_state", currentState.String())
		return
	}

	sm.mu.Lock()
	sm.scalingReason = "emergency"
	sm.mu.Unlock()

	sm.logger.Warn("entering emergency state - immediate rebalance",
		"from_state", currentState.String())

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
	sm.wgMu.Lock()
	defer sm.wgMu.Unlock()
	sm.wg.Wait()
}

// emitStateChange notifies all subscribers of a state transition.
func (sm *StateMachine) emitStateChange(state types.CalculatorState) {
	// Atomically set the new state and emit exactly one notification.
	// This prevents duplicate notifications when multiple goroutines
	// attempt the same transition concurrently.
	for {
		old := types.CalculatorState(sm.current.Load())
		if old == state {
			return // No change, nothing to do
		}

		if sm.current.CompareAndSwap(int32(old), int32(state)) { //nolint:gosec // G115: enum to int32 is safe
			sm.logger.Info("state transition", "from", old, "to", state)

			sm.subscribers.Range(func(_ any, value any) bool {
				if sub, ok := value.(*stateSubscriber); ok {
					sub.trySend(state, sm.metrics)
				}
				return true
			})

			return
		}
		// Another goroutine changed the state; retry with the new current value
	}
}
