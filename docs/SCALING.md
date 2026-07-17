# Scaling: bounded-cost partitioning with NATS `partition()` + `consumer.Dynamic`

> Serve a high-cardinality key space with a *fixed, small* number of JetStream
> consumers (K), losslessly, **without writing any new parti code**.

**Related Documentation:**
- [Consumers Guide](CONSUMERS.md) — consumer types, retention policy, storage tuning
- [Operations Guide](OPERATIONS.md) — capacity planning, NATS-side cost model
- [Architecture](ARCHITECTURE.md) — system architecture and concepts

---

## Overview

By default, parti runs one `consumer.Dynamic` per partition. The perf study shows
that scales cleanly to **N = 10,000 partitions** on a 3-node cluster — so most
deployments never need anything else.

When you have far more logical keys than you want JetStream consumers for — and
**per-key isolation is not required** — you can serve the whole key space with a
*fixed* number of numbered partition-subjects (K) and let NATS deterministically
hash keys into them. This is "be Kafka": choose a partition count K, hash keys
into K, consume K. It reuses parti's existing `consumer.Dynamic` over a fixed set
of K subjects — **no new consumer type**, and it is the only K-bounded pattern
parti ships.

Requires **NATS ≥ 2.10** (≥ 2.12 recommended at large fleets — see
[OPERATIONS.md](OPERATIONS.md)).

### When to use this

✅ Use it when:
- You have far more logical keys (entities) than you want JetStream consumers for,
  and **per-key isolation is not required** — per-*partition* (per-slot) ordering
  and ownership is enough (exactly Kafka's model).
- You're approaching the per-consumer cost wall (≳10k consumers strains RSS or the
  metacontroller snapshot), or you simply want a Kafka-style fixed partition count.

❌ Don't use it when:
- The workload needs **per-key cache locality or per-key isolation** (a stuck key
  must not block its neighbours). Then keep one `Dynamic` consumer per entity
  (K = N) — that scales cleanly to N = 10,000.

---

## How it works (one mapping + existing `Dynamic`)

```
producer ──publish ingest.<key>──▶  NATS partition() mapping  ──▶ stream WORK (work.0 … work.K-1)
                                    work.{{partition(K,1)}}              │
                                    (fnv32a(key) % K, ordering-preserving)│
                                                                          ▼
            parti Manager assigns the K partition-subjects across W workers
            (ConsistentHash, >80% cache affinity), one Dynamic durable per work.<p>;
            worker churn → lossless durable rebind (same single-filter durable).
```

A `work.<p>` partition-subject is a "slot". Each slot is **one single-filter
durable** with its **own cursor**, so the filter never changes and reassignment is
a lossless rebind — identical to today's `Dynamic` per-partition handoff. parti
sees K partitions and reassigns them exactly as it reassigns any partition set.

---

## Step-by-step

### 1. Choose K (pick once, over-provision)

`W ≤ K`. K bounds consumer count and is the isolation/cost dial: larger K ⇒
smaller per-slot head-of-line blast radius (fewer keys per slot) but more
consumers. A typical choice is K = 256 (Kafka-like). **Changing K later is a
disruptive reshuffle** (see [Operating notes](#operating-notes)).

### 2. Partition source — K numbered partitions

```go
parts := make([]types.Partition, K)
for i := range parts {
    parts[i] = types.Partition{Keys: []string{strconv.Itoa(i)}, Weight: 1} // PartitionID "0".."K-1"
}
src := source.NewStatic(parts)
```

### 3. Stream — capture the K partition-subjects

```go
js.CreateStream(ctx, jetstream.StreamConfig{
    Name:      "WORK",
    Subjects:  []string{"work.*"},
    Storage:   jetstream.FileStorage,        // durable messages
    Replicas:  3,                            // on a cluster
    Retention: jetstream.LimitsPolicy,       // recommended for Dynamic — see CONSUMERS.md
})
```

`LimitsPolicy` is the recommended retention policy for `Dynamic`; see
[Stream Retention Policy](CONSUMERS.md#stream-retention-policy) for why (and what
`WorkQueuePolicy`/`InterestPolicy` cost here).

### 4. Route keys → partition-subjects (producer side)

The producer's subject **must equal** the consumer's filter subject (`work.<p>`).

**Option A — NATS server-side mapping (zero producer code).** In the account scope
of `nats.conf` (hot-reloadable). The destination `work.<p>` matches the consumer
template; `partition(K,1)` hashes the first wildcard token (the key):

```
mappings = {
  "ingest.*": "work.{{partition(256,1)}}"
}
```

Producers just publish to `ingest.<key>`; NATS rewrites to `work.<fnv32a(key)%256>`
*before* the stream stores it.

**Option B — client-side hash (more control; enables graceful repartition later).**
The producer computes `p` and publishes `work.<p>` directly. Use a consistent-hash
partitioner if you want to grow K with minimal remapping.

### 5. The consumer — existing `consumer.Dynamic`, numeric template

```go
c, err := consumer.NewDynamic(
    js, "WORK", "worker",
    "work.{{.PartitionID}}",                 // PartitionID "7" → filter subject "work.7"
    handler,
    consumer.WithMaxDeliver(maxDeliver),      // + a dead-letter handler: bounds per-slot HoL
)
```

> **Consumer-state storage is a separate, conditional choice.** You may add
> `WithConsumerMemoryStorage(true)` (+ `WithConsumerReplicas(3)`) here to cut
> write IOPS — but only if your deployment meets the criteria in
> [Consumer Storage Tuning](CONSUMERS.md#consumer-storage-tuning). The default
> file-backed consumer state is correct for most deployments; this pattern does
> not require memory storage.

### 6. The Manager — unchanged wiring (one per worker instance)

```go
cfg := parti.DefaultConfig() // your existing config
mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(),
    parti.WithWorkerConsumerUpdater(c),
)
_ = mgr.Start(ctx)
_ = <-mgr.WaitState(parti.StateStable, 30*time.Second)
```

That's it — no new consumer type. The `Manager` assigns the K slots across the W
workers; each worker's `Dynamic` creates a durable per assigned `work.<p>`; churn
triggers lossless rebind.

---

## Operating notes

- **`W ≤ K`.** Excess workers (W > K) sit idle (harmless standby). Size K ≥ your
  max expected W.
- **Per-slot, not per-key.** A slot's `(keyspace / K)` keys share **one cursor and
  one `MaxAckPending`** → per-slot ordering, and a stuck key head-of-line-blocks
  its slot-mates. Mitigate with a larger K and `WithMaxDeliver` + a dead-letter
  path.
- **Retention = `LimitsPolicy`** (recommended). On `InterestPolicy`, messages
  published while no consumer covers a subject are discarded at publish time; on
  `WorkQueuePolicy`, overlapping filters are rejected and cross-slot moves need
  remove-before-add. See [Stream Retention Policy](CONSUMERS.md#stream-retention-policy).
- **K is pick-once.** NATS `partition()` is `fnv32a(key) % K`, so changing K
  reshuffles ~all keys (a disruptive repartition, Kafka-class). parti can
  grow/shrink its *consume-set* (`source.NatsKV.AddPartitions` / `RemovePartitions`),
  but the producer-side hash change is an ops/config action and needs a
  drain-and-cutover. If you must grow K gracefully at runtime, use Option B with a
  **consistent-hash** partitioner (remaps only ~Δ/K of keys).
- **Rebalancing is cooperative, not stop-the-world.** Unlike a Kafka eager
  consumer-group rebalance (revoke-all → re-join → re-assign barrier), worker churn
  here moves only the ~Δ/K reassigned slots; **slots that stay put keep flowing at
  baseline latency** (the `Dynamic` updater touches only added/removed subjects).
  The producer never rebalances (the `partition()` hash is independent of worker
  count). Cost is a brief per-*moved*-slot handoff blip (drain + rebind) and, for
  an *abrupt crash*, a `HeartbeatTTL` detection gap for the crashed worker's slots
  only. Retained slots keep flowing at baseline throughout.
- **Cost (projected from the perf study, memory consumer state + R=3, K=256/N=5000):**
  ~300 MiB cluster RSS, ~12 cluster IOPS, sub-ms metacontroller snapshot. Latency
  is at the JetStream floor; this buys footprint, not speed. See the "NATS-Side
  Cost" section in [OPERATIONS.md](OPERATIONS.md).

### Tuning at high partition counts

Each `Dynamic` partition consumer runs its own pull loop, and each loop re-issues
an idle pull request every `FetchTimeout`. That re-issue traffic scales with the
partition count and becomes the dominant **idle** server CPU cost at large P —
it is a floor you pay even with zero messages flowing.

- **Raise `WithFetchTimeout` to 30s at P ≳ 2000.** Measured on the v2.10 perf
  rig (W=100, P=10,000, R=3): moving FetchTimeout from the 5s default to 30s cut
  server CPU by **~0.77 cores idle / ~1.13 cores under load**, with P50 delivery
  latency flat, P99 *improved*, and delivery ratio unaffected. Values above 60s
  also work (the derived pull heartbeat is clamped to nats.go's 30s ceiling),
  but 30s captured the bulk of the win in measurement.
- **Pair it with `WithPullHeartbeatCap` to keep detection fast.** The pull
  heartbeat is derived as `FetchTimeout/2` (capped at 30s), and the first
  missed-heartbeat signal fires at roughly 2× the heartbeat. At
  `FetchTimeout=30s` that stretches the first signal to ~30s. Setting
  `WithPullHeartbeatCap(5*time.Second)` holds it at ~10s; confirmed recovery
  follows after the burst threshold (default 3 consecutive misses) plus a
  confirmation check — still several-fold faster than the uncapped
  30s-heartbeat equivalent. Idle heartbeats are cheap server→client pushes, so
  the cap costs far less than the pull re-issues you removed.
- **Leader audit cadence tracks `HeartbeatTTL`.** The leader-side apply audit
  runs once per `HeartbeatTTL` (not separately configurable); its cost scales
  with worker count and is negligible at the measured scales (W ≤ 100). If you
  raise `HeartbeatTTL` for other reasons, the audit stretches with it — no
  separate tuning is needed or possible in 2.x.

---

## Proof

This pattern is backed by live-cluster integration tests in
`test/integration/fixedpartitions/`:

- **Lossless reassignment across churn** — worker join, graceful leave, and abrupt
  crash, single-node and on a 3-node cluster (RF=3 stream + R=3 consumers), plus a
  processing-gate variant; `-race` clean.
- **Cooperative (no stop-the-world) rebalance** — per-message latency shows
  retained slots keep flowing during churn while only moved slots blip
  (`TestFixedPartitions_NoStopTheWorld`).
- **Full server-side `partition()` path** — producers publish `ingest.<key>`, NATS
  `{{partition(K,1)}}` routes to `work.<p>`, `Dynamic` consumes.

For the full design analysis, the three-way isolation↔cost trade, and the cost
model, see the feasibility assessment under
[`docs/plans/partition-scaling/`](plans/partition-scaling/01-feasibility-assessment.md).
