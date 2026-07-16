package consumer

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/recovery"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	partitesting "github.com/arloliu/parti/v2/partitest"
)

func TestQueueConfig_Validate_RequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		cfg     QueueConfig
		wantErr bool
	}{
		{
			name:    "empty config",
			cfg:     QueueConfig{},
			wantErr: true,
		},
		{
			name: "missing stream name",
			cfg: QueueConfig{
				CommonConfig:  CommonConfig{},
				ConsumerName:  "test",
				FilterSubject: "events.>",
			},
			wantErr: true,
		},
		{
			name: "missing consumer name",
			cfg: QueueConfig{
				CommonConfig:  CommonConfig{},
				StreamName:    "TEST",
				FilterSubject: "events.>",
			},
			wantErr: true,
		},
		{
			name: "missing filter subject",
			cfg: QueueConfig{
				CommonConfig: CommonConfig{},
				StreamName:   "TEST",
				ConsumerName: "test",
			},
			wantErr: true,
		},
		{
			name: "valid config",
			cfg: QueueConfig{
				CommonConfig:  CommonConfig{},
				StreamName:    "TEST",
				ConsumerName:  "test",
				FilterSubject: "events.>",
			},
			wantErr: false,
		},
		{
			name: "invalid filter subject",
			cfg: QueueConfig{
				CommonConfig:  CommonConfig{},
				StreamName:    "TEST",
				ConsumerName:  "test",
				FilterSubject: "events..>",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = tt.cfg.SetDefaults()
			err := tt.cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestQueueConfig_Validate_InvalidConsumerName(t *testing.T) {
	cfg := QueueConfig{
		CommonConfig:  CommonConfig{},
		StreamName:    "TEST",
		ConsumerName:  "invalid name!", // contains space and !
		FilterSubject: "events.>",
	}
	_ = cfg.SetDefaults()
	err := cfg.Validate()
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "invalid characters"))
}

// TestQueueConfig_Validate_WrapsErrInvalidConfig verifies that every validation
// failure in QueueConfig.Validate is reachable via errors.Is(err, ErrInvalidConfig).
func TestQueueConfig_Validate_WrapsErrInvalidConfig(t *testing.T) {
	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	tests := []struct {
		name string
		fn   func() error
	}{
		{
			name: "fuda required field (missing StreamName)",
			fn: func() error {
				_, err := NewQueue(nil, "", "c", "s.>", handler)

				return err
			},
		},
		{
			name: "fuda required field (missing ConsumerName)",
			fn: func() error {
				_, err := NewQueue(nil, "S", "", "s.>", handler)

				return err
			},
		},
		{
			name: "invalid consumer name",
			fn: func() error {
				cfg := QueueConfig{
					StreamName:    "TEST",
					ConsumerName:  "bad name!",
					FilterSubject: "events.>",
				}
				_ = cfg.SetDefaults()

				return cfg.Validate()
			},
		},
		{
			name: "invalid filter subject",
			fn: func() error {
				cfg := QueueConfig{
					StreamName:    "TEST",
					ConsumerName:  "ok",
					FilterSubject: "events..>",
				}
				_ = cfg.SetDefaults()

				return cfg.Validate()
			},
		},
		{
			name: "FetchTimeout below 1s floor",
			fn: func() error {
				cfg := QueueConfig{
					StreamName:    "TEST",
					ConsumerName:  "ok",
					FilterSubject: "events.>",
					CommonConfig:  CommonConfig{FetchTimeout: 100 * time.Millisecond},
				}
				_ = cfg.SetDefaults()

				return cfg.Validate()
			},
		},
		{
			name: "PullHeartbeatCap below 500ms floor",
			fn: func() error {
				cfg := QueueConfig{
					StreamName:    "TEST",
					ConsumerName:  "ok",
					FilterSubject: "events.>",
					CommonConfig:  CommonConfig{PullHeartbeatCap: 499 * time.Millisecond},
				}
				_ = cfg.SetDefaults()

				return cfg.Validate()
			},
		},
		{
			name: "PullHeartbeatCap above 30s ceiling",
			fn: func() error {
				cfg := QueueConfig{
					StreamName:    "TEST",
					ConsumerName:  "ok",
					FilterSubject: "events.>",
					CommonConfig:  CommonConfig{PullHeartbeatCap: 31 * time.Second},
				}
				_ = cfg.SetDefaults()

				return cfg.Validate()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn()
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrInvalidConfig),
				"expected errors.Is(err, ErrInvalidConfig), got: %v", err)
		})
	}
}

// TestQueueConfig_Validate_PullHeartbeatCap_Accepted pins the accept side of
// the boundary: unset (0) and every valid nonzero cap must pass.
func TestQueueConfig_Validate_PullHeartbeatCap_Accepted(t *testing.T) {
	for _, heartbeatCap := range []time.Duration{0, 500 * time.Millisecond, 2 * time.Second, 30 * time.Second} {
		t.Run(heartbeatCap.String(), func(t *testing.T) {
			cfg := QueueConfig{
				StreamName:    "TEST",
				ConsumerName:  "ok",
				FilterSubject: "events.>",
				CommonConfig:  CommonConfig{PullHeartbeatCap: heartbeatCap},
			}
			_ = cfg.SetDefaults()
			require.NoError(t, cfg.Validate())
		})
	}
}

func TestQueueConfig_SetDefaults(t *testing.T) {
	cfg := QueueConfig{
		CommonConfig:  CommonConfig{},
		StreamName:    "TEST",
		ConsumerName:  "test",
		FilterSubject: "events.>",
	}
	require.NoError(t, cfg.SetDefaults())

	// Check defaults are applied
	require.NotNil(t, cfg.Logger)
	require.NotNil(t, cfg.Metrics)
	require.Equal(t, 30*time.Second, cfg.AckWait)
	require.Equal(t, -1, cfg.MaxDeliver)
	require.Equal(t, 1, cfg.BatchSize)
	require.Equal(t, 5*time.Second, cfg.FetchTimeout)
	require.Equal(t, 2, cfg.MaxWaiting)
	require.Equal(t, 24*time.Hour, cfg.InactiveThreshold)
}

func TestNewQueue_RequiresJetStream(t *testing.T) {
	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil })

	_, err := NewQueue(nil, "TEST", "queue", "events.>", handler)
	require.Error(t, err)
	require.Contains(t, err.Error(), "JetStream")
}

func TestNewQueue_RequiresHandler(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = NewQueue(js, "TEST", "queue", "events.>", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "handler")
}

func TestQueue_StartStop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create a stream
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "QUEUE_TEST",
		Subjects:  []string{"queue.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil })

	q, err := NewQueue(js, "QUEUE_TEST", "queue-workers", "queue.>", handler,
		WithFetchTimeout(time.Second),
	)
	require.NoError(t, err)

	// Start the consumer
	require.NoError(t, q.Start(ctx))

	// Verify consumer was created
	stream, err := js.Stream(ctx, "QUEUE_TEST")
	require.NoError(t, err)

	_, err = stream.Consumer(ctx, "queue-workers")
	require.NoError(t, err)

	// Stop the consumer with a fresh context
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	require.NoError(t, q.Stop(stopCtx))
}

func TestQueue_ReceivesMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create a stream
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "QUEUE_MSG",
		Subjects:  []string{"qmsg.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	var handled int32
	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		atomic.AddInt32(&handled, 1)

		return nil
	})

	q, err := NewQueue(js, "QUEUE_MSG", "msg-workers", "qmsg.>", handler,
		WithBatchSize(1),
		WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)

	require.NoError(t, q.Start(ctx))
	t.Cleanup(func() { _ = q.Stop(ctx) })

	// Publish messages
	for range 5 {
		require.NoError(t, nc.Publish("qmsg.events", []byte("msg")))
	}
	_ = nc.Flush()

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&handled) >= 5
	}, 5*time.Second, 50*time.Millisecond)
}

func TestQueue_DoubleStartFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "QUEUE_DUP",
		Subjects:  []string{"dup.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil })

	q, err := NewQueue(js, "QUEUE_DUP", "dup-workers", "dup.>", handler)
	require.NoError(t, err)
	t.Cleanup(func() { _ = q.Stop(ctx) })

	require.NoError(t, q.Start(ctx))
	err = q.Start(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already started")
}

func TestQueue_ManualAck(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "QUEUE_MANUAL",
		Subjects:  []string{"manual.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	var handled int32
	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		atomic.AddInt32(&handled, 1)
		// Manual ack
		return msg.Ack()
	})

	q, err := NewQueue(js, "QUEUE_MANUAL", "manual-workers", "manual.>", handler,
		WithManualAck(true),
		WithBatchSize(1),
		WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)

	require.NoError(t, q.Start(ctx))
	t.Cleanup(func() { _ = q.Stop(ctx) })

	// Publish a message
	require.NoError(t, nc.Publish("manual.events", []byte("msg")))
	_ = nc.Flush()

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&handled) >= 1
	}, 5*time.Second, 50*time.Millisecond)
}

// --- delayWithBackoffOrExit semantics test ---

func TestQueue_DelayWithBackoffOrExit_ReturnsTrueOnCancel(t *testing.T) {
	q := &Queue{
		config: QueueConfig{
			Retry: RetryConfig{
				Base:       10 * time.Millisecond,
				Multiplier: 1.0,
				Max:        50 * time.Millisecond,
			},
		},
	}

	// Already-cancelled context should return true (=exit).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var prev time.Duration
	result := q.delayWithBackoffOrExit(ctx, &prev)
	require.True(t, result, "should return true (exit) when context is cancelled")
}

func TestQueue_DelayWithBackoffOrExit_ReturnsFalseAfterDelay(t *testing.T) {
	q := &Queue{
		config: QueueConfig{
			Retry: RetryConfig{
				Base:       1 * time.Millisecond,
				Multiplier: 1.0,
				Max:        5 * time.Millisecond,
			},
		},
	}

	ctx := context.Background()
	var prev time.Duration
	result := q.delayWithBackoffOrExit(ctx, &prev)
	require.False(t, result, "should return false (continue) after delay completes")
}

type mockRecoveryJetStream struct {
	jetstream.JetStream
	createOrUpdateConsumerFunc func(ctx context.Context, stream string, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error)
}

func (m *mockRecoveryJetStream) CreateOrUpdateConsumer(ctx context.Context, stream string, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	return m.createOrUpdateConsumerFunc(ctx, stream, cfg)
}

type stubConsumer struct {
	jetstream.Consumer
}

type queueErrorIter struct {
	err error
}

func (e *queueErrorIter) Next(opts ...jetstream.NextOpt) (jetstream.Msg, error) {
	return nil, e.err
}

func (e *queueErrorIter) Stop() {}

func (e *queueErrorIter) Drain() {}

type queueRecoveryMetrics struct {
	attemptReasons []string
	results        []string
	resultReasons  []string
	durations      []float64
}

func (m *queueRecoveryMetrics) IncrementWorkerConsumerControlRetry(string)       {}
func (m *queueRecoveryMetrics) RecordWorkerConsumerRetryBackoff(string, float64) {}
func (m *queueRecoveryMetrics) SetWorkerConsumerSubjectsCurrent(int)             {}
func (m *queueRecoveryMetrics) IncrementWorkerConsumerSubjectChange(string, int) {}
func (m *queueRecoveryMetrics) IncrementWorkerConsumerGuardrailViolation(string) {}
func (m *queueRecoveryMetrics) IncrementWorkerConsumerSubjectThresholdWarning()  {}
func (m *queueRecoveryMetrics) RecordWorkerConsumerUpdate(string)                {}
func (m *queueRecoveryMetrics) ObserveWorkerConsumerUpdateLatency(float64)       {}
func (m *queueRecoveryMetrics) IncrementWorkerConsumerIteratorRestart(string)    {}
func (m *queueRecoveryMetrics) IncrementWorkerConsumerIteratorEscalation(string) {}
func (m *queueRecoveryMetrics) SetWorkerConsumerConsecutiveIteratorFailures(int) {}
func (m *queueRecoveryMetrics) SetWorkerConsumerHealthStatus(bool)               {}
func (m *queueRecoveryMetrics) IncrementWorkerConsumerPullSuppressed(string)     {}
func (m *queueRecoveryMetrics) IncrementWorkerConsumerRecreationAttempt(reason string) {
	m.attemptReasons = append(m.attemptReasons, reason)
}

func (m *queueRecoveryMetrics) RecordWorkerConsumerRecreation(result string, reason string) {
	m.results = append(m.results, result)
	m.resultReasons = append(m.resultReasons, reason)
}

func (m *queueRecoveryMetrics) ObserveWorkerConsumerRecreationDuration(seconds float64) {
	m.durations = append(m.durations, seconds)
}

func TestQueue_RunLoop_FailedRecoveryUsesBackoff(t *testing.T) {
	m := &queueRecoveryMetrics{}
	var iterFactoryCalls atomic.Int32

	q := &Queue{
		js: &mockRecoveryJetStream{createOrUpdateConsumerFunc: func(ctx context.Context, stream string, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {
			return nil, jetstream.ErrStreamNotFound
		}},
		config: QueueConfig{
			CommonConfig: CommonConfig{
				BatchSize:    1,
				FetchTimeout: time.Second,
			},
			StreamName:   "QUEUE",
			ConsumerName: "queue-workers",
			Retry: RetryConfig{
				Base:       20 * time.Millisecond,
				Backoff:    20 * time.Millisecond,
				Multiplier: 1.0,
				Max:        20 * time.Millisecond,
			},
		},
		logger:   logging.NewNop(),
		metrics:  m,
		consumer: &stubConsumer{},
		loopDone: make(chan struct{}),
		iterFactory: func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
			iterFactoryCalls.Add(1)
			return &queueErrorIter{err: jetstream.ErrConsumerDeleted}, nil
		},
		consumerConfig: jetstream.ConsumerConfig{Durable: "queue-workers"},
		recovery: recovery.NewController(recovery.ControllerConfig{
			Strategy:       RecoverFromNew,
			BurstThreshold: 3,
			BurstWindow:    10 * time.Second,
			Logger:         logging.NewNop(),
			Metrics:        m,
		}),
		retryRNG: newRetryRNG(1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	go q.runLoop(ctx)

	time.Sleep(55 * time.Millisecond)
	cancel()
	<-q.loopDone

	require.Less(t, iterFactoryCalls.Load(), int32(5), "failed recovery should back off instead of hot-looping")
	require.NotEmpty(t, m.attemptReasons)
	require.Equal(t, []string{"failure"}, m.results[:1])
}

// queueSignalMetrics extends queueRecoveryMetrics to record iterator-restart reasons.
type queueSignalMetrics struct {
	queueRecoveryMetrics
	restartReasons []string
}

func (m *queueSignalMetrics) IncrementWorkerConsumerIteratorRestart(reason string) {
	m.restartReasons = append(m.restartReasons, reason)
}

// TestQueue_RecoveryDisabled_IteratorErrorIsSignaled pins the operator signal for
// the default configuration: with RecoveryDisabled (nil controller), a non-graceful
// iterator error (e.g. the durable was deleted) must emit a Warn log and the
// iterator-restart metric with reason "recovery_disabled". Pre-fix the only artifact
// was a Debug log — at production log levels a deleted durable was a zero-signal
// permanent stall.
func TestQueue_RecoveryDisabled_IteratorErrorIsSignaled(t *testing.T) {
	m := &queueSignalMetrics{}

	q := &Queue{
		config: QueueConfig{
			CommonConfig: CommonConfig{
				BatchSize:    1,
				FetchTimeout: time.Second,
			},
			StreamName:   "Q_RECOVERY_DISABLED",
			ConsumerName: "q-worker",
			Retry: RetryConfig{
				Base:       20 * time.Millisecond,
				Backoff:    20 * time.Millisecond,
				Multiplier: 1.0,
				Max:        20 * time.Millisecond,
			},
		},
		logger:   logging.NewNop(),
		metrics:  m,
		consumer: &stubConsumer{},
		loopDone: make(chan struct{}),
		iterFactory: func(_ jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
			return &queueErrorIter{err: jetstream.ErrNoHeartbeat}, nil
		},
		consumerConfig: jetstream.ConsumerConfig{Durable: "q-worker"},
		recovery:       nil, // RecoveryDisabled: NewController returns nil for Disabled strategy
		retryRNG:       newRetryRNG(1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	go q.runLoop(ctx)

	// One backoff cycle (20ms) is enough to observe the metric; wait two.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-q.loopDone

	require.NotEmpty(t, m.restartReasons,
		"expected IncrementWorkerConsumerIteratorRestart to be called on the nil-recovery path")
	require.Contains(t, m.restartReasons, "recovery_disabled",
		"expected reason=recovery_disabled on the nil-recovery path")
}

// TestQueue_ClosedIteratorWithLiveContext_BacksOff pins that a persistently
// closing iterator (Next always returns ErrMsgIteratorClosed) with a live
// context is bounded in factory call count over 200ms. Pre-fix processIterator
// returned nil for ErrMsgIteratorClosed regardless of ctx state, causing
// runLoop to reset backoff to 0 and recreate the iterator in a tight loop
// (hundreds of calls in 200ms).
func TestQueue_ClosedIteratorWithLiveContext_BacksOff(t *testing.T) {
	var factoryCalls atomic.Int32

	q := &Queue{
		config: QueueConfig{
			CommonConfig: CommonConfig{
				BatchSize:    1,
				FetchTimeout: time.Second,
			},
			StreamName:   "QUEUE",
			ConsumerName: "queue-workers",
			Retry: RetryConfig{
				Base:       20 * time.Millisecond,
				Backoff:    20 * time.Millisecond,
				Multiplier: 1.0,
				Max:        50 * time.Millisecond,
			},
		},
		logger:   logging.NewNop(),
		metrics:  &queueRecoveryMetrics{},
		consumer: &stubConsumer{},
		loopDone: make(chan struct{}),
		iterFactory: func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
			factoryCalls.Add(1)
			return &queueErrorIter{err: jetstream.ErrMsgIteratorClosed}, nil
		},
		consumerConfig: jetstream.ConsumerConfig{Durable: "queue-workers"},
		recovery:       nil, // RecoveryDisabled
		retryRNG:       newRetryRNG(1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	go q.runLoop(ctx)

	time.Sleep(200 * time.Millisecond)
	cancel()
	<-q.loopDone

	calls := factoryCalls.Load()
	t.Logf("factory call count over 200ms: %d", calls)
	require.Less(t, calls, int32(20),
		"ErrMsgIteratorClosed with live context must back off; got %d factory calls in 200ms", calls)
}

// TestQueue_ConsumerOptions_AppliedToLiveConsumer verifies that
// WithConsumerMemoryStorage and WithConsumerReplicas reach the live
// Queue consumer's Config.
func TestQueue_ConsumerOptions_AppliedToLiveConsumer(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streamName := "Q_OPT"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qopt.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	q, err := NewQueue(
		js,
		streamName,
		"qopt-consumer",
		"qopt.>",
		handler,
		WithConsumerMemoryStorage(true),
		WithConsumerReplicas(1),
	)
	require.NoError(t, err)

	require.NoError(t, q.Start(ctx))
	defer func() { _ = q.Stop(ctx) }()

	stream, err := js.Stream(ctx, streamName)
	require.NoError(t, err)
	cons, err := stream.Consumer(ctx, "qopt-consumer")
	require.NoError(t, err)

	consInfo, err := cons.Info(ctx)
	require.NoError(t, err)
	require.True(t, consInfo.Config.MemoryStorage, "Config.MemoryStorage = false, want true")
	require.Equal(t, 1, consInfo.Config.Replicas, "Config.Replicas = %d, want 1", consInfo.Config.Replicas)
}

// TestQueue_ConsumerReplicasExceedsStream_Surfaces10126 verifies the
// pass-through validation: NewQueue accepts WithConsumerReplicas(2)
// because it doesn't call NATS; Queue.Start surfaces the 10126 error.
func TestQueue_ConsumerReplicasExceedsStream_Surfaces10126(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streamName := "Q_VALIDATE"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qvalid.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	q, err := NewQueue(
		js,
		streamName,
		"qvalid-consumer",
		"qvalid.>",
		handler,
		WithConsumerReplicas(2),
	)
	require.NoError(t, err, "NewQueue should not call NATS; the error should surface at Start")
	require.NotNil(t, q)

	err = q.Start(ctx)
	require.Error(t, err, "Queue.Start should reject Replicas=2 with stream Replicas=1")
	// On clustered streams NATS rejects with err_code 10126 ("consumer
	// config replica count exceeds parent stream"). On single-node
	// embedded NATS the earlier guard fires instead: err_code 10074
	// ("replicas > 1 not supported in non-clustered mode"). Both
	// prove pass-through validation: parti surfaces the NATS error
	// verbatim from Queue.Start without pre-validating.
	msg := err.Error()
	require.True(t,
		strings.Contains(msg, "10126") || strings.Contains(msg, "10074"),
		"expected NATS error code 10126 or 10074 in error message, got: %v", err)
}

// TestQueue_RecoverySnapshot_CarriesConsumerOptions verifies the stored
// recovery-snapshot config has both new fields set. Single-node embedded
// NATS resolves Replicas=0 and Replicas=1 to the same Info().Config.Replicas,
// so the integration test can't distinguish them; this white-box test reads
// the cfg parti actually sent to NATS.
func TestQueue_RecoverySnapshot_CarriesConsumerOptions(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streamName := "Q_SNAP"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qsnap.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	qExplicit, err := NewQueue(js, streamName, "qsnap-explicit", "qsnap.>", handler,
		WithConsumerMemoryStorage(true), WithConsumerReplicas(1))
	require.NoError(t, err)
	require.NoError(t, qExplicit.Start(ctx))
	defer func() { _ = qExplicit.Stop(ctx) }()

	require.True(t, qExplicit.consumerConfig.MemoryStorage,
		"explicit case: consumerConfig.MemoryStorage = false, want true")
	require.Equal(t, 1, qExplicit.consumerConfig.Replicas,
		"explicit case: consumerConfig.Replicas = %d, want 1", qExplicit.consumerConfig.Replicas)

	qDefault, err := NewQueue(js, streamName, "qsnap-default", "qsnap.>", handler)
	require.NoError(t, err)
	require.NoError(t, qDefault.Start(ctx))
	defer func() { _ = qDefault.Stop(ctx) }()

	require.False(t, qDefault.consumerConfig.MemoryStorage)
	require.Equal(t, 0, qDefault.consumerConfig.Replicas)
}

// TestQueue_FetchTimeoutAbove60s_IteratorCreationSucceeds proves the derived
// pull heartbeat for a raised FetchTimeout stays within nats.go's PullHeartbeat
// validity range.
//
// Before the derivation was given an explicit ceiling, the heartbeat was
// always expiry/2 with no upper bound. nats.go v1.52.0 rejects an explicit
// PullHeartbeat outside [500ms, 30s] (jetstream_options.go configureMessages)
// with ErrInvalidOption, so any FetchTimeout above 60s derived a heartbeat
// above 30s and iterator creation failed on every attempt — the pull loop
// then spun in a restart/warn cycle forever with no terminal signal.
//
// This calls the real iterator factory directly (bypassing the async pull
// loop's retry/backoff, which would otherwise hide the failure behind an
// infinite warn-and-retry cycle) against a live NATS consumer, so it exercises
// the exact nats.go validation path a production pull loop would hit.
func TestQueue_FetchTimeoutAbove60s_IteratorCreationSucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "QUEUE_HB90",
		Subjects:  []string{"hb90.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	q, err := NewQueue(js, "QUEUE_HB90", "hb90-workers", "hb90.>", handler,
		WithFetchTimeout(90*time.Second),
	)
	require.NoError(t, err)

	cons, err := q.ensureConsumer(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = js.DeleteConsumer(ctx, "QUEUE_HB90", "hb90-workers") })

	iter, err := q.iterFactory(cons, q.config.BatchSize, q.config.FetchTimeout)
	require.NoError(t, err,
		"iterator creation must succeed with FetchTimeout > 60s once the derived "+
			"heartbeat is bounded to nats.go's 30s PullHeartbeat maximum")
	iter.Stop()
}
