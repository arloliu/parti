package durable

import (
	"fmt"
	"time"

	"github.com/arloliu/fuda"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/types"
	"github.com/go-playground/validator/v10"
	"github.com/nats-io/nats.go/jetstream"
)

// BroadcastConsumerConfig configures the broadcast fan-out consumer.
//
// The broadcast consumer creates a single JetStream durable consumer per instance
// using a wildcard FilterSubject. All workers receive all messages and process
// them locally without partition filtering.
//
// # Stream Requirement
//
// The stream MUST use LimitsPolicy or InterestPolicy. WorkQueuePolicy is
// incompatible because it delivers each message to exactly one consumer,
// defeating the fan-out purpose. With WorkQueuePolicy, only one worker would
// receive each message instead of all workers receiving and filtering locally.
//
// # Required Fields
//
//   - StreamName: JetStream stream name containing the subjects
//   - ConsumerPrefix: Prefix for durable name; final name is "<ConsumerPrefix>_broadcast_<consumerID>"
//   - WildcardFilter: JetStream FilterSubject for the consumer, e.g., "orders.>" for multi-token partition IDs
//
// Optional tuning fields are documented inline below. Zero values are replaced by
// sensible defaults via SetDefaults().
type BroadcastConsumerConfig struct {
	// StreamName is the JetStream stream name where subjects live. Required.
	StreamName string `validate:"required"`

	// ConsumerPrefix is the prefix for the durable consumer name.
	// It must contain only alphanumeric characters (a-z, A-Z, 0-9), dashes (-), or underscores (_).
	// Final name format: <ConsumerPrefix>_broadcast_<consumerID>
	// Required.
	ConsumerPrefix string `validate:"required"`

	// ConsumerID controls the identity used in the broadcast durable name.
	//
	// Accepted formats:
	//   - "fixed-string": Uses the literal value as the identity
	//   - "env:ENV_NAME": Uses the value of the specified environment variable
	//
	// If empty, the consumer will attempt to derive an identity from
	// HOSTNAME, then POD_NAME, and finally fall back to a generated short ID.
	ConsumerID string

	// WildcardFilter is the JetStream FilterSubject for the consumer.
	// Use ">" for multi-token partition IDs (recommended).
	// Use "*" only for single-token partition IDs.
	//
	// Examples:
	//   - "orders.>" for partition IDs like "region.us-east"
	//   - "orders.*.events" for single-token partition IDs like "p1"
	//
	// Required.
	WildcardFilter string `validate:"required"`

	// Logger provides structured logging. Defaults to a no-op logger when nil.
	Logger types.Logger

	// Metrics is the metrics collector for worker consumer operations.
	// If nil, no metrics are emitted from the consumer.
	Metrics types.WorkerConsumerMetrics

	// ManualAck, when true, disables the helper's automatic Ack/Nak behavior.
	// Handlers are responsible for calling msg.Ack/Nak/Term and optionally
	// msg.InProgress() to extend AckWait. This enables handler-controlled
	// backpressure. Defaults to false.
	ManualAck bool

	// AckWait is the time allowed for processing before redelivery.
	// Default: 30s.
	AckWait time.Duration `default:"30s" validate:"gt=0"`

	// MaxDeliver is the maximum redelivery attempts before moving to DLQ (if configured).
	// Default: -1 (unlimited attempts).
	MaxDeliver int `default:"-1" validate:"gte=-1"`

	// BatchSize is the max number of messages to pull per iterator request.
	// Default: 1.
	BatchSize int `default:"1" validate:"gt=0"`

	// FetchTimeout is the max time to wait when pulling a batch.
	// Default: 5s.
	FetchTimeout time.Duration `default:"5s" validate:"gt=0"`

	// MaxWaiting caps outstanding pull requests for the durable consumer.
	// Default: 2.
	MaxWaiting int `default:"2" validate:"gt=0"`

	// MaxAckPending limits the number of in-flight unacknowledged messages the server
	// will allow. If zero, the server default is used.
	MaxAckPending int `validate:"gte=0"`

	// InactiveThreshold is how long an idle consumer is kept by the server before cleanup.
	// Default: 24h.
	InactiveThreshold time.Duration `default:"24h" validate:"gt=0"`

	// AckPolicy controls JetStream ack policy. Defaults to AckExplicitPolicy.
	AckPolicy jetstream.AckPolicy

	// Retry configures backoff behavior for control-plane operations.
	Retry RetryConfig

	// IteratorFactory optionally overrides the iterator creation logic for testing or
	// advanced customization. When nil, a default factory is used.
	IteratorFactory func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)
}

// DefaultBroadcastConsumerConfig returns a BroadcastConsumerConfig with sensible defaults.
// Note: Required fields (StreamName, ConsumerPrefix, WildcardFilter)
// must still be set by the user.
func DefaultBroadcastConsumerConfig() BroadcastConsumerConfig {
	return BroadcastConsumerConfig{
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
	}
}

// SetDefaults sets default values for the configuration.
func (c *BroadcastConsumerConfig) SetDefaults() error {
	if err := fuda.SetDefaults(c); err != nil {
		return fmt.Errorf("failed to set defaults: %w", err)
	}

	if c.Logger == nil {
		c.Logger = logging.NewNop()
	}
	if c.Metrics == nil {
		c.Metrics = metrics.NewNop()
	}

	return nil
}

// Validate checks configuration constraints.
func (c *BroadcastConsumerConfig) Validate() error {
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
	if err := validateSubjectTokens(c.WildcardFilter, true); err != nil {
		return fmt.Errorf("wildcard filter is invalid: %w", err)
	}

	return nil
}
