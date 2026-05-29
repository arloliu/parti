package durable

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/types"
)

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
