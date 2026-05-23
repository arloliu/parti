package parti

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/stretchr/testify/require"
)

// TestMonitorAssignmentChanges_EnvelopeNoRaceUnderConcurrentKVTraffic is
// the regression-pin for GAP-3 from the v2.4.1->main integration-
// discipline audit (tmp/integration_discipline_audit_v2.4.1_to_main.md).
//
// The risk: the F2 envelope's restart loop in monitorAssignmentChanges
// (manager_assignment.go after the budget-reset fix) calls kv.Watch on
// m.assignmentKV inside the envelope's Work, and the runAssignmentWatchSession
// reconcile-arm calls kv.Get on the same handle. Beyond the monitor's own
// goroutine, other manager paths (waitForAssignment / fetchAssignment in
// manager_election.go and the commit-payload-fetch path at
// manager_assignment.go:787) also call Get on the same m.assignmentKV.
// Under nats.go's current model a *jetstream.KeyValue is backed by a
// *stream whose cached fields are written by Watch and read by Get; if
// these access paths run on different goroutines without serialization,
// `go test -race` trips WARNING: DATA RACE.
//
// This is the smallest race surface of the three GAP regressions:
//
//   - GAP-2 (claim resolver, internal/durable/claim_resolver_envelope_concurrency_test.go):
//     supervisor's WatchAll restart runs concurrently with batched
//     update processing AND with external ForceRefreshPartition calls
//     on the same handle.
//   - GAP-1 (source watcher, source/nats_kv_envelope_concurrency_test.go):
//     restartWatcher spawns watchLoop in a SEPARATE goroutine, so the
//     restart's kv.Watch (write) can run concurrently with the previous
//     watchLoop's tail and with kv.Get callers on the same handle.
//   - GAP-3 (this file): the post-fix monitorAssignmentChanges keeps
//     the session in the SAME goroutine as the envelope (the outer
//     for-loop drives both), so Watch and the session's Get are
//     serialized within the monitor. The race surface is between THIS
//     goroutine's Watch and OTHER manager goroutines' Get on
//     m.assignmentKV (the election-path waitForAssignment loop, the
//     commit-payload fetch, and any external test goroutines reading
//     the same handle).
//
// # Mechanism
//
//   - Tighten watcherBaseBackoff / watcherMaxBackoff / watcherMaxAttempts
//     to sub-second values via the package-private test seams.
//   - Stand up embedded NATS + a real assignment KV bucket; plant an
//     initial alias under "assignment.<workerID>".
//   - Wrap the KV with forceCloseWatcherKV so the test can drive
//     watcher closures from outside; the wrapper still calls the
//     underlying *jetstream.KeyValue's Watch (which mutates the cached
//     *stream state) so the race surface is preserved.
//   - Run monitorAssignmentChanges in a goroutine; force-close the
//     latest watcher every ~25 ms to drive restart cycles.
//   - Concurrent goroutines: 4x kv.Get on the underlying handle (NOT
//     via the wrapper — they target the same *stream that
//     wrap.Watch mutates), 1x kv.Put rewrites every 10 ms (keeps the
//     watcher updates stream alive), 1x kv.Keys probes every 5 ms.
//   - Soak for ~5 s; assert no race detector trigger plus a post-soak
//     CurrentAssignment liveness check.
//
// # Why this file lives at the repository root rather than under
// # test/integration/manager/
//
// AGENTS.md suggests test/integration/<package>/. The watcherBaseBackoff
// / watcherMaxBackoff / watcherMaxAttempts test seams are package-
// private vars in manager_assignment.go (line 25-38, with the
// "Production code must NEVER mutate these" doc). Same-package access
// is the cleanest path — same precedent as
// manager_assignment_watcher_envelope_test.go in this package, and as
// the equivalent files for GAP-1 (source/nats_kv_envelope_concurrency_test.go)
// and GAP-2 (internal/durable/claim_resolver_envelope_concurrency_test.go).
// The discipline's intent (real NATS + race detector + concurrent
// goroutines on the production code path) is met either way.
func TestMonitorAssignmentChanges_EnvelopeNoRaceUnderConcurrentKVTraffic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	// Not t.Parallel — mutates package-level test-seam vars.
	origBase := watcherBaseBackoff
	origMax := watcherMaxBackoff
	origAttempts := watcherMaxAttempts
	watcherBaseBackoff = 10 * time.Millisecond
	watcherMaxBackoff = 50 * time.Millisecond
	// Generous so the soak doesn't exhaust the budget — the property
	// under test is "no race on shared *stream cached state".
	watcherMaxAttempts = 1000
	t.Cleanup(func() {
		watcherBaseBackoff = origBase
		watcherMaxBackoff = origMax
		watcherMaxAttempts = origAttempts
	})

	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "assignment-stress")

	m, _, _, _ := newTestManager(t)
	m.assignmentKV = kv
	workerID := m.WorkerID()
	key := fmt.Sprintf("assignment.%s", workerID)

	// Plant initial alias so the first session has something to apply.
	v1 := Assignment{Version: 1, LeaderRevision: 10}
	b1, err := json.Marshal(v1)
	require.NoError(t, err)
	_, err = kv.Create(t.Context(), key, b1)
	require.NoError(t, err)

	wrap := &forceCloseWatcherKV{KeyValue: kv}

	monitorDone := make(chan struct{})
	go func() {
		m.monitorAssignmentChanges(m.ctx, wrap)
		close(monitorDone)
	}()

	// Wait for the initial watch + apply.
	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 1
	}, 3*time.Second, 10*time.Millisecond, "initial watcher must apply V=1")

	soakCtx, soakCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer soakCancel()

	var totalCloses atomic.Int64
	var totalGets atomic.Int64
	var totalKeys atomic.Int64
	var totalPuts atomic.Int64

	var soakWg sync.WaitGroup

	// Force-close goroutine: drives the envelope's outer for-loop to
	// run a fresh kv.Watch attempt every ~25 ms. Each Watch refreshes
	// nats.go's cached *stream fields — the race-write side.
	soakWg.Go(func() {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-soakCtx.Done():
				return
			case <-ticker.C:
				wrap.forceCloseLatest()
				totalCloses.Add(1)
			}
		}
	})

	// Get-side goroutines: kv.Get on the UNDERLYING handle (not via
	// the wrapper). This is what other manager paths look like
	// (manager_election.go's fetchAssignment, manager_assignment.go's
	// FetchAndVerifyCommitPayload). Same handle as wrap.Watch, same
	// cached *stream state — the race-read side.
	const numGetters = 4
	for range numGetters {
		soakWg.Go(func() {
			for {
				select {
				case <-soakCtx.Done():
					return
				default:
				}
				if _, err := kv.Get(soakCtx, key); err == nil {
					totalGets.Add(1)
				}
			}
		})
	}

	// Keys-side goroutine: kv.Keys probes on the same handle. Same
	// shape as the manager's eventual enumeration paths.
	soakWg.Go(func() {
		for {
			select {
			case <-soakCtx.Done():
				return
			default:
			}
			if _, err := kv.Keys(soakCtx); err == nil {
				totalKeys.Add(1)
			}
			time.Sleep(5 * time.Millisecond)
		}
	})

	// Put-side goroutine: monotonically increasing Version so the
	// monitor sees real assignment updates between closures.
	soakWg.Go(func() {
		i := 2
		for {
			select {
			case <-soakCtx.Done():
				return
			default:
			}
			v := Assignment{Version: int64(i), LeaderRevision: uint64(10 + i)} //nolint:gosec // small test indices
			b, err := json.Marshal(v)
			if err == nil {
				if _, err := kv.Put(soakCtx, key, b); err == nil {
					totalPuts.Add(1)
				}
			}
			i++
			time.Sleep(10 * time.Millisecond)
		}
	})

	<-soakCtx.Done()
	soakWg.Wait()

	// Liveness sanity: the monitor must still apply a fresh higher-
	// Version put after the soak. If the monitor collapsed under the
	// aggressive close cadence, this would fail and indicate the test
	// load broke the system rather than catching a race.
	finalVersion := int64(10_000)
	final := Assignment{Version: finalVersion, LeaderRevision: 99_999}
	bFinal, err := json.Marshal(final)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), key, bFinal)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version >= finalVersion
	}, 5*time.Second, 50*time.Millisecond,
		"monitor must remain functional after the concurrent soak")

	// Primary assertion: race detector did not fire during the soak.
	require.False(t, t.Failed(),
		"race detector or sub-assertion failed during the concurrent soak; "+
			"check stderr for WARNING: DATA RACE blocks. The monitor's F2 "+
			"envelope restart loop must not race with concurrent kv.Get / "+
			"kv.Keys reads on the shared *jetstream.KeyValue handle.")

	m.cancel()
	select {
	case <-monitorDone:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorAssignmentChanges did not exit after ctx cancel")
	}

	t.Logf("soak complete: %d watcher closes, %d Gets, %d Keys probes, %d Puts",
		totalCloses.Load(), totalGets.Load(), totalKeys.Load(), totalPuts.Load())
}
