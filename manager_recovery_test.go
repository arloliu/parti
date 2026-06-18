package parti

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
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

// armDegradedWithJS builds a Degraded manager wired to a live JetStream and a
// committed==snapshot assignment (commitment guard passes, refresh succeeds), so
// recovery reaches the heartbeat-bucket backstop. heartbeatBucket names the
// configured heartbeat bucket; create it for the reachable case, leave it absent
// for the unavailable case. reason is stamped as the degrade reason.
func armDegradedWithJS(t *testing.T, reason, heartbeatBucket string, createHeartbeat bool) *Manager {
	t.Helper()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _, _, _ := newTestManager(t)
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	m.js = js
	m.cfg.OperationTimeout = 2 * time.Second
	m.cfg.KVBuckets.HeartbeatBucket = heartbeatBucket
	if createHeartbeat {
		_ = partitest.CreateJetStreamKV(t, nc, heartbeatBucket)
	}
	m.assignmentKV = partitest.CreateJetStreamKV(t, nc, "selfheal-hbbackstop-asgn")

	committed := snap
	m.committedAssignment.Store(&committed)
	m.assignment.Store(snap)
	m.state.Store(int32(StateDegraded))
	m.markDegraded(time.Now().UnixNano(), reason)
	plantAssignment(t, m, snap)

	return m
}

// These tests give the two integration-only recovery-exit conjuncts a fast unit
// characterization: the reason-scoped kv-unavailable heartbeat-after-degrade gate
// (manager_degraded.go:478-486) and the GLOBAL heartbeat-bucket reachability
// backstop (manager_degraded.go:503-506). Both assert the observable State() so
// they survive the recovery-guard-pipeline refactor.

// TestAttemptRecovery_KVUnavailable_HeartbeatGate brackets both directions of
// the kv-unavailable heartbeat-after-degrade gate (manager_degraded.go:478-486)
// in one wantState-driven table. The gate's contract: a kv-unavailable degrade
// must NOT exit on the (unaffected) assignment read until a heartbeat Put
// succeeds AFTER the degrade. A zero or pre-degrade ("stale") heartbeat stamp,
// and a stamp AT the degrade instant (the `<=` boundary), all hold the worker
// Degraded; only a stamp strictly AFTER the degrade proves the failing op
// recovered and exits to Stable. Both directions of the `<=` boundary are kept
// here as a deliberate boundary bracket.
func TestAttemptRecovery_KVUnavailable_HeartbeatGate(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	sinceNano := time.Now().UnixNano()

	tests := []struct {
		name string
		// hbOffset is added to sinceNano to compute the heartbeat success stamp.
		// hasStamp == false leaves the stamp at 0 (never-stamped).
		hasStamp  bool
		hbOffset  int64
		wantState State
		// rationale is asserted via require so each former subtest's exact
		// stay/exit intent stays legible on the row.
		rationale string
	}{
		{
			name:      "never-stamped",
			hasStamp:  false,
			wantState: StateDegraded,
			rationale: "kv-unavailable with no post-degrade heartbeat must stay Degraded",
		},
		{
			// Heartbeat succeeded BEFORE the degrade — does not prove the op recovered.
			name:      "stale",
			hasStamp:  true,
			hbOffset:  -1000,
			wantState: StateDegraded,
			rationale: "kv-unavailable with a pre-degrade (stale) heartbeat must stay Degraded",
		},
		{
			// Heartbeat stamped at EXACTLY the degrade instant. The gate's boundary
			// is `<=`: a success AT the degrade instant (rec.since) does not prove the
			// op recovered AFTER we degraded, so the worker stays Degraded. This pins
			// the <= (vs <) boundary that the stale/fresh cases straddle but never
			// land on.
			name:      "equal-boundary",
			hasStamp:  true,
			hbOffset:  0,
			wantState: StateDegraded,
			rationale: "a heartbeat stamped AT the degrade instant (hbAt == rec.since) must stay Degraded",
		},
		{
			// Heartbeat succeeded AFTER the degrade — the failing op recovered, so
			// recovery exits to Stable (the other gates pass: commitment is satisfied
			// and the js==nil global backstops are inert in this unit harness).
			name:      "fresh",
			hasStamp:  true,
			hbOffset:  1000,
			wantState: StateStable,
			rationale: "kv-unavailable with a post-degrade heartbeat must exit to Stable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, _ := armDegraded(t, &snap, snap)
			plantAssignment(t, m, snap)
			m.markDegraded(sinceNano, DegradeReasonKVUnavailable)
			if tc.hasStamp {
				m.lastHeartbeatSuccessAt.Store(sinceNano + tc.hbOffset)
			} else {
				m.lastHeartbeatSuccessAt.Store(0)
			}

			m.attemptRecoveryFromDegraded()
			// The exit (Stable) row spawns a recovery-grace goroutine; the
			// stay-Degraded rows do not, so only wait when an exit is expected to
			// avoid blocking forever on rows that never call wg.Add.
			if tc.wantState == StateStable {
				m.wg.Wait()
			}

			require.Equal(t, tc.wantState, m.State(), tc.rationale)
		})
	}
}

// TestAttemptRecovery_HeartbeatBucketUnavailable_StaysDegraded_AnyReason locks
// the GLOBAL nature of the heartbeat-bucket backstop: it must block the recovery
// exit for ANY degrade reason while the heartbeat bucket is unreachable, not just
// for kv-unavailable. It backstops the "KV error threshold exceeded" whole-bucket
// reason that Family B (kv-unavailable) and Family A (recreate-only) do not cover.
// Using non-kv-unavailable reasons proves it is not accidentally reason-scoped —
// the T1 cross-reason defense a naive guard registry would silently drop.
func TestAttemptRecovery_HeartbeatBucketUnavailable_StaysDegraded_AnyReason(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping embedded-NATS test in short mode")
	}
	// All non-kv-unavailable: Family B is silent for these, so the heartbeat-bucket
	// backstop is the ONLY conjunct that can hold them Degraded.
	reasons := []string{"KV error threshold exceeded", "NATS connection down", "startup-timeout"}
	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			m := armDegradedWithJS(t, reason, "hb-missing-bucket", false)

			m.attemptRecoveryFromDegraded()

			require.Equal(t, StateDegraded, m.State(),
				"an unreachable heartbeat bucket must keep the worker Degraded for reason %q", reason)
		})
	}
}

// TestAttemptRecovery_HeartbeatBucketReachable_Exits is the opposite direction:
// when the heartbeat bucket IS reachable, the backstop does not block and recovery
// exits to Stable (the other gates pass). This proves the backstop discriminates
// on bucket reachability rather than always blocking.
func TestAttemptRecovery_HeartbeatBucketReachable_Exits(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-NATS test in short mode")
	}
	t.Parallel()
	m := armDegradedWithJS(t, "NATS connection down", "hb-present-bucket", true)

	m.attemptRecoveryFromDegraded()
	m.wg.Wait()

	require.Equal(t, StateStable, m.State(),
		"a reachable heartbeat bucket must let recovery exit to Stable")
}

// TestAttemptRecovery_StreamMissingExhausted_StaysDegraded pins the terminal
// hold for the dynamic-consumer stream-missing route: once a partition
// consumer's recovery envelope has exhausted, its loop has exited and cannot
// restart in-process (the dead subject remains in the worker-consumer's
// subject map, so a re-apply computes an empty diff), and operator stream
// recreation cannot revive it either. Recovery must therefore never exit
// this reason — rotation is the only recovery, matching the
// heartbeat-bucket backstop's terminal contract. Without the gate, the
// connection monitor exits back to Stable within ~one tick (the NATS
// connection never dropped) and the worker reports Stable while assigned
// partitions are silently not consumed.
func TestAttemptRecovery_StreamMissingExhausted_StaysDegraded(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, &snap, snap)
	plantAssignment(t, m, snap)
	m.markDegraded(time.Now().UnixNano(), DegradeReasonStreamMissingRecoveryExhausted)

	// Even with every recovery signal healthy (commitment guard satisfied by
	// armDegraded, fresh heartbeat far in the future), the hold is terminal.
	m.lastHeartbeatSuccessAt.Store(time.Now().UnixNano() + int64(time.Hour))

	m.attemptRecoveryFromDegraded()

	require.Equal(t, StateDegraded, m.State(),
		"stream-missing-recovery-exhausted must hold the worker terminally Degraded for rotation")
}

// TestAttemptRecovery_StreamMissingHold_IsReasonScoped proves the new gate
// is NOT accidentally global: a reason with no blocking gate (startup
// timeout) still exits to Stable through the same pipeline. This is the
// negative-space direction the boundary-test discipline requires.
func TestAttemptRecovery_StreamMissingHold_IsReasonScoped(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, &snap, snap)
	plantAssignment(t, m, snap)
	m.markDegraded(time.Now().UnixNano(), DegradeReasonStartupTimeout)

	m.attemptRecoveryFromDegraded()
	m.wg.Wait()

	require.Equal(t, StateStable, m.State(),
		"a non-stream-missing reason with healthy signals must still exit; the terminal hold must be reason-scoped")
}

// TestAttemptRecovery_StreamMissingExhausted_LatchSurvivesReasonOverlap pins
// the overlap defense: when stream-missing exhaustion fires while the worker
// is ALREADY Degraded for another reason, enterDegraded's CAS no-ops and the
// reason string never records the exhaustion. The atomic latch must keep the
// recovery exit terminal anyway — otherwise the other reason's recovery
// signal (here: a fresh post-degrade heartbeat satisfying the kv-unavailable
// gate) would exit to Stable with a permanently dead partition loop.
func TestAttemptRecovery_StreamMissingExhausted_LatchSurvivesReasonOverlap(t *testing.T) {
	t.Parallel()
	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	m, _ := armDegraded(t, &snap, snap)
	plantAssignment(t, m, snap)
	sinceNano := time.Now().UnixNano()
	m.markDegraded(sinceNano, DegradeReasonKVUnavailable)

	// Exhaustion fires while already Degraded: enterDegraded CAS no-ops, only
	// the latch records it.
	m.onStreamMissingError("ORDERS", errors.New("recovery exhausted"))

	// The kv-unavailable gate's own exit signal is satisfied (fresh heartbeat
	// after the degrade) — without the latch, recovery would exit to Stable.
	m.lastHeartbeatSuccessAt.Store(sinceNano + int64(time.Minute))

	m.attemptRecoveryFromDegraded()

	require.Equal(t, StateDegraded, m.State(),
		"the exhaustion latch must hold the worker Degraded even when the degrade reason belongs to another (recovered) failure")
}

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
	m.markDegraded(m.degradedSinceNano(), DegradeReasonEnumerationStall)

	// No enumeration success since the degrade: the commitment guard is satisfied,
	// but the leader must STAY Degraded — exiting would resume serving stale
	// membership while the enumeration scan is still stalling.
	m.attemptRecoveryFromDegraded()
	require.NotZero(t, m.degradedSinceNano(),
		"a leader whose worker enumeration has not recovered must stay Degraded")
	require.Equal(t, StateDegraded, m.State())

	// Stamp an enumeration success AFTER the degrade instant: the gate opens and the
	// applied leader exits. (Assert synchronously; exitDegraded transitions before
	// the recovery-grace goroutine it spawns matters.)
	m.lastEnumerationSuccessAt.Store(m.degradedSinceNano() + 1)
	m.attemptRecoveryFromDegraded()
	require.Zero(t, m.degradedSinceNano(),
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
	m.markDegraded(m.degradedSinceNano(), DegradeReasonEnumerationStall)
	// lastEnumerationSuccessAt stays 0 — a non-leader runs no enumeration and can
	// never stamp a success. The escape must let it exit anyway.

	m.attemptRecoveryFromDegraded()
	m.wg.Wait() // safe: a non-leader exit skips the recovery-grace goroutine

	require.Zero(t, m.degradedSinceNano(),
		"a non-leader must not be trapped in an enumeration-stall degrade it can never clear")
	require.Equal(t, StateStable, m.State())
}

// TestExitDegraded_OnlyClearsOnConfirmedDegradedTransition pins the
// exit-on-confirmed-Degraded contract: exitDegraded clears the degraded record
// ONLY when it performs a genuine Degraded->Stable transition. It must NOT clear
// the record when the state is not (yet) Degraded — the transient an enterDegraded
// caller is in after it published the record (single-swap CAS) but before it ran
// transitionState(StateDegraded).
//
// Without the fix, exitDegraded calls transitionState(StateStable), which returns
// true vacuously when the state is already Stable (and performs a REAL but wrong
// transition from any other active state, e.g. Scaling->Stable), then unconditionally
// Store(nil)s the record. A concurrent enterer's transitionState(StateDegraded)
// then lands last, stranding the worker in Degraded with a nil record — recovery
// and alerting both early-return on a nil record, so it cannot self-heal until an
// unrelated degrade re-arms it.
//
// enterDegraded is reachable from every active state (isValidTransition allows
// *->Degraded from Stable/Scaling/Rebalancing/Emergency/WaitingAssignment), so the
// pre-transition window can be entered from any of them — hence the table.
func TestExitDegraded_OnlyClearsOnConfirmedDegradedTransition(t *testing.T) {
	t.Parallel()

	// Every state an enterDegraded caller can be in during the pre-transition
	// window (record published, transitionState(StateDegraded) not yet run).
	preTransitionStates := []State{
		StateStable,
		StateScaling,
		StateRebalancing,
		StateEmergency,
		StateWaitingAssignment,
	}

	for _, start := range preTransitionStates {
		t.Run(start.String(), func(t *testing.T) {
			t.Parallel()

			var hookFired atomic.Bool
			m := newHookTestManager(&types.Hooks{
				OnStateChanged: func(_ context.Context, _, _ types.State) error {
					hookFired.Store(true)
					return nil
				},
			})
			defer m.cancel()

			// Arm the racy transient: a degraded record is live, but the state is
			// NOT Degraded (the enterer has not transitioned yet).
			m.markDegraded(time.Now().UnixNano(), "startup-timeout")
			m.state.Store(int32(start))

			m.exitDegraded()
			m.wg.Wait()

			require.NotNil(t, m.degraded.Load(),
				"exitDegraded must NOT clear the record when state is %s (not Degraded); "+
					"clearing it strands a concurrent enterer in Degraded+nil-record", start)
			require.Equal(t, start, m.State(),
				"exitDegraded must not perform a spurious transition out of %s when not Degraded", start)
			require.False(t, hookFired.Load(),
				"exitDegraded must not emit OnStateChanged when it did not transition out of %s", start)
		})
	}
}

// TestExitDegraded_GenuineExitStillWorks is the positive-path guard: a real
// Degraded->Stable exit still clears the record, transitions to Stable, and emits
// the OnStateChanged(Degraded, Stable) effect — i.e. the fix does not weaken the
// legitimate recovery path.
func TestExitDegraded_GenuineExitStillWorks(t *testing.T) {
	t.Parallel()

	var gotFrom, gotTo atomic.Int32
	var fired atomic.Bool
	hooks := &types.Hooks{
		OnStateChanged: func(_ context.Context, from, to types.State) error {
			gotFrom.Store(int32(from))
			gotTo.Store(int32(to))
			fired.Store(true)
			return nil
		},
	}

	m := newHookTestManager(hooks)
	defer m.cancel()

	m.markDegraded(time.Now().UnixNano(), DegradeReasonNATSConnectionDown)
	m.state.Store(int32(StateDegraded))

	m.exitDegraded()
	m.wg.Wait()

	require.Nil(t, m.degraded.Load(), "genuine exit must clear the degraded record")
	require.Equal(t, StateStable, m.State(), "genuine exit must transition to Stable")
	require.True(t, fired.Load(), "genuine exit must emit OnStateChanged")
	require.Equal(t, StateDegraded, State(gotFrom.Load()), "OnStateChanged.from must be Degraded")
	require.Equal(t, StateStable, State(gotTo.Load()), "OnStateChanged.to must be Stable")
}

// TestNP5_BlockedApplyStartupTimeout_RecoversToStableAfterUnblock is the
// unit-level proof of the "...unless the runner recovers" clause of the
// startup-timeout taxonomy.
//
// Scenario:
//  1. A handoff Apply blocks during startup while the manager sits in
//     StateWaitingAssignment. The startup watchdog fires after StartupTimeout,
//     driving enterDegraded("startup-timeout").
//  2. When the blocked Apply finally completes (releaseFirst), the runner's
//     own self-exit path (casToStableFromWaitingAssignment, invoked via
//     markStartupAssignmentApplied) CANNOT promote to Stable: its CAS only
//     succeeds from WaitingAssignment, but the state is now Degraded. So the
//     successful apply does NOT silently undo the degraded entry.
//  3. attemptRecoveryFromDegraded heals the SAME process: it refreshes the
//     assignment from KV (the planted V=1 snapshot, identical (V, LR, digest)
//     to what the apply committed), sees currentAssignmentApplied == true, and
//     calls exitDegraded — transitioning Degraded -> Stable and clearing
//     the degraded record.
//
// The asserted invariants hinge on real state transitions and the committed
// assignment, never on a bare timeout. Predicted verdict: PASS.
func TestNP5_BlockedApplyStartupTimeout_RecoversToStableAfterUnblock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-NATS test in short mode")
	}
	t.Parallel()

	m, rh, _, _ := newTestManager(t)

	// Wire a real KV bucket so refreshAssignmentFromNATS (called by recovery)
	// can read the worker's planted assignment.
	_, nc := partitest.StartEmbeddedNATS(t)
	m.assignmentKV = partitest.CreateJetStreamKV(t, nc, "np5-asgn")

	// The assignment the blocked Apply commits. plantAssignment writes the SAME
	// (V, LR, digest) to the KV key so the recovery refresh produces a snapshot
	// that currentAssignmentApplied matches against committedAssignment. NOTE:
	// plantAssignment writes KV ONLY — it leaves m.assignment at the zero
	// Assignment{} that newTestManager stored, so the V=1 apply candidate passes
	// the (V, LR) stale gate and actually reaches the barrier inside Apply.
	np5Asgn := Assignment{
		Version:        1,
		LeaderRevision: 5,
		Partitions:     []Partition{{Keys: []string{"p0"}}},
	}
	plantAssignment(t, m, np5Asgn)

	// Configure the watchdog: a 100ms StartupTimeout fires quickly while the
	// Apply is parked on the barrier. Suppress the degraded alert monitor's
	// ticker (a zero AlertInterval panics; time.Hour never fires in-test).
	m.cfg.StartupTimeout = 100 * time.Millisecond
	m.cfg.DegradedAlert.AlertInterval = time.Hour
	// startStartupTimeoutWatchdog computes its deadline from m.startedAt.
	m.startedAt = time.Now()

	// Record OnDegraded reasons (hook fires from a goroutine; guard via
	// atomic.Value-backed []string copy, per the jitter-startup sibling).
	var np5DegradedReasons atomic.Value
	np5DegradedReasons.Store([]string{})
	m.hooks.OnDegraded = func(_ context.Context, reason string) error {
		prev, _ := np5DegradedReasons.Load().([]string)
		updated := append(append([]string{}, prev...), reason)
		np5DegradedReasons.Store(updated)

		return nil
	}

	// Arm the barrier: the FIRST Apply call signals firstApplyReady then blocks
	// on releaseFirst.
	rh.blockFirstApply.Store(true)

	// Drive state to WaitingAssignment (mirrors prepareStart's transitions).
	require.True(t, m.transitionState(StateClaimingID))
	require.True(t, m.transitionState(StateElection))
	require.True(t, m.transitionState(StateWaitingAssignment))

	// Launch the apply goroutine. applyAssignment routes through the core,
	// which acquires applyStoreMu and blocks inside the coordinator Apply
	// barrier — holding the manager in WaitingAssignment for the watchdog.
	// applyDone closes after Apply unblocks AND the full commit path (snapshot
	// Store, committedAssignment.Store, applyStoreMu.Unlock) has run, so the
	// recovery refresh below never contends for applyStoreMu.
	np5ApplyDone := make(chan struct{})
	go func() {
		defer close(np5ApplyDone)
		_ = m.applyAssignment(np5Asgn)
	}()

	// Gate: ensure the Apply actually parked on the barrier before starting the
	// watchdog. A spurious pass (Apply never blocked) would fatal here.
	select {
	case <-rh.firstApplyReady:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Apply did not enter the barrier within 500ms — test cannot prove the blocked-apply scenario")
	}

	// Start the watchdog. It fires Degraded("startup-timeout") at ~100ms while
	// the Apply is still parked.
	m.startStartupTimeoutWatchdog()

	// ASSERTION 1 — WATCHDOG FIRES: state goes Degraded with reason
	// "startup-timeout".
	require.Eventually(t, func() bool {
		if m.State() != StateDegraded {
			return false
		}
		reasons, _ := np5DegradedReasons.Load().([]string)

		return slices.Contains(reasons, "startup-timeout")
	}, 2*time.Second, 10*time.Millisecond,
		"watchdog must drive Degraded('startup-timeout') while the startup Apply is blocked")

	// Release the barrier. The Apply now completes: it Stores the snapshot,
	// records committedAssignment (V=1), releases applyStoreMu, and calls
	// markStartupAssignmentApplied — whose casToStableFromWaitingAssignment CAS
	// FAILS because the state is Degraded, not WaitingAssignment.
	close(rh.releaseFirst)

	// Wait for the apply goroutine to fully finish so the recovery refresh below
	// cannot contend for applyStoreMu (and so committedAssignment is visible).
	select {
	case <-np5ApplyDone:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Apply did not complete after release")
	}

	// ASSERTION 2 — APPLY COMMITTED: committedAssignment advanced to V=1
	// (the apply's full success path ran past the unblock).
	require.Eventually(t, func() bool {
		return m.committedAssignmentOrEmpty().Version == 1
	}, 2*time.Second, 10*time.Millisecond,
		"the unblocked Apply must commit V=1")

	// ASSERTION 3 — RUNNER DID NOT SELF-EXIT: immediately before recovery, the
	// state is STILL Degraded. The successful apply's
	// casToStableFromWaitingAssignment CAS could not fire from Degraded, so the
	// watchdog's degraded entry is intact.
	require.Equal(t, StateDegraded, m.State(),
		"a successful apply must NOT self-promote out of Degraded — casToStableFromWaitingAssignment CAS fails from Degraded")

	// ASSERTION 4 — RECOVERY HEALS THE SAME PROCESS: attemptRecoveryFromDegraded
	// refreshes the (identical) planted assignment, finds the current snapshot
	// applied, and exits to Stable. Called strictly AFTER the apply completed so
	// the refresh's monotonicStore does not deadlock on applyStoreMu.
	m.attemptRecoveryFromDegraded()

	require.Equal(t, StateStable, m.State(),
		"recovery must heal the blocked-apply worker from Degraded to Stable")
	require.Zero(t, m.degradedSinceNano(),
		"exitDegraded must clear the degraded record on recovery")

	// ASSERTION 5 — NO FLAP / SINGLE ENTRY: exactly one "startup-timeout" reason
	// was recorded (the watchdog fired once), and no apply retry was stashed
	// (the apply succeeded, so the failure-path scheduleApplyRetry never ran and
	// the applied-current recovery guard did not re-arm).
	reasons, _ := np5DegradedReasons.Load().([]string)
	np5Count := 0
	for _, r := range reasons {
		if r == "startup-timeout" {
			np5Count++
		}
	}
	require.Equal(t, 1, np5Count, "watchdog must enter startup-timeout Degraded exactly once (no flap)")
	require.Nil(t, m.stashedApplyRetry.Load(),
		"an applied worker must not re-arm an apply retry during recovery")
}
