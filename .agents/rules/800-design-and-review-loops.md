# 800 - Design Discipline and Review Loops

Apply when drafting a non-trivial fix or design, revising a plan after review, or dispatching a reviewer
(`/plan-review`, `/final-plan-review`, `/post-impl-review`, or `codex:codex-rescue`).

This rule captures reusable discipline from prior multi-round review loops.
Treat the concrete examples as reminders of failure modes, not as assumptions about the current codebase.

## Quick Checklist

- [ ] Invariant stated explicitly in one sentence.
- [ ] Every "X is the only caller / path" claim verified by grep.
- [ ] Every code path that observes or mutates the relevant state enumerated.
- [ ] Atomicity primitive named (`sync.Mutex`, CAS, transaction, etc.) where two operations must coordinate.
- [ ] Tests can be written against current source; missing seams or mocks are listed as prerequisites.
- [ ] Tightly-coupled deferred issues either pulled in-scope or justified.
- [ ] Test list numbering consistent across change log, files-changed table, and test section.

## Before Drafting

For architectural changes, create or update the relevant implementation plan
before coding and wait for approval.

### 1. State the invariant, not the symptom
The bug is the shadow of a broken invariant. Name the invariant explicitly *before* sketching a fix.

- Symptom framing: "This path produces the wrong result after a second event."
- Invariant framing: "This state must always mean X, regardless of which path last observed or updated it."

The invariant tells you which code paths must maintain it. The symptom only tells you one path that breaks it.

### 2. Grep, don't claim
Every prose invariant like *"X is the only caller of Y"* or *"all updates to Z go through W"* is a grep claim. **Run the grep before writing it.** Reviewers will check; if your claim is wrong, the plan is rejected and the round wasted.

Claims that require source evidence:
- "All updates to this field go through this function."
- "This function has N callers."
- "This path is unreachable."
- "Only tests can exercise this branch."

Always grep both production code and tests unless the scope explicitly excludes one
of them.

### 3. Enumerate paths, then design
Before designing the mechanism:
1. List every code path that observes the live state.
2. List every code path that mutates it.
3. Identify atomicity needs (decide-and-commit, CAS, transaction).
4. Then pick the minimum mechanism that maintains the invariant across all of them.

Designing the mechanism first and then trying to plug paths into it produces patches-on-patches.

## During Design

### 4. Atomicity is designed in, not bolted on
If two operations must be atomic (for example, observe + commit, state-check +
transition, decide + publish), pick the primitive *first*: `sync.Mutex`,
`atomic.CompareAndSwap`, a transaction, or an existing codebase-specific
coordination primitive. Don't write code, find a race, and patch.

Concrete signals you're about to bolt on safety:
- Adding a "snapshot timestamp" to make a non-atomic clear "safe."
- Adding a state pre-check before an operation that already has its own check.
- Wrapping an existing call in a new mutex without changing the scope of the operation.

### 5. Tightly-coupled issues are not deferrable
If a deferred issue shares a field, a lock, or a code path with the in-scope fix, it's not actually deferrable — it will be brought back by a reviewer or a scope change.

Surface the entanglement up front. Either:
- Include it in scope, or
- Show why the shared resource has clean lifecycle separation that prevents cross-effects.

### 6. Test plans must compile against current source
When the design adds a mechanism, the reproducer test that would fail without it must be writable against the *existing* code. If you need a clock seam, a mock, or a fake that doesn't exist yet, that prerequisite is part of the plan — not an afterthought.

## During Review Loops

### 7. "Approve with changes" means the changes are required
It is not "almost approved." Every listed change is a required edit. Treat it the same as a reject for the affected items.

### 8. Patching past 2-3 rounds means the design is wrong
If each new patch introduces a new failure mode, stop patching. Reset the design space — go back to step 1 (state the invariant, enumerate paths). A larger refactor that closes multiple coupled defects at once is cheaper than four more single-issue patches.

Signals that a reset is cheaper than another patch:
- The fix adds a second coordination mechanism for the same state.
- The new test needs increasingly precise timing or ordering to reproduce the bug.
- The latest review finding is about an interaction between two earlier fixes.
- The plan's "out of scope" section now shares the same field, lock, or lifecycle as the in-scope fix.

### 9. Scope can shift; design must follow
When the user expands a goal mid-loop (e.g. "robust" instead of "fix FP-1"), previously deferred issues can become in-scope. Re-examine every "out of scope" item against the new goal before continuing.

### 10. The reviewer sees what you wrote, not what you meant
If a reviewer "refutes" a claim, the issue is usually that the plan text was ambiguous or stronger than the code supported. Tighten the language; cite file:line for every load-bearing claim.

## Examples Appendix

These are examples only. Do not assume the named fields, functions, or versions
exist in the current task.

- Symptom framing: "Stale firstSeen voids grace on second crash."
- Invariant framing: "firstSeen[A] = moment since A was last observed alive in any leader scan."
- False caller claim: "All `lastWorkers` updates go through `pollForChanges`." Production paths also updated it through audit repair, scaling timers, manual triggers, and partition lifecycle.
- Patch spiral: v3 -> v4 -> v5 -> v6 each added one mechanism (`pollMu` -> state pre-check -> pending-suppression -> CAS). Each introduced a new race. The stricter source-of-truth CAS redesign in v7 was larger but closed multiple races at once.

## Cross-References

- Process skills: [AGENTS.md - Skills](../../AGENTS.md#skills) - `/plan-review`, `/final-plan-review`, `/post-impl-review`.
