# Auto-Healing Quorum-Loss — Fix Plan

- **Date:** 2026-05-30
- **Status:** REVIEW-CLEAN — ready to implement. Cleared the review loop (1 `plan-review`
  xhigh + 4 `final-plan-review` high passes; reports `tmp/00-fix-plan_*_review*.md`); final
  verdict **"ready to implement, 0 findings"** (`tmp/00-fix-plan_precision_pass_review_v4.md`).
  **Implementation not started** — awaiting go-ahead.
- **Builds on:** `docs/plans/auto-healing-quorum-loss-repro/` (00–05). The reproduction +
  attribution is complete; this plan designs the fix.
- **Attribution recap (from `…-repro/05-final-synthesis.md`):** the non-recovery is
  **Defect 2** (irreversible resolver-cache tombstone) — *load-bearing*; **Defect 1**
  (connected-but-KV-timeout is unclassified, so the manager never degrades) — *enabler*;
  **Defect 3-startup** (empty-diff retry self-exit) — *real, v2.5.0-applicable, latent
  co-contributor*. All three pre-date v2.5.0 and are unfixed at HEAD.
- **Attribution confidence:** the v2.10.29 incident-version probe re-run is done — the
  sustained read failure is a *fault-nature* effect (PVC volume-offline / read-only
  storage), not a NATS-version effect, and *which* fault dynamic fired (during-outage
  window / wedged-storage / recovery-edge) is strongly-supported-not-confirmed. **This
  does not affect the fix:** F-D2 trips on ANY `Get` error and is robust to surface
  (`ErrNoResponders`/`DeadlineExceeded`) and timing — so the fix is correct regardless of
  which dynamic fired.

---

## Root causes — why the current codebase can't auto-heal (read first)

**The triggering state the codebase never modeled.** A PVC going offline / a node crashing
on enough replicas to break **one bucket's RAFT quorum** — but not the meta-cluster's —
leaves the client `CONNECTED` while reads to that bucket fail (`context deadline exceeded`
/ `nats.ErrNoResponders`). Every auto-heal path keys off the wrong signal for this
"connected-but-KV-reads-timing-out" state.

**RC1 — the manager never enters Degraded (enabler → fixed by F-D1).** `recordKVError`
counts an error only if `IsConnectivityError || IsDegradingJetStreamError`
(`manager_degraded.go:98`); neither `context.DeadlineExceeded` nor `nats.ErrNoResponders` is
in those classifiers (`internal/natsutil/errors.go:~119-136`), and `checkConnectionHealth`
keys off `nats.Conn.Status()`, still `CONNECTED`. So the circuit never trips → `StateDegraded`
is never entered → `attemptRecoveryFromDegraded` never runs. And even if it did, recovery
only re-pulls the assignment — it never re-writes claims or touches the resolver cache. The
manager self-heal is both *disabled* and *insufficient for the data plane*.

**RC2 — the resolver manufactures an irreversible tombstone (load-bearing → fixed by F-D2).**
In `ClaimBasedResolver.reconcileOnce` (`internal/durable/claim_resolver.go`), in the
`Keys()`-ok / `Get()`-fail window (which real quorum loss produces, ~1 s after leader loss):
the unreadable key is dropped from `seen` (`:995-999`), the tombstone pass synthesizes a
delete at revision **`R+1`** (`:1021-1035`), and the `applyPendingBatch` `>=` guard (`:~865`)
makes it **permanently beat** the live claim at `R`. After recovery the claim returns at `R`,
but `R+1 >= R` rejects it; `GetOwner` returns `ok=false` (`:447-455`) → pull-gating suppresses
the partition forever with `resolve_error` (`worker_consumer.go:656-659`). **Nothing in-process
clears it:** `warm()` runs once at `Start` (`:518`/`:365`), never on watcher re-establish;
`ForceRefreshPartition` loses to the same `>=` guard (`:491-496`); the watcher redelivers at
`R` (also loses). **Only a process restart clears the poison.**

**RC3 — startup empty-diff retry self-exit (latent → fixed by F-D3).** A worker *(re)starting
during* the outage: `waitForAssignment` pre-advances the snapshot before claims are written
(`manager_election.go:454`); the initial apply fails; `scheduleApplyRetry` re-applies with
`prev = CurrentAssignment()` = the full set (`manager_assignment.go:~1435`) → empty prepare
diff (`twophase.go:228-231`) → zero claims written → retry self-exits → claims absent until
restart. (Only fires if a worker restarts mid-outage.)

**The cross-cutting structural reasons there is no auto-heal:**
1. **Recovery keys off connection state, not KV-read health** — the connection never drops,
   so nothing fires (RC1).
2. **The poison lives where the manager can't reach it** — the manager's only self-heal
   (`scheduleApplyRetry`) re-writes *KV*; the tombstone is in the *consumer's in-memory
   resolver cache* (different subsystem). In steady state no apply even fires (version gate
   re-reads but doesn't re-apply), so the claim is never re-written to a revision `> R+1`.
3. **Pull-gating is fail-closed with no active re-resolve** — a missing/tombstoned claim
   suppresses pulls indefinitely; the ~150 ms suppression poll never force-refreshes.
4. **A transient read failure is converted into permanent, monotonic-revision-irreversible
   destructive state** (the `R+1` tombstone), protected against every recovery read-path.

**Not the bug (red herring):** restart-time `wrong last sequence: key exists` (err 10071) is
the expected stable-ID pool reclaim on a fresh process, not the failure.

---

## 0. Invariants the fix MUST NOT regress (load-bearing)
1. **The 3 cross-feature contracts in `AGENTS.md`** — esp. (a) whole-bucket-missing →
   every worker `StateDegraded` via `recordKVError`; (b) peer-claim-takeover → only that
   worker enters claim-lost shutdown via `onClaimerError` (which keys off
   `IsConnectivityError || IsDegradingJetStreamError`). **Any classifier change (F-D1)
   touches the exact predicate these pin.** Run their regression tests + `make test-integration -race`.
2. **Monotonic-revision cache semantics** in the resolver (`applyPendingBatch` `>=` guard)
   — load-bearing for steady-state correctness; the F-D2 fix must not weaken it.
3. **The repro suite is the regression oracle:** Tier 0 unit (`claim_resolver_quorumloss_test.go`),
   the S2/S3 black-box harness, and the NATS-only probe must all still pass / now flip to
   the fixed behavior where they previously demonstrated the bug.

---

## 1. F-D2 — resolver must not manufacture a tombstone from a transient READ failure (PRIMARY)

**Root cause (verified, `internal/durable/claim_resolver.go`):** in `reconcileOnce`, a
key whose `Keys()` listing SUCCEEDED but whose per-key `Get()` ERRORED is `continue`d
(`:995-999`) → never added to `seen` → then, because it is in the pre-`Keys` snapshot but
not in `seen`, the tombstone pass stages a synthetic delete at `R+1` (`:1021-1035`) that
permanently beats the live claim. A *transient read failure is being treated as a deletion.*

### F-D2a (root-cause, lowest-risk, highest-value) — THREE-way, not two-way
**[Review P0 fix]** The naive "listed regardless of `Get` outcome → don't tombstone" rule
is too broad: `reconcileOnce` today also relies on the tombstone pass for the
**`Get`-succeeds-with-a-delete/purge-op** case (`claim_resolver.go:1000-1003` skips adding
such a pid to `seen`). Lumping those into `listed` would leave a genuinely-deleted claim
visible (`GetOwner` `ok=true`). So distinguish **three** outcomes per listed key:
- **`Get` ERRORED** (transient — `DeadlineExceeded`/`ErrNoResponders`): add pid to a new
  `unreadable` set. **Never tombstone an `unreadable` pid** — it exists, we just couldn't
  read it this pass; a later pass re-reads it. *This is the incident fix.*
- **`Get` OK, op = `KeyValueDelete`/`KeyValuePurge`**: a genuine delete → **still tombstone**
  (stage an authoritative delete at the entry's real revision, or let the existing synthetic
  path run). Not protected.
- **`Get` OK, live claim**: `seen`, staged as upsert (unchanged).
- **Absent from `Keys` entirely**: genuinely gone → tombstone (backstop, unchanged).

Concretely: the tombstone pass skips a snapshot pid **only if it is in `unreadable`**;
absent-from-`Keys` and delete-op pids still tombstone. Preserves the legitimate-delete path
and the monotonic guard; net change local to `reconcileOnce`.

### F-D2b (defense-in-depth — self-heal a poisoned entry without restart) — REPLACEMENT, not guarded refresh
**[Review P1 fix]** Confirmed against source: `ForceRefreshPartition` returns without
updating when `existing.revision >= entry.Revision()` (`claim_resolver.go:495`). A tombstone
at `R+1` vs a live read at `R` → `R+1 >= R` → **no-op**; the current call would NOT self-heal.
So F-D2b must be an explicit **replacement** rule, not the existing guarded refresh:
- **The guard-bypass condition (Review P2 — how to identify a deleted entry).** A new method
  (e.g. `forceReplaceResolve(ctx, pid)`) does the direct `Get`, then under `r.mu`: if the
  existing cache entry has `existing.deleted == true` (the field `GetOwner` already reads at
  `claim_resolver.go:454`; the tombstone sets `{deleted:true, revision:R+1}`) AND the fetched
  claim is a **live, non-delete** op → **replace** that entry unconditionally (do NOT apply
  the `existing.revision >= entry.Revision()` guard). If `existing` is **not** `deleted`, the
  existing stale-guard applies unchanged (no regression). A fetched delete/purge op must
  **not** resurrect (leave the tombstone). This is a *new* primitive — do NOT reuse
  `ForceRefreshPartition` unchanged (it would no-op against `R+1`).
- **Wiring (resolves the bridge gap, Review P1) — consumer-local, explicit mechanism.** No
  change to `shouldSuppressPull`'s `(bool, reason)` return contract. On a `GetOwner` miss, the
  suppression site calls `forceReplaceResolve(pid)` as a **rate-limited side-effect**
  (existing `refreshCooldown`), then **still returns `(true, "resolve_error")` for THIS poll**.
  Pull-gating re-polls (~150ms); once the bounded re-resolve has replaced the tombstone, the
  **next** poll resolves `ok=true`. So suppression self-clears within ~one cooldown without
  any manager↔consumer plumbing and without an async out-param. (Inline-bounded vs
  fire-and-forget for the `forceReplaceResolve` call is an implementer choice; inline-bounded
  with a short ctx is simplest and avoids a goroutine.)
- **Decided:** event-driven re-resolve-on-suppression (consumer-local, as specified above).
  A periodic sweep of `deleted` entries was considered and rejected — event-driven is cheaper
  and tied to actual demand.

### F-D2c (observability)
Feed the reconcile `Get`/`Keys` error to a counter/log (today it is swallowed). This is a
one-way **metrics/logging feed only** — NOT a manager↔consumer control bridge (that was
dropped; see F-D1). It just surfaces resolver read failures for alerting.

**Tests:** Tier 0 unit gains a case asserting F-D2a leaves a listed-but-unreadable pid
`ok=true`; the S2 harness must now show `(b) consumer resumes = true` WITHOUT restart
(the bug test flips to the fixed assertion). Negative-space: a genuinely-deleted key still
tombstones.

---

## 2. F-D1 — classify connected-but-KV-read-unavailable as a degrading condition (SECONDARY, HIGHEST-RISK)

**Why secondary:** even when the manager degrades, `attemptRecoveryFromDegraded` only
`refreshAssignmentFromNATS` — it **never clears the resolver cache** (the resolver is a
different subsystem). So F-D1 alone does NOT fix the data-plane suppression; F-D2 does. F-D1's
value is (a) observability/alerting (the operator sees Degraded instead of silent stoppage)
and (b) a hook to drive the consumer re-resolve.

**Design (narrow, contract-safe) — [Review P1 fixes]:**
- **Predicate matches ONLY `context.DeadlineExceeded` + `nats.ErrNoResponders`** — **NOT**
  `jetstream.ErrNoStreamResponse`. Confirmed: `ErrNoStreamResponse` is already in
  `IsConnectivityError` (`natsutil/errors.go:128`) AND is the stableID whole-bucket-loss
  surface — `renew` wraps it as `ErrClaimLost`, and `onClaimerError` routes
  `ErrClaimLost`+connectivity/degrading into `recordKVError` (the whole-bucket path,
  `manager_election.go:107-115`). Including it would **steal the whole-bucket-loss route**
  and break contract 1. Do **NOT** widen `IsConnectivityError`/`IsDegradingJetStreamError`
  (contract 2's `onClaimerError` keys off exactly those).
- **Call-site scoped, not global — concrete API [Review P1].** Define a sentinel
  `ErrKVReadUnavailable` and a call-site wrapper `markKVReadUnavailable(err)` that wraps
  `context.DeadlineExceeded`/`nats.ErrNoResponders` (and ONLY those, after excluding
  stream-missing/bucket-missing) **applied only at the manager's handoff/assignment claim
  read+list sites** — i.e. the sites that already feed `recordKVError` (heartbeat / election /
  assignment-watcher / stableid-renew + the handoff claim get/list in the apply path). In
  `recordKVError`, admit the new path via `errors.Is(err, ErrKVReadUnavailable)` → degrade
  with a **distinct reason** `kv-read-unavailable` (NOT `"KV error threshold exceeded"`,
  preserving that docstring contract). Because the wrapper is applied ONLY at those sites,
  an unwrapped `DeadlineExceeded`/`ErrNoResponders` from anywhere else (or a peer-takeover
  claim-get timeout, which flows through `onClaimerError`, not these sites) never enters the
  new path. Do not add the raw errors to any global predicate.
- **No manager→consumer bridge.** [Review P1] The earlier "bridge" is dropped — there is no
  wireable surface for it (`UpdateWorkerConsumer` only takes `(workerID, partitions)`; the
  resolver is private to `WorkerConsumer`). The data-plane self-heal lives entirely in
  **F-D2b (consumer-local)**. F-D1 is therefore **purely manager-side observability**:
  the operator sees `Degraded(kv-read-unavailable)` instead of a silent stall. It does NOT
  itself fix the data plane (F-D2 does).

**RISK CALLOUT for the reviewer:** this is the change AGENTS.md warns about. Mandatory:
the 3 contract regression tests (`TestManager_LiveNATSBucketLoss*`, `TestStableID_StaleKeyTakeover_Reclaim`,
`OnDegradedHook`) + `make test-integration -race` + a classifier/routing table test (below)
proving stableID bucket-deletion still reaches whole-bucket-degraded, peer-takeover still
reaches claim-lost shutdown, and handoff `Get`/`Keys` deadline/`ErrNoResponders` reach the
new reason ONLY from the intended call-sites.
**Open decision:** is entering Degraded on transient read timeouts desirable, or does it
cause Degraded flapping on brief blips? Tunable via `KVErrorThreshold`/`KVErrorWindow`.

---

## 3. F-D3 — close the startup empty-diff retry self-exit (latent)

**Root cause (verified):** `waitForAssignment` pre-advances the snapshot
(`manager_election.go:454`/v2.5.0:428) before claims are written; the initial apply uses an
explicit empty `prev` (so it WOULD write the full set) but fails under the KV-write fault;
`scheduleApplyRetry` then re-applies with `prev = m.CurrentAssignment()` = the pre-advanced
full set → empty prepare diff (`twophase.go:228-231`) → trivial success, zero claims written,
retry self-exits.

**Decision: option 3a** (more localized than 3b, which would reorder startup). **[Review P1]**
Spec:
- Add a manager flag `initialClaimsCommitted` (atomic bool), set `true` the first time an
  apply successfully commits claims to the handoff bucket.
- In `scheduleApplyRetry`, choose the retry's `prev` by that flag: **while
  `initialClaimsCommitted == false`, retry with an explicit `Assignment{}` (empty) `prev`**
  (bootstrap semantics → full prepare diff → claims actually written); **once `true`, revert
  to `m.CurrentAssignment()`** (normal incremental semantics — unchanged behavior).
- **Concurrent commit-watcher guard:** a commit-watcher apply that commits claims sets the
  flag, so a subsequent retry sees `true` and uses `CurrentAssignment()` — it will NOT
  re-issue a full-set write over already-committed claims. The two-phase coordinator's
  per-claim CAS (`UpdateClaim` Create/Update) already makes a racing double-write safe
  (loser CAS-fails); the flag just avoids the wasteful full re-prepare. The
  concurrent-commit-watcher test (below) pins this.
- **(3b deferred, out of scope here):** coupling snapshot-advance to claim-commit is a
  larger startup-ordering change; 3a achieves the fix without it.

**Tests:** the S3 harness `long_outage_restart_only` case must flip to self-heal; add a unit
assertion that a retry after a failed initial apply re-attempts the full claim set.

---

## 4. Phasing (PR-by-PR; each independently shippable, own tests, `make pre-pr`)
1. **PR1 — F-D2a** (resolver root-cause). Smallest, highest-value, lowest-risk. Flips the S2
   harness to self-heal-after-read-recovery and the Tier 0 listed-but-unreadable case.
2. **PR2 — F-D2b/c** (re-resolve self-heal + observability). Makes any residual poisoning
   self-healing; defense-in-depth.
3. **PR3 — F-D1** (classifier + manager-side Degraded reason; **no consumer bridge** — that
   was dropped, the data-plane self-heal is consumer-local in PR2). HIGHEST RISK — full
   contract + integration gate. Ship only after PR1/PR2 (which already fix the data plane),
   so F-D1 is observability/robustness, not the sole fix.
4. **PR4 — F-D3** (startup empty-diff). Independent; flips the S3 long-outage case.

Order rationale: the data-plane fix (PR1) lands first; the risky classifier change (PR3)
lands last, when it is no longer load-bearing.

**[Review P2] Rollout note — PR1 alone does NOT fully resolve a live incident.** F-D2a
*prevents new* tombstones in **upgraded** processes only. It does **not** clear an
**already-poisoned** in-process cache (the guarded refresh loses to `R+1`; only PR2's
replacement re-resolve, or a restart, clears it), and during a **rolling upgrade** an
old-version consumer can still manufacture/retain tombstones until restarted. **For the
operational release, ship PR2 with or before PR1** so the deployed fleet both stops
poisoning and self-heals existing poison without a restart.

### Per-task model & effort (implementation dispatch)
For the **implementation** sub-agent of each PR. Use a **Claude Agent** for implementation
(repo context, idiomatic Go tests, runs `make pre-pr`); use **Codex** only for the *review*
gate (`/post-impl-review` or `/codex:review`), per the repo's external-reviewer pattern.
"Effort" for a Claude Agent is **not a literal dial** — it is the verification rigor mandated
in the dispatch prompt: verify-first (the ported test fails on the parent, passes with the
fix), the §5 test tables, and the gates named below. Reserve **Opus** where a subtle error
regresses a cross-feature contract or corrupts shared mutable state; use **Sonnet** where the
change is localized, the spec is airtight, and an oracle test already exists.

| PR | Task | Model | Effort | Why this tier |
|---|---|---|---|---|
| PR1 | F-D2a resolver root-cause | **Sonnet** | high | Localized `reconcileOnce` change; airtight spec (§1) + the Tier 0 boundary table is the exact oracle; lowest risk. Rigor goes on the 3-way split (errored vs delete-op vs absent) and on **flipping the existing Tier 0 cases non-vacuously** (they currently assert the bug). |
| PR2 | F-D2b/c replacement re-resolve | **Opus** | high | Data-plane **hot path** (`shouldSuppressPull`), mutates the **shared resolver cache** under the existing `mu`/atomic-pointer model; AGENTS.md **concurrency-stress test required**. The replace-only-when-`deleted` vs keep-the-stale-guard logic is subtle. |
| PR3 | F-D1 classifier + Degraded reason | **Opus** | **max (xhigh)** | **HIGHEST RISK.** Touches the exact classifier the 3 AGENTS.md cross-feature contracts pin; must not steal the whole-bucket route (`ErrNoStreamResponse`) or mis-route a peer-takeover (`onClaimerError`). **Mandatory gate:** the 3 contract regression tests + `make test-integration -race` + the §5 classifier/routing table. A subtle error here silently regresses a load-bearing contract. |
| PR4 | F-D3 startup empty-diff | **Opus** | high | Apply/startup **lifecycle** (`scheduleApplyRetry`/`waitForAssignment`); the `initialClaimsCommitted` flag must be set/read correctly under **concurrent applies**, and the CAS-race with the commit-watcher must hold. |

**Combined-PR note:** PR1+PR2 ship together for the operational release (rollout note above)
and both touch the resolver — if implemented as **one** change, dispatch it on **Opus** (take
the higher tier of the pair). PR1 as a standalone prevention-only PR is the only Sonnet task.

**Per-PR loop (all four):** implement (Claude Agent, above) → `/simplify` → review gate
(`/codex:review`, fall back to `/post-impl-review` for spec-compliance) → fix-loop to
merge-clean. Every PR runs `make pre-pr` (all touch `manager/` / `internal/durable` /
`internal/assignment`). Promote each scenario from `tmp/parti-repro/` into the tracked test
tree **within its PR**, flipped to assert the fixed behavior (see "promotion = per-PR" — do
not big-bang before or defer after).

## 5. Test strategy (per AGENTS.md discipline)
- Promote the repro harnesses to tracked regression guards as fixes land (`02-…repro` §5):
  Tier 0 → already in `internal/durable`; S2/S3 + probe → `test/integration/failure/` + a
  `partitest` N-node helper.
- `make pre-pr` on every PR (all touch `manager/`/`internal/durable`/`internal/assignment`).
- Negative-space tests (per `feedback_test_both_directions_of_boundary`): genuine delete still
  tombstones (F-D2); peer-takeover still claim-lost (F-D1); no Degraded flapping on a single
  blip below threshold (F-D1).

**Required test tables [from review]:**
- **F-D2a boundary** (`internal/durable` unit): {listed + live `Get`}, {listed + `Get`
  error → NOT tombstoned}, {listed + `Get` delete-op → tombstoned}, {listed + `Get` purge-op
  → tombstoned}, {absent from `Keys` → tombstoned}, {prefix-filtered key affects neither set}.
- **F-D2b replacement**: {deleted cache `R+1` + live KV `R` → refreshes to `ok=true`},
  {non-deleted cache `R+10` + KV `R` → stays unchanged (no regress)}, {direct `Get` returns
  delete/purge → does NOT resurrect}. Plus the S2 harness flips: consumer resumes WITHOUT
  restart from an already-poisoned cache.
- **F-D1 classifier/routing table**: `context.DeadlineExceeded`, `nats.ErrNoResponders`,
  `jetstream.ErrNoStreamResponse`, `ErrBucketNotFound`, `ErrStreamNotFound`, wrapped
  `ErrClaimLost` — each asserted to route to the correct path (new `kv-read-unavailable` vs
  whole-bucket-degraded vs claim-lost-shutdown), from the intended call-sites only.
- **F-D3 retry** (must match the chosen 3a flag semantics): initial apply fails after partial
  claim writes with `initialClaimsCommitted == false` → the retry uses explicit empty `prev`
  and writes the **full** set (no zero-claim self-exit). Once any apply commits claims (the
  retry itself OR a concurrent commit-watcher) the flag flips to `true` and subsequent applies
  correctly revert to `m.CurrentAssignment()` semantics; assert a concurrent commit-watcher
  racing the retry does not double-write (per-claim CAS makes the loser a no-op,
  `kv_store.go:100/116`, `twophase.go:289/317`).

## 6. Out of scope
- F2 (read-only-filesystem) incident variant.
- The optional v2.10.29 confirmation run (`…repro` #1) — orthogonal to the fix.

## 7. Open decisions for review
(F-D2b mechanism — event-driven, consumer-local — and F-D3 option 3a are now **decided** in
the body above; they are no longer open.)
1. F-D1: is auto-Degraded on transient read timeouts desirable (alerting value) vs flapping
   risk? Default `KVErrorThreshold`/`KVErrorWindow` tuning.
2. Should pull-gating additionally **fail-open after a bounded suppression timeout** as a
   last-resort safety net, independent of the resolver fix? (Overlaps the known-deferred
   "resolver fail-open" follow-up.)
