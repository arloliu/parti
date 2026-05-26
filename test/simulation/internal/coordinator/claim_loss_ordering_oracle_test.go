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

// TestClaimLossOrderingOracle_RevokeWithoutShutdownObservation_Backfills
// verifies that a revoke arriving without a prior watcher-observed
// Shutdown is backfilled (NOT counted as a violation) — the watcher poll
// cadence races the in-process Stop→revoke sequence, so a missing
// Shutdown observation at revoke time is a race, not a real ordering bug.
func TestClaimLossOrderingOracle_RevokeWithoutShutdownObservation_Backfills(t *testing.T) {
	o := NewClaimLossOrderingOracle()
	o.RegisterStableID("worker-0", "freeze-victim-1")
	o.ObserveRevocation("worker-0", "freeze-victim-1", time.Now())
	if got := o.Violations(); got != 0 {
		t.Fatalf("expected 0 violations (backfill path), got %d", got)
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
// a ReceivedMessage arriving strictly after Shutdown, for a partition
// the SAME sim worker was last known to own, is flagged.
func TestClaimLossOrderingOracle_PostShutdownMessage_Violation(t *testing.T) {
	o := NewClaimLossOrderingOracle()
	o.RegisterStableID("worker-0", "freeze-victim-1")
	o.RecordAssignment("worker-0", []int{1, 2, 3})

	t0 := time.Now()
	o.ObserveShutdown("worker-0", "freeze-victim-1", t0)
	// Message for an owned partition arrives AFTER shutdown.
	o.ObserveMessage("worker-0", 2, t0.Add(100*time.Millisecond))
	if got := o.Violations(); got != 1 {
		t.Fatalf("expected 1 violation, got %d", got)
	}

	// A message for a partition the worker did NOT own → no violation.
	o.ObserveMessage("worker-0", 99, t0.Add(200*time.Millisecond))
	if got := o.Violations(); got != 1 {
		t.Fatalf("expected violations unchanged, got %d", got)
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
