package durable

import (
"context"

"github.com/nats-io/nats.go/jetstream"
)

// messageHandler is the internal contract for processing JetStream messages.
//
// It is not part of the public API; callers pass a plain
// func(context.Context, jetstream.Msg) error to the consumer constructors.
//
// Behavior summary:
//   - The pull loop calls Handle once per fetched message.
//   - By default, when Handle returns nil, the helper automatically ACKs.
//     When Handle returns a non-nil error, the helper NAKs the message.
//   - If ManualAck is enabled, the handler must call msg.Ack/Nak/Term and
//     may call msg.InProgress() periodically to extend AckWait.
//
// Backpressure and concurrency:
//   - The pull loop is single-threaded; it does not call Handle for the next
//     message until the current Handle returns.
//
// Redelivery semantics:
//   - With AckExplicit policy, failing to ACK within AckWait causes redelivery.
//   - Exactly-once is not guaranteed; design handlers to be idempotent.
type messageHandler interface {
// Handle processes a single message.
Handle(ctx context.Context, msg jetstream.Msg) error
}

// messageHandlerFunc is a function adapter for messageHandler.
type messageHandlerFunc func(ctx context.Context, msg jetstream.Msg) error

// Handle implements messageHandler.
func (f messageHandlerFunc) Handle(ctx context.Context, msg jetstream.Msg) error { return f(ctx, msg) }
