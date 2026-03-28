package partition

import (
	"context"

	"github.com/nats-io/nats.go/jetstream"
)

// messageHandler processes JetStream messages for a partition.
// Internal use only; callers of NewJSConsumer pass a plain func.
type messageHandler interface {
	// Handle processes a JetStream message.
	Handle(ctx context.Context, msg jetstream.Msg) error
}

// messageHandlerFunc adapts a function to messageHandler.
type messageHandlerFunc func(ctx context.Context, msg jetstream.Msg) error

// Handle implements messageHandler.
func (f messageHandlerFunc) Handle(ctx context.Context, msg jetstream.Msg) error {
	return f(ctx, msg)
}
