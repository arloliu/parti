package durable

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestPartitionConsumer_Run_ProcessAndExit(t *testing.T) {
	// Setup
	logger := logging.NewNop()
	cfg := partitionConsumerConfig{
		BatchSize:    1,
		FetchTimeout: 100 * time.Millisecond,
	}

	// Mock iterator
	mockIter := &mockMessagesContext{
		msgs: []jetstream.Msg{
			&mockMsg{data: []byte("msg1")},
			&mockMsg{data: []byte("msg2")},
		},
		done: make(chan struct{}),
	}

	iterFactory := func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
		return mockIter, nil
	}

	pc := newPartitionConsumer(
		logger,
		nil, // js not needed if iterFactory is mocked and no escalation
		cfg,
		partitionConsumerOpts{
			streamName:           "stream",
			durableName:          "durable",
			subject:              "subject",
			partitionID:          "partitionID",
			consumerConfig:       jetstream.ConsumerConfig{},
			consumer:             nil, // consumer not needed if iterFactory is mocked
			iterFactory:          iterFactory,
			checkPullSuppression: nil, // no suppression
		},
	)

	// Handler
	processed := make([]string, 0)
	var mu sync.Mutex
	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		mu.Lock()
		processed = append(processed, string(msg.Data()))
		mu.Unlock()
		return nil
	})

	// Run in background
	ctx, cancel := context.WithCancel(context.Background())
	go pc.Run(ctx, handler)

	// Wait for processing
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(processed) == 2
	}, 1*time.Second, 10*time.Millisecond)

	// Stop
	cancel()
	pc.Wait()

	require.Equal(t, []string{"msg1", "msg2"}, processed)
}

// Mocks
type mockMessagesContext struct {
	msgs []jetstream.Msg
	idx  int
	mu   sync.Mutex
	done chan struct{}
}

func (m *mockMessagesContext) Next(opts ...jetstream.NextOpt) (jetstream.Msg, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idx >= len(m.msgs) {
		// Block until stopped
		m.mu.Unlock()
		<-m.done
		m.mu.Lock()
		return nil, context.Canceled
	}
	msg := m.msgs[m.idx]
	m.idx++

	return msg, nil
}

func (m *mockMessagesContext) Stop() {
	select {
	case <-m.done:
	default:
		close(m.done)
	}
}

func (m *mockMessagesContext) Drain() {}

type mockMsg struct {
	data []byte
}

func (m *mockMsg) Data() []byte                           { return m.data }
func (m *mockMsg) Ack() error                             { return nil }
func (m *mockMsg) DoubleAck(context.Context) error        { return nil }
func (m *mockMsg) Nak() error                             { return nil }
func (m *mockMsg) NakWithDelay(delay time.Duration) error { return nil }
func (m *mockMsg) Term() error                            { return nil }
func (m *mockMsg) TermWithReason(reason string) error     { return nil }
func (m *mockMsg) InProgress() error                      { return nil }
func (m *mockMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return nil, errors.New("not implemented")
}
func (m *mockMsg) Subject() string      { return "" }
func (m *mockMsg) Reply() string        { return "" }
func (m *mockMsg) Headers() nats.Header { return nil }

// --- processIterator ErrMsgIteratorClosed filter test ---

func TestPartitionConsumer_ProcessIterator_FiltersGracefulErrors(t *testing.T) {
	logger := logging.NewNop()
	cfg := partitionConsumerConfig{
		BatchSize:    1,
		FetchTimeout: 100 * time.Millisecond,
	}

	tests := []struct {
		name    string
		err     error
		wantErr bool
	}{
		{"ErrMsgIteratorClosed is filtered", jetstream.ErrMsgIteratorClosed, false},
		{"context.Canceled is filtered", context.Canceled, false},
		{"ErrConsumerDeleted is returned", jetstream.ErrConsumerDeleted, true},
		{"ErrNoHeartbeat is returned", jetstream.ErrNoHeartbeat, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create an iterator that returns the test error on first Next().
			mockIter := &errorOnNextIter{err: tt.err}

			pc := newPartitionConsumer(
				logger,
				nil,
				cfg,
				partitionConsumerOpts{
					streamName:  "stream",
					durableName: "durable",
					subject:     "subject",
					partitionID: "pid",
				},
			)

			handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
				return nil
			})

			ctx := t.Context()

			exit, iterErr := pc.processIterator(ctx, mockIter, handler)
			require.False(t, exit, "should not signal exit for iterator error")
			if tt.wantErr {
				require.ErrorIs(t, iterErr, tt.err)
			} else {
				require.NoError(t, iterErr, "graceful error should be filtered to nil")
			}
		})
	}
}

// TestPartitionConsumerDelayWithBackoff_BackoffGrows verifies that repeated calls to
// delayWithBackoffOrExit produce growing delays (the previous delay is threaded through,
// not reset to 0 each call).
//
// Pre-fix: every call passed prev=0 into jitterBackoff, so the delay was always Base
// regardless of how many times the helper was called.
func TestPartitionConsumerDelayWithBackoff_BackoffGrows(t *testing.T) {
	const (
		base     = 1 * time.Millisecond
		mult     = 2.0
		maxDelay = 8 * time.Millisecond
		seed     = int64(42)
		calls    = 6
	)

	cfg := partitionConsumerConfig{
		BatchSize:    1,
		FetchTimeout: 100 * time.Millisecond,
		Retry: RetryConfig{
			Base:       base,
			Multiplier: mult,
			Max:        maxDelay,
			Seed:       seed,
		},
	}

	pc := newPartitionConsumer(
		logging.NewNop(),
		nil,
		cfg,
		partitionConsumerOpts{
			streamName:  "stream",
			durableName: "durable",
			subject:     "subject",
			partitionID: "pid",
		},
	)

	ctx := context.Background()
	delays := make([]time.Duration, 0, calls)
	for range calls {
		delayCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		pc.delayWithBackoffOrExit(delayCtx, "test")
		cancel()
		delays = append(delays, pc.retryPrev)
	}

	require.Len(t, delays, calls)

	// Every delay must be in [Base, Max].
	for i, d := range delays {
		require.GreaterOrEqual(t, d, base, "delay[%d] below Base", i)
		require.LessOrEqual(t, d, maxDelay, "delay[%d] exceeds Max", i)
	}

	// Growth: last delay must exceed first (with a fixed seed this is deterministic).
	require.Greater(t, delays[calls-1], delays[0],
		"expected backoff to grow over %d calls; delays: %v", calls, delays)
}

// --- mock helpers ---

// errorOnNextIter returns an error immediately on Next().
type errorOnNextIter struct {
	err error
}

func (e *errorOnNextIter) Next(opts ...jetstream.NextOpt) (jetstream.Msg, error) {
	return nil, e.err
}

func (e *errorOnNextIter) Stop() {}

func (e *errorOnNextIter) Drain() {}

// --- merged from partition_consumer_envelope_test.go ---

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

// --- merged from partition_consumer_stream_missing_test.go ---

// --- minimal jetstream.JetStream stub ---

// streamMissingJS is a minimal jetstream.JetStream stub that only implements
// CreateOrUpdateConsumer (the sole method partitionConsumer's ensureConsumer
// and recreateFn call). All other methods inherit the embedded nil interface
// and will panic if called — proving the test exercises only the intended
// code path.
type streamMissingJS struct {
	jetstream.JetStream

	mu       sync.Mutex
	plan     []consumerOutcome // consumed in order
	requests []jetstream.ConsumerConfig
}

type consumerOutcome struct {
	cons jetstream.Consumer
	err  error
}

func (s *streamMissingJS) CreateOrUpdateConsumer(_ context.Context, _ string, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, cfg)
	if len(s.plan) == 0 {
		// Default to stream-not-found once the plan is exhausted to surface
		// any unexpected extra calls obviously.
		return nil, jetstream.ErrStreamNotFound
	}
	out := s.plan[0]
	s.plan = s.plan[1:]

	return out.cons, out.err
}

// --- minimal jetstream.Consumer stub ---

// streamMissingConsumer is a minimal Consumer stub used as the "before" and
// "after" consumer values; Info() returns a configurable result so the
// escalation path's pre-remediation Info() call can drive the test.
type streamMissingConsumer struct {
	jetstream.Consumer
	id      string
	infoErr error
}

func (c *streamMissingConsumer) Info(_ context.Context) (*jetstream.ConsumerInfo, error) {
	if c.infoErr != nil {
		return nil, c.infoErr
	}
	return &jetstream.ConsumerInfo{}, nil
}

// --- T-SiteA-iter ---

// TestPartitionConsumer_SiteA_StreamMissing_HookSuccess_RebuildsAndIterates
// is the T-SiteA-iter reproducer (per docs/plans/self-healing/09-pr9-spec.md
// § Reproducer tests). It pins the v3-P0.1 invariant: when the iterator-
// creation envelope's Site A detour invokes the StreamMissingHook successfully,
// the SAME Work attempt MUST also call RebuildAfterStreamRecreated, swap
// pc.consumer to the new consumer, AND call iterFactory again with the new
// consumer before returning nil — otherwise processIterator would receive
// a nil iter and panic on iter.Next().
func TestPartitionConsumer_SiteA_StreamMissing_HookSuccess_RebuildsAndIterates(t *testing.T) {
	originalCons := &streamMissingConsumer{id: "before", infoErr: errors.New("consumer gone")}
	rebuiltCons := &streamMissingConsumer{id: "after"}

	js := &streamMissingJS{
		plan: []consumerOutcome{
			// ensureConsumer (called from escalation): stream is gone.
			{cons: nil, err: jetstream.ErrStreamNotFound},
			// RebuildAfterStreamRecreated (called after hook returns nil): success.
			{cons: rebuiltCons, err: nil},
		},
	}

	var hookCalls atomic.Int32
	hookCalledStream := make(chan string, 1)
	hook := types.StreamMissingHook(func(stream string) error {
		hookCalls.Add(1)
		select {
		case hookCalledStream <- stream:
		default:
		}
		return nil
	})

	stopCh := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-stopCh:
		default:
			close(stopCh)
		}
	})

	var iterMu sync.Mutex
	type iterCall struct {
		cons   jetstream.Consumer
		callID int
	}
	var iterCalls []iterCall

	iterFactory := func(cons jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
		iterMu.Lock()
		idx := len(iterCalls)
		iterCalls = append(iterCalls, iterCall{cons: cons, callID: idx})
		iterMu.Unlock()
		// First call (against originalCons): fail to drive escalation.
		// Subsequent calls (against rebuiltCons after Site A detour):
		// return a working iter so Work returns nil.
		if cons == jetstream.Consumer(originalCons) {
			return nil, errors.New("simulated iter-create failure (consumer stale)")
		}

		return &blockingIter{stopCh: stopCh}, nil
	}

	cfg := partitionConsumerConfig{
		BatchSize:    1,
		FetchTimeout: 50 * time.Millisecond,
		// Use RecoverFromLastProcessed so the recovery.Controller is non-nil
		// — RebuildAfterStreamRecreated requires it.
		RecoveryStrategy: RecoverFromLastProcessed,
		Retry: RetryConfig{
			Backoff:    5 * time.Millisecond,
			Base:       5 * time.Millisecond,
			Multiplier: 1.0,
			Max:        10 * time.Millisecond,
		},
		// Threshold 1 → first failure triggers escalation → ensureConsumer
		// is called on the same Work attempt that just failed.
		IteratorEscalationWindow:    1 * time.Second,
		IteratorEscalationThreshold: 1,
		RecoveryRetry: RecoveryRetryConfig{
			MaxAttempts: 5,
			BaseBackoff: 5 * time.Millisecond,
			MaxBackoff:  10 * time.Millisecond,
			Jitter:      0,
		},
		StreamMissingHook: hook,
	}

	pc := newPartitionConsumer(
		logging.NewNop(),
		js,
		cfg,
		partitionConsumerOpts{
			streamName:     "STREAM",
			durableName:    "DUR",
			subject:        "subj.p1",
			partitionID:    "p1",
			consumerConfig: jetstream.ConsumerConfig{Durable: "DUR_p1"},
			consumer:       originalCons,
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

	// Wait for the hook to fire — this proves Site A detected stream-missing
	// and invoked handleStreamMissing.
	select {
	case stream := <-hookCalledStream:
		require.Equal(t, "STREAM", stream, "hook must receive the stream name")
	case <-time.After(5 * time.Second):
		t.Fatalf("StreamMissingHook never fired; Site A detour did not trigger. iterCalls=%d, js.requests=%d",
			func() int { iterMu.Lock(); defer iterMu.Unlock(); return len(iterCalls) }(),
			func() int { js.mu.Lock(); defer js.mu.Unlock(); return len(js.requests) }(),
		)
	}

	require.Eventually(t, func() bool {
		iterMu.Lock()
		defer iterMu.Unlock()
		// Need at least two calls: one against originalCons (failed) and
		// at least one against rebuiltCons (the post-rebuild iter creation
		// inside the Site A success branch).
		if len(iterCalls) < 2 {
			return false
		}

		return iterCalls[len(iterCalls)-1].cons == jetstream.Consumer(rebuiltCons)
	}, 2*time.Second, 10*time.Millisecond,
		"iterFactory must be called with the rebuilt consumer inside the same Work attempt — proving Site A doesn't return nil with iter unset")

	// pc.consumer must have been swapped to the rebuilt consumer under the lock.
	pc.consumerMu.RLock()
	got := pc.consumer
	pc.consumerMu.RUnlock()
	require.Same(t, rebuiltCons, got, "pc.consumer must point at the rebuilt consumer after the Site A success path")

	require.Equal(t, int32(1), hookCalls.Load(), "hook must fire exactly once per stream-missing episode")

	// Clean shutdown.
	cancel()
	close(stopCh)
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("partition consumer did not exit on ctx cancel")
	}
}

// TestPartitionConsumer_SiteA_PostRebuildRetry_UsesNewConsumer is the
// T-SiteA-iter-retry reproducer pinning the v5-P1.4 invariant: when the
// iterFactory fails against the freshly-rebuilt consumer (transient post-
// recreate flakiness), the NEXT envelope attempt MUST also use the rebuilt
// consumer — never the pre-rebuild handle — because the Work closure
// re-reads pc.consumer at the top of every attempt.
//
// Without the per-attempt re-read, the closure would close over the
// original `cons` value and the retry would invoke iterFactory with the
// stale (deleted) consumer indefinitely.
func TestPartitionConsumer_SiteA_PostRebuildRetry_UsesNewConsumer(t *testing.T) {
	originalCons := &streamMissingConsumer{id: "before", infoErr: errors.New("consumer gone")}
	rebuiltCons := &streamMissingConsumer{id: "after"}

	js := &streamMissingJS{
		plan: []consumerOutcome{
			{cons: nil, err: jetstream.ErrStreamNotFound}, // escalation -> stream missing
			{cons: rebuiltCons, err: nil},                 // rebuild succeeds
		},
	}

	var hookCalls atomic.Int32
	hook := types.StreamMissingHook(func(_ string) error {
		hookCalls.Add(1)
		return nil
	})

	stopCh := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-stopCh:
		default:
			close(stopCh)
		}
	})

	var iterMu sync.Mutex
	var iterCalls []jetstream.Consumer
	postRebuildFailures := 0
	const postRebuildFailuresWanted = 2 // first call against newCons after rebuild fails twice

	iterFactory := func(cons jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
		iterMu.Lock()
		iterCalls = append(iterCalls, cons)
		idx := len(iterCalls)
		iterMu.Unlock()
		if cons == jetstream.Consumer(originalCons) {
			return nil, errors.New("simulated iter-create failure (consumer stale)")
		}
		// First two calls against rebuiltCons fail transiently, third succeeds.
		if idx <= 1+postRebuildFailuresWanted {
			iterMu.Lock()
			postRebuildFailures++
			iterMu.Unlock()

			return nil, errors.New("simulated post-rebuild iter-create flakiness")
		}

		return &blockingIter{stopCh: stopCh}, nil
	}

	cfg := partitionConsumerConfig{
		BatchSize:        1,
		FetchTimeout:     50 * time.Millisecond,
		RecoveryStrategy: RecoverFromLastProcessed,
		Retry: RetryConfig{
			Backoff:    5 * time.Millisecond,
			Base:       5 * time.Millisecond,
			Multiplier: 1.0,
			Max:        10 * time.Millisecond,
		},
		IteratorEscalationWindow:    1 * time.Second,
		IteratorEscalationThreshold: 1,
		RecoveryRetry: RecoveryRetryConfig{
			MaxAttempts: 10,
			BaseBackoff: 5 * time.Millisecond,
			MaxBackoff:  10 * time.Millisecond,
			Jitter:      0,
		},
		StreamMissingHook: hook,
	}

	pc := newPartitionConsumer(
		logging.NewNop(),
		js,
		cfg,
		partitionConsumerOpts{
			streamName:     "STREAM",
			durableName:    "DUR",
			subject:        "subj.p1",
			partitionID:    "p1",
			consumerConfig: jetstream.ConsumerConfig{Durable: "DUR_p1"},
			consumer:       originalCons,
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

	// Wait until the iterator factory succeeds (third call against
	// rebuiltCons) — proving subsequent retries also use newCons.
	require.Eventually(t, func() bool {
		iterMu.Lock()
		defer iterMu.Unlock()
		return len(iterCalls) >= 4 // 1 original + 3 against rebuilt (2 fail, 1 success)
	}, 5*time.Second, 10*time.Millisecond,
		"iterFactory should be invoked against newCons across multiple envelope attempts")

	iterMu.Lock()
	calls := append([]jetstream.Consumer(nil), iterCalls...)
	iterMu.Unlock()

	// First call: against originalCons (the failure that drove the detour).
	require.Same(t, jetstream.Consumer(originalCons), calls[0],
		"first iter-create call must use the original consumer")
	// All subsequent calls: against rebuiltCons. A broken implementation
	// that captures cons once before the envelope would invoke later
	// retries against originalCons instead.
	for i := 1; i < len(calls); i++ {
		require.Same(t, jetstream.Consumer(rebuiltCons), calls[i],
			"call %d after the Site A detour must use the rebuilt consumer, got %v",
			i, calls[i])
	}

	require.Equal(t, int32(1), hookCalls.Load(),
		"hook must fire exactly once even when post-rebuild iter creation flakes")

	cancel()
	close(stopCh)
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("partition consumer did not exit on ctx cancel")
	}
}

// --- merged from partition_consumer_stream_missing_site_b_test.go ---

// TestPartitionConsumer_SiteB_StreamMissing_HookSuccess_ResetAwareRebuild
// is the T-SiteB-rebuild reproducer (per docs/plans/self-healing/09-pr9-spec.md
// § Reproducer tests). It pins the v5-P1.3 invariant: when handleIteratorFailure
// classifies the iterator error as ActionStreamMissing (the iter-runtime path —
// stream deleted while the consumer was running), the post-hook success branch
// MUST drive recovery.Controller.RebuildAfterStreamRecreated so that:
//
//   - The recreated stream consumer is built with DeliverAllPolicy (the
//     one-shot recreated-flag override), not the static dynamicbuild config.
//   - pc.consumer is swapped under consumerMu.
//   - The outer loop's next iteration uses the freshly-rebuilt consumer.
//
// A broken implementation that only calls handleStreamMissing without
// RebuildAfterStreamRecreated would either keep using the stale consumer
// or fall through to legacy ensureConsumer with the static config —
// either way the captured DeliverPolicy assertion fails.
func TestPartitionConsumer_SiteB_StreamMissing_HookSuccess_ResetAwareRebuild(t *testing.T) {
	originalCons := &streamMissingConsumer{id: "before"}
	rebuiltCons := &streamMissingConsumer{id: "after"}

	js := &streamMissingJS{
		plan: []consumerOutcome{
			// 1) recover() calls recreate(ctx, recoverCfg) → stream gone.
			{cons: nil, err: jetstream.ErrStreamNotFound},
			// 2) RebuildAfterStreamRecreated calls recreate(ctx, postHookCfg).
			//    The captured cfg.DeliverPolicy MUST be DeliverAllPolicy.
			{cons: rebuiltCons, err: nil},
		},
	}

	var hookCalls atomic.Int32
	hookCalledStream := make(chan string, 1)
	hook := types.StreamMissingHook(func(stream string) error {
		hookCalls.Add(1)
		select {
		case hookCalledStream <- stream:
		default:
		}
		return nil
	})

	stopCh := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-stopCh:
		default:
			close(stopCh)
		}
	})

	var iterMu sync.Mutex
	var iterCalls []jetstream.Consumer
	iterFactory := func(cons jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
		iterMu.Lock()
		iterCalls = append(iterCalls, cons)
		iterMu.Unlock()
		if cons == jetstream.Consumer(originalCons) {
			// Drive Site B's iter-runtime classification: an iterator that
			// returns ErrConsumerDeleted on Next() trips ClassifyError →
			// ErrorConsumerGone → recover() → recreate() → ErrStreamNotFound
			// → ActionStreamMissing.
			return &errorOnNextIter{err: jetstream.ErrConsumerDeleted}, nil
		}

		// Post-rebuild: return a healthy iterator that blocks until shutdown.
		return &blockingIter{stopCh: stopCh}, nil
	}

	cfg := partitionConsumerConfig{
		BatchSize:        1,
		FetchTimeout:     50 * time.Millisecond,
		RecoveryStrategy: RecoverFromLastProcessed,
		Retry: RetryConfig{
			Backoff:    5 * time.Millisecond,
			Base:       5 * time.Millisecond,
			Multiplier: 1.0,
			Max:        10 * time.Millisecond,
		},
		// High threshold so Site A's escalation path is disabled — we're
		// pinning Site B exclusively.
		IteratorEscalationWindow:    1 * time.Second,
		IteratorEscalationThreshold: 1000,
		RecoveryRetry: RecoveryRetryConfig{
			MaxAttempts: 10,
			BaseBackoff: 5 * time.Millisecond,
			MaxBackoff:  10 * time.Millisecond,
			Jitter:      0,
		},
		StreamMissingHook: hook,
	}

	pc := newPartitionConsumer(
		logging.NewNop(),
		js,
		cfg,
		partitionConsumerOpts{
			streamName:     "STREAM",
			durableName:    "DUR",
			subject:        "subj.p1",
			partitionID:    "p1",
			consumerConfig: jetstream.ConsumerConfig{Durable: "DUR_p1"},
			consumer:       originalCons,
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

	// Wait for the hook to fire.
	select {
	case stream := <-hookCalledStream:
		require.Equal(t, "STREAM", stream, "hook must receive the stream name")
	case <-time.After(5 * time.Second):
		js.mu.Lock()
		reqs := len(js.requests)
		js.mu.Unlock()
		iterMu.Lock()
		ic := len(iterCalls)
		iterMu.Unlock()
		t.Fatalf("StreamMissingHook never fired; Site B detour did not trigger. iterCalls=%d, js.requests=%d", ic, reqs)
	}

	// After the detour, the outer loop must reach the post-rebuild
	// blockingIter (built from rebuiltCons). Wait until iterFactory has
	// been called against rebuiltCons.
	require.Eventually(t, func() bool {
		iterMu.Lock()
		defer iterMu.Unlock()
		return slices.Contains(iterCalls, jetstream.Consumer(rebuiltCons))
	}, 5*time.Second, 10*time.Millisecond,
		"Site B success path must cause the outer loop to construct a fresh envelope against the rebuilt consumer")

	pc.consumerMu.RLock()
	got := pc.consumer
	pc.consumerMu.RUnlock()
	require.Same(t, jetstream.Consumer(rebuiltCons), got,
		"pc.consumer must be swapped to the rebuilt consumer after Site B's reset-aware rebuild")

	// The recreate request captured during RebuildAfterStreamRecreated
	// (js.requests[1]) MUST carry DeliverAllPolicy. The recreated-flag
	// override fires only when checkpoint==0 AND recreatedSinceLastBuild==true;
	// asserting the second request's DeliverPolicy pins both invariants.
	js.mu.Lock()
	require.GreaterOrEqual(t, len(js.requests), 2,
		"expected at least two CreateOrUpdateConsumer calls (recover-fail + post-hook rebuild)")
	rebuildReq := js.requests[1]
	js.mu.Unlock()
	require.Equal(t, jetstream.DeliverAllPolicy, rebuildReq.DeliverPolicy,
		"post-hook rebuild config must use DeliverAllPolicy via the recreated-flag override (BuildConfig); "+
			"a stale-flag or legacy-ensureConsumer path would emit a different policy")

	require.Equal(t, int32(1), hookCalls.Load(),
		"hook must fire exactly once per Site B stream-missing episode")

	cancel()
	close(stopCh)
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("partition consumer did not exit on ctx cancel")
	}
}

// TestPartitionConsumer_SiteB_StreamMissing_NoHook_BoundsAndFiresPermanent
// pins the P0-1 fix from post-impl review v1: when Site B's stream-missing
// detour fails repeatedly (here, no hook configured), the failure count is
// bounded by RecoveryRetry.MaxAttempts and OnPermanentFailure fires exactly
// once with a wrapped types.ErrStreamMissing — the consumer loop then exits.
//
// Without the bound, nats.go's Consumer.Messages() returns a MessagesContext
// eagerly (no remote stream validation), so a fresh outer-loop envelope
// succeeds at iter creation, hits the same ErrConsumerDeleted on Next(),
// re-enters Site B, and loops forever generating CreateOrUpdateConsumer
// traffic from Classify→recover. The streamMissingFailures counter +
// handleStreamMissingFailure helper turn that into a bounded retry that
// surfaces via the same OnPermanentFailure path as the F2 envelope.
func TestPartitionConsumer_SiteB_StreamMissing_NoHook_BoundsAndFiresPermanent(t *testing.T) {
	const maxAttempts = 3

	originalCons := &streamMissingConsumer{id: "before"}

	// Every CreateOrUpdateConsumer call (from recover()) returns ErrStreamNotFound.
	// The detour can never succeed because there is no hook to recreate the stream.
	js := &streamMissingJS{}

	stopCh := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-stopCh:
		default:
			close(stopCh)
		}
	})

	var iterMu sync.Mutex
	var iterCalls int
	iterFactory := func(_ jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
		iterMu.Lock()
		iterCalls++
		iterMu.Unlock()
		// Every iter returns ErrConsumerDeleted on Next() — drives Site B
		// classification on every outer-loop iteration.
		return &errorOnNextIter{err: jetstream.ErrConsumerDeleted}, nil
	}

	var permanentCalls atomic.Int32
	var permanentErr atomic.Pointer[error]
	cfg := partitionConsumerConfig{
		BatchSize:        1,
		FetchTimeout:     50 * time.Millisecond,
		RecoveryStrategy: RecoverFromLastProcessed,
		Retry: RetryConfig{
			Backoff:    5 * time.Millisecond,
			Base:       5 * time.Millisecond,
			Multiplier: 1.0,
			Max:        10 * time.Millisecond,
		},
		IteratorEscalationWindow:    1 * time.Second,
		IteratorEscalationThreshold: 1000, // disabled — Site A path not exercised
		RecoveryRetry: RecoveryRetryConfig{
			MaxAttempts: maxAttempts,
			BaseBackoff: 5 * time.Millisecond,
			MaxBackoff:  10 * time.Millisecond,
			Jitter:      0,
		},
		// StreamMissingHook deliberately unset — the no-hook path is what
		// drives detour failure.
		OnPermanentFailure: func(_ string, err error) {
			permanentCalls.Add(1)
			e := err
			permanentErr.Store(&e)
		},
	}

	pc := newPartitionConsumer(
		logging.NewNop(),
		js,
		cfg,
		partitionConsumerOpts{
			streamName:     "STREAM",
			durableName:    "DUR",
			subject:        "subj.p1",
			partitionID:    "p1",
			consumerConfig: jetstream.ConsumerConfig{Durable: "DUR_p1"},
			consumer:       originalCons,
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

	// The loop must EXIT ON ITS OWN once the Site B failure counter reaches
	// MaxAttempts — without ctx cancellation. Without the P0-1 fix, this
	// would time out because the loop spins forever.
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("partition consumer did not exit after Site B exhaustion; "+
			"permanentCalls=%d, iterCalls=%d (expected exactly 1 OnPermanentFailure)",
			permanentCalls.Load(),
			func() int { iterMu.Lock(); defer iterMu.Unlock(); return iterCalls }(),
		)
	}

	require.Equal(t, int32(1), permanentCalls.Load(),
		"OnPermanentFailure must fire exactly once on Site B exhaustion")

	ptr := permanentErr.Load()
	require.NotNil(t, ptr, "OnPermanentFailure must receive a non-nil error")
	require.ErrorIs(t, *ptr, types.ErrStreamMissing,
		"permanent-failure error must wrap types.ErrStreamMissing so the manager observer routes via the documented contract")
}

// TestPartitionConsumer_SiteB_StreamMissing_RecoveryDisabled_BoundsAndFiresPermanent
// pins the v2-P0-A fix: when RecoveryStrategy is the default (RecoveryDisabled),
// the nil-recovery branch of handleIteratorFailure must STILL route a
// stream-not-found signal from maybeEscalateIteratorFailures through
// handleStreamMissingFailure so the loop is bounded by RecoveryRetry.MaxAttempts
// and fires OnPermanentFailure with a wrapped types.ErrStreamMissing.
//
// Without the fix, the nil-recovery branch ignored the stream-not-found
// remediation error and the loop spun indefinitely calling EnsureConsumer
// from maybeEscalateIteratorFailures — the same exhaustion gap v1 P0-1
// flagged for the recovery-enabled path, but for the default
// RecoveryDisabled shape.
func TestPartitionConsumer_SiteB_StreamMissing_RecoveryDisabled_BoundsAndFiresPermanent(t *testing.T) {
	const maxAttempts = 3

	// infoErr is required: maybeEscalateIteratorFailures short-circuits
	// when cons.Info() succeeds (consumer still healthy → no remediation
	// needed). To drive the stream-not-found path, the stale consumer
	// must surface an Info() failure first; then escalation calls
	// ensureConsumer which returns ErrStreamNotFound from the mock js.
	originalCons := &streamMissingConsumer{id: "before", infoErr: errors.New("consumer gone")}

	// Every CreateOrUpdateConsumer call (from ensureConsumer in
	// maybeEscalateIteratorFailures) returns ErrStreamNotFound. The
	// fix routes that signal through handleStreamMissingFailure even
	// without recovery.
	js := &streamMissingJS{}

	stopCh := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-stopCh:
		default:
			close(stopCh)
		}
	})

	var iterMu sync.Mutex
	var iterCalls int
	iterFactory := func(_ jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
		iterMu.Lock()
		iterCalls++
		iterMu.Unlock()
		// Each iterator immediately errors on Next() with
		// ErrConsumerDeleted; the nil-recovery branch's escalation
		// path calls ensureConsumer which then surfaces stream-missing.
		return &errorOnNextIter{err: jetstream.ErrConsumerDeleted}, nil
	}

	var permanentCalls atomic.Int32
	var permanentErr atomic.Pointer[error]
	cfg := partitionConsumerConfig{
		BatchSize:    1,
		FetchTimeout: 50 * time.Millisecond,
		// RecoveryStrategy intentionally unset (zero = RecoveryDisabled).
		// This is the default shape v1 P0-1's fix did NOT cover.
		Retry: RetryConfig{
			Backoff:    5 * time.Millisecond,
			Base:       5 * time.Millisecond,
			Multiplier: 1.0,
			Max:        10 * time.Millisecond,
		},
		// Threshold 1 → first iter-failure triggers escalation →
		// ensureConsumer → ErrStreamNotFound → routed through
		// handleStreamMissingFailure.
		IteratorEscalationWindow:    1 * time.Second,
		IteratorEscalationThreshold: 1,
		RecoveryRetry: RecoveryRetryConfig{
			MaxAttempts: maxAttempts,
			BaseBackoff: 5 * time.Millisecond,
			MaxBackoff:  10 * time.Millisecond,
			Jitter:      0,
		},
		OnPermanentFailure: func(_ string, err error) {
			permanentCalls.Add(1)
			e := err
			permanentErr.Store(&e)
		},
	}

	pc := newPartitionConsumer(
		logging.NewNop(),
		js,
		cfg,
		partitionConsumerOpts{
			streamName:     "STREAM",
			durableName:    "DUR",
			subject:        "subj.p1",
			partitionID:    "p1",
			consumerConfig: jetstream.ConsumerConfig{Durable: "DUR_p1"},
			consumer:       originalCons,
			iterFactory:    iterFactory,
		},
	)

	// Recovery controller must be nil with the default strategy — confirm
	// the test setup actually exercises the nil-recovery branch.
	require.Nil(t, pc.recovery,
		"test must drive the nil-recovery branch; got non-nil recovery (check default strategy semantics)")

	handler := messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		pc.Run(ctx, handler)
	}()

	// The loop MUST exit on its own once Site B's counter reaches MaxAttempts.
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("partition consumer did not exit after RecoveryDisabled Site B exhaustion; "+
			"permanentCalls=%d, iterCalls=%d",
			permanentCalls.Load(),
			func() int { iterMu.Lock(); defer iterMu.Unlock(); return iterCalls }(),
		)
	}

	require.Equal(t, int32(1), permanentCalls.Load(),
		"OnPermanentFailure must fire exactly once on Site B exhaustion in the RecoveryDisabled path")

	ptr := permanentErr.Load()
	require.NotNil(t, ptr, "OnPermanentFailure must receive a non-nil error")
	require.ErrorIs(t, *ptr, types.ErrStreamMissing,
		"permanent-failure error must wrap types.ErrStreamMissing so the application can errors.Is-route the failure")
}

// TestPartitionConsumer_SiteB_ActionContinue_ResetsStreamMissingCounter pins
// the v2-P0-B fix: a partial Site B failure burst that is then "healed" by
// a successful normal recovery (ActionContinue, e.g. the stream came back
// out-of-band and the next ErrConsumerDeleted is resolved by recreating the
// consumer) MUST clear the streamMissingFailures counter so the NEXT
// stream-missing episode starts with the full RecoveryRetry.MaxAttempts
// budget. Without the reset, the per-episode reset contract documented on
// RecoveryRetryConfig is violated and a subsequent episode fires
// OnPermanentFailure prematurely.
func TestPartitionConsumer_SiteB_ActionContinue_ResetsStreamMissingCounter(t *testing.T) {
	rebuiltCons := &streamMissingConsumer{id: "rebuilt"}

	// js returns a fresh consumer on every CreateOrUpdateConsumer — drives
	// recover() → ActionContinue. The test does not actually drive a
	// stream-missing failure first; instead it pre-loads the counter and
	// then exercises the reset branch directly.
	js := &streamMissingJS{
		plan: []consumerOutcome{
			{cons: rebuiltCons, err: nil},
		},
	}

	cfg := partitionConsumerConfig{
		BatchSize:                   1,
		FetchTimeout:                50 * time.Millisecond,
		RecoveryStrategy:            RecoverFromLastProcessed,
		Retry:                       RetryConfig{Backoff: 5 * time.Millisecond, Base: 5 * time.Millisecond, Multiplier: 1.0, Max: 10 * time.Millisecond},
		IteratorEscalationWindow:    1 * time.Second,
		IteratorEscalationThreshold: 1000, // disabled
		RecoveryRetry: RecoveryRetryConfig{
			MaxAttempts: 3,
			BaseBackoff: 5 * time.Millisecond,
			MaxBackoff:  10 * time.Millisecond,
		},
	}

	pc := newPartitionConsumer(
		logging.NewNop(),
		js,
		cfg,
		partitionConsumerOpts{
			streamName:     "STREAM",
			durableName:    "DUR",
			subject:        "subj.p1",
			partitionID:    "p1",
			consumerConfig: jetstream.ConsumerConfig{Durable: "DUR_p1"},
			consumer:       &streamMissingConsumer{id: "before"},
			iterFactory: func(jetstream.Consumer, int, time.Duration) (jetstream.MessagesContext, error) {
				return nil, nil //nolint:nilnil // unused stub; handleIteratorFailure is invoked directly in this test.
			},
		},
	)

	// Recovery must be enabled so handleIteratorFailure takes the
	// classify+switch branch, where ActionContinue lives.
	require.NotNil(t, pc.recovery, "recovery must be non-nil for ActionContinue branch coverage")

	// Pre-load: simulate two prior Site B failures that did not exhaust
	// the budget. Without the reset, the next stream-missing episode would
	// fire OnPermanentFailure after a single attempt instead of three.
	pc.streamMissingFailures.Store(2)

	// Driving ErrConsumerDeleted through handleIteratorFailure runs:
	//   Classify → ErrorConsumerGone → recover() → recreate() → success
	//   → ActionContinue → reset branch (including streamMissingFailures.Store(0)).
	exit := pc.handleIteratorFailure(context.Background(), jetstream.ErrConsumerDeleted)
	require.False(t, exit, "ActionContinue must not exit the loop")

	require.Equal(t, int32(0), pc.streamMissingFailures.Load(),
		"ActionContinue success must reset the Site B stream-missing failure counter; "+
			"non-zero count here implies stale failures will shorten the next episode's MaxAttempts budget")

	// Belt-and-braces: pc.consumer must be the recovered one.
	pc.consumerMu.RLock()
	got := pc.consumer
	pc.consumerMu.RUnlock()
	require.Same(t, jetstream.Consumer(rebuiltCons), got, "ActionContinue must swap pc.consumer to the recovered one")
}

// TestPartitionConsumer_NilRecovery_RemediationSuccess_ResetsCounter pins
// the v3 P1 fix: in the RecoveryDisabled (nil-recovery) path, when
// maybeEscalateIteratorFailures successfully rebinds via the legacy
// ensureConsumer path, the streamMissingFailures counter must be cleared
// so a later stream-missing episode receives the full
// RecoveryRetry.MaxAttempts budget. Without the reset, the v2 P0-A
// increment (added in the nil-recovery branch) would leak across episodes
// — the same class of cross-episode budget leak v2 P0-B fixed for
// ActionContinue.
func TestPartitionConsumer_NilRecovery_RemediationSuccess_ResetsCounter(t *testing.T) {
	rebuiltCons := &streamMissingConsumer{id: "rebuilt"}

	// ensureConsumer succeeds on first call — drives the legacy
	// remediation success branch in maybeEscalateIteratorFailures.
	js := &streamMissingJS{
		plan: []consumerOutcome{
			{cons: rebuiltCons, err: nil},
		},
	}

	cfg := partitionConsumerConfig{
		BatchSize:    1,
		FetchTimeout: 50 * time.Millisecond,
		// RecoveryStrategy intentionally unset (zero = RecoveryDisabled)
		// to exercise the nil-recovery path that v2 P0-A bounded and
		// v3 P1 fully balances with a reset.
		Retry:                       RetryConfig{Backoff: 5 * time.Millisecond, Base: 5 * time.Millisecond, Multiplier: 1.0, Max: 10 * time.Millisecond},
		IteratorEscalationWindow:    1 * time.Second,
		IteratorEscalationThreshold: 1,
		RecoveryRetry: RecoveryRetryConfig{
			MaxAttempts: 3,
			BaseBackoff: 5 * time.Millisecond,
			MaxBackoff:  10 * time.Millisecond,
		},
	}

	// Stale-Info consumer so the escalation falls through to ensureConsumer.
	originalCons := &streamMissingConsumer{id: "before", infoErr: errors.New("consumer gone")}

	pc := newPartitionConsumer(
		logging.NewNop(),
		js,
		cfg,
		partitionConsumerOpts{
			streamName:     "STREAM",
			durableName:    "DUR",
			subject:        "subj.p1",
			partitionID:    "p1",
			consumerConfig: jetstream.ConsumerConfig{Durable: "DUR_p1"},
			consumer:       originalCons,
			iterFactory: func(jetstream.Consumer, int, time.Duration) (jetstream.MessagesContext, error) {
				return nil, nil //nolint:nilnil // unused stub
			},
		},
	)

	require.Nil(t, pc.recovery, "test must exercise the nil-recovery (RecoveryDisabled) path")

	// Pre-load: simulate two prior Site B stream-missing failures from
	// the v2 P0-A counter path. Without v3 P1's reset, a subsequent
	// stream-missing episode would fire OnPermanentFailure after one
	// attempt instead of three.
	pc.streamMissingFailures.Store(2)

	// Drive escalation directly: stale Info + ensureConsumer success →
	// remediation success branch.
	escErr := pc.maybeEscalateIteratorFailures(context.Background())
	require.NoError(t, escErr,
		"successful remediation must NOT return an error to the caller")

	require.Equal(t, int32(0), pc.streamMissingFailures.Load(),
		"successful nil-recovery remediation must reset streamMissingFailures so the next stream-missing episode receives the full MaxAttempts budget")

	pc.consumerMu.RLock()
	got := pc.consumer
	pc.consumerMu.RUnlock()
	require.Same(t, jetstream.Consumer(rebuiltCons), got,
		"successful remediation must swap pc.consumer to the rebuilt one")
}
