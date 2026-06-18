package recovery

import (
	"context"
	"errors"
	"testing"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// --- WrapForTracking ---

func TestWrapForTracking(t *testing.T) {
	cases := []struct {
		name string
		// newController builds the receiver. A nil return exercises the
		// nil-receiver path (typed-nil *Controller or NewController(Disabled)).
		newController func(t *testing.T) *Controller
		// nilController, when true, uses a typed-nil *Controller rather than
		// invoking newController.
		nilController bool
		expectWrapper bool // true => *trackingMsg, false => original *mockMsg
	}{
		{
			name:          "NilController_ReturnsOriginal",
			nilController: true,
			expectWrapper: false,
		},
		{
			name: "DisabledStrategy_ReturnsOriginal",
			newController: func(t *testing.T) *Controller {
				// NewController returns nil for Disabled strategy.
				c := NewController(ControllerConfig{Strategy: Disabled, Logger: logging.NewNop()})
				require.Nil(t, c)

				return c // nil receiver is safe
			},
			expectWrapper: false,
		},
		{
			name: "FromNew_ReturnsOriginal",
			newController: func(t *testing.T) *Controller {
				return NewController(ControllerConfig{Strategy: FromNew, Logger: logging.NewNop()})
			},
			expectWrapper: false,
		},
		{
			name: "FromBeginning_ReturnsOriginal",
			newController: func(t *testing.T) *Controller {
				return NewController(ControllerConfig{Strategy: FromBeginning, Logger: logging.NewNop()})
			},
			expectWrapper: false,
		},
		{
			name: "FromLastProcessed_ReturnsWrapper",
			newController: func(t *testing.T) *Controller {
				return NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})
			},
			expectWrapper: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c *Controller
			if !tc.nilController {
				c = tc.newController(t)
			}

			seq := uint64(1)
			if tc.expectWrapper {
				seq = 5
			}
			msg := &mockMsg{seq: seq}

			got := c.WrapForTracking(msg)

			if tc.expectWrapper {
				tm, ok := got.(*trackingMsg)
				require.True(t, ok, "FromLastProcessed should return a *trackingMsg")
				require.Equal(t, msg, tm.Msg)
				require.Equal(t, c, tm.controller)

				return
			}

			tm, ok := got.(*mockMsg)
			require.True(t, ok)
			require.Same(t, msg, tm, "non-wrapping strategy must return the original message unchanged")
		})
	}
}

// --- trackingMsg checkpoint advancement ---

func TestTrackingMsg_CheckpointAdvancement(t *testing.T) {
	cases := []struct {
		name string
		// invoke performs the message method under test and returns its error.
		invoke func(wrapped jetstream.Msg) error
		// withErr, when true, configures the mock to fail the method.
		withErr  bool
		seq      uint64
		advances bool // true => checkpoint advances to seq; false => stays 0
	}{
		{
			name:     "Ack_AdvancesCheckpoint",
			invoke:   func(w jetstream.Msg) error { return w.Ack() },
			seq:      10,
			advances: true,
		},
		{
			name:     "Ack_ErrorDoesNotAdvanceCheckpoint",
			invoke:   func(w jetstream.Msg) error { return w.Ack() },
			withErr:  true,
			seq:      10,
			advances: false,
		},
		{
			name:     "DoubleAck_AdvancesCheckpoint",
			invoke:   func(w jetstream.Msg) error { return w.DoubleAck(context.Background()) },
			seq:      20,
			advances: true,
		},
		{
			name:     "DoubleAck_ErrorDoesNotAdvanceCheckpoint",
			invoke:   func(w jetstream.Msg) error { return w.DoubleAck(context.Background()) },
			withErr:  true,
			seq:      20,
			advances: false,
		},
		{
			name:     "Nak_DoesNotAdvanceCheckpoint",
			invoke:   func(w jetstream.Msg) error { return w.Nak() },
			seq:      5,
			advances: false,
		},
		{
			name:     "Term_DoesNotAdvanceCheckpoint",
			invoke:   func(w jetstream.Msg) error { return w.Term() },
			seq:      5,
			advances: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})

			msg := &mockMsg{seq: tc.seq}
			if tc.withErr {
				switch tc.name {
				case "Ack_ErrorDoesNotAdvanceCheckpoint":
					msg.ackErr = errors.New("ack failed")
				case "DoubleAck_ErrorDoesNotAdvanceCheckpoint":
					msg.doubleAckErr = errors.New("double ack failed")
				}
			}
			wrapped := c.WrapForTracking(msg)

			require.Equal(t, uint64(0), c.checkpoint.Value(), "checkpoint starts at zero")

			err := tc.invoke(wrapped)
			if tc.withErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			if tc.advances {
				require.Equal(t, tc.seq, c.checkpoint.Value(), "checkpoint must advance to seq after success")
			} else {
				require.Equal(t, uint64(0), c.checkpoint.Value(), "checkpoint must not advance")
			}
		})
	}
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
