package consumer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/arloliu/fuda"
	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/jsutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// Dynamic is a partition-aware consumer that receives assignments from a Parti Manager.
// It manages multiple internal consumers based on assigned partitions.
//
// # Lifecycle
//
// Create with [NewDynamic], then call [Dynamic.Update] to start consuming assigned
// partitions. Clean up with [Dynamic.Stop]:
//
//	consumer, err := consumer.NewDynamic(js, "stream", "worker", "orders.{{.PartitionID}}", handler)
//	if err != nil { log.Fatal(err) }
//	defer consumer.Stop(ctx)
//
//	// Start consuming partitions (typically called by Parti Manager)
//	if err := consumer.Update(ctx, "worker-0", partitions); err != nil { log.Fatal(err) }
//
// Unlike [Static] and [Queue], Dynamic does NOT have a Start method. Consumption
// begins when [Dynamic.Update] is called with a non-empty partition list.
//
// # Thread Safety
//
// Dynamic is safe for concurrent use. [Dynamic.Update] calls are serialized
// internally to prevent race conditions during assignment changes.
type Dynamic struct {
	inner            *durable.WorkerConsumer
	js               jetstream.JetStream
	streamName       string
	recoveryStrategy RecoveryStrategy
	workQueueOnce    sync.Once
	workQueueErr     error
}

// DynamicConfig configures a Dynamic consumer.
type DynamicConfig struct {
	CommonConfig

	// StreamName is the JetStream stream to consume from.
	// Required.
	StreamName string `validate:"required"`

	// SubjectTemplate is a text/template for building subjects from partitions.
	//
	// It relies on the standard Go text/template package.
	// The template context provides a {{.PartitionID}} variable.
	//
	// Example: "orders.{{.PartitionID}}.events"
	SubjectTemplate string `validate:"required"`

	// ConsumerPrefix is the prefix for the durable consumer name.
	//
	// The final durable name is constructed dynamically for each assigned partition:
	// "<ConsumerPrefix>_<partitionID>_<hash>".
	//
	// This ensures unique, stable durability for each partition assignment.
	ConsumerPrefix string `validate:"required"`

	// ProcessingGate configures optional exclusive processing enforcement.
	//
	// When enabled, the WorkerConsumer uses a distributed lock (via KV) to ensure
	// that it is the *only* active processor for its assigned partitions.
	// This prevents split-brain processing during rebalances.
	ProcessingGate *ProcessingGateConfig

	// Resolver configures the ownership resolver used when ProcessingGate is enabled.
	//
	// It defines how ownership is claimed, refreshed, and verified.
	Resolver ResolverConfig

	// PullGatingEnabled enables pre-pull ownership/state gating for consumers.
	//
	// When true, the consumer will check if it still owns the partition before
	// issuing a pull request to JetStream. This reduces "ghost" processing of
	// messages after assignment revocation.
	PullGatingEnabled bool

	// DrainOnRemove enables graceful draining when a partition assignment is revoked.
	//
	// When true, the consumer will stop pulling new messages but finish processing
	// buffered messages before shutting down the partition consumer.
	DrainOnRemove bool

	// DrainOnRemoveTimeout caps the time spent draining a revoked partition.
	//
	// If draining takes longer than this timeout, the consumer is forcibly closed.
	// Default: 10s.
	DrainOnRemoveTimeout time.Duration `default:"10s" validate:"gte=0"`

	// MaxConcurrentSubjects limits the number of partitions (subjects) processed concurrently.
	//
	// If the manager assigns more partitions than this limit, excess partitions
	// will be ignored (and logged/warned).
	MaxConcurrentSubjects int `validate:"gte=0"`

	// AllowWorkerIDChange controls whether the worker's identity can change during runtime.
	//
	// Default: false (immutable once set). Changing WorkerID usually requires a restart.
	AllowWorkerIDChange bool

	// Retry configures the backoff behavior for control-plane operations
	// (e.g., initial connection, creating consumers).
	Retry RetryConfig

	// IteratorEscalationWindow defines the sliding time window used to aggregate
	// iterator failures for escalation detection.
	//
	// If too many iterator errors occur within this window, the consumer will
	// attempt to escalate recovery.
	//
	// Default: 60s.
	IteratorEscalationWindow time.Duration `default:"60s" validate:"gt=0"`

	// IteratorEscalationThreshold is the number of iterator failures within the
	// escalation window that triggers consumer refresh/escalation.
	//
	// Default: 3.
	IteratorEscalationThreshold int `default:"3" validate:"gt=0"`

	// PartitionRefreshMinInterval sets the minimum interval between forced claim refreshes
	// per partition when pull gating is enabled.
	//
	// This prevents excessive load on the coordination backend (KV) during high-throughput pulling.
	//
	// Default: 500ms.
	PartitionRefreshMinInterval time.Duration `default:"500ms" validate:"gt=0"`

	// IteratorFactory optionally overrides the internal iterator creation logic.
	// This is primarily used for testing to inject mock iterators.
	IteratorFactory func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)

	// RecoveryStrategy defines how a recreated consumer resumes after an unexpected deletion.
	//
	// All strategies are supported. [RecoverFromLastProcessed] works with both
	// ManualAck=false (checkpoint advances automatically) and ManualAck=true
	// (checkpoint advances when the handler calls msg.Ack() or msg.DoubleAck()).
	//
	// Note: Dynamic consumers have a separate iterator-escalation detector (see
	// [DynamicConfig.IteratorEscalationWindow]) that rebinds to the existing durable
	// on repeated failures. RecoveryStrategy controls a distinct, complementary mechanism
	// that recreates the durable itself when it has been deleted.
	//
	// # WorkQueuePolicy streams
	//
	// NATS only permits [jetstream.DeliverAllPolicy] on WorkQueuePolicy streams.
	// [RecoverFromNew] and [RecoverFromLastProcessed] are incompatible and will
	// cause the first [Dynamic.Update] call to return [ErrInvalidConfig]. Use
	// [RecoverFromBeginning] or [RecoveryDisabled] for WorkQueuePolicy streams.
	RecoveryStrategy RecoveryStrategy
}

// NewDynamic creates a new dynamic partition consumer.
func NewDynamic(
	js jetstream.JetStream,
	streamName, consumerPrefix, subjectTemplate string,
	handler MessageHandler,
	opts ...DynamicOption,
) (*Dynamic, error) {
	if js == nil {
		return nil, fmt.Errorf("%w: JetStream context is required", ErrInvalidConfig)
	}
	if streamName == "" {
		return nil, fmt.Errorf("%w: stream name is required", ErrInvalidConfig)
	}
	if consumerPrefix == "" {
		return nil, fmt.Errorf("%w: consumer prefix is required", ErrInvalidConfig)
	}
	if subjectTemplate == "" {
		return nil, fmt.Errorf("%w: subject template is required", ErrInvalidConfig)
	}
	if handler == nil {
		return nil, fmt.Errorf("%w: message handler is required", ErrInvalidConfig)
	}

	// Apply options
	o := defaultOptions()
	for _, opt := range opts {
		opt.apply(&o)
	}

	// Build configuration
	cfg := DynamicConfig{
		CommonConfig: CommonConfig{
			Logger:            o.logger,
			Metrics:           o.metrics,
			ManualAck:         o.manualAck,
			AckWait:           o.ackWait,
			MaxDeliver:        o.maxDeliver,
			BatchSize:         o.batchSize,
			FetchTimeout:      o.fetchTimeout,
			MaxWaiting:        o.maxWaiting,
			MaxAckPending:     o.maxAckPending,
			InactiveThreshold: o.inactiveThreshold,
			AckPolicy:         o.ackPolicy,

			ConsumerMemoryStorage: o.consumerMemoryStorage,
			ConsumerReplicas:      o.consumerReplicas,
		},
		StreamName:                  streamName,
		ConsumerPrefix:              consumerPrefix,
		SubjectTemplate:             subjectTemplate,
		ProcessingGate:              o.processingGate,
		Resolver:                    o.resolver,
		PullGatingEnabled:           o.pullGatingEnabled,
		DrainOnRemove:               o.drainOnRemove,
		DrainOnRemoveTimeout:        o.drainOnRemoveTimeout,
		MaxConcurrentSubjects:       o.maxConcurrentSubjects,
		AllowWorkerIDChange:         o.allowWorkerIDChange,
		Retry:                       o.retry,
		IteratorEscalationWindow:    o.iteratorEscalationWindow,
		IteratorEscalationThreshold: o.iteratorEscalationThreshold,
		PartitionRefreshMinInterval: o.partitionRefreshMinInterval,
		IteratorFactory:             o.iteratorFactory,
		RecoveryStrategy:            o.recoveryStrategy,
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// Build worker consumer config from unified DynamicConfig.
	workerCfg := durable.WorkerConsumerConfig{
		StreamName:                  cfg.StreamName,
		ConsumerPrefix:              cfg.ConsumerPrefix,
		SubjectTemplate:             cfg.SubjectTemplate,
		Logger:                      cfg.Logger,
		Metrics:                     cfg.Metrics,
		ManualAck:                   cfg.ManualAck,
		AckWait:                     cfg.AckWait,
		MaxDeliver:                  cfg.MaxDeliver,
		BatchSize:                   cfg.BatchSize,
		FetchTimeout:                cfg.FetchTimeout,
		MaxWaiting:                  cfg.MaxWaiting,
		MaxAckPending:               cfg.MaxAckPending,
		InactiveThreshold:           cfg.InactiveThreshold,
		AckPolicy:                   cfg.AckPolicy,
		ConsumerMemoryStorage:       cfg.ConsumerMemoryStorage,
		ConsumerReplicas:            cfg.ConsumerReplicas,
		ProcessingGate:              toSubscriptionGateConfig(cfg.ProcessingGate),
		Resolver:                    toSubscriptionResolverConfig(cfg.Resolver),
		PullGatingEnabled:           cfg.PullGatingEnabled,
		DrainOnRemove:               cfg.DrainOnRemove,
		DrainOnRemoveTimeout:        cfg.DrainOnRemoveTimeout,
		MaxConcurrentSubjects:       cfg.MaxConcurrentSubjects,
		AllowWorkerIDChange:         cfg.AllowWorkerIDChange,
		PartitionRefreshMinInterval: cfg.PartitionRefreshMinInterval,
		IteratorEscalationWindow:    cfg.IteratorEscalationWindow,
		IteratorEscalationThreshold: cfg.IteratorEscalationThreshold,
		RecoveryStrategy:            cfg.RecoveryStrategy,
		IteratorFactory:             cfg.IteratorFactory,
		Retry: durable.RetryConfig{
			Backoff:    cfg.Retry.Backoff,
			Max:        cfg.Retry.Max,
			Multiplier: cfg.Retry.Multiplier,
			Base:       cfg.Retry.Base,
			Seed:       cfg.Retry.Seed,
		},
	}

	inner, err := durable.NewWorkerConsumer(js, workerCfg, handler.Handle)
	if err != nil {
		return nil, err
	}

	return &Dynamic{
		inner:            inner,
		js:               js,
		streamName:       streamName,
		recoveryStrategy: o.recoveryStrategy,
	}, nil
}

// Update applies a new partition assignment set.
//
// This method creates or binds durable consumers for newly assigned partitions
// and stops consumers for removed partitions. The underlying JetStream consumers
// are NOT deleted on removal; they will be garbage-collected by the server after
// InactiveThreshold.
//
// Update is typically called by the Parti Manager when assignments change.
// On the first call, this starts consuming the assigned partitions.
//
// Parameters:
//   - ctx: Context for the update operation. Used for JetStream API calls.
//   - workerID: The stable worker ID for this instance (e.g., "worker-0").
//   - partitions: The new set of partitions to consume. Empty list stops all.
//
// Returns:
//   - error: Non-nil if partition creation fails or if workerID mutation is
//     disallowed (see [DynamicConfig.AllowWorkerIDChange]).
//
// Errors:
//   - [ErrWorkerIDMutation]: Returned when workerID changes and
//     AllowWorkerIDChange is false.
//   - [ErrMaxSubjectsExceeded]: Returned when partition count
//     exceeds MaxConcurrentSubjects.
func (d *Dynamic) Update(ctx context.Context, workerID string, partitions []types.Partition) error {
	d.workQueueOnce.Do(func() {
		d.workQueueErr = CheckWorkQueueRecoveryCompat(ctx, d.js, d.streamName, d.recoveryStrategy)
	})
	if d.workQueueErr != nil {
		return d.workQueueErr
	}
	return d.inner.UpdateWorkerConsumer(ctx, workerID, partitions)
}

// UpdateWorkerConsumer is an alias for [Dynamic.Update] that implements the
// WorkerConsumerUpdater interface used by the Parti Manager.
//
// Deprecated: Use [Dynamic.Update] for new code. This method exists for
// backward compatibility with code that expects the WorkerConsumerUpdater
// interface.
func (d *Dynamic) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []types.Partition) error {
	return d.inner.UpdateWorkerConsumer(ctx, workerID, partitions)
}

// Capabilities forwards to the inner [durable.WorkerConsumer]; implements
// the [parti.CapabilityReporter] interface so the Manager's type-assertion
// on the registered *Dynamic updater succeeds.
func (d *Dynamic) Capabilities() uint32 {
	return d.inner.Capabilities()
}

// Close stops all partition consumers.
//
// Stop gracefully stops all partition consumers.
//
// Stop cancels all internal pull loops and waits for pending message processing
// to complete (up to the context deadline). The underlying JetStream consumers
// are NOT deleted; they will be garbage-collected by the server after
// InactiveThreshold.
//
// If DrainOnRemove is enabled, Stop will first drain pending messages
// (up to DrainOnRemoveTimeout) before stopping.
//
// Stop is idempotent; calling it multiple times is safe.
//
// Parameters:
//   - ctx: Context with shutdown deadline. If the deadline expires, Stop
//     returns [context.DeadlineExceeded] but consumers will still eventually stop.
//
// Returns:
//   - error: Context error if the wait times out; nil otherwise.
func (d *Dynamic) Stop(ctx context.Context) error {
	return d.inner.Close(ctx)
}

// SetResolverMetrics sets the metrics collector for the ownership resolver.
//
// This is an advanced method for observability integration. Most users do not
// need to call this directly.
func (d *Dynamic) SetResolverMetrics(m ResolverMetrics) {
	d.inner.SetResolverMetrics(m)
}

// SetDefaults applies default values to the configuration.
func (c *DynamicConfig) SetDefaults() error {
	return fuda.SetDefaults(c)
}

// Validate checks configuration constraints.
func (c *DynamicConfig) Validate() error {
	if err := c.SetDefaults(); err != nil {
		return err
	}
	if err := fuda.Validate(c); err != nil {
		return err
	}

	if !jsutil.IsValidConsumerName(c.ConsumerPrefix) {
		return fmt.Errorf("consumer prefix %q contains invalid characters (allowed: a-z, A-Z, 0-9, -, _)", c.ConsumerPrefix)
	}

	return nil
}

// toSubscriptionGateConfig converts a consumer-owned ProcessingGateConfig to
// the internal subscription equivalent. Returns nil when cfg is nil.
func toSubscriptionGateConfig(cfg *ProcessingGateConfig) *durable.ProcessingGateConfig {
	if cfg == nil {
		return nil
	}

	return &durable.ProcessingGateConfig{
		Enabled:             cfg.Enabled,
		AllowedStates:       cfg.AllowedStates,
		WarmupDuration:      cfg.WarmupDuration,
		WarmupAllowedStates: cfg.WarmupAllowedStates,
		NakDelay:            cfg.NakDelay,
		NakJitter:           cfg.NakJitter,
		Debug:               cfg.Debug,
		Metrics:             cfg.Metrics, // consumer.GateMetrics satisfies durable.GateMetrics
	}
}

// toSubscriptionResolverConfig converts a consumer-owned ResolverConfig to the
// internal subscription equivalent.
func toSubscriptionResolverConfig(cfg ResolverConfig) durable.ResolverConfig {
	return durable.ResolverConfig{
		OwnershipResolver:   cfg.OwnershipResolver,
		HandoffBucketName:   cfg.HandoffBucketName,
		HandoffClaimsPrefix: cfg.HandoffClaimsPrefix,
		BatchWindow:         cfg.BatchWindow,
		BatchMaxItems:       cfg.BatchMaxItems,
		ReconcileInterval:   cfg.ReconcileInterval,
	}
}
