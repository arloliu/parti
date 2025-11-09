package subscription

import (
	"time"

	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/internal/metrics"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
)

// WorkerConsumerConfig configures the single durable per-worker consumer helper.
//
// Required fields:
//   - StreamName: JetStream stream name containing the subjects
//   - ConsumerPrefix: Prefix for durable name; final name is "<ConsumerPrefix>-<workerID>"
//   - SubjectTemplate: text/template expanded per partition with {{.PartitionID}}
//
// Optional tuning fields are documented inline below. Zero values are replaced by
// sensible defaults via applyDefaults().
type WorkerConsumerConfig struct {
	// StreamName is the JetStream stream name where subjects live. Required.
	StreamName string

	// ConsumerPrefix is the durable name prefix; the durable will be
	// "<ConsumerPrefix>-<workerID>". Required.
	ConsumerPrefix string

	// SubjectTemplate is a text/template used to build subjects from a
	// partition. Available field: {{.PartitionID}} which equals partition.SubjectKey().
	// Example: "work.{{.PartitionID}}" => work.source.us-east-1
	// Required.
	SubjectTemplate string

	// AckPolicy controls JetStream ack policy. Defaults to AckExplicitPolicy.
	AckPolicy jetstream.AckPolicy
	// AckWait is the time allowed for processing before redelivery.
	// Default: DefaultAckWait (30s).
	AckWait time.Duration
	// MaxDeliver is the maximum redelivery attempts before moving to DLQ (if configured).
	// Default: DefaultMaxDeliver (3).
	MaxDeliver int
	// InactiveThreshold is how long an idle consumer is kept by the server before cleanup.
	// Default: DefaultInactiveThreshold (24h).
	InactiveThreshold time.Duration

	// BatchSize is the max number of messages to pull per iterator request.
	// Default: DefaultBatchSize (1).
	BatchSize int
	// MaxWaiting is the max outstanding pull requests allowed for the consumer.
	// Default: DefaultMaxWaiting (512).
	MaxWaiting int
	// FetchTimeout is the max time to wait when pulling a batch.
	// Default: DefaultFetchTimeout (5s).
	FetchTimeout time.Duration

	// MaxRetries is the number of retries for CreateOrUpdateConsumer on errors.
	// Default: DefaultMaxRetries (3).
	MaxRetries int
	// RetryBackoff is the delay between retries for control-plane operations.
	// Default: DefaultRetryBackoff (100ms).
	RetryBackoff time.Duration

	// RetryBase is the base backoff used for decorrelated jitter retries for control-plane ops.
	// Default: DefaultRetryBase (200ms). If zero and RetryBackoff is set, RetryBase falls back to RetryBackoff.
	RetryBase time.Duration
	// RetryMultiplier grows the backoff window for decorrelated jitter.
	// Default: DefaultRetryMultiplier (1.6).
	RetryMultiplier float64
	// RetryMax caps the jittered backoff.
	// Default: DefaultRetryMax (5s).
	RetryMax time.Duration
	// MaxControlRetries caps the number of retry attempts for control-plane ops.
	// Default: DefaultMaxControlRetries (6). If zero, falls back to MaxRetries.
	MaxControlRetries int
	// RetrySeed optionally seeds the jitter RNG for deterministic tests; when zero, a random seed is used.
	RetrySeed int64

	// MaxSubjects caps the number of subjects set on the worker consumer.
	// Default: DefaultMaxSubjects (500).
	MaxSubjects int
	// AllowWorkerIDChange controls whether workerID changes are allowed after initialization.
	// Default: false (immutable once set). Intended for controlled migrations only.
	AllowWorkerIDChange bool

	// HealthFailureThreshold controls how many consecutive iterator/handler failures
	// are tolerated before the worker consumer is considered unhealthy for metrics.
	// Default: DefaultHealthFailureThreshold (3). If <=0, defaults to 3.
	HealthFailureThreshold int

	// MaxRecreationRetries caps the number of attempts to recreate a missing/invalid consumer.
	// Defaults to DefaultMaxControlRetries if zero.
	MaxRecreationRetries int

	// Logger provides structured logging. Defaults to a no-op logger when nil.
	Logger types.Logger

	// Metrics is the global metrics collector used across the library.
	// If nil, no metrics are emitted from the worker consumer helper.
	Metrics types.MetricsCollector
}

// applyDefaults fills unset optional fields with project defaults.
func (cfg *WorkerConsumerConfig) applyDefaults() {
	cfg.AckPolicy = defaultAckPolicy(cfg.AckPolicy)
	cfg.AckWait = defaultDuration(cfg.AckWait, DefaultAckWait)
	cfg.MaxDeliver = defaultInt(cfg.MaxDeliver, DefaultMaxDeliver)
	cfg.InactiveThreshold = defaultDuration(cfg.InactiveThreshold, DefaultInactiveThreshold)
	cfg.BatchSize = defaultInt(cfg.BatchSize, DefaultBatchSize)
	cfg.MaxWaiting = defaultInt(cfg.MaxWaiting, DefaultMaxWaiting)
	cfg.FetchTimeout = defaultDuration(cfg.FetchTimeout, DefaultFetchTimeout)
	cfg.MaxRetries = defaultInt(cfg.MaxRetries, DefaultMaxRetries)
	cfg.RetryBackoff = defaultDuration(cfg.RetryBackoff, DefaultRetryBackoff)
	cfg.RetryBase = defaultRetryBase(cfg.RetryBase, cfg.RetryBackoff, DefaultRetryBase)
	cfg.RetryMultiplier = defaultFloat(cfg.RetryMultiplier, DefaultRetryMultiplier)
	cfg.RetryMax = defaultDuration(cfg.RetryMax, DefaultRetryMax)
	cfg.MaxControlRetries = defaultMaxControlRetries(cfg.MaxControlRetries, cfg.MaxRetries, DefaultMaxControlRetries)
	cfg.MaxSubjects = defaultInt(cfg.MaxSubjects, DefaultMaxSubjects)
	if cfg.HealthFailureThreshold <= 0 { // allow explicit zero check per spec
		cfg.HealthFailureThreshold = DefaultHealthFailureThreshold
	}
	cfg.MaxRecreationRetries = defaultMaxControlRetries(cfg.MaxRecreationRetries, cfg.MaxControlRetries, DefaultMaxControlRetries)
	if cfg.Logger == nil {
		cfg.Logger = logging.NewNop()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.NewNop()
	}
}

// helper defaults (unexported)
func defaultAckPolicy(p jetstream.AckPolicy) jetstream.AckPolicy {
	if p == 0 {
		return jetstream.AckExplicitPolicy
	}

	return p
}

func defaultDuration(v, def time.Duration) time.Duration {
	if v == 0 {
		return def
	}

	return v
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}

	return v
}

func defaultFloat(v, def float64) float64 {
	if v == 0 {
		return def
	}

	return v
}

func defaultRetryBase(base, backoff, def time.Duration) time.Duration {
	if base == 0 {
		if backoff > 0 {
			return backoff
		}

		return def
	}

	return base
}

func defaultMaxControlRetries(v, fallback, def int) int {
	if v == 0 {
		if fallback == 0 {
			return def
		}

		return fallback
	}

	return v
}
