# PR-3 Implementation Spec — Housekeeping

Bundles three small, low-risk items from
[`00-fix-plan.md`](./00-fix-plan.md):

- **ISSUE-006** — emergency-bypass comment drift (pure docs, 2 LOC)
- **ISSUE-008** — `DiscoverHighestVersion` cold-start serial O(K) Gets
  (skip-when-commit-exists optimisation + benchmark)
- **ISSUE-004** — `CleanupAllAssignments` docstring is misleading (pure docs)

No behaviour changes outside ISSUE-008's startup path, and that path's
externally-visible result is unchanged (same returned slice; only the work
done to compute it shrinks when a `_commit` already pins the version).

---

## 1. ISSUE-006 — emergency-bypass comment drift

### 1.1 The drift

`internal/assignment/calculator.go:833-834` (inside `checkForChanges` /
`TIER 0` emergency branch):

```go
// Trigger immediate emergency rebalance
// This will force-transition the state machine even if Scaling/Rebalancing
c.enterEmergencyState(ctx)
```

This is wrong. `enterEmergencyState` → `StateMachine.EnterEmergency`
(`state_machine.go:280-289`) explicitly **defers** when the current state
is `Rebalancing` or `Emergency`:

```go
if currentState == types.CalcStateRebalancing || currentState == types.CalcStateEmergency {
    sm.logger.Warn("emergency detected but rebalance already in progress - deferring",
        "current_state", currentState.String())
    return
}
```

The deferral behaviour is correct (the next poll catches it; cascading
rebalances would be worse). The comment is what needs to change.

### 1.2 Fix

Replace the second comment line with text that matches the actual
behaviour. The first comment line ("Trigger immediate emergency
rebalance") stays — it describes the intent of the call from Idle/Scaling.

```go
// Trigger immediate emergency rebalance from Idle/Scaling. If a
// rebalance is already in flight (Rebalancing/Emergency), EnterEmergency
// defers — the next poll will catch the topology change.
c.enterEmergencyState(ctx)
```

### 1.3 Tests

None. Per `tmp/assignment_review/07-verification-plan.md` §7.6:
"documentation-only ... no new test required."

---

## 2. ISSUE-008 — cold-start serial Gets in `DiscoverHighestVersion`

### 2.1 Current cost

`internal/assignment/assignment_publisher.go:846-900`.

The flow:
1. `ListKeys` returns every key under `p.keyPrefix`.
2. Try `Get(_commit)` — when present, seed `lastCommitRev` and
   `currentVersion` from the commit's `Version`.
3. **Unconditionally** iterate every non-protocol key and `Get` it to
   read its `Assignment.Version`, just to track `highestVersion`.

Step 3 is O(K) serial NATS round-trips. For clusters with K=1000 legacy
aliases that's ~5s of leader startup latency. When `_commit` already
exists, `currentVersion` is already pinned to `commit.Version` (which is
≥ any legacy alias version, because all subsequent versions only land via
`Publish` → `commit`), so the per-key Gets contribute nothing to the
returned `currentVersion`.

The returned `[]string` of worker IDs **does** depend on every key —
that's the caller's contract for "which legacy worker aliases still
exist." We can drop the per-key `Get`, but we must still build
`workerIDs` from the listed keys.

### 2.2 Fix

When the commit-seed branch succeeds, skip the per-key `Get`. Build
`workerIDs` directly from the key list.

```go
commitSeeded := false
commitKey := p.keyPrefix + commitKeyName
if entry, gerr := p.assignmentKV.Get(ctx, commitKey); gerr == nil {
    var commit types.AssignmentCommit
    if jerr := json.Unmarshal(entry.Value(), &commit); jerr == nil {
        p.mu.Lock()
        p.lastCommitRev = entry.Revision()
        if commit.Version > p.currentVersion {
            p.currentVersion = commit.Version
        }
        p.mu.Unlock()
        commitSeeded = true
    }
}

highestVersion := int64(0)
workerIDs := make([]string, 0, len(keys))
for _, key := range keys {
    sub := strings.TrimPrefix(key, p.keyPrefix)
    if isProtocolSubcomponent(sub) {
        continue
    }
    if commitSeeded {
        // currentVersion is already pinned by the commit; we only need
        // the worker-ID set, not per-alias version reads.
        workerIDs = append(workerIDs, sub)
        continue
    }
    asgn, _, err := kvutil.GetJSON[types.Assignment](ctx, p.assignmentKV, key)
    if err != nil || asgn == nil {
        continue
    }
    if asgn.Version > highestVersion {
        highestVersion = asgn.Version
    }
    workerIDs = append(workerIDs, sub)
}

p.mu.Lock()
if highestVersion > p.currentVersion {
    p.currentVersion = highestVersion
}
p.mu.Unlock()
```

Externally-observable behaviour:
- When `_commit` exists: `currentVersion = commit.Version` (unchanged).
  `workerIDs` is the same set (every legacy alias key still appears).
  Per-key Gets are skipped → ~O(1) NATS round-trips for the version-pin,
  + 1 ListKeys.
- When `_commit` is absent: identical to today. `commitSeeded = false`,
  so the per-key Gets run as before.

Note: when `commitSeeded=true` we previously skipped any keys whose
`GetJSON` returned `err != nil || asgn == nil` (treating them as
malformed). After the fix, those keys appear in `workerIDs` based on
their key name alone. This is the correct semantic — a malformed value
for an alias key still implies "this legacy worker had an alias here";
the cleanup path doesn't care about the alias's version content. The
prior filter was incidental, not load-bearing.

### 2.3 Test impact

Existing `TestAssignmentPublisher_DiscoverHighestVersion_LegacyOnly`
(`assignment_publisher_test.go:914`) and
`TestPublisher_LegacyBootstrap_NoCommit_RecoversViaDiscoverHighestVersion`
(`:244`) use the no-commit path → unaffected.

`TestAssignmentPublisher_CleanupAllAssignments_PreservesProtocolKeys`
(`:963`) and `TestAssignmentPublisher_CleanupAllAssignments_PreservesPayloadKeys`
(see `:725`-area test) pre-populate protocol keys (commit included) and a
legacy alias, then call `DiscoverHighestVersion` and assert the returned
worker-ID set. Per the spec above, the returned set is unchanged → these
keep passing.

### 2.4 Benchmark

New benchmark per `tmp/assignment_review/07-verification-plan.md` §7.8:

**File:** `internal/assignment/assignment_publisher_test.go` (append).

**Name:** `BenchmarkDiscoverHighestVersion_WithCommit`.

**Setup:**
- Reuse the existing test fixture (whatever sets up an embedded JetStream
  + assignmentKV — see how other tests construct `f.pub`).
- Pre-populate the bucket with K=200 legacy alias keys (`assignment.<id>`)
  + 1 `_commit` key. K=200 is a tractable benchmark size — proportional
  evidence for K=1000 follows from the asymptotic. Use a single shared
  setup per `b.N` iteration via `b.ResetTimer()`.

**Body:**
```go
func BenchmarkDiscoverHighestVersion_WithCommit(b *testing.B) {
    f := newPublisherFixture(b)  // existing helper or analog
    ctx := context.Background()
    const K = 200
    // ... seed K alias keys + a _commit key ...
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        if _, err := f.pub.DiscoverHighestVersion(ctx); err != nil {
            b.Fatal(err)
        }
    }
}
```

No correctness assertion — purely a measurement. Result (informational)
should show post-fix `ns/op` ≈ ListKeys + 1 Get, independent of K.

If the precise helper signature differs from `newPublisherFixture(b)`,
match whichever helper the existing tests use (e.g.,
`setupAssignmentPublisherTest(t)` patterns at
`assignment_publisher_test.go` top — use the same setup; promote to
`testing.TB` if needed).

---

## 3. ISSUE-004 — `CleanupAllAssignments` docstring correction

### 3.1 The drift

`internal/assignment/assignment_publisher.go:902-911` currently says:

```go
// CleanupAllAssignments removes all LEGACY assignment aliases from KV.
//
// Protocol keys (assignment._commit, assignment._commit_log.*,
// assignment._payload.*) are NEVER deleted by this method — they are the
// authoritative cluster state, and a successor leader needs them to continue
// the CAS chain and to GC payloads safely.
//
// Call this on graceful Calculator shutdown to remove per-worker compat
// aliases that would otherwise linger and confuse a downgraded peer.
func (p *AssignmentPublisher) CleanupAllAssignments(ctx context.Context) error {
```

Two problems:

1. **"on graceful Calculator shutdown"** is misleading. The method has
   no production caller (`grep -r CleanupAllAssignments` shows only test
   references and the definition itself). It is NOT wired into
   `stopCalculator` or any shutdown path.
2. The method sweeps **every** non-protocol key, including aliases for
   workers that are still ACTIVE in the cluster. Calling it on
   leader-step-down would yank live workers' aliases. Per
   `00-fix-plan.md` §ISSUE-004 (P4): the docstring's "graceful Calculator
   shutdown" probably meant "entire-cluster shutdown" (admin / teardown),
   but the docstring doesn't say so.

### 3.2 Fix

Rewrite the call-context paragraph. No code changes. No new method.

```go
// CleanupAllAssignments removes ALL legacy per-worker assignment aliases
// from KV, including aliases for workers that may still be active in the
// cluster.
//
// Protocol keys (assignment._commit, assignment._commit_log.*,
// assignment._payload.*) are NEVER deleted by this method — they are the
// authoritative cluster state, and a successor leader needs them to continue
// the CAS chain and to GC payloads safely.
//
// Intended use: admin tooling / whole-cluster teardown where every parti
// instance is being decommissioned together. NOT safe to call on
// leader-step-down or single-node shutdown: it will yank aliases for
// workers that are still serving traffic on peer leaders, which would
// then require those workers to wait for the next rebalance to be
// re-published. There is currently no production caller; the method
// exists for tests and operator scripts.
//
// A leader-step-down-safe variant would need a `CleanupInactiveAssignments(
// activeWorkers []string)` method that preserves aliases of currently-live
// workers — out of scope here.
func (p *AssignmentPublisher) CleanupAllAssignments(ctx context.Context) error {
```

### 3.3 Tests

None. The existing tests call this method against a synthetic fixture
where "every legacy alias should be swept" is the intended behaviour,
and they still pass.

---

## 4. Compatibility

| Surface | Change | Compat |
|---|---|---|
| `calculator.go` comment | Pure comment update. | ✅ |
| `AssignmentPublisher.DiscoverHighestVersion` | Behaviour: returned `[]string` and post-call `currentVersion` are unchanged for both code paths. Internal work is cheaper when `_commit` exists. | ✅ |
| `AssignmentPublisher.CleanupAllAssignments` | Pure docstring update. Method behaviour unchanged. | ✅ |
| Wire format / KV schema | Unchanged. | ✅ |
| Rollback | Clean revert. | ✅ |

---

## 5. Risk audit

| Risk | Mitigation |
|---|---|
| ISSUE-008 fix changes the malformed-alias edge case (now appears in `workerIDs`) | Documented in §2.2; cleanup path doesn't read alias version content. The no-commit path is unchanged, preserving the historical filter for the bootstrap-from-scratch case. |
| Benchmark fails to compile under existing fixture API | If `newPublisherFixture(b testing.TB)` doesn't match an existing helper, mirror the setup the closest sibling test uses. Spec is non-prescriptive about the fixture call shape. |
| Docstring exaggerates risk and discourages legitimate test use | The "Intended use: admin tooling" line preserves the test-affordance framing. |

---

## 6. LOC budget

| File | Estimated LOC |
|---|---|
| `calculator.go` (comment swap) | +2 / -1 |
| `assignment_publisher.go` (DiscoverHighestVersion + CleanupAllAssignments docstring) | +12 / -3 |
| `assignment_publisher_test.go` (benchmark) | +25 |
| Total | ~10 LOC production + ~25 LOC test |

Matches the plan's "~25 LOC + 1 benchmark" estimate.

---

## 7. Out of scope

- ISSUE-001 (PR-4, gated on repro).
- ISSUE-007 (PR-2, merged in commit `4c0fdb4`).
- Adding `CleanupInactiveAssignments(activeWorkers []string)` — design
  discussion noted in the docstring, not implemented.
- Parallelising the no-commit branch of `DiscoverHighestVersion` via
  errgroup — not needed once the commit-exists branch is cheap; the
  no-commit branch only runs on truly first-time-ever leader bootstrap
  where K is small.
- Wiring `CleanupAllAssignments` into any production lifecycle.
