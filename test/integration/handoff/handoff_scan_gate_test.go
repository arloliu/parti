package handoff_test

// Live-NATS contracts for the reconcile/sweep scan gate (spec §9 integration
// items 1 and 2): an idle handoff bucket must stay consumer-flat under the
// gate, scans must resume promptly on real activity, and a rebalance must
// still converge end-to-end with two-phase handoff enabled.

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// scanGateComponent bundles one simulated worker's gated pair: a two-phase
// coordinator over a probed ClaimStore, and a ClaimBasedResolver with its own
// probed reconciler. Both halves are wired with the handle discipline the
// scan gate requires in production: a DEDICATED production handle and a
// DEDICATED probe handle per component, each obtained via its own
// js.KeyValue(ctx, bucket) call. Sharing a handle between production
// reads/writes and the gate's probe would race the cached *stream state
// (the epoch-monitor race class); newScanGateComponent never does that.
type scanGateComponent struct {
	store    handoff.ClaimStore
	coord    handoff.Coordinator
	resolver *durable.ClaimBasedResolver
}

// newScanGateComponent opens four separate KV handles on bucket (store,
// store-probe, resolver, resolver-probe) and builds one simulated worker's
// gated pair at cadence. The unexported confirm-gap fields on both the
// coordinator and the resolver are not reachable from this package and stay
// at their 2s production defaults; cadence only controls how often each
// component's ticker fires, which the scan-gate budget math accounts for.
func newScanGateComponent(t *testing.T, ctx context.Context, js jetstream.JetStream, bucket string, cadence time.Duration) scanGateComponent {
	t.Helper()

	storeKV, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)
	storeProbeKV, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)
	resKV, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)
	resProbeKV, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)

	store := handoff.NewNATSClaimStoreWithProbe(storeKV, storeProbeKV, "claims/")
	coord := handoff.New(handoff.Config{
		Store:         store,
		SweepInterval: cadence,
		TTL:           time.Minute,
		Now:           time.Now,
	}, true)
	resolver := durable.NewClaimBasedResolver(resKV, "claims/", nil,
		durable.WithReconcileInterval(cadence),
		durable.WithStreamPosProbe(resProbeKV),
	)

	return scanGateComponent{store: store, coord: coord, resolver: resolver}
}

// TestHandoffScanGate_IdleBucketConsumerFlatness pins the flatness contract
// (spec §9 integration item 1) end-to-end against real embedded NATS: it
// drives the two REAL gated components (ClaimBasedResolver +
// twoPhaseCoordinator over natsClaimStore) directly, rather than a full
// FDC-shaped manager+consumer cluster. This exercises the exact production
// code paths (default streamPos/BucketPos probe, real kv.Status, real
// Keys()/ListKeys() consumers) with deterministic budget math; the
// full-stack path is separately covered by
// TestHandoffScanGate_RebalanceConvergence below.
//
// Non-vacuity: nats.go's kv.Keys() creates a throwaway ephemeral ordered
// consumer PER CALL (see the reference cited in AGENTS.md's KV-watcher
// churn note); kv.Status() (the gate's probe) does not. A full
// reconcile/sweep pass calls Keys() exactly once; a gated skip calls neither
// Keys() nor Get() — only Status(). Counting
// $JS.EVENT.ADVISORY.CONSUMER.CREATED.KV_<bucket>.> messages is therefore a
// direct, non-vacuous proxy for "did a full scan run".
func TestHandoffScanGate_IdleBucketConsumerFlatness(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := fmt.Sprintf("itest-handoff-scan-gate-%d", time.Now().UnixNano())
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	// Dedicated connection for the advisory subscriber: it must never share
	// a handle with any production or probe KV operation.
	advNC, err := nats.Connect(nc.ConnectedUrl(), nats.Timeout(2*time.Second))
	require.NoError(t, err)
	t.Cleanup(advNC.Close)

	var created atomic.Int64
	advSubject := fmt.Sprintf("$JS.EVENT.ADVISORY.CONSUMER.CREATED.KV_%s.>", bucket)
	sub, err := advNC.Subscribe(advSubject, func(*nats.Msg) { created.Add(1) })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, advNC.Flush())

	// Three simulated workers (FDC-shaped, scaled down): each gets its own
	// coordinator + resolver pair at an aggressive 500ms cadence, all four
	// handles per component freshly opened.
	const numWorkers = 3
	const cadence = 500 * time.Millisecond
	components := make([]scanGateComponent, numWorkers)
	for i := range components {
		components[i] = newScanGateComponent(t, ctx, js, bucket, cadence)
	}

	// Seed 20 stable claims (owner "w1") via one store before starting
	// anything — plain KV Puts, not Keys()/Watch() calls, so seeding does
	// not disturb the advisory counter.
	const numClaims = 20
	seedPIDs := make([]string, numClaims)
	for j := range seedPIDs {
		pid := fmt.Sprintf("gate-flat-p%d", j)
		seedPIDs[j] = pid
		_, err := components[0].store.PutIfEpoch(ctx, pid, 0,
			handoff.NewInitialClaim(pid, "w1", time.Now(), time.Minute))
		require.NoError(t, err)
	}

	for i := range components {
		components[i].coord.Start(ctx)
		require.NoError(t, components[i].resolver.Start(ctx))
		t.Cleanup(components[i].resolver.Stop)
	}

	// Active window: several passes at 500ms cadence before the gate
	// latches. Non-vacuity check — the advisory counter must have observed
	// at least one KV_<bucket> consumer creation (each resolver's Start
	// alone issues a warm() Keys() call and a WatchAll() watcher, both
	// ephemeral consumers; each component's first reconcile/sweep tick adds
	// one more).
	time.Sleep(3 * time.Second)
	active := created.Load()
	require.GreaterOrEqual(t, active, int64(1),
		"advisory counter must observe at least one KV_<bucket> consumer creation "+
			"during the active window (harness non-vacuity: the counter must actually count)")

	// Idle window: 10s soak, no writes anywhere. Ungated, 6 components
	// (3 resolvers + 3 coordinators) ticking every 500ms would create
	// ~120 consumers in this window (6 * 10s / 500ms = 120) — one Keys()
	// call per tick. Gated, each tick that finds the bucket unchanged pays
	// the (unexported, still-2s-default) confirm gap INSIDE the pass before
	// it can skip, so a component completes at most ~5 gated cycles in 10s
	// — and none of those cycles calls Keys() at all. The only Keys() calls
	// expected in this window are the tail of the initial latch-settling
	// from the active window and any CI-load jitter, which is why the
	// budget below is a 10x-reduction floor (12, not 0) rather than an
	// exact zero.
	time.Sleep(10 * time.Second)
	idle := created.Load() - active
	const idleBudget = 12
	require.LessOrEqual(t, idle, int64(idleBudget),
		"idle handoff bucket must stay near consumer-flat under the scan gate "+
			"(observed %d full-scan consumer creations across 6 components over 10s; "+
			"ungated baseline for this shape is ~120; deleting the gate code must make "+
			"this assertion fail by an order of magnitude)", idle)
	t.Logf("idle-window consumer creations: %d (budget %d, ungated baseline ~120)", idle, idleBudget)

	// Re-engage proof: one claim update via the store advances the bucket
	// position. The first mismatching probe on either the resolver or the
	// coordinator runs its full pass IMMEDIATELY (no confirm-gap wait on
	// the mismatch path), so scans must resume well within one cadence +
	// one confirm gap.
	baseline := created.Load()
	updated := handoff.Claim{
		PartitionID: seedPIDs[0],
		Owner:       "w2",
		State:       handoff.ClaimStateStable,
		Epoch:       2,
		LastUpdated: time.Now().UTC(),
		TTLSeconds:  int64(time.Minute.Seconds()),
	}
	_, err = components[0].store.PutIfEpoch(ctx, seedPIDs[0], 1, updated)
	require.NoError(t, err)

	time.Sleep(3 * time.Second)
	require.Greater(t, created.Load(), baseline,
		"a claim write must re-engage scans (the mismatching probe runs a full pass "+
			"with no confirm-gap wait) — the gate must not have failed permanently latched")
}

// TestHandoffScanGate_RebalanceConvergence pins spec §9 integration item 2:
// a real manager cluster with EnableTwoPhaseHandoff must still converge
// assignments and claims end-to-end through join and leave, with the scan
// gate active on both the coordinator's sweep and every worker's resolver.
// Unlike the flatness test above, this does not count advisories — it pins
// that scans engage on real cluster activity through the full manager
// stack, complementing the component-level flatness/re-engage proof.
func TestHandoffScanGate_RebalanceConvergence(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = fmt.Sprintf("itest-handoff-scan-gate-rebalance-%d", time.Now().UnixNano())

	const numPartitions = 30
	src := source.NewStatic(testutil.CreateTestPartitions(numPartitions))

	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	defer cluster.StopWorkers()

	for range 3 {
		mgr := cluster.AddWorker(ctx)
		require.NoError(t, mgr.Start(ctx))
	}
	cluster.WaitForStableState(20 * time.Second)
	cluster.WaitForPartitionCoverage(numPartitions, 15*time.Second)
	requireAllClaimsStable(t, ctx, cluster.JS, cfg.KVBuckets.HandoffBucket, numPartitions)

	// Add a worker mid-soak: assignments must rebalance and every claim
	// must converge back to stable.
	newWorker := cluster.AddWorker(ctx)
	require.NoError(t, newWorker.Start(ctx))
	cluster.WaitForStableState(20 * time.Second)
	cluster.WaitForPartitionCoverage(numPartitions, 15*time.Second)
	requireAllClaimsStable(t, ctx, cluster.JS, cfg.KVBuckets.HandoffBucket, numPartitions)

	// Remove it again: re-convergence.
	cluster.RemoveWorker(3)
	cluster.WaitForStableState(20 * time.Second)
	cluster.WaitForPartitionCoverage(numPartitions, 15*time.Second)
	requireAllClaimsStable(t, ctx, cluster.JS, cfg.KVBuckets.HandoffBucket, numPartitions)
}

// requireAllClaimsStable polls InspectHandoffClaims until at least
// minClaims claims exist and every one of them has converged to Stable, or
// fails the test after the timeout. Used to pin end-to-end convergence
// after a rebalance without depending on which internal path (watcher or
// gated reconciler/sweep) delivered it.
func requireAllClaimsStable(t *testing.T, ctx context.Context, js jetstream.JetStream, bucket string, minClaims int) {
	t.Helper()

	require.Eventually(t, func() bool {
		claims, err := parti.InspectHandoffClaims(ctx, js, bucket)
		if err != nil || len(claims) < minClaims {
			return false
		}
		for _, c := range claims {
			if c.State != parti.HandoffClaimStable {
				return false
			}
		}

		return true
	}, 20*time.Second, 200*time.Millisecond,
		"handoff claims did not converge to stable after rebalance")
}
