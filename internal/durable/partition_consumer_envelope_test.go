package durable

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/internal/logging"
)

// TestPartitionConsumer_RecoveryEnvelope_ExhaustsAndStops is the P2.4d
// T1 reproducer: when iterator creation fails consecutively (the failure
// mode that manifests when the underlying consumer/stream has vanished),
// the bounded-retry envelope MUST cap consecutive attempts at MaxAttempts,
// fire OnPermanentFailure exactly once, and exit the consumption loop
// so that no further iter-create / consumer-create traffic is generated
// against the vanished resource.
//
// Without the envelope wiring, the partition consumer's outer for-loop
// retries iter creation indefinitely on every iteration with only
// pc.config.Retry's per-iteration backoff in place — there is no overall
// attempt cap and no escalation signal.
func TestPartitionConsumer_RecoveryEnvelope_ExhaustsAndStops(t *testing.T) {
	const maxAttempts = 3
	var iterCallCount atomic.Int32
	var permanentCalls atomic.Int32
	var permanentSubject atomic.Pointer[string]

	iterErr := errors.New("simulated iter-create failure (stream-gone)")

	iterFactory := func(_ jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
		iterCallCount.Add(1)
		return nil, iterErr
	}

	cfg := partitionConsumerConfig{
		BatchSize:    1,
		FetchTimeout: 50 * time.Millisecond,
		Retry: RetryConfig{
			Backoff:    10 * time.Millisecond,
			Base:       10 * time.Millisecond,
			Multiplier: 1.5,
			Max:        20 * time.Millisecond,
		},
		IteratorEscalationWindow:    1 * time.Second,
		IteratorEscalationThreshold: 1000, // disabled — would otherwise call ensureConsumer (needs real js)
		RecoveryRetry: RecoveryRetryConfig{
			MaxAttempts: maxAttempts,
			BaseBackoff: 10 * time.Millisecond,
			MaxBackoff:  20 * time.Millisecond,
			Jitter:      0,
		},
		OnPermanentFailure: func(subject string, _ error) {
			permanentCalls.Add(1)
			s := subject
			permanentSubject.Store(&s)
		},
	}

	pc := newPartitionConsumer(
		logging.NewNop(),
		nil, // js not needed; escalation threshold is gated above
		cfg,
		partitionConsumerOpts{
			streamName:     "STREAM",
			durableName:    "DUR",
			subject:        "subj.p1",
			partitionID:    "p1",
			consumerConfig: jetstream.ConsumerConfig{},
			consumer:       nil,
			iterFactory:    iterFactory,
		},
	)

	handler := messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		pc.Run(ctx, handler)
	}()

	// After MaxAttempts (3) iter-create failures the envelope must fire
	// OnPermanentFailure and the Run loop must return on its own (without
	// ctx cancel).
	select {
	case <-runDone:
		// expected — envelope exhausted, loop exited
	case <-time.After(5 * time.Second):
		t.Fatalf("partition consumer did not exit after envelope exhaustion; "+
			"iterCallCount=%d, permanentCalls=%d (expected exactly %d iter calls and 1 permanent call)",
			iterCallCount.Load(), permanentCalls.Load(), maxAttempts)
	}

	require.Equal(t, int32(maxAttempts), iterCallCount.Load(),
		"iter creation must be capped at envelope MaxAttempts; without the envelope this is unbounded")
	require.Equal(t, int32(1), permanentCalls.Load(),
		"OnPermanentFailure must fire exactly once on exhaustion")
	if s := permanentSubject.Load(); s == nil || *s != "subj.p1" {
		t.Fatalf("OnPermanentFailure must receive the partition subject, got %v", s)
	}
}

// TestPartitionConsumer_RecoveryEnvelope_RecoveredIterationsResetBudget
// is the P2.4d negative-space test (per
// feedback_test_both_directions_of_boundary). It asserts that K isolated
// iter-create failures, each followed by a full recovery (a usable
// iterator obtained AND consumed), do NOT accumulate against the
// envelope's attempt budget. Each fresh outer-loop iteration must
// construct a fresh envelope, so the total failure count over the
// lifetime can exceed MaxAttempts without triggering permanent failure
// — only consecutive failures within a single episode count.
//
// This is the test that would have caught the P2.4c v1 monotonic-budget
// bug (Bug 3) if it had been written for that PR.
func TestPartitionConsumer_RecoveryEnvelope_RecoveredIterationsResetBudget(t *testing.T) {
	const maxAttempts = 3
	const totalEpisodes = 5 // > maxAttempts in total, but none consecutive

	iterErr := errors.New("simulated transient iter-create failure")

	var (
		mu              sync.Mutex
		callIdx         int
		iterFailureSeen int
	)

	// failPlan[i] == true means: on the i-th iter-create call, return error.
	// Pattern: fail once, succeed once, fail once, succeed once, ... ensures
	// no two consecutive failures.
	failPlan := make([]bool, 0, 2*totalEpisodes)
	for range totalEpisodes {
		failPlan = append(failPlan, true)  // fail
		failPlan = append(failPlan, false) // succeed
	}

	stopCh := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-stopCh:
		default:
			close(stopCh)
		}
	})

	iterFactory := func(_ jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
		mu.Lock()
		idx := callIdx
		callIdx++
		mu.Unlock()

		if idx < len(failPlan) && failPlan[idx] {
			mu.Lock()
			iterFailureSeen++
			mu.Unlock()
			return nil, iterErr
		}
		// Succeed: return an iterator that blocks on Next() until the test ends,
		// then returns ErrMsgIteratorClosed (which the consumer treats as a clean exit).
		return &blockingIter{stopCh: stopCh}, nil
	}

	var permanentCalls atomic.Int32

	cfg := partitionConsumerConfig{
		BatchSize:    1,
		FetchTimeout: 50 * time.Millisecond,
		Retry: RetryConfig{
			Backoff:    5 * time.Millisecond,
			Base:       5 * time.Millisecond,
			Multiplier: 1.0,
			Max:        10 * time.Millisecond,
		},
		IteratorEscalationWindow:    1 * time.Second,
		IteratorEscalationThreshold: 1000, // disabled
		RecoveryRetry: RecoveryRetryConfig{
			MaxAttempts: maxAttempts,
			BaseBackoff: 5 * time.Millisecond,
			MaxBackoff:  10 * time.Millisecond,
			Jitter:      0,
		},
		OnPermanentFailure: func(_ string, _ error) { permanentCalls.Add(1) },
	}

	pc := newPartitionConsumer(
		logging.NewNop(),
		nil,
		cfg,
		partitionConsumerOpts{
			streamName:     "STREAM",
			durableName:    "DUR",
			subject:        "subj.p1",
			partitionID:    "p1",
			consumerConfig: jetstream.ConsumerConfig{},
			consumer:       nil,
			iterFactory:    iterFactory,
		},
	)

	handler := messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		pc.Run(ctx, handler)
	}()

	// Wait until we have observed >maxAttempts failures total — proves the
	// envelope did NOT accumulate budget across recovered episodes.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return iterFailureSeen >= maxAttempts+1
	}, 5*time.Second, 10*time.Millisecond, "expected >%d isolated iter-create failures across separate episodes", maxAttempts)

	require.Equal(t, int32(0), permanentCalls.Load(),
		"OnPermanentFailure must NOT fire when failures are interleaved with recoveries; "+
			"firing here would be the monotonic-budget bug (Bug 3 lineage)")

	// Clean shutdown.
	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("partition consumer did not exit on ctx cancel")
	}
}

// blockingIter is an iterator that blocks on Next() until stopCh is
// closed, then returns ErrMsgIteratorClosed (the graceful-exit sentinel
// the partition consumer's processIterator path filters to a nil
// session error). Used by the negative-space test to simulate a healthy
// iterator session that "completed cleanly" — driving a fresh outer
// for-loop iteration and a fresh envelope construction.
type blockingIter struct {
	stopCh chan struct{}
}

func (b *blockingIter) Next(_ ...jetstream.NextOpt) (jetstream.Msg, error) {
	// Short wait: simulate a brief usable iterator session. Returning
	// ErrMsgIteratorClosed causes processIterator to return iterErr=nil,
	// which loops back to a fresh outer iteration. That fresh iteration
	// is the budget reset signal the test is probing.
	select {
	case <-b.stopCh:
		return nil, jetstream.ErrMsgIteratorClosed
	case <-time.After(20 * time.Millisecond):
		return nil, jetstream.ErrMsgIteratorClosed
	}
}

func (b *blockingIter) Stop()  {}
func (b *blockingIter) Drain() {}
