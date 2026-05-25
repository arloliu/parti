# Fleet-wide Spatial Burst Diagnostic Plan

> **For agentic workers:** Implement this as a single test addition. Do not
> refactor surrounding code, do not change library defaults, do not add a
> new `MetricsCollector` method. See "Out of scope" before starting.

**Goal:** Augment the existing per-worker burst diagnostic
(`TestApplyCoalescing_UnderReElectionBurst`) with a fleet-wide spatial-burst
measurement and a multi-phase trigger sequence that exercises it. The
diagnostic produces a measurement-driven recommendation for
`Config.ApplyStartJitter`, parallel to how the per-worker view recommends
`Config.AssignmentWatcherDebounce`.

**Why:** The landed per-worker diagnostic measures *temporal* bursts at a
single worker — how many distinct versions a worker applies within a 50 ms
idle window. Under the tested scenario (Parti-leader kill on a 3-node R=3
cluster), `max_burst_size=1` reflects that the version gate +
`RebalanceCooldown` already pace consecutive distinct versions far beyond
50 ms at any one worker. This is good, but it does not measure the
*spatial* fleet-wide simultaneity that motivates `ApplyStartJitter`. The
new leader's `V=N+1` publish is observed by all surviving workers within
tens of ms of each other — a synchronized fleet event the existing metric
discards by partitioning samples per worker.

**Scope:** One commit on `worktree-herd-diagnostic-cluster`, adding a new
test (or extending the existing one — see Design §"Test shape") plus the
fleet-wide aggregation helpers. No library code changes.

**Tech stack:** Same as the landed diagnostic — Go 1.22+, embedded NATS
via `partitest.StartEmbeddedNATSCluster`, no new dependencies.

---

## Out of scope

These are explicitly NOT in this plan:

1. **Changing the default of `ApplyStartJitter` or `AssignmentWatcherDebounce`.**
   The diagnostic's output is data; whether to act on it is a separate PR.
2. **Adding a new `MetricsCollector` method.** Reuse `RecordApplyAttempt`
   (workerID, version, timestamp) — the fleet view is computed from the
   same samples already collected.
3. **Production runtime exposure.** This is a tuning diagnostic, not a
   health probe. No new API surface.
4. **Refactoring `testutil.RemoveWorker`'s 5 s timeout.** Known limitation
   already noted; the existing test sidesteps it with a direct
   `mgr.Stop(ctx)` and so will this one.
5. **Cross-region / multi-cluster topologies.** Single 3-node embedded
   cluster is the test environment.
6. **Replacing the per-worker metric.** Augment, not replace — both views
   are kept and reported side-by-side.

---

## Background — verified facts

These were verified against this branch
(`worktree-herd-diagnostic-cluster`) before writing this plan.

### The per-worker diagnostic (commit `adb4181`)

- `test/integration/manager/apply_coalescing_test.go` holds
  `TestApplyCoalescing_UnderReElectionBurst`.
- Each of the 20 workers gets its own `recordingBurstCollector` (struct
  definition at lines 19–22) wired via `parti.WithMetrics(collectors[i])`.
- `RecordApplyAttempt(workerID, version)` (lines 32–36) appends
  `burstSample{time.Now(), workerID, version}` to that worker's
  collector — one collector ≡ one worker.
- `analyzeBursts` (lines 228–287) sorts ONE worker's samples by
  timestamp and groups consecutive samples whose inter-arrival gap is
  ≤ `idleGap=50ms`. The output is per-worker `MaxBurstSize` and
  `MaxBurstDuration`.
- `aggregateMaxBurstSize` / `aggregateMaxBurstDuration` (lines 299, 309)
  take the max across all workers. They do NOT merge timelines.
- AGGREGATE banner (lines 213–218) is logged once at the end:
  `max_burst_size=1 max_burst_duration=0s recommended_debounce_window=50ms`.
- `recommendedWindow` (lines 319–334) maps `max_burst_duration` →
  `Config.AssignmentWatcherDebounce` via
  `ceil(d / 50ms) * 50ms + 50ms`, capped at 1 s.

### The apply pipeline gate

- `RecordApplyAttempt` is called from `applyAssignmentWithPrevCore`
  (`manager_assignment.go:1020`), reached only after the version gate
  at `manager_assignment.go:584`: `if oldAssignment.Version >= newAssignment.Version { return }`.
- The metric therefore fires only for strictly increasing per-worker
  versions. Same-version watcher replays are filtered before recording.

### `ApplyStartJitter` — the spatial-burst absorber

- `Config.ApplyStartJitter time.Duration` declared at `config.go:491`
  (default `"0"`, validation `gte=0`, validation `<= 10s` at
  `config.go:657`; recommended `<= StartupTimeout/4` per the field's
  Godoc — NOT a hard cap).
- Applied at `manager_assignment.go:966`: when `> 0`, the manager sleeps
  `rand.Duration(jitter)` before taking `applyStoreMu` on a fresh-version
  apply. The retry path (`applyAssignmentWithPrevSkipJitter`,
  `manager.go:203`) bypasses it.
- This is the per-worker control that smooths a fleet-wide simultaneous
  apply spike — exactly the quantity the new metric measures.

### Cluster + test infrastructure (commit `ed06e46` + main)

- `testutil.StartEmbeddedNATSCluster(t) (*nats.Conn, []*server.Server, func())`
  — 3-node cluster, R=3-capable.
- `testutil.NewWorkerClusterWithSource(t, nc, src, cfg)` — wires workers
  against a custom `parti.Config`. The new test uses this to set
  `cfg.KVBuckets.Replicas = 3`, `cfg.ApplyStartJitter = 0`,
  `cfg.AssignmentWatcherDebounce = 0`.
- `wc.WaitForLeader(timeout)` and `wc.WaitForNewLeader(oldID, timeout)`
  in `internal/testutil/cluster_helpers.go` — leadership polling.
- `wc.AddWorkerWithOptions(ctx, opts...)` — adds a worker with custom
  options (used to wire per-worker collectors and additional workers
  during P2).
- `wc.RemoveWorker(idx)` exists but has a hardcoded 5 s `Stop` timeout
  that is too short for a calculator-leader; the existing test sidesteps
  with `mgr.Stop(ctx)` using a 30 s budget.

### Empirical evidence from the landed test

From the subagent's run (logged in commit `adb4181` test output):

- Pre-kill: V=2 observed on survivors.
- Post-soak: V=4 (delta=2).
- Per-worker `max_burst_size=1` for all 20 workers (logged per-worker).

So between V=2 and V=4 the fleet collectively recorded ≥19 samples each
for V=3 and V=4. The per-worker view loses this because those samples
land in 19 separate collectors. The fleet view recovers it.

---

## Design — the metric

Two views are computed from the merged sample timeline; both are reported
because they answer different questions.

### View A — per-version fanout

For each distinct version `V` observed across all collectors:

| Field | Definition |
|---|---|
| `WorkerCount` | distinct workers that recorded `V` |
| `Span` | `max(at) - min(at)` across samples for `V` |
| `First`, `Last` | the corresponding timestamps |
| `Workers` | sorted slice of worker IDs that observed `V` |

Output (one line per version where `WorkerCount > 1`, suppressed
otherwise to avoid noise from per-worker cold-start initial apply):

```
FLEET version=2 worker_count=19 span=37ms
FLEET version=3 worker_count=19 span=42ms
FLEET version=4 worker_count=19 span=35ms
```

### View B — sliding-window peak concurrency

Walk the merged, time-sorted sample timeline. For each sample at time `t`,
count distinct workers with any sample in `[t - W, t + W]`, where
`W = idleGap = 50 ms` (reuse the per-worker constant). Track:

| Field | Definition |
|---|---|
| `PeakConcurrency` | max over all samples of "distinct workers in window" |
| `PeakAt` | timestamp at which `PeakConcurrency` was first observed |

This is intentionally **version-agnostic**: if two distinct versions arrive
at the fleet within the 100 ms window, both contribute to `PeakConcurrency`.
That's the correct shape for "total apply-pipeline pressure" — every recorded
sample represents one `applyAssignmentWithPrevCore` invocation taking
`applyStoreMu`, so cross-version overlap is real load, not double-counting.

For "how long did one fleet event last" use View A's per-version `Span`
instead (a per-version `Span > W` proves the event was longer than one
window without needing a separate "burst duration" metric).

Output:

```
AGGREGATE_FLEET (phase=leader_kill) peak_concurrency=19 worst_version_span=42ms versions_observed=2
AGGREGATE_FLEET (overall) peak_concurrency=21 worst_version_span=85ms recommended_apply_jitter=200ms
```

`worst_version_span` is the max `Span` across all `versionFanout` entries
in the phase (or overall) — i.e. the longest single-version fleet event
observed. This is what feeds the recommendation formula.

`O(N²)` naive implementation is fine for diagnostic-sized N
(≤ 2000 samples for a 20-worker, 3-phase run).

### Recommendation formula

Input: `worst_version_span` — the longest `versionFanout.Span` observed
overall **across versions with `WorkerCount > 1` only** (i.e. the
wall-clock duration of the longest *multi-worker* single-version fleet
event, from first worker's apply of that version to last worker's apply
of that version). Single-worker entries (e.g. a retry that landed alone)
are excluded so they cannot inflate the recommendation. NOT View B's
`PeakConcurrency` (which is intentionally cross-version-mixed) and NOT
a fixed window size (which would be vacuous).

```
recommended_apply_jitter =
    max(
        ceil(worst_version_span / 50ms) * 50ms,
        100ms,                                  // floor
    ) + 100ms                                    // +1 slot scheduler-jitter, +1 slot measurement-noise
    capped at 1s                                 // see "cap rationale" below
```

Justification (the formula is heuristic; the diagnostic's primary output
is the *raw* `peak_concurrency` and `worst_version_span` so operators
can apply their own rule):

- **Floor (100 ms).** In-process tests underestimate real-world scheduling
  jitter. A 100 ms floor prevents recommending sub-50 ms values that
  would be dwarfed by application-level scheduling.
- **Safety margin (+100 ms).** One 50 ms slot for scheduler-jitter
  difference between in-process measurement and cross-network production,
  plus one 50 ms slot for measurement noise (timer resolution, goroutine
  scheduling skew).
- **Cap rationale (1 s).** `Config.ApplyStartJitter` validates `<= 10s`
  (`config.go:657`). The recommendation caps at 1 s — NOT because the
  knob is limited, but because a measured `worst_version_span > 1s`
  indicates a non-jitter root cause (slow apply pipeline, KV congestion,
  hook latency). Auto-recommending multi-second jitter would mask the
  underlying problem; cap the recommendation and tell the operator to
  investigate. If you measure a span > 1 s, the diagnostic logs a warning
  recommending investigation in addition to (not instead of) the capped
  value.

If `peak_concurrency <= 1` (no spatial burst observed), emit the formula's
floor (200 ms) and log "no spatial burst observed — recommendation is
formula floor only" — same shape as how the per-worker view's `50 ms`
is the formula floor when no temporal burst is observed.

### Constraints on the measurement run

For the recommendation to be meaningful:

- `cfg.ApplyStartJitter = 0` — the diagnostic targets this knob's tuning;
  running with it on muffles the signal. Set explicitly with a comment.
- `cfg.AssignmentWatcherDebounce = 0` — already the default but set
  explicitly with a comment, same discipline as the existing test.
- `cfg.KVBuckets.Replicas = 3` — same as the per-worker test, for
  realistic JetStream semantics.

---

## Design — the test

### Test shape decision

**Recommended: extend the existing test** (`TestApplyCoalescing_UnderReElectionBurst`)
rather than add a sibling test. Reasoning:

- Both views compute from the same `recordingBurstCollector` samples;
  duplicating cluster boot + 20-worker startup costs ~30 s per run for
  no measurement benefit.
- A single test with both AGGREGATE lines side-by-side makes the
  side-by-side comparison the consumer naturally wants.
- The `make herd-diagnostic` target already matches the existing test
  name; no Makefile change.

The test's leading comment must be updated to describe BOTH views and
WHY both are computed.

If review surfaces a reason to split (e.g. independent skip semantics
or radically different soak times), revisit.

### Multi-phase trigger sequence

Three phases in one run. Each emits its own AGGREGATE_FLEET line plus
an overall line:

| Phase | Trigger | Expected fleet event | Soak |
|---|---|---|---|
| P0 cold_start | `wc.StartWorkers(ctx)` (existing) | `numWorkers` workers apply V=1 within a startup window | 5 s post-Stable |
| P1 leader_kill | `mgr.Stop()` of Parti calculator-leader (existing) | `numWorkers - 1` workers apply V=N+1, V=N+2 in narrow span | 10 s post-new-leader-publish |
| P2 worker_add | `wc.AddWorkerWithOptions(ctx) x 2` + Start | `numWorkers + 1` workers apply re-balanced V | 5 s post-version-publish |

P0 and P1 are already in the landed test. P2 is new. **P0 is a baseline
observation phase, not a pass/fail gate** — all workers applying V=1
simultaneously during cold start is expected and not a "thundering herd"
in the operational sense. P0's `peak_concurrency` is logged for
context but not asserted against; only P1 and P2 are gated (see Acceptance
criteria).

Phase boundaries use **version-publication signals**, not elapsed time,
to avoid CI-load flakiness. `WaitForAssignmentVersion(minVersion, timeout)`
already exists at `internal/testutil/cluster_helpers.go:105` and polls
until all active workers have applied at least `minVersion`. Use it to
gate phase start when a new version is expected:

```
phaseBound{name, start, end}

P0.start = before StartWorkers
P0.end   = StartWorkers returns (all workers Stable) + 5 s settle

P1: prevVersion = mgrs[survivorIdx].CurrentAssignment().Version    // CAPTURE BEFORE Stop
    P1.start = time.Now()                                          // BEFORE leader.Stop()
    leader.Stop(ctx)                                               // may publish during drain
    wc.WaitForNewLeader(oldID, 30s)
    wc.WaitForAssignmentVersion(prevVersion+1, 30s)
P1.end   = above + 10 s settle

P2: prevVersion = mgrs[survivorIdx].CurrentAssignment().Version    // CAPTURE BEFORE AddWorker
    P2.start = time.Now()                                          // BEFORE first AddWorker.Start()
    extras = wc.AddWorkerWithOptions(...) x 2
    for each: extras.Start(ctx)
    wc.WaitForAssignmentVersion(prevVersion+1, 30s)
P2.end   = above + 5 s settle
```

The `prevVersion` capture and `Pn.start = time.Now()` MUST precede the
triggering action. If captured after `Stop()` or `Start()` returns,
the replacement leader may have already published during the drain;
`prevVersion` would then be the new version, and
`WaitForAssignmentVersion(prevVersion+1)` would wait for a SECOND
publish that isn't part of the burst under test. The acceptance gate
would then mis-attribute (or time out on) the actual burst.

A sample at `at` is attributed to phase `p` iff `p.start <= at < p.end`
(half-open interval). Samples on a phase boundary belong to the later
phase; samples outside all phase windows are included only in the
"overall" report.

**Clock semantics.** `RecordApplyAttempt` is called inline at
`manager_assignment.go:1020` with `time.Now()` at the moment `applyStoreMu`
is taken — before the updater goroutine spawns. Goroutine scheduling
jitter under CI load can add 1–10 ms of skew between this timestamp and
actual updater execution. That's well inside the 50 ms half-window, so
it doesn't cause false negatives; the plan notes the limit for posterity.

### Implementation outline

Additions to `apply_coalescing_test.go` — purely additive, no changes
to existing helpers:

```go
type phaseBound struct {
    name       string
    start, end time.Time
}

type versionFanout struct {
    WorkerCount int
    Span        time.Duration
    First, Last time.Time
    Workers     []string
}

type fleetReport struct {
    PerVersion       map[int64]versionFanout
    PeakConcurrency  int
    PeakAt           time.Time

    // WorstVersionSpan and WorstVersion are computed ONLY over
    // PerVersion entries with WorkerCount > 1. A single-worker
    // retry sample appearing alone as version V would produce a
    // zero-span fanout that would inflate nothing, but explicitly
    // excluding the single-worker case prevents future readers from
    // assuming the field includes all entries.
    WorstVersionSpan time.Duration
    WorstVersion     int64

    // MultiWorkerVersions = len(PerVersion entries where WorkerCount > 1).
    // Used by acceptance criteria; exposed in sample output as
    // versions_observed=N for diagnostic clarity.
    MultiWorkerVersions int
}

// analyzeFleetBursts merges all per-worker samples into a single
// sorted timeline and returns one fleetReport per phase plus an
// "overall" report.
func analyzeFleetBursts(
    collectors []*recordingBurstCollector,
    window time.Duration,
    phases []phaseBound,
) map[string]fleetReport

func recommendedApplyJitter(r fleetReport) time.Duration
```

The existing `recordingBurstCollector`, `burstSample`, and
`analyzeBursts` are untouched.

### Resource budget

- Worker count: 20 base + 2 added in P2 = 22. `IntegrationTestConfig`
  has `WorkerIDMax = 100` — plenty of headroom.
- Wall-clock: cluster boot ~1 s + StartWorkers ~15 s + P0 soak 5 s +
  leader-detection ~1 s + leader stop ~5 s + WaitForNewLeader ~15 s +
  P1 soak 10 s + P2 add + start ~10 s + P2 soak 5 s + analysis ~1 s
  ≈ 70 s. Set test timeout to 240 s.

---

## Acceptance criteria

Run from the worktree root, in this order:

1. `go build ./...` — clean.
2. `make lint` — clean.
3. `go test -count=1 -race -run TestStartEmbeddedNATSCluster -v ./partitest/`
   — passes (no regression in cluster helper).
4. `PARTI_RUN_HERD_DIAGNOSTIC=1 go test -count=1 -timeout 240s -run TestApplyCoalescing_UnderReElectionBurst -v ./test/integration/manager/`
   — passes AND:
   - the existing AGGREGATE per-worker banner is emitted unchanged (still
     `max_burst_size=1`-ish under the same trigger);
   - the new AGGREGATE_FLEET banner is emitted for each phase and overall;
   - **P0 cold_start is observation-only**, logged but not asserted
     against (all workers applying V=1 simultaneously during startup is
     expected behavior, not a herd in the operational sense);
   - **P1 leader_kill**: define `expectedP1Workers = numWorkers - 1`
     (the killed leader is no longer applying). Assert
     `P1.PeakConcurrency >= expectedP1Workers - 2` (View B's
     cross-version-pressure metric — most surviving workers fired
     `RecordApplyAttempt` within the same 100 ms pressure window),
     AND `len(P1.PerVersion entries with WorkerCount > 1) >= 1` (View A —
     at least one *same-version* fleet event observed during P1);
   - **P2 worker_add**: define `expectedP2Workers = expectedP1Workers + 2`
     (we added 2 workers; the killed leader stays dead). Assert
     `P2.PeakConcurrency >= expectedP2Workers - 2` (same View B shape)
     AND `len(P2.PerVersion entries with WorkerCount > 1) >= 1` (same
     View A shape);
   - **overall**: `recommended_apply_jitter` is a finite duration > 0.
     If the pre-cap formula would have exceeded 1 s
     (i.e. `ceil(worst_version_span / 50ms) * 50ms + 100ms > 1s`,
     which is `worst_version_span > 900ms`), the diagnostic logs a
     "non-jitter root cause suspected; investigate" warning and clamps
     to 1 s. The clamp is not a test failure — the cap is intentional
     per the recommendation formula's cap rationale.
5. `make herd-diagnostic` — runs the same test and emits both banners.

If P1 or P2 fails its `peak_concurrency` threshold, STOP and report. Do
not commit a diagnostic that does not measure what it claims. Same
discipline as the prior round.

Debug levers if 4 fails:
- Log the merged sample timeline for the failing phase to confirm
  samples are being attributed correctly (timestamp bucketing bugs).
- Confirm `ApplyStartJitter=0` actually threaded through (debug-log
  the config the test uses).
- Confirm `WaitForAssignmentVersion` returned successfully — if it
  timed out, the new version was never published and there is no burst
  to measure (that's a different bug, in the trigger).
- Confirm `cfg.RebalanceCooldown` is not so long that P2's add doesn't
  produce a new version within the timeout.

---

## Known limitations

These do not block implementation but must be documented in the test's
leading comment so future readers don't mistake them for bugs:

1. **Retry applies are included in the metric by design.**
   `RecordApplyAttempt` is called from `applyAssignmentWithPrevCore`
   (`manager_assignment.go:1020`), which is the common entry point for
   BOTH fresh applies (with jitter) and retries (`applyAssignmentWithPrevSkipJitter`
   at `manager_assignment.go:985`, no jitter). The current
   `MetricsCollector` API (`types/metrics_collector.go:67`) has signature
   `RecordApplyAttempt(workerID string, version int64)` — no attempt
   number — so the test cannot filter retries from the sample set. This
   means `peak_concurrency` measures **total apply pipeline pressure**,
   which is the operationally-meaningful quantity (each sample takes
   `applyStoreMu` regardless of attempt origin) but does include a
   component that `ApplyStartJitter` cannot smooth. The diagnostic does
   not separate fresh-apply samples from retry-apply samples — doing so
   would require adding an attempt-number argument to
   `MetricsCollector.RecordApplyAttempt`, which is out of scope per
   this plan. Operators reading the recommendation should treat a high
   `peak_concurrency` as necessary but not sufficient evidence for
   tuning `ApplyStartJitter` in isolation; if a substantial fraction
   of the samples come from the retry path, the operationally-correct
   response is reducing retry pressure (which knobs achieve that is
   not asserted here and should be investigated against the current
   manager retry implementation), not increasing jitter.

2. **In-process clock skew (1–10 ms under CI load) is inside the
   `idleGap`.** Negligible for the metric but documented for posterity.

3. **The diagnostic measures one cluster topology (3-node R=3, 20
   workers).** A real production cluster may produce different numbers.
   Operators are encouraged to re-run the diagnostic against a copy of
   their production NATS topology to derive their own recommendation —
   the `make herd-diagnostic` target plus environment-variable overrides
   (out of this plan's scope; a future improvement) would make this
   straightforward.

---

## Implementation commits

Single commit on `worktree-herd-diagnostic-cluster`:

```
test(integration): fleet-wide spatial burst diagnostic
```

Commit message body must:
- Cite the per-worker view's measured-axis limitation as motivation;
- State that `ApplyStartJitter` and `AssignmentWatcherDebounce` are set
  to 0 in the test and WHY (measure unmuffled signal);
- Describe the AGGREGATE_FLEET line format and what `peak_concurrency`,
  `worst_version_span`, and `recommended_apply_jitter` mean;
- Document the retry-path inclusion limitation (Known Limitations §1);
- Note that the per-worker view is preserved unchanged.

No documentation updates, no godoc updates. The diagnostic's output is
self-describing via the test's leading comment block.

---

## Review trail

Append findings from `/codex:rescue` reviews to a new
`fleet-burst-diagnostic-review-trail.md` in this directory.
