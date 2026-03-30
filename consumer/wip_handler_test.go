package consumer

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

type heartbeatMsg struct {
	inProgressCalls atomic.Int32
}

func (m *heartbeatMsg) Subject() string                           { return "subject" }
func (m *heartbeatMsg) Data() []byte                              { return nil }
func (m *heartbeatMsg) Headers() nats.Header                      { return nil }
func (m *heartbeatMsg) Reply() string                             { return "" }
func (m *heartbeatMsg) Metadata() (*jetstream.MsgMetadata, error) { return nil, nil } //nolint:nilnil // mock
func (m *heartbeatMsg) Ack() error                                { return nil }
func (m *heartbeatMsg) DoubleAck(context.Context) error           { return nil }
func (m *heartbeatMsg) Nak() error                                { return nil }
func (m *heartbeatMsg) NakWithDelay(time.Duration) error          { return nil }
func (m *heartbeatMsg) InProgress() error {
	m.inProgressCalls.Add(1)
	return nil
}
func (m *heartbeatMsg) Term() error                 { return nil }
func (m *heartbeatMsg) TermWithReason(string) error { return nil }

func TestNewWIPHandler_NilHandler(t *testing.T) {
	t.Parallel()

	got := NewWIPHandler(nil, WIPConfig{Interval: 10 * time.Second})
	require.Nil(t, got)
}

func TestNewWIPHandler_DisabledInterval(t *testing.T) {
	t.Parallel()

	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil })
	got := NewWIPHandler(handler, WIPConfig{Interval: 0})
	require.Equal(t, reflect.ValueOf(handler).Pointer(), reflect.ValueOf(got).Pointer())
}

func TestWIPHandler_HeartbeatsSent(t *testing.T) {
	interval := 25 * time.Millisecond
	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		time.Sleep(110 * time.Millisecond)
		return nil
	})

	// Use negative MinInterval to disable clamping for precise test timing
	wrapped := NewWIPHandler(handler, WIPConfig{
		Interval:    interval,
		MinInterval: -1, // Disable clamping
	})
	msg := &heartbeatMsg{}

	err := wrapped.Handle(t.Context(), msg)
	require.NoError(t, err)
	require.GreaterOrEqual(t, msg.inProgressCalls.Load(), int32(2))
}

func TestWIPHandler_IntervalClamping(t *testing.T) {
	t.Parallel()

	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		time.Sleep(250 * time.Millisecond)
		return nil
	})

	// Interval below default min should be clamped to 100ms
	wrapped := NewWIPHandler(handler, WIPConfig{Interval: 10 * time.Millisecond})
	wipHandler, ok := wrapped.(*WIPHandler)
	require.True(t, ok)
	require.Equal(t, DefaultWIPMinInterval, wipHandler.interval)

	// With custom min interval
	wrapped2 := NewWIPHandler(handler, WIPConfig{
		Interval:    30 * time.Millisecond,
		MinInterval: 50 * time.Millisecond,
	})
	wipHandler2, ok := wrapped2.(*WIPHandler)
	require.True(t, ok)
	require.Equal(t, 50*time.Millisecond, wipHandler2.interval)
}

func TestWIPHandler_ContextCancellation(t *testing.T) {
	t.Parallel()

	handlerStarted := make(chan struct{})
	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		close(handlerStarted)
		<-ctx.Done()
		return ctx.Err()
	})

	wrapped := NewWIPHandler(handler, WIPConfig{
		Interval:    25 * time.Millisecond,
		MinInterval: -1,
	})
	msg := &heartbeatMsg{}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		errCh <- wrapped.Handle(ctx, msg)
	}()

	<-handlerStarted
	require.Eventually(t, func() bool {
		return msg.inProgressCalls.Load() >= 1
	}, 200*time.Millisecond, 10*time.Millisecond, "expected at least one heartbeat before cancel")
	cancel()

	err := <-errCh
	require.ErrorIs(t, err, context.Canceled)
	require.GreaterOrEqual(t, msg.inProgressCalls.Load(), int32(1))
}

func TestWIPHandler_FastHandler_NoHeartbeat(t *testing.T) {
	t.Parallel()

	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		// Fast handler completes before interval
		return nil
	})

	wrapped := NewWIPHandler(handler, WIPConfig{
		Interval:    100 * time.Millisecond,
		MinInterval: -1,
	})
	msg := &heartbeatMsg{}

	err := wrapped.Handle(t.Context(), msg)
	require.NoError(t, err)
	// No heartbeat should have been sent
	require.Equal(t, int32(0), msg.inProgressCalls.Load())
}

func TestWIPHandler_HandlerError_Propagates(t *testing.T) {
	t.Parallel()

	expectedErr := errors.New("handler failed")
	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		time.Sleep(60 * time.Millisecond)
		return expectedErr
	})

	wrapped := NewWIPHandler(handler, WIPConfig{
		Interval:    25 * time.Millisecond,
		MinInterval: -1,
	})
	msg := &heartbeatMsg{}

	err := wrapped.Handle(t.Context(), msg)
	require.ErrorIs(t, err, expectedErr)
	// Heartbeat should have been sent at least once
	require.GreaterOrEqual(t, msg.inProgressCalls.Load(), int32(1))
}

func TestWIPHandler_NegativeInterval_Disabled(t *testing.T) {
	t.Parallel()

	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil })
	got := NewWIPHandler(handler, WIPConfig{Interval: -1 * time.Second})
	require.Equal(t, reflect.ValueOf(handler).Pointer(), reflect.ValueOf(got).Pointer())
}

type errorMsg struct {
	heartbeatMsg
	inProgressErr error
}

func (m *errorMsg) InProgress() error {
	m.inProgressCalls.Add(1)
	return m.inProgressErr
}

type testLogger struct {
	warnCalled atomic.Bool
	lastMsg    atomic.Value
}

func (l *testLogger) Debug(msg string, keysAndValues ...any) {}
func (l *testLogger) Info(msg string, keysAndValues ...any)  {}
func (l *testLogger) Warn(msg string, keysAndValues ...any) {
	l.warnCalled.Store(true)
	l.lastMsg.Store(msg)
}
func (l *testLogger) Error(msg string, keysAndValues ...any) {}
func (l *testLogger) Fatal(msg string, keysAndValues ...any) {}

func TestWIPHandler_InProgressError_Logged(t *testing.T) {
	t.Parallel()

	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		time.Sleep(80 * time.Millisecond)
		return nil
	})

	logger := &testLogger{}
	wrapped := NewWIPHandler(handler, WIPConfig{
		Interval:    25 * time.Millisecond,
		MinInterval: -1,
		Logger:      logger,
	})

	msg := &errorMsg{inProgressErr: errors.New("connection closed")}

	err := wrapped.Handle(t.Context(), msg)
	require.NoError(t, err)
	require.True(t, logger.warnCalled.Load())
	require.Equal(t, "failed to extend message ack deadline", logger.lastMsg.Load())
}

func TestWIPHandler_ConcurrentHandles(t *testing.T) {
	t.Parallel()

	var handleCount atomic.Int32
	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		handleCount.Add(1)
		time.Sleep(50 * time.Millisecond)
		return nil
	})

	wrapped := NewWIPHandler(handler, WIPConfig{
		Interval:    20 * time.Millisecond,
		MinInterval: -1,
	})

	const numGoroutines = 10
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for range numGoroutines {
		go func() {
			defer wg.Done()
			msg := &heartbeatMsg{}
			err := wrapped.Handle(t.Context(), msg)
			require.NoError(t, err)
			// Each message should get its own heartbeat
			require.GreaterOrEqual(t, msg.inProgressCalls.Load(), int32(1))
		}()
	}

	wg.Wait()
	require.Equal(t, int32(numGoroutines), handleCount.Load())
}
