package parti

import (
	"errors"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/types"
)

// isWholeBucketLoss reports whether err is a whole-bucket-loss surface: a
// connectivity failure or a degrading-JetStream (bucket/stream/consumer missing)
// error. It is the single source for what was a triplicated inline union — the
// degraded circuit admits it as a non-transient (accumulating) window entry,
// markKVUnavailable refuses to re-tag it as a transient timeout, and
// onClaimerError uses it to tell bucket loss from a peer takeover.
func isWholeBucketLoss(err error) bool {
	return natsutil.IsConnectivityError(err) || natsutil.IsDegradingJetStreamError(err)
}

// kvErrorRoute is how the degraded circuit routes a KV-op error.
type kvErrorRoute uint8

const (
	// kvRouteDrop: not admitted — nil, or an unclassified non-timeout error that
	// neither the connectivity/degrading classifiers nor markKVUnavailable claim.
	kvRouteDrop kvErrorRoute = iota
	// kvRouteStreamMissing: a stream-missing error owned by the dynamic-consumer
	// stream-missing observer (DegradeReasonStreamMissingRecoveryExhausted), NOT
	// the generic KV-error threshold. Kept out of this window so an incidental
	// jetstream.ErrStreamNotFound wrap cannot double-count or trip the threshold.
	kvRouteStreamMissing
	// kvRouteWindow: admitted to the degraded-circuit error window.
	kvRouteWindow
)

// kvErrorDecision is the pure routing of a (possibly markKVUnavailable-wrapped)
// KV-op error. For kvRouteWindow, transient distinguishes an F-D1
// connected-but-KV-unavailable timeout (clearable by a healthy op; degrades with
// DegradeReasonKVUnavailable) from a whole-bucket loss (accumulates to the
// threshold; degrades with DegradeReasonKVErrorThreshold). Whole-bucket loss is
// thus the ONLY path to the threshold reason — the AGENTS.md contract.
type kvErrorDecision struct {
	route     kvErrorRoute
	transient bool
}

// classifyKVError routes a KV-op error for the degraded circuit, preserving the
// precedence the inline checks encoded: nil and stream-missing are handled first,
// then the whole-bucket / ErrKVUnavailable admission union, else dropped. It does
// NOT wrap timeouts — that is markKVUnavailable's job, applied only on the
// recordKVOpError path before classification, so direct recordKVError callers
// (recovery refresh, bucket-loss-wrapped claim loss) keep their exact routing.
func classifyKVError(err error) kvErrorDecision {
	if err == nil {
		return kvErrorDecision{route: kvRouteDrop}
	}
	if errors.Is(err, types.ErrStreamMissing) {
		return kvErrorDecision{route: kvRouteStreamMissing}
	}

	kvUnavailable := errors.Is(err, ErrKVUnavailable)
	if !isWholeBucketLoss(err) && !kvUnavailable {
		return kvErrorDecision{route: kvRouteDrop}
	}

	return kvErrorDecision{route: kvRouteWindow, transient: kvUnavailable}
}
