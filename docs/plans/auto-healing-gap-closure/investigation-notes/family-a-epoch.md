# Family A deep-dive: epoch-fence re-degrade + recover-on-wrong-signal flap (NP-2, NP-1)

Investigation of the auto-healing gap where an operator wipes+recreates Parti-owned
bucket(s) under a live worker. HEAD = `2453306` on `auto-heal-gap-investigation`.
Read-only investigation; all cites are `file:line` against the worktree.

Verdict: **confirmed-with-corrections.** Both halves of the report's root cause are
real and verified in code. But the report's fix framing is materially wrong on one
point: it presents opt-1 ("re-capture `ep.created`") and opt-2 ("refuse exit while
mismatch outstanding") as co-equal alternatives and says the proofs "are agnostic to
which fix lands" (`04-proof-findings.md:278`). **They are not.** Opt-1 alone fails
*both* proofs and is strictly *worse* than the status quo (it converts a flap into a
terminal false-healthy Stable). Only opt-2 yields the terminal Degraded both proofs
require. Details below.

---

## (a) Both halves verified in code — exact lines

### Half (i): the epoch fence never re-captures `ep.created` (the re-arm)

- `captureBucketEpoch` writes the cached Created timestamp **once**, inside
  `ensureKVBucket` at Start: `manager_setup.go:265` calls it, and it stores
  `m.bucketEpochs[bucket] = bucketEpoch{kv: probeKV, created: created}` at
  `manager_setup.go:627`. Nothing else ever writes `m.bucketEpochs` (grep:
  only writers are `manager_setup.go:613` map-init and `:627` assignment; the
  monitor goroutine `manager_startup_async.go:142` only reads).
- `checkBucketEpochs` (`manager_setup.go:669-693`) reads the **live** Created via the
  cached probe handle `ep.kv` (`:677`), and on mismatch (`:684 !live.Equal(ep.created)`)
  fires `m.enterDegraded("bucket-recreated:" + bucket)` (`:689`) and `return`s
  (`:690`). It **never updates `ep.created`.** So `ep.created` stays pinned at the
  *original pre-wipe* Created forever; every subsequent tick re-observes the same
  mismatch and re-fires `enterDegraded`.
- Tick cadence = `OperationTimeout` (`manager_setup.go:649`; default 10s, set to 1s in
  the NP-2 proof). The monitor is launched by `startPostStableMonitors`
  (`manager_startup_async.go:142`), `postStableMonitorsOnce`-guarded.

`enterDegraded` is idempotent while already degraded — it CAS-guards `degradedSince`
(`manager_degraded.go:309`). So a *second* `bucket-recreated` OnDegraded can only fire
**after an intervening `exitDegraded`** cleared `degradedSince` (`manager_degraded.go:356`).
That is the irrefutable oscillation proof: NP-2 evidence
(`tmp/repro-current-head/np2.out`) shows `bucketRecreatedDegrades=10, degradedToStable=9`
— ten entries require nine intervening exits.

> Note: the cached probe handle `ep.kv` (`manager_setup.go:615`,
> `m.js.KeyValue(ctx, bucket)`) **transparently re-binds to the recreated stream** —
> proven by the NP-2 evidence itself: if the stale handle errored on the recreated
> stream, `BucketStreamCreated` would return an error, `checkBucketEpochs` would
> `continue` at `:679-682`, and the fence would never fire. It fired 10×. So the
> nats.go KeyValue object resolves the bucket name lazily on each `Status()` call and
> reads the NEW stream's Created. This matters for the fix analysis in (c).

### Half (ii): recovery exits on the wrong signal (assignment read, never the fence)

The connection monitor ticks every 1s (`manager_degraded.go:79`). Since the
connection never dropped in this scenario, it stays on the "connection up" branch
(`:121-133`); once up for `ExitThreshold` (`:130`) it calls
`attemptRecoveryFromDegraded` **every tick**.

`attemptRecoveryFromDegraded` (`manager_degraded.go:376-416`):
1. `refreshAssignmentFromNATS()` (`:383`) — reads `assignment.<workerID>` from the
   **assignment bucket only** (`manager_assignment.go:1567-1568`).
2. `recordKVSuccess()` (`:393`) clears the whole KV-error window.
3. exit gate (`:409`): `if !m.currentAssignmentApplied(cur) { scheduleApplyRetry; return }`.
4. else `exitDegraded()` (`:415`) → `StateStable`.

The exit gate `currentAssignmentApplied` (`manager_assignment.go:1208-1217`) compares
the in-memory snapshot to `committedAssignment` on `(Version, LeaderRevision,
PartitionSetDigest, source-rev)`. **It never consults `m.bucketEpochs` or any epoch
mismatch.** Grep confirms `bucketEpochs` has zero readers outside
`manager_setup.go`/`manager_startup_async.go`. So the wipe signal is *structurally
invisible* to the exit decision. As long as the assignment bucket can satisfy the
commitment check, recovery exits — regardless of an outstanding epoch mismatch on a
*different* bucket (NP-2 recreates only heartbeat; the assignment bucket is intact).

These two loops fight at ~1 Hz: epoch tick (1s) re-degrades, recovery tick (1s)
re-stabilizes → sustained Degraded↔Stable flap. This violates the documented
`bucket-recreated:<bucket>` contract: `docs/OPERATIONS.md:126` ("Restart or rotate
workers; inspect JetStream storage before trusting the recreated bucket") and
`:750` ("Why the process does not self-heal: Parti deliberately does not auto-recreate
buckets from the live publish path… surfaces the problem via Degraded and leaves the
recovery decision to the operator"). A flap back to Stable defeats that.

> Report-accuracy check on the cited lines: the report says
> "`checkBucketEpochs (manager_setup.go:684-690)`" and "cached once at `:627`" and
> "`attemptRecoveryFromDegraded (manager_degraded.go:376-416)`". All three line ranges
> are **correct** on HEAD. The report's mechanism description of half (i)/(ii) is
> accurate; only the *fix framing* is wrong.

Orthogonality to `421f13c`/`recordKVHealthyOp` is confirmed: the epoch fence calls
`enterDegraded` **directly** (`manager_setup.go:689`), never touching `kvErrorWindow`,
so `recordKVHealthyOp` (`manager_degraded.go:266-288`) is irrelevant to this family.
The report's orthogonality claim is correct.

---

## (b) NP-1's false-healthy resting state — the precise WHY (version-independent)

NP-1 wipes+recreates **all four** buckets empty under a live 3-worker fleet (no
restart). Evidence (`tmp/repro-current-head/np1.out`): all 3 workers flap ~8
Degraded↔Stable cycles, `bucket-recreated:*` fires on all three, **all three end
`Stable` on empty recreated buckets**, run 67.7s.

The report calls this "false-healthy" but does not explain *why* the workers reach a
*resting* Stable (NP-2 keeps flapping; NP-1 settles). The structural answer:

**The exit gate (`currentAssignmentApplied`) is a commitment check that never consults
the epoch fence. Once the recreated assignment bucket can satisfy that check, recovery
exits — and in NP-1 the *whole coordination machinery is still running against the
empty buckets*, so it re-coordinates a genuinely-applied assignment and the workers
rest Stable.** Two converging sub-paths produce that satisfied gate:

1. **Leader re-publish at a monotonically-higher in-memory version (the dominant
   observed path).** The publisher's version counter `p.currentVersion` is an
   **in-memory field** (`internal/assignment/assignment_publisher.go:106`), and a fresh
   publish proposes `p.currentVersion + 1` (`:345`). The only seeding from KV,
   `DiscoverHighestVersion`, **only ever RAISES** the counter
   (`assignment_publisher.go:882-884` `if commit.Version > p.currentVersion`,
   `:917-919` `if highestVersion > p.currentVersion`) — it never lowers it. So after
   the wipe, the empty bucket yields `highestVersion=0`, the leader keeps its pre-wipe
   `currentVersion` (e.g. 2), and re-publishes version 3 — *higher* than anything in
   the empty bucket. The worker's `refreshAssignmentFromNATS` reads that v3, the
   monotonic gate accepts it (`monotonicStore` / `isApplyResultStale`,
   `manager_assignment.go:1162-1170,1443-1456`), the apply pipeline commits it, and
   `currentAssignmentApplied` is satisfied → `exitDegraded`. The NP-1 log corroborates:
   worker-0 (leader) shows `Stable->Rebalancing->Stable` right after the wipe — a real
   re-calculation+re-apply, not a no-op.
2. **Stale in-memory snapshot survives a dropped-stale refresh (the fallback path for
   non-leaders before a re-publish lands).** If `refreshAssignmentFromNATS` reads a
   lower/older version, `monotonicStore` drops it as stale and returns `nil` (the
   refresh still "succeeds"); `currentAssignmentApplied` then evaluates against the
   *unchanged pre-wipe in-memory snapshot*, which already matches `committedAssignment`
   → exit. The wipe is invisible either way.

So the false-healthy is **structural and version-monotonic**, not a counter-reset
artifact. NP-1 rests (vs NP-2 flapping) because in NP-1 the assignment bucket was
*also* wiped+recreated and the live machinery promptly re-populates it with a
genuinely-applied higher-version assignment, so after the last epoch tick observes the
recreated assignment-bucket Created (it too is in `bucketEpochs`) and re-degrades, the
*next* recovery exits and — once the in-memory `ep.created` mismatch no longer produces
a *new* enterDegraded within the observation window's tail — the worker is left Stable.
(NP-2 never settles because the heartbeat fence keeps a permanent mismatch against an
intact assignment bucket the recovery loop keeps reading.)

**Readiness / work consequence.** `StateStable` is a Ready signal
(`docs/OPERATIONS.md:387`: `/health/ready` returns ready for Stable). A readiness probe
marks the pod Ready. The worker then processes partitions backed by coordination state
that was wiped: handoff ownership claims (`parti-*-handoff` bucket — pull-gated
consumers depend on stable claims), the leader's commit log / audit history
(`parti-*-assignment` `_commit`/`_commit_log` keys), and the worker-ID lease
(`parti-*-stableid`). These are silently-broken invariants, not merely a stale number:
two workers can believe they own the same partition (handoff claims gone), the leader
audit cannot detect a behind worker (history gone), and worker-ID uniqueness is
unenforced until TTLs re-establish. This is exactly what the
`bucket-recreated`/"live data loss" contract exists to prevent by forcing rotation.

---

## (c) Opt-1 "latch once" — does it miss a second recreate? And: it does NOT fix the gap

**Critical correction to the report.** Opt-1 (re-capture `ep.created` after
`enterDegraded`) fixes *only* half (i), the re-arm. It leaves half (ii), the wrongful
exit, fully intact. Trace it against the proof invariants:

1. Epoch tick → `enterDegraded("bucket-recreated:hb")` → [opt-1: `ep.created = new`].
2. Recovery tick (conn never dropped): `refreshAssignmentFromNATS` succeeds (NP-2:
   assignment bucket intact) → `currentAssignmentApplied` true → `exitDegraded` →
   **Stable**.
3. Next epoch tick: `live == ep.created(new)` → no mismatch → no fire. Worker **rests
   Stable.**

Result: **NP-2** gets `degradedToStable = 1` (fails `require.Zero`,
`np2_..._test.go:204`) and `finalState = Stable` (fails `require.Equal(StateDegraded)`,
`:212`). **NP-1** each worker exits once → `healed = [0 1 2]` (fails `require.Empty`,
`np1_..._test.go:248`). So opt-1 alone **fails both proofs** — and is strictly *worse*
than the status quo: it removes the flap, which removes the only recurring Degraded
windows a readiness probe could sample to rotate the pod. The current flap at least
re-degrades; opt-1 produces a *terminal false-healthy Stable*. This squarely
contradicts `04-proof-findings.md:278` ("the proofs… are agnostic to which fix lands").

**Second-recreate re-arm question (part c, as asked):** *if* opt-1 were used as a
complement to opt-2 (see below), the re-arm correctly handles a second recreate,
*because the cached `ep.kv` handle transparently re-binds to the live stream* (proven
in (a) by NP-2 firing 10× through the stale handle). After latching `ep.created = new`,
the next `BucketStreamCreated(ep.kv)` reads the *current* live stream; a second
operator recreate produces yet another later Created, mismatches the latched value, and
re-fires. So detection re-arms automatically; no handle re-open is needed. The only
window missed is a recreate that lands *between* the latch write and the same tick —
negligible at OperationTimeout cadence. **But this is moot for closing the gap**: opt-1
is never standalone-correct here.

---

## (d) Opt-2 "refuse exit while mismatch outstanding" — how does it EVER exit?

It exits **only via process restart** — which is exactly the documented contract.

- Since half (i) means `ep.created` is *never* re-captured in-process, an epoch
  mismatch is **permanent** for the life of the process. Opt-2 therefore makes
  recovery refuse `exitDegraded` forever → **terminal Degraded** in-process. Both
  proofs pass: `degradedToStable = 0`, `finalState = Degraded` (NP-2), `healed = []`
  (NP-1).
- The legitimate exit is the restart path: a fresh `Start()` re-runs
  `ensureKVBucket → captureBucketEpoch` (`manager_setup.go:265`) and re-captures
  `ep.created` against the recreated stream, so the new process starts with a matching
  epoch and reaches Stable normally. This is `docs/OPERATIONS.md:760-762` ("Restart the
  workers. The restart path recreates buckets via `ensureKVBucket`… covered by
  `TestManager_Restart_AfterNATSBucketLoss`"). So "never exit in-process" is the
  *correct* behavior, not a defect.
- Does `ep.created` need re-syncing for a legitimately-reprovisioned bucket? Not
  in-process — re-sync happens at the next Start. Parti does **not** re-provision the
  bucket from the live publish path by design (`docs/OPERATIONS.md:750`); it only
  ensures buckets at Start (`ensureCoreKVBuckets`, `manager_setup.go:125-166`). So opt-2
  does not strand a legitimately-recreated bucket — the operator's runbook is "rotate
  the pod," and rotation re-syncs the epoch.

**Mechanism for opt-2.** Add an "epoch mismatch outstanding" predicate and AND it into
the exit gate. Cleanest surface: a `func (m *Manager) epochMismatchOutstanding() bool`
that re-probes each `m.bucketEpochs` entry (or reads a latched flag set by
`checkBucketEpochs` when it fires), then in `attemptRecoveryFromDegraded`
(`manager_degraded.go:~408-415`) gate `exitDegraded` on `!epochMismatchOutstanding()`
in addition to `currentAssignmentApplied`. A latched-flag variant
(`m.epochFenceTripped atomic.Bool`, set at `manager_setup.go:689`, never cleared
in-process) is simplest and race-free — it needs no extra KV probe on the recovery
tick. Recommended.

---

## (e) Is "bucket-recreated" the right response for NP-1 (all buckets gone) vs a restart/rotation?

Yes — `bucket-recreated:<bucket>` is the correct *reason*, and terminal Degraded → pod
rotation is the correct *response*, for NP-1 just as for NP-2. The documented taxonomy
(`docs/OPERATIONS.md:126`) classifies `bucket-recreated` as "ambiguous Parti-owned
data loss → Restart or rotate workers; inspect JetStream storage before trusting the
recreated bucket." NP-1 is precisely that, fleet-wide. The distinction the report draws
(NP-1 "more severe" because it's false-healthy) is about the *outcome under the bug*,
not about needing a different reason. A wipe+recreate is **not** a restart/rotation: in
a restart the worker re-runs the synchronous Start phase (re-claims worker ID, re-reads
committed assignment, re-captures epochs) before serving; in a live wipe the in-memory
state is stale and the coordination history is gone. So the fence is the right signal;
the bug is that recovery exits *despite* the signal. No new reason is needed; opt-2 (+
optionally opt-1 to quiet C3 spam) restores the intended terminal-Degraded behavior.

---

## Fix options — mechanism, surface, blast radius, contracts, residual risk

### Opt-2 (RECOMMENDED, load-bearing): gate the recovery exit on no outstanding epoch mismatch
- **Mechanism.** Latch a flag when `checkBucketEpochs` fires
  (`manager_setup.go:689`, e.g. `m.epochFenceTripped.Store(true)`), never cleared
  in-process. In `attemptRecoveryFromDegraded` (`manager_degraded.go:408-415`), AND
  `!m.epochFenceTripped.Load()` into the exit condition; on outstanding mismatch,
  stay Degraded (do not `scheduleApplyRetry` for an epoch trip — there is nothing to
  re-apply; just `return`).
- **Surface.** `manager_degraded.go:~408-415` (exit gate), `manager_setup.go:689`
  (set flag), one new atomic field in `manager.go`. ~10 lines.
- **Blast radius.** Touches the Degraded→Stable *exit* only; entry paths untouched.
- **Contracts.** C1 (whole-bucket-missing → bounded Degraded entry): unchanged — entry
  is via `recordKVError`/`enterDegraded`, not the exit gate. C2 (peer claim takeover):
  unrelated (different reason class, `onClaimerError`/`manager_election.go`). C3
  (OnDegraded once per entry): **improved** — terminal Degraded = one entry instead of
  N flap entries. C4 (Start returns after sanity phase): unrelated. Named tests:
  satisfies `TestNP2EpochFence...` and `TestNP1_LiveBucketRecreate...`; must not break
  `TestManager_F1_BucketRecreate_TripsDegraded` (entry-only, unaffected) or the
  recovery tests for the *non-epoch* Degraded reasons (the flag is only set by the
  epoch fence, so connection-down / kv-unavailable / startup-timeout recoveries still
  exit normally — verify `NP-5` blocked-apply recovery and `NP-3a` disarm control still
  pass).
- **Residual risk.** Low. One subtlety: if a future caller wants an epoch trip to be
  *recoverable* without restart (it currently is not, by design), the latch would have
  to be cleared by a re-capture path — but that re-capture path does not exist and
  building it is opt-1's job. Keep the latch un-clearable to match the documented
  restart contract. Also: ensure the flag is set even on the *first* fence fire
  (before any exit), so the very first recovery tick after the trip already refuses.

### Opt-1 (COMPLEMENT ONLY, never standalone): re-capture `ep.created` after enterDegraded
- **Mechanism.** After `enterDegraded` at `manager_setup.go:689`, set
  `ep.created = live; m.bucketEpochs[bucket] = ep` so the fence stops re-firing.
- **Surface.** `manager_setup.go:689-690` (~3 lines). Note the map write must respect
  the concurrency contract: `checkBucketEpochs` runs on the monitor goroutine only, so
  the write is single-writer, but `bucketEpochs` is read in the same goroutine — no
  lock needed *if* nothing else mutates it (currently true).
- **Blast radius.** Suppresses repeated OnDegraded spam (C3) on a sustained trip.
- **Contracts.** C3: improved (one entry, no re-fire). But **does not** restore
  terminal Degraded — see (c): standalone it *cements* false-healthy Stable and FAILS
  both proofs.
- **Residual risk.** HIGH if landed standalone (regresses the contract to terminal
  false-healthy). SAFE only layered on top of opt-2, where it merely quiets the log
  surface. Re-arm for a second recreate works (handle re-binds, (c)).
- **Recommendation.** Optional. Land opt-2 first; add opt-1 only if the repeated
  `bucket-recreated` OnDegraded entries (one per OperationTimeout tick while Degraded)
  are deemed too noisy. With opt-2's latch, the flap is already gone (no
  exit → `enterDegraded`'s CAS rejects re-entry), so the repeated-entry spam opt-1
  targets **does not even occur** — `enterDegraded` short-circuits on the still-set
  `degradedSince`. So **opt-1 is very likely unnecessary once opt-2 lands.** Verify:
  with opt-2, after the first trip the worker never exits, so `degradedSince` stays
  set, so `enterDegraded` at `:689` no-ops every subsequent tick → no OnDegraded spam.
  This means **opt-2 alone is sufficient and complete.**

### Opt-3 (alternative framing, broader): unify the exit gate for Families A and B
- **Mechanism.** Both A and B are the same `attemptRecoveryFromDegraded` wrongful-exit
  defect; the report (`04-proof-findings.md:48-52,280-282`) proposes a single exit-gate
  fix. But the *conditions differ*: A needs "no epoch mismatch outstanding"; B (NP-3b)
  needs "the failing op (heartbeat/election/stableid) actually recovered," not just the
  assignment read. A unified gate must **AND both predicates** (plus the existing
  `currentAssignmentApplied`).
- **Surface.** Same function `manager_degraded.go:408-415`, plus B's per-source
  recovery-verification machinery (out of scope here; see `04-fd1-flapping-decision.md`).
- **Blast radius / risk.** Larger; A and B must be **co-designed**, not landed
  independently, or one fix's predicate silently overrides the other's intent.
- **Recommendation.** Land opt-2 (A's epoch predicate) and B's per-source predicate as
  two ANDed clauses in one coordinated change. Do not let "one fix closes both" (report)
  be read as "one *predicate* closes both."

---

## Cross-family interactions

- **Family B (NP-3b)** shares the exact function (`attemptRecoveryFromDegraded`) and the
  exact defect class (exit on wrong signal). The report's "a single fix to the exit gate
  could close both" (`04-proof-findings.md:51-52`) is **half-right**: same function,
  *different predicates* (A: epoch-mismatch; B: failing-op-recovered). They must be ANDed
  and co-designed (opt-3). Verified: A's epoch fence calls `enterDegraded` directly and
  never touches `kvErrorWindow`, while B lives entirely in the `kvErrorWindow`/threshold
  path — so the two predicates are independent and composable.
- **C1 contract** (whole-bucket-missing → bounded Degraded entry): NP-1's wipe *does*
  also trip the threshold path first — the NP-1 log shows `kv-unavailable` as the first
  OnDegraded reason on every worker, *then* `bucket-recreated:*`. So C1's entry still
  fires correctly; the gap is purely on the *exit* side. Opt-2 leaves C1 entry intact.
- **Family C (NP-8)** is distinct (claim-loss self-stop + MemoryStorage heartbeat loss);
  no shared surface with A's exit gate.

---

## Discrepancies with the report (`04-proof-findings.md`)

1. **`:278` "the proofs… are agnostic to which fix lands" — FALSE.** Opt-1 alone fails
   both NP-2 and NP-1 and is strictly worse (terminal false-healthy vs flap). Only opt-2
   passes. The proofs are *not* fix-agnostic; they specifically reward terminal Degraded.
2. **`:275-282` presents opt-1 and opt-2 as co-equal alternatives ("Either… or…").**
   They are not alternatives: opt-2 is necessary and sufficient; opt-1 is at best a
   redundant complement (and likely unnecessary once opt-2 lands, since opt-2's
   never-exit keeps `degradedSince` set, so `enterDegraded` self-suppresses the repeat
   entries opt-1 targets).
3. **`:51-52,280-282` "a single fix to the exit gate could close both A and B."**
   Imprecise: same *function*, different *predicates* that must be ANDed and
   co-designed. Not a single drop-in.
4. **Report does not explain NP-1's *resting* (vs flapping) Stable, nor the
   version-monotonicity mechanism.** Added in (b): in-memory `currentVersion` only ever
   rises (`assignment_publisher.go:882-884,917-919`), so the leader re-publishes a
   higher version into the empty bucket and the worker genuinely re-applies it. The
   false-healthy is structural and version-independent, not a counter-reset artifact.
5. **Line cites all correct** — `manager_setup.go:684-690`, `:627`,
   `manager_degraded.go:376-416`, `:97-134`, `manager_assignment.go:1561-1568`,
   `:409-415` all verified on HEAD. No stale line numbers found.

## Confidence

High. Both halves verified directly in code with cites; the opt-1-fails-both-proofs
correction is derived by tracing the exact proof invariants
(`np2_..._test.go:204,212`, `np1_..._test.go:248`) against the unchanged exit gate; the
version-monotonicity sub-path is confirmed at `assignment_publisher.go:345,882-884,
917-919`. The empirical FAILs (`tmp/repro-current-head/np1.out`, `np2.out`) corroborate.
The one inference (opt-2 self-suppresses opt-1's target spam) is a direct consequence of
`enterDegraded`'s `degradedSince` CAS (`manager_degraded.go:309`) plus opt-2's
never-exit — strong, but worth a one-line confirmation in the eventual fix's test.
