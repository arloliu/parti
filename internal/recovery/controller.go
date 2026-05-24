package recovery

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// streamRecreatedStep* are the ordered step names HandleStreamRecreated
// reports to the test seam. Exposed as package-level constants so both
// the production code and the ordering test reference the same string,
// preventing a silent typo-desync.
const (
	streamRecreatedStepAfterBump  = "after_bump"
	streamRecreatedStepAfterReset = "after_reset"
	streamRecreatedStepAfterSeed  = "after_seed"
	streamRecreatedStepAfterFlag  = "after_flag"
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

	// streamEpoch is bumped by HandleStreamRecreated each time the
	// underlying stream is recreated. WrapForTracking captures the
	// current epoch at dispatch time; AdvanceCheckpoint compares the
	// captured epoch against the current one and silently drops the
	// advance when they differ — fencing late acks from a prior
	// stream incarnation that would otherwise re-raise the just-reset
	// checkpoint.
	streamEpoch atomic.Uint64

	// recreatedSinceLastBuild is set true by HandleStreamRecreated
	// and cleared by RebuildAfterStreamRecreated's first call (via
	// atomic.Bool.Swap). When true, BuildConfig forces DeliverAllPolicy
	// for the first post-hook consumer build so a fresh stream replays
	// from sequence 1 rather than skipping pre-bind messages.
	recreatedSinceLastBuild atomic.Bool

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
		window = defaultBurstWindow(cfg.FetchTimeout, threshold)
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
// Returns:
//   - (ActionContinue, newCons, nil) if recovery succeeded and the caller
//     should adopt newCons and reset backoff state.
//   - (ActionBackoff, nil, nil) if no recovery was needed or recovery
//     failed transiently; the caller should backoff and retry.
//   - (ActionExit, nil, nil) for graceful shutdown.
//   - (ActionStreamMissing, nil, err) when recovery determined that the
//     underlying JetStream stream is absent. The returned error wraps
//     types.ErrStreamMissing so callers can route via errors.Is. No
//     internal recovery state advances on this path.
func (c *Controller) Classify(
	ctx context.Context,
	err error,
	infoFn InfoFunc,
	baseCfg jetstream.ConsumerConfig,
	recreate RecreateFunc,
) (Action, jetstream.Consumer, error) {
	if c == nil {
		return ActionBackoff, nil, nil
	}

	class := ClassifyError(err)

	switch class {
	case ErrorGracefulExit:
		return ActionExit, nil, nil

	case ErrorConsumerGone:
		c.emitIteratorRestart("consumer_deleted")
		newCons, recErr := c.recover(ctx, "consumer_deleted", baseCfg, recreate)
		if recErr != nil {
			return ActionStreamMissing, nil, recErr
		}
		if newCons != nil {
			return ActionContinue, newCons, nil
		}

		return ActionBackoff, nil, nil

	case ErrorNeedsConfirm:
		c.emitIteratorRestart("heartbeat")
		if !c.burst.Record() {
			return ActionBackoff, nil, nil // not enough failures yet
		}
		// Burst threshold reached — confirm via consumer.Info().
		if !c.confirmConsumerGone(ctx, infoFn) {
			return ActionBackoff, nil, nil // consumer still exists, transient issue
		}
		newCons, recErr := c.recover(ctx, "consumer_not_found_after_burst", baseCfg, recreate)
		if recErr != nil {
			return ActionStreamMissing, nil, recErr
		}
		if newCons != nil {
			return ActionContinue, newCons, nil
		}

		return ActionBackoff, nil, nil

	case ErrorTransient:
		c.emitIteratorRestart("transient")
		return ActionBackoff, nil, nil
	}

	return ActionBackoff, nil, nil
}

// WrapForTracking returns a jetstream.Msg that intercepts Ack/DoubleAck to advance
// the checkpoint when the strategy is FromLastProcessed. The wrapper captures
// the controller's current streamEpoch at dispatch time so a late ack landing
// after HandleStreamRecreated can be fenced.
// For all other strategies or a nil controller it returns msg unchanged (no allocation).
func (c *Controller) WrapForTracking(msg jetstream.Msg) jetstream.Msg {
	if c == nil || c.strategy != FromLastProcessed {
		return msg
	}

	return &trackingMsg{
		Msg:        msg,
		controller: c,
		epoch:      c.streamEpoch.Load(),
	}
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
	// Capture the dispatch-time epoch (not the ack-time epoch) so the
	// non-manual-ack path obeys the same fence invariant as the wrapped
	// path: AdvanceCheckpoint silently drops the advance if the stream
	// was recreated between dispatch and ack.
	var epoch uint64
	if c != nil {
		epoch = c.streamEpoch.Load()
	}
	if err := handle(ctx, msg); err != nil {
		_ = msg.Nak()
	} else if err := msg.Ack(); err == nil {
		c.AdvanceCheckpoint(msg, epoch)
	}
}

// AdvanceCheckpoint should be called after a successful helper-owned msg.Ack()
// when ManualAck is false. The epoch parameter is the streamEpoch captured at
// message dispatch time (not ack time); if the controller's current streamEpoch
// has advanced, the call is a silent no-op — the ack is from a prior stream
// incarnation and must not re-raise the checkpoint after a HandleStreamRecreated
// reset.
func (c *Controller) AdvanceCheckpoint(msg jetstream.Msg, epoch uint64) {
	if c == nil {
		return
	}
	if epoch != c.streamEpoch.Load() {
		return // late ack from a prior stream generation; fence drops it
	}
	c.checkpoint.Advance(msg)
}

// CurrentEpoch returns the controller's current streamEpoch. Callers that
// dispatch and ack synchronously from the same goroutine (or that run on a
// consumer type where HandleStreamRecreated is never called) may use this
// to load the epoch immediately before AdvanceCheckpoint. The Dynamic
// partition consumer must capture epoch at dispatch time instead (see
// WrapForTracking and Dispatch).
func (c *Controller) CurrentEpoch() uint64 {
	if c == nil {
		return 0
	}
	return c.streamEpoch.Load()
}

// HandleStreamRecreated is called by the partition consumer's recovery
// detour after the operator-supplied StreamMissingHook returns nil.
// It performs the following ordered steps; reversing the bump↔reset
// opens a race where an old-epoch ack landing between reset and bump
// could re-raise the just-zeroed checkpoint past zero, after which
// the fresh-stream consumer would skip messages:
//
//  1. Bump streamEpoch. Any in-flight trackingMsg captured before
//     this point is from a prior epoch; its late ack will be fenced
//     by AdvanceCheckpoint.
//  2. ResetForStreamRecreate. Drop the stale checkpoint to zero.
//  3. SeedCheckpoint. If the recreated stream / restored consumer's
//     AckFloor > 0, the seed picks it up; if AckFloor is 0 (fresh
//     stream), the checkpoint stays at 0.
//  4. Set recreatedSinceLastBuild so the next BuildConfig call (via
//     RebuildAfterStreamRecreated) chooses DeliverAllPolicy when the
//     checkpoint is still 0 — preventing the fresh-stream skip hazard.
//
// Safe to call on a nil *Controller (no-op).
func (c *Controller) HandleStreamRecreated(ctx context.Context, infoFn InfoFunc) {
	c.handleStreamRecreatedWithSteps(ctx, infoFn, nil)
}

// handleStreamRecreatedWithSteps is the package-internal implementation
// of HandleStreamRecreated. The onStep callback (when non-nil) is invoked
// between each ordered step with the step name from the streamRecreatedStep*
// constants. Tests use it to deterministically inject events between
// steps; production callers always pass nil.
func (c *Controller) handleStreamRecreatedWithSteps(
	ctx context.Context,
	infoFn InfoFunc,
	onStep func(step string),
) {
	if c == nil {
		return
	}
	c.streamEpoch.Add(1)
	if onStep != nil {
		onStep(streamRecreatedStepAfterBump)
	}

	c.checkpoint.ResetForStreamRecreate()
	if onStep != nil {
		onStep(streamRecreatedStepAfterReset)
	}

	c.SeedCheckpoint(ctx, infoFn)
	if onStep != nil {
		onStep(streamRecreatedStepAfterSeed)
	}

	c.recreatedSinceLastBuild.Store(true)
	if onStep != nil {
		onStep(streamRecreatedStepAfterFlag)
	}
}

// RebuildAfterStreamRecreated builds a post-recreate consumer config
// from the (now-reset) checkpoint, the one-shot recreatedSinceLastBuild
// flag, and the strategy; calls recreate(ctx, cfg) to create the new
// consumer on the freshly-restored stream; returns the new consumer.
//
// Read-and-clears recreatedSinceLastBuild via atomic.Bool.Swap so the
// flag's one-shot semantics hold even under contention; subsequent
// rebuilds without a fresh HandleStreamRecreated see the cleared flag
// and fall through to the normal BuildConfig branches.
//
// Bypasses the recover() cooldown: callers (the partition consumer's
// stream-missing detour at Site A and Site B) call this synchronously
// immediately after the hook returns nil; the cooldown applies to
// subsequent unrelated recoveries, which won't be back-to-back with
// this single immediate rebuild.
//
// On any recreate error, wraps the cause with types.ErrStreamMissing
// so the manager observer route fires consistently for the entire
// post-hook recovery flow (both still-missing-stream and
// incompatible-restored-consumer-config cases).
//
// Called by partitionConsumer.handleStreamMissing AFTER
// HandleStreamRecreated has run successfully.
func (c *Controller) RebuildAfterStreamRecreated(
	ctx context.Context,
	baseCfg jetstream.ConsumerConfig,
	recreate RecreateFunc,
) (jetstream.Consumer, error) {
	if c == nil {
		return nil, errors.New("recovery: nil controller cannot rebuild after stream recreate")
	}

	checkpoint := c.checkpoint.Value()
	recreated := c.recreatedSinceLastBuild.Swap(false) // read-and-clear
	cfg, fallback := BuildConfig(baseCfg, c.strategy, checkpoint, recreated)
	if fallback != "" {
		c.logger.Info("recovery rebuild post-recreate used fallback strategy",
			"fallback", fallback,
			"checkpoint", checkpoint,
			"recreated_flag_consumed", recreated,
		)
	}

	newCons, err := recreate(ctx, cfg)
	if err != nil {
		// All errors during post-hook recovery wrap types.ErrStreamMissing
		// so the manager observer route is consistent regardless of the
		// underlying cause (still-missing-stream, incompatible-restored-
		// consumer-config, etc.).
		return nil, fmt.Errorf("%w: %w", types.ErrStreamMissing, err)
	}

	c.burst.Reset()
	c.mu.Lock()
	c.lastRecoveryTime = time.Now()
	c.mu.Unlock()

	return newCons, nil
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
// and re-seeds the checkpoint. Returns:
//   - (newCons, nil) on success.
//   - (nil, nil) when the attempt is short-circuited (another recovery in
//     flight, context canceled, cooldown in effect) or when recreate fails
//     with a transient error. The caller should backoff and retry.
//   - (nil, wrappedErr) where wrappedErr wraps types.ErrStreamMissing when
//     recreate fails with a stream-not-found classification. The caller is
//     expected to route this through the stream-missing detour rather than
//     advance internal recovery state — lastRecoveryTime, burst, and
//     checkpoint are intentionally NOT updated on this path.
func (c *Controller) recover(
	ctx context.Context,
	reason string,
	baseCfg jetstream.ConsumerConfig,
	recreate RecreateFunc,
) (jetstream.Consumer, error) {
	// Contract: (nil, nil) is the documented "no-op, backoff" signal —
	// Classify maps it to ActionBackoff and only ActionStreamMissing
	// surfaces a non-nil error. The nilnil lint disables below are
	// intentional and per-return.
	if !c.inProgress.CompareAndSwap(false, true) {
		return nil, nil //nolint:nilnil // another recovery in flight.
	}
	defer c.inProgress.Store(false)

	c.mu.Lock()
	defer c.mu.Unlock()

	if ctx.Err() != nil {
		return nil, nil //nolint:nilnil // ctx cancelled before lock acquired.
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

		return nil, nil //nolint:nilnil // see recover()'s contract above.
	}

	attempt := beginAttempt(c.metrics, reason)
	success := false
	defer func() { attempt.finish(success) }()

	checkpoint := c.checkpoint.Value()
	// recover() is the normal consumer-deleted recovery path (e.g.,
	// inactivity GC). It must NOT consume the recreatedSinceLastBuild
	// flag — that flag belongs to RebuildAfterStreamRecreated's post-hook
	// rebuild path. Pass false unconditionally here.
	recoverCfg, fallback := BuildConfig(baseCfg, c.strategy, checkpoint, false)

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
		return nil, nil //nolint:nilnil // see recover()'s contract above.
	}

	newCons, err := recreate(ctx, recoverCfg)
	if err != nil {
		if natsutil.IsStreamNotFound(err) {
			// Stream-missing escalation: bail without advancing
			// lastRecoveryTime, burst, or checkpoint. The caller's
			// detour invokes the operator-supplied hook and, on
			// success, calls RebuildAfterStreamRecreated which has
			// its own state-advance semantics.
			c.logger.Warn("consumer recovery surfaced stream missing",
				"op", "consumer_recovery",
				"reason", reason,
				"error", err,
			)
			return nil, fmt.Errorf("%w: %w", types.ErrStreamMissing, err)
		}
		c.logger.Warn("consumer recovery failed",
			"op", "consumer_recovery",
			"reason", reason,
			"error", err,
		)

		return nil, nil //nolint:nilnil // see recover()'s contract above.
	}

	c.burst.Reset()
	c.lastRecoveryTime = time.Now()

	c.logger.Info("consumer recovered",
		"op", "consumer_recovery",
		"strategy", c.strategy,
		"reason", reason,
	)

	success = true

	return newCons, nil
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
