package subscription

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
