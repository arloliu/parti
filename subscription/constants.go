package subscription

import "time"

// Default configuration values for WorkerConsumer.
const (
	// DefaultBatchSize is the default number of messages to fetch per pull request.
	DefaultBatchSize = 1

	// DefaultMaxWaiting is the default maximum number of outstanding pull requests.
	DefaultMaxWaiting = 512

	// DefaultFetchTimeout is the default maximum duration to wait for messages.
	DefaultFetchTimeout = 5 * time.Second

	// DefaultMaxRetries is the default maximum number of retry attempts.
	DefaultMaxRetries = 3

	// DefaultRetryBackoff is the default duration between retry attempts.
	DefaultRetryBackoff = 100 * time.Millisecond

	// DefaultAckWait is the default duration to wait for acknowledgment.
	DefaultAckWait = 30 * time.Second

	// DefaultMaxDeliver is the default maximum delivery attempts.
	DefaultMaxDeliver = 3

	// DefaultInactiveThreshold is the default inactive consumer cleanup threshold.
	DefaultInactiveThreshold = 24 * time.Hour
)
