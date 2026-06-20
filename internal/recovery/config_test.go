package recovery

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestBuildConfig(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name            string
		base            jetstream.ConsumerConfig
		strategy        Strategy
		checkpoint      uint64
		recreated       bool
		fallback        string
		deliverPolicy   jetstream.DeliverPolicy
		expectStartSeq  bool   // when true, assert OptStartSeq == startSeq
		startSeq        uint64 // expected OptStartSeq (asserted when expectStartSeq)
		expectStartTime bool   // when true, assert OptStartTime is nil (cleared)
		expectDurable   string // when non-empty, assert Durable preserved
	}{
		{
			name: "RecoverFromNew",
			base: jetstream.ConsumerConfig{
				Durable:       "test-durable",
				FilterSubject: "orders.*",
				DeliverPolicy: jetstream.DeliverLastPolicy,
				OptStartSeq:   999,
			},
			strategy:        FromNew,
			checkpoint:      0,
			fallback:        "",
			deliverPolicy:   jetstream.DeliverNewPolicy,
			expectStartSeq:  true,
			startSeq:        0,
			expectStartTime: true,
			expectDurable:   "test-durable",
		},
		{
			name:           "RecoverFromLastProcessed_WithCheckpoint",
			base:           jetstream.ConsumerConfig{Durable: "test-durable", FilterSubject: "orders.*"},
			strategy:       FromLastProcessed,
			checkpoint:     100,
			fallback:       "",
			deliverPolicy:  jetstream.DeliverByStartSequencePolicy,
			expectStartSeq: true,
			startSeq:       101, // checkpoint + 1
		},
		{
			name:           "RecoverFromLastProcessed_NoCheckpoint",
			base:           jetstream.ConsumerConfig{Durable: "test-durable"},
			strategy:       FromLastProcessed,
			checkpoint:     0,
			fallback:       "fallback_no_checkpoint",
			deliverPolicy:  jetstream.DeliverNewPolicy,
			expectStartSeq: true,
			startSeq:       0,
		},
		{
			name:           "RecoverFromBeginning",
			base:           jetstream.ConsumerConfig{Durable: "test", OptStartSeq: 50},
			strategy:       FromBeginning,
			checkpoint:     42,
			fallback:       "",
			deliverPolicy:  jetstream.DeliverAllPolicy,
			expectStartSeq: true,
			startSeq:       0, // cleared
		},
		{
			name: "ClearsStaleFields",
			base: jetstream.ConsumerConfig{
				Durable:      "test",
				OptStartSeq:  999,
				OptStartTime: &now,
			},
			strategy:        FromNew,
			checkpoint:      0,
			fallback:        "",
			deliverPolicy:   jetstream.DeliverNewPolicy,
			expectStartSeq:  true,
			startSeq:        0,
			expectStartTime: true,
		},
		{
			name:          "UnknownStrategy",
			base:          jetstream.ConsumerConfig{Durable: "test"},
			strategy:      Strategy(99),
			checkpoint:    0,
			fallback:      "unsupported_strategy_fallback",
			deliverPolicy: jetstream.DeliverNewPolicy,
		},
		{
			name:          "RecoveryDisabled",
			base:          jetstream.ConsumerConfig{Durable: "test"},
			strategy:      Disabled,
			checkpoint:    0,
			fallback:      "unsupported_strategy_fallback",
			deliverPolicy: jetstream.DeliverNewPolicy,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, fallback := BuildConfig(tc.base, tc.strategy, tc.checkpoint, tc.recreated)

			require.Equal(t, tc.fallback, fallback)
			require.Equal(t, tc.deliverPolicy, cfg.DeliverPolicy)

			if tc.expectStartSeq {
				require.Equal(t, tc.startSeq, cfg.OptStartSeq)
			}
			if tc.expectStartTime {
				require.Nil(t, cfg.OptStartTime)
			}
			if tc.expectDurable != "" {
				require.Equal(t, tc.expectDurable, cfg.Durable)
			}
		})
	}
}
