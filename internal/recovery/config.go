package recovery

import (
	"github.com/nats-io/nats.go/jetstream"
)

// BuildConfig builds a fresh consumer config for recreation, overriding DeliverPolicy
// according to strategy. The base config is never mutated.
//
// Returns the config and a non-empty fallback string if the strategy was downgraded
// (e.g., FromLastProcessed with no checkpoint falls back to FromNew).
func BuildConfig(base jetstream.ConsumerConfig, strategy Strategy, checkpoint uint64) (jetstream.ConsumerConfig, string) {
	cfg := base // copy

	// Clear stale delivery-policy fields to prevent confusion when switching policies.
	cfg.OptStartSeq = 0
	cfg.OptStartTime = nil

	switch strategy { //nolint:exhaustive // Disabled is handled by default.
	case FromNew:
		cfg.DeliverPolicy = jetstream.DeliverNewPolicy
	case FromLastProcessed:
		if checkpoint == 0 {
			cfg.DeliverPolicy = jetstream.DeliverNewPolicy
			return cfg, "fallback_no_checkpoint"
		}
		cfg.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
		cfg.OptStartSeq = checkpoint + 1
	case FromBeginning:
		cfg.DeliverPolicy = jetstream.DeliverAllPolicy
	default:
		cfg.DeliverPolicy = jetstream.DeliverNewPolicy

		return cfg, "unsupported_strategy_fallback"
	}

	return cfg, ""
}
