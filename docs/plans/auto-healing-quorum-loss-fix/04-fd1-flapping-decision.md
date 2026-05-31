# F-D1 Flapping — Decision Record

- **Date:** 2026-05-31
- **Status:** DECIDED & IMPLEMENTED.
- **Resolves:** the open decision in [`00-fix-plan.md`](00-fix-plan.md) §2 and §7.1
  — *"is auto-Degraded on transient read timeouts desirable, or does it cause
  Degraded flapping on brief blips? Tunable via `KVErrorThreshold` /
  `KVErrorWindow`."*

## Question

F-D1 auto-enters `Degraded` (reason `kv-unavailable`) when sustained
connected-but-KV-unavailable timeouts (`context.DeadlineExceeded` /
`nats.ErrNoResponders`) accumulate from the manager's periodic KV-op sites
(heartbeat / election / assignment-watcher / stableid-renew / commit). The
blast radius of a *spurious* `Degraded` is real: `OnDegraded` may be wired to a
readiness probe that rotates pods, and a leader's recovery-grace suppresses
emergency rebalance on each entry. So the question is whether the circuit flaps
under merely *transient* blips, and whether the answer is tuning or a mechanism
change.

## Investigation

A probe (`test/integration/failure/`, since folded into the regression test
below) drove an intermittent heartbeat-bucket timeout pattern (4s faulted / 8s
healed × 4 blips) and counted `OnDegraded` entries per `DegradedBehavior`
preset:

| preset | KVErrorThreshold | flaps over 4 blips | recovered |
|---|---|---|---|
| aggressive | 3 | 7 | ✅ all → Stable |
| balanced | 5 | 5 | ✅ |
| conservative | 10 | 3 | ✅ |

The **conservative** result is the tell: a single 4s blip at 500ms cadence is
~8 faulted heartbeats — *below* its threshold of 10 — yet it still flapped 3×.
That can only be cross-blip accumulation. Two findings explain the curve:

- **Finding A — recover-on-wrong-signal.** Recovery
  (`attemptRecoveryFromDegraded`) exits `Degraded` on connectivity uptime
  (`ExitThreshold`) **plus a successful assignment-bucket read** — *not* on the
  failing op recovering. A heartbeat-write timeout doesn't drop the connection,
  so the worker can exit while heartbeat writes still fault, re-accumulate, and
  re-degrade. This is the within-blip re-degradation that pushes aggressive to
  7 > 4 blips.
- **Finding B — no success-reset on the Stable path.** The error counter only
  reset on degraded-recovery, never on a successful KV op while `Stable`. So the
  circuit was **N-in-window**, not the **consecutive** errors the config doc
  claimed. Intermittent blips *summed* within `KVErrorWindow` across healthy
  periods — exactly the conservative 3-flap surprise.

## Decision

**Fix Finding B; defer Finding A.** Finding B is the dominant lever on
*intermittent-transient* flapping (the precise worry in the open decision), and
its fix is small and contained. Finding A (flapping under a *sustained* fault on
a non-assignment bucket) is narrower and touches the contract-pinned recovery
path, so it is a separate follow-up taken only if observed.

### What shipped (Finding B)

A **class-aware** healthy-op success-reset:

1. `manager_degraded.go` — the KV-error window (`kvErrorWindow`) now tags each
   entry by class (`kvErrorEvent{at, transient}`): `transient` = an F-D1
   `ErrKVUnavailable`-wrapped timeout; non-transient = whole-bucket loss
   (connectivity / degrading-JetStream).
2. `Manager.recordKVHealthyOp` clears **only the transient entries** on a
   successful periodic KV op while not degraded (no-op while degraded). The
   whole-bucket-loss entries are retained.
3. `heartbeat.Publisher.SetOnSuccess` fires after each successful periodic
   heartbeat publish; the manager wires it to `recordKVHealthyOp`. The heartbeat
   is the highest-frequency periodic KV op, so its success is the natural
   "heartbeat KV is serving" signal.

Result: a run of connected-but-KV-unavailable timeouts only trips `Degraded`
when **no success intervenes** — true consecutive-error semantics. The config
doc on `KVErrorThreshold` was corrected to match.

### Explicit semantic narrowing (accepted)

Because the heartbeat-success reset is *global* over the transient class, a
healthy heartbeat now clears F-D1 timeouts attributed to *slower* sources
(election / stableid / assignment) before they reach threshold. So F-D1 now
trips primarily on a **sustained run of heartbeat-KV failures with no
intervening success**. A sustained quorum-loss on a *non-heartbeat* bucket that
surfaces **only** as F-D1 timeouts (not as a whole-bucket
`ErrBucketNotFound`/connectivity error) may no longer trip via F-D1 alone — it
relies on the whole-bucket classification, or on the deferred Finding A
follow-up (per-source counters / verify-the-failing-op-recovered). This is a
deliberate, documented coverage change, scope-consistent with deferring A.

**Whole-bucket loss of any bucket still trips** (AGENTS.md contract 1): those
entries are non-transient and accumulate untouched even while heartbeat keeps
succeeding. This is pinned by a new guard the existing all-buckets-wipe test
cannot catch (it kills heartbeat too, so no success fires).

### Tuning

The presets are unchanged. With Finding B fixed, the thresholds now mean what
they say (consecutive transient errors), so the defaults are the right knobs for
an operator who wants F-D1 more/less eager; no preset retune was warranted.

## Tests

- `TestFD1_IntermittentKVTimeouts_NoFlap` (`test/integration/failure/`) —
  intermittent sub-threshold heartbeat blips with recovery between must NOT
  degrade. **RED on parent** (cross-blip accumulation trips), GREEN with the fix.
- `TestManager_PartialBucketLoss_HeartbeatHealthy`
  (`test/integration/manager/`) — wipes every bucket *except* heartbeat; all
  workers must still degrade. Proves the class-aware reset does not mask a
  whole-bucket loss when an unaffected bucket keeps succeeding.
- `TestManager_recordKVHealthyOp` (unit) — clears only transient entries,
  retains whole-bucket entries, no-op while degraded.

## Deferred (Finding A)

If a sustained quorum-loss on a single non-heartbeat bucket is observed to
flap or to escape F-D1, take it up as a separate follow-up: candidates are a
post-recovery cooldown (don't re-degrade for N seconds after exiting),
verifying the *failing* op recovered before exiting `Degraded`, or per-source
error counters so a heartbeat success resets only heartbeat-attributed errors.
All touch the recovery path the cross-feature contracts pin, so they go through
the full plan → review → impl loop.
