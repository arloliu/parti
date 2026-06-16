package durable

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	imetrics "github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/ratelimit"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLimiter is a test double for ratelimit.Limiter that records Wait calls
// and optionally blocks until a gate channel is closed.
type fakeLimiter struct {
	mu      sync.Mutex
	calls   int
	waitErr error // if non-nil, returned by Wait on every call
}

func newFakeLimiter() *fakeLimiter {
	return &fakeLimiter{}
}

func (f *fakeLimiter) Wait(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.waitErr
}

func (f *fakeLimiter) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeLimiter) SetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waitErr = err
}

var _ ratelimit.Limiter = (*fakeLimiter)(nil)

// rateLimitJS embeds jetstream.JetStream (as a nil interface) and overrides
// only CreateOrUpdateConsumer, which is the sole method under test.
// Other methods will panic if called — proving no other paths are exercised.
type rateLimitJS struct {
	jetstream.JetStream

	mu       sync.Mutex
	outcomes []error
	idx      int
	calls    atomic.Int32
}

func newRateLimitJS(outcomes ...error) *rateLimitJS {
	return &rateLimitJS{outcomes: outcomes}
}

func (r *rateLimitJS) CreateOrUpdateConsumer(_ context.Context, _ string, _ jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	r.calls.Add(1)

	r.mu.Lock()
	var err error
	if r.idx < len(r.outcomes) {
		err = r.outcomes[r.idx]
		r.idx++
	}
	r.mu.Unlock()

	if err != nil {
		return nil, err
	}

	return &streamMissingConsumer{}, nil // reuse existing stub from stream_missing tests
}

func (r *rateLimitJS) RPCCalls() int {
	return int(r.calls.Load())
}

// --- Test: nil limiter is never called and returns nil immediately ---

func TestConsumerCreateRateLimit_NilLimiterNeverBlocks(t *testing.T) {
	err := ratelimit.Wait(t.Context(), nil)
	require.NoError(t, err, "nil limiter must return nil immediately")
}

// --- Test: ensureConsumer gates EVERY physical RPC attempt ---
// Two transient errors then success = 3 RPC attempts; limiter called 3 times.

func TestConsumerCreateRateLimit_EnsureConsumer_GatesEveryAttempt(t *testing.T) {
	transient := errors.New("transient NATS error")
	fakeJS := newRateLimitJS(transient, transient, nil) // fail, fail, succeed
	limiter := newFakeLimiter()

	pc := &partitionConsumer{
		streamName:     "test-stream",
		js:             fakeJS,
		consumerConfig: jetstream.ConsumerConfig{Durable: "test-durable"},
		config: partitionConsumerConfig{
			ConsumerCreateLimiter: limiter,
			RecoveryRetry: RecoveryRetryConfig{
				MaxAttempts: 3,
				BaseBackoff: 1 * time.Millisecond,
				MaxBackoff:  10 * time.Millisecond,
			},
		},
	}

	cons, err := pc.ensureConsumer(t.Context())
	require.NoError(t, err)
	require.NotNil(t, cons)

	// Key invariant: limiter must gate EVERY physical attempt (not per-logical-create).
	assert.Equal(t, 3, limiter.Calls(), "limiter must be called before every physical RPC attempt including retries")
	assert.Equal(t, 3, fakeJS.RPCCalls(), "3 RPC calls expected")
}

// --- Test: recreateFn gates the single physical RPC attempt ---

func TestConsumerCreateRateLimit_RecreateFn_GatesAttempt(t *testing.T) {
	fakeJS := newRateLimitJS(nil) // succeed immediately
	limiter := newFakeLimiter()

	pc := &partitionConsumer{
		streamName: "test-stream",
		js:         fakeJS,
		config: partitionConsumerConfig{
			ConsumerCreateLimiter: limiter,
		},
	}

	recreate := pc.recreateFn()
	cons, err := recreate(t.Context(), jetstream.ConsumerConfig{Durable: "test-durable"})
	require.NoError(t, err)
	require.NotNil(t, cons)

	assert.Equal(t, 1, limiter.Calls(), "recreateFn must call limiter before the single RPC attempt")
	assert.Equal(t, 1, fakeJS.RPCCalls(), "exactly 1 RPC call expected")
}

// --- Test: limiter error before attempt 2 aborts and propagates ---
// Context cancel mid-flight: limiter returns ctx.Err() on second call.

func TestConsumerCreateRateLimit_CtxCancelAborts(t *testing.T) {
	transient := errors.New("transient")

	// A limiter that returns ctx.Err() on the second call.
	cancelLimiter := &onNthCancelLimiter{cancelOnCall: 2}
	ctx, cancel := context.WithCancel(t.Context())

	// fakeJS returns one transient error → limiter called again → cancel fires.
	fakeJS := newRateLimitJS(transient)

	pc := &partitionConsumer{
		streamName:     "test-stream",
		js:             fakeJS,
		consumerConfig: jetstream.ConsumerConfig{Durable: "test-durable"},
		config: partitionConsumerConfig{
			ConsumerCreateLimiter: cancelLimiter,
			RecoveryRetry: RecoveryRetryConfig{
				MaxAttempts: 3,
				BaseBackoff: 1 * time.Millisecond,
				MaxBackoff:  10 * time.Millisecond,
			},
		},
	}
	// Wire ctx so the limiter can cancel it.
	cancelLimiter.cancel = cancel

	_, err := pc.ensureConsumer(ctx)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled, "expected context.Canceled propagation")
}

// onNthCancelLimiter cancels the context on the Nth Wait call.
type onNthCancelLimiter struct {
	mu           sync.Mutex
	calls        int
	cancelOnCall int
	cancel       context.CancelFunc
}

func (l *onNthCancelLimiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	l.calls++
	n := l.calls
	l.mu.Unlock()

	if n >= l.cancelOnCall {
		l.cancel()
		return ctx.Err()
	}

	return nil
}

// --- Test: recreateFn aborts before the RPC when the gate returns an error ---

func TestConsumerCreateRateLimit_RecreateFn_CtxCancelAborts(t *testing.T) {
	fakeJS := newRateLimitJS(nil) // would succeed, but the gate must abort first
	limiter := newFakeLimiter()
	limiter.SetError(context.Canceled)

	pc := &partitionConsumer{
		streamName: "test-stream",
		js:         fakeJS,
		config: partitionConsumerConfig{
			ConsumerCreateLimiter: limiter,
		},
	}

	recreate := pc.recreateFn()
	_, err := recreate(t.Context(), jetstream.ConsumerConfig{Durable: "test-durable"})
	require.ErrorIs(t, err, context.Canceled, "gate ctx error must propagate from recreateFn")
	assert.Equal(t, 0, fakeJS.RPCCalls(), "no RPC must be issued when the gate aborts")
	assert.Equal(t, 1, limiter.Calls())
}

// --- Test: nil limiter → unlimited → ensureConsumer behavior unchanged ---

func TestConsumerCreateRateLimit_NilLimiter_EnsureConsumerUnchanged(t *testing.T) {
	fakeJS := newRateLimitJS(nil) // succeed immediately
	pc := &partitionConsumer{
		streamName:     "test-stream",
		js:             fakeJS,
		consumerConfig: jetstream.ConsumerConfig{Durable: "test-durable"},
		config: partitionConsumerConfig{
			ConsumerCreateLimiter: nil, // nil = unlimited
			RecoveryRetry: RecoveryRetryConfig{
				MaxAttempts: 3,
				BaseBackoff: 1 * time.Millisecond,
				MaxBackoff:  10 * time.Millisecond,
			},
		},
	}

	cons, err := pc.ensureConsumer(t.Context())
	require.NoError(t, err)
	require.NotNil(t, cons)
	assert.Equal(t, 1, fakeJS.RPCCalls())
}

// --- Test: nil limiter → unlimited → recreateFn behavior unchanged ---

func TestConsumerCreateRateLimit_NilLimiter_RecreateFnUnchanged(t *testing.T) {
	fakeJS := newRateLimitJS(nil)
	pc := &partitionConsumer{
		streamName: "test-stream",
		js:         fakeJS,
		config: partitionConsumerConfig{
			ConsumerCreateLimiter: nil,
		},
	}

	recreate := pc.recreateFn()
	cons, err := recreate(t.Context(), jetstream.ConsumerConfig{})
	require.NoError(t, err)
	require.NotNil(t, cons)
	assert.Equal(t, 1, fakeJS.RPCCalls())
}

// --- Test: aggregate / shared budget ---
// Two goroutines share one limiter. Total limiter calls = sum of all RPC attempts.

func TestConsumerCreateRateLimit_SharedBudget(t *testing.T) {
	limiter := newFakeLimiter()

	var wg sync.WaitGroup
	var totalRPC atomic.Int32

	for range 3 {
		wg.Go(func() {
			fakeJS := newRateLimitJS(nil) // each succeeds on first try
			pc := &partitionConsumer{
				streamName:     "test-stream",
				js:             fakeJS,
				consumerConfig: jetstream.ConsumerConfig{Durable: "test"},
				config: partitionConsumerConfig{
					ConsumerCreateLimiter: limiter, // shared
					RecoveryRetry: RecoveryRetryConfig{
						MaxAttempts: 3,
						BaseBackoff: 1 * time.Millisecond,
						MaxBackoff:  10 * time.Millisecond,
					},
				},
			}
			_, err := pc.ensureConsumer(context.Background())
			require.NoError(t, err)
			totalRPC.Add(int32(fakeJS.RPCCalls()))
		})
	}
	wg.Wait()

	// Each goroutine makes 1 RPC → 3 total; limiter called 3 times (shared budget).
	assert.Equal(t, 3, int(totalRPC.Load()))
	assert.Equal(t, 3, limiter.Calls(), "shared limiter must be consulted for every RPC across all goroutines")
}

// --- Metrics sidecar tests ---

func TestConsumerCreateThrottleObserver_EmitConsumerCreateThrottled(t *testing.T) {
	obs := &throttleCountMetrics{}

	// Positive wait should emit.
	emitConsumerCreateThrottled(obs, 0.1)
	assert.Equal(t, 1, obs.throttled)
	assert.InDelta(t, 0.1, obs.lastWait, 0.001)

	// Another positive wait.
	emitConsumerCreateThrottled(obs, 0.5)
	assert.Equal(t, 2, obs.throttled)
}

func TestConsumerCreateThrottleObserver_NilMetrics_NoOp(t *testing.T) {
	// Must not panic.
	assert.NotPanics(t, func() {
		emitConsumerCreateThrottled(nil, 0.5)
	})
}

func TestConsumerCreateThrottleObserver_OldStyleMetrics_NoBreak(t *testing.T) {
	// legacyMetrics embeds the types.WorkerConsumerMetrics interface directly,
	// which satisfies WorkerConsumerMetrics without gaining the sidecar methods.
	// Verify the type does NOT satisfy ConsumerCreateThrottleObserver, then
	// verify emitConsumerCreateThrottled is a no-op (does not panic, does not call any method).
	var m legacyMetrics
	_, ok := any(&m).(ConsumerCreateThrottleObserver)
	assert.False(t, ok, "legacyMetrics must not satisfy ConsumerCreateThrottleObserver")

	assert.NotPanics(t, func() {
		emitConsumerCreateThrottled(&m, 0.5)
	})
}

// legacyMetrics embeds types.WorkerConsumerMetrics as an interface field.
// A struct embedding an interface satisfies WorkerConsumerMetrics without
// inheriting any concrete methods — including the sidecar methods
// IncrementConsumerCreateThrottled / ObserveConsumerCreateThrottleWait.
// This models old-style external collectors that implement the public
// WorkerConsumerMetrics interface but predate the optional sidecar (D7).
type legacyMetrics struct {
	types.WorkerConsumerMetrics
}

var _ types.WorkerConsumerMetrics = (*legacyMetrics)(nil)

type throttleCountMetrics struct {
	imetrics.NopMetrics
	throttled int
	lastWait  float64
}

func (m *throttleCountMetrics) IncrementConsumerCreateThrottled() {
	m.throttled++
}

func (m *throttleCountMetrics) ObserveConsumerCreateThrottleWait(seconds float64) {
	m.lastWait = seconds
}
