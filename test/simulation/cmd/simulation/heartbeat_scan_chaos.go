// Heartbeat scan-flatness oracle (FM7 / FM3).
//
// The sole purpose of the leader WorkerMonitor's heartbeat-refresh suppression
// (internal/assignment/worker_monitor.go) is to cut the leader's Keys() scan
// rate on parti-heartbeat: without it, every routine heartbeat refresh (each
// worker PUTs its key every HeartbeatInterval) forces a full Keys() scan, and
// each Keys() spins up a throwaway ordered consumer. A regression that keeps
// the suppression holiday open, forces a check per watcher event, or otherwise
// re-scans on every refresh is CORRECTNESS-INVISIBLE — reassignment still
// works, ownership/gap oracles stay green — yet it silently deletes the whole
// benefit of the feature.
//
// This oracle makes that regression observable. A sampler snapshots the
// cumulative parti-heartbeat scan count (Keys + ListKeys, counted in the KV
// fault seam, kv_fault_chaos.go) at four phase boundaries; the final gate then
// computes the scan count over two quiet windows and fails the run if either
// exceeds a documented floor budget derived from the polling cadence:
//
//	Poll cadence   = HeartbeatTTL / 2 = 15s / 2 = 7.5s   (worker_monitor.go:307)
//	Scans per poll = 1  (pollForChanges -> observeAndDecide -> GetActiveWorkers
//	                     -> one Keys() scan; calculator.go)
//	Only the leader runs the monitor (Calculator.Start is leader-only), so the
//	across-all-workers sum equals the single current leader's count in a quiet
//	window.
//	Polling-only lower bound = 30 / 7.5 = 4 scans per 30s window
//	Measured quiet floor     = 6 scans per 30s window (polling 4 + ~2 residual
//	                           from watcher-session initial-replay reclassify)
//	FloorBudget = measured_floor × 2 (CI headroom) — configured in the YAML.
//
// The suppressed path (heartbeat refreshes) contributes ~0 scans in a warm
// quiet window, so the sabotage — force a scan per watcher event, or keep the
// holiday permanently re-stamped — lifts the window count to
// ~workers/HeartbeatInterval × 30s (dozens of scans), far above the budget.

package main

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	parti "github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/test/simulation/internal/config"
)

// Phase-boundary snapshots of the cumulative parti-heartbeat scan count and the
// count of snapshots that actually fired (expected hbScanExpectedSnaps).
// hbScanFloorViolations is the fatal counter surfaced by the final gate.
var (
	hbScanPhaseAStartSnap atomic.Int64
	hbScanPhaseAEndSnap   atomic.Int64
	hbScanPhaseCStartSnap atomic.Int64
	hbScanPhaseCEndSnap   atomic.Int64
	hbScanSnapsFired      atomic.Int64
	hbScanFloorViolations atomic.Int64
)

// hbScanExpectedSnaps is the number of phase-boundary snapshots the sampler
// must capture (phaseA start/end, phaseC start/end) before the floor check is
// conclusive.
const hbScanExpectedSnaps = 4

// heartbeatScanBucket resolves the heartbeat bucket name from the parti
// defaults the sim workers use (they construct parti.DefaultConfig()), so the
// oracle tracks the exact bucket the leader WorkerMonitor scans.
func heartbeatScanBucket() string {
	return parti.DefaultConfig().KVBuckets.HeartbeatBucket
}

// startHeartbeatScanFlatnessOracle arms the phase-boundary sampler when the
// scenario enables it. Snapshots are one-shot goroutines fired at fixed offsets
// from simulation start (mirroring scheduled_events), all well before shutdown.
func startHeartbeatScanFlatnessOracle(ctx context.Context, fc *simKVFaultController, cfg *config.Config) {
	hs := cfg.Chaos.HeartbeatScan
	if !hs.Enabled || fc == nil {
		return
	}

	bucket := heartbeatScanBucket()
	log.Printf("[HBScan] flatness oracle armed: bucket=%s floor_budget=%d phaseA_tail=[+%v,+%v] phaseC_tail=[+%v,+%v]",
		bucket, hs.FloorBudget, hs.PhaseATailStart, hs.PhaseATailEnd, hs.PhaseCTailStart, hs.PhaseCTailEnd)

	snapshot := func(at time.Duration, dst *atomic.Int64, label string) {
		if at <= 0 {
			return
		}
		time.AfterFunc(at, func() {
			select {
			case <-ctx.Done():
				return
			default:
			}
			v := fc.scanCount(bucket)
			dst.Store(v)
			hbScanSnapsFired.Add(1)
			log.Printf("[HBScan] snapshot %s at +%v: cumulative heartbeat scans=%d", label, at, v)
		})
	}

	snapshot(hs.PhaseATailStart, &hbScanPhaseAStartSnap, "phaseA_tail_start")
	snapshot(hs.PhaseATailEnd, &hbScanPhaseAEndSnap, "phaseA_tail_end")
	snapshot(hs.PhaseCTailStart, &hbScanPhaseCStartSnap, "phaseC_tail_start")
	snapshot(hs.PhaseCTailEnd, &hbScanPhaseCEndSnap, "phaseC_tail_end")
}

// checkHeartbeatScanFloor is the final-gate assertion for the scan-flatness
// scenario. It is a self-contained return path (mirroring the unresolved-gaps
// gate at main.go) rather than a new disjunct in the invariants OR: that block
// is duplicated and any new counter must be threaded through BOTH copies plus
// three fmt strings, so a standalone gate is materially less error-prone.
//
// It computes two quiet-window scan deltas from the sampler snapshots:
//
//	phaseA_tail = scans@phaseA_tail_end - scans@phaseA_tail_start
//	phaseC_tail = scans@phaseC_tail_end - scans@phaseC_tail_start
//
// Both windows close at fixed pre-shutdown offsets (phaseC_tail_end lands a few
// seconds before duration) so ordered-shutdown teardown scans never leak in. It
// fails if either window exceeds FloorBudget, or if the sampler did not capture
// all four snapshots (an aborted run must not silently pass).
func checkHeartbeatScanFloor(fc *simKVFaultController, cfg *config.Config) error {
	hs := cfg.Chaos.HeartbeatScan
	if !hs.Enabled || fc == nil {
		return nil
	}

	bucket := heartbeatScanBucket()
	finalCumulative := fc.scanCount(bucket)
	phaseATail := hbScanPhaseAEndSnap.Load() - hbScanPhaseAStartSnap.Load()
	phaseCTail := hbScanPhaseCEndSnap.Load() - hbScanPhaseCStartSnap.Load()

	log.Printf("[HBScan] heartbeat scan floor check: phaseA_tail=%d phaseC_tail=%d floor_budget=%d final_cumulative=%d (polling floor = 1 scan / (hbTTL/2=7.5s) ⇒ ≈4 scans per 30s quiet window)",
		phaseATail, phaseCTail, hs.FloorBudget, finalCumulative)

	if fired := hbScanSnapsFired.Load(); fired < hbScanExpectedSnaps {
		hbScanFloorViolations.Add(1)
		return fmt.Errorf("heartbeat scan floor inconclusive: only %d/%d phase snapshots fired (phaseA_tail=%d phaseC_tail=%d) — the sampler did not capture both quiet windows", fired, hbScanExpectedSnaps, phaseATail, phaseCTail)
	}
	if phaseATail > hs.FloorBudget || phaseCTail > hs.FloorBudget {
		hbScanFloorViolations.Add(1)
		return fmt.Errorf("heartbeat scan floor exceeded: phaseA_tail=%d phaseC_tail=%d floor_budget=%d — suppression is not holding the leader's parti-heartbeat scan rate at the polling floor (stuck-open holiday or per-watcher-event forced check)", phaseATail, phaseCTail, hs.FloorBudget)
	}

	return nil
}
