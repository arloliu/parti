package subscription

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/internal/logging"
	imetrics "github.com/arloliu/parti/internal/metrics"
	"github.com/nats-io/nats.go/jetstream"
)

// captureMetrics embeds NopMetrics and captures iterator restart reasons and health updates.
type captureMetrics struct {
	*imetrics.NopMetrics
	mu              sync.Mutex
	restarts        map[string]int
	healthSamples   []bool
	lastConsecutive int
}

func newCaptureMetrics() *captureMetrics {
	return &captureMetrics{NopMetrics: imetrics.NewNop(), restarts: make(map[string]int)}
}

func (c *captureMetrics) IncrementWorkerConsumerIteratorRestart(reason string) {
	c.mu.Lock()
	c.restarts[reason]++
	c.mu.Unlock()
}

func (c *captureMetrics) SetWorkerConsumerHealthStatus(healthy bool) {
	c.mu.Lock()
	c.healthSamples = append(c.healthSamples, healthy)
	c.mu.Unlock()
}

func (c *captureMetrics) SetWorkerConsumerConsecutiveIteratorFailures(count int) {
	c.mu.Lock()
	c.lastConsecutive = count
	c.mu.Unlock()
}

// fake iterator that returns a sequence of errors for Next() calls.
type errSeqIter struct {
	errs []error
	i    int
	stop bool
}

func (it *errSeqIter) Next(...jetstream.NextOpt) (jetstream.Msg, error) {
	if it.stop {
		return nil, context.Canceled
	}
	if it.i < len(it.errs) {
		e := it.errs[it.i]
		it.i++

		return nil, e
	}
	// block a bit then return context canceled to allow loop to exit on cancel
	time.Sleep(20 * time.Millisecond)

	return nil, context.Canceled
}

func (it *errSeqIter) Stop() { it.stop = true }

func (it *errSeqIter) Drain() {}

// Test heartbeat-induced restart classification.
func TestIteratorRestart_Heartbeat(t *testing.T) {
	dh := &WorkerConsumer{}
	dh.config.HealthFailureThreshold = 3
	dh.config.ConsumerPrefix = "worker"
	cm := newCaptureMetrics()
	dh.config.Metrics = cm
	dh.config.applyDefaults()
	dh.logger = logging.NewNop()

	// Custom iterator factory ignores consumer and returns an iterator that immediately signals heartbeat loss once.
	created := 0
	dh.iterFactory = func(_ jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
		created++
		if created == 1 {
			return &errSeqIter{errs: []error{jetstream.ErrNoHeartbeat}}, nil
		}
		// subsequent iterators just block and then cancel
		return &errSeqIter{errs: []error{errors.New("transient")}}, nil
	}

	// Run loop briefly
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	go dh.runWorkerPullLoop(ctx)

	<-ctx.Done()

	// Expect at least one heartbeat restart recorded
	cm.mu.Lock()
	hb := cm.restarts["heartbeat"]
	cm.mu.Unlock()
	if hb == 0 {
		t.Fatalf("expected heartbeat restart >=1, got %d", hb)
	}
}

// Test transient errors increase consecutive failures and flip health when exceeding threshold.
func TestIteratorRestart_TransientAndHealth(t *testing.T) {
	dh := &WorkerConsumer{}
	dh.config.HealthFailureThreshold = 2
	dh.config.ConsumerPrefix = "worker"
	cm := newCaptureMetrics()
	dh.config.Metrics = cm
	// Speed up backoff to allow multiple iterations within test timeout
	dh.config.RetryBase = 10 * time.Millisecond
	dh.config.RetryMax = 20 * time.Millisecond
	dh.config.RetryMultiplier = 1.2
	dh.config.applyDefaults()
	dh.logger = logging.NewNop()

	// Iterator will return transient errors repeatedly to drive consecutive failures > threshold.
	dh.iterFactory = func(_ jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
		return &errSeqIter{errs: []error{errors.New("e1"), errors.New("e2"), errors.New("e3")}}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go dh.runWorkerPullLoop(ctx)

	<-ctx.Done()

	cm.mu.Lock()
	last := cm.lastConsecutive
	unhealthy := false
	for _, h := range cm.healthSamples {
		if !h {
			unhealthy = true

			break
		}
	}
	cm.mu.Unlock()

	if last < 2 {
		t.Fatalf("expected consecutive failures tracked, got %d", last)
	}
	if !unhealthy {
		t.Fatal("expected at least one unhealthy sample when exceeding threshold")
	}
	if cm.restarts["transient"] == 0 {
		t.Fatalf("expected transient restarts >=1, got %d", cm.restarts["transient"])
	}
}
