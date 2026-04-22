package natsutil

import (
	"errors"
	"strings"

	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// IsConsumerGone reports whether err is an unambiguous "consumer deleted" signal
// from iter.Next() (HTTP 409). This is the fast path — no API confirmation needed.
//
// Do NOT use this for [jetstream.ErrNoHeartbeat]: that error is ambiguous (could be
// a transient network blip) and requires a [jetstream.Consumer.Info] check before
// triggering recovery.
func IsConsumerGone(err error) bool {
	return errors.Is(err, jetstream.ErrConsumerDeleted)
}

// IsConsumerNotFound reports whether err indicates a "consumer not found" error.
//
// Both sentinels are checked because the root nats package and the jetstream
// sub-package define separate, non-overlapping error types with the same
// semantics. In particular, [jetstream.KeyWatcher.Stop] goes through
// nats.Subscription.Unsubscribe → deleteConsumer, which returns the root
// nats sentinel, whereas other JetStream paths return the jetstream sentinel.
func IsConsumerNotFound(err error) bool {
	return errors.Is(err, nats.ErrConsumerNotFound) ||
		errors.Is(err, jetstream.ErrConsumerNotFound)
}

// IsStreamNotFound reports whether err indicates a "stream not found" error.
//
// Both the root nats and jetstream sentinels are checked for the same reason
// described in [IsConsumerNotFound].
func IsStreamNotFound(err error) bool {
	return errors.Is(err, nats.ErrStreamNotFound) ||
		errors.Is(err, jetstream.ErrStreamNotFound)
}

// IsDegradingJetStreamError reports whether err indicates that Parti's backing
// JetStream state is missing for reasons that should drive the manager into
// degraded mode if the condition is sustained.
//
// Distinct from [IsConnectivityError]: the NATS connection is up, but a bucket,
// stream, or consumer that Parti depends on has disappeared (operator wipe,
// non-replicated JetStream restart with lost data, peer promotion before Raft
// replication catches up). A single occurrence may be a transient cluster
// event; only the repeat-within-window semantics of the degraded-mode circuit
// should convert it into a state transition.
//
// Parameters:
//   - err: Error to check
//
// Returns:
//   - bool: true if err indicates missing JetStream state
func IsDegradingJetStreamError(err error) bool {
	if err == nil {
		return false
	}

	return errors.Is(err, jetstream.ErrBucketNotFound) ||
		IsStreamNotFound(err) ||
		IsConsumerNotFound(err)
}

// IsConnectivityError checks if an error is caused by connectivity issues.
//
// This includes NATS timeouts, connection refused, disconnections, etc.
// Used to determine when to enter degraded mode and use cached data.
//
// Kept in internal/natsutil to avoid importing NATS dependencies in types/ package.
//
// Parameters:
//   - err: Error to check
//
// Returns:
//   - bool: true if error indicates connectivity issue
func IsConnectivityError(err error) bool {
	if err == nil {
		return false
	}

	// Check for known connectivity error types via errors.Is (preferred).
	if errors.Is(err, types.ErrConnectivity) ||
		errors.Is(err, nats.ErrTimeout) ||
		errors.Is(err, nats.ErrNoServers) ||
		errors.Is(err, nats.ErrDisconnected) ||
		errors.Is(err, nats.ErrConnectionClosed) ||
		errors.Is(err, nats.ErrConnectionDraining) ||
		errors.Is(err, nats.ErrConnectionReconnecting) ||
		errors.Is(err, jetstream.ErrNoStreamResponse) {
		return true
	}

	// Fallback: OS-level socket errors that NATS surfaces as wrapped text.
	// These strings come from Go's net package, not NATS, so they are stable.
	errMsg := err.Error()

	return strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "i/o timeout")
}
