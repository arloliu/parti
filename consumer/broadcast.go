package consumer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/arloliu/fuda"
	"github.com/arloliu/parti/jsutil"
	"github.com/arloliu/parti/subscription"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
)

// Broadcast is a fan-out consumer where every instance receives every message.
// Uses a unique durable name per instance.
//
// # Lifecycle
//
// Create with [NewBroadcast], then call [Broadcast.Start] to begin consuming.
// Clean up with [Broadcast.Close]:
//
//	consumer, err := consumer.NewBroadcast(js, "stream", "cache-updater", "events.>", handler)
//	if err != nil { log.Fatal(err) }
//	defer consumer.Close(ctx)
//
//	if err := consumer.Start(ctx); err != nil { log.Fatal(err) }
//
// # Stream Requirement
//
// The stream MUST use LimitsPolicy or InterestPolicy. WorkQueuePolicy is
// incompatible because it delivers each message to exactly one consumer,
// defeating the fan-out purpose.
//
// # Thread Safety
//
// Broadcast is safe for concurrent use. [Broadcast.Start] and [Broadcast.Close]
// are serialized internally.
//
// # Deprecation Notice
//
// This type wraps [subscription.BroadcastConsumer]. Future versions may deprecate
// the subscription package in favor of this unified consumer API.
type Broadcast struct {
	inner *subscription.BroadcastConsumer
}

// BroadcastConfig configures a Broadcast consumer.
// Uses unified naming; converted to subscription.BroadcastConsumerConfig internally.
type BroadcastConfig struct {
	CommonConfig

	// StreamName is the JetStream stream to consume from.
	// Required.
	StreamName string `validate:"required"`

	// InstanceID identifies the unique instance of the application.
	//
	// This ID allows each instance to have its own durable consumer, ensuring
	// that every instance receives a copy of every message (fan-out).
	//
	// Accepted formats:
	//   - "fixed-string": Uses the literal value as the identity
	//   - "env:ENV_NAME": Uses the value of the specified environment variable
	//
	// If empty, the consumer will attempt to derive an identity from
	// HOSTNAME, then POD_NAME, and finally fall back to a generated short ID.
	InstanceID string

	// FilterSubject is the subject filter to consume from.
	// Supports wildcards (e.g., "orders.*", "events.>").
	// Messages matching this filter will be broadcast to all instances.
	FilterSubject string `validate:"required"`

	// ConsumerPrefix is the prefix for the durable consumer name.
	//
	// The final durable name is constructed as "<ConsumerPrefix>_broadcast_<InstanceID>".
	// This ensures that each instance gets a unique durable name, achieving fan-out.
	ConsumerPrefix string `validate:"required"`

	// Retry configures the backoff behavior for control-plane operations
	// (e.g., initial connection, creating the consumer).
	Retry RetryConfig

	// IteratorEscalationWindow defines the sliding time window used to aggregate
	// iterator failures for escalation detection.
	//
	// If too many iterator errors occur within this window, the consumer will
	// attempt to escalate recovery (e.g., recreating the consumer).
	//
	// Default: 60s.
	IteratorEscalationWindow time.Duration `default:"60s" validate:"gt=0"`

	// IteratorEscalationThreshold is the number of iterator failures within the
	// escalation window that triggers consumer refresh/escalation.
	//
	// Default: 3.
	IteratorEscalationThreshold int `default:"3" validate:"gt=0"`

	// IteratorFactory optionally overrides the internal iterator creation logic.
	// This is primarily used for testing to inject mock iterators.
	IteratorFactory func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)
}

// NewBroadcast creates a new broadcast fan-out consumer.
func NewBroadcast(
	js jetstream.JetStream,
	streamName, consumerPrefix, filterSubject string,
	handler MessageHandler,
	opts ...BroadcastOption,
) (*Broadcast, error) {
	if js == nil {
		return nil, errors.New("JetStream context is required")
	}
	if streamName == "" {
		return nil, errors.New("stream name is required")
	}
	if consumerPrefix == "" {
		return nil, errors.New("consumer prefix is required")
	}
	if filterSubject == "" {
		return nil, errors.New("filter subject is required")
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
	cfg := BroadcastConfig{
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
		StreamName:                  streamName,
		ConsumerPrefix:              consumerPrefix,
		FilterSubject:               filterSubject,
		InstanceID:                  o.instanceID,
		Retry:                       o.retry,
		IteratorEscalationWindow:    o.iteratorEscalationWindow,
		IteratorEscalationThreshold: o.iteratorEscalationThreshold,
		IteratorFactory:             o.iteratorFactory,
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// Convert unified config to subscription.BroadcastConsumerConfig
	broadcastCfg := subscription.BroadcastConsumerConfig{
		StreamName:                  cfg.StreamName,
		ConsumerPrefix:              cfg.ConsumerPrefix,
		ConsumerID:                  cfg.InstanceID,    // InstanceID -> ConsumerID
		WildcardFilter:              cfg.FilterSubject, // FilterSubject -> WildcardFilter
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
		IteratorEscalationWindow:    cfg.IteratorEscalationWindow,
		IteratorEscalationThreshold: cfg.IteratorEscalationThreshold,
		IteratorFactory:             cfg.IteratorFactory,
		Retry: subscription.RetryConfig{
			Backoff:    cfg.Retry.Backoff,
			Max:        cfg.Retry.Max,
			Multiplier: cfg.Retry.Multiplier,
			Base:       cfg.Retry.Base,
			Seed:       cfg.Retry.Seed,
		},
	}

	adapted := subscription.MessageHandlerFunc(handler.Handle)

	inner, err := subscription.NewBroadcastConsumer(js, broadcastCfg, adapted)
	if err != nil {
		return nil, err
	}

	return &Broadcast{inner: inner}, nil
}

// Start begins consuming messages.
//
// The consumer creates a durable JetStream consumer with a unique name derived
// from the InstanceID and starts a pull loop. All messages matching the
// FilterSubject are delivered to the handler.
//
// Start may only be called once. Calling Start on an already-started consumer
// is a no-op.
//
// Parameters:
//   - ctx: Context for the start operation. Used for JetStream API calls.
//
// Returns:
//   - error: Non-nil if JetStream consumer creation fails.
func (b *Broadcast) Start(ctx context.Context) error {
	// BroadcastConsumer uses UpdateWorkerConsumer to start; we wrap it as Start
	// for a more intuitive API. The workerID and partitions are ignored internally.
	return b.inner.UpdateWorkerConsumer(ctx, "", nil)
}

// UpdateWorkerConsumer implements the WorkerConsumerUpdater interface.
//
// For Broadcast consumers, this is equivalent to [Broadcast.Start]. The workerID
// and partitions arguments are ignored because Broadcast receives all messages
// matching the filter regardless of partition assignment.
//
// This method exists for compatibility with code that uses WorkerConsumerUpdater
// interface.
func (b *Broadcast) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []types.Partition) error {
	return b.inner.UpdateWorkerConsumer(ctx, workerID, partitions)
}

// Close stops the consumer.
//
// Close cancels the internal pull loop and waits for pending message processing
// to complete (up to the context deadline). The underlying JetStream consumer
// is NOT deleted; it will be garbage-collected by the server after InactiveThreshold.
//
// Close is idempotent; calling it multiple times is safe.
//
// Parameters:
//   - ctx: Context with shutdown deadline. If the deadline expires, Close
//     returns [context.DeadlineExceeded] but the consumer will still eventually stop.
//
// Returns:
//   - error: Context error if the wait times out; nil otherwise.
func (b *Broadcast) Close(ctx context.Context) error {
	return b.inner.Close(ctx)
}

// SetDefaults applies default values to the configuration.
func (c *BroadcastConfig) SetDefaults() error {
	return fuda.SetDefaults(c)
}

// Validate checks configuration constraints.
func (c *BroadcastConfig) Validate() error {
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
