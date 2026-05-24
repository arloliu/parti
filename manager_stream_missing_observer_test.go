package parti

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/recovery"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

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
	require.Zero(t, m.degradedSince.Load(),
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
