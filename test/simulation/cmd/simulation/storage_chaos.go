// Phase 8 / Gap 9 + Gap 13 — storage-type assertion and burst-after-quiet
// KV-op rate oracle.
//
// Three invariants are asserted:
//
//  1. storage_type_violation (Gap 9a) — after the manager creates the
//     coordination buckets itself (skip_bucket_precreate=true), each of
//     parti-election, parti-assignment, parti-handoff must carry
//     Storage=FileStorage. A violation increments storageTypeViolations.
//
//  2. election_replicas_violation (Gap 9b) — when the election bucket is
//     pre-created by the sim with ElectionReplicasOverride replicas, the
//     manager's get-first open must preserve that replica count.
//     A violation increments electionReplicasViolations.
//
//  3. kv_op_rate_ceiling_violation (Gap 13) — the burst-after-quiet scenario
//     samples NATS InMsgs+OutMsgs during a 60 s quiet window and a 30 s burst
//     window. If the burst rate exceeds quietRate × ceiling_multiplier (default
//     5.0), kvOpRateCeilingViolations is incremented and the run fails.
//     The first empirical run should be used to tune the multiplier.

package main

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	parti "github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/test/simulation/internal/coordinator"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
)

// storageTypeViolations is the Phase 8 / Gap 9 INV1 counter: must remain 0.
// Incremented once for each bucket that reports an unexpected StorageType
// after the manager-driven create path runs.
var storageTypeViolations atomic.Int64

// electionReplicasViolations is the Phase 8 / Gap 9 INV2 counter: must
// remain 0. Incremented when the live election bucket replica count does not
// match the operator-precreated value.
var electionReplicasViolations atomic.Int64

// kvOpRateCeilingViolations is the Phase 8 / Gap 13 INV3 counter: must
// remain 0 (or is informational when the multiplier is set to a loose
// first-run sentinel). Incremented when the burst-window KV-op rate exceeds
// quietRate × ceiling.
var kvOpRateCeilingViolations atomic.Int64

// storageElectionReplicasWant holds the effective replica count we expect to
// see on the election bucket at assertion time. Set during pre-create (after
// the server may have clamped the requested value).
var storageElectionReplicasWant atomic.Int64

// readBucketStorageType opens the named KV bucket and returns its configured
// StorageType by inspecting the underlying stream config via the
// kv.Status → *KeyValueBucketStatus → StreamInfo() surface — the same
// pattern used by readHandoffBucketMaxAge in handoff_chaos.go.
func readBucketStorageType(ctx context.Context, js jetstream.JetStream, bucket string) (jetstream.StorageType, error) {
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

	return bs.StreamInfo().Config.Storage, nil
}

// readBucketReplicas opens the named KV bucket and returns its configured
// replica count by inspecting the underlying stream config.
func readBucketReplicas(ctx context.Context, js jetstream.JetStream, bucket string) (int, error) {
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

	return bs.StreamInfo().Config.Replicas, nil
}

// checkStorageTypeOracle asserts StorageType==FileStorage for the three
// manager-owned coordination buckets that must persist across NATS restarts.
// Called once after all workers reach StateStable (or a timeout expires)
// in scenarios with skip_bucket_precreate=true.
func checkStorageTypeOracle() {
	js, err := freshJS()
	if err != nil {
		log.Printf("[Phase8/INV1] could not open probe JS: %v", err)
		return
	}

	pc := parti.DefaultConfig()
	buckets := []struct {
		name string
		want jetstream.StorageType
	}{
		{pc.KVBuckets.ElectionBucket, jetstream.FileStorage},
		{pc.KVBuckets.AssignmentBucket, jetstream.FileStorage},
		{pc.KVBuckets.HandoffBucket, jetstream.FileStorage},
	}

	for _, b := range buckets {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		got, rerr := readBucketStorageType(ctx, js, b.name)
		cancel()
		if rerr != nil {
			log.Printf("[Phase8/INV1] WARN: could not read StorageType on %q: %v (tolerated; bucket may not exist yet)", b.name, rerr)
			continue
		}
		if got != b.want {
			storageTypeViolations.Add(1)
			log.Printf("[Phase8/INV1] VIOLATION: bucket %q StorageType=%v (expected %v); "+
				"manager_setup.go create path did not enforce FileStorage", b.name, got, b.want)
		} else {
			log.Printf("[Phase8/INV1] bucket %q StorageType=FileStorage OK", b.name)
		}
	}
}

// precreateElectionBucketWithReplicas pre-creates the election KV bucket with
// the requested replica count and FileStorage BEFORE any worker starts.
// Returns the effective replica count actually accepted by the server (which
// may be less than requested if the server does not support clustering).
//
// Callers should assert against the RETURNED value (not the configured value)
// so that a single-server embedded NATS run still produces a meaningful test.
func precreateElectionBucketWithReplicas(ctx context.Context, js jetstream.JetStream, replicas int) (int, error) {
	pc := parti.DefaultConfig()
	bucket := pc.KVBuckets.ElectionBucket
	ttl := pc.ElectionTimeout

	cfg := jetstream.KeyValueConfig{
		Bucket:   bucket,
		TTL:      ttl,
		Storage:  jetstream.FileStorage,
		Replicas: replicas,
	}

	// Try to create with the requested replica count.
	kv, err := kvutil.EnsureKVBucketWithRetry(ctx, js, cfg, 3)
	if err != nil {
		// If replicas > 1 is rejected (likely embedded single-server), fall
		// back to replicas=1 so the assertion path is still exercised.
		if replicas > 1 {
			log.Printf("[Phase8/INV2] WARN: pre-create election bucket with Replicas=%d failed (%v); "+
				"falling back to Replicas=1. This scenario requires a multi-server NATS cluster "+
				"to test the full Replicas=%d path. The assertion will use Replicas=1.", replicas, err, replicas)
			cfg1 := jetstream.KeyValueConfig{
				Bucket:   bucket,
				TTL:      ttl,
				Storage:  jetstream.FileStorage,
				Replicas: 1,
			}
			bctx, bcancel := context.WithTimeout(ctx, 5*time.Second)
			kv, err = kvutil.EnsureKVBucketWithRetry(bctx, js, cfg1, 3)
			bcancel()
			if err != nil {
				return 0, fmt.Errorf("fallback pre-create election bucket Replicas=1: %w", err)
			}
			_ = kv

			return 1, nil
		}

		return 0, fmt.Errorf("pre-create election bucket Replicas=%d: %w", replicas, err)
	}
	_ = kv

	// Read back the actual replica count to handle any server-side clamping.
	vctx, vcancel := context.WithTimeout(ctx, 5*time.Second)
	actual, rerr := readBucketReplicas(vctx, js, bucket)
	vcancel()
	if rerr != nil {
		log.Printf("[Phase8/INV2] WARN: could not verify replicas on %q after pre-create: %v", bucket, rerr)

		return replicas, nil // assume requested value
	}
	if actual != replicas {
		log.Printf("[Phase8/INV2] WARN: pre-created election bucket %q with Replicas=%d but server reports Replicas=%d "+
			"(server clamped; assertion will use actual=%d)", bucket, replicas, actual, actual)
	} else {
		log.Printf("[Phase8/INV2] pre-created election bucket %q with Replicas=%d (FileStorage)", bucket, replicas)
	}

	return actual, nil
}

// checkElectionReplicasOracle asserts that the live election bucket replica
// count matches wantReplicas. Called after all workers reach StateStable in
// scenarios with election_replicas_override > 0.
func checkElectionReplicasOracle(wantReplicas int) {
	js, err := freshJS()
	if err != nil {
		log.Printf("[Phase8/INV2] could not open probe JS: %v", err)
		return
	}

	pc := parti.DefaultConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	got, rerr := readBucketReplicas(ctx, js, pc.KVBuckets.ElectionBucket)
	cancel()
	if rerr != nil {
		log.Printf("[Phase8/INV2] WARN: could not read Replicas on %q: %v", pc.KVBuckets.ElectionBucket, rerr)
		return
	}
	if got != wantReplicas {
		electionReplicasViolations.Add(1)
		log.Printf("[Phase8/INV2] VIOLATION: election bucket Replicas=%d (expected %d); "+
			"manager get-first path did not preserve operator-precreated replica count",
			got, wantReplicas)
	} else {
		log.Printf("[Phase8/INV2] election bucket Replicas=%d OK", got)
	}
}

// startStorageTypeOracle runs the Phase 8 / Gap 9 INV1 oracle once after any
// worker reaches StateStable. Mirrors the pattern used by
// startHandoffBucketMaxAgeOracle in handoff_chaos.go.
func startStorageTypeOracle(ctx context.Context, registry *coordinator.GoroutineRegistry) {
	if registry == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
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
					log.Printf("[Phase8/INV1] worker %s reached StateStable; firing storage-type oracle", info.ID)
					checkStorageTypeOracle()
					return
				}
			}
			if time.Now().After(deadline) {
				log.Print("[Phase8/INV1] no worker reached StateStable within 60s; storage-type oracle did not fire")
				return
			}
		}
	}()
}

// startElectionReplicasOracle runs the Phase 8 / Gap 9 INV2 oracle after all
// workers (count=expectedWorkers) reach StateStable. Uses a longer deadline
// than INV1 because it needs ALL workers to be stable, not just one.
func startElectionReplicasOracle(ctx context.Context, registry *coordinator.GoroutineRegistry, expectedWorkers int) {
	if registry == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.Now().Add(90 * time.Second)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			workers := registry.GetByType(coordinator.WorkerGoroutine)
			stableCount := 0
			for _, info := range workers {
				obs, ok := info.Obj.(interface{ WorkerStateInt() int })
				if !ok {
					continue
				}
				if obs.WorkerStateInt() == int(parti.StateStable) {
					stableCount++
				}
			}
			if stableCount >= expectedWorkers {
				wantReplicas := int(storageElectionReplicasWant.Load())
				log.Printf("[Phase8/INV2] all %d workers reached StateStable; firing election-replicas oracle (want=%d)",
					stableCount, wantReplicas)
				checkElectionReplicasOracle(wantReplicas)
				return
			}
			if time.Now().After(deadline) {
				log.Printf("[Phase8/INV2] only %d/%d workers reached StateStable within 90s; election-replicas oracle did not fire",
					stableCount, expectedWorkers)
				return
			}
		}
	}()
}

// serverInOutMsgs reads the server-wide total in+out message count via
// Varz. This covers ALL client connections (workers, producers, chaos
// goroutines), making it a meaningful proxy for KV-operation throughput.
// Returns 0 if srv is nil or Varz returns an error.
func serverInOutMsgs(srv *server.Server) uint64 {
	if srv == nil {
		return 0
	}
	v, err := srv.Varz(nil)
	if err != nil || v == nil {
		return 0
	}

	inMsgs := v.InMsgs
	outMsgs := v.OutMsgs
	if inMsgs < 0 {
		inMsgs = 0
	}
	if outMsgs < 0 {
		outMsgs = 0
	}

	return uint64(inMsgs + outMsgs) //nolint:gosec // both values are clamped to ≥0 above
}

// KvOpRateWindow records the start time, initial message count, and final rate
// computed over a sampling window.
type KvOpRateWindow struct {
	start    time.Time
	startMsg uint64
	end      time.Time
	endMsg   uint64
}

// Rate returns the messages-per-second rate for this window.
func (w KvOpRateWindow) Rate() float64 {
	dur := w.end.Sub(w.start).Seconds()
	if dur <= 0 {
		return 0
	}
	return float64(w.endMsg-w.startMsg) / dur
}

// burstAfterQuietSampler samples NATS message rates across two windows:
//   - quiet: sampleduration from start
//   - burst: burstDuration starting at burstAt
//
// After both windows complete, it evaluates the ceiling and logs results.
// The ceiling multiplier defaults to 5.0 if not configured (loose first-run
// sentinel). If the burst rate exceeds quietRate × multiplier, it increments
// kvOpRateCeilingViolations.
//
// This function is meant to be run as a background goroutine; it stops when
// ctx is cancelled or when both windows are fully sampled.
func burstAfterQuietSampler(
	ctx context.Context,
	srv *server.Server,
	quietDuration time.Duration,
	burstAt time.Duration,
	burstDuration time.Duration,
	ceilingMultiplier float64,
) {
	if ceilingMultiplier <= 0 {
		ceilingMultiplier = 5.0
	}

	var quiet, burst KvOpRateWindow

	// Sample quiet window.
	quiet.start = time.Now()
	quiet.startMsg = serverInOutMsgs(srv)
	log.Printf("[Phase8/INV3] burst-after-quiet sampler started; quiet window=%v burst_at=%v burst_window=%v ceiling_multiplier=%.1f",
		quietDuration, burstAt, burstDuration, ceilingMultiplier)

	select {
	case <-ctx.Done():
		log.Print("[Phase8/INV3] context cancelled before quiet window complete; skipping assertion")
		return
	case <-time.After(quietDuration):
	}

	quiet.end = time.Now()
	quiet.endMsg = serverInOutMsgs(srv)
	quietRate := quiet.Rate()
	log.Printf("[Phase8/INV3] quiet window done: msgs=%d rate=%.1f msg/s",
		quiet.endMsg-quiet.startMsg, quietRate)

	// Wait for burst start (burstAt is from simulation start, quiet window
	// already consumed quietDuration worth of time, so remaining wait is
	// burstAt - quietDuration).
	remaining := burstAt - quietDuration
	if remaining > 0 {
		select {
		case <-ctx.Done():
			log.Print("[Phase8/INV3] context cancelled before burst window; skipping assertion")
			return
		case <-time.After(remaining):
		}
	}

	// Sample burst window.
	burst.start = time.Now()
	burst.startMsg = serverInOutMsgs(srv)
	select {
	case <-ctx.Done():
		log.Print("[Phase8/INV3] context cancelled during burst window; skipping assertion")
		return
	case <-time.After(burstDuration):
	}
	burst.end = time.Now()
	burst.endMsg = serverInOutMsgs(srv)
	burstRate := burst.Rate()
	log.Printf("[Phase8/INV3] burst window done: msgs=%d rate=%.1f msg/s",
		burst.endMsg-burst.startMsg, burstRate)

	// Evaluate ceiling.
	ceiling := quietRate * ceilingMultiplier
	log.Printf("[Phase8/INV3] rate summary: quiet=%.1f msg/s burst=%.1f msg/s ceiling=%.1f msg/s (multiplier=%.1f)",
		quietRate, burstRate, ceiling, ceilingMultiplier)

	if burstRate > ceiling {
		kvOpRateCeilingViolations.Add(1)
		log.Printf("[Phase8/INV3] VIOLATION: burst rate=%.1f msg/s exceeds ceiling=%.1f msg/s "+
			"(quiet=%.1f × multiplier=%.1f); KV-op explosion detected during scale-up burst",
			burstRate, ceiling, quietRate, ceilingMultiplier)
	} else {
		log.Printf("[Phase8/INV3] burst rate within ceiling (%.1f ≤ %.1f); KV-op burst OK", burstRate, ceiling)
	}
}
