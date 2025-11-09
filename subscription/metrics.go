package subscription

import "github.com/arloliu/parti/types"

// emitControlRetry delegates retry increment to the global metrics collector if provided.
func emitControlRetry(mc types.MetricsCollector, op string) {
	if mc == nil {
		return
	}
	mc.IncrementWorkerConsumerControlRetry(op)
}

// emitRetryBackoff delegates backoff observation to the global metrics collector.
func emitRetryBackoff(mc types.MetricsCollector, op string, dSec float64) {
	if mc == nil {
		return
	}
	mc.RecordWorkerConsumerRetryBackoff(op, dSec)
}
