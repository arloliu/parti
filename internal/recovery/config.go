package recovery

import (
	"github.com/nats-io/nats.go/jetstream"
)

// BuildConfig builds a fresh consumer config for recreation, overriding DeliverPolicy
// according to strategy. The base config is never mutated.
//
// recreatedSinceLastBuild signals that the consumer is being rebuilt for the
// first time after Controller.HandleStreamRecreated ran. When the flag is true
// AND the strategy is FromLastProcessed AND the checkpoint is zero, BuildConfig
// emits DeliverAllPolicy so the new consumer replays from sequence 1 of the
// freshly-recreated stream — preventing the fresh-stream skip hazard where
// messages published before the replacement consumer is bound would be missed
// under the normal "no checkpoint → DeliverNewPolicy" fallback.
//
// Returns the config and a non-empty fallback string if the strategy was downgraded
// (e.g., FromLastProcessed with no checkpoint falls back to FromNew, unless the
// recreated override applies).
func BuildConfig(base jetstream.ConsumerConfig, strategy Strategy, checkpoint uint64, recreatedSinceLastBuild bool) (jetstream.ConsumerConfig, string) {
	cfg := base // copy

	// Clear stale delivery-policy fields to prevent confusion when switching policies.
	cfg.OptStartSeq = 0
	cfg.OptStartTime = nil

	switch strategy { //nolint:exhaustive // Disabled is handled by default.
	case FromNew:
		// Intentional: stream-missing hook is rejected at construction
		// when paired with FromNew, so production code cannot reach
		// this branch with recreatedSinceLastBuild=true. The flag is
		// ignored here to keep the FromNew contract unconditional.
		cfg.DeliverPolicy = jetstream.DeliverNewPolicy
	case FromLastProcessed:
		switch {
		case checkpoint > 0:
			cfg.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
			cfg.OptStartSeq = checkpoint + 1
		case recreatedSinceLastBuild:
			cfg.DeliverPolicy = jetstream.DeliverAllPolicy
			return cfg, "stream_recreated_replay_from_start"
		default:
			cfg.DeliverPolicy = jetstream.DeliverNewPolicy
			return cfg, "fallback_no_checkpoint"
		}
	case FromBeginning:
		// FromBeginning already replays from sequence 1 unconditionally,
		// so the recreated override is a no-op here.
		cfg.DeliverPolicy = jetstream.DeliverAllPolicy
	default:
		cfg.DeliverPolicy = jetstream.DeliverNewPolicy

		return cfg, "unsupported_strategy_fallback"
	}

	return cfg, ""
}
