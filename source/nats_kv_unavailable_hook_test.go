package source

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// f6aBucketSeq mints a fresh bucket name per test, avoiding the
// invalid-bucket-name issue with t.Name().
var f6aBucketSeq atomic.Uint64

// hookRecorder captures every SourceUnavailableHook invocation so
// the F6-A tests can assert on count and most-recent error.
type hookRecorder struct {
	mu     sync.Mutex
	errs   []error
	calls  atomic.Int64
	notify chan struct{} // closed lazily on first call when set
}

func (r *hookRecorder) fn() SourceUnavailableHook {
	return func(err error) {
		r.mu.Lock()
		r.errs = append(r.errs, err)
		r.mu.Unlock()
		r.calls.Add(1)
		if r.notify != nil {
			select {
			case r.notify <- struct{}{}:
			default:
			}
		}
	}
}

func (r *hookRecorder) snapshot() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]error, len(r.errs))
	copy(out, r.errs)

	return out
}

// gaugeMetrics is a SourceMetrics implementation that exposes the
// most-recent SetSourceBucketMissing value so tests can assert on the
// transitions without scraping logs.
type gaugeMetrics struct {
	missing atomic.Bool
	sets    atomic.Int64
}

func (g *gaugeMetrics) SetSourceBucketMissing(missing bool) {
	g.missing.Store(missing)
	g.sets.Add(1)
}

// f6aFixture bundles the four values an F6-A integration sub-test needs:
// the cached KV handle, the JetStream context (for triggering the bucket
// delete mid-test), the test context, and the bucket name.
type f6aFixture struct {
	kv     jetstream.KeyValue
	js     jetstream.JetStream
	ctx    context.Context
	bucket string
}

// newKVForF6ATest spins an embedded NATS, creates a fresh source
// bucket, and returns the fixture. The bucket name is unique per call
// so concurrent t.Parallel sub-tests do not collide.
func newKVForF6ATest(t *testing.T) f6aFixture {
	t.Helper()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	bucket := fmt.Sprintf("f6a-%d", f6aBucketSeq.Add(1))
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	return f6aFixture{kv: kv, js: js, ctx: ctx, bucket: bucket}
}

// TestNatsKV_F6A_BucketMissing_FiresHookAndSetsMetric covers the
// hook + metric path end-to-end with a real NATS server, a real
// bucket-delete, and an active reconciler.
func TestNatsKV_F6A_BucketMissing_FiresHookAndSetsMetric(t *testing.T) {
	t.Parallel()

	f := newKVForF6ATest(t)
	kv, js, ctx, bucket := f.kv, f.js, f.ctx, f.bucket

	rec := &hookRecorder{notify: make(chan struct{}, 8)}
	gauge := &gaugeMetrics{}
	src := NewNatsKV(kv, "config", nil,
		WithReconcileInterval(150*time.Millisecond),
		WithUnavailableHook(rec.fn()),
		WithMetrics(gauge),
	)
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(ctx) })

	require.False(t, gauge.missing.Load(),
		"gauge must read 0 on a healthy source pre-delete")

	require.NoError(t, js.DeleteKeyValue(ctx, bucket))

	// Wait for the hook to fire from a reconcile or watcher-restart tick.
	select {
	case <-rec.notify:
	case <-time.After(2 * time.Second):
		t.Fatalf("hook did not fire within 2s after bucket delete; calls=%d gauge=%t", rec.calls.Load(), gauge.missing.Load())
	}

	require.GreaterOrEqual(t, rec.calls.Load(), int64(1), "hook must fire at least once")
	errs := rec.snapshot()
	require.NotEmpty(t, errs)
	// Empirical reality (verified with a probe against nats.go v1.50.0):
	// after js.DeleteKeyValue, a cached KV's Get returns nats.ErrNoResponders
	// (the JetStream subject has no listener anymore), and the cached
	// watcher's Watch returns jetstream.ErrStreamNotFound (the backing
	// KV_<bucket> stream is gone). jetstream.ErrBucketNotFound is only
	// surfaced by the lookup-path js.KeyValue(...), which neither the
	// reconciler nor restartWatcher uses. The hook may legitimately receive
	// any of the three.
	lastErr := errs[len(errs)-1]
	require.True(t, isBucketUnavailableErr(lastErr),
		"hook error must be classified as bucket-unavailable; got %v", lastErr)
	require.True(t, gauge.missing.Load(),
		"metric gauge must read 1 (missing) after bucket-loss detection")
}

// TestNatsKV_F6A_HookCooldown_LimitsFiringRate ensures the hook is
// rate-limited (default 30s but overridable via a test seam) so the
// reconcile loop's tick cadence does not flood the caller.
func TestNatsKV_F6A_HookCooldown_LimitsFiringRate(t *testing.T) {
	t.Parallel()

	f := newKVForF6ATest(t)
	kv, js, ctx, bucket := f.kv, f.js, f.ctx, f.bucket

	rec := &hookRecorder{}
	gauge := &gaugeMetrics{}
	src := NewNatsKV(kv, "config", nil,
		WithReconcileInterval(80*time.Millisecond),
		WithUnavailableHook(rec.fn()),
		WithMetrics(gauge),
	)
	// Test-only seam: shorten the cooldown so the assertion runs in
	// bounded test time. Production default is 30s.
	src.unavailableCooldown = 250 * time.Millisecond

	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(ctx) })

	require.NoError(t, js.DeleteKeyValue(ctx, bucket))

	// Let the reconciler tick several times within the cooldown window.
	require.Eventually(t, func() bool { return rec.calls.Load() >= 1 },
		2*time.Second, 20*time.Millisecond,
		"first hook firing must arrive promptly after bucket delete")
	firstSeen := time.Now()

	// Polled at 4× the reconcile cadence; over one cooldown window the
	// hook must NOT fire a second time.
	time.Sleep(220 * time.Millisecond) // still inside the 250ms cooldown
	require.Equal(t, int64(1), rec.calls.Load(),
		"hook must be suppressed within the cooldown window; got %d calls", rec.calls.Load())

	// Advance past the cooldown and confirm a second firing is allowed.
	elapsed := time.Since(firstSeen)
	if elapsed < src.unavailableCooldown+50*time.Millisecond {
		time.Sleep(src.unavailableCooldown + 50*time.Millisecond - elapsed)
	}
	require.Eventually(t, func() bool { return rec.calls.Load() >= 2 },
		2*time.Second, 30*time.Millisecond,
		"hook must fire again after cooldown elapses")
}

// TestNatsKV_F6A_RecoveryClearsGauge confirms the gauge transitions
// back to 0 when the bucket is re-created. The hook is NOT re-fired
// on recovery — escalation is one-way; the gauge is the recovery
// signal.
func TestNatsKV_F6A_RecoveryClearsGauge(t *testing.T) {
	t.Parallel()

	f := newKVForF6ATest(t)
	kv, js, ctx, bucket := f.kv, f.js, f.ctx, f.bucket

	rec := &hookRecorder{notify: make(chan struct{}, 8)}
	gauge := &gaugeMetrics{}
	src := NewNatsKV(kv, "config", nil,
		WithReconcileInterval(100*time.Millisecond),
		WithUnavailableHook(rec.fn()),
		WithMetrics(gauge),
	)
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(ctx) })

	require.NoError(t, js.DeleteKeyValue(ctx, bucket))

	select {
	case <-rec.notify:
	case <-time.After(2 * time.Second):
		t.Fatal("hook did not fire within 2s after bucket delete")
	}
	require.True(t, gauge.missing.Load())
	preRecoveryCalls := rec.calls.Load()

	// Re-create the bucket under the same name; the next successful
	// kv.Get from the reconciler should clear the gauge.
	_, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	require.Eventually(t, func() bool { return !gauge.missing.Load() },
		3*time.Second, 50*time.Millisecond,
		"gauge must clear once the bucket is reachable again")
	require.Equal(t, preRecoveryCalls, rec.calls.Load(),
		"hook MUST NOT re-fire on recovery; escalation is one-way")
}

// TestNatsKV_F6A_NoHook_NoPanic exercises the documented "no hook
// configured" path: the reconcile keeps running, the metric is set,
// and nothing panics.
func TestNatsKV_F6A_NoHook_NoPanic(t *testing.T) {
	t.Parallel()

	f := newKVForF6ATest(t)
	kv, js, ctx, bucket := f.kv, f.js, f.ctx, f.bucket

	gauge := &gaugeMetrics{}
	src := NewNatsKV(kv, "config", nil,
		WithReconcileInterval(120*time.Millisecond),
		WithMetrics(gauge),
	)
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(ctx) })

	require.NoError(t, js.DeleteKeyValue(ctx, bucket))
	require.Eventually(t, func() bool { return gauge.missing.Load() },
		3*time.Second, 50*time.Millisecond,
		"gauge must still flip even without a hook configured")
}

// TestNatsKV_F6A_NoMetrics_NoPanic exercises the documented "no
// metrics configured" path: the hook still fires; nothing panics.
func TestNatsKV_F6A_NoMetrics_NoPanic(t *testing.T) {
	t.Parallel()

	f := newKVForF6ATest(t)
	kv, js, ctx, bucket := f.kv, f.js, f.ctx, f.bucket

	rec := &hookRecorder{notify: make(chan struct{}, 8)}
	src := NewNatsKV(kv, "config", nil,
		WithReconcileInterval(100*time.Millisecond),
		WithUnavailableHook(rec.fn()),
	)
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(ctx) })

	require.NoError(t, js.DeleteKeyValue(ctx, bucket))

	select {
	case <-rec.notify:
	case <-time.After(2 * time.Second):
		t.Fatal("hook did not fire within 2s after bucket delete")
	}
}

// TestNatsKV_F6A_DefaultCooldown_PureUnit exercises the cooldown
// arithmetic without spinning a NATS server. Asserts that three
// rapid-fire noteBucketUnavailable calls produce exactly one hook
// invocation under the default cooldown.
func TestNatsKV_F6A_DefaultCooldown_PureUnit(t *testing.T) {
	t.Parallel()

	rec := &hookRecorder{}
	src := &NatsKV{
		logger:          nil,
		metrics:         types.NopSourceMetrics{},
		unavailableHook: rec.fn(),
		// unavailableCooldown intentionally left at zero-value to
		// confirm the constructor-applied default is what the test
		// expects (NewNatsKV must set it to 30s; here we mimic the
		// default explicitly so the test is independent of that).
		unavailableCooldown: 30 * time.Second,
	}

	bucketGoneErr := fmt.Errorf("simulated: %w", jetstream.ErrBucketNotFound)
	for range 3 {
		require.True(t, src.noteBucketUnavailable(bucketGoneErr),
			"noteBucketUnavailable must return true on a wrapped ErrBucketNotFound")
	}
	require.Equal(t, int64(1), rec.calls.Load(),
		"three rapid-fire calls under a 30s cooldown must fire the hook exactly once")

	// Non-matching error must not advance the counters or fire.
	require.False(t, src.noteBucketUnavailable(errors.New("unrelated")))
	require.Equal(t, int64(1), rec.calls.Load())
}
