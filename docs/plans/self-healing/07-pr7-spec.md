# P2.1 (F9-A) — Election bucket → FileStorage + OperationTimeout warning

Per-PR spec for the seventh PR (first of Phase 2)
(`00-fix-plan.md` §P2.1). Prior PRs P0.1-P0.3, P1.1-P1.3 committed.

## Plan deviation (scope discovery)

The plan says: change `MemoryStorage` → `FileStorage` AND set
`Replicas: m.cfg.Replicas`. **`Config.Replicas` does not exist**
(verified: only `provision.Config.Replicas` exists, which is the
pre-provisioning helper not the runtime knob). Adding a Config knob
is feature creep for "the lowest change-risk item of Phase 2."

This PR therefore changes **only the storage type**, leaving
Replicas at the server default (1). Operators who require HA across
NATS-node restarts must pre-create the election bucket with the
desired replica count using their provisioning flow (existing
`docs/OPERATIONS.md` pattern: `nats kv add ... --replicas=3`). The
migration runbook explicitly names this.

The Storage change alone is still net-positive: a FileStorage R=1
bucket survives node-process restart in a single-node deployment;
the R≥3 win for multi-node deployments is operator-driven via
pre-creation (which Parti already respects — `EnsureKVBucketWithRetry`
is get-first).

## Background

`manager_setup.go:89` creates the election bucket with
`MemoryStorage`. On a NATS node restart the bucket's contents (the
leadership lease key) are lost; every follower notices the missing
lease, all attempt to re-acquire, and the cluster experiences
leadership churn. With FileStorage the contents survive the
restart and leadership rides through.

IOPS evidence: `docs/plans/iops-investigation/findings.md` §M1.9
shows the storage switch is effectively free (−2% / −1% within
noise) at N=1000 / N=3000 partitions.

## Scope

1. Change `manager_setup.go:89` election bucket from `MemoryStorage`
   to `FileStorage`.
2. Add `Manager.warnOnOperationTimeoutVsElection()` companion warning
   that fires at Start when `OperationTimeout > ElectionTimeout/3`.
3. Operator migration runbook in `docs/OPERATIONS.md` (one-time
   bucket delete + replica recommendation).

The heartbeat bucket stays MemoryStorage (workers re-publish every
HeartbeatInterval — its loss is recoverable without restart).
Assignment is already FileStorage. Source / handoff / stableid
unchanged.

## Hard prerequisite

**P1.3 (F1) epoch fence must be merged first.** The runbook deletes
the bucket on existing clusters; the epoch fence is what detects the
recreate event safely.

## Design

**Storage switch (one line):**

```go
// In ensureCoreKVBuckets:
electionKV, err = ensure("election", m.cfg.KVBuckets.ElectionBucket,
    m.cfg.ElectionTimeout, jetstream.FileStorage)
```

**Companion warning:**

```go
// In manager_setup.go, alongside other warn helpers:
func warnOnOperationTimeoutVsElection(cfg Config, logger types.Logger) {
    if cfg.OperationTimeout > cfg.ElectionTimeout/3 {
        logger.Warn(
            "OperationTimeout exceeds ElectionTimeout/3; a single slow "+
                "renew can consume the lease's three-attempt budget",
            "OperationTimeout", cfg.OperationTimeout,
            "ElectionTimeout", cfg.ElectionTimeout,
            "remedy", "set OperationTimeout <= ElectionTimeout/3",
        )
    }
}
```

Called from `Manager.Start` alongside the other warning helpers.

## Reproducer tests

- *T1 (companion warning).* Unit test, table-driven:
  - `OperationTimeout=5s, ElectionTimeout=30s` → silent (5 ≤ 10)
  - `OperationTimeout=11s, ElectionTimeout=30s` → warns (11 > 10)
  - `OperationTimeout=ElectionTimeout` → warns
- *T2 (storage choice).* Start Manager against embedded NATS; read
  back the election bucket's stream Storage; assert
  `jetstream.FileStorage`. On parent: returns MemoryStorage.

## Verification gates

- `make lint && make test && make test-race` green.
- Docs review: migration runbook in `docs/OPERATIONS.md` reads
  unambiguously.

## How this trips readiness

Indirectly: the change **eliminates** the dominant readiness-trip
cause (election bucket loss on routine NATS node restart). The
genuine cluster-rebuild case still trips readiness via the P1.3
epoch fence.

## Out of scope

- Adding `Config.Replicas` (separate feature, separate PR).
- Heartbeat / handoff / stableid storage changes.
- F9-B lease-aware leader (deferred to Phase 3).
