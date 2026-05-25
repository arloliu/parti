# Commit Watcher Debounce Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend the existing assignment debounce protection to the commit watcher path so rapid `assignment._commit` bursts collapse to the latest version before entering the apply pipeline.

**Architecture:** Keep one operator knob: `Config.AssignmentWatcherDebounce`. Its default remains `0`, so existing behavior is unchanged until the operator opts in. Refactor the commit watcher into a testable watch-session helper, stage the highest pending commit during the idle window, and flush on timer or watcher-session close. Preserve the two-phase-handoff "every ownership transition for this worker is observed" invariant by force-flushing the currently-pending commit before staging a new commit whose `Payloads[workerID].PayloadHash` differs from the pending one — debounce only collapses commits whose partition set for *this worker* is unchanged.

**Tech Stack:** Go, NATS JetStream KV watchers, existing `Manager` commit state machine, existing apply-attempt metrics, existing opt-in integration diagnostics.

## Subagent Dispatch & Review Discipline

Each task below is meant to be dispatched to a fresh implementer subagent following `superpowers:subagent-driven-development`. **For every task, the per-task post-implementation review loop is mandatory: run `/codex:review` (or `/post-impl-review <phase> <plan-path> <vN>` when a spec-compliance audit is needed) against the implementer's diff and iterate fix → re-review until the verdict is merge-clean (no P0/P1 findings) before marking the task complete and proceeding to the next.** Do not skip the loop even on tasks whose plan text reads "trivial" — every shipped bug in this repo's recent history slipped past 3 rounds of architectural review and was caught only by the post-impl loop.

**Per-task dispatch matrix:**

| Task | Implementer model | Codex effort | Rationale |
|------|-------------------|--------------|-----------|
| 1 — Add Commit Watcher Debounce Unit Tests | `sonnet` | `medium` | 10 test snippets + a hook field across 2 files. All code is given verbatim, but the implementer must place a single shared `fakeCommitEntryFor` helper (not per-test), preserve the existing test file's package/import style, position the new Manager hook field next to `testHookHandleAssignment`, and confirm every test fails for the *intended* reason before Task 2 runs. Haiku is borderline; `sonnet` is the safe first-pass choice. |
| 2 — Implement Commit Watch Session Debounce | `sonnet` | `high` | Critical state machine in the load-bearing file (`manager_assignment.go`). The order of guards inside `stage()` (out-of-order guard FIRST, then `workerAssignmentChanged`) is load-bearing — the round-2 plan-review caught exactly this ordering as a P1 even with pseudocode. Effort `high` so the implementer carefully preserves the guard order, the `dispatch` seam routing, and the close-vs-cancel asymmetry. |
| 3 — Prove the Fix with the Rapid-Commit Diagnostic | `sonnet` | `medium` | Extract existing test logic into a helper and add a debounced variant in `test/integration/manager/apply_coalescing_test.go`. Must not perturb sibling `TestApplyCoalescing_UnderReElectionBurst` which shares the same helper surface. |
| 4 — Update Public Docs and Operator Guidance | `haiku` | `low` | Pure text edits across `config.go`, the integration test comment, and the hardening README. Plan provides exact wording. |
| 5 — Final Validation | `haiku` | `low` | Mechanical: `gofmt`, focused `go test`, `make lint`, `make pre-pr`. Subagent runs commands and reports tails. **Escalate to a more capable model (`sonnet` or `opus`) ONLY if a failure surfaces a new code issue that requires diagnosis** — never re-dispatch the same model against a failing run without changes. |

**Effort interpretation:** the "Codex effort" column applies when an implementer or reviewer is dispatched via the codex plugin (`codex:codex-rescue`); when dispatched as a Claude subagent via the Agent tool, the "Implementer model" column applies (Claude has no `--effort` flag — model choice is the equivalent dial).

**Escalation rule:** if an implementer subagent returns `BLOCKED` or repeatedly fails the post-impl-review loop on the same finding, escalate one tier (haiku → sonnet, sonnet → opus; or medium → high → xhigh on Codex) before re-dispatching. Never silently retry the same configuration — that is how a 3-round failing loop becomes a 6-round failing loop.

---

## Context

The rapid-commit diagnostic added in this worktree proves the remaining herd-capable path:

```bash
PARTI_RUN_HERD_DIAGNOSTIC=1 go test ./test/integration/manager \
  -run TestApplyCoalescing_UnderRapidCommitBurst -v -count=3
```

Current proof signal:

- `COMMIT_BURST_AGGREGATE max_burst_size=4`
- `COMMIT_BURST_FLEET peak_concurrency=5`
- `versions_observed=4`

`runAssignmentWatchSession` already debounces legacy assignment-alias watcher events using `Config.AssignmentWatcherDebounce`. `watchCommit` currently calls `handleCommitEntry` or `handleCommitValue` immediately for each watcher or reconcile delivery. The existing `pendingApplyInFlight` / `stashedCommit` guard only coalesces while an apply is already running; it does not collapse a rapid sequence when each apply completes quickly enough to release the in-flight flag before the next commit is handled.

## Design Decision

Use the existing `AssignmentWatcherDebounce` knob for both assignment delivery watchers:

- legacy worker assignment alias watcher
- `assignment._commit` watcher

Do not add `CommitWatcherDebounce`.

Reasoning:

- Both watcher paths deliver assignment versions to the same apply pipeline.
- The default is still zero, preserving current behavior.
- A separate knob adds public API and tuning ambiguity without a clear operational benefit for the small-worker-count case.
- Applying the same idle window to both watcher *delivery* surfaces is the scope of this plan. The legacy alias watcher's `runAssignmentWatchSession` reconcile arm (a 30s `kv.Get` tick that bypasses debounce by calling `handleAssignmentEntry` directly — `manager_assignment.go:539-559`) is NOT harmonized here. That reconcile race is bounded by `watcherReconcileInterval=30s` and the dual-read selector in `handleAssignmentEntry` (`manager_assignment.go:593-610`), which consults `lastObservedCommit` and drops the alias when the commit-side view is fresher. The commit-side view is published from `handleCommitValue` (`manager_assignment.go:752-755`) inside the very flush path this plan delays, so a reconcile tick that lands during the commit debounce window CAN observe an alias whose corresponding commit is still pending. That residual race is acknowledged here; the apply it triggers still goes through the standard version/leader fences and cannot violate correctness, only the herd-collapse promise. Closing it (alias reconcile harmonization — apply the same staging path to the alias watcher's reconcile arm) is a known follow-up; it is intentionally not folded into this plan to keep the change scoped to the commit watcher path.

## File Structure

- Modify `manager.go`
  - Add `testHookHandleCommitValue` field (mirrors `testHookHandleAssignment`).

- Modify `manager_assignment.go`
  - Split `watchCommit` into watcher setup plus `runCommitWatchSession`.
  - Add commit debounce state inside `runCommitWatchSession` (pending slot, version-order guard, flush-on-partition-set-change, `dispatch` seam that routes to the test hook or `handleCommitValue`).
  - Keep delete handling and bad JSON handling unchanged.
  - Feed reconcile reads through the same stage/flush path when debounce is enabled.
  - Add the unexported `workerAssignmentChanged` and `commitContainsWorker` helpers.

- Modify `manager_commit_watcher_test.go`
  - Add commit watcher debounce unit tests using the existing in-process fake watcher style.
  - Cover burst collapse, idle-window reset, cancel-without-flush, close-with-flush, force-flush-on-partition-set-change, out-of-order-lower-version drop (same hash AND different hash), and the `workerAssignmentChanged` predicate table.

- Modify `test/integration/manager/apply_coalescing_test.go`
  - Keep the current no-debounce rapid-commit diagnostic as the before proof.
  - Factor the current rapid-commit diagnostic through a helper and run both no-debounce and debounced modes.

- Modify `config.go`
  - Update `AssignmentWatcherDebounce` docs so they explicitly cover both assignment alias and commit watcher events.

- Modify `docs/plans/thundering-herd-hardening/README.md`
  - Add this fourth hardening surface as a follow-up to PR-3, not a replacement for the original three controls.

## Task 1: Add Commit Watcher Debounce Unit Tests

> **Dispatch:** Claude `sonnet` (or Codex effort `medium`). Run the post-impl-review loop until merge-clean before proceeding.

**Files:**
- Modify: `manager.go` (add `testHookHandleCommitValue` field)
- Modify: `manager_commit_watcher_test.go`

- [ ] **Step 0: Add the commit-value test hook to Manager**

Mirror the existing `testHookHandleAssignment` hook for the commit
path. Tests for staging decisions must NOT need real payload-KV
scaffolding: production's `handleCommitValue` → `buildAssignmentFromCommit`
→ `FetchAndVerifyCommitPayload` does a real `kv.Get` on the payload
ref, which is incompatible with the lightweight `newTestManager`
fixture (`assignmentKV` is unset). The hook gives staging tests a
clean observable seam.

Add to `manager.go` immediately after `testHookHandleAssignment`:

```go
// testHookHandleCommitValue, when non-nil, is invoked instead of
// handleCommitValue from runCommitWatchSession's stage/flush paths
// (no-debounce immediate dispatch, force-flush on partition-set
// change, timer flush, and watcher-close flush). Set ONLY by tests
// in this package to assert commit debounce semantics without
// scaffolding a real assignment payload KV. Production code MUST
// NOT set this field. Same concurrency contract as
// testHookHandleAssignment: set before spawning runCommitWatchSession.
testHookHandleCommitValue func(commit *types.AssignmentCommit)
```

- [ ] **Step 1: Add fake commit entry helpers**

Add this helper near the existing watcher tests:

```go
func fakeCommitEntryWithVersion(v int64) jetstream.KeyValueEntry {
	b, _ := json.Marshal(types.AssignmentCommit{
		Version:        v,
		LeaderRevision: uint64(v),
		Workers:        []string{"someone-else"},
		Payloads:       map[string]types.AssignmentPayloadRef{},
	})
	return fakeVersionEntry{value: b}
}
```

This reuses `fakeVersionEntry` from `manager_assignment_debounce_test.go`; both files are in package `parti`.

- [ ] **Step 2: Write failing burst-collapse test**

Add:

```go
func TestCommitWatcher_DebouncesMultiVersionBurst(t *testing.T) {
	const window = 100 * time.Millisecond
	m, rh, _, _ := newTestManager(t)
	m.cfg.AssignmentWatcherDebounce = window

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runCommitWatchSession(m.ctx, nil, watcher, nil, "assignment._commit")
	}()

	for v := int64(10); v <= 14; v++ {
		watcher.ch <- fakeCommitEntryWithVersion(v)
		time.Sleep(8 * time.Millisecond)
	}

	time.Sleep(window + 100*time.Millisecond)

	require.Equal(t, int64(14), m.CurrentAssignment().Version)
	require.Equal(t, int64(1), rh.applyCount.Load(), "debounce must collapse commit burst")
}
```

- [ ] **Step 3: Write failing idle-reset test**

Add:

```go
func TestCommitWatcher_DebounceResetsOnEachEntry(t *testing.T) {
	const window = 100 * time.Millisecond
	m, rh, _, _ := newTestManager(t)
	m.cfg.AssignmentWatcherDebounce = window

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runCommitWatchSession(m.ctx, nil, watcher, nil, "assignment._commit")
	}()

	deadline := time.Now().Add(500 * time.Millisecond)
	v := int64(1)
	for time.Now().Before(deadline) {
		watcher.ch <- fakeCommitEntryWithVersion(v)
		v++
		time.Sleep(50 * time.Millisecond)
	}

	require.Zero(t, rh.applyCount.Load(), "debounce must not fire while stream is busy")

	time.Sleep(window + 100*time.Millisecond)
	require.Equal(t, int64(1), rh.applyCount.Load(), "debounce must fire once after idle")
}
```

- [ ] **Step 4: Write failing cancel-without-flush test**

Add:

```go
func TestCommitWatcher_DebounceCancelDoesNotFlush(t *testing.T) {
	const window = 5 * time.Second
	m, rh, _, _ := newTestManager(t)
	m.cfg.AssignmentWatcherDebounce = window

	watcher := newFakeKeyWatcher()
	done := make(chan struct{})
	go func() {
		_ = m.runCommitWatchSession(m.ctx, nil, watcher, nil, "assignment._commit")
		close(done)
	}()

	watcher.ch <- fakeCommitEntryWithVersion(99)
	time.Sleep(50 * time.Millisecond)
	m.cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("commit watch session did not exit after ctx cancel")
	}

	require.Zero(t, rh.applyCount.Load(), "pending commit must be dropped on ctx cancel")
}
```

- [ ] **Step 5: Write failing close-with-flush test**

Add:

```go
func TestCommitWatcher_PendingCommitFlushesOnClose(t *testing.T) {
	const window = 100 * time.Millisecond
	m, rh, _, _ := newTestManager(t)
	m.cfg.AssignmentWatcherDebounce = window

	watcher := newFakeKeyWatcher()
	done := make(chan struct{})
	go func() {
		_ = m.runCommitWatchSession(m.ctx, nil, watcher, nil, "assignment._commit")
		close(done)
	}()

	watcher.ch <- fakeCommitEntryWithVersion(42)
	close(watcher.ch)

	<-done
	require.Equal(t, int64(42), m.CurrentAssignment().Version)
	require.Equal(t, int64(1), rh.applyCount.Load(), "pending commit must flush on watcher close")
}
```

- [ ] **Step 6: Write failing partition-set-change flush test**

This test pins the P0 invariant that ownership-changing intermediate
commits are NOT collapsed away. Two commits are delivered into the
debounce window with different `Payloads[workerID].PayloadHash` for
this worker; the test asserts BOTH are processed (one as a
force-flush, one as the timer-fire flush).

Add a `fakeCommitEntryFor` helper that lets the test set the
worker's payload hash explicitly:

```go
func fakeCommitEntryFor(version int64, workerID, payloadHash string, members ...string) jetstream.KeyValueEntry {
	payloads := map[string]types.AssignmentPayloadRef{
		workerID: {PayloadHash: payloadHash},
	}
	workers := append([]string{workerID}, members...)
	b, _ := json.Marshal(types.AssignmentCommit{
		Version:        version,
		LeaderRevision: uint64(version),
		Workers:        workers,
		Payloads:       payloads,
	})
	return fakeVersionEntry{value: b}
}

func TestCommitWatcher_DebounceFlushesOnPartitionSetChange(t *testing.T) {
	const window = 100 * time.Millisecond
	m, _, _, _ := newTestManager(t)
	m.cfg.AssignmentWatcherDebounce = window
	workerID := m.WorkerID()

	// Route flushed commits through the hook so this test exercises the
	// stage/flush decision tree without depending on a real payload KV.
	var (
		mu      sync.Mutex
		flushed []*types.AssignmentCommit
	)
	m.testHookHandleCommitValue = func(c *types.AssignmentCommit) {
		mu.Lock()
		defer mu.Unlock()
		flushed = append(flushed, c)
	}

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runCommitWatchSession(m.ctx, nil, watcher, nil, "assignment._commit")
	}()

	// V=10: this worker owns hash="aaaa"
	watcher.ch <- fakeCommitEntryFor(10, workerID, "aaaa")
	time.Sleep(20 * time.Millisecond)

	// V=11: this worker owns hash="bbbb" — partition-set change.
	// stage() MUST force-flush V=10 before staging V=11 so the
	// handoff diff for the intermediate change is observed.
	watcher.ch <- fakeCommitEntryFor(11, workerID, "bbbb")

	// Wait > window for the timer to flush V=11.
	time.Sleep(window + 100*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, flushed, 2,
		"ownership-changing intermediate must not be collapsed away")
	require.Equal(t, int64(10), flushed[0].Version,
		"V=10 force-flushed before V=11 stages")
	require.Equal(t, int64(11), flushed[1].Version,
		"V=11 flushed by the timer after the idle window")
	require.Equal(t, "bbbb", flushed[1].Payloads[workerID].PayloadHash)
}
```

- [ ] **Step 7: Write failing out-of-order delivery test**

A lower commit version arriving after a higher staged version must
not replace the pending higher version. This pins the `>=` guard in
`stage()`.

```go
func TestCommitWatcher_DebounceIgnoresOutOfOrderLowerVersion(t *testing.T) {
	const window = 100 * time.Millisecond
	m, rh, _, _ := newTestManager(t)
	m.cfg.AssignmentWatcherDebounce = window

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runCommitWatchSession(m.ctx, nil, watcher, nil, "assignment._commit")
	}()

	watcher.ch <- fakeCommitEntryWithVersion(20)
	time.Sleep(20 * time.Millisecond)
	watcher.ch <- fakeCommitEntryWithVersion(15) // out-of-order, lower

	time.Sleep(window + 100*time.Millisecond)

	require.Equal(t, int64(1), rh.applyCount.Load())
	require.Equal(t, int64(20), m.CurrentAssignment().Version,
		"pending must not be overwritten by a lower-version entry")
}
```

- [ ] **Step 8: Write failing lower-version-with-different-hash test**

This pins the round-2 ordering fix: the version-order guard MUST
run before the `workerAssignmentChanged` flush-on-change check.
Otherwise a stale lower-version commit with a different worker
hash would flush a newer pending and re-stage itself.

```go
func TestCommitWatcher_DebounceIgnoresOutOfOrderLowerVersionWithDifferentHash(t *testing.T) {
	const window = 100 * time.Millisecond
	m, _, _, _ := newTestManager(t)
	m.cfg.AssignmentWatcherDebounce = window
	workerID := m.WorkerID()

	// Use the hook so we can directly observe whether V=15 ever reaches
	// the post-debounce dispatch surface. Under the round-2 ordering bug,
	// V=15 would force-flush V=20 (dispatch sees V=20), then V=15 would
	// be the new pending and timer-flush (dispatch sees V=15). That
	// would produce dispatches = [V=20, V=15]. The correct ordering must
	// produce dispatches = [V=20] — V=15 is dropped at the version guard
	// before workerAssignmentChanged runs, so the new pending is never
	// re-staged. Asserting applyCount alone does NOT discriminate: under
	// the buggy path, handleCommitValueOnce would case-(a) no-op V=15
	// (V=15 < cur.Version=V=20) but would still write V=15 to
	// lastObservedCommit, mutating selector-visible state.
	var (
		mu        sync.Mutex
		dispatches []*types.AssignmentCommit
	)
	m.testHookHandleCommitValue = func(c *types.AssignmentCommit) {
		mu.Lock()
		defer mu.Unlock()
		dispatches = append(dispatches, c)
	}

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runCommitWatchSession(m.ctx, nil, watcher, nil, "assignment._commit")
	}()

	watcher.ch <- fakeCommitEntryFor(20, workerID, "aaaa")
	time.Sleep(20 * time.Millisecond)
	watcher.ch <- fakeCommitEntryFor(15, workerID, "bbbb")

	time.Sleep(window + 100*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, dispatches, 1,
		"lower-version must be dropped before reaching dispatch — "+
			"never as a force-flush of pending nor as a re-stage")
	require.Equal(t, int64(20), dispatches[0].Version,
		"only V=20 was authoritative; V=15 must never be observed")
	require.Equal(t, "aaaa", dispatches[0].Payloads[workerID].PayloadHash)
}
```

- [ ] **Step 9: Write `workerAssignmentChanged` table test**

A focused unit test in a new test file `manager_assignment_change_test.go`
(or alongside the new commit-watcher tests) pins the predicate's
contract — schema-edge cases are easy to regress.

```go
func TestWorkerAssignmentChanged(t *testing.T) {
	mk := func(ver int64, members []string, hash string) *types.AssignmentCommit {
		c := &types.AssignmentCommit{Version: ver, Workers: members}
		c.Payloads = map[string]types.AssignmentPayloadRef{}
		for _, w := range members {
			c.Payloads[w] = types.AssignmentPayloadRef{PayloadHash: hash}
		}
		return c
	}
	cases := []struct {
		name  string
		prev  *types.AssignmentCommit
		next  *types.AssignmentCommit
		want  bool
	}{
		{"both_absent", mk(1, []string{"other"}, "x"), mk(2, []string{"other"}, "x"), false},
		{"present_to_absent", mk(1, []string{"me", "other"}, "x"), mk(2, []string{"other"}, "x"), true},
		{"absent_to_present", mk(1, []string{"other"}, "x"), mk(2, []string{"me", "other"}, "x"), true},
		{"present_same_hash", mk(1, []string{"me"}, "x"), mk(2, []string{"me"}, "x"), false},
		{"present_different_hash", mk(1, []string{"me"}, "x"), mk(2, []string{"me"}, "y"), true},
		{"nil_prev_present", nil, mk(2, []string{"me"}, "x"), true},
		{"nil_prev_absent", nil, mk(2, []string{"other"}, "x"), false},
		{"empty_payloads_present_in_workers", mk(1, []string{"me"}, "x"), &types.AssignmentCommit{Version: 2, Workers: []string{"me"}, Payloads: map[string]types.AssignmentPayloadRef{}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, workerAssignmentChanged(tc.prev, tc.next, "me"))
		})
	}
}
```

Note: `workerAssignmentChanged(nil, next, ...)` is a defensive
case — production `stage()` only calls it when `pending != nil` —
but the helper should still be total. Verify the implementation
treats `prev == nil` as "no prior view, present-in-next is a
change", matching the table above.

- [ ] **Step 10: Run tests and verify failure**

Run:

```bash
go test ./... -run 'TestCommitWatcher_Debounce|TestCommitWatcher_PendingCommitFlushesOnClose|TestCommitWatcher_DebounceFlushesOnPartitionSetChange|TestCommitWatcher_DebounceIgnoresOutOfOrderLowerVersion|TestCommitWatcher_DebounceIgnoresOutOfOrderLowerVersionWithDifferentHash|TestWorkerAssignmentChanged' -count=1
```

Expected before implementation:

```text
undefined: (*Manager).runCommitWatchSession
```

## Task 2: Implement Commit Watch Session Debounce

> **Dispatch:** Claude `sonnet` (or Codex effort `high`). Critical state machine — the guard order inside `stage()` is load-bearing. Run the post-impl-review loop until merge-clean before proceeding.

**Files:**
- Modify: `manager_assignment.go`

- [ ] **Step 1: Refactor `watchCommit` into setup plus session**

Change `watchCommit` so it only creates/stops the watcher and delegates:

```go
func (m *Manager) watchCommit(ctx context.Context, kv jetstream.KeyValue, reconcileTickC <-chan time.Time) error {
	key := "assignment." + commitKeyName
	watcher, err := kv.Watch(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to watch commit: %w", err)
	}
	return m.runCommitWatchSession(ctx, kv, watcher, reconcileTickC, key)
}
```

- [ ] **Step 2: Add `runCommitWatchSession`**

Add the helper below `watchCommit`:

```go
func (m *Manager) runCommitWatchSession(
	ctx context.Context,
	kv jetstream.KeyValue,
	watcher jetstream.KeyWatcher,
	reconcileTickC <-chan time.Time,
	key string,
) error {
	defer func() {
		if serr := watcher.Stop(); serr != nil && !natsutil.IsBenignWatcherStopErr(serr) {
			m.logError("failed to stop commit watcher", "error", serr)
		}
	}()

	window := m.cfg.AssignmentWatcherDebounce
	debounce := window > 0

	var (
		pending *types.AssignmentCommit
		timer   *time.Timer
		timerC  <-chan time.Time
	)
	if debounce {
		timer = time.NewTimer(time.Hour)
		timer.Stop()
		timerC = timer.C
	}

	workerID := m.WorkerID()

	// dispatch is the single seam through which staged commits exit the
	// debounce window. It routes to handleCommitValue in production and
	// to testHookHandleCommitValue when set, mirroring the alias-side
	// testHookHandleAssignment pattern. Read the hook field once per
	// dispatch so tests that mutate it after spawn (forbidden by the
	// hook's contract) cannot create a torn read.
	dispatch := func(commit *types.AssignmentCommit) {
		if commit == nil {
			return
		}
		if hook := m.testHookHandleCommitValue; hook != nil {
			hook(commit)
			return
		}
		m.handleCommitValue(commit)
	}

	// stage routes a freshly decoded commit through the debounce window.
	//
	// Correctness invariant (two-phase handoff): every commit that
	// changes THIS worker's effective partition set must reach
	// handleCommitValue as a discrete apply. Otherwise a skipped
	// intermediate that moved a partition away from us (worker not in
	// commit.Workers, or a payload-set change that drops a partition we
	// own) would leave our local snapshot still containing the partition
	// — and the next debounced apply's preparePhase, which only diffs
	// next-vs-previous on the local snapshot, would elide the
	// reclaim/release and let the claim store and local snapshot
	// diverge (see internal/assignment/handoff/twophase.go:208-232,
	// 334-386). The "flush before staging on payload-hash change" rule
	// below enforces this: bursts of identical-assignment-for-us
	// commits collapse to the highest version (the common case during
	// leader churn), but any intermediate that reshuffles us is
	// flushed before the new pending is staged.
	stage := func(commit *types.AssignmentCommit) {
		if commit == nil {
			return
		}
		if !debounce {
			dispatch(commit)
			return
		}
		// Out-of-order guard FIRST: drop any commit whose version is
		// strictly lower than what is already staged. This must run
		// BEFORE the workerAssignmentChanged check — otherwise a stale
		// lower-version commit with a different worker hash would
		// force-flush the newer pending and then re-stage itself,
		// regressing both the version-order guarantee and the
		// "highest pending" architecture promise. The watcher
		// stream is monotone in practice, but the reconcile arm can
		// surface a snapshot that races with a more recent watcher
		// event still in flight, so the guard is load-bearing.
		if pending != nil && commit.Version < pending.Version {
			return
		}
		// If the new commit changes THIS worker's assignment vs.
		// pending, flush pending first so the handoff diff for the
		// intermediate change is observed. workerAssignmentChanged
		// compares Workers-membership and Payloads[workerID].PayloadHash
		// — identical hashes mean identical partition slices by
		// AssignmentPayloadRef's content-addressable contract
		// (types/assignment_commit.go:51-66).
		if pending != nil && workerAssignmentChanged(pending, commit, workerID) {
			flushed := pending
			pending = nil
			dispatch(flushed)
		}
		// pending is now nil OR commit.Version >= pending.Version.
		// The replacement is unconditional in the nil case; the
		// `>= pending.Version` arm in the non-nil case is the same-or-
		// higher path that simply refreshes the timer with the freshest
		// view (typically identical hash; we already flushed otherwise).
		commitCopy := *commit
		pending = &commitCopy
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(window)
	}

	flush := func() {
		if pending == nil {
			return
		}
		commit := pending
		pending = nil
		dispatch(commit)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case entry, ok := <-watcher.Updates():
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				flush()
				return errors.New("commit watcher channel closed")
			}
			if entry == nil {
				continue
			}
			commit, ok := m.decodeCommitEntry(entry)
			if ok {
				stage(commit)
			}
		case <-timerC:
			flush()
		case <-reconcileTickC:
			if kv == nil {
				continue
			}
			current, _, gerr := kvutil.GetJSON[types.AssignmentCommit](ctx, kv, key)
			if gerr != nil || current == nil {
				continue
			}
			stage(current)
		}
	}
}
```

- [ ] **Step 3: Extract commit decoding**

Replace the JSON body of `handleCommitEntry` with a helper:

```go
func (m *Manager) handleCommitEntry(entry jetstream.KeyValueEntry) {
	commit, ok := m.decodeCommitEntry(entry)
	if !ok {
		return
	}
	m.handleCommitValue(commit)
}

func (m *Manager) decodeCommitEntry(entry jetstream.KeyValueEntry) (*types.AssignmentCommit, bool) {
	if entry.Operation() == jetstream.KeyValueDelete {
		return nil, false
	}
	var commit types.AssignmentCommit
	if err := json.Unmarshal(entry.Value(), &commit); err != nil {
		m.logError("failed to unmarshal commit", "error", err)
		return nil, false
	}
	return &commit, true
}

// workerAssignmentChanged reports whether `next` assigns a different
// partition slice to `workerID` than `prev`. Used by the commit watcher
// debounce path to force-flush pending before staging a new commit that
// would change this worker's local snapshot.
//
// "Different" means EITHER:
//   - Workers-membership flips (this worker present in one, absent in
//     the other) — case (d) revoke or case (c) acquire.
//   - Both have this worker but Payloads[workerID].PayloadHash differs
//     — case (c) acquire of a different partition set.
//
// PayloadHash is the authoritative content identity for
// AssignmentPayloadRef (types/assignment_commit.go:55-57): identical
// hashes mean identical canonical partition bytes. Equality on hash is
// therefore sound for "no partition-set change for this worker".
func workerAssignmentChanged(prev, next *types.AssignmentCommit, workerID string) bool {
	prevHas := commitContainsWorker(prev, workerID)
	nextHas := commitContainsWorker(next, workerID)
	if prevHas != nextHas {
		return true
	}
	if !prevHas {
		return false // worker absent in both — no diff for us
	}
	return prev.Payloads[workerID].PayloadHash != next.Payloads[workerID].PayloadHash
}

func commitContainsWorker(c *types.AssignmentCommit, workerID string) bool {
	if c == nil {
		return false
	}
	for _, w := range c.Workers {
		if w == workerID {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run unit tests**

Run:

```bash
go test ./... -run 'TestCommitWatcher_Debounce|TestCommitWatcher_PendingCommitFlushesOnClose|TestCommitWatcher_DebounceFlushesOnPartitionSetChange|TestCommitWatcher_DebounceIgnoresOutOfOrderLowerVersion|TestCommitWatcher_DebounceIgnoresOutOfOrderLowerVersionWithDifferentHash|TestWorkerAssignmentChanged|TestMonitorCommitChanges' -count=1
```

Expected:

```text
ok  	github.com/arloliu/parti/v2	...
```

## Task 3: Prove the Fix with the Rapid-Commit Diagnostic

> **Dispatch:** Claude `sonnet` (or Codex effort `medium`). Must not perturb sibling `TestApplyCoalescing_UnderReElectionBurst`. Run the post-impl-review loop until merge-clean before proceeding.

**Files:**
- Modify: `test/integration/manager/apply_coalescing_test.go`

- [ ] **Step 1: Factor the rapid-commit diagnostic**

Extract the current `TestApplyCoalescing_UnderRapidCommitBurst` setup, worker start, commit publication, result analysis, and logging into this helper:

```go
func runRapidCommitBurstDiagnostic(t *testing.T, debounce time.Duration) (map[string]burstReport, fleetReport) {
	t.Helper()

	const (
		numWorkers    = 5
		numPartitions = 25
		numCommits    = 4
		idleGap       = 50 * time.Millisecond
	)

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.FastTestConfig()
	cfg.ApplyStartJitter = 0
	cfg.AssignmentWatcherDebounce = debounce
	t.Logf("rapid commit burst diagnostic debounce=%s", debounce)

	collectors := make([]*recordingBurstCollector, numWorkers)
	for i := range collectors {
		collectors[i] = &recordingBurstCollector{NopMetrics: metrics.NewNop()}
	}

	wc := testutil.NewWorkerClusterWithSource(t, nc, source.NewStatic(testutil.CreateTestPartitions(numPartitions)), cfg)
	mgrs := make([]*parti.Manager, numWorkers)
	for i := range collectors {
		mgrs[i] = wc.AddWorkerWithOptions(ctx, parti.WithMetrics(collectors[i]))
	}
	defer wc.StopWorkers()

	wc.StartWorkers(ctx)
	t.Logf("all %d workers reached StateStable", numWorkers)

	workerIDs := make([]string, numWorkers)
	for i, m := range mgrs {
		workerIDs[i] = m.WorkerID()
	}

	baseCommit, _ := readAssignmentCommit(ctx, t, wc)
	wc.WaitForAssignmentVersion(baseCommit.Version, 20*time.Second)

	resetBurstCollectors(collectors)
	phaseStart := time.Now()
	publishedVersions := publishRapidCommitBurst(ctx, t, wc, mgrs, numCommits)
	wc.WaitForAssignmentVersion(publishedVersions[len(publishedVersions)-1], 20*time.Second)
	phaseEnd := time.Now()

	results := analyzeBursts(collectors, workerIDs, idleGap)
	for _, wid := range workerIDs {
		r := results[wid]
		t.Logf(
			"COMMIT_BURST worker=%s max_burst_size=%d max_burst_duration=%s p95_inter_arrival=%s total_attempts=%d",
			wid, r.MaxBurstSize, r.MaxBurstDuration.Round(time.Millisecond), r.P95InterArrival.Round(time.Millisecond), r.TotalAttempts,
		)
	}
	t.Logf("COMMIT_BURST_AGGREGATE max_burst_size=%d max_burst_duration=%s recommended_debounce_window=%s versions=%v",
		aggregateMaxBurstSize(results),
		aggregateMaxBurstDuration(results).Round(time.Millisecond),
		recommendedWindow(results),
		publishedVersions,
	)

	fleetReports := analyzeFleetBursts(collectors, idleGap, []phaseBound{{name: "commit_burst", start: phaseStart, end: phaseEnd}})
	commitBurst := fleetReports["commit_burst"]
	t.Logf("COMMIT_BURST_FLEET peak_concurrency=%d worst_version_span=%s versions_observed=%d",
		commitBurst.PeakConcurrency,
		commitBurst.WorstVersionSpan.Round(time.Millisecond),
		commitBurst.MultiWorkerVersions,
	)

	return results, commitBurst
}
```

- [ ] **Step 2: Keep the no-debounce before proof**

Make `TestApplyCoalescing_UnderRapidCommitBurst` call the helper with `0`:

```go
func TestApplyCoalescing_UnderRapidCommitBurst(t *testing.T) {
	if os.Getenv("PARTI_RUN_HERD_DIAGNOSTIC") != "1" {
		t.Skip("set PARTI_RUN_HERD_DIAGNOSTIC=1 to run")
	}

	results, commitBurst := runRapidCommitBurstDiagnostic(t, 0)

	require.GreaterOrEqual(t, aggregateMaxBurstSize(results), 2,
		"commit watcher path should expose a per-worker multi-version burst without commit debounce")
	require.GreaterOrEqual(t, commitBurst.MultiWorkerVersions, 2,
		"commit watcher path should expose multiple multi-worker commit-version fanouts")
}
```

- [ ] **Step 3: Add the with-debounce proof**

Add:

```go
func TestApplyCoalescing_UnderRapidCommitBurst_WithAssignmentDebounce(t *testing.T) {
	if os.Getenv("PARTI_RUN_HERD_DIAGNOSTIC") != "1" {
		t.Skip("set PARTI_RUN_HERD_DIAGNOSTIC=1 to run")
	}

	results, commitBurst := runRapidCommitBurstDiagnostic(t, 100*time.Millisecond)

	require.LessOrEqual(t, aggregateMaxBurstSize(results), 1,
		"commit debounce should collapse the per-worker multi-version burst")
	require.LessOrEqual(t, commitBurst.MultiWorkerVersions, 1,
		"commit debounce should leave only the final commit version as a multi-worker fanout")
}
```

The fleet `PeakConcurrency` may remain near worker count because all workers still observe the final version. That is expected; `ApplyStartJitter` is the separate control for spatial simultaneity.

- [ ] **Step 4: Run the opt-in diagnostic**

Run:

```bash
PARTI_RUN_HERD_DIAGNOSTIC=1 go test ./test/integration/manager \
  -run 'TestApplyCoalescing_UnderRapidCommitBurst' -v -count=3
```

Expected:

```text
PASS
```

The no-debounce variant should still report `max_burst_size >= 2`. The with-debounce variant should report `max_burst_size <= 1`.

## Task 4: Update Public Docs and Operator Guidance

> **Dispatch:** Claude `haiku` (or Codex effort `low`). Pure text edits with exact wording provided. Run the post-impl-review loop until merge-clean before proceeding (a docs-only diff still benefits from a wording-consistency check).

**Files:**
- Modify: `config.go`
- Modify: `docs/plans/thundering-herd-hardening/README.md`
- Modify: `test/integration/manager/apply_coalescing_test.go`

- [ ] **Step 1: Update `Config.AssignmentWatcherDebounce` Godoc**

Change the field comment from legacy-assignment-only language to:

```go
// AssignmentWatcherDebounce is the idle-window duration used to coalesce
// rapid bursts of assignment delivery watcher events into a single apply.
// It applies to both the legacy per-worker assignment alias watcher and
// the assignment._commit watcher. When > 0, each watcher path keeps the
// latest observed assignment target in a pending slot and processes it
// after the stream has been idle for the full window.
//
// Early-flush conditions (commit watcher only): the pending commit is
// flushed BEFORE the idle window elapses when (a) the watcher channel
// closes mid-window, or (b) a same-or-higher commit arrives that
// changes this worker's effective partition set — either a Workers
// membership flip (this worker added or removed) or a
// Payloads[workerID].PayloadHash difference. Both shapes are treated
// as ownership transitions; debounce collapses only commits whose
// per-worker partition set is unchanged. This preserves the two-phase
// handoff invariant that every ownership transition for this worker is
// observed as a discrete apply. Stale lower-version commits arriving
// after a higher pending version are dropped without flushing pending.
// On context cancellation (Stop), the pending commit is dropped
// without flushing — see the alias watcher's matching shutdown
// semantics. The legacy alias watcher does NOT yet have the "flush on
// assignment change" rule; harmonization is a follow-up.
```

Keep the existing default, recommendation, and 1 second cap text.

- [ ] **Step 2: Update diagnostic comments**

In `TestApplyCoalescing_UnderRapidCommitBurst`, change the explanatory comment from:

```go
// This isolates the remaining herd-capable path that AssignmentWatcherDebounce
// does not cover...
```

to:

```go
// This exercises the commit watcher path under a rapid commit burst with
// AssignmentWatcherDebounce=0 — the "before" half of the before/after
// proof. The "_WithAssignmentDebounce" counterpart proves the "after".
```

- [ ] **Step 3: Update the hardening README**

Add a new row or short follow-up section:

```markdown
| follow-up | `Config.AssignmentWatcherDebounce` also covers `assignment._commit` watcher updates | `0` (off) | Rapid commit bursts are staged for one idle window; identical-assignment bursts collapse to one apply, while same-or-higher commits that change this worker's partition set early-flush pending to preserve two-phase handoff. Stale lower-version commits are dropped. |
```

Add the rapid-commit diagnostic command:

```bash
PARTI_RUN_HERD_DIAGNOSTIC=1 go test ./test/integration/manager/ \
  -run 'TestApplyCoalescing_UnderRapidCommitBurst' -v -count=3
```

## Task 5: Final Validation

> **Dispatch:** Claude `haiku` (or Codex effort `low`). Mechanical command execution + tail reading. **If any command surfaces a new code issue, escalate diagnosis to `sonnet`/`opus` (or Codex `high`/`xhigh`) — do not re-dispatch the same model against a failing run.** Per-task review-loop discipline does not apply to this task (no code authored); instead, the *whole-implementation* final review runs after Task 5 passes — see `superpowers:subagent-driven-development`'s "Dispatch final code reviewer subagent for entire implementation" step.

**Files:**
- All modified files

- [ ] **Step 1: Format**

Run:

```bash
gofmt -w manager.go manager_assignment.go manager_commit_watcher_test.go test/integration/manager/apply_coalescing_test.go config.go
```

- [ ] **Step 2: Run focused unit tests**

Run:

```bash
go test ./... -run 'TestCommitWatcher_Debounce|TestCommitWatcher_PendingCommitFlushesOnClose|TestCommitWatcher_DebounceFlushesOnPartitionSetChange|TestCommitWatcher_DebounceIgnoresOutOfOrderLowerVersion|TestCommitWatcher_DebounceIgnoresOutOfOrderLowerVersionWithDifferentHash|TestWorkerAssignmentChanged|TestMonitorCommitChanges|TestConfig_AssignmentWatcherDebounce_Validation' -count=1
```

Expected:

```text
PASS
```

- [ ] **Step 3: Run diagnostic before/after proof**

Run:

```bash
PARTI_RUN_HERD_DIAGNOSTIC=1 go test ./test/integration/manager \
  -run 'TestApplyCoalescing_UnderRapidCommitBurst' -v -count=3
```

Expected:

```text
PASS
```

Expected log shape:

- no-debounce: `COMMIT_BURST_AGGREGATE max_burst_size >= 2`
- with-debounce: `COMMIT_BURST_AGGREGATE max_burst_size <= 1`

- [ ] **Step 4: Run default skip path**

Run:

```bash
go test ./test/integration/manager \
  -run 'TestApplyCoalescing_UnderRapidCommitBurst|TestApplyCoalescing_UnderReElectionBurst|TestRecommendedApplyJitter' -count=1
```

Expected:

```text
PASS
```

- [ ] **Step 5: Run lint**

Run:

```bash
make lint
```

Expected:

```text
0 issues.
```

- [ ] **Step 6: Run pre-PR gate**

Because this touches `manager/`-level behavior and `manager_assignment.go`, run:

```bash
make pre-pr
```

Expected:

```text
PASS
```

If the integration suite fails because embedded NATS cannot bind or the environment is overloaded, capture the exact failing tail and rerun the focused commit-watcher unit tests plus the opt-in rapid-commit diagnostic before reporting status.

## Risk Notes

- Debouncing the commit watcher delays reassignment by at most `AssignmentWatcherDebounce` when enabled. The default remains `0`.
- **Two-phase handoff correctness across skipped intermediates.** Collapsing a burst to the highest version is unsafe if an intermediate commit moved a partition this worker owns to a different worker and then back. `applyAssignmentWithPrev` diffs `previous` (the local snapshot) against `next`; if both contain the partition, `preparePhase` elides the reclaim and the claim store can diverge from the local snapshot (see `internal/assignment/handoff/twophase.go:208-232`, `334-386`; the A→B→A revert-race cleanup at `:289-315` only runs for partitions in the prepare diff, which is empty when the worker's `previous` already contains the partition). The plan's `stage()` enforces the invariant as follows: every **same-or-higher** commit that changes this worker's effective partition set (Workers membership or `Payloads[workerID].PayloadHash`) reaches `handleCommitValue` as a discrete apply, by force-flushing pending before staging the new commit. Stale **lower-version** commits arriving after a higher pending version are NOT in scope — they are dropped at the version-order guard before `workerAssignmentChanged` runs, because the higher pending version is the authoritative state and re-introducing the lower-version observation would only advance the alias-selector-visible `lastObservedCommit` to a stale value (`manager_assignment.go:732-740`). Bursts that don't reshuffle this worker (the common leader-churn / commit-republish case) still collapse to one apply. `TestCommitWatcher_DebounceFlushesOnPartitionSetChange` and `TestCommitWatcher_DebounceIgnoresOutOfOrderLowerVersionWithDifferentHash` together pin this.
- **Pre-existing analog on the legacy alias path.** `runAssignmentWatchSession` does not have the equivalent flush-on-change rule; its debounce simply replaces `pending` with the newest entry. The alias path's per-worker scope makes the failure mode narrower (the alias delivered to this worker IS this worker's view), but a calculator that reassigns the worker mid-burst still skips intermediates there. Harmonizing the alias path is out of scope here; this is recorded as a known limitation of `AssignmentWatcherDebounce > 0` predating this plan.
- Reconcile reads in `runCommitWatchSession` route through the same `stage()` path so a missed watcher event does not bypass the configured debounce window. The legacy-alias `runAssignmentWatchSession` reconcile arm does NOT have an equivalent staging — see the Design Decision section for why that residual bypass is bounded (30s tick + dual-read selector) and explicitly out of scope here.
- On manager shutdown, pending commits must be dropped rather than flushed, matching the assignment watcher's existing stop behavior.
- On watcher channel close, pending commits must be flushed before returning an error so the outer monitor can restart without losing the final delivered update.
- `pendingApplyInFlight` and `stashedCommit` remain necessary. Debounce collapses pre-apply watcher bursts; the in-flight guard still protects concurrent or long-running applies.
