package parti

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
