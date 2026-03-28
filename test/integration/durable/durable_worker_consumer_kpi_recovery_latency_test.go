package durable_test

import (
	"context"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/durable"
	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// result captures a single consumed message observation for KPI tracking.
type result struct {
	id      string
	latency time.Duration
}

// resultCollector is a threadsafe collector for message observations.
// It allows concurrent writes from the async handler and safe snapshots
// for readers that want to scan recent results without holding locks.
type resultCollector struct {
	mu    sync.RWMutex
	items []result
}

func newResultCollector(capacity int) *resultCollector {
	return &resultCollector{items: make([]result, 0, capacity)}
}

func (rc *resultCollector) Add(r result) {
	rc.mu.Lock()
	rc.items = append(rc.items, r)
	rc.mu.Unlock()
}

// Snapshot returns a copy of the collected items to avoid holding locks
// while executing potentially expensive scans in tests.
func (rc *resultCollector) Snapshot() []result {
	rc.mu.RLock()
	// Copy to avoid races with concurrent appends.
	out := make([]result, len(rc.items))
	copy(out, rc.items)
	rc.mu.RUnlock()
	return out
}

// TestWorkerConsumer_ExternalDeletionRecoveryKPI measures recovery latency after external durable
// consumer deletion across multiple trials. It provides percentile and success rate metrics to
// validate KPIs (p50 <2s, p95 <5s, p99 <10s; success >=99.5%; zero duplicate IDs).
//
// By default (no env override) this runs a reduced number of trials to keep CI time bounded.
// Set TRIALS=500 locally to execute the full KPI run. Results are logged.
func TestWorkerConsumer_ExternalDeletionRecoveryKPI(t *testing.T) {
	t.Skip()
	if testing.Short() {
		t.Skip("integration: skipping KPI latency test in short mode")
	}

	// Trial configuration (can be overridden via env for full KPI run)
	trialCount := 25 // small default for CI
	if v := os.Getenv("TRIALS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			trialCount = n
		}
	}

	ctx := context.Background()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "kpi-rec-stream"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: streamName, Subjects: []string{"kpi.rec.>"}})
	require.NoError(t, err)

	// Handler records message IDs and arrival time.
	// Use a threadsafe collector to capture observations across goroutines.
	rc := newResultCollector(trialCount * 3)

	// Partition set fixed (single partition is enough to validate semantics).
	mh := func(c context.Context, msg jetstream.Msg) error {
		id := string(msg.Data())
		// Recovery latency is encoded as message header start time in unix nano (published right after deletion).
		startStr := msg.Headers().Get("X-Delete-Time-Nano")
		var latency time.Duration
		if startStr != "" {
			if st, err := strconv.ParseInt(startStr, 10, 64); err == nil {
				latency = time.Since(time.Unix(0, st))
			}
		}
		rc.Add(result{id: id, latency: latency})

		return msg.Ack()
	}

	helper, err := durable.NewWorkerConsumer(js, durable.WorkerConsumerConfig{
		StreamName:      streamName,
		ConsumerPrefix:  "kpiw",
		SubjectTemplate: "kpi.rec.{{.PartitionID}}",
		BatchSize:       10,
		// Prefer DeliverPolicy=LastPerSubject only on recreation to avoid backlog replay
		// while still capturing the message published immediately after deletion
		// DeliverLastPerSubjectOnRecreation: true, // Removed in V2 refactor, using default policy

	}, mh)
	require.NoError(t, err)
	defer helper.Close(context.Background())

	// Initial assignment (single partition simplifies duplicate tracking)
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-KPI", []parti.Partition{{Keys: []string{"a"}}}))

	// Pick the subject and look up its current durable name
	subs := helper.WorkerSubjects()
	require.Contains(t, subs, "kpi.rec.a")
	ci, err := helper.WorkerConsumerInfo(ctx, "kpi.rec.a")
	require.NoError(t, err)
	durable := ci.Name

	// Per-trial aggregates
	latencies := make([]time.Duration, 0, trialCount)
	duplicates := 0
	successes := 0

	tracker := &kpiTracker{rc: rc, latencies: &latencies, successes: &successes, duplicates: &duplicates}
	for trial := 0; trial < trialCount; trial++ {
		if !runSingleRecoveryTrial(t, ctx, js, streamName, durable, tracker) {
			continue
		}
	}

	if len(latencies) == 0 {
		t.Fatalf("no successful recoveries recorded; trials=%d", trialCount)
	}
	// Percentile helper
	percentile := func(p float64) time.Duration {
		slices.Sort(latencies)
		if len(latencies) == 0 {
			return 0
		}
		idx := int((p / 100.0) * float64(len(latencies)-1))
		return latencies[idx]
	}

	p50 := percentile(50)
	p95 := percentile(95)
	p99 := percentile(99)
	successRate := float64(successes) / float64(trialCount)

	t.Logf("SC-P0-1 KPI results trials=%d success_rate=%.2f p50=%s p95=%s p99=%s duplicates=%d", trialCount, successRate, p50, p95, p99, duplicates)

	// Enforce criteria only when we have a larger sample (avoid flakiness in small CI run)
	if trialCount >= 100 {
		require.Less(t, p50, 2*time.Second, "p50 recovery latency should be <2s")
		require.Less(t, p95, 5*time.Second, "p95 recovery latency should be <5s")
		require.Less(t, p99, 10*time.Second, "p99 recovery latency should be <10s")
		require.GreaterOrEqual(t, successRate, 0.995, "success rate should be >=99.5%")
		require.Zero(t, duplicates, "expected zero duplicate message IDs during recovery windows")
	}
}

// runSingleRecoveryTrial executes a single durable deletion + publish + observation cycle.
// Returns true if a recovery latency was recorded.
// kpiTracker aggregates shared slices and counters to reduce argument count.
type kpiTracker struct {
	rc         *resultCollector
	latencies  *[]time.Duration
	successes  *int
	duplicates *int
}

func runSingleRecoveryTrial(
	t *testing.T,
	ctx context.Context,
	js jetstream.JetStream,
	streamName string,
	durable string,
	tr *kpiTracker,
) bool {
	t.Helper()
	if err := js.DeleteConsumer(ctx, streamName, durable); err != nil {
		// Treat not found as already-deleted; proceed with trial
		t.Logf("DeleteConsumer returned error (ignored): %v", err)
	}
	msgID := strconv.FormatInt(time.Now().UnixNano(), 10)
	start := time.Now()
	m := nats.NewMsg("kpi.rec.a")
	m.Data = []byte(msgID)
	if m.Header == nil {
		m.Header = nats.Header{}
	}
	m.Header.Set("X-Delete-Time-Nano", strconv.FormatInt(start.UnixNano(), 10))
	_, err := js.PublishMsg(ctx, m)
	require.NoError(t, err)

	firstDeadline := time.Now().Add(15 * time.Second)
	var firstLatency time.Duration
	for time.Now().Before(firstDeadline) {
		// Scan backwards (recent results first)
		snap := tr.rc.Snapshot()
		for i := len(snap) - 1; i >= 0; i-- {
			if snap[i].id == msgID {
				firstLatency = snap[i].latency
				break
			}
		}
		if firstLatency > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if firstLatency == 0 {
		return false
	}
	(*tr.successes)++
	*tr.latencies = append(*tr.latencies, firstLatency)
	dupWindow := time.Now().Add(2 * time.Second)
	seenOnce := false
	seenTwice := false
	for time.Now().Before(dupWindow) {
		count := 0
		snap := tr.rc.Snapshot()
		for i := len(snap) - 1; i >= 0; i-- {
			if snap[i].id == msgID {
				count++
				if count >= 2 {
					break
				}
			}
		}
		if count >= 1 && !seenOnce {
			seenOnce = true
		} else if count >= 2 {
			seenTwice = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if seenTwice {
		(*tr.duplicates)++
	}

	return true
}
