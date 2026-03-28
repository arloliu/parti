package consumer

import "time"

// Default configuration values for consumer types.
//
// These constants cover the most tuning-relevant settings. Internal retry,
// escalation, and health-check defaults remain unexported in the implementation.
const (
	// DefaultAckWait is the default time allowed for processing a message
	// before it is considered lost and re-delivered.
	DefaultAckWait = 30 * time.Second

	// DefaultMaxDeliver is the default maximum delivery attempts.
	// -1 means unlimited re-delivery.
	DefaultMaxDeliver = -1

	// DefaultInactiveThreshold is the default duration after which an idle
	// JetStream consumer is automatically deleted by the server.
	DefaultInactiveThreshold = 24 * time.Hour

	// DefaultFetchBatchSize is the default number of messages to pull from
	// the server in a single fetch request.
	DefaultFetchBatchSize = 1

	// DefaultFetchMaxWait is the default maximum duration to wait for a
	// batch of messages during a fetch.
	DefaultFetchMaxWait = 5 * time.Second

	// DefaultMaxAckPending is the default number of messages that can be
	// in-flight (unacknowledged) per consumer. Set to 1 for strict ordered
	// processing. Increase for higher throughput at the cost of ordering.
	DefaultMaxAckPending = 1

	// DefaultMaxWaiting is the default maximum number of outstanding pull
	// requests. Set conservatively to 2 to prevent thundering-herd issues
	// when managing many partitions per worker.
	DefaultMaxWaiting = 2
)
