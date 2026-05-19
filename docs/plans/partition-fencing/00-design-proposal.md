# Partition Fencing Design Proposal

## Status

**Not started — deferred to the next minor version.**

Design is settled and has cleared two informal review rounds plus one
precision-pass review (`reviews/01-precision-pass-review.md`, verdict
READY WITH P1 FIXES, P0=0, P1=7, P2=0). The seven P1s are text/API-contract
tightenings, not architectural reopenings; they must be folded in before
implementation begins. The architecture itself is locked.

This revision narrows v1 to **token propagation + correct ack/error semantics**
and explicitly defers the validator, test harness, and non-Stable-state token
minting. The lean surface is small enough to land safely; richer helpers can be
added later under real user demand.

See `README.md` for the deferral rationale and the open P1 list.

## Assumptions

- Parti should preserve the current `Dynamic` architecture: leader-calculated
  partition ownership, direct worker consumption, and JetStream-owned delivery /
  ack / redelivery.
- Weighted assignment remains partition-based. A partition must stay the unit of
  affinity, load balancing, and handoff.
- The existing two-phase handoff and `ProcessingGate` are useful, but they do
  not fully prove that old in-flight handler work cannot finish after ownership
  has moved.
- The first version should be opt-in and should not require users to replace
  their storage system, cache, or handler framework.

## Problem

Parti can prevent many new non-owner pulls and handler entries, but it cannot
currently prove that an old owner has stopped producing accepted side effects
after a partition has been reassigned.

Example race:

1. Worker `A` owns partition `P`.
2. `A` pulls message `m1` and enters a slow handler.
3. A rebalance assigns `P` to worker `B`.
4. The handoff claim eventually says `B` owns `P`.
5. `B` starts processing new work for `P`.
6. `A`'s old handler finishes late and writes stale state.

The processing gate helps before a handler starts. It does not automatically
protect every side effect after the handler has already started.

The safety invariant we want is:

> Once partition `P` has moved to a newer ownership epoch, no side effect from an
> older owner / older epoch can be accepted.

## Why This Matters

Partition affinity exists because workers cache partition-local state and often
hold partition-local resources. That improves throughput, but it also makes
handoff correctness more important:

- A stale handler can overwrite newer state after a rebalance.
- Two workers can briefly act on the same partition even when new pulls are
  gated correctly, because one worker may already be inside the handler.
- JetStream ack/redelivery correctness is not enough; application side effects
  may happen before ack and outside JetStream.
- A stronger old-owner drain protocol is possible, but it makes rebalances wait
  on old workers and still needs a timeout story for slow or dead workers.

Fencing gives a cleaner correctness boundary: old work may still finish, but it
cannot commit accepted side effects after ownership has advanced.

### Idempotency Is Necessary But Not Sufficient

Workers should still be responsible for idempotency. Parti uses JetStream's
at-least-once delivery model, so the same message can be delivered more than
once after retries, redelivery, crashes, or consumer recreation. Handler code
must tolerate duplicate input.

Idempotency answers:

> If I process the same input twice, do I avoid duplicating the same effect?

Fencing answers a different question:

> If an old owner finishes stale work after ownership moved, can it overwrite or
> conflict with newer owner work?

Example where idempotency is enough:

1. Message `m1` says "set order 123 status = paid".
2. Worker `A` processes `m1`.
3. `m1` is redelivered.
4. Worker `B` processes `m1`.
5. The same state is written again; no duplicate external effect occurs.

Example where idempotency is not enough:

1. Worker `A` owns partition `P` and starts slow processing for `m1`.
2. Ownership moves to worker `B`.
3. `B` processes newer `m2` and writes state version `20`.
4. `A` finishes old `m1` and writes state version `19`.

`A` did not process `m1` twice, so idempotency did not help. The problem is that
an old owner committed a stale side effect after a newer owner had advanced the
partition. That is the gap fencing closes.

Responsibility split:

- Parti provides at-least-once delivery integration, partition affinity,
  ownership metadata, and fence tokens for handlers that need strict side-effect
  safety.
- Worker/application code makes handlers idempotent and decides which side
  effects need fencing.
- The storage or external side-effect target must provide the atomic compare /
  conditional write needed for strict fencing. **Parti does not provide a
  general validator helper in v1** — atomic CAS at the side-effect target is
  the only mechanism that actually closes the race.

## What

Introduce an explicit partition ownership fence token and expose it to handlers.

The fence token identifies the authority under which a handler is allowed to
produce side effects:

```go
type FenceToken struct {
    PartitionID string
    Owner       string
    Epoch       int64
    ClaimRev    uint64 // JetStream KV revision of the claim at admission time
}
```

The token is derived from the handoff claim:

- `PartitionID` comes from the message subject / assignment partition.
- `Owner` is the worker ID currently allowed to process.
- `Epoch` is the claim epoch observed when the handler is admitted.
- `ClaimRev` is the KV revision of the claim record observed at admission.

### Why ClaimRev is required

`Epoch` alone does not satisfy the monotonicity invariant. The claims KV bucket
has a TTL (`HandoffTTL`, default `2m`); when a claim record expires it is
deleted, and any subsequent prepare for that partition starts from
`NewInitialClaim` with `Epoch: 1`. An old in-flight handler holding
`{owner=A, epoch=1}` could see its token become "valid again" against a freshly
recreated `{owner=A, epoch=1}` claim. That violates invariant 4 below.

`ClaimRev` is the per-key, monotonically increasing JetStream KV revision. A
deleted-then-recreated key gets a fresh, strictly greater revision. Comparing
`(Owner, Epoch, ClaimRev)` — or even just `ClaimRev` — eliminates the reuse
hole.

### Two-layer model

1. **Admission fence:** `ProcessingGate` admits the handler only when the worker
   is the owner in an allowed state (Stable in v1; see below).
2. **Commit fence:** side-effect code uses the token in an atomic conditional
   write against the user's storage system. The commit fence is the real
   correctness boundary; the admission token's job is to give the commit fence
   something to assert against.

### Admission is best-effort, the commit fence is authoritative

The admission token is captured from the resolver cache, which can lag the KV
truth by up to one watcher round-trip (and longer during a watcher freeze — see
the `claim_resolver_watcher_freeze` tests). A token attached at admission may
therefore already describe a no-longer-current claim by the time the handler
runs. This is acceptable: the commit fence rejects such writes at the storage
layer. Documentation should make this explicit so users do not treat token
presence as proof of currency.

## How

### 1. Capture The Fence At Handler Admission

When the processing gate admits a message, it already resolves:

- partition ID
- owner
- handoff state
- claim epoch
- (new) claim revision — must be exposed by the ownership resolver

Extend the wrapper to attach a `FenceToken` to the handler context when all of
these are true:

- ownership is known
- owner equals the local worker ID
- **state is `Stable`** (v1 restriction; see Required Semantics)
- epoch is non-zero
- claim revision is non-zero

The token is immutable once attached to the context.

### 2. Stable-only minting in v1

`processingGate` may be configured with `AllowedStates` broader than
`{Stable}` to keep handlers running during handoff transitions. However,
`NextCommit` and `NextStable` both increment the claim epoch (see
`internal/assignment/handoff/claims.go`). A token captured during `Commit` would
go stale at the very next normal `Commit → Stable` transition with no ownership
change, leading to spurious `ErrFenceStale` and redelivery churn.

For v1, fence tokens are minted **only when admitted in `Stable`**. Handlers
admitted in transitional states do not receive a token; `RequireFence(ctx)`
returns `ErrFenceMissing` for them. Documenting this as a hard rule keeps the
contract clear: *"if you got a token, you were admitted at a quiescent ownership
boundary."* Lifting this restriction can come later if a concrete workload needs
it.

### 3. Strict-admission mode

`ProcessingGate` currently fails open on subject-template parse failure (admits
the handler with no ownership decision). For fenced workloads that is
incorrect: a handler with no token would still run.

Add `ProcessingGateConfig.RequireFenceToken bool`. When true:

- Subject parse failure → NAK with the gate's standard delay (no handler call).
  Metric reason: `fence_required_parse_failure`.
- Admission in a non-token-minting state (anything other than Stable in v1) →
  NAK with the gate's standard delay.
  Metric reason: `fence_required_non_stable`.
- Resolver returns an ownership snapshot without a non-zero `ClaimRev` (e.g.
  the configured resolver does not implement `OwnershipSnapshotResolver`) →
  NAK at admission.
  Metric reason: `fence_required_missing_revision`.

NAK accounting reuses the existing `GateMetrics.IncGateNAK(reason)` interface;
no new metric symbol is added in v1. A dedicated counter can come later if
operators need dashboards that distinguish "ownership NAKs" from "fence
strictness NAKs" at the metric-name level.

Additionally, `RequireFenceToken=true` must be validated at startup: if the
configured `OwnershipResolver` does not also implement
`OwnershipSnapshotResolver` (see API Sketch), the gate constructor returns a
configuration error rather than failing per-message. Fail-at-setup is preferred
over fail-per-message when the misconfiguration is detectable statically.

Handlers should also defensively call `consumer.RequireFence(ctx)` and return
`ErrFenceMissing` if absent. The gate setting is defense in depth; the handler
check is the contract.

### 4. Ack/error semantics — sentinel + wrapper NAK

The handler must NOT call `msg.Nak()` and return `nil`. The default consumer
auto-ack path (`consumer/queue.go` and equivalent for Dynamic) treats `nil` as
success and calls `msg.Ack()`, producing a double / contradictory disposition.

The contract is:

- Handler signals fence failure by returning `consumer.ErrFenceStale` (or any
  error wrapping it).
- The consumer wrapper recognizes the sentinel and translates it to
  `msg.NakWithDelay(...)` using the same delay/jitter machinery the processing
  gate already uses for non-owner NAKs.
- The wrapper takes precedence over the default auto-ack path for this error.
- Other errors continue to use the existing auto-ack-error behavior.

This keeps handler code uniform regardless of `ManualAck` and avoids fighting
the auto-ack path.

### 5. Handler guidance

Handlers that mutate external state should follow this shape:

```go
func Handle(ctx context.Context, msg jetstream.Msg) error {
    token, err := consumer.RequireFence(ctx) // returns ErrFenceMissing if absent
    if err != nil {
        return err
    }

    next := compute(msg)

    // Atomic conditional write against the side-effect target.
    // ErrFenceStale is the sentinel the wrapper will translate to NAK.
    return store.WriteWithFence(ctx, token, next)
}
```

The user-side `WriteWithFence` is the commit fence. It is the user's
responsibility (against their own storage) to make the write atomic on
`(Owner, Epoch, ClaimRev)` — or on whichever subset their backend supports.
Examples (illustrative only; not provided by Parti):

```sql
-- SQL: single-statement conditional update.
UPDATE partition_state
SET value = ?, updated_at = ?
WHERE partition_id = ?
  AND claim_rev <= ?         -- monotonic; reject older or equal-from-stale-owner
  AND (claim_rev < ? OR owner = ?);
```

```go
// JetStream KV CAS: read with revision, write with Update(rev).
cur, err := kv.Get(ctx, key)
if err != nil { return err }
if cur.ClaimRev() != token.ClaimRev { return consumer.ErrFenceStale }
_, err = kv.Update(ctx, key, encode(next), cur.Revision())
```

```text
// Cache: version the key with the token, write unconditionally.
cache:{partition}:{claim_rev}
```

Important: a stale fence must not be acknowledged as successful work. The
sentinel/NAK path is the default and only supported behavior.

### 6. Keep Two-Phase Handoff As An Admission Optimizer

Do not make fencing depend on a stronger old-owner drain barrier in v1.

The existing pieces should be interpreted as:

- two-phase handoff: coordinates claim state transitions
- pull gating: avoids unnecessary pulls for non-owners
- processing gate: avoids starting new handler work under stale ownership
- fence token: lets the side-effect target reject stale in-flight commits

This is more robust than relying on drain alone, because it handles slow,
blocked, or wedged handlers that started before ownership moved.

## Required Semantics

### Invariants

1. A handler admitted by the processing gate receives the exact
   owner / epoch / claim revision used for the admission decision.
2. A token for revision `R` becomes stale once the claim for that partition
   advances to any different revision (including delete-then-recreate, which
   produces a strictly greater revision).
3. A stale token must be distinguishable from a missing token:
   - **stale** means "ownership/revision has advanced"
   - **missing** means "no token was attached" (gate disabled, non-Stable
     admission, subject parse failure)
4. Fencing must be monotonic: a stale token cannot become valid again. Using
   `ClaimRev` makes this provable; epoch alone does not.
5. The token must not depend on wall-clock time.

### Failure Behavior

- Owner changed: side-effect target rejects → handler returns `ErrFenceStale`
  → wrapper NAKs.
- Epoch changed with same owner: same handling.
- Claim revision changed (covers post-TTL recreate): same handling.
- Missing token in a strict handler: handler returns `ErrFenceMissing` via
  `RequireFence`. With `RequireFenceToken=true`, the gate also NAKs at admission.
- Resolver cache stale / NATS unavailable: admission token may be absent or
  describe a slightly stale claim. The commit-fence atomic write catches the
  divergence. Parti does not ship a separate "validate fence" helper in v1
  (see Deferred Work).

## API Sketch

Package placement is no longer an open question. The core type lives in
`types/` so that `internal/durable` can construct it without importing
`consumer/` (which would be an import cycle — `consumer` already imports
`internal/durable`). Handler-facing helpers live in `consumer/` as aliases /
thin wrappers.

```go
// types/fence.go
package types

type FenceToken struct {
    PartitionID string
    Owner       string
    Epoch       int64
    ClaimRev    uint64
}
```

```go
// consumer/fence.go
package consumer

import "github.com/arloliu/parti/v2/types"

type FenceToken = types.FenceToken

var (
    ErrFenceMissing = errors.New("partition fence token missing")
    ErrFenceStale   = errors.New("partition fence token stale")
)

// FenceTokenFromContext returns the token attached by the processing gate.
// Returns ok=false if no token is present.
func FenceTokenFromContext(ctx context.Context) (FenceToken, bool)

// RequireFence returns ErrFenceMissing when no token is attached.
// Use this in handlers that must always run under a fence.
func RequireFence(ctx context.Context) (FenceToken, error)
```

The existing `types.OwnershipResolver` interface is **not modified**. Adding a
method or changing a return signature would break every external implementation.
Instead, a parallel snapshot interface is added alongside it:

```go
// types/ownership.go (additions; existing OwnershipResolver unchanged)

type OwnershipSnapshot struct {
    PartitionID string
    Owner       string
    State       HandoffState
    Epoch       int64
    ClaimRev    uint64
}

// OwnershipSnapshotResolver is an optional capability that ownership
// resolvers may implement to support partition fencing. The built-in
// claim-based resolver implements both this interface and OwnershipResolver.
type OwnershipSnapshotResolver interface {
    GetOwnership(partitionID string) (OwnershipSnapshot, bool)
    ForceRefreshPartition(ctx context.Context, partitionID string) error
}
```

Behavior:

- The built-in `internal/durable.ClaimBasedResolver` implements both
  `OwnershipResolver` and `OwnershipSnapshotResolver`.
- `ProcessingGate` continues to use `OwnershipResolver.GetOwner` for normal
  admission decisions — no behavior change for non-fenced configurations.
- Fence-token minting requires the resolver to also implement
  `OwnershipSnapshotResolver`. The gate type-asserts once at construction;
  if `RequireFenceToken=true` and the assertion fails, construction returns
  an error. If `RequireFenceToken=false` and the assertion fails, the gate
  simply never mints tokens (handlers see `RequireFence(ctx) == ErrFenceMissing`).
- `OwnershipResolver` is **not** deprecated. A future API break (v3 or
  later) can revisit the question; for v2 the two interfaces coexist.

### What is intentionally NOT in v1

These were in earlier drafts and have been deferred — see Deferred Work for
rationale:

- `FenceValidator` interface and resolver-backed implementation.
- In-memory `FencedStore` test helper.
- Token minting in non-Stable admission states.
- Any storage adapter / write helper.

## Implementation Plan

### Phase 1: Resolver surface

- Add `types.OwnershipSnapshot` struct and `types.OwnershipSnapshotResolver`
  interface alongside the existing `OwnershipResolver`. Do not modify the
  existing interface.
- Make `internal/durable.ClaimBasedResolver` implement
  `OwnershipSnapshotResolver` (populating `ClaimRev` from
  `KeyValueEntry.Revision()` in the watcher). The existing `GetOwner`
  implementation is untouched, so all current callers — `processingGate`,
  `worker_consumer`, and external implementations of `OwnershipResolver` —
  keep working without changes.
- Add resolver tests covering snapshot revision monotonicity across
  prepare/commit/stable transitions and across delete-then-recreate
  (TTL expiry path).

### Phase 2: Token plumbing

- Add `types.FenceToken` and `consumer.{FenceToken alias,
  FenceTokenFromContext, RequireFence, ErrFenceMissing, ErrFenceStale}`.
- Extend `processingGate.Wrap` to attach the token to the handler context
  when admitted in `Stable` with a non-zero epoch and revision.
- Tests:
  - admitted owner in Stable receives a token with matching owner/epoch/rev
  - admitted owner in any non-Stable state receives no token
  - non-owner receives no handler call (existing behavior)
  - disallowed state receives no handler call (existing behavior)
  - unknown ownership receives no handler call after refresh attempt
  - subject parse failure with `RequireFenceToken=false` behaves as today
    (handler runs, no token)
  - subject parse failure with `RequireFenceToken=true` NAKs

### Phase 3: Wrapper sentinel handling

- Teach the consumer wrappers (Queue, Dynamic) to recognize `ErrFenceStale`
  (via `errors.Is`) and translate it into `NakWithDelay` using the gate's
  delay/jitter config. The sentinel handling takes precedence over default
  auto-ack-on-error.
- Tests for both `ManualAck=false` and `ManualAck=true` paths.

### Phase 4: Strict-admission mode

- Add `ProcessingGateConfig.RequireFenceToken bool`. Wire it into:
  - the subject-parse fail-open path (NAK, reason
    `fence_required_parse_failure`),
  - the non-Stable admission path (NAK, reason `fence_required_non_stable`),
  - the missing-`ClaimRev` path (NAK, reason
    `fence_required_missing_revision`).
- Gate constructor returns an error when `RequireFenceToken=true` and the
  configured resolver does not implement `OwnershipSnapshotResolver`.
- Tests covering each NAK reason and the construction-time error.

### Phase 5: Documentation

- Update consumer docs and Godoc with:
  - when fencing is required (handlers writing partition-keyed state)
  - the admission-token vs commit-fence model and the cache-lag caveat
  - sentinel/NAK contract
  - SQL / JetStream-KV / cache example shapes (illustrative only)
  - why `ProcessingGate` alone is an admission guard, not a side-effect fence
  - explicit note that Parti does not provide a validator helper

## Deferred Work (not v1)

Each item below was considered for v1 and intentionally cut, with rationale.

- **`FenceValidator` interface and resolver-backed implementation.** Check-then-act
  semantics encourage users to substitute the validator for an atomic CAS, which
  reintroduces the race the design is meant to close. Atomic conditional writes
  at the side-effect target are the only mechanism that actually fences, and
  v1 should not ship an API that suggests otherwise. Revisit only with a
  concrete internal need (for example, a Parti-provided JetStream-KV state
  helper).
- **In-memory `FencedStore` test helper.** Useful for downstream users, but it
  is non-trivial public surface to commit to. Defer until at least one real
  user reports needing it.
- **Token minting in non-Stable states.** Requires either a separate
  "ownership-only" revision counter that does not bump on commit/stable
  transitions, or explicit user opt-in with documented churn behavior. Out of
  scope until requested.
- **Optional integration test harness demonstrating the stale-in-flight race.**
  Worth building, but only after Phase 1–4 land; it requires the
  `ClaimRev`-bearing token to exercise the post-TTL-recreate case
  meaningfully.

## Non-Goals

- Do not build a generic database adapter layer.
- Do not require every handler to use fencing.
- Do not make leader dispatch part of this design.
- Do not require a global old-owner drain barrier for v1.
- Do not promise exactly-once processing. This remains at-least-once delivery
  with stale side effects rejected at the side-effect target.

## Side Effects And Costs

- **Public API expansion.** Once exposed, `FenceToken` shape is hard to change.
  The lean v1 minimises this surface: one struct (in `types`), three function
  symbols, two sentinel errors. `OwnershipResolver` changes are technically
  also public, but moving to a struct return makes future extension non-breaking.
- **Redelivery churn during rebalances.** `ErrFenceStale` defaults to NAK, so
  in-flight work that started under an old owner re-enters until ownership and
  delivery align. This is correct for safety; the gate's existing
  `NakDelay`/`NakJitter` controls the churn rate.
- **Operational and conceptual burden on users.** Users must now reason about
  idempotency, fencing, atomic CAS, and where their storage falls on that
  spectrum. The docs explicitly walk through this.
- **Runtime overhead.** Token plumbing itself is one context value plus already-
  read fields; negligible. The cost lives at the user's side-effect target
  (their atomic CAS). Because v1 does not ship a validator, Parti does not add
  per-message resolver/NATS work for the fencing path.
- **False sense of safety risk.** Mitigated by (a) shipping no validator in
  v1, and (b) explicit documentation that admission tokens are best-effort and
  the commit fence is authoritative.

## Open Questions

None remaining for v1 scope. All prior questions have been resolved (see below).
New questions may surface during implementation; record them in the relevant
phase's PR rather than re-opening this design doc.

## Closed Questions (resolved)

- **Package placement:** `types.FenceToken` with `consumer` alias.
- **Token identity:** include `ClaimRev` to satisfy monotonicity under
  TTL/recreate.
- **Ack handling for `ErrFenceStale`:** sentinel + wrapper NAK; handlers never
  call `msg.Nak()` directly.
- **Validator:** not shipped in v1.
- **Test harness:** not shipped in v1.
- **Token minting outside `Stable`:** not shipped in v1.
- **Strict-mode NAK metrics:** reuse `GateMetrics.IncGateNAK(reason)` with new
  reason labels (`fence_required_parse_failure`, `fence_required_non_stable`,
  `fence_required_missing_revision`). No new metric symbol in v1.
- **`RequireFence` return shape:** `(FenceToken, error)`. Keeps both APIs —
  `FenceTokenFromContext(ctx) (FenceToken, bool)` for optional/introspective
  use, `RequireFence(ctx) (FenceToken, error)` for strict handlers. Error form
  composes with `errors.Is(err, consumer.ErrFenceMissing)` and matches the
  fail-loud intent; bool would invite silent fallback.
- **`OwnershipResolver` evolution:** add a parallel
  `types.OwnershipSnapshotResolver` interface; do **not** modify or deprecate
  the existing `OwnershipResolver`. The built-in claim-based resolver
  implements both. Fence-token minting type-asserts to the snapshot interface
  at gate construction. Revisit deprecation only with a future v3/API-break
  plan.

## Recommendation

Implement fencing as a lean, opt-in handler contract layered on top of the
existing `Dynamic` + `ProcessingGate` model.

v1 ships: claim-revision in the resolver, `FenceToken` propagation in `Stable`
admissions, `RequireFence`, `ErrFenceStale` wrapper handling, and a strict
admission mode. Everything else is deferred until real usage justifies the
public-API weight.

Do not try to make two-phase handoff alone prove no stale side effects. Handoff
and gating reduce the overlap window; fencing closes the correctness gap that
remains when old in-flight work finishes late — but only if the commit fence is
an atomic CAS at the user's storage, and only if the token identity is strong
enough to survive claim-record recreation.
