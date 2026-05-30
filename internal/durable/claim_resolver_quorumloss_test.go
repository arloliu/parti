// Package durable — Tier 0 regression guard for the auto-healing quorum-loss incident.
//
// Incident: a KV bucket that lost quorum while the NATS connection stayed
// CONNECTED produced a Keys()-ok / Get()-fail reconcile window. reconcileOnce
// continued without adding the pid to `seen`, so the tombstone pass staged a
// synthetic delete at R+1 (cache revision + 1) — turning a transient read
// failure into a permanent, monotonic-revision-irreversible tombstone that only
// a process restart could clear.
//
// Fix (F-D2a): reconcileOnce now distinguishes a listed-but-unreadable key (Get
// errored) from a genuinely-gone key. A read error adds the pid to an
// `unreadable` set, and the tombstone pass skips unreadable pids — it never
// tombstones a key it simply could not read this pass. Genuine deletions (key
// absent from Keys, or Get returning a delete/purge op) still tombstone.
//
// These tests pin BOTH directions: the read-fault case must NOT poison (the
// flipped reproducer), and every genuine-deletion case must STILL tombstone (so
// the fix is not over-broad).
//
// Plan: docs/plans/auto-healing-quorum-loss-fix/00-fix-plan.md §1 (F-D2a)
//
//	docs/plans/auto-healing-quorum-loss-repro/02-consolidated-design.md §2 Defect 2, §4 Tier 0
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
// with three nil-default fields:
//
//   getErrByKey map[string]error — if a key has an entry, Get returns that
//     error while Keys() STILL lists the key (the asymmetric window).
//   getOpByKey map[string]jetstream.KeyValueOp — if a key has an entry, the
//     entry Get returns has its Operation() overridden (surfacing a listed key
//     as a genuine delete/purge).
//   keysErr error — if set, Keys() returns this error immediately.
//
// All fields are set by direct field assignment (consistent with the existing
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
	getErrByKey map[string]error                // optional per-key error override for Get
	getOpByKey  map[string]jetstream.KeyValueOp // optional per-key Operation() override for Get
	keysErr     error                           // if set, Keys() returns this error
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

// Get overrides mockKVForReconcile.Get to inject per-key errors and per-key
// operation overrides. If a key has an entry in getErrByKey the error is
// returned instead of the store value (the key deliberately stays in store, so
// Keys() still lists it — the asymmetric Keys-ok/Get-fail window). If a key has
// an entry in getOpByKey the returned entry's Operation() is overridden (so a
// listed key can surface as a genuine delete/purge tombstone via Get).
func (m *mockKVWithKeyErr) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if m.getErrByKey != nil {
		if err, hit := m.getErrByKey[key]; hit {
			return nil, err
		}
	}
	entry, err := m.mockKVForReconcile.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if m.getOpByKey != nil {
		if op, hit := m.getOpByKey[key]; hit {
			return &mockKVEntryFull{
				key:      entry.Key(),
				val:      entry.Value(),
				revision: entry.Revision(),
				op:       op,
			}, nil
		}
	}

	return entry, nil
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

// TestQuorumLoss_CaseA_GetFailDoesNotPoison is the primary F-D2a pin and the
// flipped reproducer: a Keys-ok / Get-fail window for a single pid must NOT
// tombstone the live claim. The transient read failure adds the pid to the
// `unreadable` set; the tombstone pass skips it; the cached claim survives.
//
// This is the RED→GREEN flip: on the pre-fix code this same fault staged a
// synthetic delete at R+1 and the final assertion would be ok=false. The fix
// keeps it ok=true.
//
// Non-vacuous because:
//   - The healthy-reconcile control above differs by exactly one variable
//     (Get-ok vs Get-fail) and also asserts ok=true. The pairing isolates the
//     Get-fail as the variable under test; the fix makes both outcomes equal.
//   - Step 3 proves the claim was never lost: after Get recovers, a later
//     reconcile still resolves ok=true with the correct owner — i.e. "we just
//     couldn't read it this pass; a later pass re-reads it", no restart needed.
func TestQuorumLoss_CaseA_GetFailDoesNotPoison(t *testing.T) {
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
	// reconcileOnce: Keys() ok → Get() fails → pid added to `unreadable` →
	// tombstone pass skips it → no synthetic delete staged → cache untouched.
	r.reconcileOnce(context.Background())

	owner, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok,
		// Non-vacuous: the healthy control differs only in Get-ok vs Get-fail.
		// On pre-fix code this asserted ok=false (the R+1 tombstone). The fix
		// keeps the unreadable claim live.
		"a listed-but-unreadable claim must NOT be tombstoned by a transient Get failure",
	)
	require.Equal(t, quorumTestOwner, owner, "owner must be unchanged after an unreadable-key reconcile")

	// Step 3 — recovery: restore Get, run another reconcile. The claim was never
	// lost, so it still resolves ok=true at the correct owner. No restart needed.
	kv.getErrByKey = nil
	r.reconcileOnce(context.Background())
	owner, _, _, ok = r.GetOwner(quorumTestPID)
	require.True(t, ok, "claim must remain resolvable after the read fault recovers")
	require.Equal(t, quorumTestOwner, owner)
}

// TestQuorumLoss_CaseAPrime_FleetWideReadFaultDoesNotPoison verifies that if
// ALL pids' Get calls fail in a single reconcile pass, NONE are tombstoned —
// every pid stays resolvable.
//
// This models the fleet-wide symptom from the incident report where all workers
// stopped processing after a bucket quorum loss affecting all keys. With F-D2a
// the whole fleet survives the transient read fault.
//
// Non-vacuous: we assert ok=true for ALL pids immediately after seeding (before
// the fault reconcile) AND after it. The pre-fault check proves the cache was
// genuinely populated; on pre-fix code the post-fault assertion was ok=false for
// every pid, so the ok=true here is a real flip, not a vacuous pass.
func TestQuorumLoss_CaseAPrime_FleetWideReadFaultDoesNotPoison(t *testing.T) {
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

	// Assert ALL pids survive (non-vacuous because we proved them present above
	// and pre-fix code tombstoned every one of them).
	for _, pid := range pids {
		_, _, _, ok := r.GetOwner(pid)
		require.True(t, ok, "post-fault: pid %s must survive an all-Get-fail reconcile", pid)
	}
}

// TestQuorumLoss_CaseADoublePrime_KVRewriteBeatsTheTombstone verifies that
// a claim re-write at revision R+2 (strictly greater than the tombstone at R+1)
// DOES beat the tombstone and restores the gate to open.
//
// This documents the monotonic-revision guard: a re-write at a strictly greater
// revision is the only thing that clears a tombstone in-process. The tombstone
// here is manufactured via a GENUINE delete-op (Get returns a KeyValueDelete) —
// the F-D2a fix only spares transient READ failures, so a real deletion still
// tombstones, which is exactly what this test relies on.
//
// Non-vacuous: we assert ok=false AFTER the tombstone (intermediate state)
// before delivering R+2. The final ok=true can only pass because the R+2
// write was accepted, NOT because the tombstone was absent.
func TestQuorumLoss_CaseADoublePrime_KVRewriteBeatsTheTombstone(t *testing.T) {
	// Step 1 — manufacture a genuine tombstone at R+1: Keys() lists the key but
	// Get() returns a delete operation, so the reconcile tombstone pass stages a
	// synthetic delete at R+1.
	kv := newHealthyKV(t)
	kv.getOpByKey = map[string]jetstream.KeyValueOp{
		quorumTestFullKey: jetstream.KeyValueDelete,
	}

	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	seedResolverCache(r)

	r.reconcileOnce(context.Background())

	// Non-vacuous intermediate check: tombstone is in place before the heal.
	_, _, _, ok := r.GetOwner(quorumTestPID)
	require.False(t, ok, "a genuine delete-op must tombstone the claim before the heal attempt")

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
// This is the branch the incident logs DID show ("reconcile … list keys
// failed"). Post-F-D2a it and Case A both leave the claim live (ok=true), but by
// DIFFERENT mechanisms: Keys-fail returns early so the tombstone pass never
// runs, whereas Keys-ok/Get-fail runs the tombstone pass but skips the pid via
// the `unreadable` set. This test pins the early-return mechanism specifically.
//
// Non-vacuous: the presence assertion before reconcileOnce proves the cache was
// genuinely populated. The post-reconcile ok=true cannot pass vacuously (it
// would fail if reconcileOnce had poisoned the cache via the tombstone pass).
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

// ─── F-D2a boundary table: genuine deletions must STILL tombstone ─────────────
//
// These guard that the fix is not over-broad: it spares ONLY a transient read
// failure (Get errored). Every genuine-deletion signal still tombstones. They
// pass on pre-fix code too — they are preservation guards, not RED flips — and
// turn red if the fix were widened to skip tombstoning for ANY non-`seen` pid.

// TestQuorumLoss_Boundary_GetDeleteOpStillTombstones: Keys lists the key, Get
// returns a KeyValueDelete op → a genuine deletion → still tombstoned.
func TestQuorumLoss_Boundary_GetDeleteOpStillTombstones(t *testing.T) {
	kv := newHealthyKV(t)
	kv.getOpByKey = map[string]jetstream.KeyValueOp{
		quorumTestFullKey: jetstream.KeyValueDelete,
	}
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	seedResolverCache(r)

	_, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok, "pre-reconcile: claim must be present (non-vacuous)")

	r.reconcileOnce(context.Background())

	_, _, _, ok = r.GetOwner(quorumTestPID)
	require.False(t, ok, "a Get delete-op is a genuine deletion and must still tombstone")
}

// TestQuorumLoss_Boundary_GetPurgeOpStillTombstones: same as above with a
// KeyValuePurge op.
func TestQuorumLoss_Boundary_GetPurgeOpStillTombstones(t *testing.T) {
	kv := newHealthyKV(t)
	kv.getOpByKey = map[string]jetstream.KeyValueOp{
		quorumTestFullKey: jetstream.KeyValuePurge,
	}
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	seedResolverCache(r)

	_, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok, "pre-reconcile: claim must be present (non-vacuous)")

	r.reconcileOnce(context.Background())

	_, _, _, ok = r.GetOwner(quorumTestPID)
	require.False(t, ok, "a Get purge-op is a genuine deletion and must still tombstone")
}

// TestQuorumLoss_Boundary_AbsentFromKeysStillTombstones: the cached pid is no
// longer listed by Keys at all → genuinely gone → tombstoned. This is the
// backstop deletion path; the fix must not disturb it.
func TestQuorumLoss_Boundary_AbsentFromKeysStillTombstones(t *testing.T) {
	// Store holds a DIFFERENT live claim so Keys() returns non-empty (avoiding
	// the "no keys found" branch) but does NOT list the seeded pid.
	kv := newMockKVWithKeyErr(map[string][]byte{
		"claims/OTHER99": marshalClaim(t, handoff.Claim{
			PartitionID: "OTHER99",
			Owner:       "worker-other",
			State:       handoff.ClaimStateStable,
			Epoch:       1,
		}),
	}, quorumTestRevision)
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	seedResolverCache(r) // seeds quorumTestPID, which is NOT in the store

	_, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok, "pre-reconcile: seeded claim must be present (non-vacuous)")

	r.reconcileOnce(context.Background())

	_, _, _, ok = r.GetOwner(quorumTestPID)
	require.False(t, ok, "a pid absent from Keys is genuinely gone and must still tombstone")
}

// TestQuorumLoss_Boundary_PrefixFilteredKeyIsInert: a key that does not match
// the claims prefix is skipped before Get, so it enters neither `seen` nor
// `unreadable`. This test is load-bearing against a regressed prefix filter (one
// that stopped skipping out-of-prefix keys) on BOTH sides:
//
//   - seen-side: an out-of-prefix key carrying a VALID claim. If it were
//     fetched it would be staged as an upsert and become resolvable. (A junk
//     payload would be masked by applyPendingBatch's unmarshal-skip, so the
//     value must be a real claim for the assertion to bite.)
//   - unreadable-side: an out-of-prefix key whose bare name equals a cached,
//     genuinely-gone pid and whose Get errors. If it entered `unreadable` it
//     would wrongly suppress that gone pid's tombstone.
func TestQuorumLoss_Boundary_PrefixFilteredKeyIsInert(t *testing.T) {
	// An out-of-prefix key is cached (if a regression staged it) under its
	// TrimPrefix("claims/") result, which — having no prefix to strip — is the
	// full key string. So the seen-side assertion must query that full key.
	const outSeenKey = "other/SEEN9" // out-of-prefix valid claim; must stay inert
	const gonePID = "GONE7"          // cached but genuinely gone → must tombstone

	kv := newHealthyKV(t) // store: claims/USER21 (live)
	// seen-side probe: a valid claim under an out-of-prefix key.
	kv.store[outSeenKey] = marshalClaim(t, handoff.Claim{
		PartitionID: "SEEN9",
		Owner:       "worker-out",
		State:       handoff.ClaimStateStable,
		Epoch:       1,
	})
	// unreadable-side probe: a bare key whose name collides with gonePID and
	// whose Get errors. The real prefixed key (claims/GONE7) is absent, so a
	// correct prefix filter lets gonePID tombstone; a regressed filter would
	// route this errored Get into `unreadable[gonePID]` and suppress it.
	kv.store[gonePID] = []byte("ignored")
	kv.getErrByKey = map[string]error{gonePID: context.DeadlineExceeded}

	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	// Seed cache with the live claim AND the genuinely-gone pid.
	seed := map[string]claimEntry{
		quorumTestPID: {
			owner:    quorumTestOwner,
			state:    toState(handoff.ClaimStateStable),
			epoch:    quorumTestEpoch,
			revision: quorumTestRevision,
		},
		gonePID: {
			owner:    "worker-gone",
			state:    toState(handoff.ClaimStateStable),
			epoch:    1,
			revision: quorumTestRevision,
		},
	}
	r.cache.Store(&seed)

	// Non-vacuous pre-reconcile presence checks.
	_, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok, "pre-reconcile: live claim must be present")
	_, _, _, ok = r.GetOwner(gonePID)
	require.True(t, ok, "pre-reconcile: gone pid must be present before it tombstones")

	r.reconcileOnce(context.Background())

	owner, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok, "the live claim must survive a reconcile that also saw out-of-prefix keys")
	require.Equal(t, quorumTestOwner, owner)

	// seen-side: the out-of-prefix valid claim must not have been staged. It
	// would be cached under its full-key pid if the prefix filter regressed.
	_, _, _, ok = r.GetOwner(outSeenKey)
	require.False(t, ok, "an out-of-prefix key must never become a resolvable claim (seen-side)")

	// unreadable-side: gonePID is genuinely gone (claims/GONE7 absent) and the
	// bare GONE7 key was prefix-filtered, so it must still tombstone.
	_, _, _, ok = r.GetOwner(gonePID)
	require.False(t, ok, "an out-of-prefix errored key must not suppress a genuine tombstone (unreadable-side)")
}
