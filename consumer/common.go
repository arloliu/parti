package consumer

import (
	"context"
	"fmt"
	"time"

	"github.com/arloliu/fuda"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// CommonConfig contains configuration fields shared by all consumer types.
// Embedding this in each consumer's config ensures consistent naming and defaults.
//
// These settings control the low-level behavior of the JetStream consumer,
// including acknowledgement policies, batching, timeouts, and redelivery.
type CommonConfig struct {
	// Logger provides structured logging for the consumer.
	// If nil, a no-op logger is used.
	Logger types.Logger

	// Metrics is the metrics collector for worker consumer operations.
	// If nil, a no-op collector is used.
	Metrics types.WorkerConsumerMetrics

	// ManualAck disables automatic acknowledgement of messages.
	//
	// When false (default):
	//  - If the handler returns nil, the message is automatically acknowledged.
	//  - If the handler returns an error, the message is negatively acknowledged (Nak).
	//
	// When true:
	//  - The handler MUST explicitly call msg.Ack(), msg.Nak(), or msg.Term().
	//  - A returned handler error is discarded (neither logged nor acted on);
	//    surface failures from inside the handler itself.
	ManualAck bool

	// AckWait is the time allowed for processing a message before it is considered lost
	// and re-delivered by the server.
	//
	// This should be longer than the expected maximum processing time of a single message.
	//
	// Default: 30s.
	AckWait time.Duration `default:"30s" validate:"gt=0"`

	// MaxDeliver is the maximum number of times a message will be delivered.
	//
	// If a message fails processing (Nak) or times out (AckWait) this many times,
	// it will be terminated or moved to a Dead Letter Queue (if configured on the stream).
	//
	// Default: -1 (unlimited).
	MaxDeliver int `default:"-1" validate:"gte=-1"`

	// BatchSize is the maximum number of messages to pull from the server in a single request.
	//
	// A higher batch size can improve throughput but typically increases memory usage
	// and potentially latency for individual messages if processing is slow.
	//
	// Default: 1.
	BatchSize int `default:"1" validate:"gt=0"`

	// FetchTimeout is the maximum duration to wait for a batch of messages to arrive
	// when pulling from the server.
	//
	// If no messages are available within this timeout, the pull request expires.
	// The consumer loop manages this automatically.
	//
	// Minimum: 1s. NATS rejects a PullExpiry below 1s at iterator-creation
	// time; a sub-second value previously caused Start to return success and
	// then fail every iterator creation forever — a permanently dead consumer
	// with no terminal signal. Construction now fails fast instead.
	//
	// Default: 5s.
	FetchTimeout time.Duration `default:"5s" validate:"gt=0"`

	// PullHeartbeatCap optionally bounds the derived nats.go PullHeartbeat
	// value used by the consumer's pull loop. The heartbeat is normally
	// FetchTimeout/2, clamped to nats.go's PullHeartbeat validity range
	// [500ms, 30s]; when PullHeartbeatCap > 0 the derived heartbeat is
	// further capped to this value.
	//
	// The heartbeat drives missed-heartbeat detection: nats.go's
	// ErrNoHeartbeat fires at roughly 2x the heartbeat, which is how a
	// deleted durable consumer is detected. Raising FetchTimeout to reduce
	// idle pull-request churn also raises the derived heartbeat and
	// therefore that detection latency; this cap bounds it independent of
	// FetchTimeout.
	//
	// 0 (default) disables the cap: the heartbeat stays FetchTimeout/2,
	// capped only at 30s. A nonzero value outside [500ms, 30s] (nats.go's
	// PullHeartbeat bounds) is rejected at construction with
	// ErrInvalidConfig. See [WithPullHeartbeatCap].
	PullHeartbeatCap time.Duration `validate:"gte=0"`

	// MaxWaiting is the maximum number of outstanding pull requests allowed.
	//
	// This controls the pre-fetch buffer. A value of 2 with BatchSize of 1 means
	// there can be 2 messages buffered locally (1 being processed, 1 ready).
	//
	// Default: 2.
	MaxWaiting int `default:"2" validate:"gt=0"`

	// MaxAckPending limits the number of messages that can be in-flight (unacknowledged)
	// at any given time.
	//
	// If the limit is reached, the server will pause delivery until some messages are acknowledged.
	// If zero, the server's consumer default is used.
	MaxAckPending int `validate:"gte=0"`

	// InactiveThreshold is the duration after which an idle consumer (with no active subscriptions)
	// will be automatically deleted by the server.
	//
	// For durable consumers, this should be set high enough to survive application restarts.
	//
	// Default: 24h.
	InactiveThreshold time.Duration `default:"24h" validate:"gt=0"`

	// AckPolicy controls the JetStream acknowledgement policy.
	//
	// Typically set to AckExplicitPolicy for reliable processing.
	// Defaults to AckExplicitPolicy if usually not set manually.
	AckPolicy jetstream.AckPolicy

	// ConsumerMemoryStorage, when true, sets the underlying
	// jetstream.ConsumerConfig.MemoryStorage flag on consumer create,
	// keeping the consumer's delivery/ack state in memory rather than
	// inheriting the stream's storage type.
	//
	// Default: false (inherit stream storage).
	//
	// Trade-off: the consumer's delivery/ack offsets are NOT durable
	// across coordinated cluster restart. With ConsumerReplicas ≥ 2,
	// single-node failure is still survivable via raft peers. With
	// ConsumerReplicas = 1, any failure of the consumer-state holder
	// loses ack state and triggers redelivery from DeliverPolicy.
	//
	// IMPORTANT: this field is NOT live-editable on the NATS server.
	// Changing it after the consumer exists requires delete + recreate,
	// which drops ack/delivery offsets. Pick the value at construction
	// time.
	//
	// For at-least-once work-queue patterns with idempotent handlers
	// this is typically safe and yields a large IOPS reduction. See
	// docs/plans/iops-investigation/findings.md §2 for measurements
	// and §4 for the operator decision tree.
	ConsumerMemoryStorage bool

	// ConsumerReplicas overrides the underlying
	// jetstream.ConsumerConfig.Replicas value at consumer create time.
	//
	// Default: 0 (inherit the stream's replica count). Set to 1 to
	// disable consumer-state raft replication (lowest IOPS, no
	// consumer-state HA). Values between 1 and the stream's replica
	// count give intermediate IOPS/HA trade-offs.
	//
	// Constraint: must be ≤ the parent stream's Replicas. NATS rejects
	// invalid values at consumer create with error code 10126
	// ("consumer config replica count exceeds parent stream"). parti
	// does not pre-validate; the JetStream error is surfaced verbatim
	// when the underlying consumer is created or updated
	// (Queue/Static/Broadcast at Start, Dynamic at Update).
	//
	// Unlike ConsumerMemoryStorage, this field IS live-editable on
	// the NATS server via `nats consumer edit --replicas=N`; the raft
	// group expands/shrinks in place.
	ConsumerReplicas int `validate:"gte=0"`
}

// SetDefaults applies default values to the configuration.
func (c *CommonConfig) SetDefaults() error {
	if err := fuda.SetDefaults(c); err != nil {
		return err
	}
	if c.Logger == nil {
		c.Logger = logging.NewNop()
	}
	if c.Metrics == nil {
		c.Metrics = metrics.NewNop()
	}

	return nil
}

// Validate checks configuration constraints.
func (c *CommonConfig) Validate() error {
	if err := c.SetDefaults(); err != nil {
		return err
	}

	if err := fuda.Validate(c); err != nil {
		return err
	}

	if err := validateFetchTimeoutFloor(c.FetchTimeout); err != nil {
		return err
	}

	return validatePullHeartbeatCap(c.PullHeartbeatCap)
}

// validateFetchTimeoutFloor enforces NATS's 1s PullExpiry minimum. Each
// consumer config's Validate calls this directly (they run fuda on their own
// struct rather than delegating to CommonConfig.Validate), so the floor lives
// in one place. A sub-second value previously passed validation and produced
// a consumer whose Start succeeded and then failed every iterator creation.
func validateFetchTimeoutFloor(ft time.Duration) error {
	if ft < time.Second {
		return fmt.Errorf("%w: FetchTimeout must be at least 1s (NATS PullExpiry floor), got %v", ErrInvalidConfig, ft)
	}

	return nil
}

// validatePullHeartbeatCap enforces nats.go's PullHeartbeat validity range
// [500ms, 30s] (jetstream_options.go configureConsume/configureMessages,
// nats.go v1.52.0) on a nonzero PullHeartbeatCap. Zero is always accepted:
// it means "no cap", not "zero heartbeat". Each consumer config's Validate
// calls this directly, mirroring validateFetchTimeoutFloor above.
func validatePullHeartbeatCap(heartbeatCap time.Duration) error {
	if heartbeatCap == 0 {
		return nil
	}
	if heartbeatCap < natsutil.MinPullHeartbeat || heartbeatCap > natsutil.MaxPullHeartbeat {
		return fmt.Errorf(
			"%w: PullHeartbeatCap must be 0 (disabled) or within [500ms, 30s] (nats.go PullHeartbeat range), got %v",
			ErrInvalidConfig, heartbeatCap)
	}

	return nil
}

// CheckWorkQueueRecoveryCompat returns ErrInvalidConfig when the stream uses
// WorkQueuePolicy and the recovery strategy requires a non-DeliverAllPolicy.
// NATS only permits DeliverAllPolicy on work-queue streams; RecoverFromNew
// (DeliverNewPolicy) and RecoverFromLastProcessed (DeliverByStartSequencePolicy)
// would silently fail during every recovery attempt.
//
// The check is best-effort: failures to fetch stream info are silently ignored
// so callers are not blocked by transient connectivity issues. This means
// transient JetStream API failures during the pre-flight do not block consumer
// updates; the runtime continues as if the check passed.
//
// For [Dynamic], the per-consumer outcome is cached: a pass recorded during a
// transient fetch failure is not re-evaluated until a stream-recreate resets
// the check, so a genuinely incompatible configuration may go undetected
// until recovery first misbehaves.
//
// This function is exported so the provision SDK can reuse the exact same
// implementation in its dynamic-consumer alignment check
// (see provision.ValidateLiveDynamicConsumers). Reusing rather than
// duplicating guarantees byte-equivalent error semantics with the runtime
// Dynamic.Update path.
func CheckWorkQueueRecoveryCompat(ctx context.Context, js jetstream.JetStream, streamName string, strategy RecoveryStrategy) error {
	var strategyName string
	switch strategy {
	case RecoverFromNew:
		strategyName = "RecoverFromNew"
	case RecoverFromLastProcessed:
		strategyName = "RecoverFromLastProcessed"
	default:
		return nil // RecoveryDisabled and RecoverFromBeginning are always valid
	}

	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		return nil
	}

	info, err := stream.Info(ctx)
	if err != nil {
		return nil
	}

	if info.Config.Retention != jetstream.WorkQueuePolicy {
		return nil
	}

	return fmt.Errorf(
		"%w: %s is incompatible with WorkQueuePolicy stream %q"+
			" — NATS only permits DeliverAllPolicy on work-queue streams;"+
			" use RecoverFromBeginning or RecoveryDisabled instead",
		ErrInvalidConfig, strategyName, streamName,
	)
}

// RetryConfig groups retry backoff settings.
type RetryConfig struct {
	// Backoff is the delay between retries for control-plane operations.
	// Default: 100ms.
	Backoff time.Duration `default:"100ms" validate:"gte=0"`

	// Max caps the jittered backoff.
	// Default: 5s.
	Max time.Duration `default:"5s" validate:"gte=0,gtefield=Backoff"`

	// Multiplier grows the backoff window for decorrelated jitter.
	// Default: 1.6.
	Multiplier float64 `default:"1.6" validate:"gte=1"`

	// Base is the base backoff used for decorrelated jitter retries.
	// Default: 200ms. If zero and Backoff is set, Base falls back to Backoff.
	Base time.Duration `default:"200ms" validate:"gte=0"`

	// Seed optionally seeds the jitter RNG for deterministic tests.
	// When zero, a random seed is used.
	Seed int64
}
