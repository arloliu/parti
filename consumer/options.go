package consumer

import (
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
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
	metrics           types.MetricsCollector
	manualAck         bool
	ackWait           time.Duration
	maxDeliver        int
	batchSize         int
	fetchTimeout      time.Duration
	maxWaiting        int
	maxAckPending     int
	inactiveThreshold time.Duration
	ackPolicy         jetstream.AckPolicy

	// Queue / Broadcast / Dynamic shared
	retry           RetryConfig
	iteratorFactory func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error)

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

	// Dynamic specific
	processingGate              *ProcessingGateConfig
	resolver                    ResolverConfig
	pullGatingEnabled           bool
	drainOnRemove               bool
	drainOnRemoveTimeout        time.Duration
	maxConcurrentSubjects       int
	allowWorkerIDChange         bool
	partitionRefreshMinInterval time.Duration
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

// WithMetrics sets the metrics collector for consumer operations.
//
// If nil is passed, the default no-op collector is retained.
//
// Parameters:
//   - m: Metrics collector implementation (nil is ignored)
func WithMetrics(m types.MetricsCollector) Option {
	return universalOpt(func(o *options) {
		if m != nil {
			o.metrics = m
		}
	})
}

// WithManualAck enables manual acknowledgement.
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
// When enabled, revoked partitions will finish processing buffered messages
// before shutting down. The timeout caps the drain duration.
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
