package recovery

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestBuildConfig_RecoverFromNew(t *testing.T) {
	base := jetstream.ConsumerConfig{
		Durable:       "test-durable",
		FilterSubject: "orders.*",
		DeliverPolicy: jetstream.DeliverLastPolicy,
		OptStartSeq:   999,
	}

	cfg, fallback := BuildConfig(base, FromNew, 0, false)
	require.Empty(t, fallback)
	require.Equal(t, jetstream.DeliverNewPolicy, cfg.DeliverPolicy)
	require.Equal(t, uint64(0), cfg.OptStartSeq)
	require.Nil(t, cfg.OptStartTime)
	require.Equal(t, "test-durable", cfg.Durable) // preserved
}

func TestBuildConfig_RecoverFromLastProcessed_WithCheckpoint(t *testing.T) {
	base := jetstream.ConsumerConfig{Durable: "test-durable", FilterSubject: "orders.*"}

	cfg, fallback := BuildConfig(base, FromLastProcessed, 100, false)
	require.Empty(t, fallback)
	require.Equal(t, jetstream.DeliverByStartSequencePolicy, cfg.DeliverPolicy)
	require.Equal(t, uint64(101), cfg.OptStartSeq) // checkpoint + 1
}

func TestBuildConfig_RecoverFromLastProcessed_NoCheckpoint(t *testing.T) {
	base := jetstream.ConsumerConfig{Durable: "test-durable"}

	cfg, fallback := BuildConfig(base, FromLastProcessed, 0, false)
	require.Equal(t, "fallback_no_checkpoint", fallback)
	require.Equal(t, jetstream.DeliverNewPolicy, cfg.DeliverPolicy)
	require.Equal(t, uint64(0), cfg.OptStartSeq)
}

func TestBuildConfig_RecoverFromBeginning(t *testing.T) {
	base := jetstream.ConsumerConfig{Durable: "test", OptStartSeq: 50}

	cfg, fallback := BuildConfig(base, FromBeginning, 42, false)
	require.Empty(t, fallback)
	require.Equal(t, jetstream.DeliverAllPolicy, cfg.DeliverPolicy)
	require.Equal(t, uint64(0), cfg.OptStartSeq) // cleared
}

func TestBuildConfig_ClearsStaleFields(t *testing.T) {
	now := time.Now()
	base := jetstream.ConsumerConfig{
		Durable:      "test",
		OptStartSeq:  999,
		OptStartTime: &now,
	}

	cfg, _ := BuildConfig(base, FromNew, 0, false)
	require.Equal(t, uint64(0), cfg.OptStartSeq)
	require.Nil(t, cfg.OptStartTime)
}

func TestBuildConfig_UnknownStrategy(t *testing.T) {
	base := jetstream.ConsumerConfig{Durable: "test"}

	cfg, fallback := BuildConfig(base, Strategy(99), 0, false)
	require.Equal(t, "unsupported_strategy_fallback", fallback)
	require.Equal(t, jetstream.DeliverNewPolicy, cfg.DeliverPolicy)
}

func TestBuildConfig_RecoveryDisabled(t *testing.T) {
	base := jetstream.ConsumerConfig{Durable: "test"}

	cfg, fallback := BuildConfig(base, Disabled, 0, false)
	require.Equal(t, "unsupported_strategy_fallback", fallback)
	require.Equal(t, jetstream.DeliverNewPolicy, cfg.DeliverPolicy)
}
