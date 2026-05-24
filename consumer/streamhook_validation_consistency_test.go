package consumer_test

import (
	"testing"

	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// TestStreamMissingHookStrategy_ConsistentAcrossPublicSurfaces pins the
// cross-package invariant that consumer.DynamicConfig.Validate and
// durable.WorkerConsumerConfig.Validate agree on which
// (RecoveryStrategy, StreamMissingHook) combinations are accepted or
// rejected. Both surfaces run their own package-local helper of the
// same name (intentional duplication, see the helper godoc); this
// test catches a future divergence in the accept/reject set.
//
// Each row populates every other required field with a valid value so
// the only possible failure reason is the hook/strategy combination
// under test. A vacuous-pass guard runs the "valid" row (hook
// configured with RecoverFromLastProcessed) first: both surfaces MUST
// return nil there. If either returns an error, the populated fields
// are wrong, not the strategy validation — fix the test before reading
// any further assertion.
func TestStreamMissingHookStrategy_ConsistentAcrossPublicSurfaces(t *testing.T) {
	hook := types.StreamMissingHook(func(string) error { return nil })

	build := func(hook types.StreamMissingHook, strategy consumer.RecoveryStrategy) (consumer.DynamicConfig, durable.WorkerConsumerConfig) {
		dc := consumer.DynamicConfig{
			StreamName:        "TEST_STREAM",
			ConsumerPrefix:    "wc",
			SubjectTemplate:   "events.{{.PartitionID}}",
			RecoveryStrategy:  strategy,
			StreamMissingHook: hook,
		}
		wc := durable.WorkerConsumerConfig{
			StreamName:        "TEST_STREAM",
			ConsumerPrefix:    "wc",
			SubjectTemplate:   "events.{{.PartitionID}}",
			RecoveryStrategy:  strategy,
			StreamMissingHook: hook,
		}

		return dc, wc
	}

	t.Run("baseline_valid_row_must_pass_both", func(t *testing.T) {
		// Vacuous-pass guard: hook + RecoverFromLastProcessed is the
		// canonical valid combination. If either surface rejects this,
		// the test setup is wrong, not the production code.
		dc, wc := build(hook, consumer.RecoverFromLastProcessed)
		require.NoError(t, dc.Validate(),
			"baseline (hook + RecoverFromLastProcessed) must pass DynamicConfig.Validate — if this fails, the test populated required fields wrong")
		require.NoError(t, wc.Validate(),
			"baseline must pass WorkerConsumerConfig.Validate — see above")
	})

	cases := []struct {
		name         string
		strategy     consumer.RecoveryStrategy
		hook         types.StreamMissingHook
		expectReject bool
	}{
		// No hook configured: every strategy is accepted (the existing
		// pre-hook contract).
		{"no_hook_disabled", consumer.RecoveryDisabled, nil, false},
		{"no_hook_from_new", consumer.RecoverFromNew, nil, false},
		{"no_hook_from_last_processed", consumer.RecoverFromLastProcessed, nil, false},
		{"no_hook_from_beginning", consumer.RecoverFromBeginning, nil, false},

		// Hook configured: only FromLastProcessed and FromBeginning are
		// accepted. Disabled (default) and FromNew are rejected.
		{"hook_disabled_rejected", consumer.RecoveryDisabled, hook, true},
		{"hook_from_new_rejected", consumer.RecoverFromNew, hook, true},
		{"hook_from_last_processed_accepted", consumer.RecoverFromLastProcessed, hook, false},
		{"hook_from_beginning_accepted", consumer.RecoverFromBeginning, hook, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dc, wc := build(tc.hook, tc.strategy)
			dErr := dc.Validate()
			wErr := wc.Validate()

			require.Equal(t,
				dErr == nil, wErr == nil,
				"DynamicConfig.Validate and WorkerConsumerConfig.Validate must agree on accept/reject: DynamicConfig=%v, WorkerConsumerConfig=%v",
				dErr, wErr,
			)

			if tc.expectReject {
				require.Error(t, dErr,
					"%s: hook+%s must be rejected by DynamicConfig.Validate", tc.name, tc.strategy)
				require.Error(t, wErr,
					"%s: hook+%s must be rejected by WorkerConsumerConfig.Validate", tc.name, tc.strategy)
			} else {
				require.NoError(t, dErr,
					"%s: combination must be accepted by DynamicConfig.Validate", tc.name)
				require.NoError(t, wErr,
					"%s: combination must be accepted by WorkerConsumerConfig.Validate", tc.name)
			}
		})
	}
}
