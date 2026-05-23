package durable

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestClaimResolver_WatcherRestartBoundedAndEscalates is the P2.4b
// reproducer pinned by `docs/plans/self-healing/00-fix-plan.md` § P2.4b:
//
//   - T1: Delete the handoff bucket while the resolver runs; assert
//     bounded retries and that an exhaustion signal fires once.
//
// Before the F2 envelope is wired in, the supervisor's watcher-restart
// loop spins forever on a vanished bucket, generating unbounded
// `kv.WatchAll` API load against the deleted stream and producing no
// operator-visible escalation. The bound + escalation lets the
// reconciler remain the load-bearing recovery path while signalling
// that the watcher itself has given up — the same shape as the
// source-watcher envelope shipped in P2.4a.
func TestClaimResolver_WatcherRestartBoundedAndEscalates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Tighten the envelope so the test completes in a few seconds
	// rather than the production worst-case ~80s. Save and restore so
	// other tests in the package see the production defaults.
	origMax := watcherMaxAttempts
	origBase := watcherBaseBackoff
	origCap := watcherMaxBackoff
	watcherMaxAttempts = 3
	watcherBaseBackoff = 30 * time.Millisecond
	watcherMaxBackoff = 60 * time.Millisecond
	defer func() {
		watcherMaxAttempts = origMax
		watcherBaseBackoff = origBase
		watcherMaxBackoff = origCap
	}()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff-bounded"})
	require.NoError(t, err)

	// Seed one claim so the cache has something to converge on.
	initial := handoff.Claim{
		PartitionID: "p1",
		Owner:       "worker-A",
		State:       handoff.ClaimStateStable,
		Epoch:       1,
		LastUpdated: time.Now().UTC(),
	}
	b, err := initial.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/p1", b)
	require.NoError(t, err)

	ms := newMetricsSpy()
	var exhaustCount atomic.Int32
	var exhaustErr atomic.Value // error
	r := NewClaimBasedResolver(kv, "claims/", nil,
		WithReconcileInterval(0),
		WithWatcherRetryExhausted(func(err error) {
			exhaustCount.Add(1)
			exhaustErr.Store(err)
		}),
	)
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("p1")
		return ok && owner == "worker-A"
	}, 2*time.Second, 10*time.Millisecond, "initial cache convergence")

	// Delete the bucket and then force the current watcher's channel
	// to close. The bucket delete by itself does not necessarily close
	// an already-bound watcher's Updates() channel (the nats.go KV
	// watcher's channel does not close on a NATS server restart either
	// — see project_nats_watcher_empirical_finding). Manually stopping
	// the watcher triggers the supervisor's restart path, and the
	// subsequent runWatcher calls then hit the now-deleted bucket and
	// fail repeatedly, exhausting the envelope budget.
	require.NoError(t, js.DeleteKeyValue(ctx, "handoff-bounded"))
	require.NotNil(t, r.watcher)
	// Stop() may itself error against a now-deleted stream
	// (jetstream.ErrStreamNotFound from the cached handle); that is
	// fine — the call still closes the local channel which is what we
	// need to trigger supervise's restart path.
	_ = r.watcher.Stop()

	// The envelope should reach its budget within roughly:
	//   3 attempts × (base + jittered backoff up to cap) ≈ 0–200ms.
	// Add slack for scheduling. After the budget is exhausted the
	// supervise goroutine exits and the exhaustion callback fires
	// exactly once.
	require.Eventually(t, func() bool {
		return exhaustCount.Load() == 1
	}, 5*time.Second, 10*time.Millisecond,
		"OnWatcherRetryExhausted must fire exactly once after the envelope budget is consumed")

	// The bound: establish_failed counts must NOT exceed MaxAttempts.
	// (Pre-fix the loop would spin forever; this is the load-bearing
	// regression assertion.)
	require.LessOrEqual(t, ms.watcherRestartCount(watcherRestartReasonEstablishFailed),
		watcherMaxAttempts,
		"establish_failed restarts must be bounded by MaxAttempts; "+
			"unbounded count is the original failure mode this PR closes")

	// The dedicated exhaustion-reason metric must fire exactly once so
	// operators can alert on the give-up event.
	require.Equal(t, 1, ms.watcherRestartCount(watcherRestartReasonExhausted),
		"IncWatcherRestart(\"exhausted\") must fire exactly once at exhaustion")

	// Hold the assertion that the captured error is non-nil; it should
	// be the last establishment error the envelope saw.
	require.NotNil(t, exhaustErr.Load(),
		"exhaustion callback must receive the underlying establishment error")
}
