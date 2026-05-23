# P1.2 (F3) — stableID NotFound classification → ErrClaimLost

Per-PR spec for the fifth PR (second of Phase 1)
(`00-fix-plan.md` §P1.2). Lazy-written; prior PR P1.1
(`self-healing-p11-f6a-source-unavailable-hook`) committed.

## Empirical correction to the plan (discovered during implementation)

The plan says: "classify `jetstream.ErrBucketNotFound` and
`jetstream.ErrStreamNotFound` as claim-loss in `Claimer.renew`."

Empirical surface, against nats.go v1.50.0, of every KV op on a
**cached KV handle** after `js.DeleteKeyValue(bucket)`:

| Operation | Error returned |
|---|---|
| `kv.Update(...)` | `jetstream.ErrNoStreamResponse` |
| `kv.Create(...)` | `jetstream.ErrNoStreamResponse` |
| `kv.Delete(key)` | `jetstream.ErrNoStreamResponse` |
| `kv.Get(...)`    | `nats.ErrNoResponders` |
| `js.KeyValue(...)` lookup | `jetstream.ErrBucketNotFound` |
| `kv.Watch(...)`  | `jetstream.ErrStreamNotFound` |

**Crucially: `kv.Update` returns NEITHER of the plan's named errors.**
The classifier must include `ErrNoStreamResponse` to detect the
bucket-loss case that `Claimer.renew` actually encounters in
production. The plan's named errors are kept for defense-in-depth
(future nats.go versions could change behavior, and certain retry
paths inside nats.go may surface them).

Memory pin: [[project-nats-kv-delete-surface]]
(also relevant for P1.3 F1 and the F2 envelope work).

## Background

`Claimer.renew` (`internal/stableid/claimer.go:362-370`) issues a
CAS `kv.Update` on the claim key to re-stamp the timestamp. Today
only `jetstream.ErrKeyExists` (the revision-mismatch case) is
classified as `ErrClaimLost`; everything else is wrapped in a
generic `"failed to renew ID %s"`.

If the stableID bucket is deleted under a running worker, every
subsequent renew tick returns `ErrNoStreamResponse`. The worker
treats this as a generic error, logs it, keeps running — holding
a stale claim that the bucket no longer recognizes. The
documented contract (`docs/OPERATIONS.md`) says claim-loss
triggers `claimLostShutdown` and pod rotation; this PR makes that
contract actually hold.

## Scope (small surface — classifier-only)

Replace the single-case switch in `Claimer.renew` with a multi-arm
classifier:

- `jetstream.ErrKeyExists` → `ErrClaimLost` (existing; CAS conflict)
- `jetstream.ErrNoStreamResponse` → `ErrClaimLost` (the empirical
  bucket-loss surface for `Update`)
- `jetstream.ErrBucketNotFound` → `ErrClaimLost` (defense-in-depth)
- `jetstream.ErrStreamNotFound` → `ErrClaimLost` (defense-in-depth)
- everything else → existing generic wrap

The downstream `claimLostShutdown` → `OnError` → readiness flip →
pod rotation path is unchanged.

## Design

```go
// Inside Claimer.renew, replacing the existing two-arm conditional:
newRev, err := c.kv.Update(ctx, key, []byte(value), c.lastRevision.Load())
if err != nil {
    switch {
    case errors.Is(err, jetstream.ErrKeyExists):
        // Revision mismatch: another worker took this ID over (or it
        // expired and was re-created under us). The claim is lost.
        return fmt.Errorf("%w: ID %s", ErrClaimLost, wid)
    case errors.Is(err, jetstream.ErrNoStreamResponse),
        errors.Is(err, jetstream.ErrBucketNotFound),
        errors.Is(err, jetstream.ErrStreamNotFound):
        // Bucket vanished from under us. We cannot renew, so the
        // claim is effectively lost regardless of what's in our
        // in-memory revision cache. See [[project-nats-kv-delete-surface]]
        // for which error each surface actually returns.
        return fmt.Errorf("%w: ID %s (bucket missing): %w", ErrClaimLost, wid, err)
    default:
        return fmt.Errorf("failed to renew ID %s: %w", wid, err)
    }
}
```

## Reproducer test list

- *T1 (must fail on parent — primary).* Unit test in
  `internal/stableid/claimer_test.go`: claim an ID against an embedded
  NATS, then `js.DeleteKeyValue(bucket)`. Call `renew`. Assert
  `errors.Is(err, ErrClaimLost)`. On parent the test fails (generic
  wrapped error containing `"no response from stream"`).
- *T2 (regression-guard for the existing case).* Inject a synthetic
  KV stub whose `Update` returns `jetstream.ErrKeyExists`. Assert
  `errors.Is(err, ErrClaimLost)`. Confirms the pre-existing branch
  still classifies correctly.
- *T3 (negative — non-NotFound errors NOT classified).* Synthetic stub
  returning a generic error (e.g. `errors.New("boom")`). Assert NOT
  `errors.Is(err, ErrClaimLost)` and that the error message wraps
  the original (operators still get the diagnostic).
- *T4 (defense-in-depth ErrBucketNotFound branch).* Synthetic stub
  returning `jetstream.ErrBucketNotFound`. Assert `ErrClaimLost`.
  The empirical path doesn't surface this for Update, but the
  classifier branch must work in case future nats.go versions
  start surfacing it.
- *T5 (defense-in-depth ErrStreamNotFound branch).* Same as T4 with
  `jetstream.ErrStreamNotFound`.
- *T6 (negative — context cancel still generic).* Stub returning
  `context.DeadlineExceeded`. Assert NOT `ErrClaimLost` (timeouts
  must NOT trigger pod rotation; they retry on the next tick).

## Verification gates

- `make lint && make test && make test-race` green.
- New exported symbols: none.
- Confirm `claimLostShutdown` runs on T1 — the existing self-stop
  wiring at `manager_election.go:91-98` must observe the new
  classification. Cover with a small integration test that boots
  a Manager, deletes the stableID bucket, and asserts the manager
  enters its degraded / self-stop path within
  `WorkerIDTTL + safety` of the deletion.

## How this trips readiness

Direct: vanished stableID bucket → `renew` returns `ErrClaimLost`
→ `claimLostShutdown` → existing `OnError` path → readiness flip →
pod rotation → re-claim into the (presumably re-provisioned)
bucket cleanly.

## Out of scope

- Library auto-recreating the stableID bucket (category A; forbidden).
- Preventing the brief duplicate-ID window during a wipe-and-recreate
  — closed by F1 (P1.3, the epoch fence). This PR ensures the worker
  fails *safe* (rotates promptly) but does not coordinate the wipe.
- Touching `Claimer.Claim` (the initial-claim path) — only
  `renew` is in scope per the plan. (Claim has its own handling
  for bucket-missing cases.)

## Dependencies & sequencing

Independent. After P1.1 because P1.1 introduced the
`isBucketUnavailableErr` pattern that the F2 envelope (P2.4) will
later reuse — landing P1.1 first gives the codebase one example of
the classifier pattern before P1.2 introduces a sibling.

## Memory pin (after merge)

[[project-nats-kv-delete-surface]] is the canonical reference for
which nats.go error each KV op returns after bucket delete; cite it
whenever a future change touches another error-classification branch
in the codebase.
