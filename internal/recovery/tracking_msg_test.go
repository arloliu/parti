package recovery

import (
	"context"
	"errors"
	"testing"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/stretchr/testify/require"
)

// --- WrapForTracking ---

func TestWrapForTracking_NilController_ReturnsOriginal(t *testing.T) {
	msg := &mockMsg{seq: 1}
	var c *Controller

	got := c.WrapForTracking(msg)
	tm, ok := got.(*mockMsg)
	require.True(t, ok)
	require.Same(t, msg, tm, "nil controller must return the original message unchanged")
}

func TestWrapForTracking_DisabledStrategy_ReturnsOriginal(t *testing.T) {
	// NewController returns nil for Disabled strategy.
	c := NewController(ControllerConfig{Strategy: Disabled, Logger: logging.NewNop()})
	require.Nil(t, c)

	msg := &mockMsg{seq: 1}
	got := c.WrapForTracking(msg) // nil receiver is safe
	tm, ok := got.(*mockMsg)
	require.True(t, ok)
	require.Same(t, msg, tm)
}

func TestWrapForTracking_FromNew_ReturnsOriginal(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromNew, Logger: logging.NewNop()})
	msg := &mockMsg{seq: 1}
	got := c.WrapForTracking(msg)
	tm, ok := got.(*mockMsg)
	require.True(t, ok)
	require.Same(t, msg, tm, "non-FromLastProcessed strategy must return original message")
}

func TestWrapForTracking_FromBeginning_ReturnsOriginal(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromBeginning, Logger: logging.NewNop()})
	msg := &mockMsg{seq: 1}
	got := c.WrapForTracking(msg)
	tm, ok := got.(*mockMsg)
	require.True(t, ok)
	require.Same(t, msg, tm)
}

func TestWrapForTracking_FromLastProcessed_ReturnsWrapper(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})
	msg := &mockMsg{seq: 5}
	got := c.WrapForTracking(msg)

	tm, ok := got.(*trackingMsg)
	require.True(t, ok, "FromLastProcessed should return a *trackingMsg")
	require.Equal(t, msg, tm.Msg)
	require.Equal(t, c, tm.controller)
}

// --- trackingMsg.Ack ---

func TestTrackingMsg_Ack_AdvancesCheckpoint(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})

	msg := &mockMsg{seq: 10}
	wrapped := c.WrapForTracking(msg)

	require.Equal(t, uint64(0), c.checkpoint.Value(), "checkpoint starts at zero")

	err := wrapped.Ack()
	require.NoError(t, err)
	require.Equal(t, uint64(10), c.checkpoint.Value(), "checkpoint must advance to seq 10 after Ack")
}

func TestTrackingMsg_Ack_ErrorDoesNotAdvanceCheckpoint(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})

	msg := &mockMsg{seq: 10, ackErr: errors.New("ack failed")}
	wrapped := c.WrapForTracking(msg)

	err := wrapped.Ack()
	require.Error(t, err)
	require.Equal(t, uint64(0), c.checkpoint.Value(), "failed Ack must not advance checkpoint")
}

// --- trackingMsg.DoubleAck ---

func TestTrackingMsg_DoubleAck_AdvancesCheckpoint(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})

	msg := &mockMsg{seq: 20}
	wrapped := c.WrapForTracking(msg)

	err := wrapped.DoubleAck(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(20), c.checkpoint.Value(), "checkpoint must advance after DoubleAck")
}

func TestTrackingMsg_DoubleAck_ErrorDoesNotAdvanceCheckpoint(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})

	msg := &mockMsg{seq: 20, doubleAckErr: errors.New("double ack failed")}
	wrapped := c.WrapForTracking(msg)

	err := wrapped.DoubleAck(context.Background())
	require.Error(t, err)
	require.Equal(t, uint64(0), c.checkpoint.Value())
}

// --- non-ack methods are promoted (no checkpoint side-effect) ---

func TestTrackingMsg_Nak_DoesNotAdvanceCheckpoint(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})

	msg := &mockMsg{seq: 5}
	wrapped := c.WrapForTracking(msg)

	require.NoError(t, wrapped.Nak())
	require.Equal(t, uint64(0), c.checkpoint.Value())
}

func TestTrackingMsg_Term_DoesNotAdvanceCheckpoint(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})

	msg := &mockMsg{seq: 5}
	wrapped := c.WrapForTracking(msg)

	require.NoError(t, wrapped.Term())
	require.Equal(t, uint64(0), c.checkpoint.Value())
}

// --- promoted accessors pass through correctly ---

func TestTrackingMsg_PassthroughAccessors(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})

	msg := &mockMsg{seq: 7}
	wrapped := c.WrapForTracking(msg)

	require.Equal(t, msg.Data(), wrapped.Data())
	require.Equal(t, msg.Subject(), wrapped.Subject())
	require.Equal(t, msg.Reply(), wrapped.Reply())
	require.Equal(t, msg.Headers(), wrapped.Headers())

	meta, err := wrapped.Metadata()
	require.NoError(t, err)
	require.Equal(t, uint64(7), meta.Sequence.Stream)
}

// --- monotonic: second Ack with lower seq is a no-op ---

func TestTrackingMsg_Ack_Monotonic(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})

	high := c.WrapForTracking(&mockMsg{seq: 50})
	low := c.WrapForTracking(&mockMsg{seq: 10})

	require.NoError(t, high.Ack())
	require.Equal(t, uint64(50), c.checkpoint.Value())

	require.NoError(t, low.Ack())
	require.Equal(t, uint64(50), c.checkpoint.Value(), "lower seq must not regress checkpoint")
}
