package consumer

import (
	"context"
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
		WithFetchTimeout(500*time.Millisecond),
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
	for i := 0; i < 5; i++ {
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
