package subscription

import (
	"time"

	"github.com/arloliu/parti/internal/logging"
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

	// Logger provides structured logging. Defaults to a no-op logger when nil.
	Logger types.Logger
}

// applyDefaults fills unset optional fields with project defaults.
func (cfg *WorkerConsumerConfig) applyDefaults() {
	if cfg.AckPolicy == 0 {
		cfg.AckPolicy = jetstream.AckExplicitPolicy
	}
	if cfg.AckWait == 0 {
		cfg.AckWait = DefaultAckWait
	}
	if cfg.MaxDeliver == 0 {
		cfg.MaxDeliver = DefaultMaxDeliver
	}
	if cfg.InactiveThreshold == 0 {
		cfg.InactiveThreshold = DefaultInactiveThreshold
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.MaxWaiting == 0 {
		cfg.MaxWaiting = DefaultMaxWaiting
	}
	if cfg.FetchTimeout == 0 {
		cfg.FetchTimeout = DefaultFetchTimeout
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = DefaultMaxRetries
	}
	if cfg.RetryBackoff == 0 {
		cfg.RetryBackoff = DefaultRetryBackoff
	}
	if cfg.Logger == nil {
		cfg.Logger = logging.NewNop()
	}
}
