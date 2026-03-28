package partition

import (
	"context"
	"errors"
	"sync"
	"time"

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

	consumer jetstream.Consumer

	// keyDispatcher handles per-key concurrent processing (nil if disabled)
	keyDispatcher *keyDispatcher

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

	parts, err := parsePattern(config.SubjectPattern)
	if err != nil {
		return nil, err
	}

	subject := parts.buildFilterSubject(config.Partition)
	if err := validateSubjectTokens(subject, true); err != nil {
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

	// Initialize key dispatcher if DispatchByKey is enabled
	if config.DispatchByKey != nil && *config.DispatchByKey {
		// Use pattern-aware key extractor if no custom one provided
		keyExtractor := config.KeyExtractor
		if keyExtractor == nil {
			keyExtractor = parts.keyExtractorFunc()
		}

		c.keyDispatcher = newKeyDispatcher(
			config.Logger,
			config.Metrics,
			handler,
			keyExtractor,
			config.KeyChannelBuffer,
			config.KeyIdleTimeout,
			config.ManualAck,
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
	c.consumer = cons

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

	return jsutil.EnsureConsumer(ctx, c.js, c.config.StreamName, cfg)
}

func (c *JSConsumer) run(ctx context.Context) {
	defer close(c.done)

	for {
		if ctx.Err() != nil {
			return
		}

		heartbeat := max(c.config.FetchTimeout/2, 100*time.Millisecond)

		iter, err := c.consumer.Messages(
			jetstream.PullMaxMessages(c.config.BatchSize),
			jetstream.PullExpiry(c.config.FetchTimeout),
			jetstream.PullHeartbeat(heartbeat),
		)
		if err != nil {
			c.config.Metrics.IncrementWorkerConsumerIteratorRestart("error")
			c.config.Logger.Warn("partition consumer messages iterator error", "error", err, "subject", c.subject)
			if !sleepWithContext(ctx, 200*time.Millisecond) {
				return
			}
			continue
		}

		if !c.processIterator(ctx, iter) {
			return
		}
	}
}

func (c *JSConsumer) processIterator(ctx context.Context, iter jetstream.MessagesContext) bool {
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
			// If the iterator was explicitly closed, don't retry - context is likely cancelled
			if errors.Is(err, jetstream.ErrMsgIteratorClosed) {
				c.config.Logger.Debug("partition consumer iterator closed", "subject", c.subject)
				return false
			}

			// For other errors (timeout, connection issues), log and retry with new iterator
			c.config.Logger.Debug("partition consumer iterator error, will retry", "error", err, "subject", c.subject)

			return sleepWithContext(ctx, 200*time.Millisecond)
		}

		// Dispatch to key dispatcher if enabled
		if c.keyDispatcher != nil {
			if !c.keyDispatcher.Dispatch(ctx, msg) {
				return false
			}
			continue
		}

		// Sequential processing (DispatchByKey disabled)
		if c.config.ManualAck {
			_ = c.handler.Handle(ctx, msg)
			continue
		}

		if err := c.handler.Handle(ctx, msg); err != nil {
			_ = msg.Nak()
		} else {
			_ = msg.Ack()
		}
	}
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
