package consumer

import (
	"time"

	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/ratelimit"
	"github.com/arloliu/parti/v2/internal/recovery"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// RecoveryRetryConfig tunes the bounded-retry envelope wrapped around
// the dynamic partition consumer's iterator-creation path and the
// stream-missing Site B detour. The envelope caps consecutive failures
// at [RecoveryRetryConfig.MaxAttempts]; exhaustion fires
// [Dynamic]'s OnPermanentFailure / manager observer routes once and
// the consumer loop exits.
//
// Defaults (when fields are zero): MaxAttempts=8, BaseBackoff=500ms,
// MaxBackoff=30s, Jitter=0.2. Lowering these accelerates the
// degraded-mode transition for operators willing to trade short-term
// reconnection patience for faster pod rotation; raising them
// tolerates longer NATS partitions before escalation.
type RecoveryRetryConfig = durable.RecoveryRetryConfig

// RecoveryStrategy defines how a recreated consumer decides where to resume
// after an unexpected deletion.
//
// Use [WithRecoveryStrategy] to enable auto-recovery on consumer types that
// support durable recreation semantics.
type RecoveryStrategy = recovery.Strategy

// Recovery strategy constants control how a consumer resumes after its underlying
// durable is unexpectedly deleted. The zero value [RecoveryDisabled] is safe and
// preserves the pre-existing behavior (backoff-retry only, no durable recreation).
//
// Note: [Dynamic] consumers have a separate sliding-window iterator-failure detector
// that attempts consumer rebind independently of this setting. That mechanism is always
// active and is not affected by the recovery strategy.
const (
	// RecoveryDisabled is the zero value. No strategy-aware consumer recreation is
	// performed on deletion. The consumer retries transient errors with backoff but
	// does not recreate the durable with an adjusted DeliverPolicy.
	RecoveryDisabled = recovery.Disabled

	// RecoverFromNew recreates the consumer to deliver only newly published messages.
	// Messages that arrived while the consumer was absent are skipped entirely.
	//
	// Pros: zero replay risk; safe for Queue consumers and any consumer where
	// missing a window of messages is acceptable.
	// Cons: messages published between deletion and recreation are lost.
	//
	// # WorkQueuePolicy streams
	//
	// Not compatible with WorkQueuePolicy streams. NATS only permits
	// [jetstream.DeliverAllPolicy] on work-queue streams; this strategy maps to
	// [jetstream.DeliverNewPolicy], which NATS rejects. [Queue.Start],
	// [Static.Start], and [Dynamic.Update] return [ErrInvalidConfig] when this
	// combination is detected. Use [RecoverFromBeginning] or [RecoveryDisabled]
	// on WorkQueuePolicy streams.
	RecoverFromNew = recovery.FromNew

	// RecoverFromLastProcessed recreates the consumer starting at the sequence
	// immediately after the last acknowledged message. This provides at-least-once
	// delivery without a full replay storm.
	//
	// Works with both ManualAck modes:
	//   - ManualAck=false (default): checkpoint advances automatically after each
	//     successful handler return (auto-ack path).
	//   - ManualAck=true: the message passed to the handler intercepts msg.Ack()
	//     and msg.DoubleAck(); the checkpoint advances when the handler calls either.
	//     Calling msg.Nak(), msg.Term(), or msg.NakWithDelay() does not advance it.
	//
	// Not supported by [Queue] consumers (shared durable makes per-process
	// checkpointing nondeterministic across replicas).
	//
	// # WorkQueuePolicy streams
	//
	// Not compatible with WorkQueuePolicy streams. This strategy maps to
	// [jetstream.DeliverByStartSequencePolicy], which NATS rejects on work-queue
	// streams. [Static.Start] and [Dynamic.Update] return [ErrInvalidConfig]
	// when this combination is detected.
	RecoverFromLastProcessed = recovery.FromLastProcessed

	// RecoverFromBeginning recreates the consumer to replay all messages from
	// the beginning of the stream.
	//
	// WARNING: causes a full backlog replay. Use only for small or bounded streams
	// where complete reprocessing is intentional and safe.
	//
	// This is the correct recovery strategy for WorkQueuePolicy streams because
	// it maps to [jetstream.DeliverAllPolicy] — the only DeliverPolicy NATS
	// accepts on work-queue streams. On WorkQueuePolicy, acknowledged messages
	// are deleted immediately, so in practice the "full replay" covers only the
	// unacknowledged backlog at the time of consumer deletion.
	RecoverFromBeginning = recovery.FromBeginning
)

// Option is a functional option that applies to all consumer types.
//
// Common options (like WithLogger, WithAckWait) return this interface, allow them
// to be used with any consumer constructor (NewQueue, NewBroadcast, etc.).
type Option interface {
	apply(*options)
	isQueue()
	isBroadcast()
	isStatic()
	isDynamic()
}

// QueueOption applies only to Queue consumers.
//
// This interface allows both universal Options and Queue-specific options
// to be passed to NewQueue.
// Broadcast/Static/Dynamic specific options do not implement this, enforcing type safety.
type QueueOption interface {
	apply(*options)
	isQueue()
}

// BroadcastOption applies only to Broadcast consumers.
//
// This interface allows both universal Options and Broadcast-specific options
// (like WithInstanceID) to be passed to NewBroadcast.
type BroadcastOption interface {
	apply(*options)
	isBroadcast()
}

// StaticOption applies only to Static consumers.
//
// This interface allows both universal Options and Static-specific options
// (like WithHashSeed) to be passed to NewStatic.
type StaticOption interface {
	apply(*options)
	isStatic()
}

// DynamicOption applies only to Dynamic consumers.
//
// This interface allows both universal Options and Dynamic-specific options
// (like WithProcessingGate) to be passed to NewDynamic.
type DynamicOption interface {
	apply(*options)
	isDynamic()
}

type options struct {
	// Common
	logger            types.Logger
	metrics           types.WorkerConsumerMetrics
	manualAck         bool
	ackWait           time.Duration
	maxDeliver        int
	batchSize         int
	fetchTimeout      time.Duration
	pullHeartbeatCap  time.Duration
	maxWaiting        int
	maxAckPending     int
	inactiveThreshold time.Duration
	ackPolicy         jetstream.AckPolicy

	consumerMemoryStorage bool
	consumerReplicas      int

	// Queue / Broadcast / Dynamic shared
	retry           RetryConfig
	iteratorFactory func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)

	// Recovery
	recoveryStrategy RecoveryStrategy // zero value = RecoveryDisabled

	// Dynamic specific
	iteratorEscalationWindow    time.Duration
	iteratorEscalationThreshold int

	// Broadcast specific
	instanceID string

	// Static specific
	hashSeed         uint64
	dispatchByKey    *bool
	keyChannelBuffer int
	keyIdleTimeout   time.Duration
	keyExtractor     func(msg jetstream.Msg) string

	// Dynamic specific — consumer-create rate limiting (opt-in, default nil/unlimited).
	// consumerCreateLimiter is the resolved limiter to thread into WorkerConsumerConfig.
	// consumerCreatePerSec and consumerCreateBurst are raw inputs from WithConsumerCreateRate.
	// A non-nil injected limiter (from WithConsumerCreateLimiter) wins over the rate option.
	// consumerCreateClusterRate is the aggregate overlay from WithConsumerCreateClusterRate.
	consumerCreateLimiter     ratelimit.Limiter
	consumerCreatePerSec      float64
	consumerCreateBurst       int
	consumerCreateClusterRate float64

	// Dynamic specific
	processingGate                        *ProcessingGateConfig
	resolver                              ResolverConfig
	pullGatingEnabled                     bool
	drainOnRemove                         bool
	drainOnRemoveTimeout                  time.Duration
	maxConcurrentSubjects                 int
	allowWorkerIDChange                   bool
	partitionRefreshMinInterval           time.Duration
	streamMissingHook                     types.StreamMissingHook
	onPermanentFailure                    func(subject string, err error)
	suppressManagerDegradeOnStreamMissing bool
	recoveryRetry                         RecoveryRetryConfig
}

// defaultOptions returns sensible defaults.
func defaultOptions() options {
	return options{
		// Common defaults
		logger:            logging.NewNop(),
		metrics:           metrics.NewNop(),
		ackWait:           30 * time.Second,
		maxDeliver:        -1,
		batchSize:         1,
		fetchTimeout:      5 * time.Second,
		maxWaiting:        2,
		maxAckPending:     0,
		inactiveThreshold: 24 * time.Hour,
		ackPolicy:         jetstream.AckExplicitPolicy,

		// Queue / Broadcast / Dynamic defaults
		retry: RetryConfig{
			Backoff:    100 * time.Millisecond,
			Max:        5 * time.Second,
			Multiplier: 1.6,
			Base:       200 * time.Millisecond,
		},
		iteratorEscalationWindow:    60 * time.Second,
		iteratorEscalationThreshold: 3,
		drainOnRemoveTimeout:        10 * time.Second,
		partitionRefreshMinInterval: 500 * time.Millisecond,
		resolver: ResolverConfig{
			HandoffBucketName:   "parti-handoff",
			HandoffClaimsPrefix: "claims/",
			BatchWindow:         5 * time.Millisecond,
			BatchMaxItems:       1024,
		},
	}
}

// -- Implementation Helpers --

// universalOpt implements Option (and thus all specific options).
type universalOpt func(*options)

func (f universalOpt) apply(o *options) { f(o) }
func (f universalOpt) isQueue()         {}
func (f universalOpt) isBroadcast()     {}
func (f universalOpt) isStatic()        {}
func (f universalOpt) isDynamic()       {}

// broadcastOpt implements BroadcastOption.
type broadcastOpt func(*options)

func (f broadcastOpt) apply(o *options) { f(o) }
func (f broadcastOpt) isBroadcast()     {}

// staticOpt implements StaticOption.
type staticOpt func(*options)

func (f staticOpt) apply(o *options) { f(o) }
func (f staticOpt) isStatic()        {}

// dynamicOpt implements DynamicOption.
type dynamicOpt func(*options)

func (f dynamicOpt) apply(o *options) { f(o) }
func (f dynamicOpt) isDynamic()       {}

// iterOpt implements QueueOption, BroadcastOption, and DynamicOption.
// It is NOT for StaticOption.
type iterOpt func(*options)

func (f iterOpt) apply(o *options) { f(o) }
func (f iterOpt) isQueue()         {}
func (f iterOpt) isBroadcast()     {}
func (f iterOpt) isDynamic()       {}

// -- Common Options --

// WithLogger sets the logger for consumer operations.
//
// If nil is passed, the default no-op logger is retained.
//
// Parameters:
//   - l: Logger implementation (nil is ignored)
func WithLogger(l types.Logger) Option {
	return universalOpt(func(o *options) {
		if l != nil {
			o.logger = l
		}
	})
}

// WithMetrics sets the metrics collector for worker consumer operations.
//
// The consumer package only uses WorkerConsumerMetrics methods. Passing a full
// MetricsCollector (which embeds WorkerConsumerMetrics) also works.
//
// If nil is passed, the default no-op collector is retained.
//
// Parameters:
//   - m: Metrics collector implementation (nil is ignored)
func WithMetrics(m types.WorkerConsumerMetrics) Option {
	return universalOpt(func(o *options) {
		if m != nil {
			o.metrics = m
		}
	})
}

// WithManualAck enables manual acknowledgement.
//
// When true, the handler must call msg.Ack(), msg.Nak(), or msg.Term() explicitly.
// When false (default), returning nil auto-acks and returning an error auto-naks.
//
// [RecoverFromLastProcessed] is compatible with both modes. When ManualAck=true
// the framework intercepts msg.Ack() / msg.DoubleAck() to advance the checkpoint;
// the handler still controls when to ack.
func WithManualAck(enabled bool) Option {
	return universalOpt(func(o *options) {
		o.manualAck = enabled
	})
}

// WithAckWait sets the time allowed for processing before redelivery.
//
// If a message is not acknowledged within this duration, the server
// will redeliver it. Should be longer than expected processing time.
// Values <= 0 are ignored and the default (30s) is retained.
//
// Parameters:
//   - d: Acknowledgement wait duration (must be > 0)
func WithAckWait(d time.Duration) Option {
	return universalOpt(func(o *options) {
		if d > 0 {
			o.ackWait = d
		}
	})
}

// WithMaxDeliver sets the maximum redelivery attempts.
func WithMaxDeliver(n int) Option {
	return universalOpt(func(o *options) {
		if n >= -1 {
			o.maxDeliver = n
		}
	})
}

// WithBatchSize sets the maximum number of messages to pull per request.
//
// Higher batch sizes improve throughput but increase memory usage.
// Values <= 0 are ignored and the default (1) is retained.
//
// Parameters:
//   - n: Batch size (must be > 0)
func WithBatchSize(n int) Option {
	return universalOpt(func(o *options) {
		if n > 0 {
			o.batchSize = n
		}
	})
}

// WithFetchTimeout sets the max time to wait when pulling a batch.
func WithFetchTimeout(d time.Duration) Option {
	return universalOpt(func(o *options) {
		if d > 0 {
			o.fetchTimeout = d
		}
	})
}

// WithPullHeartbeatCap bounds the derived nats.go PullHeartbeat value used by
// the consumer's pull loop. The heartbeat is normally FetchTimeout/2, clamped
// to nats.go's PullHeartbeat validity range [500ms, 30s]; when d > 0, the
// derived heartbeat is further capped to d.
//
// The heartbeat drives missed-heartbeat detection: nats.go's ErrNoHeartbeat
// fires at roughly 2x the heartbeat, which is how a deleted durable consumer
// is detected. Raising FetchTimeout to reduce idle pull-request churn also
// raises the derived heartbeat and therefore that detection latency; this
// cap bounds it independent of FetchTimeout.
//
// 0 (default) disables the cap: the heartbeat stays FetchTimeout/2, capped
// only at 30s — this is the behavior with the option unset. A nonzero d
// outside [500ms, 30s] (nats.go's PullHeartbeat bounds) is rejected at
// construction with ErrInvalidConfig.
func WithPullHeartbeatCap(d time.Duration) Option {
	return universalOpt(func(o *options) {
		o.pullHeartbeatCap = d
	})
}

// WithMaxWaiting caps outstanding pull requests.
func WithMaxWaiting(n int) Option {
	return universalOpt(func(o *options) {
		if n > 0 {
			o.maxWaiting = n
		}
	})
}

// WithMaxAckPending limits in-flight unacknowledged messages.
func WithMaxAckPending(n int) Option {
	return universalOpt(func(o *options) {
		if n >= 0 {
			o.maxAckPending = n
		}
	})
}

// WithInactiveThreshold sets how long an idle consumer is kept before cleanup.
func WithInactiveThreshold(d time.Duration) Option {
	return universalOpt(func(o *options) {
		if d > 0 {
			o.inactiveThreshold = d
		}
	})
}

// WithAckPolicy sets the JetStream ack policy.
func WithAckPolicy(p jetstream.AckPolicy) Option {
	return universalOpt(func(o *options) {
		o.ackPolicy = p
	})
}

// WithRecoveryStrategy enables strategy-aware auto-recovery on consumer deletion
// and controls the DeliverPolicy used when recreating the consumer.
//
// By default, recovery is disabled ([RecoveryDisabled]). When a strategy is set,
// iterator errors that confirm durable deletion trigger automatic recreation using
// the chosen DeliverPolicy. Without a strategy, the consumer retries with backoff
// but does not recreate the durable.
//
// Strategy trade-offs:
//   - [RecoverFromNew]: no data loss risk, but messages published during the outage
//     are skipped. Good default for most cases.
//   - [RecoverFromLastProcessed]: at-least-once delivery from the last acked message.
//     No full replay. Works with both ManualAck modes — see [RecoverFromLastProcessed].
//   - [RecoverFromBeginning]: full stream replay from message 1. Safe only for
//     small or idempotent workloads; causes a replay storm on large streams.
//
// Per-consumer support:
//   - [Queue]: [RecoverFromNew], [RecoverFromBeginning] only. [RecoverFromLastProcessed]
//     is rejected at construction time.
//   - [Broadcast], [Static], [Dynamic]: all strategies supported, including
//     [RecoverFromLastProcessed] with ManualAck=true.
//
// # WorkQueuePolicy streams
//
// NATS only allows [jetstream.DeliverAllPolicy] when creating consumers on
// WorkQueuePolicy streams. [RecoverFromNew] and [RecoverFromLastProcessed] are
// incompatible and will be rejected at startup:
//   - [Queue.Start], [Static.Start]: return [ErrInvalidConfig]
//   - [Dynamic.Update] (first call): returns [ErrInvalidConfig]
//
// Use [RecoverFromBeginning] or [RecoveryDisabled] on WorkQueuePolicy streams.
func WithRecoveryStrategy(strategy RecoveryStrategy) Option {
	return universalOpt(func(o *options) {
		o.recoveryStrategy = strategy
	})
}

// WithStreamMissingHook installs the operator-driven escalation invoked when
// the dynamic partition consumer's recovery flow detects the underlying
// JetStream stream is absent. Configuring the hook is the way an application
// learns "Parti's recovery loop has given up trying to find your stream;
// please recreate it." See [types.StreamMissingHook] for the full operator
// contract, including same-durable-name preservation, compatible-config
// reconciliation, and the post-hook checkpoint reset / epoch fence semantics.
//
// Requires a non-disabled [RecoveryStrategy]. Only
// [RecoverFromLastProcessed] (at-least-once, the common case) and
// [RecoverFromBeginning] (replay-all, intentional duplicate processing) are
// accepted at [NewDynamic] / [DynamicConfig.Validate] time. [RecoveryDisabled]
// and [RecoverFromNew] are rejected because the recreated-stream replay
// override that prevents the fresh-stream skip hazard only applies in the
// at-least-once and from-beginning branches.
//
// Without a hook configured, a stream-missing classification surfaces via
// the iterator-creation envelope's permanent-failure path: the error wraps
// [types.ErrStreamMissing] and the Parti manager routes it through its
// degraded-mode wiring so a readiness probe can rotate the pod.
//
// Applies only to [Dynamic]. The hook must be safe to call from a recovery
// goroutine and should return promptly; long-running hooks delay the
// consumer rebuild and keep the F2 envelope's attempt budget ticking.
func WithStreamMissingHook(hook types.StreamMissingHook) DynamicOption {
	return dynamicOpt(func(o *options) {
		o.streamMissingHook = hook
	})
}

// WithOnPermanentFailure installs a callback fired exactly once per
// partition consumer when its iterator-creation retry envelope or
// stream-missing Site B detour exhausts its attempt budget. The callback
// is the primary observability seam between the bounded recovery loops
// and the application's manager / readiness wiring.
//
// The callback fires synchronously on the consumption loop's goroutine
// immediately before the loop exits. It MUST be non-blocking; long work
// should be offloaded to a goroutine inside the callback so partition
// teardown is not delayed.
//
// The error passed to the callback preserves the wrap chain of the
// underlying cause. When stream-missing exhaustion drove the failure,
// errors.Is(err, [parti.ErrStreamMissing]) is true; the application can
// branch on that to distinguish "stream is gone, operator must recreate
// it" from "iter-create budget exhausted for some other reason".
//
// # Interaction with the Parti manager's auto-degraded route
//
// Registering this callback does NOT disable the Parti manager's
// auto-degraded route. When a [Dynamic] is wired into a [parti.Manager]
// (directly or via a [parti.CompositeConsumerUpdater]), the manager
// installs an observer that — for stream-missing exhaustion only — calls
// [parti.Hooks.OnError] with the wrapped error and then transitions to
// Degraded mode with reason "stream-missing-recovery-exhausted" so the
// readiness probe rotates the pod. Both this callback and the manager
// observer fire (application callback first) for stream-missing exhaustion.
//
// Applications that deliberately own degrade/rotation signaling themselves
// (e.g. they forward stream-missing events to their own readiness wiring
// inside this callback) can suppress the manager observer explicitly via
// [WithSuppressManagerDegradeOnStreamMissing].
//
// Applies only to [Dynamic]. Optional; if unset, exhaustion is logged at
// WARN with metric `iterator_restart{reason="recovery_exhausted"}` or
// `iterator_restart{reason="stream_missing_exhausted"}` but no callback
// fires (unless the [parti.Manager] observer is wired, in which case
// stream-missing exhaustion routes through it).
func WithOnPermanentFailure(fn func(subject string, err error)) DynamicOption {
	return dynamicOpt(func(o *options) {
		o.onPermanentFailure = fn
	})
}

// WithSuppressManagerDegradeOnStreamMissing disables the Parti manager's
// auto-degraded route for stream-missing recovery exhaustion on this
// Dynamic consumer.
//
// By default, when a [Dynamic] is wired into a [parti.Manager], stream-missing
// exhaustion notifies the manager observer (calling [parti.Hooks.OnError] with
// the wrapped error and entering Degraded with reason
// "stream-missing-recovery-exhausted" so the readiness probe rotates the
// pod) IN ADDITION to any application callback registered via
// [WithOnPermanentFailure]. Suppress this only when the application
// deliberately owns degrade/rotation signaling itself — e.g. it forwards
// stream-missing events to its own readiness wiring inside its
// OnPermanentFailure callback.
//
// Applies only to [Dynamic].
func WithSuppressManagerDegradeOnStreamMissing() DynamicOption {
	return dynamicOpt(func(o *options) {
		o.suppressManagerDegradeOnStreamMissing = true
	})
}

// WithRecoveryRetry tunes the bounded-retry envelope wrapped around
// the dynamic partition consumer's iterator-creation path and the
// stream-missing Site B detour. See [RecoveryRetryConfig] for the
// individual fields and their defaults.
//
// Lowering MaxAttempts / BaseBackoff / MaxBackoff is useful when an
// operator prefers fast pod-rotation on a sustained iter-create
// failure (e.g. tighter readiness SLAs). Raising them tolerates
// longer NATS partitions before escalation to OnPermanentFailure
// and degraded mode.
//
// Applies only to [Dynamic]. Zero-valued fields fall back to the
// durable layer's defaults; partial overrides are supported.
func WithRecoveryRetry(cfg RecoveryRetryConfig) DynamicOption {
	return dynamicOpt(func(o *options) {
		o.recoveryRetry = cfg
	})
}

// WithRetry sets the retry backoff configuration.
//
// Supported by: Queue, Broadcast, Dynamic.
// Static consumers use a fixed internal retry and ignore this option.
func WithRetry(cfg RetryConfig) interface {
	QueueOption
	BroadcastOption
	DynamicOption
} {
	return iterOpt(func(o *options) {
		o.retry = cfg
	})
}

// WithConsumerMemoryStorage sets the underlying
// jetstream.ConsumerConfig.MemoryStorage flag on consumer create.
//
// When true, the consumer's delivery and ack state lives in memory
// rather than inheriting the stream's storage type. The published
// message log is unaffected — it stays wherever the stream is
// configured to live.
//
// Trade-off: consumer state is NOT durable across coordinated cluster
// restart. With Replicas ≥ 2 (the default at stream R ≥ 2), single-
// node failure is survivable via raft peers. With Replicas = 1, any
// failure of the consumer-state holder triggers redelivery from
// DeliverPolicy.
//
// IMPORTANT: this option is NOT live-editable on the NATS server.
// Changing the value after the consumer exists requires delete +
// recreate, which drops ack/delivery offsets. If parti starts with a
// different value than the existing consumer was created with, the
// underlying CreateOrUpdateConsumer call fails — NATS rejects the
// storage-type change — and the error surfaces at Start (or Update,
// for Dynamic). parti does NOT auto-delete; that would silently
// drop ack state.
//
// Migration recipe (operator-driven, requires acknowledged
// at-least-once redelivery from DeliverPolicy):
//
//  1. Stop the parti workers (or scale to zero).
//  2. nats consumer rm <STREAM> <CONSUMER>  # drop old consumer
//  3. Restart parti with WithConsumerMemoryStorage(true).
//     The consumer is recreated with the new storage type.
//
// Step 2 wipes ack/delivery offsets; any in-flight messages will be
// redelivered from DeliverPolicy. Only safe for at-least-once
// pipelines with idempotent handlers.
//
// Measured impact: see docs/plans/iops-investigation/findings.md
// §3 for the cost decomposition and §4 for the decision tree.
//
// Default: false (inherit stream storage type).
func WithConsumerMemoryStorage(enabled bool) Option {
	return universalOpt(func(o *options) {
		o.consumerMemoryStorage = enabled
	})
}

// WithConsumerReplicas overrides the underlying
// jetstream.ConsumerConfig.Replicas value at consumer create time.
//
// 0 (the default) inherits the parent stream's replica count. 1
// disables consumer-state raft replication (lowest IOPS, no
// consumer-state HA). Values between 1 and the stream's Replicas
// give intermediate IOPS/HA trade-offs.
//
// Constraints (validated server-side by NATS, surfaced verbatim
// when the underlying JetStream consumer is created or updated —
// parti does not pre-validate. The error surfaces at different
// times per consumer type: Queue/Static/Broadcast at Start; Dynamic
// at Update):
//
//   - On LimitsPolicy streams (parti's default): must be
//     0 ≤ Replicas ≤ stream.Replicas. Values above stream.Replicas
//     are rejected with NATS error code 10126 ("consumer config
//     replica count exceeds parent stream").
//   - On InterestPolicy and WorkQueuePolicy streams: nonzero
//     Replicas must EQUAL stream.Replicas. So on a WorkQueuePolicy
//     stream with stream.Replicas=3, only Replicas ∈ {0, 3} is
//     accepted; Replicas=1 is rejected. This is a NATS-server-side
//     rule, not a parti choice. Practically, ANY parti consumer
//     used on an InterestPolicy or WorkQueuePolicy stream cannot
//     use Replicas=1 — pair the consumer with
//     WithConsumerMemoryStorage(true) alone for the durability-
//     preserving IOPS reduction on those retention policies.
//
// Unlike WithConsumerMemoryStorage, this option IS live-editable on
// the NATS server (`nats consumer edit --replicas=N`); the raft
// group expands/shrinks in place and converges within seconds.
//
// Negative values are silently ignored (defensive guard; matches
// existing With* style).
//
// Default: 0 (inherit stream replicas).
func WithConsumerReplicas(n int) Option {
	return universalOpt(func(o *options) {
		if n >= 0 {
			o.consumerReplicas = n
		}
	})
}

// -- Shared or Specific Options --

// WithIteratorEscalation configures iterator failure escalation.
//
// Only supported by Dynamic consumers. The escalation mechanism uses a
// sliding window to detect bursts of iterator failures and triggers
// consumer recreation when the threshold is exceeded.
func WithIteratorEscalation(window time.Duration, threshold int) DynamicOption {
	return dynamicOpt(func(o *options) {
		if window > 0 {
			o.iteratorEscalationWindow = window
		}
		if threshold > 0 {
			o.iteratorEscalationThreshold = threshold
		}
	})
}

// WithIteratorFactory sets a custom iterator factory (for testing).
// Supported by: Queue, Broadcast, Dynamic.
func WithIteratorFactory(f func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)) interface {
	QueueOption
	BroadcastOption
	DynamicOption
} {
	return iterOpt(func(o *options) {
		o.iteratorFactory = f
	})
}

// -- Broadcast specific --

// WithInstanceID sets the instance ID for broadcast consumers.
// If unset, hostname or env var is used.
func WithInstanceID(id string) BroadcastOption {
	return broadcastOpt(func(o *options) {
		o.instanceID = id
	})
}

// -- Static specific --

// WithHashSeed sets the consistent hashing seed.
func WithHashSeed(seed uint64) StaticOption {
	return staticOpt(func(o *options) {
		o.hashSeed = seed
	})
}

// WithDispatchByKey enables per-key concurrent message processing.
//
// When enabled, messages are routed to separate goroutines based on their key.
// Messages with the same key are processed sequentially (preserving order),
// while different keys are processed concurrently in parallel goroutines.
//
// IMPORTANT: SubjectPattern MUST contain {{key}} placeholder when DispatchByKey
// is enabled. The key is extracted based on the {{key}} position in the pattern.
// For example, with pattern "events.{{partition}}.{{key}}" and subject
// "events.0.customer-abc", the key is "customer-abc".
//
// Placeholders must occupy a full token between dots. Embedded placeholders
// like "events.{{key}}-v1.{{partition}}" are invalid.
//
// WARNING: This creates an UNBOUNDED number of goroutines - one goroutine per
// unique key. If your workload has millions of unique keys, memory usage will
// grow proportionally. Goroutines are cleaned up after KeyIdleTimeout of
// inactivity.
//
// Use this when:
//   - You need per-key ordering but want parallelism across keys
//   - Your key cardinality is bounded (e.g., thousands, not millions)
//   - Slow processing of one key should not block other keys
func WithDispatchByKey() StaticOption {
	v := true
	return staticOpt(func(o *options) {
		o.dispatchByKey = &v
	})
}

// WithKeyChannelBuffer sets the buffer size for each key's message channel.
//
// When the buffer is full, the main pull loop blocks (backpressure).
// Larger buffers absorb bursts but use more memory per active key.
//
// Only used when DispatchByKey is enabled.
// Default: 32
func WithKeyChannelBuffer(size int) StaticOption {
	return staticOpt(func(o *options) {
		if size > 0 {
			o.keyChannelBuffer = size
		}
	})
}

// WithKeyIdleTimeout sets how long an idle key goroutine waits before exiting.
//
// After this duration with no messages, the goroutine exits and is removed.
// A new goroutine is created if messages for that key arrive later.
//
// Only used when DispatchByKey is enabled.
// Default: 30s
func WithKeyIdleTimeout(d time.Duration) StaticOption {
	return staticOpt(func(o *options) {
		if d > 0 {
			o.keyIdleTimeout = d
		}
	})
}

// WithKeyExtractor sets a custom key extraction function.
//
// The extracted key determines which goroutine processes the message.
// Messages with the same key are guaranteed to be processed sequentially.
//
// If not set, uses a pattern-aware extractor based on the {{key}} position in
// SubjectPattern. For example, with pattern "events.{{partition}}.{{key}}"
// and subject "events.0.customer-abc", the key is "customer-abc".
//
// Only used when DispatchByKey is enabled.
func WithKeyExtractor(fn func(msg jetstream.Msg) string) StaticOption {
	return staticOpt(func(o *options) {
		o.keyExtractor = fn
	})
}

// -- Dynamic specific --

// WithProcessingGate enables processing gate with given config.
func WithProcessingGate(cfg *ProcessingGateConfig) DynamicOption {
	return dynamicOpt(func(o *options) {
		o.processingGate = cfg
	})
}

// WithResolver configures the ownership resolver.
func WithResolver(cfg ResolverConfig) DynamicOption {
	return dynamicOpt(func(o *options) {
		o.resolver = cfg
	})
}

// WithPullGating enables pre-pull ownership checks.
func WithPullGating(enabled bool) DynamicOption {
	return dynamicOpt(func(o *options) {
		o.pullGatingEnabled = enabled
	})
}

// WithDrainOnRemove configures drain behavior on subject removal.
//
// When enabled, removed partition loops drain buffered messages before
// shutting down. Draining is bounded by DrainOnRemoveTimeout; if the loops
// fail to stop within that bound, [Dynamic.Update] returns an error and the
// manager retries the apply. An already-in-flight handler invocation may still
// run to completion (best-effort, not a zero-overlap guarantee).
//
// Parameters:
//   - enabled: Whether to enable graceful draining
//   - timeout: Maximum time to wait for draining (ignored if <= 0)
func WithDrainOnRemove(enabled bool, timeout time.Duration) DynamicOption {
	return dynamicOpt(func(o *options) {
		o.drainOnRemove = enabled
		if timeout > 0 {
			o.drainOnRemoveTimeout = timeout
		}
	})
}

// WithMaxConcurrentSubjects caps concurrent per-subject consumers.
func WithMaxConcurrentSubjects(n int) DynamicOption {
	return dynamicOpt(func(o *options) {
		if n > 0 {
			o.maxConcurrentSubjects = n
		}
	})
}

// WithAllowWorkerIDChange enables worker ID mutability (advanced).
func WithAllowWorkerIDChange(enabled bool) DynamicOption {
	return dynamicOpt(func(o *options) {
		o.allowWorkerIDChange = enabled
	})
}

// WithPartitionRefreshMinInterval sets the min interval for partition refresh.
func WithPartitionRefreshMinInterval(d time.Duration) DynamicOption {
	return dynamicOpt(func(o *options) {
		if d > 0 {
			o.partitionRefreshMinInterval = d
		}
	})
}

// WithConsumerCreateRate enables a per-attempt token-bucket rate limit on
// consumer-create RPCs (initial assignment add + recovery recreation), at the
// given steady rate (events/second) with the given burst size.
//
// This is opt-in. When not configured (the default), behaviour is unchanged.
//
// The rate applies to every physical CreateOrUpdateConsumer attempt, including
// retries, so transient-error storms are paced at the same rate as normal
// creates — preventing up to 3× overshoot that would otherwise occur if
// gating were per-logical-create only.
//
// Validation: perSec must be >= 0; perSec == 0 leaves the limiter disabled
// (no rate limiting); burst must be >= 1 when perSec > 0. Invalid values are
// rejected at [NewDynamic], which returns an error wrapping [ErrInvalidConfig]
// when perSec < 0, or when perSec > 0 with burst < 1. Use
// [WithConsumerCreateLimiter] to supply a custom or shared limiter instead.
//
// Sizing guidance: rate ≈ cluster-create-budget / max-workers.
// Recommended starting values (validate by load test): rate ≈ 100/s, burst ≈ 256.
//
// # Interaction with handoff and readiness
//
// A paced apply holds the Dynamic apply lock for its duration, serialising
// subsequent applies and blocking Close. With the processing gate OFF, enabling pacing
// lengthens the period during which old and new owners are both active
// (processing-overlap window); co-enable the processing gate / pull-gating to
// suppress that overlap. A large cold start (e.g. 20 000 partitions at 100/s ≈
// 200s) may trip StartupTimeout (default 60s) — size it accordingly or accept
// a one-shot startup-degraded rotation.
//
// Applies only to [Dynamic].
func WithConsumerCreateRate(perSec float64, burst int) DynamicOption {
	return dynamicOpt(func(o *options) {
		o.consumerCreatePerSec = perSec
		o.consumerCreateBurst = burst
	})
}

// WithConsumerCreateClusterRate makes consumer-create rate limiting
// fleet-size-aware. Given a cluster-wide target (events/second), each worker
// enforces min(perWorkerCeiling, clusterPerSec/N), where perWorkerCeiling is
// the rate from [WithConsumerCreateRate] and N is the cluster worker-count the
// Parti Manager observes and pushes to this consumer.
//
// This bounds the STEADY-STATE cluster-wide create rate to clusterPerSec
// instead of N*perWorkerCeiling. The per-worker ceiling still caps any single
// worker during fleet-size transitions.
//
// Requires [WithConsumerCreateRate] (which supplies burst and the ceiling) and
// the built-in limiter; it is rejected at [NewDynamic] when used alone or with
// an injected [WithConsumerCreateLimiter] (an injected, possibly shared limiter
// is not adaptively retuned). clusterPerSec must be >= 0; 0 disables the
// overlay (static per-worker behaviour).
//
// Applies only to [Dynamic].
func WithConsumerCreateClusterRate(clusterPerSec float64) DynamicOption {
	return dynamicOpt(func(o *options) {
		o.consumerCreateClusterRate = clusterPerSec
	})
}

// WithConsumerCreateLimiter injects a custom or shared [ConsumerCreateLimiter]
// that gates every physical CreateOrUpdateConsumer attempt. Use this when
// multiple [Dynamic] consumers in the same process should share one rate budget
// across all their consumer creates; build the shared limiter with
// [NewConsumerCreateLimiter]. For a single consumer prefer [WithConsumerCreateRate].
//
// Precedence rules (any option order):
//   - A non-nil injected limiter wins over [WithConsumerCreateRate].
//   - Passing nil is a no-op: it does NOT clear a configured rate limiter.
//
// An injected or shared limiter bypasses the per-consumer throttle metrics that
// [WithConsumerCreateRate] wires up (a shared budget has no single owning
// consumer to attribute throttle events to).
//
// # Lock-order contract
//
// The injected limiter's Wait(ctx) is invoked while the Dynamic apply/update
// locks may be held. It MUST honour context cancellation and MUST NOT call back
// into Manager, Dynamic, or any operation that acquires those locks. See
// [ConsumerCreateLimiter].
//
// Applies only to [Dynamic].
func WithConsumerCreateLimiter(l ConsumerCreateLimiter) DynamicOption {
	return dynamicOpt(func(o *options) {
		if l != nil {
			o.consumerCreateLimiter = l
		}
		// nil is a no-op (spec: does not clear a configured rate).
	})
}
