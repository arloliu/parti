package ipartition

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/internal/logging"
	imetrics "github.com/arloliu/parti/v2/internal/metrics"
	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/partition"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestJSConsumer_StartTwice(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "events",
		Subjects: []string{"events.*.completed.*"},
	})
	require.NoError(t, err)

	consumer, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "events.{{key}}.completed.{{partition}}",
		},
		StreamName:   "events",
		ConsumerName: "consumer-0",
		Partition:    0,
	}, messageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }))
	require.NoError(t, err)

	require.NoError(t, consumer.Start(ctx))
	err = consumer.Start(ctx)
	require.Error(t, err)

	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	require.NoError(t, consumer.Stop(stopCtx))
}

func TestJSConsumer_StopWithoutStart(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	consumer, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  1,
			SubjectPattern: "events.{{partition}}",
		},
		StreamName:   "events",
		ConsumerName: "consumer-0",
		Partition:    0,
	}, messageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }))
	require.NoError(t, err)

	stopCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, consumer.Stop(stopCtx))
}

func TestNewJSConsumer_InvalidArgs(t *testing.T) {
	_, err := NewJSConsumer(nil, ConsumerConfig{}, messageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }))
	require.Error(t, err)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = NewJSConsumer(js, ConsumerConfig{}, nil)
	require.Error(t, err)
}

func TestJSConsumer_GracefulShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "graceful",
		Subjects: []string{"graceful.*"},
	})
	require.NoError(t, err)

	// Publish some messages
	for range 5 {
		_, err = js.Publish(ctx, "graceful.0", []byte("test"))
		require.NoError(t, err)
	}

	processed := make(chan struct{}, 10)
	consumer, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "graceful.{{partition}}",
		},
		StreamName:   "graceful",
		ConsumerName: "graceful-consumer-0",
		Partition:    0,
		FetchTimeout: 1 * time.Second,
	}, messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		processed <- struct{}{}
		return nil
	}))
	require.NoError(t, err)

	require.NoError(t, consumer.Start(ctx))

	// Wait for some messages to be processed
	for range 5 {
		select {
		case <-processed:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for message processing")
		}
	}

	// Stop should complete gracefully
	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err = consumer.Stop(stopCtx)
	require.NoError(t, err)
}

func TestJSConsumer_StopTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "timeout",
		Subjects: []string{"timeout.*"},
	})
	require.NoError(t, err)

	// Publish a message
	_, err = js.Publish(ctx, "timeout.0", []byte("test"))
	require.NoError(t, err)

	blockingHandler := make(chan struct{})
	consumer, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "timeout.{{partition}}",
		},
		StreamName:   "timeout",
		ConsumerName: "timeout-consumer-0",
		Partition:    0,
		FetchTimeout: 5 * time.Second,
		ManualAck:    true, // Don't auto-ack so message stays in-flight
	}, messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		<-blockingHandler // Block until closed
		return nil
	}))
	require.NoError(t, err)

	require.NoError(t, consumer.Start(ctx))

	// Give it time to start processing
	time.Sleep(200 * time.Millisecond)

	// Stop with very short timeout - should return context error
	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	err = consumer.Stop(stopCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	// Unblock handler so goroutine can exit cleanly
	close(blockingHandler)
}

func TestJSConsumer_StopIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "idempotent",
		Subjects: []string{"idempotent.*"},
	})
	require.NoError(t, err)

	consumer, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "idempotent.{{partition}}",
		},
		StreamName:   "idempotent",
		ConsumerName: "idempotent-consumer-0",
		Partition:    0,
		FetchTimeout: 1 * time.Second,
	}, messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		return nil
	}))
	require.NoError(t, err)

	require.NoError(t, consumer.Start(ctx))

	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// First stop should succeed
	err = consumer.Stop(stopCtx)
	require.NoError(t, err)

	// Second stop should also succeed (idempotent)
	err = consumer.Stop(stopCtx)
	require.NoError(t, err)
}

// TestJSConsumer_RecoverySnapshot_CarriesConsumerOptions verifies the
// recovery snapshot for Static consumers carries the two new fields.
// Single-node embedded NATS can't distinguish "explicit Replicas=1" from
// default; this white-box test reads the cfg parti sent to NATS.
func TestJSConsumer_RecoverySnapshot_CarriesConsumerOptions(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "ISNAP",
		Subjects: []string{"isnap.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	noopHandler := messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	jcExplicit, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "isnap.{{partition}}",
		},
		StreamName:            "ISNAP",
		ConsumerName:          "isnap-explicit",
		Partition:             0,
		ConsumerMemoryStorage: true,
		ConsumerReplicas:      1,
	}, noopHandler)
	require.NoError(t, err)
	require.NoError(t, jcExplicit.Start(ctx))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = jcExplicit.Stop(ctx)
	})

	require.True(t, jcExplicit.consumerConfig.MemoryStorage,
		"explicit: consumerConfig.MemoryStorage = false, want true")
	require.Equal(t, 1, jcExplicit.consumerConfig.Replicas,
		"explicit: consumerConfig.Replicas = %d, want 1", jcExplicit.consumerConfig.Replicas)

	jcDefault, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "isnap.{{partition}}",
		},
		StreamName:   "ISNAP",
		ConsumerName: "isnap-default",
		Partition:    1,
	}, noopHandler)
	require.NoError(t, err)
	require.NoError(t, jcDefault.Start(ctx))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = jcDefault.Stop(ctx)
	})

	require.False(t, jcDefault.consumerConfig.MemoryStorage)
	require.Equal(t, 0, jcDefault.consumerConfig.Replicas)
}

// --- stub types for nil-recovery unit test ---

// consumerWithErrorIter is a jetstream.Consumer stub whose Messages() returns
// an iterator that immediately returns a non-graceful error from Next().
type consumerWithErrorIter struct {
	jetstream.Consumer
	iterErr error
}

func (c *consumerWithErrorIter) Messages(opts ...jetstream.PullMessagesOpt) (jetstream.MessagesContext, error) {
	return &errorOnNextMsgsCtx{err: c.iterErr}, nil
}

func (c *consumerWithErrorIter) Info(_ context.Context) (*jetstream.ConsumerInfo, error) {
	return nil, context.Canceled
}

type errorOnNextMsgsCtx struct {
	err error
}

func (e *errorOnNextMsgsCtx) Next(opts ...jetstream.NextOpt) (jetstream.Msg, error) {
	return nil, e.err
}

func (e *errorOnNextMsgsCtx) Stop()  {}
func (e *errorOnNextMsgsCtx) Drain() {}

// jsConsumerSignalMetrics records IncrementWorkerConsumerIteratorRestart calls.
type jsConsumerSignalMetrics struct {
	*imetrics.NopMetrics
	restartReasons []string
}

func (m *jsConsumerSignalMetrics) IncrementWorkerConsumerIteratorRestart(reason string) {
	m.restartReasons = append(m.restartReasons, reason)
}

// TestJSConsumer_StartAfterStop_ReturnsErrConsumerStopped pins the terminal-Stop
// contract for JSConsumer: Stop resets cancel=nil, so pre-fix a second Start passed
// the started guard (cancel==nil ≡ not-started) and returned nil — contradicting the
// godoc "Start may only be called once". With DispatchByKey the keyDispatcher was
// terminally closed (sync.Once) in Stop, so the restarted loop silently fetched
// messages it could never process.
func TestJSConsumer_StartAfterStop_ReturnsErrConsumerStopped(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "stopped",
		Subjects: []string{"stopped.*"},
	})
	require.NoError(t, err)

	consumer, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "stopped.{{partition}}",
		},
		StreamName:   "stopped",
		ConsumerName: "stopped-consumer-0",
		Partition:    0,
		FetchTimeout: time.Second,
	}, messageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }))
	require.NoError(t, err)

	// Start successfully
	require.NoError(t, consumer.Start(ctx))

	// Stop the consumer (terminal)
	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	require.NoError(t, consumer.Stop(stopCtx))

	// A second Stop must remain idempotent
	require.NoError(t, consumer.Stop(stopCtx), "second Stop must be idempotent")

	// Start after Stop must return ErrConsumerStopped, not nil
	// Pre-fix: Stop reset cancel=nil, so Start saw cancel==nil (≡ not-started)
	// and proceeded, returning nil while possibly operating a broken keyDispatcher.
	err = consumer.Start(ctx)
	require.ErrorIs(t, err, types.ErrConsumerStopped,
		"Start after Stop must return ErrConsumerStopped, got: %v", err)

	// Pin the never-started variant: New→Stop→Start must also return
	// ErrConsumerStopped even if Start was never called first. The stopped
	// flag is set by Stop regardless of whether the consumer was running.
	t.Run("never-started", func(t *testing.T) {
		c2, err2 := NewJSConsumer(js, ConsumerConfig{
			PartitionConfig: partition.PartitionConfig{
				NumPartitions:  2,
				SubjectPattern: "stopped.{{partition}}",
			},
			StreamName:   "stopped",
			ConsumerName: "never-started-consumer-0",
			Partition:    0,
			FetchTimeout: time.Second,
		}, messageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }))
		require.NoError(t, err2)

		stopCtx, stopCancel := context.WithTimeout(ctx, 2*time.Second)
		defer stopCancel()
		require.NoError(t, c2.Stop(stopCtx), "Stop on never-started must be nil")

		err2 = c2.Start(ctx)
		require.ErrorIs(t, err2, types.ErrConsumerStopped,
			"Start after Stop on never-started must return ErrConsumerStopped")
	})
}

// TestJSConsumer_RecoveryDisabled_IteratorErrorIsSignaled pins the operator signal
// for the default configuration: with RecoveryDisabled (nil controller), a non-graceful
// iterator error (e.g. the durable was deleted) must emit the iterator-restart metric
// with reason "recovery_disabled". Pre-fix the only artifact was a Debug log — at
// production log levels a deleted durable was a zero-signal permanent stall.
func TestJSConsumer_RecoveryDisabled_IteratorErrorIsSignaled(t *testing.T) {
	m := &jsConsumerSignalMetrics{NopMetrics: imetrics.NewNop()}

	c := &JSConsumer{
		config: ConsumerConfig{
			Logger:       logging.NewNop(),
			Metrics:      m,
			FetchTimeout: time.Second,
			BatchSize:    1,
		},
		handler:   messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil }),
		partition: 0,
		subject:   "test.0",
		done:      make(chan struct{}),
		rc:        nil, // RecoveryDisabled: NewController returns nil for Disabled strategy
		consumer:  &consumerWithErrorIter{iterErr: jetstream.ErrNoHeartbeat},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go c.run(ctx)

	// iteratorRetryDelay is 200ms; wait for at least one cycle.
	time.Sleep(250 * time.Millisecond)
	cancel()
	<-c.done

	require.NotEmpty(t, m.restartReasons,
		"expected IncrementWorkerConsumerIteratorRestart to be called on the nil-recovery path")
	require.Contains(t, m.restartReasons, "recovery_disabled",
		"expected reason=recovery_disabled on the nil-recovery path")
}

// --- merged from consumer_stream_missing_test.go ---

// captureWarnLogger records WARN-level messages so a test can assert that a
// specific log line was emitted by a production code path. Optional t enables
// pass-through to the test log for diagnosis.
type captureWarnLogger struct {
	mu    sync.Mutex
	warns []string
	t     *testing.T
}

var _ types.Logger = (*captureWarnLogger)(nil)

func (l *captureWarnLogger) Debug(msg string, kv ...any) {
	if l.t != nil {
		l.t.Logf("DEBUG: %s %s", msg, fmt.Sprint(kv...))
	}
}
func (l *captureWarnLogger) Info(msg string, kv ...any) {
	if l.t != nil {
		l.t.Logf("INFO: %s %s", msg, fmt.Sprint(kv...))
	}
}
func (l *captureWarnLogger) Warn(msg string, kv ...any) {
	l.mu.Lock()
	l.warns = append(l.warns, msg)
	l.mu.Unlock()
	if l.t != nil {
		l.t.Logf("WARN: %s %s", msg, fmt.Sprint(kv...))
	}
}
func (l *captureWarnLogger) Error(msg string, kv ...any) {
	if l.t != nil {
		l.t.Logf("ERROR: %s %s", msg, fmt.Sprint(kv...))
	}
}
func (l *captureWarnLogger) Fatal(msg string, _ ...any) {
	panic("captureWarnLogger.Fatal: " + msg)
}

func (l *captureWarnLogger) hasWarn(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.warns {
		if strings.Contains(w, substr) {
			return true
		}
	}

	return false
}

// TestJSConsumer_ActionStreamMissing_LogsAndBacksOff pins the spec-required
// mapping for JSConsumer's non-Dynamic ActionStreamMissing branch
// (docs/plans/self-healing/09-pr9-spec.md § "File-by-file caller contract"):
// JSConsumer does not own stream lifecycle, so the stream-missing
// classification is logged for operator observability and folded into a
// backoff — it must NOT fall through to the default branch or cause the
// loop to exit silently.
func TestJSConsumer_ActionStreamMissing_LogsAndBacksOff(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const streamName = "JS_STREAM_MISSING"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{"jsm.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	log := &captureWarnLogger{t: t}

	c, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  1,
			SubjectPattern: "jsm.{{partition}}",
		},
		StreamName:       streamName,
		ConsumerName:     "jsm-consumer-0",
		Partition:        0,
		FetchTimeout:     1500 * time.Millisecond,
		Logger:           log,
		RecoveryStrategy: durable.RecoverFromBeginning,
	}, messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil }))
	require.NoError(t, err)

	require.NoError(t, c.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = c.Stop(stopCtx)
	})

	// Wait for the JS consumer to be bound on the server.
	require.Eventually(t, func() bool {
		_, infoErr := js.Consumer(ctx, streamName, "jsm-consumer-0")
		return infoErr == nil
	}, 5*time.Second, 25*time.Millisecond, "JSConsumer must bind before stream deletion")

	require.NoError(t, js.DeleteStream(ctx, streamName))

	require.Eventually(t, func() bool {
		return log.hasWarn("js consumer recovery classified stream missing")
	}, 10*time.Second, 50*time.Millisecond,
		"JSConsumer's ActionStreamMissing case must emit the expected WARN log line; absence implies the case is unwired or fell through")
}
