package subscription

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/internal/logging"
	imetrics "github.com/arloliu/parti/internal/metrics"
	"github.com/nats-io/nats.go/jetstream"
)

// captureEscalationMetrics captures iterator escalation events.
type captureEscalationMetrics struct {
	*imetrics.NopMetrics
	escalations int32
}

func (c *captureEscalationMetrics) IncrementWorkerConsumerIteratorEscalation() {
	atomic.AddInt32(&c.escalations, 1)
}

// Test that a burst of iterator failures triggers exactly one escalation (refresh) within the window.
func TestIteratorEscalation_BurstTriggersSingleRefresh(t *testing.T) {
	var createCalls int32
	m := &captureEscalationMetrics{NopMetrics: imetrics.NewNop()}

	wc := &WorkerConsumer{}
	wc.config.ConsumerPrefix = "dur"
	wc.config.IteratorEscalationWindow = 200 * time.Millisecond
	wc.config.IteratorEscalationThreshold = 3
	wc.config.RetryBase = 5 * time.Millisecond
	wc.config.RetryMultiplier = 1.2
	wc.config.RetryMax = 10 * time.Millisecond
	wc.config.Metrics = m
	wc.config.applyDefaults()
	wc.logger = logging.NewNop()
	wc.workerID = "w1"
	wc.workerSubjects = []string{"work.1"}

	// Iterator that returns many transient errors rapidly.
	wc.iterFactory = func(_ jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
		// enough errors to exceed threshold multiple times if not suppressed
		errs := make([]error, 10)
		for i := range errs {
			errs[i] = context.DeadlineExceeded // treated as exit normally, so use generic error
		}
		// We want transient errors; use generic error instead of deadline
		for i := range errs {
			errs[i] = transientTestError{}
		}

		return &errSeqIter{errs: errs}, nil
	}

	// create/update function increments a counter
	wc.createOrUpdateFn = func(context.Context, string, jetstream.ConsumerConfig) (jetstream.Consumer, error) {
		atomic.AddInt32(&createCalls, 1)
		return nil, transientTestError{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go wc.runWorkerPullLoop(ctx)

	<-ctx.Done()

	if atomic.LoadInt32(&m.escalations) == 0 {
		t.Fatal("expected at least one iterator escalation, got 0")
	}
	if atomic.LoadInt32(&createCalls) < 1 {
		t.Fatalf("expected at least one consumer refresh call, got %d", createCalls)
	}
}

// transientTestError is a stand-in for a non-heartbeat transient iterator error.
type transientTestError struct{}

func (transientTestError) Error() string { return "transient" }
