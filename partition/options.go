package partition

import (
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Option configures a PartitionConfig.
type Option func(*PartitionConfig)

// ConsumerOption configures a ConsumerConfig.
type ConsumerOption func(*ConsumerConfig)

// NewConfig builds a PartitionConfig using the provided options.
//
// Defaults are applied during validation (see PartitionConfig.Validate).
//
// Parameters:
//   - opts: Option functions to apply
//
// Returns:
//   - PartitionConfig: Config with applied options
func NewConfig(opts ...Option) PartitionConfig {
	cfg := PartitionConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	return cfg
}

// NewConsumerConfig builds a ConsumerConfig using the provided options.
//
// Defaults are applied during validation (see ConsumerConfig.Validate).
//
// Parameters:
//   - opts: ConsumerOption functions to apply
//
// Returns:
//   - ConsumerConfig: Config with applied options
func NewConsumerConfig(opts ...ConsumerOption) ConsumerConfig {
	cfg := ConsumerConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	return cfg
}

// NewPublisherWithOptions creates a Publisher using functional options.
//
// Parameters:
//   - nc: NATS connection
//   - opts: Option functions to configure PartitionConfig
//
// Returns:
//   - *Publisher: Configured publisher
//   - error: Validation or initialization error
func NewPublisherWithOptions(nc *nats.Conn, opts ...Option) (*Publisher, error) {
	cfg := NewConfig(opts...)
	return NewPublisher(nc, cfg)
}

// NewSubscriberWithOptions creates a Subscriber using functional options.
//
// Parameters:
//   - nc: NATS connection
//   - partition: Partition index (0 to N-1)
//   - handler: Message handler
//   - opts: Option functions to configure PartitionConfig
//
// Returns:
//   - *Subscriber: Configured subscriber
//   - error: Validation or initialization error
func NewSubscriberWithOptions(
	nc *nats.Conn,
	partition int,
	handler NATSMessageHandler,
	opts ...Option,
) (*Subscriber, error) {
	cfg := NewConfig(opts...)
	return NewSubscriber(nc, cfg, partition, handler)
}

// NewJSPublisherWithOptions creates a JSPublisher using functional options.
//
// Parameters:
//   - js: JetStream context
//   - opts: Option functions to configure PartitionConfig
//
// Returns:
//   - *JSPublisher: Configured JetStream publisher
//   - error: Validation or initialization error
func NewJSPublisherWithOptions(js jetstream.JetStream, opts ...Option) (*JSPublisher, error) {
	cfg := NewConfig(opts...)
	return NewJSPublisher(js, cfg)
}

// NewJSConsumerWithOptions creates a JSConsumer using functional options.
//
// Parameters:
//   - js: JetStream context
//   - handler: Message handler
//   - opts: ConsumerOption functions to configure ConsumerConfig
//
// Returns:
//   - *JSConsumer: Configured consumer
//   - error: Validation or initialization error
func NewJSConsumerWithOptions(
	js jetstream.JetStream,
	handler MessageHandler,
	opts ...ConsumerOption,
) (*JSConsumer, error) {
	cfg := NewConsumerConfig(opts...)
	return NewJSConsumer(js, cfg, handler)
}

// WithNumPartitions sets the number of partitions.
//
// Parameters:
//   - n: Number of partitions
func WithNumPartitions(n int) Option {
	return func(cfg *PartitionConfig) {
		cfg.NumPartitions = n
	}
}

// WithSubjectPattern sets the subject pattern.
//
// Parameters:
//   - pattern: Subject pattern with placeholders
func WithSubjectPattern(pattern string) Option {
	return func(cfg *PartitionConfig) {
		cfg.SubjectPattern = pattern
	}
}

// WithHashSeed sets the hash seed.
//
// Parameters:
//   - seed: Hash seed value
func WithHashSeed(seed uint64) Option {
	return func(cfg *PartitionConfig) {
		cfg.HashSeed = seed
	}
}

// WithLogger sets the logger.
//
// Parameters:
//   - logger: Logger implementation
func WithLogger(logger types.Logger) Option {
	return func(cfg *PartitionConfig) {
		cfg.Logger = logger
	}
}

// WithStreamName sets the JetStream stream name.
//
// Parameters:
//   - stream: Stream name
func WithStreamName(stream string) ConsumerOption {
	return func(cfg *ConsumerConfig) {
		cfg.StreamName = stream
	}
}

// WithConsumerName sets the JetStream durable consumer name.
//
// Parameters:
//   - name: Consumer durable name
func WithConsumerName(name string) ConsumerOption {
	return func(cfg *ConsumerConfig) {
		cfg.ConsumerName = name
	}
}

// WithPartitionIndex sets the partition index to consume.
//
// Parameters:
//   - partition: Partition index (0 to N-1)
func WithPartitionIndex(partition int) ConsumerOption {
	return func(cfg *ConsumerConfig) {
		cfg.Partition = partition
	}
}

// WithConsumerNumPartitions sets the number of partitions for the consumer.
//
// Parameters:
//   - n: Number of partitions
func WithConsumerNumPartitions(n int) ConsumerOption {
	return func(cfg *ConsumerConfig) {
		cfg.NumPartitions = n
	}
}

// WithConsumerSubjectPattern sets the subject pattern for the consumer.
//
// Parameters:
//   - pattern: Subject pattern with placeholders
func WithConsumerSubjectPattern(pattern string) ConsumerOption {
	return func(cfg *ConsumerConfig) {
		cfg.SubjectPattern = pattern
	}
}

// WithBatchSize sets the pull batch size.
//
// Parameters:
//   - batch: Maximum messages per pull
func WithBatchSize(batch int) ConsumerOption {
	return func(cfg *ConsumerConfig) {
		cfg.BatchSize = batch
	}
}

// WithFetchTimeout sets the pull fetch timeout.
//
// Parameters:
//   - timeout: Pull fetch timeout
func WithFetchTimeout(timeout time.Duration) ConsumerOption {
	return func(cfg *ConsumerConfig) {
		cfg.FetchTimeout = timeout
	}
}

// WithManualAck configures manual acknowledgment behavior.
//
// Parameters:
//   - manual: true to enable manual acking
func WithManualAck(manual bool) ConsumerOption {
	return func(cfg *ConsumerConfig) {
		cfg.ManualAck = manual
	}
}

// WithMaxDeliver sets the maximum delivery attempts.
//
// Parameters:
//   - max: Maximum delivery attempts
func WithMaxDeliver(maxDeliver int) ConsumerOption {
	return func(cfg *ConsumerConfig) {
		cfg.MaxDeliver = maxDeliver
	}
}

// WithConsumerLogger sets the consumer logger.
//
// Parameters:
//   - logger: Logger implementation
func WithConsumerLogger(logger types.Logger) ConsumerOption {
	return func(cfg *ConsumerConfig) {
		cfg.Logger = logger
	}
}
