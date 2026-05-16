package durable

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestClaimResolver_ReconcileRescueIncrementsMetric verifies that
// IncReconcileRescue fires when the reconciler applies a missed update.
// The watcher is stopped cooperatively first so the reconciler is the only
// path to convergence; once it catches up, the rescue metric must have
// incremented.
func TestClaimResolver_ReconcileRescueIncrementsMetric(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	ms := newMetricsSpy()
	// Disable drift restart so the rescue metric is the only signal
	// under test here (avoid confounding with the restart machinery).
	r := NewClaimBasedResolver(
		kv, "claims/", nil,
		WithReconcileInterval(50*time.Millisecond),
		WithDriftRestartCooldown(0),
	)
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// Stop the watcher cooperatively. Without drift-driven restart, the
	// supervisor's establish path may re-create it, but we use the
	// reconcile metric (not watcher state) as the signal here.
	require.NoError(t, r.watcher.Stop())

	// Write a claim so the reconciler observes drift on its next tick.
	c := handoff.Claim{
		PartitionID: "pRescue", Owner: "wR",
		State:       handoff.ClaimStateStable,
		Epoch:       1,
		LastUpdated: time.Now().UTC(),
	}
	b, err := c.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pRescue", b)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return ms.reconcileRescueCount() >= 1
	}, 5*time.Second, 25*time.Millisecond,
		"IncReconcileRescue must fire when reconcileOnce applies drift recovery")
}

// TestClaimResolver_ReconcileNoRescueWhenNoDrift asserts the rescue metric
// stays at zero across many reconcile ticks in steady state — i.e., the
// metric is precise to actual drift, not a per-tick counter.
func TestClaimResolver_ReconcileNoRescueWhenNoDrift(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	c := handoff.Claim{
		PartitionID: "pSteady", Owner: "wS",
		State:       handoff.ClaimStateStable,
		Epoch:       1,
		LastUpdated: time.Now().UTC(),
	}
	b, err := c.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pSteady", b)
	require.NoError(t, err)

	ms := newMetricsSpy()
	r := NewClaimBasedResolver(
		kv, "claims/", nil,
		WithReconcileInterval(50*time.Millisecond),
	)
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// Allow the watcher to populate the cache.
	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("pSteady")
		return ok && owner == "wS"
	}, 2*time.Second, 10*time.Millisecond)

	// Wait long enough for at least six reconcile ticks to fire. Negative
	// assertion: rescue counter must remain zero across the window.
	const reconcileTicks = 6
	settle := time.Duration(reconcileTicks) * 50 * time.Millisecond
	time.Sleep(settle)

	require.Zero(t, ms.reconcileRescueCount(),
		"IncReconcileRescue must NOT fire when reconcile finds no drift")
}

// TestClaimResolver_DriftTriggersWatcherRestart cooperatively stops the
// watcher, writes a claim, and asserts that the reconciler both rescues the
// cache AND signals the supervisor to restart the watcher under the
// "drift_detected" reason. After restart, a subsequent write must reach
// the cache via the new watcher.
func TestClaimResolver_DriftTriggersWatcherRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	ms := newMetricsSpy()
	// 50ms reconcile + tiny cooldown so the drift-restart fires promptly.
	r := NewClaimBasedResolver(
		kv, "claims/", nil,
		WithReconcileInterval(50*time.Millisecond),
		WithDriftRestartCooldown(100*time.Millisecond),
	)
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// Stop the watcher cooperatively — its channel closes; if no drift
	// signal fires the supervisor restarts under "channel_closed".
	require.NoError(t, r.watcher.Stop())

	// Write a claim. The reconciler observes the drift (cache is empty,
	// KV has the claim), emits rescue, and signals drift-driven restart.
	c1 := handoff.Claim{
		PartitionID: "pD1", Owner: "wA",
		State:       handoff.ClaimStateStable,
		Epoch:       1,
		LastUpdated: time.Now().UTC(),
	}
	b1, err := c1.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pD1", b1)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return ms.reconcileRescueCount() >= 1
	}, 5*time.Second, 25*time.Millisecond,
		"reconcile rescue must fire after drift observed")

	require.Eventually(t, func() bool {
		return ms.watcherRestartCount(watcherRestartReasonDriftDetected) >= 1
	}, 5*time.Second, 25*time.Millisecond,
		"watcher restart must be classified as drift_detected")

	// Verify the new watcher is actually serving updates: write a second
	// claim and assert it reaches the cache. (The race between watcher
	// re-replay and direct Updates() delivery doesn't matter; either
	// arrival path proves the new watcher is live.)
	c2 := handoff.Claim{
		PartitionID: "pD2", Owner: "wB",
		State:       handoff.ClaimStateStable,
		Epoch:       1,
		LastUpdated: time.Now().UTC(),
	}
	b2, err := c2.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pD2", b2)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("pD2")
		return ok && owner == "wB"
	}, 5*time.Second, 25*time.Millisecond,
		"new watcher must deliver subsequent writes to the cache")
}

// TestClaimResolver_DriftRestartRespectsCooldown drives two distinct drift
// events within 1 second and asserts the cooldown rate-limits drift-driven
// restarts to exactly one. The rescue metric may fire more than once (each
// reconcile drift bumps it), but the drift_detected watcher restart fires
// at most once per cooldown.
func TestClaimResolver_DriftRestartRespectsCooldown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	ms := newMetricsSpy()
	// 5s cooldown vs 50ms reconcile: the second drift event will land
	// well inside the cooldown window.
	r := NewClaimBasedResolver(
		kv, "claims/", nil,
		WithReconcileInterval(50*time.Millisecond),
		WithDriftRestartCooldown(5*time.Second),
	)
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// First drift event: stop the initial watcher and write claim1.
	require.NoError(t, r.watcher.Stop())
	c1 := handoff.Claim{
		PartitionID: "pCool1", Owner: "wA",
		State: handoff.ClaimStateStable, Epoch: 1, LastUpdated: time.Now().UTC(),
	}
	b1, err := c1.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pCool1", b1)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return ms.watcherRestartCount(watcherRestartReasonDriftDetected) >= 1
	}, 5*time.Second, 25*time.Millisecond,
		"first drift restart should fire")

	// At this point the supervisor has re-established the watcher, which
	// replays history and converges the cache with KV. To drive a second
	// drift event INSIDE the cooldown, stop the new watcher again and
	// write claim2. The reconciler must rescue (cache misses claim2) and
	// invoke requestWatcherRestartFromReconcile — which the cooldown
	// must short-circuit, leaving drift_detected at exactly 1. The
	// cooperative Stop will register as channel_closed instead.
	require.Eventually(t, func() bool {
		r.watcherMu.Lock()
		w := r.currentWatcher
		r.watcherMu.Unlock()
		// The supervisor has re-established when currentWatcher differs
		// from the initial r.watcher.
		return w != nil && w != r.watcher
	}, 2*time.Second, 25*time.Millisecond,
		"supervisor should have re-established the watcher")

	r.watcherMu.Lock()
	w2 := r.currentWatcher
	r.watcherMu.Unlock()
	require.NoError(t, w2.Stop())

	c2 := handoff.Claim{
		PartitionID: "pCool2", Owner: "wB",
		State: handoff.ClaimStateStable, Epoch: 1, LastUpdated: time.Now().UTC(),
	}
	b2, err := c2.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pCool2", b2)
	require.NoError(t, err)

	// Wait for the second drift to be rescued by the reconciler.
	require.Eventually(t, func() bool {
		return ms.reconcileRescueCount() >= 2
	}, 5*time.Second, 25*time.Millisecond,
		"second drift event must increment rescue counter")

	// Bounded negative assertion: drift_detected must remain at 1 across
	// a window well below the 5s cooldown.
	const probeDeadline = 1 * time.Second
	deadline := time.Now().Add(probeDeadline)
	for time.Now().Before(deadline) {
		require.LessOrEqual(t, ms.watcherRestartCount(watcherRestartReasonDriftDetected), 1,
			"cooldown must rate-limit drift restarts to one within the window")
		time.Sleep(50 * time.Millisecond)
	}

	require.Equal(t, 1, ms.watcherRestartCount(watcherRestartReasonDriftDetected),
		"exactly one drift_detected restart within the cooldown window")
}

// TestClaimResolver_DriftRestartDisabledByZeroCooldown verifies that a zero
// cooldown disables the watcher-restart half of the drift signal entirely:
// the rescue metric still fires but no watcher restart is issued.
func TestClaimResolver_DriftRestartDisabledByZeroCooldown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	ms := newMetricsSpy()
	r := NewClaimBasedResolver(
		kv, "claims/", nil,
		WithReconcileInterval(50*time.Millisecond),
		WithDriftRestartCooldown(0), // disable drift-driven restart
	)
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// Stop the watcher; we are about to write a claim that will appear
	// only via the reconciler.
	require.NoError(t, r.watcher.Stop())

	c := handoff.Claim{
		PartitionID: "pZero", Owner: "wZ",
		State: handoff.ClaimStateStable, Epoch: 1, LastUpdated: time.Now().UTC(),
	}
	b, err := c.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pZero", b)
	require.NoError(t, err)

	// Wait for the rescue to fire — proves reconcile saw the drift.
	require.Eventually(t, func() bool {
		return ms.reconcileRescueCount() >= 1
	}, 5*time.Second, 25*time.Millisecond,
		"rescue metric must fire even when drift-restart is disabled")

	// Now assert (negative) that no drift_detected restart is emitted
	// within a bounded window. We use a short fixed sleep here because
	// the assertion is "no event occurred", which fundamentally requires
	// a bounded wait — there is no event-driven primitive that can
	// distinguish "hasn't happened yet" from "will never happen".
	time.Sleep(400 * time.Millisecond)
	require.Zero(t, ms.watcherRestartCount(watcherRestartReasonDriftDetected),
		"WithDriftRestartCooldown(0) must disable drift-driven restart")
}

// TestClaimResolver_DriftRestartReasonClassifiedCorrectly is the regression
// guard for the supervise reason CAS. A drift-driven restart must classify
// as "drift_detected"; a subsequent cooperative close must classify as
// "channel_closed" because the CAS consumed the pending flag on the first
// restart.
func TestClaimResolver_DriftRestartReasonClassifiedCorrectly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	ms := newMetricsSpy()
	r := NewClaimBasedResolver(
		kv, "claims/", nil,
		WithReconcileInterval(50*time.Millisecond),
		WithDriftRestartCooldown(100*time.Millisecond),
	)
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// Phase A: drive a drift restart. Stop the watcher and write a
	// claim; the reconciler will rescue + signal restart.
	require.NoError(t, r.watcher.Stop())
	cA := handoff.Claim{
		PartitionID: "pClsA", Owner: "wA",
		State: handoff.ClaimStateStable, Epoch: 1, LastUpdated: time.Now().UTC(),
	}
	bA, err := cA.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pClsA", bA)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return ms.watcherRestartCount(watcherRestartReasonDriftDetected) >= 1
	}, 5*time.Second, 25*time.Millisecond,
		"phase A: drift restart must classify as drift_detected")

	// Snapshot counts before phase B so we can isolate the second event.
	drift1 := ms.watcherRestartCount(watcherRestartReasonDriftDetected)
	closed1 := ms.watcherRestartCount(watcherRestartReasonChannelClosed)

	// Phase B: cooperative close on the new watcher. Wait briefly for
	// the supervisor to have stored the new watcher in currentWatcher,
	// then stop it. We do NOT touch the KV here — there is no drift
	// trigger, so the next restart must classify as channel_closed.
	require.Eventually(t, func() bool {
		r.watcherMu.Lock()
		w := r.currentWatcher
		r.watcherMu.Unlock()
		return w != nil
	}, 2*time.Second, 25*time.Millisecond)

	r.watcherMu.Lock()
	w := r.currentWatcher
	r.watcherMu.Unlock()
	require.NotNil(t, w)
	require.NoError(t, w.Stop())

	require.Eventually(t, func() bool {
		return ms.watcherRestartCount(watcherRestartReasonChannelClosed) >= closed1+1
	}, 5*time.Second, 25*time.Millisecond,
		"phase B: cooperative close must classify as channel_closed")

	// Phase B must NOT have incremented drift_detected — the CAS in
	// phase A consumed the pending flag.
	require.Equal(t, drift1, ms.watcherRestartCount(watcherRestartReasonDriftDetected),
		"second restart (no drift signal) must NOT classify as drift_detected")
}
