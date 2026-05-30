// Package durable — Tier 0 regression guard for the auto-healing quorum-loss incident.
//
// Incident: a KV bucket that lost quorum while the NATS connection stayed
// CONNECTED produced a Keys()-ok / Get()-fail reconcile window. reconcileOnce
// continued without adding the pid to `seen`, so the tombstone pass staged a
// synthetic delete at R+1 (cache revision + 1). applyPendingBatch applied it
// because R+1 > R. After recovery the real claim returned at R; both the
// watcher and reconciler tried to re-apply it at R, but the guard
// (existing.revision >= p.revision = R+1 >= R = true) rejected it. The
// tombstone won permanently. Only a process restart (warm() re-reading KV
// from scratch) cleared it.
//
// Plan: docs/plans/auto-healing-quorum-loss-repro/
//
//	02-consolidated-design.md  §2 Defect 2, §4 Tier 0
//	03-execution-model-effort.md §3 control pairings
//
// All tests are deterministic and require no NATS server.
package durable

import (
	"context"
	"testing"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// ─── Mock extension ───────────────────────────────────────────────────────────
//
// mockKVForReconcile (defined in claim_resolver_restart_test.go) is extended
// with two nil-default fields:
//
//   getErrByKey map[string]error — if a key has an entry, Get returns that
//     error while Keys() STILL lists the key (the asymmetric window).
//   keysErr error — if set, Keys() returns this error immediately.
//
// Both fields are set by direct field assignment (consistent with the existing
// afterKeys hook pattern). No existing call sites pass them, so they default
// to nil and the behaviour of existing tests is unchanged.
//
// The Get() implementation in claim_resolver_restart_test.go does not check
// getErrByKey; we shadow it here with a promoted-type wrapper so the existing
// struct definition needs no modification. See mockKVWithKeyErr below.

// mockKVWithKeyErr wraps mockKVForReconcile and adds per-key error injection
// and a Keys-level error, while keeping the existing store/revision/afterKeys
// fields for all other behaviour. It is the mock used in EVERY quorum-loss test.
//
// Invariant: getErrByKey[k] != nil → Get(k) returns that error even though
// Keys() still lists k. This is exactly the asymmetric Keys-ok/Get-fail
// window that reconcileOnce cannot handle.
type mockKVWithKeyErr struct {
	*mockKVForReconcile
	getErrByKey map[string]error // optional per-key error override for Get
	keysErr     error            // if set, Keys() returns this error
}

func newMockKVWithKeyErr(store map[string][]byte, revision uint64) *mockKVWithKeyErr {
	return &mockKVWithKeyErr{
		mockKVForReconcile: newMockKVForReconcile(store, revision),
	}
}

// Keys overrides mockKVForReconcile.Keys so that keysErr can be returned.
// The afterKeys hook is still honoured.
func (m *mockKVWithKeyErr) Keys(ctx context.Context, opts ...jetstream.WatchOpt) ([]string, error) {
	if m.keysErr != nil {
		return nil, m.keysErr
	}
	return m.mockKVForReconcile.Keys(ctx, opts...)
}

// Get overrides mockKVForReconcile.Get to inject per-key errors. If a key has
// an entry in getErrByKey the error is returned instead of the store value.
// The key deliberately stays in store (i.e. Keys() still lists it), modelling
// the asymmetric window: Keys returns the key, but Get times out.
func (m *mockKVWithKeyErr) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if m.getErrByKey != nil {
		if err, hit := m.getErrByKey[key]; hit {
			return nil, err
		}
	}
	return m.mockKVForReconcile.Get(ctx, key)
}

// ─── Shared construction helpers ─────────────────────────────────────────────

const (
	quorumTestPID      = "USER21"
	quorumTestFullKey  = "claims/" + quorumTestPID
	quorumTestOwner    = "worker-A"
	quorumTestEpoch    = int64(1)
	quorumTestRevision = uint64(5) // R = 5; tombstone will land at R+1 = 6
)

// quorumTestClaim returns marshalled bytes for the standard live claim used
// across tests.
func quorumTestClaim(t *testing.T) []byte {
	t.Helper()
	return marshalClaim(t, handoff.Claim{
		PartitionID: quorumTestPID,
		Owner:       quorumTestOwner,
		State:       handoff.ClaimStateStable,
		Epoch:       quorumTestEpoch,
	})
}

// newHealthyKV returns a mock with a live claim at R, no error injection.
func newHealthyKV(t *testing.T) *mockKVWithKeyErr {
	t.Helper()
	return newMockKVWithKeyErr(map[string][]byte{
		quorumTestFullKey: quorumTestClaim(t),
	}, quorumTestRevision)
}

// seedResolverCache seeds the resolver's in-memory cache directly (bypassing
// Start/warm) with the standard single-pid live claim at quorumTestRevision.
// This mirrors the pattern used by
// TestClaimResolver_ReconcileDoesNotRegressLaterWatcherUpdates.
func seedResolverCache(r *ClaimBasedResolver) {
	m := map[string]claimEntry{
		quorumTestPID: {
			owner:    quorumTestOwner,
			state:    toState(handoff.ClaimStateStable),
			epoch:    quorumTestEpoch,
			revision: quorumTestRevision,
		},
	}
	r.cache.Store(&m)
}

// ─── Tests ────────────────────────────────────────────────────────────────────

// TestQuorumLoss_HealthyReconcile_Control is the mandatory false-green control.
//
// Setup: Keys-ok and Get-ok at R. After reconcileOnce the cache must retain
// the live claim (ok=true, correct owner). Case A below DIFFERS from this
// control by EXACTLY ONE variable: that key's Get fails. This pairing proves
// the Get-fail is the specific cause of the tombstone — if the control passed
// but Case A asserted ok=false for an unrelated setup reason, we would never
// know the tombstone was from the fault and not from something else.
func TestQuorumLoss_HealthyReconcile_Control(t *testing.T) {
	kv := newHealthyKV(t)
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))

	// Seed cache at R=5 (same as quorumTestRevision).
	seedResolverCache(r)

	r.reconcileOnce(context.Background())

	owner, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok, "healthy reconcile must not tombstone a live claim")
	require.Equal(t, quorumTestOwner, owner, "owner must be unchanged after healthy reconcile")
}

// TestQuorumLoss_CaseA_GetFailCreatesUnbeatableTombstone is the primary
// defect pin: Keys-ok / Get-fail for a single pid causes a permanent
// tombstone that neither reconcile nor the watcher can overcome.
//
// Non-vacuous because:
//   - The healthy-reconcile control above differs by exactly one variable
//     (Get-ok vs Get-fail) and asserts ok=true. Removing getErrByKey from this
//     test would flip the final assertion to ok=true — proven by the control.
//   - After tombstone, both read-paths (reconcile AND watcher) are exercised
//     and both fail to clear it: the revision guard R+1 >= R rejects both.
//   - The restart control at the end proves warm() over a healthy KV returns
//     ok=true — confirming process restart is the only fix.
func TestQuorumLoss_CaseA_GetFailCreatesUnbeatableTombstone(t *testing.T) {
	// Step 1 — fault setup: Keys returns the key, Get returns DeadlineExceeded.
	// This is the ONLY difference from TestQuorumLoss_HealthyReconcile_Control.
	kv := newHealthyKV(t)
	kv.getErrByKey = map[string]error{
		quorumTestFullKey: context.DeadlineExceeded,
	}

	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))

	// Seed cache at R=5 so the resolver believes there is a live claim.
	seedResolverCache(r)

	// Step 2 — trigger the reconcile under fault conditions.
	// reconcileOnce: Keys() ok → Get() fails → pid not in seen →
	// tombstone staged at R+1=6 → applyPendingBatch writes {deleted:true, rev:6}.
	r.reconcileOnce(context.Background())

	// The tombstone must now be in place.
	_, _, _, ok := r.GetOwner(quorumTestPID)
	require.False(t, ok, "fault reconcile must have tombstoned the claim")

	// Step 3 — simulate recovery: restore Get to return the live claim at R=5.
	kv.getErrByKey = nil // Get is healthy again

	// Step 3a — reconcile read-path: reconcileOnce now sees the live claim at
	// R=5 via Get, but the reconcile PRE-FILTER (claim_resolver.go:1010-1014)
	// drops it because the cached tombstone's revision (6) >= entry revision (5),
	// so no upsert is ever staged and applyPendingBatch gets an empty batch. The
	// tombstone persists. NOTE this is a DISTINCT guard from the applyPendingBatch
	// guard exercised by step 3b below — Case A covers both sites.
	r.reconcileOnce(context.Background())
	_, _, _, ok = r.GetOwner(quorumTestPID)
	require.False(t, ok,
		// Non-vacuous: we just ran reconcileOnce with a healthy KV. If the
		// reconcile pre-filter were wrong (staged R over the R+1 tombstone),
		// this would be ok=true.
		"reconcile read-path at R must NOT beat the tombstone at R+1",
	)

	// Step 3b — watcher read-path: deliver the live claim at R=5 via
	// handleWatcherUpdate + applyPendingBatch. handleWatcherUpdate stages the
	// upsert unconditionally, so here the applyPendingBatch guard (:865,
	// existing.revision 6 >= p.revision 5) is what rejects it — the same >=
	// semantics as 3a's pre-filter but a different code site.
	pendingMap := make(map[string]pending)
	watcherEntry := &mockKVEntryFull{
		key:      quorumTestFullKey,
		val:      quorumTestClaim(t),
		revision: quorumTestRevision, // R=5 < tombstone R+1=6
	}
	r.handleWatcherUpdate(watcherEntry, pendingMap)
	r.applyPendingBatch(pendingMap, "test-watcher-recovery")

	_, _, _, ok = r.GetOwner(quorumTestPID)
	require.False(t, ok,
		// Non-vacuous: we just delivered the live claim via the watcher path.
		// If the guard were wrong (accepted R over R+1), this would be ok=true.
		"watcher read-path at R must NOT beat the tombstone at R+1",
	)

	// Step 4 — restart control: a fresh resolver calling warm() over the now-
	// healthy KV reads KV directly (no in-memory tombstone) and returns ok=true.
	// This proves a process restart IS the fix, as reported in the incident.
	r2 := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	err := r2.warm(context.Background())
	require.NoError(t, err)
	owner, _, _, ok2 := r2.GetOwner(quorumTestPID)
	require.True(t, ok2, "fresh warm() over healthy KV must return ok=true (restart fixes it)")
	require.Equal(t, quorumTestOwner, owner)
}

// TestQuorumLoss_CaseAPrime_FleetWideTombstone verifies that if ALL pids' Get
// calls fail in a single reconcile pass, ALL pids are tombstoned.
//
// This models the fleet-wide symptom from the incident report where all workers
// stopped processing after a bucket quorum loss affecting all keys.
//
// Non-vacuous: we assert ok=true for ALL pids immediately after seeding (before
// the fault reconcile). After the fault reconcile we assert ALL pids are ok=false.
// Without the pre-fault presence check, the post-fault absence could pass
// vacuously (cache never populated).
func TestQuorumLoss_CaseAPrime_FleetWideTombstone(t *testing.T) {
	const numPIDs = 3
	pids := []string{"USER01", "USER02", "USER03"}

	store := make(map[string][]byte, numPIDs)
	for _, pid := range pids {
		store["claims/"+pid] = marshalClaim(t, handoff.Claim{
			PartitionID: pid,
			Owner:       "worker-" + pid,
			State:       handoff.ClaimStateStable,
			Epoch:       1,
		})
	}

	kv := newMockKVWithKeyErr(store, quorumTestRevision)

	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))

	// Seed cache with all pids at R=5.
	seedMap := make(map[string]claimEntry, numPIDs)
	for _, pid := range pids {
		seedMap[pid] = claimEntry{
			owner:    "worker-" + pid,
			state:    toState(handoff.ClaimStateStable),
			epoch:    1,
			revision: quorumTestRevision,
		}
	}
	r.cache.Store(&seedMap)

	// Non-vacuous pre-fault check: all pids present before the fault.
	for _, pid := range pids {
		_, _, _, ok := r.GetOwner(pid)
		require.True(t, ok, "pre-fault: pid %s must be present", pid)
	}

	// Inject fault: ALL pids' Get returns DeadlineExceeded.
	kv.getErrByKey = make(map[string]error, numPIDs)
	for _, pid := range pids {
		kv.getErrByKey["claims/"+pid] = context.DeadlineExceeded
	}

	// Trigger the fault reconcile.
	r.reconcileOnce(context.Background())

	// Assert ALL pids are tombstoned (non-vacuous because we proved them present above).
	for _, pid := range pids {
		_, _, _, ok := r.GetOwner(pid)
		require.False(t, ok, "post-fault: pid %s must be tombstoned after all-Get-fail", pid)
	}
}

// TestQuorumLoss_CaseADoublePrime_KVRewriteBeatsTheTombstone verifies that
// a claim re-write at revision R+2 (strictly greater than the tombstone at R+1)
// DOES beat the tombstone and restores the gate to open.
//
// This tells the fix authors that forcing a claim re-write at a new revision
// (e.g., a triggered re-apply or a handoff coordinator that re-writes on
// recovery) is a viable fix shape — without requiring a process restart.
//
// Non-vacuous: we assert ok=false AFTER the tombstone (intermediate state)
// before delivering R+2. The final ok=true can only pass because the R+2
// write was accepted, NOT because the tombstone was absent.
func TestQuorumLoss_CaseADoublePrime_KVRewriteBeatsTheTombstone(t *testing.T) {
	// Step 1 — reproduce the tombstone at R+1 (same as Case A steps 1-2).
	kv := newHealthyKV(t)
	kv.getErrByKey = map[string]error{
		quorumTestFullKey: context.DeadlineExceeded,
	}

	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	seedResolverCache(r)

	r.reconcileOnce(context.Background())

	// Non-vacuous intermediate check: tombstone is in place before the heal.
	_, _, _, ok := r.GetOwner(quorumTestPID)
	require.False(t, ok, "tombstone must be in place before the heal attempt")

	// Step 2 — deliver a claim re-write at R+2=7 via the watcher path.
	// R+2=7 > tombstone=6 → guard: 6 >= 7 = false → write accepted.
	const healRevision = quorumTestRevision + 2 // R+2 = 7
	pendingMap := make(map[string]pending)
	rewriteEntry := &mockKVEntryFull{
		key:      quorumTestFullKey,
		val:      quorumTestClaim(t),
		revision: healRevision,
	}
	r.handleWatcherUpdate(rewriteEntry, pendingMap)
	r.applyPendingBatch(pendingMap, "test-rewrite-heal")

	// The re-write at R+2 must beat the tombstone at R+1.
	owner, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok,
		// Non-vacuous: we asserted ok=false immediately above (tombstone present).
		// This ok=true can only result from the R+2 write being accepted.
		"claim re-write at R+2 must beat the tombstone at R+1",
	)
	require.Equal(t, quorumTestOwner, owner)
}

// TestQuorumLoss_CaseB_KeysFailDoesNotPoison verifies the boundary between the
// two fault branches: when Keys() ITSELF fails (not just per-key Get),
// reconcileOnce takes the early-return path and leaves the cache untouched.
//
// This is the benign branch — the one the incident logs DID show
// ("reconcile … list keys failed"). The dangerous branch is Case A (Keys-ok /
// Get-fail). This test proves the two branches produce opposite outcomes.
//
// Non-vacuous: the presence assertion before reconcileOnce proves the cache was
// genuinely populated. The post-reconcile ok=true cannot pass vacuously (it
// would fail if reconcileOnce had poisoned the cache). Contrast with Case A:
// identical setup, only the fault type differs (Keys-fail vs Get-fail), and the
// outcome is the opposite (ok=true vs ok=false).
func TestQuorumLoss_CaseB_KeysFailDoesNotPoison(t *testing.T) {
	kv := newHealthyKV(t)
	// Keys() returns DeadlineExceeded — the EARLY-RETURN branch in reconcileOnce.
	// Note: must NOT use an error containing "no keys found" — that branch
	// sets keys=nil and proceeds into the tombstone pass instead of returning.
	kv.keysErr = context.DeadlineExceeded

	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	seedResolverCache(r)

	// Non-vacuous pre-reconcile check: cache is genuinely populated.
	owner, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok, "pre-reconcile: cache must be populated (non-vacuous check)")
	require.Equal(t, quorumTestOwner, owner)

	// reconcileOnce: Keys() fails → early return (lines ~978-984 in production
	// code). The cache must be untouched.
	r.reconcileOnce(context.Background())

	owner, _, _, ok = r.GetOwner(quorumTestPID)
	require.True(t, ok,
		// Non-vacuous: we seeded the cache above AND confirmed it was present.
		// If reconcileOnce had touched the tombstone pass it would be ok=false.
		// The early-return prevents any cache modification — the claim survives.
		"Keys() failure must take the early-return path and NOT poison the cache",
	)
	require.Equal(t, quorumTestOwner, owner)
}
