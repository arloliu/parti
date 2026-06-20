package durable

import (
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkerConsumerConfig_SetDefaults(t *testing.T) {
	tests := []struct {
		name     string
		input    *WorkerConsumerConfig
		expected func(*testing.T, *WorkerConsumerConfig)
	}{
		{
			name:  "empty config gets all defaults",
			input: &WorkerConsumerConfig{},
			expected: func(t *testing.T, c *WorkerConsumerConfig) {
				assert.Equal(t, 30*time.Second, c.AckWait)
				assert.Equal(t, jetstream.AckExplicitPolicy, c.AckPolicy)
				assert.Equal(t, -1, c.MaxDeliver)
				assert.Equal(t, 1, c.BatchSize)
				assert.Equal(t, 5*time.Second, c.FetchTimeout)
				assert.Equal(t, 2, c.MaxWaiting)
				assert.Equal(t, 24*time.Hour, c.InactiveThreshold)
				assert.Equal(t, 500*time.Millisecond, c.PartitionRefreshMinInterval)
				assert.Equal(t, 60*time.Second, c.IteratorEscalationWindow)
				assert.Equal(t, 3, c.IteratorEscalationThreshold)
				assert.Equal(t, 10*time.Second, c.DrainOnRemoveTimeout)

				// Retry defaults
				assert.Equal(t, 100*time.Millisecond, c.Retry.Backoff)
				assert.Equal(t, 5*time.Second, c.Retry.Max)
				assert.Equal(t, 1.6, c.Retry.Multiplier)
				assert.Equal(t, 200*time.Millisecond, c.Retry.Base)

				// Resolver defaults
				assert.Equal(t, "parti-handoff", c.Resolver.HandoffBucketName)
				assert.Equal(t, "claims/", c.Resolver.HandoffClaimsPrefix)
				assert.Equal(t, 5*time.Millisecond, c.Resolver.BatchWindow)
				assert.Equal(t, 1024, c.Resolver.BatchMaxItems)

				// Logger and Metrics
				assert.NotNil(t, c.Logger)
				assert.NotNil(t, c.Metrics)
			},
		},
		{
			name: "partial config preserves set values",
			input: &WorkerConsumerConfig{
				AckWait:   10 * time.Second,
				BatchSize: 5,
				Retry: RetryConfig{
					Backoff: 50 * time.Millisecond,
				},
			},
			expected: func(t *testing.T, c *WorkerConsumerConfig) {
				assert.Equal(t, 10*time.Second, c.AckWait)
				assert.Equal(t, 5, c.BatchSize)
				assert.Equal(t, 50*time.Millisecond, c.Retry.Backoff)
				// Other defaults should still be applied
				assert.Equal(t, jetstream.AckExplicitPolicy, c.AckPolicy)
				assert.Equal(t, 5*time.Second, c.Retry.Max)
			},
		},
		{
			name: "processing gate defaults",
			input: &WorkerConsumerConfig{
				ProcessingGate: &ProcessingGateConfig{},
			},
			expected: func(t *testing.T, c *WorkerConsumerConfig) {
				require.NotNil(t, c.ProcessingGate)
				assert.Equal(t, []types.HandoffState{types.HandoffStateStable}, c.ProcessingGate.AllowedStates)
				assert.Equal(t, 100*time.Millisecond, c.ProcessingGate.NakDelay)
			},
		},
		{
			name: "processing gate complex defaults",
			input: &WorkerConsumerConfig{
				ProcessingGate: &ProcessingGateConfig{
					WarmupDuration: 5 * time.Second,
				},
			},
			expected: func(t *testing.T, c *WorkerConsumerConfig) {
				require.NotNil(t, c.ProcessingGate)
				assert.Equal(t, 5*time.Second, c.ProcessingGate.WarmupDuration)
				assert.Equal(t, []types.HandoffState{types.HandoffStateStable}, c.ProcessingGate.WarmupAllowedStates)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.input.SetDefaults()
			require.NoError(t, err)
			tt.expected(t, tt.input)
		})
	}
}

func TestWorkerConsumerConfig_Validate_InvalidSubjectTemplate(t *testing.T) {
	cfg := WorkerConsumerConfig{
		StreamName:      "TEST",
		ConsumerPrefix:  "wc",
		SubjectTemplate: "events.{{.PartitionID",
	}

	err := cfg.Validate()
	require.Error(t, err)
}

func TestWorkerConsumerConfig_Validate_InvalidSubjectTokens(t *testing.T) {
	cfg := WorkerConsumerConfig{
		StreamName:      "TEST",
		ConsumerPrefix:  "wc",
		SubjectTemplate: "events.>.{{.PartitionID}}",
	}

	err := cfg.Validate()
	require.Error(t, err)
}

func TestBroadcastConsumerConfig_Validate_InvalidWildcardFilter(t *testing.T) {
	cfg := BroadcastConsumerConfig{
		StreamName:     "TEST",
		ConsumerPrefix: "bc",
		WildcardFilter: "events..>",
	}

	err := cfg.Validate()
	require.Error(t, err)
}

// --- merged from config_streamhook_test.go ---

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

func TestWorkerConsumerConfig_Validate_StreamHook(t *testing.T) {
	tests := []struct {
		name string
		// strategy is the RecoveryStrategy applied to the base config.
		strategy RecoveryStrategy
		// hookSet controls whether a StreamMissingHook is installed.
		hookSet bool
		// wantErr is true when Validate must reject the combination.
		wantErr bool
		// errContains, when non-empty, must appear in the rejection message.
		errContains string
		// msg is the assertion message for this case.
		msg string
	}{
		// No hook: every RecoveryStrategy must remain accepted (was the
		// 4-case TestWorkerConsumerConfig_Validate_NoHook_AnyStrategyAccepted).
		{
			name:     "no_hook/disabled",
			strategy: RecoveryDisabled,
			hookSet:  false,
			wantErr:  false,
			msg:      "without StreamMissingHook, every RecoveryStrategy must remain accepted",
		},
		{
			name:     "no_hook/from_new",
			strategy: RecoverFromNew,
			hookSet:  false,
			wantErr:  false,
			msg:      "without StreamMissingHook, every RecoveryStrategy must remain accepted",
		},
		{
			name:     "no_hook/from_last_processed",
			strategy: RecoverFromLastProcessed,
			hookSet:  false,
			wantErr:  false,
			msg:      "without StreamMissingHook, every RecoveryStrategy must remain accepted",
		},
		{
			name:     "no_hook/from_beginning",
			strategy: RecoverFromBeginning,
			hookSet:  false,
			wantErr:  false,
			msg:      "without StreamMissingHook, every RecoveryStrategy must remain accepted",
		},
		// Hook set: strategy-specific pre-conditions.
		{
			name:        "hook/recovery_disabled_rejected",
			strategy:    RecoveryDisabled,
			hookSet:     true,
			wantErr:     true,
			errContains: "RecoveryStrategy",
			msg:         "StreamMissingHook + RecoveryDisabled must be rejected; without the controller the hook-success path cannot rebuild",
		},
		{
			name:        "hook/recover_from_new_rejected",
			strategy:    RecoverFromNew,
			hookSet:     true,
			wantErr:     true,
			errContains: "RecoverFromNew",
			msg:         "StreamMissingHook + RecoverFromNew must be rejected; the recreated-stream replay override does not apply, so messages published after a fresh-stream recreate would be silently skipped",
		},
		{
			name:     "hook/recover_from_last_processed_accepted",
			strategy: RecoverFromLastProcessed,
			hookSet:  true,
			wantErr:  false,
			msg:      "StreamMissingHook + RecoverFromLastProcessed is the canonical at-least-once configuration",
		},
		{
			name:     "hook/recover_from_beginning_accepted",
			strategy: RecoverFromBeginning,
			hookSet:  true,
			wantErr:  false,
			msg:      "StreamMissingHook + RecoverFromBeginning (replay-all) is an accepted intentional-duplicate-processing configuration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseWorkerConfigForStreamHook()
			cfg.RecoveryStrategy = tt.strategy
			if tt.hookSet {
				cfg.StreamMissingHook = types.StreamMissingHook(func(string) error { return nil })
			}

			err := cfg.Validate()
			if tt.wantErr {
				require.Error(t, err, tt.msg)
				if tt.errContains != "" {
					require.Contains(t, err.Error(), tt.errContains,
						"the rejection message must name %s so operators can fix the config", tt.errContains)
				}
				return
			}
			require.NoError(t, err, tt.msg)
		})
	}
}
