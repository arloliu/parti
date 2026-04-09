package recovery

import (
	"context"

	"github.com/nats-io/nats.go/jetstream"
)

// trackingMsg wraps a jetstream.Msg and intercepts Ack and DoubleAck to advance
// the recovery checkpoint. All other methods are promoted from the embedded interface.
//
// Used exclusively when ManualAck=true and the recovery strategy is FromLastProcessed.
// The handler receives a trackingMsg value typed as jetstream.Msg — transparent to the caller.
type trackingMsg struct {
	jetstream.Msg
	controller *Controller
}

// Ack calls the underlying Ack and, on success, advances the checkpoint.
func (m *trackingMsg) Ack() error {
	err := m.Msg.Ack()
	if err == nil {
		m.controller.AdvanceCheckpoint(m.Msg)
	}

	return err
}

// DoubleAck calls the underlying DoubleAck and, on success, advances the checkpoint.
func (m *trackingMsg) DoubleAck(ctx context.Context) error {
	err := m.Msg.DoubleAck(ctx)
	if err == nil {
		m.controller.AdvanceCheckpoint(m.Msg)
	}

	return err
}
