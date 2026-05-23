package durable

import (
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// Pins the recovery-strategy pre-conditions enforced by
// WorkerConsumerConfig.Validate when StreamMissingHook is set. The
// cross-package consistency test in consumer/ proves DynamicConfig.Validate
// agrees with this surface on the same matrix.

func baseWorkerConfigForStreamHook() WorkerConsumerConfig {
	return WorkerConsumerConfig{
		StreamName:      "TEST_STREAM",
		ConsumerPrefix:  "wc",
		SubjectTemplate: "events.{{.PartitionID}}",
	}
}

func TestWorkerConsumerConfig_Validate_NoHook_AnyStrategyAccepted(t *testing.T) {
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
			cfg := baseWorkerConfigForStreamHook()
			cfg.RecoveryStrategy = tc.strategy
			require.NoError(t, cfg.Validate(),
				"without StreamMissingHook, every RecoveryStrategy must remain accepted")
		})
	}
}

func TestWorkerConsumerConfig_Validate_Hook_RecoveryDisabled_Rejected(t *testing.T) {
	cfg := baseWorkerConfigForStreamHook()
	cfg.RecoveryStrategy = RecoveryDisabled
	cfg.StreamMissingHook = types.StreamMissingHook(func(string) error { return nil })

	err := cfg.Validate()
	require.Error(t, err,
		"StreamMissingHook + RecoveryDisabled must be rejected; without the controller the hook-success path cannot rebuild")
	require.Contains(t, err.Error(), "RecoveryStrategy",
		"the rejection message must name RecoveryStrategy so operators can fix the config")
}

func TestWorkerConsumerConfig_Validate_Hook_RecoverFromNew_Rejected(t *testing.T) {
	cfg := baseWorkerConfigForStreamHook()
	cfg.RecoveryStrategy = RecoverFromNew
	cfg.StreamMissingHook = types.StreamMissingHook(func(string) error { return nil })

	err := cfg.Validate()
	require.Error(t, err,
		"StreamMissingHook + RecoverFromNew must be rejected; the recreated-stream replay override does not apply, so messages published after a fresh-stream recreate would be silently skipped")
	require.Contains(t, err.Error(), "RecoverFromNew")
}

func TestWorkerConsumerConfig_Validate_Hook_RecoverFromLastProcessed_Accepted(t *testing.T) {
	cfg := baseWorkerConfigForStreamHook()
	cfg.RecoveryStrategy = RecoverFromLastProcessed
	cfg.StreamMissingHook = types.StreamMissingHook(func(string) error { return nil })

	require.NoError(t, cfg.Validate(),
		"StreamMissingHook + RecoverFromLastProcessed is the canonical at-least-once configuration")
}

func TestWorkerConsumerConfig_Validate_Hook_RecoverFromBeginning_Accepted(t *testing.T) {
	cfg := baseWorkerConfigForStreamHook()
	cfg.RecoveryStrategy = RecoverFromBeginning
	cfg.StreamMissingHook = types.StreamMissingHook(func(string) error { return nil })

	require.NoError(t, cfg.Validate(),
		"StreamMissingHook + RecoverFromBeginning (replay-all) is an accepted intentional-duplicate-processing configuration")
}
