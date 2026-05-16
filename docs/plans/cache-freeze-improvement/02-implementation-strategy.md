# Implementation Strategy: Partition Assignment Robustness Plan

This is operational guidance for executing
`docs/plans/cache-freeze-improvement/00-original-plan.md`. It covers model
selection, effort levels, phasing, and review gates. The robustness
plan itself remains the authoritative spec for *what* to build; this
document covers *how* to dispatch the work.

## Why phasing matters

The robustness plan touches six packages, ~4000 LOC of production
code, and ~70 tests with subtle distributed-systems semantics
(split-brain, CAS chains, content-addressable identity, capability
gating, mixed-version compatibility). The phases have **very
different cognitive demands** — picking one model/effort for the
whole job either over-pays on the easy parts or under-thinks on the
hard parts.

The phasing below also makes the work reviewable. A single PR for the
whole change would be effectively impossible to audit; phase
boundaries are designed so each phase ships independently and the
plan's invariants hold incrementally.

## Phase-by-phase dispatch

| Phase | Scope | Files | Approx LOC | Model | Effort | Why |
|---|---|---|---:|---|---|---|
| **1. Source-layer** | CAS-fenced `Update`, `Modify`/`AddPartitions`/`RemovePartitions`, content-addressable hash helper, watcher reconcile, delete fan-out, source validation/dedupe, `RevisionedPartitionSource` optional interface returning `(partitions, revision, known, err)`, **delete-event revision preservation** (`known=true` even when partition list is empty due to delete/purge — review P1 #3), **`Partition.CanonicalID`** with length-prefixed encoding (review P1 #6; no `Partition.Validate` expansion needed per review P2 #6 — length-prefixed parser handles any chars in keys) | `source/nats_kv.go`, `source/nats_kv_test.go`, `source/static.go`, `types/partition.go` | ~600 + 20–25 tests | **Sonnet 4.6** | default | Mechanical against a tight spec. CAS-retry is well-trodden; API shape is settled. Save budget for harder phases. |
| **2. Types + heartbeat publisher** | `types/heartbeat.go`, `Capabilities` constants (`CapAckV1`/`CapTwoPhaseHandoff`/`CapProcessingGate`), `SetAppliedAssignment` / `PublishNow` APIs on `internal/heartbeat/publisher.go`, monotone-update invariant, `appliedSnapshot` carries `AppliedSourceRevKnown`, **`Manager.SetCapability(cap, active)` runtime-reporting API** that the consumer/updater and two-phase coordinator call at wire-up (review P1 #5) | `types/heartbeat.go` (new), `internal/heartbeat/publisher.go`, `manager.go` (SetCapability + Capabilities accessor), related tests | ~300 + 5 tests | **Sonnet 4.6** | default | Specified concretely in §4.1. Type plumbing + thread-safe state mutation. |
| **3. Publisher rewrite** ⚠️ | Refs-always commit, three-key model with **underscore-prefixed protocol keys** (`assignment._commit` / `assignment._commit_log.<V>` / `assignment._payload.<hex(sha256)>` — review P2 #7), `kv.Create` + verify-on-`ErrKeyExists`, CAS commit with leadership recheck, commit_log writes, content-addressable payload reuse, GC loop with retention, **heartbeat-aware legacy alias pre-commit barrier** (publish step 6 of §3.5 — reads heartbeats to classify legacy-in-batch workers, retries alias writes for them, aborts batch on exhaustion — review P0 #2; flanked by leadership rechecks at steps 5 and 7), `DiscoverHighestVersion` + `cleanupStaleAssignments` must filter `assignment._` prefix | `internal/assignment/assignment_publisher.go` (rewrite), `internal/assignment/commit_gc.go` (new), 18 tests | ~800 + 18 tests | **Opus 4.7** | **xhigh** | Highest-stakes phase. Split-brain semantics, sha256-vs-xxh3 separation, `ErrKeyExists` race window with verify-back, leadership re-check timing, GC against concurrent commit. Opus xhigh is the right tool. |
| **4. Calculator + worker state machine** ⚠️ | Commit-driven worker watcher (replaces per-worker key watcher), **dual-read source-of-truth selection rule** (§3.6: commit vs. legacy alias by LeaderRevision comparison — review P0 #1), `handleCommit` state machine with all transitions including legacy-compat path, audit loop with capability gating (`CapAckV1 | CapTwoPhaseHandoff | CapProcessingGate`), **stricter `srcRevMatch`** (known commit revision requires known worker revision — review P1 #4), source-revision-vs-current semantics, two-phase escalation logic, retry-pressure-only path | `internal/assignment/calculator.go`, `manager_assignment.go`, 12 tests | ~600 + 12 tests | **Opus 4.7** | **xhigh** | Audit logic has subtle cases (malformed commit, source advanced past commit, cap-missing-on-target vs. cap-missing-on-behind, `SourceRevisionKnown` flag interaction). Direct-mode vs. two-phase-mode behaviour must be exact. |
| **5. Manager wiring** | `applyAssignment` apply-then-store-then-ack path, unified update-time + initial-assignment paths, watcher reconcile + restart, leader fencing on legacy alias, state machine transitions through `StateStable` only after ack publishes | `manager_assignment.go`, `manager_handoff.go`, `manager.go` state machine, 6 tests | ~400 + 6 tests | **Opus 4.7** | high | Cross-cutting refactor of state-machine transitions and the init path. Opus needed for state machine reasoning, but xhigh is overkill once phase 4 audit semantics are locked. |
| **6. Tests: split-brain + audit + processing-gate** | The 8 F1 architect tests (`LosingLeaderPayloadWriteCannotCorruptWinningCommit`, `CommitRefPayloadMissing_ClassifiesMalformed`, `CommitRefDigestMismatch_RejectsPayload`, `RemovedFromCommit_AppliesEmptyAssignmentAndAcks`, `PayloadGC_DoesNotDeleteCurrentCommitPayloads`, `ErrKeyExists_VerifiedAndReused`, `ErrKeyExists_HashMismatchSurfacesCollisionError`, `CrossCommitReuse_PayloadUnchanged`) + capability-gating tests (`CapMissing_SkipsEscalation` × 3 paths) + end-to-end invariant tests + the rolling-upgrade/dual-read/CapWiring/CanonicalID/protocol-key-filter tests (#47-54 in the current plan) + heartbeat dual-decoder + alias-barrier-hardening tests (#55-61) + source API surface tests (#62-67) + end-to-end invariant tests (#68-70) | `internal/assignment/*_test.go`, `test/` integration harness | ~1500 test LOC | **Opus 4.7** | high | These tests *encode* the invariants. Setup for `LosingLeaderPayloadWriteCannotCorruptWinningCommit` and `ExtendedGrace_FullChain_EscalatesViaClaims` (reading processing-gate state directly to verify the stuck worker stopped consuming) requires careful test architecture. xhigh isn't worth extra cost if phases 3–4 went cleanly. |
| **7. Docs + cleanup** | `docs/API_REFERENCE.md` updates for `Modify` / `AddPartitions` / `RemovePartitions` / `Capabilities`, godoc on new public types, `CHANGELOG.md` entry, sweep `internal/` comments for stale references to the old per-worker key model | `docs/`, all touched files (godoc), `CHANGELOG.md` | small | **Sonnet 4.6** | default | Documentation work. Cheap path. |

⚠️ = correctness-critical phases. Plan boundary should be locked
before starting; don't ask the implementing agent to also do design.

## Phase ordering and dependencies

```
Phase 1 (source)  ──┐
                    ├──> Phase 3 (publisher) ──> Phase 4 (calc+SM) ──> Phase 5 (manager) ──> Phase 6 (tests) ──> Phase 7 (docs)
Phase 2 (heartbeat) ┘
```

- Phases 1 and 2 are independent; can be done in parallel by separate
  worktrees if convenient.
- Phase 3 requires phase 1's `RevisionedPartitionSource` and phase 2's
  `Capabilities` constants in `types/`.
- Phase 4 requires phase 3's commit schema and phase 2's heartbeat
  publisher APIs.
- Phase 5 requires phase 4's audit and the commit-driven watcher
  contracts.
- Phase 6 cuts across phases 3-5 and can begin in parallel once their
  contracts are locked (i.e. after their respective phase has merged
  to a feature branch).
- Phase 7 is last; clean-up after the dust settles.

Do **not** try to ship this as a single PR. The merge-conflict
surface is too large and the review burden is unbounded.

## Cross-cutting practices

### Before each phase
- **Translate spec section to implementation plan.** Spawn the
  `Plan` agent (or do this manually with Opus xhigh) on the
  relevant section of the robustness plan. Output: a step-by-step
  list of file paths, function signatures, line numbers in existing
  code to modify, and exact test names. Don't ask the implementing
  agent to also be the architect.
- **Identify the invariants the phase must preserve.** Write them
  down. They become the test plan input and the review checklist.

### During implementation
- **Worktree isolation for phases 3-5.** Each is large enough that
  you'll want the ability to throw the branch away if a design issue
  emerges mid-implementation. Use the `Agent` tool with
  `isolation: "worktree"`.
- **Fast mode off for phases 3-5.** Fast mode trades reasoning time
  for output speed. For correctness-critical code that's hard to
  roll back, you want the model to think before writing.
- **Short feedback loops on tests.** Run the touched-package tests
  after every meaningful change. Don't batch.

### Between phases
- **Code-review pass with project skills.** Run `/qa-review` and
  `/go-api-review` against the touched packages before merging the
  phase. Catch invariant violations close to introduction, not in
  the integration phase.
- **Update the metric inventory.** Each phase introduces new
  metrics (see `parti.assignment.*`, `parti.audit.*`,
  `parti.worker.*`, `parti.gc.*`, `parti.publisher.*` in the plan).
  Keep a running checklist; missing metrics catch the eye in code
  review more than in unit tests.

### Before merge of each phase
- **`/ultrareview` pass.** Cheapest insurance against subtle
  distributed-systems mistakes. Worth the cost for phases 3-5;
  optional but recommended for phases 1-2 and 6.
- **Verify invariants list from "Before each phase" is preserved.**

## If you must pick one model/effort for the whole job

**Opus 4.7 at high effort, with one xhigh review pass at the end
before merge.** You'll over-pay on phases 1-2 and 7, but you won't
ship a correctness bug in phases 3-5. The cost of a P0 regression in
production exceeds the cost of running Opus on Sonnet-friendly
phases.

Do **not** pick Sonnet for the whole job. Phases 3-5 have race-
condition and CAS-interaction reasoning that Sonnet will get wrong
in non-obvious ways — wrong in ways the tests in phase 6 may not
catch unless the tests themselves were written by Opus.

Do **not** pick Opus xhigh for the whole job either. Over-thinking
the heartbeat type definitions and the source-validation loop is
just expensive.

## What an agent dispatch looks like (worked example)

For **Phase 3** (publisher rewrite), the dispatch pattern is:

```
1. Plan agent (Opus xhigh, no isolation):
   Input: robustness plan §3.1-3.10, current
          internal/assignment/assignment_publisher.go,
          internal/assignment/calculator.go around lines 200-260
          (where publisher is consumed).
   Output: implementation plan with file paths, function signatures,
           test names, ordering of changes.

2. Implementation agent (Opus 4.7 xhigh, worktree isolation):
   Input: the plan from step 1.
   Output: working code in worktree branch.

3. /qa-review skill on worktree branch.
4. /go-api-review skill on worktree branch.
5. /ultrareview before merging.
6. Merge.
```

For **Phase 1** (source-layer), the pattern simplifies:

```
1. Implementation agent (Sonnet 4.6 default, optional worktree):
   Input: robustness plan §1.1-1.4 (Pillar 1) and §2.1-2.5 (Pillar 2)
          and the "API surface summary" section (for the
          RevisionedPartitionSource optional-extension interface
          signature) and §4.6 (validation/dedupe).
   Output: working code.

2. /qa-review skill.
3. Merge.
```

## Estimated calendar / cost shape

This is a multi-week effort. Rough sizing assuming an experienced
operator dispatching the agents:

- Phase 1: 1-2 days (Sonnet, straightforward).
- Phase 2: 1 day (Sonnet, small surface).
- Phase 3: 3-5 days (Opus xhigh, correctness-heavy).
- Phase 4: 3-5 days (Opus xhigh, state-machine-heavy).
- Phase 5: 2-3 days (Opus high, wiring).
- Phase 6: 4-6 days (Opus high, test architecture).
- Phase 7: 1 day (Sonnet, polish).

Total: ~3 weeks of operator time, assuming each phase merges before
the next starts. Parallel phases 1+2 and partial overlap of phase 6
with 4-5 can shave a week.

Token cost shape: phases 3-4-5 dominate by ~5×. The "single setting"
fallback of Opus everywhere costs roughly 1.4-1.8× the phased
approach. Worth it only if operator coordination time is the
binding constraint.
