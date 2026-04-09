package recoveryutil

import (
	"time"

	"github.com/arloliu/parti/v2/types"
)

// Attempt tracks a single recovery attempt for metrics emission.
type Attempt struct {
	metrics types.WorkerConsumerMetrics
	reason  string
	started time.Time
}

// Begin records the start of a recovery attempt and returns a handle
// for reporting the final result and duration.
func Begin(metrics types.WorkerConsumerMetrics, reason string) Attempt {
	attempt := Attempt{
		metrics: metrics,
		reason:  metricReason(reason),
		started: time.Now(),
	}

	if metrics != nil {
		metrics.IncrementWorkerConsumerRecreationAttempt(attempt.reason)
	}

	return attempt
}

// Finish records the final outcome and duration of a recovery attempt.
func (a Attempt) Finish(success bool) {
	if a.metrics == nil {
		return
	}

	result := "failure"
	if success {
		result = "success"
	}

	a.metrics.RecordWorkerConsumerRecreation(result, a.reason)
	a.metrics.ObserveWorkerConsumerRecreationDuration(time.Since(a.started).Seconds())
}

func metricReason(reason string) string {
	switch reason {
	case "consumer_deleted":
		return "iterator_error"
	case "consumer_not_found_after_burst":
		return "not_found"
	default:
		return "unknown"
	}
}
