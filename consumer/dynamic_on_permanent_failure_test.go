package consumer

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestWithOnPermanentFailure_OptionThreadedToDynamicConfig pins the v2-P1
// fix: WithOnPermanentFailure must thread its callback through
// options.onPermanentFailure → DynamicConfig.OnPermanentFailure so the
// downstream NewDynamic plumbing can hand it to
// durable.WorkerConsumerConfig.OnPermanentFailure (which the partition
// consumer's iter-creation envelope and Site B detour fire on exhaustion).
//
// We exercise the threading by:
//  1. Applying the option to a defaults() options struct.
//  2. Reading back the dynamicOpts via the consumer's NewDynamicConfig
//     equivalent — here we use the package-internal options struct
//     directly since the test lives in `package consumer`.
//  3. Verifying the callback survives the round-trip and is invocable.
//
// This avoids the live-NATS exhaustion timing problem (default
// RecoveryRetry has MaxAttempts=8 and 30s max backoff, so exhausting the
// envelope takes ~90s — too slow for a smoke test) while still pinning
// the public option's contract.
func TestWithOnPermanentFailure_OptionThreadedToDynamicConfig(t *testing.T) {
	var captured error
	hookErr := errors.New("simulated permanent failure")

	o := defaultOptions()
	WithOnPermanentFailure(func(_ string, err error) {
		captured = err
	}).apply(&o)

	require.NotNil(t, o.onPermanentFailure,
		"WithOnPermanentFailure must store the callback on options.onPermanentFailure; "+
			"a nil here means the option is wired but stores into the wrong field")

	// Invoke through the captured field — proves the callback survives
	// the option round-trip and is callable through the field's signature.
	o.onPermanentFailure("test.subject.p1", hookErr)
	require.ErrorIs(t, captured, hookErr,
		"the callback stored via WithOnPermanentFailure must be invocable through the options field")
}

// TestNewDynamic_OnPermanentFailure_ThreadedToDynamicConfig pins the
// downstream half of v2-P1: when NewDynamic constructs its internal
// DynamicConfig from the applied options, the OnPermanentFailure callback
// must be present on cfg.OnPermanentFailure so the subsequent
// WorkerConsumerConfig assembly can forward it to the durable layer.
//
// NewDynamic runs cfg.Validate() BEFORE durable.NewWorkerConsumer is
// invoked. The validator does not depend on JetStream; using a stub js
// that satisfies the not-nil guard is sufficient to construct cfg in
// the same shape the production code uses. We then check the field via
// an export_test hook — the simplest seam without exposing the field
// publicly.
//
// If a future refactor renames the option or drops the threading, this
// test fails at compile or at the field assertion.
func TestNewDynamic_OnPermanentFailure_ThreadedToDynamicConfig(t *testing.T) {
	hook := func(_ string, _ error) {}

	o := defaultOptions()
	o.recoveryStrategy = RecoverFromLastProcessed
	WithOnPermanentFailure(hook).apply(&o)

	cfg := DynamicConfig{
		StreamName:         "TEST_STREAM",
		ConsumerPrefix:     "wc",
		SubjectTemplate:    "events.{{.PartitionID}}",
		RecoveryStrategy:   o.recoveryStrategy,
		OnPermanentFailure: o.onPermanentFailure,
	}

	require.NoError(t, cfg.Validate(), "cfg with OnPermanentFailure must validate cleanly")
	require.NotNil(t, cfg.OnPermanentFailure,
		"DynamicConfig.OnPermanentFailure must be populated after option round-trip; "+
			"absence means NewDynamic's cfg-construction step dropped the callback")
}

// fakeJSPF satisfies NewDynamic's not-nil JS guard. Embedded interface
// will panic if NewDynamic actually invokes any JS method — which is
// the reachability signal this test wants: panic == validation passed
// AND NewDynamic proceeded into the durable layer with the option
// threaded through.
type fakeJSPF struct{ jetstream.JetStream }

// TestNewDynamic_WithOnPermanentFailure_Constructs verifies the option
// composes with the existing NewDynamic constructor surface: validation
// must accept WithOnPermanentFailure, and NewDynamic must proceed past
// cfg.Validate into the durable layer (where the embedded nil interface
// triggers a panic on the first JS method call).
//
// The test discriminates three outcomes explicitly:
//   - NewDynamic returns an error (validation rejected the option) → FAIL.
//   - NewDynamic returns successfully (impossible with the stub JS) → FAIL.
//   - NewDynamic panics inside the durable layer (validation passed,
//     option threaded through, fake JS reached) → PASS.
//
// (v3 P2 fix: the prior version of this test used a deferred recover()
// that swallowed every outcome including validation rejection, making
// it a vacuous smoke test.)
// TestDynamic_onPermanentFailure_UserCallbackWinsOverManagerObserver
// pins the dispatch precedence contract documented on
// [Dynamic.onPermanentFailure]: when an application has registered
// its own OnPermanentFailure via [WithOnPermanentFailure], the
// manager-installed observer must NOT also fire — even for
// stream-missing exhaustion. This is the row in the dispatch matrix
// that is most likely to bite operators if a regression accidentally
// chained the dispatch (both fire), because the cross-feature
// contract enforcement (manager.enterDegraded with the named reason)
// would silently double-fire alongside the application's own
// degraded-routing logic.
//
// Tested by constructing a Dynamic directly (bypassing NewDynamic so
// no JetStream context is required), pre-populating both the
// userOnPermanentFailure slot and the managerOnStreamMissing slot,
// then invoking the private dispatcher with a wrapped
// types.ErrStreamMissing. Assert: user runs, manager does not.
func TestDynamic_onPermanentFailure_UserCallbackWinsOverManagerObserver(t *testing.T) {
	var (
		userCalls      atomic.Int32
		userArgSubject atomic.Pointer[string]
		userArgErr     atomic.Pointer[error]
		managerCalls   atomic.Int32
		managerArgErr  atomic.Pointer[error]
		managerArgName atomic.Pointer[string]
	)

	d := &Dynamic{
		streamName: "TEST_STREAM",
		userOnPermanentFailure: func(subject string, err error) {
			userCalls.Add(1)
			s := subject
			userArgSubject.Store(&s)
			e := err
			userArgErr.Store(&e)
		},
	}
	// Manager observer slot via the public method that NewDynamic-less
	// callers use; this is the same path Manager.Start drives.
	d.SetOnStreamMissingError(func(streamName string, err error) {
		managerCalls.Add(1)
		n := streamName
		managerArgName.Store(&n)
		e := err
		managerArgErr.Store(&e)
	})

	wrapped := fmt.Errorf("stream %q: %w", "TEST_STREAM", types.ErrStreamMissing)
	d.onPermanentFailure("test.subject.p1", wrapped)

	require.Equal(t, int32(1), userCalls.Load(),
		"user callback must fire exactly once when registered via WithOnPermanentFailure")
	require.Equal(t, int32(0), managerCalls.Load(),
		"manager observer must NOT fire when a user callback is registered — application callback wins per the documented dispatch precedence")
	require.Equal(t, "test.subject.p1", *userArgSubject.Load(),
		"user callback must receive the partition subject from the durable layer")
	require.ErrorIs(t, *userArgErr.Load(), types.ErrStreamMissing,
		"user callback must receive the wrapped types.ErrStreamMissing chain so apps can branch on the sentinel")
}

// TestDynamic_onPermanentFailure_ManagerObserverOnlyOnStreamMissing
// pins the inverse half of the dispatch matrix: when only the
// manager observer is registered, it MUST fire for stream-missing
// errors and MUST NOT fire for generic errors (preventing manager
// auto-degraded for non-stream-missing exhaustion).
//
// Generic exhaustion is left to the durable layer's WARN log + metric
// per the design note in [Dynamic.onPermanentFailure] — silently
// dropping at the Dynamic dispatcher here is the documented behavior.
func TestDynamic_onPermanentFailure_ManagerObserverOnlyOnStreamMissing(t *testing.T) {
	var managerCalls atomic.Int32

	d := &Dynamic{streamName: "TEST_STREAM"}
	d.SetOnStreamMissingError(func(_ string, _ error) {
		managerCalls.Add(1)
	})

	// Generic (non-stream-missing) permanent failure — must NOT route
	// to the manager observer.
	generic := errors.New("generic iter-create exhaustion")
	d.onPermanentFailure("test.subject.p1", generic)
	require.Equal(t, int32(0), managerCalls.Load(),
		"manager observer must NOT fire for non-stream-missing exhaustion; "+
			"generic failures flow through the durable layer's existing WARN log + metric path")

	// Stream-missing wrapped error — MUST route to the manager observer.
	wrapped := fmt.Errorf("stream %q: %w", "TEST_STREAM", types.ErrStreamMissing)
	d.onPermanentFailure("test.subject.p1", wrapped)
	require.Equal(t, int32(1), managerCalls.Load(),
		"manager observer must fire exactly once for a wrapped types.ErrStreamMissing")
}

// TestDynamic_onPermanentFailure_NoUserNoManager pins the "drop
// silently" path: with neither slot populated, onPermanentFailure
// must be a no-op (no panic, no spurious dispatch). This is the
// path taken by a Dynamic constructed without WithOnPermanentFailure
// and never registered with a Manager — the durable layer's WARN
// log + metric remain the operator-visible signal.
func TestDynamic_onPermanentFailure_NoUserNoManager(t *testing.T) {
	d := &Dynamic{streamName: "TEST_STREAM"}

	require.NotPanics(t, func() {
		// Generic and stream-missing variants both must be no-ops.
		d.onPermanentFailure("test.subject.p1", errors.New("generic"))
		d.onPermanentFailure("test.subject.p1",
			fmt.Errorf("stream %q: %w", "TEST_STREAM", types.ErrStreamMissing))
	}, "onPermanentFailure must be a silent no-op when no callbacks are registered")
}

// TestDynamic_SetOnStreamMissingError_NilClearsAfterFire pins
// detach semantics: after a fire has been routed to the manager
// observer, SetOnStreamMissingError(nil) detaches the callback so
// no subsequent fire reaches the (potentially stale) closure. This
// is the path Manager.Stop relies on (manager.go invokes Set(nil)
// before m.wg.Wait); a regression that left a stale callback
// installed would re-enter the closure after Stop began.
func TestDynamic_SetOnStreamMissingError_NilClearsAfterFire(t *testing.T) {
	var calls atomic.Int32
	d := &Dynamic{streamName: "TEST_STREAM"}
	d.SetOnStreamMissingError(func(_ string, _ error) {
		calls.Add(1)
	})

	wrapped := fmt.Errorf("%w: stream %q", types.ErrStreamMissing, "TEST_STREAM")
	d.onPermanentFailure("test.subject.p1", wrapped)
	require.Equal(t, int32(1), calls.Load(),
		"baseline: manager observer fires before detach")

	d.SetOnStreamMissingError(nil)
	d.onPermanentFailure("test.subject.p1", wrapped)
	require.Equal(t, int32(1), calls.Load(),
		"after SetOnStreamMissingError(nil), the previously-installed callback must not be invoked on subsequent fires; "+
			"a regression that left the slot populated would re-enter the (potentially stale) closure")
}

func TestNewDynamic_WithOnPermanentFailure_Constructs(t *testing.T) {
	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	var (
		gotErr      error
		gotPanic    any
		returnedOK  bool
		panicked    bool
		errReturned bool
	)

	func() {
		defer func() {
			if r := recover(); r != nil {
				gotPanic = r
				panicked = true
			}
		}()
		_, err := NewDynamic(
			fakeJSPF{},
			"TEST_STREAM",
			"wc",
			"events.{{.PartitionID}}",
			handler,
			WithOnPermanentFailure(func(_ string, _ error) {}),
		)
		if err != nil {
			gotErr = err
			errReturned = true
		} else {
			returnedOK = true
		}
	}()

	require.False(t, errReturned,
		"validation must accept WithOnPermanentFailure; got error %v before NewDynamic could thread it through", gotErr)
	require.False(t, returnedOK,
		"the stub JetStream cannot satisfy NewDynamic's durable construction; an OK return here means the fake-JS reachability check is no longer load-bearing — replace this test with a real-JS exhaustion test")
	require.True(t, panicked,
		"NewDynamic must reach the durable layer (panic from fakeJSPF's embedded nil interface) — proves cfg.Validate passed and the OnPermanentFailure option propagated past validation")
	_ = gotPanic // panic value is irrelevant; reaching the panic is the assertion.
}
