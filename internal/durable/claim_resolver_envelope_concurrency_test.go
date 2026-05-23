package durable

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestClaimResolver_EnvelopeNoRaceUnderConcurrentKVTraffic is the
// regression-pin for GAP-2 from the v2.4.1->main integration-discipline
// audit (tmp/integration_discipline_audit_v2.4.1_to_main.md).
//
// The risk: the supervisor's WatchAll restart loop (claim_resolver.go:768)
// shares the *jetstream.KeyValue handle (r.kv) with every other KV-touching
// path on the resolver — Get inside ForceRefreshPartition
// (claim_resolver.go:479), Get + Keys inside warm
// (claim_resolver.go:519/533), and any external callers operating on the
// same handle. Under nats.go's internal model a *jetstream.KeyValue is
// backed by a *stream whose cached fields are written by metadata-touching
// calls (Watch/WatchAll/Status) and read by Get/GetLastMsgForSubject. If
// these access paths run on different goroutines without serialization,
// `go test -race` trips WARNING: DATA RACE.
//
// This is the same shape as the bug fixed in commit 4937443 ("open
// dedicated KV handle for epoch probe") — but for a different monitor
// goroutine. The fix for the epoch monitor was to open a dedicated probe
// handle per bucket. We have NOT yet applied an analogous fix to the
// claim resolver because no race has been observed; this test is the
// canary that would catch one if it surfaces.
//
// # Mechanism
//
//   - Tighten watcherBaseBackoff / watcherMaxBackoff / watcherMaxAttempts
//     to sub-second values so the supervisor's restart loop can fire many
//     times in the soak window.
//   - Stand up a real embedded NATS + JetStream KV bucket.
//   - Start a ClaimBasedResolver with reconciler disabled (so the
//     supervisor's WatchAll path is the only thing restarting watchers).
//   - Concurrent goroutines: force-close the current watcher every ~25 ms
//     to drive supervisor restarts; concurrent Gets across multiple
//     partitions; concurrent Keys probes; concurrent Puts to keep the
//     watcher updates stream alive.
//   - Soak for ~5 s, then assert (a) t.Failed() is false (no race), and
//     (b) the resolver is still functional (a fresh ForceRefreshPartition
//     succeeds against a freshly-written key).
//
// # Why this file lives in internal/durable
//
// AGENTS.md § "Concurrency stress tests for monitor goroutines" calls for
// stress tests under test/integration/<package>/. The watcherBaseBackoff /
// watcherMaxBackoff / watcherMaxAttempts test seams used here are
// package-private vars in claim_resolver.go ("// Production code must
// NEVER mutate these"). Same-package access is the cleanest path — same
// as the precedent set by internal/durable/claim_resolver_restart_test.go.
// The discipline's intent (real NATS + race detector + concurrent
// goroutines on the production code path) is met either way.
func TestClaimResolver_EnvelopeNoRaceUnderConcurrentKVTraffic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	// Not t.Parallel() — mutates package-level test-seam vars.
	origBase := watcherBaseBackoff
	origMax := watcherMaxBackoff
	origAttempts := watcherMaxAttempts
	watcherBaseBackoff = 10 * time.Millisecond
	watcherMaxBackoff = 50 * time.Millisecond
	// Generous so the soak doesn't exhaust the budget on consecutive
	// rapid closures; the property under test is "no race on shared
	// *stream cached state", not "envelope exhaustion behaves
	// correctly" (that contract is pinned in
	// claim_resolver_retry_envelope_test.go).
	watcherMaxAttempts = 1000
	t.Cleanup(func() {
		watcherBaseBackoff = origBase
		watcherMaxBackoff = origMax
		watcherMaxAttempts = origAttempts
	})

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff-stress"})
	require.NoError(t, err)

	// Plant a handful of claims so the Get-side goroutines have
	// something to find. The race surface is independent of the
	// returned data — Get reads the cached *stream state regardless of
	// whether the key exists.
	const numPartitions = 8
	for i := range numPartitions {
		c := handoff.Claim{
			PartitionID: fmt.Sprintf("p%d", i),
			Owner:       "worker-A",
			State:       handoff.ClaimStateStable,
			Epoch:       1,
			LastUpdated: time.Now().UTC(),
		}
		b, err := c.Marshal()
		require.NoError(t, err)
		_, err = kv.Put(ctx, fmt.Sprintf("claims/p%d", i), b)
		require.NoError(t, err)
	}

	// Reconciler disabled so the supervisor's WatchAll restart is the
	// only path that re-establishes the watcher — isolates the race
	// surface to the WatchAll vs Get/Keys cross-goroutine pattern.
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	require.Eventually(t, func() bool {
		_, _, _, ok := r.GetOwner("p0")
		return ok
	}, 5*time.Second, 10*time.Millisecond, "initial warm must populate the cache")

	soakCtx, soakCancel := context.WithTimeout(ctx, 5*time.Second)
	defer soakCancel()

	var wg sync.WaitGroup
	var totalCloses atomic.Int64
	var totalGets atomic.Int64
	var totalKeys atomic.Int64
	var totalPuts atomic.Int64

	// Force-close goroutine: drives supervisor restarts. Each Stop()
	// closes the current watcher's Updates channel which causes
	// processWatcher to return errWatcherClosed, the supervisor to
	// pre-sleep ~10 ms, then runWatcher to call kv.WatchAll again. The
	// WatchAll call refreshes the cached *stream state — that's the
	// race-write side.
	wg.Go(func() {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-soakCtx.Done():
				return
			case <-ticker.C:
				r.watcherMu.Lock()
				w := r.currentWatcher
				r.watcherMu.Unlock()
				if w != nil {
					_ = w.Stop() // idempotent; closes Updates if not already closed
					totalCloses.Add(1)
				}
			}
		}
	})

	// Get-side goroutines: drive kv.Get reads on the same handle. This
	// is the race-read side. ErrKeyNotFound on missing keys is fine.
	const numGetters = 4
	for g := range numGetters {
		seed := g
		wg.Go(func() {
			i := 0
			for {
				select {
				case <-soakCtx.Done():
					return
				default:
				}
				key := fmt.Sprintf("claims/p%d", (seed+i)%numPartitions)
				if _, err := kv.Get(soakCtx, key); err == nil {
					totalGets.Add(1)
				}
				i++
			}
		})
	}

	// Keys-side goroutine: drive kv.Keys probes on the same handle.
	// Kept slower than Get (5 ms) since Keys is more expensive.
	wg.Go(func() {
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

	// Put-side goroutine: keep the watcher updates stream alive so
	// processWatcher sees traffic between closures.
	wg.Go(func() {
		i := 0
		for {
			select {
			case <-soakCtx.Done():
				return
			default:
			}
			pid := fmt.Sprintf("p%d", i%numPartitions)
			c := handoff.Claim{
				PartitionID: pid,
				Owner:       fmt.Sprintf("worker-%d", i),
				State:       handoff.ClaimStateStable,
				Epoch:       int64(i + 2), //nolint:gosec // small test indices
				LastUpdated: time.Now().UTC(),
			}
			b, err := c.Marshal()
			if err == nil {
				if _, err := kv.Put(soakCtx, fmt.Sprintf("claims/%s", pid), b); err == nil {
					totalPuts.Add(1)
				}
			}
			i++
			time.Sleep(10 * time.Millisecond)
		}
	})

	<-soakCtx.Done()
	wg.Wait()

	// Liveness sanity: a fresh ForceRefreshPartition against a known
	// key should succeed after the soak. If the resolver collapsed
	// under the aggressive close-cadence, this would fail and tell us
	// the test load broke the system rather than caught a race.
	livenessCtx, livenessCancel := context.WithTimeout(ctx, 3*time.Second)
	defer livenessCancel()
	// Bypass the rate-limit cooldown by waiting for it to expire if
	// needed, then issuing the refresh.
	require.Eventually(t, func() bool {
		err := r.ForceRefreshPartition(livenessCtx, "p0")
		return err == nil
	}, 3*time.Second, 50*time.Millisecond,
		"resolver must remain functional after the concurrent soak")

	// Primary assertion: the race detector did not fire during the
	// soak. t.Failed() flips to true on any -race-triggered "found
	// data race" stderr write, on any sub-test failure, and on any
	// prior require failure. Since we've already passed the liveness
	// checks above, a true here means the race detector tripped.
	require.False(t, t.Failed(),
		"race detector or sub-assertion failed during the concurrent soak; "+
			"check stderr for WARNING: DATA RACE blocks. The claim "+
			"resolver's supervisor WatchAll restart loop must not race "+
			"with concurrent kv.Get / kv.Keys reads on the shared "+
			"*jetstream.KeyValue handle.")

	t.Logf("soak complete: %d watcher closes, %d Gets, %d Keys probes, %d Puts",
		totalCloses.Load(), totalGets.Load(), totalKeys.Load(), totalPuts.Load())
}
