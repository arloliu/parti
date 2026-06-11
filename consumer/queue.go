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
	"github.com/arloliu/parti/v2/internal/recovery"
	"github.com/arloliu/parti/v2/jsutil"
	"github.com/arloliu/parti/v2/types"
)

// errIteratorClosedUnexpectedly is returned by processIterator when the iterator
// reports ErrMsgIteratorClosed while the loop context is still alive (i.e., not a
// graceful shutdown). The sentinel is unexported so callers inside runLoop handle
// it via the normal backoff path rather than forwarding to the recovery classifier
// (which would return ActionExit for ErrMsgIteratorClosed).
var errIteratorClosedUnexpectedly = errors.New("iterator closed unexpectedly")

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

	// Recovery
	recovery       *recovery.Controller     // nil when recovery is disabled
	consumerConfig jetstream.ConsumerConfig // base config stored at ensureConsumer time
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

	// RecoveryStrategy defines how a recreated consumer resumes after an unexpected deletion.
	//
	// Supported values for Queue consumers:
	//   - [RecoverFromNew]: skip messages published during the outage.
	//   - [RecoverFromBeginning]: replay all messages from the start of the stream.
	//
	// [RecoverFromLastProcessed] is rejected at construction time because Queue shares
	// one durable across all replicas — per-process checkpointing is nondeterministic.
	//
	// # WorkQueuePolicy streams
	//
	// NATS only permits [jetstream.DeliverAllPolicy] on WorkQueuePolicy streams.
	// [RecoverFromNew] maps to [jetstream.DeliverNewPolicy] and is therefore incompatible:
	// [Queue.Start] rejects the combination with [ErrInvalidConfig]. The pre-flight
	// check is best-effort — transient failures to fetch stream info are ignored
	// (see [CheckWorkQueueRecoveryCompat]) — so an incompatible combination can
	// slip past Start and only surface when recovery first misbehaves.
	// Use [RecoverFromBeginning] or [RecoveryDisabled] for WorkQueuePolicy streams.
	RecoveryStrategy RecoveryStrategy
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
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	if err := validateFetchTimeoutFloor(c.FetchTimeout); err != nil {
		return err
	}

	if !jsutil.IsValidConsumerName(c.ConsumerName) {
		return fmt.Errorf("%w: consumer name %q contains invalid characters (allowed: a-z, A-Z, 0-9, -, _)", ErrInvalidConfig, c.ConsumerName)
	}
	if err := validateSubjectTokens(c.FilterSubject, true); err != nil {
		return fmt.Errorf("%w: filter subject is invalid: %w", ErrInvalidConfig, err)
	}
	if c.RecoveryStrategy == RecoverFromLastProcessed {
		return fmt.Errorf("%w: RecoverFromLastProcessed is not supported for Queue consumers"+
			" (per-process checkpoint tracking is unsafe when multiple instances share one durable)", ErrInvalidConfig)
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
		return nil, fmt.Errorf("%w: JetStream context is required", ErrInvalidConfig)
	}
	if streamName == "" {
		return nil, fmt.Errorf("%w: stream name is required", ErrInvalidConfig)
	}
	if consumerName == "" {
		return nil, fmt.Errorf("%w: consumer name is required", ErrInvalidConfig)
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

			ConsumerMemoryStorage: o.consumerMemoryStorage,
			ConsumerReplicas:      o.consumerReplicas,
		},
		StreamName:       streamName,
		ConsumerName:     consumerName,
		FilterSubject:    filterSubject,
		Retry:            o.retry,
		IteratorFactory:  o.iteratorFactory,
		RecoveryStrategy: o.recoveryStrategy,
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
		recovery: recovery.NewController(recovery.ControllerConfig{
			Strategy:     cfg.RecoveryStrategy,
			FetchTimeout: cfg.FetchTimeout,
			Logger:       cfg.Logger,
			Metrics:      cfg.Metrics,
		}),
	}, nil
}

// defaultIterFactory creates a messages iterator with heartbeat and expiry.
func defaultIterFactory(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
	heartbeat := max(expiry/2, 100*time.Millisecond)
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

	if err := CheckWorkQueueRecoveryCompat(ctx, q.js, q.config.StreamName, q.config.RecoveryStrategy); err != nil {
		return err
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
//
// If ctx expires before the pull loop exits, Stop returns the context error
// and the consumer still counts as started: a subsequent [Queue.Start] fails,
// and a second Stop call (which waits for the loop again) is required before
// the consumer can be restarted.
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
// Callers must hold q.mu (write lock); stores the base config for recovery.
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
		MemoryStorage:     q.config.ConsumerMemoryStorage,
		Replicas:          q.config.ConsumerReplicas,
	}

	// Store base config once for recovery.
	if q.consumerConfig.Durable == "" {
		q.consumerConfig = cfg
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
		consumerConfig := q.consumerConfig
		q.mu.RUnlock()

		if cons == nil {
			// Defensive: Start always sets q.consumer before launching the loop,
			// so this exit indicates internal state was cleared unexpectedly.
			q.logger.Warn("queue consumer loop exiting: consumer reference is nil",
				"stream", q.config.StreamName,
				"consumer", q.config.ConsumerName,
			)

			return
		}

		iter, err := q.iterFactory(cons, q.config.BatchSize, q.config.FetchTimeout)
		if err != nil {
			q.metrics.IncrementWorkerConsumerIteratorRestart("create_error")
			q.logger.Error("failed to create iterator", "error", err)
			if q.delayWithBackoffOrExit(ctx, &backoff) {
				return
			}
			continue
		}
		backoff = 0

		iterErr := q.processIterator(ctx, iter)
		if iterErr == nil {
			continue
		}

		// Iterator closed while loop context is alive (not a graceful shutdown).
		// Back off and retry without forwarding to the recovery classifier, which
		// would return ActionExit for ErrMsgIteratorClosed.
		if errors.Is(iterErr, errIteratorClosedUnexpectedly) {
			if q.delayWithBackoffOrExit(ctx, &backoff) {
				return
			}
			continue
		}

		if q.recovery == nil {
			// RecoveryDisabled (the default): the recovery controller is nil and
			// classification short-circuits before any metric. A deleted durable
			// causes a permanent stall with no operator signal at production log
			// levels without this explicit path.
			q.logger.Warn("iterator error with recovery disabled; consumer will retry but cannot recreate a deleted durable",
				"error", iterErr,
				"stream", q.config.StreamName,
				"filter", q.config.FilterSubject,
			)
			q.metrics.IncrementWorkerConsumerIteratorRestart("recovery_disabled")
		}

		action, newCons, classifyErr := q.recovery.Classify(ctx, iterErr, q.consumerInfoFn(), consumerConfig, q.recreateFn())
		switch action {
		case recovery.ActionExit:
			return
		case recovery.ActionContinue:
			q.mu.Lock()
			q.consumer = newCons
			q.mu.Unlock()
			backoff = 0
			continue
		case recovery.ActionStreamMissing:
			// Queue does not own stream lifecycle (the JetStream stream
			// it consumes is operator-provisioned); the typed-error
			// classification is logged for operator observability and
			// folded into a backoff. No callback surface today.
			q.logger.Warn("queue consumer recovery classified stream missing",
				"op", "queue_stream_missing",
				"stream", q.config.StreamName,
				"error", classifyErr,
			)
			if q.delayWithBackoffOrExit(ctx, &backoff) {
				return
			}
		case recovery.ActionBackoff:
			if q.delayWithBackoffOrExit(ctx, &backoff) {
				return
			}
		}
	}
}

// consumerInfoFn returns a function that calls consumer.Info() on the current consumer.
func (q *Queue) consumerInfoFn() recovery.InfoFunc {
	return func(ctx context.Context) (*jetstream.ConsumerInfo, error) {
		q.mu.RLock()
		cons := q.consumer
		q.mu.RUnlock()
		if cons == nil {
			return nil, errors.New("no consumer")
		}
		return cons.Info(ctx)
	}
}

// recreateFn returns a function that recreates the consumer via EnsureConsumer.
func (q *Queue) recreateFn() recovery.RecreateFunc {
	return func(ctx context.Context, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {
		return jsutil.EnsureConsumer(ctx, q.js, q.config.StreamName, cfg)
	}
}

// processIterator processes messages from an iterator.
// Returns nil on graceful exit (context canceled or ErrMsgIteratorClosed).
// Returns the iterator error so the caller can classify and handle recovery.
func (q *Queue) processIterator(ctx context.Context, iter jetstream.MessagesContext) error {
	// Stop the iterator when the context is cancelled so that iter.Next()
	// unblocks with ErrMsgIteratorClosed instead of hanging indefinitely.
	stop := context.AfterFunc(ctx, iter.Stop)
	defer func() {
		_ = stop()
		iter.Stop() // Ensure iterator is stopped when exiting
	}()

	for {
		if ctx.Err() != nil {
			return nil
		}

		msg, err := iter.Next()
		if err != nil {
			if errors.Is(err, jetstream.ErrMsgIteratorClosed) || errors.Is(err, context.Canceled) {
				if ctx.Err() != nil {
					return nil // graceful shutdown: loop context cancelled
				}
				// Iterator closed while the loop context is still alive — the
				// underlying pull consumer may have been server-side closed.
				// Return the sentinel so runLoop takes the backoff path instead
				// of spinning (a nil return resets backoff to 0).
				return errIteratorClosedUnexpectedly
			}
			q.logger.Debug("iterator error", "error", err)

			return err // caller classifies for recovery or backoff
		}

		// Process the message
		handleErr := q.handler.Handle(ctx, msg)

		// Auto ack/nak if ManualAck is disabled.
		// Note: Queue does not call recovery.AdvanceCheckpoint after ack. Queue rejects
		// RecoverFromLastProcessed at construction time (shared durables make per-process
		// checkpoint tracking unsafe), so the checkpoint value is never read. Intentional.
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

// delayWithBackoffOrExit applies jittered exponential backoff.
// Returns true when the caller should exit (context cancelled),
// false when the delay completed and the caller may continue.
// This matches the convention used by partitionConsumer and broadcastConsumer.
func (q *Queue) delayWithBackoffOrExit(ctx context.Context, prev *time.Duration) bool {
	base := q.config.Retry.Base
	if base <= 0 {
		base = q.config.Retry.Backoff
	}

	*prev = jitterBackoff(*prev, base, q.config.Retry.Multiplier, q.config.Retry.Max, q.retryRNG)
	if *prev <= 0 {
		return ctx.Err() != nil
	}

	timer := time.NewTimer(*prev)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-timer.C:
		return false
	}
}
