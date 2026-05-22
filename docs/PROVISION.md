# Parti Provision Guide

> Manage the NATS resources Parti's runtime depends on using the `provision` SDK and `partictl` CLI.

**Related Documentation:**
- [Docs README](README.md) - Documentation map
- [Operations Guide](OPERATIONS.md) - Deployment, monitoring, troubleshooting
- [Configuration Guide](CONFIGURATION.md) - Runtime configuration options

---

## Table of Contents

1. [Overview](#overview)
2. [The Ownership Marker](#the-ownership-marker)
3. [Reconcile Policies](#reconcile-policies)
4. [partictl Commands](#partictl-commands)
5. [Brownfield Adoption Playbook](#brownfield-adoption-playbook)
6. [Safety Contracts](#safety-contracts)
7. [Partition Records](#partition-records)
8. [Application Streams](#application-streams)
9. [Dynamic Consumer Precreation](#dynamic-consumer-precreation)
10. [Force + Repair](#force--repair)
11. [Example Configuration](#example-configuration)

---

## Overview

The `provision` SDK and `partictl` CLI manage the NATS resources that
Parti's runtime manager depends on:

- **Control-plane KV buckets** — five buckets (`parti-stableid`,
  `parti-election`, `parti-heartbeat`, `parti-assignment`,
  `parti-handoff`) that the Parti manager uses for worker coordination.
- **The partition-source bucket** — the KV bucket that holds the
  partition definition record read by the runtime source.

`provision` manages application streams via a `streams:` block in the
config file and optionally precreates per-partition durable consumers via
the `partictl consumers` command (see
[Dynamic Consumer Precreation](#dynamic-consumer-precreation)). It
does not manage partition record contents directly. It provides:

- **Read-only inspection** — `view` lists all Parti-marked resources
  in the NATS account; `validate` checks a config file for static or
  live errors.
- **Drift detection and reconcile** — `plan` computes what is missing
  or drifted from desired config; `apply` executes the plan.

---

## The Ownership Marker

Parti stamps a set of metadata keys on every resource it manages.
These keys live in `jetstream.KeyValueConfig.Metadata` (not the
`Description` field):

| Key                    | Example value           | Meaning                                    |
|------------------------|-------------------------|--------------------------------------------|
| `parti.io/managed`     | `v1`                    | Schema version; non-empty means managed    |
| `parti.io/component`   | `control-plane:election`| Which Parti component owns this bucket     |
| `parti.io/instance`    | `prod`                  | Logical environment label (optional)       |

The marker is **informational only**. It never grants or revokes
mutation rights. Plan and Apply look up every resource by its exact NATS
bucket name; a bucket with no marker is not deleted or skipped — it is
reported as `adopted` drift so the operator sees it.

Non-Parti metadata keys (e.g. `Description: "owned by infra team"`, or
any other key not in the `parti.io/*` namespace) are always preserved
verbatim across all reconcile operations.

---

## Reconcile Policies

The `policy` field in `parti-env.yaml` (or the `-policy` CLI flag)
controls what actions `plan` and `apply` are allowed to take on existing
resources.

| Policy        | Create missing | Stamp marker | Update mutable fields | Destructive repair |
|---------------|:--------------:|:------------:|:---------------------:|:------------------:|
| `warn`        | yes            | no           | no                    | no                 |
| `adopt`       | no             | yes          | no                    | no                 |
| `safe-update` | yes            | no           | yes (marked only)     | no                 |
| `force`       | yes            | no           | yes (marked only)     | yes (gated)        |

### `warn` (default)

Creates any bucket named by config that does not exist live. Reports
drift on existing buckets — `drift-mutable` for fields that could be
reconciled in place, `drift-immutable` for fields that require
delete/recreate, and `adopted` for buckets that exist without the Parti
marker. Never mutates an existing resource.

This is the safe default for greenfield environments and for read-only
drift audits.

### `adopt`

Stamps the Parti ownership marker on every bucket named by config that
exists live and is currently unmarked. Non-Parti metadata keys on the
bucket are preserved. Creates no missing buckets and updates no
non-marker fields.

Use `adopt` as the first step when onboarding a pre-existing NATS
environment into Parti management.

### `safe-update`

Creates missing buckets (same as `warn`) and also reconciles
drift-mutable fields in place on **Parti-marked** buckets via
`js.UpdateKeyValue`. Fields that `safe-update` can reconcile:

| Field            | Applies to                   | Notes                                                   |
|------------------|------------------------------|---------------------------------------------------------|
| `Metadata`       | control-plane + partition-source | Parti marker keys updated; non-Parti keys preserved |
| `TTL`            | control-plane + partition-source |                                                     |
| `MaxValueSize`   | partition-source only        | `controlPlane` has no `maxValueSize` field              |
| `Replicas`       | control-plane + partition-source | Server enforces cluster-peer feasibility at apply time |

Fields `safe-update` **never** changes:

- `History` and `Storage` — changing these requires delete/recreate.
  Plan reports them as `drift-immutable`; repairing them requires
  `-policy force` with `allowDeleteRecreate: true` on the resource
  (see [Force + Repair](#force--repair)).
- `Description` and `MaxBytes` — no YAML field exists for these; they
  are preserved verbatim from the live bucket regardless of any other
  change in the same Apply.

Unmarked (adopted) buckets are **not** updated under `safe-update`.
Plan reports them as `adopted` drift; the operator must run `adopt`
first, then re-run `apply --policy=safe-update`.

### `force`

`force` is a strict superset of `safe-update`: it create-misses and
reconciles drift-mutable fields in place on Parti-marked resources
exactly as `safe-update` does, and additionally repairs a
drift-immutable resource by delete/recreate — but only when
**both** of the following are true:

1. The effective policy is `force`.
2. The resource's config sets `allowDeleteRecreate: true`.

Either condition alone leaves immutable drift reported but not
repaired. For detailed semantics, data-loss consequences, and operator
checklist, see [Force + Repair](#force--repair).

---

## partictl Commands

All commands accept common connection flags:

```
  -server   <url>       NATS server URL (default: $NATS_URL or nats://127.0.0.1:4222)
  -creds    <path>      NATS credentials file
  -nkey     <path>      NATS nkey seed file
  -token    <string>    NATS token
  -timeout  <duration>  Operation timeout (default: 30s)
  -json                 Emit machine-readable JSON output
```

### `partictl view`

Lists every Parti-marked KV bucket visible to the NATS account.

```
partictl view [flags]
  -f        <path>   YAML config (optional; scopes results to config-named buckets)
  -instance <name>   Filter by parti.io/instance (only when -f is absent)
```

Output is a `Snapshot` struct (`apiVersion: parti.io/provision/v1`, `kind: Snapshot`).

### `partictl validate`

Validates a config file without connecting to NATS (static check), or
with a live NATS preflight (reachability + per-bucket info probes).

```
partictl validate -f <path> [flags]
  -live    Perform live NATS validation (requires connectivity)
```

Exits 0 on success, 3 on validation error.

### `partictl plan`

Computes the desired-vs-live diff and the actions Apply would take,
without performing any mutations.

```
partictl plan -f <path> [flags]
  -policy       <policy>  warn | adopt | safe-update (default: warn or cfg.policy)
  -fail-on-drift           Exit 2 if non-informational drift is found
```

Output is a `PlanResult` struct (`kind: Plan`).

### `partictl apply`

Executes the plan actions against live NATS.

```
partictl apply -f <path> [flags]
  -policy       <policy>  warn | adopt | safe-update (default: warn or cfg.policy)
  -dry-run                Plan only — same output as plan
  -fail-on-drift           Exit 2 if drift is detected (dry-run only)
```

Output is a `Report` struct (`kind: Report`).

The `-policy` flag and the YAML `policy:` field must agree when both
are present. If they conflict, `apply` exits 3 with:

```
partictl apply: --policy=X conflicts with cfg.policy=Y in <file>. Pick one or remove the cfg.policy field.
```

Writing `policy: ""` in YAML (or omitting the field) is treated as
absent: no conflict with any `-policy` value.

### `partictl adopt`

Shorthand for `apply --policy=adopt`. Stamps the Parti marker on
unmarked buckets named by config.

```
partictl adopt -f <path> [flags]
  -dry-run   Plan only — emit the same output as plan --policy=adopt
```

`adopt` requires `-f`; there is no "adopt everything" mode. If
`cfg.policy` in the YAML is non-empty and not `adopt`, the command
exits 3 with the same conflict message as `apply`.

### `partictl partitions`

Manages the *contents* of the partition-source key — the partition table
itself — as opposed to the partition-source bucket config. See
[Partition Records](#partition-records) for the full workflow.

```
partictl partitions plan  -f <path> [flags]
  -fail-on-drift   Exit 2 if the declared table differs from the live key
partictl partitions apply -f <path> [flags]
  -prune     Allow removing records absent from the declared set
  -dry-run   Plan only — emit the same output as partitions plan
```

The `-policy` flag is not accepted: the reconcile policy governs bucket
config, not record contents.

### `partictl consumers`

Precreates per-partition durable consumers for `dynamicConsumers:` targets
that have opted in via `partitionsRef`. See
[Dynamic Consumer Precreation](#dynamic-consumer-precreation) for the full
workflow.

```
partictl consumers plan  -f <path> [flags]
  -fail-on-drift   Exit 2 if non-informational drift is found
partictl consumers apply -f <path> [flags]
  -dry-run   Plan only — emit the same output as consumers plan
```

The `-policy` flag is not accepted: precreation is policy-independent.

### Exit codes

| Code | Meaning                                                     |
|------|-------------------------------------------------------------|
| 0    | Success                                                     |
| 1    | Runtime error (operation failed after preflight)            |
| 2    | Drift detected with `-fail-on-drift`                        |
| 3    | Config parse / validation error, or flag conflict           |
| 4    | NATS connect / auth / context timeout failure               |

---

## Brownfield Adoption Playbook

For environments with pre-existing NATS buckets that were created
manually, by the NATS CLI, or by an older tool, every `plan` run reports
those buckets as `adopted` drift and emits no actions to resolve them.
The canonical two-step to take ownership is:

**Step 1 — Preview what adopt will stamp (optional but recommended):**

```bash
partictl adopt -f parti-env.yaml -dry-run
```

Review the `stamp-marker` actions in the output. Each action lists
`mergedMetadata` (the full metadata map that will be written) and
`partiKeys` (the keys the action adds or changes). Confirm no
unintended non-Parti key is being modified.

**Step 2 — Stamp the marker:**

```bash
partictl adopt -f parti-env.yaml
```

Each unmarked bucket named by config now carries the Parti ownership
marker. Non-Parti metadata keys on each bucket are preserved verbatim.

**Step 3 — Preview field-level drift (optional but recommended):**

```bash
partictl plan -f parti-env.yaml --policy=safe-update
```

After adoption, `plan --policy=safe-update` sees marked buckets and
reveals any field-level drift that was hidden under the `adopted`
finding — including `drift-immutable` findings for History or Storage
mismatches repairable under `-policy force` with `allowDeleteRecreate`
(see [Force + Repair](#force--repair)).

**Step 4 — Reconcile mutable drift:**

```bash
partictl apply -f parti-env.yaml --policy=safe-update
```

Reconciles TTL, Metadata, MaxValueSize, and Replicas in place on every
marked bucket that differs from config. Immutable drift is reported but
not acted on.

### Why two steps?

`adopt` and `safe-update` are deliberately separate:

- **Adoption is an explicit ownership transition.** The operator who
  runs `adopt` is declaring "Parti now manages these buckets." That
  decision should be visible in the operator's runbook, not absorbed
  silently into a broader reconcile pass.
- **`safe-update` never silently takes ownership of an unmarked
  bucket.** An unmarked bucket under `safe-update` continues to
  appear as `adopted` drift with no action emitted. This prevents
  accidental ownership of buckets that happen to share a name with
  a config-declared bucket.
- **Adoption reveals hidden drift.** After `adopt`, the next
  `plan` run shows field-level drift on newly-marked buckets,
  including immutable drift that needs operator attention. If
  `safe-update` absorbed adoption, that drift would be partially
  resolved while immutable findings surfaced with no clear
  before-state to compare.

---

## Safety Contracts

### Adoption is not approval of the bucket's config

Running `partictl adopt` stamps the Parti marker only. It does not
assert that the bucket's History, Storage, Replicas, TTL, or any other
field matches the desired config. After adoption, run
`plan --policy=safe-update` to reveal field-level drift —
including any drift-immutable findings that require manual intervention.

### `safe-update` preserves fields the YAML cannot express

The `update-kv` Apply path constructs its write target by copying the
just-re-read live snapshot and overwriting only the fields the YAML can
express (`TTL`, `Metadata`, `MaxValueSize`, `Replicas`). `Description`
and `MaxBytes` are **never** reset, even when other fields change in the
same Apply. Operators who set those fields out-of-band keep them.

### Per-field mutability: what safe-update reconciles and what it does not

`safe-update` reconciles drift-mutable fields: `Metadata`, `TTL`,
`MaxValueSize` (partition-source only), and `Replicas`. It does **not**
attempt to change `History` or `Storage` — those fields require
delete/recreate and are reported as `drift-immutable`. Repairing them
requires `-policy force` with `allowDeleteRecreate: true` on the
resource (see [Force + Repair](#force--repair)).

### Replicas changes are conditional on cluster size

A `safe-update` Apply that bumps `Replicas` against a NATS cluster with
fewer peers than the requested value will fail-fast. Apply records a
`ResourceError` with the underlying NATS error; remaining actions are
skipped with reason `prior-error`. The NATS server enforces peer-count
feasibility; the SDK does not pre-check it.

### Last-writer-wins on concurrent operators

NATS `UpdateStream` carries no compare-and-swap / expected-revision
token. The `update-kv` Apply path re-reads live state and verifies it
still matches the plan-time snapshot before writing (the stale-before
check). However, the window between the re-read and the write remains
open. Two operators running `safe-update` or `adopt` concurrently
against the same bucket are last-writer-wins. Operators who care about
cross-operator ordering serialize their own runs.

The stale-before check closes the **plan→apply** window (the gap
between when `plan` was run and when `apply` is run); it does not close
the re-read→write window within a single `apply` invocation.

---

## Partition Records

The commands above provision NATS *buckets*. The `partitions` command
manages the **contents** of the partition-source key: the partition table
the Parti runtime reads to know which partitions exist.

### Declaring the partition set

Add a `partitions:` list under `partitionSource:` in `parti-env.yaml`. Each
record has a `keys` list — the partition identity, one or more non-empty
strings with no dots or whitespace — and an optional `weight`, the relative
processing cost (default `0`, meaning the assignment strategy's default):

```yaml
partitionSource:
  bucket: parti-partitions
  key: partitions/v1
  partitions:
    - keys: ["orders", "0"]
      weight: 100
    - keys: ["orders", "1"]
      weight: 100
    - keys: ["audit"]
```

The bucket-provisioning commands (`plan`, `apply`, `adopt`) ignore
`partitions:` entirely — only `partictl partitions` reads it. An env file
that omits `partitions:` works unchanged for those commands; `partictl
partitions` reports an error (exit 3) when it is missing or empty.

### plan and apply

```
partictl partitions plan  -f parti-env.yaml
partictl partitions apply -f parti-env.yaml [-prune]
```

`partitions plan` reads the live partition-source key, diffs it against the
declared set by partition identity, and reports the record-level changes —
records **added**, **removed**, and **weight-changed**. It is read-only;
`-fail-on-drift` makes a non-empty diff exit 2.

`partitions apply` writes the declared table to the key as a single
compare-and-swap. `-dry-run` makes it behave exactly like `plan`.

### Removals require `-prune`

Adding records and changing weights apply freely. Removing a record — the
declared set omits a partition the live key still has — is gated: `partitions
apply` refuses the whole operation and exits 1 unless `-prune` is passed, the
same "no destructive default" posture as the bucket commands. The key is
rewritten atomically, so the refusal is all-or-nothing; there is no partial
apply.

Phase 3 has no surface for pruning the table to zero records: an empty or
omitted `partitions:` list is always rejected.

### The bucket must exist first

`partictl partitions` never creates the partition-source bucket. Run
`partictl apply -f parti-env.yaml` to provision the bucket, then `partictl
partitions apply` to publish records. A missing bucket exits 3 with a
message pointing back at `partictl apply`.

### Concurrency

`partitions apply` makes one compare-and-swap attempt. If another writer
changes the key between the plan re-read and the write, apply reports the
race and exits 1 — re-run `partitions plan`. A successful apply means its
CAS landed; it is not a guarantee the table stays converged against a
runtime writer that also updates the partition source. Operators who care
about cross-writer ordering serialize their own runs.

### Large partition tables

The partition table is stored gzip-compressed in a single KV value, so it
is bounded by the bucket's maximum value size. For a large table whose
lifecycle differs from the infrastructure config, keep the `partitions:`
list in its own file and assemble `parti-env.yaml` with a YAML include or a
templating step before invoking `partictl partitions`.

---

## Application Streams

The `streams:` block in `parti-env.yaml` declares the application
JetStream streams Parti's partition-aware consumers read from. The
bucket-provisioning commands (`plan`, `apply`, `adopt`) provision and
report these streams alongside the control-plane and partition-source
buckets — no extra command is needed when the config already runs
`partictl plan` / `apply`.

### Declaring the stream set

Add a `streams:` list to `parti-env.yaml`. Each entry maps to one
JetStream stream:

```yaml
streams:
  - name: orders
    subjects:
      - orders.>
    retention:    limits      # "limits" | "workqueue" | "interest"; default "limits"
    storage:      file        # "file" | "memory"; default "file"
    discard:      old         # "old" | "new"; default "old"
    replicas:     3           # 0 = server default (1)
    maxAge:       24h         # 0 = unlimited
    maxBytes:     0           # 0 = unlimited (stored as -1 by NATS)
    maxMsgs:      0           # 0 = unlimited (stored as -1 by NATS)
    description:  "Order work messages"
```

**`StreamCfg` fields:**

| Field         | Type            | Default    | Notes                                                            |
|---------------|-----------------|------------|------------------------------------------------------------------|
| `name`        | string          | required   | NATS stream name; must be unique within the config              |
| `subjects`    | []string        | required   | Subject patterns the stream captures; at least one required     |
| `retention`   | string          | `limits`   | `limits`, `workqueue`, or `interest`                            |
| `storage`     | string          | `file`     | `file` or `memory`                                              |
| `discard`     | string          | `old`      | `old` or `new`                                                  |
| `replicas`    | int             | 0          | 0 lets the NATS server choose (normalised to 1)                 |
| `maxAge`      | duration        | 0          | 0 means unlimited; negative values are rejected                 |
| `maxBytes`    | int64           | 0          | 0 means unlimited; NATS stores unlimited as -1 (normalized)    |
| `maxMsgs`     | int64           | 0          | 0 means unlimited; NATS stores unlimited as -1 (normalized)    |
| `description` | string          | ""         | Optional operator label                                          |

**`0` vs `-1` convention:** `maxBytes` and `maxMsgs` use `0` for
"unlimited" in config; the NATS server rewrites those zeros to `-1` in
the live stream. `plan` normalises `config 0` and `live -1` as
equivalent, so they never produce spurious drift. `maxAge` is different:
the server keeps `0` as the unlimited value and rejects negative
durations, so `maxAge: 0` maps directly to live `0` with no rewrite.

A config that omits `streams:` behaves identically to today — the field
is purely additive.

### plan / apply / adopt

The existing `partictl plan` / `apply` / `adopt` commands process
`streams:` entries automatically — no separate stream-specific command
is needed for the full-config workflow.

**`plan`** computes:
- A `create-stream` action for each declared stream that does not exist live.
- An `update-stream` action (under `safe-update`) for each Parti-marked
  stream whose mutable fields diverge from config.
- A `stamp-stream-marker` action (under `adopt`) for each declared stream
  that exists live without the Parti ownership marker.
- `application-stream` drift findings for every declared stream.

**`apply`** executes the plan actions: creates missing streams (under
`warn` and `safe-update`), reconciles mutable fields in place on
Parti-marked streams (under `safe-update`), and stamps the marker on
unmarked streams (under `adopt`).

**`adopt`** works the same as for KV buckets: it stamps the Parti
ownership marker on any declared stream that exists live and is
currently unmarked. It does not create missing streams or update fields.

**`view`** lists every Parti-marked application stream in the account
alongside the control-plane and partition-source buckets. Stream entries
appear in the `streams` array of the `Snapshot` output.

### partictl stream

`partictl stream` is a stream-scoped surface over the same SDK — useful
when an operator owns the application streams but not the control plane,
or wants to plan streams in isolation.

```
partictl stream view  [-f <config>] [-json] [-instance <name>]
partictl stream plan   -f <config>  [-json] [-fail-on-drift] [-policy <p>]
partictl stream apply  -f <config>  [-json] [-dry-run] [-policy <p>]
```

**`stream view`** — `-f` is optional. Without `-f`: inventory mode — the
instance filter comes from the `-instance` flag. With `-f`: the instance
filter comes from `cfg.Instance` (`-instance` is ignored). Either way
it lists every Parti-marked application stream in the instance —
`stream view` is an **instance-scoped inventory**, not a per-stream
lookup by config name. A config that names only some of the account's
marked application streams still sees all marked streams in the instance.

**`stream plan`** and **`stream apply`** — `-f` is required. Flags and
exit codes mirror the top-level `plan` / `apply` commands. `stream apply
-dry-run` aliases `stream plan`.

When `-f` is given, `stream` commands validate the full config file
(including any `controlPlane:` and `partitionSource:` sections) before
operating on the stream-only view, so a malformed non-stream section is
rejected with exit 3 rather than silently tolerated.

### Drift-immutable fields

`Storage` and `Retention` divergences classify as `drift-immutable` and
are never auto-reconciled, even under `safe-update`. The NATS server
rejects `file` ↔ `memory` storage changes on `UpdateStream`. Retention
is treated conservatively: Phase 4 classifies **every** retention
divergence as immutable — including `limits` ↔ `interest`, which the
server would accept — because retention is a fundamental stream property
and the `limits` ↔ `interest` update is consumer-replica-coupled (it can
fail until bound consumers are adjusted). The safe remediation for either
divergence is operator-driven delete/recreate via `-policy force` with
`allowDeleteRecreate: true` on the stream (see
[Force + Repair](#force--repair)). Plan reports these as
`drift-immutable`; apply leaves them untouched under any other policy.

The mutable fields `safe-update` reconciles in place: `Subjects`,
`Discard`, `Replicas`, `MaxAge`, `MaxBytes`, `MaxMsgs`, `Description`,
and the `managed` / `instance` marker fields. The NATS server is the
final authority — an update it rejects (e.g. a `Replicas` change on a
single-node cluster) surfaces as a `ResourceError` at apply time.

Fields not in the `StreamCfg` set (mirror, source, republish,
placement, per-subject limits) are preserved verbatim from the live
stream by `update-stream` and are never drift-classified.

### Subject coverage

Phase 4 provisions a stream with exactly the `subjects` declared in
config. It does **not** cross-check that those subjects cover the
partition subjects a `dynamicConsumers:` entry would need — verifying
subject coverage requires cross-referencing the partition set, which is a
future enhancement. Misconfigured subject coverage is not detected until
the consumer binds at runtime.

---

## Dynamic Consumer Precreation

The `consumers` command precreates the per-partition durable consumers that
Parti's runtime binds to an application stream. Before Phase 5, `provision`
only *alignment-checked* dynamic consumers; it never created them. With
precreation, operators can verify that all expected consumers exist before
workers start, and observe any missing consumers as explicit drift.

### Opting a target into precreation

Dynamic-consumer targets are declared under `dynamicConsumers:` in
`parti-env.yaml`. By default a target is **alignment-check only** — it is
inspected by `validate --live` but never created by `provision`. Setting
`partitionsRef` to the partition-source bucket name opts the target into
precreation:

```yaml
partitionSource:
  bucket: my-partitions
  key:    partitions
  partitions:
    - keys: ["orders", "0"]
    - keys: ["orders", "1"]
    - keys: ["audit"]

dynamicConsumers:
  - streamName:      orders
    consumerPrefix:  orders-consumer
    subjectTemplate: orders.{{.PartitionID}}
    partitionsRef:   my-partitions   # <-- opts this target into precreation
```

`partitionsRef` must equal `partitionSource.bucket` — it names the
partition-source the consumer's partition set is drawn from. A
`partitionsRef` that does not match is rejected at validation time (exit 3).
A target with an empty `partitionsRef` keeps its Phase 1 behavior: it is
alignment-checked by `validate --live` only and is never touched by
`consumers plan` / `apply`.

### plan and apply

```
partictl consumers plan  -f parti-env.yaml [-fail-on-drift]
partictl consumers apply -f parti-env.yaml [-dry-run]
```

`consumers plan` reads the current live state of each expected consumer and
reports the diff — which per-partition durable consumers are missing and
which already exist. `-fail-on-drift` exits 2 when any non-informational drift
is found; a fully-precreated consumer set emits only `informational` findings
and exits 0.

`consumers apply` runs `consumers plan` then creates every missing consumer.
`--dry-run` makes it behave exactly like `consumers plan`. The `-policy` flag
is not accepted; precreation is policy-independent (a consumer either exists
or is created — there is no warn / safe-update / adopt distinction for it).

The `provision` SDK exposes the same surface as `PlanConsumers` and
`ApplyConsumers`.

### The runtime-owns model — no ownership marker, no tunable management

The Parti runtime calls `js.CreateOrUpdateConsumer` on every worker start.
NATS **overwrites** the consumer's config on update (it does not merge). Phase
5 chooses the runtime as the single owner of a dynamic consumer's config.
Three deliberate consequences flow from that decision:

**No ownership marker on consumers.** Phases 1-4 stamp the Parti marker
(`parti.io/managed` / `parti.io/component` / `parti.io/instance`) in each
resource's `Metadata`. A consumer stamped by `provision` would have its
`Metadata` stripped the first time the runtime's `CreateOrUpdateConsumer`
ran without setting `Metadata` — producing an endless re-stamp loop (`apply`
stamps → worker restart strips → `plan` reports drift → `apply` stamps …).
Phase 5 therefore stamps **no marker on consumers at all.** `provision`
locates a Parti consumer the only way that is stable across runtime overwrites:
by its **deterministic durable name**, recomputed from config via the shared
`internal/dynamicbuild` package.

**No tunable management.** `provision` precreates consumers at runtime-default
tunable values (`AckWait`, `MaxDeliver`, `InactiveThreshold`, `MaxAckPending`,
`ConsumerReplicas`) and never updates them. There is no `update-consumer`
action. The source of truth for these tunables is the application's
`consumer.Dynamic` options, not a provisioning YAML; the runtime overwrites
them on every worker start. A live consumer whose tunables differ from any
value is **not** drift — the runtime owns tuning.

**Precreation is not a least-privilege enabler.** Because the runtime still
calls `CreateOrUpdateConsumer` unconditionally, precreation does **not** let
a runtime run without consumer-write permission. Phase 5's value is
**pre-flight readiness** (the consumers exist and are inspectable before
workers start) and **drift visibility** (`plan` reports missing consumers).

### The immutable-field contract

NATS rejects a `CreateOrUpdateConsumer` that changes certain fields on an
existing consumer. The ones reachable on a Parti dynamic (pull) consumer are:
`AckPolicy`, `DeliverPolicy`, `MaxWaiting`, and `MemoryStorage`. These cannot
be "owned by the runtime and overwritten later" — if provision precreates a
consumer whose immutable fields differ from the runtime's, the runtime's own
`CreateOrUpdateConsumer` on worker start **fails**.

Provision therefore precreates from `dynamicbuild.DefaultDynamicDefaults()` —
the same defaults a `consumer.Dynamic` with no options uses:

| Immutable field  | Precreated value              |
|------------------|-------------------------------|
| `AckPolicy`      | `AckExplicitPolicy`           |
| `DeliverPolicy`  | `DeliverAllPolicy` (hard-coded) |
| `MaxWaiting`     | `2`                           |
| `MemoryStorage`  | `false` (file storage)        |

**Operator responsibility:** a `consumer.Dynamic` configured with a non-default
`WithAckPolicy`, `WithMaxWaiting`, or `WithConsumerMemoryStorage(true)` must
**not** be opted into precreation. If such a consumer is already live,
`consumers plan` reports it as `drift-immutable` with the offending field named
in the finding detail — the misconfiguration is not silent.

### Honest scope — declared partition set, not the live table

A successful `partictl consumers apply` means the per-partition durable
consumers for the **declared** `partitionSource.partitions` set exist. It
does **not** certify that they match the live partition table the runtime will
read — the live table can be mutated independently
(`source.NatsKV.AddPartitions` / `RemovePartitions` / `Modify`), exactly as
Phase 3's `partitions apply` documents its own honest-scope limit.

The intended operator workflow:

```bash
# 1. Converge the live partition table to the declared set.
partictl partitions apply -f parti-env.yaml

# 2. Precreate per-partition consumers for that declared set.
partictl consumers apply -f parti-env.yaml
```

Run `partitions apply` first when live-table readiness is needed; then
`consumers apply` to precreate for the resulting set.

### The bucket and stream must exist first

`partictl consumers` never creates the partition-source bucket or the
application stream. Run `partictl apply -f parti-env.yaml` to provision both
before running `consumers plan` / `apply`. A missing stream exits 3 with a
message pointing back at `partictl apply`.

---

## Force + Repair

The `force` policy enables destructive repair of drift-immutable resources
(those where `plan` reports `drift-immutable` drift). The repair path is
deliberately gated at two independent layers — both must opt in before any
delete/recreate occurs.

### The two-layer gate

**Layer 1 — policy.** The effective reconcile policy must be `force`. Pass
`-policy force` on the CLI or set `policy: force` in `parti-env.yaml`.

**Layer 2 — per-resource opt-in.** Each resource struct has an
`allowDeleteRecreate` boolean. Delete/recreate of that resource happens only
when this field is `true`.

Both layers must opt in. Either layer alone — `force` policy with
`allowDeleteRecreate` omitted, or `allowDeleteRecreate: true` under any
other policy — leaves immutable drift reported but not repaired.

**Example: repair a partition-source bucket with a wrong `History` value:**

```yaml
# parti-env.yaml
policy: force

partitionSource:
  bucket: my-partitions
  key:    partitions
  history: 5            # desired — diverges from the live bucket's history: 1
  allowDeleteRecreate: true   # Layer 2: opt this bucket into delete/recreate
```

```bash
partictl plan  -f parti-env.yaml -policy force   # emits recreate-kv action
partictl apply -f parti-env.yaml -policy force   # deletes + recreates the bucket
```

`allowDeleteRecreate` is inert under any other policy (`warn`, `adopt`,
`safe-update`): adding it to the YAML never causes a delete unless the policy
is simultaneously `force`.

### Destructive consequences

> **Warning: the following operations are irreversible.**
>
> - **`recreate-kv`** deletes the KV bucket and **all entries it holds**.
>   For a partition-source bucket this means the live partition table is
>   erased; run `partictl partitions apply` afterwards to restore it.
>   For a control-plane bucket, all five control-plane buckets are treated
>   as a unit: if any one carries immutable drift and `allowDeleteRecreate`
>   is set on `controlPlane`, all five are deleted and recreated.
> - **`recreate-stream`** deletes the stream and **all messages it holds**.
>   JetStream **cascade-deletes every durable consumer bound to the stream**
>   — including consumers provision did not create.
> - **`recreate-consumer`** deletes the durable consumer and its
>   delivery/ack cursor — the recreated consumer starts with no position.
>   On the Parti runtime's next worker bind, the consumer's configured
>   `RecoveryStrategy` determines delivery resumption; `RecoverFromNew`
>   (skip messages published during the gap) is the strategy whose
>   semantics align with the recreate. The operator selects the strategy
>   on the `consumer.Dynamic` call — provision does not control it.

### Quiesce workers before running `apply -policy force`

`provision` does **not** check for a running cluster or live workers. It
will delete a resource out from under active consumers. Stop or quiesce
the workers that consume the affected resources before running
`apply -policy force`.

### Stale-plan safety — honest scope

At apply time, each `recreate-*` action re-reads the live state and
re-classifies the resource before deleting it. If the resource no longer
carries the immutable drift the plan recorded — a concurrent operator
already repaired it, or the live config now matches — the recreate is
skipped (`Raced: true` in the report) without deleting anything. This
re-read closes the realistic staleness window: a `force` plan minutes or
hours old that is re-applied after the environment has converged produces
no destructive action.

However, NATS exposes no compare-and-delete primitive — `DeleteKeyValue`,
`DeleteStream`, and `DeleteConsumer` are unconditional name-based
operations. The re-read cannot make the operation perfectly race-free:
**running two concurrent `force` applies against the same environment is
unsupported and an operator error.** Serialize apply runs.

### Interrupted-recreate recovery

A recreate proceeds as: re-read → re-classify → delete → create. The
**delete is the point of no return**. If the create step fails (a
permissions error, a quota limit, a context cancellation, an invalid
desired config), the resource is gone and the desired resource was not
created. The apply report records a `ResourceError` containing the sentinel
`ErrRecreateInterrupted`, and apply exits 1.

Recovery is to fix the persistent cause of the create failure and re-run
`apply`. Re-running is safe: a fresh `plan` sees the resource missing and
emits an ordinary `create-*` action — no `recreate-*`, no re-deletion. A
blind re-run will not fix a persistent cause (bad config, missing
permissions, quota exhaustion) — address those first.

---

## Example Configuration

```yaml
# parti-env.yaml
apiVersion: parti.io/v1
instance: prod
policy: warn          # the --policy flag must match this when both are given

controlPlane:
  workerIdTtl:       1h
  electionTimeout:   10s
  heartbeatTtl:      30s
  assignmentTtl:     0s  # 0 = no expiration
  replicas:          3   # 0 (default) lets NATS choose; 3 requires a 3-node cluster

partitionSource:
  bucket:       my-partitions
  key:          partitions
  storage:      file
  history:      1
  replicas:     3
  maxValueSize: 0     # 0 = no limit
  ttl:          0s    # 0 = no expiration
  partitions:         # read only by `partictl partitions`; bucket commands ignore it
    - keys: ["orders", "0"]
      weight: 100
    - keys: ["orders", "1"]
      weight: 100
    - keys: ["audit"]
```

### Minimal configuration (control-plane only, defaults for everything)

```yaml
apiVersion: parti.io/v1

controlPlane:
  workerIdTtl:     1h
  electionTimeout: 10s
  heartbeatTtl:    30s
```

Bucket names default to `parti-stableid`, `parti-election`,
`parti-heartbeat`, `parti-assignment`, and `parti-handoff`.
`policy` defaults to `warn`. `controlPlane.replicas` defaults to `0`
(NATS normalizes to 1 server-side).
