# Self-Healing Implementation Status

This file tracks per-PR delivery state. Updated 2026-05-23
(post-`KVBuckets.Replicas` follow-up).

See [`README.md`](./README.md) for the plan overview and
[`00-fix-plan.md`](./00-fix-plan.md) for the per-PR specs.

## Delivered (9 of 13 in-scope PRs + 1 follow-up)

All branches below have been pushed to `origin`. Each PR is branched
off `main`, not stacked — see "Merge ordering" below for the conflict
zones.

| Order | ID | Branch | Spec | Commit | Notes |
|---|---|---|---|---|---|
| P0.1 | F7 | `self-healing-p01-f7-conn-config` | [01](./01-pr1-spec.md) | `14d857e` | Docs + finite-MaxReconnect WARN |
| P0.2 | F8 | `self-healing-p02-f8-reconcile-guard` | [02](./02-pr2-spec.md) | `c25eb8c` | Source reconciler-disabled WARN |
| P0.3 | F10-B | `self-healing-p03-f10b-twophase-warning` | [03](./03-pr3-spec.md) | `6fbbb27` | Two-phase + no-gate WARN |
| P1.1 | F6-A | `self-healing-p11-f6a-source-unavailable-hook` | [04](./04-pr4-spec.md) | `715833c` | Source-bucket hook + metric |
| P1.2 | F3 | `self-healing-p12-f3-stableid-notfound` | [05](./05-pr5-spec.md) | `dee6d7d` | stableID NotFound → ErrClaimLost |
| P1.3 | F1 | `self-healing-p13-f1-epoch-fence` | [06](./06-pr6-spec.md) | `8017ae1` | Bucket-recreate detection |
| P2.1 | F9-A | `self-healing-p21-f9a-election-filestorage` | [07](./07-pr7-spec.md) | `375c517` | Election bucket FileStorage |
| P2.4a | F2 | `self-healing-p24a-f2-retry-envelope` | [08](./08-pr8-spec.md) | `908f181` | Retry envelope + restartWatcher wiring |
| P2.2 | F6-B | `self-healing-p22-f6b-partition-floor` | (in plan) | `8c072dc` | Calculator empty/shrunk floor |
| F/U  | —     | `self-healing-kvbuckets-replicas`              | (this STATUS) | `399dfb1` | `Config.KVBuckets.Replicas` + warn-on-mismatch helper |

Phase 0 (P0.1-P0.3) and Phase 1 (P1.1-P1.3) are complete. Phase 2
has the **three dominant fixes**: P2.1 (eliminates leadership-churn),
P2.4a (bounds the source watcher's infinite retry loop), and P2.2
(prevents reassign-to-zero thundering herd on a transient empty
partition observation).

### Follow-up: `Config.KVBuckets.Replicas`

A separate PR (not part of the original plan, but discovered during
P2.1 implementation when the plan's `m.cfg.Replicas` anchor was
found to be non-existent). Adds:

- `Config.KVBuckets.Replicas int` — JetStream stream-replication
  factor stamped onto Parti-owned KV buckets at create time. Defaults
  to 0 (legacy server-default behavior).
- `warnOnReplicasMismatch` helper — fires at `Manager.Start` when an
  existing bucket's Replicas differs from the requested value.
- Deliberately does **NOT** auto-reconcile Replicas. JetStream
  `UpdateStream` DOES support changing Replicas on an existing
  bucket (verified empirically against nats.go v1.50.0), but
  Replicas is HA-quality, not a correctness invariant like MaxAge.
  Silently rewriting an operator's bucket config could trigger
  expensive cross-node replication or mask a deliberate downsize.
  The warning is the operator-actionable signal; running
  `nats stream update KV_<bucket> --replicas=N` is the manual
  remedy.

Independent of P2.1; can land before or after. If P2.1 lands first,
the migration runbook should get a one-line addendum mentioning the
new config option as an alternative to pre-creating with
`--replicas=3`.

## Remaining (4 of 13 in-scope PRs)

Each needs its own session — none is mechanical reuse:

| Order | ID | Status | Reason |
|---|---|---|---|
| P2.4b | F2 | Not started | Envelope reuse on `claim_resolver` supervise loop. Site has nested supervisor + restart-reason classification — read carefully. |
| P2.4c | F2 | Not started | Envelope reuse on `monitorAssignmentChanges`. Different stop semantics; exhaustion must call `enterDegraded("assignment-watcher-exhausted")` per plan. |
| P2.4d | F2 | Not started | Envelope reuse on `partition_consumer.go` recovery loop. Larger surface; **P2.3's prerequisite**. |
| P2.3 | F5 | Not started | Stream-gone hook + checkpoint reset + stream-epoch generation fence. **HIGH risk** (three coordinated mechanisms; manual-ack late-ack defense). Depends on P2.4d. |
| P2.5 | F10-A | Not started | **Chaos reproducer first** (hard gate). Truncated `Keys()` defense + worker-set floor. |

Deferred to Phase 3 (post-promotion gated):
- P3.1 (F9-B) Lease-aware leader
- P3.2 (F4) In-process re-provision

## Merge ordering — IMPORTANT

The 10 branches are **not stacked**; they all branch from `main`. They
will conflict on shared files when merged. Merge in plan order to
minimize conflict resolution work:

```
plan-specs → P0.1 → P0.2 → P0.3 → P1.1 → P1.2 → P1.3 →
  P2.1 → kvbuckets-replicas → P2.4a → P2.2
```

Known overlap zones:

- `manager.go` — P0.1 (warn call), P0.3 (`capProcessingGateWarned`
  field + helper), P1.3 (epoch fence field + monitor wireup), P2.1
  (`warnOnOperationTimeoutVsElection` call) all add to disjoint
  regions; mostly mechanical merges.
- `manager_setup.go` — P0.1 adds `warnOnFiniteMaxReconnects`, P1.3
  adds `bucketEpoch` / `captureBucketEpoch` / `monitorBucketEpochs`
  helpers, P2.1 flips the election-bucket storage type + adds
  `warnOnOperationTimeoutVsElection`, `kvbuckets-replicas` stamps
  `m.cfg.KVBuckets.Replicas` in `ensureKVBucket` + adds
  `warnOnReplicasMismatch`. All in different regions of the file
  except `ensureKVBucket`, where `kvbuckets-replicas` adds two
  lines (the Replicas stamp + the mismatch warn call) — easy
  3-way merge.
- `config.go` — only `kvbuckets-replicas` touches it (adds the
  `Replicas` field to `KVBucketConfig`). No other PR conflicts.
- `source/nats_kv.go` — P0.2 adds `logWarn` helper; P1.1 adds the
  unavailable-hook scaffold + the same `logWarn` helper (the latter
  is the first to land); P2.4a replaces `restartWatcher`'s body and
  also adds a `logWarn` helper. **When merging after P0.2 or P1.1
  already provides `logWarn`, drop the duplicate from the later PR.**

The spec files (`01-pr1-spec.md` … `08-pr8-spec.md`) and this STATUS
live on the `self-healing-plan-specs` branch. Merge it first so
subsequent PR reviews can reference the specs in-tree.

## Workflow note

Per the project's `feedback_post_impl_review_workflow` memory pin, the
standard cycle is **spec → impl → /simplify → /codex:review (or
copilot post-impl) → squash on merge**. All 10 branches here have gone
through **spec → impl → make lint && make test (-race)** only. The
`/simplify` pass and external review are pending — recommended to
run as a batch before merging.

## Empirical findings discovered during implementation

Three non-obvious facts about the nats.go KV surface that the plan got
wrong (or didn't address), surfaced by reproducer probes:

1. **After `js.DeleteKeyValue`, no production call site returns
   `jetstream.ErrBucketNotFound`** (the plan's named error):
   - `kv.Get` → `nats.ErrNoResponders`
   - `kv.Watch` → `jetstream.ErrStreamNotFound`
   - `kv.Update` → `jetstream.ErrNoStreamResponse`
   - Only `js.KeyValue(...)` lookup returns `ErrBucketNotFound`.

   Pinned in memory: [[project-nats-kv-delete-surface]]. Affects
   the P1.1 (F6-A) and P1.2 (F3) classifiers — both ship with the
   empirically-correct error set.

2. **No `Config.Replicas` field exists in Parti's runtime config**
   (only in `provision.Config`). The plan's P2.1 instruction to set
   `Replicas: m.cfg.Replicas` was therefore not actionable; P2.1
   ships the storage-type change only. The `kvbuckets-replicas`
   follow-up branch adds `Config.KVBuckets.Replicas` as the
   operator-ergonomics fix (nested under `KVBuckets` rather than
   top-level because plain `Replicas` would be ambiguous with
   worker-pod replica notions).

3. **JetStream `UpdateStream` accepts post-create Replicas changes.**
   The library could in principle auto-reconcile Replicas on every
   `Manager.Start` the way it reconciles MaxAge for stableID and
   handoff buckets. The `kvbuckets-replicas` follow-up deliberately
   does NOT do this: Replicas is HA quality (not a correctness
   invariant like MaxAge), silently rewriting an existing bucket
   could trigger expensive cross-node replication the operator did
   not ask for, and the get-first contract ("pre-creation wins")
   is the safer default. The follow-up's `warnOnReplicasMismatch`
   helper surfaces divergence so operators can run
   `nats stream update KV_<bucket> --replicas=N` themselves when
   they want the change to take effect.

All three deviations are recorded in the respective per-PR spec or
commit-message bodies.
