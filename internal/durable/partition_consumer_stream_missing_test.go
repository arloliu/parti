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
	"github.com/arloliu/parti/v2/types"
)

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
