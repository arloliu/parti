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

	// Callback invoked by RunClaimedRebalanceErr for partition-lifecycle
	// rebalances. Distinct from onRebalanceCb because the partition path needs
	// errShuttingDown to propagate to restorePendingOnGraceBail rather than
	// being swallowed (see calculator.go handlePartitionRebalance).
	onPartitionRebalanceCb func(ctx context.Context, reason string) error

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
// This method must be called from the scaling-timer goroutine after the
// stabilization window expires. It uses a strict-source CAS from Scaling to
// Rebalancing: if any other transition (e.g., a concurrent emergency claim)
// has moved the state out of Scaling, the call is a no-op.
//
// On success or error, the state is returned to Idle to allow the next change
// to be picked up.
//
// Parameters:
//   - ctx: Context for the rebalance operation
func (sm *StateMachine) EnterRebalancing(ctx context.Context) {
	// Strict-source CAS: only transition Scaling -> Rebalancing. If the
	// state moved (e.g., emergency claimed it), bail.
	if !sm.compareAndSwapState(types.CalcStateScaling, types.CalcStateRebalancing) {
		currentState := types.CalculatorState(sm.current.Load())
		sm.logger.Info("skipping rebalancing: not in scaling state",
			"current_state", currentState.String())
		return
	}

	sm.logger.Info("entering rebalancing state")

	// Notify subscribers of the just-committed state change.
	sm.notifyStateChange(types.CalcStateRebalancing)

	// Get the scaling reason before rebalancing.
	sm.mu.RLock()
	reason := sm.scalingReason
	sm.mu.RUnlock()

	sm.RunClaimedRebalance(ctx, reason)
}

// TryClaimEmergency attempts to atomically claim the emergency lifecycle.
//
// Strict-source CAS from either Idle or Scaling to Emergency. Returns true if
// the claim succeeded; the caller is then responsible for invoking
// RunClaimedRebalance to execute the rebalance and return to Idle.
//
// When the claim succeeds, scalingReason is set BEFORE the state-change
// notification is fanned out to subscribers, so subscribers observing the
// Emergency state see the correct reason on their first read.
//
// Parameters:
//   - ctx: Context (currently unused but kept for symmetry with other Enter* methods)
//
// Returns:
//   - bool: true if the emergency lifecycle was successfully claimed; false if
//     another lifecycle (Rebalancing or Emergency) is already in flight.
func (sm *StateMachine) TryClaimEmergency(_ context.Context) bool {
	// Try Idle first, then Scaling. Both are valid claim sources.
	// Reject from Rebalancing or Emergency (already handling changes).
	if sm.tryClaimEmergencyFrom(types.CalcStateIdle) {
		return true
	}
	if sm.tryClaimEmergencyFrom(types.CalcStateScaling) {
		return true
	}

	currentState := types.CalculatorState(sm.current.Load())
	sm.logger.Warn("emergency detected but rebalance already in progress - deferring",
		"current_state", currentState.String())

	return false
}

// RunClaimedRebalance runs the rebalance callback for a previously-claimed
// lifecycle and returns the state machine to Idle.
//
// Must be called after a successful TryClaimEmergency or after a successful
// strict-source CAS into Rebalancing (see EnterRebalancing).
//
// Parameters:
//   - ctx: Context for the rebalance operation
//   - reason: Lifecycle reason to pass to the callback ("emergency", "cold_start", etc.)
func (sm *StateMachine) RunClaimedRebalance(ctx context.Context, reason string) {
	if sm.onRebalanceCb != nil {
		if err := sm.onRebalanceCb(ctx, reason); err != nil {
			sm.logger.Error("rebalance failed", "reason", reason, "error", err)
			sm.ReturnToIdle()

			return
		}
	}

	sm.ReturnToIdle()
}

// TryClaimRebalancing attempts to atomically claim the rebalancing lifecycle
// from Idle. Strict-source CAS from Idle to Rebalancing.
//
// Designed for partition-lifecycle callers (monitorPartitions) that need to
// drive a rebalance but did NOT first enter Scaling. Without this primitive
// monitorPartitions would bypass the FSM entirely, violating the public
// contract that Rebalancing means "active partition rebalance in progress".
//
// scalingReason is recorded BEFORE the state-change notification fans out so
// subscribers see the reason on their first read of the Rebalancing state.
//
// Returns true if the claim succeeded; the caller is then responsible for
// invoking RunClaimedRebalanceErr to execute the rebalance and return to Idle.
func (sm *StateMachine) TryClaimRebalancing(_ context.Context, reason string) bool {
	if !sm.compareAndSwapState(types.CalcStateIdle, types.CalcStateRebalancing) {
		currentState := types.CalculatorState(sm.current.Load())
		sm.logger.Debug("partition rebalance claim deferred: state machine not idle",
			"current_state", currentState.String(),
			"reason", reason)
		return false
	}

	sm.mu.Lock()
	sm.scalingReason = reason
	sm.mu.Unlock()

	sm.logger.Info("entering rebalancing state via partition-lifecycle claim",
		"reason", reason)

	sm.notifyStateChange(types.CalcStateRebalancing)

	return true
}

// RunClaimedRebalanceErr runs the partition-rebalance callback for a
// previously-claimed lifecycle and returns the callback's error to the caller.
// The FSM is returned to Idle whether the callback succeeded, failed, or
// returned errShuttingDown.
//
// Distinct from RunClaimedRebalance: this variant gives the caller the
// callback error so the partition-lifecycle path can run
// restorePendingOnGraceBail when the rebalance bails on a recovery-grace
// re-check. The general RunClaimedRebalance preserves the existing void
// contract for emergency/scaling callers whose callback swallows
// errShuttingDown.
//
// Must be called after a successful TryClaimRebalancing.
func (sm *StateMachine) RunClaimedRebalanceErr(ctx context.Context, reason string) error {
	var cbErr error
	if sm.onPartitionRebalanceCb != nil {
		cbErr = sm.onPartitionRebalanceCb(ctx, reason)
		if cbErr != nil {
			sm.logger.Error("partition rebalance failed", "reason", reason, "error", cbErr)
		}
	}
	sm.ReturnToIdle()

	return cbErr
}

// EnterEmergency transitions to emergency state for immediate rebalancing.
//
// Backward-compatible wrapper around TryClaimEmergency + RunClaimedRebalance.
// If the claim fails (Rebalancing or Emergency already in flight), the call
// is deferred — the next poll cycle will detect the topology change.
//
// Parameters:
//   - ctx: Context for the rebalance operation
func (sm *StateMachine) EnterEmergency(ctx context.Context) {
	if !sm.TryClaimEmergency(ctx) {
		return
	}
	sm.RunClaimedRebalance(ctx, "emergency")
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

// compareAndSwapState atomically swaps the state from `from` to `to`.
//
// Returns true if the swap succeeded (the previous state was exactly `from`),
// false otherwise. Does NOT notify subscribers — callers that need a
// subscriber notification must invoke notifyStateChange after a successful CAS.
//
// Used for strict-source transitions where the caller wants to fail loudly
// if another goroutine has already moved the state (e.g., scaling timer racing
// an emergency claim).
func (sm *StateMachine) compareAndSwapState(from, to types.CalculatorState) bool {
	return sm.current.CompareAndSwap(int32(from), int32(to)) //nolint:gosec // G115: enum to int32 is safe
}

// notifyStateChange fans out a state-change notification to all subscribers.
//
// Does NOT change the underlying state — the caller is responsible for that
// (typically via compareAndSwapState). Designed for use after a successful
// strict-source CAS so the side-effect (notification) happens once and only
// for the winning transition.
func (sm *StateMachine) notifyStateChange(state types.CalculatorState) {
	sm.logger.Info("state transition", "to", state)

	sm.subscribers.Range(func(_ any, value any) bool {
		if sub, ok := value.(*stateSubscriber); ok {
			sub.trySend(state, sm.metrics)
		}
		return true
	})
}

// tryClaimEmergencyFrom is the strict-source claim primitive used by
// TryClaimEmergency. It atomically transitions `from` -> Emergency, sets the
// scaling reason to "emergency" BEFORE notifying subscribers (so subscribers
// observing the Emergency state see the correct reason on their first read),
// then fans out the notification.
//
// Returns true if the CAS succeeded; false otherwise.
func (sm *StateMachine) tryClaimEmergencyFrom(from types.CalculatorState) bool {
	if !sm.compareAndSwapState(from, types.CalcStateEmergency) {
		return false
	}

	// Set scalingReason BEFORE notification so subscribers see it on their
	// first read of the Emergency state.
	sm.mu.Lock()
	sm.scalingReason = "emergency"
	sm.mu.Unlock()

	sm.logger.Warn("entering emergency state - immediate rebalance",
		"from_state", from.String())

	sm.notifyStateChange(types.CalcStateEmergency)

	return true
}

// emitStateChange notifies all subscribers of a state transition.
//
// Retained for callers (EnterScaling, ReturnToIdle) whose transition source
// is not strict (any state -> target). Strict-source callers must use
// compareAndSwapState + notifyStateChange instead.
func (sm *StateMachine) emitStateChange(state types.CalculatorState) {
	// Atomically set the new state and emit exactly one notification.
	// This prevents duplicate notifications when multiple goroutines
	// attempt the same transition concurrently.
	for {
		old := types.CalculatorState(sm.current.Load())
		if old == state {
			return
		}

		if sm.current.CompareAndSwap(int32(old), int32(state)) { //nolint:gosec // G115: enum to int32 is safe
			sm.notifyStateChange(state)
			return
		}
		// Another goroutine changed the state; retry with the new current value
	}
}
