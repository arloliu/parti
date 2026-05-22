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
8. [Example Configuration](#example-configuration)

---

## Overview

The `provision` SDK and `partictl` CLI manage the NATS resources that
Parti's runtime manager depends on:

- **Control-plane KV buckets** — five buckets (`parti-stableid`,
  `parti-election`, `parti-heartbeat`, `parti-assignment`,
  `parti-handoff`) that the Parti manager uses for worker coordination.
- **The partition-source bucket** — the KV bucket that holds the
  partition definition record read by the runtime source.

`provision` never manages application streams, dynamic consumers, or
partition record contents directly. It provides:

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
| `force`       | —              | —            | —                     | not yet supported  |

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

- `History` and `Storage` — changing these requires delete/recreate
  (destructive repair is not yet supported). Plan reports them as
  `drift-immutable`.
- `Description` and `MaxBytes` — no YAML field exists for these; they
  are preserved verbatim from the live bucket regardless of any other
  change in the same Apply.

Unmarked (adopted) buckets are **not** updated under `safe-update`.
Plan reports them as `adopted` drift; the operator must run `adopt`
first, then re-run `apply --policy=safe-update`.

### `force`

Reserved. Not yet supported.

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
mismatches that require destructive repair (not yet supported).

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
delete/recreate and are reported as `drift-immutable`. Destructive
repair is not yet supported.

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
