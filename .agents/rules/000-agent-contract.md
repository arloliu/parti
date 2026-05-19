# 000 - Agent Contract

Always apply these rules. They are the operating contract for every task in this
repository.

## Think, Read, and Do Not Guess
- State assumptions explicitly.
- If uncertain, ask rather than guess.
- Present multiple interpretations when ambiguity exists.
- Push back when a simpler approach exists.
- Stop when confused. Name what is unclear.
- Do not guess when source evidence, tests, benchmarks, docs, or grep can answer.
- Do not present unverified assumptions as facts.
- If verification is impossible or too expensive, say what is unverified and why.
- If uncertainty blocks correctness, ask before editing.
- Before adding code, read exports, immediate callers, shared utilities, and relevant rules.
- If you do not know why nearby code is structured a certain way, stop and investigate before editing.
- Do not assume a change is isolated until you have checked the surrounding call paths.

## Keep Changes Small
- Make the minimum change that solves the problem.
- Touch only what you must; clean up only your own changes.
- Do not add speculative features, one-off abstractions, or drive-by refactors.

## Surface Conflicts
- If two patterns contradict, pick one explicitly and explain why.
- Prefer the more recent, more tested, or more local convention.
- Do not blend conflicting patterns into a compromise that matches neither.

## Test Intent and Match the Codebase
- Tests must encode why behavior matters, not just what happens.
- A test that cannot fail when business logic changes is wrong.
- Follow existing conventions even when you disagree.
- If a convention looks harmful, surface it instead of forking silently.

## Fail Loud
- Define success criteria and loop until verified.
- Checkpoint after significant steps: what changed, what is verified, what remains.
- Do not say "completed" or "tests pass" if anything was skipped.
- Default to surfacing uncertainty, not hiding it.
