package durable

import "time"

// Default configuration values for WorkerConsumer.
const (
	// DefaultBatchSize is the default number of messages to fetch per pull request.
	DefaultBatchSize = 1

	// DefaultMaxWaiting is the default maximum number of outstanding pull requests.
	// This is set conservatively to 2 to prevent "thundering herd" issues when
	// managing many partitions per worker.
	DefaultMaxWaiting = 2

	// DefaultFetchTimeout is the default maximum duration to wait for messages.
	DefaultFetchTimeout = 5 * time.Second

	// DefaultMaxRetries is the default maximum number of retry attempts.
	DefaultMaxRetries = 3

	// DefaultRetryBackoff is the default duration between retry attempts.
	DefaultRetryBackoff = 100 * time.Millisecond

	// DefaultRetryBase is the default base backoff for decorrelated jitter.
	DefaultRetryBase = 200 * time.Millisecond

	// DefaultRetryMultiplier is the default multiplier used to grow backoff.
	DefaultRetryMultiplier = 1.6

	// DefaultRetryMax is the default maximum backoff cap for control-plane retries.
	DefaultRetryMax = 5 * time.Second

	// DefaultMaxControlRetries is the default maximum control-plane retry attempts.
	DefaultMaxControlRetries = 6

	// DefaultAckWait is the default duration to wait for acknowledgment.
	DefaultAckWait = 30 * time.Second

	// DefaultMaxDeliver is the default maximum delivery attempts.
	DefaultMaxDeliver = -1

	// DefaultInactiveThreshold is the default inactive consumer cleanup threshold.
	DefaultInactiveThreshold = 24 * time.Hour

	// DefaultMaxSubjects is the default cap for filter subjects applied to a worker consumer.
	DefaultMaxSubjects = 3000

	// DefaultHealthFailureThreshold is the default consecutive failure threshold
	// before considering the worker consumer unhealthy.
	DefaultHealthFailureThreshold = 3

	// DefaultIteratorEscalationWindow is the sliding time window used to detect
	// a burst of iterator failures warranting escalation (consumer refresh).
	DefaultIteratorEscalationWindow = 60 * time.Second

	// DefaultIteratorEscalationThreshold is the number of consecutive iterator
	// failures within the escalation window required to trigger a single
	// escalation event (refresh) per window.
	DefaultIteratorEscalationThreshold = 3

	// DefaultRecoveryMaxAttempts is the default attempt budget for the
	// bounded-retry envelope that wraps the partition consumer's
	// iterator-creation retry loop. After this many consecutive iter-create
	// failures the envelope transitions to permanent failure: it fires
	// OnPermanentFailure exactly once, logs at WARN, emits a metric, and
	// exits the consumption loop so no further consumer-create traffic is
	// generated against the (presumed) vanished resource.
	DefaultRecoveryMaxAttempts = 8

	// DefaultRecoveryBaseBackoff is the default base delay between iter-create
	// retry attempts. Set to 500ms to compose with (not fight)
	// recovery.Controller's internal minRecoveryInterval (500ms cooldown
	// between consecutive recover() calls) — a BaseBackoff below the
	// cooldown would silently inflate the effective attempt budget when
	// Classify hits the cooldown and returns ActionBackoff without consuming
	// an envelope attempt.
	DefaultRecoveryBaseBackoff = 500 * time.Millisecond

	// DefaultRecoveryMaxBackoff caps the per-attempt delay.
	DefaultRecoveryMaxBackoff = 30 * time.Second

	// DefaultRecoveryJitter is the ± fraction applied to each backoff delay
	// to avoid synchronized reconnect thundering herds across worker fleets.
	DefaultRecoveryJitter = 0.2
)
