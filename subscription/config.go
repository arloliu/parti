package subscription

import (
	"time"

	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
)

// DurableConfig configures the durable consumer helper.
//
// Required fields:
//   - StreamName
//   - ConsumerPrefix
//   - SubjectTemplate
//
// Optional tuning fields are documented inline below. Zero values are replaced by
// sensible defaults via applyDefaults().
type DurableConfig struct {
	StreamName      string
	ConsumerPrefix  string
	SubjectTemplate string

	AckPolicy         jetstream.AckPolicy
	AckWait           time.Duration
	MaxDeliver        int
	InactiveThreshold time.Duration

	BatchSize    int
	MaxWaiting   int
	FetchTimeout time.Duration

	MaxRetries   int
	RetryBackoff time.Duration

	Logger types.Logger
}

// applyDefaults fills unset optional fields with project defaults.
func (cfg *DurableConfig) applyDefaults() {
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
