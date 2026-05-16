# Phase 4 Follow-up — Gap 3: preparePhase recovery for stuck-prepare claims

## Origin

A latent bug in `internal/assignment/handoff/twophase.go` `preparePhase`
was identified during the v2.3.0 partition-reassignment failure
investigation and parked while the claim-resolver freeze fix was
landed. Phase 4's audit_repair makes the trigger condition materially
more likely than it was in v2.3.0 (rapid A→B→A reassignments under
"behind worker" classification), so this is the next-priority gap to
close.

## Bug

`preparePhase` at `internal/assignment/handoff/twophase.go:259-276`
short-circuits with `return nil, nil` when `cur.Owner == workerID`,
regardless of `cur.State` or `cur.PendingOwner`:

```go
// Existing claim; if stable and owned by someone else, enter prepare
if cur.Owner != workerID {
    prepared := cur.NextPrepare(workerID, now)
    // ...
    return &prepared, nil
}

return nil, nil   // <-- BUG: returns nil even if state != stable
```

`commitPhase` (line 285+) and `stabilizePhase` (line 336+) also no-op
on the stuck shape (`owner=workerID, state=prepare,
pendingOwner=otherWorker`), so the claim stays at `prepare` forever.
The processing gate then suppresses pulls with
`state_not_allowed(prepare)` indefinitely until something else writes
the claim (e.g., the next claim sweep — which only resets *expired*
non-stable claims).

### Sequence that produces it

1. Initial: claim `(owner=A, state=stable, epoch=1)`.
2. Leader reassigns A→B in commit V.
3. Worker B's Apply runs `preparePhase` for P, leaving claim at
   `(owner=A, pending=B, state=prepare, epoch=2)`.
4. Before B's `commitPhase` lands, leader publishes V+1 reverting P
   back to A (audit_repair, worker scaling, etc.). Worker B's Apply
   either aborts mid-run (context cancellation), or completes for V
   but its outputs are immediately superseded by V+1.
5. Worker A applies V+1 (`prev` does NOT include P;
   `next` includes P). `preparePhase` reads the stuck claim,
   sees `cur.Owner == workerID`, and returns nil. `commitPhase` and
   `stabilizePhase` likewise no-op.
6. Claim is permanently stuck.

### Symptom

Processing gate emits `pull suppressed reason="state_not_allowed(prepare)"`
on the partition. The user's reported production symptom was
`not_owner(...)` (different code path — the claim-resolver cache
freeze fix landed in `5bc46cc` closed that). The stuck-prepare bug
is a *distinct* failure mode that produces a different log line; it
is real but did not produce the specific report. With Phase 4's
audit_repair on, it is now more likely to fire than in v2.3.0.

## Existing reproducer

A reproducer test has been written to the working tree at
`internal/assignment/handoff/twophase_stuck_prepare_test.go`:

- `TestTwoPhase_PreparePhase_RecoversStuckPrepareOnReacquire` — seeds
  the stuck claim with a recent `LastUpdated` (so the opportunistic
  sweep does NOT mask the bug), runs `coord.Apply` as the re-acquiring
  owner, asserts the claim converges to `state=stable`. **Fails on
  current code** (state stays at `prepare`).
- `TestTwoPhase_PreparePhase_ReacquireIdempotentOnAlreadyStable` —
  guards the fix from regressing the idempotent-re-apply path; seeds a
  clean stable claim, runs Apply, asserts no gratuitous epoch bump.
  **Passes on current code** and must continue to pass after the fix.

Both tests must pass after the fix. Do NOT modify the assertions to
make them pass — the fix must produce the asserted end state.

## Scope

Files in scope:

- `internal/assignment/handoff/twophase.go` — `preparePhase` fix.
- `internal/assignment/handoff/twophase_stuck_prepare_test.go` —
  pre-existing reproducer (carry through; add additional tests below).
- `internal/assignment/handoff/twophase_test.go` — extend with
  additional edge-case tests if needed.

Files explicitly out of scope:

- `commitPhase` / `stabilizePhase` semantics (the preparePhase fix is
  sufficient — once prepare leaves the claim at stable, the later
  phases correctly no-op).
- `maybeSweepClaims` (the sweep is best-effort and runs on expired
  claims only; the preparePhase fix removes the dependency on the
  sweep for recovery).
- `direct.go` direct coordinator (no two-phase, no claim state to be
  stuck in).
- Any code outside `internal/assignment/handoff/`.

## Fix

Augment `preparePhase`'s `cur.Owner == workerID` branch to detect a
stale handoff and reset the claim to clean stable state when needed.
Suggested shape:

```go
// Existing claim; if stable and owned by someone else, enter prepare
if cur.Owner != workerID {
    prepared := cur.NextPrepare(workerID, now)
    // ... existing log + return &prepared, nil
}

// cur.Owner == workerID. The partition is being re-acquired by its
// existing owner. If a stale in-flight handoff to another worker is
// still recorded on the claim (state != stable or pendingOwner !=
// ""), reset it back to clean stable. This handles the A->B->A revert
// race where B's commitPhase never completed: without this reset, the
// claim stays at state=prepare forever and the processing gate
// suppresses pulls with state_not_allowed(prepare).
if cur.State != ClaimStateStable || cur.PendingOwner != "" {
    cleaned := *cur
    cleaned.PendingOwner = ""
    cleaned.State = ClaimStateStable
    cleaned.Epoch++
    cleaned.LastUpdated = now.UTC()
    if t.cfg.Logger != nil {
        t.cfg.Logger.Info("handoff_prepare_reset_stale",
            "partition_id", pid,
            "worker_id", workerID,
            "prev_state", string(cur.State),
            "prev_pending", cur.PendingOwner,
            "epoch", cleaned.Epoch,
        )
    }
    return &cleaned, nil
}

return nil, nil
```

Why a reset (not a forward transition through commit/stabilize) is
the right semantics: the worker is re-acquiring a partition it
already owns per the claim. The "in-flight handoff to another
worker" state is by definition stale (that handoff never completed
and is now being superseded). The end state is the same as
forward-transitioning, but a reset is one CAS instead of two and
avoids fabricating prepare/commit log lines for a transition that
never happened cleanly.

The fix MUST NOT regress the idempotent-re-Apply path
(`cur.Owner == workerID && cur.State == stable && cur.PendingOwner ==
""` → no-op, no epoch bump). The condition `cur.State != stable ||
cur.PendingOwner != ""` is exactly the negation of "already clean
stable," so falling through to the existing `return nil, nil` for
clean claims is preserved.

## Metrics

Add (and emit) one new counter:

- `IncClaimStaleHandoffReset()` on the existing handoff metrics
  interface (find it via `grep -rn "ClaimStoreStale\|handoffMetrics"`
  — there's already a `IncClaimStoreStale` for the sweeper). Increment
  when the new reset path fires. This makes the recovery observable
  in production.

If adding to the existing interface, update ALL implementations
(noop, Prometheus, simulation) so the build stays green.

## Test coverage required

The reproducer already exists in the working tree. Carry it through to
the commit (it's a new file, untracked). Add the additional tests
listed below.

1. **(carried through)** `TestTwoPhase_PreparePhase_RecoversStuckPrepareOnReacquire`
   — must pass after the fix.
2. **(carried through)** `TestTwoPhase_PreparePhase_ReacquireIdempotentOnAlreadyStable`
   — must continue to pass (no epoch bump on clean stable claim).
3. **New** `TestTwoPhase_PreparePhase_RecoversFromStaleCommit` —
   seed claim `(owner=A, pending="", state=commit, epoch=3,
   LastUpdated=now)`. Worker A re-acquires (`prev=empty, next=[P]`).
   Assert claim ends at `(owner=A, state=stable)` with epoch advanced
   to at least 4. This covers the case where a previous Apply got
   through commit but not stabilize — also a stuck-non-stable shape
   that the fix should resolve.
4. **New** `TestTwoPhase_PreparePhase_RecoversFromStaleAbort` — seed
   claim `(owner=A, pending="", state=abort, epoch=3, LastUpdated=now)`.
   Assert recovery to `(owner=A, state=stable)`. Documents that the
   fix is defensive for any non-stable state when owner is self.
5. **New** `TestTwoPhase_StaleHandoffResetMetric` — same scenario as
   test 1, with a metrics spy. Assert `IncClaimStaleHandoffReset` is
   called exactly once.
6. **New** `TestTwoPhase_PreparePhase_OwnedByOtherUnchangedSemantics`
   — regression guard: with a claim `(owner=otherWorker, state=stable)`
   and this worker re-acquiring, assert `NextPrepare` still fires
   (the existing `if cur.Owner != workerID` branch is untouched by
   the fix). Verifies the fix doesn't accidentally redirect the
   stable-other-owner path.

## Validation gates

```
make lint
go test ./internal/assignment/handoff/... -race -count=1 -timeout 60s
go test ./... -race -count=1 -short -timeout 300s
go vet ./...
go build ./...
```

`TestManager_DegradedHook` is a documented flake outside scope; rerun
once if it fails.

## Verify the reproducer fails on parent

Before declaring the fix done, verify the reproducer fails on the
parent commit (current `main` HEAD before your changes):

```
git stash --include-untracked
git checkout HEAD -- internal/assignment/handoff
# Copy the reproducer into the parent tree:
git stash pop -- internal/assignment/handoff/twophase_stuck_prepare_test.go || true
go test ./internal/assignment/handoff/ -run TestTwoPhase_PreparePhase_RecoversStuckPrepareOnReacquire -count=1 -timeout 30s
# Expected: FAIL
git stash drop || true
```

(Adjust the exact mechanism — the goal is to prove the reproducer
fails on the unfixed code. The test file already does fail when run
against the current `twophase.go`; the agent has likely already
observed this if they ran the file before writing the fix.)

## Non-goals

- Do NOT touch `commitPhase` or `stabilizePhase` defensively. The
  preparePhase fix is sufficient by analysis (once prepare leaves the
  claim clean stable, the later phases correctly no-op). Defensive
  changes risk breaking the existing prepare→commit→stabilize flow.
- Do NOT change `maybeSweepClaims`.
- Do NOT modify the resolver, the manager, or any code outside the
  handoff package.
- Do NOT add an "unknown state" defensive branch — `ClaimStateUnknown`
  is a parsing fallback and the existing code already handles it
  acceptably.

## Risk / rollback

- Single function modified. No public API change. No data plane
  change.
- All existing two-phase tests must continue to pass unchanged.
- Rollback: revert the commit.

## Commit message template

```
fix(handoff): preparePhase recovers stuck-prepare claims on re-acquire

Closes a latent two-phase coordinator bug where a partition's claim
could be permanently stuck at (owner=A, pending=B, state=prepare)
after an A->B->A reassignment whose new-owner commit never landed.
preparePhase short-circuited with return nil when cur.Owner ==
workerID regardless of state/pending; commitPhase and stabilizePhase
also no-opped on that shape. The processing gate then suppressed
pulls indefinitely with state_not_allowed(prepare).

Phase 4's audit_repair path makes the trigger materially more
likely than it was in v2.3.0, motivating this targeted fix.

preparePhase now detects cur.Owner == workerID combined with a
non-clean stable state and resets the claim to (owner=self,
pending="", state=stable, epoch+1). Idempotent re-apply on a
clean stable claim is preserved (no gratuitous epoch bump).

Adds IncClaimStaleHandoffReset metric, emitted on the new recovery
path so operators can observe how often the race fires.

Reproducer + regression guards in twophase_stuck_prepare_test.go.
```

DO NOT add Co-Authored-By or any attribution trailers.
