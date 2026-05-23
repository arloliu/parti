package source

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// f8BucketSeq makes each sub-test get a fresh KV bucket name without
// embedding t.Name() (which contains characters NATS rejects).
var f8BucketSeq atomic.Uint64

// reconcileDisabledSpy captures WARN messages so the F8 reconciler-
// disabled tests can assert the warning fires (or stays silent)
// depending on whether a leadership probe is wired.
type reconcileDisabledSpy struct {
	mu    sync.Mutex
	warns []string
}

func (l *reconcileDisabledSpy) Debug(string, ...any) {}
func (l *reconcileDisabledSpy) Info(string, ...any)  {}
func (l *reconcileDisabledSpy) Warn(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}
func (l *reconcileDisabledSpy) Error(string, ...any) {}
func (l *reconcileDisabledSpy) Fatal(string, ...any) {}

// snapshot returns a copy of the recorded WARN lines so callers can
// inline the substring assertion (avoids unparam complaints on a
// helper whose argument is a single test-scoped constant).
func (l *reconcileDisabledSpy) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.warns))
	copy(out, l.warns)

	return out
}

var _ types.Logger = (*reconcileDisabledSpy)(nil)

// newKVForReconcileWarningTest spins up an embedded NATS, creates a KV
// bucket, and returns the bucket handle ready for NewNatsKV — extracted
// so the four sub-tests below share the boilerplate without duplicating
// it.
func newKVForReconcileWarningTest(t *testing.T) (jetstream.KeyValue, context.Context) {
	t.Helper()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	bucket := fmt.Sprintf("f8warn-%d", f8BucketSeq.Add(1))
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: bucket,
	})
	require.NoError(t, err)

	return kv, ctx
}

// TestNatsKV_WarnsWhenReconcilerDisabled covers the four meaningful
// configurations of WithReconcileInterval × WithLeadershipProbe:
//
//   - interval <= 0 AND no probe → reconciler completely disabled →
//     server-restart recovery does not work → MUST warn.
//   - interval <= 0 AND probe wired → reconciler still runs (driven by
//     the probe's cadence choice) → MUST be silent.
//   - default interval (no option) → reconciler runs normally → silent.
//   - explicit positive interval → reconciler runs normally → silent.
//
// The warning is the only signal that surfaces this readiness-blind
// configuration to operators (the reconcileLoop silently exits at
// source/nats_kv.go L812-815). See memory pin
// project_nats_watcher_empirical_finding for why the reconciler is
// load-bearing.
func TestNatsKV_WarnsWhenReconcilerDisabled(t *testing.T) {
	const warnSubstr = "source reconciler disabled"

	countWarns := func(spy *reconcileDisabledSpy) int {
		n := 0
		for _, w := range spy.snapshot() {
			if strings.Contains(w, warnSubstr) {
				n++
			}
		}

		return n
	}

	t.Run("interval=0, no probe → warns", func(t *testing.T) {
		kv, ctx := newKVForReconcileWarningTest(t)
		spy := &reconcileDisabledSpy{}
		src := NewNatsKV(kv, "config", spy, WithReconcileInterval(0))
		require.NoError(t, src.Start(ctx))
		t.Cleanup(func() { _ = src.Stop(ctx) })

		require.Equal(t, 1, countWarns(spy),
			"reconciler disabled + no leadership probe must warn exactly once")
	})

	t.Run("interval=0, probe set → silent", func(t *testing.T) {
		kv, ctx := newKVForReconcileWarningTest(t)
		spy := &reconcileDisabledSpy{}
		src := NewNatsKV(kv, "config", spy,
			WithReconcileInterval(0),
			WithLeadershipProbe(func() bool { return true }),
		)
		require.NoError(t, src.Start(ctx))
		t.Cleanup(func() { _ = src.Stop(ctx) })

		require.Equal(t, 0, countWarns(spy),
			"a wired leadership probe drives the reconciler; the warning must NOT fire")
	})

	t.Run("default interval → silent", func(t *testing.T) {
		kv, ctx := newKVForReconcileWarningTest(t)
		spy := &reconcileDisabledSpy{}
		src := NewNatsKV(kv, "config", spy) // no options; defaults apply
		require.NoError(t, src.Start(ctx))
		t.Cleanup(func() { _ = src.Stop(ctx) })

		require.Equal(t, 0, countWarns(spy),
			"default reconciler interval is the happy path; warning must not fire")
	})

	t.Run("negative interval, no probe → warns", func(t *testing.T) {
		kv, ctx := newKVForReconcileWarningTest(t)
		spy := &reconcileDisabledSpy{}
		src := NewNatsKV(kv, "config", spy, WithReconcileInterval(-1*time.Second))
		require.NoError(t, src.Start(ctx))
		t.Cleanup(func() { _ = src.Stop(ctx) })

		require.Equal(t, 1, countWarns(spy),
			"a negative interval is treated the same as zero by reconcileLoop and must warn")
	})

	t.Run("nil logger does not panic", func(t *testing.T) {
		kv, ctx := newKVForReconcileWarningTest(t)
		require.NotPanics(t, func() {
			src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
			require.NoError(t, src.Start(ctx))
			_ = src.Stop(ctx)
		})
	})
}
