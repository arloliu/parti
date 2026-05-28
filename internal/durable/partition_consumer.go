package durable

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/internal/recovery"
	"github.com/arloliu/parti/v2/internal/retry"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// partitionConsumerConfig configures a single partition consumer.
type partitionConsumerConfig struct {
	// Pull settings
	BatchSize    int
	FetchTimeout time.Duration
	ManualAck    bool

	// Recovery
	RecoveryStrategy RecoveryStrategy

	// Retry/Escalation
	Retry                       RetryConfig
	IteratorEscalationWindow    time.Duration
	IteratorEscalationThreshold int

	// RecoveryRetry configures the bounded-retry envelope wrapped around
	// the iterator-creation retry loop. See RecoveryRetryConfig.
	RecoveryRetry RecoveryRetryConfig

	// OnPermanentFailure is fired exactly once when the iterator-creation
	// envelope exhausts its attempt budget. See WorkerConsumerConfig.
	OnPermanentFailure func(subject string, err error)

	// OnUnservable, when non-nil, is fired (rate-limited, non-terminal) while
	// this partition's consumer exists but its raft group is unavailable past
	// UnservableWindow — a condition parti cannot fix (operator must recover
	// NATS). The consume loop keeps retrying so recovery is automatic on restore.
	OnUnservable func(subject string, err error)

	// UnservableWindow is how long the unservable condition must persist before
	// OnUnservable fires. Zero uses the recovery default (10s).
	UnservableWindow time.Duration

	// StreamMissingHook is the operator-supplied escalation invoked when
	// the partition consumer's recovery flow detects the underlying
	// JetStream stream is absent. Nil means "no hook configured" — the
	// detour logs and surfaces the loss via the F2 envelope's exhaustion
	// path. See WorkerConsumerConfig.StreamMissingHook for the full
	// operator contract.
	StreamMissingHook types.StreamMissingHook

	// OnStreamRecreated is invoked after a successful post-hook
	// HandleStreamRecreated. Used by the consumer.Dynamic wiring layer
	// to reset its WorkQueue compat cache so the next Update re-runs
	// the check against the fresh stream identity.
	OnStreamRecreated func()

	// Metrics
	Metrics types.WorkerConsumerMetrics
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

	// streamMissingFailures is the consecutive Site B stream-missing detour
	// failure count (hook absent, hook returned error, or post-hook rebuild
	// failed). The Site A path is already bounded by the F2 envelope;
	// Site B (iter-runtime classification via Next() → ErrConsumerDeleted)
	// lives outside the iter-creation envelope, because nats.go's
	// Consumer.Messages() does not validate the remote stream up front
	// (it returns a MessagesContext eagerly and only surfaces consumer-gone
	// later via Next()), so a fresh outer-loop envelope construction would
	// succeed at iter creation, hit the same Next() error, and re-enter
	// Site B forever. This counter bounds that loop: it increments on every
	// Site B detour failure, is reset to zero on success, and triggers
	// OnPermanentFailure + loop exit when it reaches RecoveryRetry.MaxAttempts.
	streamMissingFailures atomic.Int32

	// Info lock
	consInfoMu sync.Mutex

	// Recovery
	recovery *recovery.Controller
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
	// Apply recovery-envelope defaults once at construction so callers
	// (including in-package tests) that omit RecoveryRetry don't trip
	// retry.New's required-field panics on every outer-loop iteration.
	if config.RecoveryRetry.MaxAttempts <= 0 {
		config.RecoveryRetry.MaxAttempts = DefaultRecoveryMaxAttempts
	}
	if config.RecoveryRetry.BaseBackoff <= 0 {
		config.RecoveryRetry.BaseBackoff = DefaultRecoveryBaseBackoff
	}
	if config.RecoveryRetry.MaxBackoff < config.RecoveryRetry.BaseBackoff {
		config.RecoveryRetry.MaxBackoff = config.RecoveryRetry.BaseBackoff
	}

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
		recovery: recovery.NewController(recovery.ControllerConfig{
			Strategy:         config.RecoveryStrategy,
			FetchTimeout:     config.FetchTimeout,
			Subject:          opts.subject,
			UnservableWindow: config.UnservableWindow,
			OnUnservable:     config.OnUnservable,
			Logger:           logger,
			Metrics:          config.Metrics,
		}),
	}
}

// Run starts the consumption loop. It blocks until the context is canceled.
func (pc *partitionConsumer) Run(ctx context.Context, handler messageHandler) {
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

	// Seed the recovery checkpoint from the current server-side ack floor so that
	// a fresh process binding to an existing durable doesn't replay from zero.
	pc.recovery.SeedCheckpoint(ctx, pc.consumerInfoFn())

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

		// Iterator creation runs under a bounded-retry envelope; ErrExhausted
		// (after OnPermanent fired) and ctx-cancel both terminate the loop.
		// See RecoveryRetryConfig for the per-episode budget-reset semantics.
		iter, err := pc.runIteratorEnvelope(ctx, cons, batch, expiry)
		if err != nil {
			return
		}

		exit, iterErr := pc.processIterator(ctx, iter, handler)
		if exit {
			return
		}
		if iterErr != nil && pc.handleIteratorFailure(ctx, iterErr) {
			return
		}
		// loop continues to recreate iterator on next iteration
	}
}

// runIteratorEnvelope drives one bounded-retry envelope around iterator
// creation. Returns (iter, nil) on success, (nil, ErrExhausted) once
// OnPermanentFailure has fired, or (nil, ctx.Err()) on cancellation.
// Mirrors source/nats_kv.go:restartWatcher (P2.4a).
func (pc *partitionConsumer) runIteratorEnvelope(
	ctx context.Context,
	cons jetstream.Consumer,
	batch int,
	expiry time.Duration,
) (jetstream.MessagesContext, error) {
	cfg := pc.config.RecoveryRetry

	var iter jetstream.MessagesContext
	env := retry.New(retry.Config{
		Work: func(workCtx context.Context) error {
			// Re-read pc.consumer at the start of every attempt so a
			// mid-envelope Site A rebuild (which stores a fresh consumer
			// under consumerMu) is visible to subsequent attempts in the
			// same envelope episode. Closing over the outer `cons` would
			// pin retries to the pre-rebuild consumer handle.
			pc.consumerMu.RLock()
			attemptCons := pc.consumer
			pc.consumerMu.RUnlock()
			if attemptCons == nil {
				attemptCons = cons
			}

			pc.consInfoMu.Lock()
			i, err := pc.iterFactory(attemptCons, batch, expiry)
			pc.consInfoMu.Unlock()
			if err == nil {
				iter = i
				return nil
			}

			pc.logger.Warn("iterator creation failed", "subject", pc.subject, "error", err)
			if pc.config.Metrics != nil {
				pc.config.Metrics.IncrementWorkerConsumerIteratorRestart("transient")
			}
			// Legacy per-subject escalation runs inside the envelope regardless
			// of recovery strategy (creation failures are distinct from Next()
			// failures and don't go through recovery.Classify). It surfaces a
			// non-nil stream-not-found error only when ensureConsumer failed —
			// the Site A signal that the underlying stream is gone.
			escErr := pc.maybeEscalateIteratorFailures(workCtx)
			if escErr != nil && natsutil.IsStreamNotFound(escErr) {
				newIter, detourErr := pc.runStreamMissingDetour(workCtx, batch, expiry)
				if detourErr != nil {
					// Counts as the current attempt; the envelope keeps
					// retrying until MaxAttempts, then OnPermanentFailure
					// fires with the wrapped types.ErrStreamMissing.
					return detourErr
				}
				iter = newIter

				return nil
			}

			return err
		},
		OnPermanent: func(err error) {
			pc.logger.Warn("partition consumer iterator-creation budget exhausted; entering permanent failure",
				"op", "partition_consumer_permanent_failure",
				"subject", pc.subject,
				"durable", pc.durableName,
				"max_attempts", cfg.MaxAttempts,
				"error", err)
			if pc.config.Metrics != nil {
				pc.config.Metrics.IncrementWorkerConsumerIteratorRestart("recovery_exhausted")
			}
			if pc.config.OnPermanentFailure != nil {
				pc.config.OnPermanentFailure(pc.subject, err)
			}
		},
		OnProgress: func(attempt int, err error) {
			pc.logger.Debug("partition consumer iterator-creation retry",
				"subject", pc.subject,
				"attempt", attempt,
				"max_attempts", cfg.MaxAttempts,
				"error", err)
		},
		BaseBackoff: cfg.BaseBackoff,
		MaxBackoff:  cfg.MaxBackoff,
		MaxAttempts: cfg.MaxAttempts,
		Jitter:      cfg.Jitter,
	})
	if err := env.Run(ctx); err != nil {
		return nil, err
	}

	return iter, nil
}

func (pc *partitionConsumer) handleIteratorFailure(ctx context.Context, iterErr error) bool {
	pc.logger.Warn("iterator error", "subject", pc.subject, "error", iterErr)

	// When recovery is disabled, use the pre-existing escalation mechanism.
	if pc.recovery == nil {
		if pc.config.Metrics != nil {
			if errors.Is(iterErr, jetstream.ErrNoHeartbeat) {
				pc.config.Metrics.IncrementWorkerConsumerIteratorRestart("heartbeat")
			} else {
				pc.config.Metrics.IncrementWorkerConsumerIteratorRestart("transient")
			}
		}
		// Recovery is disabled, so HandleStreamRecreated / rebuild
		// machinery is unavailable. The hook would also have been
		// rejected at construction time by the strategy validator, so
		// there is nothing to "recreate". But a stream-not-found signal
		// from the escalation IS the spec's Site B exhaustion trigger;
		// routing it through handleStreamMissingFailure bounds the
		// otherwise-infinite retry loop using the same
		// RecoveryRetry.MaxAttempts cap as the recovery-enabled path.
		// (v2 P0-A fix.)
		if escErr := pc.maybeEscalateIteratorFailures(ctx); escErr != nil && natsutil.IsStreamNotFound(escErr) {
			wrapped := fmt.Errorf("%w: stream %q: %w", types.ErrStreamMissing, pc.streamName, escErr)
			return pc.handleStreamMissingFailure(ctx, iterErr, wrapped)
		}

		return pc.delayWithBackoffOrExit(ctx, "iterate")
	}

	action, newCons, classifyErr := pc.recovery.Classify(ctx, iterErr, pc.consumerInfoFn(), pc.consumerConfig, pc.recreateFn())
	switch action {
	case recovery.ActionExit:
		return true
	case recovery.ActionStreamMissing:
		// Site B detour: classifyErr already wraps types.ErrStreamMissing.
		// The shared rebuild swaps pc.consumer with one built via the
		// recreated-flag override (DeliverAllPolicy + reset checkpoint).
		// Failures are bounded by handleStreamMissingFailure (see field
		// comment on streamMissingFailures): consecutive failures up to
		// RecoveryRetry.MaxAttempts back off, then OnPermanentFailure
		// fires once and the loop exits.
		if _, err := pc.rebuildAfterStreamMissing(ctx); err != nil {
			return pc.handleStreamMissingFailure(ctx, classifyErr, err)
		}

		return false // outer loop iterates → fresh envelope uses the rebuilt pc.consumer
	case recovery.ActionContinue:
		pc.consumerMu.Lock()
		pc.consumer = newCons
		pc.consumerMu.Unlock()
		// Reset escalation counters so prior burst failures don't bleed into the new consumer.
		pc.iterEscMu.Lock()
		pc.iterFailureTimes = pc.iterFailureTimes[:0]
		pc.lastEscalation = time.Time{}
		pc.iterEscMu.Unlock()
		// Reset the Site B stream-missing failure counter on any successful
		// normal recovery so a prior partial-failure burst from a now-resolved
		// stream-missing episode doesn't shorten the next episode's budget
		// below RecoveryRetry.MaxAttempts. (v2 P0-B fix — the
		// RecoveryRetryConfig per-episode reset contract.)
		pc.streamMissingFailures.Store(0)
		// SeedCheckpoint on a freshly created consumer is typically a no-op:
		// the new consumer's ack floor is 0 and the checkpoint monotonically
		// advances, so it will not regress. Called here as a best-effort update
		// in case the consumer was recreated over an existing durable.
		pc.recovery.SeedCheckpoint(ctx, pc.consumerInfoFn())

		return false
	case recovery.ActionBackoff:
		return pc.delayWithBackoffOrExit(ctx, "iterate")
	}

	return pc.delayWithBackoffOrExit(ctx, "iterate")
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
	handler messageHandler,
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
			pc.logger.Debug("iterator next error", "subject", pc.subject, "error", err)
			iter.Stop()
			if errors.Is(err, jetstream.ErrMsgIteratorClosed) || errors.Is(err, context.Canceled) {
				return false, nil // graceful shutdown, not an iterator error
			}
			return false, err
		}

		// A delivered message proves the consumer is serviceable again; clear any
		// in-progress unservable episode (emits the recovered log if one fired).
		pc.recovery.NoteProgress()

		if handler == nil {
			_ = msg.Nak()
			continue
		}

		pc.recovery.Dispatch(ctx, msg, pc.config.ManualAck, handler.Handle)
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

// maybeEscalateIteratorFailures records an iterator failure timestamp and
// attempts per-subject remediation (recreate/bind the durable) when a burst
// occurs within the configured window.
//
// Returns a non-nil error only when remediation surfaced a stream-not-found
// classification from the underlying `ensureConsumer` call — this is the
// signal the iterator-creation envelope (Site A) uses to enter the
// stream-missing detour. All other remediation outcomes — no escalation,
// healthy consumer info, transient ensureConsumer error, or successful
// rebind — return nil; the original iterator-creation error is what the
// envelope counts against its budget.
func (pc *partitionConsumer) maybeEscalateIteratorFailures(ctx context.Context) error {
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
	pc.iterFailureTimes = recovery.TrimTimes(pc.iterFailureTimes, now.Add(-window))
	count := len(pc.iterFailureTimes)
	lastEsc := pc.lastEscalation
	canEscalate := count >= threshold && (lastEsc.IsZero() || now.Sub(lastEsc) >= window)
	if !canEscalate {
		return nil
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
			// Consumer is healthy — any prior Site B stream-missing
			// failure count (incremented by handleStreamMissingFailure
			// in either recovery-enabled or nil-recovery branches) is
			// no longer load-bearing, so a later episode receives the
			// full RecoveryRetry.MaxAttempts budget. (v3 P1 fix.)
			pc.streamMissingFailures.Store(0)
			return nil
		}
	}

	newCons, err := pc.ensureConsumer(ctx)
	if err != nil {
		pc.logger.Warn("per-subject consumer remediation failed",
			"subject", pc.subject,
			"durable", pc.durableName,
			"error", err,
		)
		// Stream-not-found is the ONLY remediation error the Site A
		// caller must learn about — other failures are transient and
		// the envelope's per-attempt budget already accounts for them
		// via the original iterFactory error.
		if natsutil.IsStreamNotFound(err) {
			return err
		}

		return nil
	}

	// Update loop reference under lock
	pc.consumerMu.Lock()
	pc.consumer = newCons
	pc.consumerMu.Unlock()

	// Legacy remediation succeeded — same per-episode reset contract
	// applies as in the Info-healthy branch above. (v3 P1 fix.)
	pc.streamMissingFailures.Store(0)

	pc.logger.Info("per-subject consumer remediated", "subject", pc.subject, "durable", pc.durableName)

	return nil
}

// ensureConsumer creates or updates the durable consumer.
func (pc *partitionConsumer) ensureConsumer(ctx context.Context) (jetstream.Consumer, error) {
	var lastErr error
	const maxAttempts = 3

	for i := range maxAttempts {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		cons, err := pc.js.CreateOrUpdateConsumer(ctx, pc.streamName, pc.consumerConfig)
		if err == nil {
			return cons, nil
		}

		lastErr = err
		// If stream not found, it's a configuration error, not transient.
		if natsutil.IsStreamNotFound(err) {
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
func (pc *partitionConsumer) delayWithBackoffOrExit(ctx context.Context, purpose string) bool { //nolint:unparam // purpose is used for logging/metrics; all sites currently pass "iterate" but may diverge.
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

// consumerInfoFn returns an InfoFunc that reads consumer info under appropriate locks.
func (pc *partitionConsumer) consumerInfoFn() recovery.InfoFunc {
	return func(ctx context.Context) (*jetstream.ConsumerInfo, error) {
		pc.consumerMu.RLock()
		cons := pc.consumer
		pc.consumerMu.RUnlock()
		if cons == nil {
			return nil, errors.New("no consumer")
		}
		pc.consInfoMu.Lock()
		defer pc.consInfoMu.Unlock()

		return cons.Info(ctx)
	}
}

// recreateFn returns a RecreateFunc that creates or updates the consumer on the stream.
func (pc *partitionConsumer) recreateFn() recovery.RecreateFunc {
	return func(ctx context.Context, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {
		return pc.js.CreateOrUpdateConsumer(ctx, pc.streamName, cfg)
	}
}

// rebuildAfterStreamMissing runs the shared post-detection sequence used by
// both Site A (iter-creation envelope) and Site B (iter-runtime failure):
// invoke the StreamMissingHook, drive recovery.Controller.RebuildAfterStreamRecreated,
// swap pc.consumer under consumerMu, and reset the per-subject iterator-
// escalation counters so the legacy escalation doesn't fire spuriously on
// the freshly-rebuilt consumer.
//
// Returns the rebuilt consumer on success, or an error wrapped with
// types.ErrStreamMissing on hook absence / hook failure / rebuild failure.
// The Site A caller follows up with iterFactory; the Site B caller returns
// to the outer loop which constructs the next envelope.
func (pc *partitionConsumer) rebuildAfterStreamMissing(ctx context.Context) (jetstream.Consumer, error) {
	if err := pc.handleStreamMissing(ctx); err != nil {
		return nil, err
	}

	// RebuildAfterStreamRecreated already wraps the underlying cause
	// with types.ErrStreamMissing (covering both still-missing-stream
	// and incompatible-restored-consumer-config cases) so the manager
	// observer route fires consistently on exhaustion.
	newCons, err := pc.recovery.RebuildAfterStreamRecreated(ctx, pc.consumerConfig, pc.recreateFn())
	if err != nil {
		return nil, err
	}

	pc.consumerMu.Lock()
	pc.consumer = newCons
	pc.consumerMu.Unlock()

	pc.iterEscMu.Lock()
	pc.iterFailureTimes = pc.iterFailureTimes[:0]
	pc.lastEscalation = time.Time{}
	pc.iterEscMu.Unlock()

	// Reset the Site B failure counter on any successful rebuild — once a
	// fresh consumer is bound, prior detour failures are no longer
	// load-bearing toward exhaustion. Site A's failure path counts via
	// the F2 envelope and does not consume this counter; resetting here
	// is still correct for Site A because a successful rebuild proves the
	// stream is back regardless of which detour drove it.
	pc.streamMissingFailures.Store(0)

	return newCons, nil
}

// handleStreamMissingFailure is the Site B failure-handler. Site B's
// detour failures (hook absent, hook returned error, post-hook rebuild
// failed) are bounded by RecoveryRetry.MaxAttempts because the outer-loop
// envelope cannot catch them — see streamMissingFailures field doc for the
// nats.go Messages() semantics that make Site B failures invisible to the
// F2 envelope's iter-creation counter.
//
// Returns true to exit the consumer loop when exhaustion fires
// OnPermanentFailure; otherwise backs off and returns false so the outer
// loop iterates and re-enters Site B for the next attempt. The cause
// passed to OnPermanentFailure is wrapped with types.ErrStreamMissing
// (rebuildAfterStreamMissing's contract preserves the wrap), so the
// downstream manager observer route fires consistently.
func (pc *partitionConsumer) handleStreamMissingFailure(ctx context.Context, classifyErr, detourErr error) bool {
	count := pc.streamMissingFailures.Add(1)
	maxAttempts := pc.config.RecoveryRetry.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultRecoveryMaxAttempts
	}

	pc.logger.Warn("stream-missing detour failed on iter-runtime path",
		"op", "stream_missing_site_b_failed",
		"subject", pc.subject,
		"stream", pc.streamName,
		"attempt", count,
		"max_attempts", maxAttempts,
		"classify_error", classifyErr,
		"detour_error", detourErr,
	)

	if int(count) >= maxAttempts {
		pc.logger.Warn("stream-missing detour exhausted; entering permanent failure",
			"op", "stream_missing_site_b_exhausted",
			"subject", pc.subject,
			"stream", pc.streamName,
			"attempts", count,
			"error", detourErr,
		)
		if pc.config.Metrics != nil {
			pc.config.Metrics.IncrementWorkerConsumerIteratorRestart("stream_missing_exhausted")
		}
		if pc.config.OnPermanentFailure != nil {
			pc.config.OnPermanentFailure(pc.subject, detourErr)
		}

		return true // exit the consumer loop
	}

	return pc.delayWithBackoffOrExit(ctx, "iterate")
}

// runStreamMissingDetour is the Site A wrapper around rebuildAfterStreamMissing:
// after the shared rebuild succeeds, it creates a fresh iterator from the new
// consumer so the envelope's Work attempt can return success with a usable iter.
//
// A nil error means pc.consumer has been swapped AND the returned iter is
// pinned for the envelope to drain. Any non-nil error counts against the
// envelope's attempt budget; exhaustion routes through OnPermanentFailure
// to the manager observer. Post-rebuild iter-creation flakiness retries on
// the next attempt — Work re-reads pc.consumer (now = newCons) at the top.
func (pc *partitionConsumer) runStreamMissingDetour(
	ctx context.Context,
	batch int,
	expiry time.Duration,
) (jetstream.MessagesContext, error) {
	newCons, err := pc.rebuildAfterStreamMissing(ctx)
	if err != nil {
		return nil, err
	}

	pc.consInfoMu.Lock()
	newIter, iterErr := pc.iterFactory(newCons, batch, expiry)
	pc.consInfoMu.Unlock()
	if iterErr != nil {
		return nil, iterErr
	}

	return newIter, nil
}

// handleStreamMissing invokes the operator-supplied StreamMissingHook and,
// on success, drives the recovery controller's post-hook reset sequence
// (HandleStreamRecreated + the OnStreamRecreated callback). The reset runs
// BEFORE the upstream callback so the compat-check cache resets only after
// the controller has bumped the epoch — preventing a racing Update from
// caching a stale "compatible" result keyed against the previous stream.
//
// Returns nil when the hook returned nil; the caller's detour is then
// expected to call RebuildAfterStreamRecreated. Returns a wrapped
// types.ErrStreamMissing when the hook is absent or returned an error.
func (pc *partitionConsumer) handleStreamMissing(ctx context.Context) error {
	hook := pc.config.StreamMissingHook
	if hook == nil {
		pc.logger.Warn("stream-missing detected; no hook configured",
			"op", "stream_missing_no_hook",
			"subject", pc.subject,
			"stream", pc.streamName,
		)
		if pc.config.Metrics != nil {
			pc.config.Metrics.IncrementWorkerConsumerIteratorRestart("stream_missing_no_hook")
		}

		return fmt.Errorf("%w: stream %q", types.ErrStreamMissing, pc.streamName)
	}

	pc.logger.Info("invoking stream-missing hook",
		"op", "stream_missing_hook_invoke",
		"subject", pc.subject,
		"stream", pc.streamName,
	)
	hookErr := hook(pc.streamName)
	if hookErr != nil {
		pc.logger.Warn("stream-missing hook returned error",
			"op", "stream_missing_hook_error",
			"subject", pc.subject,
			"stream", pc.streamName,
			"error", hookErr,
		)
		if pc.config.Metrics != nil {
			pc.config.Metrics.IncrementWorkerConsumerIteratorRestart("stream_recreate_error")
		}

		return fmt.Errorf("%w: stream %q: %w", types.ErrStreamMissing, pc.streamName, hookErr)
	}

	// Hook succeeded — drive the post-hook reset (controller first, then
	// the upstream OnStreamRecreated callback; see docstring for ordering rationale).
	pc.recovery.HandleStreamRecreated(ctx, pc.consumerInfoFn())
	if pc.config.OnStreamRecreated != nil {
		pc.config.OnStreamRecreated()
	}
	if pc.config.Metrics != nil {
		pc.config.Metrics.IncrementWorkerConsumerIteratorRestart("stream_recreate_success")
	}

	return nil
}
