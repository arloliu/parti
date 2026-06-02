package parti

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDegradeReason_LiteralValues pins every degrade reason to its exact string
// value. The recovery gates and most tests reference the named consts, so a
// rename of a const's VALUE would otherwise drift silently across prod and tests
// together (they would all move in lockstep). This is the one place that
// hard-codes the literals an operator's OnDegraded handler matches on, so changing
// a value — the operator-facing contract — fails here loudly.
func TestDegradeReason_LiteralValues(t *testing.T) {
	t.Parallel()
	require.Equal(t, "kv-unavailable", DegradeReasonKVUnavailable)
	require.Equal(t, "heartbeat-enumeration-stall", DegradeReasonEnumerationStall)
	require.Equal(t, "assignment-watcher-exhausted", DegradeReasonAssignmentWatcherExhausted)
	require.Equal(t, "KV error threshold exceeded", DegradeReasonKVErrorThreshold)
	require.Equal(t, "NATS connection down", DegradeReasonNATSConnectionDown)
	require.Equal(t, "stream-missing-recovery-exhausted", DegradeReasonStreamMissingRecoveryExhausted)
	require.Equal(t, "startup-timeout", DegradeReasonStartupTimeout)
	require.Equal(t, "startup-background-panic", DegradeReasonStartupBackgroundPanic)
	require.Equal(t, "bucket-recreated:parti-heartbeat", degradeReasonBucketRecreated("parti-heartbeat"))
}
