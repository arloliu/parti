package consumer

import (
	"context"
	"errors"
	"fmt"
	rand "math/rand/v2"
	"sync"
	"time"

	"github.com/arloliu/fuda"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/jsutil"
	"github.com/arloliu/parti/v2/types"
)

// Queue is a load-balanced consumer where multiple instances share one durable.
// Each message is delivered to exactly one instance (queue group semantics).
//
// Unlike [Broadcast] (fan-out to all), Queue distributes messages across replicas.
// This is useful for classic worker queue patterns where each message should be
// processed by exactly one worker.
//
// # Lifecycle
//
// Create with [NewQueue], start consumption with [Queue.Start], and clean up
// with [Queue.Stop]:
//
//	q, err := consumer.NewQueue(js, "stream", "consumer", "subject.>", handler)
//	if err != nil { log.Fatal(err) }
//	defer q.Stop(ctx)
//
//	if err := q.Start(ctx); err != nil { log.Fatal(err) }
//
// # Thread Safety
//
// Queue is safe for concurrent use. [Queue.Start] and [Queue.Stop] are
// serialized internally.
type Queue struct {
	js       jetstream.JetStream
	config   QueueConfig
	logger   types.Logger
	metrics  types.WorkerConsumerMetrics
	handler  MessageHandler
	retryRNG *rand.Rand

	mu          sync.RWMutex
	consumer    jetstream.Consumer
	loopCancel  context.CancelFunc
	loopDone    chan struct{}
	loopStarted bool

	iterFactory func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)
}

// QueueConfig configures a Queue consumer.
// Embeds CommonConfig for shared fields.
type QueueConfig struct {
	CommonConfig

	// StreamName is the name of the JetStream stream to consume from.
	// This field is required and must match an existing stream.
	StreamName string `validate:"required"`

	// FilterSubject is the subject filter to consume from.
	// Supports wildcards (e.g., "orders.*", "events.>").
	// Only messages matching this filter will be delivered to the consumer.
	FilterSubject string `validate:"required"`

	// ConsumerName is the durable consumer name.
	// This name must be unique within the stream.
	// For Queue consumers, this name identifies the shared consumer group;
	// multiple instances using the same ConsumerName will share the load.
	ConsumerName string `validate:"required"`

	// Retry configures the backoff behavior for control-plane operations
	// (e.g., initial connection, creating the consumer).
	Retry RetryConfig

	// IteratorFactory optionally overrides the internal iterator creation logic.
	// This is primarily used for testing to inject mock iterators.
	IteratorFactory func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)
}

// DefaultQueueConfig returns a QueueConfig with sensible defaults.
// Note: Required fields (StreamName, ConsumerName, FilterSubject)
// must still be set by the user.
func DefaultQueueConfig() QueueConfig {
	return QueueConfig{
		CommonConfig: CommonConfig{
			AckWait:           30 * time.Second,
			MaxDeliver:        -1,
			BatchSize:         1,
			FetchTimeout:      5 * time.Second,
			MaxWaiting:        2,
			InactiveThreshold: 24 * time.Hour,
			AckPolicy:         jetstream.AckExplicitPolicy,
		},
		Retry: RetryConfig{
			Backoff:    100 * time.Millisecond,
			Max:        5 * time.Second,
			Multiplier: 1.6,
			Base:       200 * time.Millisecond,
		},
	}
}

// SetDefaults sets default values for the configuration.
func (c *QueueConfig) SetDefaults() error {
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
func (c *QueueConfig) Validate() error {
	if err := c.SetDefaults(); err != nil {
		return err
	}
	if err := fuda.Validate(c); err != nil {
		return err
	}

	if !jsutil.IsValidConsumerName(c.ConsumerName) {
		return fmt.Errorf("consumer name %q contains invalid characters (allowed: a-z, A-Z, 0-9, -, _)", c.ConsumerName)
	}
	if err := validateSubjectTokens(c.FilterSubject, true); err != nil {
		return fmt.Errorf("filter subject is invalid: %w", err)
	}

	return nil
}

// NewQueue creates a new queue (load-balanced) consumer.
func NewQueue(
	js jetstream.JetStream,
	streamName, consumerName, filterSubject string,
	handler MessageHandler,
	opts ...QueueOption,
) (*Queue, error) {
	if js == nil {
		return nil, errors.New("JetStream context is required")
	}
	if streamName == "" {
		return nil, errors.New("stream name is required")
	}
	if consumerName == "" {
		return nil, errors.New("consumer name is required")
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
	cfg := QueueConfig{
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
		StreamName:      streamName,
		ConsumerName:    consumerName,
		FilterSubject:   filterSubject,
		Retry:           o.retry,
		IteratorFactory: o.iteratorFactory,
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	iterFactory := cfg.IteratorFactory
	if iterFactory == nil {
		iterFactory = defaultIterFactory
	}

	return &Queue{
		js:          js,
		config:      cfg,
		logger:      cfg.Logger,
		metrics:     cfg.Metrics,
		handler:     handler,
		loopDone:    make(chan struct{}),
		retryRNG:    newRetryRNG(cfg.Retry.Seed),
		iterFactory: iterFactory,
	}, nil
}

// defaultIterFactory creates a messages iterator with heartbeat and expiry.
func defaultIterFactory(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
	heartbeat := expiry / 2
	if heartbeat < 100*time.Millisecond {
		heartbeat = 100 * time.Millisecond
	}
	return cons.Messages(
		jetstream.PullMaxMessages(batch),
		jetstream.PullExpiry(expiry),
		jetstream.PullHeartbeat(heartbeat),
	)
}

// Start begins consuming messages.
func (q *Queue) Start(ctx context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.loopStarted {
		return errors.New("queue consumer already started")
	}

	// Create or get existing consumer
	cons, err := q.ensureConsumer(ctx)
	if err != nil {
		return fmt.Errorf("failed to create consumer: %w", err)
	}
	q.consumer = cons

	// Start the pull loop
	loopCtx, cancel := context.WithCancel(context.Background())
	q.loopCancel = cancel
	q.loopStarted = true
	q.loopDone = make(chan struct{})

	go q.runLoop(loopCtx)

	q.logger.Info("queue consumer started",
		"stream", q.config.StreamName,
		"consumer", q.config.ConsumerName,
		"filter", q.config.FilterSubject,
	)

	return nil
}

// Stop gracefully stops the consumer.
func (q *Queue) Stop(ctx context.Context) error {
	q.mu.Lock()
	if !q.loopStarted {
		q.mu.Unlock()
		return nil
	}

	cancel := q.loopCancel
	done := q.loopDone
	q.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	select {
	case <-done:
		// Loop exited cleanly
	case <-ctx.Done():
		return ctx.Err()
	}

	q.mu.Lock()
	q.loopStarted = false
	q.mu.Unlock()

	q.logger.Info("queue consumer stopped",
		"stream", q.config.StreamName,
		"consumer", q.config.ConsumerName,
	)

	return nil
}

// ensureConsumer creates or retrieves the shared durable consumer.
func (q *Queue) ensureConsumer(ctx context.Context) (jetstream.Consumer, error) {
	cfg := jetstream.ConsumerConfig{
		Durable:           q.config.ConsumerName,
		FilterSubject:     q.config.FilterSubject,
		AckPolicy:         q.config.AckPolicy,
		AckWait:           q.config.AckWait,
		MaxDeliver:        q.config.MaxDeliver,
		MaxWaiting:        q.config.MaxWaiting,
		MaxAckPending:     q.config.MaxAckPending,
		InactiveThreshold: q.config.InactiveThreshold,
	}

	return jsutil.EnsureConsumer(ctx, q.js, q.config.StreamName, cfg)
}

// runLoop is the main message processing loop.
func (q *Queue) runLoop(ctx context.Context) {
	defer close(q.loopDone)

	var backoff time.Duration

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		q.mu.RLock()
		cons := q.consumer
		q.mu.RUnlock()

		if cons == nil {
			return
		}

		iter, err := q.iterFactory(cons, q.config.BatchSize, q.config.FetchTimeout)
		if err != nil {
			q.metrics.IncrementWorkerConsumerIteratorRestart("create_error")
			q.logger.Error("failed to create iterator", "error", err)
			if !q.delayWithBackoffOrExit(ctx, &backoff) {
				return
			}
			continue
		}
		backoff = 0

		hadError := q.processIterator(ctx, iter)
		if hadError {
			q.metrics.IncrementWorkerConsumerIteratorRestart("transient_error")
			if !q.delayWithBackoffOrExit(ctx, &backoff) {
				return
			}
			continue
		}
		backoff = 0
	}
}

// processIterator processes messages from an iterator.
// Returns true if it exits due to a transient iterator error.
func (q *Queue) processIterator(ctx context.Context, iter jetstream.MessagesContext) bool {
	// Stop the iterator when the context is cancelled so that iter.Next()
	// unblocks with ErrMsgIteratorClosed instead of hanging indefinitely.
	stop := context.AfterFunc(ctx, iter.Stop)
	defer func() {
		_ = stop()
		iter.Stop() // Ensure iterator is stopped when exiting
	}()

	for {
		if ctx.Err() != nil {
			return false
		}

		msg, err := iter.Next()
		if err != nil {
			if errors.Is(err, jetstream.ErrMsgIteratorClosed) {
				return false
			}
			// Transient error, will retry with new iterator
			q.logger.Debug("iterator error", "error", err)
			return true
		}

		// Process the message
		handleErr := q.handler.Handle(ctx, msg)

		// Auto ack/nak if ManualAck is disabled
		if !q.config.ManualAck {
			if handleErr != nil {
				if nakErr := msg.Nak(); nakErr != nil {
					q.logger.Error("failed to nak message", "error", nakErr)
				}
			} else {
				if ackErr := msg.Ack(); ackErr != nil {
					q.logger.Error("failed to ack message", "error", ackErr)
				}
			}
		}
	}
}

func (q *Queue) delayWithBackoffOrExit(ctx context.Context, prev *time.Duration) bool {
	base := q.config.Retry.Base
	if base <= 0 {
		base = q.config.Retry.Backoff
	}

	*prev = jitterBackoff(*prev, base, q.config.Retry.Multiplier, q.config.Retry.Max, q.retryRNG)
	if *prev <= 0 {
		return ctx.Err() == nil
	}

	timer := time.NewTimer(*prev)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
