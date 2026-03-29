package parti

import (
	"context"
	"fmt"
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

// transitionState transitions to a new state and triggers hooks.
func (m *Manager) transitionState(from, to State) {
	// Validate state transition
	if !m.isValidTransition(from, to) {
		m.logError("invalid state transition attempted",
			"from", from.String(),
			"to", to.String(),
		)

		return
	}

	m.state.Store(int32(to)) //nolint:gosec // State values are controlled enum

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
	// Define valid state transitions
	validTransitions := map[State][]State{
		StateInit:              {StateClaimingID, StateShutdown},
		StateClaimingID:        {StateElection, StateShutdown},
		StateElection:          {StateWaitingAssignment, StateShutdown},
		StateWaitingAssignment: {StateStable, StateScaling, StateRebalancing, StateEmergency, StateShutdown},
		StateStable:            {StateScaling, StateRebalancing, StateEmergency, StateShutdown},
		StateScaling:           {StateRebalancing, StateWaitingAssignment, StateStable, StateShutdown},
		StateRebalancing:       {StateStable, StateWaitingAssignment, StateShutdown},
		StateEmergency:         {StateStable, StateWaitingAssignment, StateShutdown},
		StateShutdown:          {}, // Terminal state - no transitions allowed
	}

	allowedStates, exists := validTransitions[from]
	if !exists {
		return false
	}

	for _, allowed := range allowedStates {
		if allowed == to {
			return true
		}
	}

	return false
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
		if currentState != StateScaling && currentState != StateRebalancing && currentState != StateEmergency {
			// Already stable or in a non-active state, no transition needed.
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

	// Only transition if state actually changed
	if currentState != targetState {
		m.transitionState(currentState, targetState)
	}

	return nil
}
