package parti

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/hooks"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestAssignmentSupersedesForStash pins the full applied-identity ordering used
// by the retry/coalesce stash. The ordering mirrors the applied-ack identity
// the core apply gate trusts (isApplyResultStale / currentAssignmentApplied):
// (Version, LeaderRevision) lex, then PartitionSetDigest, then source-revision
// known/value. At identical full identity the candidate is an idempotent
// duplicate and cur is kept.
func TestAssignmentSupersedesForStash(t *testing.T) {
	partsA := []types.Partition{{Keys: []string{"alpha"}}}
	partsB := []types.Partition{{Keys: []string{"beta"}}, {Keys: []string{"gamma"}}}

	base := func() Assignment {
		return Assignment{Version: 7, LeaderRevision: 10, Partitions: partsA}
	}

	tests := []struct {
		name      string
		cur       Assignment
		candidate Assignment
		want      bool // candidate supersedes cur (replace)
	}{
		{
			name:      "higher version replaces",
			cur:       base(),
			candidate: Assignment{Version: 8, LeaderRevision: 1, Partitions: partsA},
			want:      true,
		},
		{
			name:      "lower version is dropped",
			cur:       base(),
			candidate: Assignment{Version: 6, LeaderRevision: 99, Partitions: partsB},
			want:      false,
		},
		{
			name:      "same version higher leader revision replaces",
			cur:       base(),
			candidate: Assignment{Version: 7, LeaderRevision: 20, Partitions: partsA},
			want:      true,
		},
		{
			name:      "same version lower leader revision is dropped",
			cur:       Assignment{Version: 7, LeaderRevision: 20, Partitions: partsA},
			candidate: Assignment{Version: 7, LeaderRevision: 10, Partitions: partsB},
			want:      false,
		},
		{
			name:      "same (V,LR) different digest replaces (last arrival wins)",
			cur:       base(),
			candidate: Assignment{Version: 7, LeaderRevision: 10, Partitions: partsB},
			want:      true,
		},
		{
			name: "same (V,LR) same digest but candidate adds known source replaces",
			cur:  base(),
			candidate: Assignment{
				Version: 7, LeaderRevision: 10, Partitions: partsA,
				SourceRevisionKnown: true, SourceRevision: 5,
			},
			want: true,
		},
		{
			name: "same (V,LR) same digest different known source value replaces",
			cur: Assignment{
				Version: 7, LeaderRevision: 10, Partitions: partsA,
				SourceRevisionKnown: true, SourceRevision: 5,
			},
			candidate: Assignment{
				Version: 7, LeaderRevision: 10, Partitions: partsA,
				SourceRevisionKnown: true, SourceRevision: 6,
			},
			want: true,
		},
		{
			name:      "identical full identity keeps cur (idempotent duplicate)",
			cur:       base(),
			candidate: base(),
			want:      false,
		},
		{
			name: "identical full identity with source keeps cur",
			cur: Assignment{
				Version: 7, LeaderRevision: 10, Partitions: partsA,
				SourceRevisionKnown: true, SourceRevision: 5,
			},
			candidate: Assignment{
				Version: 7, LeaderRevision: 10, Partitions: partsA,
				SourceRevisionKnown: true, SourceRevision: 5,
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := assignmentSupersedesForStash(tc.candidate, tc.cur)
			if got != tc.want {
				t.Fatalf("assignmentSupersedesForStash(candidate, cur) = %v, want %v", got, tc.want)
			}
		})
	}
}

// ============================================================================
// Reproducer: same-version / different-digest commit lost by the
// scheduleApplyRetry version-only coalescing comparison.
// ============================================================================
//
// THE DEFECT
//
// When two assignments share the same Version but carry different partition
// digests — a real product of a CAS-race between the leader's legacy-alias
// pre-publish and the commit CAS, acknowledged by manager_assignment.go's
// committedAssignment/digest commentary (~:1216-1224) — and BOTH of their
// initial applies fail, the second one is silently dropped by
// scheduleApplyRetry's coalescing guard:
//
//	// scheduleApplyRetry (manager_assignment.go ~:1528-1530)
//	cur := m.stashedApplyRetry.Load()
//	if cur != nil && cur.Version >= newAssignment.Version {
//	    break // <-- DROPS the newer (different-digest, higher-LR) target
//	}
//
// The guard compares Version ONLY. It does not consider LeaderRevision or the
// partition-set digest. So a stashed legacy-alias A (Version=n, LR=r1,
// digest D1) shadows a later COMMIT authority B (Version=n, LR=r2 > r1,
// digest D2): B.Version (n) is not > A.Version (n), so B is discarded.
//
// THE INTERLEAVING (each step verified against source)
//
//  1. Alias A arrives (handleAssignmentEntry): version gate n-1<n passes,
//     LR fence passes, selectAuthority -> LegacyAlias -> applyAssignment(A).
//  2. A's apply FAILS (updater returns error) -> scheduleApplyRetry(A):
//     stash empty -> stash=A. Retry goroutine spawns, sleeps >=1s.
//  3. Commit B arrives (handleCommitValue): case (a) passes (snapshot still
//     n-1), LR fence passes, selectAuthority -> Commit -> applyAssignment(B).
//  4. B's apply ALSO FAILS -> scheduleApplyRetry(B): cur.Version (n) >=
//     B.Version (n) -> B DROPPED. <<< THE LOST UPDATE.
//  5. Updater recovers. Retry pops A, applies successfully ->
//     committedAssignment = (n, r1, D1). Heartbeat acks D1.
//  6. Nothing re-delivers B in default (direct) mode: commit redelivery is
//     version-gated (case (a): n <= n -> no-op); apply errors bypass the
//     degraded circuit so the worker is never Degraded; the leader audit is
//     dead without two-phase. The worker reports Stable on the WRONG digest.
//
// WHY IT MATTERS
//
// The partitions in D2 \ D1 are never served by this worker, yet the worker
// reports Stable and acks D1. This is a silent partition-ownership divergence:
// no error surfaces, no degraded transition fires, and the heartbeat audit
// sees a self-consistent (but wrong) ack.
//
// THE REGRESSION GUARD
//
// After the retry drains, the worker's committedAssignment digest must be D2
// (the commit authority). Pre-fix, the version-only coalescing guard dropped B
// so the worker converged on the stale alias digest D1. This test pins the
// fixed convergence: the coalescing guard now compares full
// (Version, LeaderRevision) / identity, preserving B (D2).

// failNCoordinator is a handoff.Coordinator stub whose Apply fails the first
// failUntil calls (returning a synthetic error), then succeeds. It also
// records the digest of every assignment it was asked to apply, in order, so
// the test can prove which payload won the retry race.
type failNCoordinator struct {
	failUntil  int64
	applyCount atomic.Int64

	mu            atomicDigestLog
	committedSeen atomic.Uint64 // digest of the last SUCCESSFUL apply's next set
}

type atomicDigestLog struct {
	mu atomic.Pointer[[]digestEntry]
}

type digestEntry struct {
	version int64
	lr      uint64
	digest  uint64
	ok      bool // true if this apply succeeded
}

func (c *failNCoordinator) Start(_ context.Context) {}

func (c *failNCoordinator) Apply(_ context.Context, _ string, _, next types.Assignment) error {
	n := c.applyCount.Add(1)
	digest := types.PartitionSetDigest(next.Partitions)
	if n <= c.failUntil {
		c.appendLog(digestEntry{version: next.Version, lr: next.LeaderRevision, digest: digest, ok: false})
		return errors.New("synthetic apply failure")
	}
	c.committedSeen.Store(digest)
	c.appendLog(digestEntry{version: next.Version, lr: next.LeaderRevision, digest: digest, ok: true})

	return nil
}

func (c *failNCoordinator) appendLog(e digestEntry) {
	for {
		cur := c.mu.mu.Load()
		var next []digestEntry
		if cur != nil {
			next = append(next, *cur...)
		}
		next = append(next, e)
		if c.mu.mu.CompareAndSwap(cur, &next) {
			return
		}
	}
}

func (c *failNCoordinator) log() []digestEntry {
	if p := c.mu.mu.Load(); p != nil {
		return *p
	}
	return nil
}

// newAliasEntry builds the legacy `assignment.<W>` alias envelope: a
// jetstream.KeyValueEntry whose Value() is the JSON encoding of a full
// Assignment. Reuses the aliasEntry stub declared in
// manager_commit_state_machine_test.go.
func newAliasEntry(t *testing.T, a Assignment) aliasEntry {
	t.Helper()
	b, err := json.Marshal(a)
	require.NoError(t, err)
	return aliasEntry{value: b}
}

// publishCommitPayload stores a gzip-compressed, hash-addressed assignment
// payload in the KV bucket and returns the ref the commit must carry. Mirrors
// the leader's commit-payload publish so buildAssignmentFromCommit's
// FetchAndVerifyCommitPayload succeeds.
func publishCommitPayload(t *testing.T, kv jetstream.KeyValue, parts []types.Partition) types.AssignmentPayloadRef {
	t.Helper()
	payload := types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    parts,
	}
	canonical, err := json.Marshal(payload)
	require.NoError(t, err)
	hash := sha256.Sum256(canonical)
	hashHex := hex.EncodeToString(hash[:])
	key := "assignment._payload." + hashHex

	var gzBuf bytes.Buffer
	gzw, _ := gzip.NewWriterLevel(&gzBuf, gzip.BestCompression)
	_, err = gzw.Write(canonical)
	require.NoError(t, err)
	require.NoError(t, gzw.Close())
	_, err = kv.Create(t.Context(), key, gzBuf.Bytes())
	require.NoError(t, err)

	return types.AssignmentPayloadRef{
		Key:         key,
		PayloadHash: hashHex,
		SetDigest:   types.PartitionSetDigest(parts),
	}
}

// newRetryDigestManager builds a Manager fixture wired with the failing
// coordinator and a live KV bucket for commit-payload fetches.
func newRetryDigestManager(t *testing.T, fc *failNCoordinator) *Manager {
	t.Helper()
	_, nc := partitest.StartEmbeddedNATS(t)
	akv := partitest.CreateJetStreamKV(t, nc, "apply-retry-digest-asgn")

	nopHooks := hooks.NewNop()
	m := &Manager{
		cfg:                TestConfig(),
		hooks:              &nopHooks,
		metrics:            newRecordingMetrics(),
		logger:             logging.NewNop(),
		heartbeat:          &recordingHeartbeat{},
		handoffCoordinator: fc,
		assignmentKV:       akv,
	}
	m.workerID.Store("worker-test")
	m.assignment.Store(Assignment{})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	m.ctx = ctx
	m.cancel = cancel

	return m
}

// TestApplyRetry_SameVersionDifferentDigest_CommitLost reproduces the lost
// commit. See the file-level comment for the full interleaving and rationale.
func TestApplyRetry_SameVersionDifferentDigest_CommitLost(t *testing.T) {
	const (
		version    = int64(7)
		aliasLR    = uint64(10) // r1
		commitLR   = uint64(20) // r2 > r1
		applyDelay = 8 * time.Second
	)

	// Fail the first two applies (alias A, commit B); the third (retry of the
	// surviving stash) succeeds.
	fc := &failNCoordinator{failUntil: 2}
	m := newRetryDigestManager(t, fc)
	wid := m.WorkerID()

	// Establish a prior applied snapshot at version-1 so the version gate
	// (oldAssignment.Version >= newAssignment.Version) passes for version n.
	prior := Assignment{Version: version - 1, LeaderRevision: aliasLR}
	m.assignment.Store(prior)
	committedPrior := prior
	m.committedAssignment.Store(&committedPrior)
	m.lastSeenLeaderRevision.Store(0)

	// Partition sets: D1 (alias A) and D2 (commit B) are DIFFERENT and both
	// non-empty so the digests differ and neither is the empty digest (0).
	partsA := []types.Partition{{Keys: []string{"alpha"}}}
	partsB := []types.Partition{{Keys: []string{"beta"}}, {Keys: []string{"gamma"}}}
	digestD1 := types.PartitionSetDigest(partsA)
	digestD2 := types.PartitionSetDigest(partsB)
	require.NotEqual(t, digestD1, digestD2, "test precondition: D1 != D2")
	require.NotZero(t, digestD1)
	require.NotZero(t, digestD2)

	// --- Step 1+2: alias A arrives and its apply fails, stashing A. ---
	aliasA := Assignment{
		Version:        version,
		LeaderRevision: aliasLR,
		Partitions:     partsA,
	}
	m.handleAssignmentEntry(wid, newAliasEntry(t, aliasA))

	// A's apply must have failed and stashed A for retry.
	require.Equal(t, int64(1), fc.applyCount.Load(), "step 2: alias A apply attempted once")
	stashed := m.stashedApplyRetry.Load()
	require.NotNil(t, stashed, "step 2: A must be stashed for retry")
	require.Equal(t, version, stashed.Version)
	require.Equal(t, digestD1, types.PartitionSetDigest(stashed.Partitions), "step 2: stash holds D1")
	require.True(t, m.applyRetryActive.Load(), "step 2: retry goroutine active")

	// --- Step 3+4: commit B arrives (same version, higher LR, digest D2),
	//     its apply fails, and scheduleApplyRetry DROPS it. ---
	ref := publishCommitPayload(t, m.assignmentKV, partsB)
	commitB := &types.AssignmentCommit{
		Version:        version,
		LeaderRevision: commitLR,
		Workers:        []string{wid},
		Payloads:       map[string]types.AssignmentPayloadRef{wid: ref},
	}
	m.handleCommitValue(commitB)

	require.Equal(t, int64(2), fc.applyCount.Load(), "step 4: commit B apply attempted once")
	// With the full applied-identity coalesce, B (same version, higher LR,
	// digest D2) supersedes the stashed alias A (D1): the stash now holds D2.
	// Pre-fix the version-only guard kept D1 here, which is the lost update.
	stashedAfterB := m.stashedApplyRetry.Load()
	require.NotNil(t, stashedAfterB)
	require.Equal(t, digestD2, types.PartitionSetDigest(stashedAfterB.Partitions),
		"step 4: scheduleApplyRetry must coalesce by full identity and keep commit B (D2), not the stale alias D1")
	require.Equal(t, commitLR, stashedAfterB.LeaderRevision,
		"step 4: stash must carry the commit authority's LeaderRevision r2")

	// --- Step 5: wait for the retry goroutine to drain the stash and apply
	//     successfully. The retry initial backoff is ~1s (+/-20%). ---
	deadline := time.Now().Add(applyDelay)
	for time.Now().Before(deadline) {
		if c := m.committedAssignment.Load(); c != nil && c.Version == version {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	committed := m.committedAssignment.Load()
	require.NotNil(t, committed, "step 5: retry must have committed a version-n assignment")
	require.Equal(t, version, committed.Version, "step 5: retry applied version n")

	finalDigest := types.PartitionSetDigest(committed.Partitions)

	t.Logf("apply log (digest, lr, ok per attempt): %+v", fc.log())
	t.Logf("final committed: version=%d lr=%d digest=%d (D1=%d D2=%d)",
		committed.Version, committed.LeaderRevision, finalDigest, digestD1, digestD2)

	// Regression guard: the worker must converge on the COMMIT authority's
	// digest (D2). Pre-fix, the version-only stash guard dropped B and the
	// worker converged on the stale alias digest D1.
	require.Equal(t, digestD2, finalDigest,
		"regression: worker must commit the commit-authority digest D2; "+
			"pre-fix the version-only coalescing guard silently dropped the same-version/higher-LR commit B (D2)")
	require.Equal(t, commitLR, committed.LeaderRevision,
		"committed assignment must carry the commit authority's LeaderRevision r2")
}

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
