package coordinator

import (
	"testing"
	"time"
)

// TestClaimLossOrderingOracle_StopBeforeRevoke_NoViolation verifies the
// happy path: shutdown observed first, then revocation. Should NOT
// increment violations.
func TestClaimLossOrderingOracle_StopBeforeRevoke_NoViolation(t *testing.T) {
	o := NewClaimLossOrderingOracle()
	o.RegisterStableID("worker-0", "freeze-victim-1")
	o.RecordAssignment("worker-0", []int{1, 2, 3})

	t0 := time.Now()
	o.ObserveShutdown("worker-0", "freeze-victim-1", t0)
	o.ObserveRevocation("worker-0", "freeze-victim-1", t0.Add(100*time.Millisecond))

	if got := o.Violations(); got != 0 {
		t.Fatalf("expected 0 violations, got %d", got)
	}
}

// TestClaimLossOrderingOracle_RevokeWithoutShutdownObservation_NoSampler_Backfills
// verifies the legacy code path: when NO state sampler is installed, the
// oracle preserves the original tolerate-and-backfill behavior. This branch
// is kept for the no-coordinator unit tests; production wiring always
// installs a sampler so the silent-pass regression cannot recur.
func TestClaimLossOrderingOracle_RevokeWithoutShutdownObservation_NoSampler_Backfills(t *testing.T) {
	o := NewClaimLossOrderingOracle()
	o.RegisterStableID("worker-0", "freeze-victim-1")
	o.ObserveRevocation("worker-0", "freeze-victim-1", time.Now())
	if got := o.Violations(); got != 0 {
		t.Fatalf("expected 0 violations (no-sampler backfill path), got %d", got)
	}
}

// TestClaimLossOrderingOracle_RevokeBeforeShutdown_StateShutdownNow_Backfills
// is the (a) regression test from the post-impl review: when a revoke
// arrives before the watcher observed Shutdown, the sampler is consulted
// and reports the worker IS currently in Shutdown. The watcher merely
// lagged; this is the legitimate poll-cadence race the backfill exists
// for. NO violation expected.
func TestClaimLossOrderingOracle_RevokeBeforeShutdown_StateShutdownNow_Backfills(t *testing.T) {
	o := NewClaimLossOrderingOracle()
	o.RegisterStableID("worker-0", "freeze-victim-1")
	// Sampler reports the worker is RIGHT NOW in shutdown state.
	o.SetStateSampler(func(simWorkerID string) (int, bool) {
		if simWorkerID == "worker-0" {
			return workerStateShutdown, true
		}
		return 0, false
	})
	o.ObserveRevocation("worker-0", "freeze-victim-1", time.Now())
	if got := o.Violations(); got != 0 {
		t.Fatalf("expected 0 violations when sampler confirms shutdown, got %d", got)
	}
}

// TestClaimLossOrderingOracle_RevokeBeforeShutdown_StateStableNow_Backfills
// is the (b) regression test, REVISED in response to the empirical
// finding from chaos_network_disconnect_long: the production
// UpdateWorkerConsumer(workerID, nil) revoke surface is DUAL-PURPOSE —
// it carries both claimLostShutdown -> revokeWorkerConsumer (the
// regression case) and the apply loop's legitimate rebalance-to-empty
// (manager.go cold-empty bootstrap; leader-driven reassignment dropping
// a worker to zero partitions). State stays Stable for the rebalance
// case. Counting a revoke-while-Stable as a violation produces false
// positives on every long-disconnect / scale-down test.
//
// The sharp negative signal for Stop-before-revoke is the
// post-shutdown ObserveMessage check (received_message for a previously-
// owned partition arriving strictly AFTER an observed Shutdown), which
// is unambiguous because rebalance does NOT generate post-Shutdown
// traffic for previously-owned partitions.
func TestClaimLossOrderingOracle_RevokeBeforeShutdown_StateStableNow_Backfills(t *testing.T) {
	o := NewClaimLossOrderingOracle()
	o.RegisterStableID("worker-0", "freeze-victim-1")
	// Sampler reports the worker is RIGHT NOW in stable state (rebalance path).
	o.SetStateSampler(func(simWorkerID string) (int, bool) {
		if simWorkerID == "worker-0" {
			return workerStateStable, true
		}

		return 0, false
	})
	o.ObserveRevocation("worker-0", "freeze-victim-1", time.Now())
	if got := o.Violations(); got != 0 {
		t.Fatalf("expected 0 violations (rebalance-to-empty revoke path), got %d", got)
	}
}

// TestClaimLossOrderingOracle_RevokeBeforeShutdown_SamplerUnknown_Backfills
// covers the sampler-installed-but-worker-unknown branch. Tolerated
// (audit-only via the log line) for the same reason as the StableNow
// case — the unknown could be either a missed-Shutdown or a
// rebalance-to-empty whose ProcessWorker entry has already been
// unregistered.
func TestClaimLossOrderingOracle_RevokeBeforeShutdown_SamplerUnknown_Backfills(t *testing.T) {
	o := NewClaimLossOrderingOracle()
	o.RegisterStableID("worker-0", "freeze-victim-1")
	o.SetStateSampler(func(simWorkerID string) (int, bool) {
		return 0, false // worker no longer in registry
	})
	o.ObserveRevocation("worker-0", "freeze-victim-1", time.Now())
	if got := o.Violations(); got != 0 {
		t.Fatalf("expected 0 violations (sampler-unknown audit-only path), got %d", got)
	}
}

// TestClaimLossOrderingOracle_RevokeStableThenLaterMessage_NoViolation
// proves the rebalance path stays clean even when a later message
// arrives for a previously-revoked partition. This is the P1 regression
// from post-impl review v2: the v1 fix silently wrote shutdownBySim on
// a sampler-Stable revoke, which then poisoned ObserveMessage and made
// a legitimate "leader reassigns the partition back to this worker"
// rebalance look like post_shutdown_message traffic.
func TestClaimLossOrderingOracle_RevokeStableThenLaterMessage_NoViolation(t *testing.T) {
	o := NewClaimLossOrderingOracle()
	o.RegisterStableID("worker-0", "freeze-victim-1")
	o.RecordAssignment("worker-0", []int{1, 2, 3})
	o.SetStateSampler(func(simWorkerID string) (int, bool) {
		if simWorkerID == "worker-0" {
			return workerStateStable, true
		}

		return 0, false
	})
	t0 := time.Now()
	// Rebalance-to-empty revoke (sampler reports Stable).
	o.ObserveRevocation("worker-0", "freeze-victim-1", t0)
	// Leader reassigns the same partition back to worker-0 and a
	// message arrives well after the revoke. Must NOT count as a
	// post-shutdown violation — no real Shutdown was observed.
	o.RecordAssignment("worker-0", []int{1, 2, 3})
	o.ObserveMessage("worker-0", 2, t0.Add(500*time.Millisecond))
	if got := o.Violations(); got != 0 {
		t.Fatalf("rebalance-then-message path: expected 0 violations, got %d", got)
	}
}

// TestClaimLossOrderingOracle_RevokeStableThenRealShutdownThenMessage_Violation
// proves the post-shutdown gate STILL fires when a real Shutdown is
// observed after a sampler-Stable revoke. The earlier audit-only
// revoke must not mask a later, genuine Stop-before-revoke regression
// signal.
func TestClaimLossOrderingOracle_RevokeStableThenRealShutdownThenMessage_Violation(t *testing.T) {
	o := NewClaimLossOrderingOracle()
	o.RegisterStableID("worker-0", "freeze-victim-1")
	o.RecordAssignment("worker-0", []int{1, 2, 3})
	o.SetStateSampler(func(simWorkerID string) (int, bool) {
		if simWorkerID == "worker-0" {
			return workerStateStable, true
		}

		return 0, false
	})
	t0 := time.Now()
	// Audit-only sampler-Stable revoke.
	o.ObserveRevocation("worker-0", "freeze-victim-1", t0)
	// Real shutdown observed later, with the worker still holding the
	// same partitions at the time of the shutdown snapshot.
	o.RecordAssignment("worker-0", []int{1, 2, 3})
	tShut := t0.Add(1 * time.Second)
	o.ObserveShutdown("worker-0", "freeze-victim-1", tShut)
	// Message beyond the drain tolerance window for an owned partition →
	// violation (the worker kept consuming well past the stop boundary).
	o.ObserveMessage("worker-0", 2, tShut.Add(claimLossDrainToleranceWindow+100*time.Millisecond))
	if got := o.Violations(); got != 1 {
		t.Fatalf("real shutdown then post-shutdown message: expected 1 violation, got %d", got)
	}
}

// TestClaimLossOrderingOracle_RevokeTimestampBeforeShutdown_Violation
// verifies the defensive check: if both signals fire but the revoke
// timestamp PRECEDES the shutdown timestamp, that's still a violation
// (Manager.Stop completed before the revoke goroutine ran, but the
// watcher polled the Shutdown state LATER — yet our recorded revoke
// timestamp is earlier than the watcher-recorded Shutdown timestamp,
// which would mean revoke fired before Stop in production code paths).
func TestClaimLossOrderingOracle_RevokeTimestampBeforeShutdown_Violation(t *testing.T) {
	o := NewClaimLossOrderingOracle()
	o.RegisterStableID("worker-0", "freeze-victim-1")
	t0 := time.Now()
	o.ObserveShutdown("worker-0", "freeze-victim-1", t0)
	// Revoke timestamp is BEFORE shutdown timestamp.
	o.ObserveRevocation("worker-0", "freeze-victim-1", t0.Add(-100*time.Millisecond))
	if got := o.Violations(); got != 1 {
		t.Fatalf("expected 1 violation, got %d", got)
	}
}

// TestClaimLossOrderingOracle_PostShutdownMessage_Violation verifies that
// a ReceivedMessage arriving MORE THAN the drain tolerance window after
// Shutdown, for a partition the SAME sim worker was last known to own, is
// flagged — this is the "worker kept consuming past the stop boundary"
// signal the oracle exists to catch.
func TestClaimLossOrderingOracle_PostShutdownMessage_Violation(t *testing.T) {
	o := NewClaimLossOrderingOracle()
	o.RegisterStableID("worker-0", "freeze-victim-1")
	o.RecordAssignment("worker-0", []int{1, 2, 3})

	t0 := time.Now()
	o.ObserveShutdown("worker-0", "freeze-victim-1", t0)
	// Message for an owned partition arrives well beyond the drain window.
	o.ObserveMessage("worker-0", 2, t0.Add(claimLossDrainToleranceWindow+100*time.Millisecond))
	if got := o.Violations(); got != 1 {
		t.Fatalf("expected 1 violation, got %d", got)
	}

	// A message for a partition the worker did NOT own → no violation.
	o.ObserveMessage("worker-0", 99, t0.Add(claimLossDrainToleranceWindow+200*time.Millisecond))
	if got := o.Violations(); got != 1 {
		t.Fatalf("expected violations unchanged, got %d", got)
	}
}

// TestClaimLossOrderingOracle_PostShutdownMessage_WithinDrainWindow_NoViolation
// is the regression guard for the drain false positive that failed CI on
// chaos_stableid_claim_steal / chaos_bucket_peer_takeover: parti transitions
// to StateShutdown before revoking the worker consumer, and WithDrainOnRemove
// lets an in-flight handler complete. A message for a previously-owned
// partition arriving inside the drain tolerance window is CORRECT behavior and
// MUST NOT be flagged.
func TestClaimLossOrderingOracle_PostShutdownMessage_WithinDrainWindow_NoViolation(t *testing.T) {
	o := NewClaimLossOrderingOracle()
	o.RegisterStableID("worker-0", "freeze-victim-1")
	o.RecordAssignment("worker-0", []int{1, 2, 3})

	t0 := time.Now()
	o.ObserveShutdown("worker-0", "freeze-victim-1", t0)
	// A single drained frame lands shortly after the observed shutdown,
	// still inside the drain window — the empirically observed CI gap was
	// ~170ms. No violation.
	o.ObserveMessage("worker-0", 2, t0.Add(claimLossDrainToleranceWindow-100*time.Millisecond))
	// A frame exactly at the window boundary is still tolerated (strictly
	// greater-than is required to flag).
	o.ObserveMessage("worker-0", 2, t0.Add(claimLossDrainToleranceWindow))
	if got := o.Violations(); got != 0 {
		t.Fatalf("in-window drain: expected 0 violations, got %d", got)
	}
}

// TestClaimLossOrderingOracle_SuccessorReclaimsStableID_NoViolation is the
// load-bearing regression test for the bug found running
// chaos_stableid_maxage_expiry: when worker-3 takes over freeze-victim-1
// via stale-takeover, its post-takeover messages MUST NOT be attributed
// to worker-0's earlier shutdown. The shutdown is keyed per-sim-worker,
// so worker-3 starts with a clean slate even though it inherits the
// stable ID.
func TestClaimLossOrderingOracle_SuccessorReclaimsStableID_NoViolation(t *testing.T) {
	o := NewClaimLossOrderingOracle()
	// worker-0 originally holds freeze-victim-1 with partitions {1,2,3}.
	o.RegisterStableID("worker-0", "freeze-victim-1")
	o.RecordAssignment("worker-0", []int{1, 2, 3})

	t0 := time.Now()
	o.ObserveShutdown("worker-0", "freeze-victim-1", t0)

	// worker-3 takes over the stable ID and gets assigned the same
	// partitions.
	o.RegisterStableID("worker-3", "freeze-victim-1")
	o.RecordAssignment("worker-3", []int{1, 2, 3})
	// worker-3 receives messages — these MUST NOT be flagged as
	// post-shutdown violations of worker-0.
	o.ObserveMessage("worker-3", 1, t0.Add(1*time.Second))
	o.ObserveMessage("worker-3", 2, t0.Add(2*time.Second))
	if got := o.Violations(); got != 0 {
		t.Fatalf("successor worker reclaiming stable ID: expected 0 violations, got %d", got)
	}
}

// TestClaimLossOrderingOracle_PreShutdownMessage_NoViolation verifies
// messages arriving BEFORE shutdown are tolerated.
func TestClaimLossOrderingOracle_PreShutdownMessage_NoViolation(t *testing.T) {
	o := NewClaimLossOrderingOracle()
	o.RegisterStableID("worker-0", "freeze-victim-1")
	o.RecordAssignment("worker-0", []int{1, 2, 3})

	t0 := time.Now()
	// Message arrives before shutdown.
	o.ObserveMessage("worker-0", 2, t0.Add(-100*time.Millisecond))
	o.ObserveShutdown("worker-0", "freeze-victim-1", t0)
	if got := o.Violations(); got != 0 {
		t.Fatalf("expected 0 violations, got %d", got)
	}
}

// TestClaimLossOrderingOracle_UnmappedWorker_NoOp verifies that messages
// for workers without a registered stable ID are silently ignored.
func TestClaimLossOrderingOracle_UnmappedWorker_NoOp(t *testing.T) {
	o := NewClaimLossOrderingOracle()
	o.ObserveMessage("ghost-worker", 1, time.Now())
	if got := o.Violations(); got != 0 {
		t.Fatalf("expected 0 violations for unmapped worker, got %d", got)
	}
}
