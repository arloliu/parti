package parti

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/heartbeat"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// TestMonitorCalculatorState_ReconcileRecoversDroppedState verifies the
// Cause-A fix from PR-3 §5.1: a calculator state change that is dropped by
// the subscriber buffer AND remains current at the next reconcile tick is
// projected to the manager within 2 * calculatorStateReconcileInterval.
//
// The reconcile invariant is "eventual projection of the current calculator
// state, NOT replay of completed transitions" — this test asserts the former
// only.
func TestMonitorCalculatorState_ReconcileRecoversDroppedState(t *testing.T) {
	// NOTE: NOT t.Parallel() — this test mutates the package-level
	// calculatorStateReconcileInterval var, which is read concurrently by
	// every other test that starts monitorCalculatorState (e.g.
	// TestMonitorCalculatorState_ReadyChannelEstablishesSubscriptionFirst).
	// Running in parallel triggers a -race finding on that read/write.
	// Shorten the reconcile interval so the test runs deterministically and
	// quickly. Reset on cleanup so other tests are unaffected.
	prev := calculatorStateReconcileInterval
	calculatorStateReconcileInterval = 50 * time.Millisecond
	t.Cleanup(func() { calculatorStateReconcileInterval = prev })

	var (
		mu       sync.Mutex
		recorded [][2]types.State
		recordCh = make(chan struct{}, 16)
		hooksCfg types.Hooks
	)
	hooksCfg = types.Hooks{
		OnStateChanged: func(_ context.Context, from, to types.State) error {
			mu.Lock()
			recorded = append(recorded, [2]types.State{from, to})
			mu.Unlock()
			select {
			case recordCh <- struct{}{}:
			default:
			}

			return nil
		},
	}

	m := &Manager{
		cfg:             Config{DegradedAlert: DegradedAlertConfig{AlertInterval: time.Minute}},
		hooks:           &hooksCfg,
		metrics:         metrics.NewNop(),
		logger:          logging.NewNop(),
		connMonitorStop: make(chan struct{}),
		heartbeat:       heartbeat.NewNop(),
	}
	m.state.Store(int32(StateStable))
	m.startupAssignmentApplied.Store(true)
	m.workerID.Store("worker-test")
	m.assignment.Store(Assignment{})
	m.ctx, m.cancel = context.WithCancel(context.Background())
	t.Cleanup(m.cancel)

	// stateCh stays open but the test never sends on it — simulates a
	// dropped event burst. The reconcile arm must surface the current state
	// via GetState.
	stateCh := make(chan types.CalculatorState, 4)
	calc := &monitorTestCalculator{stateCh: stateCh}
	calc.setState(types.CalcStateIdle)

	readyCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.monitorCalculatorState(calc, readyCh)
	}()
	<-readyCh

	// Drive calculator into Scaling without sending on stateCh: every event
	// is treated as "dropped". Reconcile must observe Scaling within
	// 2 * calculatorStateReconcileInterval.
	calc.setState(types.CalcStateScaling)

	require.Eventually(t, func() bool {
		return m.State() == StateScaling
	}, 5*calculatorStateReconcileInterval, 10*time.Millisecond,
		"reconcile must project current calculator state after a subscriber drop")

	require.Equal(t, StateScaling, m.State())

	// Stop the goroutine cleanly.
	m.cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorCalculatorState did not exit on ctx cancel")
	}
	m.wg.Wait()

	// OnStateChanged recorder must contain exactly one (Stable, Scaling)
	// tuple from the reconcile-driven projection. No replay of any
	// intermediate state — none existed for the dropped burst.
	mu.Lock()
	defer mu.Unlock()
	var got [][2]types.State
	for _, p := range recorded {
		if p[0] == StateStable && p[1] == StateScaling {
			got = append(got, p)
		}
	}
	require.Len(t, got, 1, "exactly one (Stable, Scaling) transition expected; got recorder=%v", recorded)
}

// TestPartitionLifecycle_DrivesManagerStateRebalancing verifies PR-3 §5.2 at
// the Manager surface: when the calculator transitions Idle → Rebalancing → Idle
// (the canonical partition-lifecycle path under the FSM), the Manager's public
// State() reaches StateRebalancing and the OnStateChanged hook fires both
// (Stable, Rebalancing) and (Rebalancing, Stable) tuples.
//
// This complements the calculator-FSM coverage in
// TestPartitionLifecycle_DrivesFSMRebalancing (which exercises the calculator
// state stream end-to-end via real NATS / partition source); this test drives
// the calculator-state channel directly so the manager-side projection is
// proved in isolation.
func TestPartitionLifecycle_DrivesManagerStateRebalancing(t *testing.T) {
	// NOT t.Parallel(): mutates calculatorStateReconcileInterval (see the
	// reconcile test for the same reason).
	prev := calculatorStateReconcileInterval
	calculatorStateReconcileInterval = 1 * time.Hour // disable reconcile drift
	t.Cleanup(func() { calculatorStateReconcileInterval = prev })

	var (
		mu       sync.Mutex
		recorded [][2]types.State
		recordCh = make(chan struct{}, 16)
		hooksCfg types.Hooks
	)
	hooksCfg = types.Hooks{
		OnStateChanged: func(_ context.Context, from, to types.State) error {
			mu.Lock()
			recorded = append(recorded, [2]types.State{from, to})
			mu.Unlock()
			select {
			case recordCh <- struct{}{}:
			default:
			}

			return nil
		},
	}

	m := &Manager{
		cfg:             Config{DegradedAlert: DegradedAlertConfig{AlertInterval: time.Minute}},
		hooks:           &hooksCfg,
		metrics:         metrics.NewNop(),
		logger:          logging.NewNop(),
		connMonitorStop: make(chan struct{}),
		heartbeat:       heartbeat.NewNop(),
	}
	m.state.Store(int32(StateStable))
	m.startupAssignmentApplied.Store(true)
	m.workerID.Store("worker-test")
	m.assignment.Store(Assignment{})
	m.ctx, m.cancel = context.WithCancel(context.Background())
	t.Cleanup(m.cancel)

	stateCh := make(chan types.CalculatorState, 4)
	calc := &monitorTestCalculator{stateCh: stateCh}
	calc.setState(types.CalcStateIdle)

	readyCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.monitorCalculatorState(calc, readyCh)
	}()
	<-readyCh

	// Drive Idle → Rebalancing → Idle through the subscriber channel, as a
	// partition-source change would.
	calc.setState(types.CalcStateRebalancing)
	stateCh <- types.CalcStateRebalancing

	require.Eventually(t, func() bool {
		return m.State() == StateRebalancing
	}, 2*time.Second, 5*time.Millisecond,
		"Manager.State() must reach StateRebalancing after calc emits CalcStateRebalancing")

	calc.setState(types.CalcStateIdle)
	stateCh <- types.CalcStateIdle

	require.Eventually(t, func() bool {
		return m.State() == StateStable
	}, 2*time.Second, 5*time.Millisecond,
		"Manager.State() must return to StateStable after calc emits CalcStateIdle")

	// Stop the monitor cleanly so the recorder snapshot is final.
	m.cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorCalculatorState did not exit on ctx cancel")
	}
	m.wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	var sawEnter, sawExit bool
	for _, p := range recorded {
		if p[0] == StateStable && p[1] == StateRebalancing {
			sawEnter = true
		}
		if p[0] == StateRebalancing && p[1] == StateStable {
			sawExit = true
		}
	}
	require.True(t, sawEnter,
		"OnStateChanged must record (Stable, Rebalancing); got %v", recorded)
	require.True(t, sawExit,
		"OnStateChanged must record (Rebalancing, Stable); got %v", recorded)
}
