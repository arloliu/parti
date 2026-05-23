package consumer

import (
	"testing"

	"github.com/arloliu/parti/v2/types"
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
