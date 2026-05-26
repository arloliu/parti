// Phase 2 source-convergence driver.
//
// Periodically mutates the SINGLE `partitions` key in the parti-sim-source
// bucket via a transient source.NatsKV instance (codec parity with
// production), registers a source-convergence expectation against the
// coordinator's oracle, and waits for every worker's AssignmentReport to
// reflect the mutated partition set within T_converge.
//
// T_converge = max(reconcileInterval × 3, 30s). With the worker's
// SourceReconcileInterval=5s, this resolves to 30s.
//
// The driver is intentionally simple: alternate between removing and
// re-adding the highest partition ID. The "expected" set is the post-
// mutation full partition list, and "forbidden" is the partition that
// was removed (if any). This exercises BOTH the additive and subtractive
// reconciler paths.

package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
)

// startSourceConvergenceDriver launches the periodic mutate-and-assert
// loop. Idempotent: only runs when PartitionSource is "nats_kv" and the
// coordinator's source-convergence oracle is non-nil.
//
// Parameters:
//   - ctx: simulation root context.
//   - bucket: source bucket name (e.g., "parti-sim-source").
//   - basePartitions: full partition list as a slice of partition IDs in
//     [0, len). The driver toggles the highest ID off/on each mutation.
//   - period: interval between mutations (e.g., 60s; the first fires at
//     period after the driver starts).
//   - tConverge: convergence deadline for each expectation.
func startSourceConvergenceDriver(
	ctx context.Context,
	bucket string,
	basePartitions []types.Partition,
	period time.Duration,
	tConverge time.Duration,
) {
	if aioCoord == nil || aioCoord.SourceConvergenceOracle() == nil {
		log.Print("[SourceConvergence] driver skipped: oracle not enabled")
		return
	}
	if len(basePartitions) < 2 {
		log.Printf("[SourceConvergence] driver skipped: need >=2 partitions, got %d", len(basePartitions))
		return
	}

	go func() {
		// Wait for the cluster to reach a stable baseline before starting
		// mutations. The producer warmup sleep is 500ms; allow several
		// reconcile cycles for initial assignment to settle.
		select {
		case <-ctx.Done():
			return
		case <-time.After(period):
		}

		js, err := freshJS()
		if err != nil {
			log.Printf("[SourceConvergence] failed to open probe JS: %v", err)
			return
		}
		kv, kerr := kvutil.EnsureKVBucket(ctx, js, bucket, 0)
		if kerr != nil {
			log.Printf("[SourceConvergence] failed to ensure source bucket %q: %v", bucket, kerr)
			return
		}
		// Build a transient NatsKV instance for codec-parity Update calls.
		// We do NOT call Start (which would launch its own watcher loop);
		// Update is safe without Start because the unknown-revision path
		// at source/nats_kv.go:579 issues a Create on the first call and
		// transitions to update-with-rev internally.
		transient := source.NewNatsKV(kv, "partitions", nil)

		ticker := time.NewTicker(period)
		defer ticker.Stop()
		toggled := false
		mutationCount := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				mutationCount++
				newSet, expectedIDs, forbiddenIDs := nextMutation(basePartitions, toggled)
				label := fmt.Sprintf("source_convergence_mutation_%d", mutationCount)
				log.Printf("[SourceConvergence] mutation #%d: toggled=%v expected=%d forbidden=%d", mutationCount, toggled, len(expectedIDs), len(forbiddenIDs))
				// Register expectation BEFORE the Update so no snapshot
				// can race ahead of the active window.
				if o := aioCoord.SourceConvergenceOracle(); o != nil {
					// Watcher_stall is the chaos primitive that races
					// with this driver. INV2 says no spurious
					// source-unavailable during a clean stall — the
					// expectation disallows source-unavailable. INV3
					// (composed bucket_delete) explicitly expects it,
					// but the driver does not register expectations
					// in that scenario (handled by the bucket_delete
					// expectation registration path).
					o.ExpectAfter(label, expectedIDs, forbiddenIDs, tConverge, false /* disallow source-unavailable */)
				}
				upCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				if uerr := transient.Update(upCtx, newSet); uerr != nil {
					log.Printf("[SourceConvergence] Update failed (mutation=%d): %v", mutationCount, uerr)
				}
				cancel()
				toggled = !toggled
			}
		}
	}()
	log.Printf("[SourceConvergence] driver started (bucket=%s period=%v t_converge=%v)", bucket, period, tConverge)
}

// nextMutation produces the next partition list. When toggled=false, the
// HIGHEST partition is REMOVED; when toggled=true, all partitions are
// restored. Returns:
//   - newSet: the new full partition list to push to KV.
//   - expectedIDs: integer keys expected to appear in some worker's
//     assignment after convergence.
//   - forbiddenIDs: integer keys that must NOT appear (only set when a
//     partition was removed; empty when restoring).
func nextMutation(base []types.Partition, toggled bool) (newSet []types.Partition, expectedIDs, forbiddenIDs []int) {
	if !toggled {
		// Drop the highest partition.
		newSet = append(newSet, base[:len(base)-1]...)
		highestKey := base[len(base)-1].Keys[0]
		expectedIDs = idsOfPartitions(newSet)
		var hid int
		_, _ = fmt.Sscanf(highestKey, "%d", &hid)
		forbiddenIDs = []int{hid}

		return newSet, expectedIDs, forbiddenIDs
	}
	// Restore: full set.
	newSet = append(newSet, base...)
	expectedIDs = idsOfPartitions(newSet)

	return newSet, expectedIDs, nil
}

func idsOfPartitions(ps []types.Partition) []int {
	out := make([]int, 0, len(ps))
	for _, p := range ps {
		if len(p.Keys) == 0 {
			continue
		}
		var id int
		_, err := fmt.Sscanf(p.Keys[0], "%d", &id)
		if err == nil {
			out = append(out, id)
		}
	}

	return out
}
