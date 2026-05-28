package recovery

import (
	"context"
	"errors"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/types"
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

// ConfirmResult classifies the outcome of a consumer.Info() call made during the
// confirm path (after an ambiguous ErrNoHeartbeat burst). It is a lossless
// replacement for the previous boolean "is the consumer gone?" so the controller
// can route each distinct failure class to the correct owner without rerouting
// stream-missing / degrading / connectivity failures into the per-consumer
// unservable-notification path.
type ConfirmResult int

const (
	// ConfirmHealthy means the consumer exists and is serviceable (Info returned
	// no error and, for a replicated consumer, has a raft leader). Resets any
	// in-progress unservable episode.
	ConfirmHealthy ConfirmResult = iota

	// ConfirmGone means the consumer is unambiguously absent (ConsumerNotFound).
	// Routes to the existing recreate path.
	ConfirmGone

	// ConfirmDegrading means a bucket/stream/consumer is MISSING or the stream is
	// gone — owned by the manager's degraded / stream-missing routing, NOT by the
	// per-consumer unservable path. Must never be counted as unservable.
	ConfirmDegrading

	// ConfirmConnectivity means the NATS connection itself is impaired. Owned by
	// the connection monitor / manager degraded path; not counted as unservable.
	ConfirmConnectivity

	// ConfirmUnservable means the consumer EXISTS but its own raft group is
	// unavailable while the connection is up — a 503 / "JetStream system
	// temporarily unavailable" / no-quorum API error, NoResponders from the
	// consumer Info, or a leaderless ConsumerInfo. This is the only class that
	// drives the unservable-notification.
	ConfirmUnservable
)

// ClassifyConfirm maps a consumer.Info() result to a ConfirmResult. The order of
// checks is significant: ConsumerNotFound is matched before the degrading check
// (IsDegradingJetStreamError also matches consumer-not-found), and connectivity
// is matched before the unservable catch-all.
func ClassifyConfirm(ci *jetstream.ConsumerInfo, err error) ConfirmResult {
	if err == nil {
		// A replicated consumer with no elected leader is unservable. A
		// non-replicated (R1, nil Cluster) consumer with no error is healthy.
		if ci != nil && ci.Cluster != nil && ci.Cluster.Leader == "" {
			return ConfirmUnservable
		}

		return ConfirmHealthy
	}
	if natsutil.IsConsumerNotFound(err) {
		return ConfirmGone
	}
	if natsutil.IsDegradingJetStreamError(err) || errors.Is(err, types.ErrStreamMissing) {
		return ConfirmDegrading
	}
	if natsutil.IsConnectivityError(err) {
		return ConfirmConnectivity
	}

	// Non-NotFound, non-degrading, non-connectivity error while the connection is
	// up: the consumer's raft group is unavailable (503/no-quorum/NoResponders).
	return ConfirmUnservable
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
