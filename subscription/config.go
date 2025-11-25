package subscription

import (
	"time"

	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/internal/metrics"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
)

// RetryConfig groups retry backoff settings.
type RetryConfig struct {
	// Backoff is the delay between retries for control-plane operations.
	// Default: DefaultRetryBackoff (100ms).
	Backoff time.Duration

	// Max caps the jittered backoff.
	// Default: DefaultRetryMax (5s).
	Max time.Duration

	// Multiplier grows the backoff window for decorrelated jitter.
	// Default: DefaultRetryMultiplier (1.6).
	Multiplier float64

	// Base is the base backoff used for decorrelated jitter retries for control-plane ops.
	// Default: DefaultRetryBase (200ms). If zero and Backoff is set, Base falls back to Backoff.
	Base time.Duration

	// Seed optionally seeds the jitter RNG for deterministic tests; when zero, a random seed is used.
	Seed int64
}

// ResolverConfig groups ownership resolver settings.
type ResolverConfig struct {
	// OwnershipResolver (advanced) supplies custom ownership lookups.
	// When nil and ProcessingGate.Enabled is true, a claim-based resolver
	// is auto-created using HandoffBucketName/Prefix. When non-nil, it
	// overrides the automatic resolver creation.
	OwnershipResolver types.OwnershipResolver

	// HandoffBucketName specifies the KV bucket name for handoff claims.
	// When ProcessingGate.Enabled is true and this is set, WorkerConsumer
	// will automatically get/create the KV bucket and start a claim resolver.
	// Defaults to "parti-handoff" if empty and gate is enabled.
	// Should match the HandoffBucket in parti.Config.KVBuckets for consistency.
	HandoffBucketName string

	// HandoffClaimsPrefix is the key prefix for handoff claims in the KV bucket.
	// Defaults to "claims/" if empty and gate is enabled.
	HandoffClaimsPrefix string

	// BatchWindow sets the resolver KV watch coalescing window for the
	// auto-created claim-based resolver when ProcessingGate is enabled.
	// If zero, a default (5ms) is used. Ignored when a custom OwnershipResolver is provided.
	BatchWindow time.Duration

	// BatchMaxItems caps the number of unique partition updates coalesced
	// into a single apply. If zero, a default (1024) is used. Ignored when a custom
	// OwnershipResolver is provided.
	BatchMaxItems int
}

// WorkerConsumerConfig configures the single durable per-worker consumer helper.
//
// Required fields:
//   - StreamName: JetStream stream name containing the subjects
//   - ConsumerPrefix: Prefix for durable name; final name is "<ConsumerPrefix>-<workerID>"
//   - SubjectTemplate: text/template expanded per partition with {{.PartitionID}}
//
// Optional tuning fields are documented inline below. Zero values are replaced by
// sensible defaults via SetDefaults().
type WorkerConsumerConfig struct {
	// StreamName is the JetStream stream name where subjects live. Required.
	StreamName string

	// ConsumerPrefix is the prefix for the durable consumer name.
	// It must contain only alphanumeric characters, dashes, or underscores.
	// Required.
	ConsumerPrefix string

	// SubjectTemplate is a text/template used to build subjects from a
	// partition. Available field: {{.PartitionID}} which equals partition.SubjectKey().
	// Example: "work.{{.PartitionID}}" => work.source.us-east-1
	// Required.
	SubjectTemplate string

	// Logger provides structured logging. Defaults to a no-op logger when nil.
	Logger types.Logger

	// Metrics is the global metrics collector used across the library.
	// If nil, no metrics are emitted from the worker consumer helper.
	Metrics types.MetricsCollector

	// ManualAck, when true, disables the helper's automatic Ack/Nak behavior.
	// Handlers are responsible for calling msg.Ack/Nak/Term and optionally
	// msg.InProgress() to extend AckWait. This enables handler-controlled
	// backpressure via an internal work queue. Defaults to false.
	//
	// Recommendation: Enable this for any asynchronous processing (e.g. spawning goroutines).
	// Keep false (default) for simple synchronous handlers to ensure safety.
	ManualAck bool

	// ProcessingGate configures optional exclusive processing enforcement.
	// When enabled, WorkerConsumer automatically creates and manages a claim-based
	// ownership resolver backed by the handoff KV bucket. The resolver lifecycle
	// (start/stop) is handled internally - no manual management needed.
	//
	// Recommendation: Enable this for stateful workloads or when strict partition
	// exclusivity is required. Without this, ownership transitions may be "loose",
	// resulting in brief periods where a partition is processed by a worker that
	// is no longer the assigned owner (though duplicates are rare).
	ProcessingGate *ProcessingGateConfig

	// Resolver configures the ownership resolver when ProcessingGate is enabled.
	Resolver ResolverConfig

	// PullGatingEnabled enables pre-pull ownership/state gating for per-subject consumers.
	// When true, pulls are suppressed (not issued) if the current worker is not the owner or
	// the handoff state is not Commit/Stable. Reduces NAK churn during handoffs.
	PullGatingEnabled bool

	// DrainOnRemove enables a graceful per-subject drain when a subject is removed
	// from the worker's assignment. When true, the consumer helper will stop issuing
	// new pulls for the subject and wait for pending acknowledgements to reach zero
	// (or until DrainOnRemoveTimeout elapses) before cancelling the loop. This reduces
	// NAK churn and minimizes gaps during scale-down and rebalancing.
	// Default: false (immediate cancellation as before).
	DrainOnRemove bool

	// DrainOnRemoveTimeout caps the drain wait per subject when DrainOnRemove is enabled.
	// Default: 10s when zero.
	DrainOnRemoveTimeout time.Duration

	// AckWait is the time allowed for processing before redelivery.
	// Default: DefaultAckWait (30s).
	AckWait time.Duration

	// MaxDeliver is the maximum redelivery attempts before moving to DLQ (if configured).
	// Default: DefaultMaxDeliver (-1 = unlimited attempts; relies on server default/unlimited behavior).
	MaxDeliver int

	// BatchSize is the max number of messages to pull per iterator request.
	// Default: DefaultBatchSize (1).
	BatchSize int

	// FetchTimeout is the max time to wait when pulling a batch.
	// Default: DefaultFetchTimeout (5s).
	FetchTimeout time.Duration

	// MaxWaiting caps outstanding pull requests for each per-subject durable.
	// Default: DefaultMaxWaiting (2).
	MaxWaiting int

	// MaxAckPending limits the number of in-flight unacknowledged messages the server will allow
	// for each per-subject durable. If zero, the server default is used. This is most useful when using
	// ManualAck and background processing to cap concurrent in-flight work at the server layer.
	MaxAckPending int

	// MaxConcurrentSubjects caps the total number of per-subject consumers/loops.
	// When exceeded, additional subjects are skipped with a warning and metric increment.
	MaxConcurrentSubjects int

	// AckPolicy controls JetStream ack policy. Defaults to AckExplicitPolicy.
	AckPolicy jetstream.AckPolicy

	// InactiveThreshold is how long an idle consumer is kept by the server before cleanup.
	// Default: DefaultInactiveThreshold (24h).
	InactiveThreshold time.Duration

	// PartitionRefreshMinInterval sets the minimum interval between forced claim refreshes
	// per partition when pull gating is enabled. This throttles KV hits when many
	// subjects are suppressed. Default: 500ms when zero.
	PartitionRefreshMinInterval time.Duration

	// Retry configures backoff behavior for control-plane operations.
	Retry RetryConfig

	// IteratorEscalationWindow defines the sliding time window used to aggregate
	// iterator failures for escalation detection. If zero, defaults to
	// DefaultIteratorEscalationWindow.
	IteratorEscalationWindow time.Duration

	// IteratorEscalationThreshold is the number of iterator failures within the
	// escalation window that triggers a single escalation (consumer refresh).
	// If zero, defaults to DefaultIteratorEscalationThreshold.
	IteratorEscalationThreshold int

	// AllowWorkerIDChange controls whether workerID changes are allowed after initialization.
	// Default: false (immutable once set). Intended for controlled migrations only.
	AllowWorkerIDChange bool

	// IteratorFactory optionally overrides the iterator creation logic for testing or
	// advanced customization. When nil, a default factory is used that configures
	// heartbeat and expiry based on BatchSize and FetchTimeout.
	IteratorFactory func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)
}

// DefaultWorkerConsumerConfig returns a WorkerConsumerConfig with sensible defaults.
// Note: Required fields (StreamName, ConsumerPrefix, SubjectTemplate) must still be set by the user.
func DefaultWorkerConsumerConfig() WorkerConsumerConfig {
	return WorkerConsumerConfig{
		AckPolicy:         jetstream.AckExplicitPolicy,
		AckWait:           DefaultAckWait,
		MaxDeliver:        DefaultMaxDeliver,
		InactiveThreshold: DefaultInactiveThreshold,
		BatchSize:         DefaultBatchSize,
		MaxWaiting:        DefaultMaxWaiting,
		FetchTimeout:      DefaultFetchTimeout,
		Retry: RetryConfig{
			Backoff:    DefaultRetryBackoff,
			Base:       DefaultRetryBase,
			Multiplier: DefaultRetryMultiplier,
			Max:        DefaultRetryMax,
		},
		IteratorEscalationWindow:    DefaultIteratorEscalationWindow,
		IteratorEscalationThreshold: DefaultIteratorEscalationThreshold,
		PartitionRefreshMinInterval: 500 * time.Millisecond,
		DrainOnRemoveTimeout:        10 * time.Second,
		Resolver: ResolverConfig{
			HandoffBucketName:   "parti-handoff",
			HandoffClaimsPrefix: "claims/",
		},
	}
}

// SetDefaults applies default values to zero-valued configuration fields.
func SetDefaults(cfg *WorkerConsumerConfig) {
	defaults := DefaultWorkerConsumerConfig()

	cfg.AckPolicy = defaultAckPolicy(cfg.AckPolicy)
	cfg.AckWait = defaultDuration(cfg.AckWait, defaults.AckWait)
	cfg.MaxDeliver = defaultInt(cfg.MaxDeliver, defaults.MaxDeliver)
	cfg.InactiveThreshold = defaultDuration(cfg.InactiveThreshold, defaults.InactiveThreshold)
	cfg.BatchSize = defaultInt(cfg.BatchSize, defaults.BatchSize)
	cfg.MaxWaiting = defaultInt(cfg.MaxWaiting, defaults.MaxWaiting)
	cfg.FetchTimeout = defaultDuration(cfg.FetchTimeout, defaults.FetchTimeout)

	// Retry defaults
	cfg.Retry.Backoff = defaultDuration(cfg.Retry.Backoff, defaults.Retry.Backoff)
	cfg.Retry.Base = defaultRetryBase(cfg.Retry.Base, cfg.Retry.Backoff, defaults.Retry.Base)
	cfg.Retry.Multiplier = defaultFloat(cfg.Retry.Multiplier, defaults.Retry.Multiplier)
	cfg.Retry.Max = defaultDuration(cfg.Retry.Max, defaults.Retry.Max)

	cfg.IteratorEscalationWindow = defaultDuration(cfg.IteratorEscalationWindow, defaults.IteratorEscalationWindow)
	cfg.IteratorEscalationThreshold = defaultInt(cfg.IteratorEscalationThreshold, defaults.IteratorEscalationThreshold)

	// Throttle default for resolver refreshes
	if cfg.PartitionRefreshMinInterval <= 0 {
		cfg.PartitionRefreshMinInterval = defaults.PartitionRefreshMinInterval
	}

	if cfg.Logger == nil {
		cfg.Logger = logging.NewNop()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.NewNop()
	}

	cfg.applyProcessingGateDefaults()
}

func (cfg *WorkerConsumerConfig) applyProcessingGateDefaults() {
	// ProcessingGate defaults when provided
	if cfg.ProcessingGate != nil {
		cfg.ProcessingGate.applyDefaults()

		// Apply bucket and prefix defaults when gate is enabled and no custom resolver
		if cfg.ProcessingGate.Enabled && cfg.Resolver.OwnershipResolver == nil {
			defaults := DefaultWorkerConsumerConfig()
			if cfg.Resolver.HandoffBucketName == "" {
				cfg.Resolver.HandoffBucketName = defaults.Resolver.HandoffBucketName
			}
			if cfg.Resolver.HandoffClaimsPrefix == "" {
				cfg.Resolver.HandoffClaimsPrefix = defaults.Resolver.HandoffClaimsPrefix
			}
		}

		// Enable pull gating by default to ensure exclusivity with per-subject durables,
		// regardless of resolver type, unless explicitly disabled by the caller.
		if cfg.ProcessingGate.Enabled && !cfg.PullGatingEnabled {
			cfg.PullGatingEnabled = true
		}

		// Drain defaults
		if cfg.DrainOnRemoveTimeout == 0 {
			cfg.DrainOnRemoveTimeout = 10 * time.Second
		}
	}
}

// helper defaults (unexported)
func defaultAckPolicy(p jetstream.AckPolicy) jetstream.AckPolicy {
	if p == 0 {
		return jetstream.AckExplicitPolicy
	}

	return p
}

func defaultDuration(v, def time.Duration) time.Duration {
	if v == 0 {
		return def
	}

	return v
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}

	return v
}

func defaultFloat(v, def float64) float64 {
	if v == 0 {
		return def
	}

	return v
}

func defaultRetryBase(base, backoff, def time.Duration) time.Duration {
	if base == 0 {
		if backoff > 0 {
			return backoff
		}

		return def
	}

	return base
}
