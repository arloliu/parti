package durable

import (
	"fmt"
	"time"

	"github.com/arloliu/fuda"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/recovery"
	"github.com/arloliu/parti/v2/types"
	"github.com/go-playground/validator/v10"
	"github.com/nats-io/nats.go/jetstream"
)

// RecoveryStrategy is an alias for [recovery.Strategy].
// It is kept here for backward compatibility with existing config structs.
type RecoveryStrategy = recovery.Strategy

// Recovery strategy constants re-exported from the recovery package.
const (
	RecoveryDisabled         = recovery.Disabled
	RecoverFromNew           = recovery.FromNew
	RecoverFromLastProcessed = recovery.FromLastProcessed
	RecoverFromBeginning     = recovery.FromBeginning
)

// RetryConfig groups retry backoff settings.
type RetryConfig struct {
	// Backoff is the delay between retries for control-plane operations.
	// Default: DefaultRetryBackoff (100ms).
	Backoff time.Duration `default:"100ms" validate:"gte=0"`

	// Max caps the jittered backoff.
	// Default: DefaultRetryMax (5s).
	Max time.Duration `default:"5s" validate:"gte=0,gtefield=Backoff"`

	// Multiplier grows the backoff window for decorrelated jitter.
	// Default: DefaultRetryMultiplier (1.6).
	Multiplier float64 `default:"1.6" validate:"gte=1"`

	// Base is the base backoff used for decorrelated jitter retries for control-plane ops.
	// Default: DefaultRetryBase (200ms). If zero and Backoff is set, Base falls back to Backoff.
	Base time.Duration `default:"200ms" validate:"gte=0"`

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
	HandoffBucketName string `default:"parti-handoff"`

	// HandoffClaimsPrefix is the key prefix for handoff claims in the KV bucket.
	// Defaults to "claims/" if empty and gate is enabled.
	HandoffClaimsPrefix string `default:"claims/"`

	// BatchWindow sets the resolver KV watch coalescing window for the
	// auto-created claim-based resolver when ProcessingGate is enabled.
	// If zero, a default (5ms) is used. Ignored when a custom OwnershipResolver is provided.
	BatchWindow time.Duration `default:"5ms" validate:"gte=0"`

	// BatchMaxItems caps the number of unique partition updates coalesced
	// into a single apply. If zero, a default (1024) is used. Ignored when a custom
	// OwnershipResolver is provided.
	BatchMaxItems int `default:"1024" validate:"gt=0"`

	// ReconcileInterval is the cadence at which the auto-created claim-based
	// resolver re-lists the handoff bucket and reconciles its cache against
	// KV. This is the recovery path for silent watcher stalls (the nats.go
	// KV watcher does NOT surface NATS server restarts as Updates() channel
	// close; only explicit Stop / connection close / subscription teardown
	// does). After such a stall the cache stays stale for at most one
	// reconcile period.
	//
	// Defaults to 30s when zero. Negative values are rejected at startup.
	// Ignored when a custom OwnershipResolver is provided.
	ReconcileInterval time.Duration `default:"30s" validate:"gte=0"`
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
	StreamName string `validate:"required"`

	// ConsumerPrefix is the prefix for the durable consumer name.
	// It must contain only alphanumeric characters (a-z, A-Z, 0-9), dashes (-), or underscores (_).
	// Required.
	ConsumerPrefix string `validate:"required"`

	// SubjectTemplate is a text/template used to build subjects from a partition.
	// Uses Go's text/template syntax.
	//
	// Available template fields:
	//   - {{.PartitionID}}: Partition keys joined with "." (equals partition.SubjectKey())
	//
	// Note: Only PartitionID is available. Other Partition fields (Keys, Weight) are not
	// directly accessible in the template.
	//
	// Example: "orders.{{.PartitionID}}.events" with Keys=["region", "us-east"] produces
	// "orders.region.us-east.events"
	//
	// Required.
	SubjectTemplate string `validate:"required"`

	// Logger provides structured logging. Defaults to a no-op logger when nil.
	Logger types.Logger

	// Metrics is the metrics collector for worker consumer operations.
	// If nil, no metrics are emitted from the worker consumer helper.
	Metrics types.WorkerConsumerMetrics

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
	//
	// Default: nil (processing gate disabled).
	ProcessingGate *ProcessingGateConfig `validate:"omitempty"`

	// Resolver configures the ownership resolver when ProcessingGate is enabled.
	Resolver ResolverConfig

	// PullGatingEnabled is the resolved decision for pre-pull ownership/state
	// gating on per-subject consumers. When true, pulls are suppressed (not
	// issued) if the current worker is not the owner or the handoff state is
	// not Commit/Stable, which reduces NAK churn during handoffs. SetDefaults
	// may auto-enable this when the processing gate is on; see
	// PullGatingConfigured.
	PullGatingEnabled bool

	// PullGatingConfigured records whether PullGatingEnabled reflects an
	// explicit caller choice. When false (the zero value), SetDefaults treats
	// pull gating as "not configured" and auto-enables it if the processing
	// gate is enabled. When true, SetDefaults preserves PullGatingEnabled as
	// given, so an explicit disable survives. A direct constructor that wants
	// pull gating off under an enabled processing gate must therefore set this
	// to true alongside PullGatingEnabled: false.
	PullGatingConfigured bool

	// DrainOnRemove enables a graceful per-subject drain when a subject is removed
	// from the worker's assignment. When true, the consumer helper will stop issuing
	// new pulls for the subject and wait for pending acknowledgements to reach zero
	// (or until DrainOnRemoveTimeout elapses) before cancelling the loop. This reduces
	// NAK churn and minimizes gaps during scale-down and rebalancing.
	// Default: false (immediate cancellation as before).
	DrainOnRemove bool

	// DrainOnRemoveTimeout caps the drain wait per subject when DrainOnRemove is enabled.
	// Default: 10s when zero.
	DrainOnRemoveTimeout time.Duration `default:"10s" validate:"gte=0"`

	// AckWait is the time allowed for processing before redelivery.
	// Default: DefaultAckWait (30s).
	AckWait time.Duration `default:"30s" validate:"gt=0"`

	// MaxDeliver is the maximum redelivery attempts before moving to DLQ (if configured).
	// Default: DefaultMaxDeliver (-1 = unlimited attempts; relies on server default/unlimited behavior).
	MaxDeliver int `default:"-1" validate:"gte=-1"`

	// BatchSize is the max number of messages to pull per iterator request.
	// Default: DefaultBatchSize (1).
	BatchSize int `default:"1" validate:"gt=0"`

	// FetchTimeout is the max time to wait when pulling a batch.
	// Default: DefaultFetchTimeout (5s).
	FetchTimeout time.Duration `default:"5s" validate:"gt=0"`

	// MaxWaiting caps outstanding pull requests for each per-subject durable.
	// Default: DefaultMaxWaiting (2).
	MaxWaiting int `default:"2" validate:"gt=0"`

	// MaxAckPending limits the number of in-flight unacknowledged messages the server will allow
	// for each per-subject durable. If zero, the server default is used. This is most useful when using
	// ManualAck and background processing to cap concurrent in-flight work at the server layer.
	MaxAckPending int `validate:"gte=0"`

	// ConsumerMemoryStorage forwards to jetstream.ConsumerConfig.MemoryStorage
	// on consumer create. When true, the consumer's delivery/ack state is
	// kept in memory rather than inheriting the stream's storage type.
	// See consumer.WithConsumerMemoryStorage for full semantics and the
	// non-live-editable caveat.
	// Default: false (inherit stream storage type).
	ConsumerMemoryStorage bool

	// ConsumerReplicas overrides jetstream.ConsumerConfig.Replicas on
	// consumer create. 0 (default) inherits the parent stream's replica
	// count; lower values reduce consumer-state raft replication.
	// See consumer.WithConsumerReplicas for the validation rule (must be
	// ≤ stream replicas, NATS error 10126 on violation).
	ConsumerReplicas int

	// MaxConcurrentSubjects caps the total number of per-subject consumers/loops.
	// When exceeded, additional subjects are skipped with a warning and metric increment.
	// Default: 0 (no cap).
	MaxConcurrentSubjects int `validate:"gte=0"`

	// AckPolicy controls JetStream ack policy. Defaults to AckExplicitPolicy.
	AckPolicy jetstream.AckPolicy

	// InactiveThreshold is how long an idle consumer is kept by the server before cleanup.
	// Default: DefaultInactiveThreshold (24h).
	InactiveThreshold time.Duration `default:"24h" validate:"gt=0"`

	// PartitionRefreshMinInterval sets the minimum interval between forced claim refreshes
	// per partition when pull gating is enabled. This throttles KV hits when many
	// subjects are suppressed. Default: 500ms when zero.
	PartitionRefreshMinInterval time.Duration `default:"500ms" validate:"gt=0"`

	// Retry configures backoff behavior for control-plane operations.
	Retry RetryConfig

	// IteratorEscalationWindow defines the sliding time window used to aggregate
	// iterator failures for escalation detection. If zero, defaults to
	// DefaultIteratorEscalationWindow (60s).
	IteratorEscalationWindow time.Duration `default:"60s" validate:"gt=0"`

	// IteratorEscalationThreshold is the number of iterator failures within the
	// escalation window that triggers a single escalation (consumer refresh).
	// If zero, defaults to DefaultIteratorEscalationThreshold (3).
	IteratorEscalationThreshold int `default:"3" validate:"gt=0"`

	// AllowWorkerIDChange controls whether workerID changes are allowed after initialization.
	// Default: false (immutable once set). Intended for controlled migrations only.
	AllowWorkerIDChange bool

	// RecoveryStrategy defines how a recreated consumer resumes after an unexpected deletion.
	// Default: RecoveryDisabled (no auto-recovery).
	RecoveryStrategy RecoveryStrategy

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

// SetDefaults sets default values for the configuration.
func (c *WorkerConsumerConfig) SetDefaults() error {
	if err := fuda.SetDefaults(c); err != nil {
		return fmt.Errorf("failed to set defaults: %w", err)
	}

	if c.Logger == nil {
		c.Logger = logging.NewNop()
	}
	if c.Metrics == nil {
		c.Metrics = metrics.NewNop()
	}

	// Apply defaults for nested structs
	if c.ProcessingGate != nil {
		if err := c.ProcessingGate.applyDefaults(); err != nil {
			return err
		}

		// Enable pull gating by default to ensure exclusivity with per-subject
		// durables, regardless of resolver type. Only an unconfigured
		// pull-gating setting is auto-enabled here, so a deliberate disable
		// survives. The rule is idempotent: PullGatingConfigured is never
		// cleared, so the repeated SetDefaults call from Validate preserves
		// this decision.
		if c.ProcessingGate.Enabled && !c.PullGatingConfigured {
			c.PullGatingEnabled = true
		}
	}

	return nil
}

// Validate checks configuration constraints.
func (c *WorkerConsumerConfig) Validate() error {
	if err := c.SetDefaults(); err != nil {
		return err
	}

	validate := validator.New(validator.WithRequiredStructEnabled())
	if err := validate.Struct(c); err != nil {
		return fmt.Errorf("configuration validation failed: %w", err)
	}

	for _, r := range c.ConsumerPrefix {
		if !isAllowedConsumerRune(r) {
			return fmt.Errorf("consumer prefix %q contains invalid characters (allowed: a-z, A-Z, 0-9, -, _)", c.ConsumerPrefix)
		}
	}
	if err := validateSubjectTemplate(c.SubjectTemplate, true); err != nil {
		return fmt.Errorf("subject template is invalid: %w", err)
	}

	return nil
}
