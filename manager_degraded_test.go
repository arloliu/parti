package parti

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func TestManager_recordKVError(t *testing.T) {
	logger := logging.NewNop()
	nopMetrics := metrics.NewNop()
	cfg := Config{
		DegradedBehavior: DegradedBehaviorConfig{
			KVErrorThreshold: 3,
			KVErrorWindow:    10 * time.Second,
		},
		DegradedAlert: DegradedAlertConfig{
			AlertInterval: 1 * time.Minute,
		},
	}

	t.Run("ignores nil error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.recordKVError(nil)
		require.Equal(t, int32(0), m.kvErrorCount.Load())
	})

	t.Run("ignores non-connectivity error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.recordKVError(nats.ErrInvalidArg) // Not a connectivity error
		require.Equal(t, int32(0), m.kvErrorCount.Load())
	})

	t.Run("records connectivity error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.recordKVError(nats.ErrTimeout)
		require.Equal(t, int32(1), m.kvErrorCount.Load())
	})

	t.Run("triggers degraded mode", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		// Trigger threshold
		for i := 0; i < 3; i++ {
			m.recordKVError(nats.ErrTimeout)
		}

		require.Equal(t, int32(3), m.kvErrorCount.Load())
		require.NotNil(t, m.degradedSince.Load())
		require.Equal(t, StateDegraded, m.State())
	})

	t.Run("resets on success", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.recordKVError(nats.ErrTimeout)
		require.Equal(t, int32(1), m.kvErrorCount.Load())

		m.recordKVSuccess()
		require.Equal(t, int32(0), m.kvErrorCount.Load())
		require.Empty(t, m.kvErrorWindow)
	})
}

func TestManager_enterDegraded_rejectsShutdown(t *testing.T) {
	logger := logging.NewNop()
	nopMetrics := metrics.NewNop()
	cfg := Config{
		DegradedAlert: DegradedAlertConfig{
			AlertInterval: 1 * time.Minute,
		},
	}

	t.Run("blocks degraded entry from Shutdown state", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.state.Store(int32(StateShutdown))

		m.enterDegraded("test: should be rejected")

		require.Equal(t, StateShutdown, m.State(),
			"enterDegraded must not override StateShutdown")
		require.Nil(t, m.degradedSince.Load(),
			"degradedSince must remain nil when enterDegraded is rejected")
	})

	t.Run("allows degraded entry from Stable state", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.state.Store(int32(StateStable))

		m.enterDegraded("test: should be allowed")

		// Cancel context to stop monitorDegradedAlerts goroutine
		cancel()
		m.wg.Wait()

		require.Equal(t, StateDegraded, m.State())
		require.NotNil(t, m.degradedSince.Load())
	})
}

func TestManager_exitDegraded_safeWithCAS(t *testing.T) {
	logger := logging.NewNop()
	nopMetrics := metrics.NewNop()
	cfg := Config{
		DegradedAlert: DegradedAlertConfig{
			AlertInterval: 1 * time.Minute,
		},
	}

	t.Run("does not overwrite Shutdown with Stable", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		// Simulate: entered degraded mode, then Stop set StateShutdown
		now := time.Now()
		m.degradedSince.Store(&now)
		m.state.Store(int32(StateShutdown))

		m.exitDegraded()

		require.Equal(t, StateShutdown, m.State(),
			"exitDegraded must not overwrite StateShutdown with Stable")
	})

	t.Run("exits normally from Degraded to Stable", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		now := time.Now()
		m.degradedSince.Store(&now)
		m.state.Store(int32(StateDegraded))

		m.exitDegraded()
		m.wg.Wait()

		require.Equal(t, StateStable, m.State())
		require.Nil(t, m.degradedSince.Load())
	})
}
