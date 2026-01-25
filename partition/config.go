package partition

import (
	"errors"
	"fmt"
	"time"

	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/types"
	"github.com/creasty/defaults"
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
	// Examples:
	//   - "events.completed.{{partition}}"       → "events.completed.0"
	//   - "events.{{key}}.{{partition}}"         → "events.tool-abc.3"
	//   - "orders.{{partition}}.{{key}}.created" → "orders.2.customer-xyz.created"
	//
	// Validation:
	//   - Must contain {{partition}} placeholder
	//   - Must not produce empty NATS subject tokens (e.g., "events..{{partition}}" is invalid)
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

	// StreamName is the JetStream stream to consume from. Required.
	StreamName string `validate:"required"`

	// ConsumerName is the durable consumer name. Required.
	// Typically unique per partition, e.g., "processor-0", "processor-1".
	ConsumerName string `validate:"required"`

	// Partition is the partition index to consume (0 to N-1). Required.
	// Validated at construction: must be < NumPartitions.
	Partition int `validate:"gte=0,ltfield=NumPartitions"`

	// Pull configuration
	BatchSize    int           `default:"100"`
	FetchTimeout time.Duration `default:"5s"`

	// ManualAck disables automatic ack/nak behavior.
	// When false (default): handler returns nil → Ack, handler returns error → Nak.
	// When true: handler must call msg.Ack/Nak/Term explicitly.
	ManualAck bool `default:"false"`

	// MaxDeliver sets the maximum number of delivery attempts for a message.
	// After MaxDeliver attempts, the message is moved to the dead letter subject
	// (if configured on the stream) or discarded.
	// Default: 0 (use JetStream stream default, typically unlimited).
	MaxDeliver int `default:"0" validate:"gte=0"`

	// Logger for structured logging. Inherits from PartitionConfig if nil.
	Logger types.Logger
}

func (cfg *PartitionConfig) setDefaults() error {
	if err := defaults.Set(cfg); err != nil {
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

	// Validate sample publish subject (no wildcards allowed in publish subject).
	sample := parts.buildSubject("key", 0)
	if err := validateSubjectTokens(sample, false); err != nil {
		return err
	}

	// Validate filter subject (wildcards allowed when {{key}} is present).
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
	if err := defaults.Set(cfg); err != nil {
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
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.FetchTimeout <= 0 {
		cfg.FetchTimeout = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = cfg.PartitionConfig.Logger
	}
	if cfg.Logger == nil {
		cfg.Logger = logging.NewNop()
	}

	return nil
}
