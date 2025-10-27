package testutil

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// mockManager implements ManagerWaiter for testing.
type mockManager struct {
	currentState atomic.Int32
	transitions  []stateTransition
}

type stateTransition struct {
	delay time.Duration
	state types.State
}

func newMockManager(initialState types.State) *mockManager {
	m := &mockManager{}
	m.currentState.Store(int32(initialState))

	return m
}

func (m *mockManager) State() types.State {
	return types.State(m.currentState.Load())
}

func (m *mockManager) scheduleTransitions(transitions ...stateTransition) {
	m.transitions = transitions
	go func() {
		for _, t := range transitions {
			time.Sleep(t.delay)
			m.currentState.Store(int32(t.state))
		}
	}()
}

func (m *mockManager) WaitState(expectedState types.State, timeout time.Duration) <-chan error {
	ch := make(chan error, 1)
	go func() {
		defer close(ch)

		if m.State() == expectedState {
			ch <- nil

			return
		}

		ticker := time.NewTicker(10 * time.Millisecond)
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

func TestWaitAllManagersState_AllSucceed(t *testing.T) {
	mgr1 := newMockManager(types.StateInit)
	mgr2 := newMockManager(types.StateInit)
	mgr3 := newMockManager(types.StateInit)

	// Schedule state transitions at different times
	mgr1.scheduleTransitions(stateTransition{50 * time.Millisecond, types.StateStable})
	mgr2.scheduleTransitions(stateTransition{100 * time.Millisecond, types.StateStable})
	mgr3.scheduleTransitions(stateTransition{150 * time.Millisecond, types.StateStable})

	managers := []ManagerWaiter{mgr1, mgr2, mgr3}

	ctx := context.Background()
	err := WaitAllManagersState(ctx, managers, types.StateStable, 1*time.Second)

	require.NoError(t, err)
	require.Equal(t, types.StateStable, mgr1.State())
	require.Equal(t, types.StateStable, mgr2.State())
	require.Equal(t, types.StateStable, mgr3.State())
}

func TestWaitAllManagersState_OneTimeout(t *testing.T) {
	mgr1 := newMockManager(types.StateInit)
	mgr2 := newMockManager(types.StateInit)
	mgr3 := newMockManager(types.StateInit)

	// mgr1 and mgr2 succeed, mgr3 never transitions
	mgr1.scheduleTransitions(stateTransition{50 * time.Millisecond, types.StateStable})
	mgr2.scheduleTransitions(stateTransition{100 * time.Millisecond, types.StateStable})

	managers := []ManagerWaiter{mgr1, mgr2, mgr3}

	ctx := context.Background()
	start := time.Now()
	err := WaitAllManagersState(ctx, managers, types.StateStable, 500*time.Millisecond)

	elapsed := time.Since(start)

	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Contains(t, err.Error(), "manager[2]")
	require.Contains(t, err.Error(), "Stable")
	// Should return quickly after first timeout (500ms), not wait for all
	require.Less(t, elapsed, 800*time.Millisecond)
}

func TestWaitAllManagersState_ContextCancellation(t *testing.T) {
	mgr1 := newMockManager(types.StateInit)
	mgr2 := newMockManager(types.StateInit)

	// Both managers transition slowly
	mgr1.scheduleTransitions(stateTransition{500 * time.Millisecond, types.StateStable})
	mgr2.scheduleTransitions(stateTransition{500 * time.Millisecond, types.StateStable})

	managers := []ManagerWaiter{mgr1, mgr2}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := WaitAllManagersState(ctx, managers, types.StateStable, 1*time.Second)

	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestWaitAllManagersState_EmptyManagers(t *testing.T) {
	var managers []ManagerWaiter

	ctx := context.Background()
	err := WaitAllManagersState(ctx, managers, types.StateStable, 1*time.Second)

	require.NoError(t, err)
}

func TestWaitAllManagersState_AlreadyInState(t *testing.T) {
	mgr1 := newMockManager(types.StateStable)
	mgr2 := newMockManager(types.StateStable)
	mgr3 := newMockManager(types.StateStable)

	managers := []ManagerWaiter{mgr1, mgr2, mgr3}

	ctx := context.Background()
	start := time.Now()
	err := WaitAllManagersState(ctx, managers, types.StateStable, 1*time.Second)
	elapsed := time.Since(start)

	require.NoError(t, err)
	// Should return almost immediately since all are already in state
	require.Less(t, elapsed, 100*time.Millisecond)
}

func TestWaitAnyManagerState_FirstSucceeds(t *testing.T) {
	mgr1 := newMockManager(types.StateInit)
	mgr2 := newMockManager(types.StateInit)
	mgr3 := newMockManager(types.StateInit)

	// mgr2 succeeds first
	mgr1.scheduleTransitions(stateTransition{150 * time.Millisecond, types.StateElection})
	mgr2.scheduleTransitions(stateTransition{50 * time.Millisecond, types.StateElection})
	mgr3.scheduleTransitions(stateTransition{200 * time.Millisecond, types.StateElection})

	managers := []ManagerWaiter{mgr1, mgr2, mgr3}

	idx, err := WaitAnyManagerState(managers, types.StateElection, 1*time.Second)

	require.NoError(t, err)
	require.Equal(t, 1, idx) // mgr2 (index 1) succeeded first
}

func TestWaitAnyManagerState_AllTimeout(t *testing.T) {
	mgr1 := newMockManager(types.StateInit)
	mgr2 := newMockManager(types.StateInit)
	mgr3 := newMockManager(types.StateInit)

	// None transition in time
	mgr1.scheduleTransitions(stateTransition{2 * time.Second, types.StateElection})
	mgr2.scheduleTransitions(stateTransition{2 * time.Second, types.StateElection})
	mgr3.scheduleTransitions(stateTransition{2 * time.Second, types.StateElection})

	managers := []ManagerWaiter{mgr1, mgr2, mgr3}

	idx, err := WaitAnyManagerState(managers, types.StateElection, 500*time.Millisecond)

	require.Error(t, err)
	require.Equal(t, -1, idx)
	require.Contains(t, err.Error(), "all managers failed")
	require.Contains(t, err.Error(), "Election")
	require.Contains(t, err.Error(), "manager[0]")
	require.Contains(t, err.Error(), "manager[1]")
	require.Contains(t, err.Error(), "manager[2]")
}

func TestWaitAnyManagerState_EmptyManagers(t *testing.T) {
	var managers []ManagerWaiter

	idx, err := WaitAnyManagerState(managers, types.StateStable, 1*time.Second)

	require.Error(t, err)
	require.Equal(t, -1, idx)
	require.Contains(t, err.Error(), "no managers provided")
}

func TestWaitAnyManagerState_AlreadyInState(t *testing.T) {
	mgr1 := newMockManager(types.StateInit)
	mgr2 := newMockManager(types.StateElection) // Already in target state
	mgr3 := newMockManager(types.StateInit)

	managers := []ManagerWaiter{mgr1, mgr2, mgr3}

	start := time.Now()
	idx, err := WaitAnyManagerState(managers, types.StateElection, 1*time.Second)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Equal(t, 1, idx) // mgr2 (index 1) was already in state
	// Should return quickly
	require.Less(t, elapsed, 100*time.Millisecond)
}

func TestWaitManagerStates_SequentialSuccess(t *testing.T) {
	mgr := newMockManager(types.StateInit)

	// Schedule sequential state transitions
	mgr.scheduleTransitions(
		stateTransition{50 * time.Millisecond, types.StateClaimingID},
		stateTransition{100 * time.Millisecond, types.StateElection},
		stateTransition{150 * time.Millisecond, types.StateStable},
	)

	states := []types.State{
		types.StateInit, // Already in this state
		types.StateClaimingID,
		types.StateElection,
		types.StateStable,
	}

	ctx := context.Background()
	err := WaitManagerStates(ctx, mgr, states, 1*time.Second)

	require.NoError(t, err)
	require.Equal(t, types.StateStable, mgr.State())
}

func TestWaitManagerStates_TimeoutOnSecondState(t *testing.T) {
	mgr := newMockManager(types.StateInit)

	// First transition succeeds, second never happens
	mgr.scheduleTransitions(
		stateTransition{50 * time.Millisecond, types.StateClaimingID},
	)

	states := []types.State{
		types.StateInit,
		types.StateClaimingID,
		types.StateElection, // This will timeout
	}

	ctx := context.Background()
	err := WaitManagerStates(ctx, mgr, states, 300*time.Millisecond)

	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Contains(t, err.Error(), "state[2]")
	require.Contains(t, err.Error(), "Election")
}

func TestWaitManagerStates_ContextCancellation(t *testing.T) {
	mgr := newMockManager(types.StateInit)

	// Slow transition
	mgr.scheduleTransitions(
		stateTransition{500 * time.Millisecond, types.StateClaimingID},
	)

	states := []types.State{
		types.StateInit,
		types.StateClaimingID,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := WaitManagerStates(ctx, mgr, states, 1*time.Second)

	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestWaitManagerStates_EmptyStates(t *testing.T) {
	mgr := newMockManager(types.StateInit)

	var states []types.State

	ctx := context.Background()
	err := WaitManagerStates(ctx, mgr, states, 1*time.Second)

	require.NoError(t, err)
}

func TestWaitAllManagersState_StressTest(t *testing.T) {
	const numManagers = 20

	managers := make([]ManagerWaiter, numManagers)
	for i := range managers {
		mgr := newMockManager(types.StateInit)
		// Stagger transitions 0-200ms
		mgr.scheduleTransitions(stateTransition{
			delay: time.Duration(i*10) * time.Millisecond,
			state: types.StateStable,
		})
		managers[i] = mgr
	}

	ctx := context.Background()
	start := time.Now()
	err := WaitAllManagersState(ctx, managers, types.StateStable, 2*time.Second)
	elapsed := time.Since(start)

	require.NoError(t, err)
	// Should complete around 200ms (last transition) + small overhead
	require.Less(t, elapsed, 500*time.Millisecond)

	// Verify all reached the state
	for i, mgr := range managers {
		m, ok := mgr.(*mockManager)
		require.True(t, ok)
		require.Equal(t, types.StateStable, m.State(), "manager %d should be stable", i)
	}
}

func TestWaitAnyManagerState_RaceCondition(t *testing.T) {
	const numManagers = 10

	managers := make([]ManagerWaiter, numManagers)
	for i := range managers {
		mgr := newMockManager(types.StateInit)
		// All transition at roughly same time (racing)
		mgr.scheduleTransitions(stateTransition{
			delay: 50 * time.Millisecond,
			state: types.StateElection,
		})
		managers[i] = mgr
	}

	idx, err := WaitAnyManagerState(managers, types.StateElection, 1*time.Second)

	require.NoError(t, err)
	require.GreaterOrEqual(t, idx, 0)
	require.Less(t, idx, numManagers)

	// Verify at least the returned manager is in leader state
	m, ok := managers[idx].(*mockManager)
	require.True(t, ok)
	require.Equal(t, types.StateElection, m.State())
}

func TestWaitAllManagersState_EarlyFailure(t *testing.T) {
	mgr1 := newMockManager(types.StateInit)
	mgr2 := newMockManager(types.StateInit)
	mgr3 := newMockManager(types.StateInit)

	// mgr1 succeeds, mgr2 fails quickly, mgr3 would succeed slowly
	mgr1.scheduleTransitions(stateTransition{50 * time.Millisecond, types.StateStable})
	// mgr2 never transitions (will timeout)
	mgr3.scheduleTransitions(stateTransition{2 * time.Second, types.StateStable})

	managers := []ManagerWaiter{mgr1, mgr2, mgr3}

	ctx := context.Background()
	start := time.Now()
	err := WaitAllManagersState(ctx, managers, types.StateStable, 300*time.Millisecond)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	// Should fail around 300ms (mgr2 timeout), not wait 2s for mgr3
	require.Less(t, elapsed, 600*time.Millisecond)
}

func TestWaitManagerWaiter_TypeAssertion(t *testing.T) {
	// Verify mockManager implements ManagerWaiter
	var _ ManagerWaiter = (*mockManager)(nil)

	// Verify we can use the interface
	mgr := newMockManager(types.StateInit)
	var iface ManagerWaiter = mgr

	ch := iface.WaitState(types.StateInit, 1*time.Second)
	err := <-ch

	require.NoError(t, err)
}

func TestWaitAllManagersState_MixedStates(t *testing.T) {
	// Test waiting for managers starting in different states
	mgr1 := newMockManager(types.StateStable)   // Already at target
	mgr2 := newMockManager(types.StateInit)     // Needs transition
	mgr3 := newMockManager(types.StateElection) // Different state, needs transition

	mgr2.scheduleTransitions(stateTransition{50 * time.Millisecond, types.StateStable})
	mgr3.scheduleTransitions(stateTransition{100 * time.Millisecond, types.StateStable})

	managers := []ManagerWaiter{mgr1, mgr2, mgr3}

	ctx := context.Background()
	err := WaitAllManagersState(ctx, managers, types.StateStable, 1*time.Second)

	require.NoError(t, err)
	require.Equal(t, types.StateStable, mgr1.State())
	require.Equal(t, types.StateStable, mgr2.State())
	require.Equal(t, types.StateStable, mgr3.State())
}

func TestWaitAnyManagerState_SomeTimeout(t *testing.T) {
	mgr1 := newMockManager(types.StateInit)
	mgr2 := newMockManager(types.StateInit)
	mgr3 := newMockManager(types.StateInit)

	// mgr1 and mgr2 timeout, mgr3 succeeds
	mgr3.scheduleTransitions(stateTransition{100 * time.Millisecond, types.StateElection})

	managers := []ManagerWaiter{mgr1, mgr2, mgr3}

	idx, err := WaitAnyManagerState(managers, types.StateElection, 500*time.Millisecond)

	require.NoError(t, err)
	require.Equal(t, 2, idx) // mgr3 (index 2) succeeded
}

func BenchmarkWaitAllManagersState(b *testing.B) {
	managers := make([]ManagerWaiter, 10)
	for i := range managers {
		managers[i] = newMockManager(types.StateStable)
	}

	ctx := context.Background()

	b.ResetTimer()
	for b.Loop() {
		_ = WaitAllManagersState(ctx, managers, types.StateStable, 1*time.Second)
	}
}

func BenchmarkWaitAnyManagerState(b *testing.B) {
	managers := make([]ManagerWaiter, 10)
	for i := range managers {
		managers[i] = newMockManager(types.StateElection)
	}

	b.ResetTimer()
	for b.Loop() {
		_, _ = WaitAnyManagerState(managers, types.StateElection, 1*time.Second)
	}
}
