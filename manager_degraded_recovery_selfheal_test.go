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

// armDegraded puts the manager into Degraded with the given committed assignment
// (nil = never applied) and in-memory snapshot, then returns it ready for
// attemptRecoveryFromDegraded. The committed assignment is the new source of
// truth for "has the current assignment been applied+acked" (replaces the old
// one-way initialClaimsCommitted latch).
func armDegraded(t *testing.T, committed *Assignment, snapshot Assignment) (*Manager, *recordingHandoff) {
	t.Helper()
	m, rh, _, _ := newTestManager(t)
	_, nc := partitest.StartEmbeddedNATS(t)
	m.assignmentKV = partitest.CreateJetStreamKV(t, nc, "selfheal-asgn")
	m.assignment.Store(snapshot)
	if committed != nil {
		m.committedAssignment.Store(committed)
	}
	m.state.Store(int32(StateDegraded))
	// These are reason-agnostic commitment-guard recovery tests, so arm a
	// non-kv-unavailable reason — the kv-unavailable conjunct is skipped and
	// recovery keys on the commitment guard alone. markDegraded publishes since and
	// reason together, mirroring production enterDegraded's single-swap record.
	m.markDegraded(time.Now().UnixNano(), "NATS connection down")

	return m, rh
}

func TestAttemptRecovery_NeverApplied_NonEmpty_StaysDegradedAndRearms(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, nil, snap) // nothing committed
	// KV holds the SAME assignment, so refresh succeeds and snapshot stays V1.
	plantAssignment(t, m, snap)

	m.attemptRecoveryFromDegraded()

	require.NotZero(t, m.degradedSinceNano(),
		"a worker holding an unapplied assignment must STAY degraded, not exit")
	require.Equal(t, StateDegraded, m.State())
	stash := m.stashedApplyRetry.Load()
	require.NotNil(t, stash, "recovery must re-arm a bootstrap apply")
	require.Equal(t, int64(1), stash.Version, "re-arm targets the current assignment version")
}

func TestAttemptRecovery_Applied_ExitsToStable(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	committed := snap
	m, _ := armDegraded(t, &committed, snap) // current assignment already applied+acked
	plantAssignment(t, m, snap)

	m.attemptRecoveryFromDegraded()
	m.wg.Wait()

	require.Zero(t, m.degradedSinceNano(), "an applied worker recovers normally")
	require.Equal(t, StateStable, m.State())
	require.Nil(t, m.stashedApplyRetry.Load(), "no bootstrap re-arm for an applied worker")
}

// TestAttemptRecovery_LatchedVersionAdvance_RearmsAtNewVersion is the core
// reproducer for THIS PR's defect: a worker that committed V1 then has the
// snapshot advanced to a failed V2 via the recovery refresh must NOT exit — the
// version-only/latch-based guard wrongly did, reporting Stable with V2 unwritten.
func TestAttemptRecovery_LatchedVersionAdvance_RearmsAtNewVersion(t *testing.T) {
	t.Parallel()
	v1 := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	committed := v1
	m, _ := armDegraded(t, &committed, v1) // committed at V1
	// A failed version advance: KV now holds V2; refresh stores it into the snapshot.
	v2 := Assignment{Version: 2, LeaderRevision: 8, Partitions: []Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}}}
	plantAssignment(t, m, v2)

	m.attemptRecoveryFromDegraded()

	require.NotZero(t, m.degradedSinceNano(),
		"a worker committed at V1 but holding an unapplied V2 must STAY degraded")
	require.Equal(t, int64(2), m.CurrentAssignment().Version, "refresh advances the snapshot to V2")
	stash := m.stashedApplyRetry.Load()
	require.NotNil(t, stash, "must re-arm a V2 apply")
	require.Equal(t, int64(2), stash.Version, "re-arm targets V2 (cur read AFTER refresh)")
}

// TestAttemptRecovery_SameVersionDifferentDigest_Rearms pins the digest term of
// the identity (review v1 P0): the publisher can expose two different partition
// sets at the same version/LR (legacy alias before commit CAS), so version alone
// must not be treated as applied.
func TestAttemptRecovery_SameVersionDifferentDigest_Rearms(t *testing.T) {
	t.Parallel()
	setA := Assignment{Version: 3, LeaderRevision: 7, Partitions: []Partition{{Keys: []string{"p0"}}}}
	committed := setA
	m, _ := armDegraded(t, &committed, setA)
	// Same version AND same LR, DIFFERENT partition set lands in KV.
	setB := Assignment{Version: 3, LeaderRevision: 7, Partitions: []Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}}}
	plantAssignment(t, m, setB)

	m.attemptRecoveryFromDegraded()

	require.NotZero(t, m.degradedSinceNano(),
		"same version but a different partition-set digest is NOT applied — must re-arm")
	require.NotNil(t, m.stashedApplyRetry.Load())
}

// TestAttemptRecovery_SameVersionHigherLR_Rearms pins the LeaderRevision term
// (review v2 P1): a same-version/same-digest higher-LR re-issue that entered the
// snapshot via refresh has a stale applied-ack until re-applied; the leader audit
// flags an LR mismatch as behind.
func TestAttemptRecovery_SameVersionHigherLR_Rearms(t *testing.T) {
	t.Parallel()
	parts := []Partition{{Keys: []string{"p0"}}}
	committed := Assignment{Version: 4, LeaderRevision: 9, Partitions: parts}
	m, _ := armDegraded(t, &committed, committed)
	// Same version, same partitions, HIGHER leader revision lands in KV.
	higher := Assignment{Version: 4, LeaderRevision: 11, Partitions: parts}
	plantAssignment(t, m, higher)

	m.attemptRecoveryFromDegraded()

	require.NotZero(t, m.degradedSinceNano(),
		"same version/digest but a higher leader revision is NOT applied — must re-arm")
	require.NotNil(t, m.stashedApplyRetry.Load())
}

// TestAttemptRecovery_SourceKnownVsUnknownAlias_ExitsNoDowngrade pins the
// audit-asymmetric source comparison (review v3 P0): the degraded refresh reads
// the legacy alias, which DROPS source revision. A worker that applied a
// known-source assignment must NOT re-arm against the source-unknown alias (which
// would downgrade its known-source ack to unknown — the audit then flags it as
// behind). cur-source-unknown always matches a known-source committed.
func TestAttemptRecovery_SourceKnownVsUnknownAlias_ExitsNoDowngrade(t *testing.T) {
	t.Parallel()
	parts := []Partition{{Keys: []string{"p0"}}}
	committed := Assignment{Version: 5, LeaderRevision: 5, Partitions: parts, SourceRevision: 42, SourceRevisionKnown: true}
	m, _ := armDegraded(t, &committed, committed)
	// Legacy alias: same version/LR/partitions but source revision DROPPED.
	alias := Assignment{Version: 5, LeaderRevision: 5, Partitions: parts}
	plantAssignment(t, m, alias)

	m.attemptRecoveryFromDegraded()
	m.wg.Wait()

	require.Zero(t, m.degradedSinceNano(),
		"a source-unknown alias must NOT trigger a re-arm that downgrades a known-source ack")
	require.Equal(t, StateStable, m.State())
	require.Nil(t, m.stashedApplyRetry.Load())
	c := m.committedAssignment.Load()
	require.NotNil(t, c)
	require.True(t, c.SourceRevisionKnown, "the known-source ack must NOT be downgraded")
}

// TestAttemptRecovery_ColdZero_ExitsToStable: a never-assigned worker (committed
// nil → empty@0) whose snapshot is also empty@0 is applied-by-identity and exits.
func TestAttemptRecovery_ColdZero_ExitsToStable(t *testing.T) {
	t.Parallel()
	m, _ := armDegraded(t, nil, Assignment{})
	plantAssignment(t, m, Assignment{}) // KV holds empty@0; refresh keeps empty@0

	m.attemptRecoveryFromDegraded()
	m.wg.Wait()

	require.Zero(t, m.degradedSinceNano(),
		"a never-assigned worker (empty@0 == committed empty@0) exits normally")
	require.Equal(t, StateStable, m.State())
	require.Nil(t, m.stashedApplyRetry.Load())
}

// TestAttemptRecovery_VersionedEmptyRevoke_Rearms: an implicit-revoke (commit
// case (d)) empty assignment carrying a real version that was never applied must
// re-arm (the apply still tears down the consumer and re-acks the version) — NOT
// exit as Stable with a stale ack. This is the empty-set arm of review v2 P0.
func TestAttemptRecovery_VersionedEmptyRevoke_Rearms(t *testing.T) {
	t.Parallel()
	// Committed a non-empty V1; a revoke-all empty@V2 lands in KV.
	committed := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, &committed, committed)
	plantAssignment(t, m, Assignment{Version: 2, LeaderRevision: 8}) // empty@V2

	m.attemptRecoveryFromDegraded()

	require.NotZero(t, m.degradedSinceNano(),
		"a versioned empty revoke that was never applied must re-arm, not exit")
	require.Equal(t, int64(2), m.CurrentAssignment().Version)
	stash := m.stashedApplyRetry.Load()
	require.NotNil(t, stash, "must re-arm the empty-revoke apply")
	require.Equal(t, int64(2), stash.Version)
}

// TestRecoveryGuard_TornRead_NoMissedHeal pins the §2.6 lock-free safety property
// deterministically (the -race stress in test/integration/failure only proves "no
// data race"). The guard reads CurrentAssignment() and committedAssignment as two
// independent atomics, so a torn read can present either skew. The invariant: the
// guard exits ONLY when the cur it observed has actually been committed
// (currentAssignmentApplied is true IFF committed identity == cur identity), so no
// torn read can cause a missed heal — at worst a redundant, stale-dropped re-arm
// that the next tick corrects.
func TestRecoveryGuard_TornRead_NoMissedHeal(t *testing.T) {
	t.Parallel()
	v1 := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	v2 := Assignment{Version: 2, LeaderRevision: 8, Partitions: []Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}}}

	// Predicate invariant: a torn read presents some (cur, committed) pair; the
	// guard exits only when they match identity. Prove the predicate never
	// reports "applied" for an unapplied cur, in BOTH skews.
	t.Run("predicate exits only on a genuine identity match", func(t *testing.T) {
		t.Parallel()
		m, _, _, _ := newTestManager(t)

		// new-snapshot/old-committed skew: cur=V2, committed=V1 → not applied.
		committedV1 := v1
		m.committedAssignment.Store(&committedV1)
		require.False(t, m.currentAssignmentApplied(v2),
			"new-cur/old-committed must NOT be treated as applied (would miss the V2 heal)")

		// old-snapshot/new-committed skew: cur=V1, committed=V2 → not applied.
		committedV2 := v2
		m.committedAssignment.Store(&committedV2)
		require.False(t, m.currentAssignmentApplied(v1),
			"old-cur/new-committed must NOT be treated as applied (no false exit)")

		// The only exit: committed identity actually matches the observed cur.
		require.True(t, m.currentAssignmentApplied(v2), "a genuine identity match exits")
	})

	// Guard level, new-snapshot/old-committed: re-arms the real new version.
	t.Run("new-snapshot/old-committed re-arms the current version", func(t *testing.T) {
		t.Parallel()
		committed := v1
		m, _ := armDegraded(t, &committed, v1)
		plantAssignment(t, m, v2) // refresh advances snapshot to V2

		m.attemptRecoveryFromDegraded()

		require.NotZero(t, m.degradedSinceNano(), "must stay degraded, not falsely exit")
		stash := m.stashedApplyRetry.Load()
		require.NotNil(t, stash)
		require.Equal(t, int64(2), stash.Version, "re-arms the real current version V2")
	})

	// Guard level, old-snapshot/new-committed: must not falsely exit; any re-arm
	// of the stale lower version is dropped by the (V,LR) gate on the next apply.
	t.Run("old-snapshot/new-committed does not falsely exit; stale re-arm is gate-dropped", func(t *testing.T) {
		t.Parallel()
		committed := v2
		m, _ := armDegraded(t, &committed, v1) // committed ahead of snapshot (torn-read shape)
		plantAssignment(t, m, v1)              // refresh keeps snapshot at V1

		m.attemptRecoveryFromDegraded()

		require.NotZero(t, m.degradedSinceNano(),
			"committed-ahead-of-snapshot must NOT be treated as applied — no missed heal")
		// Whatever was re-armed at the stale V1 is dropped once the real V2 is the
		// current snapshot, so it cannot corrupt the heal.
		require.True(t, isApplyResultStale(v1, v2),
			"a stale V1 re-arm is dropped by the (V,LR) gate against the eventual V2 snapshot")
	})
}

func TestAttemptRecovery_RefreshFails_ReturnsBeforeGuard(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, nil, snap)
	// Do NOT plant the key: refreshAssignmentFromNATS's Get fails → return
	// before the guard, no re-arm, stays degraded (whole-bucket-loss shape).

	m.attemptRecoveryFromDegraded()

	require.NotZero(t, m.degradedSinceNano(), "stays degraded when the refresh read fails")
	require.Nil(t, m.stashedApplyRetry.Load(),
		"a failed refresh must not re-arm a bootstrap apply (guard not reached)")
}
