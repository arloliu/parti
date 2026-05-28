package recovery

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// --- mock metrics ---

type testMetrics struct {
	iterRestartReasons []string
	attemptReasons     []string
	results            []string
	resultReasons      []string
	durations          []float64
}

func (m *testMetrics) IncrementWorkerConsumerControlRetry(string)       {}
func (m *testMetrics) RecordWorkerConsumerRetryBackoff(string, float64) {}
func (m *testMetrics) SetWorkerConsumerSubjectsCurrent(int)             {}
func (m *testMetrics) IncrementWorkerConsumerSubjectChange(string, int) {}
func (m *testMetrics) IncrementWorkerConsumerGuardrailViolation(string) {}
func (m *testMetrics) IncrementWorkerConsumerSubjectThresholdWarning()  {}
func (m *testMetrics) RecordWorkerConsumerUpdate(string)                {}
func (m *testMetrics) ObserveWorkerConsumerUpdateLatency(float64)       {}
func (m *testMetrics) IncrementWorkerConsumerIteratorEscalation(string) {}
func (m *testMetrics) SetWorkerConsumerConsecutiveIteratorFailures(int) {}
func (m *testMetrics) SetWorkerConsumerHealthStatus(bool)               {}
func (m *testMetrics) IncrementWorkerConsumerPullSuppressed(string)     {}
func (m *testMetrics) IncrementWorkerConsumerIteratorRestart(reason string) {
	m.iterRestartReasons = append(m.iterRestartReasons, reason)
}
func (m *testMetrics) IncrementWorkerConsumerRecreationAttempt(reason string) {
	m.attemptReasons = append(m.attemptReasons, reason)
}
func (m *testMetrics) RecordWorkerConsumerRecreation(result string, reason string) {
	m.results = append(m.results, result)
	m.resultReasons = append(m.resultReasons, reason)
}
func (m *testMetrics) ObserveWorkerConsumerRecreationDuration(seconds float64) {
	m.durations = append(m.durations, seconds)
}

var nopLog = logging.NewNop()

// --- stubs ---

type stubConsumer struct{ jetstream.Consumer }

func alwaysSucceedRecreate(_ context.Context, _ jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	return &stubConsumer{}, nil
}

func alwaysFailRecreate(_ context.Context, _ jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	return nil, errors.New("recreate failed")
}

func alwaysNotFoundInfo(_ context.Context) (*jetstream.ConsumerInfo, error) {
	return nil, jetstream.ErrConsumerNotFound
}

func alwaysSuccessInfo(_ context.Context) (*jetstream.ConsumerInfo, error) {
	return &jetstream.ConsumerInfo{AckFloor: jetstream.SequenceInfo{Stream: 42}}, nil
}

var baseCfg = jetstream.ConsumerConfig{Durable: "test"}

// --- tests ---

func TestNewController_DisabledReturnsNil(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: Disabled})
	require.Nil(t, c)
}

func TestNilController_SafeToCall(t *testing.T) {
	var c *Controller
	require.Equal(t, Disabled, c.Strategy())

	action, cons, _ := c.Classify(context.Background(), jetstream.ErrConsumerDeleted, nil, baseCfg, nil)
	require.Equal(t, ActionBackoff, action)
	require.Nil(t, cons)

	c.AdvanceCheckpoint(&mockMsg{seq: 10}, 0)
	c.SeedCheckpoint(context.Background(), nil)
	c.ResetBurst()
}

func TestClassify_GracefulExit(t *testing.T) {
	c := NewController(ControllerConfig{
		Strategy: FromNew,
		Logger:   nopLog,
	})

	action, _, _ := c.Classify(context.Background(), jetstream.ErrMsgIteratorClosed, nil, baseCfg, nil)
	require.Equal(t, ActionExit, action)

	action, _, _ = c.Classify(context.Background(), context.Canceled, nil, baseCfg, nil)
	require.Equal(t, ActionExit, action)

	action, _, _ = c.Classify(context.Background(), nil, nil, baseCfg, nil)
	require.Equal(t, ActionExit, action)
}

func TestClassify_ConsumerDeleted_RecoverySucceeds(t *testing.T) {
	metrics := &testMetrics{}
	c := NewController(ControllerConfig{
		Strategy: FromNew,
		Logger:   nopLog,
		Metrics:  metrics,
	})

	action, newCons, _ := c.Classify(context.Background(), jetstream.ErrConsumerDeleted, nil, baseCfg, alwaysSucceedRecreate)
	require.Equal(t, ActionContinue, action)
	require.NotNil(t, newCons)
	require.Equal(t, []string{"consumer_deleted"}, metrics.iterRestartReasons)
	require.Equal(t, []string{"success"}, metrics.results)
}

// TestClassify_ConsumerDeleted_RecreateStreamNotFound pins the P2.3 wire
// contract: when the recreate function returns a stream-not-found error,
// Classify returns ActionStreamMissing with a non-nil error wrapping
// types.ErrStreamMissing — and does NOT advance internal recovery state
// (lastRecoveryTime stays zero so the caller's detour can run immediately).
func TestClassify_ConsumerDeleted_RecreateStreamNotFound(t *testing.T) {
	metrics := &testMetrics{}
	c := NewController(ControllerConfig{
		Strategy: FromNew,
		Logger:   nopLog,
		Metrics:  metrics,
	})

	streamNotFound := func(_ context.Context, _ jetstream.ConsumerConfig) (jetstream.Consumer, error) {
		return nil, jetstream.ErrStreamNotFound
	}

	action, newCons, err := c.Classify(context.Background(), jetstream.ErrConsumerDeleted, nil, baseCfg, streamNotFound)
	require.Equal(t, ActionStreamMissing, action)
	require.Nil(t, newCons)
	require.ErrorIs(t, err, types.ErrStreamMissing,
		"stream-not-found recreate error must surface as wrapped types.ErrStreamMissing")
	require.ErrorIs(t, err, jetstream.ErrStreamNotFound,
		"original cause must remain in the wrap chain so the operator can diagnose")

	// State must NOT advance — the detour relies on lastRecoveryTime
	// being unset so its immediate post-hook RebuildAfterStreamRecreated
	// is not throttled by the cooldown.
	require.True(t, c.lastRecoveryTime.IsZero(), "stream-missing must not advance lastRecoveryTime")
	require.Equal(t, []string{"failure"}, metrics.results, "metric still records the failed attempt")
}

func TestClassify_ConsumerDeleted_RecoveryFails(t *testing.T) {
	metrics := &testMetrics{}
	c := NewController(ControllerConfig{
		Strategy: FromNew,
		Logger:   nopLog,
		Metrics:  metrics,
	})

	action, newCons, _ := c.Classify(context.Background(), jetstream.ErrConsumerDeleted, nil, baseCfg, alwaysFailRecreate)
	require.Equal(t, ActionBackoff, action)
	require.Nil(t, newCons)
	require.Equal(t, []string{"failure"}, metrics.results)
}

func TestClassify_NoHeartbeat_BelowThreshold(t *testing.T) {
	metrics := &testMetrics{}
	c := NewController(ControllerConfig{
		Strategy:       FromNew,
		BurstThreshold: 3,
		BurstWindow:    10 * time.Second,
		Logger:         nopLog,
		Metrics:        metrics,
	})

	// 2 heartbeat errors — below threshold of 3
	action, _, _ := c.Classify(context.Background(), jetstream.ErrNoHeartbeat, nil, baseCfg, nil)
	require.Equal(t, ActionBackoff, action)
	action, _, _ = c.Classify(context.Background(), jetstream.ErrNoHeartbeat, nil, baseCfg, nil)
	require.Equal(t, ActionBackoff, action)

	require.Equal(t, []string{"heartbeat", "heartbeat"}, metrics.iterRestartReasons)
	require.Empty(t, metrics.attemptReasons, "no recovery attempt yet")
}

func TestClassify_NoHeartbeat_BurstConfirmedGone(t *testing.T) {
	metrics := &testMetrics{}
	c := NewController(ControllerConfig{
		Strategy:       FromNew,
		BurstThreshold: 2,
		BurstWindow:    10 * time.Second,
		Logger:         nopLog,
		Metrics:        metrics,
	})

	// 1st: below threshold
	action, _, _ := c.Classify(context.Background(), jetstream.ErrNoHeartbeat, alwaysNotFoundInfo, baseCfg, alwaysSucceedRecreate)
	require.Equal(t, ActionBackoff, action)

	// 2nd: threshold reached, Info() confirms gone, recovery succeeds
	action, newCons, _ := c.Classify(context.Background(), jetstream.ErrNoHeartbeat, alwaysNotFoundInfo, baseCfg, alwaysSucceedRecreate)
	require.Equal(t, ActionContinue, action)
	require.NotNil(t, newCons)
	require.Equal(t, []string{"success"}, metrics.results)
}

func TestClassify_NoHeartbeat_BurstButConsumerStillExists(t *testing.T) {
	c := NewController(ControllerConfig{
		Strategy:       FromNew,
		BurstThreshold: 2,
		BurstWindow:    10 * time.Second,
		Logger:         nopLog,
	})

	_, _, _ = c.Classify(context.Background(), jetstream.ErrNoHeartbeat, alwaysSuccessInfo, baseCfg, nil)
	// 2nd: threshold reached, but Info() says consumer exists — no recovery
	action, newCons, _ := c.Classify(context.Background(), jetstream.ErrNoHeartbeat, alwaysSuccessInfo, baseCfg, nil)
	require.Equal(t, ActionBackoff, action)
	require.Nil(t, newCons)
}

func TestController_UnservableDetection(t *testing.T) {
	ctx := context.Background()
	unservableInfo := func(_ context.Context) (*jetstream.ConsumerInfo, error) {
		return nil, nats.ErrNoResponders
	}
	healthyInfo := func(_ context.Context) (*jetstream.ConsumerInfo, error) {
		return &jetstream.ConsumerInfo{Cluster: &jetstream.ClusterInfo{Leader: "n0"}}, nil
	}
	degradingInfo := func(_ context.Context) (*jetstream.ConsumerInfo, error) {
		return nil, jetstream.ErrStreamNotFound
	}

	t.Run("fires after sustained window, re-arms on recovery", func(t *testing.T) {
		var mu sync.Mutex
		var fires int
		c := NewController(ControllerConfig{
			Strategy: FromNew, BurstThreshold: 1, BurstWindow: time.Second,
			Subject: "sub.A", UnservableWindow: 40 * time.Millisecond,
			OnUnservable: func(_ string, _ error) { mu.Lock(); fires++; mu.Unlock() },
			Logger:       nopLog,
		})
		count := func() int { mu.Lock(); defer mu.Unlock(); return fires }

		// First unservable confirm starts the episode but is not yet sustained.
		_, _, _ = c.Classify(ctx, jetstream.ErrNoHeartbeat, unservableInfo, baseCfg, nil)
		require.Equal(t, 0, count())

		time.Sleep(60 * time.Millisecond)
		_, _, _ = c.Classify(ctx, jetstream.ErrNoHeartbeat, unservableInfo, baseCfg, nil)
		require.Equal(t, 1, count(), "fires once sustained past the window")

		// Immediate re-confirm: no re-fire within the window.
		_, _, _ = c.Classify(ctx, jetstream.ErrNoHeartbeat, unservableInfo, baseCfg, nil)
		require.Equal(t, 1, count())

		// Healthy confirm clears the episode (recovered + re-arm).
		_, _, _ = c.Classify(ctx, jetstream.ErrNoHeartbeat, healthyInfo, baseCfg, nil)
		// New unservable confirm restarts the episode; not yet sustained → no new fire.
		_, _, _ = c.Classify(ctx, jetstream.ErrNoHeartbeat, unservableInfo, baseCfg, nil)
		require.Equal(t, 1, count(), "episode re-armed after recovery; not sustained yet")
	})

	t.Run("degrading does not fire (manager owns it)", func(t *testing.T) {
		var n atomic.Int64
		c := NewController(ControllerConfig{
			Strategy: FromNew, BurstThreshold: 1, BurstWindow: time.Second,
			Subject: "sub.B", UnservableWindow: 20 * time.Millisecond,
			OnUnservable: func(string, error) { n.Add(1) }, Logger: nopLog,
		})
		_, _, _ = c.Classify(ctx, jetstream.ErrNoHeartbeat, degradingInfo, baseCfg, nil)
		time.Sleep(40 * time.Millisecond)
		_, _, _ = c.Classify(ctx, jetstream.ErrNoHeartbeat, degradingInfo, baseCfg, nil)
		require.Equal(t, int64(0), n.Load(), "stream-missing/degrading is owned by the manager, not unservable")
	})

	t.Run("opt-out when no hook is set", func(t *testing.T) {
		c := NewController(ControllerConfig{
			Strategy: FromNew, BurstThreshold: 1, BurstWindow: time.Second, Logger: nopLog,
		})
		action, _, _ := c.Classify(ctx, jetstream.ErrNoHeartbeat, unservableInfo, baseCfg, nil)
		require.Equal(t, ActionBackoff, action) // no panic, no detection
	})

	t.Run("NoteProgress clears the episode", func(t *testing.T) {
		var n atomic.Int64
		c := NewController(ControllerConfig{
			Strategy: FromNew, BurstThreshold: 1, BurstWindow: time.Second,
			Subject: "sub.C", UnservableWindow: 40 * time.Millisecond,
			OnUnservable: func(string, error) { n.Add(1) }, Logger: nopLog,
		})
		_, _, _ = c.Classify(ctx, jetstream.ErrNoHeartbeat, unservableInfo, baseCfg, nil)
		c.NoteProgress() // a delivery proves serviceability before the window elapses
		time.Sleep(60 * time.Millisecond)
		_, _, _ = c.Classify(ctx, jetstream.ErrNoHeartbeat, unservableInfo, baseCfg, nil) // episode restarts
		require.Equal(t, int64(0), n.Load(), "NoteProgress reset the episode so the window restarts")
	})
}

func TestClassify_TransientError(t *testing.T) {
	metrics := &testMetrics{}
	c := NewController(ControllerConfig{
		Strategy: FromNew,
		Logger:   nopLog,
		Metrics:  metrics,
	})

	action, _, _ := c.Classify(context.Background(), errors.New("something"), nil, baseCfg, nil)
	require.Equal(t, ActionBackoff, action)
	require.Equal(t, []string{"transient"}, metrics.iterRestartReasons)
}

func TestClassify_CancelledContext_NoRecovery(t *testing.T) {
	c := NewController(ControllerConfig{
		Strategy: FromNew,
		Logger:   nopLog,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	action, _, _ := c.Classify(ctx, jetstream.ErrConsumerDeleted, nil, baseCfg, alwaysSucceedRecreate)
	// Recovery should fail because ctx is cancelled
	require.Equal(t, ActionBackoff, action)
}

func TestSeedCheckpoint(t *testing.T) {
	c := NewController(ControllerConfig{
		Strategy: FromLastProcessed,
		Logger:   nopLog,
	})

	c.SeedCheckpoint(context.Background(), alwaysSuccessInfo)
	require.Equal(t, uint64(42), c.checkpoint.Value())
}

func TestSeedCheckpoint_SkippedWhenNotLastProcessed(t *testing.T) {
	c := NewController(ControllerConfig{
		Strategy: FromNew,
		Logger:   nopLog,
	})

	c.SeedCheckpoint(context.Background(), alwaysSuccessInfo)
	require.Equal(t, uint64(0), c.checkpoint.Value())
}

func TestRecovery_Serialization(t *testing.T) {
	metrics := &testMetrics{}
	c := NewController(ControllerConfig{
		Strategy: FromNew,
		Logger:   nopLog,
		Metrics:  metrics,
	})

	// Simulate in-progress recovery by setting the flag.
	c.inProgress.Store(true)

	action, newCons, _ := c.Classify(context.Background(), jetstream.ErrConsumerDeleted, nil, baseCfg, alwaysSucceedRecreate)
	// Recovery should be skipped because another is in progress.
	require.Equal(t, ActionBackoff, action)
	require.Nil(t, newCons)

	c.inProgress.Store(false)
}

// TestController_Classify_Concurrent verifies that concurrent Classify calls
// never run more than one recovery at a time. The inProgress CAS must serialise
// the recreate function so that at most one goroutine executes it simultaneously.
func TestController_Classify_Concurrent(t *testing.T) {
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32

	recreate := func(_ context.Context, _ jetstream.ConsumerConfig) (jetstream.Consumer, error) { //nolint:unparam
		cur := inFlight.Add(1)
		// Track the high-water mark with a CAS loop.
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond) // hold the slot briefly to expose races
		inFlight.Add(-1)

		return &stubConsumer{}, nil
	}

	c := NewController(ControllerConfig{
		Strategy: FromNew,
		Logger:   nopLog,
	})
	c.minRecoveryInterval = 0

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()
			_, _, _ = c.Classify(context.Background(), jetstream.ErrConsumerDeleted, nil, baseCfg, recreate)
		}()
	}

	wg.Wait()

	// inProgress CAS ensures at most one recreation runs concurrently.
	require.Equal(t, int32(1), maxInFlight.Load(), "at most one recovery may run concurrently")
}

// TestController_AdvanceAndClassify_Concurrent verifies that concurrent
// AdvanceCheckpoint calls (from the message-processing goroutine) and
// Classify calls (from the loop goroutine) do not race on checkpoint state.
// The race detector must not report any violations.
func TestController_AdvanceAndClassify_Concurrent(t *testing.T) {
	c := NewController(ControllerConfig{
		Strategy: FromLastProcessed,
		Logger:   nopLog,
	})
	c.minRecoveryInterval = 0

	ctx := t.Context()

	var wg sync.WaitGroup

	// Goroutine 1: advance checkpoint continuously (simulates auto-ack path).
	wg.Go(func() {
		for i := range 500 {
			c.AdvanceCheckpoint(&mockMsg{seq: uint64(i + 1)}, 0)
		}
	})

	// Goroutine 2: call Classify with a consumer-deleted error (simulates runLoop
	// hitting recovery while AdvanceCheckpoint is still running).
	wg.Go(func() {
		for range 50 {
			_, _, _ = c.Classify(ctx, jetstream.ErrConsumerDeleted, nil, baseCfg,
				func(_ context.Context, _ jetstream.ConsumerConfig) (jetstream.Consumer, error) {
					return &stubConsumer{}, nil
				},
			)
		}
	})

	wg.Wait()
}
