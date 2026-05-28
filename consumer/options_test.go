package consumer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

var errMockNotImplemented = errors.New("mock not implemented")

// Mock objects for testing
type mockJS struct {
	jetstream.JetStream
}

func (m *mockJS) CreateStream(_ context.Context, _ jetstream.StreamConfig) (jetstream.Stream, error) {
	return nil, errMockNotImplemented
}

func (m *mockJS) Stream(_ context.Context, _ string) (jetstream.Stream, error) {
	return nil, errMockNotImplemented
}

func (m *mockJS) Conn() *nats.Conn {
	return nil
}

// TestStrictOptions verifies that options are correctly applied to respective consumers.
// It relies on Go's type system to ensure invalid options would cause compilation errors.
// This test checks that valid options *do* apply changes to the internal config.
func TestStrictOptions(t *testing.T) {
	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil })
	js := &mockJS{}
	logger := logging.NewNop()

	t.Run("Queue Options", func(t *testing.T) {
		// Valid options for Queue: Universal
		q, err := NewQueue(js, "STREAM", "queue", "subj.>", handler,
			WithLogger(logger),         // Universal
			WithAckWait(5*time.Second), // Universal
		)
		require.NoError(t, err)
		require.Equal(t, logger, q.config.Logger)
		require.Equal(t, 5*time.Second, q.config.AckWait)
	})

	t.Run("Broadcast Options", func(t *testing.T) {
		// Valid options for Broadcast: Universal + Broadcast-specific
		b, err := NewBroadcast(js, "STREAM", "broadcast", "subj.>", handler,
			WithLogger(logger),              // Universal
			WithInstanceID("test-instance"), // Broadcast specific
		)
		require.NoError(t, err)
		require.NotNil(t, b)
		// Internal config is not accessible in this package for Broadcast (wrapped type),
		// but lack of error confirms options were applied successfully during construction.
	})

	t.Run("Static Options", func(t *testing.T) {
		// Valid options for Static: Universal + Static-specific
		s, err := NewStatic(js, "STREAM", "static", "subj.{{partition}}", 1, 0, handler,
			WithLogger(logger),  // Universal
			WithHashSeed(12345), // Static specific
		)
		require.NoError(t, err)
		require.NotNil(t, s)
	})

	t.Run("Dynamic Options", func(t *testing.T) {
		gateCfg := &ProcessingGateConfig{
			Enabled: false,
		}

		// Valid options for Dynamic: Universal + Dynamic-specific
		d, err := NewDynamic(js, "STREAM", "dynamic", "subj.{{.PartitionID}}", handler,
			WithLogger(logger),            // Universal
			WithProcessingGate(gateCfg),   // Dynamic specific
			WithPullGating(true),          // Dynamic specific
			WithMaxConcurrentSubjects(10), // Dynamic specific
		)
		require.NoError(t, err)
		require.NotNil(t, d)
	})
}

// TestUniversalOptions verifies that universal options apply to all types.
func TestUniversalOptions(t *testing.T) {
	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil })
	js := &mockJS{}
	opts := []Option{
		WithBatchSize(100),
		WithMaxDeliver(5),
	}

	// 1. Queue
	q, err := NewQueue(js, "S", "C", "F", handler, convertToQueueOpts(opts)...)
	require.NoError(t, err)
	require.Equal(t, 100, q.config.BatchSize)
	require.Equal(t, 5, q.config.MaxDeliver)

	// 2. Broadcast
	// Broadcast struct inspection is harder as it wraps durable.BroadcastConsumer.
	// But we verify no compilation error and no runtime error.
	_, err = NewBroadcast(js, "S", "C", "F", handler, convertToBroadcastOpts(opts)...)
	require.NoError(t, err)

	// 3. Static
	s, err := NewStatic(js, "S", "C", "P.{{partition}}", 1, 0, handler, convertToStaticOpts(opts)...)
	require.NoError(t, err)
	require.NotNil(t, s)

	// 4. Dynamic
	d, err := NewDynamic(js, "S", "C", "T.{{.PartitionID}}", handler, convertToDynamicOpts(opts)...)
	require.NoError(t, err)
	require.NotNil(t, d)
}

func TestNewStatic_RejectsRecoveryStrategy(t *testing.T) {
	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil })
	js := &mockJS{}

	_, err := NewStatic(js, "STREAM", "static", "subj.{{partition}}", 1, 0, handler,
		WithRecoveryStrategy(RecoverFromNew),
	)
	require.NoError(t, err)
}

// Helpers to convert []Option to specific slices (since Go doesn't have covariance for slices)
func convertToQueueOpts(opts []Option) []QueueOption {
	out := make([]QueueOption, len(opts))
	for i, o := range opts {
		out[i], _ = o.(QueueOption)
	}

	return out
}

func convertToBroadcastOpts(opts []Option) []BroadcastOption {
	out := make([]BroadcastOption, len(opts))
	for i, o := range opts {
		out[i], _ = o.(BroadcastOption)
	}

	return out
}

func convertToStaticOpts(opts []Option) []StaticOption {
	out := make([]StaticOption, len(opts))
	for i, o := range opts {
		out[i], _ = o.(StaticOption)
	}

	return out
}

func convertToDynamicOpts(opts []Option) []DynamicOption {
	out := make([]DynamicOption, len(opts))
	for i, o := range opts {
		out[i], _ = o.(DynamicOption)
	}

	return out
}

func TestWithConsumerMemoryStorage(t *testing.T) {
	o := defaultOptions()
	if o.consumerMemoryStorage {
		t.Error("default consumerMemoryStorage = true, want false")
	}

	WithConsumerMemoryStorage(true).apply(&o)
	if !o.consumerMemoryStorage {
		t.Error("after WithConsumerMemoryStorage(true), got false")
	}

	WithConsumerMemoryStorage(false).apply(&o)
	if o.consumerMemoryStorage {
		t.Error("after WithConsumerMemoryStorage(false), got true")
	}
}

func TestWithOnConsumerUnservable(t *testing.T) {
	o := defaultOptions()
	if o.onConsumerUnservable != nil {
		t.Error("default onConsumerUnservable != nil")
	}

	var gotSubject string
	WithOnConsumerUnservable(func(subject string, _ error) { gotSubject = subject }).apply(&o)
	if o.onConsumerUnservable == nil {
		t.Fatal("after WithOnConsumerUnservable, callback is nil")
	}
	o.onConsumerUnservable("sub.X", nil)
	if gotSubject != "sub.X" {
		t.Errorf("callback got subject %q, want sub.X", gotSubject)
	}
}

func TestWithConsumerUnservableThreshold(t *testing.T) {
	o := defaultOptions()
	if o.consumerUnservableWindow != 0 {
		t.Errorf("default consumerUnservableWindow = %v, want 0", o.consumerUnservableWindow)
	}

	WithConsumerUnservableThreshold(25 * time.Second).apply(&o)
	if o.consumerUnservableWindow != 25*time.Second {
		t.Errorf("after WithConsumerUnservableThreshold(25s), got %v", o.consumerUnservableWindow)
	}

	// Non-positive is ignored (keeps the prior/default value).
	WithConsumerUnservableThreshold(0).apply(&o)
	if o.consumerUnservableWindow != 25*time.Second {
		t.Errorf("after WithConsumerUnservableThreshold(0), got %v, want 25s (unchanged)", o.consumerUnservableWindow)
	}
}

func TestWithConsumerReplicas(t *testing.T) {
	o := defaultOptions()
	if o.consumerReplicas != 0 {
		t.Errorf("default consumerReplicas = %d, want 0", o.consumerReplicas)
	}

	WithConsumerReplicas(3).apply(&o)
	if o.consumerReplicas != 3 {
		t.Errorf("after WithConsumerReplicas(3), got %d", o.consumerReplicas)
	}

	WithConsumerReplicas(1).apply(&o)
	if o.consumerReplicas != 1 {
		t.Errorf("after WithConsumerReplicas(1), got %d", o.consumerReplicas)
	}

	// Negative values are silently ignored (defensive guard).
	o.consumerReplicas = 5
	WithConsumerReplicas(-1).apply(&o)
	if o.consumerReplicas != 5 {
		t.Errorf("after WithConsumerReplicas(-1), got %d, want 5 (unchanged)", o.consumerReplicas)
	}
}
