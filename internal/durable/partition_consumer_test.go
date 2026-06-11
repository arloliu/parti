package durable

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestPartitionConsumer_Run_ProcessAndExit(t *testing.T) {
	// Setup
	logger := logging.NewNop()
	cfg := partitionConsumerConfig{
		BatchSize:    1,
		FetchTimeout: 100 * time.Millisecond,
	}

	// Mock iterator
	mockIter := &mockMessagesContext{
		msgs: []jetstream.Msg{
			&mockMsg{data: []byte("msg1")},
			&mockMsg{data: []byte("msg2")},
		},
		done: make(chan struct{}),
	}

	iterFactory := func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
		return mockIter, nil
	}

	pc := newPartitionConsumer(
		logger,
		nil, // js not needed if iterFactory is mocked and no escalation
		cfg,
		partitionConsumerOpts{
			streamName:           "stream",
			durableName:          "durable",
			subject:              "subject",
			partitionID:          "partitionID",
			consumerConfig:       jetstream.ConsumerConfig{},
			consumer:             nil, // consumer not needed if iterFactory is mocked
			iterFactory:          iterFactory,
			checkPullSuppression: nil, // no suppression
		},
	)

	// Handler
	processed := make([]string, 0)
	var mu sync.Mutex
	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		mu.Lock()
		processed = append(processed, string(msg.Data()))
		mu.Unlock()
		return nil
	})

	// Run in background
	ctx, cancel := context.WithCancel(context.Background())
	go pc.Run(ctx, handler)

	// Wait for processing
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(processed) == 2
	}, 1*time.Second, 10*time.Millisecond)

	// Stop
	cancel()
	pc.Wait()

	require.Equal(t, []string{"msg1", "msg2"}, processed)
}

// Mocks
type mockMessagesContext struct {
	msgs []jetstream.Msg
	idx  int
	mu   sync.Mutex
	done chan struct{}
}

func (m *mockMessagesContext) Next(opts ...jetstream.NextOpt) (jetstream.Msg, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idx >= len(m.msgs) {
		// Block until stopped
		m.mu.Unlock()
		<-m.done
		m.mu.Lock()
		return nil, context.Canceled
	}
	msg := m.msgs[m.idx]
	m.idx++

	return msg, nil
}

func (m *mockMessagesContext) Stop() {
	select {
	case <-m.done:
	default:
		close(m.done)
	}
}

func (m *mockMessagesContext) Drain() {}

type mockMsg struct {
	data []byte
}

func (m *mockMsg) Data() []byte                           { return m.data }
func (m *mockMsg) Ack() error                             { return nil }
func (m *mockMsg) DoubleAck(context.Context) error        { return nil }
func (m *mockMsg) Nak() error                             { return nil }
func (m *mockMsg) NakWithDelay(delay time.Duration) error { return nil }
func (m *mockMsg) Term() error                            { return nil }
func (m *mockMsg) TermWithReason(reason string) error     { return nil }
func (m *mockMsg) InProgress() error                      { return nil }
func (m *mockMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return nil, errors.New("not implemented")
}
func (m *mockMsg) Subject() string      { return "" }
func (m *mockMsg) Reply() string        { return "" }
func (m *mockMsg) Headers() nats.Header { return nil }

// --- processIterator ErrMsgIteratorClosed filter test ---

func TestPartitionConsumer_ProcessIterator_FiltersGracefulErrors(t *testing.T) {
	logger := logging.NewNop()
	cfg := partitionConsumerConfig{
		BatchSize:    1,
		FetchTimeout: 100 * time.Millisecond,
	}

	tests := []struct {
		name    string
		err     error
		wantErr bool
	}{
		{"ErrMsgIteratorClosed is filtered", jetstream.ErrMsgIteratorClosed, false},
		{"context.Canceled is filtered", context.Canceled, false},
		{"ErrConsumerDeleted is returned", jetstream.ErrConsumerDeleted, true},
		{"ErrNoHeartbeat is returned", jetstream.ErrNoHeartbeat, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create an iterator that returns the test error on first Next().
			mockIter := &errorOnNextIter{err: tt.err}

			pc := newPartitionConsumer(
				logger,
				nil,
				cfg,
				partitionConsumerOpts{
					streamName:  "stream",
					durableName: "durable",
					subject:     "subject",
					partitionID: "pid",
				},
			)

			handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
				return nil
			})

			ctx := t.Context()

			exit, iterErr := pc.processIterator(ctx, mockIter, handler)
			require.False(t, exit, "should not signal exit for iterator error")
			if tt.wantErr {
				require.ErrorIs(t, iterErr, tt.err)
			} else {
				require.NoError(t, iterErr, "graceful error should be filtered to nil")
			}
		})
	}
}

// TestPartitionConsumerDelayWithBackoff_BackoffGrows verifies that repeated calls to
// delayWithBackoffOrExit produce growing delays (the previous delay is threaded through,
// not reset to 0 each call).
//
// Pre-fix: every call passed prev=0 into jitterBackoff, so the delay was always Base
// regardless of how many times the helper was called.
func TestPartitionConsumerDelayWithBackoff_BackoffGrows(t *testing.T) {
	const (
		base     = 1 * time.Millisecond
		mult     = 2.0
		maxDelay = 8 * time.Millisecond
		seed     = int64(42)
		calls    = 6
	)

	cfg := partitionConsumerConfig{
		BatchSize:    1,
		FetchTimeout: 100 * time.Millisecond,
		Retry: RetryConfig{
			Base:       base,
			Multiplier: mult,
			Max:        maxDelay,
			Seed:       seed,
		},
	}

	pc := newPartitionConsumer(
		logging.NewNop(),
		nil,
		cfg,
		partitionConsumerOpts{
			streamName:  "stream",
			durableName: "durable",
			subject:     "subject",
			partitionID: "pid",
		},
	)

	ctx := context.Background()
	delays := make([]time.Duration, 0, calls)
	for range calls {
		delayCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		pc.delayWithBackoffOrExit(delayCtx, "test")
		cancel()
		delays = append(delays, pc.retryPrev)
	}

	require.Len(t, delays, calls)

	// Every delay must be in [Base, Max].
	for i, d := range delays {
		require.GreaterOrEqual(t, d, base, "delay[%d] below Base", i)
		require.LessOrEqual(t, d, maxDelay, "delay[%d] exceeds Max", i)
	}

	// Growth: last delay must exceed first (with a fixed seed this is deterministic).
	require.Greater(t, delays[calls-1], delays[0],
		"expected backoff to grow over %d calls; delays: %v", calls, delays)
}

// --- mock helpers ---

// errorOnNextIter returns an error immediately on Next().
type errorOnNextIter struct {
	err error
}

func (e *errorOnNextIter) Next(opts ...jetstream.NextOpt) (jetstream.Msg, error) {
	return nil, e.err
}

func (e *errorOnNextIter) Stop() {}

func (e *errorOnNextIter) Drain() {}
