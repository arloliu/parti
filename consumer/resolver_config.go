package consumer

import (
	"time"

	"github.com/arloliu/parti/v2/types"
)

// ResolverConfig configures the ownership resolver used when ProcessingGate is
// enabled.
//
// It defines how ownership claims are tracked and verified. When
// ProcessingGate.Enabled is true and no custom OwnershipResolver is provided,
// a claim-based resolver is automatically created using the KV bucket specified
// by HandoffBucketName.
type ResolverConfig struct {
	// OwnershipResolver (advanced) supplies custom ownership lookups.
	// When nil and ProcessingGate.Enabled is true, a claim-based resolver
	// is auto-created using HandoffBucketName/HandoffClaimsPrefix. When
	// non-nil, it overrides the automatic resolver creation.
	OwnershipResolver types.OwnershipResolver

	// HandoffBucketName specifies the KV bucket name for handoff claims.
	// When ProcessingGate.Enabled is true and this is set, the Dynamic consumer
	// will automatically get/create the KV bucket and start a claim resolver.
	// Should match the HandoffBucket in parti.Config.KVBuckets for consistency.
	// Default: "parti-handoff".
	HandoffBucketName string `default:"parti-handoff"`

	// HandoffClaimsPrefix is the key prefix for handoff claims in the KV bucket.
	// Default: "claims/".
	HandoffClaimsPrefix string `default:"claims/"`

	// BatchWindow is the coalescing window for batching claim updates into the
	// auto-created claim-based resolver when ProcessingGate is enabled.
	// If zero, a default (5ms) is used. Ignored when a custom OwnershipResolver
	// is provided.
	BatchWindow time.Duration `default:"5ms" validate:"gte=0"`

	// BatchMaxItems caps the number of unique partition updates coalesced into a
	// single apply. If zero, a default (1024) is used. Ignored when a custom
	// OwnershipResolver is provided.
	BatchMaxItems int `default:"1024" validate:"gt=0"`

	// ReconcileInterval is the cadence at which the auto-created claim-based
	// resolver re-lists the handoff bucket and reconciles its cache against
	// KV. This is the recovery mechanism for silent watcher stalls (the
	// nats.go KV watcher does NOT surface NATS server restarts as Updates()
	// channel close; only an explicit Stop / connection close / subscription
	// teardown does). After such a stall the cache stays stale for at most
	// one reconcile period.
	//
	// Choose a value shorter than parti.Config.ExtendedApplyGracePeriod
	// (default 5 × HeartbeatTTL) so the leader-side audit cannot escalate
	// audit_repair while the worker's resolver cache is still stale. With
	// the default HeartbeatTTL=15s, this gives an audit grace of 75s and
	// the default 30s reconcile is comfortably inside it. If you have tuned
	// HeartbeatTTL below ~6s, set ReconcileInterval to HeartbeatTTL or
	// HeartbeatTTL/2.
	//
	// Zero uses the default (30s). Negative values are rejected at startup.
	// Ignored when a custom OwnershipResolver is provided.
	ReconcileInterval time.Duration `default:"30s" validate:"gte=0"`
}
