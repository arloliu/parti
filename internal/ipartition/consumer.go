package ipartition

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/arloliu/parti/v2/internal/partutil"
	"github.com/arloliu/parti/v2/internal/recovery"
	"github.com/arloliu/parti/v2/jsutil"
	"github.com/nats-io/nats.go/jetstream"
)

// PartitionJSConsumer defines the interface for JetStream partition-aware consuming.
type PartitionJSConsumer interface {
	// Start begins consuming messages in a background goroutine.
	Start(ctx context.Context) error

	// Stop gracefully stops the consumer.
	Stop(ctx context.Context) error

	// Partition returns the partition index this consumer handles.
	Partition() int

	// Subject returns the NATS subject this consumer subscribes to.
	Subject() string
}

// JSConsumer consumes messages from a single static partition using JetStream.
//
// This consumer is designed for StatefulSet deployments where each pod
// handles a fixed partition based on its ordinal index.
//
// When DispatchByKey is enabled, messages are routed to per-key goroutines
// for concurrent processing while preserving per-key ordering.
type JSConsumer struct {
	js      jetstream.JetStream
	config  ConsumerConfig
	handler messageHandler

	partition int
	subject   string

	consumer       jetstream.Consumer
	consumerConfig jetstream.ConsumerConfig
	consumerMu     sync.RWMutex

	// keyDispatcher handles per-key concurrent processing (nil if disabled)
	keyDispatcher *keyDispatcher

	// Recovery controller (nil when recovery is disabled)
	rc *recovery.Controller

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewJSConsumer creates a JetStream consumer for a specific partition.
//
// Parameters:
//   - js: JetStream context
//   - config: Consumer configuration
//   - fn: Message handler function
//
// Returns:
//   - *JSConsumer: Configured consumer
//   - error: Validation or initialization error
func NewJSConsumer(
	js jetstream.JetStream,
	config ConsumerConfig,
	fn func(context.Context, jetstream.Msg) error,
) (*JSConsumer, error) {
	if js == nil {
		return nil, errors.New("jetstream context is required")
	}
	if fn == nil {
		return nil, errors.New("message handler is required")
	}
	handler := messageHandlerFunc(fn)
	if err := config.Validate(); err != nil {
		return nil, err
	}

	parts, err := partutil.ParsePattern(config.SubjectPattern)
	if err != nil {
		return nil, err
	}

	subject := parts.BuildFilterSubject(config.Partition)
	if err := partutil.ValidateSubjectTokens(subject, true); err != nil {
		return nil, err
	}

	c := &JSConsumer{
		js:        js,
		config:    config,
		handler:   handler,
		partition: config.Partition,
		subject:   subject,
		done:      make(chan struct{}),
	}

	c.rc = recovery.NewController(recovery.ControllerConfig{
		Strategy:       config.RecoveryStrategy,
		BurstThreshold: recovery.DefaultBurstThreshold,
		BurstWindow:    recovery.DefaultBurstWindow(config.FetchTimeout),
		Logger:         config.Logger,
		Metrics:        config.Metrics,
	})

	// Initialize key dispatcher if DispatchByKey is enabled
	if config.DispatchByKey != nil && *config.DispatchByKey {
		// Use pattern-aware key extractor if no custom one provided
		keyExtractor := config.KeyExtractor
		if keyExtractor == nil {
			keyExtractor = KeyExtractorFunc(parts.KeyExtractorFunc())
		}

		var onAck func(jetstream.Msg)
		if c.rc != nil && c.rc.Strategy() == recovery.FromLastProcessed && !config.ManualAck {
			onAck = c.rc.AdvanceCheckpoint
		}

		var wrapMsg func(jetstream.Msg) jetstream.Msg
		if c.rc != nil && c.rc.Strategy() == recovery.FromLastProcessed && config.ManualAck {
			wrapMsg = c.rc.WrapForTracking
		}

		c.keyDispatcher = newKeyDispatcher(
			config.Logger,
			handler,
			keyExtractor,
			config.KeyChannelBuffer,
			config.KeyIdleTimeout,
			config.ManualAck,
			onAck,
			wrapMsg,
		)
	}

	return c, nil
}

// Start begins consuming messages in a background goroutine.
//
// Parameters:
//   - ctx: Context for lifecycle control
//
// Returns:
//   - error: Startup error
func (c *JSConsumer) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return errors.New("consumer already started")
	}

	cons, err := c.ensureConsumer(ctx)
	if err != nil {
		return err
	}
	c.consumerMu.Lock()
	c.consumer = cons
	c.consumerMu.Unlock()

	loopCtx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.done = make(chan struct{})

	go c.run(loopCtx)

	return nil
}

// Stop gracefully stops the consumer and drains pending messages.
//
// Parameters:
//   - ctx: Context with shutdown deadline
//
// Returns:
//   - error: Shutdown error (context cancellation)
func (c *JSConsumer) Stop(ctx context.Context) error {
	c.mu.Lock()
	cancel := c.cancel
	done := c.done
	dispatcher := c.keyDispatcher
	c.cancel = nil
	c.mu.Unlock()

	if cancel == nil {
		return nil
	}
	cancel()

	// Wait for run loop to stop first
	select {
	case <-done:
		// Run loop stopped, now close dispatcher
	case <-ctx.Done():
		return ctx.Err()
	}

	// Close key dispatcher if enabled (after run loop has stopped)
	if dispatcher != nil {
		return dispatcher.Close(ctx)
	}

	return nil
}

// Partition returns the partition index this consumer handles.
//
// Returns:
//   - int: Partition index
func (c *JSConsumer) Partition() int {
	return c.partition
}

// Subject returns the NATS subject this consumer subscribes to.
//
// Returns:
//   - string: Subject filter for this partition
func (c *JSConsumer) Subject() string {
	return c.subject
}

func (c *JSConsumer) ensureConsumer(ctx context.Context) (jetstream.Consumer, error) {
	cfg := jetstream.ConsumerConfig{
		Durable:           c.config.ConsumerName,
		AckPolicy:         c.config.AckPolicy,
		FilterSubject:     c.subject,
		MaxDeliver:        c.config.MaxDeliver,
		AckWait:           c.config.AckWait,
		MaxAckPending:     c.config.MaxAckPending,
		InactiveThreshold: c.config.InactiveThreshold,
		MaxWaiting:        c.config.MaxWaiting,
	}

	c.consumerMu.Lock()
	c.consumerConfig = cfg
	c.consumerMu.Unlock()

	return jsutil.EnsureConsumer(ctx, c.js, c.config.StreamName, cfg)
}

func (c *JSConsumer) run(ctx context.Context) {
	defer close(c.done)

	c.rc.SeedCheckpoint(ctx, c.consumerInfoFn())

	for {
		if ctx.Err() != nil {
			return
		}

		c.consumerMu.RLock()
		cons := c.consumer
		c.consumerMu.RUnlock()
		if cons == nil {
			return
		}

		heartbeat := max(c.config.FetchTimeout/2, 100*time.Millisecond)

		iter, err := cons.Messages(
			jetstream.PullMaxMessages(c.config.BatchSize),
			jetstream.PullExpiry(c.config.FetchTimeout),
			jetstream.PullHeartbeat(heartbeat),
		)
		if err != nil {
			c.config.Metrics.IncrementWorkerConsumerIteratorRestart("transient")
			c.config.Logger.Warn("partition consumer messages iterator error", "error", err, "subject", c.subject)
			if !sleepWithContext(ctx) {
				return
			}
			continue
		}

		iterErr := c.processIterator(ctx, iter)
		if iterErr == nil {
			continue
		}

		c.consumerMu.RLock()
		consumerConfig := c.consumerConfig
		c.consumerMu.RUnlock()

		action, newCons := c.rc.Classify(ctx, iterErr, c.consumerInfoFn(), consumerConfig, c.recreateFn())
		switch action {
		case recovery.ActionExit:
			return
		case recovery.ActionContinue:
			c.consumerMu.Lock()
			c.consumer = newCons
			c.consumerMu.Unlock()
			// SeedCheckpoint on a freshly created consumer is typically a no-op:
			// the new consumer's ack floor is 0 and the checkpoint monotonically
			// advances, so it will not regress. Called here as a best-effort update
			// in case the consumer was recreated over an existing durable.
			c.rc.SeedCheckpoint(ctx, c.consumerInfoFn())

			continue
		case recovery.ActionBackoff:
			if !sleepWithContext(ctx) {
				return
			}
		}
	}
}

func (c *JSConsumer) processIterator(ctx context.Context, iter jetstream.MessagesContext) error {
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
				c.config.Logger.Debug("partition consumer iterator closed", "subject", c.subject)
				return nil
			}

			c.config.Logger.Debug("partition consumer iterator error, will retry", "error", err, "subject", c.subject)

			return err
		}

		// Dispatch to key dispatcher if enabled
		if c.keyDispatcher != nil {
			if !c.keyDispatcher.Dispatch(ctx, msg) {
				return nil
			}
			continue
		}

		// Sequential processing (DispatchByKey disabled)
		if c.config.ManualAck {
			_ = c.handler.Handle(ctx, c.rc.WrapForTracking(msg))
			continue
		}

		if err := c.handler.Handle(ctx, msg); err != nil {
			_ = msg.Nak()
		} else {
			if err := msg.Ack(); err == nil {
				c.rc.AdvanceCheckpoint(msg)
			}
		}
	}
}

func (c *JSConsumer) consumerInfoFn() recovery.InfoFunc {
	return func(ctx context.Context) (*jetstream.ConsumerInfo, error) {
		c.consumerMu.RLock()
		cons := c.consumer
		c.consumerMu.RUnlock()
		if cons == nil {
			return nil, errors.New("no consumer")
		}
		return cons.Info(ctx)
	}
}

func (c *JSConsumer) recreateFn() recovery.RecreateFunc {
	return func(ctx context.Context, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {
		return jsutil.EnsureConsumer(ctx, c.js, c.config.StreamName, cfg)
	}
}

const iteratorRetryDelay = 200 * time.Millisecond

func sleepWithContext(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(iteratorRetryDelay):
		return true
	}
}
