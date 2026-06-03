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
		require.NotZero(t, m.degradedSinceNano())
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

// TestManager_recordKVHealthyOp pins the F-D1 healthy-op success-reset: a
// successful periodic KV op while not degraded clears ONLY the transient
// (connected-but-KV-unavailable) error entries, leaving whole-bucket-loss
// entries to accumulate. This is what gives the F-D1 circuit consecutive-error
// semantics without masking a whole-bucket loss when an unaffected bucket keeps
// succeeding.
func TestManager_recordKVHealthyOp(t *testing.T) {
	logger := logging.NewNop()
	nopMetrics := metrics.NewNop()
	cfg := Config{
		DegradedBehavior: DegradedBehaviorConfig{
			// High threshold so recording errors never trips Degraded here —
			// this test isolates the window bookkeeping, not the trip path.
			KVErrorThreshold: 100,
			KVErrorWindow:    10 * time.Second,
		},
		DegradedAlert: DegradedAlertConfig{AlertInterval: 1 * time.Minute},
	}

	t.Run("clears only transient entries", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		m.recordKVError(nats.ErrTimeout)                  // whole-bucket (connectivity)
		m.recordKVOpError(context.DeadlineExceeded)       // transient (F-D1)
		m.recordKVOpError(context.DeadlineExceeded)       // transient (F-D1)
		require.Equal(t, int32(3), m.kvErrorCount.Load()) // sanity

		m.recordKVHealthyOp()

		require.Equal(t, int32(1), m.kvErrorCount.Load(),
			"only the whole-bucket entry must survive a healthy op")
		require.Len(t, m.kvErrorWindow, 1)
		require.False(t, m.kvErrorWindow[0].transient,
			"the surviving entry must be the whole-bucket-loss one")
	})

	t.Run("no-op while degraded", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		// Record BEFORE marking degraded (recordKVError short-circuits while degraded).
		m.recordKVError(nats.ErrTimeout)            // whole-bucket
		m.recordKVOpError(context.DeadlineExceeded) // transient
		require.Equal(t, int32(2), m.kvErrorCount.Load())

		m.markDegraded(time.Now().UnixNano(), DegradeReasonNATSConnectionDown)
		m.recordKVHealthyOp()

		require.Equal(t, int32(2), m.kvErrorCount.Load(),
			"a healthy op while degraded must not touch the window")
		require.Len(t, m.kvErrorWindow, 2)
	})

	t.Run("empty window is a no-op", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		m.recordKVHealthyOp()
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
		require.Zero(t, m.degradedSinceNano(),
			"the degraded record must remain unset when enterDegraded is rejected")
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
		require.NotZero(t, m.degradedSinceNano())
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
		m.markDegraded(time.Now().UnixNano(), DegradeReasonNATSConnectionDown)
		m.state.Store(int32(StateShutdown))

		m.exitDegraded()

		require.Equal(t, StateShutdown, m.State(),
			"exitDegraded must not overwrite StateShutdown with Stable")
	})

	t.Run("exits normally from Degraded to Stable", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		m.markDegraded(time.Now().UnixNano(), DegradeReasonNATSConnectionDown)
		m.state.Store(int32(StateDegraded))

		m.exitDegraded()
		m.wg.Wait()

		require.Equal(t, StateStable, m.State())
		require.Zero(t, m.degradedSinceNano())
	})
}

// TestEnterDegraded_ReentryAfterExit verifies enter -> exit -> re-enter works:
// exitDegraded clears the degraded record (Store(nil)), so a later enterDegraded
// wins CompareAndSwap(nil, rec) and re-arms. This locks the re-entry contract that
// an earlier typed-nil atomic.Value storage bug once broke (Store((*time.Time)(nil))
// produced a non-nil interface that permanently blocked re-entry); the pointer
// record makes "cleared" an honest nil, so re-entry cannot be silently blocked.
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
	require.NotZero(t, m.degradedSinceNano())

	// Recovery: exit degraded
	m.exitDegraded()
	require.Equal(t, StateStable, m.State(), "must return to Stable")
	require.Zero(t, m.degradedSinceNano(), "the degraded record must be cleared after exit")

	// Re-entry: must succeed after reset (the core bug fix)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	m.ctx = ctx2
	m.cancel = cancel2

	m.enterDegraded("second failure")
	cancel2()
	m.wg.Wait()

	require.Equal(t, StateDegraded, m.State(), "re-entry into Degraded must work after recovery")
	require.NotZero(t, m.degradedSinceNano(), "the degraded record must be set on re-entry")
}
