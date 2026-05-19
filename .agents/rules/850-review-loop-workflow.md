# 850 - Review Loop Workflow

Apply when running a plan-track or implementation-track loop in this repo —
specifically, when deciding which reviewer skill to dispatch, when to stop,
and how to handle findings between rounds. This rule documents the canonical
sequence so it does not have to be re-described every task.

For design discipline that applies inside each round (state the invariant,
grep don't claim, atomicity is designed in), see
[`800-design-and-review-loops.md`](800-design-and-review-loops.md). 800 is the
*how to think during a round*; 850 is *how to sequence the rounds*.

## Canonical Sequence

### Plan track

```
draft plan
  → /plan-review (architectural pass, effort=xhigh)
  → revise plan per findings
  → /plan-review again until no P0/P1 architecture findings
  → /final-plan-review (precision pass, effort=high)
  → revise per findings
  → hand off to implementation
```

### Implementation track

```
implement (or /simplify a draft)
  → /post-impl-review v1 (effort=xhigh)
  → fix findings
  → /post-impl-review v2 (effort=xhigh)
  → fix findings
  → /post-impl-review v3+ (effort=high; step down per skill cost note)
  → merge when verdict=merge and zero P0/P1
```

### Lightweight alternatives

Skip the full structured review when you only want a fresh outside-model pass
over a diff — use `/codex:review --wait` or `/codex:adversarial-review --wait`
directly. These do not write a versioned report; they are not substitutes for
`/post-impl-review` when the spec-vs-impl audit is the point.

### Stopping conditions

- **Plan-track:** stop when `/final-plan-review` returns no P0 and no P1
  findings, or returns a verdict equivalent to "ready to implement."
- **Impl-track:** stop when `/post-impl-review` returns verdict=`merge` with
  zero P0 and zero P1. P2 polish items are not merge-blockers.

## Between-Round Discipline

1. **Read the full report, then triage.** Each P0/P1 gets one of:
   *accept* (apply the fix), *argue back* (the reviewer was wrong — record
   why with `file:line`), or *defer* (legitimate but out of this round's
   scope — file a follow-up). Silent drops are not allowed.
2. **Edits land in the plan or code, not in chat.** The next round's reviewer
   must see the change in the file, not in a conversation summary.
3. **Do not auto-apply reviewer suggestions.** Even when the fix looks
   obvious, the report is *input to your edit*, not a patch. Reviewers are
   wrong often enough that blind application creates new findings.
4. **Argue-back must cite source.** "I disagree, see `file:line` showing X"
   is acceptable; "I think this is fine" is not.
5. **Surface the next dispatch as a cost gate.** Each external reviewer
   round costs real tokens and 2–8 minutes wall time. Propose the next round
   to the user before dispatching; do not chain rounds silently.

## Re-Dispatch Guard

Before dispatching v2+ of any reviewer, confirm material change since the
last report. The reviewer skills enforce this at dispatch time; the rule
here is the *intent*: re-reviewing unchanged input wastes the budget and
typically reproduces the prior verdict. If nothing changed but the loop has
not converged, the next step is a human judgment call — not another dispatch.

## Stage Escalation

- Precision pass surfaces a P0/P1 with architectural shape →
  reopen `/plan-review`. `/final-plan-review` cannot redesign.
- `/post-impl-review` surfaces a finding that requires re-architecture (not
  just code fixes) → stop the impl loop, reopen `/plan-review` on the
  affected pillar, then return to impl.
- More than 2–3 rounds at the same stage with new findings each time →
  the underlying approach is wrong (see
  [`800-design-and-review-loops.md`](800-design-and-review-loops.md) §8,
  "Patching past 2-3 rounds means the design is wrong"). Reset, do not
  patch further.

## Cross-References

- [`800-design-and-review-loops.md`](800-design-and-review-loops.md) —
  design discipline that applies *within* each round.
- [`AGENTS.md` — Skills](../../AGENTS.md#skills) — invocation details for
  `/plan-review`, `/final-plan-review`, `/post-impl-review`.
