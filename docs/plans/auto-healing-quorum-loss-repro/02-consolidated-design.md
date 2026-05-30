# Auto-Healing Quorum-Loss Reproduction — Consolidated Design

- **Date:** 2026-05-29
- **Status:** DRAFT for user review — consolidates perspective A (`00-repro-design.md`,
  Claude), perspective B (`01-codex-investigation.md`, independent blind Codex), and a
  third mechanism surfaced during apply-path verification (Defect 3).
- **Source incident:** `tmp/parti_auto_healing_issue.md`
- **Scope:** reproduce + black-box baseline + version-compare. **The fix is a separate,
  later effort.**

> Supersedes `00-repro-design.md` as the working design. `00` and `01` are retained as
> the two independent inputs; this file is the merge.

---

## 1. The shape of the answer (read this first)

There are **three independently-verified-in-code mechanisms** that can each leave a
worker stalled after a connected KV-timeout outage. They are **not** ranked
root-causes competing for one slot — they are **distinct incident scenarios that
produce the same symptom** ("work stops, only a pod restart recovers"). **The logs
alone cannot tell us which one fired.** So the job of the reproduction is not merely to
*reproduce a stall* — it is to **discriminate** between the mechanisms, because that is
what tells the fix authors what to fix.

The single most important structural fact, which took the whole investigation to state
cleanly:

> **The claim *resolver* (consumer-side cache, where the tombstone lives) and the
> handoff *coordinator* (manager-side claim writer) are different subsystems.** The
> manager's only in-process self-heal — `scheduleApplyRetry` — re-writes **KV**. The
> tombstone poisons the **resolver's in-memory cache**. Whether the manager's KV
> re-write reaches and repairs the resolver's cache is exactly the hinge the repro
> must settle.

### The three mechanisms (all FACT-in-code; which one fired = open)
- **Defect 1 — manager never enters Degraded.** `context.DeadlineExceeded` is
  classified as neither connectivity nor degrading, so the Degraded/recovery machinery
  never engages. **Verified secondary:** the pull gate is manager-state-independent, so
  this alone cannot close the data plane, and the Degraded-recovery path never
  re-writes claims anyway. It is a real gap (it disables the manager's *assignment*
  re-pull), but not by itself the restart-only cause.
- **Defect 2 — irreversible resolver tombstone.** A `Keys()`-ok / `Get()`-fail
  reconcile synthesizes a delete tombstone at `revision R+1` that permanently beats the
  live claim at `R` in the resolver cache. **Load-bearing for restart-only recovery —
  but only in the steady-state scenario** (see §2).
- **Defect 3 — uncommitted claim + bounded retry.** An apply interrupted mid-flight
  leaves some partitions with no claim in KV. `scheduleApplyRetry` *does* retry, so
  this **self-heals** once KV recovers — *unless* the retry didn't outlive the outage,
  was version-gated out, or the failure was on a path that never scheduled a retry.

---

## 2. Root cause — reconciled (three mechanisms, two scenarios)

### Defect 1 — manager never enters Degraded — **FACT (logic); secondary (verified)**
- `natsutil/errors.go::IsConnectivityError` matches `nats.ErrTimeout`, `i/o timeout`,
  `connection refused`, etc. — **not** Go's `context.DeadlineExceeded`.
- `manager_degraded.go::recordKVError` early-returns unless
  `IsConnectivityError || IsDegradingJetStreamError`, so the KV-error counter never
  moves → `enterDegraded` is never called from KV timeouts.
- `checkConnectionHealth` keys off `nats.Conn.Status()`. In F1 the connection **stays
  CONNECTED** (meta-quorum survives on 3/5 nodes; only the RF=3 handoff bucket loses
  quorum), so it doesn't fire either.
- **Why secondary (verified):** `shouldSuppressPull` reads *only* `resolver.GetOwner`
  — **zero** references to Degraded/`State()` (`worker_consumer.go:641-695`). And
  `attemptRecoveryFromDegraded` calls only `refreshAssignmentFromNATS` /
  `recordKVSuccess` / `exitDegraded` — it **never re-writes claims**. So Defect 1
  neither closes the gate nor, if fixed, would clear a poisoned cache. Its real cost:
  the manager's assignment re-pull self-heal is silently disabled during the outage.

### Defect 2 — irreversible resolver tombstone — **FACT (mechanism), Claude-verified against source**
In `internal/durable/claim_resolver.go::reconcileOnce`:
1. Snapshot cache, then `Keys()` (`:977`). If `Keys()` **errors**, early-return, cache
   untouched (`:978-984`) — benign.
2. If `Keys()` **succeeds** but a per-key `Get()` **fails**, the loop `continue`s
   (`:995-999`) → that pid is **never added to `seen`**.
3. Tombstone pass: every snapshot pid not in `seen` is staged as a synthetic delete at
   `e.revision + 1` (`:1021-1035`).
4. `applyPendingBatch` writes `{deleted:true, revision:R+1}` because `R >= R+1` is false
   (`:862-868`).
5. **After recovery**, the live claim returns at revision `R`; the upsert is rejected
   because `R+1 >= R` is now true. The tombstone wins.
6. `GetOwner` returns `ok=false` for a deleted entry (`:447-455`) → `shouldSuppressPull`
   → `resolve_error`, suppressed (`worker_consumer.go:656-659`).
7. **No read-path clears it (verified):** the reconciler stages recovered claims at `R`
   (`:1010-1015`); the watcher stages at `upd.Revision()` = `R` (`handleWatcherUpdate`,
   `:845`); both lose to `R+1` at the shared guard. `warm()` — the only full cache
   *replace* (`:553`) — runs **once, in `Start()` (`:365`)**, never on watcher
   re-establish (which goes `runWatcher` → merge, not `warm`). So **only a new process**
   clears it.

#### The one way to beat the tombstone WITHOUT a restart — and why it usually doesn't happen
The guard is beaten only by a claim **write at a revision `> R+1`**. Claim writes go
through `kv_store.go::UpdateClaim`: **`Create` for a brand-new claim (`currentRev==0`),
`Update` (CAS) for every subsequent state transition** — and each `Update` bumps the KV
revision. So a *fresh apply that re-writes the claim* (prepare→commit→stabilize) would
push the claim to `R+2`/`R+3`…, which the watcher then delivers and which **beats the
tombstone**.

**But that re-write only happens if an apply fires.** And here is the steady-state trap:
if the manager is **Stable on the version whose claims are already in KV at `R`**, the
version gate (`handleAssignmentEntry`: skip when `oldVersion >= newVersion`) means the
assignment reconcile **re-reads but never re-applies**, and `scheduleApplyRetry` has
nothing stashed. **No apply → no claim re-write → tombstone stands until restart.**

→ **This is the precise, load-bearing explanation for restart-only recovery in the
steady-state scenario.** Defect 2 is not "latent"; it is the unique non-restart-proof
mechanism *when poisoning happens to an already-committed-version cache during the
recovery window*.

### Defect 3 — uncommitted claim + bounded retry — **FACT (mechanism), Explore-verified; self-healing CONDITIONAL**

> **UPDATE — settled by the S3 reproduction (see `04-tier1-s3-verdict.md`).** The
> "self-heals once KV recovers" claim below holds for the **rebalance** shape but
> is **FALSE for the initial-startup window**: the startup apply *does* schedule a
> retry, but the retry is neutered by an **empty prepare diff** (the snapshot is
> pre-advanced to the full set by `waitForAssignment`, `manager_election.go:454`,
> with no claim writes), so it self-exits writing zero claims and the partitions
> stay claim-less until restart. This is a distinct, source-verified third
> restart-only path — not the "(a) no retry scheduled" or "(c) version-gate stash
> drop" hypotheses guessed below.

`manager_assignment.go:1248` calls `handoffCoordinator.Apply`. The two-phase coordinator
(`twophase.go` prepare/commit/stabilize) writes claims via `UpdateClaim`; on a mid-apply
`context deadline exceeded` it returns an error with **no rollback and no claim
deletion** (verified: no delete/abort path; sweep only resets non-stable claims). So a
partial apply leaves some partitions claim-less in KV.

On failure, `scheduleApplyRetry` (`:1399-1454`) spawns a **robust** retry loop:
coalesces to the highest version, exponential backoff to 30s, exits only on `ctx.Done()`
(Stop) or success. **So Defect 3 self-heals once KV recovers** — the retry eventually
re-runs Apply, writes the missing claims, and (because those are fresh `Create`s or
CAS `Update`s at new revisions) the resolver picks them up via watcher.

**Defect 3 explains restart-only recovery only in a narrow window:** (a) the failure was
on a path that never called `scheduleApplyRetry` (e.g. the *initial* startup apply — to
be checked), or (b) the retry loop exited via `ctx.Done()` without the process
restarting (contradiction — so unlikely), or (c) a version-gate interaction dropped the
stash. **If none of those hold, Defect 3 should NOT need a restart** — which is exactly
why it is a *discriminating* test (§4).

### The two scenarios, side by side
| | **Scenario S2 (Defect 2)** | **Scenario S3 (Defect 3)** |
|---|---|---|
| Pre-outage state | Stable; claims committed at `R` | apply in-flight (rebalance / startup) |
| What breaks | recovery-window reconcile poisons cache | partial claim writes, apply errors |
| KV after recovery | claims present at `R` | some claims missing |
| In-process self-heal? | **No** — no apply fires, tombstone unbeaten | **Yes (expected)** — `scheduleApplyRetry` rewrites |
| Restart-only? | **Yes** | Only if retry didn't outlive / never fired |
| Log fingerprint | `reconcile … list keys failed`, `resolve_error` | `handoff apply failed: claim get … deadline` |

The incident logs contain **both** fingerprints, so neither scenario is excluded. The
asymmetry risk from §0 remains: the only *direct* evidence (`list keys failed`) is the
**benign** reconcile branch — i.e. Defect 2's required `Keys`-ok/`Get`-fail window is
not directly witnessed. **Discriminate, don't assume.**

### Restart-time `wrong last sequence: key exists` (10071) — **FACT, not the bug**
Expected stable-ID pool re-walk on restart (`stableid/claimer.go`): `kv.Create` collides
on the still-present key, code falls through to reclaim. A red herring.

---

## 3. HEAD verdict — **VERIFIED FACT**

All five critical-path files for **Defect 1 + Defect 2** are **byte-identical
between v2.5.0 and HEAD** (`git diff v2.5.0 HEAD --numstat` empty):
`claim_resolver.go`, `worker_consumer.go`, `partition_consumer.go`,
`natsutil/errors.go`, `manager_degraded.go`. So the resolver-tombstone and
error-classification verdicts are HEAD-and-v2.5.0 identical.

> **CORRECTION (do not repeat the original overclaim).** The apply path is **NOT**
> unchanged: `git diff --numstat v2.5.0 HEAD` shows `manager_assignment.go`
> **+337/−15**, `manager_election.go` **+26**, `internal/assignment/handoff/twophase.go`
> **+3/−3**. `scheduleApplyRetry` was refactored — v2.5.0 retried via
> `applyAssignment(*pending)`, HEAD via
> `applyAssignmentWithPrevSkipJitter(m.CurrentAssignment(), *pending)`. **However, the
> Defect-3 startup empty-diff finding (`04`) was traced on both versions and HOLDS on
> v2.5.0**: v2.5.0's `applyAssignment` → `applyAssignmentWithPrev(m.CurrentAssignment(), …)`
> uses the same `m.CurrentAssignment()` prev-source, and the pre-advance + twophase
> empty-diff early-return are both present on v2.5.0. So the startup path **is**
> attributable to the incident build (source-verified; reproduced on HEAD). The
> refactor changed names/jitter, not the mechanism. Defects 1 and 2 are unaffected
> (byte-identical files).

The 39 post-tag commits are sim/test/docs + apply-jitter, watcher/commit debounce,
handoff phase concurrency, source recreated-bucket recovery (a *different* reconcile
path), and a nats.go v1.50→v1.52 bump.

**All three mechanisms also pre-date v2.5.0 — VERIFIED.** v2.4.1 already contains the
synthetic-tombstone reconcile (`e.revision + 1`), the fail-closed pull gate
(`resolve_error`), the same `applyPendingBatch` `>=` guard, and the unclassified
`context.DeadlineExceeded`. v2.5.0's self-healing work added recovery for
whole-bucket-loss / connectivity loss and never touched these paths.

**→ HEAD does NOT fix this** for Defects 1 and 2 — settled by `git diff` *before*
any test runs: those files are byte-identical. (The apply path *did* change, so
Defect-3 behavior must be read on the specific version — see the correction above.
Consequence for the matrix — see §4.)

---

## 4. Reproduction design — discriminating, not just reproducing

One standalone Go module at `tmp/parti-repro/` (own `go.mod`); parti consumed only
through its **public** API.

### Tier 0 — deterministic resolver-unit repro (PRIMARY) — pins Defect 2 + its boundary
Exercise `ClaimBasedResolver` directly with a fake `jetstream.KeyValue`.

- **Case A (the bug):** warm with `claims/USER21@R` → `Keys`-ok / `Get`-fail →
  `reconcileOnce` → restore `Get` to `R` and also deliver via watcher → **assert
  `GetOwner` STILL `ok=false`** through both recovery read-paths. Control: fresh
  `warm()` returns `ok=true` (restart-fixes-it).
- **Case A′ (fleet-wide):** `Keys`-ok / **all**-`Get`-fail in one pass → assert **all**
  PIDs tombstoned — the mechanism for the report's "all fail."
- **Case A″ (the heal, for the fix authors):** after Case A's tombstone at `R+1`,
  deliver a claim **re-write at `R+2`** via the watcher → **assert `GetOwner` becomes
  `ok=true`**. This proves a KV re-write *does* beat the tombstone — i.e. tells the fix
  authors whether "re-write on recovery" is a viable fix and confirms the S2-vs-S3
  distinction is real.
- **Case B (boundary, reconciler path only):** `Keys()` **itself** fails →
  `reconcileOnce` early-returns → **assert `GetOwner` STILL `ok=true`** (no poisoning).
  Scope caveat: this models the *reconciler* path only; what the real nats.go watcher
  does under quorum loss is a Tier 2 question (prior empirical finding: its `Updates()`
  channel does *not* close on server restart).

Fast, fully deterministic, no NATS, no race. A + A′ + A″ + B are the regression guard.

### Tier 1 — consumer-level discriminator (symptom-injection through the public API)
Wrap the handoff bucket's `jetstream.KeyValue` behind a fault-injecting shim while the
connection stays `CONNECTED`. Drive `NewDynamic(...)` exactly as FDC does
(`WithPullGating(true)`, `WithProcessingGate`, `WithResolver{HandoffBucketName}`, FDC
timings). Two scenarios, run separately:

- **S2 — steady-state poisoning:** reach Stable with claims committed → inject the
  asymmetric `Keys`-ok/`Get`-fail window (no rebalance) → restore healthy → **assert
  work does NOT resume without restart** (Defect 2), and manager never enters Degraded
  (Defect 1). `Stop()/Start()` recovers.
- **S3 — in-flight apply (THE discriminating test):** trigger a rebalance so an apply
  is in-flight, kill handoff KV mid-apply, then restore → **assert whether
  `scheduleApplyRetry` heals without a restart.** *If it heals, S3 is FALSIFIED as the
  incident's cause* (production required a restart), leaving S2 (or something else).
  This is the highest-value test in the whole effort — it is real evidence, not just a
  reproduced bug.

### Tier 2 — faithful 5-node embedded cluster (the asymmetry linchpin), gated `PARTI_REPRO_CLUSTER=1`
The only tier that can answer **whether real quorum loss actually produces the
`Keys`-ok/`Get`-fail window** S2 depends on (Tiers 0/1 engineer it by hand). 5 embedded
`nats-server`s, file storage, client seeded with all 5 URLs (stays connected through
kills). parti buckets RF=3 (handoff/election/assignment=file, heartbeat=memory),
consumer RF=3 memory. Reach Stable, read the handoff stream's actual replica placement
(`StreamInfo().Cluster`), kill the 2 followers that break *that bucket's* quorum while
meta survives (3/5), assert `nc.IsConnected()` throughout, observe which mechanism
fires, restart the 2 nodes, check self-heal. `partitest.StartEmbeddedNATSCluster` is
hardcoded to 3 nodes with no kill API → needs a new N-node helper + `KillNode(i)`.
Slower/flakier; fidelity + linchpin, not a CI gate.

### Version handling — collapsed (per the HEAD verdict)
`git diff` already proves v2.4.1 / v2.5.0 / HEAD are identical on every relevant path,
so running the full repro three times proves nothing the diff doesn't. **Run the repro
once on HEAD; cite the `git diff` as the proof for v2.4.1 and v2.5.0.** A single
narrow confirmation run on v2.5.0 (the literal incident binary) is optional belt-and-
suspenders, not required.

| version | code on the 3 mechanisms | repro action |
|---|---|---|
| v2.4.1 | identical (verified) | none — cite diff |
| v2.5.0 (incident) | identical (verified) | optional 1 confirmation run |
| HEAD | identical (verified) | **run full Tier 0/1/2** |

---

## 5. Deliverables (scope stops before the fix)
- `tmp/parti-repro/` module: Tier 0 (A/A′/A″/B) + Tier 1 (S2/S3) + Tier 2 (env-gated).
- The **S3 discrimination verdict**: does `scheduleApplyRetry` self-heal? (falsifies or
  keeps Defect 3 as the incident cause).
- The **Tier 2 verdict**: does real quorum loss produce the `Keys`-ok/`Get`-fail window?
- **Retrospective — "what we got wrong":**
  1. The error taxonomy never modeled **connected-but-KV-reads-timing-out**; recovery
     keys off connection state, not KV-read health.
  2. The resolver reconcile **manufactures destructive state** (synthetic tombstones)
     from a *read* failure, monotonic-revision **irreversible** by any in-process
     read-path — a transient fault made permanent. Reconcile also swallows the error
     silently (never feeds `recordKVError`).
  3. Pull-gating is **fail-closed with no active re-resolve** on the suppression poll.
  4. The apply path has **no rollback** on partial failure and relies on a single retry
     loop whose non-firing (steady-state version gate) is invisible.
  5. Coverage blind spot: the self-healing suite never exercised
     quorum-loss-while-connected, the `Keys`-ok/`Get`-fail reconcile window, or
     mid-apply KV death.
  6. **The defects long pre-date the feature that hid them.** v2.5.0's "self-healing"
     was scoped to the manager/connection layer and left the consumer/resolver data
     plane unhealable.
- **Promotion note:** once a fix is green, lift Tier 0 into `internal/durable` unit
  tests, Tier 1/2 harness into `partitest`, and the integration test into
  `test/integration/failure/` (per AGENTS.md integration-discipline rules).

## 6. Out of scope
- Designing / implementing the fix (separate effort; must address all three mechanisms
  + a gate re-open / cache re-resolve path).
- F2 (read-only-filesystem / session-down) variant.

## 7. Open questions the repro must settle (not assumed here)
- **S3 discrimination:** does `scheduleApplyRetry` self-heal a mid-apply KV death after
  recovery? (Tier 1 S3.) Does the *initial startup* apply path schedule a retry at all?
- **S2 trigger reality:** does real NATS bucket-quorum-loss produce a `Keys`-ok /
  `Get`-fail window? (Tier 2.) What does the real nats.go KV watcher deliver under
  quorum loss — delete / stale snapshot / nothing?
- **Heal viability:** Case A″ confirms a KV re-write at `>R+1` beats the tombstone — is
  forcing such a re-write (or a resolver cache re-warm) the right fix shape? (Fix phase.)
