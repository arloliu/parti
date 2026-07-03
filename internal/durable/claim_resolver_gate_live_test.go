package durable

// In-package live-NATS proof that the reconciler's watcher-recovery
// contract survives with the scan gate active (spec §9 integration item
// 3). In-package so the watcher seams (watcherMu/currentWatcher,
// gateConfirmGap) are reachable — the same live pattern as
// claim_resolver_test.go:576 (TestClaimResolver_ReconcileRescueIncrementsMetric).

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestGateLive_ReconcileRecoveryWithGateActive is the live proof of
// ProbeKVStreamPos's success path on a real *jetstream.KeyValueBucketStatus,
// combined with the pre-existing watcher-recovery contract: even with the
// scan gate latched (real double-probe confirmation, shortened here via
// r.gateConfirmGap so the test does not have to wait out the 2s
// production default), a silently-stalled watcher must still be rescued by
// the periodic reconciler within one interval once the bucket position
// advances.
//
// The gate's own unit suite (claim_resolver_gate_test.go) proves this
// against a fake KV and a fake probe; this test proves it against a real
// embedded NATS server and the real streamPos probe wired via
// WithStreamPosProbe, so ProbeKVStreamPos's *jetstream.KeyValueBucketStatus
// type assertion and field mapping are exercised for real, not simulated.
func TestGateLive_ReconcileRecoveryWithGateActive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := "gate-live-recovery"
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	seed := handoff.NewInitialClaim("pA", "w1", time.Now(), time.Minute)
	seedBytes, err := seed.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pA", seedBytes)
	require.NoError(t, err)

	// Dedicated probe handle — never shared with kv, the production
	// handle, per the WithStreamPosProbe handle-ownership contract.
	probeKV, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)

	cl := &captureLogger{}
	r := NewClaimBasedResolver(kv, "claims/", cl,
		WithReconcileInterval(300*time.Millisecond),
		WithStreamPosProbe(probeKV),
	)
	// Shorten the confirm gap so the gate latches on a test timescale; this
	// is the in-package seam the interface-only integration tests cannot
	// reach.
	r.gateConfirmGap = 20 * time.Millisecond
	t.Cleanup(r.Stop)

	require.NoError(t, r.Start(ctx))

	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("pA")

		return ok && owner == "w1"
	}, 3*time.Second, 20*time.Millisecond, "resolver did not resolve pA at startup")

	// Let the gate latch across several gated reconcile ticks (300ms
	// interval + 20ms confirm gap each ⇒ several cycles in 1.5s).
	time.Sleep(1500 * time.Millisecond)

	// Status resolves through stream.Info and mutates the handle's cached
	// stream state, so even this sanity check needs its own handle — the
	// production handle is live inside the resolver.
	sanityKV, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)
	status, err := sanityKV.Status(ctx)
	require.NoError(t, err)
	require.Positive(t, status.Values(), "sanity: bucket must still carry the seeded entry")

	owner, _, _, ok := r.GetOwner("pA")
	require.True(t, ok)
	require.Equal(t, "w1", owner, "resolver must keep resolving while the gate is latched")

	// Assert the gate actually engaged BEFORE the recovery write below —
	// proving it was active when recovery happens, not permanently
	// failed-open (which would make this pin vacuous).
	require.True(t, cl.has("claim resolver reconcile gate engaged"),
		"the scan gate must have engaged (Debug line) during the latch-settling window, "+
			"before the watcher-kill + recovery write")

	// Kill the watcher out-of-band, simulating a silent stall (the nats.go
	// KV watcher does not surface NATS server restarts as a channel
	// close). If the supervisor races ahead and re-establishes the watcher
	// first, that is also fine — the point under test is convergence
	// within one reconcile interval regardless of which path delivers it.
	r.watcherMu.Lock()
	w := r.currentWatcher
	r.watcherMu.Unlock()
	_ = w.Stop()

	// Immediately write a new claim body for pA directly via kv.Put — the
	// watcher (stopped) cannot see this; only the gated reconciler (or a
	// supervisor-restarted watcher's replay) can deliver it.
	next := handoff.Claim{
		PartitionID: "pA",
		Owner:       "w2",
		State:       handoff.ClaimStateStable,
		Epoch:       seed.Epoch + 1,
		LastUpdated: time.Now().UTC(),
		TTLSeconds:  seed.TTLSeconds,
	}
	nextBytes, err := next.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pA", nextBytes)
	require.NoError(t, err)

	// Three seconds is several 300ms reconcile intervals: the gate must
	// unlatch on the LastSeq advance (a mismatching probe skips the
	// confirm-gap wait and runs a full pass immediately) and the
	// reconciler must apply the new claim body.
	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("pA")

		return ok && owner == "w2"
	}, 3*time.Second, 10*time.Millisecond,
		"reconciler did not recover pA to w2 within the bound; the gate must not "+
			"suppress recovery once the bucket position advances")
}
