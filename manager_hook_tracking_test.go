package parti

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/election"
	"github.com/arloliu/parti/v2/internal/heartbeat"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/stableid"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// newHookTestManager creates a minimal Manager with all internal components set to
// Nop implementations, suitable for testing hook dispatch and state transitions
// without requiring a NATS connection.
func newHookTestManager(hooks *types.Hooks) *Manager {
	m := &Manager{
		cfg: Config{
			DegradedAlert: DegradedAlertConfig{
				AlertInterval: 1 * time.Minute,
			},
		},
		hooks:           hooks,
		metrics:         metrics.NewNop(),
		logger:          logging.NewNop(),
		connMonitorStop: make(chan struct{}),
		idClaimer:       stableid.NewNop(),
		election:        election.NewNopElection(),
		heartbeat:       heartbeat.NewNop(),
	}
	m.state.Store(int32(StateInit))
	m.workerID.Store("")
	m.assignment.Store(Assignment{})
	// Set up a valid context so hooks can use it.
	m.ctx, m.cancel = context.WithCancel(context.Background())

	return m
}

func TestHookGoroutinesTrackedByWaitGroup(t *testing.T) {
	t.Parallel()

	t.Run("transitionState waits for OnStateChanged", func(t *testing.T) {
		t.Parallel()

		var hookDone atomic.Bool
		hooks := &types.Hooks{
			OnStateChanged: func(_ context.Context, _, _ types.State) error {
				time.Sleep(50 * time.Millisecond)
				hookDone.Store(true)
				return nil
			},
		}

		m := newHookTestManager(hooks)
		defer m.cancel()

		// Trigger transition that fires the hook
		m.transitionState(StateClaimingID)

		// Wait for all tracked goroutines
		m.wg.Wait()

		require.True(t, hookDone.Load(), "OnStateChanged hook must complete before wg.Wait returns")
	})

	t.Run("transitionState handles nil OnStateChanged safely", func(t *testing.T) {
		t.Parallel()

		hooks := &types.Hooks{} // OnStateChanged is nil
		m := newHookTestManager(hooks)
		defer m.cancel()

		// Must not panic
		require.NotPanics(t, func() {
			m.transitionState(StateClaimingID)
		})

		m.wg.Wait()
	})

	t.Run("logError waits for OnError", func(t *testing.T) {
		t.Parallel()

		var hookDone atomic.Bool
		hooks := &types.Hooks{
			OnError: func(_ context.Context, _ error) error {
				time.Sleep(50 * time.Millisecond)
				hookDone.Store(true)
				return nil
			},
		}

		m := newHookTestManager(hooks)
		defer m.cancel()

		testErr := errors.New("test error")
		m.logError("something failed", "error", testErr)

		// Wait for all tracked goroutines
		m.wg.Wait()

		require.True(t, hookDone.Load(), "OnError hook must complete before wg.Wait returns")
	})

	t.Run("logError handles nil OnError safely", func(t *testing.T) {
		t.Parallel()

		hooks := &types.Hooks{} // OnError is nil
		m := newHookTestManager(hooks)
		defer m.cancel()

		testErr := errors.New("test error")

		require.NotPanics(t, func() {
			m.logError("something failed", "error", testErr)
		})

		m.wg.Wait()
	})

	t.Run("enterDegraded waits for OnStateChanged and OnDegraded", func(t *testing.T) {
		t.Parallel()

		var stateHookDone atomic.Bool
		var degradedHookDone atomic.Bool

		hooks := &types.Hooks{
			OnStateChanged: func(_ context.Context, _, _ types.State) error {
				time.Sleep(50 * time.Millisecond)
				stateHookDone.Store(true)
				return nil
			},
			OnDegraded: func(_ context.Context, _ string) error {
				time.Sleep(50 * time.Millisecond)
				degradedHookDone.Store(true)
				return nil
			},
		}

		m := newHookTestManager(hooks)
		// Set a valid state that allows transition check within enterDegraded
		m.state.Store(int32(StateStable))

		m.enterDegraded("test: NATS down")

		// Cancel to stop monitorDegradedAlerts goroutine launched by enterDegraded
		m.cancel()
		m.wg.Wait()

		require.True(t, stateHookDone.Load(), "OnStateChanged hook must complete before wg.Wait returns")
		require.True(t, degradedHookDone.Load(), "OnDegraded hook must complete before wg.Wait returns")
	})

	t.Run("exitDegraded waits for OnStateChanged", func(t *testing.T) {
		t.Parallel()

		var hookDone atomic.Bool

		hooks := &types.Hooks{
			OnStateChanged: func(_ context.Context, _, _ types.State) error {
				time.Sleep(50 * time.Millisecond)
				hookDone.Store(true)
				return nil
			},
		}

		m := newHookTestManager(hooks)
		defer m.cancel()
		// Put into degraded state first
		m.degradedSince.Store(time.Now().UnixNano())
		m.state.Store(int32(StateDegraded))

		m.exitDegraded()

		// Wait for all tracked goroutines
		m.wg.Wait()

		require.True(t, hookDone.Load(), "OnStateChanged hook must complete before wg.Wait returns")
	})

	t.Run("multiple hooks complete before wg.Wait", func(t *testing.T) {
		t.Parallel()

		var hookCount atomic.Int32

		hooks := &types.Hooks{
			OnStateChanged: func(_ context.Context, _, _ types.State) error {
				time.Sleep(30 * time.Millisecond)
				hookCount.Add(1)
				return nil
			},
		}

		m := newHookTestManager(hooks)
		defer m.cancel()

		// Trigger several state transitions rapidly (state machine starts at Init)
		m.transitionState(StateClaimingID)
		m.transitionState(StateElection)
		m.transitionState(StateWaitingAssignment)

		m.wg.Wait()

		require.Equal(t, int32(3), hookCount.Load(),
			"all 3 hook invocations must complete before wg.Wait returns")
	})
}
