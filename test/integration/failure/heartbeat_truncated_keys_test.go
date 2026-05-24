package failure_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/partitest"
)

// TestHeartbeatBucket_TruncatedKeysObservable is the diagnostic reproducer
// (T0) for F10-A. Its passing IS the empirical observation that justifies
// the worker-set shrink-confirmation defense in Calculator.getActiveWorkers
// and the worker-set floor in rebalance.
//
// What it pins: nats.go's jetstream.KeyValue.Keys() can return
// (partial-slice, nil) — a silently truncated read — when the per-call
// context is cancelled after at least one entry has been delivered but
// before the watcher's initial-pending marker. The mechanism, traced
// against nats.go v1.50.0 source:
//
//   - jetstream/kv.go:1372-1393: Keys() ranges over watcher.Updates()
//     and breaks on the nil marker; if the channel CLOSES before the
//     marker, the for-range exits with whatever has been received so far.
//   - jetstream/kv.go:1335-1339: SetClosedHandler closes the updates
//     channel when the underlying subscription terminates.
//   - js.go:2052-2058: a context-cancellation goroutine fires
//     sub.Unsubscribe() the moment the watcher's nats.Context(ctx)
//     cancels — propagating as a CLEAN subscription close, not an error
//     visible through Updates().
//
// Together: ctx cancellation mid-scan tears the subscription down, the
// closed-handler closes Updates(), Keys() compacts what it has, and
// returns (partial, nil) at kv.go:1391-1392 without ever surfacing the
// ctx error. Calculator code that trusts a (workers, nil) return as
// fresh ground truth would then act on a phantom mass-disappearance.
//
// This test stays in the tree as a forward-observation pin: if a future
// nats.go release closes this gap, the test FAILS (no truncation ever
// observed), prompting a review of whether the F10-A defense is still
// load-bearing.
//
// Why a loop: the per-attempt outcome is probabilistic (the cancel must
// race in between message-delivery and marker-send). Empirical hit rates
// on developer hardware fall in the 20–60 % range depending on scheduler
// load and `-race` overhead; calibrating against the upper end is
// optimistic for CI, which is typically slower and more contended.
// Sizing for the conservative case: at a 1 % per-attempt rate the
// 500-attempt loop drives P(false-negative) to (0.99)^500 ≈ 6.6e-3, and
// at the realistic 5 % the bound is (0.95)^500 ≈ 7e-12. If truncation
// is never observed across all 500 attempts that is itself a meaningful
// signal — either the nats.go behavior has changed or the test
// environment cannot reproduce the race — and the failure message
// reports the per-bucket counts so the operator can triage.
func TestHeartbeatBucket_TruncatedKeysObservable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, nc := partitest.StartEmbeddedNATS(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket:  "heartbeat_trunc_probe",
		History: 1,
	})
	require.NoError(t, err)

	// Pre-populate the bucket with enough heartbeat keys that the scan
	// cannot complete inside the cancel window. 500 was calibrated
	// empirically on an embedded NATS / localhost path; smaller counts
	// often complete before the cancel fires, larger counts add runtime
	// without raising the per-attempt hit rate.
	const totalKeys = 500
	for i := range totalKeys {
		key := fmt.Sprintf("hb.worker-%04d", i)
		_, err := kv.PutString(t.Context(), key, "x")
		require.NoError(t, err)
	}

	// Sweep the cancel delay across ~50µs..10ms. Different delays land
	// in different points of the scan; the wider spread hardens the
	// reproducer against slow/contended CI where the post-first-
	// message / pre-marker window falls outside the optimistic
	// developer-hardware band.
	const attempts = 500
	var (
		observedTruncated  int
		observedFull       int
		observedNoKeys     int
		observedCtxErr     int
		observedOtherErr   int
		minObservedPartial = totalKeys
		maxObservedPartial int
	)

	for i := range attempts {
		delay := time.Duration(50+(i*20)) * time.Microsecond

		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(delay, cancel)

		keys, kerr := kv.Keys(ctx)
		cancel()

		switch {
		case kerr != nil:
			switch {
			case errors.Is(kerr, jetstream.ErrNoKeysFound):
				observedNoKeys++
			case errors.Is(kerr, context.Canceled), errors.Is(kerr, context.DeadlineExceeded):
				observedCtxErr++
			default:
				observedOtherErr++
				t.Logf("attempt %d: unexpected err=%v (delay=%v)", i, kerr, delay)
			}
		case len(keys) == totalKeys:
			observedFull++
		case len(keys) > 0 && len(keys) < totalKeys:
			observedTruncated++
			if len(keys) < minObservedPartial {
				minObservedPartial = len(keys)
			}
			if len(keys) > maxObservedPartial {
				maxObservedPartial = len(keys)
			}
		default:
			t.Logf("attempt %d: unclassified result len=%d err=%v", i, len(keys), kerr)
		}
	}

	t.Logf("results over %d attempts: truncated=%d full=%d no-keys=%d ctx-err=%d other-err=%d",
		attempts, observedTruncated, observedFull, observedNoKeys, observedCtxErr, observedOtherErr)
	if observedTruncated > 0 {
		t.Logf("truncated read sizes: min=%d max=%d (totalKeys=%d)",
			minObservedPartial, maxObservedPartial, totalKeys)
	}

	require.Positive(t, observedTruncated,
		"expected to observe at least one (partial, nil) result from Keys() over %d attempts; "+
			"if this assertion fails, the nats.go truncation behavior has changed or the "+
			"environment cannot reproduce the race. Counts: full=%d no-keys=%d ctx-err=%d other-err=%d",
		attempts, observedFull, observedNoKeys, observedCtxErr, observedOtherErr)
}
