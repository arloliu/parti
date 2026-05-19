# Partition Fencing

Design and implementation plan for partition ownership fence tokens — a
correctness boundary that lets the side-effect target reject stale writes from
old in-flight handlers after a partition has been reassigned.

## Why this exists

`ProcessingGate` admits handlers only when the local worker is the current
owner, but it cannot protect side effects that happen *after* a handler has
already started running. If ownership moves while a slow handler is in flight,
the old handler can finish late and overwrite newer state with stale data.
Idempotency does not close this gap because the old and new handlers are
processing different inputs.

Fencing adds an explicit token to admitted handlers. The token identifies the
ownership authority under which the handler is allowed to commit. The
side-effect target uses the token in an atomic conditional write; stale tokens
fail the write and the handler returns a sentinel that the consumer wrapper
translates into a NAK.

## Status

| Phase | State |
|---|---|
| Initial draft | Done |
| Review round 1 (six structural concerns) | Done — addressed inline |
| Review round 2 (over-engineering / scope) | Done — narrowed v1 surface |
| Open-questions resolution (metrics / API shape / resolver evolution) | Done |
| Precision-pass review | Done — `reviews/01-precision-pass-review.md`, READY WITH P1 FIXES (P0=0, P1=7, P2=0) |
| Fold P1 fixes into proposal | **Pending** |
| Phase 1 — Resolver surface (`OwnershipSnapshotResolver`) | **Not started** |
| Phase 2 — Token plumbing (`FenceToken`, gate attaches in Stable) | **Not started** |
| Phase 3 — Wrapper sentinel handling (`ErrFenceStale` → NAK) | **Not started** |
| Phase 4 — Strict-admission mode (`RequireFenceToken`) | **Not started** |
| Phase 5 — Documentation | **Not started** |

**Deferred to the next minor version.** This work is not part of the current
release cycle. The design is locked and ready to pick up; no further
architectural review is needed before implementation, only the P1 fold-in
below.

## Open P1 findings to fold in before implementation

From `reviews/01-precision-pass-review.md`:

1. **Single authoritative resolver read at admission.** Specify that when the
   snapshot resolver is available, *one* `GetOwnership` read serves as both
   the admission decision and the token source. Forbid the two-read pattern
   that would race against cache updates.
2. **Queue sentinel delay source.** Queue carries no `ProcessingGateConfig`,
   so it has no gate delay/jitter to reuse. Spec should say Queue uses
   immediate `msg.Nak()` for `ErrFenceStale` (Queue isn't partition-aware;
   fence semantics rarely apply).
3. **Static and Broadcast scope.** Explicitly mark Static and Broadcast as
   out-of-scope for v1 sentinel handling; their existing generic
   error-to-NAK behavior already handles `ErrFenceStale` reasonably without
   honoring gate delay/jitter.
4. **`ErrFenceMissing` disposition.** State that `ErrFenceMissing` is not
   wrapper-translated; it surfaces as a normal error (auto-NAK in
   `ManualAck=false`, manual disposition in `ManualAck=true`). Add a
   construction-time validation: `RequireFenceToken=true` with a disabled
   gate must fail at construction.
5. **Where strict-mode validation lives.** The snapshot-interface assertion
   must run at `NewDynamic` / `NewWorkerConsumer`, not inside the per-subject
   `newProcessingGate` call (which would surface the error mid-`Update`
   after a partial durable setup).
6. **SQL example bug.** The `SET` clause in the handler-guidance SQL example
   must also advance `owner`, `epoch`, and `claim_rev`; the current example
   updates only `value` and `updated_at`, so a copied implementation would
   leave stale row metadata that future stale tokens could still match.
7. **Stale "struct return" sentence.** The "Side Effects And Costs" section
   still mentions changing `OwnershipResolver` to a struct return — a
   leftover from the pre-parallel-interface draft. Rewrite to describe the
   parallel-interface non-breaking change.

## Layout

```
docs/plans/partition-fencing/
├── README.md                          # This file: status + open P1 fold-in list
├── 00-design-proposal.md              # Authoritative spec (locked architecture, pending P1 fold-in)
└── reviews/
    └── 01-precision-pass-review.md    # Codex final-plan-review, verdict READY WITH P1 FIXES
```

When implementation starts, add per-phase spec files
(`01-phase1-resolver-surface.md`, etc.) following the convention used by
`docs/plans/assignment-correctness-fixes/`.
