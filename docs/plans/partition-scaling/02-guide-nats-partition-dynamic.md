# Guide: bounded-cost partitioning with NATS `partition()` + `consumer.Dynamic`

**Goal:** serve a high-cardinality key space with a *fixed, small* number of
JetStream consumers (K), losslessly, **without writing any new parti code** — by
giving parti's existing `consumer.Dynamic` a fixed set of **K numbered
partition-subjects** and letting NATS deterministically hash keys into them.

This is the **recommended** realization of the "virtual partition" idea (see the
[feasibility assessment](01-feasibility-assessment.md)). It is "be Kafka": choose a
partition count K, hash keys into K, consume K.

> **Status: assessed POSITIVE and proven.** NATS `partition()` → JetStream capture →
> per-subject `Dynamic` consumption is verified (Exp10), and end-to-end lossless
> slot reassignment across worker **join, graceful leave, and abrupt crash** is
> verified single-node *and* on a 3-node cluster (RF=3 stream + R=3 consumers),
> `-race` clean — `test/integration/fixedpartitions/` (Exp11). Requires **NATS ≥ 2.10**.

---

## When to use this

✅ Use it when:
- You have far more logical keys (entities) than you want JetStream consumers for,
  and **per-key isolation is not required** — per-*partition* (per-slot) ordering
  and ownership is enough (exactly Kafka's model).
- You're approaching the per-consumer cost wall (≳10k consumers strains RSS or the
  metacontroller snapshot), or you simply want a Kafka-style fixed partition count.

❌ Don't use it when:
- The workload needs **per-key cache locality or per-key isolation** (a stuck key
  must not block its neighbours). Then keep one `Dynamic` consumer per entity
  (K=N) — the perf study shows that scales cleanly to N=10,000.

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
durable** with its **own cursor**, so the filter never changes and reassignment is a
lossless rebind — identical to today's `Dynamic` per-partition handoff. parti sees
K partitions and reassigns them exactly as it reassigns any partition set.

---

## Step-by-step

### 1. Choose K (pick once, over-provision)
`W ≤ K`. K bounds consumer count and is the isolation/cost dial: larger K ⇒ smaller
per-slot head-of-line blast radius (fewer keys per slot) but more consumers. A
typical choice is K=256 (Kafka-like). **Changing K later is a disruptive reshuffle**
(see [Operating notes](#operating-notes)).

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
    Retention: jetstream.LimitsPolicy,       // recommended (see notes)
})
```

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
*before* the stream stores it (verified, Exp10).

**Option B — client-side hash (more control; enables graceful repartition later).**
The producer computes `p` and publishes `work.<p>` directly. Use parti's
`strategy`-style consistent hashing if you want to grow K with minimal remapping.

### 5. The consumer — existing `consumer.Dynamic`, numeric template
```go
c, err := consumer.NewDynamic(
    js, "WORK", "worker",
    "work.{{.PartitionID}}",                 // PartitionID "7" → filter subject "work.7"
    handler,
    consumer.WithConsumerMemoryStorage(true), // perf study's recommended config
    consumer.WithConsumerReplicas(3),         // on a cluster
    consumer.WithMaxDeliver(maxDeliver),      // + a dead-letter handler: bounds per-slot HoL
)
```

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

## Minimal complete example (per worker process)

```go
func runWorker(ctx context.Context, js jetstream.JetStream, K int) (*parti.Manager, error) {
    parts := make([]types.Partition, K)
    for i := range parts {
        parts[i] = types.Partition{Keys: []string{strconv.Itoa(i)}, Weight: 1}
    }
    src := source.NewStatic(parts)

    handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
        // ... process msg (idempotent-friendly); return nil to auto-ack ...
        return nil
    })

    c, err := consumer.NewDynamic(js, "WORK", "worker", "work.{{.PartitionID}}", handler,
        consumer.WithConsumerMemoryStorage(true),
        consumer.WithConsumerReplicas(3),
    )
    if err != nil {
        return nil, err
    }

    cfg := parti.DefaultConfig()
    mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(),
        parti.WithWorkerConsumerUpdater(c))
    if err != nil {
        return nil, err
    }
    return mgr, mgr.Start(ctx)
}
```
Run this in every worker pod; they auto-claim distinct worker IDs and split the K
slots. The stream + the `nats.conf` mapping (Step 3/4A) are one-time setup.

---

## Operating notes

- **`W ≤ K`.** Excess workers (W > K) sit idle (harmless standby). Size K ≥ your max
  expected W.
- **Per-slot, not per-key.** A slot's `(keyspace / K)` keys share **one cursor and
  one `MaxAckPending`** → per-slot ordering, and a stuck key head-of-line-blocks its
  slot-mates. Mitigate with a larger K and `WithMaxDeliver` + a dead-letter path.
- **Retention = `LimitsPolicy`** (recommended). On `InterestPolicy`, messages
  published while no consumer covers a subject are discarded at publish time; on
  `WorkQueuePolicy`, overlapping filters are rejected and cross-slot moves need
  remove-before-add (see assessment §8 / Exp5–7,9).
- **K is pick-once.** NATS `partition()` is `fnv32a(key) % K`, so changing K
  reshuffles ~all keys (a disruptive repartition, Kafka-class — verified Exp10).
  parti can grow/shrink its *consume-set* (`source.NatsKV.AddPartitions` /
  `RemovePartitions`), but the producer-side hash change is an ops/config action and
  needs a drain-and-cutover. If you must grow K gracefully at runtime, use Option 4B
  with a **consistent-hash** partitioner (remaps only ~Δ/K of keys).
- **Rebalancing is cooperative, not stop-the-world.** Unlike a Kafka eager
  consumer-group rebalance (revoke-all → re-join → re-assign barrier), worker churn
  here moves only the ~Δ/K reassigned slots; **slots that stay put keep flowing at
  baseline latency** (the `Dynamic` updater touches only added/removed subjects).
  The producer never rebalances (the `partition()` hash is independent of worker
  count). Cost is a brief per-*moved*-slot handoff blip (drain + rebind — ~40–80ms
  single-node in Exp12; can hit the `DrainOnRemoveTimeout` (default 10s) under load;
  real-cluster value unmeasured) and, for an *abrupt crash*, a `HeartbeatTTL`
  detection gap for the crashed worker's slots only. Retained slots keep flowing at
  baseline throughout. Measured: Exp12 (`TestFixedPartitions_NoStopTheWorld`).
- **Cost (projected from the perf study, mem consumer state + R=3, K=256/N=5000):**
  ~300 MiB cluster RSS, ~12 cluster IOPS, sub-ms metacontroller snapshot — trivial.
  Latency is at the JetStream floor; this buys footprint, not speed.

---

## Proof / references
- **Composition + runtime repartition:** `poc/` Exp10 (`vp_natspart_test.go`).
- **Lossless reassignment across churn** (join / graceful leave / abrupt crash;
  single-node + 3-node cluster RF=3/R=3; + processing-gate variant), `-race` clean:
  `test/integration/fixedpartitions/` (Exp11).
- **Cooperative (no stop-the-world) rebalance** — per-message latency shows retained
  slots keep flowing during churn while only moved slots blip:
  `test/integration/fixedpartitions/` (Exp12, `TestFixedPartitions_NoStopTheWorld`).
- **Full analysis, three-way trade, cost model, naming, caveats:**
  [01-feasibility-assessment.md](01-feasibility-assessment.md).
- **Underlying mechanics:** the shared-cursor experiments `poc/` Exp1–9.
