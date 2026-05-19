# Simulation — Phase 4: CI Coverage Expansion

Single-PR plan to widen what the parti simulation actually exercises **in
CI**, addressing audit items **H3** (one-config CI), **H4** (network_disconnect
targets random worker, not leader), and the processing-gate-on / RoundRobin
gaps called out under coverage_gaps §3 (Processing Gate) and §8 (Strategy
coverage).

## Why

Phase 1–3 made the oracle correct, the code CLAUDE.md-compliant, and the
ownership-violation invariant verifiable. But CI today runs **one** YAML
(`chaos_comprehensive.yaml`) with:

- `network_disconnect` commented out (`chaos_comprehensive.yaml:38`)
- No `enforce_exclusive_consumption` (defaults to false — Processing Gate
  is OFF in CI)
- `WeightedConsistentHash` only (the other two strategies have **zero**
  CI coverage)

The audit's strongest claim was that this combination "lets every chaos
primitive other than worker_crash / leader_failure / scale_up / scale_down /
producer_crash / worker_pause / slow_consumer go untested in CI." That's
what Phase 4 fixes.

The new `OwnershipViolationCount > 0` invariant from Phase 3 will start
firing in CI **only** when the gate is actually enabled — making Phase 4 the
phase that turns Phase 3's oracle into a real CI signal.

## Out of scope

- **Abrupt-kill semantics** (H5): all chaos events are still `cancel()`-based
  in all-in-one mode. Real SIGKILL behavior (where the manager doesn't get
  to release its claim) needs a different worker-control surface. Separate
  PR.
- **Heartbeat-watcher invariants** (H2): Phase 5.
- **Checkpoint completeness** (H7): Phase 5.
- **Removing the dead `Late/Lost/Failures` audit-metric read paths** in
  `cmd/simulation/main.go:659-661`: cosmetic; out of scope here.
- **Stop-on-failure tightening** (R11): out of scope; CI uses
  `--stop-on-failure=false` so this doesn't bite today.

## Changes

### 1. Enable `network_disconnect` in the primary CI config

**File:** `test/simulation/configs/chaos_comprehensive.yaml`

Uncomment the existing line:
```yaml
# - network_disconnect
```
→
```yaml
- network_disconnect
- network_disconnect_leader   # NEW — see §3 below
```

Both random-target and leader-target variants will fire under the existing
15s–25s chaos interval. Combined chaos exposure stays the same scale
(events of varying intensity), but coverage now includes network-isolation
recovery.

### 2. Processing-Gate CI coverage (DEFERRED to Phase 5)

**Original intent:** turn on the Processing Gate in
`chaos_comprehensive.yaml` so Phase 3's `OwnershipViolationCount > 0`
invariant has actual CI signal.

**Empirical finding during gate-4 verification:** enabling
`enforce_exclusive_consumption: true` in the 8-minute comprehensive
chaos workload produced two distinct failure modes across local 8m runs:

1. **Run 1**: total Sent==Received (59887/59887), no message loss, but
   tracker over-escalated 64 holes to gaps at shutdown — a tracker
   accounting drift (FN4 in the original audit) amplified by gate-on
   NAK retries.
2. **Run 2**: catastrophic — only 34,034 of 58,437 messages delivered
   (40% loss). Gate-driven NAK loops under aggressive 15-25s chaos
   prevented drain.

A lighter focused config (`chaos_gate.yaml`, 2m duration, moderate
20-30s chaos interval) ran cleanly on message-delivery — but consistently
surfaced a **real ownership violation**: `partition=19 seq=153 processed
by both worker-5 and worker-0` after a graceful restart + network
disconnect. Phase 3's invariant did its job and caught a previously
undetected cross-worker collision pattern.

**This finding is what Phase 3 was built to surface, and it needs
Phase 5 investigation.** Two hypotheses:
- A real parti exclusivity-contract bug (Processing Gate failing under
  the specific handoff race that combines a graceful worker restart with
  a subsequent network disconnect).
- A simulation-oracle gap: the chaos worker-restart handler may allocate
  a fresh worker ID for the replacement instance, making JetStream
  redelivery to that new ID look like a cross-worker collision even
  though parti's stable-worker-ID model would treat them as the same
  logical worker.

Either way, the right Phase 4 outcome is **don't ship a CI job that
fails on every run while the root cause is unknown**.

**Phase 4 action:**
- Keep `chaos_gate.yaml` in the repo as a developer-run config with a
  header explaining the gate-on situation and how to reproduce.
- Do NOT add a CI job that runs it.
- Document the gate-on finding as Phase 5's first investigation item.

### 3. Add a leader-targeted `network_disconnect_leader` chaos event

**Files:**
- `test/simulation/internal/coordinator/chaos.go` — register the new event type
- `test/simulation/cmd/simulation/main.go` — dispatch handler (mirrors
  `LeaderFailureEvent`'s leader-selection pattern)

**Why a new event type rather than a `target:` parameter on the existing
one:** the chaos YAML format is event-name driven (no per-event params in
the config). Adding `network_disconnect_leader` is the smallest API
extension and keeps configs readable.

**Required registration sites** (round-1 reviewer correction —
`generateEventParams` and `String()` were missed in the original plan):

| File:Symbol | Why |
|---|---|
| `coordinator/chaos.go` — `ChaosEvent` constants | Defines the new typed name. |
| `coordinator/chaos.go` — `generateEventParams` switch (~line 177) | The existing `NetworkDisconnectEvent` arm produces a randomized 5–30s duration; without an entry for the leader variant, scheduled events arrive with empty params and the dispatcher's fallback (10s fixed) doesn't mirror the existing behavior. |
| `coordinator/chaos.go` — `String()` switch (~line 307) | Without an entry, logs use the "Unknown Event" path. |
| `cmd/simulation/main.go` — three switch case statements (~lines 947, 1040, 1141) | The process-mode fallback, the goroutine guard, and the goroutine dispatcher. The new variant is **all-in-one only** (see "Process-mode behavior" note below); the process-mode fallback should log+skip. |

Chaos config storage is plain `[]string` (`config.go:176`) cast to
`ChaosEvent` (`chaos.go:82`), so there is no typed YAML parser to update.

**Implementation sketch** (in `main.go`, alongside the existing
`NetworkDisconnectEvent` case at line 1141):

```go
case coordinator.NetworkDisconnectLeaderEvent:
    workers := registry.GetByType(coordinator.WorkerGoroutine)
    var leader *coordinator.GoroutineInfo
    for _, w := range workers {
        if wobj, ok := w.Obj.(*worker.Worker); ok && wobj.IsLeader() {
            leader = w
            break
        }
    }
    if leader == nil {
        log.Println("[Chaos] No leader worker found to disconnect")
        return
    }
    dur, ok := params["duration"].(time.Duration)
    if !ok || dur <= 0 {
        dur = 10 * time.Second
    }
    if wobj, ok := leader.Obj.(*worker.Worker); ok {
        log.Printf("[Chaos] Disconnecting LEADER worker %s for %v", leader.ID, dur)
        wobj.Disconnect()
        time.AfterFunc(dur, func() {
            log.Printf("[Chaos] Reconnecting LEADER worker %s", leader.ID)
            wobj.Reconnect()
        })
    }
```

The chaos `events:` list now accepts `network_disconnect_leader` as a
keyword. The scheduler treats it as a separate event with its own
interval slot (sharing the global chaos interval — no new tuning needed).

**Process-mode behavior:** the new event is **all-in-one only**.
Process-mode's existing `leader_failure` handler at
`main.go:1598-1602` is not a real leader lookup (it kills the first
running worker process). Rather than approximate leader-disconnect via
the same approximation, the process-mode case statement for
`NetworkDisconnectLeaderEvent` will log a "process mode does not
support leader-disconnect; skipping" message and return. All Phase 4
CI configs use `mode: all-in-one`, so the skip is invisible in CI.

**Unit test** (round-1 reviewer P1.3 — events need a deterministic
proof, not just probabilistic CI exposure):

Add `test/simulation/internal/coordinator/chaos_test.go` (or extend
existing) — `TestNetworkDisconnectLeaderEvent_ParamsAndStringer`:
construct a `ChaosController`, generate params for
`NetworkDisconnectLeaderEvent` via the same path the scheduler uses
(`generateEventParams`), assert (a) the duration param is in the
expected range, (b) `String()` returns the human-readable name (not
"Unknown Event"), (c) the event is in the global event-list constant.

This guards against the registration sites becoming stale on future
refactors.

**Out-of-scope variants:**
- `worker_crash_leader` (could mirror this pattern) — not done; the
  existing `leader_failure` covers cancel-based crashes.
- `worker_pause_leader` — could be useful for slow-leader scenarios, but
  pausing the leader without freezing its heartbeat is the same
  "Slow Neighbor" gap the audit also flagged. Out of scope.

### 4. Add a CI job for the `chaos_network_disconnect.yaml` config

**File:** `.github/workflows/simulation-stress.yml`

The current workflow has one job (`chaos-simulation`) running
`chaos_comprehensive.yaml`. Add a sibling job that runs the existing
`chaos_network_disconnect.yaml`.

**Config update (round-1 reviewer P1.3 — 30s without cooldown is
probabilistic and may not exercise the leader-disconnect arm at all):**
extend the focused config to 90s duration with a 15s cooldown. Chaos
fires every 5–10s, so over 90s the scheduler injects ~10 events; with
two configured event names the leader-disconnect arm should fire at
least once with high probability. The 15s cooldown gives the last
disconnect time to reconnect and the workers time to drain before the
final report. Also set `coordinator.gap_aging: 60s` explicitly (current
config relies on the 45s default — too aggressive given the chaos
interval).

```yaml
jobs:
  chaos-simulation:
    # existing job
    ...

  network-disconnect-simulation:
    runs-on: ubuntu-latest
    timeout-minutes: 10
    steps:
      - name: Checkout
        uses: actions/checkout@v6
      - name: Setup Go
        uses: actions/setup-go@v6
        with:
          go-version-file: go.mod
      - name: Build Simulation
        run: go build -o tmp/simulation ./test/simulation/cmd/simulation
      - name: Run Network-Disconnect Simulation
        run: |
          set +e
          max_attempts=3
          for attempt in $(seq 1 $max_attempts); do
            echo "::group::Attempt $attempt/$max_attempts"
            ./tmp/simulation \
              --config test/simulation/configs/chaos_network_disconnect.yaml \
              --duration 90s --cooldown 15s \
              --stop-on-failure=false \
              2>&1 | tee "tmp/network-attempt${attempt}.log"
            rc=${PIPESTATUS[0]}
            echo "::endgroup::"
            if [ $rc -eq 0 ]; then exit 0; fi
          done
          exit 1
      - name: Upload Failure Report
        if: failure()
        uses: actions/upload-artifact@v4
        with:
          name: network-disconnect-failure-report
          path: |
            failure_report.json
            tmp/network-attempt*.log
```

This job runs ~1.5–2 minutes per attempt (90s simulation + 15s cooldown
+ a few seconds of setup); 10-min timeout gives 3-retry headroom matching
the existing job's pattern. CLI flags `--duration 90s` and `--cooldown 15s`
override the YAML defaults to keep the exact verification contract in one
place (in step with the comprehensive job's `--duration 8m --cooldown 90s`
override pattern).

The existing `chaos_network_disconnect.yaml` currently picks **random**
workers (uses the existing `network_disconnect` event). Update that
config to also include `network_disconnect_leader` so the new event is
exercised end-to-end.

### 5. Add a `RoundRobin` strategy CI job

**Files:**
- `test/simulation/configs/chaos_roundrobin.yaml` (new)
- `.github/workflows/simulation-stress.yml` (new job)

`RoundRobin` is implemented (`worker.go:185-186`) but has zero config
coverage. Add a focused config that:
- Same partition/worker count as `chaos_comprehensive.yaml`
- `assignment_strategy: RoundRobin`
- Same chaos events as comprehensive (to surface strategy-specific bugs
  under chaos)
- 90-second duration (shorter than comprehensive's 8m to bound CI cost)

**Drain/gap-aging contract (round-1 reviewer P2.1):** RoundRobin
intentionally does not preserve cache affinity on rebalance
(`strategy/round_robin.go:14-16`), so chaos churn produces more
reassignment than WeightedConsistentHash. Don't copy comprehensive's
`gap_aging: 240s` blindly (way too long for a 90s run — would never
escalate) and don't use the 45s default (too aggressive under the
larger reassignment volume). Set explicitly:

```yaml
simulation:
  duration: 90s
coordinator:
  gap_aging: 60s
  # final cooldown so the post-chaos drain has time to fill holes
  # the simulation framework reads --cooldown from CLI; the CI job
  # passes 20s.
chaos:
  enabled: true
  interval: "10s-15s"   # roughly 6–9 events over 90s
```

CI invocation passes `--cooldown 20s` (matching the comprehensive job's
pattern but scaled to the shorter run).

Add a sibling CI job mirroring §4's pattern. Total CI added: ~3–5 minutes
per PR.

## Risks and verification

**Risk 1 (gate-on CI flakiness):** Enabling `enforce_exclusive_consumption`
in `chaos_comprehensive.yaml` adds NAK retries under handoff. The audit's
own e2e validation (Phase 3 prep, 30s with this config) showed clean
runs, but production stress at 50 partitions × 5 msg/sec × 8m may behave
differently. **Mitigation:** the existing CI workflow already retries 3
times. If flakiness appears, the warmup duration (10s) and `gap_aging`
(240s) give large headroom. A pre-merge dry run of the updated config in
the worktree (locally) will catch obvious regressions.

**Risk 2 (new chaos event missing from a typed switch):** Adding
`NetworkDisconnectLeaderEvent` requires updating all four current
`chaos.go` typed surfaces plus the three `main.go` case statements —
see the table in §3 above. There is no typed YAML parser (events are
stored as `[]string` and cast to `ChaosEvent` at the boundary), so
the plan does **not** include a parser edit. The implementation will
grep for `NetworkDisconnectEvent` and ensure each occurrence has a
matching new-variant entry.

**Risk 3 (cold-start window vs gate warmup):** The default `ColdStartWindow`
in the simulation is 10s (`worker.go:215`). The gate warmup is also 10s
in the new config. These run independently — warmup is per-worker, cold
start is global. No coupling concern.

**Verification gates** (all must pass):
1. `make lint` clean.
2. `make test` clean.
3. `go test ./test/simulation/...` clean (includes the new
   `TestNetworkDisconnectLeaderEvent_ParamsAndStringer`).
4. **Full 8-minute gate-on dry run** of `chaos_comprehensive.yaml` —
   matching the exact CI invocation:
   ```bash
   ./bin/simulation \
     --config test/simulation/configs/chaos_comprehensive.yaml \
     --duration 8m --cooldown 90s --stop-on-failure=false
   ```
   No ownership violations, no gaps, stability invariants pass. This is
   the merge gate that the 30s authoring run could not provide.
   Round-1 reviewer P1.2 explicitly required matching the CI 8m+cooldown
   shape — accepted as a hard prerequisite.
5. Local 90-second dry run of `chaos_network_disconnect.yaml` (new shape)
   — both `network_disconnect` and `network_disconnect_leader` events
   observed in the log, no gaps, no violations.
6. Local 90-second dry run of `chaos_roundrobin.yaml` — no gaps, no
   violations.

## Implementation order

1. Define `NetworkDisconnectLeaderEvent` constant in `chaos.go`.
2. Add the new event to **all four** `chaos.go` surfaces: constant,
   `generateEventParams` switch (duration 5–30s, same as random variant),
   `String()` switch (human-readable name), and the global event-list.
3. Wire dispatcher in `main.go` (3 case statements): goroutine
   dispatcher (~line 1141) implements the disconnect; goroutine guard
   (~line 1040) includes the new event; process-mode fallback
   (~line 947) logs+skips.
4. Add `TestNetworkDisconnectLeaderEvent_ParamsAndStringer` unit test.
5. Update `chaos_comprehensive.yaml`: enable gate + add both
   network_disconnect events.
6. Update `chaos_network_disconnect.yaml`: extend duration to 90s,
   set `gap_aging: 60s`, include the new leader variant.
7. Create `chaos_roundrobin.yaml`: per §5 contract above.
8. Update `.github/workflows/simulation-stress.yml`: 2 new jobs
   (network-disconnect, roundrobin), each with the build step inline
   (matrix would conflate failure attribution).
9. `make lint` + `make test` (gates 1–3).
10. **Full 8m gate-on local dry run** (gate 4 — the merge prerequisite).
11. Local dry runs for §4 and §5 configs (gates 5–6).

## Commit message (draft)

```
ci(simulation): expand CI coverage — network disconnect, gate-on, RoundRobin

Closes audit gaps H3 (single-config CI), H4 (network_disconnect targets
random instead of leader), coverage_gaps §3 (Processing Gate gate-off in
CI), and §8 (RoundRobin has zero CI coverage).

- chaos_comprehensive.yaml (the existing CI config):
  - Enable network_disconnect (previously commented out).
  - Add new network_disconnect_leader event (see below).
  - Turn on enforce_exclusive_consumption with allowed_states
    ["commit", "stable"] — the production-recommended setting and the
    only way Phase 3's OwnershipViolationCount > 0 invariant produces
    real CI signal.

- New chaos event NetworkDisconnectLeaderEvent: mirrors LeaderFailureEvent's
  leader-selection logic but calls wobj.Disconnect() instead of Cancel(),
  exercising the "split brain" scenario the audit's DESIGN_REVIEW.md called
  for. Registered in coordinator/chaos.go and dispatched in main.go.

- chaos_network_disconnect.yaml: add the new leader-targeted variant so
  this focused config exercises both random and leader disconnect.

- chaos_roundrobin.yaml (new): RoundRobin strategy + the comprehensive
  chaos workload at 90s duration.

- .github/workflows/simulation-stress.yml: two new jobs running the
  network-disconnect and RoundRobin configs alongside the existing
  comprehensive job. Each gets the same 3-retry pattern. Total CI time
  budget ~+5 minutes per PR.

Plan: docs/plans/sim-oracle-phase4/00-plan.md
```
