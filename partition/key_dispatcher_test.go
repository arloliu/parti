package partition

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
)

// mockMsg implements jetstream.Msg for testing.
type mockMsg struct {
	subject string
	acked   atomic.Bool
	naked   atomic.Bool
}

func (m *mockMsg) Subject() string                           { return m.subject }
func (m *mockMsg) Data() []byte                              { return nil }
func (m *mockMsg) Headers() nats.Header                      { return nil }
func (m *mockMsg) Reply() string                             { return "" }
func (m *mockMsg) Metadata() (*jetstream.MsgMetadata, error) { return nil, nil } //nolint:nilnil // mock
func (m *mockMsg) Ack() error                                { m.acked.Store(true); return nil }
func (m *mockMsg) DoubleAck(context.Context) error           { return nil }
func (m *mockMsg) Nak() error                                { m.naked.Store(true); return nil }
func (m *mockMsg) NakWithDelay(time.Duration) error          { return nil }
func (m *mockMsg) InProgress() error                         { return nil }
func (m *mockMsg) Term() error                               { return nil }
func (m *mockMsg) TermWithReason(string) error               { return nil }

// testKeyExtractor extracts the last token from the subject as the key.
// This is a test helper that mimics the pattern-aware extraction for simple cases.
func testKeyExtractor(msg jetstream.Msg) string {
	subject := msg.Subject()
	if idx := strings.LastIndex(subject, "."); idx >= 0 && idx < len(subject)-1 {
		return subject[idx+1:]
	}
	return subject
}

func TestKeyDispatcher_DispatchByKey(t *testing.T) {
	logger := logging.NewNop()

	var mu sync.Mutex
	processed := make(map[string][]string) // key -> list of subjects processed

	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		key := testKeyExtractor(msg)
		mu.Lock()
		processed[key] = append(processed[key], msg.Subject())
		mu.Unlock()
		return nil
	})

	kd := newKeyDispatcher(logger, metrics.NewNop(), handler, testKeyExtractor, 16, 100*time.Millisecond, false)
	defer func() {
		_ = kd.Close(t.Context())
	}()

	// Dispatch messages for different keys
	ctx := t.Context()
	messages := []string{
		"events.0.key-a",
		"events.0.key-b",
		"events.0.key-a",
		"events.0.key-c",
		"events.0.key-b",
	}

	for _, subj := range messages {
		msg := &mockMsg{subject: subj}
		ok := kd.Dispatch(ctx, msg)
		require.True(t, ok)
	}

	// Wait for processing
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	// Verify each key was processed
	assert.Len(t, processed["key-a"], 2)
	assert.Len(t, processed["key-b"], 2)
	assert.Len(t, processed["key-c"], 1)

	// Verify we created workers for each key
	assert.Equal(t, 3, kd.ActiveKeys())
}

func TestKeyDispatcher_PerKeyOrdering(t *testing.T) {
	logger := logging.NewNop()

	var mu sync.Mutex
	order := make(map[string][]int) // key -> order of processing

	var counter atomic.Int32
	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		key := testKeyExtractor(msg)
		idx := int(counter.Add(1))

		// Simulate variable processing time
		time.Sleep(time.Duration(5+idx%3) * time.Millisecond)

		mu.Lock()
		order[key] = append(order[key], idx)
		mu.Unlock()

		return nil
	})

	kd := newKeyDispatcher(logger, metrics.NewNop(), handler, testKeyExtractor, 64, 100*time.Millisecond, false)
	defer func() {
		_ = kd.Close(t.Context())
	}()

	ctx := t.Context()

	// Send 10 messages for key-a, 10 for key-b
	for i := range 20 {
		var subj string
		if i%2 == 0 {
			subj = "events.0.key-a"
		} else {
			subj = "events.0.key-b"
		}
		msg := &mockMsg{subject: subj}
		ok := kd.Dispatch(ctx, msg)
		require.True(t, ok)
	}

	// Wait for all processing
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	// Verify ordering within each key is preserved (monotonically increasing)
	for key, indices := range order {
		for i := 1; i < len(indices); i++ {
			assert.Less(t, indices[i-1], indices[i],
				"key %s should have monotonic processing order", key)
		}
	}
}

func TestKeyDispatcher_IdleTimeout(t *testing.T) {
	logger := logging.NewNop()
	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		return nil
	})

	// Short idle timeout for testing
	kd := newKeyDispatcher(logger, metrics.NewNop(), handler, testKeyExtractor, 16, 50*time.Millisecond, false)
	defer func() {
		_ = kd.Close(t.Context())
	}()

	ctx := t.Context()

	// Dispatch a message
	msg := &mockMsg{subject: "events.0.temp-key"}
	ok := kd.Dispatch(ctx, msg)
	require.True(t, ok)

	// Wait for processing
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, 1, kd.ActiveKeys())

	// Wait for idle timeout
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, kd.ActiveKeys(), "worker should exit after idle timeout")
}

func TestKeyDispatcher_Close(t *testing.T) {
	logger := logging.NewNop()

	var processed atomic.Int32
	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		time.Sleep(10 * time.Millisecond)
		processed.Add(1)
		return nil
	})

	kd := newKeyDispatcher(logger, metrics.NewNop(), handler, testKeyExtractor, 16, 1*time.Second, false)

	ctx := t.Context()

	// Dispatch several messages
	for i := range 5 {
		msg := &mockMsg{subject: "events.0.key-" + string(rune('a'+i))}
		kd.Dispatch(ctx, msg)
	}

	// Close should wait for workers
	closeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	err := kd.Close(closeCtx)
	require.NoError(t, err)

	assert.Equal(t, int32(5), processed.Load(), "all messages should be processed before close")
	assert.Equal(t, 0, kd.ActiveKeys())
}

func TestKeyDispatcher_Backpressure(t *testing.T) {
	logger := logging.NewNop()

	// Handler that blocks
	blockCh := make(chan struct{})
	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		<-blockCh
		return nil
	})

	// Small buffer of 2
	kd := newKeyDispatcher(logger, metrics.NewNop(), handler, testKeyExtractor, 2, 1*time.Second, false)
	defer func() {
		close(blockCh)
		_ = kd.Close(t.Context())
	}()

	ctx := t.Context()

	// First 3 messages should go through (1 processing + 2 buffered)
	for i := range 3 {
		msg := &mockMsg{subject: "events.0.same-key"}
		ok := kd.Dispatch(ctx, msg)
		require.True(t, ok, "message %d should be dispatched", i)
	}

	// Fourth message should block (buffer full, handler blocked)
	dispatchDone := make(chan bool)
	go func() {
		msg := &mockMsg{subject: "events.0.same-key"}
		ok := kd.Dispatch(ctx, msg)
		dispatchDone <- ok
	}()

	// Verify dispatch is blocked
	select {
	case <-dispatchDone:
		t.Fatal("dispatch should be blocked due to backpressure")
	case <-time.After(50 * time.Millisecond):
		// Expected - dispatch is blocked
	}
}

func TestKeyDispatcher_CustomKeyExtractor(t *testing.T) {
	logger := logging.NewNop()

	var mu sync.Mutex
	processed := make(map[string]int)

	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		// Use custom extractor's result for tracking
		mu.Lock()
		processed[msg.Subject()]++
		mu.Unlock()
		return nil
	})

	// Custom extractor: use first token as key
	customExtractor := func(msg jetstream.Msg) string {
		subj := msg.Subject()
		if idx := indexByte(subj, '.'); idx >= 0 {
			return subj[:idx]
		}
		return subj
	}

	kd := newKeyDispatcher(logger, metrics.NewNop(), handler, customExtractor, 16, 100*time.Millisecond, false)
	defer func() {
		_ = kd.Close(t.Context())
	}()

	ctx := t.Context()

	// These should all go to the same "events" key worker
	messages := []string{
		"events.a.1",
		"events.b.2",
		"events.c.3",
	}

	for _, subj := range messages {
		msg := &mockMsg{subject: subj}
		kd.Dispatch(ctx, msg)
	}

	// Wait for processing
	time.Sleep(50 * time.Millisecond)

	// Should have only 1 active key (all use "events")
	assert.Equal(t, 1, kd.ActiveKeys())
}

func TestKeyDispatcher_ManualAck(t *testing.T) {
	logger := logging.NewNop()

	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		// Don't call ack - handler is responsible
		return nil
	})

	kd := newKeyDispatcher(logger, metrics.NewNop(), handler, testKeyExtractor, 16, 100*time.Millisecond, true)
	defer func() {
		_ = kd.Close(t.Context())
	}()

	msg := &mockMsg{subject: "events.0.key"}
	kd.Dispatch(t.Context(), msg)

	time.Sleep(20 * time.Millisecond)

	// With manualAck, dispatcher should not call ack/nak
	assert.False(t, msg.acked.Load())
	assert.False(t, msg.naked.Load())
}

func TestKeyDispatcher_AutoAck(t *testing.T) {
	logger := logging.NewNop()

	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		return nil // success
	})

	kd := newKeyDispatcher(logger, metrics.NewNop(), handler, testKeyExtractor, 16, 100*time.Millisecond, false)
	defer func() {
		_ = kd.Close(t.Context())
	}()

	msg := &mockMsg{subject: "events.0.key"}
	kd.Dispatch(t.Context(), msg)

	time.Sleep(20 * time.Millisecond)

	// With auto-ack, successful handler should ack
	assert.True(t, msg.acked.Load())
	assert.False(t, msg.naked.Load())
}

func TestKeyDispatcher_AutoNak(t *testing.T) {
	logger := logging.NewNop()

	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		return assert.AnError // failure
	})

	kd := newKeyDispatcher(logger, metrics.NewNop(), handler, testKeyExtractor, 16, 100*time.Millisecond, false)
	defer func() {
		_ = kd.Close(t.Context())
	}()

	msg := &mockMsg{subject: "events.0.key"}
	kd.Dispatch(t.Context(), msg)

	time.Sleep(20 * time.Millisecond)

	// With auto-ack, failed handler should nak
	assert.False(t, msg.acked.Load())
	assert.True(t, msg.naked.Load())
}

// Helper to find byte index
func indexByte(s string, b byte) int {
	for i := range len(s) {
		if s[i] == b {
			return i
		}
	}
	return -1
}
