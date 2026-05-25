package manager_test

import (
	"cmp"
	"context"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	parti "github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
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
//
// The diagnostic boots a 3-node embedded NATS cluster (via
// testutil.StartEmbeddedNATSCluster), starts N=20 parti managers backed by
// R=3 KV buckets, waits for steady state, then kills the current Parti
// calculator leader. Killing the NATS JetStream meta-leader was found to be
// ineffective: the NATS meta-leader and Parti's calculator leader are separate
// election layers, and multi-URL-seeded nats.Conn reconnects the Parti leader
// to a surviving node within milliseconds, leaving the Parti election
// undisturbed. The 3-node cluster and R=3 KV buckets are retained because they
// exercise the apply pipeline under the same JetStream replication semantics
// that production uses.
//
// Triggering a Parti-leader re-election forces:
//  1. The killed leader's stable-ID claim expires → peer takeover via stableid election
//  2. The new Parti leader's calculator publishes Version=N+1 (a genuinely new version)
//  3. All surviving workers' assignment-watchers see Version N+1 simultaneously
//  4. The version gate (manager_assignment.go: oldVersion >= newVersion → skip) lets N+1 through
//  5. applyAssignment fires across the fleet at roughly the same time — that is the burst
//
// Config.AssignmentWatcherDebounce is left at 0 (its default) deliberately:
// this diagnostic measures the *raw* burst size to inform what the debounce
// default should be. Running it with debounce enabled would mute the signal.
//
// The test collects RecordApplyAttempt timestamps, groups them into bursts
// (consecutive calls within idleGap of each other), and emits:
//
//   - per-worker: max burst size, max burst duration, p95 inter-arrival
//   - AGGREGATE banner: fleet-wide max + recommended_debounce_window
//
// The recommended_debounce_window value is the operator-facing guidance for
// Config.AssignmentWatcherDebounce. Paste it into the PR description and
// release notes.
//
// Opt-in: set PARTI_RUN_HERD_DIAGNOSTIC=1 to run.
func TestApplyCoalescing_UnderReElectionBurst(t *testing.T) {
	if os.Getenv("PARTI_RUN_HERD_DIAGNOSTIC") != "1" {
		t.Skip("set PARTI_RUN_HERD_DIAGNOSTIC=1 to run")
	}

	const (
		numWorkers    = 20
		numPartitions = 100
		idleGap       = 50 * time.Millisecond
		// preKillSettle lets watcher pipelines quiesce before the kill so the
		// measurement reflects re-election burst, not startup noise.
		preKillSettle = 5 * time.Second
	)

	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
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
	cfg := testutil.IntegrationTestConfig()
	cfg.KVBuckets.Replicas = 3
	cfg.HeartbeatTTL = 2 * time.Second
	cfg.ElectionTimeout = 1 * time.Second
	t.Logf("config: KVBuckets.Replicas=%d HeartbeatTTL=%s ElectionTimeout=%s",
		cfg.KVBuckets.Replicas, cfg.HeartbeatTTL, cfg.ElectionTimeout)

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

	// Log pre-kill assignment version from a surviving worker (proves the new
	// leader will publish a strictly higher version after takeover).
	survivorIdx := (leaderIdx + 1) % numWorkers
	preKillVersion := mgrs[survivorIdx].CurrentAssignment().Version
	t.Logf("pre-kill: worker=%s assignment_version=%d", mgrs[survivorIdx].WorkerID(), preKillVersion)
	t.Logf("killing parti calculator leader: index=%d worker=%s", leaderIdx, oldLeaderID)

	// Stop the leader using the test context (180 s total budget) so the
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

	// Soak to capture the apply-attempt burst as the new leader's published
	// Version=N+1 propagates through every surviving worker's watcher.
	const postKillSoak = 10 * time.Second
	t.Logf("soaking %s to capture burst tail", postKillSoak)
	select {
	case <-time.After(postKillSoak):
	case <-ctx.Done():
		t.Fatalf("context cancelled during soak: %v", ctx.Err())
	}

	// Log post-soak assignment version from the same survivor to confirm
	// the new leader published Version=preKillVersion+N (confirms the
	// trigger fired; if equal, the election did not complete).
	postSoakVersion := mgrs[survivorIdx].CurrentAssignment().Version
	t.Logf("post-soak: worker=%s assignment_version=%d (delta=%d)",
		mgrs[survivorIdx].WorkerID(), postSoakVersion, postSoakVersion-preKillVersion)

	// Collect samples and compute per-worker burst statistics.
	workerIDs := make([]string, numWorkers)
	for i, m := range mgrs {
		workerIDs[i] = m.WorkerID()
	}
	results := analyzeBursts(collectors, workerIDs, idleGap)

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
	w := time.Duration(roundedNs) + step

	if w > time.Second {
		w = time.Second
	}

	return w
}
