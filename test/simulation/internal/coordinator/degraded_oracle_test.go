package coordinator

import (
	"testing"
	"time"
)

// TestDegradedReasonOracle_BucketDelete_BucketUnavailableMatches verifies
// that a bucket_delete expectation with the "bucket-unavailable:" substring
// matches a WorkerDegradedReport whose Reason carries the bucket name as a
// suffix (production format: "bucket-unavailable:parti-stableid").
func TestDegradedReasonOracle_BucketDelete_BucketUnavailableMatches(t *testing.T) {
	o := NewDegradedReasonOracle()
	o.ExpectAfter("bucket_delete:parti-stableid",
		[]string{"bucket-unavailable:", "bucket-recreated:parti-stableid"},
		2*time.Second, "", false)

	o.Ingest(WorkerDegradedReport{
		WorkerID: "simulation-worker-3",
		Reason:   "bucket-unavailable:parti-stableid",
		At:       time.Now(),
	})

	if got := o.ExpectedDegradedObserved(); got != 1 {
		t.Fatalf("expected observed=1, got %d", got)
	}
	if got := o.ExpectedDegradedMissing(); got != 0 {
		t.Fatalf("expected missing=0, got %d", got)
	}
}

// TestDegradedReasonOracle_BucketRecreate_ExactReasonMatches verifies that
// a bucket_recreate expectation with the "bucket-recreated:parti-assignment"
// substring matches the corresponding production reason format.
func TestDegradedReasonOracle_BucketRecreate_ExactReasonMatches(t *testing.T) {
	o := NewDegradedReasonOracle()
	o.ExpectAfter("bucket_recreate:parti-assignment",
		[]string{"bucket-recreated:parti-assignment"},
		2*time.Second, "", false)

	o.Ingest(WorkerDegradedReport{
		WorkerID: "simulation-worker-1",
		Reason:   "bucket-recreated:parti-assignment",
		At:       time.Now(),
	})

	if got := o.ExpectedDegradedObserved(); got != 1 {
		t.Fatalf("expected observed=1, got %d", got)
	}
}

// TestDegradedReasonOracle_MissingAfterDeadline verifies that an expectation
// with no matching observation increments expected_degraded_missing after
// Sweep is called past the deadline.
func TestDegradedReasonOracle_MissingAfterDeadline(t *testing.T) {
	o := NewDegradedReasonOracle()
	o.ExpectAfter("bucket_delete:parti-stableid",
		[]string{"bucket-unavailable:"},
		10*time.Millisecond, "", false)

	time.Sleep(20 * time.Millisecond)
	o.Sweep(time.Now())

	if got := o.ExpectedDegradedMissing(); got != 1 {
		t.Fatalf("expected missing=1, got %d", got)
	}
	if got := o.ExpectedDegradedObserved(); got != 0 {
		t.Fatalf("expected observed=0, got %d", got)
	}
}

// TestDegradedReasonOracle_NonMatchingReasonIgnored verifies that a
// WorkerDegradedReport whose Reason does not match any registered substring
// is ignored (does not count as observed).
func TestDegradedReasonOracle_NonMatchingReasonIgnored(t *testing.T) {
	o := NewDegradedReasonOracle()
	o.ExpectAfter("bucket_delete:parti-stableid",
		[]string{"bucket-unavailable:", "bucket-recreated:parti-stableid"},
		2*time.Second, "", false)

	o.Ingest(WorkerDegradedReport{
		WorkerID: "simulation-worker-3",
		Reason:   "some-other-reason",
		At:       time.Now(),
	})

	if got := o.ExpectedDegradedObserved(); got != 0 {
		t.Fatalf("expected observed=0 (no match), got %d", got)
	}
}

// TestDegradedReasonOracle_PeerTakeover_ClaimLostExpected verifies that a
// peer-takeover expectation correctly classifies the targeted worker's
// claim-lost shutdown as expected, and any OTHER worker's claim-lost
// shutdown as unexpected.
func TestDegradedReasonOracle_PeerTakeover_ClaimLostExpected(t *testing.T) {
	o := NewDegradedReasonOracle()
	o.ExpectAfter("bucket_peer_takeover:simulation-worker-5",
		nil, 2*time.Second, "simulation-worker-5", true)

	// Targeted worker: expected
	o.ObserveClaimLostShutdown("simulation-worker-5")
	if got := o.UnexpectedClaimLostShutdown(); got != 0 {
		t.Fatalf("expected unexpected_claim_lost=0 for targeted worker, got %d", got)
	}

	// Other worker: unexpected
	o.ObserveClaimLostShutdown("simulation-worker-9")
	if got := o.UnexpectedClaimLostShutdown(); got != 1 {
		t.Fatalf("expected unexpected_claim_lost=1 for non-targeted worker, got %d", got)
	}
}

// TestDegradedReasonOracle_ClaimLost_NoActiveExpectation verifies that a
// claim-lost shutdown observed when no peer-takeover expectation is active
// always counts as unexpected — this is the "whole-bucket-loss mis-routed
// into claimLostShutdown" failure mode INV1 is designed to catch.
func TestDegradedReasonOracle_ClaimLost_NoActiveExpectation(t *testing.T) {
	o := NewDegradedReasonOracle()

	o.ObserveClaimLostShutdown("simulation-worker-2")
	if got := o.UnexpectedClaimLostShutdown(); got != 1 {
		t.Fatalf("expected unexpected_claim_lost=1, got %d", got)
	}
}

// TestDegradedReasonOracle_TargetWorkerFilter verifies that a peer-takeover
// expectation with a target worker ignores degraded reports from other
// workers.
func TestDegradedReasonOracle_TargetWorkerFilter(t *testing.T) {
	o := NewDegradedReasonOracle()
	// Use a degraded-reason substring AND target worker filter — the
	// peer-takeover primitive does not normally trigger degraded reports,
	// but the filter logic is exercised by this test.
	o.ExpectAfter("bucket_peer_takeover:simulation-worker-5",
		[]string{"some-reason"}, 2*time.Second, "simulation-worker-5", true)

	// Mismatched worker — should NOT match.
	o.Ingest(WorkerDegradedReport{
		WorkerID: "simulation-worker-2",
		Reason:   "some-reason",
		At:       time.Now(),
	})
	if got := o.ExpectedDegradedObserved(); got != 0 {
		t.Fatalf("expected observed=0 (worker filter), got %d", got)
	}

	// Matching worker — should match.
	o.Ingest(WorkerDegradedReport{
		WorkerID: "simulation-worker-5",
		Reason:   "some-reason",
		At:       time.Now(),
	})
	if got := o.ExpectedDegradedObserved(); got != 1 {
		t.Fatalf("expected observed=1 (matching worker), got %d", got)
	}
}
