# Tier 1 S3 — Discrimination Verdict

- **Date:** 2026-05-29
- **Status:** FINDINGS — verified empirically (tests re-run under `-race`) **and**
  in source (mechanism traced to specific lines).
- **Scope:** the S3 question from `02-consolidated-design.md` §4/§7 — *does the
  manager's apply-retry self-heal a mid-apply handoff KV-write failure, or is a
  restart required?* — measured as a **discriminator**, not just a stall repro.

## What was run
A standalone black-box harness at `tmp/parti-repro/` (own `go.mod`, `replace …
=> local HEAD; gitignored — see "Harness disposition" below). It drives the
**public** API only: a `jetstream.JetStream`-interface wrapper (per `03` §4)
faults **only the handoff bucket's writes** (`Create`/`Update`/`Put` →
`context.DeadlineExceeded`) via deterministic call-counting, leaving reads and
`Watch` **live**, and is handed to both `parti.NewManager` and
`consumer.NewDynamic`. Single embedded NATS (`partitest.StartEmbeddedNATS`) —
symptom injection, not real quorum loss (that is Tier 2). Two measured layers:
**(a)** does the claim reappear in the handoff KV (read directly, unwrapped);
**(b)** does the consumer resume pulling (publish → handler receipt).

## Verdict table (reproduced under `-race`)
| Scenario | (a) KV claim reappears | (b) consumer resumes | Degraded? |
|---|---|---|---|
| 0 — baseline (faults off) | yes | yes | no |
| A — steady-state rebalance apply | **yes** | **yes** | no |
| B — startup apply, short outage (~0.5s) | yes | yes | no |
| B — startup apply, long outage (~5s) | **no** | **no** | **no** |
| B — long outage, after fresh restart | yes | yes | n/a |

Negative control held: publishing while faulted was **not** consumed within the
bound, so `(b)=true` carries information (not a fail-open probe). The baseline
reaching Stable + pulling through the wrapped `js` also proves **no parti code
type-asserts `jetstream.JetStream` to a concrete type** — the seam is sound.

## Discrimination conclusion
1. **Defect 3 self-heals in the running-fleet (rebalance) shape — FALSIFIED as
   the incident cause for a running fleet.** Scenario A heals end-to-end: a
   rebalance apply runs with `prev = CurrentAssignment()` = the live set, so the
   prepare diff is non-empty and `scheduleApplyRetry` re-attempts the missing
   write until it lands.
2. **A clean S3 (writes-faulted, reads-live) does NOT produce the tombstone
   signature.** The killer "(a) heals / (b) stays suppressed" outcome — Defect 2
   — did **not** appear, precisely because reads stayed live (no
   `Keys`-ok/`Get`-fail window). So for a *running* fleet, the restart-only
   incident still points to **Defect 2 / S2**, which needs the read-fault
   asymmetry — a different scenario and the open Tier 2 question.
3. **NEW: a startup-timed KV-write fault is a THIRD restart-only path** (Scenario
   B, long outage) — distinct from both Defect 3's self-heal and Defect 2's
   tombstone. The claim is simply **absent** in KV (not tombstoned), the manager
   reaches Stable with claims absent, never enters Degraded, and only a restart
   fixes it (demonstrated, not inferred: the RESTART-FIXES-IT step writes the
   claims and the consumer resumes).

## The new mechanism — source-verified **on HEAD** (v2.5.0 applicability UNCONFIRMED)

> **Version note (Codex flagged the apply-path refactor; now traced and RESOLVED).**
> The apply path *did* change between v2.5.0 and HEAD (`manager_assignment.go`
> +337/−15, `manager_election.go` +26, `twophase.go` +3/−3) — so `02` §3's old
> "unchanged in the relevant functions" was wrong. **But the empty-diff mechanism
> holds on v2.5.0 too**, verified by source trace: v2.5.0's retry path
> `scheduleApplyRetry → applyAssignment(*pending) → applyAssignmentWithPrev(m.CurrentAssignment(), *pending)`
> (v2.5.0 `manager_assignment.go:868`) passes that `prev` straight to
> `handoffCoordinator.Apply(..., oldAssignment, ...)` (`:931`) — the **same
> `m.CurrentAssignment()` prev-source** as HEAD's `applyAssignmentWithPrevSkipJitter`;
> the pre-advance (`waitForAssignment` store, v2.5.0 `manager_election.go:428`) and
> the twophase empty-diff early-return (v2.5.0 `twophase.go:230`) are both present.
> The HEAD refactor renamed the retry call and added jitter-skip; it did **not**
> change the prev-source or the mechanism. So this startup restart-only path **applies
> to the v2.5.0 incident build** (source-verified; empirically reproduced on HEAD —
> not separately re-run against a v2.5.0 binary, but the path is structurally
> identical). Defects 1 and 2 are unaffected (byte-identical files).

The initial startup apply **does** schedule a retry (so `02` §2's "(a) maybe no
retry is scheduled" hypothesis is **disproven**), but the retry is **neutered by
an empty prepare diff**, which is a more precise mechanism than `02` §2's "(c)
version-gate dropped the stash":
1. `waitForAssignment` stores the leader's full assignment **straight into the
   snapshot with no claim writes and no apply hook** — `m.assignment.Store(*curAssignment)`
   (`manager_election.go:454`). Empirically: the snapshot jumps to
   `version=1/parts=2` at ~676ms while faults are still firing and the hook trace
   stays flat (no `asgChanged`).
2. The one-shot initial apply (which uses an explicit empty `prev`, so it *would*
   write the full set) fails under the write-fault and stashes a retry.
3. `scheduleApplyRetry` reads `prev := m.CurrentAssignment()`
   (`manager_assignment.go:1434`) — now the **full set** (from step 1).
4. Two-phase prepare computes `toPrepare` = partitions in `next` not in `prev`;
   full-set vs full-set → **empty** → `return nil` (`twophase.go:228-231`):
   trivial "success" writing **zero** claims → the retry self-exits. Empirically:
   faults freeze at 7 from ~1076ms through the entire armed window — the retry
   stopped attempting writes.
5. Claims stay absent until a fresh process (whose `waitForAssignment` re-runs
   against KV that now has the leader's assignment but the worker re-applies from
   an empty `prev`) writes them.

**Defect 1 confirmed exactly as described:** mid-apply `context.DeadlineExceeded`
never trips `recordKVError`, so the manager never enters Degraded in any scenario.

## Corrections to `02-consolidated-design.md` §2
- Defect 3's blanket "`scheduleApplyRetry` … self-heals once KV recovers" is
  **true for the rebalance shape but FALSE for the initial-startup window.**
- The §2 "to-be-checked" startup hypothesis is now **checked**: the initial apply
  *does* schedule a retry; the failure is the empty-prepare-diff self-exit above,
  not a missing retry and not the version-gate stash drop.

## Incident attribution (current best reading)
The reported incident was a **running** fleet → shape A → self-heals → so the
restart-only behavior points to **Defect 2 / S2** (the resolver tombstone, which
needs a read-fault asymmetry this clean write-only S3 deliberately excluded). The
new Scenario-B finding adds a **second live hypothesis**: a worker that was
(re)starting *during* the outage could be stuck restart-only via the empty-diff
path. Both are consistent with the logs; discriminating them further needs the
read-fault scenario and Tier 2.

## Remains open
- **Tier 2:** does *real* bucket quorum loss produce the `Keys`-ok/`Get`-fail
  read asymmetry S2/Defect 2 depend on? (Single-node injection cannot answer this.)
- **Read-fault S2 at the consumer level** (Tier 1 variant): fault handoff *reads*
  while live, to reproduce the (a)-heals/(b)-suppressed tombstone end-to-end
  through the public API (Tier 0 already pins the resolver-unit mechanism).
- **Short-outage heal is timing-fragile** (recovery landing inside the ~0.5–0.7s
  initial-apply window) — reported honestly, not as "short outages self-heal."
- **Startup-timeout watchdog path** (fault outliving `StartupTimeout` → Degraded
  via `startup-timeout`, distinct from Defect 1) was not exercised — restore
  happens before the 60s timeout to keep attribution clean.

## Harness disposition
The harness lives in **gitignored** `tmp/parti-repro/` (per the design — a
throwaway black-box). It is therefore **not version-controlled**; only this
verdict is. Per `02` §5's promotion note, the durable home once a fix is green is
`test/integration/failure/` (+ a `partitest` N-node helper for Tier 2). If the
harness should be preserved before then, it must be force-added or relocated to a
tracked path.
