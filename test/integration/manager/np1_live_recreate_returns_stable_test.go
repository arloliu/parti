package manager_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// np1Transition is a single observed (from,to) state-machine edge, tagged with
// the wall-clock time it was delivered so the assertion can filter to only the
// transitions that happened AFTER the operator recreated the buckets.
type np1Transition struct {
	from types.State
	to   types.State
	at   time.Time
}

// np1Probe is the per-worker flap detector. OnStateChanged appends every edge;
// OnDegraded appends every reason. Hooks fire from background goroutines, so all
// access is mutex-guarded.
type np1Probe struct {
	mu          sync.Mutex
	transitions []np1Transition
	reasons     []string
}

func (p *np1Probe) onStateChanged(_ context.Context, from, to types.State) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.transitions = append(p.transitions, np1Transition{from: from, to: to, at: time.Now()})

	return nil
}

func (p *np1Probe) onDegraded(_ context.Context, reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reasons = append(p.reasons, reason)

	return nil
}

// snapshotTransitions returns a copy of every recorded edge.
func (p *np1Probe) snapshotTransitions() []np1Transition {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]np1Transition, len(p.transitions))
	copy(out, p.transitions)

	return out
}

// snapshotReasons returns a copy of every recorded OnDegraded reason.
func (p *np1Probe) snapshotReasons() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.reasons))
	copy(out, p.reasons)

	return out
}

// np1HasDegradedToStableAfter reports whether the probe recorded a
// Degraded->Stable transition strictly after the given instant. This is the
// hard flap signal: an in-process heal back to Stable after the operator
// recreated the buckets.
func np1HasDegradedToStableAfter(p *np1Probe, after time.Time) bool {
	for _, tr := range p.snapshotTransitions() {
		if tr.from == types.StateDegraded && tr.to == types.StateStable && tr.at.After(after) {
			return true
		}
	}

	return false
}

// np1HasBucketRecreatedReason reports whether the probe ever fired an
// OnDegraded reason beginning with "bucket-recreated:". This is the
// discriminator (logged, not gated): if it fired AND a Degraded->Stable
// edge appears after the recreate, the flap is a recovery-exit flap driven by
// the epoch fence; if it never fired but a heal still occurred, the heal is a
// false-stable from a different path.
func np1HasBucketRecreatedReason(p *np1Probe) bool {
	for _, r := range p.snapshotReasons() {
		if strings.HasPrefix(r, "bucket-recreated:") {
			return true
		}
	}

	return false
}

// TestNP1_LiveBucketRecreate_MustNotReturnToStable probes whether a running
// (NOT restarted) worker self-heals back to Stable after an operator wipes and
// then recreates all four Parti control-plane buckets EMPTY underneath it.
//
// The documented contract (manager_live_bucket_loss_test.go, docs/OPERATIONS.md
// "Live NATS data loss") is: a live data loss drives every worker Degraded, and
// recovery is a process restart / pod rotation — Parti performs NO in-process
// self-heal of lost bucket metadata. The epoch fence (monitorBucketEpochs)
// re-enters Degraded with reason "bucket-recreated:<bucket>" precisely so the
// readiness probe can rotate the pod rather than silently resuming.
//
// This test EMPIRICALLY determines which of two behaviors current main exhibits
// once the buckets exist again but are empty:
//   - clean stays-degraded: no worker ever transitions Degraded->Stable after
//     the recreate (the documented contract holds in-process).
//   - heal-then-redegrade flap: some worker transitions Degraded->Stable (a
//     spurious recovery) before — possibly — the epoch fence drags it back.
//
// HARD INVARIANT (deterministic, via the OnStateChanged edge slices): after the
// recreate instant, NO worker may record a Degraded->Stable transition. If main
// flaps, this FAILS; if main stays cleanly Degraded, this PASSES.
//
// Unlike manager_epoch_fence_test.go this uses a REAL external
// js.DeleteKeyValue + js.CreateKeyValue against a live 3-worker cluster — no
// internal handle-swap.
func TestNP1_LiveBucketRecreate_MustNotReturnToStable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	// Degrade fast on whole-bucket loss: three failed KV publishes trip the
	// threshold. Keep a generous window so transient jitter does not age the
	// errors out before the threshold is reached.
	cfg.DegradedBehavior.KVErrorThreshold = 3
	cfg.DegradedBehavior.KVErrorWindow = 30 * time.Second

	partitions := testutil.CreateTestPartitions(10)
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Window long enough that a heal-then-redegrade flap can manifest if it
	// exists: a couple of recovery ticks, an epoch-fence poll (OperationTimeout),
	// and an election cycle. Size the outer context to comfortably contain
	// startup + all four phases.
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	const clusterSize = 3
	probes := make([]*np1Probe, 0, clusterSize)

	cluster := &testutil.WorkerCluster{
		Workers:  make([]*parti.Manager, 0),
		Config:   cfg,
		Source:   src,
		Strategy: assignStrat,
		NC:       nc,
		JS:       js,
		T:        t,
	}
	for range clusterSize {
		probe := &np1Probe{}
		probes = append(probes, probe)
		hooks := &parti.Hooks{
			OnStateChanged: probe.onStateChanged,
			OnDegraded:     probe.onDegraded,
		}
		cluster.AddWorkerWithOptions(ctx, parti.WithHooks(hooks))
	}
	defer cluster.StopWorkers()

	// Phase 1: bring the cluster up and let it stabilize.
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(15 * time.Second)
	leader := cluster.VerifyExactlyOneLeader()
	t.Logf("phase 1: stable, leader=%s", leader.WorkerID())

	preWipeVersions := make(map[string]int64)
	for _, mgr := range cluster.GetActiveWorkers() {
		preWipeVersions[mgr.WorkerID()] = mgr.CurrentAssignment().Version
	}
	t.Logf("phase 1: pre-wipe assignment versions: %v", preWipeVersions)

	// Phase 2: wipe all four Parti-managed buckets while the workers run.
	buckets := np1PartiBuckets(cfg)
	np1WipeBuckets(t, ctx, js, buckets)
	wipedAt := time.Now()
	t.Logf("phase 2: wiped %d buckets at %s", len(buckets), wipedAt.Format(time.RFC3339Nano))

	// NON-VACUITY (part 1): every worker must genuinely reach Degraded.
	const degradedWithin = 25 * time.Second
	require.Eventually(t, func() bool {
		return np1CountDegraded(cluster) == clusterSize
	}, degradedWithin, 500*time.Millisecond,
		"all %d workers must enter Degraded within %s of the live wipe", clusterSize, degradedWithin)
	t.Logf("phase 2: all %d workers Degraded within %s", clusterSize,
		time.Since(wipedAt).Round(100*time.Millisecond))

	// Phase 3: with the workers STILL running, the operator recreates all four
	// buckets EMPTY using the exact per-bucket TTL/storage the manager startup
	// uses (see np1RecreateBuckets), then confirms they exist.
	np1RecreateBuckets(t, ctx, js, cfg)
	recreatedAt := time.Now()
	t.Logf("phase 3: operator recreated 4 empty buckets at %s", recreatedAt.Format(time.RFC3339Nano))

	// Phase 4: observe a window long enough for a heal-then-redegrade flap to
	// manifest if it exists — >= 3*OperationTimeout (epoch-fence ticks) plus a
	// couple of recovery ticks and an election cycle.
	observeWindow := 3*cfg.OperationTimeout +
		2*cfg.DegradedBehavior.RecoveryGracePeriod +
		cfg.ElectionTimeout
	if observeWindow < 35*time.Second {
		observeWindow = 35 * time.Second
	}
	t.Logf("phase 4: observing for %s after recreate for any Degraded->Stable flap", observeWindow)

	// Let the window elapse via a context-bounded timer so the test does not
	// outrun its outer deadline.
	timer := time.NewTimer(observeWindow)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatalf("outer context expired before the %s observation window completed: %v", observeWindow, ctx.Err())
	}

	// HARD INVARIANT: after recreatedAt, NO worker may record a Degraded->Stable
	// transition. The documented contract is degraded + restart/rotation — no
	// in-process heal. np1HealedAfter logs the discriminator + classification.
	healed := np1HealedAfter(t, probes, recreatedAt)
	np1LogFinalStates(t, cluster, probes)

	require.Empty(t, healed,
		"contract violation: workers %v recorded a Degraded->Stable transition after the operator "+
			"recreated the empty buckets at %s; the documented contract is degraded + restart/rotation "+
			"with NO in-process heal (see manager_live_bucket_loss_test.go / docs/OPERATIONS.md)",
		healed, recreatedAt.Format(time.RFC3339Nano))
}

// np1FormatTransitions renders a transition slice compactly for diagnostics.
func np1FormatTransitions(trs []np1Transition) []string {
	out := make([]string, 0, len(trs))
	for _, tr := range trs {
		out = append(out, tr.from.String()+"->"+tr.to.String())
	}

	return out
}

// np1PartiBuckets returns the four Parti-owned control-plane bucket names.
func np1PartiBuckets(cfg parti.Config) []string {
	return []string{
		cfg.KVBuckets.StableIDBucket,
		cfg.KVBuckets.ElectionBucket,
		cfg.KVBuckets.HeartbeatBucket,
		cfg.KVBuckets.AssignmentBucket,
	}
}

// np1WipeBuckets deletes the named buckets while the workers run, then confirms
// each is gone (guards against a silent delete no-op).
func np1WipeBuckets(t *testing.T, ctx context.Context, js jetstream.JetStream, buckets []string) {
	t.Helper()
	for _, b := range buckets {
		if err := js.DeleteKeyValue(ctx, b); err != nil &&
			!errors.Is(err, jetstream.ErrBucketNotFound) {
			t.Fatalf("failed to wipe bucket %s: %v", b, err)
		}
	}
	for _, b := range buckets {
		_, err := js.KeyValue(ctx, b)
		require.ErrorIsf(t, err, jetstream.ErrBucketNotFound, "bucket %s must be gone after wipe", b)
	}
}

// np1RecreateBuckets recreates the four Parti buckets EMPTY with the exact
// per-bucket TTL/storage the manager startup uses (parti.BuildControlPlaneKVConfig
// is byte-equivalent to ensureKVBucket; mirrors manager_setup.go), then confirms
// each exists again.
func np1RecreateBuckets(t *testing.T, ctx context.Context, js jetstream.JetStream, cfg parti.Config) {
	t.Helper()
	recreate := []struct {
		bucket  string
		ttl     time.Duration
		storage jetstream.StorageType
	}{
		{cfg.KVBuckets.StableIDBucket, cfg.WorkerIDTTL, jetstream.FileStorage},
		{cfg.KVBuckets.ElectionBucket, cfg.ElectionTimeout, jetstream.FileStorage},
		{cfg.KVBuckets.HeartbeatBucket, cfg.HeartbeatTTL, jetstream.MemoryStorage},
		{cfg.KVBuckets.AssignmentBucket, cfg.KVBuckets.AssignmentTTL, jetstream.FileStorage},
	}
	for _, r := range recreate {
		_, err := js.CreateKeyValue(ctx, parti.BuildControlPlaneKVConfig(r.bucket, r.ttl, r.storage))
		require.NoErrorf(t, err, "operator recreate of bucket %s must succeed", r.bucket)
	}
	for _, r := range recreate {
		_, err := js.KeyValue(ctx, r.bucket)
		require.NoErrorf(t, err, "bucket %s must exist after operator recreate", r.bucket)
	}
}

// np1CountDegraded returns how many active (non-Shutdown) workers are Degraded.
func np1CountDegraded(cluster *testutil.WorkerCluster) int {
	degraded := 0
	for _, mgr := range cluster.GetActiveWorkers() {
		if mgr.State() == types.StateDegraded {
			degraded++
		}
	}

	return degraded
}

// np1HealedAfter returns the indices of workers that recorded a Degraded->Stable
// transition after `after` (the hard flap signal), and logs the bucket-recreated
// discriminator plus a flap/false-stable/clean classification.
func np1HealedAfter(t *testing.T, probes []*np1Probe, after time.Time) []int {
	t.Helper()
	healed := make([]int, 0, len(probes))
	for i, p := range probes {
		if np1HasDegradedToStableAfter(p, after) {
			healed = append(healed, i)
		}
	}

	bucketRecreatedFired := false
	for i, p := range probes {
		if np1HasBucketRecreatedReason(p) {
			bucketRecreatedFired = true
			t.Logf("discriminator: worker %d fired an OnDegraded \"bucket-recreated:\" reason; reasons=%v",
				i, p.snapshotReasons())
		}
	}

	switch {
	case len(healed) > 0 && bucketRecreatedFired:
		t.Logf("classification: recovery-exit flap — workers %v healed to Stable after recreate AND "+
			"epoch fence fired bucket-recreated (heal then epoch-fence re-degrade)", healed)
	case len(healed) > 0:
		t.Logf("classification: false-stable — workers %v healed to Stable after recreate but the epoch "+
			"fence never fired bucket-recreated", healed)
	default:
		t.Logf("classification: clean stays-degraded — no worker returned to Stable after recreate "+
			"(bucket-recreated fired=%t)", bucketRecreatedFired)
	}

	return healed
}

// np1LogFinalStates logs each worker's final state and full transition/reason list.
func np1LogFinalStates(t *testing.T, cluster *testutil.WorkerCluster, probes []*np1Probe) {
	t.Helper()
	for i, mgr := range cluster.GetActiveWorkers() {
		t.Logf("phase 4: worker %d (%s) final state=%s", i, mgr.WorkerID(), mgr.State().String())
	}
	for i, p := range probes {
		t.Logf("phase 4: worker %d transitions=%v reasons=%v",
			i, np1FormatTransitions(p.snapshotTransitions()), p.snapshotReasons())
	}
}
