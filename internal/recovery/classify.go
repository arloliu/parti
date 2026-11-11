package recovery

import (
	"context"
	"errors"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/nats-io/nats.go/jetstream"
)

// ErrorClass categorizes an iterator error for recovery decision-making.
type ErrorClass int

const (
	// ErrorTransient means the error is a temporary issue (retry with backoff).
	ErrorTransient ErrorClass = iota

	// ErrorConsumerGone means the consumer was unambiguously deleted
	// (ErrConsumerDeleted / 409). Recovery can proceed immediately.
	ErrorConsumerGone

	// ErrorNeedsConfirm means the error is ambiguous (ErrNoHeartbeat) and requires
	// a consumer.Info() confirmation to determine if the consumer is truly gone.
	ErrorNeedsConfirm

	// ErrorGracefulExit means the iterator was closed or context canceled.
	// The consumer loop should exit cleanly.
	ErrorGracefulExit
)

// ClassifyError categorizes an iterator error without side effects.
func ClassifyError(err error) ErrorClass {
	if err == nil {
		return ErrorGracefulExit
	}

	if errors.Is(err, jetstream.ErrMsgIteratorClosed) || errors.Is(err, context.Canceled) {
		return ErrorGracefulExit
	}

	if natsutil.IsConsumerGone(err) {
		return ErrorConsumerGone
	}

	if errors.Is(err, jetstream.ErrNoHeartbeat) {
		return ErrorNeedsConfirm
	}

	return ErrorTransient
}

// Action is the recommended action after the controller classifies an error.
type Action int

const (
	// ActionContinue means recovery succeeded; reset backoff and continue the loop.
	ActionContinue Action = iota

	// ActionBackoff means no recovery was needed or recovery failed; backoff and retry.
	ActionBackoff

	// ActionExit means the loop should exit (graceful shutdown).
	ActionExit

	// ActionStreamMissing means recovery detected that the underlying
	// JetStream stream is absent. The companion error returned by
	// Classify wraps types.ErrStreamMissing so callers can route the
	// signal to the operator-supplied StreamMissingHook (Dynamic
	// partition consumer) or log + backoff (Queue, Broadcast,
	// internal/ipartition — they do not own stream lifecycle).
	ActionStreamMissing
)

// KeepIterator reports whether a pull loop should carry on with the same iterator
// after iterErr was classified as action.
//
// It is true for a missed pull heartbeat that Classify answered with ActionBackoff:
// the consumer handle is unchanged and the stream was not found missing.
// That covers a miss that needs no recovery,
// and a recovery that was due but produced no new consumer on this attempt.
// nats.go has reset the iterator by the time it reports the miss,
// so the next Next call issues a fresh pull request.
// Stopping the iterator instead discards the messages the server already delivered to it;
// those come back only after AckWait, or never once MaxDeliver is reached.
func KeepIterator(iterErr error, action Action) bool {
	return action == ActionBackoff && errors.Is(iterErr, jetstream.ErrNoHeartbeat)
}
