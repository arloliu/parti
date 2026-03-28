package partition

import (
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Option configures a PartitionConfig.
type Option func(*PartitionConfig)

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
