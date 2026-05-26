// Phase 4a stable-ID chaos primitives.
//
// The claim-steal primitive (revision-bumping kv.Put against the victim's
// claim key) is shared with Phase 1's bucket_peer_takeover and lives in
// bucket_chaos.go (handleBucketPeerTakeover). The Phase 4 alias
// StableIDClaimStealEvent dispatches there directly.
//
// This file adds the StableIDTinyPoolRespawnEvent primitive: spawn a fresh
// all-in-one worker into the dead worker's tiny stable-ID pool so its
// startup Claim hits the stale-takeover Update path
// (internal/stableid/claimer.go:219-243). The fresh worker must be
// scheduled strictly later than the staleThreshold = 3 * max(WorkerIDTTL/3,
// 100ms) plus scheduler margin; scheduling it inside TTL only returns
// ErrNoAvailableID (claimer.go:244-253).

package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/arloliu/parti/v2/test/simulation/internal/coordinator"
	"github.com/arloliu/parti/v2/test/simulation/internal/metrics"
	"github.com/arloliu/parti/v2/test/simulation/internal/worker"
)

// handleTargetedWorkerCrash cancels a SPECIFIC worker goroutine without
// triggering the auto-restart path. Phase 4 tiny-pool scenarios depend on
// this so the stale-takeover respawn can claim the dead worker's stable
// ID via Update (claimer.go:219-243). The default worker_crash handler
// always restarts, which would re-claim the same ID inside TTL and
// produce ErrNoAvailableID instead of exercising the stale-takeover path.
func handleTargetedWorkerCrash(registry *coordinator.GoroutineRegistry, targetWorker string, metricsCollector *metrics.Collector, checkpointMgr *coordinator.CheckpointManager) {
	workers := registry.GetByType(coordinator.WorkerGoroutine)
	for _, info := range workers {
		if info.ID != targetWorker {
			continue
		}
		log.Printf("[Chaos] Targeted crash (no auto-restart): %s", targetWorker)
		info.Cancel()
		registry.MarkInactive(info.ID)
		if aioCoord != nil {
			aioCoord.NotifyWorkerStopped(info.ID)
		}
		if metricsCollector != nil {
			metricsCollector.SetWorkersActive(registry.GetActiveCount(coordinator.WorkerGoroutine))
		}
		if checkpointMgr != nil {
			checkpointMgr.SetActiveWorkers(registry.GetActiveCount(coordinator.WorkerGoroutine))
		}

		return
	}
	log.Printf("[Chaos] Targeted crash: worker %s not in registry", targetWorker)
}

// applyStableIDEnv reads the four STABLEID_* env vars via the getenv
// function (parameterised so unit tests don't have to mutate the real
// environment) and sets the corresponding worker.Config fields when
// present. Empty / missing env vars leave the fields at zero so the
// override-if-set semantics in worker.NewWorker preserve the sim defaults.
//
// Parse failures return a non-nil error so the process exits loudly
// (Phase 4c risk note 924-928): silent fallback would mask scenario bugs.
func applyStableIDEnv(cfg *worker.Config, getenv func(string) string) error {
	if cfg == nil || getenv == nil {
		return nil
	}
	if v := getenv("STABLEID_PREFIX"); v != "" {
		cfg.WorkerIDPrefix = v
	}
	if v := getenv("STABLEID_MIN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("STABLEID_MIN=%q: %w", v, err)
		}
		cfg.WorkerIDMin = n
	}
	if v := getenv("STABLEID_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("STABLEID_MAX=%q: %w", v, err)
		}
		cfg.WorkerIDMax = n
	}
	if v := getenv("STABLEID_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("STABLEID_TTL=%q: %w", v, err)
		}
		cfg.WorkerIDTTL = d
	}

	return nil
}

// handleStableIDTinyPoolRespawn spawns a fresh all-in-one worker carrying
// the per-worker pool overrides recorded in the scenario config under the
// target role's sim-side worker ID. The new worker's runtime ID is
// allocated via nextWorkerIndex so it doesn't collide with the dead
// worker, but the spawnAllInOneWorker path reads PerWorker[<runtime ID>]
// — so we install the same overrides under the new ID transiently before
// spawning. Idempotent: caller schedules this AFTER the dead worker's
// claim is stale.
//
// Safe in all-in-one mode only. Process-mode is a log-and-skip until
// Phase 7b consumes Phase 4c's env-var transport.
func handleStableIDTinyPoolRespawn(ctx context.Context, registry *coordinator.GoroutineRegistry, role string) {
	if aioCfg == nil {
		log.Println("[Chaos] stableid_tiny_pool_respawn: aioCfg not initialized")
		return
	}
	if role == "" {
		log.Println("[Chaos] stableid_tiny_pool_respawn: target_role empty; need scenario-provided sim-side worker ID")
		return
	}
	roleOverride, ok := aioCfg.Workers.PerWorker[role]
	if !ok {
		log.Printf("[Chaos] stableid_tiny_pool_respawn: no per_worker override for role %q; skipping", role)
		return
	}

	newID := fmt.Sprintf("worker-%d", nextWorkerIndex(registry))
	// Install the same per-worker override under the new ID so the
	// spawnAllInOneWorker path picks them up. Mutate aioCfg in place; this
	// is the dispatcher goroutine so no concurrent reads of the PerWorker
	// map happen.
	if aioCfg.Workers.PerWorker == nil {
		log.Println("[Chaos] stableid_tiny_pool_respawn: PerWorker map nil; cannot install respawn override")
		return
	}
	aioCfg.Workers.PerWorker[newID] = roleOverride
	log.Printf("[Chaos] stableid_tiny_pool_respawn: spawning %s with overrides=%+v at t=%v", newID, roleOverride, time.Now().Format(time.RFC3339Nano))

	if !spawnAllInOneWorker(ctx, newID) {
		log.Printf("[Chaos] stableid_tiny_pool_respawn: spawn of %s failed", newID)
		// Clean up the transient override to avoid a stale entry.
		delete(aioCfg.Workers.PerWorker, newID)
	}
}
