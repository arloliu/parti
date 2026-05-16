# NATS IOPS Investigation — Findings

> **Status: scaffold.** This document is the Phase 6 deliverable. The
> structure is fixed by `00-attribution-plan.md` §Success criteria;
> every quantitative claim below is `<TBD>` and must be filled in by
> the operator/analyst once the Phase 5 campaign data lands and
> `scripts/analyze.py` has been run against it.
>
> Inputs the analyst must have on hand before editing this file:
>
> - `test/iops-investigation/results/<campaign>/analysis/slope_table.csv`
> - `test/iops-investigation/results/<campaign>/analysis/attribution_table.csv`
> - `test/iops-investigation/results/<campaign>/analysis/mitigation_table.csv`
> - `test/iops-investigation/results/<campaign>/analysis/mde.csv`
> - `test/iops-investigation/results/<campaign>/campaign-manifest.yaml`
> - The M4 / M4.1 calibration CSV used to produce the analysis.
>
> The interpretation pass is **Opus 4.7 xhigh** per the plan: "this is
> the part of the work where the data and the hypothesis disagree about
> half the time, and the right call is 'which signal do I trust'".

---

## 1. Executive summary

> One-paragraph operator answer: which knob to turn first, why, and how
> much it saves. Reference the recommended mitigation by name and cite
> its measured slope reduction in the four budget columns.

`<TBD after Phase 5 data>`

---

## 2. Per-subsystem IOPS budget

The four-column budget per plan §00 line 17:

```
( read_rpc_ops/s ,  write_mutation_ops/s ,  block_read_iops ,  block_write_iops )
```

### B-prod baseline (M1.2), default knobs

| Partition count `N` | `read_rpc_ops/s` | `write_mutation_ops/s` | `block_read_iops` | `block_write_iops` |
|---|---|---|---|---|
| 500  | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| 1000 | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| 2000 | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| 3000 | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |

Per-partition slope `β̂₁` (with 95 % CI) per column for M1.2:

| Column | `β̂₁` | 95 % CI | R² | Verdict vs MDE |
|---|---|---|---|---|
| `read_rpc_ops` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| `write_mutation_ops` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| `block_read_iops` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| `block_write_iops` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |

Per-subsystem attribution at N=2000 (rows from
`attribution_table.csv`):

| Subsystem | `read_rpc_ops/s` | `write_mutation_ops/s` | `block_read_iops` | `block_write_iops` |
|---|---|---|---|---|
| Election           | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| Heartbeat          | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| Stable-ID          | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| Assignment         | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| Handoff (H1)       | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| Partition source   | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| Pull consumers (H2 via M4.1) | — | — | `<TBD>` | `<TBD>` |
| **Residual**       | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |

The success criterion is that `residual` <= `2 x SE(beta1)` for the
cell in every column independently (plan §Success criteria, item 3).

`<TBD: state per-column whether residual passed or failed the criterion>`

---

## 3. Hypothesis verdicts

For each hypothesis: `confirmed | falsified | inconclusive`, with the
slope evidence and the relevant ablation comparison.

### H1 — Two-phase handoff sweep dominates the per-partition slope (B-prod only)

- **Verdict:** `<TBD>`
- **Evidence:** M1.2 vs M1.3 (SweepInterval=5min) slope delta on
  `read_rpc_ops`: `<TBD>` (MDE `<TBD>`); M1.2 vs M1.4 (two-phase off)
  slope delta: `<TBD>`.
- **B-prod vs B-lib:** M1.2 - M1.1 slope on `read_rpc_ops` = `<TBD>`.
  If positive and >> MDE, H1's predicted ~ N/6 cluster slope is
  reproduced.

### H2 — Per-partition durable JS pull consumer state churn

- **Verdict:** `<TBD>`
- **Evidence:** M1.5 (FetchTimeout=30s), M1.6 (Queue consumer),
  M1.7 (data stream memory) vs M1.2. Slope deltas on
  `block_write_iops`: `<TBD>`. Cross-check against the M4.1-attributed
  H2 column in `attribution_table.csv`.

### H3 — Heartbeat + stable-ID + election: constant write floor

- **Verdict:** `<TBD>`
- **Evidence:** Intercept `β̂₀` on `write_mutation_ops` for M1.2:
  `<TBD>` (expected ~ 1.5 ops/s cluster total per plan §H3). M1.8
  (HeartbeatInterval=10s) intercept delta on `write_mutation_ops`:
  `<TBD>` (predicted ~ -0.5).

### H4 — JetStream RAFT / metadata overhead

- **Verdict:** `<TBD>`
- **Evidence:** M1.0 (no-Parti control) intercept on
  `block_write_iops`: `<TBD>`. This is the per-node RAFT/metadata
  floor; if M1.2's intercept ~ M1.0 intercept + H3 floor, H4 is the
  whole non-slope story.

### H5 — Residual O(N) slope after H1/H2 ablated

- **Verdict:** `<TBD>`
- **Evidence:** After M1.4 (two-phase off) + M1.6 (Queue), residual
  block_*_iops slope: `<TBD>`. If `> MDE`, the campaign has surfaced
  an unattributed O(N) mechanism — open a follow-up plan.

---

## 4. Verified mitigations

Ranked by measured slope reduction on `block_write_iops` (the user's
reported metric), filtered to entries where `verified = true` in
`mitigation_table.csv`.

| Rank | Mitigation | Cell | `Δβ̂₁` block_write | `Δβ̂₁` read_rpc | Side effects |
|---|---|---|---|---|---|
| 1 | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| 2 | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| 3 | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |

**Recommended first action:** `<TBD>`. Operator cost: `<TBD>`. Risk:
`<TBD>`.

If a code-change recommendation is being made (e.g. replace
`maybeSweepClaims` with a watcher-snapshot replay), open a follow-up
plan `02-followup-<name>.md` rather than expanding scope here — per
plan §Verification of candidate mitigations, item 5.

---

## 5. Methodology (summary)

- **Estimator:** OLS line `y_ij = β₀ + β₁ × N_i + ε_ij` on
  replicate-level observations (not per-N means). 95 % CI = `β̂₁ ±
  t₀.₉₇₅,df × SE(β̂₁)`. Implementation: `statsmodels.OLS` in
  `scripts/analyze.py`.
- **MDE:** Derived from M1.0 (no-Parti control). `MDE_slope =
  t₀.₉₇₅,df × SE_no_Parti(β̂₁)`. Per-column MDE in `mde.csv`.
- **Outlier rule:** Tukey fences (median ± 1.5 × IQR) over the 5 run-
  level means per cell. Operator re-ran flagged points once during
  Phase 5; `tukey_outliers.tsv` in the analysis dir cross-checks the
  operator's outlier log.
- **Attribution arithmetic:** §M3 four-column budget. Client RPC
  columns from the harness wrapper; block I/O columns from the M4 KV-
  paths factors plus M4.1 idle-pull interpolation.
- **M4.1 interpolation:** Linear between bracketing `C_stream` grid
  points, per-node-role, at the run's `(FetchTimeout, data_storage,
  R)` slice. See `internal/calibrate/m41reduce.go` for the row schema.

Full details: `docs/plans/iops-investigation/00-attribution-plan.md`
and `test/iops-investigation/RUNBOOK.md`.

---

## 6. Raw data

- **Campaign manifest:**
  `test/iops-investigation/results/<campaign>/campaign-manifest.yaml`
- **Pre-registered schedule:**
  `test/iops-investigation/results/<campaign>/schedule.tsv`
- **Per-run artifacts:**
  `test/iops-investigation/results/<campaign>/run-NNN-<cell>-N<N>-rep<r>/`
- **M4 calibration:**
  `test/iops-investigation/results/calibration/m4_<date>.csv`
- **Analysis outputs:**
  `test/iops-investigation/results/<campaign>/analysis/`
  - `slope_table.csv`
  - `attribution_table.csv`
  - `mitigation_table.csv`
  - `mde.csv`
  - `tukey_outliers.tsv`

Campaign seed (for replay): `<TBD>`. NATS image digest: `<TBD>`.
Parti commit: `<TBD>` (for M1.11 HEAD comparison: `<TBD>`).

---

## 7. Open questions

> Things the campaign surfaced that were NOT pre-registered. Examples
> the analyst should be alert for:
>
> - Slope sign flips between B-prod (M1.2) and B-lib (M1.1) on any
>   column. Plan predicts they coincide except on `read_rpc_ops`.
> - Per-bucket residual on `read_rpc_ops` >> 0 — implies the
>   instrumentedjs wrapper missed a method.
> - Leadership churn during M1.2 / M1.5 / M1.6 runs (`manifest.yaml`
>   carries the pre/post leader names).
> - M1.10 (R=5) slope significantly different from M1.2 in ways the
>   M4.1 calibration grid does not predict — suggests RAFT cost is
>   non-linear in R for the stream geometry under test.
> - HEAD vs v2.3.0 (M1.11) regression or improvement.

`<TBD>`
