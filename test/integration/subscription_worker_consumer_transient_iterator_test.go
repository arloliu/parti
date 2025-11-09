package integration_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/subscription"
	partitesting "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// transientCaptureMetrics captures iterator restart counts and health transitions.
type transientCaptureMetrics struct {
	// Embed a no-op metrics collector to satisfy the full interface; we override the methods we care about.
	// Using testing package's minimal mock avoids needing full Prometheus instrumentation here.
	mu              sync.Mutex
	restarts        map[string]int
	lastConsecutive int
	healthSamples   []bool
}

// Ensure initialization.
func newTransientCaptureMetrics() *transientCaptureMetrics {
	return &transientCaptureMetrics{restarts: make(map[string]int)}
}

// Implement minimal WorkerConsumer metrics hooks used in the pull loop.
func (m *transientCaptureMetrics) IncrementWorkerConsumerIteratorRestart(reason string) {
	m.mu.Lock()
	m.restarts[reason]++
	m.mu.Unlock()
}

func (m *transientCaptureMetrics) SetWorkerConsumerConsecutiveIteratorFailures(count int) {
	m.mu.Lock()
	m.lastConsecutive = count
	m.mu.Unlock()
}

func (m *transientCaptureMetrics) SetWorkerConsumerHealthStatus(healthy bool) {
	m.mu.Lock()
	m.healthSamples = append(m.healthSamples, healthy)
	m.mu.Unlock()
}

// Unused interface surface methods (no-ops) — kept simple for test scope.
func (m *transientCaptureMetrics) IncrementWorkerConsumerControlRetry(string)       {}
func (m *transientCaptureMetrics) RecordWorkerConsumerRetryBackoff(string, float64) {}
func (m *transientCaptureMetrics) SetWorkerConsumerSubjectsCurrent(int)             {}
func (m *transientCaptureMetrics) IncrementWorkerConsumerSubjectChange(string, int) {}
func (m *transientCaptureMetrics) IncrementWorkerConsumerGuardrailViolation(string) {}
func (m *transientCaptureMetrics) IncrementWorkerConsumerSubjectThresholdWarning()  {}
func (m *transientCaptureMetrics) RecordWorkerConsumerUpdate(string)                {}
func (m *transientCaptureMetrics) ObserveWorkerConsumerUpdateLatency(float64)       {}
func (m *transientCaptureMetrics) IncrementWorkerConsumerRecreationAttempt(string)  {}
func (m *transientCaptureMetrics) RecordWorkerConsumerRecreation(string, string)    {}
func (m *transientCaptureMetrics) ObserveWorkerConsumerRecreationDuration(float64)  {}
func (m *transientCaptureMetrics) IncrementWorkerConsumerIteratorEscalation()       {}

// Remaining interface groups (manager/calculator/etc.) — no-ops for isolation.
func (m *transientCaptureMetrics) RecordStateTransition(_, _ parti.State, _ float64) {}
func (m *transientCaptureMetrics) RecordLeadershipChange(string)                     {}
func (m *transientCaptureMetrics) RecordDegradedDuration(float64)                    {}
func (m *transientCaptureMetrics) SetDegradedMode(float64)                           {}
func (m *transientCaptureMetrics) SetCacheAge(float64)                               {}
func (m *transientCaptureMetrics) SetAlertLevel(int)                                 {}
func (m *transientCaptureMetrics) IncrementAlertEmitted(string)                      {}
func (m *transientCaptureMetrics) RecordRebalanceDuration(float64, string)           {}
func (m *transientCaptureMetrics) RecordRebalanceAttempt(string, bool)               {}
func (m *transientCaptureMetrics) RecordPartitionCount(int)                          {}
func (m *transientCaptureMetrics) RecordKVOperationDuration(string, float64)         {}
func (m *transientCaptureMetrics) RecordStateChangeDropped()                         {}
func (m *transientCaptureMetrics) RecordEmergencyRebalance(int)                      {}
func (m *transientCaptureMetrics) RecordWorkerChange(int, int)                       {}
func (m *transientCaptureMetrics) RecordActiveWorkers(int)                           {}
func (m *transientCaptureMetrics) RecordCacheUsage(string, float64)                  {}
func (m *transientCaptureMetrics) IncrementCacheFallback(string)                     {}
func (m *transientCaptureMetrics) RecordHeartbeat(string, bool)                      {}
func (m *transientCaptureMetrics) RecordAssignmentChange(int, int, int64)            {}

// TestWorkerConsumer_TransientIteratorFailures simulates real transient iterator creation failures
// followed by successful recovery using an embedded NATS JetStream instance.
// It verifies that restart metrics are emitted and messages are eventually processed.
func TestWorkerConsumer_TransientIteratorFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := context.Background()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Stream for test subjects
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "transient-stream", Subjects: []string{"work.*"}})
	require.NoError(t, err)

	// Publish a few messages that will be consumed after iterator recovers.
	for i := 0; i < 5; i++ {
		_, err = js.Publish(ctx, "work.1", []byte("m"))
		require.NoError(t, err)
	}

	// Handler counts successful ACKs.
	var handled atomic.Int64
	mh := subscription.MessageHandlerFunc(func(c context.Context, msg jetstream.Msg) error {
		handled.Add(1)
		return msg.Ack()
	})

	// Metrics capture.
	metrics := newTransientCaptureMetrics()

	// Helper configuration with high escalation threshold to isolate transient restart path.
	var factoryCalls int32
	helper, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
		StreamName:                  "transient-stream",
		ConsumerPrefix:              "wkr",
		SubjectTemplate:             "work.{{.PartitionID}}",
		BatchSize:                   5,
		IteratorEscalationThreshold: 100, // disable escalation for this test
		Metrics:                     metrics,
		IteratorFactory: func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
			c := atomic.AddInt32(&factoryCalls, 1)
			if c <= 2 {
				return nil, errors.New("transient iterator init failure")
			}
			return cons.Messages(
				jetstream.PullMaxMessages(batch),
				jetstream.PullExpiry(expiry),
				jetstream.PullHeartbeat(expiry/2),
			)
		},
	}, mh)
	require.NoError(t, err)
	defer helper.Close(context.Background())

	// factoryCalls counter tracks transient failures before successful iterator creation
	// (first two attempts return error, subsequent attempts succeed.)
	// Already injected via IteratorFactory in config.

	// Apply initial assignment (single partition -> subject work.1) and start loop implicitly.
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-TI", []parti.Partition{{Keys: []string{"1"}}}))

	// Wait for messages to be processed post-recovery.
	require.Eventually(t, func() bool { return handled.Load() >= 5 }, 5*time.Second, 50*time.Millisecond)

	// Verify restart metrics captured at least two transient restarts.
	metrics.mu.Lock()
	transientCount := metrics.restarts["transient"]
	lastConsecutive := metrics.lastConsecutive
	metrics.mu.Unlock()

	require.GreaterOrEqual(t, transientCount, 2, "expected >=2 transient restarts")
	// Iterator creation failures don't increment consecutiveFailures; ensure it's zero.
	require.Equal(t, 0, lastConsecutive, "consecutive failures should remain 0 for iterator creation errors")
}
