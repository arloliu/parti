# Virtual-partition feasibility POC

A throwaway, self-contained Go module (separate from the parti module — excluded
from `make test`/`lint`) that answers the load-bearing **empirical** questions
about the "virtual partition" (pooled subject-filtered consumer) idea against a
real embedded NATS server (`nats-server v2.14.1`, the version pinned in the
parent module). Observed behavior, not documentation reading.

## Run

```
cd docs/plans/partition-scaling/poc
go test -v -count=1 ./...
```

~35 s; each experiment spins up its own in-process JetStream server.

## What it proves

| Experiment | User question it answers | Observed result |
|---|---|---|
| **Exp1** `FilterMutationLoss` | "does it miss messages during partition reassignment?" | **Yes — when you mutate a live filter set.** Adding subject `p1` (backlog at stream seq 1–5) to a durable whose shared cursor had already advanced past seq 10 → the 5 backlog msgs were **never delivered**; only the 3 `p1` msgs published *after* the update arrived. |
| **Exp2** `DurableRebindNoLoss` | (the counter-case) | **No loss — when the filter set is unchanged.** Owner #1 acked seq 1–2 of slot durable `{p2,p3}`, more msgs landed (seq 3–10), owner #1 vanished, owner #2 **rebound the same durable by name** → received **all 8 uncommitted msgs (seq 3–10)**. The shared cursor / ack floor is preserved across the ownership change. |
| **Exp3** `HeadOfLineBlocking` | "what happens when a message in one subject isn't acked, or slow?" | **It starves the co-located subjects.** With `MaxAckPending=5`, not acking the `stuck` subject filled the per-consumer pending budget; the `healthy` subject got **0** delivered until the stuck msgs were acked, then all 10 flowed. `MaxAckPending` is shared across all filter subjects → blast radius = the whole slot. |
| **Exp4** `UpdateSeamlessness` | "can a consumer update its subject filter at runtime seamlessly?" | **Yes.** `UpdateConsumer` adding a subject to a *live, actively-pulling* consumer returned no error, **no recreation**, **cursor preserved** (no redelivery of acked seq 1–5), the existing subject was undisturbed, and the new subject's post-update msgs flowed. |
| **Exp5** `SubjectRemovalInFlight` | "what happens to in-flight unacked msgs when their subject is removed from the filter?" | **Retention-dependent.** All 5 msgs (2 `a`, 3 `b`) were delivered-unacked (`NumAckPending=5`) before `b` was removed. On **LimitsPolicy**: the removed-subject msgs are **NOT cancelled** — stream count stays 5 and they keep redelivering against the slot's `MaxAckPending` until acked (the slot still owns them); `NumAckPending` stayed 5 across redelivery → redelivered-but-unacked still counts against the budget. On **InterestPolicy**: removing `b` from the filter dropped the stream from 5 → **2** — the 3 in-flight `b` msgs were **deleted server-side** (no other interested consumer) and **never redelivered** (lost to this consumer). Acking a removed-subject msg returns no error in either mode. *(Distinct from Exp7: here the msgs DID enter the stream and were delivered; removal then dropped them.)* |
| **Exp6** `WorkQueueFilterRules` | "does WorkQueuePolicy permit overlapping / changing filter sets?" | **No overlap allowed.** A second consumer with an overlapping filter is rejected by the server: `code=400 err_code=10100 "filtered consumer not unique on workqueue stream"`. **Disjoint** filters are fine. Moving a subject between slots requires **remove-before-add** — growing one slot's filter while another still owns the subject is rejected (transient overlap forbidden). |
| **Exp7** `InterestPublishTimeInterest` | "on InterestPolicy, are msgs retained for a subject no consumer currently filters?" | **No — discarded at publish time.** 5 msgs published to `p1` while no consumer filter covered `p1` left the stream holding only the 3 `p0` msgs; a `p1` consumer created afterwards received **0**. Interest is evaluated at publish time. |
| **Exp8** `LossIsDeliveryGapNotDeletion` | "when a migration 'loses' a msg, is it destroyed — does it never get processed?" | **On `LimitsPolicy`: a per-consumer delivery gap, not deletion.** The migrated consumer skipped backlog seq `1..5`; a **fresh** consumer on the same subject read **all 8** (`1..5,11..13`); the stream still held all 13 msgs. So the skipped msgs stay in the stream but the migrated consumer **never re-delivers them** (cursor moved past; no per-partition ack floor to rewind to) → never processed under normal flow, recoverable only by a different consumer/rewind. On `InterestPolicy` they can be physically deleted (Exp5/Exp7) = gone for good. |
| **Exp9** `WorkQueueMigrationGap` | "does WorkQueuePolicy (consume-once) avoid the migration gap?" | **No.** WorkQueue *retains* the no-owner-window backlog (vs Interest's discard) but the gaining bucket's advanced cursor **still skips seq 1..5** (only new `[11 12 13]` delivered) → 5 msgs **stranded** in the stream. **Duplicates are eliminated** (acked msgs deleted, nothing to re-deliver). But recovery is **harder**: an overlapping recovery consumer is **rejected `10100`** until the subject is removed from the bucket (then it reads `[1..5]`). Implication: WorkQueue's disjoint rule turns any transient handoff overlap into a hard failure → penalizes the per-worker `Pooled` design (subject moves between *different* bucket durables) while `Grouped`/`Dynamic` (rebind the *same* durable) stay clean. |
| **Exp10** `NATSPartitionMapping` | "does NATS server-side `partition()` compose with JetStream + Dynamic, and can K change at runtime?" | **Composition ✅, runtime repartition = modulo reshuffle.** An account mapping `ingest.* → work.{{partition(K,1)}}` rewrites the subject **before** stream capture: the `work.>` stream stored `work.<p>` and a per-`work.<p>` single-filter (Dynamic-style) consumer read all 20 keys losslessly — so the "near-zero-code" path (NATS partition + existing `Dynamic`) works. Mappings **are** live-changeable (`AddMapping`/`RemoveMapping`), but `partition()` is `fnv32a(key)%K` (**modulo**, `subject_transform.go:469`), so K=4→8 moved **11/20** keys to different partition-subjects (≈all at scale) — runtime repartition is **possible but disruptive** (per-key ordering breaks across cutover), the same class as Kafka repartitioning. Graceful repartition needs a client-side **consistent-hash** partitioner instead. |

## Exp11 / Exp12 — live-cluster integration proof (separate location)

Exp1–10 above are in this NATS-only module. **Exp11/Exp12** need the **parti
library** (real `Manager` + assignment + handoff), so they live in the main module
at [`test/integration/fixedpartitions/`](../../../../test/integration/fixedpartitions/).
They configure the **recommended design with public API only** (`source.NewStatic(16)`
+ `consumer.NewDynamic` over `work.{{.PartitionID}}` + `parti.NewManager`, no new
consumer type).

**Exp11** proves lossless slot reassignment across **4 scenarios** (all `-race`
clean): (1) single-node worker join + graceful leave (875/875); (2) **3-node
cluster, RF=3 stream + R=3 consumers, abrupt worker CRASH** (connection killed →
TTL-driven reassignment, 1165/1165); (3) **two-phase handoff + processing gate +
resolver** across churn (875/875); (4) **server-side `partition()` mapping
end-to-end** (`ingest.*` → `work.<p>`, 875/875). All **0 lost**.

**Exp12** (`TestFixedPartitions_NoStopTheWorld`) answers the **stop-the-world**
question that losslessness alone cannot: a Kafka-style eager rebalance would *also*
be lossless — it would just stamp the full pause onto every in-flight message. So
Exp12 measures **per-message latency** (recv − publish) during churn, split by slot
class. Result (deterministic across runs, also `-race`): with K=16 and a producer
hitting all 16 subjects every 20ms, a worker join (2→3) then leave (3→2) leaves
**6 retained slots at ~0.1ms latency unchanged from baseline** (0 deliveries delayed
>100ms, worst 250ms-epoch max 0.4ms) while only the **10 moved slots** show a brief
localized handoff blip (~40–80ms single-node; one `-race` outlier hit a ~30s
drain-timeout — a moved-slot cost, not a `-race` slowdown; real-cluster value
unmeasured). Retained-slot delivery dead-air == the producer's own publish gap
(~20ms) — retained throughput never stalled. This is **cooperative per-slot
rebalancing, not a global freeze** (mechanism: `computeSubjectDiff` in
`internal/durable/worker_consumer.go` touches only added/removed subjects; retained
pull loops are never stopped).

Run: `go test ./test/integration/fixedpartitions/ -race`. See the
[**how-to guide**](../02-guide-nats-partition-dynamic.md).

## The one-line takeaway

The filter-mutation **operation is seamless** (Exp4) — and that very seamlessness
(it preserves the consumer's single shared cursor) is **exactly why** backlog
below the cursor on a newly-added subject is skipped (Exp1). Loss is a property
of **mutating a live consumer's filter set**, *not* of multi-subject consumers
per se: rebinding the **same** filter set from a new owner is lossless (Exp2).

This is the empirical basis for the central design fork in
[`../01-feasibility-assessment.md`](../01-feasibility-assessment.md): a
**W-coupled** design re-hashes subjects across consumers on every worker-churn
event (→ routine Exp1 loss), whereas a **decoupled fixed-K slot** design only
ever rebinds whole slots on failover (→ Exp2 lossless) and mutates filters only
on rare planned re-slotting.
