package manager_test

import (
	"cmp"
	"context"
	"encoding/json"
	"math"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	parti "github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// recordingBurstCollector embeds NopMetrics and timestamps every
// RecordApplyAttempt call so the diagnostic can group them into bursts.
type recordingBurstCollector struct {
	*metrics.NopMetrics
	mu    sync.Mutex
	calls []burstSample
}

type burstSample struct {
	at       time.Time
	workerID string
	version  int64
}

func (r *recordingBurstCollector) RecordApplyAttempt(workerID string, version int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, burstSample{time.Now(), workerID, version})
}

// TestApplyCoalescing_UnderReElectionBurst measures apply-attempt burst
// behavior across a worker fleet during a Parti calculator-leader re-election.
// It produces TWO complementary views from the same RecordApplyAttempt samples:
//
// VIEW 1 — per-worker temporal burst (existing):
// Groups each worker's own samples by time proximity (idleGap=50ms) and reports
// max_burst_size and max_burst_duration per worker plus a fleet AGGREGATE banner.
// Recommended metric: Config.AssignmentWatcherDebounce.
// Limitation: partitions samples by worker, so it cannot see simultaneous
// applies across the fleet; under the tested trigger it correctly shows
// max_burst_size=1 because each worker applies each version at most once.
//
// VIEW 2 — fleet-wide spatial burst (new):
// Merges all worker sample timelines into one sorted list and measures:
//   - Per-version fanout: how many distinct workers applied version V, and
//     over what wall-clock span (first worker's RecordApplyAttempt call to
//     last worker's call for that version). FLEET lines are logged for each
//     version with WorkerCount > 1.
//   - Peak concurrency: the max number of distinct workers with any sample
//     in a sliding 100 ms window ([t-W, t+W], W=idleGap=50ms). This metric
//     is intentionally version-agnostic: cross-version overlap is real apply
//     pipeline pressure (each sample takes applyStoreMu) and should count.
//
// Recommended metric: Config.ApplyStartJitter.
// Output: AGGREGATE_FLEET banner per phase plus an overall banner with
// peak_concurrency, worst_version_span, and recommended_apply_jitter.
//
// WHY BOTH VIEWS:
// The per-worker view answers "does any single worker see a burst of distinct
// versions?" (temporal concentration). The fleet view answers "do many workers
// apply the same version simultaneously?" (spatial simultaneity). These are
// orthogonal failure modes with different knob mitigations. The landed scenario
// (Parti-leader kill on R=3, 20 workers) shows max_burst_size=1 per worker
// (temporal bursts well-paced by version gate + RebalanceCooldown) but
// peak_concurrency ≈ numWorkers-1 (spatial bursts exist and motivate jitter).
//
// PHASE SEQUENCE:
//
//	P0 cold_start   — all workers start simultaneously (observation-only, not gated)
//	P1 leader_kill  — Parti calculator leader is stopped; surviving workers apply V=N+1
//	P2 worker_add   — two workers added; fleet rebalances to a new version
//
// Phase boundaries are version-publication signals (WaitForAssignmentVersion),
// not elapsed-time guards, to avoid CI-load flakiness.
//
// KNOWN LIMITATIONS (do not mistake for bugs):
//  1. Retry applies are included in the metric by design. RecordApplyAttempt is
//     called from applyAssignmentWithPrevCore (manager_assignment.go:1020),
//     which is the common entry point for BOTH fresh applies (with jitter) and
//     retries (applyAssignmentWithPrevSkipJitter, no jitter). The MetricsCollector
//     API has no attempt-number argument, so the test cannot filter retries from
//     the sample set. peak_concurrency therefore measures total apply pipeline
//     pressure — the operationally-meaningful quantity, but one that includes a
//     component ApplyStartJitter cannot smooth. Operators should treat a high
//     peak_concurrency as necessary but not sufficient evidence for tuning
//     ApplyStartJitter in isolation.
//  2. In-process clock skew (1–10 ms under CI load) is inside the idleGap
//     window. Negligible for the metric but documented for posterity.
//  3. The diagnostic measures one cluster topology (3-node R=3, 20 workers).
//     Real production clusters may produce different numbers. Operators are
//     encouraged to re-run the diagnostic against their own topology.
//
// CONSTRAINTS ON THE MEASUREMENT RUN:
//
//	Config.ApplyStartJitter = 0      — targeting this knob; non-zero would mute the signal.
//	Config.AssignmentWatcherDebounce = 0 — default, but set explicitly for clarity.
//	Config.KVBuckets.Replicas = 3    — realistic JetStream replication semantics.
//
// Opt-in: set PARTI_RUN_HERD_DIAGNOSTIC=1 to run.
//
//nolint:cyclop,gocyclo
func TestApplyCoalescing_UnderReElectionBurst(t *testing.T) {
	if os.Getenv("PARTI_RUN_HERD_DIAGNOSTIC") != "1" {
		t.Skip("set PARTI_RUN_HERD_DIAGNOSTIC=1 to run")
	}

	const (
		numWorkers      = 20
		numExtraWorkers = 2 // added during P2 worker_add phase
		numPartitions   = 100
		idleGap         = 50 * time.Millisecond
		// preKillSettle lets watcher pipelines quiesce before the kill so the
		// measurement reflects re-election burst, not startup noise.
		preKillSettle = 5 * time.Second
	)

	ctx, cancel := context.WithTimeout(t.Context(), 240*time.Second)
	defer cancel()

	// Start 3-node embedded NATS cluster with JetStream.
	nc, servers, cleanup := testutil.StartEmbeddedNATSCluster(t)
	defer cleanup()

	t.Logf("cluster: %d nodes started", len(servers))

	// Build config with R=3 so KV buckets replicate across all 3 nodes.
	// R=3 ensures the KV buckets survive the Parti-leader stop and lets the
	// new leader publish to a healthy stream — the same failure envelope
	// production sees when a calculator worker restarts.
	//
	// Use reduced heartbeat/election timeouts so the Parti leader re-election
	// completes well within the 30s WaitForNewLeader timeout:
	//   HeartbeatTTL=2s + ElectionTimeout=1s → failover in ~3s
	// This is safe here because we're measuring the burst, not correctness.
	//
	// ApplyStartJitter=0: this diagnostic targets that knob; running with it
	// non-zero would mute the spatial-burst signal we are trying to measure.
	// AssignmentWatcherDebounce=0: its default, but set explicitly to document
	// the same measurement discipline as the per-worker view.
	cfg := testutil.IntegrationTestConfig()
	cfg.KVBuckets.Replicas = 3
	cfg.HeartbeatTTL = 2 * time.Second
	cfg.ElectionTimeout = 1 * time.Second
	cfg.ApplyStartJitter = 0          // must be 0 to measure unmuffled spatial-burst signal
	cfg.AssignmentWatcherDebounce = 0 // default, explicit for measurement discipline
	t.Logf("config: KVBuckets.Replicas=%d HeartbeatTTL=%s ElectionTimeout=%s ApplyStartJitter=%s AssignmentWatcherDebounce=%s",
		cfg.KVBuckets.Replicas, cfg.HeartbeatTTL, cfg.ElectionTimeout, cfg.ApplyStartJitter, cfg.AssignmentWatcherDebounce)

	// Build per-worker recording collectors, keyed by slot index.
	collectors := make([]*recordingBurstCollector, numWorkers)
	for i := range collectors {
		collectors[i] = &recordingBurstCollector{NopMetrics: metrics.NewNop()}
	}

	// Start numWorkers steady-state workers, each with its own recording
	// collector wired via WithMetrics.
	wc := testutil.NewWorkerClusterWithSource(t, nc, source.NewStatic(testutil.CreateTestPartitions(numPartitions)), cfg)
	mgrs := make([]*parti.Manager, numWorkers)
	for i := range collectors {
		mgrs[i] = wc.AddWorkerWithOptions(ctx, parti.WithMetrics(collectors[i]))
	}
	defer wc.StopWorkers()

	// ── P0 cold_start ───────────────────────────────────────────────────────
	// P0 is observation-only: all workers applying V=1 simultaneously during
	// cold start is expected — not a "thundering herd" in the operational
	// sense. P0's peak_concurrency is logged but NOT asserted against.
	p0Start := time.Now()

	// StartWorkers starts all workers and blocks until every one reaches
	// StateStable (15 s per worker, enforced inside StartWorkers).
	wc.StartWorkers(ctx)
	t.Logf("all %d workers reached StateStable", numWorkers)

	// Let watcher pipelines quiesce before the kill so the burst measurement
	// is not contaminated by startup-time publish noise.
	t.Logf("settling for %s before parti-leader kill...", preKillSettle)
	select {
	case <-time.After(preKillSettle):
	case <-ctx.Done():
		t.Fatalf("context cancelled during pre-kill settle: %v", ctx.Err())
	}

	p0End := time.Now()

	// ── P1 leader_kill ───────────────────────────────────────────────────────
	// Identify the current Parti calculator leader.
	leader := wc.WaitForLeader(15 * time.Second)
	oldLeaderID := leader.WorkerID()

	// Find its index in the local mgrs slice.
	leaderIdx := -1
	for i, m := range mgrs {
		if m.WorkerID() == oldLeaderID {
			leaderIdx = i
			break
		}
	}
	require.GreaterOrEqual(t, leaderIdx, 0, "leader not in mgrs slice")

	survivorIdx := (leaderIdx + 1) % numWorkers

	// IMPORTANT: capture prevVersion and p1Start BEFORE the triggering action.
	// If captured after Stop() returns, the replacement leader may have already
	// published during drain, making prevVersion the new version and causing
	// WaitForAssignmentVersion to wait for a second publish that isn't the
	// burst under test.
	p1PrevVersion := mgrs[survivorIdx].CurrentAssignment().Version
	p1Start := time.Now()

	t.Logf("pre-kill: worker=%s assignment_version=%d", mgrs[survivorIdx].WorkerID(), p1PrevVersion)
	t.Logf("killing parti calculator leader: index=%d worker=%s", leaderIdx, oldLeaderID)

	// Stop the leader using the test context (240 s total budget) so the
	// goroutine drain is not cut short. StopWorkers() will skip it because
	// the manager transitions to StateShutdown before StopWorkers runs,
	// preventing a double-stop. The local mgrs slice still holds the
	// (now-stopped) Manager at mgrs[leaderIdx]; its pre-kill collector
	// samples remain valid for burst analysis.
	leaderMgr := mgrs[leaderIdx]
	stopCtx, stopCancel := context.WithTimeout(ctx, 30*time.Second)
	if err := leaderMgr.Stop(stopCtx); err != nil {
		t.Logf("leader stop returned: %v (non-fatal; proceeding)", err)
	}
	stopCancel()

	// Wait for a new Parti leader to emerge on a surviving worker.
	newLeader := wc.WaitForNewLeader(oldLeaderID, 30*time.Second)
	t.Logf("new parti calculator leader: %s", newLeader.WorkerID())

	// Wait for the version gate to confirm a new version was published.
	wc.WaitForAssignmentVersion(p1PrevVersion+1, 30*time.Second)

	// Soak to capture the apply-attempt burst as the new leader's published
	// Version=N+1 propagates through every surviving worker's watcher.
	const postKillSoak = 10 * time.Second
	t.Logf("soaking %s to capture burst tail", postKillSoak)
	select {
	case <-time.After(postKillSoak):
	case <-ctx.Done():
		t.Fatalf("context cancelled during soak: %v", ctx.Err())
	}

	p1End := time.Now()

	// Log post-P1 assignment version from the same survivor to confirm
	// the new leader published Version=p1PrevVersion+N (confirms the
	// trigger fired; if equal, the election did not complete).
	postP1Version := mgrs[survivorIdx].CurrentAssignment().Version
	t.Logf("post-P1 soak: worker=%s assignment_version=%d (delta=%d)",
		mgrs[survivorIdx].WorkerID(), postP1Version, postP1Version-p1PrevVersion)

	// ── P2 worker_add ────────────────────────────────────────────────────────
	// Add 2 workers to trigger a fleet rebalance. The new workers cause the
	// calculator to publish a new version that all workers (including the
	// new ones) apply simultaneously.
	//
	// IMPORTANT: capture p2PrevVersion and p2Start BEFORE the first Start()
	// call for the same reason as P1.
	p2PrevVersion := mgrs[survivorIdx].CurrentAssignment().Version
	p2Start := time.Now()

	// Allocate collectors for the two new workers.
	extraCollectors := make([]*recordingBurstCollector, numExtraWorkers)
	for i := range extraCollectors {
		extraCollectors[i] = &recordingBurstCollector{NopMetrics: metrics.NewNop()}
	}

	// Add and start the two extra workers; wc tracks them and StopWorkers covers cleanup.
	extraMgrs := make([]*parti.Manager, numExtraWorkers)
	for i := range extraMgrs {
		extraMgrs[i] = wc.AddWorkerWithOptions(ctx, parti.WithMetrics(extraCollectors[i]))
		require.NoError(t, extraMgrs[i].Start(ctx), "extra worker %d failed to start", i)
	}
	t.Logf("added %d extra workers; waiting for fleet rebalance to v>%d", numExtraWorkers, p2PrevVersion)

	// Wait for the fleet to converge on the new version triggered by the added workers.
	wc.WaitForAssignmentVersion(p2PrevVersion+1, 30*time.Second)

	// Soak to capture the rebalance burst tail.
	const postAddSoak = 5 * time.Second
	t.Logf("soaking %s to capture P2 burst tail", postAddSoak)
	select {
	case <-time.After(postAddSoak):
	case <-ctx.Done():
		t.Fatalf("context cancelled during P2 soak: %v", ctx.Err())
	}

	p2End := time.Now()

	postP2Version := mgrs[survivorIdx].CurrentAssignment().Version
	t.Logf("post-P2 soak: worker=%s assignment_version=%d (delta=%d)",
		mgrs[survivorIdx].WorkerID(), postP2Version, postP2Version-p2PrevVersion)

	// ── Per-worker analysis (View 1 — unchanged) ────────────────────────────
	// Merge original 20-worker collectors with the 2 extra workers added in P2.
	totalWorkers := numWorkers + numExtraWorkers
	allCollectors := make([]*recordingBurstCollector, totalWorkers)
	copy(allCollectors, collectors)
	copy(allCollectors[numWorkers:], extraCollectors)

	workerIDs := make([]string, totalWorkers)
	for i, m := range mgrs {
		workerIDs[i] = m.WorkerID()
	}
	for i, m := range extraMgrs {
		workerIDs[numWorkers+i] = m.WorkerID()
	}

	results := analyzeBursts(allCollectors, workerIDs, idleGap)

	for _, wid := range workerIDs {
		r := results[wid]
		t.Logf(
			"worker=%s max_burst_size=%d max_burst_duration=%s p95_inter_arrival=%s total_attempts=%d",
			wid, r.MaxBurstSize, r.MaxBurstDuration.Round(time.Millisecond), r.P95InterArrival.Round(time.Millisecond), r.TotalAttempts,
		)
	}

	t.Logf(
		"AGGREGATE max_burst_size=%d max_burst_duration=%s recommended_debounce_window=%s",
		aggregateMaxBurstSize(results),
		aggregateMaxBurstDuration(results).Round(time.Millisecond),
		recommendedWindow(results),
	)

	// ── Fleet analysis (View 2 — new) ───────────────────────────────────────
	phases := []phaseBound{
		{name: "cold_start", start: p0Start, end: p0End},
		{name: "leader_kill", start: p1Start, end: p1End},
		{name: "worker_add", start: p2Start, end: p2End},
	}

	fleetReports := analyzeFleetBursts(allCollectors, idleGap, phases)

	// Log per-version fanout lines and AGGREGATE_FLEET per phase.
	for _, ph := range phases {
		r := fleetReports[ph.name]
		// Log per-version FLEET lines (only versions with WorkerCount > 1).
		versions := make([]int64, 0, len(r.PerVersion))
		for v := range r.PerVersion {
			versions = append(versions, v)
		}
		slices.SortFunc(versions, cmp.Compare)
		for _, v := range versions {
			fv := r.PerVersion[v]
			if fv.WorkerCount > 1 {
				t.Logf("FLEET (phase=%s) version=%d worker_count=%d span=%s",
					ph.name, v, fv.WorkerCount, fv.Span.Round(time.Millisecond))
			}
		}
		t.Logf("AGGREGATE_FLEET (phase=%s) peak_concurrency=%d worst_version_span=%s versions_observed=%d",
			ph.name,
			r.PeakConcurrency,
			r.WorstVersionSpan.Round(time.Millisecond),
			r.MultiWorkerVersions,
		)
	}

	overall := fleetReports["overall"]
	// Log per-version FLEET lines for the overall report.
	allVersions := make([]int64, 0, len(overall.PerVersion))
	for v := range overall.PerVersion {
		allVersions = append(allVersions, v)
	}
	slices.SortFunc(allVersions, cmp.Compare)
	for _, v := range allVersions {
		fv := overall.PerVersion[v]
		if fv.WorkerCount > 1 {
			t.Logf("FLEET (overall) version=%d worker_count=%d span=%s",
				v, fv.WorkerCount, fv.Span.Round(time.Millisecond))
		}
	}

	jitter := recommendedApplyJitter(overall)
	// If no spatial burst was observed, log the explanatory message before the banner.
	if overall.PeakConcurrency <= 1 {
		t.Log("no spatial burst observed — recommendation is formula floor only")
	}
	// Warn if the pre-cap formula would exceed 1 s (non-jitter root cause).
	const step = 50 * time.Millisecond
	ws := overall.WorstVersionSpan
	preCapNs := ((int64(ws)+int64(step)-1)/int64(step))*int64(step) + int64(100*time.Millisecond)
	if preCapNs > int64(time.Second) {
		t.Logf("WARNING: pre-cap formula value %s exceeds 1s cap; worst_version_span=%s suggests a non-jitter root cause — investigate apply pipeline latency",
			time.Duration(preCapNs).Round(time.Millisecond),
			ws.Round(time.Millisecond))
	}

	t.Logf("AGGREGATE_FLEET (overall) peak_concurrency=%d worst_version_span=%s recommended_apply_jitter=%s",
		overall.PeakConcurrency,
		overall.WorstVersionSpan.Round(time.Millisecond),
		jitter,
	)

	// ── Assertions ───────────────────────────────────────────────────────────
	// P0 is observation-only; no assertions.

	// P1 leader_kill assertions.
	p1Report := fleetReports["leader_kill"]
	expectedP1Workers := numWorkers - 1 // killed leader is no longer applying
	require.GreaterOrEqual(t,
		p1Report.PeakConcurrency, expectedP1Workers-2,
		"P1 peak_concurrency too low: most surviving workers should have fired RecordApplyAttempt within the same 100ms window")
	p1MultiWorkerVersions := 0
	for _, fv := range p1Report.PerVersion {
		if fv.WorkerCount > 1 {
			p1MultiWorkerVersions++
		}
	}
	require.GreaterOrEqual(t, p1MultiWorkerVersions, 1,
		"P1: expected at least one multi-worker version fanout (View A)")

	// P2 worker_add assertions.
	p2Report := fleetReports["worker_add"]
	expectedP2Workers := expectedP1Workers + numExtraWorkers // killed leader stays dead
	require.GreaterOrEqual(t,
		p2Report.PeakConcurrency, expectedP2Workers-2,
		"P2 peak_concurrency too low: most fleet workers should have fired RecordApplyAttempt within the same 100ms window")
	p2MultiWorkerVersions := 0
	for _, fv := range p2Report.PerVersion {
		if fv.WorkerCount > 1 {
			p2MultiWorkerVersions++
		}
	}
	require.GreaterOrEqual(t, p2MultiWorkerVersions, 1,
		"P2: expected at least one multi-worker version fanout (View A)")

	// Overall: recommended_apply_jitter must be a positive finite duration.
	require.Greater(t, jitter, time.Duration(0), "recommended_apply_jitter must be > 0")
}

// runRapidCommitBurstDiagnostic is the shared body for rapid-commit burst
// diagnostics. It starts a small fleet, waits for stability, injects a burst
// of rapid assignment._commit updates, and returns per-worker burst statistics
// and a fleet-wide burst report. debounce sets AssignmentWatcherDebounce so
// callers can compare the no-debounce (debounce=0) and debounced paths.
func runRapidCommitBurstDiagnostic(t *testing.T, debounce time.Duration) (map[string]burstReport, fleetReport) {
	t.Helper()

	const (
		numWorkers    = 5
		numPartitions = 25
		numCommits    = 4
		idleGap       = 50 * time.Millisecond
	)

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.FastTestConfig()
	cfg.ApplyStartJitter = 0
	cfg.AssignmentWatcherDebounce = debounce
	t.Logf("rapid commit burst diagnostic debounce=%s", debounce)

	collectors := make([]*recordingBurstCollector, numWorkers)
	for i := range collectors {
		collectors[i] = &recordingBurstCollector{NopMetrics: metrics.NewNop()}
	}

	wc := testutil.NewWorkerClusterWithSource(t, nc, source.NewStatic(testutil.CreateTestPartitions(numPartitions)), cfg)
	mgrs := make([]*parti.Manager, numWorkers)
	for i := range collectors {
		mgrs[i] = wc.AddWorkerWithOptions(ctx, parti.WithMetrics(collectors[i]))
	}
	defer wc.StopWorkers()

	wc.StartWorkers(ctx)
	t.Logf("all %d workers reached StateStable", numWorkers)

	workerIDs := make([]string, numWorkers)
	for i, m := range mgrs {
		workerIDs[i] = m.WorkerID()
	}

	baseCommit, _ := readAssignmentCommit(ctx, t, wc)
	wc.WaitForAssignmentVersion(baseCommit.Version, 20*time.Second)

	resetBurstCollectors(collectors)
	phaseStart := time.Now()
	publishedVersions := publishRapidCommitBurst(ctx, t, wc, mgrs, numCommits)
	wc.WaitForAssignmentVersion(publishedVersions[len(publishedVersions)-1], 20*time.Second)
	phaseEnd := time.Now()

	results := analyzeBursts(collectors, workerIDs, idleGap)
	for _, wid := range workerIDs {
		r := results[wid]
		t.Logf(
			"COMMIT_BURST worker=%s max_burst_size=%d max_burst_duration=%s p95_inter_arrival=%s total_attempts=%d",
			wid, r.MaxBurstSize, r.MaxBurstDuration.Round(time.Millisecond), r.P95InterArrival.Round(time.Millisecond), r.TotalAttempts,
		)
	}
	t.Logf("COMMIT_BURST_AGGREGATE max_burst_size=%d max_burst_duration=%s recommended_debounce_window=%s versions=%v",
		aggregateMaxBurstSize(results),
		aggregateMaxBurstDuration(results).Round(time.Millisecond),
		recommendedWindow(results),
		publishedVersions,
	)

	fleetReports := analyzeFleetBursts(collectors, idleGap, []phaseBound{{name: "commit_burst", start: phaseStart, end: phaseEnd}})
	commitBurst := fleetReports["commit_burst"]
	t.Logf("COMMIT_BURST_FLEET peak_concurrency=%d worst_version_span=%s versions_observed=%d",
		commitBurst.PeakConcurrency,
		commitBurst.WorstVersionSpan.Round(time.Millisecond),
		commitBurst.MultiWorkerVersions,
	)

	return results, commitBurst
}

// TestApplyCoalescing_UnderRapidCommitBurst measures the commit watcher path
// directly. It injects a short sequence of valid assignment._commit updates
// after the fleet is stable and verifies that each worker can enter the apply
// pipeline for several distinct versions inside one debounce-sized window.
//
// This exercises the commit watcher path under a rapid commit burst with
// AssignmentWatcherDebounce=0 — the "before" half of the before/after
// proof. The "_WithAssignmentDebounce" counterpart proves the "after".
func TestApplyCoalescing_UnderRapidCommitBurst(t *testing.T) {
	if os.Getenv("PARTI_RUN_HERD_DIAGNOSTIC") != "1" {
		t.Skip("set PARTI_RUN_HERD_DIAGNOSTIC=1 to run")
	}

	results, commitBurst := runRapidCommitBurstDiagnostic(t, 0)

	require.GreaterOrEqual(t, aggregateMaxBurstSize(results), 2,
		"commit watcher path should expose a per-worker multi-version burst without commit debounce")
	require.GreaterOrEqual(t, commitBurst.MultiWorkerVersions, 2,
		"commit watcher path should expose multiple multi-worker commit-version fanouts")
}

// TestApplyCoalescing_UnderRapidCommitBurst_WithAssignmentDebounce proves that
// setting AssignmentWatcherDebounce=100ms collapses the per-worker multi-version
// burst: only the final commit version surfaces as a multi-worker fanout.
func TestApplyCoalescing_UnderRapidCommitBurst_WithAssignmentDebounce(t *testing.T) {
	if os.Getenv("PARTI_RUN_HERD_DIAGNOSTIC") != "1" {
		t.Skip("set PARTI_RUN_HERD_DIAGNOSTIC=1 to run")
	}

	results, commitBurst := runRapidCommitBurstDiagnostic(t, 100*time.Millisecond)

	require.LessOrEqual(t, aggregateMaxBurstSize(results), 1,
		"commit debounce should collapse the per-worker multi-version burst")
	require.LessOrEqual(t, commitBurst.MultiWorkerVersions, 1,
		"commit debounce should leave only the final commit version as a multi-worker fanout")
}

func resetBurstCollectors(collectors []*recordingBurstCollector) {
	for _, c := range collectors {
		c.mu.Lock()
		c.calls = nil
		c.mu.Unlock()
	}
}

func publishRapidCommitBurst(
	ctx context.Context,
	t *testing.T,
	wc *testutil.WorkerCluster,
	mgrs []*parti.Manager,
	count int,
) []int64 {
	t.Helper()

	base, prevRev := readAssignmentCommit(ctx, t, wc)

	workerIDs := make([]string, 0, len(mgrs))
	for _, m := range mgrs {
		workerIDs = append(workerIDs, m.WorkerID())
	}
	slices.Sort(workerIDs)
	for _, workerID := range workerIDs {
		_, ok := base.Payloads[workerID]
		require.True(t, ok, "baseline commit missing payload for worker %s", workerID)
	}

	assignmentKV, err := wc.JS.KeyValue(ctx, wc.Config.KVBuckets.AssignmentBucket)
	require.NoError(t, err, "assignment KV must exist after workers start")

	versions := make([]int64, 0, count)
	for i := range count {
		next := base
		next.Version = base.Version + int64(i) + 1
		next.PublishedAt = time.Now().UTC()
		next.Workers = workerIDs
		next.PrevCommitRev = prevRev
		next.Lifecycle = "diagnostic_commit_burst"

		encoded, err := json.Marshal(next)
		require.NoError(t, err, "marshal diagnostic commit version %d", next.Version)

		newRev, err := assignmentKV.Update(ctx, "assignment._commit", encoded, prevRev)
		require.NoError(t, err, "publish diagnostic commit version %d", next.Version)

		prevRev = newRev
		versions = append(versions, next.Version)
	}

	t.Logf("published rapid diagnostic commit versions=%v", versions)

	return versions
}

func readAssignmentCommit(
	ctx context.Context,
	t *testing.T,
	wc *testutil.WorkerCluster,
) (types.AssignmentCommit, uint64) {
	t.Helper()

	assignmentKV, err := wc.JS.KeyValue(ctx, wc.Config.KVBuckets.AssignmentBucket)
	require.NoError(t, err, "assignment KV must exist after workers start")

	entry, err := assignmentKV.Get(ctx, "assignment._commit")
	require.NoError(t, err, "baseline assignment._commit must exist after workers are stable")

	var commit types.AssignmentCommit
	require.NoError(t, json.Unmarshal(entry.Value(), &commit), "decode baseline assignment._commit")

	return commit, entry.Revision()
}

type burstReport struct {
	MaxBurstSize     int
	MaxBurstDuration time.Duration
	P95InterArrival  time.Duration
	TotalAttempts    int
}

// analyzeBursts groups each worker's RecordApplyAttempt timestamps into
// "bursts" — consecutive calls separated by at most idleGap — and returns
// per-worker statistics. workerIDs provides the canonical key order so that
// workers with zero attempts still appear in the result map.
func analyzeBursts(collectors []*recordingBurstCollector, workerIDs []string, idleGap time.Duration) map[string]burstReport {
	out := make(map[string]burstReport, len(collectors))

	for i, c := range collectors {
		wid := workerIDs[i]

		c.mu.Lock()
		samples := append([]burstSample(nil), c.calls...)
		c.mu.Unlock()

		slices.SortFunc(samples, func(a, b burstSample) int { return a.at.Compare(b.at) })

		var (
			currentBurst []burstSample
			gaps         []time.Duration
			maxSize      int
			maxDur       time.Duration
		)

		flush := func() {
			if len(currentBurst) > maxSize {
				maxSize = len(currentBurst)
			}
			if len(currentBurst) >= 2 {
				dur := currentBurst[len(currentBurst)-1].at.Sub(currentBurst[0].at)
				if dur > maxDur {
					maxDur = dur
				}
			}
			currentBurst = nil
		}

		for idx, s := range samples {
			if idx > 0 {
				gap := s.at.Sub(samples[idx-1].at)
				if gap <= idleGap {
					gaps = append(gaps, gap)
					currentBurst = append(currentBurst, s)
					continue
				}
				flush()
			}
			currentBurst = []burstSample{s}
		}
		flush()

		out[wid] = burstReport{
			MaxBurstSize:     maxSize,
			MaxBurstDuration: maxDur,
			P95InterArrival:  percentileDur(gaps, 0.95),
			TotalAttempts:    len(samples),
		}
	}

	return out
}

func percentileDur(xs []time.Duration, p float64) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), xs...)
	slices.SortFunc(sorted, cmp.Compare)
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

func aggregateMaxBurstSize(rs map[string]burstReport) int {
	m := 0
	for _, r := range rs {
		if r.MaxBurstSize > m {
			m = r.MaxBurstSize
		}
	}
	return m
}

func aggregateMaxBurstDuration(rs map[string]burstReport) time.Duration {
	var m time.Duration
	for _, r := range rs {
		if r.MaxBurstDuration > m {
			m = r.MaxBurstDuration
		}
	}
	return m
}

// recommendedWindow rounds the aggregate max burst duration up to the nearest
// 50 ms and adds a 50 ms safety margin. Caps at 1 s to bound
// reassignment-latency overhead.
func recommendedWindow(rs map[string]burstReport) time.Duration {
	const step = 50 * time.Millisecond

	d := aggregateMaxBurstDuration(rs)

	// Round up to next multiple of step using integer arithmetic on the
	// underlying int64 nanosecond values, then convert back.
	ns := int64(d)
	stepNs := int64(step)
	roundedNs := ((ns + stepNs - 1) / stepNs) * stepNs
	w := min(time.Duration(roundedNs)+step, time.Second)

	return w
}

// ── Fleet-wide spatial burst analysis (View 2) ──────────────────────────────

// phaseBound marks the half-open [start, end) interval for a named phase.
// Samples with at >= start and at < end are attributed to this phase.
// Samples on a boundary belong to the later phase.
type phaseBound struct {
	name       string
	start, end time.Time
}

// versionFanout captures the fleet-wide spread of a single assignment version:
// how many distinct workers applied it and over what wall-clock span.
type versionFanout struct {
	WorkerCount int
	Span        time.Duration
	First, Last time.Time
	Workers     []string
}

// fleetReport summarises fleet-wide spatial burst behaviour for one
// measurement window (a named phase or the overall run).
type fleetReport struct {
	// PerVersion maps assignment version → fanout across the fleet.
	PerVersion map[int64]versionFanout

	// PeakConcurrency is the max number of distinct workers with any sample
	// in a sliding 100 ms window ([t-W, t+W], W=idleGap) centred on any
	// single sample. Intentionally version-agnostic: cross-version samples
	// that overlap in time represent real apply-pipeline pressure.
	PeakConcurrency int

	// PeakAt is the timestamp of the first sample where PeakConcurrency was
	// first observed.
	PeakAt time.Time

	// WorstVersionSpan and WorstVersion are computed ONLY over PerVersion
	// entries with WorkerCount > 1. A single-worker retry sample appearing
	// alone as version V would produce a zero-span fanout that would inflate
	// nothing, but explicitly excluding it prevents future readers from
	// assuming the field includes all entries.
	WorstVersionSpan time.Duration
	WorstVersion     int64

	// MultiWorkerVersions is the count of PerVersion entries where
	// WorkerCount > 1. Logged as versions_observed=N in the AGGREGATE_FLEET
	// banner for diagnostic clarity.
	MultiWorkerVersions int
}

// analyzeFleetBursts merges all per-worker sample timelines, attributes each
// sample to a named phase (or "overall"), and returns one fleetReport per
// phase plus an "overall" report covering every sample.
//
// The window parameter is the half-window used for peak-concurrency computation:
// for each sample at time t, distinct workers with any sample in [t-window, t+window]
// are counted. Reuse idleGap (50ms) so the full pressure window is 100ms.
//
//nolint:cyclop
func analyzeFleetBursts(
	collectors []*recordingBurstCollector,
	window time.Duration,
	phases []phaseBound,
) map[string]fleetReport {
	// 1. Collect all samples across all workers into one flat slice.
	var all []burstSample
	for _, c := range collectors {
		c.mu.Lock()
		all = append(all, c.calls...)
		c.mu.Unlock()
	}

	// Sort by timestamp.
	slices.SortFunc(all, func(a, b burstSample) int { return a.at.Compare(b.at) })

	// 2. Build a result map: one entry per phase name + "overall".
	out := make(map[string]fleetReport, len(phases)+1)
	for _, ph := range phases {
		out[ph.name] = fleetReport{PerVersion: make(map[int64]versionFanout)}
	}
	out["overall"] = fleetReport{PerVersion: make(map[int64]versionFanout)}

	// 3. Attribute each sample to its phase(s).
	// A sample belongs to the first phase whose [start, end) interval contains it,
	// plus always to "overall".
	phaseFor := func(at time.Time) string {
		for _, ph := range phases {
			if !at.Before(ph.start) && at.Before(ph.end) {
				return ph.name
			}
		}
		return "" // outside all phase windows — only goes into "overall"
	}

	// 4. Build per-version fanout for each scope.
	updateFanout := func(r *fleetReport, s burstSample) {
		fv := r.PerVersion[s.version]
		if fv.WorkerCount == 0 {
			// First sample for this version in this scope.
			fv.First = s.at
			fv.Last = s.at
		}
		// Track distinct workers.
		alreadySeen := slices.Contains(fv.Workers, s.workerID)
		if !alreadySeen {
			fv.WorkerCount++
			fv.Workers = append(fv.Workers, s.workerID)
		}
		if s.at.Before(fv.First) {
			fv.First = s.at
		}
		if s.at.After(fv.Last) {
			fv.Last = s.at
		}
		fv.Span = fv.Last.Sub(fv.First)
		r.PerVersion[s.version] = fv
	}

	for _, s := range all {
		// Update "overall".
		r := out["overall"]
		updateFanout(&r, s)
		out["overall"] = r

		// Update phase scope (if any).
		if ph := phaseFor(s.at); ph != "" {
			rph := out[ph]
			updateFanout(&rph, s)
			out[ph] = rph
		}
	}

	// 5. Compute WorstVersionSpan and MultiWorkerVersions for each scope.
	for key, r := range out {
		for v, fv := range r.PerVersion {
			if fv.WorkerCount <= 1 {
				continue
			}
			r.MultiWorkerVersions++
			if fv.Span > r.WorstVersionSpan {
				r.WorstVersionSpan = fv.Span
				r.WorstVersion = v
			}
		}
		out[key] = r
	}

	// 6. Compute PeakConcurrency (sliding window, O(N²) over per-scope samples).
	// For each scope, build a filtered sample slice, then walk it.
	computePeak := func(samples []burstSample) (int, time.Time) {
		if len(samples) == 0 {
			return 0, time.Time{}
		}
		peak := 0
		var peakAt time.Time
		for i, s := range samples {
			seen := make(map[string]struct{})
			// Walk backwards within window.
			for j := i; j >= 0; j-- {
				if s.at.Sub(samples[j].at) > window {
					break
				}
				seen[samples[j].workerID] = struct{}{}
			}
			// Walk forwards within window.
			for j := i + 1; j < len(samples); j++ {
				if samples[j].at.Sub(s.at) > window {
					break
				}
				seen[samples[j].workerID] = struct{}{}
			}
			if len(seen) > peak {
				peak = len(seen)
				peakAt = s.at
			}
		}

		return peak, peakAt
	}

	// Build per-scope sample slices (already sorted globally; extract subset).
	overallSamples := all
	pc, pa := computePeak(overallSamples)
	r := out["overall"]
	r.PeakConcurrency = pc
	r.PeakAt = pa
	out["overall"] = r

	for _, ph := range phases {
		var phaseSamples []burstSample
		for _, s := range all {
			if !s.at.Before(ph.start) && s.at.Before(ph.end) {
				phaseSamples = append(phaseSamples, s)
			}
		}
		pc, pa := computePeak(phaseSamples)
		r := out[ph.name]
		r.PeakConcurrency = pc
		r.PeakAt = pa
		out[ph.name] = r
	}

	return out
}

// TestRecommendedApplyJitter is a pure unit test of the recommendation formula.
// It exercises three branches without any cluster setup or env-var gate:
// the 1 s cap, the floor-only path (no spatial burst), and a typical measured case.
func TestRecommendedApplyJitter(t *testing.T) {
	t.Run("cap_branch", func(t *testing.T) {
		// pre-cap formula: ceil(901ms/50ms)*50ms + 100ms = 950ms + 100ms = 1050ms → capped to 1s
		got := recommendedApplyJitter(fleetReport{PeakConcurrency: 2, WorstVersionSpan: 901 * time.Millisecond})
		require.Equal(t, time.Second, got)
	})

	t.Run("floor_only_when_no_spatial_burst", func(t *testing.T) {
		// PeakConcurrency <= 1 → formula floor: floor(100ms) + margin(100ms) = 200ms.
		// WorstVersionSpan is non-zero so that the assertion catches a regression that
		// removed the <= 1 early return: without the branch, this input would compute
		// the typical-case 250ms via the standard formula.
		got := recommendedApplyJitter(fleetReport{PeakConcurrency: 1, WorstVersionSpan: 106 * time.Millisecond})
		require.Equal(t, 200*time.Millisecond, got)
	})

	t.Run("typical", func(t *testing.T) {
		// Matches the plan's worked example:
		// ceil(106ms/50ms)*50ms = 150ms; max(150ms, 100ms floor) = 150ms; +100ms margin = 250ms
		got := recommendedApplyJitter(fleetReport{PeakConcurrency: 5, WorstVersionSpan: 106 * time.Millisecond})
		require.Equal(t, 250*time.Millisecond, got)
	})
}

// recommendedApplyJitter computes the recommended Config.ApplyStartJitter
// from a fleetReport:
//
//	max(ceil(worst_version_span / 50ms) * 50ms, 100ms) + 100ms, capped at 1s.
//
// The floor (100 ms) prevents recommending sub-50 ms values that would be
// dwarfed by application-level scheduling jitter. The +100 ms safety margin
// covers one slot of scheduler-jitter difference (test → production) and one
// slot of measurement noise. The 1 s cap signals a non-jitter root cause.
//
// If peak_concurrency <= 1 (no spatial burst observed), the formula floor
// (200 ms) is returned.
func recommendedApplyJitter(r fleetReport) time.Duration {
	const (
		step   = 50 * time.Millisecond
		floor  = 100 * time.Millisecond
		margin = 100 * time.Millisecond
		cap1s  = time.Second
	)

	if r.PeakConcurrency <= 1 {
		// No spatial burst observed; return formula floor.
		return floor + margin
	}

	ws := r.WorstVersionSpan
	// ceil(ws / step) * step, using integer arithmetic.
	wsNs := int64(ws)
	stepNs := int64(step)
	roundedNs := int64(math.Ceil(float64(wsNs)/float64(stepNs))) * stepNs
	rounded := max(time.Duration(roundedNs), floor)

	result := min(rounded+margin, cap1s)

	return result
}
