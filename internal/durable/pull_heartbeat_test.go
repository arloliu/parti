package durable

import (
	"context"
	"errors"
	"testing"
	"time"

	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestWorkerConsumer_IteratorFactoryInjection_TakesPrecedenceOverPullHeartbeatCap
// pins that a caller-supplied IteratorFactory still wins over the
// PullHeartbeatCap-derived default — the cap only affects the default
// derivation path, never an injected override.
func TestWorkerConsumer_IteratorFactoryInjection_TakesPrecedenceOverPullHeartbeatCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	sentinel := errors.New("sentinel: injected factory called")
	injected := func(_ jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
		return nil, sentinel
	}

	cfg := WorkerConsumerConfig{
		StreamName:       "S",
		ConsumerPrefix:   "wc",
		SubjectTemplate:  "s.{{.PartitionID}}",
		PullHeartbeatCap: 2 * time.Second,
		IteratorFactory:  injected,
	}

	wc, err := NewWorkerConsumer(js, cfg, func(context.Context, jetstream.Msg) error { return nil })
	require.NoError(t, err)

	_, err = wc.iterFactory(nil, 1, 30*time.Second)
	require.ErrorIs(t, err, sentinel, "custom IteratorFactory must take precedence over the PullHeartbeatCap-derived default")
}

// TestBroadcastConsumer_IteratorFactoryInjection_TakesPrecedenceOverPullHeartbeatCap
// is the BroadcastConsumer equivalent of the WorkerConsumer precedence pin
// above: both share the same makeDefaultIterFactory/derivePullHeartbeat
// derivation, and both must respect the same injection-seam precedence.
func TestBroadcastConsumer_IteratorFactoryInjection_TakesPrecedenceOverPullHeartbeatCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	sentinel := errors.New("sentinel: injected factory called")
	injected := func(_ jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
		return nil, sentinel
	}

	cfg := BroadcastConsumerConfig{
		StreamName:       "S",
		ConsumerPrefix:   "bc",
		WildcardFilter:   "s.>",
		PullHeartbeatCap: 2 * time.Second,
		IteratorFactory:  injected,
	}

	bc, err := NewBroadcastConsumer(js, cfg, func(context.Context, jetstream.Msg) error { return nil })
	require.NoError(t, err)

	_, err = bc.iterFactory(nil, 1, 30*time.Second)
	require.ErrorIs(t, err, sentinel, "custom IteratorFactory must take precedence over the PullHeartbeatCap-derived default")
}

// TestWorkerConsumerConfig_Validate_PullHeartbeatCapNegative pins that the
// struct-tag floor (`validate:"gte=0"`) rejects a negative PullHeartbeatCap
// at this layer, before validatePullHeartbeatCap's range check ever runs.
func TestWorkerConsumerConfig_Validate_PullHeartbeatCapNegative(t *testing.T) {
	cfg := WorkerConsumerConfig{
		StreamName:       "TEST",
		ConsumerPrefix:   "wc",
		SubjectTemplate:  "events.{{.PartitionID}}",
		PullHeartbeatCap: -time.Second,
	}

	err := cfg.Validate()
	require.Error(t, err)
}

// TestWorkerConsumerConfig_Validate_PullHeartbeatCapBounds pins that this
// internal config door enforces the same [500ms, 30s] nats.go PullHeartbeat
// range as the public consumer package: a config built directly through
// internal/durable (bypassing consumer.WithPullHeartbeatCap) must not be
// able to smuggle in a cap that natsutil.DerivePullHeartbeat would turn into
// a value nats.go rejects at iterator creation.
func TestWorkerConsumerConfig_Validate_PullHeartbeatCapBounds(t *testing.T) {
	validate := func(heartbeatCap time.Duration) error {
		cfg := WorkerConsumerConfig{
			StreamName:       "TEST",
			ConsumerPrefix:   "wc",
			SubjectTemplate:  "events.{{.PartitionID}}",
			PullHeartbeatCap: heartbeatCap,
		}

		return cfg.Validate()
	}

	require.NoError(t, validate(0), "0 (disabled) must pass")
	require.Error(t, validate(499*time.Millisecond), "PullHeartbeatCap below the 500ms nats.go PullHeartbeat floor must fail validation")
	require.NoError(t, validate(500*time.Millisecond), "PullHeartbeatCap == 500ms is the boundary and must pass")
	require.NoError(t, validate(30*time.Second), "PullHeartbeatCap == 30s is the boundary and must pass")
	require.Error(t, validate(30*time.Second+time.Nanosecond), "PullHeartbeatCap above the 30s nats.go PullHeartbeat ceiling must fail validation")
}

// TestBroadcastConsumerConfig_Validate_PullHeartbeatCapBounds is the
// BroadcastConsumerConfig equivalent of the WorkerConsumerConfig bounds test
// above: both share the durable package's validatePullHeartbeatCap helper.
func TestBroadcastConsumerConfig_Validate_PullHeartbeatCapBounds(t *testing.T) {
	validate := func(heartbeatCap time.Duration) error {
		cfg := BroadcastConsumerConfig{
			StreamName:       "TEST",
			ConsumerPrefix:   "bc",
			WildcardFilter:   "events.>",
			PullHeartbeatCap: heartbeatCap,
		}

		return cfg.Validate()
	}

	require.NoError(t, validate(0), "0 (disabled) must pass")
	require.Error(t, validate(499*time.Millisecond), "PullHeartbeatCap below the 500ms nats.go PullHeartbeat floor must fail validation")
	require.NoError(t, validate(500*time.Millisecond), "PullHeartbeatCap == 500ms is the boundary and must pass")
	require.NoError(t, validate(30*time.Second), "PullHeartbeatCap == 30s is the boundary and must pass")
	require.Error(t, validate(30*time.Second+time.Nanosecond), "PullHeartbeatCap above the 30s nats.go PullHeartbeat ceiling must fail validation")
}
