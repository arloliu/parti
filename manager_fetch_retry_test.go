package parti

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/hooks"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Reproducer: assignment-bearing commit silently dropped when its payload
// fetch fails.
// ============================================================================
//
// THE DEFECT (found by external review, codex-u2-r5-report.md §1.9)
//
// buildAssignmentFromCommit's payload fetch/verify failure (ok=false) used to
// be handled by handleCommitValueOnce as a bare drop: pendingApplyInFlight
// was cleared and the function returned, with no scheduleApplyRetry call and
// no degraded transition. scheduleApplyRetry was only reachable after a
// SUCCESSFUL fetch whose handoffCoordinator.Apply then failed. Reconcile only
// ever re-delivers the CURRENT singleton commit (runCommitWatchSession's
// onReconcile re-GETs the same key), so once a newer commit lands, a
// superseded intermediate version's payload can never be recovered — a
// transient KV blip or a payload that was garbage-collected while still
// needed silently elides that version forever.
//
// THE FIX
//
// buildAssignmentFromCommit ok=false now routes through
// scheduleCommitFetchRetry, a sibling of scheduleApplyRetry with the
// identical bounded-backoff envelope, coalescing, and unbounded-until-
// success-or-shutdown behavior (manager_assignment.go).

// newFetchRetryTestManager builds a Manager fixture wired with a real
// embedded-NATS KV bucket (so FetchAndVerifyCommitPayload's Get can
// genuinely fail or succeed) and an always-succeeding handoff coordinator,
// mirroring newRetryDigestManager's shape (manager_apply_retry_test.go) but
// also returning the recordingMetrics handle this file's tests need.
func newFetchRetryTestManager(t *testing.T) (*Manager, *failNCoordinator, *recordingMetrics) {
	t.Helper()
	fc := &failNCoordinator{}
	m, rm := newFetchRetryTestManagerWithCoordinator(t, fc)

	return m, fc, rm
}

// newFetchRetryTestManagerWithCoordinator is newFetchRetryTestManager's
// construction shared with an injectable coordinator, for tests that need
// finer Apply-timing control than failNCoordinator's simple fail-then-succeed
// count offers (e.g. the accepted-target fence interleaving tests, which must
// deterministically hold one retry loop's success open while asserting on
// the other).
func newFetchRetryTestManagerWithCoordinator(t *testing.T, coord handoff.Coordinator) (*Manager, *recordingMetrics) {
	t.Helper()
	_, nc := partitest.StartEmbeddedNATS(t)
	akv := partitest.CreateJetStreamKV(t, nc, "fetch-retry-asgn")

	rm := newRecordingMetrics()
	nopHooks := hooks.NewNop()
	m := &Manager{
		cfg:                TestConfig(),
		hooks:              &nopHooks,
		metrics:            rm,
		logger:             logging.NewNop(),
		heartbeat:          &recordingHeartbeat{},
		handoffCoordinator: coord,
		assignmentKV:       akv,
	}
	m.workerID.Store("worker-test")
	m.assignment.Store(Assignment{})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	m.ctx = ctx
	m.cancel = cancel

	return m, rm
}

// fenceRaceCoordinator gives the accepted-target-fence interleaving test
// (T1) deterministic control over when a specific version's Apply call may
// succeed. Without this, whether V6's apply-retry stores V6 before or after
// V5's fetch-retry attempts its own apply is a race between two independent
// 1s+jitter backoff timers — the confirmed defect only reproduces on ONE of
// the two possible orderings (V5 attempting apply while V6 has NOT yet
// stored), so a timer race would make the test flaky in both directions.
//
// Every call to heldVersion fails until release() is called, after which
// every call (past-failed retries included) succeeds; every other version's
// call always succeeds. Deliberately non-blocking: applyAssignmentWithPrevCore
// holds applyStoreMu across the ENTIRE Apply call, so a coordinator that
// blocked INSIDE Apply would hold that mutex hostage and deadlock the other
// version's retry attempt against it (every apply path — commit, alias,
// fetch-retry, apply-retry — serializes on the same mutex). Fast, repeated
// failure achieves the same "cannot store yet" guarantee without ever
// holding the lock across a wait. Every call is recorded in arrival order
// for post-hoc assertions.
type fenceRaceCoordinator struct {
	heldVersion int64
	released    atomic.Bool

	mu    sync.Mutex
	calls []digestEntry
}

func newFenceRaceCoordinator(heldVersion int64) *fenceRaceCoordinator {
	return &fenceRaceCoordinator{heldVersion: heldVersion}
}

func (c *fenceRaceCoordinator) Start(_ context.Context) {}

func (c *fenceRaceCoordinator) Apply(_ context.Context, _ string, _, next types.Assignment) error {
	if next.Version == c.heldVersion && !c.released.Load() {
		c.record(next, false)

		return errors.New("synthetic apply failure (held by test)")
	}

	c.record(next, true)

	return nil
}

func (c *fenceRaceCoordinator) record(next types.Assignment, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, digestEntry{
		version: next.Version,
		lr:      next.LeaderRevision,
		digest:  types.PartitionSetDigest(next.Partitions),
		ok:      ok,
	})
}

func (c *fenceRaceCoordinator) log() []digestEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]digestEntry, len(c.calls))
	copy(out, c.calls)

	return out
}

// release lets heldVersion's Apply calls succeed from this point on. Safe to
// call multiple times (idempotent atomic.Bool store).
func (c *fenceRaceCoordinator) release() {
	c.released.Store(true)
}

// computeCommitPayloadRef computes the content-addressed ref publishCommitPayload
// (manager_apply_retry_test.go) would write for parts, WITHOUT publishing it —
// lets a test build a commit that references a not-yet-existing payload key so
// the first fetch attempt genuinely fails.
func computeCommitPayloadRef(t *testing.T, parts []types.Partition) types.AssignmentPayloadRef {
	t.Helper()
	payload := types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    parts,
	}
	canonical, err := json.Marshal(payload)
	require.NoError(t, err)
	hash := sha256.Sum256(canonical)
	hashHex := hex.EncodeToString(hash[:])

	return types.AssignmentPayloadRef{
		Key:         "assignment._payload." + hashHex,
		PayloadHash: hashHex,
		SetDigest:   types.PartitionSetDigest(parts),
	}
}

// TestFetchRetry_PayloadFetchFailure_RetriesAndApplies is the primary
// reproducer. It fails on the pre-fix parent (require.Eventually times out:
// nothing ever retries a fetch failure) and passes post-fix (the retry loop
// eventually observes the payload once it is published and applies it).
func TestFetchRetry_PayloadFetchFailure_RetriesAndApplies(t *testing.T) {
	t.Parallel()
	m, fc, rm := newFetchRetryTestManager(t)
	wid := m.WorkerID()

	parts := []types.Partition{{Keys: []string{"alpha"}}}
	ref := computeCommitPayloadRef(t, parts) // NOT published yet — fetch must fail.

	commit := &types.AssignmentCommit{
		Version:        5,
		LeaderRevision: 10,
		Workers:        []string{wid},
		Payloads:       map[string]types.AssignmentPayloadRef{wid: ref},
	}

	m.handleCommitValue(commit)

	// Immediately after: the fetch failed, so nothing applied yet and the
	// commit must not have been silently forgotten (this alone is already
	// true pre-fix, since case (c) drop leaves state untouched).
	require.Equal(t, int64(0), fc.applyCount.Load(), "fetch failure must not call Apply")
	require.Nil(t, m.committedAssignment.Load(), "must not be committed before the payload is fetchable")
	require.GreaterOrEqual(t, rm.payloadFetchError.Load(), int64(1), "fetch failure must be recorded")

	// The retry loop's active flag is set synchronously (CompareAndSwap)
	// before scheduleCommitFetchRetry returns, i.e. before handleCommitValue
	// above returned — no sleep needed to "give it a moment to arm".
	require.True(t, m.fetchRetryActive.Load(), "fetch-retry loop must be active synchronously after the failed fetch")

	// Simulate the transient KV blip resolving: publish the payload the
	// commit already references. This genuinely exercises "recover after
	// failure" (the loop's first attempt already failed above), not
	// "succeed on the very first attempt".
	publishCommitPayload(t, m.assignmentKV, parts)

	require.Eventually(t, func() bool {
		c := m.committedAssignment.Load()
		return c != nil && c.Version == 5
	}, 6*time.Second, 50*time.Millisecond,
		"an assignment-bearing commit must not be silently dropped on payload-fetch failure: "+
			"the fetch-retry loop must eventually apply it once the payload becomes fetchable")

	committed := m.committedAssignment.Load()
	require.Equal(t, types.PartitionSetDigest(parts), types.PartitionSetDigest(committed.Partitions))
	require.Equal(t, int64(1), fc.applyCount.Load(), "exactly one Apply — the retry's successful attempt")
}

// TestFetchRetry_CoalescesToHighestVersion pins the fetch-retry stash's
// coalescing contract: when a second commit also fails to fetch its payload
// while the first is still pending retry, the stash must hold the newer
// (higher-identity) commit, not the older one — mirroring
// scheduleApplyRetry's assignmentSupersedesForStash coalescing
// (TestApplyRetry_SameVersionDifferentDigest_CommitLost) but for the
// commit-typed fetch-retry stash and commitSupersedesForStash.
func TestFetchRetry_CoalescesToHighestVersion(t *testing.T) {
	t.Parallel()
	m, fc, _ := newFetchRetryTestManager(t)
	wid := m.WorkerID()

	partsV5 := []types.Partition{{Keys: []string{"alpha"}}}
	partsV6 := []types.Partition{{Keys: []string{"beta"}}, {Keys: []string{"gamma"}}}
	refV5 := computeCommitPayloadRef(t, partsV5) // never published in this test
	refV6 := computeCommitPayloadRef(t, partsV6) // never published in this test

	commitV5 := &types.AssignmentCommit{
		Version: 5, LeaderRevision: 10, Workers: []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{wid: refV5},
	}
	commitV6 := &types.AssignmentCommit{
		Version: 6, LeaderRevision: 20, Workers: []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{wid: refV6},
	}

	m.handleCommitValue(commitV5)
	stashed := m.stashedFetchRetry.Load()
	require.NotNil(t, stashed, "V5's fetch failure must schedule a fetch retry")
	require.Equal(t, int64(5), stashed.Version)
	require.True(t, m.fetchRetryActive.Load(), "retry goroutine must be active")

	// V6 arrives (fresh path — its own fetch also fails) before the pending
	// retry's backoff fires (initial backoff is ~1s).
	m.handleCommitValue(commitV6)
	stashedAfterV6 := m.stashedFetchRetry.Load()
	require.NotNil(t, stashedAfterV6)
	require.Equal(t, int64(6), stashedAfterV6.Version,
		"the fetch-retry stash must coalesce to the higher-version commit V6, not keep the superseded V5")

	require.Equal(t, int64(0), fc.applyCount.Load(), "neither commit ever fetched its payload, so Apply must never run")
}

// TestFetchRetry_Supersession_NewerAppliesDirectly_LateFetchDropped covers
// the brief's supersession case: a newer commit applies via the normal
// (non-retry) path while an older commit's fetch retry is still pending. When
// the older retry eventually DOES manage to fetch its payload, the apply-time
// (V, LR) stale gate must silently drop it — no double-apply, no regression,
// no elision (the newer version is what's live; the older one is subsumed by
// it, not "lost").
func TestFetchRetry_Supersession_NewerAppliesDirectly_LateFetchDropped(t *testing.T) {
	t.Parallel()
	m, fc, rm := newFetchRetryTestManager(t)
	wid := m.WorkerID()

	partsV5 := []types.Partition{{Keys: []string{"alpha"}}}
	partsV6 := []types.Partition{{Keys: []string{"beta"}}, {Keys: []string{"gamma"}}}
	refV5 := computeCommitPayloadRef(t, partsV5) // published LATE, after V6 already applied
	refV6 := computeCommitPayloadRef(t, partsV6)

	commitV5 := &types.AssignmentCommit{
		Version: 5, LeaderRevision: 10, Workers: []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{wid: refV5},
	}
	m.handleCommitValue(commitV5)
	require.NotNil(t, m.stashedFetchRetry.Load(), "V5 fetch failure must be stashed for retry")

	// V6's payload is available immediately, so it applies directly through
	// the normal (non-retry) path.
	publishCommitPayload(t, m.assignmentKV, partsV6)
	commitV6 := &types.AssignmentCommit{
		Version: 6, LeaderRevision: 20, Workers: []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{wid: refV6},
	}
	m.handleCommitValue(commitV6)

	require.Eventually(t, func() bool {
		c := m.committedAssignment.Load()
		return c != nil && c.Version == 6
	}, 2*time.Second, 20*time.Millisecond, "V6 must apply directly")

	// Now let V5's payload become fetchable too (the "blip resolves" case),
	// but it is now stale relative to the already-applied V6.
	publishCommitPayload(t, m.assignmentKV, partsV5)

	// Wait past the fetch-retry's backoff window for it to have fetched V5
	// and attempted (and been gate-dropped for) its apply.
	require.Eventually(t, func() bool {
		return rm.staleSnapshotStoreDropped.Load() >= 1
	}, 6*time.Second, 50*time.Millisecond,
		"the late-fetched, now-stale V5 must be dropped by the (V, LR) stale gate")

	committed := m.committedAssignment.Load()
	require.NotNil(t, committed)
	require.Equal(t, int64(6), committed.Version, "committed assignment must remain V6 — no regression to the stale V5")
	require.Equal(t, types.PartitionSetDigest(partsV6), types.PartitionSetDigest(committed.Partitions))
	require.Equal(t, int64(1), fc.applyCount.Load(),
		"Apply must have run exactly once (for V6) — V5's late apply attempt must be gate-dropped before Apply, not double-applied")
}

// TestFetchRetry_ExhaustionMirrorsApplyRetry_NoEscalation pins the "no new
// escalation policy" requirement: a commit whose payload is permanently
// unfetchable (never published) is retried forever with the same bounded
// backoff an Apply failure gets — never escalated to a degraded transition
// or any other new policy, and never silently given up on either. This
// mirrors the pre-existing scheduleApplyRetry exhaustion behavior (an
// Apply failure's retry loop also has no max-attempts cutoff — it retries
// until success or shutdown).
func TestFetchRetry_ExhaustionMirrorsApplyRetry_NoEscalation(t *testing.T) {
	t.Parallel()
	m, fc, rm := newFetchRetryTestManager(t)
	wid := m.WorkerID()

	parts := []types.Partition{{Keys: []string{"alpha"}}}
	ref := computeCommitPayloadRef(t, parts) // never published — permanently unfetchable in this test

	commit := &types.AssignmentCommit{
		Version: 5, LeaderRevision: 10, Workers: []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{wid: ref},
	}
	m.handleCommitValue(commit)

	require.True(t, m.fetchRetryActive.Load(), "retry goroutine must be active immediately after the first failure")

	// Wait long enough for at least two retry attempts (backoff starts at
	// ~1s) without ever publishing the payload.
	require.Eventually(t, func() bool {
		return rm.payloadFetchError.Load() >= 3
	}, 8*time.Second, 50*time.Millisecond,
		"a permanently unfetchable payload must keep retrying (no give-up) exactly like an Apply failure would")

	// No escalation: never applied, never a new/different policy fired.
	require.Equal(t, int64(0), fc.applyCount.Load(), "Apply must never run for a payload that never became fetchable")
	require.Nil(t, m.committedAssignment.Load())
	require.True(t, m.fetchRetryActive.Load(), "the retry loop must still be running, not have given up")

	// Sanity: the manager's high-level state was never pushed anywhere by
	// this path (no enterDegraded wiring exists here, so State stays at its
	// zero value) — confirms no new escalation/degraded transition was
	// introduced by the fix.
	require.Equal(t, StateInit, m.State())
}

// TestCommitSupersedesForStash pins ALL branches of the commit-typed
// coalescing comparator (manager_assignment.go) that
// TestFetchRetry_CoalescesToHighestVersion only exercises the simplest
// (strictly-newer-Version) case of. Mirrors
// TestAssignmentSupersedesForStash (manager_apply_retry_test.go), the
// Assignment-typed sibling comparator over the same (Version,
// LeaderRevision, identity) ordering.
func TestCommitSupersedesForStash(t *testing.T) {
	base := func() *types.AssignmentCommit {
		return &types.AssignmentCommit{Version: 7, LeaderRevision: 10, BatchDigest: 100}
	}

	tests := []struct {
		name      string
		cur       *types.AssignmentCommit
		candidate *types.AssignmentCommit
		want      bool // candidate supersedes cur (replace)
	}{
		{
			name:      "strictly newer version replaces",
			cur:       base(),
			candidate: &types.AssignmentCommit{Version: 8, LeaderRevision: 1, BatchDigest: 1},
			want:      true,
		},
		{
			name:      "strictly older version is dropped",
			cur:       base(),
			candidate: &types.AssignmentCommit{Version: 6, LeaderRevision: 99, BatchDigest: 1},
			want:      false,
		},
		{
			name:      "same version higher leader revision replaces",
			cur:       base(),
			candidate: &types.AssignmentCommit{Version: 7, LeaderRevision: 20, BatchDigest: 100},
			want:      true,
		},
		{
			name:      "same version lower leader revision is dropped",
			cur:       &types.AssignmentCommit{Version: 7, LeaderRevision: 20, BatchDigest: 100},
			candidate: &types.AssignmentCommit{Version: 7, LeaderRevision: 10, BatchDigest: 1},
			want:      false,
		},
		{
			name:      "equal (V,LR) different BatchDigest replaces",
			cur:       base(),
			candidate: &types.AssignmentCommit{Version: 7, LeaderRevision: 10, BatchDigest: 200},
			want:      true,
		},
		{
			name: "equal (V,LR) same BatchDigest but candidate adds known source revision replaces",
			cur:  base(),
			candidate: &types.AssignmentCommit{
				Version: 7, LeaderRevision: 10, BatchDigest: 100,
				SourceRevisionKnown: true, SourceRevision: 5,
			},
			want: true,
		},
		{
			name: "equal (V,LR) same BatchDigest different known source revision value replaces",
			cur: &types.AssignmentCommit{
				Version: 7, LeaderRevision: 10, BatchDigest: 100,
				SourceRevisionKnown: true, SourceRevision: 5,
			},
			candidate: &types.AssignmentCommit{
				Version: 7, LeaderRevision: 10, BatchDigest: 100,
				SourceRevisionKnown: true, SourceRevision: 6,
			},
			want: true,
		},
		{
			name:      "identical duplicate keeps cur",
			cur:       base(),
			candidate: base(),
			want:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := commitSupersedesForStash(tc.candidate, tc.cur)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestCommitHandler_EqualVersionDifferentContent_DoesNotReachApply pins a
// KNOWN-OPEN hazard that internal/assignment/handoff's version-adjacency
// fail-open cannot see: the commit handler's case (a) no-op
// (`commit.Version <= cur.Version` in handleCommitValueOnce) fires on
// Version alone, with no check on content, before Apply is ever called.
// A second commit at the SAME Version as the last applied one, carrying
// DIFFERENT partition content, is silently dropped by that comparison —
// so the coordinator's own equal-Version fail-open
// (transitionVersionAdjacent's equality arm,
// internal/assignment/handoff/twophase.go) is production-unreachable
// through this path.
//
// This is documented current behavior; the equal-Version authority-
// divergence hazard (an alias and a commit can expose different content at
// the same Version) is deferred to the claim-level commit-identity fence
// project (issue #74, design under docs/design/). The sibling emission
// path — a publisher's CAS-loss recovery reusing an external winner's
// Version — is CLOSED: the coherent reseed in
// internal/assignment/assignment_publisher.go advances currentVersion
// alongside lastCommitRev, so a recovered publisher's next proposal is
// strictly beyond both observed Versions (pinned by
// TestPublisher_CASLossReseed_*). Divergence deliveries that do occur are
// counted by the equal-version divergence metric at this drop site.
func TestCommitHandler_EqualVersionDifferentContent_DoesNotReachApply(t *testing.T) {
	t.Parallel()
	m, fc, _ := newFetchRetryTestManager(t)
	wid := m.WorkerID()

	// First commit at V5 with content X: applies normally through the
	// commit-watcher entry point.
	partsX := []types.Partition{{Keys: []string{"alpha"}}}
	refX := publishCommitPayload(t, m.assignmentKV, partsX)
	commitX := &types.AssignmentCommit{
		Version: 5, LeaderRevision: 10, Workers: []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{wid: refX},
	}
	m.handleCommitValue(commitX)

	require.Equal(t, int64(1), fc.applyCount.Load(), "setup: the first commit must apply")
	require.Equal(t, int64(5), m.CurrentAssignment().Version, "setup: worker must be at V5")
	require.Equal(t, types.PartitionSetDigest(partsX), types.PartitionSetDigest(m.CurrentAssignment().Partitions),
		"setup: applied content must be X")

	// Second commit at the SAME Version 5 with DIFFERENT content Y,
	// delivered through the same entry point the watcher path uses.
	partsY := []types.Partition{{Keys: []string{"beta"}}}
	refY := publishCommitPayload(t, m.assignmentKV, partsY)
	commitY := &types.AssignmentCommit{
		Version: 5, LeaderRevision: 11, Workers: []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{wid: refY},
	}
	m.handleCommitValue(commitY)

	require.Equal(t, int64(1), fc.applyCount.Load(),
		"documented current behavior: an equal-Version delivery is dropped by the "+
			"commit.Version <= cur.Version no-op case before it ever reaches Apply, "+
			"regardless of differing content")
	require.Equal(t, int64(5), m.CurrentAssignment().Version)
	require.Equal(t, types.PartitionSetDigest(partsX), types.PartitionSetDigest(m.CurrentAssignment().Partitions),
		"the applied assignment content must be unchanged by the dropped equal-Version delivery")
}

// ============================================================================
// Reproducer: shared accepted-target fence across the fetch-retry and
// apply-retry loops.
// ============================================================================
//
// THE DEFECT (found by the final cross-model review)
//
// The fetch-retry stash (stashedFetchRetry) and the apply-retry stash
// (stashedApplyRetry) are independent, and applyAssignmentWithPrevCore's
// stale gate compares a candidate ONLY against CurrentAssignment (the last
// successfully STORED snapshot) — it has no visibility into what the OTHER
// retry loop has already admitted. Two symmetric interleavings let a STALE
// admitted target reach the coordinator:
//
//   - V5's payload fetch fails (fetch-retry stash) while V6 is admitted,
//     builds, and its Apply fails (apply-retry stash). V5's fetch then
//     recovers: the stale gate alone sees current=V4 (nothing stored yet),
//     V5 > V4, so V5 would apply — even though V6 is the fresher admitted
//     target.
//   - The reverse: V5 Apply-fails into the apply-retry stash; V6 arrives but
//     its fetch fails into the fetch-retry stash; V5's apply-retry fires
//     before V6's fetch recovers and would apply V5 over the fresher V6.
//
// THE FIX
//
// Manager.highestAcceptedTarget tracks the highest (Version, LeaderRevision)
// target ever ADMITTED by a delivery gate this session (handleCommitValueOnce
// and handleAssignmentEntry, raised via raiseAcceptedTarget BEFORE the
// payload fetch so a fetch failure still raises it). applyAssignmentWithPrevCore
// checks a candidate against this fence immediately after the existing
// CurrentAssignment stale gate, in the same applyStoreMu critical section,
// and drops anything strictly behind it before any coordinator-side effect.

// TestAcceptedTargetFence_LateFetchRecoveryDroppedByNewerApplyFailure is T1:
// interleaving (a). V5's fetch fails first; V6 is then admitted, builds, and
// its Apply fails. V5's fetch later recovers and its retry fires — the fence
// (raised to V6's coordinates at V6's admission, before V6's Apply ever ran)
// must drop it before the coordinator ever sees it. V6's own apply-retry
// then succeeds, converging the worker on V6.
func TestAcceptedTargetFence_LateFetchRecoveryDroppedByNewerApplyFailure(t *testing.T) {
	t.Parallel()
	// V6 (heldVersion) fails every Apply call until release() is called, so
	// V6 cannot store ahead of this test's control no matter how many times
	// its own apply-retry loop fires in the background — see
	// fenceRaceCoordinator's Godoc for why a plain failNCoordinator (or a
	// coordinator that BLOCKS inside Apply) would make this interleaving a
	// flaky race, or deadlock on applyStoreMu.
	coord := newFenceRaceCoordinator(6)
	m, _ := newFetchRetryTestManagerWithCoordinator(t, coord)
	wid := m.WorkerID()

	partsV5 := []types.Partition{{Keys: []string{"alpha"}}}
	partsV6 := []types.Partition{{Keys: []string{"beta"}}, {Keys: []string{"gamma"}}}
	refV5 := computeCommitPayloadRef(t, partsV5) // NOT published yet — V5's fetch must fail.

	commitV5 := &types.AssignmentCommit{
		Version: 5, LeaderRevision: 10, Workers: []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{wid: refV5},
	}
	m.handleCommitValue(commitV5)
	require.NotNil(t, m.stashedFetchRetry.Load(), "V5's fetch failure must schedule a fetch retry")
	require.True(t, m.fetchRetryActive.Load())

	// V6's payload IS published, so it builds successfully; its Apply is the
	// coordinator's first call to V6, which fails (fenceRaceCoordinator's
	// heldVersion=6 fails every call until release()).
	refV6 := publishCommitPayload(t, m.assignmentKV, partsV6)
	commitV6 := &types.AssignmentCommit{
		Version: 6, LeaderRevision: 20, Workers: []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{wid: refV6},
	}
	m.handleCommitValue(commitV6)
	require.NotNil(t, m.stashedApplyRetry.Load(), "V6's Apply failure must schedule an apply retry")
	require.True(t, m.applyRetryActive.Load())

	// The fence must already be at V6's coordinates: V6's admission (in
	// handleCommitValueOnce, before its build/Apply) raised it past V5's.
	fence := m.highestAcceptedTarget.Load()
	require.NotNil(t, fence)
	require.Equal(t, int64(6), fence.Version)
	require.Equal(t, uint64(20), fence.LeaderRevision)

	// V5's payload becomes fetchable — simulating the transient blip
	// resolving. Its fetch-retry loop will pick this up on its next
	// backoff tick.
	publishCommitPayload(t, m.assignmentKV, partsV5)

	// Wait for V5's fetch-retry episode to conclude — either it applied
	// (the pre-fix defect) or was dropped by the fence (post-fix) — signaled
	// by the fetch-retry loop exiting. V6 is guaranteed to still be
	// un-stored here (every one of its background apply-retry attempts
	// keeps failing until release() below), so CurrentAssignment stays at
	// the zero-value V0 snapshot for this entire window: exactly the
	// interleaving the confirmed defect needs (V5's retry sees a snapshot
	// that has NOT yet advanced past it).
	require.Eventually(t, func() bool {
		return !m.fetchRetryActive.Load()
	}, 15*time.Second, 50*time.Millisecond,
		"V5's fetch-retry attempt must conclude (apply or fence-drop) while V6 is still unreleased")

	// The core assertion: the coordinator must never receive V5 — at this
	// point V6 is STILL unreleased, so the only way a V5 entry could appear
	// is the pre-fix defect: the old stale gate alone, comparing against
	// the still-empty snapshot, lets V5 through.
	for _, e := range coord.log() {
		require.NotEqual(t, int64(5), e.version, "V5 must never reach the coordinator's Apply")
	}
	require.Nil(t, m.committedAssignment.Load(), "V5 must not have been stored either, while V6 is still unreleased")

	// Release V6 and let its next apply-retry attempt succeed.
	coord.release()

	require.Eventually(t, func() bool {
		c := m.committedAssignment.Load()
		return c != nil && c.Version == 6
	}, 15*time.Second, 50*time.Millisecond,
		"V6's apply-retry must succeed once released and converge the worker on V6")

	committed := m.committedAssignment.Load()
	require.Equal(t, types.PartitionSetDigest(partsV6), types.PartitionSetDigest(committed.Partitions))

	// Explicit fence-equality check (T3's second half): V6's apply-retry
	// succeeded while the fence was AT V6's own coordinates — candidate ==
	// fence must pass, not just candidate < fence being dropped.
	finalFence := m.highestAcceptedTarget.Load()
	require.NotNil(t, finalFence)
	require.Equal(t, int64(6), finalFence.Version)
	require.Equal(t, uint64(20), finalFence.LeaderRevision)
}

// TestAcceptedTargetFence_ApplyRetryDroppedByNewerFetchAdmission is T2:
// interleaving (b), the reverse of T1. V5 is admitted and its Apply fails
// first; V6 is then admitted (raising the fence past V5) but its fetch
// fails. When V5's apply-retry fires, the fence must drop it before the
// coordinator ever sees it — even though V6 itself has not yet built or
// applied anything. V6's fetch later recovers and applies normally.
func TestAcceptedTargetFence_ApplyRetryDroppedByNewerFetchAdmission(t *testing.T) {
	t.Parallel()
	m, fc, rm := newFetchRetryTestManager(t)
	wid := m.WorkerID()
	fc.failUntil = 1 // V5's initial Apply attempt fails; all later calls succeed.

	partsV5 := []types.Partition{{Keys: []string{"alpha"}}}
	partsV6 := []types.Partition{{Keys: []string{"beta"}}, {Keys: []string{"gamma"}}}
	refV5 := publishCommitPayload(t, m.assignmentKV, partsV5) // fetchable immediately
	refV6 := computeCommitPayloadRef(t, partsV6)              // NOT published yet — V6's fetch must fail.

	commitV5 := &types.AssignmentCommit{
		Version: 5, LeaderRevision: 10, Workers: []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{wid: refV5},
	}
	m.handleCommitValue(commitV5)
	require.NotNil(t, m.stashedApplyRetry.Load(), "V5's Apply failure must schedule an apply retry")
	require.True(t, m.applyRetryActive.Load())
	require.Equal(t, int64(1), fc.applyCount.Load(), "setup: exactly one Apply call so far (V5's failed attempt)")

	commitV6 := &types.AssignmentCommit{
		Version: 6, LeaderRevision: 20, Workers: []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{wid: refV6},
	}
	m.handleCommitValue(commitV6)
	require.NotNil(t, m.stashedFetchRetry.Load(), "V6's fetch failure must schedule a fetch retry")
	require.True(t, m.fetchRetryActive.Load())

	// The fence must already be at V6's coordinates, raised at V6's
	// admission BEFORE its (failed) payload fetch.
	fence := m.highestAcceptedTarget.Load()
	require.NotNil(t, fence)
	require.Equal(t, int64(6), fence.Version)
	require.Equal(t, uint64(20), fence.LeaderRevision)

	// Drive V5's apply-retry: wait for it to fire and be dropped by the
	// fence (recorded via the shared stale-drop metric).
	require.Eventually(t, func() bool {
		return rm.staleSnapshotStoreDropped.Load() >= 1
	}, 15*time.Second, 50*time.Millisecond,
		"V5's apply-retry must fire and be dropped by the accepted-target fence")

	require.Equal(t, int64(1), fc.applyCount.Load(),
		"the coordinator must never receive V5's retry — the fence drops it before Apply")

	// Now let V6's fetch recover and apply.
	publishCommitPayload(t, m.assignmentKV, partsV6)

	require.Eventually(t, func() bool {
		c := m.committedAssignment.Load()
		return c != nil && c.Version == 6
	}, 15*time.Second, 50*time.Millisecond,
		"V6's fetch-retry must eventually fetch its payload and apply")

	committed := m.committedAssignment.Load()
	require.Equal(t, types.PartitionSetDigest(partsV6), types.PartitionSetDigest(committed.Partitions))

	require.Equal(t, int64(2), fc.applyCount.Load(),
		"exactly two Apply calls total: V5's failed attempt and V6's successful one — "+
			"V5's dropped retry never called Apply")
	for _, e := range fc.log() {
		if e.version == 5 {
			require.False(t, e.ok, "the only V5 log entry must be its initial failed attempt")
		}
	}
}

// TestAcceptedTargetFence_EqualToFencePasses is T3's first half: a lone
// commit whose fetch fails and later recovers, with no sibling commit ever
// admitted, must still apply normally — the fence sits AT the candidate's
// own coordinates (raised by the candidate's own admission), and candidate
// == fence must pass, not be treated as stale. Guards against an
// off-by-one/strict-vs-inclusive regression in the fence comparison.
func TestAcceptedTargetFence_EqualToFencePasses(t *testing.T) {
	t.Parallel()
	m, fc, _ := newFetchRetryTestManager(t)
	wid := m.WorkerID()

	parts := []types.Partition{{Keys: []string{"alpha"}}}
	ref := computeCommitPayloadRef(t, parts) // not published yet — fetch must fail first.

	commit := &types.AssignmentCommit{
		Version: 5, LeaderRevision: 10, Workers: []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{wid: ref},
	}
	m.handleCommitValue(commit)
	require.NotNil(t, m.stashedFetchRetry.Load())

	// The fence is raised at admission, before the fetch even runs, to
	// exactly this commit's own coordinates.
	fence := m.highestAcceptedTarget.Load()
	require.NotNil(t, fence)
	require.Equal(t, int64(5), fence.Version)
	require.Equal(t, uint64(10), fence.LeaderRevision)

	publishCommitPayload(t, m.assignmentKV, parts)

	require.Eventually(t, func() bool {
		c := m.committedAssignment.Load()
		return c != nil && c.Version == 5
	}, 6*time.Second, 50*time.Millisecond,
		"a lone commit's recovered fetch-retry must apply normally when the fence equals its own coordinates")

	committed := m.committedAssignment.Load()
	require.Equal(t, types.PartitionSetDigest(parts), types.PartitionSetDigest(committed.Partitions))
	require.Equal(t, int64(1), fc.applyCount.Load())
}
