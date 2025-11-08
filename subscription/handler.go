package subscription

import (
	"context"

	"github.com/nats-io/nats.go/jetstream"
)

// MessageHandler processes messages from JetStream consumers.
type MessageHandler interface {
	// Handle processes a message and returns error if processing failed.
	Handle(ctx context.Context, msg jetstream.Msg) error
}

// MessageHandlerFunc is a function adapter for MessageHandler.
type MessageHandlerFunc func(ctx context.Context, msg jetstream.Msg) error

// Handle implements MessageHandler interface.
func (f MessageHandlerFunc) Handle(ctx context.Context, msg jetstream.Msg) error { return f(ctx, msg) }
