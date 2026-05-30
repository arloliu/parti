package parti

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/stretchr/testify/require"
)

// plantAssignment writes an assignment to the worker's KV key so a subsequent
// refreshAssignmentFromNATS succeeds and advances the snapshot to it.
func plantAssignment(t *testing.T, m *Manager, a Assignment) {
	t.Helper()
	key := fmt.Sprintf("assignment.%s", m.WorkerID())
	b, err := json.Marshal(a)
	require.NoError(t, err)
	// Create-or-update: the key may already exist from a prior plant.
	if _, err := m.assignmentKV.Create(t.Context(), key, b); err != nil {
		_, err = m.assignmentKV.Put(t.Context(), key, b)
		require.NoError(t, err)
	}
}

// armDegraded puts the manager into Degraded with the given latch state and
// in-memory snapshot, then returns it ready for attemptRecoveryFromDegraded.
func armDegraded(t *testing.T, latched bool, snapshot Assignment) (*Manager, *recordingHandoff) {
	t.Helper()
	m, rh, _, _ := newTestManager(t)
	_, nc := partitest.StartEmbeddedNATS(t)
	m.assignmentKV = partitest.CreateJetStreamKV(t, nc, "selfheal-asgn")
	m.assignment.Store(snapshot)
	m.initialClaimsCommitted.Store(latched)
	m.state.Store(int32(StateDegraded))
	m.degradedSince.Store(time.Now().UnixNano())

	return m, rh
}

func TestAttemptRecovery_UnlatchedNonEmpty_StaysDegradedAndRearms(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, false, snap)
	// KV holds the SAME assignment, so refresh succeeds and snapshot stays V1.
	plantAssignment(t, m, snap)

	m.attemptRecoveryFromDegraded()

	require.NotZero(t, m.degradedSince.Load(),
		"unlatched worker holding an uncommitted assignment must STAY degraded, not exit")
	require.Equal(t, StateDegraded, m.State())
	stash := m.stashedApplyRetry.Load()
	require.NotNil(t, stash, "recovery must re-arm a bootstrap apply")
	require.Equal(t, int64(1), stash.Version, "re-arm targets the current assignment version")
}

func TestAttemptRecovery_Latched_ExitsToStable(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, true, snap) // claims already committed
	plantAssignment(t, m, snap)

	m.attemptRecoveryFromDegraded()
	m.wg.Wait()

	require.Zero(t, m.degradedSince.Load(), "a committed worker recovers normally")
	require.Equal(t, StateStable, m.State())
	require.Nil(t, m.stashedApplyRetry.Load(), "no bootstrap re-arm for a committed worker")
}

func TestAttemptRecovery_UnlatchedEmptyAssignment_ExitsToStable(t *testing.T) {
	t.Parallel()
	m, _ := armDegraded(t, false, Assignment{})
	// KV holds an empty-partition assignment at V1; refresh advances to it.
	plantAssignment(t, m, Assignment{Version: 1, LeaderRevision: 5})

	m.attemptRecoveryFromDegraded()
	m.wg.Wait()

	require.Zero(t, m.degradedSince.Load(),
		"a worker that owns no partitions has no claims to write — exit is correct")
	require.Equal(t, StateStable, m.State())
	require.Nil(t, m.stashedApplyRetry.Load())
}

func TestAttemptRecovery_RefreshFails_ReturnsBeforeGuard(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, false, snap)
	// Do NOT plant the key: refreshAssignmentFromNATS's Get fails → return
	// before the guard, no re-arm, stays degraded (whole-bucket-loss shape).

	m.attemptRecoveryFromDegraded()

	require.NotZero(t, m.degradedSince.Load(), "stays degraded when the refresh read fails")
	require.Nil(t, m.stashedApplyRetry.Load(),
		"a failed refresh must not re-arm a bootstrap apply (guard not reached)")
}

func TestAttemptRecovery_VersionAdvanceDuringWindow_RearmsAtNewVersion(t *testing.T) {
	t.Parallel()
	// Snapshot pinned at V1 (what the worker had when it degraded).
	v1 := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, false, v1)
	// A version advance landed in KV during the Degraded window: V2.
	v2 := Assignment{Version: 2, LeaderRevision: 8, Partitions: []Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}}}
	plantAssignment(t, m, v2)

	m.attemptRecoveryFromDegraded()

	require.NotZero(t, m.degradedSince.Load(), "must stay degraded")
	require.Equal(t, int64(2), m.CurrentAssignment().Version,
		"refresh must advance the snapshot to V2 before the re-arm reads cur")
	stash := m.stashedApplyRetry.Load()
	require.NotNil(t, stash, "must re-arm a bootstrap apply")
	require.Equal(t, int64(2), stash.Version,
		"re-arm MUST target V2 (cur read AFTER refresh) — not the stale V1 the gate would drop")
}
