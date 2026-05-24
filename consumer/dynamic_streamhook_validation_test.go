package consumer

import (
	"context"
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// Pins the recovery-strategy pre-conditions enforced by
// DynamicConfig.Validate when StreamMissingHook is set. Sibling to the
// internal/durable surface; the consumer_test cross-package consistency
// test proves these two surfaces agree on the same matrix.

func baseDynamicConfigForStreamHook() DynamicConfig {
	return DynamicConfig{
		StreamName:      "TEST_STREAM",
		ConsumerPrefix:  "wc",
		SubjectTemplate: "events.{{.PartitionID}}",
	}
}

func TestDynamicConfig_Validate_NoHook_AnyStrategyAccepted(t *testing.T) {
	cases := []struct {
		name     string
		strategy RecoveryStrategy
	}{
		{"disabled", RecoveryDisabled},
		{"from_new", RecoverFromNew},
		{"from_last_processed", RecoverFromLastProcessed},
		{"from_beginning", RecoverFromBeginning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseDynamicConfigForStreamHook()
			cfg.RecoveryStrategy = tc.strategy
			require.NoError(t, cfg.Validate(),
				"without StreamMissingHook, every RecoveryStrategy must remain accepted")
		})
	}
}

func TestDynamicConfig_Validate_Hook_RecoveryDisabled_Rejected(t *testing.T) {
	cfg := baseDynamicConfigForStreamHook()
	cfg.RecoveryStrategy = RecoveryDisabled
	cfg.StreamMissingHook = types.StreamMissingHook(func(string) error { return nil })

	err := cfg.Validate()
	require.Error(t, err,
		"StreamMissingHook + RecoveryDisabled must be rejected; without the controller the hook-success path cannot rebuild")
	require.ErrorIs(t, err, ErrInvalidConfig,
		"rejection must wrap ErrInvalidConfig so callers using errors.Is can route the failure")
}

func TestDynamicConfig_Validate_Hook_RecoverFromNew_Rejected(t *testing.T) {
	cfg := baseDynamicConfigForStreamHook()
	cfg.RecoveryStrategy = RecoverFromNew
	cfg.StreamMissingHook = types.StreamMissingHook(func(string) error { return nil })

	err := cfg.Validate()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "RecoverFromNew")
}

func TestDynamicConfig_Validate_Hook_RecoverFromLastProcessed_Accepted(t *testing.T) {
	cfg := baseDynamicConfigForStreamHook()
	cfg.RecoveryStrategy = RecoverFromLastProcessed
	cfg.StreamMissingHook = types.StreamMissingHook(func(string) error { return nil })

	require.NoError(t, cfg.Validate())
}

func TestDynamicConfig_Validate_Hook_RecoverFromBeginning_Accepted(t *testing.T) {
	cfg := baseDynamicConfigForStreamHook()
	cfg.RecoveryStrategy = RecoverFromBeginning
	cfg.StreamMissingHook = types.StreamMissingHook(func(string) error { return nil })

	require.NoError(t, cfg.Validate())
}

// TestWithStreamMissingHook_OptionThreadedThroughNewDynamic pins the public
// option-surface wiring: WithStreamMissingHook → o.streamMissingHook →
// cfg.StreamMissingHook → cfg.Validate(). Without the threading, the option
// would silently no-op and the strategy validation would never fire — a
// caller pairing WithStreamMissingHook with RecoverFromNew would then
// encounter the message-skip hazard at runtime instead of seeing the
// rejection at construction time. cfg.Validate runs BEFORE
// NewWorkerConsumer in NewDynamic, so an invalid-strategy rejection here
// is observable evidence that the option reached the config without
// requiring a live JetStream.
func TestWithStreamMissingHook_OptionThreadedThroughNewDynamic(t *testing.T) {
	_, err := NewDynamic(
		fakeJS{}, // satisfies the not-nil check; NewDynamic fails at cfg.Validate before any js call.
		"TEST_STREAM",
		"wc",
		"events.{{.PartitionID}}",
		MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil }),
		WithRecoveryStrategy(RecoverFromNew),
		WithStreamMissingHook(func(string) error { return nil }),
	)
	require.Error(t, err, "WithStreamMissingHook + RecoverFromNew must be rejected via the validator")
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "RecoverFromNew",
		"rejection text must name the incompatible strategy so the caller can fix it")
}

// fakeJS is a minimal jetstream.JetStream satisfying NewDynamic's not-nil
// guard. The validator runs before any JetStream method is invoked; the
// embedded nil interface panics if reached, which would be an obvious
// signal that the validator was bypassed.
type fakeJS struct{ jetstream.JetStream }
