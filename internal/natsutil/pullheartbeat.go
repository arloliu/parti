package natsutil

import "time"

// MinPullHeartbeat and MaxPullHeartbeat are nats.go v1.52.0's validity bounds
// for an explicit jetstream.PullHeartbeat option: configureConsume and
// configureMessages both reject a value outside [500ms, 30s] with
// ErrInvalidOption (jetstream_options.go), and pull.go separately rejects a
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
