package partition

import (
	"errors"
	"fmt"
	"time"

	"github.com/arloliu/fuda"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/partutil"
	"github.com/arloliu/parti/v2/partition"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// ConsumerConfig configures the JetStream partition consumer.
type ConsumerConfig struct {
	// PartitionConfig is embedded for partition settings.
	partition.PartitionConfig

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
	// Default: -1 (unlimited).
	MaxDeliver int `default:"-1" validate:"gte=-1"`

	// AckWait is the time allowed for processing a message before it is considered lost
	// and re-delivered by the server.
	//
	// Default: 30s.
	AckWait time.Duration `default:"30s" validate:"gt=0"`

	// MaxAckPending limits the number of messages that can be in-flight (unacknowledged)
	// at any given time.
	MaxAckPending int `validate:"gte=0"`

	// InactiveThreshold is the duration after which an idle consumer (with no active subscriptions)
	// will be automatically deleted by the server.
	//
	// Default: 24h.
	InactiveThreshold time.Duration `default:"24h" validate:"gt=0"`

	// AckPolicy controls the JetStream acknowledgement policy.
	AckPolicy jetstream.AckPolicy

	// DispatchByKey enables per-key concurrent message processing.
	//
	// IMPORTANT: SubjectPattern MUST contain {{key}} placeholder when DispatchByKey is enabled.
	//
	// Default: nil (disabled, all messages processed sequentially in one goroutine)
	DispatchByKey *bool

	// KeyChannelBuffer is the buffer size for each key's message channel.
	//
	// Only used when DispatchByKey is enabled.
	// Default: 32
	KeyChannelBuffer int `default:"32" validate:"gte=1"`

	// KeyIdleTimeout determines how long an idle key goroutine waits before exiting.
	//
	// Only used when DispatchByKey is enabled.
	// Default: 30s
	KeyIdleTimeout time.Duration `default:"30s"`

	// KeyExtractor extracts the routing key from a message.
	//
	// If nil, uses a pattern-aware extractor based on the {{key}} position in SubjectPattern.
	// Only used when DispatchByKey is enabled.
	KeyExtractor KeyExtractorFunc

	// Logger for structured logging. Inherits from PartitionConfig if nil.
	Logger types.Logger
}

// Validate validates the consumer configuration.
//
// Returns:
//   - error: Validation or initialization error
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
		return partutil.ErrPartitionOutOfRange
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
	if cfg.AckPolicy == jetstream.AckNonePolicy {
		cfg.AckPolicy = jetstream.AckExplicitPolicy
	}

	// Validate DispatchByKey requires {{key}} in SubjectPattern
	if cfg.DispatchByKey != nil && *cfg.DispatchByKey {
		parts, err := partutil.ParsePattern(cfg.SubjectPattern)
		if err != nil {
			return err
		}
		if !parts.HasKey {
			return partutil.ErrDispatchByKeyRequiresKeyPlaceholder
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
