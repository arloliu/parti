package recovery

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// InfoFunc returns the current consumer info. Used to confirm consumer deletion
// after ambiguous ErrNoHeartbeat signals.
type InfoFunc func(ctx context.Context) (*jetstream.ConsumerInfo, error)

// RecreateFunc recreates the consumer with the given config.
// Each consumer type provides its own implementation (e.g., jsutil.EnsureConsumer or js.CreateOrUpdateConsumer).
type RecreateFunc func(ctx context.Context, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error)

// ControllerConfig configures a Controller.
type ControllerConfig struct {
	// Strategy determines how the consumer is recreated after deletion.
	Strategy Strategy

	// FetchTimeout is the consumer's pull fetch timeout. Used to auto-compute
	// BurstWindow when BurstWindow is zero.
	FetchTimeout time.Duration

	// BurstThreshold is the number of ErrNoHeartbeat failures within BurstWindow
	// before a consumer.Info() confirmation is triggered.
	// If zero, defaults to defaultBurstThreshold (3).
	BurstThreshold int

	// BurstWindow is the sliding window for burst detection.
	// If zero, auto-computed from FetchTimeout: FetchTimeout*(BurstThreshold+1)+3s.
	BurstWindow time.Duration

	Logger  types.Logger
	Metrics types.WorkerConsumerMetrics
}

// defaultRecoveryMinInterval is the minimum time between consecutive recovery
// attempts. This prevents a tight create-delete loop when a consumer is deleted
// immediately after creation (e.g., an external script or an extremely low
// InactiveThreshold). Each recovery attempt is a network call, but without
// this guard the loop can still generate high NATS API load.
const defaultRecoveryMinInterval = 500 * time.Millisecond

// Controller encapsulates all auto-recovery state and logic for a single durable consumer.
// It is composed into consumer types (Queue, Broadcast, partitionConsumer, JSConsumer)
// rather than each implementing recovery independently.
//
// A nil *Controller is safe; all methods are no-ops when the receiver is nil,
// allowing consumer types to hold a nil controller when recovery is disabled.
type Controller struct {
	strategy   Strategy
	checkpoint Checkpoint
	burst      BurstDetector

	mu                  sync.Mutex
	inProgress          atomic.Bool
	consInfoMu          sync.Mutex
	lastRecoveryTime    time.Time
	minRecoveryInterval time.Duration

	logger  types.Logger
	metrics types.WorkerConsumerMetrics
}

// NewController creates a Controller. Returns nil if strategy is RecoveryDisabled,
// so callers can use a nil check to skip recovery entirely.
func NewController(cfg ControllerConfig) *Controller {
	if cfg.Strategy == Disabled {
		return nil
	}

	threshold := cfg.BurstThreshold
	if threshold <= 0 {
		threshold = defaultBurstThreshold
	}
	window := cfg.BurstWindow
	if window <= 0 {
		window = defaultBurstWindow(cfg.FetchTimeout)
	}

	return &Controller{
		strategy:            cfg.Strategy,
		checkpoint:          newCheckpoint(cfg.Logger),
		burst:               NewBurstDetector(window, threshold),
		minRecoveryInterval: defaultRecoveryMinInterval,
		logger:              cfg.Logger,
		metrics:             cfg.Metrics,
	}
}

// Strategy returns the configured recovery strategy.
func (c *Controller) Strategy() Strategy {
	if c == nil {
		return Disabled
	}
	return c.strategy
}

// Classify examines an iterator error and determines the recommended action.
//
// For ErrorConsumerGone (ErrConsumerDeleted), it attempts recovery immediately.
// For ErrorNeedsConfirm (ErrNoHeartbeat), it records in the burst detector and
// confirms via infoFn when the threshold is reached.
//
// baseCfg and recreate are used to perform recovery when needed.
// infoFn is called only when burst threshold is reached.
//
// Returns ActionContinue if recovery succeeded, ActionBackoff if the caller should
// retry with backoff, or ActionExit for graceful shutdown.
func (c *Controller) Classify(
	ctx context.Context,
	err error,
	infoFn InfoFunc,
	baseCfg jetstream.ConsumerConfig,
	recreate RecreateFunc,
) (Action, jetstream.Consumer) {
	if c == nil {
		return ActionBackoff, nil
	}

	class := ClassifyError(err)

	switch class {
	case ErrorGracefulExit:
		return ActionExit, nil

	case ErrorConsumerGone:
		c.emitIteratorRestart("consumer_deleted")
		newCons, ok := c.recover(ctx, "consumer_deleted", baseCfg, recreate)
		if ok {
			return ActionContinue, newCons
		}
		return ActionBackoff, nil

	case ErrorNeedsConfirm:
		c.emitIteratorRestart("heartbeat")
		if !c.burst.Record() {
			return ActionBackoff, nil // not enough failures yet
		}
		// Burst threshold reached — confirm via consumer.Info().
		if !c.confirmConsumerGone(ctx, infoFn) {
			return ActionBackoff, nil // consumer still exists, transient issue
		}
		newCons, ok := c.recover(ctx, "consumer_not_found_after_burst", baseCfg, recreate)
		if ok {
			return ActionContinue, newCons
		}

		return ActionBackoff, nil

	case ErrorTransient:
		c.emitIteratorRestart("transient")
		return ActionBackoff, nil
	}

	return ActionBackoff, nil
}

// WrapForTracking returns a jetstream.Msg that intercepts Ack/DoubleAck to advance
// the checkpoint when the strategy is FromLastProcessed.
// For all other strategies or a nil controller it returns msg unchanged (no allocation).
func (c *Controller) WrapForTracking(msg jetstream.Msg) jetstream.Msg {
	if c == nil || c.strategy != FromLastProcessed {
		return msg
	}

	return &trackingMsg{Msg: msg, controller: c}
}

// Dispatch delivers msg to handle with the recovery-aware dispatch policy:
//   - ManualAck=true:  msg is passed through WrapForTracking; Ack/DoubleAck
//     calls inside the handler advance the checkpoint automatically.
//   - ManualAck=false: handle is called, then msg is Acked or Nacked;
//     the checkpoint advances on a successful Ack.
//
// Dispatch is safe to call on a nil *Controller.
func (c *Controller) Dispatch(ctx context.Context, msg jetstream.Msg, manualAck bool, handle func(context.Context, jetstream.Msg) error) {
	if manualAck {
		_ = handle(ctx, c.WrapForTracking(msg))
		return
	}
	if err := handle(ctx, msg); err != nil {
		_ = msg.Nak()
	} else if err := msg.Ack(); err == nil {
		c.AdvanceCheckpoint(msg)
	}
}

// AdvanceCheckpoint should be called after a successful helper-owned msg.Ack()
// when ManualAck is false. It monotonically advances the checkpoint.
func (c *Controller) AdvanceCheckpoint(msg jetstream.Msg) {
	if c == nil {
		return
	}
	c.checkpoint.Advance(msg)
}

// SeedCheckpoint reads the ack floor from consumer info and seeds the checkpoint.
// Called once at startup after binding and after each successful recovery.
// A failure is non-fatal: the checkpoint stays at its current value and
// BuildConfig will fall back to RecoverFromNew if checkpoint is zero.
func (c *Controller) SeedCheckpoint(ctx context.Context, infoFn InfoFunc) {
	if c == nil || c.strategy != FromLastProcessed {
		return
	}

	info, err := c.callInfo(ctx, infoFn)
	if err != nil {
		c.logger.Warn("failed to seed recovery checkpoint from consumer info", "error", err)
		return
	}

	if seq := info.AckFloor.Stream; seq > 0 {
		c.checkpoint.Seed(seq)
		c.logger.Debug("recovery checkpoint seeded", "ack_floor_seq", seq)
	}
}

// ResetBurst clears the burst detector state. Called after successful recovery.
func (c *Controller) ResetBurst() {
	if c == nil {
		return
	}
	c.burst.Reset()
}

// recover serializes recovery attempts, builds recovery config, calls recreate,
// and re-seeds the checkpoint. Returns the new consumer and true on success.
func (c *Controller) recover(
	ctx context.Context,
	reason string,
	baseCfg jetstream.ConsumerConfig,
	recreate RecreateFunc,
) (jetstream.Consumer, bool) {
	if !c.inProgress.CompareAndSwap(false, true) {
		return nil, false // another recovery in flight
	}
	defer c.inProgress.Store(false)

	c.mu.Lock()
	defer c.mu.Unlock()

	if ctx.Err() != nil {
		return nil, false
	}

	// Rate-limit consecutive recoveries to prevent a tight create-delete loop
	// when the consumer is deleted immediately after recreation (e.g., adversarial
	// admin script or extremely low InactiveThreshold).
	now := time.Now()
	if !c.lastRecoveryTime.IsZero() && now.Sub(c.lastRecoveryTime) < c.minRecoveryInterval {
		c.logger.Debug("recovery cooldown in effect, skipping",
			"reason", reason,
			"elapsed_ms", now.Sub(c.lastRecoveryTime).Milliseconds(),
			"min_interval_ms", c.minRecoveryInterval.Milliseconds(),
		)

		return nil, false
	}

	attempt := beginAttempt(c.metrics, reason)
	success := false
	defer func() { attempt.finish(success) }()

	checkpoint := c.checkpoint.Value()
	recoverCfg, fallback := BuildConfig(baseCfg, c.strategy, checkpoint)

	if fallback != "" {
		c.logger.Warn("recovery used fallback strategy",
			"reason", reason,
			"fallback", fallback,
		)
	}

	c.logger.Info("recovering consumer",
		"op", "consumer_recovery",
		"strategy", c.strategy,
		"reason", reason,
		"checkpoint", checkpoint,
	)

	if ctx.Err() != nil {
		return nil, false
	}

	newCons, err := recreate(ctx, recoverCfg)
	if err != nil {
		c.logger.Warn("consumer recovery failed",
			"op", "consumer_recovery",
			"reason", reason,
			"error", err,
		)
		return nil, false
	}

	c.burst.Reset()
	c.lastRecoveryTime = time.Now()

	c.logger.Info("consumer recovered",
		"op", "consumer_recovery",
		"strategy", c.strategy,
		"reason", reason,
	)

	success = true

	return newCons, true
}

// confirmConsumerGone calls consumer.Info() and returns true if the consumer is confirmed gone.
func (c *Controller) confirmConsumerGone(ctx context.Context, infoFn InfoFunc) bool {
	if infoFn == nil {
		return false
	}

	c.consInfoMu.Lock()
	_, err := infoFn(ctx)
	c.consInfoMu.Unlock()

	return natsutil.IsConsumerNotFound(err)
}

// callInfo calls consumer.Info() with serialization.
func (c *Controller) callInfo(ctx context.Context, infoFn InfoFunc) (*jetstream.ConsumerInfo, error) {
	if infoFn == nil {
		return nil, errors.New("no info function provided")
	}

	c.consInfoMu.Lock()
	defer c.consInfoMu.Unlock()
	return infoFn(ctx)
}

func (c *Controller) emitIteratorRestart(reason string) {
	if c.metrics != nil {
		c.metrics.IncrementWorkerConsumerIteratorRestart(reason)
	}
}

// --- metrics helpers (subsume recoveryutil) ---

type attempt struct {
	metrics types.WorkerConsumerMetrics
	reason  string
	started time.Time
}

func beginAttempt(metrics types.WorkerConsumerMetrics, reason string) attempt {
	a := attempt{
		metrics: metrics,
		reason:  metricReason(reason),
		started: time.Now(),
	}
	if metrics != nil {
		metrics.IncrementWorkerConsumerRecreationAttempt(a.reason)
	}

	return a
}

func (a attempt) finish(success bool) {
	if a.metrics == nil {
		return
	}
	result := "failure"
	if success {
		result = "success"
	}
	a.metrics.RecordWorkerConsumerRecreation(result, a.reason)
	a.metrics.ObserveWorkerConsumerRecreationDuration(time.Since(a.started).Seconds())
}

func metricReason(reason string) string {
	switch reason {
	case "consumer_deleted":
		return "iterator_error"
	case "consumer_not_found_after_burst":
		return "not_found"
	default:
		return "unknown"
	}
}
