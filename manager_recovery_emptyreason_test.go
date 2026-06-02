package parti

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAttemptRecovery_EmptyReason_StaysDegraded locks the empty-reason guard in
// attemptRecoveryFromDegraded: an unset degrade reason — the brief window between
// the winning enterDegraded's degradedSince CAS and its reason store — must block
// the recovery exit even when the commitment guard is satisfied and no backstop
// fires.
//
// The reason-scoped gates (kv-unavailable heartbeat stamp, enumeration stamp) can
// never match an empty reason, so without this guard a blank reason would fall
// straight through to exitDegraded and falsely report Stable while a degrade entry
// is still being published. This is distinct from manager_reason_ownership_test.go,
// which pins the store-after-CAS / clear-before-since field ordering white-box;
// here we pin the observable consequence (State stays Degraded) so it survives the
// degraded-state encapsulation refactor.
//
// Note: the guard is a CONJUNCTIVE blocking check, not an ordered one — its
// placement among the reason-scoped gates is behaviour-irrelevant (empty never
// matches them); only its PRESENCE before exitDegraded is load-bearing, which is
// exactly what this test locks.
func TestAttemptRecovery_EmptyReason_StaysDegraded(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, &snap, snap) // committed == snapshot: commitment guard passes
	plantAssignment(t, m, snap)         // refresh succeeds, snapshot stays at snap

	// Arm the unset-reason window: a recovery tick that observes degradedSince != 0
	// but reason == "" (the winner CAS'd but has not yet stored its reason).
	m.lastDegradedReason.Store("")

	m.attemptRecoveryFromDegraded()

	require.Equal(t, StateDegraded, m.State(),
		"an empty degrade reason must block the recovery exit (gate closes the post-CAS-pre-store window)")
}
