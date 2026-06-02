package parti

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAttemptRecovery_EnumerationStall_LeaderStaysUntilEnumerationRecovers proves
// the NP-10 reason-scoped exit gate: a leader degraded with
// DegradeReasonEnumerationStall must NOT exit on the (unaffected) assignment
// commitment read while its Keys scan is still timing out — it would resume
// serving stale membership while blind. It exits only once an enumeration success
// is stamped AFTER the degrade.
func TestAttemptRecovery_EnumerationStall_LeaderStaysUntilEnumerationRecovers(t *testing.T) {
	t.Parallel()

	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	committed := snap
	m, _ := armDegraded(t, &committed, snap) // current assignment applied+acked
	plantAssignment(t, m, snap)
	m.isLeader.Store(true)
	m.lastDegradedReason.Store(DegradeReasonEnumerationStall)

	// No enumeration success since the degrade: the commitment guard is satisfied,
	// but the leader must STAY Degraded — exiting would resume serving stale
	// membership while the enumeration scan is still stalling.
	m.attemptRecoveryFromDegraded()
	require.NotZero(t, m.degradedSince.Load(),
		"a leader whose worker enumeration has not recovered must stay Degraded")
	require.Equal(t, StateDegraded, m.State())

	// Stamp an enumeration success AFTER degradedSince: the gate opens and the
	// applied leader exits. (Assert synchronously; exitDegraded transitions before
	// the recovery-grace goroutine it spawns matters.)
	m.lastEnumerationSuccessAt.Store(m.degradedSince.Load() + 1)
	m.attemptRecoveryFromDegraded()
	require.Zero(t, m.degradedSince.Load(),
		"a stamped enumeration success must let the leader exit Degraded")
	require.Equal(t, StateStable, m.State())
}

// TestAttemptRecovery_EnumerationStall_NonLeaderEscapesStuckDegrade is the
// load-bearing leadership-loss-escape test. A leader that degraded with
// DegradeReasonEnumerationStall and then LOST leadership runs no enumeration
// (startCalculator is leader-only via stopCalculator), so it can never stamp an
// enumeration success. Without the "&& m.isLeader.Load()" escape in the exit
// conjunct, such a worker would be trapped in Degraded forever — a gate on a
// capability that can no longer fire. With the escape, the gate is N/A for a
// non-leader and the worker exits normally.
func TestAttemptRecovery_EnumerationStall_NonLeaderEscapesStuckDegrade(t *testing.T) {
	t.Parallel()

	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	committed := snap
	m, _ := armDegraded(t, &committed, snap)
	plantAssignment(t, m, snap)
	m.isLeader.Store(false) // lost leadership while enumeration-stall-degraded
	m.lastDegradedReason.Store(DegradeReasonEnumerationStall)
	// lastEnumerationSuccessAt stays 0 — a non-leader runs no enumeration and can
	// never stamp a success. The escape must let it exit anyway.

	m.attemptRecoveryFromDegraded()
	m.wg.Wait() // safe: a non-leader exit skips the recovery-grace goroutine

	require.Zero(t, m.degradedSince.Load(),
		"a non-leader must not be trapped in an enumeration-stall degrade it can never clear")
	require.Equal(t, StateStable, m.State())
}
