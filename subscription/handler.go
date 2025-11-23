package subscription

import (
	"context"

	"github.com/nats-io/nats.go/jetstream"
)

// MessageHandler defines the contract for processing JetStream messages yielded by the
// WorkerConsumer pull loop.
//
// Behavior summary:
//   - The helper fetches a message and calls Handle once per message.
//   - By default, when Handle returns nil, the helper automatically ACKs the message.
//     When Handle returns a non-nil error, the helper NAKs the message.
//   - If ManualAck is enabled in WorkerConsumerConfig, the helper does not perform
//     any disposition; the handler is responsible for calling msg.Ack/Nak/Term and
//     may call msg.InProgress() periodically to extend AckWait while processing.
//
// Backpressure and concurrency:
//   - The pull loop is single-threaded: it does not call Handle for the next message
//     until the current Handle returns. Handlers can implement backpressure by blocking
//     (e.g., when an internal work queue is full). In ManualAck mode, returning quickly
//     is safe only if the handler arranges for a background worker to take over and
//     eventually ACK/Nak/Term.
//   - If you return quickly without ManualAck, the helper will immediately ACK on
//     success, so calling InProgress() afterwards has no effect.
//
// Redelivery semantics:
//   - With AckExplicit policy, failing to ACK within AckWait causes redelivery.
//     Use msg.InProgress() to extend the deadline when work takes longer than AckWait.
//   - Exactly-once is not guaranteed; design handlers to be idempotent.
//
// Parameters:
//   - ctx: Request-scoped context for cancellation and deadlines
//   - msg: The JetStream message to process
//
// Returns:
//   - error: nil on success; non-nil to indicate failure. In default mode, the helper NAKs on error.
//
// Example (default mode):
//
//	var h MessageHandler = MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
//	    // process msg.Data(); return error to NAK
//	    return nil // helper will ACK
//	})
//
// Example (manual ack mode with background worker):
//
//	var h MessageHandler = MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
//	    // enqueue to internal queue; if full, block to apply backpressure
//	    if !enqueue(msg) { return context.DeadlineExceeded }
//	    return nil // do not Ack here; background worker will Ack/Nak/Term
//	})
type MessageHandler interface {
	// Handle processes a single message.
	//
	// In default mode, return nil for the helper to ACK, or an error for the helper to NAK.
	// In ManualAck mode, the handler must call msg.Ack/Nak/Term exactly once and may
	// periodically call msg.InProgress() to extend AckWait while long-running work completes.
	Handle(ctx context.Context, msg jetstream.Msg) error
}

// MessageHandlerFunc is a function adapter for MessageHandler.
type MessageHandlerFunc func(ctx context.Context, msg jetstream.Msg) error

// Handle implements MessageHandler interface.
func (f MessageHandlerFunc) Handle(ctx context.Context, msg jetstream.Msg) error { return f(ctx, msg) }
