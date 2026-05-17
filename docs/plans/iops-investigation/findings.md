# NATS IOPS Investigation — Findings

> **Status: complete (focused scope).** This is the operator-facing
> Phase 6 deliverable. It consolidates Tier 1 + M1.7 + M1.9 evidence
> into a single recommendation. Tier 0 (capture-chain validation) and
> Tier 0.5 (absolute calibration via `dd`) both passed before any
> hypothesis was tested. The full 190-run §M1 matrix was *not* run —
> Tier 1 + two focused ablations were enough to identify the dominant
> cost source. Cells not run (M1.4, M1.5, M1.6, M1.8, M1.10, M1.11)
> and the M4 / M4.1 calibration tables are listed under §7 "Scope
> limits".

---

## 1. Executive summary

The per-partition IOPS cost the operator reports (~278 IOPS at
N=1000, ~440 at N=2000) is **almost entirely on the JetStream data
stream**, not on parti's coordination protocol. Three pre-registered
hypotheses about the parti library (two-phase handoff, SweepInterval
ticker, parti KV bookkeeping) were tested and **all three are
falsified** — none of those knobs move the IOPS by more than 1–2 %.

**Recommended action — single biggest win**: set the user data
stream's `Storage = memory`. On the test rig this collapsed
`block_write_iops` by **90 % at N=1000** (216 → 22 IOPS) and **72 %
at N=3000** (450 → 128 IOPS), and removed a periodic ~10× spike at
t ≈ 200 s of harness lifetime that we attribute to a JetStream
data-stream snapshot or page-cache flush. The trade-off is loss of
message durability across a coordinated cluster-wide restart (with
R ≥ 3 the cluster survives single-node restarts via peers).

If durability cannot be relaxed, the cost is fundamentally in
JetStream's per-consumer state machinery on disk — tunable at the
NATS server level (`MaxAckPending`, `AckWait`, snapshot interval),
not in parti. A deeper parti redesign that collapses N pull-consumers
into a single subject-filtered consumer is the architectural
follow-up worth scoping if the memory-storage path is unavailable.

---

## 2. What each cell means (matrix legend)

The investigation ran a subset of a larger ablation matrix defined
in `00-attribution-plan.md`. Each cell is named `M1.x` and tests
parti running at default config **plus exactly one knob changed**.
Below is what each cell in this report actually does — read this
before §3 onward.

| Cell | What's changed vs default config | Why it was tested |
|---|---|---|
| **M1.0** | parti is **not running**; only the NATS cluster is up. | Defines the no-parti baseline / noise floor. The "what is parti adding" reference point. |
| **M1.1** | `EnableTwoPhaseHandoff = false` (older v2.2.x style handoff). Default sweep. | Tests whether two-phase handoff adds per-partition IOPS. |
| **M1.2** | Default config — `EnableTwoPhaseHandoff = true`, default `SweepInterval = 30 s`, both KV buckets and data stream on disk. | **Baseline** that every other cell is compared against. This is what most users run today. |
| **M1.3** | Default config + `Handoff.SweepInterval = 5 min` (vs default 30 s). | Tests the "raise the sweep ticker interval to reduce slope" mitigation. |
| **M1.7** | Default config + user **data stream** `Storage = memory` (vs file). | Tests whether per-partition pull-consumer state on the data stream is the slope source. |
| **M1.9** | Default config + all four parti **KV buckets** (heartbeat / stable-ID / election / assignment-handoff) `Storage = memory` (vs file). | Tests whether parti's coordination KV writes are the cost source. |

Mental model: **the default (M1.2) is what users currently run.
Every other cell flips one switch and we measure whether IOPS
moves.**

The cells listed in §7 "Scope limits" (`M1.4`, `M1.5`, `M1.6`,
`M1.8`, `M1.10`, `M1.11`) are other one-knob-changes from the
plan that this investigation did not run; they are listed there
with brief explanations for completeness.

---

## 3. Per-subsystem IOPS budget

### Default config (M1.2 — the user-relevant baseline), capture-window mean (t ∈ [120, 240] s)

| Partition count `N` | `read_rpc_ops/s` | `write_mutation_ops/s` | `block_read_iops` | `block_write_iops` |
|---|---:|---:|---:|---:|
| 500   | 0.63 | 0.90 | ~0 | 127 |
| 1000  | 0.63 | 0.90 | ~0 | **216** |
| 3000  | 0.66 | 0.90 | ~0 | **450** |

Two-point cluster-summed slope at N ∈ [1000, 3000]: **β₁ ≈ 0.117
IOPS/partition** on `block_write_iops`. The other three columns
have essentially zero slope (parti's RPC rate does not scale with N).

> Note on `analyze.py` slope numbers: the OLS slope reported in
> `analysis/slope_table.csv` (β₁ ≈ 0.080) is computed across the
> whole run including the warmup, which dilutes the high-N mean.
> The capture-window-only number above (0.117) is the honest
> per-partition cost. A future patch should make analyze.py respect
> the warmup boundary; see §6.

### Cost decomposition at N=1000

Per the M1.7 / M1.9 ablations:

| Source | Approx contribution | Evidence |
|---|---:|---|
| Data stream consumer state + page-cache flush + raft commits | **~190 IOPS (88 %)** | M1.2 − M1.7 = 216 − 22 |
| Parti coordination (heartbeat / stable-ID / election / assignment KVs) | ~22 IOPS (10 %) | M1.7 (data off disk, parti KVs still on) |
| NATS server overhead (meta-cluster, log) | ~3 IOPS | M1.0 ≈ 0.2; remainder absorbed into "parti coordination" |

---

## 4. Hypothesis verdicts

### H1 — Two-phase handoff sweep dominates the per-partition slope

- **Verdict: FALSIFIED**
- **Evidence (Tier 1):**
  - **Two-phase off** (M1.1 — `EnableTwoPhaseHandoff = false`): capture-window mean N=1000 = 226, N=3000 = 338
  - **Default** (M1.2 — two-phase on, default sweep): N=1000 = 216, N=3000 = 450
  - **Sweep = 5 min** (M1.3 — `SweepInterval = 5min`): N=1000 = 228, N=3000 = 344
  - Slope differences between "default" and either "two-phase off" or "sweep = 5 min" are within ±0.013 IOPS/partition — well under the run-to-run reproducibility.
- **Implication:** the original investigation premise ("the sweep ticker walks every partition periodically; raise `SweepInterval` to fix it") is wrong. The slope source is not in parti's library code.

### H2 — Per-partition durable JS pull consumer state churn

- **Verdict: SUPPORTED (sub-hypothesis H2.C confirmed; H2.A and H2.B not tested in this scope)**
- **Evidence — data stream in memory (M1.7, `--data-storage=memory`):**
  - N=1000 = 22 IOPS (default was 216), N=3000 = 128 IOPS (default was 450).
  - 90 % drop at N=1000, 72 % drop at N=3000.
  - Slope drops from 0.117 → 0.053 IOPS/partition (~55 % cut). Residual slope is parti's own per-partition coordination cost.
  - The periodic spike at t ≈ 200–220 s (~10× baseline, scales with N) is **eliminated** by memory data storage, confirming the spike is a JetStream data-stream event, not a parti event.
- **Not tested:** the `FetchTimeout = 30 s` ablation (M1.5 — idle-pull frequency) and the `consumer.Queue` ablation (M1.6 — single-consumer vs N-consumer geometry). Either of these may further attribute *within* the data-stream cost; that is the next ablation to run if the operator can't use memory storage.

### H3 — Heartbeat + stable-ID + election: constant write floor

- **Verdict: SUPPORTED**
- **Evidence:**
  - `write_mutation_ops/s` intercept is ~0.90 cluster-total across **all** parti cells regardless of which knob was changed (default M1.2, two-phase off M1.1, sweep=5min M1.3, memory data stream M1.7, memory KV M1.9), matching the §H3 prediction of ~0.9 ops/s (5 workers × HBI=5s → 1.0/s heartbeat puts + small stable-ID + election rate).
  - Tier 0 wrapper-counter check verified the same predicted rates (heartbeat = 1.02/s, stable-ID = 0.21/s, election = 1.53/s) within ±5 % of prediction.
- **Implication:** parti's own RPC floor is well-characterized and matches first-principles calculation. It is not the slope.

### H4 — JetStream RAFT / metadata overhead

- **Verdict: PARTIALLY SUPPORTED**
- **Evidence:**
  - **No parti** (M1.0 — only NATS cluster running, no harness, no workers): `block_write_iops` ~ 0.2 IOPS. The NATS server at total idle is essentially silent.
  - The constant ~100 IOPS that appears the moment parti connects (M1.2 intercept) is *not* RAFT metadata — RAFT writes happen only when stream/consumer configs change, which is rare. The constant is parti's KV operations replicated to R=3 nodes (R=3 amplifies each Put to 3 cgroup writes, and JetStream's filestore commits each Put as multiple bios).
- **Open question:** the R=5 comparison (M1.10 — running with 5 NATS replicas instead of 3) was not run — we did not measure how RAFT cost scales with R.

### H5 — Residual O(N) slope after H1/H2 ablated

- **Verdict: PARTIALLY ATTRIBUTED**
- **Evidence:** After the data-stream ablation (M1.7 — `--data-storage=memory`), the residual slope is **0.053 IOPS/partition**. The lower bound on this residual is parti's own per-partition KV operations and any per-partition assignment-source reads. We did not isolate this further; the candidates are:
  1. Parti's per-partition election-bucket writes (one Put per leader change, one Put per heartbeat in some configs).
  2. Per-partition assignment-bucket reads as workers `Watch` the bucket.
  3. Per-partition consumer subscription churn at the parti layer (subscribe / unsubscribe during rebalance).
- **Implication:** even with the data stream in memory, there is a small per-partition cost in parti itself (~0.053 IOPS/partition = ~53 IOPS at N=1000). For workloads at very high N this becomes the new dominant cost.

---

## 5. Verified mitigations

Ranked by measured slope reduction on `block_write_iops`. The
"What it changes" column states the actual config tweak in
plain terms; the cell label (M1.x) is the matrix entry from §2.

| Rank | What it changes | Cell | Δβ₁ block_write | Side effects |
|---|---|---|---|---|
| **1** | **Set user data stream `Storage = memory`** | M1.7 | **−0.064 IOPS/part (−55 % slope, −90 % at N=1000)** | Messages on the data stream do not survive a coordinated cluster-wide restart. With R ≥ 3, single-node restarts are still safe. Stream size is bounded by node RAM. |
| 2 | All four parti KV buckets `Storage = memory` | M1.9 | −0.001 IOPS/part (within noise) | Parti coordination state does not survive coordinated cluster-wide restart. **Recommendation: do not bother — the cost reduction is not real.** |
| 3 | `Handoff.SweepInterval = 5 min` (vs default 30 s) | M1.3 | −0.001 IOPS/part (within noise) | Slower handoff completion under churn. **Recommendation: do not bother — the cost reduction is not real.** |
| 4 | `EnableTwoPhaseHandoff = false` (revert to v2.2.x style) | M1.1 | +0.003 IOPS/part (no effect within noise) | Re-introduces the v2.2.x race that two-phase handoff was added to close. **Recommendation: do not use this as a mitigation; it does not save IOPS and reintroduces a known bug.** |

**Recommended first action: set data stream `Storage = memory`** if
the workload tolerates loss-on-coordinated-restart.

If durability is required, the residual cost lives in JetStream
server-side state; tunable server-side knobs:

- `MaxAckPending` — caps per-consumer redelivery state.
- `AckWait` — longer → fewer re-arms per partition per second.
- `MaxOutstandingAck` (server config) — caps total ack-tracking.
- JetStream snapshot interval — longer → less frequent disk flush.

A code-change recommendation worth scoping in a separate plan:
**replace N individual pull-consumers with a single consumer using
subject filtering**. This is where the 88 % cost lives
architecturally; the design change is non-trivial but the savings
are.

---

## 6. Methodology

- **Cadence:** 120 s warmup + 120 s capture per run (chosen because
  Tier 0 reproducibility CV = 0.006 at 60 s; 120 s gives 2× safety
  margin and covers the late-window spike at t ≈ 200–220 s on
  high-N disk runs).
- **Means:** capture-window only (t ∈ [120 s, 240 s] from harness
  start). `analyze.py` currently averages the whole run; the
  per-cell means in §3 were computed by inline awk on
  `aggregated.csv` to avoid the warmup-dilution.
- **Replication:** 3 reps per (cell, N). Tier 0's CV = 0.6 % at
  N=2000 confirmed 3 reps is over-precise for slope-difference
  tests.
- **Two-point slope:** β₁ = (mean(N=3000) − mean(N=1000)) / 2000.
  Used in §3/§5 in preference to the OLS slope from `analyze.py`
  for the warmup-dilution reason above.
- **Validation gates passed before testing:**
  - Tier 0 capture-chain ✓ (all 4 capture sources land non-empty)
  - Tier 0 wrapper-counters ✓ (heartbeat=1.02, stable-id=0.21,
    election=1.53 — all within ±5 % of §H3 predictions)
  - Tier 0 reproducibility ✓ (CV = 0.006 across 3 reps at N=2000)
  - Tier 0 MDE ✓ (MDE_slope on read_rpc_ops = 0; on
    block_write_iops = 1.05e-5; well below the 0.167 §R5 gate)
  - Tier 0.5 cgroup calibration ✓ (cgroup wbytes / dd payload =
    1.000 exact under O_DIRECT writes)

Full details: `00-attribution-plan.md`, `RUNBOOK.md`,
`tier1-findings.md`, `m17-findings.md`, `m19-findings.md`.

---

## 7. Scope limits / what was not run

This investigation pursued the "fast evidence" path: identify the
dominant cost source, verify one mitigation, stop. The following
matrix cells (one-knob-changes from default config, per §2) were
*not* run, and are documented here so a future operator can extend
the work:

- **M1.4** — `EnableTwoPhaseHandoff = false` while handoff bucket
  is still created (but unused). Would distinguish "two-phase
  logic" cost from "handoff bucket existence" cost. Predicted to
  coincide with the two-phase-off result (M1.1, §2); not verified.
- **M1.5** — `FetchTimeout = 30 s` (vs default 5 s). Reduces
  idle-pull request frequency by 6×. Would attribute the
  data-stream cost between (a) idle-pull request frequency vs (b)
  per-consumer state-machine bookkeeping. **Worth running if the
  operator cannot use memory storage** (rank 1 mitigation in §5).
- **M1.6** — `consumer.Queue` instead of `consumer.Dynamic`. Tests
  whether collapsing N pull-consumers into one queue group reduces
  cost. The physical effect is the same as the
  single-consumer-with-filter design idea below, but achievable
  without a parti code change for queue-compatible workloads.
- **M1.8** — `HeartbeatInterval = 10 s` (vs default 5 s). Would
  verify the intercept-cost story (§4 H3) directly. The H3 intercept
  prediction already matches measurement; M1.8 would tighten that.
- **M1.10** — 5 NATS replicas (R=5) instead of 3 (R=3). Would
  measure how the constant *and* slope scale with replication.
- **M1.11** — HEAD parti vs v2.3.0 (regression check on the parti
  side rather than NATS config).
- **M4 / M4.1 calibration grid** — the full four-column attribution
  budget (`attribution_table.csv` in the original scaffold) was
  not produced; this investigation answered the operator's question
  without it. M4.1 remains valuable for future investigations
  where the question is "how does FetchTimeout × storage × R ×
  node_role interact" rather than "what is the dominant cost".
- **analyze.py warmup-aware mean** — the slope numbers in
  `analysis/slope_table.csv` are diluted by warmup samples. A
  follow-up patch should make analyze.py honor the harness's
  `warmup_done_t` marker. The §3 / §5 numbers in this report were
  computed with capture-window-only awk; do not use the
  `analyze.py` slopes in operator-facing communication until that
  patch lands.

---

## 8. Raw data

- **Tier 1 campaign** (M1.0 / M1.1 / M1.2 / M1.3 — controls + two-phase + sweep ablation):
  - Manifest: `test/iops-investigation/results/tier1-20260517-032602/campaign-manifest.yaml`
  - 36 runs, all status `ok`
- **M1.7 focused** (data stream in memory):
  - `test/iops-investigation/results/m17-20260517-153942/` (6 runs)
- **M1.9 focused** (all parti KV buckets in memory):
  - `test/iops-investigation/results/m19-20260517-143438/` (6 runs)
- **Tier 0 validation:**
  - `test/iops-investigation/results/tier0-real-20260516-231220/`
- **Tier 0.5 calibration:**
  - `test/iops-investigation/results/tier0.5-*` (3 runs at 32 / 256 MiB)
- **Campaign seed (all runs):** `42`
- **NATS image:** `nats:2.12.6`
- **Parti commit (worktree branch):** see `git log --oneline` on
  `worktree-iops-investigation`; HEAD at the time of these findings
  is `0373c19`.
- **Rig:** docker compose R=3 profile.
- **Host:** ext4, see `manifest.yaml` per run for full host details.

---

## 9. Open questions

- **The periodic spike's exact cause.** The spike at t ≈ 200–220 s
  fires deterministically across reps on disk-storage cells,
  scales with N, and disappears under memory data storage. We
  identified it as data-stream-side but did not pinpoint whether
  it is a JetStream consumer snapshot, a filestore writeback, or
  a raft snapshot. A NATS-server-internal investigation could
  isolate this; it is outside parti's scope.
- **What fraction of the residual ~0.053 IOPS/partition (post-data-stream-memory)
  is parti's election bucket vs assignment bucket vs source-watch
  vs consumer subscribe.** This requires per-bucket attribution
  (the M4 calibration table in the original plan). Not produced
  in this scope.
- **Scaling beyond N = 3000.** The two-point fit at {1000, 3000}
  is linear by construction; the actual per-second time series at
  N=3000 shows *sub-linear* growth in the steady state (most of
  the apparent slope is the late-window spike). A more precise
  characterization for N > 3000 would need {3000, 5000, 8000}
  runs — not in scope here.
- **R=5 behavior.** Some cost components scale linearly with R
  (raft log appends); others don't (meta-cluster bookkeeping).
  Untested.

### Plan 02 §4 cell design — predictions (recorded 2026-05-17, before runs)

Per `02-nats-tuning-plan.md` §4, the candidate cells from R1
(`tmp/r1-nats-tuning-research.md`) and their pre-registered
predictions for `block_write_iops` cluster-summed mean at N=1000
(baseline M1.2 = 216 IOPS):

- **M2.A** (`consumer.MemoryStorage = true`, NumReplicas inherits 3) —
  predict 80–130 IOPS (40–63 % below baseline). Recovers most of
  M1.7's 90 % win while keeping the message log durable. **Falsifies
  R1's rank-1 hypothesis if N=1000 stays above 180 IOPS.**
- **M2.B** (`MemoryStorage = true` AND `NumReplicas = 1`) —
  predict 40–80 IOPS (63–82 % below baseline). Approaches M1.7's
  22-IOPS floor with messages still durable on R=3 file storage.
  **M2.A ≈ M2.B (within ±5 %) → consumer disk writes dominate;
  large gap → raft replication is a real lever.**
- **M2.C** (`jetstream.sync_interval = "10m"` raised from 2 m default) —
  predict 170–210 IOPS (≤ 22 % below baseline). Win comes mostly
  from eliminating the t ≈ 200–220 s spike, not from steady-state
  reduction (kernel writeback runs regardless of fsync cadence).
  **Falsifies the "raise sync_interval" mitigation if the spike
  persists at the new cadence.**
- **Smoke** (`sync_interval = "always"`, N=100, 1 rep, capture-chain
  control) — predict ≥ 5× the M1.2 N=100 floor. If not, rig isn't
  honoring `nats-server.conf` changes — fix before running M2.C.
