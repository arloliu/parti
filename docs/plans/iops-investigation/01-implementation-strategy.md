# 01 — Implementation Strategy

Companion to `00-attribution-plan.md`. Lays out the phased build, the
suggested **model + effort** per phase, and the review checkpoints
between phases.

The phases are independent enough that a single engineer with one
model session per phase can carry the work through. None of the
phases assumes the next one is done. Phase boundaries are also the
natural review checkpoints.

## Model + effort guide

The plan only suggests defaults. Override when the task tells you to —
a stuck Sonnet run can be promoted; an obvious template-fill on Opus
xhigh is wasteful.

| Model | Best for | Avoid for |
|---|---|---|
| Sonnet 4.6 (medium) | Boilerplate, well-defined templates, file shuffling, mechanical edits across many files | Anything where the *shape* of the deliverable is still being decided |
| Opus 4.7 (medium) | API integration with a clear contract, mid-size implementation tasks, well-scoped diagnostics | Long surveys, multi-step research, broad refactors |
| Opus 4.7 (high) | Wrapper / shim design where coverage matters, statistical glue code where correctness is load-bearing, anything that has to interact correctly with parti's invariants | Boilerplate (wasteful) |
| Opus 4.7 (xhigh / Fast) | Interpretation of measurement results; trade-off decisions; deciding "is this finding load-bearing?"; failures-analysis when the data and the hypothesis disagree | Routine implementation |
| Copilot `gpt-5.5` `xhigh` (via /post-impl-review, /plan-review, /final-plan-review) | Reviewing finished implementation against a written spec; surfacing inconsistencies between sections of a plan | Implementation (it's a reviewer, not an author) |

Use `/fast` for Opus 4.7 when iterating — same model, faster output.
Don't use it for Sonnet.

## Orchestrator model

The table above is for the *worker* doing each phase. The
**orchestrator** — the conversation that decides which phase to run
next, dispatches subagents, evaluates results, and synthesises across
phases — has a different cost profile, mostly judgment over context
rather than implementation.

| Situation | Model + effort |
|---|---|
| Default driving — routing, dispatching subagents, summarising results | **Opus 4.7 (medium)**, with `/fast` toggled on for iteration speed |
| Phase boundary review — "is this actually ready for the next phase?" (end of phases 2, 3, 4) | Opus 4.7 (**high**) |
| Phase 6 findings synthesis — "the data and the hypothesis disagree; which signal do I trust?" | Opus 4.7 (**xhigh**); matches the worker recommendation for that phase so the orchestrator can actually evaluate the worker's output |
| Unexpected result from a subagent that breaks the plan | Bump to **high** for one turn while deciding whether to revise the plan or push through |

Why not Sonnet for the orchestrator: across the six phases the
orchestrator holds the plan, the implementation strategy, the 8-round
plan-review history, and whatever the latest subagent dropped on you.
That's the natural home for Opus 4.7 1M-context; auto-compression
absorbs the bulk and the orchestrator only needs to summon
high-effort reasoning at decision points.

Why not Copilot `gpt-5.5 xhigh` for the orchestrator: it's a one-shot
reviewer model, not a multi-turn agent. It can't dispatch subagents
or persist context across turns. Reserve it for `/plan-review`,
`/post-impl-review`, and `/final-plan-review` — exactly where it's
already used in this strategy.

## Phase 1 — Rig bring-up

**Goal:** a docker-compose stack the harness can connect to, with
NATS image/version overridable via env vars, fresh-volume hygiene
between runs, and the cluster reachable from the harness binary.

**Tasks:**

1. `test/perf-measurement/docker/docker-compose.yaml` — 3-node and
   5-node variants (override via `PERF_RIG_NATS_REPLICAS`),
   `image: ${PERF_RIG_NATS_IMAGE:-nats:2.12.6}`, JetStream enabled,
   named volumes per node, cluster routes. Mirrors the
   `test/simulation/docker/` convention.
2. `test/perf-measurement/docker/nats-server.conf` — minimal JS
   configuration matching the user's prod where known
   (`jetstream { store_dir: /data }`, cluster section, sizing).
3. `test/perf-measurement/Makefile` — `make up`, `make down`,
   `make reset` (= `down -v && up`), and `make image-digest` (records
   `docker image inspect` output for the manifest).
4. `test/perf-measurement/.gitignore` — `results/`.
5. Document the env-var contract in
   `test/perf-measurement/README.md`.

**Suggested model + effort:** **Sonnet 4.6 (medium)**. Pure templating
+ compose. Promote to Opus only if the user's prod has unusual NATS
config (TLS, leafnodes, MQTT) that requires judgment to mirror.

**Done means:** `make reset && nats stream ls --server localhost:4222`
returns 0 streams against a fresh 3-node cluster; volumes are clearly
mapped on disk; image override via env var works.

**Review:** quick visual check; no /post-impl-review needed for
boilerplate of this size.

## Phase 2 — Instrumented harness

**Goal:** a single Go binary that runs the workload, wraps every
JetStream / KeyValue call with a counter, verifies storage classes,
and exposes flags for every independent variable in §M1.

**Tasks:**

1. `test/perf-measurement/internal/instrumentedjs/` — wrapper for
   `jetstream.JetStream` and `jetstream.KeyValue` with per-`(bucket,
   op)` counters. **Load-bearing for the entire attribution story.**
2. `test/perf-measurement/internal/storageverify/` — given the
   manifest's expected storage class for each stream, calls
   `stream info` and asserts.
3. `test/perf-measurement/cmd/harness/main.go` — workload binary with
   the flags in §R2, hygiene checks from §R4 (Stable / not Degraded
   before capture), and a clean shutdown.
4. `test/perf-measurement/go.mod` — separate module so the rig can
   pin its own parti version (`v2.3.0` for the main matrix, HEAD for
   M1.11). Co-locates harness, calibrate, aggregate binaries under one
   module.

**Suggested model + effort:**

- 2a (wrapper): **Opus 4.7 (high)**. Coverage matters — missing a
  method on the wrapper means missing attribution. The wrapper has to
  implement every method on `jetstream.JetStream` that parti uses, and
  every method on `jetstream.KeyValue`. Get this right once.
- 2b (storage verifier): **Sonnet 4.6 (medium)**. Small, well-defined.
- 2c (main binary): **Opus 4.7 (medium)**. Knob plumbing + lifecycle.
  Worth Opus because the lifecycle (Manager.Start → wait Stable →
  measure → Stop) interacts with parti's state machine.

**Done means:** harness starts a 5-worker cluster against the
compose-managed NATS, partitions converge, every worker reports
Stable, counters tick visibly.

**Review:** **/post-impl-review** with Copilot gpt-5.5 xhigh once the
wrapper + main binary land. The wrapper is the kind of code where a
missed method silently zeroes a column in the budget.

## Phase 3 — Capture pipeline

**Goal:** per-run output that aggregates into a single CSV the
analysis can read directly, sourced from cgroup, iostat,
node_exporter, jsz, and the harness counters.

**Tasks:**

1. `test/perf-measurement/scripts/capture-cgroup-io.sh` — 1 Hz poller
   reading `/sys/fs/cgroup/system.slice/docker-<id>.scope/io.stat` for
   each NATS container, diffing between samples.
2. `test/perf-measurement/scripts/capture-iostat.sh` — secondary
   host-level `iostat -x -d -t 1`.
3. `test/perf-measurement/scripts/capture-jsz.sh` — `curl :8222/jsz`
   and `:8222/varz` polled at 5 s.
4. `test/perf-measurement/scripts/prometheus-node-exporter.yaml` —
   drop-in compose service exporting host-level disk metrics.
5. `test/perf-measurement/cmd/aggregate/main.go` — reads all four
   sources + harness counters, reconciles them into one per-run CSV
   with the columns §R3 specifies. Aborts loudly if cgroup-totals and
   iostat disagree by > 5 %. Go for parity with the rest of the rig.
6. `test/perf-measurement/scripts/run-matrix.sh` — drives M1.0–M1.11
   according to a pre-registered random schedule (R5), with seed
   recorded.

**Suggested model + effort:**

- 3a–3d (capture scripts): **Sonnet 4.6 (medium)**. Shell + cron-loop
  shapes. The "aborts loudly on cgroup vs iostat divergence" is one
  line of arithmetic.
- 3e (`cmd/aggregate/main.go`): **Opus 4.7 (high)**. This is where the
  four-column budget is computed; getting per-run reconciliation right
  is what unlocks attribution.
- 3f (run-matrix.sh): **Sonnet 4.6 (medium)**. Bash for-loop with a
  seeded random shuffle.

**Done means:** a smoke run (n=1, N=100, B-lib) produces a CSV
identical in shape to what the matrix expects; cgroup totals and
iostat are within 5 %.

**Review:** **/post-impl-review** focused on `aggregate.py` and the
random-schedule generation. Counter wiring + reconciliation is where
the analysis can silently go wrong.

## Phase 4 — Calibration (M4 + M4.1)

**Goal:** the calibration tables M3 depends on. M4 is the basic
KV-paths table; M4.1 is the 48-grid idle-pull table.

**Tasks:**

1. `test/perf-measurement/cmd/calibrate/main.go` — calibration driver
   binary. Sub-commands: `kv-put`, `kv-get`, `kv-keys`, `kv-watch`,
   `kv-put-mem`, `idle-pull`.
2. The `idle-pull` sub-command takes flags `--c-stream`,
   `--fetch-timeout`, `--data-storage`, `--replicas`. Pre-creates a
   data stream with the specified storage + R, creates `C_stream`
   durable pull consumers, runs them at idle for 60 s, captures
   per-node iostat/cgroup output, classifies node-role from
   `stream info`. Discards captures where leadership moves.
3. `test/perf-measurement/scripts/run-m4.sh` — sweeps the grid,
   writes results to
   `test/perf-measurement/results/m4/m4_calibration.csv`.

**Suggested model + effort:**

- 4a (calibration driver, basic KV paths): **Sonnet 4.6 (medium)**.
  Mostly drives the `nats` CLI or thin Go equivalents.
- 4b (M4.1 idle-pull driver): **Opus 4.7 (medium)**. Stream creation
  + leader observation + capture coordination has more moving parts.
- 4c (grid sweep script): **Sonnet 4.6 (medium)**.

**Done means:** `m4_calibration.csv` exists and has the 48 idle-pull
rows + the basic KV-paths rows. M3 can interpolate at any grid point
without manual intervention.

**Review:** **/post-impl-review** on the M4.1 idle-pull driver only.
The driver's correctness defines the H2 attribution; the basic KV
paths are simpler.

## Phase 5 — Main matrix execution

**Goal:** all 190 runs of §M1, captured.

**Tasks:**

1. Validate the rig: run M1.0 (no-Parti control) end-to-end, derive
   MDE, confirm `MDE_slope < 0.167 RPC/s/partition`. If not, raise n or
   move to a quieter host (R5).
2. Run M1.1 through M1.11 according to the random schedule.
3. Apply the pre-registered Tukey-fence outlier rule (R5). Re-runs
   are inserted at new random positions.

**Suggested model + effort:** **Sonnet 4.6 (medium)** as long as
everything works. If a run consistently fails (Manager won't reach
Stable, storage class mismatch, leader churn), promote to **Opus 4.7
(high)** for the diagnosis pass; the rig is now interacting with
parti's real state machine and the right call may be non-obvious.

**Done means:** `test/perf-measurement/results/run-NNN-<label>/aggregated.csv`
exists for all 190 runs.

**Review:** none per-run. Audit at the end: a spot-check pass over a
random 10 % of `manifest.yaml` files to confirm the recorded config
matches the run label.

## Phase 6 — Analysis and findings

**Goal:** `docs/plans/iops-investigation/findings.md` records the per-subsystem IOPS budget,
confirmed and falsified hypotheses, and the recommended operator
actions.

**Tasks:**

1. `test/perf-measurement/scripts/analyze.py` — runs the OLS fits per
   §M2 across all configs, produces a slope/intercept/CI table per
   column. Python here (pandas / statsmodels) — the stats glue is more
   ergonomic in Python than Go; this is the only Python in the rig.
2. Per-config attribution: compute `residual_<column>` per N, check
   for residual that grows with N.
3. Decide on each hypothesis: confirmed / falsified / inconclusive.
4. Write `findings.md`. Include: per-subsystem IOPS budget for the
   user's reported topology, table of verified mitigations with
   measured slope reductions, raw-data links.
5. If H1 is confirmed and the operator cost of `SweepInterval = 5 min`
   is acceptable, recommend that as the immediate mitigation; if a
   code change is justified, open a follow-up plan (`02-followup-…`)
   rather than expanding scope here.

**Suggested model + effort:** **Opus 4.7 (xhigh)** for the
interpretation pass. This is the part of the work where the data and
the hypothesis disagree about half the time, and the right call is
"which signal do I trust" — model-effort matters most here.

**Done means:** `findings.md` is written, linked from the README, and
points at the raw data. If the user reads only this one file they
should leave with: which knob to turn first, why, and how much it
will save.

**Review:** **/plan-review** on `findings.md` if a code-change
follow-up is being proposed; otherwise the findings document stands
on its own.

## Pre-execution checkpoint

Before Phase 5 starts (i.e. after Phases 1–4 are merge-clean) and
before any real runs, do a final dry run:

- Spin up the rig.
- Run M1.0 once at N=2000, n=1, and confirm the aggregator produces
  a CSV in the expected shape.
- Confirm cgroup vs iostat totals match within 5 %.
- Confirm the harness's wrapper counters sum to a sensible total
  (heartbeat bucket → ~1 Put/s with 5 workers, etc.) — sanity-check
  against the H3 floor predictions.

Only after that proceeds the 70-hour matrix run.

## Review cadence summary

| Phase | Reviewer | Trigger |
|---|---|---|
| 1 | self | visual check |
| 2 | /post-impl-review | wrapper + main binary land |
| 3 | /post-impl-review | aggregate.py + run-matrix.sh land |
| 4 | /post-impl-review | M4.1 idle-pull driver lands |
| 5 | self | end-of-phase spot-check |
| 6 | /plan-review | if a follow-up code-change plan is proposed |

`/plan-review` and `/post-impl-review` both dispatch Copilot gpt-5.5
xhigh. They cost real tokens and 2–8 minutes each; don't dispatch
speculatively.

Independent secondary review (e.g. Gemini 3.1 Pro on the final
findings) is optional and adds cost but is recommended once before
sharing externally — see how it caught what Copilot missed in the
plan-review loop.
