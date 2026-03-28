package subscription

import (
	"sync/atomic"

	imetrics "github.com/arloliu/parti/v2/internal/metrics"
)

// captureEscalationMetrics captures iterator escalation events.
type captureEscalationMetrics struct {
	*imetrics.NopMetrics
	escalations int32
}

func (c *captureEscalationMetrics) IncrementWorkerConsumerIteratorEscalation(_ string) {
	atomic.AddInt32(&c.escalations, 1)
}

// Implement new pull suppression metric (no-op capture needed for tests not asserting it yet).
func (c *captureEscalationMetrics) IncrementWorkerConsumerPullSuppressed(_ string) {}
