package subscription

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// partitionConsumerConfig configures a single partition consumer.
type partitionConsumerConfig struct {
	// Pull settings
	BatchSize    int
	FetchTimeout time.Duration
	ManualAck    bool

	// Retry/Escalation
	Retry                       RetryConfig
	IteratorEscalationWindow    time.Duration
	IteratorEscalationThreshold int

	// Metrics
	Metrics types.MetricsCollector
}

// partitionConsumer manages the consumption loop for a single partition (subject).
// It handles message pulling, failure escalation, and graceful draining.
type partitionConsumer struct {
	logger types.Logger
	js     jetstream.JetStream

	// Identities
	streamName  string
	durableName string
	subject     string
	partitionID string

	// Configuration
	config         partitionConsumerConfig
	consumerConfig jetstream.ConsumerConfig

	// Callbacks
	iterFactory          func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)
	checkPullSuppression func(ctx context.Context) (bool, string)

	// State
	consumer   jetstream.Consumer
	consumerMu sync.RWMutex // Protects consumer replacement

	// Loop control
	lifecycleMu sync.Mutex
	cancel      func()
	stopped     bool
	done        chan struct{}

	// Failure tracking
	iterFailureTimes []time.Time
	lastEscalation   time.Time
	iterEscMu        sync.Mutex

	// Info lock
	consInfoMu sync.Mutex
}

// partitionConsumerOpts contains the identity and dependency options for a partition consumer.
type partitionConsumerOpts struct {
	streamName           string
	durableName          string
	subject              string
	partitionID          string
	consumerConfig       jetstream.ConsumerConfig
	consumer             jetstream.Consumer
	iterFactory          func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)
	checkPullSuppression func(ctx context.Context) (bool, string)
}

// newPartitionConsumer creates a new partition consumer.
func newPartitionConsumer(
	logger types.Logger,
	js jetstream.JetStream,
	config partitionConsumerConfig,
	opts partitionConsumerOpts,
) *partitionConsumer {
	return &partitionConsumer{
		logger:               logger,
		js:                   js,
		streamName:           opts.streamName,
		durableName:          opts.durableName,
		subject:              opts.subject,
		partitionID:          opts.partitionID,
		config:               config,
		consumerConfig:       opts.consumerConfig,
		consumer:             opts.consumer,
		iterFactory:          opts.iterFactory,
		checkPullSuppression: opts.checkPullSuppression,
		done:                 make(chan struct{}),
	}
}

// Run starts the consumption loop. It blocks until the context is canceled.
func (pc *partitionConsumer) Run(ctx context.Context, handler MessageHandler) {
	pc.lifecycleMu.Lock()
	if pc.stopped {
		pc.lifecycleMu.Unlock()
		close(pc.done)
		return
	}

	// Create a child context for cancellation if not already provided
	ctx, cancel := context.WithCancel(ctx)
	pc.cancel = cancel
	pc.lifecycleMu.Unlock()

	pc.logger.Debug("partition consumer loop starting", "subject", pc.subject)
	defer func() {
		pc.logger.Debug("partition consumer loop stopped", "subject", pc.subject)
		close(pc.done)
	}()

	batch := pc.config.BatchSize
	expiry := pc.config.FetchTimeout

	for {
		if ctx.Err() != nil {
			return
		}

		if pc.checkPullSuppression != nil {
			if suppressed, reason := pc.checkPullSuppression(ctx); suppressed {
				pc.logger.Debug("pull suppressed", "subject", pc.subject, "reason", reason)
				select {
				case <-ctx.Done():
					return
				case <-time.After(150 * time.Millisecond):
					continue
				}
			}
		}

		// Get current consumer under lock
		pc.consumerMu.RLock()
		cons := pc.consumer
		pc.consumerMu.RUnlock()

		// Create iterator and enter message processing loop.
		pc.consInfoMu.Lock()
		iter, err := pc.iterFactory(cons, batch, expiry)
		pc.consInfoMu.Unlock()
		if err != nil {
			pc.logger.Warn("iterator creation failed", "subject", pc.subject, "error", err)
			if pc.config.Metrics != nil {
				pc.config.Metrics.IncrementWorkerConsumerIteratorRestart("transient")
			}

			pc.maybeEscalateIteratorFailures(ctx)

			if pc.delayWithBackoffOrExit(ctx, "iterate") {
				return
			}

			continue
		}

		exit, iterErr := pc.processIterator(ctx, iter, handler)
		if exit {
			return
		}
		if iterErr != nil {
			pc.logger.Warn("iterator error", "subject", pc.subject, "error", iterErr)
			// Classify restart reason for metrics parity
			if pc.config.Metrics != nil {
				if errors.Is(iterErr, jetstream.ErrNoHeartbeat) {
					pc.config.Metrics.IncrementWorkerConsumerIteratorRestart("heartbeat")
				} else {
					pc.config.Metrics.IncrementWorkerConsumerIteratorRestart("transient")
				}
			}
			pc.maybeEscalateIteratorFailures(ctx)
			if pc.delayWithBackoffOrExit(ctx, "iterate") {
				return
			}
		}
		// loop continues to recreate iterator on next iteration
	}
}

// Stop cancels the consumption loop.
func (pc *partitionConsumer) Stop() {
	pc.lifecycleMu.Lock()
	defer pc.lifecycleMu.Unlock()

	pc.stopped = true
	if pc.cancel != nil {
		pc.cancel()
	}
}

// Wait waits for the consumption loop to exit.
func (pc *partitionConsumer) Wait() {
	<-pc.done
}

// Consumer returns the current underlying JetStream consumer.
func (pc *partitionConsumer) Consumer() jetstream.Consumer {
	pc.consumerMu.RLock()
	defer pc.consumerMu.RUnlock()
	return pc.consumer
}

// Info returns the consumer info, using the internal lock to prevent races.
func (pc *partitionConsumer) Info(ctx context.Context) (*jetstream.ConsumerInfo, error) {
	pc.consumerMu.RLock()
	cons := pc.consumer
	pc.consumerMu.RUnlock()

	if cons == nil {
		return nil, errors.New("consumer not initialized")
	}

	pc.consInfoMu.Lock()
	defer pc.consInfoMu.Unlock()

	return cons.Info(ctx)
}

// Drain waits until the server reports no pending acknowledgements or the context times out.
func (pc *partitionConsumer) Drain(ctx context.Context) {
	pc.consumerMu.RLock()
	cons := pc.consumer
	pc.consumerMu.RUnlock()

	if cons == nil {
		return
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Serialize Info calls to avoid underlying client races
			pc.consInfoMu.Lock()
			info, err := cons.Info(ctx)
			pc.consInfoMu.Unlock()
			if err != nil {
				// If info fails, break drain early to avoid blocking removals
				return
			}
			if info.NumAckPending == 0 {
				return
			}
		}
	}
}

func (pc *partitionConsumer) processIterator(
	ctx context.Context,
	iter jetstream.MessagesContext,
	handler MessageHandler,
) (exit bool, iterErr error) {
	stopperCh := pc.startIterStopper(ctx, iter)
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
			return true, nil
		}

		msg, err := iter.Next()
		if err != nil {
			iter.Stop()
			pc.logger.Debug("iterator next error", "subject", pc.subject, "error", err)
			return false, err
		}

		if handler == nil {
			_ = msg.Nak()
			continue
		}

		if pc.config.ManualAck {
			_ = handler.Handle(ctx, msg)
			continue
		}

		if err := handler.Handle(ctx, msg); err != nil {
			_ = msg.Nak()
		} else {
			_ = msg.Ack()
		}
	}
}

func (pc *partitionConsumer) startIterStopper(ctx context.Context, iter jetstream.MessagesContext) chan struct{} {
	stopperCh := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			iter.Stop()
		case <-stopperCh:
			return
		}
	}()

	return stopperCh
}

// maybeEscalateIteratorFailures records an iterator failure timestamp and attempts per-subject
// remediation (recreate/bind the durable) when a burst occurs within the configured window.
func (pc *partitionConsumer) maybeEscalateIteratorFailures(ctx context.Context) {
	pc.iterEscMu.Lock()
	defer pc.iterEscMu.Unlock()

	window := pc.config.IteratorEscalationWindow
	if window <= 0 {
		window = DefaultIteratorEscalationWindow
	}
	threshold := pc.config.IteratorEscalationThreshold
	if threshold <= 0 {
		threshold = DefaultIteratorEscalationThreshold
	}

	now := time.Now()
	pc.iterFailureTimes = append(pc.iterFailureTimes, now)
	cutoff := now.Add(-window)
	i := 0
	for ; i < len(pc.iterFailureTimes); i++ {
		if pc.iterFailureTimes[i].After(cutoff) {
			break
		}
	}
	if i > 0 && i <= len(pc.iterFailureTimes) {
		pc.iterFailureTimes = append([]time.Time(nil), pc.iterFailureTimes[i:]...)
	}
	count := len(pc.iterFailureTimes)
	lastEsc := pc.lastEscalation
	canEscalate := count >= threshold && (lastEsc.IsZero() || now.Sub(lastEsc) >= window)
	if !canEscalate {
		return
	}

	if pc.config.Metrics != nil {
		pc.config.Metrics.IncrementWorkerConsumerIteratorEscalation("burst")
	}

	pc.lastEscalation = now
	pc.logger.Info("iterator escalation triggered",
		"op", "iterator_escalation",
		"subject", pc.subject,
		"count_in_window", count,
		"threshold", threshold,
		"window_ms", window.Milliseconds(),
	)

	// Attempt remediation: check consumer info; if missing or error, recreate/rebind.
	pc.consumerMu.RLock()
	cons := pc.consumer
	pc.consumerMu.RUnlock()

	if cons != nil {
		pc.consInfoMu.Lock()
		_, err := cons.Info(ctx)
		pc.consInfoMu.Unlock()
		if err == nil {
			return
		}
	}

	newCons, err := pc.ensureConsumer(ctx)
	if err != nil {
		pc.logger.Warn("per-subject consumer remediation failed",
			"subject", pc.subject,
			"durable", pc.durableName,
			"error", err,
		)
		return
	}

	// Update loop reference under lock
	pc.consumerMu.Lock()
	pc.consumer = newCons
	pc.consumerMu.Unlock()

	pc.logger.Info("per-subject consumer remediated", "subject", pc.subject, "durable", pc.durableName)
}

// ensureConsumer creates or updates the durable consumer.
func (pc *partitionConsumer) ensureConsumer(ctx context.Context) (jetstream.Consumer, error) {
	var lastErr error
	const maxAttempts = 3

	for i := 0; i < maxAttempts; i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		cons, err := pc.js.CreateOrUpdateConsumer(ctx, pc.streamName, pc.consumerConfig)
		if err == nil {
			return cons, nil
		}

		lastErr = err
		// If stream not found, it's a configuration error, not transient.
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return nil, err
		}

		if i < maxAttempts-1 {
			delay := jitterBackoff(time.Duration(i)*50*time.Millisecond, 50*time.Millisecond, 2.0, 200*time.Millisecond, nil)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
				continue
			}
		}
	}

	return nil, fmt.Errorf("failed after %d attempts: %w", maxAttempts, lastErr)
}

// delayWithBackoffOrExit applies jittered exponential backoff according to config.
func (pc *partitionConsumer) delayWithBackoffOrExit(ctx context.Context, purpose string) bool {
	delay := jitterBackoff(0, pc.config.Retry.Base, pc.config.Retry.Multiplier, pc.config.Retry.Max, nil)
	pc.logger.Debug("backoff", "purpose", purpose, "delay_ms", delay.Milliseconds())
	emitControlRetry(pc.config.Metrics, purpose)
	emitRetryBackoff(pc.config.Metrics, purpose, delay.Seconds())

	select {
	case <-ctx.Done():
		return true
	case <-time.After(delay):
		return false
	}
}
