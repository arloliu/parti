package parti

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/stretchr/testify/require"
)

// PR-2 W15+W16 — pre-Apply (V, LR) serialized gate inside applyStoreMu.
// See docs/plans/worker-state-hardening/02-pr2-spec.md.

// Test 5.1 — W15 cross-path: blocked commit Apply does not regress fresher
// alias snapshot. The slower path acquires applyStoreMu first, blocks
// inside its Apply; the alias path waits, then acquires the lock after
// the commit Stores (V=10, LR=3). The alias's stale check sees (V=10, LR=3)
// vs candidate (V=10, LR=4) — alias is fresher, Apply proceeds, snapshot
// ends at (V=10, LR=4).
func TestApplySerialization_W15_FreshAliasWinsOverBlockedCommit(t *testing.T) {
	t.Parallel()
	m, rh, _, rm := newTestManager(t)

	// First Apply will block until releaseFirst is closed.
	rh.blockFirstApply.Store(true)

	c1 := Assignment{Version: 10, LeaderRevision: 3}
	a1 := Assignment{Version: 10, LeaderRevision: 4}

	g1Done := make(chan struct{})
	go func() {
		defer close(g1Done)
		_ = m.applyAssignment(c1)
	}()

	// Wait for G1 to enter Apply (holding applyStoreMu).
	<-rh.firstApplyReady

	g2Done := make(chan struct{})
	go func() {
		defer close(g2Done)
		_ = m.applyAssignment(a1)
	}()

	// Give G2 time to block on applyStoreMu.
	time.Sleep(50 * time.Millisecond)

	// Release G1. It Stores (V=10, LR=3), then G2 acquires the lock,
	// stale-checks (10,4) vs (10,3) → not stale → Applies, Stores (10,4).
	close(rh.releaseFirst)

	select {
	case <-g1Done:
	case <-time.After(2 * time.Second):
		t.Fatal("G1 (commit C1) did not return")
	}
	select {
	case <-g2Done:
	case <-time.After(2 * time.Second):
		t.Fatal("G2 (alias A1) did not return")
	}

	cur := m.CurrentAssignment()
	require.Equal(t, int64(10), cur.Version)
	require.Equal(t, uint64(4), cur.LeaderRevision,
		"alias's higher LR must end as the snapshot")
	require.Equal(t, uint64(4), m.lastSeenLeaderRevision.Load(),
		"LSR must advance to 4")
	require.Equal(t, int64(2), rh.applyCount.Load(),
		"both candidates applied (commit + alias)")
	require.Equal(t, int64(0), rm.staleSnapshotStoreDropped.Load(),
		"no stale drop — both passed the gate in sequence")
}

// Test 5.1b — reverse ordering. The fresher alias acquires the lock first;
// the commit waits, then stale-gate-drops with the metric.
func TestApplySerialization_W15_StaleCommitDroppedAfterFresherAlias(t *testing.T) {
	t.Parallel()
	m, rh, _, rm := newTestManager(t)

	rh.blockFirstApply.Store(true)

	a1 := Assignment{Version: 10, LeaderRevision: 4}
	c1 := Assignment{Version: 10, LeaderRevision: 3}

	g1Done := make(chan struct{})
	go func() {
		defer close(g1Done)
		_ = m.applyAssignment(a1)
	}()

	<-rh.firstApplyReady

	g2Done := make(chan struct{})
	go func() {
		defer close(g2Done)
		_ = m.applyAssignment(c1)
	}()

	time.Sleep(50 * time.Millisecond)
	close(rh.releaseFirst)

	select {
	case <-g1Done:
	case <-time.After(2 * time.Second):
		t.Fatal("G1 (alias A1) did not return")
	}
	select {
	case <-g2Done:
	case <-time.After(2 * time.Second):
		t.Fatal("G2 (commit C1) did not return")
	}

	cur := m.CurrentAssignment()
	require.Equal(t, int64(10), cur.Version)
	require.Equal(t, uint64(4), cur.LeaderRevision,
		"alias's snapshot must remain — commit was stale-dropped")
	require.Equal(t, int64(1), rh.applyCount.Load(),
		"only alias's Apply ran — commit's was gate-dropped before Apply")
	require.Equal(t, int64(1), rm.staleSnapshotStoreDropped.Load())
}

// Test 5.2 — W16: stale apply-retry does not regress fresher snapshot.
// C1 (V=5, LR=10) fails Apply → retry armed. C2 (V=10, LR=15) succeeds.
// Retry fires after 1s backoff but is stale-gate-dropped before its Apply.
func TestApplySerialization_W16_StaleRetryDroppedBeforeApply(t *testing.T) {
	t.Parallel()
	m, rh, _, rm := newTestManager(t)

	rh.errOnce.Store(&errBox{err: errors.New("transient apply failure")})

	c1 := Assignment{Version: 5, LeaderRevision: 10}
	err := m.applyAssignment(c1)
	require.Error(t, err, "first apply must fail")
	require.Equal(t, int64(1), rh.applyCount.Load())

	c2 := Assignment{Version: 10, LeaderRevision: 15}
	err = m.applyAssignment(c2)
	require.NoError(t, err)

	cur := m.CurrentAssignment()
	require.Equal(t, int64(10), cur.Version)
	require.Equal(t, uint64(15), cur.LeaderRevision)
	require.Equal(t, int64(2), rh.applyCount.Load(),
		"C2's Apply ran (count = failed-C1 + C2)")

	// Wait past the 1s+jitter retry backoff for the retry to fire and
	// be gate-dropped.
	require.Eventually(t, func() bool {
		return rm.staleSnapshotStoreDropped.Load() == 1
	}, 5*time.Second, 50*time.Millisecond,
		"retry must fire and be stale-gate-dropped within 5s")

	// Snapshot must not have regressed; retry's Apply must NOT have run.
	cur = m.CurrentAssignment()
	require.Equal(t, int64(10), cur.Version, "snapshot must not regress")
	require.Equal(t, uint64(15), cur.LeaderRevision, "LSR must not regress")
	require.Equal(t, int64(2), rh.applyCount.Load(),
		"retry's Apply MUST NOT have run — gate dropped it before Apply")
}

// Test 5.3 — Idempotent reapply same (V, LR) is not stale.
func TestApplySerialization_IdempotentReapplyAdmitted(t *testing.T) {
	t.Parallel()
	m, rh, _, rm := newTestManager(t)

	a1 := Assignment{Version: 5, LeaderRevision: 10}
	require.NoError(t, m.applyAssignment(a1))

	cur := m.CurrentAssignment()
	require.Equal(t, int64(5), cur.Version)
	require.Equal(t, uint64(10), cur.LeaderRevision)

	require.NoError(t, m.applyAssignment(a1))

	cur = m.CurrentAssignment()
	require.Equal(t, int64(5), cur.Version)
	require.Equal(t, uint64(10), cur.LeaderRevision)
	require.Equal(t, int64(0), rm.staleSnapshotStoreDropped.Load(),
		"gate must NOT fire on idempotent reapply")
	require.Equal(t, int64(2), rh.applyCount.Load(),
		"both Applies must run (idempotent)")
}

// Test 5.4 — V=0 carve-out semantics. Phase A: V=0 over V=0 admits.
// Phase B: V=0 over V>0 is dropped.
func TestApplySerialization_V0_BootstrapAdmittedRegressionDropped(t *testing.T) {
	t.Parallel()
	m, rh, _, rm := newTestManager(t)

	// Phase A: bootstrap V=0 over initial V=0 snapshot.
	zero := Assignment{}
	require.NoError(t, m.applyAssignment(zero))

	require.Equal(t, int64(1), rh.applyCount.Load(), "bootstrap Apply ran")
	require.Equal(t, int64(0), rm.staleSnapshotStoreDropped.Load(),
		"V=0 over V=0 must NOT trigger gate")

	// Apply a real V>0 snapshot.
	a1 := Assignment{Version: 10, LeaderRevision: 20}
	require.NoError(t, m.applyAssignment(a1))
	cur := m.CurrentAssignment()
	require.Equal(t, int64(10), cur.Version)

	// Phase B: V=0 over V=10 must be gate-dropped.
	require.NoError(t, m.applyAssignment(zero))
	cur = m.CurrentAssignment()
	require.Equal(t, int64(10), cur.Version,
		"V=0 candidate must NOT regress snapshot")
	require.Equal(t, uint64(20), cur.LeaderRevision)
	require.Equal(t, int64(1), rm.staleSnapshotStoreDropped.Load(),
		"V=0 over V>0 must fire the gate")
	require.Equal(t, int64(2), rh.applyCount.Load(),
		"V=0 candidate's Apply must NOT have run (count = bootstrap-zero + A1)")
}

// Test 5.5 — Gate does NOT fire on Apply failure.
func TestApplySerialization_ApplyErrorDoesNotFireGate(t *testing.T) {
	t.Parallel()
	m, rh, _, rm := newTestManager(t)

	rh.errOnce.Store(&errBox{err: errors.New("transient apply failure")})

	a1 := Assignment{Version: 10, LeaderRevision: 20}
	err := m.applyAssignment(a1)
	require.Error(t, err, "Apply must return the error")

	require.Equal(t, int64(0), rm.staleSnapshotStoreDropped.Load(),
		"gate must NOT fire on Apply error — the candidate was fresh")
	require.Equal(t, int64(1), rh.applyCount.Load(),
		"failed Apply still counts")
	require.Equal(t, Assignment{}, m.CurrentAssignment(),
		"Store must NOT have run on Apply error")
}

// Test 5.6 — refreshAssignmentFromNATS honors the gate.
func TestApplySerialization_RefreshHonorsGate(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "pr2-refresh-gate")

	m, _, _, rm := newTestManager(t)
	m.assignmentKV = kv

	key := fmt.Sprintf("assignment.%s", m.WorkerID())

	// Plant V=5/LR=10 in KV and refresh — gate admits, snapshot advances.
	v5 := Assignment{Version: 5, LeaderRevision: 10}
	b5, err := json.Marshal(v5)
	require.NoError(t, err)
	_, err = kv.Create(t.Context(), key, b5)
	require.NoError(t, err)

	require.NoError(t, m.refreshAssignmentFromNATS())

	cur := m.CurrentAssignment()
	require.Equal(t, int64(5), cur.Version)
	require.Equal(t, uint64(10), cur.LeaderRevision)
	require.Equal(t, uint64(10), m.lastSeenLeaderRevision.Load(),
		"refresh must advance LSR consistently with snapshot")
	require.Equal(t, int64(0), rm.staleSnapshotStoreDropped.Load())

	// Apply V=10/LR=20 directly — fresher than KV's V=5/LR=10.
	a2 := Assignment{Version: 10, LeaderRevision: 20}
	require.NoError(t, m.applyAssignment(a2))
	cur = m.CurrentAssignment()
	require.Equal(t, int64(10), cur.Version)

	// Overwrite KV to V=5/LR=11 (older V than snapshot).
	v5b := Assignment{Version: 5, LeaderRevision: 11}
	b5b, err := json.Marshal(v5b)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), key, b5b)
	require.NoError(t, err)

	// Refresh: must be gate-dropped (V=5 < V=10).
	require.NoError(t, m.refreshAssignmentFromNATS())

	cur = m.CurrentAssignment()
	require.Equal(t, int64(10), cur.Version,
		"refresh must NOT regress the fresher snapshot")
	require.Equal(t, uint64(20), cur.LeaderRevision)
	require.Equal(t, int64(1), rm.staleSnapshotStoreDropped.Load(),
		"refresh must fire the gate on stale KV")
}
