package consumer

import (
	"context"
	"errors"
	"fmt"

	"github.com/arloliu/fuda"
	"github.com/arloliu/parti/jsutil"
	"github.com/arloliu/parti/partition"
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
//
// # Deprecation Notice
//
// This type wraps [partition.JSConsumer]. Future versions may deprecate the
// partition package in favor of this unified consumer API.
type Static struct {
	inner *partition.JSConsumer
}

// StaticConfig configures a Static consumer.
// Uses unified naming; converted to partition.ConsumerConfig internally.
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
}

// NewStatic creates a new static partition consumer.
func NewStatic(
	js jetstream.JetStream,
	streamName, consumerName, subjectPattern string,
	numPartitions, partIdx int,
	handler MessageHandler,
	opts ...StaticOption,
) (*Static, error) {
	if js == nil {
		return nil, errors.New("JetStream context is required")
	}
	if streamName == "" {
		return nil, errors.New("stream name is required")
	}
	if consumerName == "" {
		return nil, errors.New("consumer name is required")
	}
	if subjectPattern == "" {
		return nil, errors.New("subject pattern is required")
	}
	if numPartitions <= 0 {
		return nil, errors.New("num partitions must be greater than 0")
	}
	if partIdx < 0 || partIdx >= numPartitions {
		return nil, errors.New("partition index out of range")
	}
	if handler == nil {
		return nil, errors.New("message handler is required")
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
			MaxWaiting:        o.maxWaiting,
			MaxAckPending:     o.maxAckPending,
			InactiveThreshold: o.inactiveThreshold,
			AckPolicy:         o.ackPolicy,
		},
		StreamName:     streamName,
		ConsumerName:   consumerName,
		SubjectPattern: subjectPattern,
		NumPartitions:  numPartitions,
		Partition:      partIdx,
		HashSeed:       o.hashSeed,
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// Convert unified config to partition.ConsumerConfig
	partitionCfg := partition.ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  cfg.NumPartitions,
			SubjectPattern: cfg.SubjectPattern,
			HashSeed:       cfg.HashSeed,
			Logger:         cfg.Logger,
		},
		StreamName:       cfg.StreamName,
		ConsumerName:     cfg.ConsumerName,
		Partition:        cfg.Partition,
		BatchSize:        cfg.BatchSize,
		FetchTimeout:     cfg.FetchTimeout,
		ManualAck:        cfg.ManualAck,
		MaxDeliver:       cfg.MaxDeliver,
		Logger:           cfg.Logger,
		DispatchByKey:    o.dispatchByKey,
		KeyChannelBuffer: o.keyChannelBuffer,
		KeyIdleTimeout:   o.keyIdleTimeout,
		KeyExtractor:     o.keyExtractor,
	}

	adapted := partition.MessageHandlerFunc(handler.Handle)

	inner, err := partition.NewJSConsumer(js, partitionCfg, adapted)
	if err != nil {
		return nil, err
	}

	return &Static{inner: inner}, nil
}

// Start begins consuming messages in a background goroutine.
//
// The consumer creates or binds to a durable JetStream consumer and starts
// a pull loop. Messages are delivered to the handler configured at creation.
//
// Start may only be called once. Calling Start on an already-started consumer
// returns an error.
//
// Parameters:
//   - ctx: Context for lifecycle control. Cancellation stops the consumer.
//
// Returns:
//   - error: Non-nil if the consumer is already started or if JetStream
//     consumer creation fails.
func (s *Static) Start(ctx context.Context) error {
	return s.inner.Start(ctx)
}

// Stop gracefully stops the consumer.
//
// Stop cancels the internal pull loop and waits for pending message processing
// to complete (up to the context deadline). The underlying JetStream consumer
// is NOT deleted; it will be garbage-collected by the server after InactiveThreshold.
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
		return err
	}

	if !jsutil.IsValidConsumerName(c.ConsumerName) {
		return fmt.Errorf("consumer name %q contains invalid characters (allowed: a-z, A-Z, 0-9, -, _)", c.ConsumerName)
	}

	// Cross-field validation: partition must be within range
	if c.Partition >= c.NumPartitions {
		return fmt.Errorf("partition index %d out of range [0, %d)", c.Partition, c.NumPartitions)
	}

	return nil
}
