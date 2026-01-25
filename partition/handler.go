package partition

import (
	"context"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// MessageHandler processes JetStream messages for a partition.
type MessageHandler interface {
	// Handle processes a JetStream message.
	Handle(ctx context.Context, msg jetstream.Msg) error
}

// MessageHandlerFunc adapts a function to MessageHandler.
type MessageHandlerFunc func(ctx context.Context, msg jetstream.Msg) error

// Handle implements MessageHandler.
func (f MessageHandlerFunc) Handle(ctx context.Context, msg jetstream.Msg) error {
	return f(ctx, msg)
}

// NATSMessageHandler processes core NATS messages for a partition.
type NATSMessageHandler interface {
	// Handle processes a core NATS message.
	Handle(ctx context.Context, msg *nats.Msg) error
}

// NATSMessageHandlerFunc adapts a function to NATSMessageHandler.
type NATSMessageHandlerFunc func(ctx context.Context, msg *nats.Msg) error

// Handle implements NATSMessageHandler.
func (f NATSMessageHandlerFunc) Handle(ctx context.Context, msg *nats.Msg) error {
	return f(ctx, msg)
}
