package parti

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestManager_WaitState_AlreadyInState(t *testing.T) {
	m := &Manager{}
	m.state.Store(int32(StateStable))

	// Should return immediately if already in expected state
	start := time.Now()
	errCh := m.WaitState(StateStable, 5*time.Second)
	err := <-errCh

	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Less(t, elapsed, 100*time.Millisecond, "Should return immediately when already in state")
}

func TestManager_WaitState_StateTransition(t *testing.T) {
	m := &Manager{}
	m.state.Store(int32(StateInit))

	// Start waiting for Stable state
	errCh := m.WaitState(StateStable, 2*time.Second)

	// Transition to target state after a delay
	go func() {
		time.Sleep(200 * time.Millisecond)
		m.state.Store(int32(StateStable))
	}()

	// Should receive nil when state is reached
	err := <-errCh
	require.NoError(t, err)
}

func TestManager_WaitState_Timeout(t *testing.T) {
	m := &Manager{}
	m.state.Store(int32(StateInit))

	// Wait for a state that never happens
	start := time.Now()
	errCh := m.WaitState(StateStable, 500*time.Millisecond)
	err := <-errCh

	elapsed := time.Since(start)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.GreaterOrEqual(t, elapsed, 500*time.Millisecond, "Should wait for full timeout")
	require.Less(t, elapsed, 600*time.Millisecond, "Should not wait significantly longer than timeout")
}

func TestManager_WaitState_MultipleWaiters(t *testing.T) {
	m := &Manager{}
	m.state.Store(int32(StateInit))

	// Start multiple goroutines waiting for the same state
	numWaiters := 5
	results := make(chan error, numWaiters)

	for i := 0; i < numWaiters; i++ {
		go func() {
			errCh := m.WaitState(StateStable, 2*time.Second)
			results <- <-errCh
		}()
	}

	// Transition to target state
	time.Sleep(100 * time.Millisecond)
	m.state.Store(int32(StateStable))

	// All waiters should succeed
	for i := 0; i < numWaiters; i++ {
		err := <-results
		require.NoError(t, err, "Waiter %d should succeed", i)
	}
}

func TestManager_WaitState_SelectPattern(t *testing.T) {
	m := &Manager{}
	m.state.Store(int32(StateInit))

	// Use select pattern with context
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Transition to target state after delay
	go func() {
		time.Sleep(100 * time.Millisecond)
		m.state.Store(int32(StateStable))
	}()

	// Use select to handle both state wait and context cancellation
	select {
	case err := <-m.WaitState(StateStable, 1*time.Second):
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("Context cancelled before state reached")
	}
}

func TestManager_WaitState_SequentialStates(t *testing.T) {
	m := &Manager{}
	m.state.Store(int32(StateInit))

	// Simulate state progression with delays longer than polling interval (50ms)
	// to ensure each state is observed
	go func() {
		time.Sleep(100 * time.Millisecond)
		m.state.Store(int32(StateClaimingID))
		time.Sleep(100 * time.Millisecond)
		m.state.Store(int32(StateElection))
		time.Sleep(100 * time.Millisecond)
		m.state.Store(int32(StateStable))
	}()

	// Wait for each state in sequence
	err := <-m.WaitState(StateClaimingID, 500*time.Millisecond)
	require.NoError(t, err)

	err = <-m.WaitState(StateElection, 500*time.Millisecond)
	require.NoError(t, err)

	err = <-m.WaitState(StateStable, 500*time.Millisecond)
	require.NoError(t, err)
}

func TestManager_WaitState_ChannelClosedAfterResult(t *testing.T) {
	m := &Manager{}
	m.state.Store(int32(StateStable))

	errCh := m.WaitState(StateStable, 1*time.Second)

	// First read should get nil
	err := <-errCh
	require.NoError(t, err)

	// Second read should indicate channel is closed (zero value + false)
	err, ok := <-errCh
	require.False(t, ok, "Channel should be closed after sending result")
	require.Nil(t, err, "Closed channel should return nil error")
}

func TestManager_WaitState_MultipleManagersPattern(t *testing.T) {
	// Create multiple managers
	managers := make([]*Manager, 3)
	for i := range managers {
		managers[i] = &Manager{}
		managers[i].state.Store(int32(StateInit))
	}

	// Transition them to Stable at different times
	go func() {
		time.Sleep(50 * time.Millisecond)
		managers[0].state.Store(int32(StateStable))
		time.Sleep(50 * time.Millisecond)
		managers[1].state.Store(int32(StateStable))
		time.Sleep(50 * time.Millisecond)
		managers[2].state.Store(int32(StateStable))
	}()

	// Wait for all managers to reach Stable
	errCh := make(chan error, len(managers))
	for _, mgr := range managers {
		go func(m *Manager) {
			err := <-m.WaitState(StateStable, 1*time.Second)
			if err != nil {
				errCh <- err
			} else {
				errCh <- nil
			}
		}(mgr)
	}

	// Collect results
	for i := range managers {
		err := <-errCh
		require.NoError(t, err, "Manager %d should reach Stable", i)
	}
}
