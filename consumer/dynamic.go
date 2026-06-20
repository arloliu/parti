package consumer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/fuda"
	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/internal/ratelimit"
	"github.com/arloliu/parti/v2/internal/recovery"
	"github.com/arloliu/parti/v2/jsutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// Compile-time assertion: *Dynamic satisfies recovery.StreamMissingObserver
// so parti.Manager's type-assertion during prepareStart bridges the
// stream-missing recovery exhaustion event into enterDegraded.
var _ recovery.StreamMissingObserver = (*Dynamic)(nil)

// Dynamic is a partition-aware consumer that receives assignments from a Parti Manager.
// It manages multiple internal consumers based on assigned partitions.
//
// # Lifecycle
//
// Create with [NewDynamic], then call [Dynamic.Update] to start consuming assigned
// partitions. Clean up with [Dynamic.Stop]:
//
//	consumer, err := consumer.NewDynamic(js, "stream", "worker", "orders.{{.PartitionID}}", handler)
//	if err != nil { log.Fatal(err) }
//	defer consumer.Stop(ctx)
//
//	// Start consuming partitions (typically called by Parti Manager)
//	if err := consumer.Update(ctx, "worker-0", partitions); err != nil { log.Fatal(err) }
//
// Unlike [Static] and [Queue], Dynamic does NOT have a Start method. Consumption
// begins when [Dynamic.Update] is called with a non-empty partition list.
//
// # Thread Safety
//
// Dynamic is safe for concurrent use. [Dynamic.Update] calls are serialized
// internally to prevent race conditions during assignment changes.
type Dynamic struct {
	inner            *durable.WorkerConsumer
	js               jetstream.JetStream
	streamName       string
	recoveryStrategy RecoveryStrategy

	// workQueueResult holds the immutable outcome of the most recent
	// WorkQueuePolicy/recovery compatibility check. nil means "not yet
	// checked, or cleared after a stream recreate". Publishing the
	// result through a single atomic.Pointer to an immutable struct
	// eliminates the v1 publication race between a "checked" flag and
	// the error value.
	workQueueResult atomic.Pointer[compatCheckResult]
	// workQueueMu serializes the once-style check so resetCompatCheck
	// cannot interleave between an in-flight slow-path's compute and
	// publish steps (the pre-v3 stale-store race).
	workQueueMu sync.Mutex

	// userOnPermanentFailure is the application-supplied callback
	// captured from WithOnPermanentFailure. When non-nil it fires
	// first in the dispatcher, before the manager-installed observer
	// is consulted (see [WithOnPermanentFailure]).
	// Nil if the application did not supply one.
	userOnPermanentFailure func(subject string, err error)

	// suppressManagerDegrade, when true, prevents the dispatcher from
	// notifying the manager-installed stream-missing observer. Set via
	// WithSuppressManagerDegradeOnStreamMissing for applications that
	// deliberately own rotation/degrade signaling themselves.
	suppressManagerDegrade bool

	// managerOnStreamMissing holds the closure registered via
	// SetOnStreamMissingError. The atomic.Pointer indirection lets
	// Manager.Start install the callback AFTER NewDynamic has already
	// built the durable layer (which captures d.onPermanentFailure by
	// value); the dispatcher reads the pointer at fire time so a late
	// Store is honored. nil pointer means "no manager observer wired".
	managerOnStreamMissing atomic.Pointer[func(streamName string, err error)]

	// Adaptive consumer-create rate (fleet-size-aware). createRateSetter is the
	// built-in limiter cast to RateSetter, non-nil only when WithConsumerCreateRate
	// AND WithConsumerCreateClusterRate are set. createPerSec is the per-worker
	// ceiling; createClusterRate is the cluster target.
	createRateSetter  ratelimit.RateSetter
	createPerSec      float64
	createClusterRate float64
}

// compatCheckResult is the immutable outcome of one WorkQueue
// compatibility check. A nil *compatCheckResult means "not yet
// checked, or reset after a stream recreate".
type compatCheckResult struct {
	err error // non-nil iff the check failed
}

// compatCheckFn is the seam Dynamic invokes to run the
// WorkQueue/recovery compatibility check. Tests swap this to inject
// canned outcomes or to block the slow-path inside the mutex region
// without standing up a real jetstream.JetStream. Production callers
// never reassign it.
var compatCheckFn = CheckWorkQueueRecoveryCompat

// DynamicConfig configures a Dynamic consumer.
type DynamicConfig struct {
	CommonConfig

	// StreamName is the JetStream stream to consume from.
	// Required.
	StreamName string `validate:"required"`

	// SubjectTemplate is a text/template for building subjects from partitions.
	//
	// It relies on the standard Go text/template package.
	// The template context provides a {{.PartitionID}} variable.
	//
	// Example: "orders.{{.PartitionID}}.events"
	SubjectTemplate string `validate:"required"`

	// ConsumerPrefix is the prefix for the durable consumer name.
	//
	// The final durable name is constructed dynamically for each assigned partition:
	// "<ConsumerPrefix>_<partitionID>_<hash>".
	//
	// This ensures unique, stable durability for each partition assignment.
	ConsumerPrefix string `validate:"required"`

	// ProcessingGate configures optional per-message admission control.
	//
	// When enabled, the consumer checks KV-backed ownership claims before each
	// handler invocation and NAKs deliveries for partitions it does not own.
	// This bounds — but does not eliminate — cross-worker overlap during
	// rebalances: an already-in-flight handler invocation cannot be revoked,
	// and AckWait-expiry redelivery through the shared per-partition durable
	// can hand the same message to the new owner while the old owner's handler
	// is still running. Wrap long-running handlers with [NewWIPHandler] to
	// prevent that expiry. See docs/LIFECYCLE.md "Two-Phase Handoff" for the
	// per-tier overlap contract.
	ProcessingGate *ProcessingGateConfig

	// Resolver configures the ownership resolver used when ProcessingGate is enabled.
	//
	// It defines how ownership is claimed, refreshed, and verified.
	Resolver ResolverConfig

	// PullGatingEnabled enables pre-pull ownership/state gating for consumers.
	//
	// When true, the consumer will check if it still owns the partition before
	// issuing a pull request to JetStream. This reduces "ghost" processing of
	// messages after assignment revocation.
	PullGatingEnabled bool

	// DrainOnRemove enables graceful draining when a partition assignment is revoked.
	//
	// When true, removal first polls the partition's durable until the server
	// reports no pending acknowledgements — the pull loop keeps running during
	// this wait — and only then stops the loop. Both the drain wait and the
	// loop-stop wait are bounded by DrainOnRemoveTimeout.
	DrainOnRemove bool

	// DrainOnRemoveTimeout caps the time spent draining a revoked partition
	// and bounds the wait for its pull loop to stop.
	//
	// The wait is best-effort: if loops have not stopped within this bound,
	// [Dynamic.Update] returns an error (the manager retries the apply with
	// backoff) while an already-in-flight handler invocation may still run
	// to completion. Default: 10s.
	DrainOnRemoveTimeout time.Duration `default:"10s" validate:"gte=0"`

	// MaxConcurrentSubjects limits the number of partitions (subjects) processed concurrently.
	//
	// If an assignment's deduped subject count exceeds this limit,
	// [Dynamic.Update] rejects the whole update with [ErrMaxSubjectsExceeded]
	// before making any changes; the manager retries the apply with backoff
	// and the previous owners keep consuming. Size this above the worst-case
	// per-worker partition count (e.g. after scale-down) or leave it 0
	// (unlimited).
	MaxConcurrentSubjects int `validate:"gte=0"`

	// AllowWorkerIDChange controls whether the worker's identity can change during runtime.
	//
	// Default: false (immutable once set). Changing WorkerID usually requires a restart.
	AllowWorkerIDChange bool

	// Retry configures the backoff behavior for the consume loop.
	//
	// On each iterator failure the loop sleeps for a jittered delay that grows via
	// decorrelated jitter from Base toward Max using the Multiplier factor. Seed
	// makes the sequence deterministic (useful in tests); zero uses the package-level
	// PRNG. The delay resets to zero after every successful iteration cycle.
	Retry RetryConfig

	// IteratorEscalationWindow defines the sliding time window used to aggregate
	// iterator failures for escalation detection.
	//
	// If too many iterator errors occur within this window, the consumer will
	// attempt to escalate recovery.
	//
	// Default: 60s.
	IteratorEscalationWindow time.Duration `default:"60s" validate:"gt=0"`

	// IteratorEscalationThreshold is the number of iterator failures within the
	// escalation window that triggers consumer refresh/escalation.
	//
	// Default: 3.
	IteratorEscalationThreshold int `default:"3" validate:"gt=0"`

	// PartitionRefreshMinInterval sets the minimum interval between forced claim refreshes
	// per partition when pull gating is enabled.
	//
	// This prevents excessive load on the coordination backend (KV) during high-throughput pulling.
	//
	// Default: 500ms.
	PartitionRefreshMinInterval time.Duration `default:"500ms" validate:"gt=0"`

	// IteratorFactory optionally overrides the internal iterator creation logic.
	// This is primarily used for testing to inject mock iterators.
	IteratorFactory func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)

	// RecoveryStrategy defines how a recreated consumer resumes after an unexpected deletion.
	//
	// All strategies are supported. [RecoverFromLastProcessed] works with both
	// ManualAck=false (checkpoint advances automatically) and ManualAck=true
	// (checkpoint advances when the handler calls msg.Ack() or msg.DoubleAck()).
	//
	// Note: Dynamic consumers have a separate iterator-escalation detector (see
	// [DynamicConfig.IteratorEscalationWindow]) that rebinds to the existing durable
	// on repeated failures. RecoveryStrategy controls a distinct, complementary mechanism
	// that recreates the durable itself when it has been deleted.
	//
	// # WorkQueuePolicy streams
	//
	// NATS only permits [jetstream.DeliverAllPolicy] on WorkQueuePolicy streams.
	// [RecoverFromNew] and [RecoverFromLastProcessed] are incompatible and will
	// cause the first [Dynamic.Update] call to return [ErrInvalidConfig]. Use
	// [RecoverFromBeginning] or [RecoveryDisabled] for WorkQueuePolicy streams.
	RecoveryStrategy RecoveryStrategy

	// StreamMissingHook fires when the dynamic partition consumer cannot
	// create a consumer because the underlying JetStream stream is absent.
	// The hook is the operator-driven recovery escalation path; the
	// library does not recreate streams itself.
	//
	// Returning nil indicates the operator has recreated the stream. The
	// library then bumps the recovery controller's stream-epoch, resets
	// its checkpoint, re-seeds from the new consumer info, and rebuilds
	// the consumer. Returning a non-nil error (or omitting the hook
	// entirely) surfaces the loss via the F2 envelope's exhaustion path:
	// OnPermanentFailure fires with the error wrapped in
	// [types.ErrStreamMissing] so the manager routes it to enterDegraded.
	//
	// Requires [RecoveryStrategy] in {[RecoverFromLastProcessed],
	// [RecoverFromBeginning]}; [RecoveryDisabled] (default) and
	// [RecoverFromNew] are rejected at [DynamicConfig.Validate] time.
	// See [types.StreamMissingHook] godoc for the full operator
	// contract (same-durable-name preservation, compatible-config
	// restore, fresh-stream replay semantics).
	StreamMissingHook types.StreamMissingHook

	// OnPermanentFailure is the application-facing observability seam for
	// permanent partition-consumer failure. It fires exactly once per
	// partition consumer when the iterator-creation envelope or the
	// stream-missing Site B detour exhausts its attempt budget; the
	// consumption loop exits immediately afterwards.
	//
	// The callback runs synchronously on the consumption loop's goroutine
	// and MUST be non-blocking. The error preserves the wrap chain:
	// errors.Is(err, [types.ErrStreamMissing]) is true when stream-missing
	// exhaustion drove the failure, so applications can route those
	// distinctly from generic envelope exhaustion.
	//
	// Registering this callback does NOT disable the Parti manager's
	// auto-degraded route: for stream-missing exhaustion the manager
	// observer is notified after this callback returns. Use
	// [DynamicConfig.SuppressManagerDegradeOnStreamMissing] (option form:
	// [WithSuppressManagerDegradeOnStreamMissing]) to opt out explicitly.
	// See [WithOnPermanentFailure] for the full contract.
	OnPermanentFailure func(subject string, err error)

	// SuppressManagerDegradeOnStreamMissing disables the Parti manager's
	// auto-degraded route for stream-missing recovery exhaustion. By default
	// the manager observer is notified even when OnPermanentFailure is set
	// (both fire; application callback first). Set this only when the
	// application deliberately owns degrade/rotation signaling itself.
	SuppressManagerDegradeOnStreamMissing bool

	// RecoveryRetry tunes the bounded-retry envelope wrapped around
	// the iterator-creation path and the stream-missing Site B
	// detour. Zero-valued fields fall back to the durable layer's
	// defaults (MaxAttempts=8, BaseBackoff=500ms, MaxBackoff=30s).
	// See [WithRecoveryRetry] for the option form.
	RecoveryRetry RecoveryRetryConfig
}

// validateStreamMissingHookStrategy enforces the recovery-strategy
// pre-conditions for StreamMissingHook at the consumer/ surface.
// Intentionally duplicated from internal/durable's same-named helper
// because internal/durable cannot import consumer/ and a shared
// package would just be coupling for ~15 lines. Cross-package
// consistency is pinned by a consumer_test that runs both public
// Validate surfaces over the same matrix.
func validateStreamMissingHookStrategy(hookConfigured bool, strategy RecoveryStrategy) error {
	if !hookConfigured {
		return nil
	}
	switch strategy { //nolint:exhaustive // explicit branches; default catches the rest.
	case RecoverFromLastProcessed, RecoverFromBeginning:
		return nil
	case RecoveryDisabled:
		return fmt.Errorf(
			"%w: StreamMissingHook requires a non-disabled RecoveryStrategy; "+
				"use WithRecoveryStrategy(RecoverFromLastProcessed) for at-least-once "+
				"or WithRecoveryStrategy(RecoverFromBeginning) for replay-all",
			ErrInvalidConfig)
	case RecoverFromNew:
		return fmt.Errorf(
			"%w: StreamMissingHook is incompatible with RecoverFromNew "+
				"because the recreated-stream replay override only applies to "+
				"RecoverFromLastProcessed and RecoverFromBeginning; "+
				"RecoverFromNew would silently skip messages published after a fresh-stream recreate",
			ErrInvalidConfig)
	default:
		return fmt.Errorf(
			"%w: StreamMissingHook with unknown RecoveryStrategy %v", ErrInvalidConfig, strategy)
	}
}

// NewDynamic creates a new dynamic partition consumer.
func NewDynamic(
	js jetstream.JetStream,
	streamName, consumerPrefix, subjectTemplate string,
	handler MessageHandler,
	opts ...DynamicOption,
) (*Dynamic, error) {
	if js == nil {
		return nil, fmt.Errorf("%w: JetStream context is required", ErrInvalidConfig)
	}
	if streamName == "" {
		return nil, fmt.Errorf("%w: stream name is required", ErrInvalidConfig)
	}
	if consumerPrefix == "" {
		return nil, fmt.Errorf("%w: consumer prefix is required", ErrInvalidConfig)
	}
	if subjectTemplate == "" {
		return nil, fmt.Errorf("%w: subject template is required", ErrInvalidConfig)
	}
	if handler == nil {
		return nil, fmt.Errorf("%w: message handler is required", ErrInvalidConfig)
	}

	// Apply options
	o := defaultOptions()
	for _, opt := range opts {
		opt.apply(&o)
	}

	// Validate and resolve the consumer-create rate limiter.
	// Precedence: a non-nil injected limiter wins over the rate option.
	// WithConsumerCreateLimiter(nil) is a no-op (does not clear a rate).
	resolvedLimiter, err := resolveConsumerCreateLimiter(o)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	// Build configuration
	cfg := DynamicConfig{
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
		StreamName:                            streamName,
		ConsumerPrefix:                        consumerPrefix,
		SubjectTemplate:                       subjectTemplate,
		ProcessingGate:                        o.processingGate,
		Resolver:                              o.resolver,
		PullGatingEnabled:                     o.pullGatingEnabled,
		DrainOnRemove:                         o.drainOnRemove,
		DrainOnRemoveTimeout:                  o.drainOnRemoveTimeout,
		MaxConcurrentSubjects:                 o.maxConcurrentSubjects,
		AllowWorkerIDChange:                   o.allowWorkerIDChange,
		Retry:                                 o.retry,
		IteratorEscalationWindow:              o.iteratorEscalationWindow,
		IteratorEscalationThreshold:           o.iteratorEscalationThreshold,
		PartitionRefreshMinInterval:           o.partitionRefreshMinInterval,
		IteratorFactory:                       o.iteratorFactory,
		RecoveryStrategy:                      o.recoveryStrategy,
		StreamMissingHook:                     o.streamMissingHook,
		OnPermanentFailure:                    o.onPermanentFailure,
		SuppressManagerDegradeOnStreamMissing: o.suppressManagerDegradeOnStreamMissing,
		RecoveryRetry:                         o.recoveryRetry,
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// Construct Dynamic first so the OnStreamRecreated closure can reference
	// d.resetCompatCheck. The inner WorkerConsumer is assigned after
	// construction succeeds; passing the closure value into workerCfg below
	// is safe because the closure captures the d pointer (which does not
	// change between this point and the return statement).
	d := &Dynamic{
		js:                     js,
		streamName:             streamName,
		recoveryStrategy:       o.recoveryStrategy,
		userOnPermanentFailure: cfg.OnPermanentFailure,
		suppressManagerDegrade: cfg.SuppressManagerDegradeOnStreamMissing,
	}

	// Build worker consumer config from unified DynamicConfig.
	workerCfg := durable.WorkerConsumerConfig{
		StreamName:                  cfg.StreamName,
		ConsumerPrefix:              cfg.ConsumerPrefix,
		SubjectTemplate:             cfg.SubjectTemplate,
		Logger:                      cfg.Logger,
		Metrics:                     cfg.Metrics,
		ManualAck:                   cfg.ManualAck,
		AckWait:                     cfg.AckWait,
		MaxDeliver:                  cfg.MaxDeliver,
		BatchSize:                   cfg.BatchSize,
		FetchTimeout:                cfg.FetchTimeout,
		MaxWaiting:                  cfg.MaxWaiting,
		MaxAckPending:               cfg.MaxAckPending,
		InactiveThreshold:           cfg.InactiveThreshold,
		AckPolicy:                   cfg.AckPolicy,
		ConsumerMemoryStorage:       cfg.ConsumerMemoryStorage,
		ConsumerReplicas:            cfg.ConsumerReplicas,
		ProcessingGate:              toSubscriptionGateConfig(cfg.ProcessingGate),
		Resolver:                    toSubscriptionResolverConfig(cfg.Resolver),
		PullGatingEnabled:           cfg.PullGatingEnabled,
		DrainOnRemove:               cfg.DrainOnRemove,
		DrainOnRemoveTimeout:        cfg.DrainOnRemoveTimeout,
		MaxConcurrentSubjects:       cfg.MaxConcurrentSubjects,
		AllowWorkerIDChange:         cfg.AllowWorkerIDChange,
		PartitionRefreshMinInterval: cfg.PartitionRefreshMinInterval,
		IteratorEscalationWindow:    cfg.IteratorEscalationWindow,
		IteratorEscalationThreshold: cfg.IteratorEscalationThreshold,
		RecoveryStrategy:            cfg.RecoveryStrategy,
		IteratorFactory:             cfg.IteratorFactory,
		StreamMissingHook:           cfg.StreamMissingHook,
		// Install the indirection dispatcher unconditionally. It reads
		// d.userOnPermanentFailure (captured above) and
		// d.managerOnStreamMissing (installed later via
		// SetOnStreamMissingError) at fire time, so a Manager.Start call
		// occurring AFTER NewDynamic still reaches the durable layer.
		OnPermanentFailure:    d.onPermanentFailure,
		OnStreamRecreated:     d.resetCompatCheck,
		ConsumerCreateLimiter: resolvedLimiter,
		Retry: durable.RetryConfig{
			Backoff:    cfg.Retry.Backoff,
			Max:        cfg.Retry.Max,
			Multiplier: cfg.Retry.Multiplier,
			Base:       cfg.Retry.Base,
			Seed:       cfg.Retry.Seed,
		},
		RecoveryRetry: cfg.RecoveryRetry,
	}

	inner, err := durable.NewWorkerConsumer(js, workerCfg, handler.Handle)
	if err != nil {
		return nil, err
	}
	d.inner = inner

	if o.consumerCreateClusterRate > 0 {
		if rs, ok := resolvedLimiter.(ratelimit.RateSetter); ok {
			d.createRateSetter = rs
			d.createPerSec = o.consumerCreatePerSec
			d.createClusterRate = o.consumerCreateClusterRate
		}
	}

	return d, nil
}

// onPermanentFailure is the indirection dispatcher installed as
// WorkerConsumerConfig.OnPermanentFailure for every Dynamic. It runs on
// the partition consumer's goroutine when iterator-creation envelope
// or Site B detour exhaustion fires.
//
// Dispatch rules:
//  1. An application-supplied OnPermanentFailure callback (captured from
//     WithOnPermanentFailure) fires first when registered.
//  2. The manager-installed observer (set via SetOnStreamMissingError) is
//     then ALSO notified for stream-missing exhaustion, so platform
//     self-healing (enterDegraded -> rotation) does not silently turn off
//     when an application adds its own observability callback. Applications
//     that own degrade signaling themselves opt out explicitly via
//     WithSuppressManagerDegradeOnStreamMissing.
//  3. Generic (non-stream-missing) exhaustion never reaches the manager
//     observer; the durable layer's WARN log + metric remain the
//     operator-visible signal for those.
func (d *Dynamic) onPermanentFailure(subject string, err error) {
	if d.userOnPermanentFailure != nil {
		d.userOnPermanentFailure(subject, err)
	}
	if d.suppressManagerDegrade {
		return
	}
	if fn := d.managerOnStreamMissing.Load(); fn != nil && errors.Is(err, types.ErrStreamMissing) {
		(*fn)(d.streamName, err)
	}
}

// SetOnStreamMissingError installs a manager-side observer for
// stream-missing recovery exhaustion. The Parti manager calls this
// during Manager.Start so a stream-missing failure surfaces as a named
// degraded entry ("stream-missing-recovery-exhausted") rather than
// counting against the generic KV-error threshold.
//
// fn receives the dynamic consumer's stream name and the wrapped
// types.ErrStreamMissing error chain. Calling with fn == nil clears
// any previously-installed observer.
//
// Application-supplied callbacks registered via [WithOnPermanentFailure]
// do not suppress this observer: both fire for stream-missing exhaustion
// (application callback first, then this observer), unless the application
// explicitly opts out via [WithSuppressManagerDegradeOnStreamMissing].
// See [Dynamic.onPermanentFailure] for the full dispatch rules.
//
// Safe for concurrent use; calls before, during, or after NewDynamic
// are equivalent — the durable layer reads the slot at fire time.
//
// Dynamic implements [recovery.StreamMissingObserver] via this method.
func (d *Dynamic) SetOnStreamMissingError(fn func(streamName string, err error)) {
	if fn == nil {
		d.managerOnStreamMissing.Store(nil)
		return
	}
	d.managerOnStreamMissing.Store(&fn)
}

// Update applies a new partition assignment set.
//
// This method creates or binds durable consumers for newly assigned partitions
// and stops consumers for removed partitions. The underlying JetStream consumers
// are NOT deleted on removal; they will be garbage-collected by the server after
// InactiveThreshold.
//
// Update is typically called by the Parti Manager when assignments change.
// On the first call, this starts consuming the assigned partitions.
//
// Parameters:
//   - ctx: Context for the update operation. Used for JetStream API calls.
//   - workerID: The stable worker ID for this instance (e.g., "worker-0").
//   - partitions: The new set of partitions to consume. Empty list stops all.
//
// Returns:
//   - error: Non-nil if partition creation fails or if workerID mutation is
//     disallowed (see [DynamicConfig.AllowWorkerIDChange]).
//
// Errors:
//   - [ErrWorkerIDMutation]: Returned when workerID changes and
//     AllowWorkerIDChange is false.
//   - [ErrMaxSubjectsExceeded]: Returned when partition count
//     exceeds MaxConcurrentSubjects.
//   - Non-sentinel error: Returned when one or more removed partition loops
//     fail to stop within DrainOnRemoveTimeout; the stop wait is bounded by
//     that timeout even when DrainOnRemove is false. Tracking entries are
//     cleared so the manager's retry converges.
func (d *Dynamic) Update(ctx context.Context, workerID string, partitions []types.Partition) error {
	if d.inner.Closed() {
		return fmt.Errorf("dynamic consumer: %w", ErrConsumerStopped)
	}
	if err := d.ensureCompatChecked(ctx); err != nil {
		return err
	}
	return d.inner.UpdateWorkerConsumer(ctx, workerID, partitions)
}

// UpdateWorkerConsumer is an alias for [Dynamic.Update] that implements the
// WorkerConsumerUpdater interface used by the Parti Manager.
//
// Deprecated: Use [Dynamic.Update] for new code. This method exists for
// backward compatibility with code that expects the WorkerConsumerUpdater
// interface.
func (d *Dynamic) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []types.Partition) error {
	// Run the same compat check as the Update path. Pre-fix this method
	// bypassed the check entirely, so a stale Dynamic registered with
	// the manager could silently accept an incompatible
	// WorkQueuePolicy/recovery combination on a recreated stream.
	return d.Update(ctx, workerID, partitions)
}

// ensureCompatChecked runs the WorkQueue/recovery compatibility check at
// most once per reset cycle. The fast path is a single atomic load; the
// result struct is immutable once published, so there is no publication
// race between the "is it checked" flag and the error value (they are one
// pointer). The slow path takes workQueueMu so resetCompatCheck cannot
// interleave between a still-running compat check's compute and publish
// steps.
func (d *Dynamic) ensureCompatChecked(ctx context.Context) error {
	if r := d.workQueueResult.Load(); r != nil {
		return r.err
	}
	d.workQueueMu.Lock()
	defer d.workQueueMu.Unlock()
	if r := d.workQueueResult.Load(); r != nil {
		// another goroutine published while we waited
		return r.err
	}
	err := compatCheckFn(ctx, d.js, d.streamName, d.recoveryStrategy)
	d.workQueueResult.Store(&compatCheckResult{err: err})

	return err
}

// resetCompatCheck clears the cached result so the next Update or
// UpdateWorkerConsumer re-runs the WorkQueue/recovery compatibility
// check. Wired into the partition consumer's stream-missing recovery
// detour: after Controller.HandleStreamRecreated rebuilds the consumer
// against a freshly-restored stream, the cached compatibility verdict
// no longer applies to the new stream and must be re-armed.
//
// Takes workQueueMu so the reset cannot interleave between an
// in-flight slow-path check's compute and publish steps (the pre-v3
// stale-store race fixed in v3).
func (d *Dynamic) resetCompatCheck() {
	d.workQueueMu.Lock()
	defer d.workQueueMu.Unlock()
	d.workQueueResult.Store(nil)
}

// Capabilities forwards to the inner [durable.WorkerConsumer]; implements
// the [parti.CapabilityReporter] interface so the Manager's type-assertion
// on the registered *Dynamic updater succeeds.
func (d *Dynamic) Capabilities() uint32 {
	return d.inner.Capabilities()
}

// ObserveWorkerCount implements the Parti manager's fleet-size-observer optional
// interface. The Manager calls it with the current committed cluster worker-count
// N so the Dynamic consumer can retune its consumer-create limiter to
// min(perWorkerCeiling, clusterRate/N).
//
// It is a no-op unless both WithConsumerCreateRate and WithConsumerCreateClusterRate
// were configured (createRateSetter is nil otherwise, e.g. an injected limiter).
// Non-blocking and non-reentrant per the observer contract:
// it only calls SetRate on the built-in limiter.
func (d *Dynamic) ObserveWorkerCount(n int) {
	if d.createRateSetter == nil {
		return
	}
	d.createRateSetter.SetRate(ratelimit.EffectiveRate(d.createPerSec, d.createClusterRate, n))
}

// Compile-time assertion: *Dynamic satisfies the fleet-size-observer interface
// expected by the Parti Manager's optional type-assertion.
var _ interface{ ObserveWorkerCount(int) } = (*Dynamic)(nil)

// Stop gracefully stops all partition consumers.
//
// Stop cancels all internal pull loops and waits for pending message processing
// to complete (up to the context deadline). The underlying JetStream consumers
// are NOT deleted; they will be garbage-collected by the server after
// InactiveThreshold.
//
// If DrainOnRemove is enabled, Stop will first drain pending messages
// (up to DrainOnRemoveTimeout) before stopping.
//
// Stop is terminal: subsequent manager updates (via [Dynamic.Update] or
// [Dynamic.UpdateWorkerConsumer]) will fail with [ErrConsumerStopped]. Create a
// new [Dynamic] to consume again. To avoid a loud retry loop in the manager,
// stop the manager first or deregister this consumer before calling Stop.
//
// Stop is idempotent; calling it multiple times is safe.
//
// Parameters:
//   - ctx: Context with shutdown deadline. If the deadline expires, Stop
//     returns [context.DeadlineExceeded] but consumers will still eventually stop.
//
// Returns:
//   - error: Context error if the wait times out; nil otherwise.
func (d *Dynamic) Stop(ctx context.Context) error {
	return d.inner.Close(ctx)
}

// SetResolverMetrics sets the metrics collector for the ownership resolver.
//
// This is an advanced method for observability integration. Most users do not
// need to call this directly.
func (d *Dynamic) SetResolverMetrics(m ResolverMetrics) {
	d.inner.SetResolverMetrics(m)
}

// SetDefaults applies default values to the configuration.
func (c *DynamicConfig) SetDefaults() error {
	return fuda.SetDefaults(c)
}

// Validate checks configuration constraints.
func (c *DynamicConfig) Validate() error {
	if err := c.SetDefaults(); err != nil {
		return err
	}
	if err := fuda.Validate(c); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	if err := validateFetchTimeoutFloor(c.FetchTimeout); err != nil {
		return err
	}

	if !jsutil.IsValidConsumerName(c.ConsumerPrefix) {
		return fmt.Errorf("%w: consumer prefix %q contains invalid characters (allowed: a-z, A-Z, 0-9, -, _)", ErrInvalidConfig, c.ConsumerPrefix)
	}

	return validateStreamMissingHookStrategy(c.StreamMissingHook != nil, c.RecoveryStrategy)
}

// toSubscriptionGateConfig converts a consumer-owned ProcessingGateConfig to
// the internal subscription equivalent. Returns nil when cfg is nil.
func toSubscriptionGateConfig(cfg *ProcessingGateConfig) *durable.ProcessingGateConfig {
	if cfg == nil {
		return nil
	}

	return &durable.ProcessingGateConfig{
		Enabled:             cfg.Enabled,
		AllowedStates:       cfg.AllowedStates,
		WarmupDuration:      cfg.WarmupDuration,
		WarmupAllowedStates: cfg.WarmupAllowedStates,
		NakDelay:            cfg.NakDelay,
		NakJitter:           cfg.NakJitter,
		Debug:               cfg.Debug,
		Metrics:             cfg.Metrics, // consumer.GateMetrics satisfies durable.GateMetrics
	}
}

// toSubscriptionResolverConfig converts a consumer-owned ResolverConfig to the
// internal subscription equivalent.
func toSubscriptionResolverConfig(cfg ResolverConfig) durable.ResolverConfig {
	return durable.ResolverConfig{
		OwnershipResolver:   cfg.OwnershipResolver,
		HandoffBucketName:   cfg.HandoffBucketName,
		HandoffClaimsPrefix: cfg.HandoffClaimsPrefix,
		BatchWindow:         cfg.BatchWindow,
		BatchMaxItems:       cfg.BatchMaxItems,
		ReconcileInterval:   cfg.ReconcileInterval,
	}
}

// resolveConsumerCreateLimiter validates rate-limit options and returns the
// effective limiter, or nil when none is configured.
//
// Precedence (any option order): a non-nil injected limiter wins over a rate.
// WithConsumerCreateLimiter(nil) is a no-op and never clears a configured rate.
func resolveConsumerCreateLimiter(o options) (ratelimit.Limiter, error) {
	// Validate the cluster-rate overlay up front — independent of which limiter
	// wins — so a negative value is rejected on every path.
	if o.consumerCreateClusterRate < 0 {
		return nil, fmt.Errorf("WithConsumerCreateClusterRate: clusterPerSec must be >= 0, got %v", o.consumerCreateClusterRate)
	}

	// A non-nil injected limiter always wins; it is not adaptively retuned.
	if o.consumerCreateLimiter != nil {
		if o.consumerCreateClusterRate > 0 {
			return nil, errors.New("WithConsumerCreateClusterRate cannot be combined with an injected WithConsumerCreateLimiter (an injected limiter is not adaptively retuned)")
		}
		return o.consumerCreateLimiter, nil
	}

	// No per-worker rate configured. A cluster rate alone is invalid (it needs
	// the per-worker ceiling and burst); otherwise return unlimited (nil).
	if o.consumerCreatePerSec == 0 {
		if o.consumerCreateClusterRate > 0 {
			return nil, errors.New("WithConsumerCreateClusterRate requires WithConsumerCreateRate (the per-worker ceiling and burst source)")
		}
		return ratelimit.Limiter(nil), nil //nolint:nilnil // nil limiter is the intended "unlimited" sentinel
	}

	// Validate the rate option.
	if o.consumerCreatePerSec < 0 {
		return nil, fmt.Errorf("WithConsumerCreateRate: perSec must be >= 0, got %v", o.consumerCreatePerSec)
	}
	if o.consumerCreateBurst < 1 {
		return nil, fmt.Errorf("WithConsumerCreateRate: burst must be >= 1 when perSec > 0, got %d", o.consumerCreateBurst)
	}

	// Build a throttle observer adapter if the configured metrics implements
	// the optional sidecar. The adapter bridges ratelimit.ThrottleObserver
	// to durable.ConsumerCreateThrottleObserver via a type assertion at
	// limiter construction time (not at emit time, so no repeated assertion).
	var obs ratelimit.ThrottleObserver
	if o.metrics != nil {
		if throttleObs, ok := o.metrics.(consumerCreateThrottleObserver); ok {
			obs = throttleObserverAdapter{throttleObs}
		}
	}

	return ratelimit.New(o.consumerCreatePerSec, o.consumerCreateBurst, obs), nil
}

// consumerCreateThrottleObserver mirrors durable.ConsumerCreateThrottleObserver
// in the consumer package to avoid importing internal/durable from options.go
// (which does import it, but avoids a non-trivial dependency exposure).
// Both interfaces are defined identically; only the package differs.
type consumerCreateThrottleObserver interface {
	IncrementConsumerCreateThrottled()
	ObserveConsumerCreateThrottleWait(seconds float64)
}

// throttleObserverAdapter bridges consumerCreateThrottleObserver to
// ratelimit.ThrottleObserver.
type throttleObserverAdapter struct {
	obs consumerCreateThrottleObserver
}

func (a throttleObserverAdapter) IncrementThrottled() {
	a.obs.IncrementConsumerCreateThrottled()
}

func (a throttleObserverAdapter) ObserveWait(seconds float64) {
	a.obs.ObserveConsumerCreateThrottleWait(seconds)
}
