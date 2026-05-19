## Summary

Verdict: READY WITH P1 FIXES.

No P0 correctness redesign issue surfaced in this precision pass. The main risk is stale or under-specified implementation text: the proposal is internally settled on stable-only token minting, a parallel snapshot resolver, and sentinel-based NAKs, but several sections still leave enough ambiguity for an implementer to change the wrong API, mint a token from a different cache revision than the admission decision, or wire inconsistent ack behavior across consumer types.

## Findings by severity

### P0

None.

### P1

1. Token minting can race the admission decision unless the plan says which resolver read is authoritative.

   Plan claim: `Required Semantics / Invariants` says a handler receives the exact owner / epoch / claim revision used for the admission decision, while `API Sketch / Behavior` says `ProcessingGate` continues to use `OwnershipResolver.GetOwner` for normal admission and separately requires `OwnershipSnapshotResolver` for token minting.

   Source evidence: current `OwnershipResolver.GetOwner` returns owner/state/epoch/ok, but no revision (`types/ownership.go:31`, `types/ownership.go:42`). The built-in resolver's `GetOwner` reads the atomic cache pointer in that call (`internal/durable/claim_resolver.go:385`, `internal/durable/claim_resolver.go:386`) and returns the cached owner/state/epoch (`internal/durable/claim_resolver.go:391`, `internal/durable/claim_resolver.go:396`). The same cache pointer is replaced on force refresh (`internal/durable/claim_resolver.go:441`, `internal/durable/claim_resolver.go:447`) and watcher batches (`internal/durable/claim_resolver.go:686`, `internal/durable/claim_resolver.go:732`).

   Precision problem: if implementation admits with `GetOwner(pid)` and then calls `GetOwnership(pid)` for the token, a cache update between the two reads can attach a token that was not the basis for admission. Tighten the plan to require one authoritative snapshot read for both admission and token construction when the snapshot resolver is available, or explicitly require comparing the snapshot back to the `GetOwner` tuple and skipping/NAKing on mismatch.

2. Queue cannot currently use "the gate's delay/jitter" for `ErrFenceStale`.

   Plan claim: `Ack/error semantics - sentinel + wrapper NAK` and `Implementation Plan / Phase 3` require Queue and Dynamic wrappers to translate `ErrFenceStale` into `NakWithDelay` using the processing gate's delay/jitter config.

   Source evidence: Queue has its own `QueueConfig` and no processing gate field (`consumer/queue.go:64`, `consumer/queue.go:108`). The gate delay knobs live on `ProcessingGateConfig` (`consumer/gate_config.go:39`, `consumer/gate_config.go:46`), and Dynamic carries that config (`consumer/dynamic.go:71`, `consumer/dynamic.go:76`). Queue's current auto-disposition path only switches between `msg.Nak()` and `msg.Ack()` when `ManualAck` is false (`consumer/queue.go:469`, `consumer/queue.go:475`).

   Precision problem: the Queue part of Phase 3 has no defined delay source. Specify whether Queue should use immediate `msg.Nak()`, a new Queue-level delay config, a package default, or whether Queue is out of scope for delayed sentinel handling.

3. Static and Broadcast sentinel scope is not explicit.

   Plan claim: `Implementation Plan / Phase 3` names only Queue and Dynamic, while `Ack/error semantics - sentinel + wrapper NAK` describes handler behavior in general terms and says the contract is uniform regardless of `ManualAck`.

   Source evidence: Static is a public consumer that delegates to `ipartition.NewJSConsumer` (`consumer/static.go:109`, `consumer/static.go:208`). The static sequential path uses recovery dispatch (`internal/ipartition/consumer.go:346`, `internal/ipartition/consumer.go:347`), and static key dispatch has its own error-to-`Nak` / nil-to-`Ack` logic (`internal/ipartition/key_dispatcher.go:252`, `internal/ipartition/key_dispatcher.go:255`). Broadcast also dispatches handler results through recovery dispatch (`internal/durable/broadcast_consumer.go:392`, `internal/durable/broadcast_consumer.go:398`). Recovery dispatch ignores handler errors in `ManualAck=true`, and in auto-ack mode maps any non-nil error to immediate `msg.Nak()` (`internal/recovery/controller.go:185`, `internal/recovery/controller.go:193`).

   Precision problem: an implementer can reasonably either update only Queue/Dynamic, or update every public consumer that can observe `consumer.ErrFenceStale`. The plan should explicitly say Static and Broadcast remain existing generic error behavior, or add them to Phase 3 and its tests.

4. `ErrFenceMissing` disposition is unspecified for `ManualAck=true` and gate-disabled strict handlers.

   Plan claim: `Required Semantics / Invariants` defines "missing" to include gate disabled, non-Stable admission, and subject parse failure; `Failure Behavior` says a strict handler returns `ErrFenceMissing` via `RequireFence`; `Ack/error semantics - sentinel + wrapper NAK` defines special wrapper handling only for `ErrFenceStale`.

   Source evidence: shared consumer docs state that with `ManualAck=true`, handlers must explicitly call `Ack`, `Nak`, or `Term`, and returning an error does not trigger an action (`consumer/common.go:35`, `consumer/common.go:38`). Queue's auto-disposition block is skipped entirely when `ManualAck` is true (`consumer/queue.go:469`, `consumer/queue.go:479`). Recovery dispatch also returns immediately after the handler in manual-ack mode (`internal/recovery/controller.go:185`, `internal/recovery/controller.go:188`).

   Precision problem: if a strict fenced handler runs with gate disabled and `ManualAck=true`, `RequireFence` returns `ErrFenceMissing`, but no wrapper disposition is specified. State whether `ErrFenceMissing` intentionally times out under manual ack, should be translated like `ErrFenceStale`, or should be made unreachable by config validation.

5. Strict-mode setup validation is underspecified relative to the current Dynamic call path.

   Plan claim: `Strict-admission mode` says `RequireFenceToken=true` is validated at startup and the gate constructor returns a configuration error if the resolver does not implement `OwnershipSnapshotResolver`; `Implementation Plan / Phase 4` repeats that the gate constructor returns an error.

   Source evidence: `Dynamic.NewDynamic` builds a `durable.WorkerConsumer` before any subject loop exists (`consumer/dynamic.go:233`, `consumer/dynamic.go:272`). `NewWorkerConsumer` only initializes the resolver at construction time (`internal/durable/worker_consumer.go:120`, `internal/durable/worker_consumer.go:123`). The current processing gate constructor is called later inside `addSubjectLoop`, after the per-subject durable has already been created or bound (`internal/durable/worker_consumer.go:376`, `internal/durable/worker_consumer.go:390`).

   Precision problem: if the implementer only puts the snapshot-interface check in `newProcessingGate`, the error occurs during `Dynamic.Update` per subject and after durable setup, not cleanly at public construction/startup. Specify the exact validation site and whether a partially-created per-subject durable is acceptable on this configuration error.

6. The SQL fencing example does not advance the stored fence columns.

   Plan claim: `Handler guidance` presents an illustrative SQL conditional update that filters on `claim_rev <= ?` and `(claim_rev < ? OR owner = ?)`, but the `SET` clause only updates `value` and `updated_at`.

   Precision problem: as written, a copied implementation can accept a newer token without recording the newer `owner` / `claim_rev` in the row, so a later stale token may still satisfy the predicate against the old row metadata. The placeholders are also ambiguous: the two `claim_rev` parameters should be named as token/current values, and the example should show whether the write sets `owner`, `epoch`, and `claim_rev` to the token values. This is documentation pseudocode, not a source-code mismatch, but it is exactly the kind of example that users and implementers copy.

7. The public API cost section still contains stale "struct return" text for `OwnershipResolver`.

   Plan claim: `API Sketch` says the existing `types.OwnershipResolver` is not modified and a parallel `OwnershipSnapshotResolver` is added. `Side Effects And Costs` later says "`OwnershipResolver` changes are technically also public, but moving to a struct return makes future extension non-breaking."

   Source evidence: the current public interface is `GetOwner(partitionID string) (string, HandoffState, int64, bool)` (`types/ownership.go:31`, `types/ownership.go:42`).

   Precision problem: the side-effects paragraph is stale from the earlier "change GetOwner to struct return" design and contradicts the settled parallel-interface plan. Remove or rewrite it so implementers do not touch the existing interface.

### P2

None.

## Verdict

READY WITH P1 FIXES.

Fix the seven P1 precision issues before handing this to implementation. They are text/API-contract problems, not evidence that the core fencing architecture needs to be reopened.
