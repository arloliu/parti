package parti

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Reproducer: retry-loop exit races a concurrent producer's activation CAS,
// stranding the producer's stashed target forever.
// ============================================================================
//
// THE DEFECT (found by external review, codex-b2-postimpl.md P1)
//
// Both scheduleApplyRetry's loop and scheduleCommitFetchRetry's loop observe
// an empty stash and commit to returning while their active flag
// (applyRetryActive / fetchRetryActive) is STILL true — the flag only clears
// in the deferred cleanup that runs after the goroutine body returns. A
// producer that stashes a value and loses the activation CAS inside that
// window (the flag has not cleared yet) leaves its value stranded: the
// exiting loop never sees it, and no new loop is spawned to pick it up.
// Unbounded: reconcile cannot re-deliver a same-Version/higher-LeaderRevision
// commit past the version gate (manager_assignment.go case (a)), so a
// stranded commit or assignment can be lost until a genuinely newer one
// happens to arrive.
//
// THE FIX
//
// activateApplyRetryLoop/activateFetchRetryLoop's deferred cleanup clears the
// active flag and then RE-CHECKS the stash; if a producer raced the window
// and lost, the stash is non-empty and the re-check re-activates a loop for
// it (idempotent against a producer that instead wins the CAS itself).
//
// THE HOOK
//
// testHookRetryLoopEmptyObserved fires synchronously, on the retry loop's own
// goroutine, at the exact point (immediately after the empty-stash
// observation, before the deferred flag clear) where a racing producer's
// stash+CAS would lose. Calling the schedule function from inside the hook
// deterministically reproduces the race without any real concurrency: at
// that point the active flag is still true, so the hook's own activation CAS
// fails exactly as a genuinely concurrent producer's would.

// TestFetchRetry_LostWakeup_StashedDuringExitWindowIsRecovered reproduces the
// fetch-retry loop's lost-wakeup window at its post-apply empty-stash exit.
// Pre-fix: B is stashed and stranded forever (test times out). Post-fix: the
// ownership-handoff re-check respawns a loop and B eventually applies.
func TestFetchRetry_LostWakeup_StashedDuringExitWindowIsRecovered(t *testing.T) {
	t.Parallel()
	m, _, _ := newFetchRetryTestManager(t)
	wid := m.WorkerID()

	partsA := []types.Partition{{Keys: []string{"alpha"}}}
	partsB := []types.Partition{{Keys: []string{"beta"}}, {Keys: []string{"gamma"}}}
	refA := computeCommitPayloadRef(t, partsA) // not published yet: A's first fetch must fail
	refB := computeCommitPayloadRef(t, partsB)

	commitA := &types.AssignmentCommit{
		Version: 5, LeaderRevision: 10, Workers: []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{wid: refA},
	}
	commitB := &types.AssignmentCommit{
		Version: 6, LeaderRevision: 20, Workers: []string{wid},
		Payloads: map[string]types.AssignmentPayloadRef{wid: refB},
	}

	var fired atomic.Bool
	m.testHookRetryLoopEmptyObserved = func() {
		// Fire exactly once, at the fetch loop's post-apply empty-stash exit
		// after A has successfully applied. Simulates commit B's handler
		// stashing B and losing the activation CAS in the window between
		// this loop's empty-stash observation and its deferred flag clear.
		if !fired.CompareAndSwap(false, true) {
			return
		}
		m.scheduleCommitFetchRetry(commitB)
	}

	m.handleCommitValue(commitA)
	require.True(t, m.fetchRetryActive.Load(), "A's fetch failure must activate the retry loop synchronously")

	// B's payload is published up front so its eventual (respawned) fetch
	// succeeds; only A's payload needs to be initially unfetchable to drive
	// the loop through one failed+retried attempt.
	publishCommitPayload(t, m.assignmentKV, partsB)
	// A's payload becomes fetchable before the retry loop's first backoff
	// fires (~1s+/-20% jitter), so the retry succeeds and reaches the
	// post-apply empty-stash exit where the hook is armed.
	publishCommitPayload(t, m.assignmentKV, partsA)

	require.Eventually(t, func() bool {
		c := m.committedAssignment.Load()
		return c != nil && c.Version == 5
	}, 6*time.Second, 50*time.Millisecond, "A must apply via the retry loop")

	require.True(t, fired.Load(), "the hook must have fired at the post-apply empty-stash exit")

	// WITHOUT the fix, B is stashed but stranded: fetchRetryActive cleared to
	// false with no loop re-checking the stash, so this never becomes true.
	require.Eventually(t, func() bool {
		c := m.committedAssignment.Load()
		return c != nil && c.Version == 6
	}, 8*time.Second, 50*time.Millisecond,
		"B must not be stranded: the ownership-handoff re-check must respawn the retry loop and apply it")
}

// TestApplyRetry_LostWakeup_StashedDuringExitWindowIsRecovered mirrors the
// fetch-retry reproducer for scheduleApplyRetry's identical lost-wakeup
// window at its post-apply empty-stash exit.
func TestApplyRetry_LostWakeup_StashedDuringExitWindowIsRecovered(t *testing.T) {
	t.Parallel()
	fc := &failNCoordinator{failUntil: 1} // only A's first Apply attempt fails
	m := newRetryDigestManager(t, fc)

	a := Assignment{Version: 5, LeaderRevision: 10, Partitions: []types.Partition{{Keys: []string{"alpha"}}}}
	b := Assignment{Version: 10, LeaderRevision: 20, Partitions: []types.Partition{{Keys: []string{"beta"}}}}

	var fired atomic.Bool
	m.testHookRetryLoopEmptyObserved = func() {
		// Fire exactly once, at the apply loop's post-apply empty-stash exit
		// after A has successfully applied. Simulates a concurrent producer
		// stashing B and losing the activation CAS in the same window.
		if !fired.CompareAndSwap(false, true) {
			return
		}
		m.scheduleApplyRetry(b)
	}

	err := m.applyAssignment(a)
	require.Error(t, err, "A's first apply must fail and arm the retry loop")
	require.True(t, m.applyRetryActive.Load(), "A's apply failure must activate the retry loop synchronously")

	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 5
	}, 6*time.Second, 50*time.Millisecond, "A must apply via the retry loop")

	require.True(t, fired.Load(), "the hook must have fired at the post-apply empty-stash exit")

	// WITHOUT the fix, B is stashed but stranded: applyRetryActive cleared to
	// false with no loop re-checking the stash, so this never becomes true.
	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 10
	}, 8*time.Second, 50*time.Millisecond,
		"B must not be stranded: the ownership-handoff re-check must respawn the retry loop and apply it")
}
