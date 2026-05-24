package durable

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/arloliu/parti/v2/internal/recovery"
	"github.com/arloliu/parti/v2/jsutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

type consumerIDSource string

const (
	consumerIDSourceFixed     consumerIDSource = "fixed"
	consumerIDSourceEnv       consumerIDSource = "env"
	consumerIDSourceGenerated consumerIDSource = "generated"
)

// BroadcastConsumer manages a single JetStream durable consumer per instance using
// a wildcard subject filter. All messages matching the filter are passed to the handler
// regardless of partition assignment.
//
// # Stream Requirement
//
// The underlying stream MUST use LimitsPolicy or InterestPolicy.
// WorkQueuePolicy is incompatible with broadcast pattern because it delivers
// each message to exactly one consumer, preventing fan-out.
//
// # Thread Safety
//
//   - UpdateWorkerConsumer is serialized; concurrent calls block on internal mutex
//   - Close is safe to call concurrently with UpdateWorkerConsumer
//
// # Message Handling
//
//   - All messages matching the WildcardFilter are passed to the handler
//   - Messages are ACKed after successful handling (or ManualAck)
//
// # Blocking Behavior
//
//   - Close may block up to the context deadline waiting for the pull loop to stop
type BroadcastConsumer struct {
	js      jetstream.JetStream
	config  BroadcastConsumerConfig
	logger  types.Logger
	handler messageHandler

	mu       sync.RWMutex
	updateMu sync.Mutex // Serializes UpdateWorkerConsumer calls

	consumerID       string
	consumerIDSource consumerIDSource

	// Consumer state
	consumer       jetstream.Consumer
	consumerMu     sync.RWMutex
	consumerConfig jetstream.ConsumerConfig // stored base config for recovery

	// Loop control
	loopCancel  context.CancelFunc
	loopDone    chan struct{}
	loopStarted bool

	// Iterator factory
	iterFactory func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)

	// Recovery
	recovery *recovery.Controller
}

// NewBroadcastConsumer creates a new broadcast fan-out consumer.
//
// The consumer uses a wildcard FilterSubject to receive all messages.
// On first UpdateWorkerConsumer call, creates the durable consumer and starts
// the pull loop. The durable name is derived from ConsumerID.
//
// # Stream Requirement
//
// The underlying stream MUST use LimitsPolicy or InterestPolicy.
// WorkQueuePolicy is incompatible because it delivers each message to
// exactly one consumer, defeating the fan-out purpose.
func NewBroadcastConsumer(js jetstream.JetStream, cfg BroadcastConsumerConfig, fn func(context.Context, jetstream.Msg) error) (*BroadcastConsumer, error) {
	if js == nil {
		return nil, errors.New("JetStream context is required")
	}
	if fn == nil {
		return nil, errors.New("message handler is required")
	}

	handler := messageHandlerFunc(fn)

	if err := cfg.SetDefaults(); err != nil {
		return nil, fmt.Errorf("failed to set defaults: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	consumerID, consumerIDSource, err := resolveBroadcastConsumerID(cfg)
	if err != nil {
		return nil, err
	}

	bc := &BroadcastConsumer{
		js:               js,
		config:           cfg,
		logger:           cfg.Logger,
		handler:          handler,
		consumerID:       consumerID,
		consumerIDSource: consumerIDSource,
		iterFactory:      defaultIterFactory,
		loopDone:         make(chan struct{}),
		recovery: recovery.NewController(recovery.ControllerConfig{
			Strategy:     cfg.RecoveryStrategy,
			FetchTimeout: cfg.FetchTimeout,
			Logger:       cfg.Logger,
			Metrics:      cfg.Metrics,
		}),
	}

	// Allow injection for tests
	if cfg.IteratorFactory != nil {
		bc.iterFactory = cfg.IteratorFactory
	}

	return bc, nil
}

// UpdateWorkerConsumer implements WorkerConsumerUpdater interface.
// It ensures the consumer loop is started using ConsumerID for the durable name.
//
// The partitions argument is IGNORED as this consumer receives all messages matching the wildcard.
// Partition updates take effect strictly by ensuring the consumer is active.
func (bc *BroadcastConsumer) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []types.Partition) error {
	bc.updateMu.Lock()
	defer bc.updateMu.Unlock()

	bc.mu.Lock()
	started := bc.loopStarted
	bc.mu.Unlock()

	// First call: create consumer and start loop
	if !started {
		if err := bc.startConsumerLoop(ctx); err != nil {
			return err
		}
	}

	// We log the partitions count just for visibility, but we don't filter by them.
	bc.logger.Debug("broadcast consumer updated",
		"workerID", workerID,
		"consumer_id", bc.consumerID,
		"status", "active",
		"ignored_partitions_count", len(partitions),
	)

	return nil
}

// Close stops the consumer loop. Consumer is left for server GC via InactiveThreshold.
func (bc *BroadcastConsumer) Close(ctx context.Context) error {
	bc.updateMu.Lock()
	defer bc.updateMu.Unlock()

	bc.mu.Lock()
	if bc.loopCancel != nil {
		bc.loopCancel()
	}
	started := bc.loopStarted
	bc.mu.Unlock()

	if !started {
		return nil
	}

	// Wait for loop to finish with context respect
	select {
	case <-bc.loopDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// startConsumerLoop creates the durable consumer and starts the pull loop.
func (bc *BroadcastConsumer) startConsumerLoop(ctx context.Context) error {
	var (
		cons     jetstream.Consumer
		consCfg  jetstream.ConsumerConfig
		err      error
		lastErr  error
		attempts = 3
	)

	for range attempts {
		durableName := bc.durableName()
		cons, consCfg, err = bc.ensureConsumer(ctx, durableName)
		if err == nil {
			bc.consumerMu.Lock()
			bc.consumer = cons
			bc.consumerConfig = consCfg
			bc.consumerMu.Unlock()

			bc.logger.Info("broadcast consumer ready",
				"op", "create_broadcast_consumer",
				"durable", durableName,
				"filter", bc.config.WildcardFilter,
			)

			break
		}

		lastErr = err
		if !isConsumerNameConflict(err) || bc.consumerIDSource == consumerIDSourceFixed {
			return fmt.Errorf("create broadcast consumer %s: %w", durableName, err)
		}

		consumerID, idErr := generateShortConsumerID()
		if idErr != nil {
			return idErr
		}

		bc.mu.Lock()
		bc.consumerID = consumerID
		bc.consumerIDSource = consumerIDSourceGenerated
		bc.mu.Unlock()
	}

	if err != nil {
		return fmt.Errorf("create broadcast consumer failed after %d attempts: %w", attempts, lastErr)
	}

	// Create loop context
	loopCtx, cancel := context.WithCancel(context.Background())
	bc.mu.Lock()
	bc.loopCancel = cancel
	bc.loopStarted = true
	bc.mu.Unlock()

	// Start the pull loop
	go bc.runLoop(loopCtx)

	return nil
}

// durableName returns the durable consumer name for this broadcast consumer.
func (bc *BroadcastConsumer) durableName() string {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return fmt.Sprintf("%s_broadcast_%s", bc.config.ConsumerPrefix, sanitizeConsumerName(bc.consumerID))
}

// ensureConsumer creates or updates the durable consumer with wildcard filter.
// It returns the consumer config that was used so the caller can snapshot it
// for recovery after a successful creation.
func (bc *BroadcastConsumer) ensureConsumer(ctx context.Context, durable string) (jetstream.Consumer, jetstream.ConsumerConfig, error) {
	cfg := jetstream.ConsumerConfig{
		Name:              durable,
		Durable:           durable,
		FilterSubject:     bc.config.WildcardFilter,
		AckPolicy:         bc.config.AckPolicy,
		AckWait:           bc.config.AckWait,
		MaxDeliver:        bc.config.MaxDeliver,
		InactiveThreshold: bc.config.InactiveThreshold,
		MaxWaiting:        bc.config.MaxWaiting,
		MaxAckPending:     bc.config.MaxAckPending,
		MemoryStorage:     bc.config.ConsumerMemoryStorage,
		Replicas:          bc.config.ConsumerReplicas,
	}

	cons, err := jsutil.EnsureConsumer(ctx, bc.js, bc.config.StreamName, cfg)

	return cons, cfg, err
}

// runLoop is the main message processing loop.
func (bc *BroadcastConsumer) runLoop(ctx context.Context) {
	defer close(bc.loopDone)

	bc.logger.Debug("broadcast consumer loop starting", "filter", bc.config.WildcardFilter)
	defer bc.logger.Debug("broadcast consumer loop stopped", "filter", bc.config.WildcardFilter)

	// Seed the recovery checkpoint from the server-side ack floor.
	bc.recovery.SeedCheckpoint(ctx, bc.consumerInfoFn())

	batch := bc.config.BatchSize
	expiry := bc.config.FetchTimeout

	for {
		if ctx.Err() != nil {
			return
		}

		bc.consumerMu.RLock()
		cons := bc.consumer
		bc.consumerMu.RUnlock()

		iter, err := bc.iterFactory(cons, batch, expiry)
		if err != nil {
			bc.logger.Warn("iterator creation failed", "error", err)
			if bc.config.Metrics != nil {
				bc.config.Metrics.IncrementWorkerConsumerIteratorRestart("transient")
			}

			if bc.delayOrExit(ctx) {
				return
			}

			continue
		}

		iterErr := bc.processIterator(ctx, iter)
		if iterErr == nil {
			continue
		}

		bc.consumerMu.RLock()
		consumerConfig := bc.consumerConfig
		bc.consumerMu.RUnlock()

		action, newCons, classifyErr := bc.recovery.Classify(ctx, iterErr, bc.consumerInfoFn(), consumerConfig, bc.recreateFn())
		switch action {
		case recovery.ActionExit:
			return
		case recovery.ActionContinue:
			bc.consumerMu.Lock()
			bc.consumer = newCons
			bc.consumerMu.Unlock()
			// SeedCheckpoint on a freshly created consumer is typically a no-op:
			// the new consumer's ack floor is 0 and the checkpoint monotonically
			// advances, so it will not regress. Called here as a best-effort update
			// in case the consumer was recreated over an existing durable.
			bc.recovery.SeedCheckpoint(ctx, bc.consumerInfoFn())

			continue
		case recovery.ActionStreamMissing:
			// Broadcast does not own stream lifecycle; log for operator
			// observability and backoff. No callback surface today.
			bc.logger.Warn("broadcast consumer recovery classified stream missing",
				"op", "broadcast_stream_missing",
				"stream", bc.config.StreamName,
				"error", classifyErr,
			)
			if bc.delayOrExit(ctx) {
				return
			}
		case recovery.ActionBackoff:
			if bc.delayOrExit(ctx) {
				return
			}
		}
	}
}

// processIterator processes messages from the iterator until error or context cancellation.
// Returns nil on graceful exit (context canceled or ErrMsgIteratorClosed).
// Returns the iterator error so the caller can classify and handle recovery.
func (bc *BroadcastConsumer) processIterator(ctx context.Context, iter jetstream.MessagesContext) error {
	// Start stopper goroutine
	stopperCh := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			iter.Stop()
		case <-stopperCh:
		}
	}()

	defer func() {
		select {
		case <-stopperCh:
		default:
			close(stopperCh)
		}
	}()

	for {
		if ctx.Err() != nil {
			iter.Stop()
			return nil
		}

		msg, err := iter.Next()
		if err != nil {
			bc.logger.Debug("iterator next error", "error", err)
			iter.Stop()
			if errors.Is(err, jetstream.ErrMsgIteratorClosed) || errors.Is(err, context.Canceled) {
				return nil // graceful shutdown
			}
			return err // caller classifies for recovery or backoff
		}

		// Process message via handler
		if bc.handler == nil {
			_ = msg.Nak()
			continue
		}

		bc.recovery.Dispatch(ctx, msg, bc.config.ManualAck, bc.handler.Handle)
	}
}

// delayOrExit applies backoff delay or returns true if context is cancelled.
func (bc *BroadcastConsumer) delayOrExit(ctx context.Context) bool {
	delay := jitterBackoff(0, bc.config.Retry.Base, bc.config.Retry.Multiplier, bc.config.Retry.Max, nil)
	bc.logger.Debug("backoff", "delay_ms", delay.Milliseconds())

	select {
	case <-ctx.Done():
		return true
	case <-time.After(delay):
		return false
	}
}

// consumerInfoFn returns a function that calls consumer.Info() on the current consumer.
func (bc *BroadcastConsumer) consumerInfoFn() recovery.InfoFunc {
	return func(ctx context.Context) (*jetstream.ConsumerInfo, error) {
		bc.consumerMu.RLock()
		cons := bc.consumer
		bc.consumerMu.RUnlock()
		if cons == nil {
			return nil, errors.New("no consumer")
		}
		return cons.Info(ctx)
	}
}

// recreateFn returns a function that recreates the consumer via EnsureConsumer.
func (bc *BroadcastConsumer) recreateFn() recovery.RecreateFunc {
	return func(ctx context.Context, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {
		return jsutil.EnsureConsumer(ctx, bc.js, bc.config.StreamName, cfg)
	}
}

func resolveBroadcastConsumerID(cfg BroadcastConsumerConfig) (string, consumerIDSource, error) {
	configuredID := strings.TrimSpace(cfg.ConsumerID)
	if configuredID == "" {
		// Check environment variables for a non-empty value
		if envID := firstNonEmptyEnv("HOSTNAME", "POD_NAME"); envID != "" {
			return envID, consumerIDSourceEnv, nil
		}

		return generateConsumerIDFallback()
	}

	if after, ok := strings.CutPrefix(configuredID, "env:"); ok {
		envName := strings.TrimSpace(after)
		if envName == "" {
			return "", consumerIDSourceFixed, errors.New("consumer ID env name is required")
		}

		if envID := strings.TrimSpace(os.Getenv(envName)); envID != "" {
			return envID, consumerIDSourceEnv, nil
		}

		return generateConsumerIDFallback()
	}

	return configuredID, consumerIDSourceFixed, nil
}

func generateConsumerIDFallback() (string, consumerIDSource, error) {
	consumerID, err := generateShortConsumerID()
	if err != nil {
		return "", consumerIDSourceFixed, err
	}

	return consumerID, consumerIDSourceGenerated, nil
}

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}

	return ""
}

func generateShortConsumerID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate consumer ID: %w", err)
	}

	return hex.EncodeToString(b[:]), nil
}

func isConsumerNameConflict(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "consumer name already in use") ||
		strings.Contains(msg, "name already in use") ||
		strings.Contains(msg, "consumer already exists") ||
		// NATS returns this when a pull consumer is created with a name that
		// is already in use by an existing push consumer on the same stream.
		strings.Contains(msg, "can not update push consumer to pull based")
}
