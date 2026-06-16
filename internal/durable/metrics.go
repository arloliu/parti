package durable

import "github.com/arloliu/parti/v2/types"

// ConsumerCreateThrottleObserver is an optional sidecar interface that a
// types.WorkerConsumerMetrics implementation may satisfy. When the configured
// Metrics value implements this interface, throttle events are emitted via it.
//
// This is defined separately from types.WorkerConsumerMetrics (D7) so that
// external collectors that implement the existing public interface are
// unaffected by the new methods — they simply never receive throttle calls.
// Internal collectors (NopMetrics, PrometheusMetrics) implement this interface.
//
// Only positive-delay waits are recorded; burst-absorbed creates (where a
// token was available immediately) do not trigger these methods.
type ConsumerCreateThrottleObserver interface {
	// IncrementConsumerCreateThrottled increments the counter of consumer-create
	// attempts that were actually delayed by the rate limiter.
	IncrementConsumerCreateThrottled()
	// ObserveConsumerCreateThrottleWait records the actual wait duration in seconds.
	ObserveConsumerCreateThrottleWait(seconds float64)
}

// emitConsumerCreateThrottled type-asserts mc to ConsumerCreateThrottleObserver
// and emits a throttle event when the assertion succeeds.
// This is nil-safe: both mc==nil and the missing-interface case are no-ops.
func emitConsumerCreateThrottled(mc types.WorkerConsumerMetrics, waitSeconds float64) {
	if mc == nil {
		return
	}
	if obs, ok := mc.(ConsumerCreateThrottleObserver); ok {
		obs.IncrementConsumerCreateThrottled()
		obs.ObserveConsumerCreateThrottleWait(waitSeconds)
	}
}

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
