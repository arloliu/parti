package parti

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// TestExitDegraded_OnlyClearsOnConfirmedDegradedTransition pins the
// exit-on-confirmed-Degraded contract: exitDegraded clears the degraded record
// ONLY when it performs a genuine Degraded->Stable transition. It must NOT clear
// the record when the state is not (yet) Degraded — the transient an enterDegraded
// caller is in after it published the record (single-swap CAS) but before it ran
// transitionState(StateDegraded).
//
// Without the fix, exitDegraded calls transitionState(StateStable), which returns
// true vacuously when the state is already Stable (and performs a REAL but wrong
// transition from any other active state, e.g. Scaling->Stable), then unconditionally
// Store(nil)s the record. A concurrent enterer's transitionState(StateDegraded)
// then lands last, stranding the worker in Degraded with a nil record — recovery
// and alerting both early-return on a nil record, so it cannot self-heal until an
// unrelated degrade re-arms it.
//
// enterDegraded is reachable from every active state (isValidTransition allows
// *->Degraded from Stable/Scaling/Rebalancing/Emergency/WaitingAssignment), so the
// pre-transition window can be entered from any of them — hence the table.
func TestExitDegraded_OnlyClearsOnConfirmedDegradedTransition(t *testing.T) {
	t.Parallel()

	// Every state an enterDegraded caller can be in during the pre-transition
	// window (record published, transitionState(StateDegraded) not yet run).
	preTransitionStates := []State{
		StateStable,
		StateScaling,
		StateRebalancing,
		StateEmergency,
		StateWaitingAssignment,
	}

	for _, start := range preTransitionStates {
		t.Run(start.String(), func(t *testing.T) {
			t.Parallel()

			var hookFired atomic.Bool
			m := newHookTestManager(&types.Hooks{
				OnStateChanged: func(_ context.Context, _, _ types.State) error {
					hookFired.Store(true)
					return nil
				},
			})
			defer m.cancel()

			// Arm the racy transient: a degraded record is live, but the state is
			// NOT Degraded (the enterer has not transitioned yet).
			m.markDegraded(time.Now().UnixNano(), "startup-timeout")
			m.state.Store(int32(start))

			m.exitDegraded()
			m.wg.Wait()

			require.NotNil(t, m.degraded.Load(),
				"exitDegraded must NOT clear the record when state is %s (not Degraded); "+
					"clearing it strands a concurrent enterer in Degraded+nil-record", start)
			require.Equal(t, start, m.State(),
				"exitDegraded must not perform a spurious transition out of %s when not Degraded", start)
			require.False(t, hookFired.Load(),
				"exitDegraded must not emit OnStateChanged when it did not transition out of %s", start)
		})
	}
}

// TestExitDegraded_GenuineExitStillWorks is the positive-path guard: a real
// Degraded->Stable exit still clears the record, transitions to Stable, and emits
// the OnStateChanged(Degraded, Stable) effect — i.e. the fix does not weaken the
// legitimate recovery path.
func TestExitDegraded_GenuineExitStillWorks(t *testing.T) {
	t.Parallel()

	var gotFrom, gotTo atomic.Int32
	var fired atomic.Bool
	hooks := &types.Hooks{
		OnStateChanged: func(_ context.Context, from, to types.State) error {
			gotFrom.Store(int32(from))
			gotTo.Store(int32(to))
			fired.Store(true)
			return nil
		},
	}

	m := newHookTestManager(hooks)
	defer m.cancel()

	m.markDegraded(time.Now().UnixNano(), DegradeReasonNATSConnectionDown)
	m.state.Store(int32(StateDegraded))

	m.exitDegraded()
	m.wg.Wait()

	require.Nil(t, m.degraded.Load(), "genuine exit must clear the degraded record")
	require.Equal(t, StateStable, m.State(), "genuine exit must transition to Stable")
	require.True(t, fired.Load(), "genuine exit must emit OnStateChanged")
	require.Equal(t, StateDegraded, State(gotFrom.Load()), "OnStateChanged.from must be Degraded")
	require.Equal(t, StateStable, State(gotTo.Load()), "OnStateChanged.to must be Stable")
}
