package partition

import (
	"context"

	"github.com/nats-io/nats.go"
)

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
