// Phase 1 bucket-lifecycle chaos primitives.
//
// Each primitive opens a FRESH JetStream context against the
// already-connected NATS conn (NOT the manager's connection). This mirrors
// the production probe pattern at manager_setup.go:613 (captureBucketEpoch
// opens a dedicated KV handle for the epoch fence), so the chaos action
// cannot inadvertently interact with cached manager state.
//
// Tolerated-error contracts per primitive are documented inline. The
// primitives fail the run ONLY on UNEXPECTED errors; tolerated errors
// indicate the production classifier path under test is working.
//
// Process-mode dispatch is intentionally a log-and-skip per the plan's
// risk note (lines 386-388). Whole-bucket actions in process mode would
// need cross-process visibility into the worker JetStream contexts which
// the simulation does not currently provide.

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/test/simulation/internal/coordinator"
	"github.com/arloliu/parti/v2/test/simulation/internal/worker"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// freshJS opens a fresh JetStream context against aioNS.
//
// This is the "probe context" pattern: each chaos action gets a brand-new
// jetstream.JetStream so it cannot share cached *kvs or *stream state with
// the manager goroutines. nats.go's KV / stream objects are NOT documented
// as safe for concurrent reuse across goroutines.
func freshJS() (jetstream.JetStream, error) {
	if aioNS == nil {
		return nil, errors.New("aioNS not initialized")
	}
	return jetstream.New(aioNS)
}

// isSimSourceBucket reports whether the bucket name corresponds to the
// Phase 2 simulation partition-source bucket. Hard-coded to the default
// to avoid threading the worker config through every chaos handler.
func isSimSourceBucket(bucket string) bool {
	return bucket == "parti-sim-source"
}

// bucketTTL returns the TTL to use when re-creating a bucket. Mirrors the
// pre-create table at main.go:233-242 so the recreate restores the same
// bucket configuration the cluster was using.
func bucketTTL(bucket string) time.Duration {
	pc := parti.DefaultConfig()
	switch bucket {
	case pc.KVBuckets.StableIDBucket:
		return pc.WorkerIDTTL
	case pc.KVBuckets.ElectionBucket:
		return pc.ElectionTimeout
	case pc.KVBuckets.HeartbeatBucket:
		return pc.HeartbeatTTL
	case pc.KVBuckets.AssignmentBucket:
		return pc.KVBuckets.AssignmentTTL
	default:
		// HandoffBucket and any unknown bucket get TTL=0 (matches the
		// sim's overridden HandoffTTL at main.go:231).
		return 0
	}
}

// isBucketAlreadyMissing classifies an error from js.DeleteKeyValue against
// a bucket that may have already been removed. Tolerated values per plan
// lines 292-296:
//   - nats.ErrStreamNotFound (legacy nats.JetStreamContext surface; the
//     jetstream.* API typically maps the same condition to
//     jetstream.ErrStreamNotFound).
//   - jetstream.ErrStreamNotFound (modern API).
//   - nats.ErrNoResponders (server racing with the delete).
//   - substring "stream not found" (some server surfaces a plain text
//     error; mirrored in embedded.go:108-116).
func isBucketAlreadyMissing(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, nats.ErrStreamNotFound) ||
		errors.Is(err, jetstream.ErrStreamNotFound) ||
		errors.Is(err, jetstream.ErrBucketNotFound) ||
		errors.Is(err, nats.ErrNoResponders) {
		return true
	}

	return strings.Contains(err.Error(), "stream not found")
}

// isProbeBucketUnavailable classifies an error from a fresh kv.Get / kv.Status
// after the bucket has been deleted. The set mirrors the production
// classifier at source/nats_kv.go:1217-1220 plus the connectivity sentinel
// jetstream.ErrNoStreamResponse (consumed by stableid/claimer.go:369-371).
func isProbeBucketUnavailable(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, jetstream.ErrBucketNotFound) ||
		errors.Is(err, jetstream.ErrStreamNotFound) ||
		errors.Is(err, jetstream.ErrNoStreamResponse) ||
		errors.Is(err, nats.ErrNoResponders)
}

// handleBucketDelete deletes a single Parti-owned KV bucket via a fresh
// probe JS context, verifies the deletion via a fresh kv.Get (tolerating
// the documented post-delete error surface), and registers the expected
// degraded-reason expectation on the coordinator's oracle.
//
// Invariant INV1 (plan lines 336-344): after delete on parti-stableid,
// every running worker emits a WorkerDegradedReport with Reason matching
// "bucket-unavailable:" OR "bucket-recreated:parti-stableid" within
// 3 × OperationTimeout. No worker should reach Shutdown via
// claimLostShutdown in this window.
func handleBucketDelete(ctx context.Context, bucket string) {
	js, err := freshJS()
	if err != nil {
		log.Printf("[Chaos] bucket_delete: failed to open probe JS: %v", err)
		return
	}

	// Register the degraded-reason expectation BEFORE issuing the delete so
	// no observation can race ahead of the active window.
	if aioCoord != nil {
		if o := aioCoord.DegradedReasonOracle(); o != nil {
			pc := parti.DefaultConfig()
			window := 2 * pc.WorkerIDTTL
			var subs []string
			switch {
			case isSimSourceBucket(bucket):
				// Phase 2 INV3: deleting the partition-source bucket does
				// NOT route through manager.OnDegraded (the production
				// SourceUnavailableHook is independent — see
				// source/nats_kv.go:123-148). The sim's worker-side
				// adapter (worker.go nats_kv branch) translates the hook
				// into a synthetic WorkerDegradedReport with reason
				// "source-unavailable:<bucket>". That's the only reason
				// we expect; no recordKVError / bucket-recreated path
				// applies.
				subs = []string{"source-unavailable:" + bucket}
			default:
				// Production reasons (see manager_degraded.go:144 and
				// manager_setup.go:687):
				//   - "KV error threshold exceeded" — recordKVError path
				//     after KVErrorThreshold connectivity/degrading-JS errors
				//     accumulate in KVErrorWindow.
				//   - "bucket-recreated:<bucket>" — monitorBucketEpochs
				//     when the bucket is wiped and a peer re-ensures it
				//     before this worker's renew misses the threshold.
				subs = []string{"KV error threshold exceeded", "bucket-recreated:" + bucket}
			}
			o.ExpectAfter("bucket_delete:"+bucket, subs, window, "", false)
		}
	}

	delCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := js.DeleteKeyValue(delCtx, bucket); err != nil {
		if !isBucketAlreadyMissing(err) {
			log.Printf("[Chaos] bucket_delete: UNEXPECTED error deleting %q: %v", bucket, err)
			return
		}
		log.Printf("[Chaos] bucket_delete: bucket %q already missing (tolerated): %v", bucket, err)
	}
	log.Printf("[Chaos] bucket_delete: deleted %q", bucket)

	// Verify with a fresh KV lookup — exercises the production-classifier
	// error surface end-to-end.
	probeCtx, probeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer probeCancel()
	kv, kerr := js.KeyValue(probeCtx, bucket)
	switch {
	case kerr == nil:
		// Bucket re-ensured by a fast worker; the get below will tell us
		// the live status.
		if _, gerr := kv.Get(probeCtx, "__chaos-probe__"); gerr != nil && !isProbeBucketUnavailable(gerr) && !errors.Is(gerr, jetstream.ErrKeyNotFound) {
			log.Printf("[Chaos] bucket_delete: probe Get returned UNEXPECTED error: %v", gerr)
		}
	case isProbeBucketUnavailable(kerr):
		log.Printf("[Chaos] bucket_delete: probe KeyValue confirmed bucket gone (tolerated): %v", kerr)
	default:
		log.Printf("[Chaos] bucket_delete: probe KeyValue returned UNEXPECTED error: %v", kerr)
	}
}

// handleBucketRecreate deletes a bucket then immediately re-creates it via
// kvutil.EnsureKVBucket. This is the "epoch fence" trigger: the backing
// stream's Created timestamp will be fresh, and monitorBucketEpochs (one
// tick = OperationTimeout) will compare it to the cached value and call
// enterDegraded("bucket-recreated:<bucket>").
//
// Invariant INV3 (plan lines 354-359): after recreate of parti-assignment,
// at least one worker enters degraded with Reason exactly
// "bucket-recreated:parti-assignment" within 2 × OperationTimeout.
func handleBucketRecreate(ctx context.Context, bucket string) {
	js, err := freshJS()
	if err != nil {
		log.Printf("[Chaos] bucket_recreate: failed to open probe JS: %v", err)
		return
	}

	// Capture pre-delete Created timestamp for assertion in the log.
	var preCreated time.Time
	if kv, kerr := js.KeyValue(ctx, bucket); kerr == nil {
		if t, terr := kvutil.BucketStreamCreated(ctx, kv); terr == nil {
			preCreated = t
		}
	}

	// Register the expectation BEFORE deletion so it cannot race.
	if aioCoord != nil {
		if o := aioCoord.DegradedReasonOracle(); o != nil {
			pc := parti.DefaultConfig()
			// Plan INV3 specified 2 × OperationTimeout (=20s), which is
			// the epoch-fence ticker cadence. That suffices to OBSERVE
			// a single transition, but if the scenario fires multiple
			// chaos events the OnDegraded hook only re-fires on
			// transitions out-of and back-into degraded. To keep this
			// oracle stable under chaos repetition, the window is
			// widened to 2 × WorkerIDTTL so the idempotent
			// re-registration covers the gap between transitions.
			window := 2 * pc.WorkerIDTTL
			subs := []string{"bucket-recreated:" + bucket}
			o.ExpectAfter("bucket_recreate:"+bucket, subs, window, "", false)
		}
	}

	delCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if derr := js.DeleteKeyValue(delCtx, bucket); derr != nil && !isBucketAlreadyMissing(derr) {
		log.Printf("[Chaos] bucket_recreate: UNEXPECTED error deleting %q: %v", bucket, derr)
		return
	}

	ensureCtx, ensureCancel := context.WithTimeout(ctx, 5*time.Second)
	defer ensureCancel()
	if _, eerr := kvutil.EnsureKVBucket(ensureCtx, js, bucket, bucketTTL(bucket)); eerr != nil {
		log.Printf("[Chaos] bucket_recreate: UNEXPECTED error ensuring %q: %v", bucket, eerr)
		return
	}

	// Verify the Created timestamp changed.
	if kv, kerr := js.KeyValue(ctx, bucket); kerr == nil {
		if postCreated, terr := kvutil.BucketStreamCreated(ctx, kv); terr == nil {
			if !preCreated.IsZero() && postCreated.Equal(preCreated) {
				log.Printf("[Chaos] bucket_recreate: WARNING Created timestamp did not change for %q (pre=%v post=%v)", bucket, preCreated, postCreated)
			} else {
				log.Printf("[Chaos] bucket_recreate: recreated %q pre=%v post=%v", bucket, preCreated, postCreated)
			}
		}
	}
}

// handleBucketPeerTakeover steals a specific worker's StableID claim key
// by Putting a fresh value at the worker's claim key. Put is REVISION-
// BUMPING (unlike Update which is revision-checked), and the next renew
// (claimer.go:362-369) maps the resulting ErrKeyExists to ErrClaimLost,
// which onClaimerError (manager_election.go:106-122) routes to
// claimLostShutdown — but ONLY when natsutil.IsConnectivityError and
// natsutil.IsDegradingJetStreamError both return false. Since the bucket
// itself is healthy here, both predicates return false and the takeover
// branch fires.
//
// Invariant INV2 (plan lines 345-353): EXACTLY worker W's manager reaches
// State() == Shutdown within WorkerIDTTL × 2.
func handleBucketPeerTakeover(ctx context.Context, registry *coordinator.GoroutineRegistry, targetWorker string) {
	js, err := freshJS()
	if err != nil {
		log.Printf("[Chaos] bucket_peer_takeover: failed to open probe JS: %v", err)
		return
	}

	pc := parti.DefaultConfig()
	bucket := pc.KVBuckets.StableIDBucket

	// Pick a victim worker: either explicit targetWorker or random live worker.
	workers := registry.GetByType(coordinator.WorkerGoroutine)
	if len(workers) == 0 {
		log.Println("[Chaos] bucket_peer_takeover: no live workers")
		return
	}
	var stableID string
	if targetWorker == "" || targetWorker == "random" {
		// Pick the first live worker with a claimed stable ID.
		for _, info := range workers {
			if wobj, ok := info.Obj.(*worker.Worker); ok {
				if id := wobj.StableWorkerID(); id != "" {
					stableID = id
					break
				}
			}
		}
	} else {
		// Resolve targetWorker (registry ID, e.g. "worker-0") to its
		// stable worker ID (e.g. "simulation-worker-7").
		for _, info := range workers {
			if info.ID != targetWorker {
				continue
			}
			if wobj, ok := info.Obj.(*worker.Worker); ok {
				stableID = wobj.StableWorkerID()
			}
			break
		}
	}
	if stableID == "" {
		log.Printf("[Chaos] bucket_peer_takeover: could not resolve stable worker ID (targetWorker=%q)", targetWorker)
		return
	}

	// Register the expectation BEFORE the Put.
	if aioCoord != nil {
		if o := aioCoord.DegradedReasonOracle(); o != nil {
			// WorkerIDTTL × 2 window per plan; we look for the
			// claim-lost shutdown (not a degraded reason) — the
			// oracle's ObserveClaimLostShutdown path consumes this.
			window := 2 * pc.WorkerIDTTL
			// No reason substrings because the peer-takeover path
			// does NOT enter degraded; it goes directly to
			// claimLostShutdown. The oracle keeps the expectation
			// alive for the ObserveClaimLostShutdown match.
			o.ExpectAfter("bucket_peer_takeover:"+stableID, nil, window, stableID, true)
		}
	}

	kv, kerr := js.KeyValue(ctx, bucket)
	if kerr != nil {
		// Tolerate ONLY when the bucket itself is concurrently being
		// recreated (documented composition with bucket_recreate of
		// parti-stableid). All other errors are unexpected.
		if isProbeBucketUnavailable(kerr) {
			log.Printf("[Chaos] bucket_peer_takeover: bucket %q unavailable (tolerated; concurrent recreate?): %v", bucket, kerr)
			return
		}
		log.Printf("[Chaos] bucket_peer_takeover: UNEXPECTED KeyValue error: %v", kerr)
		return
	}

	value := fmt.Sprintf("chaos-takeover-%d", time.Now().UnixNano())
	putCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Put always bumps revision unconditionally — the victim's next
	// Update(...lastRevision) will fail with ErrKeyExists →
	// stableid.ErrClaimLost → claimLostShutdown.
	if _, perr := kv.Put(putCtx, stableID, []byte(value)); perr != nil {
		if errors.Is(perr, nats.ErrNoResponders) {
			log.Printf("[Chaos] bucket_peer_takeover: ErrNoResponders (tolerated): %v", perr)
			return
		}
		log.Printf("[Chaos] bucket_peer_takeover: UNEXPECTED Put error: %v", perr)
		return
	}
	log.Printf("[Chaos] bucket_peer_takeover: stole claim key %q (worker stable_id=%s)", stableID, stableID)
}
