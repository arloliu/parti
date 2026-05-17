# PR-4 Implementation Spec — Publisher CAS-Loss Refresh

Implements **ISSUE-001** from
[`00-fix-plan.md`](./00-fix-plan.md).

**Gating evidence (now satisfied):** the repro at
`internal/assignment/issue001_repro_test.go` (drafted during the
investigation, will be promoted to `assignment_publisher_test.go` by
this PR) demonstrates that a valid, healthy leader can land in an
indefinite `ErrCommitCASFailed` loop. The Codex-flagged hypothesis that
the pre-alias `LeaderCheck` fence catches this in production is
empirically false: that fence is an *election* fence (does the live
election key still name this worker?), not a *commit-rev* fence — a
valid leader passes it cleanly while its `lastCommitRev` is stale.

---

## 1. The bug, in one paragraph

When `_commit`'s live KV revision advances past
`p.lastCommitRev` (the only production source is another leader's
in-flight CAS landing during a leader handoff), this leader's CAS
fails. `Publish` returns `ErrCommitCASFailed` but does **not** refresh
`p.lastCommitRev` from the live entry. Every subsequent `Publish` from
this leader fails the same way, indefinitely — until either a
Calculator restart (which calls `BootstrapLastCommit`) or another
leader handoff occurs. Workers continue on the old assignment but the
cluster cannot rebalance.

---

## 2. Fix design

On `ErrCommitCASFailed`, re-Get `_commit` and refresh
`p.lastCommitRev`. Single inline op under the existing `p.mu` lock
(do **not** call `BootstrapLastCommit` — it takes `p.mu` and would
deadlock with `Publish`).

### 2.1 Scope of the refresh

- **Refresh `p.lastCommitRev` only.** Do not also refresh
  `p.lastCommit` (the payload cache used by the audit) or
  `p.lastCommitObservedAtMono`. Rationale:
  - Audit drift is brief — the next `Publish` (which is now unblocked)
    will refresh `lastCommit` and `lastCommitObservedAtMono` on the
    successful CAS write.
  - Touching the payload cache from the CAS-failure path would
    encode "we believe the other leader's commit is now authoritative"
    into our audit state, which is racy: we don't know whether the
    other leader's commit semantically matches what we wanted to
    publish.
  - Minimum change to fix the stuck-leader symptom.
- **Refresh on `ErrCommitCASFailed` only.** Other Publish error paths
  (`ErrCoverageMismatch`, `ErrLeadershipLostPreAlias`,
  `ErrLeadershipLostPostAlias`, `ErrAliasBarrierFailed`, payload write
  errors, KV transient errors on Update other than CAS conflict) do
  NOT touch `lastCommitRev`. They are independent failure modes; their
  recovery is the existing retry-on-next-rebalance-event path.

### 2.2 Code change

In `assignment_publisher.go`, the CAS-error branch
(currently `:421-432`):

```go
if err != nil {
    // CAS lost OR Create lost (another leader created the commit first).
    // Either way: surrender. The lost batch's payload writes are inert;
    // the legacy aliases written at step 6 are documented exposure.
    p.metrics.IncrementCommitAborts()
    p.metrics.IncrementBatchAborted("commit_cas_failed")
    if aliasWritten {
        p.metrics.IncrementAliasVisibleUncommitted()
    }

    // ISSUE-001: when the failure is a true CAS conflict, refresh
    // lastCommitRev from the live _commit so the next Publish can
    // re-CAS. Transient KV errors short-circuit inside the helper.
    p.refreshLastCommitRevAfterCASFailureLocked(ctx, err)

    return fmt.Errorf("%w: %w", types.ErrCommitCASFailed, err)
}
```

The CAS-vs-transient gate lives inside the helper (see §2.3 helper
shape) — keeping the cyclomatic complexity of `Publish` unchanged
and putting the error-class predicate in a single, easily-audited
place.

Note: the existing `fmt.Errorf("%w: %w", types.ErrCommitCASFailed, err)`
wrap tags ALL Create/Update errors as `ErrCommitCASFailed` from the
caller's perspective — that is pre-PR-4 behavior and is left
unchanged. The new gate concerns only the internal refresh side-effect.

New private helper, placed near `BootstrapLastCommit`:

```go
// refreshLastCommitRevAfterCASFailureLocked re-reads the live _commit
// KV revision and updates p.lastCommitRev so the next Publish can
// re-CAS. Caller must hold p.mu. Best-effort: KV errors and missing
// entries leave lastCommitRev unchanged (the next refresh attempt or
// a Calculator restart will recover).
func (p *AssignmentPublisher) refreshLastCommitRevAfterCASFailureLocked(ctx context.Context) {
    commitKey := p.keyPrefix + commitKeyName
    entry, err := p.assignmentKV.Get(ctx, commitKey)
    if err != nil {
        p.logger.Debug("post-CAS-failure refresh: could not read live _commit",
            "key", commitKey, "error", err)
        return
    }
    if entry.Revision() > p.lastCommitRev {
        p.lastCommitRev = entry.Revision()
    }
}
```

That's the entire production change.

### 2.3 Why not call `BootstrapLastCommit`?

- `BootstrapLastCommit` acquires `p.mu` (`:1055`). Calling it from
  `Publish` (which already holds `p.mu` for its full duration —
  `:313-314`) would deadlock on the same goroutine attempting to
  re-lock a non-reentrant `sync.Mutex` → panic in tests, hang in prod.
- `BootstrapLastCommit` also refreshes `lastCommit` and
  `lastCommitObservedAtMono` — see §2.1 for why we deliberately don't.

---

## 3. Tests

### 3.1 Promote the repro

Move the investigation file `issue001_repro_test.go` content (the test
body) into `assignment_publisher_test.go` with the name
`TestPublisher_CASFailure_RefreshesLastCommitRev_AndRecovers`. The
pre-fix assertions in the draft (asserting the second Publish ALSO
returns `ErrCommitCASFailed`) are inverted to post-fix assertions:

```go
// First Publish after external write → CAS must fail (the external
// write advanced the live revision past our cached lastCommitRev).
err := f.pub.Publish(ctx, /* same input as initial publish */)
require.Error(t, err)
require.ErrorIs(t, err, types.ErrCommitCASFailed)

// Second Publish must SUCCEED — the post-CAS-failure refresh updated
// lastCommitRev from the live entry, so the next CAS is against the
// current revision.
require.NoError(t, f.pub.Publish(ctx, /* same input */))
```

Delete the investigation file `issue001_repro_test.go` after promoting
(it lives only in this worktree; it was not committed).

### 3.2 Negative test — non-CAS errors do NOT refresh

Add `TestPublisher_NonCASFailures_DoNotRefreshLastCommitRev`. Setup:

- Successful initial publish → `lastCommitRev = R`.
- Force a Publish failure that is NOT `ErrCommitCASFailed` — easiest
  shape: drive `LeaderCheckFn` to return a mismatch so Publish returns
  `ErrLeadershipLostPreAlias` (use the fixture's `leaderRev.Store()`
  pattern at `assignment_publisher_test.go:65` — the existing tests
  do this; mirror that).
- Assert `lastCommitRev` is still `R`.

Confirms the refresh is gated on the right error class — we don't
incur the refresh-Get on every Publish failure.

### 3.3 Refresh-failure tolerance

Not a separate test. The refresh helper logs and returns on error
(KV unreachable, key missing). Existing test infrastructure doesn't
have a clean seam to inject a KV-Get failure mid-flow without heavy
mocking; the behavior is asserted by code review (`return` on err)
and covered by the regression test (where the refresh succeeds).

### 3.4 Non-CAS KV write-error gating

Not a separate test. The CAS-failure branch gates the refresh call
on `errors.Is(err, jetstream.ErrKeyExists)` — the JetStream sentinel
for both Create-collision and Update-revision-mismatch. Inducing a
non-ErrKeyExists write error on the embedded NATS fixture (e.g., a
network-layer fault during `Update`) requires a wrapping KV mock
that would dwarf the production change in setup cost. The gate is a
single `errors.Is` predicate plain to inspect; combined with the
pre-alias-failure negative test (§3.2) which proves non-CAS errors
from a different code path don't touch `lastCommitRev`, code review
is the appropriate test surface.

---

## 4. Compatibility

| Surface | Change | Compat |
|---|---|---|
| `AssignmentPublisher.Publish` | Behavior on `ErrCommitCASFailed`: now refreshes `lastCommitRev` from live `_commit` before returning. Return error and signature unchanged. | ✅ |
| New private method `refreshLastCommitRevAfterCASFailureLocked` | Additive; private. | ✅ |
| `p.lastCommit` payload cache | Unchanged on CAS failure (see §2.1). | ✅ |
| `p.lastCommitObservedAtMono` | Unchanged on CAS failure. | ✅ |
| Wire format / KV schema | Unchanged. | ✅ |
| Metrics | `commit_aborts` / `commit_cas_failed` / `alias_visible_uncommitted` increment as before. No new metric. | ✅ |
| Rollback | Clean revert (single helper + single call site). | ✅ |

---

## 5. Risk audit

| Risk | Mitigation |
|---|---|
| Refresh Get itself fails transiently | Best-effort: leave `lastCommitRev` unchanged. Next CAS will fail again and trigger another refresh attempt; a Calculator restart is the final recovery path (same as today). |
| Refresh Get returns a revision LOWER than `p.lastCommitRev` | Guarded: only update when `entry.Revision() > p.lastCommitRev`. (Would only happen on a clock/replication anomaly in NATS; defensive.) |
| Audit reads stale `lastCommit` payload while CAS-failure recovery is in flight | Brief — the next `Publish` (now unblocked) refreshes the payload cache via the successful-CAS path. Audit grace windows tolerate this. See §2.1. |
| The "external writer" was a malicious split-brain — refreshing makes us co-conspire | Not a regression: pre-fix, the leader would still attempt to publish after a Calculator restart against the same live state. The refresh just removes the multi-hour stuck window. Election fence still prevents split-brain leaders from publishing. |
| New helper holds `p.mu` while doing a KV Get | Identical to what `Publish` already does on its happy path (the CAS Update itself is a KV op under `p.mu`). No new lock-hold extension beyond an existing pattern. |

---

## 6. LOC budget

| File | Estimated LOC |
|---|---|
| `assignment_publisher.go` (1-line call + ~12 LOC helper + comment) | ~15 |
| `assignment_publisher_test.go` (promote repro + add negative test) | ~80 |
| Total | ~15 LOC production + ~80 LOC tests |

Matches the plan's "~50 LOC + 2 tests" estimate (production is well
under; tests are slightly over because the repro promotes verbatim).

---

## 7. Out of scope

- ISSUE-002/003/004/005/006/007/008 (other PRs, all merged).
- Refreshing `p.lastCommit` payload cache on CAS failure (see §2.1
  rationale).
- Adding a metric for the refresh attempt — existing
  `commit_aborts` / `commit_cas_failed` metrics already indicate a
  CAS failure happened; a dedicated "refresh attempted/succeeded"
  metric would be over-engineering for a code path that should
  rarely fire.
- Generalizing the refresh helper into a "post-failure recovery"
  framework — the only failure with this recovery shape is
  `ErrCommitCASFailed`. Other error classes have different recovery
  semantics (§2.1).
- Replacing the `BootstrapLastCommit` lock acquisition with a
  reentrant lock or split-method design — the deadlock is avoided
  by writing the helper inline; no broader refactor needed.
