package consumer

import (
	"context"
	"fmt"

	"github.com/arloliu/fuda"
	"github.com/arloliu/parti/v2/internal/ipartition"
	"github.com/arloliu/parti/v2/jsutil"
	"github.com/arloliu/parti/v2/partition"
	"github.com/nats-io/nats.go/jetstream"
)

// Static is a consumer bound to a single, fixed partition.
// Use for StatefulSet deployments where pod ordinal determines partition.
//
// # Lifecycle
//
// Create with [NewStatic], start consumption with [Static.Start], and clean up
// with [Static.Stop]:
//
//	consumer, err := consumer.NewStatic(js, "stream", "consumer-0", "events.{{partition}}", 10, 0, handler)
//	if err != nil { log.Fatal(err) }
//	defer consumer.Stop(ctx)
//
//	if err := consumer.Start(ctx); err != nil { log.Fatal(err) }
//
// # Thread Safety
//
// Static is safe for concurrent use. [Static.Start] and [Static.Stop] are
// serialized internally.
type Static struct {
	inner            *ipartition.JSConsumer
	js               jetstream.JetStream
	streamName       string
	recoveryStrategy RecoveryStrategy
}

// StaticConfig configures a Static consumer.
type StaticConfig struct {
	CommonConfig

	// StreamName is the JetStream stream to consume from.
	// Required.
	StreamName string `validate:"required"`

	// NumPartitions is the total number of partitions for the stream.
	//
	// This defines the sharding factor. Messages are consistently hashed
	// to a partition index in the range [0, NumPartitions-1].
	// Must be > 0.
	//
	// WARNING: Changing NumPartitions changes the hash mapping.
	NumPartitions int `validate:"required,gt=0"`

	// Partition is the specific partition index this consumer should process.
	//
	// Must be in the range [0, NumPartitions-1].
	// Typically assigned based on the application instance's ordinal (e.g., StatefulSet index).
	Partition int `validate:"gte=0"`

	// SubjectPattern is the subject template with placeholders.
	//
	// Placeholders:
	//   - {{partition}} - Replaced with partition index (0 to N-1). Required.
	//   - {{key}}       - Replaced with the partition key. Optional.
	//
	// Placeholders must occupy a full token between dots. Embedded placeholders
	// like "events.{{partition}}-v1" are invalid.
	//
	// Examples:
	//   - "events.completed.{{partition}}"       → "events.completed.0"
	//   - "events.{{key}}.{{partition}}"         → "events.tool-abc.3"
	//   - "orders.{{partition}}.{{key}}.created" → "orders.2.customer-xyz.created"
	//
	// Validation:
	//   - Must contain {{partition}} placeholder
	//   - Must not produce empty NATS subject tokens (e.g., "events..{{partition}}" is invalid)
	SubjectPattern string `validate:"required"`

	// ConsumerName is the durable consumer name.
	//
	// Must be unique per partition.
	// The final durable name on the server often incorporates the partition index
	// to avoid collisions between partitions, or the user must ensure uniqueness.
	ConsumerName string `validate:"required"`

	// HashSeed is an optional seed for the consistent hashing algorithm.
	//
	// Using a consistent seed ensures that the same message key always maps to
	// the same partition index across restarts/redeployments.
	HashSeed uint64

	// RecoveryStrategy defines how a recreated consumer resumes after an unexpected deletion.
	//
	// All strategies are supported. [RecoverFromLastProcessed] works with both
	// ManualAck=false (checkpoint advances automatically) and ManualAck=true
	// (checkpoint advances when the handler calls msg.Ack() or msg.DoubleAck()).
	//
	// # WorkQueuePolicy streams
	//
	// NATS only permits [jetstream.DeliverAllPolicy] on WorkQueuePolicy streams.
	// [RecoverFromNew] and [RecoverFromLastProcessed] are incompatible and will
	// cause [Static.Start] to return [ErrInvalidConfig]. Use [RecoverFromBeginning]
	// or [RecoveryDisabled] for WorkQueuePolicy streams.
	RecoveryStrategy RecoveryStrategy
}

// NewStatic creates a new static partition consumer.
func NewStatic(
	js jetstream.JetStream,
	streamName, consumerName, subjectPattern string,
	numPartitions, partitionIndex int,
	handler MessageHandler,
	opts ...StaticOption,
) (*Static, error) {
	if js == nil {
		return nil, fmt.Errorf("%w: JetStream context is required", ErrInvalidConfig)
	}
	if streamName == "" {
		return nil, fmt.Errorf("%w: stream name is required", ErrInvalidConfig)
	}
	if consumerName == "" {
		return nil, fmt.Errorf("%w: consumer name is required", ErrInvalidConfig)
	}
	if subjectPattern == "" {
		return nil, fmt.Errorf("%w: subject pattern is required", ErrInvalidConfig)
	}
	if numPartitions <= 0 {
		return nil, fmt.Errorf("%w: num partitions must be greater than 0", ErrInvalidConfig)
	}
	if partitionIndex < 0 || partitionIndex >= numPartitions {
		return nil, fmt.Errorf("%w: partition index out of range", ErrInvalidConfig)
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
	cfg := StaticConfig{
		CommonConfig: CommonConfig{
			Logger:            o.logger,
			Metrics:           o.metrics,
			ManualAck:         o.manualAck,
			AckWait:           o.ackWait,
			MaxDeliver:        o.maxDeliver,
			BatchSize:         o.batchSize,
			FetchTimeout:      o.fetchTimeout,
			PullHeartbeatCap:  o.pullHeartbeatCap,
			MaxWaiting:        o.maxWaiting,
			MaxAckPending:     o.maxAckPending,
			InactiveThreshold: o.inactiveThreshold,
			AckPolicy:         o.ackPolicy,

			ConsumerMemoryStorage: o.consumerMemoryStorage,
			ConsumerReplicas:      o.consumerReplicas,
		},
		StreamName:       streamName,
		ConsumerName:     consumerName,
		SubjectPattern:   subjectPattern,
		NumPartitions:    numPartitions,
		Partition:        partitionIndex,
		HashSeed:         o.hashSeed,
		RecoveryStrategy: o.recoveryStrategy,
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// Build consumer config from unified StaticConfig.
	partitionCfg := ipartition.ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  cfg.NumPartitions,
			SubjectPattern: cfg.SubjectPattern,
			HashSeed:       cfg.HashSeed,
			Logger:         cfg.Logger,
		},
		StreamName:        cfg.StreamName,
		ConsumerName:      cfg.ConsumerName,
		Partition:         cfg.Partition,
		BatchSize:         cfg.BatchSize,
		FetchTimeout:      cfg.FetchTimeout,
		PullHeartbeatCap:  cfg.PullHeartbeatCap,
		ManualAck:         cfg.ManualAck,
		MaxDeliver:        cfg.MaxDeliver,
		Logger:            cfg.Logger,
		DispatchByKey:     o.dispatchByKey,
		KeyChannelBuffer:  o.keyChannelBuffer,
		KeyIdleTimeout:    o.keyIdleTimeout,
		KeyExtractor:      o.keyExtractor,
		AckWait:           cfg.AckWait,
		MaxAckPending:     cfg.MaxAckPending,
		InactiveThreshold: cfg.InactiveThreshold,
		AckPolicy:         cfg.AckPolicy,
		Metrics:           o.metrics,
		MaxWaiting:        cfg.MaxWaiting,
		RecoveryStrategy:  cfg.RecoveryStrategy,

		ConsumerMemoryStorage: cfg.ConsumerMemoryStorage,
		ConsumerReplicas:      cfg.ConsumerReplicas,
	}

	inner, err := ipartition.NewJSConsumer(js, partitionCfg, handler.Handle)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	return &Static{
		inner:            inner,
		js:               js,
		streamName:       streamName,
		recoveryStrategy: o.recoveryStrategy,
	}, nil
}

// Start begins consuming messages in a background goroutine.
//
// The consumer creates or binds to a durable JetStream consumer and starts
// a pull loop. Messages are delivered to the handler configured at creation.
//
// Start may only be called once. Calling Start on an already-started consumer
// returns an error. After [Static.Stop] is called, Start returns
// [ErrConsumerStopped]; create a new instance to consume again.
//
// Parameters:
//   - ctx: Context for lifecycle control. Cancellation stops the consumer.
//
// Returns:
//   - error: Non-nil if the consumer is already started, [ErrConsumerStopped]
//     if the consumer has been stopped, or a JetStream consumer creation error.
func (s *Static) Start(ctx context.Context) error {
	if s.inner.Stopped() {
		return fmt.Errorf("static consumer: %w", ErrConsumerStopped)
	}
	if err := CheckWorkQueueRecoveryCompat(ctx, s.js, s.streamName, s.recoveryStrategy); err != nil {
		return err
	}
	return s.inner.Start(ctx)
}

// Stop gracefully stops the consumer.
//
// Stop cancels the internal pull loop and waits for pending message processing
// to complete (up to the context deadline). The underlying JetStream consumer
// is NOT deleted; it will be garbage-collected by the server after InactiveThreshold.
//
// Stop is terminal: after Stop returns, any subsequent call to [Static.Start]
// returns [ErrConsumerStopped]. Create a new instance to consume again.
//
// If the consumer is stopped while still registered with a running manager, the
// manager will retry the failed update indefinitely (by design). Stop the manager
// first, or deregister this consumer before calling Stop.
//
// Stop is idempotent; calling it multiple times is safe.
//
// Parameters:
//   - ctx: Context with shutdown deadline. If the deadline expires, Stop returns
//     [context.DeadlineExceeded] but the consumer will still eventually stop.
//
// Returns:
//   - error: Context error if the wait times out; nil otherwise.
func (s *Static) Stop(ctx context.Context) error {
	return s.inner.Stop(ctx)
}

// Partition returns the partition index this consumer handles.
//
// Returns:
//   - int: The zero-based partition index (0 to NumPartitions-1).
func (s *Static) Partition() int {
	return s.inner.Partition()
}

// Subject returns the NATS subject this consumer subscribes to.
//
// The subject is derived from the SubjectPattern with the partition placeholder
// replaced by the actual partition index. If the pattern contains {{key}},
// it is replaced with a wildcard (*) for subscription.
//
// Returns:
//   - string: The filter subject, e.g., "events.*.0" or "orders.2.>".
func (s *Static) Subject() string {
	return s.inner.Subject()
}

// SetDefaults applies default values to the configuration.
func (c *StaticConfig) SetDefaults() error {
	return fuda.SetDefaults(c)
}

// Validate checks configuration constraints.
func (c *StaticConfig) Validate() error {
	if err := c.SetDefaults(); err != nil {
		return err
	}
	if err := fuda.Validate(c); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	if err := validateFetchTimeoutFloor(c.FetchTimeout); err != nil {
		return err
	}

	if err := validatePullHeartbeatCap(c.PullHeartbeatCap); err != nil {
		return err
	}

	if !jsutil.IsValidConsumerName(c.ConsumerName) {
		return fmt.Errorf("%w: consumer name %q contains invalid characters (allowed: a-z, A-Z, 0-9, -, _)", ErrInvalidConfig, c.ConsumerName)
	}

	// Cross-field validation: partition must be within range
	if c.Partition >= c.NumPartitions {
		return fmt.Errorf("%w: partition index %d out of range [0, %d)", ErrInvalidConfig, c.Partition, c.NumPartitions)
	}

	return nil
}

// ParseStatefulSetOrdinal extracts the ordinal index from a StatefulSet pod hostname.
//
// Wraps [partition.ParseStatefulSetOrdinal].
func ParseStatefulSetOrdinal(hostname string) (int, error) {
	return partition.ParseStatefulSetOrdinal(hostname)
}

// GetPartitionFromEnv reads the partition index from environment.
// Wraps [partition.GetPartitionFromEnv].
func GetPartitionFromEnv() (int, error) {
	return partition.GetPartitionFromEnv()
}
