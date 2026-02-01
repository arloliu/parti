package consumer

import (
	"context"

	"github.com/nats-io/nats.go/jetstream"
)

// MessageHandler processes JetStream messages.
// This is the unified handler interface for all consumer types in this package.
type MessageHandler interface {
	// Handle processes a JetStream message.
	// Return nil to indicate successful processing; return an error for failures.
	// Ack/Nak behavior depends on the consumer's ManualAck setting:
	//   - ManualAck=false: nil → Ack, error → Nak (default behavior)
	//   - ManualAck=true: handler must call msg.Ack/Nak/Term explicitly
	Handle(ctx context.Context, msg jetstream.Msg) error
}

// MessageHandlerFunc adapts a function to MessageHandler.
type MessageHandlerFunc func(ctx context.Context, msg jetstream.Msg) error

// Handle implements MessageHandler.
func (f MessageHandlerFunc) Handle(ctx context.Context, msg jetstream.Msg) error {
	return f(ctx, msg)
}
