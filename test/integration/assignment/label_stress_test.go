package assignment_test

// Concurrency stress test for the label-assignment feature's two new
// ticker/watcher paths, per the AGENTS.md "monitor goroutine on a ticker"
// rule: aggressive cadence, concurrent production KV traffic, and the race
// detector as the primary oracle. Patterned on
// test/integration/manager/manager_epoch_monitor_concurrency_test.go.
//
// The two paths under stress:
//
//  1. monitorLabelRecheck (internal/assignment/calculator.go) — the leader's
//     label re-check ticker. It owns the guaranteed second observation for
//     deferred label decisions (park confirmation, grace expiry) and shares
//     nats.go's cached *stream state with the assignment publisher and the
//     source watcher on the same buckets. Armed by the batch-pool-down
//     window g3 opens: 300ms after the pool empties, the grace timer expires
//     and requests a "grace_expiry" recheck.
//  2. WorkerMonitor's per-heartbeat-PUT label fingerprint decode
//     (internal/assignment/worker_monitor.go: dropLabelFingerprint /
//     SetOnLabelChange) — decoded via xxh3 on every heartbeat entry the
//     leader observes, sharing the heartbeat bucket's cached *stream state
//     with the heartbeat publisher and the election/heartbeat scan paths.
//     The decode runs continuously (every heartbeat PUT from every worker).
//     g3's relabeled restart (below) does not reach the SetOnLabelChange
//     change-branch itself: the graceful stop it performs deletes the old
//     heartbeat key first, and dropLabelFingerprint forgets the retained
//     fingerprint on that delete, so the rejoin seeds silently as a
//     first-seen join by design (worker_monitor.go: the comment on
//     dropLabelFingerprint) — that branch exists for a stale-ID takeover
//     without an intervening leave, pinned against a live NATS watcher by
//     TestCalculator_TightTakeover_LabelChangeTriggersRebalance.
//     What the relabeled restart does reliably exercise is a live
//     worker-side incarnation-guard reject: the leader's in-flight
//     assignment for the reused worker ID still carries the old ["batch"]
//     labels-of-record, which mismatch the rejoined worker's new
//     ["batch","vip"] configured labels, so the worker rejects it; the next
//     ordinary rebalance then re-derives that worker's labels-of-record from
//     its current heartbeat and converges.
//
// Both paths run continuously on a live 3-worker cluster with real label
// churn, so unit tests exercising either monitor in isolation cannot
// reproduce a race between them and the production goroutines sharing the
// same KV handles — only a live-cluster -race run can.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// soakFailure captures the first hard failure reported by a background soak
// goroutine so the main test goroutine can require.Empty it after the soak
// completes. testify's require.* wraps t.FailNow, which per the testing
// package's documented contract must only be called from the goroutine
// running the test itself — so soak goroutines record here instead of
// asserting directly, and the main goroutine turns the record into a real
// (hard) test failure once every soak goroutine has returned.
type soakFailure struct {
	mu  sync.Mutex
	msg string
}

// record stores msg as the failure reason if none has been recorded yet
// (first-writer-wins — later detail from other goroutines is redundant once
// the soak is already known to be broken).
func (f *soakFailure) record(msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.msg == "" {
		f.msg = msg
	}
}

// get returns the recorded failure reason, or "" if none was recorded.
func (f *soakFailure) get() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.msg
}

// versionSet is a concurrency-safe set of observed commit versions, used as
// part of the liveness proxy (spec: "assignment version strictly increases
// during the soak").
type versionSet struct {
	mu   sync.Mutex
	seen map[int64]struct{}
}

func newVersionSet() *versionSet {
	return &versionSet{seen: make(map[int64]struct{})}
}

func (s *versionSet) record(v int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen[v] = struct{}{}
}

func (s *versionSet) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.seen)
}

// TestLabelMachinery_NoRaceUnderConcurrentTraffic drives a real 3-worker
// cluster (labels [vip], [batch], []) through ~5 seconds of concurrent label
// churn, coverage verification, and a worker bounce, all racing the leader's
// label-recheck ticker and the worker monitor's per-heartbeat label
// fingerprint decode against the same KV buckets those production paths use.
//
//   - g1 (every 150ms): rewrites the 12-partition source via src.Update,
//     rotating each partition's Label across vip/batch/unlabeled. This is a
//     label-only edit — Keys and Weight never change — driven through the
//     source API directly (not an out-of-band raw KV write): this test
//     stresses the monitor/watcher machinery under load, not the watch-path
//     propagation itself (pinned by TestLabelPromotion_EndToEnd and
//     friends).
//
//   - g2 (every 200ms): reads the latest assignment commit, then verifies the
//     coverage invariant assigned ∪ parked == source AGAINST THAT SAME
//     COMMIT via checkCoverage. checkCoverage's payload fetches are
//     content-addressed off commit.Payloads, so once a specific *commit is
//     in hand its payloads can never drift to a different commit's parked
//     set — reading the commit first and deriving everything else from it is
//     the fetch-consistency pattern the rest of this package's tests already
//     rely on (see label_orphan_stale_test.go). The identity set passed to
//     checkCoverage (basePartitions) never changes even though the live
//     source's labels do: Label is documented as excluded from CanonicalID,
//     HashID, and PartitionSetDigest, so a label-agnostic identity snapshot
//     is valid at every tick regardless of which label rotation is live.
//     A transient fetch failure (checkCoverage's cov.assigned == nil — no
//     commit yet, or a referenced payload not yet visible) is treated as
//     "not converged", not a violation: this is expected during the g3
//     worker bounce, when a deliberate restart can leave a payload
//     momentarily unwritten. Only a SUCCESSFULLY fetched commit that fails
//     the invariant (cov.assigned != nil, cov.ok == false) is a hard
//     failure, recorded via soakFailure.
//
//   - g3 (once, at +2s): stops the batch worker, waits 2s, then restarts it
//     under the same worker ID with labels ("batch", "vip") instead of its
//     original ("batch") — exercising pool-empty detection, defer/confirm,
//     park, and re-home while g1's label churn is still in flight. The
//     relabeled restart reliably produces a live worker-side
//     incarnation-guard reject: the leader's in-flight assignment for the
//     reused worker ID still carries the old ["batch"] labels-of-record,
//     which mismatch the new incarnation's ["batch","vip"] configured
//     labels, so the worker rejects the payload; the next ordinary rebalance
//     re-derives the worker's labels-of-record from its current heartbeat
//     and converges. (It does not additionally exercise the
//     SetOnLabelChange change-branch — see the file header comment.) The
//     batch pool stays populated post-restart (the worker still carries
//     "batch"), so full coverage is still expected within the post-soak
//     convergence window.
//
// Liveness proxy: the assignment version must strictly progress across the
// soak (final commit version > the pre-soak version), and g2 must have
// observed a handful of distinct commit versions along the way — not just
// one at the start and one at the end. Label churn debounces internally, so
// this is a soak-wide progress assertion, not a per-tick monotonic-growth
// assertion.
//
// Pass criteria: no race-detector trigger, no coverage violation, and the
// cluster returns to a stable full-coverage state (ParkedCount == 0,
// assigned == source) within 20s after the churn stops.
func TestLabelMachinery_NoRaceUnderConcurrentTraffic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Generous overall budget: g3's restart WaitState alone can take up to
	// 15s, and the post-soak convergence Eventually can take up to 20s on
	// top of the 5s soak, well within this ceiling even under CI load.
	ctx, cancel := context.WithTimeout(t.Context(), 280*time.Second)
	defer cancel()

	const numPartitions = 12
	basePartitions := make([]types.Partition, numPartitions)
	for i := range basePartitions {
		basePartitions[i] = types.Partition{Keys: []string{"ls-p", fmt.Sprintf("%02d", i)}, Weight: 1}
	}

	srcKV, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "label-stress-partitions"})
	require.NoError(t, err)
	src := source.NewNatsKV(srcKV, "partitions", nil)
	require.NoError(t, src.Update(ctx, basePartitions))
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(context.Background()) })

	cfg := testutil.IntegrationTestConfig()
	// Aggressive: park/spill transitions churn constantly, maximising how
	// many times the label-recheck ticker and the fingerprint decode fire
	// per second of soak.
	cfg.LabelSpillGrace = 300 * time.Millisecond

	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("vip"))
	batchWorker := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("batch"))
	cluster.AddWorkerWithOptions(ctx) // unlabeled
	t.Cleanup(cluster.StopWorkers)
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(20 * time.Second)

	assignKV := openAssignmentKV(t, ctx, js, cfg)

	initialCommit := readLatestCommit(ctx, assignKV)
	require.NotNil(t, initialCommit, "cluster must have a committed assignment before the soak begins")
	initialVersion := initialCommit.Version

	var failure soakFailure
	observedVersions := newVersionSet()

	soakCtx, soakCancel := context.WithTimeout(ctx, 5*time.Second)
	defer soakCancel()

	var wg sync.WaitGroup

	// g1: label-only source churn every 150ms, rotating each partition's
	// Label across vip/batch/unlabeled (a rotating subset — the specific
	// partitions in each bucket shift every tick).
	wg.Go(func() {
		runLabelChurn(ctx, soakCtx, src, basePartitions, &failure)
	})

	// g2: every 200ms, read the commit and verify the coverage invariant
	// against that exact commit; track distinct versions for the liveness
	// proxy.
	wg.Go(func() {
		runCoverageWatch(ctx, soakCtx, assignKV, basePartitions, observedVersions, &failure)
	})

	// g3: stop the batch worker mid-soak, then restart it under the same
	// worker ID with an added "vip" label 2s later — exercising pool-empty
	// detection, defer/confirm, park, re-home, and a live incarnation-guard
	// reject while g1's churn is still in flight. Uses the outer ctx (not
	// soakCtx) for its waits so the restart can complete even if it lands
	// close to (or just after) the soak's 5s boundary.
	wg.Go(func() {
		runBatchWorkerBounce(ctx, cluster, batchWorker, &failure)
	})

	<-soakCtx.Done()
	wg.Wait()

	require.Empty(t, failure.get(), "a soak goroutine reported a hard failure")

	// Pass criterion: the cluster returns to a stable full-coverage state
	// (nothing parked, every source partition assigned exactly once) within
	// 20s after the churn stops.
	require.Eventually(t, func() bool {
		commit := readLatestCommit(ctx, assignKV)
		if commit == nil || commit.ParkedCount != 0 {
			return false
		}

		return checkCoverage(ctx, assignKV, commit, basePartitions).ok
	}, 20*time.Second, 200*time.Millisecond,
		"cluster must return to full coverage with nothing parked within 20s after the churn stops")

	// Liveness proxy: the assignment version made real, sustained progress
	// across the soak — not just one initial and one final bump.
	finalCommit := readLatestCommit(ctx, assignKV)
	require.NotNil(t, finalCommit)
	require.Greater(t, finalCommit.Version, initialVersion,
		"assignment version must have advanced across the soak (liveness proxy)")
	require.GreaterOrEqual(t, observedVersions.count(), 3,
		"expected a handful of distinct assignment versions observed during the soak, got %d", observedVersions.count())

	// Confirm the test ran without the race detector failing. See the
	// epoch-monitor template's doc comment for why t.Failed() is the most
	// reliable in-body signal available for a race-detector trigger.
	require.False(t, t.Failed(),
		"race detector or a soak assertion failed during the concurrent run; "+
			"check stderr for WARNING: DATA RACE blocks")
}

// runLabelChurn is g1: every 150ms it rewrites the source via src.Update,
// rotating each partition's Label across vip/batch/unlabeled (a rotating
// subset — the specific partitions in each bucket shift every tick). Keys
// and Weight never change, so every write is a pure label-only edit driven
// through the source API directly, not an out-of-band raw KV write: this
// test stresses the monitor/watcher machinery under load, not the
// watch-path propagation itself (pinned by TestLabelPromotion_EndToEnd and
// friends). ctx bounds the KV write itself; soakCtx bounds the loop's
// lifetime.
func runLabelChurn(ctx, soakCtx context.Context, src *source.NatsKV, base []types.Partition, failure *soakFailure) {
	rotatingLabels := [3]string{"vip", "batch", ""}
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	tick := 0
	for {
		select {
		case <-soakCtx.Done():
			return
		case <-ticker.C:
		}
		tick++
		parts := make([]types.Partition, len(base))
		for i, p := range base {
			p.Label = rotatingLabels[(i+tick)%len(rotatingLabels)]
			parts[i] = p
		}
		if err := src.Update(ctx, parts); err != nil && ctx.Err() == nil {
			failure.record(fmt.Sprintf("g1: source label-only update failed: %v", err))

			return
		}
	}
}

// runCoverageWatch is g2: every 200ms it reads the latest assignment commit,
// then verifies the coverage invariant assigned ∪ parked == source AGAINST
// THAT SAME COMMIT via checkCoverage. checkCoverage's payload fetches are
// content-addressed off commit.Payloads, so once a specific *commit is in
// hand its payloads can never drift to a different commit's parked set —
// reading the commit first and deriving everything else from it is the
// fetch-consistency pattern the rest of this package's tests rely on (see
// label_orphan_stale_test.go). base never changes even though the live
// source's labels do: Label is documented as excluded from CanonicalID,
// HashID, and PartitionSetDigest, so this label-agnostic identity snapshot
// is valid at every tick regardless of which label rotation is live.
//
// A transient fetch failure (checkCoverage's cov.assigned == nil — no
// commit yet, or a referenced payload not yet visible) is treated as "not
// converged", not a violation: this is expected during the g3 worker
// bounce, when a deliberate restart can leave a payload momentarily
// unwritten. Only a SUCCESSFULLY fetched commit that fails the invariant
// (cov.assigned != nil, cov.ok == false) is a hard failure, recorded via
// soakFailure. ctx bounds the KV reads; soakCtx bounds the loop's lifetime.
func runCoverageWatch(
	ctx, soakCtx context.Context,
	assignKV jetstream.KeyValue,
	base []types.Partition,
	observed *versionSet,
	failure *soakFailure,
) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-soakCtx.Done():
			return
		case <-ticker.C:
		}

		commit := readLatestCommit(ctx, assignKV)
		if commit == nil {
			continue // cold-start / transient; not yet converged
		}
		observed.record(commit.Version)

		cov := checkCoverage(ctx, assignKV, commit, base)
		if cov.ok {
			continue
		}
		if cov.assigned == nil {
			// Transient: payload fetch pending (e.g. racing the g3 worker
			// bounce). Only a SUCCESSFULLY fetched commit that fails the
			// invariant is an orphan violation.
			continue
		}
		failure.record(fmt.Sprintf("coverage violation at commit version %d: %s", commit.Version, cov.reason))

		return
	}
}

// runBatchWorkerBounce is g3: it stops the batch worker mid-soak, waits 2s,
// then restarts it under the same worker ID with labels ("batch", "vip")
// instead of its original ("batch") — exercising pool-empty detection,
// defer/confirm, park, and re-home while g1's label churn is still in
// flight. The relabeled restart reliably produces a live worker-side
// incarnation-guard reject: the leader's in-flight assignment for the
// reused worker ID still carries the old ["batch"] labels-of-record, which
// mismatch the new incarnation's ["batch","vip"] configured labels, so the
// worker rejects the payload; the next ordinary rebalance re-derives the
// worker's labels-of-record from its current heartbeat and converges. The
// batch pool stays populated post-restart (the worker still carries
// "batch"). It waits on ctx (not soakCtx) so the restart can complete even
// if it lands close to (or just after) the soak's boundary.
//
// The stop below is the same bounded call this package's stopWorker helper
// makes (5s timeout), inlined so any error can be routed through soakFailure
// instead of require.NoError — testify's require.* must only be invoked from
// the test's own goroutine, and this runs on g3.
func runBatchWorkerBounce(
	ctx context.Context,
	cluster *testutil.WorkerCluster,
	batchWorker *parti.Manager,
	failure *soakFailure,
) {
	select {
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
		return
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := batchWorker.Stop(stopCtx)
	stopCancel()
	if err != nil {
		failure.record(fmt.Sprintf("g3: failed to stop batch worker %s: %v", batchWorker.WorkerID(), err))

		return
	}

	select {
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
		return
	}
	newBatch := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("batch", "vip"))
	if err := newBatch.Start(ctx); err != nil {
		failure.record(fmt.Sprintf("g3: restarted batch worker failed to start: %v", err))

		return
	}
	if err := <-newBatch.WaitState(parti.StateStable, 15*time.Second); err != nil {
		failure.record(fmt.Sprintf("g3: restarted batch worker did not reach StateStable: %v", err))
	}
}
