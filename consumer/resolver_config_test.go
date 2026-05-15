package consumer

import (
	"testing"
	"time"

	"github.com/arloliu/fuda"
	"github.com/stretchr/testify/require"
)

// TestResolverConfig_DefaultsReconcileInterval verifies the default `30s`
// reconcile cadence is applied through the fuda struct tag when the field
// is left at zero.
func TestResolverConfig_DefaultsReconcileInterval(t *testing.T) {
	cfg := ResolverConfig{}
	require.NoError(t, fuda.SetDefaults(&cfg))
	require.Equal(t, 30*time.Second, cfg.ReconcileInterval,
		"ResolverConfig.ReconcileInterval default must be 30s")
}

// TestResolverConfig_RejectsNegativeReconcileInterval guards the validate
// tag (`gte=0`). A negative value must fail validation so operators learn
// about misconfiguration before runtime.
func TestResolverConfig_RejectsNegativeReconcileInterval(t *testing.T) {
	cfg := DynamicConfig{
		StreamName:      "TEST",
		SubjectTemplate: "events.{{.PartitionID}}",
		ConsumerPrefix:  "test",
	}
	cfg.Resolver.ReconcileInterval = -1 * time.Second

	err := cfg.Validate()
	require.Error(t, err, "negative ReconcileInterval must fail validation")
	require.Contains(t, err.Error(), "ReconcileInterval")
}

// TestToSubscriptionResolverConfig_CopiesReconcileInterval verifies that
// the consumer-layer ResolverConfig.ReconcileInterval is copied across to
// the internal durable equivalent.
func TestToSubscriptionResolverConfig_CopiesReconcileInterval(t *testing.T) {
	cfg := ResolverConfig{
		HandoffBucketName:   "x",
		HandoffClaimsPrefix: "claims/",
		BatchWindow:         10 * time.Millisecond,
		BatchMaxItems:       128,
		ReconcileInterval:   7 * time.Second,
	}
	out := toSubscriptionResolverConfig(cfg)
	require.Equal(t, 7*time.Second, out.ReconcileInterval,
		"toSubscriptionResolverConfig must propagate ReconcileInterval")
}
