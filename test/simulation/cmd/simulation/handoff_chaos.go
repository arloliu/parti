// Phase 6 / Gap 5 — Handoff-bucket MaxAge + orphan stable-claim primitives.
//
// Two chaos surfaces live here:
//
//  1. readHandoffBucketMaxAge + handoffBucketMaxAgeViolations — the live-
//     MaxAge oracle. After every worker reaches StateStable, the coordinator
//     opens a fresh JS handle, reads the handoff bucket's stream config via
//     the SAME surface the manager uses at manager_setup.go:kvStreamConfig
//     (kv.Status(ctx) → *jetstream.KeyValueBucketStatus → StreamInfo().Config),
//     and asserts MaxAge == 0. A non-zero value means
//     reconcileHandoffBucketMaxAge failed to heal an inherited TTL —
//     fatal, since stable ownership claims would silently expire and
//     permanently suppress pull-gated consumers.
//
//  2. handleHandoffOrphanClaimWrite + orphanStableClaimDrift — the stable-
//     claim sweep-skip oracle. A synthetic ClaimStateStable claim is written
//     to the handoff bucket with Owner not present in any AssignmentReport
//     and a non-colliding partition ID (chaos-orphan-<nano>). The primitive
//     then polls the same key over a window of ≥ 2 × Handoff.SweepInterval;
//     ANY change to the on-wire bytes (delete, owner reset, state change,
//     revision bump) increments orphanStableClaimDrift and fails the run.
//     This validates twophase.go:434-488 which unconditionally skips
//     ClaimStateStable claims during the sweeper's expired-claim reset path.
//
// The orphan claim is encoded using the EXPORTED parti.HandoffClaim type
// (same JSON shape as the internal Claim struct, verified by line-for-line
// field equivalence at manager_handoff.go:HandoffClaim ↔
// internal/assignment/handoff/claims.go:Claim). No internal types are
// imported.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/test/simulation/internal/coordinator"
	"github.com/nats-io/nats.go/jetstream"
)

// handoffBucketMaxAgeViolations is the Phase 6 / Gap 5 INV1 counter:
// must remain 0 after every worker reaches StateStable. Any increment
// indicates reconcileHandoffBucketMaxAge failed to heal an inherited
// MaxAge — stable ownership claims would silently expire.
var handoffBucketMaxAgeViolations atomic.Int64

// orphanStableClaimDrift is the Phase 6 / Gap 5 INV2 counter: must remain 0
// across the observation window. Any increment indicates the sweeper at
// twophase.go:434-488 did NOT honor the ClaimStateStable skip clause —
// orphan stable claims would be silently lost, breaking the handoff
// contract's "stable claims persist" invariant.
var orphanStableClaimDrift atomic.Int64

// orphanStableClaimSweepObserved is a best-effort observability counter:
// it increments each time the orphan-claim oracle confirms the claim is
// still present and well-formed during a poll cycle. A scenario that
// completes with this counter at 0 indicates the polling never fired
// (e.g. the chaos event was scheduled too late). The drift counter is
// the load-bearing invariant; this is diagnostic only.
var orphanStableClaimSweepObserved atomic.Int64

// readHandoffBucketMaxAge reads the live MaxAge from the handoff bucket
// using a FRESH JS handle, mirroring the exact API path the manager uses
// at manager_setup.go:kvStreamConfig (kv.Status → KeyValueBucketStatus →
// StreamInfo().Config.MaxAge). Returning the live value lets callers
// distinguish "healed to 0" from "inherited MaxAge survived".
func readHandoffBucketMaxAge(ctx context.Context, js jetstream.JetStream, bucket string) (time.Duration, error) {
	kv, err := js.KeyValue(ctx, bucket)
	if err != nil {
		return 0, fmt.Errorf("open KV %q: %w", bucket, err)
	}
	status, err := kv.Status(ctx)
	if err != nil {
		return 0, fmt.Errorf("status %q: %w", bucket, err)
	}
	bs, ok := status.(*jetstream.KeyValueBucketStatus)
	if !ok || bs.StreamInfo() == nil {
		return 0, fmt.Errorf("KV bucket status %T is not introspectable", status)
	}

	return bs.StreamInfo().Config.MaxAge, nil
}

// checkHandoffBucketMaxAgeOracle runs the Phase 6 / Gap 5 INV1 oracle.
// It is safe to call multiple times — only the first observed violation
// is recorded as a counter increment per worker-stable transition (the
// counter is monotonic and the invariant is binary).
func checkHandoffBucketMaxAgeOracle() {
	js, err := freshJS()
	if err != nil {
		log.Printf("[Phase6/INV1] could not open probe JS: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bucket := parti.DefaultConfig().KVBuckets.HandoffBucket
	live, rerr := readHandoffBucketMaxAge(ctx, js, bucket)
	if rerr != nil {
		log.Printf("[Phase6/INV1] could not read live MaxAge on %q: %v (tolerated; bucket may be transiently unavailable)", bucket, rerr)
		return
	}
	if live != 0 {
		handoffBucketMaxAgeViolations.Add(1)
		log.Printf("[Phase6/INV1] VIOLATION: handoff bucket %q live MaxAge=%v (expected 0); "+
			"reconcileHandoffBucketMaxAge did not heal an inherited TTL", bucket, live)
		return
	}
	log.Printf("[Phase6/INV1] handoff bucket %q MaxAge=0 (reconciled)", bucket)
}

// orphanClaimKey is the KV key (relative to the bucket root) where the
// orphan claim is written. The manager's claim store uses prefix
// "claims/" + SubjectKey() (internal/assignment/handoff/kv_store.go:75).
// We use a sentinel partition ID guaranteed not to collide with any
// real simulation partition (which are numeric strings 0..N-1).
const (
	orphanClaimPrefix       = "claims/"
	orphanClaimPartitionID  = "chaos-orphan-partition"
	orphanClaimOwner        = "chaos-orphan-owner-not-in-any-assignment"
	orphanClaimPollInterval = 2 * time.Second
)

func orphanClaimKey() string { return orphanClaimPrefix + orphanClaimPartitionID }

// handleHandoffOrphanClaimWrite writes a synthetic ClaimStateStable claim
// to the handoff bucket via a FRESH JS probe context and then polls the
// same key for the configured observation window. ANY change in the
// on-wire bytes (delete, owner reset, state change, revision bump beyond
// the initial Create) is recorded as drift.
//
// observationWindow defaults to 60s (= 2 × default 30s SweepInterval).
// The caller is expected to time the scenario so the window fits inside
// the simulation duration with margin.
func handleHandoffOrphanClaimWrite(ctx context.Context, observationWindow time.Duration) {
	if observationWindow <= 0 {
		observationWindow = 60 * time.Second
	}

	js, err := freshJS()
	if err != nil {
		log.Printf("[Phase6/INV2] handoff_orphan_claim_write: failed to open probe JS: %v", err)
		return
	}

	bucket := parti.DefaultConfig().KVBuckets.HandoffBucket

	openCtx, openCancel := context.WithTimeout(ctx, 5*time.Second)
	kv, err := js.KeyValue(openCtx, bucket)
	openCancel()
	if err != nil {
		log.Printf("[Phase6/INV2] handoff_orphan_claim_write: open KV %q failed: %v", bucket, err)
		return
	}

	// Construct an orphan stable claim. Owner is a sentinel string
	// guaranteed to NOT appear in any AssignmentReport (no worker
	// uses that ID). TTLSeconds is advisory only (claims.go:31): a
	// large value means IsExpired returns false during the window
	// even if the sweeper's expiry check were broken — but the
	// load-bearing skip clause is `cur.State == ClaimStateStable`.
	now := time.Now().UTC()
	claim := parti.HandoffClaim{
		PartitionID: orphanClaimPartitionID,
		Owner:       orphanClaimOwner,
		State:       parti.HandoffClaimStable,
		Epoch:       1,
		LastUpdated: now,
		TTLSeconds:  int64(time.Hour.Seconds()),
	}
	payload, merr := json.Marshal(claim)
	if merr != nil {
		log.Printf("[Phase6/INV2] handoff_orphan_claim_write: marshal failed: %v", merr)
		return
	}

	key := orphanClaimKey()
	writeCtx, writeCancel := context.WithTimeout(ctx, 5*time.Second)
	createdRev, cerr := kv.Create(writeCtx, key, payload)
	writeCancel()
	if cerr != nil {
		// If the key already exists (rare in a fresh scenario, but
		// possible if a previous run leaked state), fall back to Put
		// so the oracle still has a baseline.
		if !errors.Is(cerr, jetstream.ErrKeyExists) {
			log.Printf("[Phase6/INV2] handoff_orphan_claim_write: Create failed: %v", cerr)
			return
		}
		putCtx, putCancel := context.WithTimeout(ctx, 5*time.Second)
		rev, perr := kv.Put(putCtx, key, payload)
		putCancel()
		if perr != nil {
			log.Printf("[Phase6/INV2] handoff_orphan_claim_write: Put fallback failed: %v", perr)
			return
		}
		createdRev = rev
	}
	log.Printf("[Phase6/INV2] orphan stable claim written: key=%q rev=%d owner=%q state=%q (window=%v)",
		key, createdRev, orphanClaimOwner, parti.HandoffClaimStable, observationWindow)

	go orphanClaimDriftWatcher(ctx, key, payload, createdRev, observationWindow)
}

// orphanClaimDriftWatcher polls the orphan claim key over the observation
// window. Any change — value mutation, revision bump, deletion — is
// recorded as drift. The polling cadence is intentionally faster than
// SweepInterval so a single sweep can be observed.
func orphanClaimDriftWatcher(ctx context.Context, key string, want []byte, startRev uint64, window time.Duration) {
	js, err := freshJS()
	if err != nil {
		log.Printf("[Phase6/INV2] drift watcher: failed to open probe JS: %v", err)
		return
	}
	bucket := parti.DefaultConfig().KVBuckets.HandoffBucket
	openCtx, openCancel := context.WithTimeout(ctx, 5*time.Second)
	kv, err := js.KeyValue(openCtx, bucket)
	openCancel()
	if err != nil {
		log.Printf("[Phase6/INV2] drift watcher: open KV %q failed: %v", bucket, err)
		return
	}

	deadline := time.Now().Add(window)
	ticker := time.NewTicker(orphanClaimPollInterval)
	defer ticker.Stop()
	driftAlreadyRecorded := false

	for {
		select {
		case <-ctx.Done():
			log.Printf("[Phase6/INV2] drift watcher: context cancelled before window expired (drift=%d)",
				orphanStableClaimDrift.Load())
			return
		case <-ticker.C:
		}

		readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
		entry, gerr := kv.Get(readCtx, key)
		readCancel()
		if gerr != nil {
			if errors.Is(gerr, context.Canceled) || errors.Is(gerr, context.DeadlineExceeded) {
				return
			}
			// Deletion / bucket loss is drift — but bucket-loss could be
			// caused by an unrelated test (we don't expect that in the
			// Phase 6 scenarios). Either way, the stable-claim contract
			// is broken if the key disappears.
			if !driftAlreadyRecorded {
				orphanStableClaimDrift.Add(1)
				driftAlreadyRecorded = true
				log.Printf("[Phase6/INV2] DRIFT: orphan claim key %q became unreadable: %v "+
					"(sweeper or bucket failure; contract violated)", key, gerr)
			}
		} else {
			// Compare revision AND payload bytes. Either change ⇒ drift.
			if entry.Revision() != startRev || !bytesEqual(entry.Value(), want) {
				if !driftAlreadyRecorded {
					orphanStableClaimDrift.Add(1)
					driftAlreadyRecorded = true
					log.Printf("[Phase6/INV2] DRIFT: orphan claim key %q mutated: "+
						"rev start=%d now=%d, payload_changed=%v",
						key, startRev, entry.Revision(), !bytesEqual(entry.Value(), want))
				}
			} else {
				orphanStableClaimSweepObserved.Add(1)
			}
		}

		if time.Now().After(deadline) {
			log.Printf("[Phase6/INV2] drift watcher: window expired (drift=%d, polls_clean=%d)",
				orphanStableClaimDrift.Load(), orphanStableClaimSweepObserved.Load())
			return
		}
	}
}

// bytesEqual is a tiny helper to avoid importing bytes just for one Equal.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// startHandoffBucketMaxAgeOracle runs the Phase 6 / Gap 5 INV1 oracle once
// after the initial cohort of workers (or any subset registered in the
// goroutine registry) has reached StateStable. The oracle polls each
// worker's WorkerStateInt() and fires checkHandoffBucketMaxAgeOracle as
// soon as at least one worker reaches StateStable — that is sufficient,
// because reconcileHandoffBucketMaxAge runs during EACH manager's Start
// path, and any worker reaching Stable means at least one manager passed
// through (and survived) the reconciler.
//
// Called from runAllInOne after the worker spawn loop completes.
func startHandoffBucketMaxAgeOracle(ctx context.Context, registry *coordinator.GoroutineRegistry) {
	if registry == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		// Bound: at most ~60s of polling. If no worker reaches Stable in
		// that window the scenario is broken in some other way and the
		// oracle's silence is the smaller problem.
		deadline := time.Now().Add(60 * time.Second)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			for _, info := range registry.GetByType(coordinator.WorkerGoroutine) {
				obs, ok := info.Obj.(interface{ WorkerStateInt() int })
				if !ok {
					continue
				}
				if obs.WorkerStateInt() == int(parti.StateStable) {
					log.Printf("[Phase6/INV1] worker %s reached StateStable; firing MaxAge oracle", info.ID)
					checkHandoffBucketMaxAgeOracle()
					return
				}
			}
			if time.Now().After(deadline) {
				log.Print("[Phase6/INV1] no worker reached StateStable within 60s; oracle did not fire")
				return
			}
		}
	}()
}
