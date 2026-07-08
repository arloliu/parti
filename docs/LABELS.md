# Label-Based Partition Assignment

> Route specific partitions to specific worker pools — dedicated capacity for
> a task class, without a second management plane.

**Related Documentation:**
- [Docs README](README.md) - Documentation map
- [Strategies & Sources](STRATEGIES.md) - Assignment strategies (labels route *into* a strategy, not around it)
- [Configuration Guide](CONFIGURATION.md) - Configuration options
- [Lifecycle](LIFECYCLE.md) - Worker states, handoff, degraded mode
- [Operations Guide](OPERATIONS.md) - Metrics, troubleshooting
- [Kubernetes Guide](KUBERNETES.md) - Deployment patterns (one Deployment per label pool)
- [Provision Guide](PROVISION.md) - `partitions:` records, including labels

---

## Table of Contents

- [Label-Based Partition Assignment](#label-based-partition-assignment)
  - [Table of Contents](#table-of-contents)
  - [Overview](#overview)
  - [The model](#the-model)
  - [Configuring labels](#configuring-labels)
    - [Worker labels](#worker-labels)
    - [Assignment policy (fleet-uniform)](#assignment-policy-fleet-uniform)
  - [The VIP promotion workflow](#the-vip-promotion-workflow)
  - [Parking and spill](#parking-and-spill)
    - [Worst-case stall](#worst-case-stall)
    - [Grace clocks reset on leader failover](#grace-clocks-reset-on-leader-failover)
  - [The stale-incarnation guard](#the-stale-incarnation-guard)
  - [Rollout rules](#rollout-rules)
  - [Recommended pattern: one `WorkerIDPrefix` per deployment](#recommended-pattern-one-workeridprefix-per-deployment)
  - [What operators see](#what-operators-see)
  - [Gotchas](#gotchas)

---

## Overview

Weighted assignment (`Partition.Weight`, see [Strategies](STRATEGIES.md)) balances
*expected* cost across a single worker pool, but it can't stop one long task
from occupying a worker while shorter, unrelated work queues up behind it on
the same box. When a subset of partitions needs their own dedicated capacity
— a "VIP" tier, a latency-sensitive tenant, a GPU-backed task class — labels
let you segregate them onto a separate worker pool inside the *same* Parti
management plane: one heartbeat/election/assignment bucket set, one partition
list, one leader, but two (or more) isolation domains.

The common deployment shape is multiple Kubernetes Deployments — say `general`
and `vip` — each running the same binary with a different `WorkerLabels`
value baked into its pod config. The VIP set is not static: operators promote
and demote partitions between pools at runtime by rewriting the partition
list, without redeploying or restarting any worker.

## The model

Two independent, deliberately simple primitives:

- **`types.Partition.Label`** — one *optional* string label per partition.
  Empty means unlabeled.
- **`types.Heartbeat.Labels`** — a worker's label *set*, published on every
  heartbeat. Fixed for the lifetime of the process: labels are read from
  config at startup, never mutated at runtime. Changing a worker's labels
  means changing its config and restarting it — which already triggers the
  normal join/leave rebalance machinery.

The match rule is a single membership test: **a labeled partition is
eligible for a worker only if the partition's label is a member of the
worker's label set.** There is no key=value selector language, no
expressions — one flat string per partition, one flat string set per worker.

`Label` is a **routing hint, not identity**. It is deliberately excluded from
`Partition.CanonicalID()`, `HashID()`, `Compare()`, and
`PartitionSetDigest()`. Two partitions with the same keys and different
labels are the *same* partition with a new routing hint — which is exactly
the VIP-promotion operation. The practical consequence: **relabeling a
partition that is already sitting on a worker that matches its new label
moves no ownership.** No consumer detaches, no consumer attaches — the
partition's identity never changed, so the handoff machinery (which diffs by
key/`HashID`) sees nothing to hand off. The worker does run one apply+ack
cycle for the updated payload bytes, but it is a no-op at the consumer layer.

Unlabeled partitions default to unlabeled workers only (see
[Assignment policy](#assignment-policy-fleet-uniform) below) — labeling a
pool of workers reserves them for their class even before any partition
carries that label.

## Configuring labels

### Worker labels

Set a worker's label set once, at construction:

```go
cfg := parti.DefaultConfig()
cfg.WorkerLabels = []string{"vip"}
mgr, _ := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash())
```

or, when several workers share one `Config` value (a common pattern in test
harnesses or when building N managers from one template) and need distinct
labels:

```go
mgr, _ := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(),
    parti.WithWorkerLabels("vip"),
)
```

`WithWorkerLabels` overrides `Config.WorkerLabels` when both are set.
`NewManager` validates, sorts, and deduplicates the set: each label follows
the same charset rules as a partition key (non-empty, no dots, no
whitespace), capped at 64 bytes; at most 16 labels per worker. An invalid set
makes `NewManager` return an error at construction time, not at some later
runtime surprise.

Labeling partitions uses the same `Partition.Label` field you already know
from `types.Partition`:

```go
partitions := []types.Partition{
    {Keys: []string{"tenant-acme"}, Label: "vip"},
    {Keys: []string{"tenant-widgets"}}, // unlabeled — general pool
}
```

`Partition.Validate()` applies the identical charset and length rules to
`Label`.

### Assignment policy (fleet-uniform)

Two `Config` knobs govern *how* the leader routes labeled and unlabeled
work. Both are **leader-side and must be identical across every manager in
the fleet** — exactly the same contract that already applies to the
configured `AssignmentStrategy`. Worker *labels* are meant to differ per
deployment; assignment *policy* is not.

| Field | Default | Description |
|---|---|---|
| `UnlabeledPartitionPolicy` | `"dedicated"` | `"dedicated"`: unlabeled partitions go to unlabeled workers only, falling back to all workers when no unlabeled worker is live. `"shared"`: unlabeled partitions go to any worker, labeled or not. |
| `LabelSpillGrace` | `60s` | How long a label's worker pool must be continuously empty before its partitions spill to the fallback ladder. `0` spills immediately. |

```yaml
unlabeledPartitionPolicy: dedicated
labelSpillGrace: 60s
```

With zero labels anywhere in the fleet (no labeled partitions, no labeled
workers), the whole pipeline degenerates to today's single
`Strategy.Assign(allWorkers, allPartitions)` call — every partition lands on
the same worker it would have landed on in a pre-label deployment. Enabling
labels never moves an unlabeled deployment's work. (The first commit an
upgraded leader publishes does re-encode payloads once with the new label
fields — a benign re-apply with no ownership movement; see
[Rollout rules](#rollout-rules).)

## The VIP promotion workflow

In production the partition list normally lives in the `source.NatsKV`
source (see [Strategies & Sources](STRATEGIES.md#natskv-source)). Promoting
or demoting a partition is a **full-list rewrite** through the existing
update path — there is no targeted "patch one partition's label" API.
`source.NatsKV.Modify` does a CAS-fenced read-modify-write, which is the
natural fit:

```go
// kv obtained as in the NatsKV Source guide (docs/STRATEGIES.md).
src := source.NewNatsKV(kv, "partitions", logger)

// Promote "tenant-acme" to the vip pool.
err := src.Modify(ctx, func(partitions []types.Partition) []types.Partition {
    for i := range partitions {
        if partitions[i].ID() == "tenant-acme" {
            partitions[i].Label = "vip"
        }
    }
    return partitions
})
if err != nil {
    log.Fatal(err)
}
```

The same rewrite works through the `provision` SDK/CLI's `partitions:`
records (`partictl partitions plan` / `apply`) — each record has an optional
`label:` field alongside `keys:` and `weight:`:

```yaml
partitionSource:
  bucket: parti-partitions
  key: partitions/v1
  partitions:
    - keys: ["tenant-acme"]
      label: vip
    - keys: ["tenant-widgets"]
```

`partictl partitions plan` reports label-only edits as a change (alongside
weight changes) — a relabel is never a silent no-op in plan output.

The leader detects a label-only rewrite as a real change (it is not
collapsed into "nothing changed" just because the partition's keys and
weight are untouched) and fires a rebalance. If the target pool has an
eligible worker, the promoted partition moves there on the next assignment;
if it doesn't, it parks — see below.

## Parking and spill

A **parked** partition is deliberately unassigned: no worker owns it, no
consumer attaches to it, and any messages published for it queue durably in
JetStream until assignment resumes. Parking is the leader's response to a
labeled partition whose pool has gone completely empty — every worker that
carries that label has left the fleet (scaled to zero, crashed, or simply
never existed yet).

Concretely: a `vip`-labeled partition whose only `vip` worker is drained
during a deploy does **not** immediately fall back to the general pool.
Instead the leader parks it — leaving it unowned rather than handing VIP work
to a general-purpose worker — for up to `LabelSpillGrace` (default 60s). If a
`vip` worker rejoins within that window, the partition is assigned to it with
no spill ever having happened. If the grace window fully elapses with the
pool still empty, the partition spills to the fallback ladder: it goes to an
unlabeled worker if one exists, or — only in an all-labeled fleet — to any
worker. A parked or spilled partition is never silently dropped from
accounting: the assignment commit records how many partitions were parked
and a digest of exactly which ones, so every batch is fully accounted for
(assigned ∪ parked always equals the source set).

Two properties worth calling out:

- **Routine rolling updates don't park.** As long as Kubernetes'
  `maxUnavailable` keeps at least one pod of the label alive during a
  rollout, the survivors absorb the label's partitions immediately — no
  grace delay, no parking.
- **A single bad observation never parks or spills anything.** Before the
  leader acts on "this pool just went empty" (or "this worker's labels are
  unreadable"), it requires the same signal on two consecutive rebalance
  passes. One transient blip is deferred and re-checked automatically within
  a few seconds — it does not extend the effective grace window — and a
  second, contrary observation cancels the deferral entirely. This exists
  specifically so a one-off heartbeat read glitch can't revoke a live
  worker's VIP partitions or trigger a spill that shouldn't happen.

### Worst-case stall

For a *total* outage of a label's pool (every worker carrying that label is
gone), the worst-case time before a labeled partition either resumes on a
replacement worker or spills to the fallback ladder is bounded by:

```
heartbeat-TTL detection  +  confirmation  +  LabelSpillGrace  +  rebalance / handoff
```

- **Heartbeat-TTL detection** — the leader must first notice the workers are
  gone, bounded by `Config.HeartbeatTTL`.
- **Confirmation** — the automatic two-observation check described above; a
  small, fixed, non-configurable delay (typically the next rebalance cycle —
  sub-second — with a bounded few-second retry if the leader happens to be
  mid-rebalance already).
- **`LabelSpillGrace`** — the configured grace window itself.
- **Rebalance / handoff** — the normal debounce and two-phase handoff or
  processing-gate latency any reassignment already pays.

Size `LabelSpillGrace` well below the affected class's latency SLO. The 60s
default is chosen to comfortably cover a pod reschedule or a leader failover
blip without spilling VIP work prematurely.

### Grace clocks reset on leader failover

The "how long has this pool been empty" clock lives in the leader's memory,
not in durable KV. A leader failover starts every grace clock fresh. In the
rare case where a pool outage straddles *K* consecutive leader failovers, the
worst-case stall extends to roughly `(K+1) × LabelSpillGrace` plus detection
and handoff overhead. This is a deliberate trade: persisting grace clocks
across failovers buys little in practice (failovers are rare and grace
windows are short) at the cost of a new durable write path. Size
`LabelSpillGrace` with this in mind if your fleet expects frequent leadership
churn.

## The stale-incarnation guard

Stable worker IDs (`worker-0`, `worker-1`, …) are claimed from a pool that
can outlive any single process — and, if two deployments share a
`WorkerIDPrefix`, a pool that can be claimed by a *different deployment's*
pod after a restart. Leader-side pool matching alone cannot prevent a
mislabeled apply in that scenario: if a `vip` pod holding `worker-0` is
replaced by a `general` pod that reclaims the same ID before the leader
notices any membership change, the general pod would otherwise apply the
stale VIP assignment it inherited and start processing VIP work on the wrong
class of machine — worse than a transient misroute, because nothing about
the worker *set* changed, so no rebalance would ever fire to correct it.

Every worker therefore checks, on every assignment apply, whether the
payload's labels-of-record (the label set the leader believed this worker
had when it computed the assignment) match the worker's own configured
labels:

- Labels match → apply normally.
- Labels are known and **don't** match → **reject** the payload outright: no
  consumer attaches or detaches, the worker does not acknowledge the
  assignment, and it keeps heartbeating (with its true labels) on its
  current assignment. This is a first-class, expected outcome, not an error
  to retry — retrying the same payload would be futile, since it can never
  become applicable to this incarnation.
- Labels are unknown (the payload came from a pre-label leader) → apply, for
  backward compatibility during a rolling upgrade.

Convergence after a rejection doesn't wait for a full audit cycle: the
leader's heartbeat watcher already sees every heartbeat's contents on every
publish, and a worker whose observed labels differ from what it last
published for that same worker ID triggers an immediate targeted rebalance,
independent of whether the *set* of live worker IDs changed at all. During
the (bounded) rejection window the partition's messages simply queue
durably — strictly preferable to a wrong-class worker processing them.

You do not need to configure or invoke this guard; it runs unconditionally
whenever any worker in the fleet carries labels. See
[What operators see](#what-operators-see) for the log line and metric it
emits, and the recommended pattern below for shrinking how often it can ever
fire.

## Rollout rules

Labels are additive and JSON-compatible in both directions — an old leader
ignores labels entirely (legacy assignment, no parking), and a new leader
treats old, label-less workers as unlabeled. That compatibility does **not**
extend to the two rules below; skipping either produces a silent failure
mode, not a loud error.

1. **Upgrade every deployment to a label-aware version before labeling any
   partition.** Any worker can win leader election. If even one deployment
   in the fleet is still running a pre-label version, a leadership handoff
   to that version flips the whole fleet back to legacy, label-blind
   assignment — with no warning — until leadership returns to a label-aware
   worker.
2. **Upgrade every writer of the partition list before relying on labels —
   including the `provision` CLI.** A writer built against an old
   `types.Partition` (or an old `provision` binary) drops the `Label` field
   on a full-list rewrite. Because label-aware leaders correctly detect this
   as a real change, the effect is not "no-op" but **active, silent
   demotion**: every VIP partition loses its label on the very next write
   from the stale tool, and the leader dutifully reassigns them to the
   general pool.

Both rules apply the same "fleet-uniform" discipline already documented for
the assignment-strategy choice: get every process onto compatible code
first, *then* turn the feature on operationally.

On the very first commit published by an upgraded leader, every worker's
payload gets one benign re-hash: the labels-of-record presence bit enters the
canonical payload bytes for the first time, which changes `PayloadHash` even
though no partition moved. Every worker runs one apply+ack cycle with no
ownership change as a result — expected, and harmless.

## Recommended pattern: one `WorkerIDPrefix` per deployment

Give each Deployment its own `WorkerIDPrefix` (e.g. `vip-0`, `vip-1`, … vs.
`worker-0`, `worker-1`, …) instead of sharing one prefix across deployments
with different label sets.

This makes cross-deployment stable-ID takeover **structurally impossible** —
a `general` pod can never claim an ID from the `vip` pool, because the pools
don't overlap. That shrinks the [stale-incarnation guard](#the-stale-incarnation-guard)'s
job down to the one residual case it exists to cover: a *single* deployment
relabeling itself across a rollout while keeping its own prefix. As a bonus,
worker IDs become self-describing in logs and metrics: `vip-3` identifies its
pool at a glance, where `worker-17` requires a lookup.

## What operators see

Label-aware observability is opt-in via `types.LabelMetrics`, an optional
extension interface. If your `MetricsCollector` also implements
`LabelMetrics`, the manager and calculator type-assert it and start
recording; if not, label mode still runs, it just isn't instrumented. All
methods are recomputed every rebalance, and a label that drops out of the
current partition/worker snapshot has its gauges explicitly zeroed rather
than left stale:

| Method | Kind | Meaning |
|---|---|---|
| `RecordLabelPoolSize(label, workers)` | gauge | Workers currently eligible for `label`. |
| `RecordParkedPartitions(label, count)` | gauge | Partitions of `label` currently parked. |
| `IncrementLabelSpill(label)` | counter | A partition of `label` spilled to the fallback ladder. |
| `IncrementLabelChangeTrigger()` | counter | A rebalance was triggered by a detected worker-label change (the incarnation-guard convergence path). |
| `IncrementLabelIncarnationReject()` | counter | A worker rejected an assignment payload computed for a different incarnation of its ID. |
| `IncrementUnlabeledFallback()` | counter | Under `dedicated` policy: an unlabeled partition had to be served by a labeled worker because the unlabeled pool was empty. Under `shared` policy this counter stays at zero in normal operation (unlabeled work landing on labeled workers is routine there, not a fallback) — it fires only in the degenerate case of no live workers at all. |

The gauges are leader-scoped: a leader that steps down zeroes every per-label
gauge it was reporting, so a deposed leader's metrics export doesn't freeze
at a stale non-zero reading.

Grep for these log lines when diagnosing a labeled deployment:

| Log line | Level | Where |
|---|---|---|
| `resolved worker labels` | Info | Once at manager startup, labeled workers only — confirms the label set this process actually applied (`WithWorkerLabels` vs. `Config.WorkerLabels`). Unlabeled workers do not emit it. |
| `worker label change detected` | Info | The leader's heartbeat watcher saw a worker publish a different label set under a worker ID it had seen before — the stale-incarnation trigger firing. |
| `rejecting assignment computed for a different incarnation of this worker ID` | Warn | The stale-incarnation guard fired on this worker. Logged with `payload_labels` and `worker_labels` so you can see the mismatch directly. |
| `startup: current commit is for a different incarnation; waiting for a label-correct commit` | Warn | Same guard, at startup, on the commit path. |
| `startup: current alias assignment is for a different incarnation; waiting for a label-correct assignment` | Warn | Same guard, at startup, on the legacy-alias path. |
| `initial assignment deferred pending label observation confirmation` | Info | The leader's first rebalance after startup saw a disruptive label observation and is waiting for the automatic second (confirming) observation before acting. |
| `label recheck requested` | Debug | The leader's label re-check timer or a label-change signal woke the rebalance path (with a `reason` field). Useful for confirming the grace-expiry or confirmation timers are actually firing. |

## Gotchas

- **Reserved-but-idle is by design.** Under the default `dedicated` policy,
  labeling a set of workers before labeling any partitions leaves those
  workers idle — they're reserved for their class, not folded back into
  general work. Use `shared` if you'd rather let idle labeled capacity absorb
  unlabeled work.
- **A spilled partition never invades a different label's pool.** The spill
  ladder always prefers unlabeled workers; it only reaches into "any worker"
  when the fleet has no unlabeled workers at all. A `vip` pool outage cannot
  spill into a separate `gpu` pool's dedicated capacity.
- **Custom `AssignmentStrategy` implementations need no changes.** Labels are
  handled entirely above the strategy interface — see
  [Strategies & Sources](STRATEGIES.md#assignment-strategies) for why. Your
  strategy still just receives a worker list and a partition list; it simply
  gets called once per label pool instead of once for the whole fleet.
- **`Partition.Weight` still balances *within* a pool.** Cross-pool weight
  balancing is not a goal — pools are isolation domains, not shared capacity
  to be split by weight.
