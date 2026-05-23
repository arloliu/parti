# P1.3 (F1) — Epoch fence: detect bucket wipe-and-recreate

Per-PR spec for the sixth PR (third of Phase 1)
(`00-fix-plan.md` §P1.3). Prior PRs P1.1, P1.2 committed.

## Background

Today the Manager caches KV handles (`m.assignmentKV`,
`m.heartbeatKV`, etc.) at `Start`. If the cluster operator wipes a
bucket and re-creates one under the same name (e.g. via a runbook
that misjudges scope, or as part of the F9-A migration), the cached
handles bind to the new stream silently. Writes succeed but the
new bucket has none of the prior state — workers continue running
against a freshly empty universe. The current connection monitor
only detects connectivity-level errors, not bucket-identity
changes, so the worker stays `Ready`.

This is the highest-impact **readiness-blind** gap. F9-A's
migration runbook explicitly performs a wipe-and-recreate, so F1
is a hard prerequisite for P2.1.

## Design deviation from the plan (recorded)

The plan §P1.3 says "each KV-watcher reconciler gains a single new
step." That requires touching four packages' reconciler loops
(assignment, commit, claim-resolver, source) — high blast-radius
for what is, behaviorally, "poll five buckets' Created timestamps."

This implementation uses a **single centralized monitor goroutine**
(`monitorBucketEpochs`) instead. Equivalent detection latency
(default 30s, matching the dominant reconciler cadence), much
smaller surface area. The monitor is started by `Manager.Start`
after the five Parti-owned buckets have been ensured, runs until
`m.ctx` is cancelled, and calls `m.enterDegraded("bucket-recreated:<bucket>")`
on the first detected mismatch.

The deviation does not change the contract: detection latency
~30s (configurable via the same `OperationTimeout` knob the
ensure paths use), single `OnDegraded` invocation, no recovery
attempted in-process.

## Scope (additive — detection only)

1. New `kvutil.BucketStreamCreated(ctx, kv)` helper returning
   the JetStream stream's `Created` timestamp.
2. New `Manager.bucketEpochs map[string]time.Time` populated at
   `Start` for each of the five Parti-owned buckets (stableid,
   election, heartbeat, assignment, handoff).
3. New `Manager.captureBucketEpoch(bucket, kv)` helper called
   from each existing ensure path.
4. New `Manager.monitorBucketEpochs(ctx)` goroutine started from
   `Start` after all buckets are captured.
5. New degraded reason constant `degradedReasonBucketRecreated`
   and a per-bucket formatted reason string
   `"bucket-recreated:<bucketName>"` passed to `enterDegraded`.

No public API change. The `OnDegraded` hook already receives the
reason; no caller migration needed.

## Reproducer tests

- *T1 (primary, per bucket).* For each of the five Parti-owned
  buckets: start a Manager, capture the epoch, delete + recreate
  the bucket, assert `OnDegraded` fires within 2 × monitor tick
  with reason `bucket-recreated:<bucket>`. On parent the
  detection does not exist — test times out.
- *T2 (happy path).* Same bucket, no recreate. Monitor ticks for
  3 cycles; no `OnDegraded`. Prevents false-positive.
- *T3 (NATS restart with state intact).* Use the existing
  `claim_resolver_nats_restart_test.go` server-restart pattern.
  Restart NATS server preserving the StoreDir; assert NO epoch
  fence trip — `Created` is preserved across the restart.

## How this trips readiness

Direct: detection → degraded → `OnDegraded` → readiness flip →
pod rotation → restart re-provisions every missing bucket via
get-first `EnsureKVBucket`. The previously-silent wipe-and-recreate
path now triggers the standard recovery loop.

## Dependencies

Independent of P1.1, P1.2. **Hard prerequisite for P2.1 (F9-A)** —
F9-A's migration runbook performs a bucket-delete, and the F1
fence is what catches the resulting recreate event safely.

## Out of scope

- Healing the corruption in-process (fail-loud only).
- `provision/marker` alternative (mentioned in plan; not chosen).
- Source bucket — covered by P1.1 (F6-A) hook, not the epoch fence.
