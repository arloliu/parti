# Parti Self-Healing Review — Findings & Recommendations

**Date:** 2026-05-23
**Branch:** main
**Status:** Investigation deliverable — for review. No code changed.

**Scope:** Self-healing posture of the Parti library, with emphasis on dynamic
partitioning, against the stated requirements:

1. Keep working when the NATS cluster has problems and is rebuilt.
2. When consumer / KV buckets are gone, try to re-create them *if possible*.
3. **Partial NATS abnormality** — when the election / heartbeat subsystem fails
   while the rest of NATS works, retain the current leader and avoid a follower
   promoting into a second leader.
4. **Double assignment** — whether the same partition can be owned and processed
   by two workers at once.

**Deployment (confirmed):** Kubernetes, with the `OnDegraded` → readiness-probe
→ pod-rotation pattern (`examples/degraded-readiness`).

---

## 1. Scope & Method

Five read-only investigations covered the independent self-healing surfaces:

- **Connectivity layer** — `internal/natsutil/`, connection lifecycle, the
  KV-watcher + reconciler pattern.
- **KV-bucket-backed state** — `internal/election/`, `internal/stableid/`,
  `source/nats_kv.go`, `internal/kvbuckets/`, `manager_handoff.go`,
  `internal/durable/claim_resolver.go`, `provision/`.
- **JetStream stream / consumer recovery** — `internal/recovery/`,
  `internal/durable/partition_consumer.go`, `worker_consumer.go`,
  `consumer/dynamic.go`, `internal/dynamicbuild/`.
- **Manager orchestration** — `manager.go`, `manager_setup.go`,
  `manager_degraded.go`, `manager_assignment.go`, `manager_election.go`,
  `internal/heartbeat/`.
- **Election stability & double-assignment** — `internal/election/`,
  `internal/assignment/` (calculator, worker monitor, audit, emergency),
  `internal/assignment/handoff/`, `internal/durable/processing_gate.go`.

Every claim is backed by a `file:line` citation. Where a behavior could not be
verified from source it is marked **unverified** and the reason given. Two
findings (F9, F10) were added after the initial review in response to the
partial-failure and double-assignment considerations.

---

## 2. Executive Summary

Parti's self-healing is **strong for transient faults and consumer loss, has a
hard floor at server-side state loss, and has two latent correctness gaps under
*partial* NATS failure** (the new F9/F10 area).

Connection reconnect (delegated to nats.go), silent KV-watcher stalls (covered
by four 30 s reconcilers), and durable-consumer deletion (covered by the
`recovery.Controller`) all heal cleanly and are proven by integration tests.

The floor: **no runtime code path re-creates a missing stream or KV bucket.**
Provisioning runs exactly once, inside `Manager.Start` (`manager.go:417,434,441`),
and is get-first. When server-side state vanishes while a worker runs, the
library *detects* the loss and enters **degraded mode** to keep processing on
the cached assignment, but does not repair the state — recovery needs a process
restart (which, on this Kubernetes deployment, the readiness probe triggers) or
an operator `provision` run.

Findings worse than "needs a restart" — the **correctness gaps**:

- **F1** — a wipe-and-recreate of buckets under the same names is neither
  detected nor healed; the worker runs against incoherent empty state with no
  readiness signal.
- **F2** — several recovery loops retry forever, silently and unbounded.
- **F10** — under a *partial* heartbeat-bucket failure, live workers can be
  declared dead and their partitions reassigned; combined with degraded mode
  (which keeps the wrongly-evicted worker processing), this is the realistic
  route to genuine double ownership.
- **F6-B** — the partition-input analogue: if the source returns an empty
  (or sharply shrunk non-empty) observation, the leader currently proceeds to
  reassign every partition to zero, silently and across the fleet, with no
  degraded signal. (The erroring path already aborts cleanly — see F6.)
- **F9-A** — the partial-failure / leadership-churn concern: the current code
  is in fact **split-brain-safe** (eager leader step-down + the
  `CheckLeadership` publish fence), but it pays for that safety with
  leadership *churn* on every transient election-bucket blip. Per the IOPS
  investigation, the dominant cause — the election bucket being
  `MemoryStorage` (lost on any node restart) — is closed by switching to
  `FileStorage` R≥3, a one-line code change at essentially zero IOPS cost
  (F9-B, the lease-aware leader for residual transient unavailability, is
  **deferred**).

### How the findings bite dynamic partitioning

Dynamic partitioning transits every layer here — election, the assignment KV,
the partition source, JetStream streams, durable per-partition consumers — so
the findings are not separable from it:

- **F1** corrupts dynamic assignment: a `parti-election`-bucket wipe resets the
  leader-revision space (`LeaderRevision` is sourced from the election agent's
  `Revision()`, `manager_election.go:316-323`), making the
  `lastSeenLeaderRevision` ordering fence meaningless; a `parti-assignment`-bucket
  wipe resets the commit/CAS history the assignment watcher relies on.
- **F2 / F5** turn a stream wipe into a fleet-wide thundering herd of
  per-partition consumer recovery attempts.
- **F3** lets duplicate worker IDs hand the same partition out twice after a
  wipe-and-recreate.
- **F4** is what would let a leader's recomputed assignment reach followers
  without a fleet-wide restart.
- **F6** has two faces: silent source loss (F6-A — no readiness signal) and
  an unguarded **empty / sharply shrunk** non-erroring source observation
  that can reassign every partition to zero (F6-B — the partition-input
  "minimum credible inputs" floor). The erroring path is already correctly
  aborted at `calculator.go:1280-1283`.
- **F9-A** is leadership stability for the dynamic calculator: churn at the
  leader/calculator boundary forces an assignment recompute and cache-affinity
  loss on every transient election-bucket fault. Switching the election bucket
  to `FileStorage` R≥3 is the primary fix — near-free per IOPS analysis. F9-B
  (lease-aware leader) is deferred unless residual transient churn warrants
  the HIGH-risk election-code change.
- **F10** is the dynamic-partitioning correctness invariant itself: one
  partition, one active owner.

### Deployment context: the readiness-probe lens

On this Kubernetes deployment the readiness probe *is* the recovery mechanism:
a worker that fails readiness is rotated, and the restart re-provisions every
missing bucket via get-first `EnsureKVBucket`. A finding's true danger is
therefore **"does it fail to trip the readiness probe?"**

- **Trips readiness → already acceptably handled** (at the cost of a pod
  restart): bucket-absent cluster rebuild, the `MaxReconnects` zombie.
- **Readiness-blind → defeats the recovery design** (worker stays `Ready` while
  silently broken): **F1** (no degraded entry), **F5** (consumer-layer recovery
  loop does not feed the degraded circuit), **F6** (source loss never escalates
  to the manager), **F8** (a silently-stalled watcher), and **F10** (a wrong
  reassignment looks like a valid one — no error anywhere).

The unifying objective: **every unrecoverable failure must trip the readiness
probe.**

### Reading the risk ratings

The user asked for a risk evaluation and ranking, and stressed: *these fixes
are risky, easy to produce side effects; keep changes as small as possible and
verify step by step.* Every finding in Section 6 therefore carries two ratings:

- **Impact if unfixed** — severity of the gap (LOW / MEDIUM / HIGH).
- **Change risk** — how dangerous the *fix itself* is: blast radius, how much
  load-bearing concurrency/coordination code it touches, how easily a mistuned
  fix creates a *new* fault (LOW / MEDIUM / HIGH).

Section 7 consolidates both into a single ranked table and an implementation
discipline (one finding per PR, reproducer-first, step-by-step verification).
Section 8 turns that into a phased sequence. **A high-impact finding is not
automatically done first** — a low-risk quick win that builds the
verify-as-you-go rhythm is sequenced ahead of a high-risk correctness fix.

---

## 3. What Self-Heals Today (Inventory)

The working baseline — recommendations must not regress it.

| Mechanism | What it heals | Evidence |
|---|---|---|
| nats.go reconnect (caller-owned `*nats.Conn`) | TCP drop, server bounce; rebinds JetStream subscriptions | `manager.go:410-414` (conn is injected) |
| Four KV-watcher reconcilers @ 30 s | Silent watcher stalls after a server restart — the *load-bearing* recovery path | assignment/commit `manager_assignment.go:325,478`; claim-resolver `claim_resolver.go:748`; source `source/nats_kv.go:812` |
| Claim-resolver drift-driven watcher rebind | Forcibly re-binds a silently-stalled watcher on detected drift | `claim_resolver.go:882,902` |
| Heartbeat-monitor polling backstop @ `hbTTL/2` | Topology changes missed by a stalled heartbeat watcher | `internal/assignment/worker_monitor.go:267-294` |
| `recovery.Controller` + legacy escalation | **Durable consumer deleted** under a running consumer — recreated | `internal/recovery/controller.go:237`; `partition_consumer.go:372-439` |
| `EmergencyDetector` grace window | Absorbs a *transient* worker-absence blip — a worker must be absent across `gracePeriod` before being declared dead | `internal/assignment/emergency.go:89-133` |
| `LeaderRevision` + commit fences | A former leader cannot make a stale assignment authoritative | `manager_assignment.go:434-437,602-606,831-847`; `CheckLeadership` `nats_election.go:351-390` |
| Degraded mode | Detects sustained connectivity / JetStream-state loss; keeps processing on cached assignment; escalating alerts; `OnDegraded` hook | `manager_degraded.go:33-55,80-132,153-190` |
| Get-first provisioning at `Start` | Re-creates *missing* buckets — **but only on process restart** | `kvutil` `EnsureKVBucketWithRetry`; `manager_setup.go:158-186` |
| `provision` SDK / `partictl apply` | Re-runnable; re-creates missing streams + buckets | `provision/apply_recreate.go:21-22,106-117` |

**Key empirical finding (re-confirmed):** a NATS server restart does **not**
reliably close a KV watcher's `Updates()` channel — the client transparently
rebinds, the channel stays open, the watcher silently stops delivering. The
**periodic reconciler is the load-bearing recovery path**
(`test/integration/failure/claim_resolver_nats_restart_test.go:34-41`).

---

## 4. Failure-Mode Matrix

✓ self-heals · ⚠ partial / restart-dependent · ✗ does not self-heal

| # | Scenario | Current behavior | Verdict |
|---|---|---|---|
| 1 | Brief connection blip (< `EnterThreshold`) | nats.go reconnects; watchers may stall; reconcilers re-sync ≤ 30 s | ✓ |
| 2 | Server restart, state intact (same `StoreDir`, R≥3) † | Watchers silently stall; reconcilers re-sync; claim-resolver force-rebinds | ✓ |
| 3 | Server restart, ephemeral storage — buckets/streams empty or gone | Degrading-JS errors → degraded mode; no in-process re-create | ✗ in-process · ⚠ via restart |
| 4 | Full cluster rebuild, buckets absent | Detected → degraded; stays degraded until restart / operator re-provision | ✗ in-process · ⚠ via restart |
| 5 | Wipe-and-recreate, same bucket names | KV ops succeed against empty state; **not detected, no degraded signal** | ✗ (F1) |
| 6 | JetStream message stream deleted | Dynamic-consumer recovery fails; unbounded silent retry loop, no give-up | ✗ (F2 / F5) |
| 7 | Durable consumer deleted | `recovery.Controller` + legacy escalation re-create it | ✓ |
| 8 | Partition-source bucket deleted | Watcher/reconcile spin on generic errors; recovers only if the *user* re-creates it; no escalation | ⚠ (F6-A) |
| 9 | Outage longer than `MaxReconnects × ReconnectWait` | Caller's conn goes `CLOSED`; degraded forever; reconcilers fail on a dead conn — zombie | ⚠ on k8s — degraded → rotated (F7) |
| 10 | **Election bucket transient blip** (slow/erroring, key still present) | Calculator stops + restarts (churn); on a context-timeout renew error the election term is retained, on a definite-loss error a leaderless gap ≤ `ElectionTimeout` opens; **no two leaders** | ⚠ residual churn (F9-B, deferred) |
| 11 | **NATS node restart — election bucket** (`MemoryStorage`) † | Election bucket lost on that node; if not R≥3-survivable, leadership outage until pod rotation | ⚠ — closed by **F9-A** (FileStorage R≥3 switch) |
| 12 | **Partial heartbeat-bucket degradation** (`Keys()` returns a truncated list) | Live workers may be declared dead and reassigned past the grace window; no shrink floor | ✗ (F10-A) |
| 13 | **Partition source returns empty / sharply shrunk (non-erroring) list mid-run** | Leader currently recomputes against the suspicious input and can publish a zero-partition assignment fleet-wide; no degraded signal. (Snapshot *errors* already abort cleanly, `calculator.go:1280-1283`.) | ✗ (F6-B) |

† The election bucket is `MemoryStorage` (`manager_setup.go:89`). It survives a
single-node restart only if Raft-replicated across surviving nodes (R≥3); a
full-cluster restart or an R=1 memory bucket loses it outright.

---

## 5. The Central Design Tension (re-creation)

`docs/OPERATIONS.md:626` states the current position:

> "Parti deliberately does not auto-recreate buckets from the live publish path.
> Recreating on a transient `ErrStreamNotFound` during a JetStream leader
> reshuffle would cause the data loss it was trying to prevent … Parti cannot
> distinguish 'data permanently gone' from 'data coming back'."

Sound — but only for resources that carry irreplaceable **data**. The resources
Parti depends on fall into four categories:

| Category | Resources | Loss of content means… | Re-creation policy |
|---|---|---|---|
| **A. User-owned data** | Partition-source KV bucket | Partition definitions gone — real data loss | Library must not re-create; surface loudly (F6). |
| **B. Message streams** | JetStream work/subject streams | Unprocessed messages gone — real data loss | Never auto-recreate from the consumer layer; escalate via a caller hook (F5). |
| **C. Regenerable coordination, leader-rebuilt** | `parti-assignment`, `parti-heartbeat` | Nothing irreplaceable — recomputed / re-published in seconds | Eligible for in-process re-create, opt-in, after a confirmation delay (F4). |
| **D. Coordination, continuity-sensitive** | `parti-election`, `parti-handoff` | Regenerable, but a naive empty re-create resets the leader-revision space / in-flight handoffs | Eligible with extra guards (F1 epoch fence; handoff abort). |

The `OPERATIONS.md:626` rationale applies fully to A and B. For C and D the risk
is far smaller and a **confirmation delay** closes the "coming back" race —
this is the basis for F4 (demoted to optional under the k8s deployment).

---

## 6. Findings & Recommendations

Finding numbers (F1–F10) are stable identifiers, **not** a priority order — see
Section 7 for the risk-ranked order and Section 8 for the phased sequence. Each
finding states **Impact if unfixed** and **Change risk** of the fix.

---

### F1 — Wipe-and-recreate under the same names is silent corruption

**Finding.** If buckets are deleted and re-created with the same names (operator
`nats kv rm` + recreate, backup/restore, ephemeral-storage node restart), every
KV operation succeeds against the new empty streams. `monitorNATSConnection`
checks only `conn.Status()` (`manager_degraded.go:33-35`); `recordKVError` fires
only on connectivity or NotFound-family errors (`manager_degraded.go:84`).
Neither triggers. The worker keeps running on cached handles `m.assignmentKV` /
`m.heartbeatKV` (`manager.go:455-456`) against wiped state: the election
revision space resets (stale `lastSeenLeaderRevision` fences become
meaningless), in-flight handoff / stable-ID claims have vanished, and **no
`OnDegraded` ever fires** — so on this k8s deployment the pod is never rotated.

**Recommendation R1 — epoch fence.** Cache an immutable per-bucket identity at
`Start` and verify it in the reconcilers. The JetStream stream backing each KV
bucket has an immutable server-assigned `Created` timestamp — cache
`StreamInfo().Created` per bucket at `Start`; have each reconciler re-read stream
info and compare. A changed `Created` ⇒ the bucket was re-created underneath the
worker ⇒ enter degraded / fire `OnDegraded` with a distinct reason. (Alternative:
reuse the `provision` marker mechanism, `provision/marker.go`.)

**Design tension:** none — pure fail-loud, independent of the re-create debate.

**Impact if unfixed:** **HIGH** — the only true silent-corruption path; on k8s
it is also readiness-blind, so the deployment's own recovery mechanism never
engages.

**Change risk:** **MEDIUM** — additive (detection only; does not change recovery
behavior), but it adds a new degraded-entry trigger on the hot reconcile path; a
wrong epoch comparison causes spurious degraded entry → spurious pod rotation.
Mitigated by: the `Created` timestamp is a stable server value; the fix needs a
reproducer that wipes+recreates a bucket and asserts degraded entry.

**Does not:** recover the lost data — converts silent corruption into a visible,
actionable signal. Pairs with F9 (closes the follower side of the split-brain
surface — see F9).

---

### F2 — Unbounded, silent, infinite retry loops

**Finding.** Multiple recovery loops retry forever against a genuinely-gone
resource, with no attempt cap, no permanent-failure state, no escalation:

- Dynamic-consumer recovery on a deleted stream: `recreateFn`
  (`partition_consumer.go:508-512`) → `controller.go:294-302` logs and fails →
  the run loop backs off and `continue`s (`partition_consumer.go:195-199`)
  forever.
- `source.NatsKV.restartWatcher` retries `kv.Watch` forever on a missing bucket
  (`source/nats_kv.go:772-805`).
- `ClaimBasedResolver` retries `WatchAll` forever on a missing handoff bucket
  (`claim_resolver.go:558-571`).
- `monitorAssignmentChanges` retries forever — the comment at
  `manager_assignment.go:339` names "a wiped assignment bucket" as the expected
  case.

During a cluster rebuild every worker generates sustained, unbounded NATS API
pressure — a thundering herd aimed at the cluster while it is most fragile.

**Recommendation R2 — bounded retry with a give-up state.** Standardize a retry
envelope for every "recover an existing resource" loop: exponential backoff with
a hard ceiling; a bounded attempt budget per failure episode (reset on success);
on exhaustion, transition to an explicit permanent-failure state, fire
`OnError` / a hook **wired so it can fail the readiness probe** (§2), set a
metric, and stop generating API load until connectivity is re-confirmed.

**Design tension:** minimal — the only judgement call is the attempt budget;
make it configurable with a safe default.

**Impact if unfixed:** **MEDIUM–HIGH** — thundering herd; no clean operator
alert; load never abates.

**Change risk:** **MEDIUM–HIGH** — changes retry semantics across *many* call
sites; a too-aggressive give-up turns a recoverable blip into a premature
permanent failure. **Must be done loop-by-loop, one PR per loop**, each with its
own reproducer — do not change all loops in one change.

**Does not:** re-create anything — bounds the damage and makes failure
observable. Prerequisite for F4/F5.

---

### F3 — `stableID` renew misclassifies "bucket gone" as transient

**Finding.** `Claimer.renew` translates **only** `jetstream.ErrKeyExists` into
`ErrClaimLost` (`internal/stableid/claimer.go:364-368`); every other error,
including `ErrBucketNotFound` / `ErrStreamNotFound`, returns the generic
`"failed to renew ID"` (`claimer.go:369`). So the claim-loss self-stop
(`claimer.go:329` → `claimLostShutdown`, `manager_election.go:91-98`) never
fires when the bucket is gone; the worker keeps `m.workerID` set and keeps
advertising it. After a wipe-and-recreate (F1), two workers can each `Create`
the *same* ID into the fresh empty bucket — **duplicate worker IDs**.

**Recommendation R3.** In `renew`, classify `ErrBucketNotFound` /
`ErrStreamNotFound` explicitly and treat a vanished claim backing-store as
claim-loss — return `ErrClaimLost` so the existing self-stop runs (worker stops
→ `OnError` → k8s restarts it → it re-claims cleanly). Matches the documented
contract (`docs/OPERATIONS.md:649-659`).

**Design tension:** low.

**Impact if unfixed:** **MEDIUM–HIGH** — duplicate worker IDs break the core
stable-ID invariant and (via F10) can hand a partition to two workers.

**Change risk:** **MEDIUM** — small, localized surface (one error-classification
branch), but it changes *when the worker self-stops*; a wrong classification
causes a spurious self-stop (and pod rotation). Needs a reproducer that deletes
the stableID bucket and asserts `ErrClaimLost`.

**Does not:** prevent the brief duplicate-ID window during a wipe — that is
closed by F1 (detection); R3 ensures the worker fails *safe*.

---

### F4 — No in-process re-provision of coordination buckets *(demoted — optional)*

> **Re-prioritized.** With Kubernetes readiness-probe recovery confirmed, the
> bucket-absent cluster rebuild already heals: degraded → failed readiness → pod
> rotation → restart re-provisions. F4 is **not** a recovery requirement — it is
> an optimization that avoids a restart. Retained for completeness; it is the
> last phase and may reasonably be dropped. **One exception worth weighing:**
> the election bucket is `MemoryStorage` (`manager_setup.go:89`), so a routine
> NATS node restart can wipe it — an in-process re-create *for the election
> bucket specifically* would avoid a pod rotation on an ordinary node bounce.
> See F9.

**Finding.** `ensureStableIDKV` / `ensureCoreKVBuckets` / `setupHandoff` are
reachable only from `Manager.Start` (`manager.go:417,434,441`). No path
re-creates a coordination bucket at runtime; `attemptRecoveryFromDegraded` only
does a KV `Get` (`manager_degraded.go:236`).

**Recommendation R4 — gated, opt-in, in-process re-create** for the regenerable
coordination buckets (`parti-assignment`, `parti-heartbeat`, `parti-election`,
`parti-handoff`), off by default, only after the bucket is confirmed absent on a
healthy connection for a sustained window (`RecreateGracePeriod`). Excludes the
source bucket and message streams. `parti-stableid` is **not** an in-process
target — F3 handles it via restart.

**Design tension:** modifies the `OPERATIONS.md:626` stance — done narrowly
(coordination buckets only, opt-in, confirmation delay); the operator-owned
default is preserved.

**Impact if unfixed:** **LOW–MEDIUM** on this deployment — pod rotation already
re-provisions.

**Change risk:** **HIGH** — creates buckets at runtime; largest blast radius;
can race a recovering cluster. If implemented, gate behind F1 + F2 and an
opt-in flag.

**Does not:** re-create message streams or the source bucket.

---

### F5 — A deleted message stream wedges every dynamic partition consumer

**Finding.** When the JetStream **stream** a dynamic consumer pulls from is
deleted, neither recovery path can succeed — a consumer cannot be created on a
missing stream. The legacy escalation bails (`partition_consumer.go:457-460`);
the `recovery.Controller` logs and fails (`controller.go:294-302`). No code in
`internal/durable/`, `internal/recovery/`, or `consumer/` creates a stream. The
run loop retries forever. Secondary: `CheckWorkQueueRecoveryCompat` runs under
`sync.Once` only inside `Dynamic.Update` (`consumer/dynamic.go:309-316`);
`Dynamic.UpdateWorkerConsumer` — the path the manager and the handoff
coordinators actually use — bypasses it entirely (`:319-327`), so a stream
re-created with a different retention policy is never re-validated on any path.

**Recommendation R5.** Message streams stay category B — do **not** auto-recreate
from the consumer layer. Apply the F2 envelope; add a caller hook
`OnStreamMissing(streamName string) error` so provisioning-owning callers can
re-create the stream. **Critically for k8s:** the dynamic-consumer recovery loop
lives in `internal/durable/` and does not feed the manager's degraded circuit,
so a stream wipe never trips readiness today. Note the limit precisely — unlike
a KV-bucket loss, a **pod restart does NOT recover a deleted message stream**:
`Manager.Start` ensures only Parti's KV buckets (`manager.go:417,434,441`), and
`ensureConsumer` bails on stream-not-found without creating the stream
(`partition_consumer.go:457-460`). Readiness can therefore only *stop a silently
wedged pod and alert orchestration*; the stream itself must be restored by an
operator or a provisioning path wired to `OnStreamMissing`. Re-arm the
work-queue-compat check after a successful recovery — and run it on the
**manager-facing path too** (`Dynamic.UpdateWorkerConsumer` currently skips it).
Reseed the checkpoint from the *new* stream's `AckFloor` after a stream
re-create (`recovery/config.go:27-28` + monotonic `checkpoint.go:33-40` would
otherwise start a `FromLastProcessed` consumer past an empty stream's tip and
deliver nothing).

**Design tension:** none — respects `OPERATIONS.md:626` for category B.

**Impact if unfixed:** **HIGH** — readiness-blind; every dynamic partition
consumer wedges on a stream wipe with no recovery and no signal.

**Change risk:** **MEDIUM** — the hook is additive; the bounded-retry change
inherits F2's risk; the checkpoint reseed touches delivery-position logic and
needs care (a wrong reseed silently skips or re-delivers messages).

---

### F6 — Partition-source loss is invisible AND can trigger an empty-source reassignment

**Finding.** `source.NatsKV` never creates the source bucket — it receives a
handle (`source/nats_kv.go:178`); the bucket is the user's by contract. Two
problems compound on a source loss:

- *F6-A (signaling).* On deletion, `restartWatcher` loops on `kv.Watch`
  (`nats_kv.go:772-805`) and `reconcileOnce` treats `ErrBucketNotFound` as a
  generic logged error (`nats_kv.go:864-878`) — both forever, with **no
  escalation to the Manager and no degraded signal.** The watcher *will*
  recover once the user restores the bucket, but the loss is invisible until
  then.
- *F6-B (freeze).* If the partition source returns an **empty list, or a
  sharply shrunk (but non-empty) list**, the leader currently proceeds to
  recompute and republish an assignment based on that suspicious input —
  potentially with **zero partitions**, removing every worker's partitions
  across the fleet. The calculator already aborts cleanly on a snapshot
  *error* (`internal/assignment/calculator.go:1280-1283`), so the error path
  is correctly handled (previous assignment is preserved by abortion of the
  rebalance attempt). The gap is the *suspicious-but-non-erroring* case:
  there is no analogue of the existing `len(workers) == 0` floor
  (`calculator.go:1290-1296`) for the partition input.

**Recommendation R6-A — make the loss loud (additive).** Keep category-A policy
(the library must not re-create a user-owned bucket) but distinguish
`ErrBucketNotFound` from transient errors; surface it via a metric
(`parti_source_bucket_missing`) and a hook (`OnSourceUnavailable`) wireable to
readiness; document the responsibility split.

**Recommendation R6-B — freeze the reassignment on a suspicious non-erroring
source observation.** The behavioral contract: an **empty** or **sharply
shrunk (but non-empty)** source observation must **not** propagate into a
fleet-wide reassignment. (The *erroring* path is already correct — see the
finding.) The implementation phase chooses between two natural locations:

- *Source-layer suppression (likely the smaller change).* The
  `source.NatsKV` watcher already holds the partition set as state for its
  `Updates()` channel; it can cache its last non-empty observation and **not
  emit** an empty/shrunk result downstream until a new non-empty observation
  arrives (or until the shrunk observation is confirmed across multiple polls).
- *Calculator-layer guard.* Add a `len(partitions) == 0` floor at the
  rebalance attempt analogous to the existing `len(workers) == 0`
  (`calculator.go:1290-1296`), plus a shrunk-detection guard using a cached
  last-known partition count. Note that the current calculator does **not**
  retain inter-poll partition state — it processes each observation
  independently — so this option adds state that does not exist today.

Either location must also handle the *sharply shrunk* (but non-empty) case
with a confirmation-across-polls / grace window analogous to
`EmergencyDetector` for workers (`internal/assignment/emergency.go:89-133`)
and the F10-A worker-set floor. Together these form a *"minimum credible
inputs"* doctrine for the calculator: never reassign on a suspicious
observation of either input.

**Design tension:** R6-B prefers brief staleness over correctness loss. A
*legitimate* partition-set shrink (operator removed a partition) is delayed by
the grace window. That is a deliberate trade — delaying a legitimate shrink by
seconds is far cheaper than reassigning every partition to zero on a transient
source blip.

**Impact if unfixed:** **F6-A** is **MEDIUM** (readiness-blind silence; loss of
source is invisible). **F6-B** is **HIGH** (a non-erroring empty / sharply
shrunk source observation can trigger a fleet-wide reassign-to-zero — silent,
no degraded signal — which is exactly the failure the "keep working when the
source goes" requirement is about; the erroring path is already correctly
aborted at `calculator.go:1280-1283`).

**Change risk:** **F6-A** is **LOW–MEDIUM** (additive — a metric, a hook, one
error classification). **F6-B** is **MEDIUM** (touches the calculator input
path; mistuned, it can suppress a legitimate partition-set shrink — parallels
F10-A's risk profile and needs the same reproducer-first discipline).

---

### F7 — Caller connection config can turn an outage into a zombie

**Finding.** The core library never creates the `nats.Conn` — it is injected
(`manager.go:410-414`) — and registers zero connection callbacks. The public
docstrings (`doc.go:30`, `manager.go:258`) and example apps
(`examples/basic/main.go:34`) use bare `nats.Connect`, inheriting the nats.go
default `MaxReconnects = 60`. After ~60 failed attempts the connection goes
permanently `CLOSED`; degraded mode keeps the worker "alive" forever on a stale
cache.

**Recommendation R7 — documentation + a startup warning, no behavior change.**
Document the required posture (`MaxReconnects = -1`, sane `ReconnectWait` /
`ReconnectJitter`, `RetryOnFailedConnect`) in `doc.go` / `docs/OPERATIONS.md` /
examples. Optionally, at `Start`, inspect `conn.Opts` and **warn** if
`MaxReconnects` is finite — the pattern `warnOnShortAuditGrace`
(`manager_setup.go:387-406`) already uses.

**Design tension:** none.

**Impact if unfixed:** **LOW–MEDIUM** — on k8s the zombie still enters degraded
mode, so the readiness probe rotates it; the cost is an avoidable pod restart
and operator confusion.

**Change risk:** **LOW** — docs plus an optional read-only startup warning.

---

### F8 — `source.WithReconcileInterval(0)` silently disables the recovery path

**Finding.** Because the reconciler — not the watcher's close-detection — is what
recovers a silent stall, disabling it removes the only server-restart recovery
path for that watcher. The **public consumer path is safe**: `ReconcileInterval
= 0` on `consumer.ResolverConfig` means "use the 30 s default," and only
negative values are rejected (`consumer/resolver_config.go:61-63`,
`internal/durable/config.go:89`); `worker_consumer.go:536-545` passes an
already-normalized positive value. The foot-gun is the **partition source**:
`source.WithReconcileInterval(0)` is documented only as "disables polling"
(`source/nats_kv.go:50-63`), and `reconcileLoop` exits when the interval is
non-positive with no leadership probe set (`source/nats_kv.go:807-814`) — the
source watcher then has no recovery from a NATS server restart (it stalls
silently and never rebinds).

**Recommendation R8.** For `source.WithReconcileInterval`, either reject `0` /
clamp to a minimum with a startup warning, or update the Godoc to state plainly
that disabling the reconciler disables server-restart recovery for the source
watcher. No change is needed on the `consumer.ResolverConfig` path.

**Design tension:** none.

**Impact if unfixed:** **LOW** — bites only a user who explicitly disables the
source reconciler; when it bites it is a readiness-blind silent stall.

**Change risk:** **LOW** — Godoc plus an optional config warning; tiny surface.

---

### F9 — Election leadership churn under partial NATS failure

**The concern, addressed directly.** The danger raised — "election/heartbeat
fail → a follower promotes → two leaders" — is correctly identified as the thing
to avoid. The current code *happens to avoid it*, by erring toward eager leader
step-down, and at a real churn cost. The proposed remedy ("retain the current
leader") is right in spirit but, implemented naively, would create exactly the
split-brain it is meant to prevent. Details:

**Finding — current behavior is split-brain-SAFE but CHURNY.**

- *Manager side — unconditional flip.* `monitorLeadership` bounds each renew
  with `OperationTimeout` (`renewCtx`). On *any* renew error it does
  `m.isLeader.Store(false)`, fires `OnLeadershipChanged(false)`, and
  `stopCalculator()` (`manager_election.go:206-230`) — no retry.
- *Election-agent side — error-dependent.* `RenewLeadership` clears the local
  term only on a *definite* loss error; on `context.Canceled` /
  `context.DeadlineExceeded` it deliberately does **not** call
  `clearLeadership()` (`nats_election.go:190-207`). Two regimes follow:
  - *Definite-loss renew error* (wrong revision, a non-context NATS error) →
    `clearLeadership()` → `termRevision=0`. The term is fully surrendered;
    re-acquisition needs a fresh `kv.Create` after TTL expiry.
  - *Context-timeout renew error* (the `OperationTimeout` fired — the typical
    "election bucket slow/abnormal" symptom) → the agent **retains**
    `isLeader`, `workerID`, `termRevision`. On the next tick the manager is in
    the follower branch, but `RequestLeadership` sees the agent still holds the
    term and re-issues a *renew* rather than a `kv.Create`
    (`nats_election.go:112-130`). If the blip cleared, the worker re-renews
    into the **same term** — no key release, no follower takeover.
- *Follower side — TTL-gated.* A follower acquires only via `kv.Create`
  (`nats_election.go:140`), which succeeds only after the leader key's TTL
  expires. The election bucket TTL = `ElectionTimeout` (`manager_setup.go:89`,
  default 10 s); the leader renews every `ElectionTimeout/3`.
- *Split-brain safety holds in both regimes.* The calculator is stopped for the
  whole window (so no leader publishes), and the assignment publisher's
  `CheckLeadership` fence (`nats_election.go:351-390`, wired as `LeaderCheck` at
  `manager_assignment.go:123` — the only non-test call site) independently
  rejects a publish from a stale term. There is **no two-leaders path** today.
- *The real cost — churn at the manager/calculator layer.* Even in the
  context-timeout regime where the election *term* is retained, the manager
  still flips `m.isLeader` and stops+restarts the calculator on every blip —
  firing `OnLeadershipChanged` twice and forcing an assignment recompute. In
  the definite-loss regime the term is surrendered too, adding a leaderless gap
  of up to `ElectionTimeout`. On `MemoryStorage` (see below) this churn fires on
  *every* NATS node restart.

**The `MemoryStorage` amplifier — the dominant churn source.** The election
bucket is `MemoryStorage` (`manager_setup.go:89`) — the most fragile bucket
Parti owns. A NATS node restart can wipe it (a memory bucket survives a
single-node loss only if R≥3; a full-cluster restart loses it outright). The
cached `electionKV` handle then errors on both renew and acquire, so the
cluster is leaderless until pod rotation. **Routine k8s NATS node bounces
therefore cause fleet-wide leadership thrash, and this is the dominant churn
cause** — far more than transient JetStream Raft reshuffles on a healthy bucket.

**Recommendation R9-A — switch the election bucket from `MemoryStorage` to
`FileStorage`, R≥3 (PRIMARY; LOW risk, near-zero IOPS cost).** Per the IOPS
investigation, cell M1.9 (*all parti KV buckets → memory*) yields **−2 % / −1 %
of total IOPS at N=1000 / N=3000 — within measurement noise**
(`docs/plans/iops-investigation/findings.md` §2). The inverse (memory → file)
is the same noise-level delta, so the FileStorage switch is **effectively free**
on the IOPS dimension while eliminating the dominant churn cause. TTL semantics
are unchanged (the bucket TTL behaves identically on either storage), so the
existing election machinery works without modification. The code diff is a one-line storage-type change at `manager_setup.go:89`; the
operational migration on an existing cluster is more involved — see *Migration
constraint* below.

- *Migration constraint.* `EnsureKVBucket` is **get-then-create**
  (`kvutil/bucket.go:50-64`) — it returns the existing bucket if present,
  creates only on `ErrBucketNotFound`, and **never updates an existing bucket's
  storage type**. So a live `MemoryStorage` election bucket is *not*
  transparently upgraded by the new code. Existing deployments require the
  operator to **delete the bucket** so the new code's `Start` recreates it
  with the new storage (or `partictl apply` with a force / a similar
  re-provision). The delete-then-recreate is exactly the scenario **F1's
  epoch fence is designed to detect**, so **F9-A depends on F1 shipping
  first** (see Section 8 sequencing). Without F1, the migration itself can
  put workers into a stuck-stale `lastSeenLeaderRevision` state until pod
  rotation.
- *Companion: validate `OperationTimeout ≤ ElectionTimeout/3`.* While the
  F9-A PR is already touching the election machinery, add a startup
  validation/warning for this relationship. The lease design gives the leader
  three renew attempts within one `ElectionTimeout` window (renew every
  `ElectionTimeout/3`, `manager_election.go:192`), but each renew is bounded
  by `OperationTimeout` (`manager_election.go:207-209`). With both defaults
  at 10 s (`config.go:360,366`) the *worst-case* single renew can consume the
  entire lease window — `monitorLeadership` is a single-goroutine ticker so a
  hanging renew drops subsequent ticks (`manager_election.go:191-275`). In
  steady state every renew completes in milliseconds and this never fires;
  the concern is only the adversarial slow-NATS case, which is the same case
  F9-A and F9-B target. A warning ("`OperationTimeout` should be ≤
  `ElectionTimeout/3` to preserve the three-attempt renew budget") follows
  the existing `warnOnShortAuditGrace` pattern (`manager_setup.go:387-406`),
  is **LOW change risk** (read-only validation), and naturally lives in the
  F9-A PR. *Not* a stand-alone finding.

**Recommendation R9-B — lease-aware leader (hold through transient blips, step
down at the lease deadline) — DEFERRED.** F9-A closes the dominant *bucket-loss*
churn source but does not close *transient bucket unavailability* (e.g. a
JetStream Raft leader reshuffle on the election bucket causes a few seconds of
errors against the existing bucket). The lease-aware change would close that
residual:

- *Recommended (minimal) variant.* The election agent **already** retains the
  term on a context-timeout renew error; the churn comes from
  `monitorLeadership` flipping `m.isLeader` and stopping the calculator anyway.
  The minimal fix extends that existing leniency up into the manager: on an
  *ambiguous* (context-timeout) renew error, do **not** immediately flip
  `m.isLeader` / stop the calculator — retry the renew, tracking the *initiation
  timestamp of the last successful renew*. Keep leadership while
  `now − lastSuccessfulRenew < ElectionTimeout − margin`; **force self-demote**
  (flip + stop the calculator + `clearLeadership`) once that deadline passes, or
  immediately on a *definite-loss* error. This rides out a transient blip
  without calculator churn yet preserves the split-brain guarantee.
- *Invariant (must hold):* `leader_local_stepdown_deadline + clock_skew ≤
  follower_earliest_acquire` (= bucket TTL since the last write). Derive the
  leader deadline *from* the bucket TTL — never let it exceed it.
- *Follower side — unchanged.* Followers already only acquire after a genuine
  TTL expiry; that is correct, keep it. A follower wrongly promoting requires
  the key to be *wrongly absent* — which is the **F1** wipe-and-recreate case.
  F9-A + F9-B (leader side) + F1 (follower side, epoch fence) together close
  the full split-brain surface.
- *Coupled with F10-A / F6-B — a kept leader must not act on bad inputs.*
  "Retain the leader" is only safe if the kept leader also quiesces destructive
  calculator actions while the input data is untrustworthy (F10-A for workers;
  F6-B for partitions). Otherwise R9-B trades a two-leaders risk for a
  wrong-eviction risk.
- *Audit prerequisite.* `CheckLeadership` is currently wired only into the
  assignment calculator's publish (`LeaderCheck`, `manager_assignment.go:123` —
  the only non-test call site). Before relying on "hold the leader longer,"
  audit that no other leader-exclusive write exists unfenced.

**Defer R9-B until after F9-A is deployed and operationally observed.** If
post-F9-A churn from transient bucket-unavailability proves significant in
practice, revisit; otherwise the HIGH change risk of touching election code is
not justified to address a residual minor churn source.

**Design tension — surfaced.** The simplest correct option for the residual
case is "do nothing — the current design is safe; just document why." R9-A is
recommended because the storage switch is free and addresses the dominant churn
at LOW risk; R9-B is *explicitly contingent* on operational evidence after
R9-A, not pre-emptive.

**Impact if unfixed:**

- **F9-A**: **MEDIUM–HIGH** — the dominant operational churn cause; a routine
  NATS node bounce = fleet-wide leadership thrash and a leaderless gap.
- **F9-B (post-F9-A)**: **LOW–MEDIUM** — only the residual
  transient-unavailability churn remains; not a correctness bug.

**Change risk:**

- **F9-A**: **LOW** — one-line storage type change; identical KV semantics;
  near-zero IOPS impact verified by M1.9 (`iops-investigation/findings.md` §2).
  Migration requires F1 first; the change itself is mechanically trivial.
- **F9-B**: **HIGH** — touches election code, the most safety-critical
  coordination in the system; a mistake here *is* split-brain. The minimal
  variant is still **MEDIUM–HIGH**.

---

### F10 — Double assignment / double partition ownership

**Investigation result.** The literal first hypothesis — that the silent
per-key `Get` omission in `GetHeartbeats` (`worker_monitor.go:229-231`) shrinks
the worker set and causes reassignment — is **REFUTED**: a worker missing from
the `GetHeartbeats` map is classified `unverifiable`, not `behind`
(`calculator_audit.go:111-114`), and `maybeEscalateAudit` escalates only the
`behind` set (`calculator_audit.go:169-183`). That path is fenced. A different,
real path was found.

**F10-A — Partial heartbeat-read degradation → false worker death
(UNFENCED).** Worker-death detection runs through `GetActiveWorkers`
(`worker_monitor.go:162`) → `heartbeatKV.Keys()`. In nats.go v1.50.0,
`KeyValue.Keys()` ranges an ordered-consumer watcher channel; the watcher's
`SetClosedHandler` closes `Updates()` **without a `nil` end-of-data marker**
(`jetstream/kv.go:1335-1339`). If that ordered consumer is dropped/reset
mid-scan, `Keys()` returns the **partial slice it accumulated, with
`err == nil`** (`jetstream/kv.go:1373-1393`). `getActiveWorkers` sees no error →
treats it as a *fresh, complete* observation (`calculator.go:1071-1102`).
Connectivity errors are correctly handled (cache fallback, `fresh=false`,
`calculator.go:1076-1092`) — but a *silently truncated non-empty list* is not an
error. If the truncation is sustained past `EmergencyGracePeriod`, the omitted
workers are confirmed dead (`emergency.go:89-133`) and their partitions
reassigned. There is **no shrink floor**: `rebalance()` guards only
`len(workers) == 0` (`calculator.go:1290-1296`); a drop from N to 1 only logs
(`calculator.go:1271-1277`).

- *Severity is conditional.* If the wrongly-omitted worker is healthy and
  connected, it receives the corrected assignment and stops — a bounded
  overlap (see F10-D). It escalates to **genuine unbounded double ownership**
  when the wrongly-evicted worker is *simultaneously* unable to receive its
  corrected assignment — i.e. it is itself in degraded mode on a cached
  assignment, which degraded mode is *designed* to keep processing. A correlated
  partial NATS failure (heartbeat path flaky for both the leader's read and a
  worker's write/read) makes this realistic.
- *Verification status — important.* The `Keys()` partial-return behavior is
  established from nats.go source (`jetstream/kv.go:1335-1393`), **not
  empirically reproduced.** Per the repo's verify-first discipline, a fix must
  be preceded by a chaos test that drops the heartbeat-bucket ordered consumer
  mid-scan and reproduces the truncated read. Treat F10-A as *strongly
  indicated, pending empirical confirmation.*

**Recommendation R10-A.** (1) Make `GetActiveWorkers` defend against a silently
truncated `Keys()`: cross-check the result count against the last-known worker
count and treat a large unexplained single-poll shrink as non-fresh (degraded),
or require two consecutive shrunk reads before trusting it. (2) Add a "minimum
credible worker set" floor in `rebalance()` analogous to the `len == 0` guard —
refuse to reassign on a suspiciously large shrink without emergency confirmation
across the full grace window.

**F10-B — Two-phase handoff without consumer-side gating (UNFENCED,
config-dependent).** `EnableTwoPhaseHandoff` (default false, `config.go:412`)
makes the leader write per-partition claims to the handoff KV, but enabling it
only starts the manager-side coordinator and reports `CapTwoPhaseHandoff` — it
does not prove the *consumer* will consult those claims. The consumer-side gate
is `ProcessingGate.Enabled` (default false, `processing_gate.go:19,135-139`);
when a processing gate is configured, `WorkerConsumerConfig.SetDefaults`
auto-enables pull gating (`internal/durable/config.go:295-305`) — so it is one
flag, not two. An operator who enables two-phase handoff alone, without a
processing gate on the consumer, believes they have fencing but does not: the
claims are written and never consulted.

**Recommendation R10-B.** Warn (or reject) when `EnableTwoPhaseHandoff` is true
but the consumer does not actually wire a processing gate — but **not at
`Start`**. `CapProcessingGate` is a runtime "successfully wired" bit, set only
after a gate-wrapped handler is created during assignment apply
(`internal/durable/worker_consumer.go:386-398,604-623`), and the manager samples
consumer capabilities only after `handoffCoordinator.Apply` in
`applyAssignmentWithPrev` (`manager_assignment.go:851-859` →
`reportConsumerCapabilities`, `manager.go:815-832`). A `Start`-time check would
false-positive on every correctly-configured gated consumer before its first
non-empty assignment. Place the check at the capability-sampling point instead:
after the first non-empty two-phase apply, warn/reject if `CapProcessingGate`
has still not appeared. (If a construction-time "gate-capable" predicate is
added later, the check could move earlier — but the runtime bit cannot.)

**F10-C — In-flight handler commits after a partition moved (UNFENCED,
deferred).** A handler that *started* under old ownership can finish and commit
side effects after the partition has moved. The commit-fence design
(`docs/plans/partition-fencing/`) is **unimplemented**; the README marks the
design ready to pick up but still lists P1 fixes to fold into the proposal
before implementation begins (`partition-fencing/README.md:22-41,43-75`). Note
its status; no action proposed here beyond keeping it on the roadmap with the
pending P1 fold-in.

**F10-D — Bounded overlap during a normal move (BY DESIGN, not a gap).** In the
default `direct` handoff mode, when P moves A→B, A removes P and B adds P
asynchronously; a brief duplicate-consumption window is documented and accepted
(`config.go:402-405`, `docs/LIFECYCLE.md:252`). Listed for completeness — no
change recommended.

**Stale-leader path — FENCED.** A former leader cannot re-grant a partition:
`LeaderRevision` embed + watcher rejection (`manager_assignment.go:434-437,
602-606`), the pre-Apply `(Version, LeaderRevision)` stale gate (`:831-847`),
and the publisher `LeaderCheck` + `_commit` CAS. No action needed.

**Design tension:** R10-A must not over-correct — a floor that is too strict
*suppresses a legitimate emergency rebalance* when a real mass worker death
occurs. The floor must interact correctly with the existing `EmergencyDetector`
grace window, not duplicate or fight it.

**Impact if unfixed:** **HIGH** — F10-A is the realistic route to genuine double
ownership (the dynamic-partitioning correctness invariant: one partition, one
active owner) and is readiness-blind (a wrong reassignment looks valid).
F10-B/F10-C are MEDIUM.

**Change risk:** **MEDIUM** for R10-A (the floor touches the live rebalance
decision; mistuned, it suppresses real emergencies — needs the chaos
reproducer first and careful grace-window interplay). **LOW** for R10-B (a
diagnostic warning emitted at the first two-phase apply — no behavior change).

---

## 7. Risk & Impact Ranking + Implementation Discipline

The user's constraint: *these fixes are risky and easy to produce side effects;
keep changes as small as possible and verify them step by step.* The ranking
below is ordered to **build a verify-as-you-go rhythm with low-risk work first**,
then take the high-impact correctness fixes carefully, then the high-risk
coordination work last.

| Order | Finding | Impact if unfixed | Change risk | Reproducer required first | Notes |
|---|---|---|---|---|---|
| 1 | **F7** connection-config docs + warning | LOW–MED | **LOW** | no | Docs + read-only startup warning. Safe warm-up. |
| 2 | **F8** source reconcile-disable guard | LOW | **LOW** | no | Godoc + optional config warning; `source` only. |
| 3 | **F10-B** two-phase config validation | MED | **LOW** | no | Warning at first two-phase apply — no behavior change. |
| 4 | **F6-A** source-bucket escalation hook | MED | LOW–MED | yes (bucket-delete test) | Additive metric + `OnSourceUnavailable` hook. |
| 5 | **F3** stableID NotFound classification | MED–HIGH | **MED** | yes | Small surface; changes self-stop trigger. |
| 6 | **F1** epoch fence | **HIGH** | **MED** | yes (wipe+recreate test) | Highest-impact readiness-blind gap; additive detection. **Prerequisite for F9-A.** |
| 7 | **F9-A** election bucket → `FileStorage` R≥3 | MED–HIGH | **LOW** | yes (storage-migrate test) | One-line + migration; ~0 IOPS cost per M1.9. Depends on F1. Includes a companion `OperationTimeout ≤ ElectionTimeout/3` startup-warning. |
| 8 | **F6-B** partition-input freeze (no reassign on empty / shrunk non-erroring source) | **HIGH** | **MED** | yes | Parallel to F10-A; "minimum credible inputs" for the calculator. Erroring path already handled. |
| 9 | **F5** stream-gone handling | **HIGH** | **MED** | yes | `OnStreamMissing` hook additive; checkpoint reseed needs care. |
| 10 | **F2** bounded retry / give-up | MED–HIGH | **MED–HIGH** | yes (per loop) | **One PR per loop** — never all at once. |
| 11 | **F10-A** false-death floor + truncated-`Keys()` defense | **HIGH** | **MED** | **yes — chaos test FIRST** | Must not suppress legitimate emergencies. |
| 12 | **F9-B** lease-aware leader | LOW–MED (post-F9-A) | **HIGH** | yes | DEFERRED — only if residual transient churn warrants. Split-brain risk; minimal variant still MED–HIGH. |
| 13 | **F4** in-process re-provision | LOW–MED (k8s) | **HIGH** | yes | Optional; may be dropped. F9-A largely subsumes the election-bucket-only case. |

### Implementation discipline (applies to every item)

1. **One finding per PR.** F2 is further split — one PR per retry loop. Never
   bundle findings; a bundled change cannot be bisected when a regression
   appears.
2. **Reproducer-first.** Where "Reproducer required" is *yes*, write the failing
   test **first** and confirm it fails on the parent commit before writing the
   fix (the repo's established verify-first discipline). For F10-A this means a
   chaos test that reproduces the truncated `Keys()` read — the fix is not
   justified until the bug is observed.
3. **Step-by-step verification.** Run the full lint + build + test suite between
   every finding; do not start the next until the previous is merge-clean.
4. **Extra scrutiny for HIGH change-risk (F9-B, F4, F2).** These get an external
   review round (`/post-impl-review`) before merge; F9-B additionally needs the
   lease-ordering invariant stated and checked explicitly. F9-A is mechanically
   trivial but its *migration* needs F1 in place first.
5. **Additive before behavioral.** Items 1–4 and 6 are mostly additive
   (warnings, metrics, hooks, detection) — they make failures *visible* without
   changing recovery behavior, so they are safe to land early and they improve
   observability for the riskier items that follow.
6. **No drive-by refactors.** Each PR touches only what its finding requires.
   A recovery-controller consolidation is a separate, future effort — do not
   fold it in.

---

## 8. Suggested Sequencing

Four phases, ordered by the ranking in Section 7 — low-risk confidence-builders
first, the high-impact correctness gaps next done carefully, the high-risk
coordination work last.

- **Phase 0 — low-risk quick wins (F7, F8, F10-B).** Docs, config guards, and a
  diagnostic warning (F10-B fires at the first two-phase apply, not at `Start`).
  No behavior change. Establishes the one-PR-per-finding, verify-between rhythm.
- **Phase 1 — make readiness-blind failures visible (F6-A, F3, F1).** Additive
  detection / escalation so the deployment's readiness-probe recovery can
  actually engage on a wipe-recreate (F1), a source loss (F6-A), and a stableID
  bucket loss (F3). F1 ships here because it is a prerequisite for F9-A's
  migration. Each with a reproducer.
- **Phase 2 — eliminate the dominant churn and bound operational failures
  (F9-A, F6-B, F5, F2, F10-A).** F9-A first (the election-bucket storage switch
  — one line, near-zero IOPS cost per M1.9; depends on F1 from Phase 1). Then
  F6-B (partition-input freeze), F5 (stream-gone hook + `OnStreamMissing`), F2
  (bounded retry loops, one PR per loop), and F10-A (false-death floor — chaos
  reproducer first). After this phase every unrecoverable failure trips the
  readiness probe, and the dominant leadership-churn source is gone.
- **Phase 3 — DEFERRED high-risk and optional (F9-B, F4).** F9-B (lease-aware
  leader) is gated on operational evidence after F9-A — only ship if residual
  transient bucket-unavailability churn is significant; the HIGH change risk is
  not justified pre-emptively. F4 (in-process re-provision) is largely
  subsumed by F9-A for the election bucket; consider only if the rest of the
  bucket-loss pod-rotation cost is operationally significant.

Phases 0–2 deliver the correctness and observability wins at LOW–MEDIUM change
risk. Phase 3 is deliberately isolated so the riskiest change lands alone,
against an otherwise-stable baseline.

---

## 9. Resolved Decisions

All open questions have been resolved by the user. Recorded here for the
implementation phase to reference.

1. **Deployment target — RESOLVED.** Kubernetes + `OnDegraded` → readiness-probe
   → pod-rotation. Drives Section 2's readiness lens; F4 demoted to optional.
2. **F10-A empirical confirmation — RESOLVED: build the chaos reproducer first.**
   F10-A rests on nats.go-source analysis of `Keys()` truncation
   (`jetstream/kv.go:1335-1393`); the fix must not precede an empirical
   reproduction. Section 7 row 11 and Section 8 Phase 2 both pin "chaos test
   FIRST" as a hard prerequisite.
3. **F9 — RESOLVED: evaluate IOPS first, then F9-A primary / F9-B deferred.**
   IOPS investigation cell M1.9 (*all parti KV buckets → memory*) yields
   **−2 % / −1 % of total IOPS at N=1000 / N=3000 — within noise**
   (`docs/plans/iops-investigation/findings.md` §2). So switching the election
   bucket from `MemoryStorage` to `FileStorage` R≥3 (F9-A) is **effectively
   free on IOPS** and closes the dominant churn cause at LOW change risk.
   F9-B (lease-aware leader, HIGH change risk) is **deferred** unless the
   residual transient-bucket-unavailability churn after F9-A proves
   operationally significant.
4. **`MemoryStorage` for the election bucket — RESOLVED: switch to
   `FileStorage` R≥3 (F9-A).** Same decision as #3.
5. **New public API hooks — RESOLVED: add both `OnStreamMissing` (F5) and
   `OnSourceUnavailable` (F6-A).** AND a load-bearing behavioral requirement:
   when the source is unavailable or returns empty, the leader must **retain
   the cached partition list and not proceed with reassignment until the
   source returns a non-empty list** — captured as F6-B in Section 6 and as
   Phase 2 row 8 in the ranking.
6. **F4 default — RESOLVED: off-by-default (opt-in).** Consistent with the
   "small changes, verify step by step" constraint and with F9-A largely
   subsuming the election-bucket-only motivation.

---

## Appendix — Evidence Index

- **Connectivity / reconcilers:** `manager.go:410-414,386-393`;
  `manager_assignment.go:325,367,478,509,398-418`;
  `claim_resolver.go:504,525,613,748,783,882,902`;
  `source/nats_kv.go:729,772,812,864`;
  `internal/assignment/worker_monitor.go:267-294,299,349`;
  `test/integration/failure/claim_resolver_nats_restart_test.go:34-41`.
- **KV buckets:** `config.go:14-37`;
  `manager_setup.go:39-53,89-113,158-186,224-270,291-349`;
  `internal/kvbuckets/builder.go:35`; `kvutil` `EnsureKVBucketWithRetry`;
  `internal/election/nats_election.go:140,154,200-202`;
  `internal/stableid/claimer.go:299-338,329,347,364-369`.
- **Stream / consumer recovery:** `internal/recovery/classify.go:32-50`;
  `internal/recovery/controller.go:118-165,237-316`;
  `internal/recovery/config.go:12-38`; `internal/recovery/checkpoint.go:33-59`;
  `internal/durable/partition_consumer.go:195-199,372-439,442-474,508-512`;
  `consumer/dynamic.go:310-315`.
- **Manager / degraded mode:** `manager.go:417,434,441,455-456`;
  `manager_degraded.go:12-30,33-55,80-132,153-190,229-245`;
  `manager_election.go:91-98,191-275,210-244,361`.
- **Election (F9):** `internal/election/nats_election.go:140,176-218,351-390`;
  `manager_election.go:191-275`; `manager_setup.go:89`;
  `config.go:362-366` (`ElectionTimeout`).
- **Double assignment (F10):** `internal/assignment/worker_monitor.go:162-194,
  208-236`; `internal/assignment/calculator.go:880,976-991,1071-1102,1238-1383,
  1271-1296`; `internal/assignment/calculator_audit.go:59-86,111-114,169-183`;
  `internal/assignment/emergency.go:89-133`;
  `internal/assignment/handoff/{direct.go:28-41,twophase.go:57-121,
  coordinator.go:107-139}`;
  `consumer/dynamic.go:83-88`; `internal/durable/processing_gate.go:19,137-153`;
  `config.go:400-416`; `docs/LIFECYCLE.md:180-254`;
  `docs/plans/partition-fencing/README.md:22-41`;
  `nats.go@v1.50.0/jetstream/kv.go:1335-1393`.
- **Documented design rationale:** `docs/OPERATIONS.md:616-647,649-659,626`.
