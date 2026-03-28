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
}
