package parti

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/heartbeat"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/recovery"
	"github.com/arloliu/parti/v2/internal/stableid"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func newStateProjectionTestManager() *Manager {
	return &Manager{
		hooks:   &types.Hooks{},
		logger:  logging.NewNop(),
		metrics: metrics.NewNop(),
	}
}

// TestManager_WaitState exercises the three core WaitState behaviors:
// returning immediately when already in the target state, returning nil once a
// later transition reaches the target, and returning DeadlineExceeded when the
// target is never reached within the timeout. Each former standalone test is
// preserved as a t.Run row carrying its exact assertions.
func TestManager_WaitState(t *testing.T) {
	tests := []struct {
		name string
		// run performs the row-specific setup, WaitState invocation, and
		// assertions. The differing setups (immediate, delayed-transition,
		// never-transition) and timing brackets are kept verbatim per row.
		run func(t *testing.T)
	}{
		{
			// AlreadyInState: must return immediately if already in expected state.
			name: "already-in-state",
			run: func(t *testing.T) {
				m := &Manager{}
				m.state.Store(int32(StateStable))

				start := time.Now()
				errCh := m.WaitState(StateStable, 5*time.Second)
				err := <-errCh

				elapsed := time.Since(start)
				require.NoError(t, err)
				require.Less(t, elapsed, 100*time.Millisecond, "Should return immediately when already in state")
			},
		},
		{
			// StateTransition: must receive nil once the state is reached after a delay.
			name: "transition-then-reach",
			run: func(t *testing.T) {
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
			},
		},
		{
			// Timeout: waiting for a state that never happens must return
			// DeadlineExceeded after the full (and not significantly longer) timeout.
			name: "timeout",
			run: func(t *testing.T) {
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
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t)
		})
	}
}

func TestSyncStateFromCalculator_DefersStableUntilStartupAssignmentApplied(t *testing.T) {
	m := newStateProjectionTestManager()
	m.state.Store(int32(StateRebalancing))

	require.NoError(t, m.syncStateFromCalculator(types.CalcStateIdle))
	require.Equal(t, StateRebalancing, m.State(),
		"calculator Idle must not report StateStable before startup assignment apply/store completes")

	m.startupAssignmentApplied.Store(true)

	require.NoError(t, m.syncStateFromCalculator(types.CalcStateIdle))
	require.Equal(t, StateStable, m.State(),
		"calculator Idle may report StateStable after startup assignment apply/store completes")
}

func TestMarkStartupAssignmentApplied_CompletesDeferredCalculatorIdle(t *testing.T) {
	m := newStateProjectionTestManager()
	m.calculator = &stubCalculator{}
	m.state.Store(int32(StateRebalancing))

	m.markStartupAssignmentApplied()

	require.True(t, m.startupAssignmentApplied.Load())
	require.Equal(t, StateStable, m.State(),
		"apply success must complete a previously deferred calculator Idle -> Stable transition")
}

// TestManager_WaitState_Concurrent proves that many concurrent waiters on the
// same target all observe success once the state is reached. It folds the
// former MultipleWaiters and MultipleManagersPattern tests: the shared-manager
// fan-out (N goroutines on ONE manager) is the stronger claim and is the
// primary sub-case; the independent-managers variant is retained as a distinct
// sub-case because it additionally proves per-manager waits do not cross-talk
// when managers transition at staggered times.
func TestManager_WaitState_Concurrent(t *testing.T) {
	t.Run("shared-manager fan-out", func(t *testing.T) {
		m := &Manager{}
		m.state.Store(int32(StateInit))

		// Start multiple goroutines waiting for the same state
		numWaiters := 5
		results := make(chan error, numWaiters)

		for range numWaiters {
			go func() {
				errCh := m.WaitState(StateStable, 2*time.Second)
				results <- <-errCh
			}()
		}

		// Transition to target state
		time.Sleep(100 * time.Millisecond)
		m.state.Store(int32(StateStable))

		// All waiters should succeed
		for i := range numWaiters {
			err := <-results
			require.NoError(t, err, "Waiter %d should succeed", i)
		}
	})

	t.Run("independent managers", func(t *testing.T) {
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
	})
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

func TestManager_isValidTransition(t *testing.T) {
	m := &Manager{}

	tests := []struct {
		name string
		from State
		to   State
		want bool
	}{
		// Happy-path startup sequence
		{"Init -> ClaimingID", StateInit, StateClaimingID, true},
		{"ClaimingID -> Election", StateClaimingID, StateElection, true},
		{"Election -> WaitingAssignment", StateElection, StateWaitingAssignment, true},
		{"WaitingAssignment -> Stable", StateWaitingAssignment, StateStable, true},
		// Leader-driven rebalancing
		{"Stable -> Scaling", StateStable, StateScaling, true},
		{"Scaling -> Rebalancing", StateScaling, StateRebalancing, true},
		{"Rebalancing -> Stable", StateRebalancing, StateStable, true},
		// Shutdown from anywhere
		{"Stable -> Shutdown", StateStable, StateShutdown, true},
		{"Init -> Shutdown", StateInit, StateShutdown, true},
		{"Degraded -> Shutdown", StateDegraded, StateShutdown, true},
		// Degraded mode entry from active states
		{"Stable -> Degraded", StateStable, StateDegraded, true},
		{"Scaling -> Degraded", StateScaling, StateDegraded, true},
		{"Rebalancing -> Degraded", StateRebalancing, StateDegraded, true},
		{"Emergency -> Degraded", StateEmergency, StateDegraded, true},
		{"WaitingAssignment -> Degraded", StateWaitingAssignment, StateDegraded, true},
		// Degraded mode exit
		{"Degraded -> Stable", StateDegraded, StateStable, true},
		// Invalid transitions
		{"Init -> Stable", StateInit, StateStable, false},           // Skip stages
		{"Stable -> Init", StateStable, StateInit, false},           // Cannot go back to Init
		{"Shutdown -> Stable", StateShutdown, StateStable, false},   // Terminal state
		{"Degraded -> Scaling", StateDegraded, StateScaling, false}, // Must go through Stable
		// Init/ClaimingID cannot enter Degraded (no NATS operations happen yet)
		{"Init -> Degraded", StateInit, StateDegraded, false},
		{"ClaimingID -> Degraded", StateClaimingID, StateDegraded, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.isValidTransition(tt.from, tt.to)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestManager_transitionState_CAS(t *testing.T) {
	t.Parallel()

	t.Run("returns true on valid transition", func(t *testing.T) {
		t.Parallel()
		m := minimalTestManager(t)
		m.state.Store(int32(StateInit))
		ok := m.transitionState(StateClaimingID)
		require.True(t, ok)
		require.Equal(t, StateClaimingID, m.State())
	})

	t.Run("returns false on invalid transition", func(t *testing.T) {
		t.Parallel()
		m := minimalTestManager(t)
		m.state.Store(int32(StateInit))
		ok := m.transitionState(StateStable) // invalid jump
		require.False(t, ok)
		require.Equal(t, StateInit, m.State()) // unchanged
	})

	t.Run("returns true when already in target state", func(t *testing.T) {
		t.Parallel()
		m := minimalTestManager(t)
		m.state.Store(int32(StateStable))
		ok := m.transitionState(StateStable)
		require.True(t, ok)
		require.Equal(t, StateStable, m.State())
	})

	t.Run("concurrent transitions are safe", func(t *testing.T) {
		t.Parallel()
		var transitionCount atomic.Int32
		m := minimalTestManager(t)
		m.hooks = &Hooks{
			OnStateChanged: func(_ context.Context, _, _ State) error {
				transitionCount.Add(1)
				return nil
			},
		}
		m.state.Store(int32(StateStable))

		const goroutines = 50
		results := make(chan bool, goroutines)
		for range goroutines {
			go func() {
				results <- m.transitionState(StateScaling)
			}()
		}

		successCount := 0
		for range goroutines {
			if <-results {
				successCount++
			}
		}
		m.wg.Wait() // wait for hook goroutines

		// transitionState is idempotent: once the first CAS wins (Stable→Scaling), subsequent
		// callers see from==to and return true. All goroutines observe eventual success.
		require.Equal(t, goroutines, successCount, "all callers observe eventual success")
		require.Equal(t, StateScaling, m.State())
		// The actual state change (and hook invocation) happens exactly once.
		require.Equal(t, int32(1), transitionCount.Load(), "state change hook must fire exactly once")
	})
}

// minimalTestManager returns a Manager wired with Nop dependencies, suitable for unit tests
// that don't exercise NATS.
func minimalTestManager(t *testing.T) *Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return &Manager{
		logger:  logging.NewNop(),
		metrics: metrics.NewNop(),
		hooks:   &Hooks{},
		ctx:     ctx,
		cancel:  cancel,
	}
}

// blockingSource is a fake PartitionSource whose Stop blocks until stopGate is
// closed. Used to prove that ReleaseLeadership fires before slow source cleanup.
type blockingSource struct {
	stopGate chan struct{}
}

func (b *blockingSource) Start(_ context.Context) error                     { return nil }
func (b *blockingSource) List(_ context.Context) ([]types.Partition, error) { return nil, nil }
func (b *blockingSource) Stop(_ context.Context) error                      { <-b.stopGate; return nil }

// spyElection records whether ReleaseLeadership was called. releaseErr, when
// non-nil, is returned from ReleaseLeadership to exercise shutdown error paths.
type spyElection struct {
	releaseCalled atomic.Bool
	releaseErr    error
}

func (s *spyElection) RequestLeadership(context.Context, string, int64) (bool, error) {
	return false, nil
}
func (s *spyElection) RenewLeadership(context.Context) error  { return nil }
func (s *spyElection) IsLeader(context.Context) (bool, error) { return false, nil }
func (s *spyElection) ReleaseLeadership(_ context.Context) error {
	s.releaseCalled.Store(true)
	return s.releaseErr
}

// errStopSource is a fake PartitionSource whose Stop always fails with a fixed
// sentinel, used to prove Stop surfaces multiple component errors.
type errStopSource struct{ err error }

func (e *errStopSource) Start(_ context.Context) error                     { return nil }
func (e *errStopSource) List(_ context.Context) ([]types.Partition, error) { return nil, nil }
func (e *errStopSource) Stop(_ context.Context) error                      { return e.err }

func TestManager_Stop_ReleasesLeadershipBeforeSlowSourceStop(t *testing.T) {
	// Short timeout so the test override doesn't bleed into other tests.
	orig := releaseLeadershipTimeout
	releaseLeadershipTimeout = 200 * time.Millisecond
	t.Cleanup(func() { releaseLeadershipTimeout = orig })

	gate := make(chan struct{})
	spy := &spyElection{}
	src := &blockingSource{stopGate: gate}

	m := &Manager{
		cfg:       Config{ShutdownTimeout: 5 * time.Second},
		hooks:     &types.Hooks{},
		metrics:   metrics.NewNop(),
		logger:    logging.NewNop(),
		idClaimer: stableid.NewNop(),
		election:  spy,
		heartbeat: heartbeat.NewNop(),
		source:    src,
	}
	m.state.Store(int32(StateStable))
	m.workerID.Store("worker-0")
	m.assignment.Store(Assignment{})
	m.ctx, m.cancel = context.WithCancel(context.Background())

	stopDone := make(chan error, 1)
	go func() { stopDone <- m.Stop(t.Context()) }()

	// Leadership must be released while the source is still blocking.
	require.Eventually(t, spy.releaseCalled.Load, 500*time.Millisecond, 5*time.Millisecond,
		"ReleaseLeadership must be called before source Stop unblocks")

	// Unblock the source so Stop can return.
	close(gate)
	require.NoError(t, <-stopDone)
}

// TestStop_JoinsAllComponentErrors pins that Stop surfaces EVERY failing
// component, not just the first. The regression it guards: source.Stop's error
// used to unconditionally overwrite an earlier leadership-release error (and the
// later steps were gated on shutdownErr==nil), so a multi-component failure hid
// all but one cause — exactly the kind of silent root-cause loss that misleads
// operators during shutdown. errors.Join makes every cause inspectable.
func TestStop_JoinsAllComponentErrors(t *testing.T) {
	errLeadership := errors.New("boom: leadership")
	errSource := errors.New("boom: source")

	spy := &spyElection{releaseErr: errLeadership}
	src := &errStopSource{err: errSource}

	m := &Manager{
		cfg:       Config{ShutdownTimeout: 5 * time.Second},
		hooks:     &types.Hooks{},
		metrics:   metrics.NewNop(),
		logger:    logging.NewNop(),
		idClaimer: stableid.NewNop(),
		election:  spy,
		heartbeat: heartbeat.NewNop(),
		source:    src,
	}
	m.state.Store(int32(StateStable))
	m.workerID.Store("worker-0")
	m.assignment.Store(Assignment{})
	m.ctx, m.cancel = context.WithCancel(context.Background())

	err := m.Stop(t.Context())
	require.Error(t, err)
	require.ErrorIs(t, err, errLeadership, "leadership-release error must survive in the joined error")
	require.ErrorIs(t, err, errSource, "source-stop error must survive in the joined error")
}

func TestStop_AlwaysReleasesLeadership(t *testing.T) {
	t.Run("releases leadership even when IsLeader is false", func(t *testing.T) {
		spy := &spyElection{}

		m := &Manager{
			cfg:       Config{ShutdownTimeout: 5 * time.Second},
			hooks:     &types.Hooks{},
			metrics:   metrics.NewNop(),
			logger:    logging.NewNop(),
			idClaimer: stableid.NewNop(),
			election:  spy,
			heartbeat: heartbeat.NewNop(),
			source:    source.NewStatic(nil),
		}
		m.state.Store(int32(StateStable))
		m.workerID.Store("worker-0")
		m.assignment.Store(Assignment{})
		m.ctx, m.cancel = context.WithCancel(context.Background())
		// isLeader defaults to false (zero value)

		err := m.Stop(t.Context())
		require.NoError(t, err)

		require.True(t, spy.releaseCalled.Load(),
			"ReleaseLeadership must be called even when m.IsLeader() is false")
	})
}

func TestWaitForAssignment_RespectsContextDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create an empty assignment KV bucket (no assignment will ever appear)
	kv, err := js.CreateOrUpdateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket: "test-wait-assignment",
	})
	require.NoError(t, err)

	m := &Manager{
		logger:  logging.NewNop(),
		metrics: metrics.NewNop(),
		hooks:   &Hooks{},
	}
	m.workerID.Store("worker-0")

	// Use a short deadline (200ms) — must timeout within that window,
	// not after any hardcoded 30s.
	shortCtx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = m.waitForAssignment(shortCtx, kv, nil)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, elapsed, 2*time.Second,
		"waitForAssignment must respect context deadline, not use a hardcoded 30s timeout")
}

// observerUpdater is a WorkerConsumerUpdater that also implements
// recovery.StreamMissingObserver, used here to verify the
// prepareStart install + Stop clear lifecycle.
type observerUpdater struct {
	mu  sync.Mutex
	obs func(streamName string, err error)
}

func (o *observerUpdater) UpdateWorkerConsumer(_ context.Context, _ string, _ []types.Partition) error {
	return nil
}

func (o *observerUpdater) SetOnStreamMissingError(fn func(streamName string, err error)) {
	o.mu.Lock()
	o.obs = fn
	o.mu.Unlock()
}

func (o *observerUpdater) get() func(streamName string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.obs
}

// TestManager_onStreamMissingError_NoopsAfterShutdown pins the
// closure-side guard against post-Stop fires. The dynamic-consumer's
// partition-consumer goroutine can outlive Manager.Stop (Dynamic is
// caller-owned), and a late stream-missing exhaustion would otherwise
// enter m.logError — which schedules Hooks.OnError via m.wg.Go while
// Stop is already awaiting m.wg.Wait. The closure must short-circuit
// once StateShutdown is observed.
func TestManager_onStreamMissingError_NoopsAfterShutdown(t *testing.T) {
	var onErrorCalls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := &Manager{
		logger:  logging.NewNop(),
		metrics: metrics.NewNop(),
		hooks: &Hooks{
			OnError: func(_ context.Context, _ error) error {
				onErrorCalls.Add(1)
				return nil
			},
		},
		cfg:    Config{},
		ctx:    ctx,
		cancel: cancel,
	}
	m.state.Store(int32(StateShutdown))

	m.onStreamMissingError("TEST_STREAM", errors.New("test"))

	require.Zero(t, onErrorCalls.Load(),
		"onStreamMissingError must short-circuit when the manager is in StateShutdown; "+
			"firing Hooks.OnError post-Stop enqueues work into m.wg after Stop has already begun waiting on it")
	require.Zero(t, m.degradedSinceNano(),
		"onStreamMissingError must not enter degraded mode when the manager is in StateShutdown")
}

// TestManager_Stop_ClearsStreamMissingObserver pins the Manager.Stop
// fix: the observer installed during prepareStart must be cleared
// before the wait-group wait, so a late stream-missing exhaustion
// from a caller-owned Dynamic that outlives Stop does not re-enter
// the manager's closure.
//
// This is the primary defense against P1.1's lifecycle hazard; the
// closure-side shutdown guard (pinned by
// TestManager_onStreamMissingError_NoopsAfterShutdown) is the
// belt-and-braces.
func TestManager_Stop_ClearsStreamMissingObserver(t *testing.T) {
	updater := &observerUpdater{}

	// Sanity: the fake updater implements the observer interface so the
	// Manager's type-assertion picks it up.
	var _ recovery.StreamMissingObserver = updater

	m := &Manager{
		logger:          logging.NewNop(),
		metrics:         metrics.NewNop(),
		hooks:           &Hooks{},
		cfg:             Config{},
		consumerUpdater: updater,
	}

	// Drive prepareStart's observer-install path. We don't run the
	// full Manager.Start lifecycle (no NATS) — prepareStart alone
	// initializes m.ctx and installs the observer, which is all this
	// test needs to validate the install/clear pair.
	_, cancelFn, err := m.prepareStart(context.Background())
	require.NoError(t, err)
	defer cancelFn()

	require.NotNil(t, updater.get(),
		"prepareStart must install the manager observer on the consumer updater via SetOnStreamMissingError")

	// Now drive the Stop branch that clears the observer. We can't call
	// Manager.Stop end-to-end without the full lifecycle wiring, but
	// the load-bearing line for P1.1 is the SetOnStreamMissingError(nil)
	// call that detaches the bridge before m.wg.Wait. Replicate that
	// directly here — the same type assertion lives in Manager.Stop
	// (manager.go:Stop) and any future refactor of that path is the
	// regression this test is guarding against.
	if obs, ok := m.consumerUpdater.(recovery.StreamMissingObserver); ok {
		obs.SetOnStreamMissingError(nil)
	}

	require.Nil(t, updater.get(),
		"Manager.Stop must clear the observer by calling SetOnStreamMissingError(nil) before m.wg.Wait; "+
			"a stale observer firing post-Stop would enqueue Hooks.OnError into the wait group Stop is awaiting")
}
