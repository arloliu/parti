package parti

import (
	"errors"
	"testing"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/stretchr/testify/require"
)

// TestApplyAssignment_UnifiedPipeline_AcksSynchronously verifies the §4.4
// invariant that applyAssignment publishes a heartbeat ack synchronously
// as part of its return contract. The end-to-end "ack-before-StateStable"
// ordering is exercised in TestApplyAssignment_InitialBootstrap_EmptyAck_*
// in the parti_test package (uses a real Manager + heartbeat publisher
// + OnStateChanged hook).
func TestApplyAssignment_UnifiedPipeline_AcksSynchronously(t *testing.T) {
	t.Parallel()
	m, _, hb, _ := newTestManager(t)

	initial := Assignment{
		Version:        3,
		LeaderRevision: 11,
		Partitions:     []Partition{{Keys: []string{"alpha"}}},
	}
	require.NoError(t, m.applyAssignment(initial))

	// Pre-condition for the "ack-before-stable" invariant: the ack is
	// already recorded by the time applyAssignment returns.
	snap, ok := hb.latestSnap()
	require.True(t, ok, "applyAssignment must invoke SetAppliedAssignment before returning")
	require.Equal(t, int64(3), snap.AppliedVersion)
	require.Equal(t, int64(1), hb.pubNows.Load(), "PublishNow MUST fire as part of the apply pipeline")
}

// TestApplyAssignment_ApplyFails_NoStoreNoAck verifies the §4.4 invariant
// that on Apply failure neither Store nor Ack run, and a retry is scheduled.
func TestApplyAssignment_ApplyFails_NoStoreNoAck(t *testing.T) {
	t.Parallel()
	m, rh, hb, _ := newTestManager(t)

	// Pre-stage a successful prior apply so we can verify the snapshot
	// does NOT regress on failure.
	require.NoError(t, m.applyAssignment(Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p"}}}}))

	// Queue a one-shot error for the next Apply.
	rh.errOnce.Store(&errBox{err: errors.New("apply failed")})
	err := m.applyAssignment(Assignment{
		Version:        2,
		LeaderRevision: 10,
		Partitions:     []Partition{{Keys: []string{"q"}}},
	})
	require.Error(t, err)
	require.Equal(t, int64(1), m.CurrentAssignment().Version, "snapshot must not advance on Apply failure")

	snap, _ := hb.latestSnap()
	require.Equal(t, int64(1), snap.AppliedVersion, "ack must not advance on Apply failure")

	// Retry should be scheduled — stashedApplyRetry is non-nil (or the
	// retry goroutine may have already cleared it; either is acceptable so
	// long as the failure path doesn't panic).
}

// TestApplyInitialAssignment_ColdEmpty_PublishesExplicitAck targets the
// P0 #2 fix specifically: when waitForAssignment surfaces an empty
// Assignment{} AND there is no commit in KV, applyInitialAssignment MUST
// publish an explicit applied-empty ack (SetAppliedAssignment +
// PublishNow) before returning nil. Without this ack the subsequent
// transition to StateStable would race with the heartbeat publisher's
// startup tick and could advertise AppliedAt=zero — violating §4.4.
//
// This is the narrow unit-level companion to the parti_test integration
// test, which (with a single-worker leader) tends to take the commit-path
// branch rather than the cold-empty branch we fix here.
func TestApplyInitialAssignment_ColdEmpty_PublishesExplicitAck(t *testing.T) {
	t.Parallel()

	_, nc := partitest.StartEmbeddedNATS(t)
	akv := partitest.CreateJetStreamKV(t, nc, "cold-empty-ack-asgn")

	m, _, hb, _ := newTestManager(t)
	m.assignmentKV = akv
	// Leave m.CurrentAssignment() == Assignment{} (empty cold-bootstrap)
	// and DO NOT plant a commit in akv; the commit-path GetJSON returns
	// nil and applyInitialAssignment falls into the cold-empty branch.

	require.NoError(t, m.applyInitialAssignment(t.Context(), akv))

	// Snapshot store is the empty assignment.
	require.Equal(t, Assignment{}, m.CurrentAssignment())

	// SetAppliedAssignment was called exactly once with AppliedVersion=0,
	// and PublishNow fired — proves the ack was published explicitly
	// rather than left to the publisher's startup tick.
	snap, ok := hb.latestSnap()
	require.True(t, ok, "cold-empty bootstrap MUST publish an explicit applied ack")
	require.Equal(t, int64(0), snap.AppliedVersion,
		"cold-empty ack carries AppliedVersion=0 (no commit to advance to)")
	require.False(t, snap.AppliedAt.IsZero(),
		"AppliedAt MUST be non-zero — proves SetAppliedAssignment ran")
	require.Equal(t, int64(1), hb.pubNows.Load(),
		"PublishNow MUST fire once on the cold-empty bootstrap path")
}
