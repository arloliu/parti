# Parti Kubernetes Operator Guide

> Reconcile NATS infrastructure declaratively from a Kubernetes cluster using
> the `ProvisionedPartiEnv` custom resource.

**Related Documentation:**
- [Provision Guide](PROVISION.md) — provision SDK and `partictl` CLI for direct NATS resource management
- [Operations Guide](OPERATIONS.md) — deployment, monitoring, troubleshooting

---

## Table of Contents

1. [Overview](#overview)
2. [Non-Goals](#non-goals)
3. [Installing the Operator](#installing-the-operator)
4. [The ProvisionedPartiEnv CRD](#the-provisionedpartienv-crd)
   - [Spec Fields](#spec-fields)
   - [Status Fields](#status-fields)
5. [NATS Authentication](#nats-authentication)
6. [Worked Example](#worked-example)
7. [Operator Flags](#operator-flags)

---

## Overview

The Parti operator is a [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime)
controller that watches `ProvisionedPartiEnv` custom resources and reconciles
the NATS infrastructure each resource declares — control-plane KV buckets,
a partition-source KV bucket, and application JetStream streams — by driving
the same `provision` SDK that `partictl` uses.

The operator adds no provisioning logic of its own. Every reconcile cycle
maps the CR `Spec` to a `provision.Config`, connects to NATS using credentials
from a Kubernetes `Secret`, then calls `provision.Apply`. The `Status`
subresource records the outcome (the `Ready` condition, plan drift counts, and
apply executed/error counts) after every cycle.

The operator binary and the `ProvisionedPartiEnv` CRD live in a **nested Go
module** at `k8s/` (`github.com/arloliu/parti/v2/k8s`). The root
`github.com/arloliu/parti/v2` library module gains **zero** new dependencies —
the entire `controller-runtime` / `k8s.io` dependency tree is isolated in the
nested module.

---

## Non-Goals

> **Important — read before deploying.**

- **Deleting a `ProvisionedPartiEnv` CR does not deprovision any NATS
  resource.** The operator carries no finalizer. When you `kubectl delete` a
  `ProvisionedPartiEnv`, reconciliation stops; the NATS KV buckets and streams
  it managed are left intact. Destroying NATS resources requires manual action
  (the `partictl` CLI or the NATS CLI directly).

- **The operator does not manage partition records or dynamic consumers.**
  The CR `Spec` omits `partitions` and `dynamicConsumers` by design.
  Partition-table contents are managed by `partictl partitions`; per-partition
  consumer precreation is managed by `partictl consumers`. Both are one-shot
  operator-driven operations — reconciling them continuously would conflict
  with the Parti runtime's own ownership of those resources.

- **`policy: force` in a continuously-reconciling CR will delete and recreate
  resources when an immutable field is edited.** The operator applies
  `Spec.policy` verbatim on every reconcile. A CR with `policy: force` and
  `allowDeleteRecreate: true` on a resource will trigger a delete/recreate the
  next time the operator detects drift-immutable divergence — including an
  edit you just made to an immutable field like `storage` or `history`. Review
  the [Force + Repair](PROVISION.md#force--repair) section in the Provision
  Guide before setting `policy: force`.

---

## Installing the Operator

The operator ships plain YAML manifests under `k8s/config/`. Apply them in
the following order so each resource's dependencies exist first:

**Step 1 — Create the namespace:**

```bash
kubectl apply -f k8s/config/manager/namespace.yaml
```

This creates the `parti-operator-system` namespace that the operator's
`ServiceAccount` and `Deployment` live in.

**Step 2 — Install the CRD:**

```bash
kubectl apply -f k8s/config/crd/parti.io_provisionedpartienvs.yaml
```

**Step 3 — Apply RBAC:**

```bash
kubectl apply -f k8s/config/rbac/serviceaccount.yaml
kubectl apply -f k8s/config/rbac/role.yaml
kubectl apply -f k8s/config/rbac/clusterrolebinding.yaml
```

These three files create:
- A `ServiceAccount` named `provisionedpartienv-manager` in `parti-operator-system`.
- A `ClusterRole` named `provisionedpartienv-manager` granting `get`/`list`/`watch`/`update`/`patch` on `provisionedpartienvs`, `get`/`patch`/`update` on `provisionedpartienvs/status`, `get` on `secrets`, and `create`/`patch` on `events`.
- A `ClusterRoleBinding` binding that role to the `ServiceAccount`.

**Step 4 — Deploy the operator:**

```bash
kubectl apply -f k8s/config/manager/deployment.yaml
```

This creates a one-replica `Deployment` named `provisionedpartienv-manager`
in `parti-operator-system`. Edit the `image:` field to point at a built
operator image before applying (the manifest ships `parti-operator:latest` as
a placeholder).

Verify the operator is running:

```bash
kubectl get pods -n parti-operator-system
kubectl get crd provisionedpartienvs.parti.io
```

---

## The ProvisionedPartiEnv CRD

- **API group / version:** `parti.io/v1alpha1`
- **Kind:** `ProvisionedPartiEnv`
- **Short name:** `ppe`
- **Scope:** Namespaced

```bash
kubectl get ppe -A
```

### Spec Fields

#### Top-level

| Field | Type | Required | Description |
|---|---|---|---|
| `nats` | `NATSConnection` | **required** | NATS server address and optional authentication. |
| `instance` | string | optional | Logical environment label. Maps to `provision.Config.Instance`. |
| `policy` | string | optional | Reconcile policy: `warn` \| `adopt` \| `safe-update` \| `force`. Empty defaults to `warn` (the SDK default). |
| `controlPlane` | `*ControlPlaneSpec` | optional | Desired control-plane KV buckets. Omitting leaves control-plane provisioning to SDK defaults. |
| `partitionSource` | `*PartitionSourceSpec` | optional | Desired partition-source KV bucket. |
| `streams` | `[]StreamSpec` | optional | Desired application JetStream streams. |

#### `nats` — NATSConnection

| Field | Type | Required | Description |
|---|---|---|---|
| `server` | string | **required** | NATS server URL, e.g. `nats://nats.example.com:4222`. |
| `credentialsSecret` | `*NATSAuthSecret` | optional | Reference to a Secret in the same namespace as the CR. Omitting means anonymous access. |

#### `nats.credentialsSecret` — NATSAuthSecret

At most one of `credentialsKey`, `tokenKey`, or `nkeyKey` may be set. Setting
more than one is rejected with `Ready=False` reason `InvalidSpec`.

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | **required** | Name of the Secret in the CR's namespace. |
| `credentialsKey` | string | optional | Data key whose value is a NATS `.creds` file (NKey + JWT bundle). |
| `tokenKey` | string | optional | Data key whose value is a NATS authentication token. |
| `nkeyKey` | string | optional | Data key whose value is an NKey user seed. |

#### `controlPlane` — ControlPlaneSpec

All fields are optional.

| Field | Type | Default | Description |
|---|---|---|---|
| `stableIdBucket` | string | `parti-stableid` | Name of the stable-ID KV bucket. |
| `electionBucket` | string | `parti-election` | Name of the election KV bucket. |
| `heartbeatBucket` | string | `parti-heartbeat` | Name of the heartbeat KV bucket. |
| `assignmentBucket` | string | `parti-assignment` | Name of the assignment KV bucket. |
| `handoffBucket` | string | `parti-handoff` | Name of the handoff KV bucket. Only used when `enableTwoPhaseHandoff` is true. |
| `workerIdTtl` | duration | — | TTL for stable-ID entries (e.g. `75s`). |
| `electionTimeout` | duration | — | TTL for election entries (e.g. `10s`). |
| `heartbeatTtl` | duration | — | TTL for heartbeat entries (e.g. `15s`). |
| `assignmentTtl` | duration | `0s` | TTL for assignment entries. `0s` means no expiration. |
| `handoffTtl` | duration | — | Advisory sweep TTL for recovering stuck in-flight handoff claims. Only relevant when `enableTwoPhaseHandoff` is true. |
| `enableTwoPhaseHandoff` | bool | `false` | Gates the handoff KV bucket. When `true`, `handoffTtl` must be > 0. |
| `replicas` | int32 | `0` | Desired NATS replica count for every control-plane KV bucket. `0` leaves the server default (1). Minimum: 0. |
| `allowDeleteRecreate` | bool | `false` | Opts control-plane buckets into delete/recreate under `policy: force` when they carry drift-immutable divergence. |

Duration fields use Go duration syntax: `30s`, `5m`, `1h`.

#### `partitionSource` — PartitionSourceSpec

All fields are optional. The `partitions` field present in `provision.Config`
is deliberately omitted — partition-record contents are managed by
`partictl partitions`, not by the operator.

| Field | Type | Default | Description |
|---|---|---|---|
| `bucket` | string | — | Name of the partition-source KV bucket. |
| `key` | string | — | KV key within `bucket` that holds the partition table. |
| `replicas` | int32 | `0` | Desired replica count. `0` leaves the server default (1). Minimum: 0. |
| `storage` | string | `file` | NATS storage backend: `file` or `memory`. |
| `history` | int32 | — | KV history depth. Range: 0–255 (enforced by the CRD schema before the operator reads it). |
| `maxValueSize` | int32 | `0` | Maximum per-value size in bytes. `0` means no limit. Minimum: 0. |
| `ttl` | duration | `0s` | Per-entry TTL. `0s` means no expiration. |
| `allowDeleteRecreate` | bool | `false` | Opts this bucket into delete/recreate under `policy: force` when it carries drift-immutable divergence. |

#### `streams[]` — StreamSpec

Each entry in the `streams` list maps to one JetStream stream. `name` and
`subjects` are required; all other fields are optional.

| Field | Type | Default | Description |
|---|---|---|---|
| `name` | string | **required** | JetStream stream name. |
| `subjects` | []string | **required** | Subject patterns the stream captures (at least one). |
| `retention` | string | `limits` | Message retention policy: `limits` \| `workqueue` \| `interest`. |
| `storage` | string | `file` | Storage backend: `file` \| `memory`. |
| `discard` | string | `old` | Drop policy when limits are reached: `old` \| `new`. |
| `replicas` | int32 | `0` | Desired replica count. `0` leaves the server default (1). Minimum: 0. |
| `maxAge` | duration | `0s` | Maximum message age. `0s` means unlimited. |
| `maxBytes` | int64 | `0` | Maximum total byte size. `0` means unlimited. Minimum: 0. |
| `maxMsgs` | int64 | `0` | Maximum message count. `0` means unlimited. Minimum: 0. |
| `description` | string | — | Optional human-readable description. |
| `allowDeleteRecreate` | bool | `false` | Opts this stream into delete/recreate under `policy: force` when it carries drift-immutable divergence. |

`maxBytes` and `maxMsgs` follow the same `0`/`-1` convention as the
`provision` SDK: `0` in the CR means "unlimited"; the NATS server stores
unlimited as `-1`. The operator normalises the two representations as
equivalent so they never produce spurious drift.

---

### Status Fields

The operator writes status after every reconcile that produces an outcome (a
reconcile cancelled mid-flight due to manager shutdown persists no status —
the next reconcile records the real result).

#### `Ready` condition

`ProvisionedPartiEnv` carries a single condition type, `Ready`. The `Status`
and `Reason` fields carry the full outcome:

| `Status` | `Reason` | Meaning |
|---|---|---|
| `True` | `Reconciled` | The last reconcile applied cleanly. |
| `False` | `InvalidSpec` | The CR spec is statically invalid (bad mapping or `provision.Validate` failure). The operator does not requeue; fix the spec and re-apply. |
| `False` | `SecretMissing` | The referenced Secret does not exist or a named data key is absent. The operator retries with exponential backoff. |
| `False` | `NATSUnreachable` | The operator could not connect to NATS (network unreachable, bad credentials, malformed seed). The operator retries with exponential backoff. |
| `False` | `ApplyError` | `provision.Apply` completed but one or more resources failed. `lastApply.errors` lists the first error messages. The operator requeues after `resync-period`. |

```bash
kubectl get ppe <name> -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'
```

#### Other status fields

| Field | Type | Description |
|---|---|---|
| `observedGeneration` | int64 | The `.metadata.generation` the last reconcile acted on. Use to confirm a spec update was picked up. |
| `lastReconcileTime` | time | Timestamp of the most recent reconcile that produced a status write. |
| `lastPlan` | `*PlanSummary` | Counts from the most recent `provision.Plan` call. |
| `lastApply` | `*ApplySummary` | Counts and error messages from the most recent `provision.Apply` call. |

#### `lastPlan` — PlanSummary

| Field | Type | Description |
|---|---|---|
| `actionCount` | int | Total number of planned actions. |
| `driftInformational` | int | Count of `informational` drift findings. |
| `driftMutable` | int | Count of `drift-mutable` drift findings. |
| `driftImmutable` | int | Count of `drift-immutable` drift findings. |
| `driftAdopted` | int | Count of `adopted` drift findings. |

#### `lastApply` — ApplySummary

| Field | Type | Description |
|---|---|---|
| `executedCount` | int | Number of actions successfully executed. |
| `skippedCount` | int | Number of actions skipped. |
| `errorCount` | int | Number of resource-level errors encountered. |
| `aborted` | bool | `true` when apply was cancelled mid-execution. |
| `errors` | []string | First N resource error messages (capped to keep the status object small). |

---

## NATS Authentication

The operator reads NATS credentials from a Kubernetes `Secret` in the same
namespace as the `ProvisionedPartiEnv` CR. Reference the Secret in
`spec.nats.credentialsSecret`.

Three authentication modes are supported. Set exactly one data key in
`NATSAuthSecret`; setting more than one is rejected before any NATS connection
attempt.

### `.creds` file (NKey + JWT bundle)

```yaml
# Secret
apiVersion: v1
kind: Secret
metadata:
  name: my-nats-creds
  namespace: default
stringData:
  credentials: |
    -----BEGIN NATS USER JWT-----
    <your-jwt-here>
    ------END NATS USER JWT------

    -----BEGIN USER NKEY SEED-----
    <your-nkey-seed-here>
    ------END USER NKEY SEED------
```

```yaml
# CR reference
spec:
  nats:
    server: nats://nats.example.com:4222
    credentialsSecret:
      name: my-nats-creds
      credentialsKey: credentials   # data key name in the Secret
```

### Token

```yaml
spec:
  nats:
    server: nats://nats.example.com:4222
    credentialsSecret:
      name: my-nats-creds
      tokenKey: token               # Secret data key holding the token string
```

### NKey seed

```yaml
spec:
  nats:
    server: nats://nats.example.com:4222
    credentialsSecret:
      name: my-nats-creds
      nkeyKey: nkey                 # Secret data key holding the NKey user seed
```

### Anonymous (no Secret)

Omit `credentialsSecret` entirely:

```yaml
spec:
  nats:
    server: nats://nats.example.com:4222
```

---

## Worked Example

The sample manifests under `k8s/config/samples/` show a complete minimal
deployment.

### Step 1 — Create the credentials Secret

```bash
kubectl apply -f k8s/config/samples/nats_credentials_secret.yaml
```

The sample ships a placeholder `.creds` bundle. Replace `<replace-with-actual-jwt>`
and `<replace-with-actual-nkey-seed>` with real values before deploying to a
non-anonymous NATS server.

```yaml
# k8s/config/samples/nats_credentials_secret.yaml
apiVersion: v1
kind: Secret
metadata:
  name: sample-nats-creds
  namespace: default
stringData:
  credentials: |
    -----BEGIN NATS USER JWT-----
    <replace-with-actual-jwt>
    ------END NATS USER JWT------

    -----BEGIN USER NKEY SEED-----
    <replace-with-actual-nkey-seed>
    ------END USER NKEY SEED------
```

### Step 2 — Apply the CR

```bash
kubectl apply -f k8s/config/samples/provisionedpartienv_sample.yaml
```

```yaml
# k8s/config/samples/provisionedpartienv_sample.yaml
apiVersion: parti.io/v1alpha1
kind: ProvisionedPartiEnv
metadata:
  name: sample-parti-env
  namespace: default
spec:
  nats:
    server: nats://nats.example.com:4222
    credentialsSecret:
      name: sample-nats-creds
      credentialsKey: credentials
  instance: sample
  policy: warn
  controlPlane:
    replicas: 1
    workerIdTtl: 30s
    electionTimeout: 5s
    heartbeatTtl: 10s
    assignmentTtl: 0s
  partitionSource:
    bucket: sample-partitions
    key: partitions
    replicas: 1
    storage: file
    history: 1
  streams:
  - name: sample-events
    subjects:
    - events.>
    retention: limits
    storage: file
    replicas: 1
    maxBytes: 1073741824
    maxMsgs: 1000000
```

### Step 3 — Verify reconciliation

```bash
kubectl get ppe sample-parti-env
# NAME               READY   POLICY   AGE
# sample-parti-env   True    warn     30s

kubectl describe ppe sample-parti-env
```

A successful first reconcile sets `Ready=True` reason `Reconciled` and
populates `lastPlan` (drift counts) and `lastApply` (executed count) in
`status`. The operator requeues after `resync-period` (default `5m`) to
detect future drift.

### What the operator provisions

Given the sample CR, the operator calls `provision.Apply` which creates:
- Five control-plane KV buckets (`parti-stableid`, `parti-election`,
  `parti-heartbeat`, `parti-assignment`, `parti-handoff`) with the declared
  TTLs and replica count.
- A partition-source KV bucket `sample-partitions` at key `partitions`.
- A JetStream stream `sample-events` capturing `events.>`.

The partition table contents and per-partition consumers are **not** created by
the operator — use `partictl partitions apply` and `partictl consumers apply`
for those steps.

---

## Operator Flags

The operator binary (`k8s/cmd/manager`) accepts the following flags:

| Flag | Default | Description |
|---|---|---|
| `-metrics-bind-address` | `:8080` | Address the metrics endpoint binds to. Set to `0` to disable. |
| `-health-probe-bind-address` | `:8081` | Address the health-probe endpoint binds to. |
| `-leader-elect` | `true` | Enable leader election. Disable only for single-replica deployments. |
| `-resync-period` | `5m` | How often a converged `ProvisionedPartiEnv` is re-reconciled to detect drift. |

The zap logger flags (`-zap-log-level`, `-zap-encoder`, etc.) are also
available.

The deployed `Deployment` manifest (`k8s/config/manager/deployment.yaml`)
passes `-resync-period=5m` and `-leader-elect=true` explicitly. Override them
by editing the `args` list in the manifest.
