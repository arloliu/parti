package consumer

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

	// Wait for consumer to be ready
	time.Sleep(100 * time.Millisecond)

	// Publish messages
	for i := 0; i < 5; i++ {
		require.NoError(t, nc.Publish("qmsg.events", []byte("msg")))
	}
	_ = nc.Flush()

	// Wait for messages to be handled
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&handled) >= 5 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&handled), int32(5))
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

	// Wait for consumer to be ready
	time.Sleep(100 * time.Millisecond)

	// Publish a message
	require.NoError(t, nc.Publish("manual.events", []byte("msg")))
	_ = nc.Flush()

	// Wait for message to be handled
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&handled) >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&handled), int32(1))
}
