package consumer

import "github.com/arloliu/parti/v2/internal/durable"

// Re-exported sentinel errors from internal packages.
// Use [errors.Is] to check for these errors in [Dynamic.Update] return values.
var (
	// ErrWorkerIDMutation is returned by [Dynamic.Update] when the workerID
	// changes and [DynamicConfig.AllowWorkerIDChange] is false.
	ErrWorkerIDMutation = durable.ErrWorkerIDMutation

	// ErrMaxSubjectsExceeded is returned by [Dynamic.Update] when the
	// partition count exceeds MaxConcurrentSubjects.
	ErrMaxSubjectsExceeded = durable.ErrMaxSubjectsExceeded
)
