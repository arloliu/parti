package durable

import "github.com/arloliu/parti/v2/types"

// emitControlRetry delegates retry increment to the metrics collector if provided.
func emitControlRetry(mc types.WorkerConsumerMetrics, op string) {
	if mc == nil {
		return
	}
	mc.IncrementWorkerConsumerControlRetry(op)
}

// emitRetryBackoff delegates backoff observation to the metrics collector.
func emitRetryBackoff(mc types.WorkerConsumerMetrics, op string, dSec float64) {
	if mc == nil {
		return
	}
	mc.RecordWorkerConsumerRetryBackoff(op, dSec)
}
