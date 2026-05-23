# P2.4a (F2) — Bounded-retry envelope + restartWatcher wiring

Per-PR spec for the eighth PR (P2.4a — first of four F2 sub-PRs).
Prior PRs through P2.1 committed. Phase 2 dominant fixes begin here.

## Scope deviation (recorded)

The plan §P2.4a says: "After exhaustion, `restartWatcher` fires
`OnSourceUnavailable` (P1.1's hook) and stops retrying until the
source reconciler successfully re-reads via `kv.Get`."

P1.1's hook lives on a separate branch (`self-healing-p11-f6a-source-unavailable-hook`),
not on `main`. This PR is independent and lands cleanly on `main`,
so wiring to that specific hook waits for the merge sequence.
Instead, the envelope-exhaustion path here:

1. Logs the exhaustion at WARN.
2. Exits the `restartWatcher` goroutine, leaving the periodic
   reconciler as the sole recovery path (already documented in
   `project_nats_watcher_empirical_finding` as load-bearing).
3. Records the exhaustion event via a new
   `parti_source_watcher_restart_exhausted` counter (registered
   through the new `types.SourceMetrics.IncWatcherRestartExhausted`
   method — additive only).

When P1.1 and P2.4a merge, the integration PR (or a follow-up)
wires the exhaustion to `OnSourceUnavailable` directly. This split
keeps both PRs reviewable independently against `main`.

## Background

Today `restartWatcher` (`source/nats_kv.go:772-805`) retries
forever on `watchFn` error. With the bucket gone, every retry
produces `nats.ErrNoResponders` / `jetstream.ErrStreamNotFound`
and a logError line. The loop is the **dominant thundering-herd
risk** in the source layer: a transient NATS reconnect surge can
have every worker hammering the same subject in lockstep, and a
permanent bucket-loss has the loop running until process death.

## Design

### New package `internal/retry/envelope.go`

```go
// retryClass classifies an error from the work function for the
// envelope's next action.
type retryClass int

const (
    retryTransient retryClass = iota
    retryGiveUp
    retryFatal
)

type retryEnvelope struct {
    work        func(ctx context.Context) error
    classify    func(err error) retryClass
    onPermanent func(err error)
    onProgress  func(attempt int, err error)
    baseBackoff time.Duration
    maxBackoff  time.Duration
    maxAttempts int
    jitter      float64
}

func (e *retryEnvelope) run(ctx context.Context) error
```

`run` returns:
- `nil` on success
- `ctx.Err()` if cancelled
- `errRetryExhausted` on attempt budget exhaustion (after firing
  `onPermanent`)
- the work's error directly on `retryFatal` (no retries)

### Wiring at `restartWatcher`

```go
func (s *NatsKV) restartWatcher(ctx context.Context) {
    env := retry.NewEnvelope(retry.Config{
        Work:        s.tryRebindWatcher, // wraps watchFn + bookkeeping
        Classify:    s.classifyRebindErr,
        OnPermanent: s.onRebindExhausted,
        BaseBackoff: watcherBaseBackoff,
        MaxBackoff:  watcherMaxBackoff,
        MaxAttempts: 6, // ~64s @ 1s base; matches one reconcile cycle
        Jitter:      watcherJitter,
    })
    _ = env.Run(ctx) // ctx cancellation and exhaustion both exit the goroutine
}
```

## Reproducer tests (envelope package)

Unit tests against the envelope directly, no NATS dependency:

- *T1.* Success on first attempt → 1 call, no backoff, returns nil.
- *T2.* Transient × 3 then success → 4 calls total, returns nil.
- *T3.* Transient × MaxAttempts → returns errRetryExhausted,
  onPermanent fired exactly once with the LAST error.
- *T4.* GiveUp on first call → onPermanent fired immediately with
  that error; returns errRetryExhausted.
- *T5.* Fatal on first call → returns the original error directly;
  onPermanent NOT fired (fatal ≠ permanent — fatal means caller
  needs to handle it; permanent means we gave up).
- *T6.* Context cancel mid-backoff → returns ctx.Err(); onPermanent
  NOT fired.
- *T7.* Backoff capped at MaxBackoff (no exponential overshoot).
- *T8.* Jitter applied (statistical: 100 runs; backoff in
  [base × (1-jitter), base × (1+jitter)]).

## Verification gates

- `make lint && make test && make test-race` green.
- New exported symbols audited (only the `internal/retry` package's
  surface; no public-API change).

## How this trips readiness

P2.4a alone does NOT trip readiness — exhaustion exits the
restart goroutine, leaving the reconciler as recovery. The
readiness trip arrives once P1.1's hook is wired (after both
land on main; the integration is a small follow-up).

## Out of scope

- Wiring P1.1's `OnSourceUnavailable` (separate branch, separate
  merge).
- Applying the envelope to other call sites — P2.4b/c/d.
