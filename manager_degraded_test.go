package parti

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
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

	t.Run("records degrading jetstream error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.recordKVError(jetstream.ErrBucketNotFound)
		require.Equal(t, int32(1), m.kvErrorCount.Load())
	})

	t.Run("triggers degraded mode", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.state.Store(int32(StateStable)) // must be a state that allows Degraded entry

		// Trigger threshold
		for range 3 {
			m.recordKVError(nats.ErrTimeout)
		}
		// Cancel to stop the monitorDegradedAlerts goroutine started by enterDegraded
		cancel()
		m.wg.Wait()

		require.Equal(t, int32(3), m.kvErrorCount.Load())
		require.NotZero(t, m.degradedSince.Load())
		require.Equal(t, StateDegraded, m.State())
	})

	t.Run("ignores stream-missing errors", func(t *testing.T) {
		// Cross-feature contract pin: stream-missing exhaustion is
		// routed through the dynamic-consumer observer to
		// enterDegraded("stream-missing-recovery-exhausted"), NOT
		// through the generic KV-error threshold. A stream-missing
		// error that incidentally wraps jetstream.ErrStreamNotFound
		// (which natsutil treats as a degrading-JetStream error) must
		// be short-circuited here so it does not double-count.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		// Wrap mirroring partition_consumer.go's wrapping pattern:
		// types.ErrStreamMissing PLUS the degrading-JetStream cause.
		wrapped := fmt.Errorf("%w: stream %q: %w", types.ErrStreamMissing, "TEST", jetstream.ErrStreamNotFound)
		m.recordKVError(wrapped)
		require.Equal(t, int32(0), m.kvErrorCount.Load(),
			"stream-missing errors must not count against the KV threshold")
		require.Empty(t, m.kvErrorWindow,
			"stream-missing errors must not append to the KV error window")
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
		require.Zero(t, m.degradedSince.Load(),
			"degradedSince must remain zero when enterDegraded is rejected")
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
		require.NotZero(t, m.degradedSince.Load())
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
		m.degradedSince.Store(time.Now().UnixNano())
		m.state.Store(int32(StateShutdown))

		m.exitDegraded()

		require.Equal(t, StateShutdown, m.State(),
			"exitDegraded must not overwrite StateShutdown with Stable")
	})

	t.Run("exits normally from Degraded to Stable", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		m.degradedSince.Store(time.Now().UnixNano())
		m.state.Store(int32(StateDegraded))

		m.exitDegraded()
		m.wg.Wait()

		require.Equal(t, StateStable, m.State())
		require.Zero(t, m.degradedSince.Load())
	})
}

// TestEnterDegraded_ReentryAfterExit verifies that enterDegraded works correctly
// after exitDegraded resets degradedSince to 0. This tests the fix for the
// atomic.Value typed-nil re-entry bug where Store((*time.Time)(nil)) produced a
// non-nil interface, permanently blocking future enterDegraded calls.
func TestEnterDegraded_ReentryAfterExit(t *testing.T) {
	logger := logging.NewNop()
	nopMetrics := metrics.NewNop()
	cfg := Config{
		DegradedAlert: DegradedAlertConfig{
			AlertInterval: 1 * time.Minute,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
	m.state.Store(int32(StateStable))

	// First degraded entry
	m.enterDegraded("first failure")
	cancel()    // stop monitorDegradedAlerts
	m.wg.Wait() // wait for goroutines to exit

	require.Equal(t, StateDegraded, m.State(), "must enter Degraded")
	require.NotZero(t, m.degradedSince.Load())

	// Recovery: exit degraded
	m.exitDegraded()
	require.Equal(t, StateStable, m.State(), "must return to Stable")
	require.Zero(t, m.degradedSince.Load(), "degradedSince must be 0 after exit")

	// Re-entry: must succeed after reset (the core bug fix)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	m.ctx = ctx2
	m.cancel = cancel2

	m.enterDegraded("second failure")
	cancel2()
	m.wg.Wait()

	require.Equal(t, StateDegraded, m.State(), "re-entry into Degraded must work after recovery")
	require.NotZero(t, m.degradedSince.Load(), "degradedSince must be set on re-entry")
}
