package parti

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/arloliu/parti/v2/types"
)

// WaitState waits for the manager to reach the expected state within the timeout period.
//
// This method is useful for testing and synchronization scenarios where you need to
// wait for the manager to reach a specific state before proceeding.
//
// The method returns a read-only channel that will receive exactly one value:
//   - nil if the expected state is reached within the timeout
//   - context.DeadlineExceeded if the timeout expires before reaching the state
//
// The channel is closed after sending the result, allowing safe use in select statements.
//
// Parameters:
//   - expectedState: The state to wait for
//   - timeout: Maximum duration to wait for the state
//
// Returns:
//   - <-chan error: A channel that receives the result (nil on success, error on timeout)
//
// Example:
//
//	// Wait for manager to reach Stable state
//	errCh := manager.WaitState(StateStable, 10*time.Second)
//	if err := <-errCh; err != nil {
//	    log.Printf("Failed to reach Stable state: %v", err)
//	}
//
//	// Using with select for multiple operations
//	select {
//	case err := <-manager.WaitState(StateStable, 5*time.Second):
//	    if err != nil {
//	        return fmt.Errorf("timeout waiting for stable state: %w", err)
//	    }
//	case <-ctx.Done():
//	    return ctx.Err()
//	}
//
//	// Waiting for multiple managers
//	for i, mgr := range managers {
//	    if err := <-mgr.WaitState(StateStable, 10*time.Second); err != nil {
//	        return fmt.Errorf("manager %d failed: %w", i, err)
//	    }
//	}
func (m *Manager) WaitState(expectedState State, timeout time.Duration) <-chan error {
	ch := make(chan error, 1) // Buffered to prevent goroutine leak

	go func() {
		defer close(ch)

		// Check if already in expected state
		if m.State() == expectedState {
			ch <- nil
			return
		}

		// Poll for state changes
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()

		timeoutTimer := time.NewTimer(timeout)
		defer timeoutTimer.Stop()

		for {
			select {
			case <-ticker.C:
				if m.State() == expectedState {
					ch <- nil
					return
				}
			case <-timeoutTimer.C:
				ch <- context.DeadlineExceeded
				return
			}
		}
	}()

	return ch
}

// transitionState atomically transitions to the given state using CAS.
//
// It reads the current state, validates the transition, and attempts CAS.
// On CAS failure caused by a concurrent state change, it retries with the
// newly observed current state. Returns true on success, false if the
// transition is invalid from every observed current state or if the
// manager has already reached the target state.
//
// This approach eliminates the TOCTOU race where the caller read the state,
// validated it, and then stored unconditionally — allowing concurrent writers
// to silently overwrite each other.
func (m *Manager) transitionState(to State) bool {
	const maxRetries = 16

	for i := range maxRetries {
		from := m.State()
		if from == to {
			return true // Already in target state; treat as success
		}
		if !m.isValidTransition(from, to) {
			if i == 0 {
				// Only log on first attempt to avoid noise from CAS retries
				m.logError("invalid state transition attempted",
					"from", from.String(),
					"to", to.String(),
				)
			}

			return false
		}

		if m.state.CompareAndSwap(int32(from), int32(to)) { //nolint:gosec // State values are controlled enum
			m.emitTransitionEffects(from, to)

			return true
		}
		// CAS failed: another goroutine changed state concurrently; retry
	}

	m.logError("state transition abandoned: too many concurrent attempts",
		"to", to.String(),
		"current", m.State().String(),
	)

	return false
}

// emitTransitionEffects fires the observable side effects of a state transition
// that has just been committed: the structured log line, the OnStateChanged hook
// (dispatched through invokeHook so Stop's WaitGroup waits for it), and the
// RecordStateTransition metric. The transition paths — the CAS-retry
// transitionState and the lock-free casToStableFrom* helpers
// (casToStableFromWaitingAssignment, casToStableFromDegraded) — commit the state
// themselves and then call this, so observers see an identical transition
// regardless of which path ran. from is taken by value, so the hook closure
// captures the committed transition without the caller needing a manual copy.
func (m *Manager) emitTransitionEffects(from, to State) {
	m.logger.Info("state transition",
		"from", from.String(),
		"to", to.String(),
		"worker_id", m.WorkerID(),
	)

	// Trigger state change hook (tracked by WaitGroup so Stop waits for completion)
	if m.hooks.OnStateChanged != nil {
		m.invokeHook("state change", func() error {
			return m.hooks.OnStateChanged(m.ctx, from, to)
		})
	}

	// Record metrics (always non-nil, defaults to nopMetrics)
	m.metrics.RecordStateTransition(from, to, 0)
}

// isValidTransition validates that a state transition is allowed.
//
// Returns:
//   - bool: true if transition is valid, false otherwise
func (m *Manager) isValidTransition(from, to State) bool {
	// Define valid state transitions.
	// StateDegraded is a first-class state reachable from any active (non-Init/Shutdown)
	// state, and exits only to Stable or Shutdown.
	validTransitions := map[State][]State{
		StateInit:              {StateClaimingID, StateShutdown},
		StateClaimingID:        {StateElection, StateShutdown},
		StateElection:          {StateWaitingAssignment, StateShutdown},
		StateWaitingAssignment: {StateStable, StateScaling, StateRebalancing, StateEmergency, StateDegraded, StateShutdown},
		StateStable:            {StateScaling, StateRebalancing, StateEmergency, StateDegraded, StateShutdown},
		StateScaling:           {StateRebalancing, StateWaitingAssignment, StateStable, StateDegraded, StateShutdown},
		StateRebalancing:       {StateStable, StateWaitingAssignment, StateDegraded, StateShutdown},
		StateEmergency:         {StateStable, StateWaitingAssignment, StateDegraded, StateShutdown},
		StateDegraded:          {StateStable, StateShutdown},
		StateShutdown:          {}, // Terminal state - no transitions allowed
	}

	allowedStates, exists := validTransitions[from]
	if !exists {
		return false
	}

	return slices.Contains(allowedStates, to)
}

// syncStateFromCalculator updates Manager state based on Calculator state.
//
// State mapping:
//   - CalcStateIdle       → StateStable (if Manager is in active state)
//   - CalcStateScaling    → StateScaling
//   - CalcStateRebalancing → StateRebalancing
//   - CalcStateEmergency  → StateEmergency
//
// Parameters:
//   - calcState: Current calculator state to synchronize with
//
// Returns:
//   - error: State transition error if invalid transition attempted
func (m *Manager) syncStateFromCalculator(calcState types.CalculatorState) error {
	currentState := m.State()

	// Skip if Manager is in initialization or shutdown states
	// BUT allow Scaling/Rebalancing/Emergency states to be processed even from WaitingAssignment
	if currentState == StateInit || currentState == StateClaimingID ||
		currentState == StateElection || currentState == StateShutdown {
		return nil
	}

	// Special handling for WaitingAssignment: only process active calculator states
	if currentState == StateWaitingAssignment {
		if calcState == types.CalcStateIdle {
			return nil
		}
		// Allow Scaling/Rebalancing/Emergency to transition from WaitingAssignment
	}

	var targetState State

	switch calcState {
	case types.CalcStateIdle:
		// Only transition to Stable if we're in an intermediate state.
		// This prevents flapping back to Stable when we're already stable,
		// which can happen when subscribing to calculator state changes
		// (the calculator sends its current state immediately on subscription).
		if !isCalculatorOwnedActiveState(currentState) {
			// Already stable or in a non-active state, no transition needed.
			return nil
		}
		if !m.startupAssignmentApplied.Load() {
			// Startup readiness is owned by the local apply/store/ack path.
			// Calculator Idle can arrive before the leader has applied its own
			// startup assignment; do not advertise StateStable until that path
			// completes.
			return nil
		}

		targetState = StateStable

	case types.CalcStateScaling:
		targetState = StateScaling

	case types.CalcStateRebalancing:
		targetState = StateRebalancing

	case types.CalcStateEmergency:
		targetState = StateEmergency

	default:
		return fmt.Errorf("unknown calculator state: %v", calcState)
	}

	// Only transition if state actually changed; CAS inside transitionState handles concurrent modifications
	if currentState != targetState {
		m.transitionState(targetState)
	}

	return nil
}
