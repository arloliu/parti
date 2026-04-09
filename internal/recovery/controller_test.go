package recovery

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
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

	action, cons := c.Classify(context.Background(), jetstream.ErrConsumerDeleted, nil, baseCfg, nil)
	require.Equal(t, ActionBackoff, action)
	require.Nil(t, cons)

	c.AdvanceCheckpoint(&mockMsg{seq: 10})
	c.SeedCheckpoint(context.Background(), nil)
	c.ResetBurst()
}

func TestClassify_GracefulExit(t *testing.T) {
	c := NewController(ControllerConfig{
		Strategy: FromNew,
		Logger:   nopLog,
	})

	action, _ := c.Classify(context.Background(), jetstream.ErrMsgIteratorClosed, nil, baseCfg, nil)
	require.Equal(t, ActionExit, action)

	action, _ = c.Classify(context.Background(), context.Canceled, nil, baseCfg, nil)
	require.Equal(t, ActionExit, action)

	action, _ = c.Classify(context.Background(), nil, nil, baseCfg, nil)
	require.Equal(t, ActionExit, action)
}

func TestClassify_ConsumerDeleted_RecoverySucceeds(t *testing.T) {
	metrics := &testMetrics{}
	c := NewController(ControllerConfig{
		Strategy: FromNew,
		Logger:   nopLog,
		Metrics:  metrics,
	})

	action, newCons := c.Classify(context.Background(), jetstream.ErrConsumerDeleted, nil, baseCfg, alwaysSucceedRecreate)
	require.Equal(t, ActionContinue, action)
	require.NotNil(t, newCons)
	require.Equal(t, []string{"consumer_deleted"}, metrics.iterRestartReasons)
	require.Equal(t, []string{"success"}, metrics.results)
}

func TestClassify_ConsumerDeleted_RecoveryFails(t *testing.T) {
	metrics := &testMetrics{}
	c := NewController(ControllerConfig{
		Strategy: FromNew,
		Logger:   nopLog,
		Metrics:  metrics,
	})

	action, newCons := c.Classify(context.Background(), jetstream.ErrConsumerDeleted, nil, baseCfg, alwaysFailRecreate)
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
	action, _ := c.Classify(context.Background(), jetstream.ErrNoHeartbeat, nil, baseCfg, nil)
	require.Equal(t, ActionBackoff, action)
	action, _ = c.Classify(context.Background(), jetstream.ErrNoHeartbeat, nil, baseCfg, nil)
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
	action, _ := c.Classify(context.Background(), jetstream.ErrNoHeartbeat, alwaysNotFoundInfo, baseCfg, alwaysSucceedRecreate)
	require.Equal(t, ActionBackoff, action)

	// 2nd: threshold reached, Info() confirms gone, recovery succeeds
	action, newCons := c.Classify(context.Background(), jetstream.ErrNoHeartbeat, alwaysNotFoundInfo, baseCfg, alwaysSucceedRecreate)
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

	c.Classify(context.Background(), jetstream.ErrNoHeartbeat, alwaysSuccessInfo, baseCfg, nil)
	// 2nd: threshold reached, but Info() says consumer exists — no recovery
	action, newCons := c.Classify(context.Background(), jetstream.ErrNoHeartbeat, alwaysSuccessInfo, baseCfg, nil)
	require.Equal(t, ActionBackoff, action)
	require.Nil(t, newCons)
}

func TestClassify_TransientError(t *testing.T) {
	metrics := &testMetrics{}
	c := NewController(ControllerConfig{
		Strategy: FromNew,
		Logger:   nopLog,
		Metrics:  metrics,
	})

	action, _ := c.Classify(context.Background(), errors.New("something"), nil, baseCfg, nil)
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

	action, _ := c.Classify(ctx, jetstream.ErrConsumerDeleted, nil, baseCfg, alwaysSucceedRecreate)
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

	action, newCons := c.Classify(context.Background(), jetstream.ErrConsumerDeleted, nil, baseCfg, alwaysSucceedRecreate)
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
			c.Classify(context.Background(), jetstream.ErrConsumerDeleted, nil, baseCfg, recreate)
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
			c.AdvanceCheckpoint(&mockMsg{seq: uint64(i + 1)})
		}
	})

	// Goroutine 2: call Classify with a consumer-deleted error (simulates runLoop
	// hitting recovery while AdvanceCheckpoint is still running).
	wg.Go(func() {
		for range 50 {
			c.Classify(ctx, jetstream.ErrConsumerDeleted, nil, baseCfg,
				func(_ context.Context, _ jetstream.ConsumerConfig) (jetstream.Consumer, error) {
					return &stubConsumer{}, nil
				},
			)
		}
	})

	wg.Wait()
}
