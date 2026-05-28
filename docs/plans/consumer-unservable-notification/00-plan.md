# Unservable-Consumer Notification — Implementation Plan

**Goal:** Parti is a critical-mission orchestrator. It must auto-heal from
*temporal* NATS failures without human intervention, and — for failures it
genuinely cannot fix (the NATS cluster needs operator recovery) — it must
**notify the application** so the app can raise an alert. This plan closes the
one signalling gap that black-box reproducers proved exists: a partition
consumer that is **unservable but still exists** (raft quorum lost) produces no
app-facing signal today.

**Status:** IMPLEMENTED and verified (2026-05-29). Reproducers codex-review-clean;
plan-review rounds 1–2 clean; feature landed across `internal/recovery`,
`internal/durable`, `consumer`; black-box notify + false-positive-silence tests
green; AGENTS.md contracts + unit gate green.

### Implementation refinements (code reality vs the plan text above)
1. **`ConfirmDegrading` → `ActionBackoff` (NOT `ActionStreamMissing`).** §3.4
   proposed routing degrading confirms to `ActionStreamMissing`. The implemented
   choice is the more conservative `ActionBackoff` (unchanged from prior behavior)
   so the existing stream-missing route — which surfaces via the recover/iterator
   path, not the confirm path — keeps its current timing/semantics. The contract
   goal (degrading is never counted as unservable) is fully met either way; this
   avoids a behavior change to the stream-missing tests.
2. **Full-outage is NOT asserted silent.** §5 item 8 assumed the unservable signal
   stays silent during a full 3-of-5 outage. Empirically the consumer IS unservable
   then (Info returns 503 while the client is reconnected to a survivor), so it may
   legitimately fire alongside the manager's OnDegraded — both signals are true.
   Asserting silence there would pin incorrect behavior, so that guard was dropped;
   the manager degrade/heal contract is covered by `cluster_full_outage_recovery_test.go`.
3. **Detection is opt-in** via `WithOnConsumerUnservable`; unset preserves prior
   behavior exactly (zero new logs/signals), keeping the change backward-compatible.
4. **"Rebalance does not heal an unservable consumer"** is a logical consequence of
   the deterministic per-partition durable name: any worker that takes over the
   partition binds the SAME (stuck) consumer, so reassignment cannot fix
   quorum-loss — only the operator restoring NATS can. A dedicated multi-worker
   integration test for this was prototyped but removed: forcing a rebalance *while
   2 of 5 nodes are down* depends on leader re-election + handoff under reduced
   quorum, which is ~50/50 timing-flaky under full-suite `-race` load and is
   orthogonal to this feature. Rebalance behavior itself is covered by the
   manager/leader-election integration suites.

---

## 1. Motivation — the gap, proven black-box

Reproducers under `test/integration/failure/` assert only app-observable
outcomes (handler deliveries, app hooks, connection status), independent of
parti internals. They establish the following behavior matrix.

| Failure | Can parti fix it alone? | Behavior | Reproducer |
|---------|------------------------|----------|------------|
| Consumer **really gone** (ConsumerNotFound / ErrConsumerDeleted), cluster has capacity | YES — recreate | Iterator detects deletion → recreates → delivery resumes | `cluster_partial_crash_test.go` (healthy-consumer delete→recreate) |
| Consumer **quorum-lost but exists**, cluster later recovers | YES — wait & resume | Transient backoff retries; resumes on the **same** consumer when quorum returns; no loss, no gross duplication (observed maxDup=1) | `cluster_quorum_restored_test.go` |
| Single-node / PVC (volume) loss, quorum holds | YES — transparent | Wiped node re-replicates from peers; worker never degrades; consumption continues | `cluster_pvc_loss_test.go` |
| Whole cluster / meta quorum lost (3-of-5) | NO — operator | Manager enters Degraded + OnDegraded fires; auto-heals to Stable on restart | `cluster_full_outage_recovery_test.go` |
| Consumer **quorum-lost but exists**, cluster does **not** recover | NO — operator | **Delivery silently stalls; NO app signal** (OnPermanentFailure stays 0; manager stays Stable because its RF5 KV survives 2-of-5; connection stays UP) ← **THE GAP** | `cluster_unservable_signal_gap_test.go` |
| Unservable partition reassigned to another worker | NO — operator | Rebalance does **not** heal it (the new owner binds the same stuck consumer) | `cluster_multiworker_unservable_test.go` |

So "try its best to auto-heal" is **already satisfied** for every recoverable
shape. The only missing piece is the bottom rows: when a consumer is unservable
in a way parti cannot fix, the app must be told.

## 2. Established empirical facts (do not re-investigate)

These were measured with embedded multi-node NATS clusters (see the reproducers
and the spike history). They constrain the design:

1. **A quorum-lost memory-RF3 consumer surfaces `503` / `NoResponders` — never
   `ConsumerNotFound`.** So parti's existing recovery (which recreates only on
   `ConsumerNotFound` / `ErrConsumerDeleted`) does not fire, and the iterator
   backs off transiently forever with no escalation.
2. **Parti cannot reliably recreate a quorum-lost consumer.** `DeleteConsumer`
   needs the consumer's own raft quorum, which is gone; it usually times out
   (non-deterministic — occasionally succeeds if the raft leader survived). So a
   delete-then-recreate recovery path is NOT viable for quorum loss.
3. **When the lost nodes return, parti resumes on the same consumer
   automatically** (raft state restored from the ≥1 surviving peer). Verified no
   loss and no gross duplication.
4. **NATS caps stream/consumer replicas at 5.** Combined with consumers
   co-locating on stream peers, "all of a consumer's peers die" cannot be
   isolated from stream-quorum loss — it degenerates into the quorum-loss gap or
   the full-outage case. There is therefore no separate total-peer-loss design
   case.
5. **The manager does NOT degrade on partial (2-of-5) loss** when its KV buckets
   are RF5 (quorum 3-of-5 holds). So manager-level signals (OnDegraded) do not
   cover the unservable-consumer case; the signal must be per-consumer.

## 3. Design

### 3.1 What parti does (unchanged, already correct)
- ConsumerNotFound / ErrConsumerDeleted → recreate (the manager-leader-owned
  creation path). Keep.
- Transient errors → backoff + retry (auto-resumes when quorum returns). Keep.
- No aggressive recreate of a quorum-lost consumer. Rejected because (a) delete
  is unreliable under quorum loss (fact 2) and (b) a replacement under a new name
  would double-deliver when the old consumer's quorum returns (fact 3) and would
  break the deterministic per-partition durable naming used for handoff.

### 3.2 What parti adds — the notification
A **non-terminal** per-consumer "unservable" signal, gated by an explicit
multi-state confirm classifier (this replaces the prior "neither NotFound nor
connectivity" formulation, which review found could reroute stream-missing /
degrading failures into the new hook).

#### 3.2.1 Confirm-result classifier (the P0 fix)
Today `Controller.confirmConsumerGone` (`internal/recovery/controller.go:543-553`)
calls `Info()` and returns a **bool** = `IsConsumerNotFound(err)`, discarding the
actual error. Replace it with a **lossless multi-state classifier** over the
`Info()` outcome, evaluated in this exact priority order:

1. `err` is `nil` **and** `ConsumerInfo.Cluster` has a non-empty `Leader`
   → **`healthy`** (consumer is serviceable; reset the unservable sequence).
2. `natsutil.IsConsumerNotFound(err)` → **`gone`** → existing recreate path
   (unchanged). Excludes the recreate contract from the unservable path.
3. `natsutil.IsDegradingJetStreamError(err)` (bucket/stream/consumer **missing**,
   `internal/natsutil/errors.go:92-100`) **or** `errors.Is(err, types.ErrStreamMissing)`
   → **`degrading`** → existing stream-missing / `recordKVError` → Degraded route
   (unchanged). This is the contract the P0 protects (AGENTS.md §"Cross-feature
   contracts" 1 + the stream-missing route in `manager_setup.go:49-83`).
4. `natsutil.IsConnectivityError(err)` (`internal/natsutil/errors.go:114-137`)
   → **`connectivity`** → plain backoff, **no** unservable signal (the connection
   layer / manager owns this; it also should not happen here since the criterion
   requires the connection UP).
5. Otherwise — `err` is an `Info()` response indicating the consumer's own raft
   group is unavailable while the connection is up: a `503` / `JetStream system
   temporarily unavailable` / no-quorum API error, `nats.ErrNoResponders` from the
   consumer Info, **or** `err == nil` but `ConsumerInfo.Cluster.Leader == ""`
   (leaderless) → **`unservable`** → the only state that increments the counter.

**Decision on `nats.ErrNoResponders`:** when returned by the consumer's own
`Info()` while the NATS connection is `CONNECTED`, it means the consumer's raft
group has no responder (no leader) → classify as **`unservable`**. It is NOT
treated as connectivity (and `IsConnectivityError` does not match it today, so no
existing routing is disturbed).

**Optional hardening (recommended, resolve in impl):** before emitting
`unservable`, cross-check that the **parent stream** is healthy (its `Info()`
returns a leader). If the stream itself is unhealthy, prefer `degrading` so the
manager owns it. This prevents a stream-wide outage from being reported as a
per-consumer problem.

#### 3.2.2 Counting + reset (P1 fix — reset on confirm RESULT, not ActionContinue)
- Maintain `consecutiveUnservable` on the `Controller`.
- On each confirm: `unservable` → increment; **any other class** (`healthy`,
  `gone`, `degrading`, `connectivity`) **and** any successful iterator progress
  (`ActionContinue`) → **reset to 0**. Reset is keyed on the confirm result /
  progress, NOT solely on `ActionContinue` (review P1: a successful `Info()` in the
  confirm path yields `ActionBackoff`, not `ActionContinue`, so resetting only on
  `ActionContinue` would let non-adjacent leaderless blips accumulate).
- The sequence must be **adjacent** unservable confirms; a single healthy confirm
  in between breaks it. This is what makes the false-positive guard real.

#### 3.2.3 Threshold, cadence, recovery (P1 fix — finalized, see §3.5)
When `consecutiveUnservable` first crosses the configured window/threshold (§3.5):
1. Emit an ERROR log, then rate-limited re-fires while it persists (§3.5 cadence).
2. Fire the non-terminal hook (§3.3) once on entry, then on the same rate-limited
   cadence.
3. **Keep retrying** — never exit the consume loop; this preserves automatic
   recovery the instant the operator restores NATS (fact 3).
4. On the next `healthy`/`ActionContinue`, emit an INFO "recovered" log and clear
   the episode (so a future episode re-fires from clean state).

- Traffic-independent: keys on confirm/`Info()` outcomes, not "no delivery in T",
  so an idle partition is never misclassified.
- **Distinct from `OnPermanentFailure`**, which is terminal (fires immediately
  before the consume loop exits on iterator-*creation* retry exhaustion /
  stream-missing). Unservable is recoverable and non-terminal.

### 3.3 Public API (decided)
```go
// consumer package
func WithOnConsumerUnservable(fn func(subject string, err error)) DynamicOption
```
Non-terminal callback fired (rate-limited) while a partition consumer is
unservable-but-existing and needs operator attention; the err preserves the
underlying cause (e.g. wrapped 503). Fires on the consume loop goroutine and MUST
be non-blocking.

**Interaction with `WithOnPermanentFailure` (called out per review):**
`WithOnPermanentFailure`'s godoc (`consumer/options.go:489-518`) documents that
registering it **disables the manager's auto-degraded route for stream-missing
exhaustion** (the dispatcher hands permanent failures to the app and stops).
`WithOnConsumerUnservable` is **independent and additive**: it does not change
that suppression and does not route through the manager observer. The two hooks
fire on disjoint conditions — `OnPermanentFailure` is terminal (iterator-create
exhaustion / stream-missing exit), `OnConsumerUnservable` is non-terminal
(consumer-raft unavailable while alive). Setting one does not enable/disable the
other. The plan does NOT change the existing `OnPermanentFailure` ↔ manager
auto-degraded suppression behavior.

Recovery is **log-only for v1** (an INFO "recovered" line + episode clear); a
paired `OnConsumerRecovered` callback is explicitly deferred (not needed for the
alert use case — the app alerts on the unservable signal and can clear its own
alert via its monitoring system).

### 3.4 Where the code goes
- `internal/recovery/controller.go`: replace bool `confirmConsumerGone` with the
  §3.2.1 multi-state classifier (returns an enum, e.g. `confirmResult`). In
  `Classify`'s `ErrorNeedsConfirm` branch (`:183-200`), switch on the enum:
  `gone` → existing recreate; `degrading` → return `ActionStreamMissing` so the
  existing stream-missing/manager route owns it (do NOT count); `connectivity` →
  `ActionBackoff` (no count); `unservable` → increment `consecutiveUnservable`,
  and when it crosses the §3.5 window fire `onUnservable` + escalating log, then
  `ActionBackoff` (keep retrying); `healthy` → reset + `ActionBackoff`. Reset
  `consecutiveUnservable` on `healthy` and on the `ActionContinue` success branches
  (`:171-179`, `:192-198`). Thread `onUnservable func(subject string, err error)`
  + window/cadence config into `Controller` via `ControllerConfig`.
- `consumer/options.go` + `consumer/dynamic.go`: add `WithOnConsumerUnservable`
  (a `DynamicOption`), plumb through `DynamicConfig` → `WorkerConsumerConfig` →
  `internal/durable` `partitionConsumerConfig` → `ControllerConfig`.
- Mirror the alert-level escalation cadence from `manager_degraded.go`
  (`monitorDegradedAlerts` `:311-371`) for the log re-fire; do not invent a new
  shape.
- Do NOT touch `recordKVError` / the manager degraded path — `degrading` and
  `connectivity` confirms keep flowing to it unchanged.

### 3.5 Timing model (finalized — replaces the prior open questions)
The detector is **duration-anchored** so it provably outlasts healthy leader
churn:

- **New dedicated option** `WithConsumerUnservableThreshold(window time.Duration)`
  rather than overloading `WithIteratorEscalation` (which means "burst → recreate"
  and would couple two unrelated policies). Default **`window = 10s`**.
- The detector fires when unservable confirms have persisted **continuously for
  ≥ window**. Implementation: track the timestamp of the first unservable confirm
  in the current adjacent run; fire when `now - firstUnservable ≥ window`. (A raw
  count is insufficient because confirm cadence varies; anchoring on elapsed time
  makes the false-positive bound explicit.)
- **Why 10s is safe vs election churn:** NATS Raft leader election + settle for a
  KV/consumer group completes in low single-digit seconds; the integration
  configs use `ElectionTimeout` 1–3s (`internal/testutil/nats.go`). A 10s window
  comfortably exceeds a single election/settle, so normal churn never fires. Apps
  with slower clusters raise the window.
- **Re-fire cadence:** after the first fire, re-fire (hook + escalating log) on the
  manager-style interval (default reuse `DegradedAlert.AlertInterval` semantics,
  escalating Info→Warn→Error→Critical) so a persisting outage keeps reminding
  without log-spam.
- **Recovery:** first `healthy`/`ActionContinue` after firing → INFO "recovered"
  log + clear episode (re-arm for future episodes).

## 4. Acceptance criteria (existing reproducers — must stay green)
The reproducers in §1 are the regression spec. After implementation:
- `cluster_unservable_signal_gap_test.go` **flips**: the "app gets no signal"
  assertions become "the app receives the unservable signal within a bounded
  window" (and the connection-UP / no-degrade facts remain).
- All other reproducers remain unchanged and green.

## 5. New tests (write with the implementation)

### 5.1 Unit — `internal/recovery` (deterministic, small window/threshold)
1. **Confirm-classifier table** over `Info()` outcomes → expected enum:
   `ConsumerNotFound` → `gone`; `StreamNotFound` / wrapped `types.ErrStreamMissing`
   → `degrading`; `nats.ErrTimeout` / `jetstream.ErrNoStreamResponse` →
   `connectivity`; `nats.ErrNoResponders` → `unservable`; API `503`/no-quorum →
   `unservable`; leaderless `ConsumerInfo` (nil err, empty `Leader`) → `unservable`;
   healthy leaderful `ConsumerInfo` → `healthy`.
2. **Routing**: `degrading` returns `ActionStreamMissing` (manager route), is NOT
   counted; `connectivity` returns `ActionBackoff`, not counted; `gone` recreates.
   Pins the P0 contract.
3. **Adjacent-reset**: interleave `unservable → healthy → unservable` confirms and
   assert the hook does NOT fire until `window` of *adjacent* unservable confirms;
   assert a success clears the episode and stops re-fires (P1 reset semantics).
4. **Existing pin preserved**: `alwaysSuccessInfo` after burst still yields
   `ActionBackoff` and no recreate (`controller_test.go:215-227`) — the classifier
   change must not regress it.

### 5.2 Unit — option plumbing
`WithOnConsumerUnservable` + `WithConsumerUnservableThreshold` flow through
`DynamicConfig` → `WorkerConsumerConfig` → `partitionConsumerConfig` →
`ControllerConfig`.

### 5.3 Integration — `test/integration/failure` (black-box, app-observable)
5. **Fires on sustained unservability:** flip `cluster_unservable_signal_gap_test.go`
   — sustained 2-of-5 loss → `WithOnConsumerUnservable` fires for the affected
   subject within a bounded window; preserve the existing connection-UP and
   manager-not-Degraded assertions; err is non-connectivity, non-NotFound.
6. **Silent on temporal blip (false-positive guard):** kill 2 nodes then
   `RestartNode` them **before** the window elapses → the hook does NOT fire and
   delivery resumes. Use a small explicit window for determinism.
7. **Silent on truly-gone:** delete a healthy consumer → recreated; unservable hook
   does NOT fire (routes to recreate).
8. **Silent on full outage (manager owns it):** 3-of-5 loss → manager `OnDegraded`
   fires; assert `WithOnConsumerUnservable` stays silent (negative-space guard).
9. **Silent on stream-missing (manager owns it):** stream-missing exhaustion still
   reaches the manager degraded route (`stream_missing_no_hook_test.go` pattern);
   assert the unservable hook is NOT fired.
10. **Clears on recovery:** sustained loss fires the hook; restore nodes → delivery
    resumes (same consumer, per `cluster_quorum_restored_test.go`) and the recovered
    INFO log is emitted; the hook stops re-firing.

## 6. Resolved decisions (were open; finalized via plan-review round 1)
- **Threshold model**: duration-anchored `window`, default **10s**, via a NEW
  option `WithConsumerUnservableThreshold` (not `WithIteratorEscalation`). Justified
  vs election churn in §3.5.
- **Reset**: on the confirm RESULT (any non-`unservable` class) and on
  `ActionContinue`, not on `ActionContinue` alone (§3.2.2).
- **Recovery**: log-only + episode clear for v1; paired `OnConsumerRecovered`
  callback deferred (§3.3).
- **Re-fire cadence**: manager-style escalating alert levels on
  `DegradedAlert.AlertInterval` semantics (§3.5).
- **`nats.ErrNoResponders` from consumer Info while connection UP**: classified
  `unservable` (§3.2.1).

## 7. Cross-feature contract guardrails (AGENTS.md — must not regress)
- Whole-bucket-missing → every worker Degraded (do not reroute bucket-missing into
  the unservable path — it must stay `recordKVError` → degraded).
- Peer claim takeover → only that worker claim-lost.
- OnDegraded fires once per Degraded entry.
- Start returns after sanity-check phase (async start).
- ConsumerNotFound → recreate and `ErrStreamMissing` routing must remain intact;
  the unservable criterion explicitly excludes both.
- Run `make test-integration -race` and the three contract tests when touching
  recovery classification.
