# P1.1 (F6-A) — Source-bucket escalation hook + metric

Per-PR spec for the fourth PR (first of Phase 1)
(`00-fix-plan.md` §P1.1). Lazy-written. Prior PRs P0.1/P0.2/P0.3
committed on their branches.

## Empirical correction to the plan (discovered during implementation)

The plan (`00-fix-plan.md` §P1.1 Anchors) says: detection fires on
`jetstream.ErrBucketNotFound` in `restartWatcher` and `reconcileOnce`.
Empirically, against nats.go v1.50.0:

- `js.KeyValue(ctx, bucket)` after `js.DeleteKeyValue` → returns
  `jetstream.ErrBucketNotFound` ✓ (the lookup path).
- **`kv.Get(ctx, key)` on a cached KV handle after delete → returns
  `nats.ErrNoResponders`** ✗ (the reconcile path; no JetStream
  responder is bound to the subject anymore).
- **`kv.Watch(ctx, key)` on a cached watcher after delete → returns
  `jetstream.ErrStreamNotFound`** ✗ (the restartWatcher path; the
  KV bucket is backed by a stream named `KV_<bucket>` and that
  stream is gone).

Neither call site that the plan names actually surfaces
`ErrBucketNotFound`. The implementation therefore classifies all
three errors as "bucket unavailable" via an `isBucketUnavailableErr`
helper. `nats.ErrNoResponders` is broader than "bucket deleted" (it
also fires during transient NATS reconnect), but the cooldown +
gauge-clears-on-success design tolerates the false positive: the
hook fires once, the gauge clears on the next successful op, and
the operator's readiness probe sees only a brief blip. This is
preferable to missing the genuine bucket-loss case.

This finding deserves a memory pin (`project_nats_kv_delete_surface`)
in the same family as `project_nats_watcher_empirical_finding`.

## Background

`source.NatsKV` accepts a `jetstream.KeyValue` handle from the caller
(`source/nats_kv.go:178`) — the library never creates the bucket. If
the bucket is deleted on the live connection, two read sites surface
the loss:

- `restartWatcher` (`source/nats_kv.go:772-805`) retries forever on
  `watchFn` error. With the bucket gone, every retry produces
  `jetstream.ErrBucketNotFound` and a logError line.
- `reconcileOnce` (`source/nats_kv.go:864-878`) treats anything other
  than `ErrKeyNotFound` as a generic log line.

Neither path escalates. The Manager continues running with a stale
in-memory partition list and the operator has no readiness signal.

## Scope (additive — new public API)

1. New exported callback type `SourceUnavailableHook func(err error)`.
2. New option `source.WithUnavailableHook(SourceUnavailableHook)`.
3. New metric interface `types.SourceMetrics` (single method:
   `SetSourceBucketMissing(missing bool)`) + `NopSourceMetrics`
   default, mirroring `HandoffMetricsRecorder`'s pattern. New
   `source.WithMetrics(types.SourceMetrics)` option.
4. Detection in `restartWatcher` and `reconcileOnce`: at the first
   observation of `jetstream.ErrBucketNotFound`, fire the hook
   (rate-limited, default 30s cooldown) and set the metric to
   `missing=true`. On a successful subsequent watcher restart or
   `kv.Get`, set the metric back to `missing=false`.

No public API on the existing types is changed; only additions. The
existing `WithReconcileInterval` / `WithLeadershipProbe` /
`WithUpdateRetries` options keep their current signatures.

## Design

### Public type and options

```go
// SourceUnavailableHook fires when the partition-source bucket is
// observed to be missing on the live connection. The library does not
// recreate the bucket (user-owned; review §5 category A). Wire this
// into your readiness logic — e.g. flip a readiness flag and let the
// pod rotate — so the orchestrator can rebuild the bucket and let
// startup re-provision cleanly.
//
// The hook is invoked synchronously from the source's reconciler /
// restart goroutine. Implementations MUST be non-blocking and MUST
// NOT call back into the source (recursive Get / Update will deadlock
// against the watcher's restart path).
type SourceUnavailableHook func(err error)

// WithUnavailableHook registers a hook that fires when the partition-
// source bucket is observed to be missing. See SourceUnavailableHook
// for the deadlock contract.
//
// Without a hook, the loss is still logged and the metric is set, but
// no escalation path runs — the library cannot recreate a user-owned
// bucket on its own.
func WithUnavailableHook(h SourceUnavailableHook) NatsKVOption
```

### Metric interface (new file `types/source_metrics.go`)

```go
// SourceMetrics captures source-layer health metrics.
//
// Implementations must be non-blocking and thread-safe. A no-op
// implementation (NopSourceMetrics) is provided for callers that do
// not need source metrics.
//
// This interface is intentionally separate from MetricsCollector
// because the source layer is independent of the Manager runtime and
// is constructed independently (a source can run without a Manager).
type SourceMetrics interface {
    // SetSourceBucketMissing sets the parti_source_bucket_missing
    // gauge (0 = available, 1 = missing). Called from the source
    // reconciler at the first ErrBucketNotFound observation and
    // again when a subsequent operation succeeds.
    SetSourceBucketMissing(missing bool)
}

type NopSourceMetrics struct{}

func (NopSourceMetrics) SetSourceBucketMissing(bool) {}
```

### NatsKV state additions

```go
type NatsKV struct {
    // ... existing fields ...

    // F6-A: bucket-unavailability escalation
    unavailableHook   SourceUnavailableHook
    metrics           types.SourceMetrics
    unavailableMu     sync.Mutex  // serialises lastHookAt / bucketMissing transitions
    lastUnavailableAt time.Time   // most-recent hook invocation; zero == not fired
    unavailableCooldown time.Duration // default 30s; mirrors defaultReconcileInterval
    bucketMissing     bool        // current gauge state (under unavailableMu)
}
```

Defaults applied in `NewNatsKV`:
- `metrics = types.NopSourceMetrics{}`
- `unavailableCooldown = 30 * time.Second` (matches the default
  reconcile cadence; one cooldown ≈ one reconcile cycle)

### Detection sites

Both `restartWatcher` (after each `watchFn(ctx)` error) and
`reconcileOnce` (the generic-error branch) gain a call to a new
helper:

```go
// noteBucketUnavailable inspects err for jetstream.ErrBucketNotFound
// and, when matched, fires the hook (rate-limited) and sets the
// metric gauge. Returns true iff the error matched, so the caller
// can keep its existing log line distinct.
func (s *NatsKV) noteBucketUnavailable(err error) bool {
    if !errors.Is(err, jetstream.ErrBucketNotFound) {
        return false
    }
    s.unavailableMu.Lock()
    fire := s.unavailableHook != nil &&
        time.Since(s.lastUnavailableAt) >= s.unavailableCooldown
    if fire {
        s.lastUnavailableAt = time.Now()
    }
    if !s.bucketMissing {
        s.bucketMissing = true
        s.metrics.SetSourceBucketMissing(true)
    }
    s.unavailableMu.Unlock()

    if fire {
        s.unavailableHook(err)
    }

    return true
}

// noteBucketAvailable is called from the success paths of
// restartWatcher (watcher created) and reconcileOnce (kv.Get OK)
// to clear the gauge once the source is reachable again. The hook
// is NOT fired on recovery — only on degradation.
func (s *NatsKV) noteBucketAvailable() {
    s.unavailableMu.Lock()
    if s.bucketMissing {
        s.bucketMissing = false
        s.metrics.SetSourceBucketMissing(false)
    }
    s.unavailableMu.Unlock()
}
```

Call-site changes (minimal):

```go
// In restartWatcher, replacing the existing error log:
watcher, err := s.watchFn(ctx)
if err == nil {
    // ... existing success path ...
    s.noteBucketAvailable()
    return
}
if !s.noteBucketUnavailable(err) {
    s.logError("failed to restart watcher, will retry", "error", err, "backoff", backoff)
} else {
    s.logError("source bucket missing; will retry", "error", err, "backoff", backoff)
}
```

```go
// In reconcileOnce, replacing the generic-error branch:
entry, err := s.kv.Get(ctx, s.key)
if err != nil {
    if errors.Is(err, jetstream.ErrKeyNotFound) {
        s.applyEmptyPreservingKnown()
        s.noteBucketAvailable() // KeyNotFound implies bucket exists
        return
    }
    if s.noteBucketUnavailable(err) {
        s.logError("reconcile: source bucket missing", "error", err)
        return
    }
    s.logError("reconcile: failed to fetch partitions", "error", err)
    return
}
// ... existing success path; before applyLocal:
s.noteBucketAvailable()
```

## Reproducer test list

- *T1 (must fail on parent — hook fires).* Integration test under
  `test/integration/failure/`: start an embedded NATS, create a
  source bucket, wire `NatsKV` with `WithUnavailableHook` (records
  invocations) and `WithReconcileInterval(200ms)` for a fast tick,
  start it, then delete the bucket. Assert the hook fires within
  one reconcile interval (≤ ~500 ms allowing for tick alignment)
  with an error matching `jetstream.ErrBucketNotFound`. On parent:
  hook field absent (compile error) OR, after introducing the
  option scaffolding with no detection, the hook never fires (the
  loss is silent).
- *T2 (metric gauge set on loss).* Same setup; assert the metric
  stub's gauge reads `true` after detection.
- *T3 (cooldown — hook fired at most once per cooldown window).*
  With cooldown overridden to 100ms via a test seam, delete the
  bucket and let reconcile tick several times (3-5 ticks within
  the cooldown). Assert the hook fires exactly once. Then advance
  past the cooldown and trigger one more tick: assert the hook
  fires a second time. The metric gauge stays `true` throughout.
- *T4 (recovery clears the gauge).* After T1 fires the hook,
  re-create the bucket (same name). On the next reconcile tick the
  source recovers; assert the metric gauge reads `false`. The hook
  is **not** re-fired on recovery (one-way escalation; the gauge
  carries the recovery signal).
- *T5 (no hook configured — silent path).* Same setup without
  `WithUnavailableHook`. Delete the bucket. Assert no panic, the
  reconcile loop keeps running, the metric gauge reads `true`.
- *T6 (no metrics configured — silent path).* Same setup with the
  hook but no `WithMetrics` (default Nop). Delete the bucket.
  Assert no panic; the hook still fires.
- *T7 (default 30s cooldown).* Unit test (no NATS): construct a
  `NatsKV`, default cooldown applies; call `noteBucketUnavailable`
  three times in rapid succession; assert hook fired exactly once.
  The metric is set on every call (because the gauge tracks the
  state, not the events).

## Verification gates

- `make lint && make test && make test-race && make test-integration`
  green.
- New exported symbols audited: `SourceUnavailableHook`,
  `WithUnavailableHook`, `WithMetrics`, `types.SourceMetrics`,
  `types.NopSourceMetrics`. No existing exported API changed.
- `docs/OPERATIONS.md` updated under the existing degraded-mode
  section to name the hook as the primary signal for source-bucket
  loss; remind the operator that the library does not recreate the
  bucket.

## How this trips readiness

The k8s operator wires `OnSourceUnavailable` into a
readiness-probe flag (mirroring the existing `OnDegraded` pattern).
A silent source loss now fails readiness → pod rotation → on
restart, the source bucket is presumably re-provisioned by the
operator's startup flow (or the failure surfaces consistently and
the operator intervenes).

## Out of scope

- Library auto-recreating the source bucket — category A; forbidden.
- The retry-bounding loop on `restartWatcher` — F2's territory
  (PR P2.4a). This PR keeps `restartWatcher`'s forever-retry
  behavior; it just adds escalation on bucket-loss.
- Wiring the hook into Manager's degraded-mode machinery (the hook
  is caller-owned; the Manager doesn't need to consume it).

## Dependencies & sequencing

Independent. First PR of Phase 1 because it is the smallest
additive change in the phase.
