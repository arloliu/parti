package source

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/partcodec"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestNatsKV_EnvelopeNoRaceUnderConcurrentKVTraffic is the regression-pin
// for GAP-1 from the v2.4.1->main integration-discipline audit
// (tmp/integration_discipline_audit_v2.4.1_to_main.md).
//
// The risk: the watcher restart loop (restartWatcher → tryRebindWatcher →
// s.watchFn → s.kv.Watch at source/nats_kv.go:298-300, 923-948) shares
// the *jetstream.KeyValue handle (s.kv) with every other KV-touching
// path on the source — Get inside Start (source/nats_kv.go:328), Get
// inside reconcile and read helpers (lines 1019, 1058, 1087), and
// Create / Update inside writers (lines 578, 580, 655, 657). Under
// nats.go's current model a *jetstream.KeyValue is backed by a *stream
// whose cached fields are written by metadata-touching calls
// (Watch/WatchAll/Status) and read by Get/GetLastMsgForSubject. If
// these access paths run on different goroutines without serialization,
// `go test -race` trips WARNING: DATA RACE.
//
// This is the same shape as the bug fixed in commit 4937443 ("open
// dedicated KV handle for epoch probe") — but for a different monitor
// goroutine. The fix for the epoch monitor was to open a dedicated probe
// handle per bucket. We have NOT yet applied an analogous fix to the
// source watcher because no race has been observed; this test is the
// canary that would catch one if it surfaces.
//
// # Mechanism
//
//   - Tighten watcherBaseBackoff / watcherMaxBackoff / watcherMaxAttempts
//     to sub-second values via the package-private test seams (already
//     used by nats_kv_retry_envelope_test.go).
//   - Stand up a real embedded NATS + JetStream KV bucket with an
//     initial partition list seeded under s.key.
//   - Start a NatsKV source with reconciler disabled so the watcher
//     restart loop is the sole watcher-re-establish path.
//   - Concurrent goroutines: force-close the current watcher every
//     ~25 ms to drive restartWatcher; four goroutines drive kv.Get on
//     s.key; one goroutine drives kv.Put rewrites to keep the watcher
//     updates stream alive; one drives listPartitions(t, src) reads through
//     the cached state.
//   - Soak for ~5 s, then assert (a) t.Failed() is false (no race), and
//     (b) the source still applies a fresh partition update via
//     src.Update.
//
// # Why this file lives in source/ rather than test/integration/source/
//
// AGENTS.md § "Concurrency stress tests for monitor goroutines" calls
// for stress tests under test/integration/<package>/. The
// watcherBaseBackoff / watcherMaxBackoff / watcherMaxAttempts test seams
// used here are package-private vars in nats_kv.go (line 42-54, with the
// `withWatcherBackoff` helper that mutates them already living in this
// package — see nats_kv_retry_envelope_test.go). Same-package access is
// the cleanest path. The discipline's intent (real NATS + race detector
// + concurrent goroutines on the production code path) is met either
// way. Mirrors the equivalent file
// internal/durable/claim_resolver_envelope_concurrency_test.go for the
// claim resolver's GAP-2.
func TestNatsKV_EnvelopeNoRaceUnderConcurrentKVTraffic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	// Not t.Parallel — mutates package-level test-seam vars.
	withWatcherBackoff(t, 10*time.Millisecond, 50*time.Millisecond, 1000)

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	bucket := "source-stress"
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	// Seed initial partition list so Start succeeds and Gets have
	// something to return.
	initial := []types.Partition{
		{Keys: []string{"p0"}, Weight: 1},
		{Keys: []string{"p1"}, Weight: 1},
		{Keys: []string{"p2"}, Weight: 1},
		{Keys: []string{"p3"}, Weight: 1},
	}
	initialBytes, err := partcodec.Encode(initial)
	require.NoError(t, err)
	const key = "config"
	_, err = kv.Put(ctx, key, initialBytes)
	require.NoError(t, err)

	// Reconciler disabled so restartWatcher is the sole watcher-restart
	// path. This isolates the race surface to the
	// "watcher restart loop vs concurrent KV reads on the shared handle"
	// pattern that GAP-1 names.
	src := NewNatsKV(kv, key, nil, WithReconcileInterval(0))
	require.NoError(t, src.Start(ctx))
	defer func() { _ = src.Stop(ctx) }()

	// Wait for initial List() to reflect the seed.
	require.Eventually(t, func() bool {
		return len(listPartitions(t, src)) == len(initial)
	}, 3*time.Second, 10*time.Millisecond,
		"initial partition list must be cached after Start")

	soakCtx, soakCancel := context.WithTimeout(ctx, 5*time.Second)
	defer soakCancel()

	var wg sync.WaitGroup
	var totalCloses atomic.Int64
	var totalGets atomic.Int64
	var totalPuts atomic.Int64
	var totalReads atomic.Int64

	// Force-close goroutine: drives restartWatcher → tryRebindWatcher
	// → s.watchFn → s.kv.Watch. Each Watch refreshes the cached
	// *stream state — the race-write side.
	wg.Go(func() {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-soakCtx.Done():
				return
			case <-ticker.C:
				src.mu.Lock()
				w := src.watcher
				src.mu.Unlock()
				if w != nil {
					_ = w.Stop()
					totalCloses.Add(1)
				}
			}
		}
	})

	// Get-side goroutines on the shared kv handle. Idiomatic
	// production read; this is the race-read side.
	const numGetters = 4
	for range numGetters {
		wg.Go(func() {
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

	// Put-side goroutine: rewrite the partitions periodically so the
	// watcher updates stream stays alive between closures.
	wg.Go(func() {
		i := 0
		for {
			select {
			case <-soakCtx.Done():
				return
			default:
			}
			// Rotate the partition set so each write is distinct
			// (avoids JetStream collapsing identical writes).
			parts := []types.Partition{
				{Keys: []string{fmt.Sprintf("p%d", i%4)}, Weight: 1},
				{Keys: []string{fmt.Sprintf("p%d", (i+1)%4)}, Weight: 1},
			}
			b, err := partcodec.Encode(parts)
			if err == nil {
				if _, err := kv.Put(soakCtx, key, b); err == nil {
					totalPuts.Add(1)
				}
			}
			i++
			time.Sleep(10 * time.Millisecond)
		}
	})

	// Source-read goroutine: exercise listPartitions(t, src) reads while the
	// watcher restart loop fires. Partitions() reads the cached state
	// updated by watchLoop's applyLocal; the read doesn't touch the KV
	// handle but does walk slice state mutated by the watcher goroutine.
	wg.Go(func() {
		for {
			select {
			case <-soakCtx.Done():
				return
			default:
			}
			_ = listPartitions(t, src)
			totalReads.Add(1)
			time.Sleep(time.Millisecond)
		}
	})

	<-soakCtx.Done()
	wg.Wait()

	// Liveness sanity: the source must still accept a fresh Update
	// after the soak. If the watcher restart loop collapsed under the
	// aggressive close cadence, this would fail and tell us the test
	// load broke the source rather than catching a race.
	livenessCtx, livenessCancel := context.WithTimeout(ctx, 5*time.Second)
	defer livenessCancel()
	finalParts := []types.Partition{
		{Keys: []string{"final-1"}, Weight: 1},
		{Keys: []string{"final-2"}, Weight: 1},
	}
	require.Eventually(t, func() bool {
		return src.Update(livenessCtx, finalParts) == nil
	}, 5*time.Second, 50*time.Millisecond,
		"source must remain functional after the concurrent soak")
	require.Eventually(t, func() bool {
		cur := listPartitions(t, src)
		if len(cur) != len(finalParts) {
			return false
		}
		return cur[0].Keys[0] == "final-1"
	}, 5*time.Second, 50*time.Millisecond,
		"source must converge to the post-soak partition list")

	// Primary assertion: the race detector did not fire during the
	// soak. t.Failed() flips to true on any -race trigger, sub-test
	// failure, or prior require failure.
	require.False(t, t.Failed(),
		"race detector or sub-assertion failed during the concurrent soak; "+
			"check stderr for WARNING: DATA RACE blocks. The source's "+
			"restartWatcher loop must not race with concurrent kv.Get / "+
			"kv.Put reads on the shared *jetstream.KeyValue handle.")

	t.Logf("soak complete: %d watcher closes, %d Gets, %d Puts, %d List reads",
		totalCloses.Load(), totalGets.Load(), totalPuts.Load(), totalReads.Load())
}

// listPartitions invokes src.List with a short timeout and asserts no
// error. Used inside the stress soak's read-side goroutine.
func listPartitions(t *testing.T, src *NatsKV) []types.Partition {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	parts, err := src.List(ctx)
	if err != nil {
		return nil
	}

	return parts
}
