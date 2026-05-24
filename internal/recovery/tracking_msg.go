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
//
// epoch is the streamEpoch captured at dispatch time. AdvanceCheckpoint compares
// the captured epoch against the controller's current streamEpoch and silently
// drops the advance when they differ — fencing late acks from a prior stream
// incarnation.
type trackingMsg struct {
	jetstream.Msg
	controller *Controller
	epoch      uint64
}

// Ack calls the underlying Ack and, on success, advances the checkpoint.
// The dispatch-time epoch is passed through so the controller can fence
// acks from a prior stream generation.
func (m *trackingMsg) Ack() error {
	err := m.Msg.Ack()
	if err == nil {
		m.controller.AdvanceCheckpoint(m.Msg, m.epoch)
	}

	return err
}

// DoubleAck calls the underlying DoubleAck and, on success, advances the checkpoint.
func (m *trackingMsg) DoubleAck(ctx context.Context) error {
	err := m.Msg.DoubleAck(ctx)
	if err == nil {
		m.controller.AdvanceCheckpoint(m.Msg, m.epoch)
	}

	return err
}
