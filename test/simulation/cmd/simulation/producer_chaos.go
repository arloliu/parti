// Phase 5 producer + assignment-publisher chaos primitives.
//
// Two new chaos actions live here:
//
//  1. handleProducerDisconnect — severs and restores the producer's own NATS
//     connection (NOT the shared manager/worker connection). Calls
//     Producer.Disconnect(d) which schedules Reconnect automatically and starts
//     the T_pub=5s watchdog for INV1.
//
//  2. handleAssignmentCASStorm — opens a FRESH JetStream context (dedicated
//     probe handle pattern, same as bucket_chaos.go) and repeatedly issues
//     stale-revision Update calls against the assignment._commit key for the
//     configured window. The competing writes race the production leader's
//     commit path; tolerates ErrKeyExists / wrong-sequence errors as expected.
//     INV2 is observed via SnapshotOverlapClassifier and the
//     assignment_cas_storm_unexpected_panic counter.

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/v2/test/simulation/internal/coordinator"
	"github.com/arloliu/parti/v2/test/simulation/internal/producer"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// casStormUnexpectedPanics is the INV2 safety counter: must remain 0 across
// all AssignmentCASStorm chaos windows. Any increment indicates the storm
// goroutine hit an unhandled panic — a coding error, not an expected KV
// error.
var casStormUnexpectedPanics atomic.Int64

// handleProducerDisconnect disconnects a random live producer's NATS
// connection for the given duration using its NetworkControl. The producer
// must have been started with a dedicated NATS conn and SetNetworkControl
// must have been called; if no qualifying producer is found, the event is
// logged and skipped (consistent with process-mode log-and-skip pattern).
func handleProducerDisconnect(registry *coordinator.GoroutineRegistry, duration time.Duration) {
	producers := registry.GetByType(coordinator.ProducerGoroutine)
	if len(producers) == 0 {
		log.Println("[Chaos] producer_disconnect: no active producers; skipping")
		return
	}

	// Pick the first producer with a NetworkControl wired.
	for _, info := range producers {
		p, ok := info.Obj.(*producer.Producer)
		if !ok {
			continue
		}
		log.Printf("[Chaos] producer_disconnect: disconnecting %s for %v (INV1 watchdog armed)", info.ID, duration)
		p.Disconnect(duration)
		return
	}
	log.Println("[Chaos] producer_disconnect: no producer with NetworkControl found; skipping")
}

// handleAssignmentCASStorm spawns a goroutine that repeatedly writes stale
// revisions against the assignment._commit key in the parti-assignment bucket
// for the given duration. Uses a dedicated probe JS context (freshJS()) so
// the storm cannot interact with cached manager KV state.
//
// Tolerated errors (expected and not bugs):
//   - jetstream.ErrKeyExists — Create lost; another writer holds the key.
//   - Wrong-sequence / stale-revision surface (nats.ErrTimeout,
//     ErrKeyWrongLastSequence, string "wrong last msg seq").
//   - jetstream.ErrKeyNotFound — commit key does not exist yet (leader
//     hasn't published its first commit; handled by switching to Create).
//   - nats.ErrNoResponders — bucket deleted concurrently (tolerated).
//   - context.Canceled / context.DeadlineExceeded — storm window expired.
//
// Only an unhandled panic increments casStormUnexpectedPanics.
func handleAssignmentCASStorm(ctx context.Context, duration time.Duration) {
	js, err := freshJS()
	if err != nil {
		log.Printf("[Chaos] assignment_cas_storm: failed to open probe JS: %v", err)
		return
	}

	log.Printf("[Chaos] assignment_cas_storm: starting competing _commit writer for %v", duration)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				casStormUnexpectedPanics.Add(1)
				log.Printf("[Chaos] assignment_cas_storm: UNEXPECTED PANIC: %v (INV2 violation)", r)
			}
		}()
		runCASStorm(ctx, js, duration)
	}()
}

// runCASStorm is the inner loop for the CAS storm. It is separated from the
// goroutine wrapper so the recover() in handleAssignmentCASStorm can catch any
// panic from this function.
func runCASStorm(ctx context.Context, js jetstream.JetStream, duration time.Duration) {
	const (
		bucket    = "parti-assignment"
		commitKey = "assignment._commit"
	)

	deadline := time.Now().Add(duration)
	stormCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	// Open KV handle on the probe context.
	kv, err := js.KeyValue(stormCtx, bucket)
	if err != nil {
		if isProbeBucketUnavailable(err) {
			log.Printf("[Chaos] assignment_cas_storm: bucket %q unavailable at storm start (tolerated): %v", bucket, err)
			return
		}
		log.Printf("[Chaos] assignment_cas_storm: UNEXPECTED KeyValue error: %v", err)
		return
	}

	staleValue := []byte(fmt.Sprintf("chaos-cas-storm-%d", time.Now().UnixNano()))
	iteration := 0

	for {
		select {
		case <-stormCtx.Done():
			log.Printf("[Chaos] assignment_cas_storm: storm window expired after %d iterations", iteration)
			return
		default:
		}

		iteration++

		// Read the current revision so we can attempt a stale-rev Update.
		getCtx, getCancel := context.WithTimeout(stormCtx, 2*time.Second)
		entry, gerr := kv.Get(getCtx, commitKey)
		getCancel()

		if gerr != nil {
			switch {
			case errors.Is(gerr, context.Canceled), errors.Is(gerr, context.DeadlineExceeded):
				return
			case errors.Is(gerr, jetstream.ErrKeyNotFound):
				// Commit key hasn't been written yet; attempt a Create with
				// the stale value so the leader's first commit can win via
				// its own Create (ErrKeyExists → tolerated).
				createCtx, createCancel := context.WithTimeout(stormCtx, 2*time.Second)
				_, cerr := kv.Create(createCtx, commitKey, staleValue)
				createCancel()
				if cerr != nil && !isCASRaceError(cerr) && !errors.Is(cerr, context.Canceled) && !errors.Is(cerr, context.DeadlineExceeded) {
					log.Printf("[Chaos] assignment_cas_storm: Create returned unexpected error: %v", cerr)
				}
			case isProbeBucketUnavailable(gerr):
				log.Printf("[Chaos] assignment_cas_storm: bucket unavailable during storm (tolerated): %v", gerr)
				time.Sleep(100 * time.Millisecond)
			default:
				log.Printf("[Chaos] assignment_cas_storm: Get returned unexpected error: %v", gerr)
			}

			time.Sleep(50 * time.Millisecond)

			continue
		}

		// Issue stale-revision Update: use rev-1 (or 1 when rev==1) so the
		// production leader's Update at the correct revision wins, and our
		// Update returns a "wrong last sequence" error.
		staleRev := entry.Revision()
		if staleRev > 1 {
			staleRev--
		}

		updateCtx, updateCancel := context.WithTimeout(stormCtx, 2*time.Second)
		_, uerr := kv.Update(updateCtx, commitKey, staleValue, staleRev)
		updateCancel()

		if uerr != nil && !isCASRaceError(uerr) && !errors.Is(uerr, context.Canceled) && !errors.Is(uerr, context.DeadlineExceeded) {
			log.Printf("[Chaos] assignment_cas_storm: Update returned unexpected error (iter=%d): %v", iteration, uerr)
		}

		// Brief pause to avoid spinning too hot and starving the event loop.
		time.Sleep(50 * time.Millisecond)
	}
}

// isCASRaceError classifies KV errors that are expected when two writers race
// on the same key revision. These are NOT bugs; they are the point of the
// CAS storm.
func isCASRaceError(err error) bool {
	if err == nil {
		return false
	}
	// nats.go surfaces CAS conflicts through multiple error paths depending
	// on server version and API surface.
	if errors.Is(err, jetstream.ErrKeyExists) ||
		errors.Is(err, nats.ErrNoResponders) ||
		errors.Is(err, jetstream.ErrKeyNotFound) ||
		errors.Is(err, jetstream.ErrBucketNotFound) ||
		errors.Is(err, jetstream.ErrStreamNotFound) {
		return true
	}
	// Wrong-sequence / stale-revision error; nats.go typically surfaces this
	// as a descriptive string wrapped in a generic error.
	s := err.Error()

	return containsAny(s, "wrong last msg", "wrong last sequence", "stream sequence mismatch")
}

// containsAny reports whether s contains any of the substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}

	return false
}
