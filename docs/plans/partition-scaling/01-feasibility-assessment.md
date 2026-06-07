# Partition Scaling — Feasibility & Recommendation

## TL;DR — recommendation

**Don't build a new consumer type. For bounded-cost, lossless work at scale,
configure parti's *existing* `consumer.Dynamic` over a *fixed* set of K numbered
partition-subjects, and let NATS server-side `partition()` (or a client hash) route
keys into them.** This is "be Kafka": pick a partition count K, hash keys into K,
consume K — using zero new parti code. See the step-by-step
[**guide**](02-guide-nats-partition-dynamic.md).

- **Proven end-to-end** (public API only, no new type): real `Manager` +
  `source.NewStatic(K)` + `Dynamic` over `work.{{.PartitionID}}` delivered **every**
  message across worker **join, graceful leave, AND an abrupt crash** — single-node
  *and* on a 3-node cluster (RF=3 stream + R=3 consumers): 0 lost, `-race` clean
  (Exp11, `test/integration/fixedpartitions/`). NATS `partition()` → JetStream capture
  is also verified (Exp10).
- **It is a trade, not a free win.** You accept **per-slot** (not per-key)
  granularity: per-slot ordering, head-of-line blocking, and cache locality; K is
  the dial. This is exactly Kafka's per-partition trade. `Dynamic`-per-entity
  (K=N) keeps per-key isolation but costs N consumers; the perf study shows that
  already scales cleanly to **N=10,000**, so **for most deployments plain `Dynamic`
  + memory consumer state + R=3 remains the answer.**
- **K is pick-once.** Changing K is a disruptive modulo reshuffle (NATS
  `partition()` = `fnv32a%K`); over-provision K up front (e.g. K=256). Graceful
  runtime-K needs a client-side consistent-hash partitioner instead.
- **Three designs** (§3): `Dynamic` (K=N, lossless + per-key cache, expensive) /
  `Grouped` (decoupled fixed-K multi-filter, lossless-on-churn, *only* needed when
  ingest is fixed and you can't re-partition) / `Pooled` (M-per-worker, lossy/dup
  on migration, cheapest, idempotent workloads only). The recommended
  `partition()` + `Dynamic`-over-K path realizes the `Grouped` *goal* with **no new
  code** whenever you control ingest.

*Use it when:* ≳10k partitions/cluster strain per-consumer RSS or the metacontroller
snapshot, OR you want a Kafka-style fixed partition count. *Avoid it when:* the
workload needs per-partition cache locality or per-key isolation — then `Dynamic`
(K=N) is strictly better.

---

**Status:** assessment complete, design/implementation NOT started (2026-06-06); not
committed to build. **Verdict:** architecturally feasible in the decoupled fixed-K
form, but a genuine isolation↔cost trade — not a free improvement over
`consumer.Dynamic`, and not worth building until a deployment is demonstrably past
Dynamic's comfort zone.

> **"Virtual partition" =** a *fixed* pool of **K** JetStream durables, each serving
> several of **N** partition subjects via NATS 2.10+ multi-`FilterSubjects`, so
> **K ≪ N**. The user's framing fixes K = M·W (M consumers × W workers). This doc
> shows that M·W framing is the *wrong* way to fix K (lossy on routine churn, §3)
> and the viable design fixes K **independently of W**. This is the codebase's
> parked **FINDING-A / "dynamic-consumer-collapse"** idea, motivated by the IOPS
> study (per-consumer state ≈80% of data-plane cost) and the ~10k-consumer
> metacontroller-snapshot warning. Narrow to the `Dynamic` geometry (`Queue` already
> shares one durable; `Broadcast`/`Static` aren't N-per-partition).

---

## 1. The one idea everything follows from

A JetStream consumer has **one delivery cursor / ack-floor shared across all its
`FilterSubjects`.** Every result below is a corollary. We pinned it empirically
(POC under [`poc/`](poc/), 10 experiments against embedded `nats-server v2.14.1`,
all PASS):

| # | Experiment | Result | Corollary |
|---|---|---|---|
| **1** | add a backlogged subject to a consumer whose cursor advanced past it | backlog **skipped** (5 lost, 3 new delivered) | **mutating a live filter set is lossy for below-cursor backlog** |
| **2** | a new owner rebinds the **same** durable by name | **all 8 uncommitted msgs delivered, zero loss** | **rebinding an unchanged filter set is lossless** |
| **3** | one subject stops acking, `MaxAckPending=5` | co-located subject gets **0** until acks free the budget | **`MaxAckPending` is shared → one stuck partition starves its whole slot** |
| **4** | `UpdateConsumer` adds a subject to a *live* consumer | no recreation, no error, **cursor preserved**, new subject flows | **filter update is seamless — and that is exactly why Exp1 loses** |
| **5** | remove a stuck subject from the filter (Limits vs Interest) | Limits: msgs **keep redelivering**; Interest: **stranded/deleted** | **filtering away a stuck partition is not an escape hatch (Limits)** |
| **6** | `WorkQueuePolicy` overlapping / add-before-remove filters | rejected `err_code=10100` | **WorkQueue forces disjoint slots + remove-before-add moves** |
| **7** | `InterestPolicy` publish with no covering filter | msgs **discarded at publish time** | **no-owner window loses data on Interest/WorkQueue** |

The headline: **loss is a property of *mutating a live filter set* (Exp1), not
of multi-subject consumers per se (Exp2).** The whole design question is
therefore: *how often does the filter set of a live slot have to change?*

---

## 2. K is a continuous isolation↔cost dial

parti's existing consumer types are the two endpoints; the virtual partition is
everything in between:

```
   consumer.Queue            virtual partition            consumer.Dynamic
       K = 1        ◀────────── choose K ──────────▶          K = N
  1 shared durable        K durables, N/K subjects        N durables, 1 each
  cheapest, flat-in-N      cost ∝ K, blast radius ∝ N/K    most expensive
  NO per-partition         per-SLOT ownership              per-PARTITION ownership
  ownership                                                 (lossless handoff)
```

So "virtual partition" is not a new mechanism so much as **the freedom to pick K
between 1 and N.** This directly serves the stated goal ("balance isolation vs
scalability"): K is the balance point. The cost question (§7) and the isolation
question (§6 head-of-line) pull K in opposite directions; that tension *is* the
design.

---

## 3. The central fork: W-coupled (lossy) vs decoupled (viable)

There are two mappings, and **which one moves on worker churn decides
everything**:

```
   real partition (subject)  ──①──▶  slot / virtual partition  ──②──▶  worker
        N of them              hash         K of them            assigned       W of them
```

- **①  partition → slot** — the slot's *identity* (= its `FilterSubjects`).
- **②  slot → worker** — who currently pulls the slot durable.

| Variant | What fixes K | ① on worker churn | Result |
|---|---|---|---|
| **W-coupled** (the literal M×W) | K = M·W tracks **worker count** | ① is derived from ②, so **① changes every churn** → filter mutation | **Exp1 loss on every autoscale/failover** |
| **Decoupled** (fixed K) | K fixed **independent of W** | ① is a stable hash, untouched by churn; only ② moves | **Exp2 lossless failover** (rebind, identical to Dynamic) — filter mutation only on rare planned re-slotting |

The user's M·W proposal is the **W-coupled** one, and it is the trap: a partition
rebalanced to a new worker has its subject **added to a consumer whose cursor
already advanced** → Exp1. **Only the decoupled fixed-K design is viable** for a
lossless contract. Everything below assumes decoupled unless stated.

### Why the user's "but workers still get reassigned consumers when W changes" worry is answered

Correct observation: with fixed K, slots **do** still move between workers when W
changes, and a worker's set of slots changes. **But moving a *slot* between
workers is a lossless durable rebind (Exp2), because the slot's filter set is
unchanged** — it is the *identical* mechanism `Dynamic` uses to move a partition
between workers today (`internal/durable/worker_consumer.go:22-24`: durables are
never deleted on Close; the new owner rebinds by name and resumes from the
server-side ack floor, `partition_consumer.go:189-191`). The lossy operation
(Exp1) is mutating a slot's *membership* (①), which fixed-K confines to rare,
planned events. So: **slots-move-between-workers is fine; subjects-move-between-slots
is the dangerous one.**

### Worker scaling (HPA) and cache locality — the deepest face of the trade

The assignment unit today is the **real partition**, and parti's strategies are
built for **cache affinity**: `ConsistentHash` *"achieves >80% cache affinity during
rebalancing"*, `WeightedConsistentHash` *"maximizes cache affinity during scaling
events"* (`strategy/doc.go:14,21`). On an HPA event the K slots redistribute across
the new worker count. The interaction is nuanced — not a clean break, but a real
degradation:

- **Aggregate churn is ~comparable, not worse.** Reassigning K slots is the *same*
  consistent-hash over K units instead of N; it moves a similar *fraction*, so
  aggregate cache invalidation ≈ Dynamic's for balanced slots. An expectation, not a
  guarantee — `WeightedConsistentHash` can move normal partitions under load caps
  (`weighted_consistent_hash.go:192-220`), and slot-granular movement is lumpier
  (higher variance). "Fixed K makes scaling churn dramatically worse" is unfounded.
- **What genuinely degrades is granularity, not volume:** cache affinity drops from
  per-partition to **per-slot** (a slot move bulk-invalidates all N/K co-located
  partitions); rebalancing is **lumpy** (smallest move = a whole slot, can
  overshoot); and there is **no per-partition pinning** (the ① hash fixes which
  partitions share fate).

**The pigeonhole, in cache terms:** Dynamic's per-partition durable *is* its
per-partition cache-affinity unit, so collapsing to K consumers collapses affinity
to the slot — you cannot have **per-partition cache affinity + fixed-K shared
consumers + losslessness** at once. The only design that keeps per-partition
stickiness is the lossy one (the W-coupled `Pooled` below). So decoupled fixed-K is
a **poor fit for per-partition-cache-heavy workloads** unless the cache is re-keyed
per slot.

### The refined W-coupled design — M consumers per worker ("Pooled")

A better W-coupled design: keep **leader→worker assignment exactly as today**
(per-partition `ConsistentHash`, unchanged) and have **each worker bucket its own
~N/W partitions into M local multi-filter durables**. Total = **M·W, independent of
N** (N=3000, M=10, W=50 → 500 consumers; grows only with W). Three real merits:
**preserves worker cache locality** (leader→worker unchanged → only the ~Δ-fraction
that move lose cache, like Dynamic); **lowest invasiveness** — a pure
`WorkerConsumerUpdater` impl, *zero* assignment/manager/source/strategy change; and
**count independent of N**.

**But it is structurally lossy on migration.** When partition P moves A→B, P is
added to one of B's *existing* bucket durables whose shared cursor has its own
history: cursor **ahead** of P's position → messages **skipped (loss, Exp1)**;
**behind** → **duplicates**. Only a per-partition durable (Dynamic) carries a
partition's cursor across a worker move (Exp2); sharing a cursor trades that away.
Magnitude is bounded to the migrating partitions (~Δ/W per HPA) and small for
keep-up workers; `DrainOnRemove` shrinks but cannot close it without reintroducing
the Exp3 head-of-line stall.

**Verdict: the `Pooled` / best-effort contract** — lowest-cost, cache-friendly, for
**idempotent / loss-or-dup-tolerant** workloads. If you need lossless handoff it is
disqualified — use Dynamic or decoupled Grouped.

### The three real candidates

| | `Dynamic` (today) | `Grouped` (decoupled slot) | `Pooled` (M-per-worker) |
|---|---|---|---|
| Consumer count | N | fixed/bounded K | M·W (indep. of N) |
| Lossless handoff | ✅ unconditional | ✅ on worker churn (lossy on re-slot) | ❌ lossy/dup on migration |
| Per-partition cache locality | ✅ | ❌ (per-slot) | ✅ (worker-level) |
| Load-balancing granularity | per-partition | per-slot (coarse) | per-partition |
| Head-of-line isolation | ✅ per-partition | ❌ per-slot | ❌ per-bucket |
| Invasiveness to build | — | moderate (grouping layer + type) | **low** (consumer type only) |
| Natural fit | default | cache-light, ≳10k, lossless+cheap | idempotent / loss-tolerant + cache-heavy |

No row dominates: **lossless + per-partition cache locality is unique to Dynamic**
(K=N is the price). `Grouped` keeps lossless by giving up cache granularity;
`Pooled` keeps cache locality by giving up losslessness. That is the whole
decision.

**Scope of the trilemma:** it holds *within parti's current architecture* —
"workers bind durables and process messages locally." A fundamentally different,
heavier design — a **router/dispatcher tier** where K consumers pull and then
re-route each message to the per-partition owner (a second NATS hop or RPC) —
could in principle hold all three (fixed K + lossless + per-partition affinity),
because ownership is decoupled from which consumer reads the stream. That is a
different product (an extra tier, extra latency, its own delivery/ordering
contract) and is out of scope here, but the pigeonhole is a property of the
direct-consumer model, not a law of nature.

---

## 4. The user's three explicit questions, answered

**Q1 — Can a consumer update its subject filter at runtime seamlessly?**
**Yes** (Exp4). `UpdateConsumer` on a live, actively-pulling durable adds/removes
`FilterSubjects` with no recreation, no error, the existing subjects undisturbed,
and the shared cursor preserved. The operation is genuinely seamless.

**Q2 — Does it miss messages during partition reassignment?**
**It depends on the fork (§3) — and that is the whole story.** Worker failover in
the decoupled design is lossless (Exp2). The lossy cases (Exp1) are exactly the
filter-mutation transitions: re-slotting a partition between slots, growing/
shrinking K, and **onboarding a partition whose subject already carries stream
backlog below the slot's cursor** (a genuinely new partition with no prior data
is safe). The seamlessness from Q1 is *why* this loss is silent — `UpdateConsumer`
returns no error while skipping the backlog.

*What "loss" physically is* (Exp8): on `LimitsPolicy` it is a **per-consumer
delivery gap, not deletion** — the skipped messages stay in the stream (a fresh
consumer reads all of them), but the migrated consumer's shared cursor has moved
past them and there is **no per-partition ack floor to rewind to**, so the normal
delivery path **never re-feeds them to the handler**: those specific messages are
never processed, while newer messages on the same subject continue normally
(recoverable only by a separate consumer / explicit rewind). On `InterestPolicy`
the messages can be **physically deleted** (Exp5/Exp7) — gone for good. The mirror
case (gaining cursor *behind* the partition's position) yields **duplicates**
instead of a gap — hence the at-least-once-with-gaps contract and the idempotency
requirement.

**Q3 — What happens when a message on one subject isn't acked / is slow?**
**It starves every partition sharing that slot** (Exp3): `MaxAckPending` is one
budget shared across all `FilterSubjects`, so a stuck partition fills it and
co-located partitions get nothing. Worse, parti's default **`MaxDeliver=-1`
(unlimited redelivery)** (`internal/durable/config.go:224`, queue + dynamicbuild
defaults) means there is **no automatic release valve** — a permanently-stuck
partition starves its slot forever. And **you cannot filter the stuck partition
away to recover** on `LimitsPolicy`: its in-flight msgs keep redelivering against
the budget after removal (Exp5). Blast radius = N/K partitions; duration =
unbounded at the default. Mitigations: larger K (smaller slots), per-slot
`MaxAckPending` sized to N/K, and a finite `MaxDeliver` + dead-letter (which
parti does not have today).

---

## 5. Implementation in parti — invasiveness (reconciled verdict: MODERATE)

Two independent analyses + two adversarial verifiers converged here after
initially disagreeing; the reconciliation is precise:

**Unchanged** (verified by reading every candidate breaking path):
- **Manager** is a pure pass-through of `next.Partitions`.
- **Two-phase handoff coordinator** keys claims per-partition by `SubjectKey()`
  (`internal/assignment/handoff/twophase.go:254`) — works unchanged under a
  coherent assignment (all of a slot's partitions flip owner together).
- **Publisher / digest / diff** operate on opaque partition sets
  (`PartitionSetDigest` sorts+dedupes; `checkCoverage` is a union set-equality) —
  grouping-invariant.

**Mandatory new code** (this is why it is not "zero, just a consumer type"):

1. **A global-view grouping layer** — the partition→slot mapping **cannot** live in
   the per-worker `WorkerConsumerUpdater`, which only ever sees *one worker's*
   partition slice (`options.go:131-144`). If `W1` owns `{a,b}` and `W2` owns `{c}`
   all hashing to slot `S1`, the updater sees disjoint fragments → two workers bind
   one durable (Exp3 stall / Exp1 loss), an orphan, or slots forced to worker
   boundaries (= the lossy W-coupled variant). Grouping must live where the global
   set is visible — two shapes:
   - **Shape A — custom `PartitionSource`** of K atomic slots, assigned by a stock
     strategy (coherence automatic, control-plane O(K)). **But** a `types.Partition`
     carries *one* subject, not a set (`SubjectKey()` dot-joins `Keys`,
     `types/partition.go:54-66`), and parti's ownership is **real-subject-keyed** —
     claims key on `SubjectKey()` (`handoff/twophase.go:248-254`) and the processing
     gate resolves the *message* subject → partition ID
     (`processing_gate.go:147-183`). Slot-ID units + real-subject messages **mismatch**
     → Shape A needs a **slot-aware gate, claim, recovery redesign** + a
     slot-membership versioning contract.
   - **Shape B — custom slot-coherent `AssignmentStrategy`** over the real N
     partitions, routing a slot's partitions to one worker. **Preserves
     per-real-partition claims + the existing gate** (O(N) control-plane, but KV is
     ~1% of cost, so cheap; data-plane = K either way). **Lower-risk** — prefer it.
   **Neither preserves the per-partition recovery checkpoint:** recovery state is one
   `Checkpoint` per `partitionConsumer` = per-durable = **per-slot**
   (`internal/recovery/checkpoint.go:11-18`), so a per-real-partition resume point
   must be designed in regardless of shape (same root cause as Q2's "no floor to
   rewind to").
2. **A new multi-`FilterSubjects` whole-slot consumer type** — used nowhere today
   (current runtime is one durable per *singular* subject,
   `internal/dynamicbuild/builder.go:108`): slot-named durables stable across owners,
   multi-filter provisioning, `InactiveThreshold` tuning to survive the failover gap.
3. **A re-slotting protocol** — the one lossy, planned operation (drain-then-move or
   accept loss/replay) when N or K changes.

A slot *is* representable as a `types.Partition` (`{Keys, Weight}`, the assignment
unit), which is why the Manager/handoff machinery is reusable — but the global-view
grouping layer is non-negotiable, so "zero manager/assignment change" is only
half-true.

> **Discipline gate.** This is a *static* feasibility read for the multi-filter
> Shape A/B. Per this repo's history (static review has blessed a fatal design
> before — the self-healing pre-assignment-gate deadlock), a "done" gate needs a
> live integration proof. **For the recommended NATS-`partition()`-+-`Dynamic`-over-K
> path that proof now EXISTS — Exp11** (`test/integration/fixedpartitions/`,
> public API only, 4 scenarios, all `-race` clean): a real `Manager` +
> `source.NewStatic(16)` + `Dynamic` over `work.{{.PartitionID}}` delivered every
> message (0 lost) across — (1) single-node worker **join + graceful leave**
> (2→3→2, 875/875); (2) **3-node cluster, RF=3 stream + R=3 consumers, abrupt worker
> CRASH** (connection killed → heartbeat-TTL-driven reassignment, 1165/1165);
> (3) **two-phase handoff + processing gate + resolver** wired, across churn
> (875/875); and (4) **full server-side `partition()` path** (`ingest.*` →
> `work.{{partition(K,1)}}` → `Dynamic`, 875/875). **Exp12**
> (`TestFixedPartitions_NoStopTheWorld`) adds a *continuity* proof — measured
> per-message latency shows the reassignment is cooperative per-slot, not a
> Kafka-style global stop-the-world (see "Does this avoid Kafka's stop-the-world
> rebalance?" below). Remaining before production: scale (K-sweep, production load);
> `InactiveThreshold`-gap stress; and **moved-slot handoff latency on a real cluster
> and under sustained load** (Exp12 measured *retained*-slot continuity single-node;
> the moved-slot blip was ~40–80ms single-node but hit a ~30s drain-timeout outlier
> under `-race`, and is unmeasured on a multi-node cluster). The multi-filter
> Shape A/B (only needed when you cannot re-partition at ingest) still needs its own
> integration proof.

### The Kafka lens — and a near-zero-code path via NATS server-side `partition()`

`Grouped` is essentially **Kafka**: a slot = a Kafka *partition* (fixed count,
keys hash into it, own cursor, moves between consumers losslessly, HoL within it);
a real partition/subject = a Kafka *key* (hashes to a partition, shares its cursor,
can't migrate independently). Its trade-offs *are* Kafka's: per-partition (not
per-key) cache locality, intra-partition HoL, painful repartitioning. The lens
also explains the cost: **`Dynamic` is finer-grained than Kafka** — one
consumer/cursor *per key* — which Kafka never offered (per-key = one partition per
key = the same cost wall). `Grouped` is "retreat to Kafka granularity to bound the
count."

**NATS already ships the key→slot hash.** The core subject-mapping transform
`{{partition(n, tokens…)}}` (`nats-server/v2 server/subject_transform.go:30,46,500`
→ `getHashPartition`) deterministically maps a subject to one of `n` partition
subjects by hashing chosen tokens — same key → same partition, ordering-preserving.
This is Kafka-style fixed partitioning, server-side, at ingest.

**Consequence — the cleanest bounded-lossless design likely needs *no new parti
code*:** map keys → **K fixed partition-subjects** at ingest with `partition()`,
register those K subjects in the partition source, and run parti's **existing
`consumer.Dynamic` over the K subjects**:
- **K durables, bounded** (decoupled from key cardinality *and* from W).
- **Fully lossless** — each slot is a *single-filter* durable with its own cursor;
  the filter set **never changes**, so the Exp1 filter-mutation loss **cannot
  occur**; only the binding worker changes (Exp2 lossless rebind = Dynamic's proven
  handoff).
- **No multi-filter, no shared cursor, and no gate/claim/recovery redesign** — to
  parti a slot simply *is* a partition-subject, so the existing real-subject-keyed
  machinery works unchanged. This sidesteps the entire §5 Shape-A/B seam problem.

**This supersedes the multi-filter `Grouped` for the common case.** The
consumer-side multi-filter design (Shape A/B above) only earns its complexity when
the N subjects are **pre-existing and semantically fixed** — you cannot collapse
them at ingest. If you control ingest, prefer **NATS `partition()` + `Dynamic` over
K**. (Composition verified empirically — Exp10: `ingest.*` is rewritten to
`work.<p>` *before* JetStream capture, and a per-`work.<p>` single-filter consumer
reads it losslessly.)

**Changing K at runtime is a disruptive reshuffle, so K is pick-once (Exp10).**
Mappings are live-changeable (`AddMapping`/`RemoveMapping`, `accounts.go:692,816`),
but `partition()` is `fnv32a(key) % K` — **plain modulo** (`subject_transform.go:469`)
— so changing K remaps *~all* keys (Exp10: K=4→8 moved 11/20), not the ~Δ/K a
consistent hash would, breaking per-key ordering across the cutover. Same class of
pain as Kafka repartitioning. **Over-provision K up front.**

**"From the parti side":** parti is *downstream* of `partition()` and does not own
the producer hash. It *can* grow/shrink its consume-set at runtime
(`source.NatsKV.AddPartitions`/`RemovePartitions`, `source/nats_kv.go:716,752` — the
assignment machinery then creates/retires durables), but the producer-hash change is
an ops action and a safe repartition needs a drain-and-cutover (maintenance window),
not a hot event. **If graceful runtime repartition is a hard requirement,** drop NATS
`partition()` for a **client-side consistent-hash partitioner** parti owns (remaps
only ~Δ/K). Irreducible floor: even consistent hashing disrupts the keys that move —
the choice is only *how many* (modulo ≈ all; consistent ≈ Δ/K), never zero.

### Does this avoid Kafka's stop-the-world rebalance? Yes — and it is measured (Exp12)

Kafka's classic pain is the **eager rebalance protocol**: any consumer-group
membership change triggers a coordinator-driven **revoke-all → re-join →
re-assign** barrier during which *every* consumer stops processing *every*
partition — a true stop-the-world, not just the partitions that move. (Kafka 2.4+
added *incremental cooperative* rebalancing, KIP-429, to revoke only the moving
partitions.) The recommended `partition()` + `Dynamic`-over-fixed-K design avoids
the eager barrier on **two independent axes**:

1. **Producer side: no rebalance at all.** `partition()` is a stateless,
   deterministic ingest-time hash. Worker churn never touches it — K is fixed, the
   key→subject layout is fixed, producers keep publishing to the same K subjects
   regardless of worker count. There is **no producer-side coordination point to
   stop** (a structural advantage Kafka's consumer-group model does not have —
   decoupling K from W is what buys it; the rejected W-coupled design would not).
2. **Consumer side: per-slot handoff, not a group-wide barrier.** parti has **no
   coordinator revoke-all step.** The leader recomputes the assignment and each
   worker applies only its **delta**: `ConsistentHash` moves only the ~Δ/K slots
   that must move; the `Dynamic` updater's `computeSubjectDiff`
   (`internal/durable/worker_consumer.go:264`) touches only added/removed subjects,
   so **retained slots' pull loops are never stopped or restarted**; each moved slot
   hands off independently. This is structurally Kafka's *incremental cooperative*
   mode (KIP-429), not the old eager barrier.

**Empirical proof — Exp12** (`TestFixedPartitions_NoStopTheWorld`). Losslessness
(Exp11) alone cannot settle this: an eager stop-the-world is *also* lossless — it
just stamps the full pause onto every in-flight message as latency. So Exp12
measures **per-message latency** (recv − publish) during churn, split by slot class.
With K=16 and a producer publishing to all 16 subjects every 20ms, a worker join
(2→3) then graceful leave (3→2) yields (deterministic across runs, also `-race`):

| | retained slots (6) | moved slots (10) |
|---|---|---|
| latency during churn | **p99 0.1ms, max 0.4ms** (= quiet baseline) | p99 0.2ms, **max ~40–80ms** (single-node) |
| deliveries delayed >100ms | **0** | (the localized handoff blip) |
| worst 250ms-epoch max | **0.4ms** | — |

Retained-slot delivery dead-air during churn equalled the producer's own publish
gap (~20ms) — retained throughput never went dark. **Retained slots show zero
disruption; only moved slots blip.** All 6 retained slots were owned by a worker
that *also* handled a moved slot, so this is the strong claim — a **busy** worker
kept its *other* slots flowing while reassigning some, not an idle bystander.

**Why the zero is trustworthy (positive control).** The moved-slot blip is the
test's own positive control: the same instrument that recorded `0` slow retained
deliveries simultaneously recorded the moved-slot delay (tens of ms). So
`retainedSlow=0` means "the machinery that *did* detect the moved-slot blip saw
nothing on retained slots", not "the measurement is broken and reads zero" — both
directions of the boundary are exercised. That is cooperative per-slot rebalancing,
and it directly contradicts a global freeze (which would have spiked the retained
slots too).

**Residual localized pauses remain (honest, and they are *not* group-wide):**
- **Per-moved-slot handoff window.** A reassigned slot is briefly unavailable while
  the old owner drains (`DrainOnRemove`) and the new owner binds the durable.
  Confined to that slot's keys; blast radius ≈ Δ/K of slots.
- **Single-node moved-slot blip ≈ 40–80ms; real-cluster value is unmeasured.** The
  blip is the drain + durable-rebind window for a reassigned slot. Under `-race` one
  moved slot was occasionally delivered ~30s late — *not* `-race` slowdown (that is
  only ~10×, i.e. ~0.8s) but consistent with the slot's handoff hitting
  `DrainOnRemoveTimeout` (default **10s**, stacked with rebind/retry); still
  lossless, just very late. Crucially this is confined to **moved** slots — retained
  slots stayed sub-ms even in that run, because they do **zero** per-slot work during
  a reassignment. Moved-slot handoff latency on a real cluster (FileStorage + RAFT +
  network RTT) and under sustained load is **not yet measured** (see "remaining
  before production") — and a multi-second dark window on a *moved* slot during
  routine HPA is worth flagging to adopters, not assuming away.
- **Abrupt crash adds a detection gap.** A crashed (vs gracefully-leaving) worker's
  slots wait for `HeartbeatTTL` (~seconds, configurable) before reassignment — the
  analog of Kafka's `session.timeout.ms`, and for *those* slots only (Exp11
  scenario 2).
- **Scope.** Exp12 is single-node graceful churn — the cleanest discriminator.
  Distributed stop-the-world risks (split-brain under partition, asymmetric network
  delays) are out of its scope; the architectural argument (no group-wide barrier)
  still holds, but is not separately measured here.

**Bottom line:** the design mitigates Kafka's stop-the-world — the producer never
rebalances, and the consumer side is cooperative (only ~Δ/K moved slots blip
briefly; retained slots and the producer keep flowing). It does *not* eliminate the
per-moved-slot pause or the crash-detection TTL gap, but those are localized to the
affected slots, exactly like Kafka's *cooperative* mode.

---

## 6. Delivery contract (decoupled) — the honest guarantee

**"At-least-once delivery + per-partition ordering, *provided a partition never
changes slots after its first delivery* (the partition→slot map is immutable
post-launch); loss-on-re-slot otherwise; one stuck partition head-of-line-blocks
all partitions sharing its slot."**

Transition matrix (each cell = Exp1-vs-Exp2 applied):

| Transition | Decoupled | W-coupled |
|---|---|---|
| worker join / graceful leave / death | **lossless** (slot rebind, Exp2) | **lossy** (re-hash, Exp1) |
| partition removed from a slot | lossless (filter shrink never adds below-cursor) | lossless |
| **new** partition added (no prior backlog) | lossless | lossy |
| partition added **with pre-existing stream backlog** | **lossy** (Exp1) | lossy |
| K resize (re-slot) | **lossy** + dup/reorder window | lossy |
| hot-partition rebalance slot A→B | **lossy** unless drained-first | lossy |

**Contrast with `Dynamic`** (the cost of the trade): Dynamic gives at-least-once
+ per-partition order **unconditionally** (no "never move" caveat) and **no
head-of-line blast radius**, because each partition is its own single-filter
durable with its own cursor and own `MaxAckPending`. The virtual partition trades
Dynamic's unconditional safety + per-partition isolation for K ≪ N cost.

Ordering within a slot: across co-located partitions, delivery is interleaved by
**global stream sequence**, not per-partition-fair, and subject to the Exp3
blast radius.

---

## 7. Cost & scaling — what you actually buy

The perf-measurement study proved cost scales with **consumer count**, not
partition count (report 05). So `cost(K) ≈ a + b·K + c·X`, reusing report 03's
fitted cluster slopes:

| Metric | Dynamic (K=N=5000) measured | Virtual @ K=200 (projected) | Prize |
|---|---:|---:|---:|
| Cluster RSS | ~4,100 MiB | `90 + 0.793·200 + 0.673·100` ≈ **316 MiB** | **~13×** |
| Cluster IOPS (mem state) | ~146 | `3.6 + 0.028·200 + 0.0237·100` ≈ **~12** | **~12×** |
| Latency P95 | ~1.3 ms | ~1.3 ms (delivery floor) | none (already at floor) |
| Metacontroller snapshot | scales with consumers | sees **K**, not N | decoupled from N |

Key facts:
- **RSS is the larger prize** (~13× at K=200) and the most trustworthy number.
- **Both numbers are projections from *single-filter* consumer slopes** — a
  multi-filter slot durable carrying N/K subjects was never benchmarked, so treat
  IOPS especially as directional (a large reduction), not exact. Only K=1 (queue
  floor) and K=N (Dynamic) are empirical endpoints; a direct K-sweep would confirm
  the slope.
- **Savings are linear in K** (`0.793·(N−K)` MiB) — **no cost knee**, so K is bounded
  *from below by blast radius* (§4 Q3), not by cost: K=500 still sits ~7–8× below
  Dynamic.
- **Metacontroller** is the cleanest K ≪ N win: the 1.286 s production incident was
  10,172 consumers; K=200 presents 200 regardless of N.

**But weigh against the baseline:** the perf study already showed `Dynamic` +
memory consumer state + R=3 scales to **N=10,000 cleanly** (lossless, flat
~1.4 ms P99, ~30 IOPS/node, snapshot ~30 ms async). So below ~10k partitions/
cluster the cost virtual partitions remove is **modest** (RSS ~0.16 MiB/consumer/
node and a cheap async snapshot). The prize is real only at the high end.

---

## 8. Pros / cons

**Pros**
- ~13× RSS and ~12× IOPS reduction at K=200/N=5000 (projected from single-filter
  slopes; RSS is the solid number); metacontroller snapshot
  decoupled from N (the ≫10k-consumer regime).
- Reuses parti's Manager / handoff machinery unchanged (the grouping lives in a
  custom strategy or source + a new consumer type — see §5 Shape A/B).
- Latency unchanged (already at the JetStream delivery floor).
- K is a tunable isolation/cost dial; can degrade gracefully toward Queue.

**Cons** (the trade — detail in §3/§4/§6)
- **New loss surfaces `Dynamic` lacks** (all Exp1): re-slot, onboarding-backlog, and
  K-resize loss → a *conditional* guarantee, not unconditional.
- **Head-of-line blast radius = whole slot** (Exp3), unbounded at the default
  `MaxDeliver=-1` (no dead-letter path today).
- **Coarser balancing + per-slot (not per-partition) cache locality** (§3) — whole-slot
  moves, no per-partition pinning; poor fit for per-partition-cache-heavy workloads.
- **Retention foot-guns** — safe only on `LimitsPolicy`: `InterestPolicy` discards
  no-covering-filter publishes (Exp7) and strands removed-subject msgs (Exp5);
  `WorkQueuePolicy` forbids overlapping filters (Exp6/9), so it kills dups but turns
  any transient handoff overlap into a hard `10100` — which penalizes `Pooled`
  (subject moves between *different* durables) while leaving `Grouped`/`Dynamic` clean
  (same durable rebind). A point *for* `Grouped` over `Pooled`.
- **Moderate to build** (new consumer type + grouping layer + re-slot protocol) and
  must clear an integration proof.

---

## 9. Naming

The four existing types are single topology adjectives (`Dynamic`, `Queue`,
`Broadcast`, `Static`). Pick the name for the **lossless decoupled** contract
(shipping the lossy one would contradict parti's lossless-handoff value).

- **`consumer.Grouped`** (recommend) for the lossless decoupled type — "many
  partitions grouped onto one durable", on-axis and accurate. (Disambiguate from
  Queue's "queue group": grouping = partitions-per-durable, not consumers-per-durable.)
- **`consumer.Pooled`** reserved for the lossy W-coupled variant — "pool" honestly
  signals shared-cursor / no per-partition continuity.
- Runners-up: `Sharded` ("shard" ≈ partition, risks reading as split-into-more),
  `Virtual` (off-axis). Eliminated: `Bucketed` (collides with NATS KV "bucket"),
  `Striped` (wrong direction).

---

## 10. Recommendation

**Don't build it speculatively** (see TL;DR for the why). When a deployment *does*
outgrow Dynamic — **≳10k partitions/cluster** straining per-consumer RSS or the
metacontroller snapshot, or a hard fleet-wide RSS ceiling — pick the variant by the
workload's loss tolerance (§3 three-way):

- **Lossless + bounded count + you control ingest** → **don't build a type at all.**
  Map keys → K partition-subjects with NATS `partition()` and run **existing
  `consumer.Dynamic` over K** (§5 Kafka lens; guide). Lossless, K durables, ≈zero new
  code. **Reach for this first.**
- **Loss-tolerant / idempotent + cache-heavy** → **`Pooled`** (M-per-worker). Cheapest
  to build (pure `WorkerConsumerUpdater`, zero assignment change), preserves worker
  cache locality, count = M·W. Best-effort: requires idempotent handlers.
- **Lossless + cache-light + ingest fixed (can't re-partition)** → **`Grouped`**,
  decoupled, **Shape B** (preserves real-subject-keyed claims + gate; Shape A must
  redesign both + add slot-versioning — §5). Recovery goes slot-level either way (§5).
  Accept the §6 conditional guarantee + per-slot cache/balancing.

**Do NOT use any variant when the workload relies on per-partition cache locality**
(§3) — affinity collapses to per-slot; `Dynamic` is strictly better unless the cache
can be re-keyed per slot. Either way: `LimitsPolicy`, a finite `MaxDeliver` +
dead-letter for the HoL blast, and **gate "done" on a live-cluster integration proof**
(handoff + gate + recovery + `InactiveThreshold` under churn, `-race`), not static
review. **For most users the answer stays `consumer.Dynamic` + memory state + R=3.**

---

## 11. Open questions a real design must close

**Already verified** (the recommended `partition()` + `Dynamic`-over-K path; see §5
discipline gate for detail): end-to-end lossless reassignment across
join/graceful-leave/abrupt-crash (single-node + 3-node cluster RF=3, + processing
gate, + the full server-side `partition()` path) — Exp11, 4 scenarios; cooperative,
no-stop-the-world rebalance — Exp12; `partition()`→JetStream composition + runtime
modulo-reshuffle — Exp10. All `-race` clean.

**Still open for a production `Grouped`/`Pooled` build:**
- **K-sweep benchmark** with real multi-filter durables (intermediate K is projected
  from the K=1 and K=N endpoints only).
- **Re-slotting protocol**: drain-then-move vs accept loss/replay vs start-seq rewind
  + handler dedupe.
- **Per-slot `MaxAckPending` sizing** vs N/K, and a **dead-letter** path (absent today)
  to bound the HoL blast.
- **Retention** baked into the contract (Limits strongly preferred; Exp5–7 hazards on
  Interest/WorkQueue).
- **Concurrent double-bind** during slot handoff under `-race` on a real cluster
  (static read: bounded to at-least-once dup like Dynamic, K-fold wider — prove it).
- **File-storage + R>1** re-confirmation of the Exp1/Exp2 boundaries (POC ran memory).
- **Slot-membership versioning + recovery redesign** (§5): make assignment-rev,
  slot-map-rev, and the applied filter set atomic; design a per-real-partition resume
  point under a slot's shared cursor.
- **Ordering/parallelism within a slot**: one pull-loop serializes a slot's N/K
  partitions (less than Dynamic's N loops); a demuxed consumer needs explicit
  per-subject ordering + ack rules.
- **Moved-slot handoff latency** on a real cluster / under sustained load (Exp12
  measured *retained*-slot continuity single-node only — §5).

---

*Empirical basis: [`poc/`](poc/) — 10 experiments, `go test ./...` (~35 s).
Cost basis: `../perf-measurement/` reports 03/04/05. Parked-idea provenance:
`../assignment-correctness-fixes/00-fix-plan.md` FINDING-A.*
