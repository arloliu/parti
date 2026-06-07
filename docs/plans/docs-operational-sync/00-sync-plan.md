# Operational Docs Sync Plan

**Date:** 2026-06-07
**Scope:** `docs/` operational guides — `CONFIGURATION.md`, `OPERATIONS.md`,
`CONSUMERS.md`, `USER_GUIDE.md`, `ARCHITECTURE.md`, `KUBERNETES.md`.
**Driver:** Reconcile the operational docs with the verified state of `main`
and the three investigation campaigns:
- `docs/plans/iops-investigation/` (IOPS attribution, KV-storage findings)
- `docs/plans/perf-measurement/` (Dynamic-consumer cost + latency model, to N=10k)
- `docs/plans/partition-scaling/` (NATS `partition()` + `Dynamic` over fixed K)

This is a **plan only**. No edits applied yet. Each fix below cites the source
of truth it was verified against.

---

## The one correctness trap (read first)

The word **`MemoryStorage`** points in **two opposite directions**. The sync
must keep them separate everywhere, loudly:

| Subject | Correct storage | Why |
|---|---|---|
| **Parti coordination KV buckets** (stableid, election, heartbeat, assignment, handoff) | **`FileStorage`** | Memory-KV was *falsified* as an IOPS mitigation (M1.9: ~1–2% savings, within noise). Worse, a `MemoryStorage` heartbeat bucket is **lost on a single-node restart and flaps the fleet** `Degraded`↔`Stable` (v2.6.0 fix). All five buckets are `FileStorage` in code (`manager_setup.go:92,158,162,166,182`). |
| **Per-consumer JetStream state** (the consumer's own ack/cursor store) | **`MemoryStorage` — but only when the decision tree says so** | Per-consumer state files are **72–81% of cluster write IOPS** (iops-investigation §4). Moving consumer state to memory is the real lever — but it is **conditional**, not a blanket default (see B2). |

A reader must never come away thinking "memory for the buckets too." Every
edit that mentions consumer `MemoryStorage` states "consumer state, *not* the
parti KV buckets."

**Second trap — do not flatten the recommendation.** The findings are a
**decision tree**, not "always set `WithConsumerMemoryStorage(true)`." The
default file-backed config is correct for most deployments. The plan's authored
content (Part B) is anchored to the Q1→Q2→Q3 tree below, never to a flat directive.

```
Q1. Is disk-IOPS pressure actually high?
    (≥10k partitions per NATS pod, or noisy-neighbour latency tail,
     or provisioned-IOPS billing, or a constrained dev/test cluster)
    NO  → stop. Default (file-backed consumer state, R=3) is fine.
    YES → continue.
Q2. Can the workload tolerate redelivery ONLY on a coordinated cluster-wide restart?
    YES → WithConsumerMemoryStorage(true), Replicas inherited (R≥3).   [M2.A]
          ~90% IOPS cut at N=1000, ~72% at N=3000. Keeps consumer HA. ← recommended when Q1=YES
    NO  → continue.
Q3. Idempotent handler that tolerates redelivery on single-node failure too?
    YES → WithConsumerMemoryStorage(true) + WithConsumerReplicas(1).   [M2.B]
          ~99% cut; per-partition cost collapses to flat-in-N.
    NO  → keep file-backed state, or redesign (consumer.Queue / fixed-K).
```

---

## Part A — STALE fixes (must-fix, low risk, source-verified)

### A1. `CONFIGURATION.md` "Bucket Purposes and Storage Type" table — WRONG storage column
- **Lines ~185–196.** Table lists `ElectionBucket` and `HeartbeatBucket` as
  **`Memory`**. Source: all five buckets are `FileStorage`
  (`manager_setup.go:92,158,162,166,182`); v2.6.0 CHANGELOG documents the election +
  heartbeat switch.
- **Fix:** set the `Default Storage` column to **`File`** for every bucket.

### A2. `CONFIGURATION.md:195-197` rationale paragraph — encodes the *old* memory-KV reasoning + a wrong migration command
- Current text (line 195): *"Heartbeat and election buckets use `MemoryStorage`
  to minimize PVC IOPS … intrinsically ephemeral …"* — false on `main`.
- Current text (line 197): the manual-migration hint says `nats kv del <bucket>`.
  **Wrong command** — `nats kv del` deletes a *key*; bucket removal is
  `nats kv rm` (the OPERATIONS runbook uses `nats kv rm` at `OPERATIONS.md:633,689`).
  *[codex round-1 catch, verified]*
- **Fix:** rewrite the rationale to: all five coordination buckets use
  `FileStorage`; election/heartbeat were switched from Memory in v2.5.0/v2.6.0
  because a single-node restart lost a memory stream and flapped the fleet; the
  added write IOPS is a flat, partition-count-independent term (memory-KV was
  measured to save only ~1–2%, so the durability win dominates). Point to the
  OPERATIONS migration sections, and correct the command to `nats kv rm`.

### A3. `CONFIGURATION.md:132` heartbeat-interval prose — OPTIONAL reword (NOT a contradiction)
- *"Defaults are tuned for low IOPS on file-backed JetStream clusters."*
  *[codex round-1: this is still TRUE — heartbeat writes hit file-backed KV, so
  slower interval = less write load regardless of storage type. Downgraded from
  must-fix STALE to optional clarity reword.]*
- **Fix (optional):** keep the interval/TTL guidance as-is; only touch if a
  reader could misread it as implying memory/ephemeral heartbeat storage.

### A4. NATS version floor — `USER_GUIDE.md:95`, `OPERATIONS.md:31` (ADDITIVE, not a verified stale-fix)
- Both say **"2.10.0+"**. *[codex round-1: do NOT claim this is source-verified.
  go.mod pinning server `v2.14.1` is the embedded test-server version, not proof
  CI validates 2.10.0. Leave the documented `2.10.0+` minimum untouched.]*
- **Fix (additive only):** keep `2.10.0+` as the documented minimum, add
  "**≥2.12 recommended** for large fleets — async metacontroller snapshots keep
  snapshot cost off the critical path (≤~30 ms async at 10k consumers)." Do not
  assert 2.10 is tested.

### A6. `OPERATIONS.md:248` Docker Compose pins `nats:2.10-alpine` *[codex round-1 catch]*
- If A4 adds "≥2.12 recommended," this example contradicts it.
- **Fix:** bump to a current tag (e.g. `nats:2.14-alpine`) OR add a one-line
  "minimum-version local example; see version guidance above" note.

### A7. SOURCE follow-up — `manager_setup.go:457-470` `warnOnStorageMismatch` is inverted *[codex round-1 catch; NOT a docs edit]*
- The comment and the operator-facing `Warn` log both say *"even after parti's
  defaults switched to **memory** storage / IOPS reduction from the
  memory-storage default is NOT active"* and emit `nats kv del <bucket>`. Both
  are wrong on `main`: defaults are **FileStorage**, and the command should be
  `nats kv rm`. Same defect as A2.
- **Fix:** a small SOURCE change (comment + log string). **DECISION (user,
  2026-06-07): include as a paired follow-up commit** after the docs land —
  reword the comment + `Warn` string (memory→file framing) and correct
  `nats kv del` → `nats kv rm`. Touches `manager/` surface → triggers the
  `make pre-pr` gate (lint + `make test -race` + `make test-integration`).

### A5 — DROPPED (user, 2026-06-07): leave `ARCHITECTURE.md` KV table as-is.

---

## Part B — MISSING additions (authoring; anchored to the decision tree)

### B1. `CONSUMERS.md` option tables — add the two shipped options + fix the "all options" framing
- **Lines ~672–697.** `WithConsumerMemoryStorage` and `WithConsumerReplicas`
  exist on Dynamic/Queue/Static/Broadcast (`consumer/options.go:597,639`;
  `consumer/common.go:102-144`) but appear in **no** options table.
- **Fix:** add both to the common options table, with one-line descriptions
  that flag `WithConsumerMemoryStorage` as **not live-editable** and
  `WithConsumerReplicas` as **live-editable** (per the Godoc).
- *[codex round-1 catch]* the tables are introduced at `CONSUMERS.md:660` as
  "All consumers accept functional options" but are already **non-exhaustive**
  (source also has common `WithMaxWaiting`, `WithAckPolicy` at
  `consumer/options.go:371-403`, plus Static/Dynamic options at `:719-835`).
  **Fix:** relabel the tables as "**Selected / commonly-used options**" and add
  a pointer to `API_REFERENCE.md` / Godoc for the complete list — rather than
  silently implying the storage options are the only addition. Full
  option-table exhaustiveness is a larger doc-sync pass, out of scope here.

### B2. `CONSUMERS.md` — new "Tuning consumer storage for scale" subsection
- Author the **decision tree** (Q1→Q2→Q3 above) verbatim-in-spirit, with the
  measured numbers (M2.A ~90%/72%, M2.B ~99% flat-in-N) and the redelivery
  trade-offs per branch. **Open with the disambiguation callout** (consumer
  state ≠ parti KV buckets). Do **not** write "recommended = always memory."
- *[codex round-1]* **Wording guard:** do NOT carry the perf report's phrase
  "production default" for memory+R3 (it's the report's internal label,
  `03-findings-production-mem-r3.md:8-16`). Operator-facing text says
  "**recommended once the decision tree reaches Q2**," never "default."

### B7. `CONSUMERS.md` — new "Stream Retention Policy" section (consumer-type × policy)
*[Added 2026-06-07 after a dedicated codex judgment pass — see consensus log
Round 3. Stream retention policy is the hardest JetStream concept and the
per-type guidance is currently scattered (Broadcast callout @245, WorkQueue
recovery restriction @416). Consolidate into one matrix + a two-axis decision.]*

- **Placement:** new `## Stream Retention Policy` section after Overview (or
  after Consumer Types), plus a one-line "Recommended retention: …" in each
  type's subsection linking to it. **Cross-link, do NOT duplicate** the existing
  Broadcast callout (245) and WorkQueuePolicy Restriction (416) — a second
  drifting copy is worse than none.
- **The two-axis framing (lead with this, don't flatten to per-type rules):**
  (1) must messages survive after processing — replay / multiple readers /
  no-consumer windows? → `LimitsPolicy`. (2) dedicated consume-once queue,
  single non-overlapping consumer set, restricted recovery acceptable? →
  `WorkQueuePolicy`. Interest is a narrow third option.
- **Reconciled matrix (codex-judged, source-verified):**

  | Consumer | Recommended | Acceptable (caveats) | Avoid / Forbidden |
  |---|---|---|---|
  | `Queue` | **WorkQueuePolicy** — dedicated consume-once queue (shared durable ⇒ delete-on-ack, auto-trim) | **LimitsPolicy** — shared stream, replay, or `RecoverFromNew` needed | Interest (no-cover publishes lost) |
  | `Static` | **LimitsPolicy** (retain/replay) **or WorkQueuePolicy** (non-overlapping consume-once) | Interest — only with full partition coverage pre-created | — |
  | `Dynamic` | **LimitsPolicy** — recommended; preserves all recovery strategies and replay; the partition-scaling scaling guide assumes it | WorkQueue — **viable, now proven for BOTH paths**: graceful join+leave (`TestDynamic_OnWorkQueueStream`, 876/876, 0 overlap) and 3-node RF=3 abrupt crash (`TestDynamic_OnWorkQueueStream_ClusterCrash`, 1166/1166, 0 overlap), `-race` clean. The remaining trade is real but bounded: recovery limited to `Beginning`/`Disabled` (`RecoverFromNew`/`LastProcessed` banned) + delete-on-ack (no replay) + single consumer set. Choose it for auto-trim/bounded-storage; choose Limits for recovery flexibility | Interest — no-cover discard on unassigned/GC'd windows |
  | `Broadcast` | **LimitsPolicy** — every instance gets every message; no churn-pinning | InterestPolicy — only if stable instance IDs, all recipients pre-created, low churn / short `InactiveThreshold`, no-recipient discard acceptable | **WorkQueuePolicy** — single delivery defeats fan-out (hard) |

- **Cross-cutting notes that MUST accompany the table:**
  1. **WorkQueue recovery cost:** consumers limited to `DeliverAllPolicy` ⇒ only
     `RecoverFromBeginning`/`RecoveryDisabled`; `RecoverFromNew`/`LastProcessed`
     rejected (already in the WorkQueuePolicy Restriction section — link it).
  2. **Replicas on Interest/WorkQueue:** nonzero consumer replicas must **equal**
     stream replicas (NATS rule, `consumer/options.go:621-629`). So on a typical
     RF3 stream `WithConsumerReplicas(1)` is rejected — the M2.B IOPS lever (B2)
     is unavailable; use M2.A (`WithConsumerMemoryStorage(true)` + inherited
     replicas) on those policies.
  3. **Interest ghost-durable pinning (source-verified in nats-server):** a
     stopped consumer's durable keeps interest until the durable is *deleted*;
     parti's `Stop()` doesn't delete it (GC after `InactiveThreshold`, 24h
     default), so Interest pins un-acked messages behind churned instances —
     the decisive reason Broadcast defaults to Limits, not Interest.
- **Overstatement guard:** Dynamic recommendation is firm `LimitsPolicy` but
  NOT "forbidden" for WorkQueue. WorkQueue is empirically VIABLE for Dynamic
  (both graceful + crash proven); the recommendation rests on the recovery
  trade-off, not impossibility. The doc must say "Limits recommended (keeps
  recovery + replay); WorkQueue is a valid consume-once alternative if you
  accept Beginning/Disabled-only recovery" — NOT "WorkQueue may not work." Only
  Broadcast gets a hard "WorkQueue forbidden."
- **Empirical proof artifact:** `test/integration/fixedpartitions/workqueue_dynamic_test.go`
  — two tests, both `WorkQueuePolicy`, both capturing `OnError` to assert no
  `10100`: `TestDynamic_OnWorkQueueStream` (Exp11 scenario 1, single-node
  join+leave → 876/876, 0 overlap) and `TestDynamic_OnWorkQueueStream_ClusterCrash`
  (Exp11 scenario 2, 3-node RF=3 + R=3 consumers, abrupt crash → 1166/1166, 0
  overlap). `go vet` clean; both `-race` clean together (45.8s). Keep in-tree as
  the WorkQueue-viability record. NOTE: consumer `Replicas` must equal stream
  replicas on WorkQueue (R=3 here), so the crash test uses
  `WithConsumerMemoryStorage(true)`+`WithConsumerReplicas(3)` — corroborates
  cross-cutting note #2.

### B3. Recommended pattern for partitioning at scale — NATS `partition()` + `Dynamic` over fixed K
- **DECISION (user, 2026-06-07): promote into a real docs page** — proposed
  `docs/SCALING.md` (name to confirm at review). Lift
  `docs/plans/partition-scaling/02-guide-nats-partition-dynamic.md` into `docs/`
  as a first-class page.
- The recommended posture to carry over: `consumer.Dynamic` + memory consumer
  state + R=3 scales cleanly to **N=10,000**; reach for the fixed-K
  `partition()` pattern only when ≥10k partitions/cluster strain per-consumer
  RSS or the metacontroller snapshot.
- **Trim on promotion (do not imply shipped features):** the source guide and
  feasibility doc discuss speculative `consumer.Grouped` / `consumer.Pooled`
  types that **do not ship** (assessment-only). The promoted page documents
  only what ships today — NATS `partition()` (or a client hash) + the existing
  `consumer.Dynamic` over a fixed K of numbered subjects — and explicitly notes
  the fixed-K *types* are not built and should not be built speculatively.
- Cross-link the new page from `CONSUMERS.md` and `OPERATIONS.md` (B4).
- Carry the verified delivery contract: at-least-once + per-partition order, but
  only if a partition never changes slots after first delivery; one stuck key
  HoL-blocks its whole slot; `Retention: LimitsPolicy` (not Interest/WorkQueue);
  K is pick-once (over-provision, e.g. K=256). Proven by `poc/` Exp1–10 +
  `test/integration/fixedpartitions/` Exp11/12.
- *[codex round-1]* **Overstatement guard on promotion:** the source guide's
  examples set `WithConsumerMemoryStorage(true)` and call it the perf study's
  recommended config (`02-guide-…:101-110,144-147`). On promotion, add a
  sentence near the first example: *this assumes the deployment already
  satisfies the IOPS/scale decision tree (B2); default file-backed consumer
  state remains fine otherwise.* Do not present memory state as unconditional.

### B4. `OPERATIONS.md` — new NATS-side capacity/cost model (separate axis)
- The existing "Resource Estimates" table (~929–933, per-**worker-pod** RSS
  50–200 MB) answers a *different* question than the perf model (NATS-**server**
  cost per partition). **Add, do not overwrite.**
- New content (perf-measurement, production config = memory consumer state + R=3,
  validated to N=10,000):
  - **RSS is the binding constraint:** ~**0.793 MiB cluster RSS per partition**
    (+ ~90 MiB baseline). IOPS and CPU never wall on modern NVMe/gp3.
  - **Latency is flat ~1.3 ms P95/P99, independent of N** to 5000+ (no
    per-partition fetch tax vs the JetStream floor).
  - Worked point: K=256 / N=5000 fixed-K ≈ ~300 MiB cluster RSS, ~12 IOPS,
    sub-ms metacontroller snapshot.
  - **Cluster sizing:** ≥**3 nodes, R=3** for consumer/KV HA (the validated
    production posture). RF=5 only if message-stream durability needs 5-way.
    (The perf rig's 5-node/RF=5 was a test-isolation artifact, not a
    recommendation.)

### B5. `OPERATIONS.md` worker-count guidelines table (~915–925) — caps at "256+"
- Reads as if 256 partitions is "large." The library is validated to **10,000**.
- **Fix:** add a row/note distinguishing **worker count** (the table's axis)
  from **partition count** (validated to 10k; see B4). Don't conflate.

### B6. `OPERATIONS.md` — metacontroller note for large fleets
- New short note: on NATS **≥2.12** snapshots are async (~30 ms background at
  10k consumers, 20–40× below the pre-async 1.286 s incident). **Do not lower
  `meta_compact_size`** (inert below the 8 MB floor) and **do not set
  `JetStreamMetaCompactSync`** (forces blocking snapshots). There is no knob to
  turn at ≤10k on ≥2.12.

---

## Part C — Out of scope / already synced (do NOT touch)
- `OPERATIONS.md` Election + Heartbeat **Bucket Storage Migration** sections
  (@613, @661) — already correct and complete.
- `PROVISION.md:748-751` — already references `WithConsumerMemoryStorage`.
- `docs/plans/**` and `docs/design/**` — internal; not doc-sync targets.

---

## Execution order (once scope confirmed)
1. **Part A** first (mechanical STALE fixes, lowest risk) — A1, A2, A3, A4 (A5 optional).
2. **Part B** authored content — B1, B2 (CONSUMERS), B4, B5, B6 (OPERATIONS),
   then B3 (the link/promote decision).
3. Re-grep `docs/*.md` for `MemoryStorage`/`low.*IOPS`/`ephemeral` to confirm no
   stale sibling survived (per global-grep discipline).
4. `/post-impl-review` is overkill for docs; instead a final read-through +
   `make lint` (no Go changed, but cheap) before commit.

## Cross-model consensus log
- **Round 1 (codex, read-only, 2026-06-07):** independently verified the core
  findings from source with tighter cites — AGREE on all of: 5 buckets =
  FileStorage (`CONFIGURATION.md:187-195` stale); both consumer options ship +
  live-edit semantics; decision tree is conditional; Grouped/Pooled don't ship.
  Refinements ACCEPTED into the plan: A2 command fix (`kv del`→`kv rm`), A3
  downgraded (not a contradiction), A4 reframed as additive (2.10 not
  source-verified), A6 (Docker tag), A7 (source `warnOnStorageMismatch` is
  inverted), B1 table-framing, B2/B3 "production default" wording guards. No
  evidence-backed disagreement remained → substance consensus reached round 1.
  Round 2 = confirm the amended plan.
- **Round 2 (codex, read-only, 2026-06-07):** CONFIRMED A2/A3/A4/A6/A7/B1/B2/B3
  faithfully incorporated; both `MemoryStorage` directions preserved; **no
  execution blockers — "ready to execute as-is."** One non-blocking cite nit
  (five-buckets range) fixed. **Consensus reached — plan is execution-ready,
  pending the user's final go-ahead.**
- **Round 3 (codex retention-policy judgment, read-only, 2026-06-07):**
  independent judge on consumer-type × retention-policy → produced B7. AGREE:
  Queue→WorkQueue(dedicated), Static→WorkQueue(non-overlap), Broadcast→Limits
  (ghost-durable pinning **confirmed in nats-server source**:
  `stream.go:8771,8777`, `consumer.go:3822,6657,6666,6718`). CORRECTED 3 of my
  claims, all accepted: (1) Dynamic WorkQueue is *not forbidden* (single-filter
  rebind, no overlap) — softened to "firm Limits recommendation, WorkQueue
  unproven+recovery-cost, not forbidden"; (2) `Replicas=1` rejection rule is
  "must equal stream replicas" (RF3 rejects 1, RF1 accepts); (3) Queue
  `DeliverAll` is JetStream's default, not parti-set. No unresolved disagreement
  → consensus. B7 matrix is the reconciled output.

## Resolved scope (user, 2026-06-07)
- **Depth:** Part A + Part B (full sync).
- **B3:** promote the fixed-K guide into a real `docs/` page (proposed
  `docs/SCALING.md`, name to confirm), trimming the unshipped `Grouped`/`Pooled`
  design.
- **Status:** plan handed back for review — **no doc edits applied yet**, awaiting
  go-ahead.

## Still to confirm at review
- Final name for the promoted scaling page (`docs/SCALING.md`?).
- Whether `ARCHITECTURE.md` A5 (add a storage note to the KV table) is in or out.
