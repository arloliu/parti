# Label-Based Partition Assignment — Design Spec

- **Status**: v5 — external review clean (5 rounds; round 5 verdict
  "ready to implement", no findings; reports at
  `tmp/label-assignment_v1_review.md` … `tmp/label-assignment_v5_review.md`)
- **Date**: 2026-07-07
- **Target version**: v2.9.0 (additive minor)
- **Packages touched**: `types/`, `source/`, `provision/`,
  `internal/assignment/`, root (`config.go`, manager wiring, apply path),
  `internal/heartbeat/`, docs

## 1. Motivation

Weighted partitioning balances *expected* cost across workers, but a single
long-running task still occupies its worker serially and blocks the shorter
work queued behind it. Operators need to segregate task *classes* onto
dedicated worker pools.

Primary use case ("VIP partitions"): a subset of partitions must be served by
dedicated workers. The VIP set changes **at runtime** — operators rewrite the
partition list in the NATS KV source to promote/demote partitions, without
redeploying workers.

Typical deployment: multiple Kubernetes Deployments, each with its own label
set baked into pod config. All pods share one parti management plane (one
heartbeat/election/assignment bucket set, one partition list, one leader).

## 2. Requirements (decided)

| # | Decision |
|---|----------|
| R1 | Worker carries a **set** of labels; partition carries **one optional** label. Match rule: partition label ∈ worker's label set. |
| R2 | Worker labels are **fixed at process startup** (from config). Label changes happen via pod restart, which already triggers join/leave rebalances. |
| R3 | Empty-pool fallback: **spill after a grace window**. A labeled partition whose pool has no live worker is *parked* (deliberately unassigned) until the pool has been empty for a configurable grace duration, then spills to the fallback ladder. Grace `0` = spill immediately. |
| R4 | Unlabeled partitions go to **unlabeled workers only** by default (`dedicated` policy); configurable to **all workers** (`shared` policy). |
| R5 | Production partition list source is the **NATS KV source**; label edits arrive as full-list rewrites through the existing update path. No targeted label-patch API. |
| R6 | Assignment-policy configuration (grace, unlabeled policy) is **leader-side and must be fleet-uniform**, same contract as the existing `AssignmentStrategy` choice. Worker *labels* differ per deployment by design; assignment *policy* must not. |

## 3. Non-goals

- Key=value label selectors or expression matching (k8s-style). One flat
  string label per partition; flat string set per worker.
- Runtime mutation of a worker's label set (`Manager.SetLabels`).
- Cross-pool load balancing. Pools are isolation domains; balance within a
  pool is the configured strategy's job.
- Per-label or per-partition grace overrides. One global grace duration.
- A targeted "patch one partition's label" write API (R5).
- Persisting grace clocks across leader failover (see §8.4).

## 4. Data model changes

### 4.1 `types.Partition` (`types/partition.go`)

```go
// Label optionally pins this partition to workers that carry the same
// label (see Heartbeat.Labels). Empty means unlabeled: the partition is
// assigned according to the unlabeled-partition policy. Label is a
// routing hint, NOT part of partition identity: it does not participate
// in CanonicalID, HashID, Compare, or PartitionSetDigest.
Label string `json:"label,omitempty" yaml:"label,omitempty"`
```

- `Partition.Validate()` additionally validates a non-empty `Label` with the
  same charset rules as keys (non-empty, no dots, no whitespace), plus a
  length cap of 64 bytes (labels become metric label values; see §13). Empty
  label is valid (= unlabeled).
- **Copy/transform-path audit (load-bearing).** Several production paths
  reconstruct `types.Partition` field-by-field and would silently strip
  `Label`, breaking the VIP flow before `partitionsEqual` ever runs. Every
  one of these MUST preserve `Label`, each pinned by a label round-trip test:
  - `source/nats_kv.go:1274` `deepCopyPartitions` — builds
    `types.Partition{Weight: p.Weight}` + copied keys. Without the fix, a
    decoded labeled list is stripped before storage/comparison, so the
    calculator never sees labels at all.
  - `source/nats_kv.go:1327` `validateAndDedupe` — same construction; strips
    labels from every `Update`/`Modify`/`AddPartitions` write path.
  - `provision/partition_records.go:223` `clonePartition` (and via it
    `clonePartitions`, `diffPartitions`) — the in-repo provisioning writer
    would erase labels on any plan/apply round trip.
  - `provision/partition_records.go` `diffPartitions` — treats only `Weight`
    differences as "changed"; a label-only edit must surface as a change in
    plan output (exact change-record shape is an implementation-plan detail,
    but label changes MUST be visible in plans, not dropped).
  - `provision/apply_partitions.go:322` `partitionTablesEqual` — Weight-only
    equality; a label-only apply would be skipped as a no-op.

  Implementation rule: any code that copies a `Partition` must copy the
  struct value (`cp := p` then re-allocate `Keys`) rather than enumerating
  fields, so future field additions cannot regress this class again. A
  repo-wide `types.Partition{` sweep at implementation time confirms no
  other production copy sites exist (audit as of this spec: the five above;
  strategy/partcodec paths copy whole values or pass through JSON and are
  safe).
- **Identity is unchanged.** `CanonicalID`, `HashID`, `Compare`, and
  `PartitionSetDigest` deliberately exclude `Label`:
  - Same keys + different label = the *same* partition with a new routing
    hint, which is exactly the VIP-promotion semantics.
  - If a label edit does not change where a partition lands (it was already
    on a matching worker), **no ownership movement** occurs: identity digests
    stay equal, handoff diffing (key/HashID-based) sees no acquire/revoke,
    and no consumer detaches or attaches. Note the weaker-than-"no churn"
    phrasing: assignment payloads carry `Label` (uniform copy rule below),
    so a label-only edit still changes the worker's canonical payload bytes
    and `PayloadHash`, and the worker runs one apply+ack cycle that is a
    no-op at the consumer layer. A test pins "no consumer detach/attach on
    label-only promotion of an already-matching partition".
  - Dedup in `source.validateAndDedupe` (by `CanonicalID`) still rejects two
    entries with the same keys, regardless of labels — correct, since they
    would be one partition with two contradictory hints.

### 4.2 `types.Heartbeat` (`types/heartbeat.go`)

```go
// Labels is the worker's label set, fixed for the process lifetime.
// Sorted and deduplicated at publish time. Empty for unlabeled workers
// and for pre-label workers (additive JSON field).
Labels []string `json:"labels,omitempty"`
```

- Published on every beat by `internal/heartbeat/publisher.go`.
- `SchemaVersion` stays 1: the field is additive JSON; old leaders ignore it,
  old workers omit it (decodes as nil = unlabeled). No capability bit — this
  is routing data, not a safety mechanism the audit path must gate on.
- **No new KV bucket.** Labels ride the existing heartbeat channel and
  inherit its liveness semantics: a label set is only ever read from a live
  heartbeat.
- Why not attach labels to the stable ID claim: stable IDs (`worker-0`, …)
  are claimed from a pool **shared across deployments**, so a given ID can
  belong to a "vip" pod today and a general pod after restart/takeover.
  Labels keyed to the stable ID would go stale; labels in the live heartbeat
  are correct by construction.

### 4.3 Wire additions: labels-of-record and parked metadata

`WorkerLabels` records the label set the leader read from this worker's
heartbeat when computing the assignment. It powers the worker-side
stale-incarnation guard (§9): a worker whose own labels differ from the
labels-of-record knows the payload was computed for a different process
incarnation behind the same stable ID.

It must live on the object commit-path workers actually fetch. Commits point
workers at content-addressed `AssignmentPayload`s (`types/assignment_commit.go:25-34`,
currently `SchemaVersion` + `Partitions` only); `buildAssignmentFromCommit`
synthesizes the runtime `Assignment` from `payload.Partitions` plus commit
metadata (`manager_assignment.go:1212-1220`). A field on `types.Assignment`
alone would therefore never reach commit-path workers. So:

```go
// AssignmentPayload gains:
// WorkerLabels      []string `json:"worker_labels,omitempty"`       // labels-of-record
// WorkerLabelsKnown bool     `json:"worker_labels_known,omitempty"` // presence bit
```

- **`WorkerLabelsKnown` is required because `omitempty` alone cannot
  distinguish "computed for an unlabeled worker" from "computed by a
  pre-label leader"** — an empty `[]string` marshals as absent, so a labeled
  pod taking over an unlabeled worker's ID would see the absent field and
  compat-apply a payload computed for a different label set (violating
  I11). Label-aware leaders ALWAYS set `WorkerLabelsKnown=true`, including
  for unlabeled workers (labels empty). Guard rule: `Known && !setEqual` →
  reject; `Known && setEqual` → apply; `!Known` → pre-label compat apply.
  This mirrors the existing `SourceRevisionKnown` presence-bit precedent
  (`types/partition.go` Assignment / `types/assignment_commit.go`).
- Both fields are part of the canonical payload bytes and therefore of
  `PayloadHash` (content identity) — two workers with identical partition
  sets but different labels-of-record are different payloads, which is
  correct. `AssignmentPayloadRef.SetDigest` stays partition-only.
  Compat note: the first commit published by an upgraded leader re-hashes
  every worker's payload (the presence bit enters the canonical bytes), so
  the fleet runs one apply+ack cycle with no ownership movement — same
  benign shape as a label-only edit (§4.1).
- `buildAssignmentFromCommit` copies `payload.WorkerLabels` and
  `payload.WorkerLabelsKnown` into the runtime `types.Assignment` (which
  gains the same fields for in-process use), and `buildLegacyAlias`
  (`assignment_publisher.go:1327-1341`) copies both onto the legacy alias
  envelope, so the guard sees labels-of-record with presence on **both**
  wire paths.
- The commit record gains parked-partition metadata (`ParkedCount`,
  `ParkedDigest` — §8.2).

All fields are additive JSON; old readers ignore them. Payload content-key
reuse across commits is preserved whenever partition set AND labels-of-record
are unchanged.

## 5. Configuration surface

New `Config` fields (`config.go`), following existing yaml/default/validate
conventions:

```go
// WorkerLabels is this worker's label set (R1/R2). Fixed at startup.
// Validated with partition-key charset rules; deduplicated and sorted.
// Caps: at most 16 labels per worker, each at most 64 bytes — bounds
// heartbeat payload growth and metric cardinality (§13).
WorkerLabels []string `yaml:"workerLabels" validate:"max=16,dive,max=64"`

// UnlabeledPartitionPolicy controls which workers receive unlabeled
// partitions (R4). "dedicated" (default): unlabeled workers only,
// falling back to all workers when no unlabeled worker is live.
// "shared": all workers. Leader-side; must be fleet-uniform (R6).
UnlabeledPartitionPolicy string `yaml:"unlabeledPartitionPolicy" default:"dedicated" validate:"oneof=dedicated shared"`

// LabelSpillGrace is how long a label's pool must be continuously empty
// before its partitions spill to the fallback ladder (R3). 0 spills
// immediately. Leader-side; must be fleet-uniform (R6).
LabelSpillGrace time.Duration `yaml:"labelSpillGrace" default:"60s" validate:"gte=0"`
```

Defaults preserve legacy behavior for unlabeled fleets (see invariant I1).

## 6. Leader-side label discovery

At each rebalance, after `getActiveWorkersFiltered` produces the active
worker-ID list, the calculator fetches the current heartbeat **values** for
exactly those IDs to learn label sets. Mechanism: a new
`WorkerMonitor.GetHeartbeatsFor(ctx, workerIDs)` that performs one bounded KV
`Get` per listed worker — unlike the existing `GetHeartbeats`
(`internal/assignment/worker_monitor.go:240`), it does not re-run the
`Keys()` scan the caller just performed.

- **Freshness over caching.** Labels are read fresh every rebalance. A
  long-lived `workerID → labels` cache is rejected: a stable-ID takeover can
  swap the process behind an ID without the heartbeat key ever expiring, so
  cached labels can be wrong within a TTL window. Rebalances are infrequent
  (join/leave/source-change/audit-driven), so N `Get`s per rebalance is
  acceptable.
- **Failure taxonomy first: `unknown` is only for isolated per-worker
  failures.** Broad failures must never be laundered into label decisions:
  if the heartbeat `Keys()` enumeration fails, or per-worker `Get` failures
  are classified connectivity/degrading-JetStream (`natsutil`), or more
  than max(1, 10% of the active set) of the `Get`s fail, the rebalance
  aborts and the error routes through the existing KV-error/degraded
  machinery (`recordKVError` → `KVErrorThreshold` → Degraded) — preserving
  the whole-bucket-loss contract (AGENTS.md cross-feature contract 1).
  Without this split, a heartbeat-bucket outage with a still-writable
  assignment bucket would read as "every worker's labels unknown" and,
  after confirmation, publish explicit empty assignments for the entire
  fleet — a mass revocation caused by unreadable labels, not by absent
  workers. At small fleet sizes the threshold degenerates soundly:
  max(1, 10%) = 1 for N ≤ 10, so one unreadable worker is isolated
  (unknown-label handling) and two of three is a broad failure (abort +
  KV-error path); tests pin both sides (§14).
- **Unreadable labels are `unknown`, never guessed.** If an isolated
  worker's heartbeat `Get` (after one bounded inline retry) still fails,
  its label set is *unknown* — NOT "unlabeled" and NOT its labels from a
  previous read. Both guesses misroute: a stale-ID takeover can change the
  labels behind a worker ID at any time, so a previous read may describe a
  dead process; and "unlabeled" would hand general work to what may be a
  dedicated worker under the `dedicated` policy. Unknown-label handling
  follows the defer-once-then-act rule (§8.5): the first rebalance
  observing an unknown-label worker defers (dedicated sentinel + label
  re-check timer, §8.3); if the read still fails on the re-check, the
  worker is excluded from every pool and receives an explicit empty
  assignment entry (I8 — no stale KV leak), with a warning log and metric.
  Its partitions redistribute; a later successful read re-homes them.
  Correctness over churn.
- Legacy workers (SchemaVersion 0 or a decoded heartbeat without the field)
  are unlabeled — that is a *successful* read of an empty set, distinct from
  unknown.

## 7. Assignment pipeline

The single strategy call at `internal/assignment/calculator.go:1707`
(`c.Strategy.Assign(workers, partitions)`) is replaced by a grouping layer.
The `types.AssignmentStrategy` interface is **unchanged**; the configured
strategy runs once per pool. (Alternatives rejected: extending the strategy
interface with worker metadata forces every custom strategy — see
`docs/STRATEGIES.md` — to reimplement label routing, and parking/grace is
stateful, which contradicts the stateless-strategy contract; a wrapper
strategy with an injected label lookup hides KV reads inside `Assign` and
breaks its determinism contract.)

```
inputs:  workers      []string            (active set, post-filter)
         labelsOf     map[string][]string  (from §6)
         partitions   []types.Partition    (source snapshot)
         policy       dedicated | shared
         grace        time.Duration

pools:
  workers with unknown labels (§6) are excluded from every pool below and
  receive explicit empty entries in the merge step (I8)
  for each label L: pool[L] = workers whose set contains L   (may overlap)
  unlabeledWorkers  = workers with empty label set
  generalPool       = policy==dedicated ? unlabeledWorkers : all workers
  fallbackPool      = unlabeledWorkers if non-empty, else all workers

groups:
  group[L]  = partitions with Label == L
  group[""] = unlabeled partitions

assign:
  group[""]:
    target = generalPool if non-empty, else all workers
    merge(Strategy.Assign(target, group[""]))
  for each label L in sorted order:
    if pool[L] non-empty:
        clear emptySince[L]
        merge(Strategy.Assign(pool[L], group[L]))
    else:   // empty-pool transition must first be CONFIRMED (§8.5)
        if emptySince[L] unset: emptySince[L] = now
        if now - emptySince[L] < grace:
            park group[L]                       // deliberately unassigned
        else:
            merge(Strategy.Assign(fallbackPool, group[L]))   // spill

merge contract (I8):
  result keys == active worker set, exactly — every worker gets an entry,
  empty slice when it received nothing. Overlapping pools concatenate.

prune:
  drop emptySince entries for labels absent from the current snapshot
  (a demoted/removed label must not leak a stale clock)
```

Notes:

- **Spill ladder prefers unlabeled workers** so a `vip` outage never invades
  a *different* label's dedicated pool. In an all-labeled fleet the ladder
  degenerates to "all workers".
- **Strategies must never see an empty worker list** (`ErrNoWorkers`,
  `strategy/round_robin.go:55`). Every `Assign` call above is guarded: pools
  are non-empty by branch condition; `generalPool`/`fallbackPool` fall back
  to all workers; the `len(workers) == 0` case exits the rebalance earlier
  (existing behavior at `calculator.go:1657`).
- **I8 is load-bearing**: the publisher updates only workers present in the
  assignments map and deletes only workers in `WorkersToRemove` — a live
  worker missing from the map would keep a stale assignment in KV
  indefinitely. Existing strategies already emit empty entries for all
  workers they were given (`round_robin.go:68`); the merge step guarantees
  it across pools, including labeled workers whose label has no partitions
  (reserved capacity, intentionally idle under `dedicated`).
- **Determinism**: pools and groups are built from sorted inputs, labels are
  processed in sorted order, and strategies sort internally. Same inputs
  (including clock-derived park decisions) → same output.
- **Dedicated reserves capacity even with zero labeled partitions.** With
  labeled workers and no labeled partitions, `dedicated` keeps the labeled
  workers idle by design (they are reserved for their class); `shared` uses
  them for general work. Operators who label workers before labeling
  partitions should expect this.
- Cross-pool weight balancing is out of scope (§3); `Partition.Weight`
  continues to balance *within* each pool, which satisfies the "N matched
  workers → split equally or by weight" requirement.

## 8. Parking and the grace window

### 8.1 Semantics

A parked partition is deliberately unassigned: no worker owns it, no consumer
attaches, and messages published to it queue durably in JetStream until
assignment resumes. Worst-case processing stall for a total pool outage:

```
heartbeat-TTL detection + LabelSpillGrace + rebalance debounce + handoff/attach
```

Parking only occurs when a label's pool is **completely empty**. Routine
rolling updates never park (k8s `maxUnavailable` keeps pods alive; survivors
absorb the label's partitions with no grace delay).

### 8.2 Coverage accounting and durable parked metadata

The assignment publisher enforces strict set-equality between assigned
partitions and the source set it is given, aborting the batch on mismatch
(`internal/assignment/assignment_publisher.go:337`, `ErrCoverageMismatch`).
Parking must be **explicit in that contract**, not smuggled in by shrinking
the source input: `PublishInput.SourcePartitions` keeps its documented
meaning ("pass exactly what was returned from the source snapshot",
`assignment_publisher.go:280-284`), and the calculator additionally passes
`ParkedPartitions`. The coverage check becomes:

```
assigned ∪ parked == source   AND   assigned ∩ parked == ∅
```

with any violation still aborting the batch. Rationale: the earlier draft
passed `eligible = snapshot − parked` as the source input, which silently
changed the durable meaning of `AssignmentCommit.BatchDigest` — documented
as matching *the source partition set's* digest at publish time
(`types/assignment_commit.go:111-114`, computed from the source input at
`assignment_publisher.go:406`) — so a crash after commit would leave no
durable record of which partitions were intentionally parked versus lost.

Instead, `BatchDigest` keeps its exact current semantics (full source set),
and the commit record gains additive fields:

```go
// ParkedCount is the number of partitions intentionally left unassigned
// (label pool empty, spill grace not yet expired) in this batch.
ParkedCount int `json:"parked_count,omitempty"`
// ParkedDigest is xxh3 over the sorted CanonicalIDs of the parked set
// (types.PartitionSetDigest). Zero when nothing is parked.
ParkedDigest uint64 `json:"parked_digest,omitempty"`
```

Every batch identity question is then answerable from the durable commit
alone: full source (BatchDigest), who got what (Payloads), what was parked
and how much (ParkedDigest/ParkedCount). The ack-audit path
(`calculator_audit.go`) is untouched — it classifies only `commit.Workers`
against payload refs and heartbeat digests, and parked partitions belong to
no worker.

The existing orphaned-partitions gauge (`calculator.go:1717-1725`) keeps its
meaning "accidentally unassigned": it compares assigned count against
`len(source) − len(parked)`, while parked counts are reported separately. An
accidental orphan is a bug signal; a parked partition is a policy outcome.

### 8.3 The label re-check timer (grace expiry AND deferral re-arm)

No existing event fires when a grace window lapses: if a pool empties and no
worker ever joins again, nothing else would ever re-run the rebalance, and
parked partitions would wait forever instead of spilling. The same holds for
§8.5 deferrals: the existing suspicious-observation sentinels are swallowed
as benign no-ops with no label-aware re-arm — `handleRebalance` treats both
sentinels as "keep cached assignment" no-ops (`calculator.go:1511-1524`),
the partition-lifecycle path re-arms only the *partition* sentinel and
explicitly relies on the worker-monitor poll to re-fire for worker-shape
issues (`calculator.go:779-797`), and that poll short-circuits when the
worker set is unchanged. A label-only source edit that creates an
empty-pool group would be deferred once and then **never re-observed** —
no worker topology change, no source change, nothing pending.

Therefore the calculator owns a **leader-only one-shot label re-check
timer**, armed whenever a rebalance ends with any of:

1. parked groups → delay = minimum remaining grace across parked labels;
2. a §8.5 deferral (first adverse observation: pool-empty transition or
   unknown-label worker) → delay = a short fixed re-check interval
   (implementation constant, ~5s) so the confirming observation is
   guaranteed to happen without any external event.

The deferral abort uses a dedicated sentinel (`errLabelObservationDeferred`,
same benign-no-op handling shape as the existing suspicious sentinels) so it
is never conflated with them and never depends on their re-arm behavior.

**Delivery path**: the timer does not call `rebalance` directly and does not
ride the worker-set change-check (which no-ops on an unchanged set,
`calculator.go:1052`). It funnels into the same `requestLabelRecheck`
entrypoint as the watcher label-change signal (§9): a sticky
`pendingLabelRecheck` flag plus a `TryClaimRebalancing("label_change")`
attempt, with the partition-lifecycle pending-retry semantics
(`calculator.go:823-846`, `882-932`). If the claim loses to a busy state
machine, a recovery-grace window, or cooldown, the flag persists and is
re-attempted on the existing drain tick and on the next timer fire; it is
cleared only when a rebalance actually completes. Consequently a
defer→confirm or park→spill transition cannot be lost to a one-shot timer
firing at the wrong moment. The timer is disarmed on leadership loss, on
calculator stop, and whenever a rebalance completes with nothing parked and
nothing deferred (the sticky flag, if set, still guarantees one more
recheck).

Per the repository's monitor-goroutine rule, this new timer path gets a
live-cluster concurrency stress test (see §14).

### 8.4 Grace clocks are per-leader-term

`emptySince` lives in calculator memory. On leader failover the new leader
starts fresh clocks, so a pool outage that straddles K failovers can stall up
to `(K+1) × grace` plus detection/handoff overhead. Accepted: persisting
clocks to KV buys little (failovers are rare, grace is short) and adds a
write path. Documented in operator docs with the sizing guidance: set grace
well below the affected class's latency SLO; 60s default covers pod
reschedule and leader failover blips.

### 8.5 Adverse-observation confirmation (defer once, then act)

The F10-A worker-shrink floor is **total-count** based
(`calculator.go:1240-1264`): losing 1 worker out of 11 is not suspicious
fleet-wide, yet it can be 100% of a label's pool. A transient heartbeat
`Keys()` omission of the only `vip` worker would otherwise park (and after
grace, spill) VIP partitions — revoking them from a still-alive worker —
without any of the existing shrink defenses engaging. The same shape applies
to a transiently unreadable heartbeat value (§6): acting on one bad
observation converts a read blip into churn.

Rule: **a disruptive label observation must be confirmed by two consecutive
rebalance-time observations before the calculator acts on it.** Two
triggers share the mechanism:

1. A label pool that was non-empty in the previous committed rebalance is
   observed empty.
2. A worker's labels are unknown after the bounded inline retry (§6).

On the first such observation the rebalance aborts with the dedicated
`errLabelObservationDeferred` sentinel and arms the label re-check timer
(§8.3), which guarantees the confirming observation without any external
event — the existing suspicious-observation machinery must NOT be reused
for re-arming, because it is swallowed without label-aware retry (see §8.3
citations). On the second consecutive observation the calculator proceeds
(park the pool / empty-assign the worker). A successful contrary
observation resets the counter. `emptySince[L]` starts at the *first* empty
observation, so confirmation does not extend the effective grace window.

Honest scoping: a single-worker phantom scan-loss already causes
reassignment on main today (only sharp fleet-level shrink is floored); this
gate does not fix that pre-existing exposure. What it guards is the *new*
label behaviors — parking (a service stop, worse than today's
reassign-and-keep-serving) and spill — from firing on one bad read.

## 9. Worker-side stale-incarnation guard

Leader-side grouping alone cannot enforce the match rule: the worker
startup/apply path applies whatever the current commit assigns to its worker
ID, with no label awareness. `applyInitialAssignment` reads the existing
commit and applies the payload for `m.WorkerID()` (`manager.go:693-716` →
`buildAssignmentFromCommit`), and stable IDs are claimed from a pool shared
across deployments. Failure case: `worker-0` was a `vip` pod with a
committed VIP assignment; a general pod later claims `worker-0`; it applies
the old payload and attaches to VIP partitions. Worse than a transient: in a
tight takeover the heartbeat key never lapses (the new process starts
beating before the old key's TTL expires), the leader observes **no
membership change, so no rebalance ever fires** — the misroute is unbounded,
not a window.

Guard: on every apply (initial and watcher-driven, commit path and legacy
alias path), the worker compares the payload's labels-of-record
(`AssignmentPayload.WorkerLabels`, propagated per §4.3) against its own
configured label set (sorted-set equality):

- **`WorkerLabelsKnown` && set-equal** → apply normally.
- **`WorkerLabelsKnown` && mismatch** → reject the payload: log at Warn with
  both label sets, skip the apply entirely (no consumer attach or detach),
  do not ack. The worker keeps heartbeating (its heartbeat carries its true
  labels) and stays on its current (empty, at startup) assignment.
- **`!WorkerLabelsKnown`** (payload from a pre-label leader) → apply, for
  compatibility. Label-aware leaders always set the presence bit — including
  for unlabeled workers (§4.3) — so this branch fires only for genuinely
  pre-label payloads, and under the documented rollout ordering (§11) those
  carry no labeled partitions.

**Rejection is a first-class outcome, not a failure.** The current control
flow would otherwise defeat the guard or spin: `applyInitialAssignment`
treats `buildAssignmentFromCommit` `ok=false` as "payload unverifiable" and
falls back to the legacy alias (`manager.go:706-727`) — and a pre-label
alias would compat-apply, bypassing the guard through the back door; a
returned apply error feeds the apply-retry machinery, which would retry a
payload that can never become applicable. So the spec requires a distinct
rejected outcome (e.g. `errLabelIncarnationRejected` / a third result
state) with exactly these semantics, on both the initial-apply and
watcher-driven paths:

- NO legacy-alias fallback (the alias carries the same labels-of-record per
  §4.3 and would be rejected identically; falling back to a *pre-label*
  alias would bypass the guard).
- NO `scheduleApplyRetry` — retrying the same commit is futile; convergence
  arrives as a NEW commit via the label-change trigger below, delivered by
  the existing assignment watcher.
- NO snapshot/LSR/ack advancement; heartbeating continues; a Warn log and a
  rejection metric fire.
- At startup this reads as "no applicable assignment yet": the worker stays
  in `WaitingAssignment` and the startup watchdog semantics below apply.

The check is **incarnation identity, not partition-label matching**: it asks
"was this payload computed for the process I am?", never "do these
partitions match my labels?". Deliberate spill (R3) places labeled
partitions on non-matching workers *with* correct labels-of-record, and the
guard must not second-guess that placement — filtering partitions
worker-side would break spill.

Convergence after a reject — the **label-change trigger** (primary
mechanism): the worker monitor's heartbeat watcher already receives the full
KV entry, value included, on every heartbeat PUT
(`worker_monitor.go:processWatcherEvents`, entries from `watcher.Updates()`).
Today a PUT on a recently-seen key is suppressed as a refresh ("worker was
continuously alive; skip the check") — which is precisely the tight-takeover
hole: the new process beats the same key, so nothing ever fires. The fix:
the watcher decodes each PUT's heartbeat and keeps a per-key label
fingerprint alongside its existing `lastSeen` session state; a PUT whose
labels differ from the fingerprint signals a label change.

**The signal must NOT ride the existing worker-set change-check.** The
watcher's debounced callback lands in `pollForChanges` → `observeAndDecide`,
which short-circuits to a no-op when the worker-ID set is unchanged
(`calculator.go:1052` `if !changed { return pollActionNone, nil }`) — and in
a tight takeover the set IS unchanged, so the edge would be swallowed.
Instead the monitor exposes a separate label-change notification (new
callback), and the calculator routes it — together with the §8.3 re-check
timer — through one entrypoint:

```
requestLabelRecheck(reason):
  set sticky pendingLabelRecheck flag (with reason)
  attempt TryClaimRebalancing("label_change")   // partition-lifecycle style
  if the state machine is busy / in recovery grace / cooling down:
      flag persists; re-attempted by the existing drain tick and by the
      §8.3 timer — cleared ONLY when a rebalance actually completes
```

This reuses the pending-retry semantics the partition-lifecycle path already
has (`calculator.go:823-846`, `882-932`) rather than the worker-set path, so
a label recheck can never be lost to the unchanged-set short-circuit or to
an inopportune busy state. The claimed rebalance is a full standard
rebalance: fresh worker enumeration, fresh label reads (§6), republish with
correct labels-of-record — which the rejecting worker then applies.
Properties:

- Zero extra IO (values already arrive on the watch); cost is one small-JSON
  decode per heartbeat PUT.
- Independent of apply mode, capability bits, and audit configuration.
- **Fingerprint state survives watcher restarts** (level-triggered, not
  edge-triggered). Session-local state — like the existing `lastSeen` map,
  rebuilt from each session's initial replay
  (`worker_monitor.go:381-396`) — would lose the only label-change edge if
  the takeover PUT lands while the watch is closed/backing off
  (`worker_monitor.go:328-360`): the new session would seed its
  fingerprints from the already-new values and no subsequent beat would
  differ. So the fingerprint map is owned by the monitor for the leader's
  lifetime, and on every session (re)establishment the initial replay is
  **compared against the retained fingerprints** — any label difference
  fires `requestLabelRecheck("watcher_restart")`. Keys first seen in a
  replay (no retained fingerprint) seed silently: a genuinely new worker is
  a join, already covered by the worker-change path. The map is cleared on
  leadership loss. (The simpler alternative — an unconditional
  `requestLabelRecheck` after every watcher re-establishment — is correct
  but fires full rebalances exactly when NATS is flaky and watchers churn;
  the comparison variant is precise for the same guarantee.)
- The leader-failover race is covered independently: a newly-elected
  leader's calculator always performs an immediate initial assignment
  (`calculator.go:346-351`, `cold_start_immediate`/`takeover_immediate`),
  which reads labels fresh.

The ack-audit is explicitly **not** relied on: in direct mode (the default,
`config.go:502` `EnableTwoPhaseHandoff=false`) audit escalation is Warn-only
(`calculator_audit.go` direct-mode branch), and even in two-phase mode
escalation is gated on worker capability bits and an eligible target set. A
rejecting worker also never acks, so where audit-repair IS enabled it acts
as a supplementary net; the spec's convergence bound comes from the
label-change trigger (watcher debounce + rebalance latency), not from audit
cadence.

During the reject window the partitions are unserved (messages queue
durably) — strictly preferable to running VIP work on a wrong-class pod,
where the processing gate would let a long task pin the partition for its
full duration. If the window exceeds `StartupTimeout` the startup watchdog
fires Degraded(`startup-timeout`), which is a correct, observable signal,
not a defect.

Operational complement: `WorkerIDPrefix` is already per-manager config
(`config.go:384`). Giving each deployment a distinct prefix (e.g. `vip`,
`worker`) makes cross-deployment stable-ID takeover structurally impossible
and is the **recommended deployment pattern** in the docs. The guard remains
required for the residual case (a deployment's labels changed across a
rollout while keeping its prefix) and as defense in depth.

## 10. Change propagation (load-bearing for the VIP flow)

`source/nats_kv.go`'s `partitionsEqual` (line 1285) decides whether an
applied KV entry changed the partition state and therefore whether watchers
are notified. It currently compares **Keys and Weight only**. Without a fix,
a label-only rewrite — the primary VIP promotion operation — would decode,
compare "equal", set `changed=false`, and **never notify the leader**: the
promotion would be silently swallowed until an unrelated event rebalanced.

Fix: `partitionsEqual` also compares `Label` (same pattern as the existing
`Weight` comparison). This covers both the watch path and the reconcile
path, since both funnel through `applyLocalLocked`. **Necessary but not
sufficient**: `applyLocalLocked` deep-copies through `deepCopyPartitions`
before comparing/storing, so without the §4.1 copy-path fixes both sides of
the comparison are already label-stripped and the label-aware comparison
sees nothing. The regression test must therefore exercise the full path:
label-only KV update → decode → store → change notification fires → leader
rebalances (§14), not `partitionsEqual` in isolation.

The static source (`source/static.go`) carries `[]types.Partition` verbatim
and needs no change beyond the shared `Validate()` update.

## 11. Compatibility and rollout

Additive minor (v2.9.0). JSON compatibility in both directions:

| Fleet state | Behavior |
|---|---|
| Old leader, any workers/partitions | Labels ignored entirely; legacy assignment (label-blind, no parking). |
| New leader, old (label-less) workers | Workers treated as unlabeled; correct per R1. |
| New leader, labeled partitions, no labeled workers | Park → grace → spill ladder; degenerates to legacy placement after grace. |
| No labels anywhere | Invariant I1: pipeline degenerates to a single `Assign(all, all)` — assignment output identical to today. |

Two **documented rollout-ordering rules** (operator docs + CHANGELOG):

1. **Upgrade every deployment before labeling anything.** Any pod can win
   leadership; a mixed fleet flips between labeled and legacy assignment on
   failover.
2. **Upgrade every partition-list writer first.** A writer built against the
   old `types.Partition` drops the `Label` field on a full-list rewrite,
   silently demoting all VIP partitions (the new label-aware
   `partitionsEqual` will faithfully propagate that stripped list as a real
   change). This explicitly includes the in-repo `provision` SDK/CLI, whose
   clone/diff/equality paths are part of the §4.1 fix list — an old provision
   binary is a label-stripping writer.

Policy uniformity (R6) is documented alongside the existing "all managers
must configure the same strategy" contract.

Recommended (not required) deployment pattern: give each Deployment a
distinct `WorkerIDPrefix` (`config.go:384`). This makes cross-deployment
stable-ID takeover structurally impossible, shrinking the incarnation
guard's job (§9) to the rare same-deployment relabel case, and makes worker
IDs self-describing in logs and metrics (`vip-3` vs `worker-3`).

## 12. Invariants

| # | Invariant |
|---|---|
| I1 | With zero labels (no labeled partitions, no labeled workers), assignment output is identical to the pre-feature pipeline for the same inputs. |
| I2 | Every source partition is either assigned to exactly one worker or explicitly parked; parked requires: its label's pool is confirmed empty (§8.5) AND grace has not expired. The publisher enforces `assigned ∪ parked == source` and `assigned ∩ parked == ∅` per batch, and the commit durably records the parked set (`ParkedDigest`/`ParkedCount`). |
| I3 | `Label` never affects partition identity: `CanonicalID`, `HashID`, `Compare`, `PartitionSetDigest` are label-blind. |
| I4 | A label-only change to the source list fires a change notification (new `partitionsEqual`). |
| I5 | Spilled partitions land on unlabeled workers whenever any exist; they never land on a different label's dedicated workers while an unlabeled worker is live. |
| I6 | When anything is parked, a leader-local timer guarantees a rebalance re-check no later than grace expiry (modulo debounce), without external events. |
| I7 | A worker's label set is immutable for its process lifetime and is only ever read from its live heartbeat. |
| I8 | Merged assignment map keys == active worker set, exactly (merge contract in §7 — no stale-assignment leaks, no phantom workers). |
| I9 | Each partition appears in exactly one label group, so per-pool strategy outputs merge without duplication and set-equality coverage holds. |
| I10 | `Label` survives every production copy/transform path (source deep-copy, validate/dedupe, provision clone/diff/equality — §4.1 audit list), each pinned by a round-trip test. |
| I11 | A worker never applies an assignment payload with `WorkerLabelsKnown=true` whose labels-of-record differ from its own configured label set — commit path AND legacy alias path (§9). Rejection triggers no alias fallback, no apply retry, and no ack; label-aware leaders always set the presence bit, so the compat branch fires only for pre-label payloads. |
| I12 | The calculator acts on a disruptive label observation (previously non-empty pool reads empty; worker labels unreadable) only after two consecutive rebalance-time observations (§8.5). |
| I13 | A change in the labels behind a live worker ID (stale-ID takeover) is detected from heartbeat PUTs by the leader's worker monitor — **including changes that land while the watch session is closed or restarting** (retained fingerprints vs initial replay) — and triggers a rebalance via `requestLabelRecheck`, a path that bypasses the unchanged-worker-set short-circuit and survives busy states via the sticky pending flag, independent of apply mode, capability bits, and audit configuration (§9). |

## 13. Observability

New metrics via an **optional extension interface** (type-asserted, so
existing `types.MetricsCollector` implementors don't break):

- parked partition count, per label (gauge)
- label pool size (workers per label, gauge)
- spill activations (counter, per label) and park→spill transitions
- unlabeled-policy fallback activations (unlabeled group served by all
  workers because no unlabeled worker was live)
- label-change triggers (counter: worker monitor observed a label change
  behind a live worker ID, §9) and incarnation-guard rejections (counter,
  worker-side)

Log lines at state transitions: pool empty (grace start), park, spill, pool
recovered (partitions re-homed), payload rejected by incarnation guard (§9),
worker labels unreadable (§6). Exact interface shape and metric names are an
implementation-plan decision, but **lifecycle semantics are part of this
spec**:

- Per-label gauges are recomputed and re-published on every completed
  rebalance; a label absent from the current source snapshot has its gauges
  explicitly zeroed/deleted in the same pass (no stale `vip` parked-count
  after the last `vip` partition is demoted — pairs with the `emptySince`
  prune step in §7).
- Metric label values are the validated label strings; cardinality is
  bounded by validation (§4.1 charset + 64-byte cap, §5 per-worker count
  cap) and by the operator-controlled label vocabulary. No derived or
  synthesized label values.
- Counters (spill activations, guard rejections) are monotonic per process;
  gauges are absolute per rebalance.

## 14. Testing plan

Unit (`internal/assignment/`, `types/`, `source/`):

- Pool/group construction; `dedicated` vs `shared` policy branches; spill
  ladder including the no-unlabeled-workers degenerate case; grace
  park→spill transitions with an injected clock; `emptySince` pruning for
  labels that disappear from the snapshot (§7 prune step); merge contract I8
  (workers in no pool get empty entries); determinism (repeated runs, same
  inputs → same map); I1 golden test (label-free inputs produce output
  identical to a direct `Strategy.Assign` call).
- `Partition.Validate` label rules; heartbeat encode/decode with labels
  (including legacy payloads).
- **Label round-trip regressions for every §4.1 copy path**:
  `deepCopyPartitions`, `validateAndDedupe`, provision
  `clonePartition`/`diffPartitions`/`partitionTablesEqual` (label-only edits
  must appear in plans and not be skipped as no-ops).
- **Label-only change propagation, full path** (§10): label-only KV update →
  decode → store → `changed` → notification observed. Both watch and
  reconcile paths (a `partitionsEqual`-only unit test cannot catch the
  deep-copy strip).
- Publisher-facing: `assigned ∪ parked == source` and disjointness
  enforcement (violation aborts with `ErrCoverageMismatch`); commit carries
  `ParkedDigest`/`ParkedCount`; a partition omitted from BOTH assignment and
  parked set fails coverage; orphan gauge vs parked accounting.
- Incarnation guard (§9) unit tests: `Known` + mismatch → reject + no ack;
  `Known` + set-equal → apply; `!Known` (pre-label payload) → compat apply;
  reject leaves prior state untouched. Exercised on BOTH wire paths: commit
  payload fetch (`AssignmentPayload.WorkerLabels`/`WorkerLabelsKnown`) and
  legacy alias envelope.
- Label-change trigger (§9): heartbeat PUT with changed labels on a
  recently-refreshed key escapes refresh suppression and reaches
  `requestLabelRecheck` — proven to force a rebalance **despite an unchanged
  worker-ID set** (the `calculator.go:1052` short-circuit must not swallow
  it); unchanged labels stay suppressed (scan-skip behavior preserved).
- Watcher-restart edge loss (§9): labels change while the heartbeat watch
  session is closed/restarting (heartbeat key never lapses, no source
  change, default direct mode); the retained-fingerprint vs initial-replay
  comparison fires `requestLabelRecheck` exactly once and the fleet
  converges without audit. Unit-level: replay with unchanged labels fires
  nothing; replay with a first-seen key seeds silently.
- Presence bit (§4.3): a new-leader payload for an unlabeled worker carries
  `WorkerLabelsKnown=true` and is rejected by a labeled reclaiming pod; a
  genuinely pre-label payload (bit absent) compat-applies; payload-hash
  change from the presence bit causes one no-op apply cycle and no ownership
  movement.
- Rejection control flow (§9): guard reject on the commit path does NOT fall
  back to a present legacy alias; does NOT schedule apply retries; worker
  stays unacked and applies the next label-correct commit.
- Label re-check under busy state (§8.3): timer fires while the calculator
  is Scaling/Rebalancing or in recovery grace; the sticky flag survives and
  the recheck eventually runs (defer→confirm and park→spill both complete).
- Payload identity: label-only edit changes `PayloadHash` (worker applies)
  but causes no consumer detach/attach for an already-matching worker
  (§4.1 no-ownership-movement claim).
- Heartbeat failure taxonomy (§6): whole-bucket loss / connectivity-classed
  failures abort the rebalance and route through the KV-error path (never
  publish empty assignments); an isolated single-worker Get failure defers,
  then empty-assigns only that worker. Small-fleet pins: 1-of-2 and 1-of-3
  Get failures are isolated unknowns; 2-of-3 is a broad failure that aborts
  and degrades.
- Defer-once-then-act (§8.5): first empty-pool observation defers, second
  consecutive parks; contrary observation resets; same for unknown-label
  workers; `emptySince` starts at first observation (confirmation does not
  extend grace). **No-external-event progression**: a label-only source
  edit creating an empty-pool group (label typo; no worker ever carries
  it) must progress defer → confirm → park → grace-expire → spill purely
  on the label re-check timer (§8.3), with no worker or source event.
- I3 label-blindness unit tests: `CanonicalID`, `HashID`, `Compare`,
  `PartitionSetDigest` each proven equal across differing labels; I7
  normalization test (labels sorted/deduped once at startup, read only
  from live heartbeats).

Integration (`test/integration/`, live NATS, `-race`):

- Runtime VIP promotion: rewrite KV list adding a label → partition moves to
  the labeled worker; demote → moves back.
- Pool outage lifecycle: stop all labeled workers → partitions park within
  detection window (no consumer attached, messages queue) → spill after
  grace → labeled worker returns → partitions re-home.
- Mixed fleet under both policies, including labeled-workers-idle
  reservation under `dedicated`.
- **Stale-incarnation takeover** (default config: direct mode, audit
  repair inert): the same stable ID is reclaimed by a worker with different
  labels while the old commit still assigns labeled partitions, with the
  heartbeat key never lapsing (tight takeover, no membership event). Assert
  the new worker never attaches those partitions (guard reject, no alias
  fallback, no apply-retry spin), and that the **label-change trigger**
  converges the fleet: fingerprint edge → `requestLabelRecheck` → rebalance
  → new commit → worker applies. A separate test proves audit escalation
  acts as a supplementary net when two-phase mode and worker capabilities
  are present.
- Mixed-version leader bouncing: old (label-blind) and new leaders alternate
  via failover with labeled partitions present; assignment flips between
  legacy and labeled placement but never loses coverage and never wedges.
- Source purge/delete while partitions are parked: partition-shrink guard
  behavior, `emptySince` pruning, parked gauges zeroed.
- Partial batch crash with parked partitions present: kill the leader
  between payload writes and commit; restart/audit must converge without
  treating parked partitions as lost.
- **Grace-timer concurrency stress test** per the repository
  monitor-goroutine rule (aggressive cadence, concurrent KV traffic, `-race`;
  template: `test/integration/manager/epoch_monitor_concurrency_test.go`).
- Every invariant I1-I13 maps to at least one named test; the implementation
  plan carries the explicit invariant→test matrix (the I3/I7 encodings above
  are already fixed by this spec).

Gates:

- This feature touches `internal/assignment/` and `source/` → `make pre-pr`
  (lint + unit `-race` + integration `-race`) before PR.
- The four cross-feature contracts in `AGENTS.md` are not intentionally
  touched (no error classification/routing changes), but their pinned tests
  run as part of the integration suite regardless.

## 15. Documentation

- New operator-facing section (README pointer + `docs/` page or STRATEGIES.md
  section): label model, VIP workflow, policy/grace tuning, rollout-ordering
  rules, worst-case-stall formula (§8.1), fleet-uniform-policy contract.
- Godoc for all new exported fields/config.
- CHANGELOG entry under v2.9.0.

## 16. Open items deferred to the implementation plan

1. Final metrics extension-interface shape and metric names (§13).
2. `GetHeartbeatsFor` exact signature and whether the audit path should share
   it; whether the watcher's per-PUT decode (§9) can feed the rebalance-time
   label read as a freshness hint (optimization only — correctness must not
   depend on it).
3. Whether config validation warns when `WorkerLabels` is set but the
   feature is otherwise unused (parallel to the existing inert-config
   startup WARN precedent).
4. Docs placement (new page vs STRATEGIES.md extension).
5. Provision plan/apply change-record shape for label edits (must be
   visible; exact type is plan detail — §4.1).
6. Whether the §8.5 confirmation counter needs its own config knob or a
   fixed value of 2 suffices (spec default: fixed 2).
7. The §8.3 deferral re-check interval constant (spec default: ~5s;
   bounded below by the rebalance debounce).
