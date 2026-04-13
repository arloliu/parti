package consumer

import (
	"context"
	"fmt"
	"time"

	"github.com/arloliu/fuda"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// CommonConfig contains configuration fields shared by all consumer types.
// Embedding this in each consumer's config ensures consistent naming and defaults.
//
// These settings control the low-level behavior of the JetStream consumer,
// including acknowledgement policies, batching, timeouts, and redelivery.
type CommonConfig struct {
	// Logger provides structured logging for the consumer.
	// If nil, a no-op logger is used.
	Logger types.Logger

	// Metrics is the metrics collector for worker consumer operations.
	// If nil, a no-op collector is used.
	Metrics types.WorkerConsumerMetrics

	// ManualAck disables automatic acknowledgement of messages.
	//
	// When false (default):
	//  - If the handler returns nil, the message is automatically acknowledged.
	//  - If the handler returns an error, the message is negatively acknowledged (Nak).
	//
	// When true:
	//  - The handler MUST explicitly call msg.Ack(), msg.Nak(), or msg.Term().
	//  - Returning an error from the handler is still logged but does not trigger Action.
	ManualAck bool

	// AckWait is the time allowed for processing a message before it is considered lost
	// and re-delivered by the server.
	//
	// This should be longer than the expected maximum processing time of a single message.
	//
	// Default: 30s.
	AckWait time.Duration `default:"30s" validate:"gt=0"`

	// MaxDeliver is the maximum number of times a message will be delivered.
	//
	// If a message fails processing (Nak) or times out (AckWait) this many times,
	// it will be terminated or moved to a Dead Letter Queue (if configured on the stream).
	//
	// Default: -1 (unlimited).
	MaxDeliver int `default:"-1" validate:"gte=-1"`

	// BatchSize is the maximum number of messages to pull from the server in a single request.
	//
	// A higher batch size can improve throughput but typically increases memory usage
	// and potentially latency for individual messages if processing is slow.
	//
	// Default: 1.
	BatchSize int `default:"1" validate:"gt=0"`

	// FetchTimeout is the maximum duration to wait for a batch of messages to arrive
	// when pulling from the server.
	//
	// If no messages are available within this timeout, the pull request expires.
	// The consumer loop manages this automatically.
	//
	// Default: 5s.
	FetchTimeout time.Duration `default:"5s" validate:"gt=0"`

	// MaxWaiting is the maximum number of outstanding pull requests allowed.
	//
	// This controls the pre-fetch buffer. A value of 2 with BatchSize of 1 means
	// there can be 2 messages buffered locally (1 being processed, 1 ready).
	//
	// Default: 2.
	MaxWaiting int `default:"2" validate:"gt=0"`

	// MaxAckPending limits the number of messages that can be in-flight (unacknowledged)
	// at any given time.
	//
	// If the limit is reached, the server will pause delivery until some messages are acknowledged.
	// If zero, the server's consumer default is used.
	MaxAckPending int `validate:"gte=0"`

	// InactiveThreshold is the duration after which an idle consumer (with no active subscriptions)
	// will be automatically deleted by the server.
	//
	// For durable consumers, this should be set high enough to survive application restarts.
	//
	// Default: 24h.
	InactiveThreshold time.Duration `default:"24h" validate:"gt=0"`

	// AckPolicy controls the JetStream acknowledgement policy.
	//
	// Typically set to AckExplicitPolicy for reliable processing.
	// Defaults to AckExplicitPolicy if usually not set manually.
	AckPolicy jetstream.AckPolicy
}

// SetDefaults applies default values to the configuration.
func (c *CommonConfig) SetDefaults() error {
	if err := fuda.SetDefaults(c); err != nil {
		return err
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
func (c *CommonConfig) Validate() error {
	if err := c.SetDefaults(); err != nil {
		return err
	}

	return fuda.Validate(c)
}

// checkWorkQueueRecoveryCompat returns ErrInvalidConfig when the stream uses
// WorkQueuePolicy and the recovery strategy requires a non-DeliverAllPolicy.
// NATS only permits DeliverAllPolicy on work-queue streams; RecoverFromNew
// (DeliverNewPolicy) and RecoverFromLastProcessed (DeliverByStartSequencePolicy)
// would silently fail during every recovery attempt.
//
// The check is best-effort: failures to fetch stream info are silently ignored
// so callers are not blocked by transient connectivity issues.
func checkWorkQueueRecoveryCompat(ctx context.Context, js jetstream.JetStream, streamName string, strategy RecoveryStrategy) error {
	var strategyName string
	switch strategy {
	case RecoverFromNew:
		strategyName = "RecoverFromNew"
	case RecoverFromLastProcessed:
		strategyName = "RecoverFromLastProcessed"
	default:
		return nil // RecoveryDisabled and RecoverFromBeginning are always valid
	}

	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		return nil
	}

	info, err := stream.Info(ctx)
	if err != nil {
		return nil
	}

	if info.Config.Retention != jetstream.WorkQueuePolicy {
		return nil
	}

	return fmt.Errorf(
		"%w: %s is incompatible with WorkQueuePolicy stream %q"+
			" — NATS only permits DeliverAllPolicy on work-queue streams;"+
			" use RecoverFromBeginning or RecoveryDisabled instead",
		ErrInvalidConfig, strategyName, streamName,
	)
}

// RetryConfig groups retry backoff settings.
type RetryConfig struct {
	// Backoff is the delay between retries for control-plane operations.
	// Default: 100ms.
	Backoff time.Duration `default:"100ms" validate:"gte=0"`

	// Max caps the jittered backoff.
	// Default: 5s.
	Max time.Duration `default:"5s" validate:"gte=0,gtefield=Backoff"`

	// Multiplier grows the backoff window for decorrelated jitter.
	// Default: 1.6.
	Multiplier float64 `default:"1.6" validate:"gte=1"`

	// Base is the base backoff used for decorrelated jitter retries.
	// Default: 200ms. If zero and Backoff is set, Base falls back to Backoff.
	Base time.Duration `default:"200ms" validate:"gte=0"`

	// Seed optionally seeds the jitter RNG for deterministic tests.
	// When zero, a random seed is used.
	Seed int64
}
