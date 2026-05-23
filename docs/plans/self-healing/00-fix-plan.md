# Self-Healing Hardening — Phased Implementation Plan

Derived from [`findings.md`](./findings.md) (reviewed clean across 9 rounds;
trail in [`review-trail.md`](./review-trail.md); do not re-investigate). This
document turns the ten findings into a sequenced per-PR plan organized by the
phases recorded in the findings doc's §8.

## Organizing invariant

**Every unrecoverable failure must trip the Kubernetes readiness probe.**

Deployment is k8s with `OnDegraded → readiness probe → pod rotation`
(review §2). A finding's danger is its readiness blindness — a worker that stays
`Ready` while silently broken defeats the recovery design. Each per-PR section
states *how this trips readiness* in one line so the engineer can lose neither
sight of the invariant nor of the deployment context that justifies it.

## Resolved decisions (do not re-litigate)

1. **Plan shape** — phased; one finding per PR; F2 split further to one PR per
   retry loop. Per-PR specs are written **lazily**, only when the prior PR is
   merge-clean (mirrors `docs/plans/worker-state-hardening/README.md` — avoids
   speculative spec drift).
2. **F4 stays optional, off by default** — likely dropped given F9-A subsumes
   the election-bucket case.
3. **F9 split** — F9-A primary (one-line `MemoryStorage → FileStorage R≥3`),
   F9-B deferred. F9-A depends on F1.
4. **Public hooks added** — `OnStreamMissing` (F5) and `OnSourceUnavailable`
   (F6-A); plus the F6-B behavioural requirement (leader retains cached
   partition list; no fleet-wide reassignment on empty / shrunk non-erroring
   source observation).
5. **F6-B implementation location** — **calculator-layer** (see F6-B §Design
   for the trade-off vs source-layer).
6. **F10-A is gated on a chaos reproducer first**. Truncated `Keys()` ordered
   consumer drop scenario must be empirically reproduced before any fix lands.
7. **F2 envelope** lands with the first call-site wiring (emerging-helper
   convention); subsequent F2 PRs reuse.

## Implementation discipline (applies to every PR)

The discipline below is stated once and assumed in every per-PR section.

1. **One finding per PR.** Bundle nothing. F2 is itself split — one PR per
   retry loop.
2. **Reproducer-first.** For every finding marked *reproducer required*, write
   the failing test, confirm it fails on the parent commit, then implement.
   Memory pin: `feedback_verify_first_with_reproducer`. For F10-A specifically,
   the chaos reproducer is a hard gate — no fix until the truncated `Keys()`
   read is empirically observed.
3. **Validation loop per PR.** Spec → impl → `/simplify` → `/post-impl-review`
   loop → squash on merge verdict (memory pin:
   `feedback_post_impl_review_workflow`). External-reviewer dispatch via
   `/codex:review` first; fall back to `copilot` only on codex failure
   (memory pin: `feedback_codex_review_preferred`).
4. **Lint + build + test must be green before the next PR begins.** No
   "I'll fix this in the next PR" cross-PR carry-overs. The plan tolerates the
   per-PR cost; it does not tolerate compounding debt.
5. **No drive-by refactors.** Each PR touches only what its finding requires.
   A recovery-controller consolidation is a separate, future effort — do not
   fold it in here.
6. **Commit-message hygiene.** Plan body uses `F1 / F9-A / Phase 2` labels
   freely; **derived commits and PR titles MUST NOT** (memory pin:
   `feedback_no_plan_jargon_in_commits`). A reader of `git log` lacks this
   plan's context.
7. **Implementation-agent + review-effort tuning.** Model recommendations
   are stated in version-neutral terms so the guidance survives model
   renames:
   - **Use the strongest available Claude model** (today: Opus-tier) for
     any PR that (a) carries a `MED–HIGH` or `HIGH` change-risk rating in
     the table below, (b) introduces new state on the `Calculator` struct,
     (c) introduces new ordering or sentinel contracts in the recovery
     controller, OR (d) requires a chaos reproducer. The escalation
     list this produces for this plan: **P1.3 (F1), P2.1 (F9-A),
     P2.2 (F6-B), P2.4a (F2 envelope), P2.4d (F2 partition_consumer),
     P2.3 (F5), P2.5 (F10-A)**. P2.1 escalates not for code complexity
     but because the migration runbook is operator-facing and ships
     with the PR.
   - **Use the standard tier** (today: Sonnet-tier) on `LOW`/`MED`
     change-risk PRs that are mechanically bounded: P0.1, P0.2, P0.3,
     P1.1, P1.2, P2.4b, P2.4c.
   - **Post-impl review effort** follows the AGENTS.md convention plus
     the change-risk column: every PR in the escalation list above runs
     `/codex:review` at `xhigh`; every other in-scope PR runs at
     `high`. F2 PRs after P2.4a (the envelope-introduction PR) run at
     `high` once P2.4a has locked the envelope shape at `xhigh`.

## Phased PR sequence

Numbers (e.g. `P0.1`) are stable identifiers used in the per-finding sections
below. They are **not** commit-message labels.

The `Tier` column is a version-neutral model recommendation per the
heuristic in discipline rule 7 above: **strong** = use the strongest
available Claude model (today Opus-tier); **standard** = standard tier
(today Sonnet-tier). The `Review` column is the `/codex:review` effort
for that PR's post-impl review.

| Order | ID | Finding | Impact | Change risk | Reproducer required | Tier | Review | Notes |
|---|---|---|---|---|---|---|---|---|
| P0.1 | F7 | Connection-config docs + startup warning | LOW–MED | **LOW** | no | standard | `high` | Warm-up; docs + read-only warning |
| P0.2 | F8 | `source.WithReconcileInterval(0)` guard | LOW | **LOW** | no | standard | `high` | Godoc + optional config warning |
| P0.3 | F10-B | Two-phase config diagnostic warning | MED | **LOW** | no | standard | `high` | Warning at first two-phase apply (NOT at `Start`) |
| P1.1 | F6-A | Source-bucket escalation hook | MED | LOW–MED | yes | standard | `high` | `OnSourceUnavailable` hook + metric |
| P1.2 | F3 | stableID NotFound classification | MED–HIGH | **MED** | yes | standard | `high` | Small surface; changes self-stop trigger |
| P1.3 | F1 | Epoch fence (bucket re-create detection) | **HIGH** | **MED** | yes | **strong** | `xhigh` | **Prerequisite for F9-A migration** |
| P2.1 | F9-A | Election bucket → `FileStorage` R≥3 + `OperationTimeout` warning | MED–HIGH | **LOW** | yes | **strong** | `xhigh` | Depends on F1; operator-facing migration runbook |
| P2.2 | F6-B | Calculator-layer "minimum credible partition input" guard | **HIGH** | **MED** | yes | **strong** | `xhigh` | New `Calculator` state; symmetric with F10-A |
| P2.4a | F2 | Bounded-retry envelope + `restartWatcher` wiring | MED–HIGH | **MED–HIGH** | yes | **strong** | `xhigh` | Envelope crystallises; locks shape for 3 reuse PRs |
| P2.4b | F2 | Apply envelope to `claim_resolver.go` handoff watcher | MED–HIGH | **MED–HIGH** | yes | standard | `high` | Reuses envelope from P2.4a |
| P2.4c | F2 | Apply envelope to `monitorAssignmentChanges` | MED–HIGH | **MED–HIGH** | yes | standard | `high` | Reuses envelope from P2.4a |
| P2.4d | F2 | Apply envelope to `partition_consumer.go` recovery | MED–HIGH | **MED–HIGH** | yes | **strong** | `xhigh` | Larger surface; **prerequisite for P2.3** |
| P2.3 | F5 | Stream-gone hook + checkpoint reset | **HIGH** | **MED** | yes | **strong** | `xhigh` | Three coordinated mechanisms; depends on P2.4d |
| P2.5 | F10-A | Truncated-`Keys()` defense + worker-set floor | **HIGH** | **MED** | **yes — chaos test FIRST** | **strong** | `xhigh` | Hard gate — no fix until reproducer reproduces |
| P3.1 | F9-B | Lease-aware leader (DEFERRED) | LOW–MED post-F9-A | **HIGH** | yes | tbd | tbd | Gated; re-evaluate at re-promotion |
| P3.2 | F4 | In-process re-provision of coordination buckets (OPTIONAL) | LOW–MED on k8s | **HIGH** | yes | tbd | tbd | Gated; likely dropped |

13 in-scope PRs (P0–P2). 2 deferred (P3). After P2 every unrecoverable failure
trips the readiness probe and the dominant leadership-churn source is gone.

---

## Phase 0 — Low-risk warm-ups

Three additive, behavior-neutral PRs that establish the per-PR rhythm. No
behavior change; just observability and configuration safety.

### P0.1 (F7) — Connection-config docs + startup warning

**Anchors** (verified against the review doc; re-verify at PR start):
- `manager.go:258, 410-414` — conn is caller-injected; no callbacks registered
- `doc.go:30`, `examples/basic/main.go:34` — examples use bare `nats.Connect`
- `manager_setup.go:387-406` — `warnOnShortAuditGrace` pattern to mirror

**Scope.** Add documentation for the required nats.go connection posture
(`MaxReconnects = -1`, sane `ReconnectWait`/`ReconnectJitter`,
`RetryOnFailedConnect`) and, at `Manager.Start`, emit a read-only warning if
`conn.Opts.MaxReconnects` is finite. No behavior change.

**Design.**
- Docs: edit `doc.go` package comment, `docs/OPERATIONS.md` (connection
  section), and the basic example. Document the zombie outcome that finite
  `MaxReconnects` produces (review §F7).
- Warning: in `manager_setup.go`, after the existing `warnOnShortAuditGrace`,
  add a parallel `warnOnFiniteMaxReconnects(m.conn.Opts, m.logger)`. Read-only;
  emits a `Warn`-level log line; does not block `Start`.

**Reproducer test list.** No reproducer required (no behavior change). One
unit test for the warning emission (table-driven across `MaxReconnects = -1`,
0, finite-positive).

**Verification gates.**
- `make lint && make test` green.
- Manual: spin a manager with `MaxReconnects = 5` and confirm the warning
  appears once at `Start`; spin with `-1` and confirm silence.
- Godoc review: `go doc github.com/arloliu/parti/v2` shows the updated
  guidance.

**How this trips readiness.** It doesn't directly — but documents the
posture so finite `MaxReconnects` (which today turns an outage into a stuck
`CLOSED` zombie that *does* enter degraded mode and rotate the pod) is
operator-visible and avoidable up front.

**Dependencies & sequencing.** Independent. First PR of Phase 0 because
it is the smallest no-behavior-change change.

**Out of scope.** Programmatic enforcement (rejecting finite `MaxReconnects`)
— warning only, per review §F7. Caller's responsibility to fix.

---

### P0.2 (F8) — `source.WithReconcileInterval(0)` guard

**Anchors.**
- `source/nats_kv.go:50-63, 807-814` — `reconcileLoop` exits on non-positive
  interval with no leadership probe; only documents "disables polling"
- `consumer/resolver_config.go:61-63`, `internal/durable/config.go:89` — the
  **safe** consumer path; do not touch
- `manager_setup.go:387-406` — `warnOnShortAuditGrace` pattern

**Scope.** Source-only. Either clamp `0` to a minimum + warn, or update Godoc
to state plainly that disabling the reconciler disables server-restart
recovery for the source watcher.

**Design — locked.** **Godoc + optional config warning, no rejection.** The
review doc allows both options; rejection is more disruptive (existing users
who explicitly disable polling break on upgrade), and the safer route is to
make the foot-gun loud:
- Update `WithReconcileInterval`'s Godoc to add a paragraph naming the
  consequence: "Setting this to zero disables the reconciler, which is the
  load-bearing recovery path for silently-stalled KV watchers after a NATS
  server restart (see `test/integration/failure/claim_resolver_nats_restart_test.go`
  and review §3 'load-bearing reconciler'). Disable only with full awareness."
- At source `Start`, if `s.reconcileInterval <= 0` AND no leadership probe is
  set, emit a warn-level log line ("source reconciler disabled; server-restart
  recovery will not work").

**Reproducer test list.** No correctness reproducer. One unit test for the
warning (interval=0 emits, interval>0 silent).

**Verification gates.**
- `make lint && make test` green.
- Godoc lint: `go doc github.com/arloliu/parti/v2/source.WithReconcileInterval`
  shows the new paragraph.

**How this trips readiness.** Indirect: documents that the foot-gun creates a
readiness-blind silent stall so operators avoid it.

**Dependencies & sequencing.** Independent. Sequenced after P0.1 only
because the warning helper pattern (mirrors P0.1's) is more familiar
once P0.1 has landed.

**Out of scope.** Touching `consumer.ResolverConfig` or
`internal/durable/config.go` — both are already safe (review §F8).

---

### P0.3 (F10-B) — Two-phase config diagnostic warning

**Anchors.**
- `config.go:412` — `EnableTwoPhaseHandoff` default false
- `internal/durable/processing_gate.go:19, 135-139` — `ProcessingGate.Enabled`
  default false
- `internal/durable/worker_consumer.go:386-398, 604-623` — `CapProcessingGate`
  set after gate-wrapped handler creation
- `manager_assignment.go:851-859` → `manager.go:815-832` —
  `reportConsumerCapabilities` after first non-empty two-phase apply
- `internal/durable/config.go:295-305` — gate-config → pull-gating auto-enable

**Scope.** Add a warn-level log when `EnableTwoPhaseHandoff == true` and after
the first non-empty two-phase apply the manager has still **not** seen
`CapProcessingGate` in the consumer's reported capabilities. **Not** a
`Start`-time check (would false-positive on every correctly-configured gated
consumer before its first assignment).

**Design — locked.** Place the check inside `reportConsumerCapabilities`
(`manager.go:815-832`), after the existing capability merge. Single log line;
no behavior change.

```go
// In reportConsumerCapabilities, after merging caps:
if m.cfg.EnableTwoPhaseHandoff &&
    !m.capProcessingGateWarned &&
    !caps.Has(types.CapProcessingGate) {
    m.logger.Warn("two-phase handoff is enabled but the consumer reports no processing gate; partition claims are written and never consulted",
        "remedy", "wire a processing gate on the consumer (e.g. consumer.Dynamic) so claims fence delivery")
    m.capProcessingGateWarned = true
}
```

The `capProcessingGateWarned` field guards against repeated warnings on each
re-apply.

**Reproducer test list.**
- *T1 (must fail on parent).* Set `EnableTwoPhaseHandoff = true` on a manager
  whose consumer does not wire a processing gate. Trigger an apply that
  reports capabilities with no `CapProcessingGate` bit. Assert the warning is
  emitted exactly once. On parent (no warning logic), the assertion fails.
- *T2 (positive case).* Same setup but with a gated consumer. Assert **no**
  warning is emitted. Confirms no false-positive on the happy path.

**Verification gates.**
- `make lint && make test && make test-race` green.
- Code-review checklist: warning fires exactly once across N applies (the
  guard field is the design's load-bearing element).

**How this trips readiness.** It doesn't directly. The warning makes a
misconfiguration that today produces an unfenced two-phase claim flow
**operator-visible** — they can re-deploy with the gate enabled, which is
the actual fix.

**Dependencies & sequencing.** Independent. Last of Phase 0 — the
behavior the test exercises (capability sampling after a real
two-phase apply) is the most integration-shaped of the Phase 0
tests, so landing P0.1/P0.2 first keeps the per-PR test-shape
gradient gentle.

**Out of scope.**
- Rejecting the misconfiguration at `Start` (cannot — capability bit is
  set at runtime, not at construction; review explicitly forbids this).
- Adding a construction-time "gate-capable" predicate (mentioned as a
  follow-up in the review; not in scope here).

---

## Phase 1 — Additive correctness (make readiness-blind failures visible)

Three PRs that add detection and escalation paths so the readiness-probe
recovery can actually engage. F1 specifically must ship here because F9-A's
migration depends on it.

### P1.1 (F6-A) — Source-bucket escalation hook

**Anchors.**
- `source/nats_kv.go:178` — handle is injected; library never creates it
- `source/nats_kv.go:772-805` — `restartWatcher` retries forever
- `source/nats_kv.go:864-878` — `reconcileOnce` treats `ErrBucketNotFound` as
  generic logged error

**Scope.** Add an `OnSourceUnavailable(err error)` hook and a
`parti_source_bucket_missing` metric. When `restartWatcher` or `reconcileOnce`
sees `jetstream.ErrBucketNotFound`, the hook fires (rate-limited) and the
metric is set. Wiring the hook to readiness is the **caller's** responsibility
(documented).

**Design — locked.**
- New public type:
  ```go
  // SourceUnavailableHook fires when the partition-source bucket is observed
  // to be missing on the live connection. Wire into your readiness logic to
  // rotate the pod if the source vanishes (the library cannot recreate a
  // user-owned bucket; review §5 category A).
  type SourceUnavailableHook func(err error)
  ```
- `source.NatsKV` accepts the hook via a new option `WithUnavailableHook`.
- Hook firing: at the first observation of `ErrBucketNotFound` in either
  `restartWatcher` or `reconcileOnce`. Subsequent observations within
  a `cooldown` (default 30 s, matching the reconcile interval) suppress the
  hook to avoid log-spam; the metric stays set until a successful operation
  clears it.
- Metric: `parti_source_bucket_missing` (gauge 0/1) registered through the
  existing metrics interface (see other `parti_*` metrics for the registration
  site).

**Reproducer test list.**
- *T1 (must fail on parent).* Integration test under
  `test/integration/failure/`: create a source bucket, start the manager,
  delete the bucket, assert `OnSourceUnavailable` fires within 30 s and the
  metric reads 1. On parent, the hook is absent — test fails at compile time
  (or, after introducing the hook field via testing helper, at the
  `firedWithin` assertion).
- *T2.* The recreate-bucket case: after T1, re-create the bucket; assert
  the metric returns to 0 within one reconcile interval.

**Verification gates.**
- `make lint && make test && make test-race && make test-integration` green.
- Confirm hook signature is reflected in `docs/OPERATIONS.md` (responsibility
  split between library and operator restated).
- New exported symbol audit: only `SourceUnavailableHook` and `WithUnavailableHook`
  added; no existing exported API changed.

**How this trips readiness.** The k8s operator wires `OnSourceUnavailable`
into a readiness-probe flag (mirroring the existing `OnDegraded` pattern); a
silent source loss now fails readiness and rotates the pod.

**Dependencies & sequencing.** Independent. Ships first in Phase 1 because
it is the smallest additive change in the phase.

**Out of scope.**
- Library auto-recreating the source bucket (category A — forbidden).
- The retry-bounding loop on `restartWatcher` — that is F2's territory
  (PR P2.4a).

---

### P1.2 (F3) — stableID NotFound classification

**Anchors.**
- `internal/stableid/claimer.go:364-368` — renew translates only
  `jetstream.ErrKeyExists` to `ErrClaimLost`
- `internal/stableid/claimer.go:369` — every other error returns the generic
  `"failed to renew ID"`
- `internal/stableid/claimer.go:329` — `claimLostShutdown` path
- `manager_election.go:91-98` — self-stop wiring
- `docs/OPERATIONS.md:649-659` — documented contract

**Scope.** In `Claimer.renew`, classify `jetstream.ErrBucketNotFound` and
`jetstream.ErrStreamNotFound` as claim-loss (return `ErrClaimLost`) so the
existing self-stop machinery runs. Matches the documented contract.

**Design — locked.** Single error-classification branch in `renew`:

```go
// Inside Claimer.renew, replacing the existing single-case classification:
switch {
case errors.Is(err, jetstream.ErrKeyExists):
    return ErrClaimLost
case errors.Is(err, jetstream.ErrBucketNotFound),
     errors.Is(err, jetstream.ErrStreamNotFound):
    return ErrClaimLost
default:
    return fmt.Errorf("failed to renew ID: %w", err)
}
```

The downstream self-stop (`claimLostShutdown` → `OnError` → pod rotation) is
unchanged.

**Reproducer test list.**
- *T1 (must fail on parent).* Unit test in `internal/stableid/claimer_test.go`:
  inject a KV stub whose `Update`-during-renew returns
  `jetstream.ErrBucketNotFound`; call `renew`; assert
  `errors.Is(err, ErrClaimLost)`. On parent the test fails (gets generic
  wrapped error).
- *T2 (must fail on parent).* Same with `jetstream.ErrStreamNotFound`.
- *T3 (regression-guard).* The existing `ErrKeyExists` case still classifies
  as `ErrClaimLost` (regression test for the pre-existing branch).
- *T4 (negative).* Some other NATS error (e.g. a context timeout) does **not**
  classify as `ErrClaimLost` — must keep the generic wrap. Prevents
  over-classification.

**Verification gates.**
- `make lint && make test && make test-race` green.
- Manual: confirm `claimLostShutdown` actually runs on T1/T2 by also adding
  a small integration-style test that wires the real shutdown path.

**How this trips readiness.** A vanished stableID bucket now triggers
`claimLostShutdown` → `OnError` → existing readiness flip → pod restart →
re-claim into the (presumably re-provisioned) bucket cleanly.

**Dependencies & sequencing.** Independent. After P1.1 because P1.1 is
smaller; before P1.3 because F1 is the riskier landing.

**Out of scope.**
- Preventing the brief duplicate-ID window during a wipe — closed by
  F1 (epoch fence). R3 ensures the worker fails *safe*.

---

### P1.3 (F1) — Epoch fence (bucket re-create detection)

**Anchors.**
- `manager.go:455-456` — cached `m.assignmentKV` / `m.heartbeatKV` handles
- `manager_degraded.go:33-55` — `monitorNATSConnection` (only checks
  `conn.Status()`)
- `manager_degraded.go:80-132` — `recordKVError` (only fires on
  connectivity/NotFound)
- `manager_setup.go:158-186` — KV bucket creation in `Start`
- `provision/marker.go` — alternative epoch source (mentioned in review)

**Scope.** Cache an immutable per-bucket identity at `Start` and verify it in
the reconcilers. Detect wipe-and-recreate under the same names; enter degraded
/ fire `OnDegraded` with a distinct reason. **This is the highest-impact
readiness-blind gap** and the prerequisite for F9-A's migration.

**Design — locked.** **Use the JetStream stream `Created` timestamp** as the
epoch source. Rationale:
- It is an immutable server value, available via `StreamInfo` on any KV bucket
  (each bucket is backed by a `KV_<name>` JetStream stream).
- It changes precisely when the stream backing the bucket is recreated — the
  exact event F1 must detect.
- It is independent of the `provision/marker` mechanism (which is provisioning
  state and could itself be wiped in the F1 scenario).

Mechanism:
1. At `Start`, after `EnsureKVBucket` for each Parti-owned bucket
   (`parti-assignment`, `parti-heartbeat`, `parti-election`, `parti-handoff`,
   `parti-stableid`), call `kvutil.StreamInfoForBucket(ctx, js, bucketName)`
   (new helper) and cache `Created`/`StreamName` into a `bucketEpoch` map on
   the manager.
2. Each KV-watcher reconciler (assignment, commit, claim-resolver, source —
   per review §3 inventory) gains a single new step after its existing
   reconcile pass: re-read `StreamInfo`; if the cached `Created` does not
   match the live `Created`, call `m.enterDegraded("bucket-recreated:<name>")`
   (new degraded reason constant).
3. `OnDegraded` receives the new reason verbatim. The existing
   `OnDegraded` → readiness probe wiring then rotates the pod.

New error / sentinel:
```go
// In manager_degraded.go:
const degradedReasonBucketRecreated = "bucket-recreated"
```

No recovery is attempted in-process. F1 is detection-only; rotation is the
recovery.

**Reproducer test list.**
- *T1 (must fail on parent — primary).* Integration test under
  `test/integration/failure/`: start manager → record stream `Created` →
  shut down → delete bucket + stream → re-create bucket under the same name
  → re-start the watchers (or use a fresh manager instance per test isolation
  rules). Assert: within one reconcile interval, `OnDegraded` fires with
  `reason == "bucket-recreated:<bucket>"`. On parent, the assertion times out.
- *T2.* The happy-path case: same bucket, no recreate. Reconcilers fire
  normally; no `OnDegraded`. Prevents false-positive on a healthy bucket.
- *T3.* A NATS server restart with state intact (per the load-bearing
  reconciler empirical finding) does **not** trip the epoch fence —
  `Created` is preserved across the restart because the stream is the same.
- *T4 (per-bucket coverage).* T1 must run against each of the five Parti
  buckets (assignment / heartbeat / election / handoff / stableid) since they
  each have an independent reconciler call site. Parameterize T1.

**Verification gates.**
- `make lint && make test && make test-race && make test-integration` green.
- Manual: confirm the new `kvutil` helper does not regress existing
  `EnsureKVBucket` callers (zero touch — it's a read-only addition).
- Confirm the degraded reason string is exposed in metrics (`parti_degraded_total`
  or equivalent counter) tagged by reason.

**How this trips readiness.** Direct: detection enters degraded → `OnDegraded`
→ readiness flip → pod rotation → restart re-provisions every missing bucket
via get-first `EnsureKVBucket`. The previously-silent wipe-and-recreate path
now triggers the *standard* recovery loop (findings.md §3 "load-bearing
reconciler" baseline).

**Dependencies & sequencing.** Independent of P1.1 and P1.2. **Must merge
before P2.1 (F9-A)** — F9-A's operator migration runbook deletes the
election bucket on existing clusters, and without F1 in place the migration
itself can leave workers in a stuck-stale `lastSeenLeaderRevision` state
until pod rotation. The plan therefore makes F1 a hard prerequisite for F9-A.

**Out of scope.**
- Healing the corruption in-process. F1 is fail-loud only.
- The `provision/marker` alternative — mentioned in the review but not chosen
  here (see Design rationale).

---

## Phase 2 — Dominant fixes

Five findings (P2.1–P2.5), one of which is itself split into four PRs
(P2.4a–d for F2). After this phase, every unrecoverable failure trips the
readiness probe and the dominant leadership-churn source is gone.

### P2.1 (F9-A) — Election bucket → `FileStorage` R≥3 + companion warning

**Anchors.**
- `manager_setup.go:89` — the one-line change (`MemoryStorage` →
  `FileStorage` R≥3)
- `internal/election/nats_election.go:140, 176-218, 351-390` — election
  semantics (TTL-driven; identical on either storage)
- `manager_election.go:191-275` — `monitorLeadership`; the renew loop and
  `OperationTimeout` clamp
- `config.go:360, 366` — `OperationTimeout` and `ElectionTimeout` defaults
  (both 10 s)
- `manager_setup.go:387-406` — `warnOnShortAuditGrace` pattern (mirrored
  by the companion warning)
- `kvutil/bucket.go:50-64` — `EnsureKVBucket` is get-then-create; does **not**
  upgrade an existing bucket's storage type (the migration constraint)
- `docs/plans/iops-investigation/findings.md` §2 cell M1.9 — IOPS evidence:
  −2 % / −1 % at N=1000 / N=3000 (within noise) → the switch is
  **effectively free** on the IOPS dimension

**Scope.** Two things in one PR (review §F9-A explicitly bundles the warning
companion with the storage switch because both touch the election machinery):
1. Change the election bucket storage from `MemoryStorage` to `FileStorage`,
   `Replicas: 3` (or whatever R≥3 the cluster is configured for; current
   cluster defaults already use the manager's `Replicas` config field for
   other buckets — apply that same value).
2. Add a `Start`-time validation warning: if `OperationTimeout >
   ElectionTimeout/3`, emit a `Warn`-level log.

**Design — locked.**

*Storage switch:* edit `manager_setup.go:89`'s `jetstream.KeyValueConfig` to
replace
```go
Storage: jetstream.MemoryStorage,
```
with
```go
Storage:  jetstream.FileStorage,
Replicas: m.cfg.Replicas, // or the package-level default if Replicas is unset
```
matching the storage block used by the other Parti buckets.

*Companion warning* (read-only; no behavior change):
```go
// In manager_setup.go, alongside warnOnShortAuditGrace:
func warnOnOperationTimeoutVsElection(cfg Config, logger types.Logger) {
    if cfg.OperationTimeout > cfg.ElectionTimeout/3 {
        logger.Warn("OperationTimeout exceeds ElectionTimeout/3; a single slow renew can consume the lease's three-attempt budget",
            "OperationTimeout", cfg.OperationTimeout,
            "ElectionTimeout", cfg.ElectionTimeout,
            "remedy", "set OperationTimeout <= ElectionTimeout/3 (both default to 10s — at the default pair a single slow renew can consume the lease window)")
    }
}
```
Call from `Manager.Start` alongside the existing warning helpers.

**F1 dependency check.** The PR description and the spec MUST state plainly:
*Do not merge this PR before P1.3 (F1) is on `main`*. CI cannot enforce it,
so the gate is human discipline + the README PR-sequencing table. The plan
reviewer should reject this PR if the F1 epoch-fence commit is not present
in the base.

**Operator migration runbook.** Because `EnsureKVBucket` is get-then-create,
a live `MemoryStorage` election bucket is *not* transparently upgraded by the
new code. Operators must explicitly delete the bucket. **F1's epoch fence is
exactly what detects the resulting recreate event**, so the runbook is safe
once F1 has shipped. Add to `docs/OPERATIONS.md`:

> #### Election bucket storage migration (after upgrading to the release that includes this fix)
>
> The election bucket's storage type changes from `MemoryStorage` to
> `FileStorage` with this release. Because Parti's `EnsureKVBucket` is
> get-then-create (`kvutil/bucket.go:50-64`), an existing `MemoryStorage`
> bucket is **not** upgraded automatically. Existing clusters must perform
> a one-time bucket replacement:
>
> 1. **Pre-flight: confirm F1 (epoch fence) is in the running build.**
>    F1 is the migration's safety net — it detects the bucket-recreate
>    event and routes affected workers through the standard degraded path.
>    Verify by either (a) confirming the deployed release tag is at or past
>    the release that landed F1 (release notes name the F1 commit
>    explicitly), or (b) grepping the manager's structured logs for
>    `bucket-recreated` or `degraded_reason="bucket-recreated"` in the
>    operational record — any prior occurrence confirms the fence is wired.
>    If F1 is not present, **abort** and upgrade further first.
> 2. During a planned maintenance window, with no live leadership churn
>    expected, remove the bucket from the live cluster. Use the bucket
>    removal command (not the key-delete command):
>    ```
>    nats kv rm parti-<cluster>-election
>    ```
>    Then confirm the underlying JetStream stream is also gone:
>    ```
>    nats stream ls | grep KV_parti-<cluster>-election   # expect no output
>    ```
>    (`nats kv rm` removes both the bucket and its backing stream; the
>    grep is a paranoia check before workers reconnect.)
> 3. Restart each Parti worker (rolling restart is acceptable). Each restart
>    re-runs `EnsureKVBucket`, which now creates the bucket with
>    `FileStorage` R≥3.
> 4. Workers that observed the deletion *before* their own restart enter
>    degraded mode via the F1 epoch fence (reason
>    `bucket-recreated:parti-<cluster>-election`); the existing
>    `OnDegraded → readiness probe → pod rotation` path completes the
>    migration automatically. The runbook does not need to handle this
>    case manually.
> 5. Verify post-migration: a NATS node restart no longer causes leadership
>    churn across the worker fleet. Specifically:
>    ```
>    nats kv info parti-<cluster>-election | grep "Storage:"   # expect "File"
>    ```

Add a SOP cross-reference in `docs/OPERATIONS.md` from the existing degraded
mode section.

**Reproducer test list.**
- *T1 (must fail on parent — IOPS regression guard).* Microbenchmark that
  confirms the `FileStorage` switch does not regress IOPS beyond the noise
  band recorded in M1.9. Run a 60 s capture window at N = 1000 partitions.
  Assert mean IOPS ≤ M1.2 baseline + 2 %. On the *parent* commit, this test
  is trivially green; the value of the test is forward — it pins the IOPS
  invariant for future work that might touch the election bucket. (This is
  the only forward-looking test in the plan; documented as such.)
- *T2 (must fail on parent — bucket-loss survival).* Integration test:
  start manager → confirm leadership → restart a single NATS node (out of
  R≥3) → assert leadership is **preserved** (no churn, no
  `OnLeadershipChanged(false)` then `(true)` ping-pong). On parent
  (`MemoryStorage`), leadership flips on the node-restart; on the fix it
  rides through. This is the actual correctness assertion.
- *T3 (companion warning).* Unit test: construct a `Config` with
  `OperationTimeout = 30s` and `ElectionTimeout = 30s`. Call `Start`. Assert
  the warning is emitted. Construct again with `OperationTimeout = 5s`
  and `ElectionTimeout = 30s`; assert no warning.
- *T4 (migration safety).* Integration: simulate the migration runbook
  (delete bucket while workers are running, assert `OnDegraded` fires with
  `reason == "bucket-recreated:parti-election"` per F1; restart workers;
  confirm new bucket is `FileStorage`). Validates the runbook end-to-end.

**Verification gates.**
- `make lint && make test && make test-race && make test-integration` green.
- M1.9-style IOPS smoke (T1) within ±2 % of baseline.
- Docs review: `docs/OPERATIONS.md` migration runbook section reads
  unambiguously; operator can execute without questions.

**How this trips readiness.** Indirectly: the change **eliminates** the
dominant readiness-trip cause (election bucket loss on routine NATS node
restart). After the switch, an R≥3 cluster survives single-node restarts
without leader churn. Readiness still trips for the genuine cluster-rebuild
case (F1 epoch fence + cached-bucket-handle failure), as designed.

**Dependencies & sequencing.** **Hard dependency on P1.3 (F1).** First of
Phase 2 because it is the lowest change-risk item in the phase and removes
the dominant operational pain.

**Out of scope.**
- F9-B (lease-aware leader) — deferred to Phase 3.
- Other bucket storage changes — **this PR changes only the election
  bucket**. The `heartbeat` bucket is also currently `MemoryStorage`
  (`manager_setup.go:93`) by design (workers re-publish every
  `HeartbeatInterval`, per the comment at `manager_setup.go:57-62`);
  changing it is **not** in scope here. The `assignment` bucket is
  already `FileStorage` (`manager_setup.go:97`). If the operational
  data later supports switching `heartbeat` too, that is a separate
  PR with its own IOPS justification.

---

### P2.2 (F6-B) — Calculator-layer "minimum credible partition input" guard

**Anchors.**
- `internal/assignment/calculator.go:1071-1102` — `getActiveWorkers` worker-set
  read; the symmetric path for workers
- `internal/assignment/calculator.go:1238-1383` — `rebalance` entry and the
  decision surface
- `internal/assignment/calculator.go:1280-1283` — existing snapshot-error
  abort (the **erroring** path is already correctly handled)
- `internal/assignment/calculator.go:1290-1296` — existing `len(workers) == 0`
  floor; the analogue for partitions does not exist
- `internal/assignment/emergency.go:89-133` — `EmergencyDetector`
  grace-window pattern; mirrors the across-polls confirmation we need for
  shrunk-but-non-empty observations
- `source/nats_kv.go:704` — `applyEmptyPreservingKnown` (the source-layer
  pattern; intentionally NOT extended — see Design)
- `types/partition_source.go:13-15` — `Snapshot` contract that distinguishes
  "never-written" from "written-then-deleted" (the reason source-layer
  suppression is wrong)

**Scope.** Behavioral contract: an **empty** or **sharply shrunk
(but non-empty)** partition-source observation MUST NOT propagate into a
fleet-wide reassignment. The erroring path is already correctly aborted
(`calculator.go:1280-1283`) — this PR closes the *suspicious-but-non-erroring*
gap.

**Design — locked. Calculator-layer.** Source-layer was considered (review
§F6-B "likely the smaller change") and rejected because:
1. `Snapshot()`'s documented contract explicitly distinguishes
   "never-written" (`known=false, revision=0`) from "written-then-deleted"
   (`known=true, revision=deleteRev, empty`). Suppressing empty/shrunk
   observations at the source-layer would mutate the snapshot to lie
   about what is in KV, breaking that contract for every other consumer
   of the source.
2. Third-party `WatchablePartitionSource` implementations would not be
   covered — the contract would be source-implementation-specific.
3. The calculator already owns the symmetric `len(workers) == 0` floor
   (`calculator.go:1290-1296`) and the cross-poll confirmation pattern
   (`EmergencyDetector`). Locating F6-B's mechanism alongside its
   symmetric F10-A counterpart keeps "minimum credible inputs" as a
   single calculator-side doctrine.

Cost surfaced honestly: **the calculator gains new inter-poll state**
(last-known partition count + shrunk-confirmation counter). It does not
have this state today (review §F6-B explicitly flags this). That cost is
the price of the contract preservation and third-party coverage above.

*Mechanism:*
1. Add to `Calculator`:
   ```go
   // lastKnownPartitionCount is the most recent non-suspicious
   // partition count the calculator has acted on. Suspicious observations
   // (empty, or sharply shrunk) do not advance it.
   lastKnownPartitionCount int
   // partitionShrunkObservations counts consecutive suspicious
   // PARTITION observations. Confirmed (=> trusted) once it reaches
   // PartitionShrinkConfirmationCount. Named distinctly from F10-A's
   // workerShrunkObservations to avoid the cross-feature collision
   // both fields would otherwise create on the Calculator struct.
   partitionShrunkObservations int
   ```
2. New config field on `Calculator` config:
   ```go
   // PartitionShrinkConfirmationCount is the number of consecutive
   // suspicious partition-source observations required before the
   // calculator trusts the shrink. Default 3.
   PartitionShrinkConfirmationCount int
   // PartitionShrinkConfirmationThresholdPct gates the "sharply shrunk"
   // definition. A new count below this percentage of
   // lastKnownPartitionCount is suspicious. Default 50 (i.e. >=50%
   // shrink in one poll is suspicious).
   PartitionShrinkConfirmationThresholdPct int
   ```
3. New guard inside `rebalance`, placed *immediately after* the existing
   `len(workers) == 0` floor at `calculator.go:1290-1296`:
   ```go
   if c.lastKnownPartitionCount > 0 {
       if len(partitions) == 0 {
           // Empty observation; never trust without confirmation.
           c.partitionShrunkObservations++
           if c.partitionShrunkObservations < c.cfg.PartitionShrinkConfirmationCount {
               c.logger.Warn("ignoring empty partition observation pending confirmation",
                   "lastKnown", c.lastKnownPartitionCount,
                   "observation", c.partitionShrunkObservations,
                   "needed", c.cfg.PartitionShrinkConfirmationCount)
               return errSuspiciousPartitionObservation // existing-style sentinel; calculator caller treats like errEmptyWorkers
           }
       } else if len(partitions)*100 < c.lastKnownPartitionCount*c.cfg.PartitionShrinkConfirmationThresholdPct {
           // Sharply shrunk but non-empty.
           c.partitionShrunkObservations++
           if c.partitionShrunkObservations < c.cfg.PartitionShrinkConfirmationCount {
               c.logger.Warn("ignoring sharply-shrunk partition observation pending confirmation",
                   "lastKnown", c.lastKnownPartitionCount,
                   "observation", len(partitions),
                   "thresholdPct", c.cfg.PartitionShrinkConfirmationThresholdPct,
                   "consecutive", c.partitionShrunkObservations)
               return errSuspiciousPartitionObservation
           }
       } else {
           c.partitionShrunkObservations = 0 // observation healed; reset
       }
   }
   c.lastKnownPartitionCount = len(partitions)
   ```
4. Surface the sentinel cleanly: `errSuspiciousPartitionObservation`
   should NOT escalate (it is a "skip this rebalance, keep cached
   assignment" — exactly mirrors the existing `errSuspiciousWorkerSet`
   that F10-A will introduce). It is a non-erroring "do nothing" path
   from the caller's POV.

**Reproducer test list.**
- *T1 (must fail on parent — empty observation).* Calculator unit test:
  prime with N=100 partitions, run one rebalance to set
  `lastKnownPartitionCount=100`. Inject a source whose next `Snapshot`
  returns `[]`. Call `rebalance` for the **first
  `PartitionShrinkConfirmationCount-1` polls in total** (i.e. inject
  empty, call rebalance; then if `PartitionShrinkConfirmationCount > 2`,
  inject empty and call rebalance `PartitionShrinkConfirmationCount-2`
  more times). Across all of those, assert no commit is published and
  `lastKnownPartitionCount` still equals 100. Do **not** call rebalance
  a `PartitionShrinkConfirmationCount`-th time — that one is T3's
  territory (the design proceeds on the Nth observation, per the
  pseudocode `if c.partitionShrunkObservations <
  PartitionShrinkConfirmationCount`). On parent, the test fails on the
  very first empty observation — a zero-partition assignment commit
  fires.
- *T2 (must fail on parent — sharply shrunk).* Same setup; inject N=10
  (90 % shrink, well below threshold). Assert suppression across
  `PartitionShrinkConfirmationCount-1` polls (same off-by-one
  arithmetic as T1).
- *T3 (confirmation honoured — boundary).* Same setup; inject the
  suspicious observation across `PartitionShrinkConfirmationCount`
  consecutive polls. Assert: polls 1..(N-1) suppress (no commit);
  poll N proceeds (commit fires). Confirms the boundary
  (legitimate shrink is delayed by the confirmation window — accepted
  trade per review §F6-B "design tension").
- *T4 (legitimate growth not gated).* Inject N=200 after N=100. Assert
  rebalance proceeds immediately (growth is never suspicious).
- *T5 (counter reset on heal).* Inject N=5 once (below threshold),
  then N=100. Assert `partitionShrunkObservations` resets to 0 on the
  healing observation.
- *T6 (suspicious-forever policy — locks the explicit stance).* Inject
  N=0 across `PartitionShrinkConfirmationCount + 5` consecutive polls
  (i.e. the source returns `[]` forever, non-erroring). Assert: (a)
  during the first `PartitionShrinkConfirmationCount-1` polls, no
  commit is published and the suspicious-observation counter advances;
  (b) at the confirmation-completing poll, the rebalance proceeds with
  N=0 — confirmed shrink is honored as legitimate operator intent;
  (c) the `Warn` log line and the
  `parti_partition_observation_suspicious_total` counter document the
  acceptance. This test is **not** a failure-mode assertion — it pins
  the explicit policy so a future change cannot silently re-introduce
  fleet-wide reassign-to-zero on a transient blip nor silently change
  the post-confirmation policy.

**Verification gates.**
- `make lint && make test && make test-race` green.
- Code-review attention to interplay with the existing
  `EmergencyDetector` worker-side path: confirm they do not duplicate or
  cancel each other (they operate on different inputs — workers vs
  partitions — so they should compose cleanly, but document that
  explicitly).
- Manual: simulate a partition-source flake (operator runbook for one
  test cluster) — confirm the warning logs fire and the cached
  assignment is preserved.

**How this trips readiness.** It doesn't, by design — and this is a
deliberate policy call surfaced explicitly. The contract is two-layer:

1. *During the confirmation window:* hold cached assignment; emit a
   `Warn` log each time the suppression fires. Readiness stays green.
   This is the load-bearing safety on a transient blip — F6-B's main
   value.
2. *After the confirmation count is met:* the calculator trusts the
   shrink and rebalances. This may be (a) legitimate operator intent
   (e.g. the operator removed partitions in the source bucket), or
   (b) a sustained source-side bug returning `[]` with no error. The
   library **cannot distinguish (a) from (b) without operator input**;
   F6-A (`OnSourceUnavailable`) does NOT fire here because the source
   returns no error.

The explicit policy: **F6-B treats post-confirmation shrink as
legitimate operator intent.** The safety F6-B provides is the
*confirmation window* itself — confirmed across multiple polls is
strong enough evidence to act on. For the residual case (sustained
non-erroring `[]` from a buggy source), the documented mitigation
is operator vigilance via the per-suppression `Warn` log lines and
the `parti_partition_observation_suspicious_total` counter (new
metric introduced by this PR). `docs/OPERATIONS.md` must be updated
in this PR's deliverable list to name the limit explicitly: "a
sustained non-erroring empty/shrunk source observation will, after
`PartitionShrinkConfirmationCount` consecutive polls, be honored as
legitimate; operators must monitor the suspicious-observation
counter for unexpected sustained shrinks."

This is consistent with the organizing invariant — the invariant
governs *unrecoverable* failures, and a non-erroring source
observation is, by the library's contract with `PartitionSource`,
recoverable (the next poll may heal). The doctrine is "minimum
credible inputs"; once the input is confirmed across N polls it is
credible.

If post-merge operational evidence shows this policy is wrong (e.g.
recurring incidents where a buggy source caused fleet-wide
reassign-to-zero after N confirmations), revisit by adding an
explicit suspicious-source hook (`OnSourcePartitionsSuspicious`)
that fires at the confirmation boundary. That extension is **not**
in scope here; it is a follow-up gated on evidence.

**Dependencies & sequencing.** **Soft dependency on P1.1 (F6-A)** — both
fixes the same problem from complementary angles, but each is independent
and would work standalone. Sequencing P2.2 after P2.1 in Phase 2 because
P2.1 is lower-risk; P2.2 then lands against a stable election baseline.

**Out of scope.**
- Touching the source layer (`source.NatsKV`). The design explicitly
  keeps the source contract intact.
- Generalising to other inputs beyond workers and partitions. The
  doctrine is "minimum credible inputs" for the two inputs we have;
  future inputs would extend the pattern when added.

---

### P2.3 (F5) — Stream-gone hook + checkpoint reset for recreated stream

**Anchors.**
- `internal/durable/partition_consumer.go:195-199, 372-439, 442-474,
  457-460, 508-512` — `recreateFn`, legacy escalation, the bailing
  point on stream-not-found
- `internal/recovery/controller.go:206-225, 237, 294-302` —
  `SeedCheckpoint` (monotonic; only seeds when `AckFloor.Stream > 0`);
  `recover` path; log-and-fail path
- `internal/recovery/checkpoint.go:30-40` — `Checkpoint.Seed` is
  monotonic — **cannot lower** a stale checkpoint from a deleted stream
- `internal/recovery/config.go:22-28` — `BuildConfig`'s
  `FromLastProcessed`-with-`checkpoint==0` fallback to
  `DeliverNewPolicy` (the reason a naive seed-to-zero would skip
  every fresh-stream message)
- `consumer/dynamic.go:309-316, 319-327` — `CheckWorkQueueRecoveryCompat`
  under `sync.Once` only in `Update`; `UpdateWorkerConsumer` bypasses it

**Scope.** Three things in one PR:
1. Add a public `OnStreamMissing(streamName string) error` hook so
   provisioning-owning callers can re-create the stream.
2. Re-arm `CheckWorkQueueRecoveryCompat` on **both** consumer-update
   paths (`Update` AND `UpdateWorkerConsumer`) — the manager-facing
   path currently bypasses the check.
3. **Reset (not seed) the checkpoint for a recreated stream** AND
   pick a deliver policy that does not silently skip the fresh
   stream's contents. The current monotonic `Checkpoint.Seed` cannot
   lower a stale checkpoint, and `BuildConfig`'s `checkpoint==0` path
   degrades to `DeliverNewPolicy`. Both must be addressed.

**Design — locked.** Streams remain category B (the library does **not**
auto-recreate). The hook is the escalation path; the consumer-side
machinery hardens against a stream that *has* been recreated.

*New public type:*
```go
// StreamMissingHook fires when a dynamic consumer cannot create a
// consumer because the underlying JetStream stream is absent. The hook
// is the escalation path — the library does not recreate streams.
// Returning a nil error indicates the caller has re-created the stream
// (e.g. via parti.Provision); the consumer loop will retry. A non-nil
// error or no hook configured surfaces the loss via OnError so the
// readiness probe can rotate the pod.
type StreamMissingHook func(streamName string) error
```

Wire into `WorkerConsumerConfig` and the manager (`Manager.Config`).
`partition_consumer.go:457-460` invokes the hook on `ErrStreamNotFound`
instead of bailing silently.

*Re-arm path:* introduce a small `resetCompatCheckOnce()` helper on
`consumer.Dynamic` and call it from both `Update` and
`UpdateWorkerConsumer` on stream-recreate (signalled by the hook
returning nil). The `sync.Once` is replaced with an atomic flag the
helper resets.

*Checkpoint reset for a recreated stream.* Three coordinated
additions in `internal/recovery/`, with **explicit control-flow
ordering** and a **stream-epoch generation fence** to defend against
late acks from the old stream.

**Ordering — pinned.** Current `Controller.recover`
(`internal/recovery/controller.go:237-316`) reads the checkpoint
(`:273`) and calls `BuildConfig` (`:274`) *before* `recreate` runs
(`:294`). The naive "hook fires inside `recreate`" placement would
let the *pre-reset* checkpoint be used to build the *first
post-recreate* consumer — exactly the T2 skip case. The spec
mandates this ordering for the stream-missing recovery path:

1. Recovery loop detects stream-missing (either the upstream
   error classifies to a new `recovery.errStreamMissing` sentinel
   OR `recreate` returns a wrapped `jetstream.ErrStreamNotFound`).
2. The loop **bails out of the current `recover` attempt without
   advancing** and invokes the `OnStreamMissing` hook.
3. If the hook returns nil (recreate succeeded), the loop calls
   `Controller.HandleStreamRecreated(ctx, infoFn)` (defined below)
   which performs the reset + epoch bump + re-seed.
4. The loop then **re-enters `recover` from the top.** The fresh
   `recover` call reads the now-reset checkpoint and builds the
   replacement consumer config with `DeliverAllPolicy` (or
   `AckFloor + 1` for the restored-from-backup case).

The spec adds a new `RecreateFunc` return contract: if `recreate`
returns `(nil, errStreamMissing)`, `recover` MUST NOT update the
checkpoint or burst state — it returns control to the caller (the
partition-consumer run loop) which performs steps 2–4 and re-invokes
`recover`. This makes the ordering explicit at the function-contract
level, not implicit in call-site discipline.

**Stream-epoch generation fence — non-negotiable.** Manual-ack mode
(`Controller.Dispatch` with `manualAck=true`,
`internal/recovery/controller.go:185-188`, and
`trackingMsg.Ack`/`DoubleAck`,
`internal/recovery/tracking_msg.go:19-37`) hands a `trackingMsg` to
the handler and returns without waiting for the ack. A late ack from
an OLD-stream handler — arriving AFTER `ResetForStreamRecreate` has
zeroed the checkpoint — would call `AdvanceCheckpoint` and re-raise
the checkpoint to an old-stream sequence, defeating the entire
reset. The fence:

```go
// In Controller, alongside the checkpoint:
streamEpoch atomic.Uint64   // bumped exactly once per HandleStreamRecreated
```

```go
// trackingMsg captures the epoch at wrap-time:
type trackingMsg struct {
    jetstream.Msg
    controller *Controller
    epoch      uint64  // captured from controller.streamEpoch at WrapForTracking
}

// AdvanceCheckpoint ignores acks from a prior epoch.
func (c *Controller) AdvanceCheckpoint(msg jetstream.Msg, epoch uint64) {
    if c == nil { return }
    if epoch != c.streamEpoch.Load() {
        // Late ack from a prior stream generation. Drop silently.
        return
    }
    c.checkpoint.Advance(msg)
}
```

The non-manual-ack path (`Dispatch` with `manualAck=false`,
`controller.go:190-194`) is implicitly fenced: the dispatch loop's
`Ack → AdvanceCheckpoint` chain runs synchronously on the same
goroutine that consumed the message, so it cannot outlive a
`HandleStreamRecreated` that ran on the recovery goroutine — but the
fence still applies (it captures the current epoch at dispatch
time and the comparison protects against any future async-dispatch
addition). Document the invariant: **`AdvanceCheckpoint` MUST be
called with the epoch captured at message dispatch time, never at
ack time.**

**The three coordinated additions.**

1. Non-monotonic reset on `Checkpoint`:
   ```go
   // ResetForStreamRecreate clears the checkpoint to zero. Unlike Seed,
   // this is NON-monotonic and is only legal after the caller has
   // confirmed (via StreamInfo) that the stream is a new identity
   // (different Created timestamp / different stream-state lineage).
   // The Controller's recreated-stream path is the only legitimate
   // caller — direct use elsewhere will silently drop progress.
   func (cp *Checkpoint) ResetForStreamRecreate() {
       cp.maxAckedStreamSeq.Store(0)
   }
   ```
2. `Controller.HandleStreamRecreated(ctx, infoFn)` — runs **after**
   the hook returns nil and **before** the recovery loop re-enters
   `recover`. Three steps in this exact order:
   - Bump `streamEpoch` (`c.streamEpoch.Add(1)`). Any in-flight
     `trackingMsg` from before this point is now from a prior
     epoch; late acks will no-op via the fence.
   - `cp.ResetForStreamRecreate()` — drop the stale checkpoint.
   - `SeedCheckpoint(ctx, infoFn)` against the new stream. If the
     new stream's `AckFloor.Stream > 0` (operator restored from a
     backup with non-zero ack floor), the seed picks it up; if it
     is 0 (fresh stream), the checkpoint stays at 0.
   - Set `recreatedSinceLastBuild atomic.Bool` so the next
     `BuildConfig` knows to choose `DeliverAllPolicy`.
3. New deliver-policy rule in `BuildConfig`: when
   `recreatedSinceLastBuild` is true AND strategy is `FromLastProcessed`
   AND checkpoint is 0, override to **`DeliverAllPolicy`** (replay
   everything in the fresh stream from sequence 1) instead of falling
   back to `DeliverNewPolicy`. The flag is one-shot; the next normal
   recovery rebuilds use the existing rules.
   ```go
   case FromLastProcessed:
       switch {
       case checkpoint > 0:
           cfg.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
           cfg.OptStartSeq = checkpoint + 1
       case recreatedSinceLastBuild:
           cfg.DeliverPolicy = jetstream.DeliverAllPolicy
           return cfg, "stream_recreated_replay_from_start"
       default:
           cfg.DeliverPolicy = jetstream.DeliverNewPolicy
           return cfg, "fallback_no_checkpoint"
       }
   ```

`BuildConfig`'s signature widens to accept the
`recreatedSinceLastBuild` flag (or the existing `Controller.recover`
reads-and-clears the flag before calling `BuildConfig` and passes the
choice explicitly). Implementation may pick whichever shape is least
invasive; the spec-level contract is "after stream recreate, the
first build uses DeliverAllPolicy when checkpoint is 0, not
DeliverNewPolicy."

**Reproducer test list.**
- *T1 (must fail on parent — hook fires).* Integration test under
  `test/integration/failure/`: start manager with a non-nil
  `OnStreamMissing` hook that records its invocation; delete the
  dynamic-consumer stream; assert the hook fires within
  one recovery-controller cycle, with the correct stream name. On
  parent: hook field absent; test fails at compile or, after
  introducing the field, at the firedWithin assertion.
- *T2 (must fail on parent — fresh-stream delivery).* Set up the
  recovery controller with strategy `FromLastProcessed` and an
  in-memory checkpoint advanced to value 100 (simulates progress
  in the original stream). Hook re-creates the stream with
  `AckFloor.Stream == 0` (fresh stream). Publish 5 messages **after
  the recreate but before the replacement consumer is bound** — this
  is the load-bearing detail. Assert **two** things: (a) the
  `jetstream.ConsumerConfig` passed to the first post-hook
  `CreateOrUpdateConsumer` call has `DeliverPolicy == DeliverAllPolicy`
  (capture via a spy `RecreateFunc`), and (b) all 5 messages are
  delivered on resume with stream sequences 1..5. On parent: the
  monotonic `Checkpoint.Seed` cannot lower from 100, `BuildConfig`
  falls through to `DeliverByStartSequencePolicy` with
  `OptStartSeq = 101` (or to `DeliverNewPolicy` if it had been 0),
  and the consumer skips the five messages.
- *T2b (restored-from-backup variant).* Same as T2 but the hook
  restores from a snapshot with `AckFloor.Stream == 50`. Assert: (a)
  the spy `RecreateFunc` captures `DeliverPolicy == DeliverByStartSequencePolicy`
  with `OptStartSeq == 51`, and (b) the consumer resumes at sequence
  51. Confirms the re-seed path picks up a non-zero ack floor when
  present (NOT `DeliverAllPolicy` in this case — the flag drops out
  because checkpoint > 0 after re-seed).
- *T2c (must fail on parent — late-ack epoch fence).* Set up with
  manual-ack mode AND `FromLastProcessed`. Prime the checkpoint to
  value 100. Receive a tracking message at stream sequence 80; do
  NOT ack it yet. Trigger `HandleStreamRecreated` (which bumps the
  epoch, resets the checkpoint to 0, and seeds from a fresh stream).
  THEN call `Ack` on the held tracking message. Assert: the
  checkpoint stays at 0 — the late ack from the prior epoch is
  silently dropped. On parent: the checkpoint advances to 80, which
  causes the subsequent recreated-stream consumer to skip messages
  1..80 of the new stream. **Forward-pin: this test exercises the
  generation fence; without it, T2 would still pass but a real
  manual-ack workload would silently lose messages.**
- *T3 (compat re-arm).* Re-create the stream with a *different*
  retention policy (e.g. `LimitsPolicy` → `WorkQueuePolicy`); assert
  the compat check fires on the next `UpdateWorkerConsumer` call — on
  parent, the check is `sync.Once`-gated and never re-runs.
- *T4 (no hook = OnError + readiness flip).* Same setup with hook
  unset. Delete the stream. Assert `OnError` fires with a stream-gone
  error class; assert the existing degraded-mode wiring takes the
  worker out of `Ready`. *Note:* this assertion depends on the
  envelope being wired into `partition_consumer.go` — landed in P2.4d
  before this PR (see Dependencies & sequencing below).
- *T5 (hook returns error = no retry storm).* Hook returns a non-nil
  error. Assert the consumer enters the permanent-failure state (per
  the F2 envelope this PR depends on) and does NOT generate further
  retry traffic. *Note:* same envelope dependency as T4.
- *T6 (one-shot recreated-flag clears).* Trigger the recreate path
  once; let the consumer process N messages cleanly; trigger an
  unrelated transient consumer-delete recovery (no stream recreate).
  Assert that second recovery uses the normal `FromLastProcessed`
  path with the advanced checkpoint, *not* `DeliverAllPolicy`. Proves
  the `recreatedSinceLastBuild` flag is single-shot.

**Verification gates.**
- `make lint && make test && make test-race && make test-integration` green.
- Docs: `docs/CONSUMERS.md` documents `StreamMissingHook` with a
  worked `provision`-based recreate example.
- New exported symbol audit: only `StreamMissingHook` and the wiring
  fields added.

**How this trips readiness.** Two paths:
1. Hook present, re-creates successfully → no readiness trip; consumer
   resumes from `AckFloor` of the fresh stream.
2. Hook absent or returns error → after F2's envelope-bounded retries
   the consumer enters permanent failure → `OnError` fires → existing
   degraded-mode wiring trips readiness → pod rotation. **Note:** a
   pod restart alone does NOT restore the stream (`Manager.Start`
   ensures only KV buckets, not message streams; review §F5 names this
   precisely). The restart only stops the silently-wedged worker and
   alerts orchestration; restoring the stream is the operator's
   responsibility.

**Dependencies & sequencing.** **Depends on P2.4d** — the F2 envelope
applied to the `partition_consumer.go` dynamic-consumer recovery loop
is what turns "no hook = `OnError` + readiness flip" and "hook returns
error = no retry storm" into observable behavior. Today the loop
backs-off-and-retries forever (`partition_consumer.go:195-227`,
`recovery/controller.go:294-302`), so P2.3's T4/T5 tests would fail
on parent + P2.4a alone (which wires only the source watcher).
Sequenced in Phase 2 as: P2.1 → P2.2 → P2.4a → P2.4b → P2.4c →
**P2.4d → P2.3** → P2.5. P2.4d MUST land before P2.3.

**Out of scope.**
- Library auto-recreating the stream (category B forbidden).
- The retry envelope itself (F2's territory).
- The recovery-controller consolidation (separate, future effort).

---

### P2.4 (F2) — Bounded-retry envelope across four loops

**Anchors.**
- `internal/durable/partition_consumer.go:195-199, 508-512` — dynamic
  consumer recovery
- `source/nats_kv.go:772-805` — `restartWatcher`
- `internal/durable/claim_resolver.go:558-571` — handoff watcher
- `manager_assignment.go:339` — `monitorAssignmentChanges`
- `test/integration/failure/claim_resolver_nats_restart_test.go` — the
  test pattern to mirror for restart-driven reproducers

**Scope.** Apply the F2 envelope to four retry loops, one PR per loop.
Each loop today retries forever on a genuinely-gone resource with no
attempt cap, no permanent-failure state, no escalation. The envelope
adds: exponential backoff with a hard ceiling; a bounded attempt
budget per failure episode (reset on success); on exhaustion, an
explicit permanent-failure state; on entering that state, fire
`OnError` and a metric; stop generating API load until connectivity
is re-confirmed.

**Design — locked.** **Emerging-helper convention.** The envelope's
shape only crystallises once one site is wired. PR P2.4a introduces
the envelope **alongside** its first call-site wiring at
`restartWatcher` (smallest, best-isolated site; reproducer pattern is
"delete the bucket / restart the server" which the
`claim_resolver_nats_restart_test.go` pattern already supports). PRs
P2.4b–d then reuse.

Envelope sketch (the engineer crystallises in PR P2.4a):
```go
// retryEnvelope is a bounded-retry loop with permanent-failure
// state and one-shot escalation. Reset on a successful operation
// returning from the work func; exhausted budget transitions to
// permanent failure and stops calling work until ResetOnConnect is
// signalled.
type retryEnvelope struct {
    work             func(ctx context.Context) error
    classify         func(err error) retryClass  // transient / give-up / fatal
    onPermanent      func(err error)             // fires once at exhaustion
    onProgress       func(attempt int, err error)
    baseBackoff      time.Duration
    maxBackoff       time.Duration
    maxAttempts      int                          // budget per episode
    jitter           float64
}

// classify maps an error to:
//   transient: schedule retry, bump attempts
//   giveUp:    transition to permanent-failure immediately
//   fatal:     return error to caller without retry
type retryClass int
const (
    retryTransient retryClass = iota
    retryGiveUp
    retryFatal
)
```

Live location: `internal/retry/envelope.go` (new package), exported only
within the module — not part of the public API surface.

**Configuration:** each call site exposes its own `MaxAttempts` /
`BaseBackoff` / `MaxBackoff` knobs through the existing config layer
(`Config` for manager-owned loops, `WorkerConsumerConfig` for the
consumer loop, `NatsKVOption` for the source watcher). Sane defaults
mirror the current per-site values where present.

**PR-by-PR scope.**

#### P2.4a (F2) — Envelope + `source/nats_kv.go:restartWatcher`

Smallest, best-isolated site. Introduces `internal/retry/envelope.go`
alongside the wiring. After exhaustion, `restartWatcher` fires
`OnSourceUnavailable` (P1.1's hook) and stops retrying until the
source reconciler successfully re-reads via `kv.Get` (the "connectivity
re-confirmed" signal).

*Reproducer (must fail on parent).*
- *T1.* Delete the source bucket; assert `restartWatcher` retries
  bounded times (`MaxAttempts`); after exhaustion, assert
  `OnSourceUnavailable` fires AND `restartWatcher` stops generating
  watcher-create traffic. On parent: retries continue indefinitely.
- *T2.* Bucket re-created mid-retry; assert the envelope resets
  the attempt counter on the next success and resumes normal
  operation.

#### P2.4b (F2) — Apply envelope to `claim_resolver.go` handoff watcher

Smallest reuse; same shape as P2.4a. On exhaustion, fires `OnError`
via the resolver's existing error surface (resolver does not have a
dedicated public hook; review §F2 names this).

*Reproducer.*
- *T1.* Delete the handoff bucket; assert bounded retries; assert
  `OnError` fires once at exhaustion.

#### P2.4c (F2) — Apply envelope to `monitorAssignmentChanges`

The comment at `manager_assignment.go:339` already names "wiped
assignment bucket" as the expected case. On exhaustion, this loop's
escalation feeds `enterDegraded` (the assignment bucket is a hard
correctness dependency).

*Reproducer.*
- *T1.* Delete the assignment bucket while the manager runs; assert
  bounded retries; assert degraded mode is entered after exhaustion
  (not by `recordKVError` — which today handles only
  connectivity/NotFound — but by the envelope's permanent-failure
  path explicitly calling `enterDegraded("assignment-watcher-exhausted")`).

#### P2.4d (F2) — Apply envelope to `partition_consumer.go` recovery

Final loop and the larger surface. P2.4d lands **before** P2.3 (F5)
in Phase 2's sequence, so this PR wires the envelope and the
permanent-failure → `OnError` path without depending on the
`OnStreamMissing` hook (which P2.3 layers on top later). On
exhaustion at this stage:
- Permanent failure fires `OnError` with the recovery-exhausted
  reason; the existing degraded-mode wiring trips readiness.
- The `OnStreamMissing` hook is **not** wired in P2.4d. P2.3 wires
  it as an additional "yield once to the hook before transitioning
  to permanent failure" step; that interaction is verified by
  P2.3's T4/T5 reproducers, not here.

*Reproducer.*
- *T1.* Delete the dynamic-consumer stream; assert bounded retries
  (per the envelope's `MaxAttempts`); assert permanent failure fires
  `OnError` exactly once at exhaustion; assert no further consumer-
  create traffic after the permanent-failure transition.

**Verification gates (every F2 PR).**
- `make lint && make test && make test-race && make test-integration`
  green.
- Manual review of the configured `MaxAttempts` per site — too aggressive
  turns a recoverable blip into a premature permanent failure; too lax
  defeats the bound. Defaults must be justified in the spec.
- The `internal/retry/envelope.go` package introduced in P2.4a stays
  internal — no public-API surface added.

**How this trips readiness.** Each loop's exhaustion path eventually
trips readiness (P2.4a via `OnSourceUnavailable`; P2.4b/c via degraded
mode; P2.4d via `OnError` after the hook fails). Without F2 the loop
generates infinite retry pressure and **never** trips readiness.

**Dependencies & sequencing.**
- P2.4a is the envelope introduction PR.
- P2.4b/c/d reuse the envelope; each is independent of the others
  *except* that P2.3 (F5) depends specifically on P2.4d (envelope
  wired into `partition_consumer.go`).
- Sequencing within Phase 2: P2.1 → P2.2 → P2.4a → P2.4b → P2.4c →
  P2.4d → P2.3 → P2.5. P2.4d **must land before** P2.3 because P2.3's
  readiness behavior is observable only after the dynamic-consumer
  recovery loop is bounded.

**Out of scope.**
- The recovery-controller consolidation (separate, future effort).
- Public-API extensions beyond the per-site config knobs.

---

### P2.5 (F10-A) — Truncated-`Keys()` defense + worker-set floor

**Anchors.**
- `internal/assignment/worker_monitor.go:162-194, 229-231` — `GetActiveWorkers`
  → `heartbeatKV.Keys()` path
- `internal/assignment/calculator.go:1071-1102` — `getActiveWorkers`;
  connectivity-error path correctly handles cache fallback
- `internal/assignment/calculator.go:1290-1296` — the existing
  `len(workers) == 0` floor; the analogue we extend
- `internal/assignment/calculator.go:1271-1277` — the existing log-only
  "drop from N to 1" path; needs hardening
- `internal/assignment/emergency.go:89-133` — `EmergencyDetector` grace
  window — must compose, not duplicate
- nats.go `jetstream/kv.go:1335-1393` (v1.50.0) — empirical source of
  the truncation hypothesis

**Scope.** Two-part defense:
1. **Truncated-`Keys()` defense.** Make `GetActiveWorkers` defend
   against a silently truncated `Keys()` result: cross-check the
   result count against the last-known worker count; treat a large
   unexplained single-poll shrink as non-fresh (degraded) OR require
   two consecutive shrunk reads before trusting it.
2. **"Minimum credible worker set" floor.** Refuse to reassign on a
   suspiciously large worker-set shrink without emergency confirmation
   across the full grace window.

**Hard gate.** **The chaos reproducer is the first step. No fix is
written until the bug is empirically observed**, per review §6 F10-A
verification status and user constraint. The reproducer goes in
`test/integration/failure/heartbeat_truncated_keys_test.go` (new
file) and exercises an ordered-consumer drop mid-`Keys()` scan via
NATS chaos (terminate the ordered consumer while a scan is in flight,
or use the nats.go test hooks if available).

*Gate semantics (precise).*
- **T0 (the gate itself)** is a *diagnostic reproducer* that
  demonstrates `KeyValue.Keys()` can return a truncated slice with
  `err == nil` under ordered-consumer drop. T0 **passes on the parent
  commit** — its passing IS the empirical observation that justifies
  the fix. The PR is gated by T0 existing and being green on the
  parent commit before any defense is written. T0 may remain in the
  tree as a regression check (the truncation is a nats.go behavior,
  not a Parti bug, so the diagnostic is a forward observation pin).
- **T1/T2 below** are the *fixed-behavior* tests: they fail on the
  parent + T0 combination (because the defense is not in place) and
  pass after the fix lands.

**Design — locked.** Reproducer-gated. Below is the *proposed* design
to ship once T0 reproduces; the engineer revises if the empirical
observation contradicts the source-analysis prediction.

*Truncated-`Keys()` defense — placed in `Calculator.getActiveWorkers`,
NOT `WorkerMonitor.GetActiveWorkers`.* `WorkerMonitor.GetActiveWorkers`
returns `([]string, error)` only (`internal/assignment/worker_monitor.go:162`);
the `fresh` flag lives one layer up at `calculator.go:1071-1102` where
connectivity errors already become cached-with-`fresh=false`. The new
defense reuses that same shape:
```go
// In Calculator.getActiveWorkers, after monitor.GetActiveWorkers returns:
workers, err := c.monitor.GetActiveWorkers(ctx)
if err != nil {
    // ... existing connectivity-error path (cache fallback, fresh=false) ...
}
// New: cross-check against last-known count.
if c.lastKnownWorkerCount > 0 {
    thresholdCount := c.lastKnownWorkerCount * c.cfg.WorkerShrinkConfirmationThresholdPct / 100
    if len(workers) < thresholdCount {
        c.workerShrunkObservations++
        if c.workerShrunkObservations < c.cfg.WorkerShrinkConfirmationCount {
            c.Logger.Warn("ignoring suspiciously-shrunk worker observation pending confirmation",
                "lastKnown", c.lastKnownWorkerCount,
                "observation", len(workers),
                "thresholdPct", c.cfg.WorkerShrinkConfirmationThresholdPct,
                "consecutive", c.workerShrunkObservations)
            // Same shape as the connectivity-error path: cached + fresh=false.
            // Critically: do NOT update c.lastKnownWorkerCount on a
            // suspicious observation — keep the cached baseline.
            if cached, age, ok := c.getCachedWorkers(); ok {
                c.Metrics.RecordCacheUsage("workers", age.Seconds())
                c.Metrics.IncrementCacheFallback("suspicious_shrink")
                return cached, false, nil
            }
            // No cache: degrade explicitly so the caller does not act on
            // a suspicious observation.
            return nil, false, fmt.Errorf("%w: %w", types.ErrDegraded, errSuspiciousWorkerSet)
        }
    } else {
        c.workerShrunkObservations = 0
    }
}
c.lastKnownWorkerCount = len(workers)
c.updateCachedWorkers(workers)
return workers, true, nil
```

*Worker-set floor in `rebalance` — compose with the existing emergency
buffer.* The existing emergency lifecycle assigns confirmed-dead
workers into `c.disappearedWorkers` (`calculator.go:52, 1008`) and
consumes that buffer only on the emergency-rebalance branch
(`calculator.go:1193-1219`). Use that buffer as the "confirmed deaths"
signal — there is no need to add a new `HasConfirmedDeaths()` method:
```go
// Inside rebalance, immediately after the existing
// len(workers) == 0 guard at calculator.go:1290-1296:
if c.lastKnownWorkerCount > 0 {
    thresholdCount := c.lastKnownWorkerCount * c.cfg.WorkerShrinkConfirmationThresholdPct / 100
    if len(workers) < thresholdCount && len(c.disappearedWorkers) == 0 {
        // Suspicious aggregate shrink with no emergency-confirmed deaths.
        // The next poll will re-check once EmergencyDetector has had
        // the grace window to confirm.
        c.Logger.Warn("ignoring rebalance on suspiciously-shrunk worker set; no emergency-confirmed deaths",
            "currentCount", len(workers),
            "lastKnownCount", c.lastKnownWorkerCount)
        return errSuspiciousWorkerSet
    }
}
```
Note: `c.disappearedWorkers` is the existing buffer; reading
`len(...) == 0` here does NOT mutate it. The emergency-rebalance
branch is unaffected — if EmergencyDetector has confirmed deaths,
`c.disappearedWorkers` is non-empty and the floor releases.

**Interaction with EmergencyDetector.** Crucial: the floor
**composes** with the existing grace window, not duplicates or fights
it. `EmergencyDetector` confirms *individual* worker deaths across the
grace window (`internal/assignment/emergency.go:89-132`); the calculator
captures confirmed deaths into `c.disappearedWorkers` (calculator.go:1000-1015).
The F10-A floor refuses to act on a suspicious *aggregate* until
`c.disappearedWorkers` is non-empty. The floor surfaces a sentinel
that does not escalate; the next poll re-checks against the
now-updated emergency state.

*Config additions on `CalculatorConfig`:*
- `WorkerShrinkConfirmationCount` (default 2)
- `WorkerShrinkConfirmationThresholdPct` (default 50)

*Calculator state additions:*
- `lastKnownWorkerCount int`
- `workerShrunkObservations int` (named distinctly from P2.2's
  `partitionShrunkObservations` to avoid the cross-feature collision
  both fields would create on the same `Calculator` struct).

Neither field is exposed publicly; both are guarded by the existing
`pollMu` (so the floor reads them under the same lock the emergency
buffer already uses).

**Reproducer test list (T0 chaos test FIRST is the gate).**
- *T0 (diagnostic reproducer; the gate).* New integration test that
  drops the heartbeat-bucket ordered consumer mid-`Keys()` scan.
  Assert the truncated read is empirically observable — `Keys()`
  returns `(partial-slice, nil)`. **T0 must PASS on the parent
  commit** (its passing is the empirical observation that justifies
  the fix). No defense is written until T0 reproduces. T0 stays in
  the tree as a forward-observation pin against the nats.go behavior
  (`jetstream/kv.go:1335-1393`).
- *T1 (must fail on parent + T0; pass after fix).* Calculator unit
  test: prime `c.lastKnownWorkerCount = 10`; inject a `WorkerMonitor`
  stub that returns a 3-worker slice with `err == nil` (simulating
  the truncated read T0 demonstrates). Call `Calculator.getActiveWorkers`.
  Assert: returns the cached 10-worker set with `fresh=false` AND
  `c.lastKnownWorkerCount` still equals 10 (suspicious observation
  did not advance the baseline). On parent: returns 3 workers with
  `fresh=true` — buggy behavior.
- *T2 (must fail on parent + T0; pass after fix — the floor).* Same
  prime; inject a monitor that returns a 3-worker slice (70 % shrink
  past the 50 % threshold) AND `c.disappearedWorkers` is empty
  (EmergencyDetector has not confirmed deaths). Call `rebalance`.
  Assert: returns `errSuspiciousWorkerSet`; no commit published. On
  parent: commit fires; partitions reassigned to the 3-worker set.
- *T3 (confirmation honoured — released by emergency).* Same as T2
  but set `c.disappearedWorkers = []string{"w1","w2","w3","w4","w5","w6","w7"}`
  (EmergencyDetector confirmed seven deaths). Call `rebalance`.
  Assert: the rebalance proceeds normally and a commit fires. Confirms
  the floor releases when the emergency lifecycle has run.
- *T4 (composition with degraded mode — the double-ownership case).*
  Combine T1's setup with a separate worker in degraded mode (cached
  assignment, still processing). Assert: across `WorkerShrinkConfirmationCount`
  polls of truncated reads, no commit reassigns the degraded worker's
  partitions to another worker — proves the realistic
  double-ownership path is closed (review §F10-A "severity is
  conditional").
- *T5 (counter reset).* Inject one shrunk read followed by a healed
  read (returns the full 10 workers); assert
  `c.workerShrunkObservations` resets to 0 and `c.lastKnownWorkerCount`
  advances to the healed count.
- *T6 (confirmation budget allows real mass-death).* Prime with
  10 workers; inject the same 3-worker observation across
  `WorkerShrinkConfirmationCount + 2` polls AND simultaneously run
  EmergencyDetector with confirmations of the missing 7 (so
  `c.disappearedWorkers` populates after the grace window). Assert
  the rebalance proceeds on the confirmation-completing poll and
  partitions are correctly redistributed. Proves the floor does
  not suppress a legitimate mass-death scenario.
- *T7 (cross-feature counter isolation — landed once both P2.2 and
  P2.5 are present).* Calculator unit test exercising both counters
  in the same `Calculator` instance: inject a partition-suspicious
  observation (P2.2 path); assert `partitionShrunkObservations++` but
  `workerShrunkObservations == 0`. Then inject a worker-suspicious
  observation (P2.5 path); assert `workerShrunkObservations++` but
  `partitionShrunkObservations` is unchanged. Proves the two
  cross-feature counters do not leak into each other (the bug the
  v2 plan-review flagged would have manifested as a shared single
  counter corrupting both confirmation windows).

**Verification gates.**
- `make lint && make test && make test-race && make test-integration`
  green.
- Confirm **T0 passes on the parent commit** before any defense is
  written. This is the empirical-observation gate.
- Confirm T1, T2 **fail on parent + T0** and pass after the fix lands.
- Confirm T6 — the floor MUST NOT suppress a *legitimate* mass-death
  rebalance once `c.disappearedWorkers` is populated by the emergency
  lifecycle.
- Audit: every call site that reads `Calculator.getActiveWorkers`
  continues to handle `fresh=false` correctly (review §F10-A says the
  connectivity-error path "is correctly handled (cache fallback,
  fresh=false)" — the new path reuses that exact shape, so all
  existing callers MUST already tolerate the case). Pre-implementation
  grep verifies the claim.

**How this trips readiness.** Indirectly: the *kept* leader on a
truncated read does not act, so no wrong reassignment happens. If
the underlying heartbeat bucket is genuinely broken (vs. a transient
truncated read), the F1 epoch fence + F2 envelope wiring eventually
trip readiness. F10-A is the **prevention of double ownership during
the window** — a correctness fix, not a recovery fix.

**Dependencies & sequencing.** Independent of F1/F9-A but composes
with EmergencyDetector. Sequenced **last in Phase 2** because:
- The chaos reproducer is the hardest test to build; deferring it
  to the end of the phase amortises the risk against the rest of
  the phase already being merge-clean.
- The floor's tuning interacts with the grace-window discipline
  established by P2.2 (F6-B) — handling P2.2 first makes the
  "minimum credible inputs" doctrine already-live, which clarifies
  the F10-A design.

**Out of scope.**
- F10-B (already in P0.3).
- F10-C (partition-fencing — separate plan
  [`docs/plans/partition-fencing/`](../partition-fencing/)).
- F10-D (bounded overlap during normal moves — by design, not a
  gap).

---

## Phase 3 — Deferred and optional

These items do **not** ship with the rest. Their entries below are
**deferred sketches, not implementation-ready specs.** The value of
including them in this plan is naming the **specific operational
signal** that would re-promote them. If either item is re-promoted,
its sketch must be expanded into the full five-field per-PR spec
(scope / design / reproducer list / verification gates / dependencies)
before any code is written, following the same shape as the Phase 0–2
entries.

### P3.1 (F9-B) — Lease-aware leader (DEFERRED)

**Scope sketch.** Hold leadership through transient bucket
unavailability; step down at the lease deadline. Implements the
"do not flip `m.isLeader` on context-timeout renew errors" lenience
in `monitorLeadership`, tracking the *initiation timestamp of the
last successful renew* and force-self-demoting at
`now − lastSuccessfulRenew >= ElectionTimeout − margin`. Follower
side unchanged. Lease invariant:
`leader_local_stepdown_deadline + clock_skew ≤
follower_earliest_acquire` (= bucket TTL since last write).

**Re-promotion signal.** Ship F9-B only if **after F9-A is in
production**, telemetry shows residual leadership churn caused by
*transient bucket unavailability* (not bucket loss — that's already
closed by F9-A). Concretely: collect `OnLeadershipChanged(false →
true)` flip pairs per worker per hour; threshold ≥ 1 per hour
sustained across a non-trivial fraction of workers, with the
underlying NATS election bucket showing context-timeout renew errors
in the same window. If churn stays under that, the HIGH change risk
of touching election code is **not justified**.

**Hard prerequisite.** F9-A must be in production for at least one
release cycle's worth of observation. F1 must also be present.

**Audit prerequisite.** `CheckLeadership` is currently the only
non-test `LeaderCheck` call site (`manager_assignment.go:123`).
Before relying on "hold the leader longer," audit that no other
leader-exclusive write exists unfenced. The audit is part of F9-B
when it activates.

**Coupling with F10-A / F6-B.** A kept leader must not act on bad
inputs. F10-A's worker-set floor and F6-B's partition-input floor
are the load-bearing safety on the kept-leader window. Both must
have been in production stably before F9-B activates. (This is
already true in Phase 2's sequencing.)

---

### P3.2 (F4) — In-process re-provision of coordination buckets (OPTIONAL)

**Scope sketch.** Gated, opt-in, in-process re-create for the
regenerable coordination buckets (`parti-assignment`,
`parti-heartbeat`, `parti-handoff`), off by default, only after the
bucket is confirmed absent on a healthy connection for a sustained
window (`RecreateGracePeriod`). Excludes the source bucket
(category A) and message streams (category B). `parti-election` is
**not** an F4 target because F9-A subsumes it. `parti-stableid` is
**not** an in-process target — F3 handles it via restart.

**Re-promotion signal.** Ship F4 only if pod-rotation cost on
cluster-rebuild scenarios proves operationally significant after
Phase 2 has been stable. Concretely: measure the average wall-clock
gap between an `OnDegraded` event and the worker resuming `Ready`
post-rotation; threshold ≥ 60 s sustained across multiple incidents
for the case where the rebuild is "buckets gone but cluster
otherwise healthy." Below that threshold, the HIGH change risk is
not justified.

**Hard prerequisite.** F1 + F2 must be present (review §F4 names
both). F4 reuses F1's bucket-identity detection (the bucket-recreate
event is the *opposite* of the bucket-absent event, but the
mechanism is the same), and the F2 envelope provides the bounded
confirmation-window machinery.

---

## Appendix — Cross-references

- Authoritative source: [`findings.md`](./findings.md)
- Review trail (consolidated, 9 rounds): [`review-trail.md`](./review-trail.md)
- IOPS justification for F9-A:
  [`docs/plans/iops-investigation/findings.md`](../iops-investigation/findings.md) §2 cell M1.9
- Plan-shape mirror: [`docs/plans/worker-state-hardening/README.md`](../worker-state-hardening/README.md)
  (lazy per-PR specs)
- Reproducer-first discipline: `.agents/rules/300-testing.md`,
  memory `feedback_verify_first_with_reproducer`
- Post-impl-review workflow: memory `feedback_post_impl_review_workflow`,
  preferred dispatch via `/codex:review`
  (memory `feedback_codex_review_preferred`)
- Commit-message hygiene: `.agents/rules/550-git-conventions.md`,
  memory `feedback_no_plan_jargon_in_commits`
- Partition-fencing roadmap (F10-C scope cross-reference):
  [`docs/plans/partition-fencing/README.md`](../partition-fencing/README.md)
