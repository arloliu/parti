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
// behavior across a worker fleet during assignment churn.
//
// The diagnostic boots an embedded single-node NATS cluster, starts N=20
// parti managers, waits for steady state, then drives rebalancing by adding
// extra workers in three waves. Each wave causes the leader calculator to
// publish a new assignment version; every existing worker's watcher fires
// and calls RecordApplyAttempt. The test collects those samples, groups them
// into bursts (consecutive calls within idleGap of each other), and emits:
//
//   - per-worker: max burst size, max burst duration, p95 inter-arrival
//   - AGGREGATE banner: fleet-wide max + recommended_debounce_window
//
// The recommended_debounce_window value is the operator-facing guidance for
// Config.AssignmentWatcherDebounce. Paste it into the PR description and
// release notes.
//
// Deviation from plan: the plan specified a 3-node embedded NATS cluster with
// a forced meta-leader kill to produce the churn. No 3-node cluster helper
// exists in this repo (only testutil.StartEmbeddedNATS — single-node). The
// substitute trigger is worker-churn rebalancing: adding workers mid-soak
// causes the leader to re-publish an assignment that every existing watcher
// sees, producing an equivalent burst on the apply pipeline.
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
		soakAfter     = 10 * time.Second
		numWaves      = 3
	)

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Build per-worker recording collectors, keyed by slot index.
	collectors := make([]*recordingBurstCollector, numWorkers)
	for i := range collectors {
		collectors[i] = &recordingBurstCollector{NopMetrics: metrics.NewNop()}
	}

	// Phase 1: start numWorkers steady-state workers, each with its own
	// recording collector wired via WithMetrics.
	wc := testutil.NewWorkerCluster(t, nc, numPartitions)
	mgrs := make([]*parti.Manager, numWorkers)
	for i := range collectors {
		mgrs[i] = wc.AddWorkerWithOptions(ctx, parti.WithMetrics(collectors[i]))
	}
	defer wc.StopWorkers()

	// StartWorkers starts all workers and blocks until every one reaches
	// StateStable (15 s per worker, enforced inside StartWorkers).
	wc.StartWorkers(ctx)
	t.Logf("phase 1: %d workers reached StateStable", numWorkers)

	// Phase 2: drive churn by adding extra workers in numWaves waves.
	// Each addition triggers a rebalance: the leader calculator computes
	// new partition assignments and publishes a fresh version. Every
	// existing worker's assignment-watcher fires and calls RecordApplyAttempt
	// on the apply pipeline — that's the burst this diagnostic measures.
	waveInterval := soakAfter / time.Duration(numWaves)
	for wave := range numWaves {
		extra := wc.AddWorkerWithOptions(ctx, parti.WithMetrics(&recordingBurstCollector{NopMetrics: metrics.NewNop()}))
		require.NoError(t, extra.Start(ctx))
		t.Logf("phase 2 wave %d: extra worker started, sleeping %s for burst tail", wave+1, waveInterval)
		select {
		case <-time.After(waveInterval):
		case <-ctx.Done():
			t.Fatalf("context cancelled during soak: %v", ctx.Err())
		}
	}

	// Phase 3: collect samples and compute per-worker burst statistics.
	// Map key is the Manager's WorkerID so workers with zero attempts still
	// appear (avoiding the empty-key collision that arises if we key by
	// burstSample.workerID alone).
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
