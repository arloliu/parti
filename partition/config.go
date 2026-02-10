package partition

import (
	"errors"
	"fmt"
	"time"

	"github.com/arloliu/fuda"
	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/internal/metrics"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
)

// PartitionConfig configures static partition-based publishing and consuming.
type PartitionConfig struct {
	// NumPartitions is the total number of partitions (N).
	// Must be > 0. Partition indices range from 0 to NumPartitions-1.
	// WARNING: Changing NumPartitions changes the hash mapping.
	NumPartitions int `validate:"required,gt=0"`

	// SubjectPattern is the subject template with placeholders.
	//
	// Placeholders:
	//   - {{partition}} - Replaced with partition index (0 to N-1). Required.
	//   - {{key}}       - Replaced with the partition key. Optional.
	//
	// Placeholders must occupy a full token between dots. Embedded placeholders
	// like "orders.{{partition}}-v1" are invalid.
	//
	// Examples:
	//   - "events.completed.{{partition}}"       → "events.completed.0"
	//   - "events.{{key}}.{{partition}}"         → "events.tool-abc.3"
	//   - "orders.{{partition}}.{{key}}.created" → "orders.2.customer-xyz.created"
	//   - "events.*.{{key}}.{{partition}}"       → "events.*.tool-abc.3" (consumer filter)
	//   - "events.{{key}}.{{partition}}.>"       → "events.tool-abc.3.>" (consumer filter)
	//
	// Validation:
	//   - Must contain {{partition}} placeholder
	//   - Must not produce empty NATS subject tokens (e.g., "events..{{partition}}" is invalid)
	//   - Wildcards are allowed for consumers/subscribers:
	//     - "*" can appear in any token
	//     - ">" can only appear as the last token
	//   - Publishers require concrete subjects; wildcard patterns are rejected for publishing
	SubjectPattern string `validate:"required,contains={{partition}}"`

	// HashSeed is an optional seed for the consistent hash function.
	// Use 0 for default behavior, non-zero for deterministic hashing.
	HashSeed uint64

	// Logger provides structured logging. Defaults to no-op logger when nil.
	Logger types.Logger
}

// ConsumerConfig configures the JetStream partition consumer.
type ConsumerConfig struct {
	// PartitionConfig is embedded for partition settings.
	PartitionConfig

	// Metrics is the metrics collector for consumer operations.
	// If nil, a no-op collector is used.
	Metrics types.MetricsCollector

	// StreamName is the JetStream stream to consume from. Required.
	StreamName string `validate:"required"`

	// ConsumerName is the durable consumer name. Required.
	// Typically unique per partition, e.g., "processor-0", "processor-1".
	ConsumerName string `validate:"required"`

	// Partition is the partition index to consume (0 to N-1). Required.
	// Validated at construction: must be < NumPartitions.
	Partition int `validate:"gte=0,ltfield=NumPartitions"`

	// BatchSize is the number of messages to fetch per pull request.
	// Default: 1
	BatchSize int `default:"1" validate:"gt=0"`

	// FetchTimeout is the maximum time to wait for messages in each pull.
	FetchTimeout time.Duration `default:"5s"`

	// MaxWaiting is the maximum number of outstanding pull requests allowed.
	// Default: 2.
	MaxWaiting int `default:"2" validate:"gt=0"`

	// ManualAck disables automatic ack/nak behavior.
	// When false (default): handler returns nil → Ack, handler returns error → Nak.
	// When true: handler must call msg.Ack/Nak/Term explicitly.
	ManualAck bool `default:"false"`

	// MaxDeliver sets the maximum number of delivery attempts for a message.
	// After MaxDeliver attempts, the message is moved to the dead letter subject
	// (if configured on the stream) or discarded.
	// Default: -1 (unlimited).
	MaxDeliver int `default:"-1" validate:"gte=-1"`

	// AckWait is the time allowed for processing a message before it is considered lost
	// and re-delivered by the server.
	//
	// Default: 30s.
	AckWait time.Duration `default:"30s" validate:"gt=0"`

	// MaxAckPending limits the number of messages that can be in-flight (unacknowledged)
	// at any given time.
	//
	// If the limit is reached, the server will pause delivery until some messages are acknowledged.
	// If zero, the server's consumer default is used.
	MaxAckPending int `validate:"gte=0"`

	// InactiveThreshold is the duration after which an idle consumer (with no active subscriptions)
	// will be automatically deleted by the server.
	//
	// Default: 24h.
	InactiveThreshold time.Duration `default:"24h" validate:"gt=0"`

	// AckPolicy controls the JetStream acknowledgement policy.
	//
	// Typically set to AckExplicitPolicy for reliable processing.
	// Defaults to AckExplicitPolicy if usually not set manually.
	AckPolicy jetstream.AckPolicy

	// DispatchByKey enables per-key concurrent message processing.
	//
	// When enabled, messages are routed to separate goroutines based on their key.
	// Messages with the same key are processed sequentially (preserving order),
	// while different keys are processed concurrently in parallel goroutines.
	//
	// IMPORTANT: SubjectPattern MUST contain {{key}} placeholder when DispatchByKey
	// is enabled. The key is extracted based on the {{key}} position in the pattern.
	// For example, with pattern "events.{{partition}}.{{key}}" and subject
	// "events.0.customer-abc", the key is "customer-abc".
	//
	// WARNING: This creates an UNBOUNDED number of goroutines - one goroutine per
	// unique key. If your workload has millions of unique keys, memory usage will
	// grow proportionally. Goroutines are cleaned up after KeyIdleTimeout of
	// inactivity.
	//
	// Use this when:
	//   - You need per-key ordering but want parallelism across keys
	//   - Your key cardinality is bounded (e.g., thousands, not millions)
	//   - Slow processing of one key should not block other keys
	//
	// Default: nil (disabled, all messages processed sequentially in one goroutine)
	DispatchByKey *bool

	// KeyChannelBuffer is the buffer size for each key's message channel.
	//
	// When the buffer is full, the main pull loop blocks (backpressure).
	// Larger buffers absorb bursts but use more memory per active key.
	//
	// Only used when DispatchByKey is enabled.
	// Default: 32
	KeyChannelBuffer int `default:"32" validate:"gte=1"`

	// KeyIdleTimeout determines how long an idle key goroutine waits before exiting.
	//
	// After this duration with no messages, the goroutine exits and is removed.
	// A new goroutine is created if messages for that key arrive later.
	//
	// Only used when DispatchByKey is enabled.
	// Default: 30s
	KeyIdleTimeout time.Duration `default:"30s"`

	// KeyExtractor extracts the routing key from a message.
	//
	// The extracted key determines which goroutine processes the message.
	// Messages with the same key are guaranteed to be processed sequentially.
	//
	// If nil, uses a pattern-aware extractor based on the {{key}} position in
	// SubjectPattern. For example, with pattern "events.{{partition}}.{{key}}"
	// and subject "events.0.customer-abc", the key is "customer-abc".
	//
	// Only used when DispatchByKey is enabled.
	KeyExtractor KeyExtractorFunc

	// Logger for structured logging. Inherits from PartitionConfig if nil.
	Logger types.Logger
}

func (cfg *PartitionConfig) setDefaults() error {
	if err := fuda.SetDefaults(cfg); err != nil {
		return fmt.Errorf("failed to set defaults: %w", err)
	}
	if cfg.Logger == nil {
		cfg.Logger = logging.NewNop()
	}

	return nil
}

// Validate validates the partition configuration.
func (cfg *PartitionConfig) Validate() error {
	if cfg == nil {
		return errors.New("partition config is required")
	}
	if err := cfg.setDefaults(); err != nil {
		return err
	}
	if cfg.NumPartitions <= 0 {
		return errors.New("num partitions must be > 0")
	}
	if cfg.SubjectPattern == "" {
		return errors.New("subject pattern is required")
	}
	parts, err := parsePattern(cfg.SubjectPattern)
	if err != nil {
		return err
	}

	// Validate filter subject (wildcards allowed in patterns).
	filter := parts.buildFilterSubject(0)

	return validateSubjectTokens(filter, true)
}

// Validate validates the consumer configuration.
func (cfg *ConsumerConfig) Validate() error {
	if cfg == nil {
		return errors.New("consumer config is required")
	}
	if err := cfg.PartitionConfig.Validate(); err != nil {
		return err
	}
	if err := fuda.SetDefaults(cfg); err != nil {
		return fmt.Errorf("failed to set defaults: %w", err)
	}
	if cfg.StreamName == "" {
		return errors.New("stream name is required")
	}
	if cfg.ConsumerName == "" {
		return errors.New("consumer name is required")
	}
	if cfg.Partition < 0 || cfg.Partition >= cfg.NumPartitions {
		return ErrPartitionOutOfRange
	}
	if cfg.FetchTimeout <= 0 {
		cfg.FetchTimeout = 5 * time.Second
	}
	if cfg.MaxWaiting <= 0 {
		cfg.MaxWaiting = 2
	}
	if cfg.AckWait <= 0 {
		cfg.AckWait = 30 * time.Second
	}
	if cfg.InactiveThreshold <= 0 {
		cfg.InactiveThreshold = 24 * time.Hour
	}
	if cfg.AckPolicy == jetstream.AckNonePolicy { // Assuming explicit is default if 0
		cfg.AckPolicy = jetstream.AckExplicitPolicy
	}

	// Validate DispatchByKey requires {{key}} in SubjectPattern
	if cfg.DispatchByKey != nil && *cfg.DispatchByKey {
		parts, err := parsePattern(cfg.SubjectPattern)
		if err != nil {
			return err
		}
		if !parts.hasKey {
			return ErrDispatchByKeyRequiresKeyPlaceholder
		}
	}

	if cfg.Logger == nil {
		cfg.Logger = cfg.PartitionConfig.Logger
	}
	if cfg.Logger == nil {
		cfg.Logger = logging.NewNop()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.NewNop()
	}

	return nil
}
