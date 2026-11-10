package natsutil

import (
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// MinPullHeartbeat and MaxPullHeartbeat are nats.go's validity bounds
// for an explicit jetstream.PullHeartbeat option: configureConsume and
// configureMessages both reject a value outside [500ms, 30s] with
// ErrInvalidOption (jetstream_options.go), and nats.go separately rejects a
// heartbeat greater than half the expiry ("must be less than 50% of expiry").
const (
	MinPullHeartbeat = 500 * time.Millisecond
	MaxPullHeartbeat = 30 * time.Second
)

// DerivePullHeartbeat computes the PullHeartbeat value for a pull expiry
// (FetchTimeout), optionally bounded further by heartbeatCap.
//
// heartbeat = expiry/2, clamped to nats.go's PullHeartbeat validity range
// [MinPullHeartbeat, MaxPullHeartbeat], then capped by heartbeatCap when
// heartbeatCap > 0.
//
// The 500ms floor arm is unreachable only through the validated public
// consumer config paths: every FetchTimeout entry point there enforces a 1s
// floor upstream of this call (consumer.validateFetchTimeoutFloor), so
// expiry/2 >= 500ms always for those callers. It is reachable through
// internal callers that bypass that gate — internal/durable accepts any
// positive FetchTimeout, and internal/ipartition only defaults values <= 0 —
// so a positive sub-second FetchTimeout can drive expiry/2 below 500ms and
// hit the floor. When it does, the floored heartbeat can then exceed
// expiry/2, which nats.go separately rejects ("must be less than 50% of
// expiry") at iterator-creation setup; this function has no way to enforce
// the 1s precondition itself. The floor exists so this function never
// returns a value outside nats.go's own [500ms, 30s] PullHeartbeat range —
// it does not guarantee the value satisfies the expiry/2 relation.
//
// Without an upper bound, raising FetchTimeout above 60s (to cut idle
// MSG.NEXT pull-request churn) derived a heartbeat above nats.go's 30s
// ceiling, which nats.go rejected outright — iterator creation failed on
// every attempt and the pull loop spun in a restart/warn cycle forever with
// no terminal signal. The 30s clamp fixes that unconditionally; heartbeatCap
// lets a caller pull the ceiling down further to bound missed-heartbeat
// (ErrNoHeartbeat) detection latency, which fires at roughly 2x the
// heartbeat and is how a deleted durable consumer is detected.
func DerivePullHeartbeat(expiry, heartbeatCap time.Duration) time.Duration {
	heartbeat := max(expiry/2, MinPullHeartbeat)
	heartbeat = min(heartbeat, MaxPullHeartbeat)
	if heartbeatCap > 0 {
		heartbeat = min(heartbeat, heartbeatCap)
	}

	return heartbeat
}

// PullMessagesOpts returns the options every parti pull iterator is created with.
// PullHeartbeat is derived from expiry and heartbeatCap by [DerivePullHeartbeat].
//
// PullThresholdMessages(1) makes nats.go issue the next pull request
// only once the previous one is used up,
// so an iterator normally has at most one pull request waiting on the server.
// With nats.go's default threshold (half the batch) one iterator overlaps
// pull requests, and nats-server handles overlap badly in two ways:
// a request that does not fit in the consumer's MaxWaiting is dropped without a reply,
// and when one of two overlapping requests ends, the server forgets the inbox they share,
// so after a consumer leader change the surviving request gets no "409 Leadership Change".
// In both cases no request is left on the server while the client still expects messages,
// so delivery stalls until the heartbeat is missed.
//
// A lost request or message can still leave the client waiting with nothing on the server;
// the missed heartbeat remains the recovery for that.
func PullMessagesOpts(batch int, expiry, heartbeatCap time.Duration) []jetstream.PullMessagesOpt {
	return []jetstream.PullMessagesOpt{
		jetstream.PullMaxMessages(batch),
		jetstream.PullThresholdMessages(1),
		jetstream.PullExpiry(expiry),
		jetstream.PullHeartbeat(DerivePullHeartbeat(expiry, heartbeatCap)),
	}
}
