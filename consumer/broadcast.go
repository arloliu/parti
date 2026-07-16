package consumer

import (
	"context"
	"fmt"
	"time"

	"github.com/arloliu/fuda"
	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/jsutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// Broadcast is a fan-out consumer where every instance receives every message.
// Uses a unique durable name per instance.
//
// # Lifecycle
//
// Create with [NewBroadcast], then call [Broadcast.Start] to begin consuming.
// Clean up with [Broadcast.Stop]:
//
//	consumer, err := consumer.NewBroadcast(js, "stream", "cache-updater", "events.>", handler)
//	if err != nil { log.Fatal(err) }
//	defer consumer.Stop(ctx)
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
// Broadcast is safe for concurrent use. [Broadcast.Start] and [Broadcast.Stop]
// are serialized internally.
type Broadcast struct {
	inner *durable.BroadcastConsumer
}

// BroadcastConfig configures a Broadcast consumer.
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

	// Retry configures the backoff behavior for the consume loop.
	//
	// On each iterator failure the loop sleeps for a jittered delay that grows via
	// decorrelated jitter from Base toward Max using the Multiplier factor. Seed
	// makes the sequence deterministic (useful in tests); zero uses the package-level
	// PRNG. The delay resets to zero after every successful iteration cycle.
	Retry RetryConfig

	// IteratorFactory optionally overrides the internal iterator creation logic.
	// This is primarily used for testing to inject mock iterators.
	IteratorFactory func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)

	// RecoveryStrategy defines how a recreated consumer resumes after an unexpected deletion.
	//
	// All strategies are supported. [RecoverFromLastProcessed] works with both
	// ManualAck=false (checkpoint advances automatically) and ManualAck=true
	// (checkpoint advances when the handler calls msg.Ack() or msg.DoubleAck()).
	RecoveryStrategy RecoveryStrategy
}

// NewBroadcast creates a new broadcast fan-out consumer.
func NewBroadcast(
	js jetstream.JetStream,
	streamName, consumerPrefix, filterSubject string,
	handler MessageHandler,
	opts ...BroadcastOption,
) (*Broadcast, error) {
	if js == nil {
		return nil, fmt.Errorf("%w: JetStream context is required", ErrInvalidConfig)
	}
	if streamName == "" {
		return nil, fmt.Errorf("%w: stream name is required", ErrInvalidConfig)
	}
	if consumerPrefix == "" {
		return nil, fmt.Errorf("%w: consumer prefix is required", ErrInvalidConfig)
	}
	if filterSubject == "" {
		return nil, fmt.Errorf("%w: filter subject is required", ErrInvalidConfig)
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
	cfg := BroadcastConfig{
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
		ConsumerPrefix:   consumerPrefix,
		FilterSubject:    filterSubject,
		InstanceID:       o.instanceID,
		Retry:            o.retry,
		IteratorFactory:  o.iteratorFactory,
		RecoveryStrategy: o.recoveryStrategy,
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// Build broadcast consumer config from unified BroadcastConfig.
	broadcastCfg := durable.BroadcastConsumerConfig{
		StreamName:        cfg.StreamName,
		ConsumerPrefix:    cfg.ConsumerPrefix,
		ConsumerID:        cfg.InstanceID,    // InstanceID -> ConsumerID
		WildcardFilter:    cfg.FilterSubject, // FilterSubject -> WildcardFilter
		Logger:            cfg.Logger,
		Metrics:           cfg.Metrics,
		ManualAck:         cfg.ManualAck,
		AckWait:           cfg.AckWait,
		MaxDeliver:        cfg.MaxDeliver,
		BatchSize:         cfg.BatchSize,
		FetchTimeout:      cfg.FetchTimeout,
		PullHeartbeatCap:  cfg.PullHeartbeatCap,
		MaxWaiting:        cfg.MaxWaiting,
		MaxAckPending:     cfg.MaxAckPending,
		InactiveThreshold: cfg.InactiveThreshold,
		AckPolicy:         cfg.AckPolicy,
		RecoveryStrategy:  cfg.RecoveryStrategy,
		IteratorFactory:   cfg.IteratorFactory,

		ConsumerMemoryStorage: cfg.ConsumerMemoryStorage,
		ConsumerReplicas:      cfg.ConsumerReplicas,
		Retry: durable.RetryConfig{
			Backoff:    cfg.Retry.Backoff,
			Max:        cfg.Retry.Max,
			Multiplier: cfg.Retry.Multiplier,
			Base:       cfg.Retry.Base,
			Seed:       cfg.Retry.Seed,
		},
	}

	inner, err := durable.NewBroadcastConsumer(js, broadcastCfg, handler.Handle)
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
// Stop is terminal: after [Broadcast.Stop] is called, Start returns
// [ErrConsumerStopped]. Create a new instance to consume again.
//
// Parameters:
//   - ctx: Context for the start operation. Used for JetStream API calls.
//
// Returns:
//   - error: Non-nil if JetStream consumer creation fails, or [ErrConsumerStopped]
//     if the consumer has been stopped.
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

// Stop gracefully stops the consumer.
//
// Stop cancels the internal pull loop and waits for pending message processing
// to complete (up to the context deadline). The underlying JetStream consumer
// is NOT deleted; it will be garbage-collected by the server after InactiveThreshold.
//
// Stop is terminal: after Stop returns, any subsequent call to [Broadcast.Start] or
// [Broadcast.UpdateWorkerConsumer] returns [ErrConsumerStopped]. Create a new
// instance to consume again.
//
// If the consumer is stopped while still registered with a running manager, the
// manager will retry the failed update indefinitely (by design). Stop the manager
// first, or deregister this consumer before calling Stop.
//
// Stop is idempotent; calling it multiple times is safe.
//
// Parameters:
//   - ctx: Context with shutdown deadline. If the deadline expires, Stop
//     returns [context.DeadlineExceeded] but the consumer will still eventually stop.
//
// Returns:
//   - error: Context error if the wait times out; nil otherwise.
func (b *Broadcast) Stop(ctx context.Context) error {
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
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	if err := validateFetchTimeoutFloor(c.FetchTimeout); err != nil {
		return err
	}

	if err := validatePullHeartbeatCap(c.PullHeartbeatCap); err != nil {
		return err
	}

	if !jsutil.IsValidConsumerName(c.ConsumerPrefix) {
		return fmt.Errorf("%w: consumer prefix %q contains invalid characters (allowed: a-z, A-Z, 0-9, -, _)", ErrInvalidConfig, c.ConsumerPrefix)
	}

	return nil
}
