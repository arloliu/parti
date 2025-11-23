package testutil

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/arloliu/parti/types"
)

// ManagerWaiter defines the subset of Manager methods needed for waiting.
// This allows the helper to work with both real managers and test doubles.
type ManagerWaiter interface {
	// WaitState waits for the manager to reach the expected state within the timeout.
	WaitState(expectedState types.State, timeout time.Duration) <-chan error
}

// WaitAllManagersState waits for all managers to reach the expected state.
// This helper implements Pattern 4 (parallel wait with early failure) from the
// WaitState design discussion, which is the recommended approach for integration tests.
//
// If any manager fails to reach the state within the timeout, the function returns
// immediately with the first error encountered. If the context is cancelled, all
// waiting operations are abandoned and context.Canceled is returned.
//
// Parameters:
//   - ctx: Context for cancellation (recommended for test cleanup)
//   - managers: Slice of managers to wait on
//   - expectedState: Target state for all managers
//   - timeout: Maximum time to wait for each individual manager
//
// Returns:
//   - error: nil if all managers reached the state, first error encountered otherwise
//
// Example:
//
//	managers := []*parti.Manager{mgr1, mgr2, mgr3}
//	err := testutil.WaitAllManagersState(ctx, managers, types.StateStable, 30*time.Second)
//	require.NoError(t, err, "all managers should reach stable state")
func WaitAllManagersState(
	ctx context.Context,
	managers []ManagerWaiter,
	expectedState types.State,
	timeout time.Duration,
) error {
	if len(managers) == 0 {
		return nil
	}

	// Use errgroup-like pattern for parallel wait with early failure
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
		cancel   context.Context
		cancelFn context.CancelFunc
	)

	cancel, cancelFn = context.WithCancel(ctx)
	defer cancelFn()

	wg.Add(len(managers))
	for i, mgr := range managers {
		go func(index int, m ManagerWaiter) {
			defer wg.Done()

			select {
			case err := <-m.WaitState(expectedState, timeout):
				if err != nil {
					errOnce.Do(func() {
						firstErr = fmt.Errorf("manager[%d] failed to reach state %s: %w", index, expectedState, err)
						cancelFn() // Cancel other waiters on first failure
					})
				}
			case <-cancel.Done():
				// Context cancelled (either by parent or first failure)
				return
			}
		}(i, mgr)
	}

	wg.Wait()

	if firstErr != nil {
		return firstErr
	}

	if cancel.Err() != nil {
		return cancel.Err()
	}

	return nil
}

// WaitAnyManagerState waits for any manager to reach the expected state.
// This helper implements Pattern 2 (wait for ANY) from the WaitState design discussion.
//
// The function returns as soon as the first manager reaches the state. If all managers
// fail to reach the state within the timeout, a combined error is returned.
//
// Parameters:
//   - managers: Slice of managers to wait on
//   - expectedState: Target state to wait for
//   - timeout: Maximum time to wait for each individual manager
//
// Returns:
//   - int: Index of the first manager that reached the state (-1 if none succeeded)
//   - error: nil if any manager reached the state, combined error if all failed
//
// Example:
//
//	managers := []*parti.Manager{mgr1, mgr2, mgr3}
//	idx, err := testutil.WaitAnyManagerState(managers, types.StateLeader, 10*time.Second)
//	require.NoError(t, err, "at least one manager should become leader")
//	t.Logf("Manager %d became leader first", idx)
func WaitAnyManagerState(
	managers []ManagerWaiter,
	expectedState types.State,
	timeout time.Duration,
) (int, error) {
	if len(managers) == 0 {
		return -1, errors.New("no managers provided")
	}

	type result struct {
		index int
		err   error
	}

	resultCh := make(chan result, len(managers))

	// Start waiting on all managers
	for i, mgr := range managers {
		go func(index int, m ManagerWaiter) {
			err := <-m.WaitState(expectedState, timeout)
			resultCh <- result{index: index, err: err}
		}(i, mgr)
	}

	// Wait for first success or all failures
	errs := make([]error, 0, 1)
	for range managers {
		r := <-resultCh
		if r.err == nil {
			// First success - return immediately without waiting for other managers
			return r.index, nil
		}
		errs = append(errs, fmt.Errorf("manager[%d]: %w", r.index, r.err))
	}

	// All failed, return combined error
	return -1, fmt.Errorf("all managers failed to reach state %s: %w", expectedState, errors.Join(errs...))
}

// WaitManagerStates waits for a manager to progress through a sequence of states.
// This is useful for testing state machine transitions.
//
// Parameters:
//   - ctx: Context for cancellation
//   - mgr: Manager to watch
//   - states: Sequence of states to wait for (in order)
//   - timeout: Maximum time to wait for each individual state transition
//
// Returns:
//   - error: nil if all states reached, error on first failure
//
// Example:
//
//	states := []types.State{
//	    types.StateInit,
//	    types.StateClaimingID,
//	    types.StateElection,
//	    types.StateStable,
//	}
//	err := testutil.WaitManagerStates(ctx, mgr, states, 5*time.Second)
//	require.NoError(t, err, "manager should progress through all states")
func WaitManagerStates(
	ctx context.Context,
	mgr ManagerWaiter,
	states []types.State,
	timeout time.Duration,
) error {
	for i, state := range states {
		select {
		case err := <-mgr.WaitState(state, timeout):
			if err != nil {
				return fmt.Errorf("failed to reach state[%d] %s: %w", i, state, err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}
