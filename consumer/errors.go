package consumer

import (
	"errors"

	"github.com/arloliu/parti/v2/internal/durable"
)

// ErrInvalidConfig is returned by constructors ([NewQueue], [NewStatic], [NewDynamic],
// [NewBroadcast]) when a required parameter is nil, empty, or out of range.
//
// Use [errors.Is] to check for this error programmatically:
//
//	q, err := consumer.NewQueue(js, "", "", "", handler)
//	if errors.Is(err, consumer.ErrInvalidConfig) {
//	    // handle validation failure
//	}
var ErrInvalidConfig = errors.New("invalid consumer configuration")

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
