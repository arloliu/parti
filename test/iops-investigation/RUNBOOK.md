# IOPS Investigation — Operator Runbook

This runbook covers the Phase 5 execution campaign: running the M1.0–M1.11
measurement matrix, validating the rig, handling outliers, and handing off to
Phase 6 analysis.

**Time budget:** depends on the tier you pick — see §3. Tier 0 (~45 min)
validates that the rig is producing honest measurements and gates every
later tier. Tier 1 (~10 h) is usually enough to identify the dominant
IOPS source and verify one mitigation. Tier 3 (~70 h) is the full §M1
matrix for externally-citable findings. Schedule on a dedicated host
with no concurrent workloads.

---

## 1. Prerequisites

### Host requirements

| Requirement | Why |
|---|---|
| Linux, cgroup v2 (`/sys/fs/cgroup` mounted with `type=cgroup2`) | `scripts/capture-cgroup-io.sh` reads per-container `io.stat` under the systemd cgroup hierarchy. |
| `sysstat` package (`iostat` in PATH) | `scripts/capture-iostat.sh` wraps `iostat -x -d -t 1`. |
| Docker Engine ≥ 24 with compose v2 | Runs the 3 / 5-node NATS cluster. |
| At least 20 GiB free disk on the results volume | 190 runs × ~100 MiB capture artifacts = ~19 GiB. |
| No other disk-intensive workloads during capture | Cgroup-vs-iostat cross-check fails loudly on noisy hosts (§R4 of the plan). |

### Software versions (pinned in go.mod)

```
github.com/arloliu/parti/v2  v2.3.0     # campaign baseline; M1.11 swaps this
nats-server                  v2.12.6    # via IOPS_RIG_NATS_IMAGE=nats:2.12.6
```

### Build the harness binary once before starting

```bash
cd test/iops-investigation
go build -o cmd/harness/harness ./cmd/harness
go build -o cmd/aggregate/aggregate ./cmd/aggregate
```

### Verify cgroup v2 is active

```bash
mount | grep cgroup2
# Must print at least one line; if empty, the host uses cgroup v1 and
# capture-cgroup-io.sh will produce empty files.
```

### Verify the rig smoke test passes

```bash
cd test/iops-investigation
go test -race -count=1 -run TestE2E_HarnessToAggregatedCSV ./cmd/harness/
# Must finish in < 60 s and print PASS.
```

---

## 2. Pre-execution checkpoint

Run these gates **in order** before touching the full matrix. Each gate is a
go/no-go: do not proceed past a failing gate.

### Gate 1 — M4 calibration (server-internal pull-consumer baseline)

M4 measures the no-Parti NATS server I/O cost for the pull-consumer grid.
It must complete before any harness run so you have `c_stream` per-stream
calibration numbers to fold into the attribution budget.

```bash
cd test/iops-investigation
make reset
go run ./cmd/calibrate \
  --nats-url nats://localhost:4222 \
  --output results/calibration/m4_$(date +%Y%m%d).csv
```

Accept the calibration if the per-cell IOPS noise is < 5 % CoV across 3
replicates. Record the output path; Phase 6 analysis imports it.

### Gate 2 — M1.0 control run (no-Parti noise floor)

M1.0 runs the NATS cluster **without** the harness binary to establish
`MDE_slope` — the no-Parti disk-I/O slope. The matrix script synthesises a
minimal `manifest.yaml` and header-only `rpc_counts.csv` for each M1.0 run so
the aggregator produces an `aggregated.csv` with empty RPC columns.

```bash
bash scripts/run-matrix.sh \
  --seed 42 \
  --cells M1.0 \
  --results-dir results/$(date +%Y%m%d)/
```

M1.0 has 4 N-values × 5 reps = 20 runs (~6 h). After they complete:

```bash
# Quick sanity: every M1.0 run must have an aggregated.csv with no rpc_read_ columns.
for d in results/$(date +%Y%m%d)/run-*-M1.0-*/; do
  head -1 "$d/aggregated.csv" | grep -q "rpc_read_" && echo "BAD: $d" || true
done
```

### Gate 3 — MDE acceptance check

Before committing to the full 170-run knob matrix, confirm that the MDE
(minimum detectable effect) is ≤ the H1 predicted slope. The H1 slope is:

```
H1_slope = 5 workers × (1 / 30 s) = 0.167 RPC/s/partition
```

Fit a simple OLS line `y_ij = β₀ + β₁ × N_i + ε_ij` on the M1.0
`rpc_read_PARTI_HANDOFF` column across all 20 runs. If `MDE_slope ≥ 0.167`,
the rig is too noisy to detect the hypothesised signal; investigate host noise
sources before proceeding. Run the analyser in slopes-only mode to compute the
MDE without needing the M4 calibration:

```bash
python3 scripts/analyze.py \
  --results-dir results/$(date +%Y%m%d)/ \
  --slopes-only \
  --out results/$(date +%Y%m%d)/analysis/
```

Read `MDE_slope` from `analysis/mde.csv` (column `mde_slope`, row
`column=read_rpc_ops`).

**Proceed to Gate 4 only if `MDE_slope < 0.167 RPC/s/partition`.**

---

## 3. Execution tiers — pick one before running

The §M1 matrix (190 runs × 17 min ≈ 70 h) is sized for an exhaustive
four-column attribution budget at n=5 statistical rigor. For the actual
goal — *identify the dominant IOPS source and verify one mitigation
moves the slope* — a much smaller subset is usually enough. Pick a tier,
run it, decide whether to escalate.

**Always run Tier 0 first** — it validates that the rig is producing
honest measurements before any quantitative claim is made. A noisy
or miswired rig will produce confidently wrong tier-1 results;
Tier 0 catches that in ~45 minutes.

### Tier 0 — measurement-chain validation (~45 min)

**Principle:** don't trust an instrument you haven't calibrated. Tier 0
proves the rig is measuring what it claims to measure, before any
hypothesis is tested. Each check exercises one link in the chain:

| Check | What it proves | Time |
|---|---|---:|
| 1. `capture-chain` | All four capture sources (cgroup/iostat/jsz/node_exporter) land non-empty per run; the aggregator's strict cgroup-vs-iostat divergence guard passes. | ~3 min |
| 2. `wrapper-counters` | `instrumentedjs` wrapper RPC counts match §H3's predicted cluster rates (heartbeat ≈ W / HBI, stable-ID ≈ W / 25 s, election ≈ 1.5 ops/s) within ±50 %. Confirms the wrapper isn't silently dropping methods. | ~3 min |
| 3. `reproducibility` | Per-run mean block_write_iops varies by less than 25 % across 3 back-to-back identical runs. Confirms the host is quiet enough to fit a slope. | ~9 min |
| 4. `mde` | The no-Parti M1.0 slope SE puts `MDE_slope(read_rpc_ops) < 0.167 RPC/s/partition`, the §R5 acceptance gate that determines whether the rig has the resolution to see the H1 signal. | ~27 min |

Run all four with one command:

```bash
bash scripts/tier0-validate.sh --seed 42 \
  --results-dir results/tier0-$(date +%Y%m%d)/
```

The script schedules every check via `run-matrix.sh` with compressed
warmup/capture windows (30 s / 60 s by default — tunable via
`--warmup-secs` / `--capture-secs`) and prints a PASS/FAIL summary at
the end. Exit code: **0 = all pass**, **1 = any fail**, **2 = nothing
ran** (e.g. all checks `--skip`'d or `--dry-run`). The script must
PASS before you start Tier 1.

#### Failure interpretation

| Failed check | Most-likely cause | Where to look |
|---|---|---|
| `capture-chain` | Missing capture prerequisites (cgroup v2, iostat, curl), partial captures, or aggregator strict-mode tripping on cgroup ↔ iostat divergence > 5 %. | `make_reset` worked? `iostat` installed? Re-read §1. The aggregator's strict-mode divergence is usually a kernel/io.stat permissions issue. |
| `wrapper-counters` | Wrapper is missing a method (silent attribution hole), OR parti's actual cadence differs from §H3 predictions. | Check `instrumentedjs/instrumentedjs.go`'s wrapped methods against parti's call surface; check `parti.Config` defaults match the §H3 predicted rates. |
| `reproducibility` | Host has concurrent workloads, page-cache warmth varies, or volume is on a noisy disk. | Run `iostat` on the host between checks; the disk should be near-idle. Pin CPU governor to performance. |
| `mde` | Rig noise floor exceeds the H1 signal target. | Lengthen `--capture-secs` (e.g. 120 s), raise `--reps` to 5, or move to a quieter host. The plan's full-cadence 10 min × n=5 was sized exactly for this case. |

#### What Tier 0 does *not* prove

- That the §M1 hypotheses are correct — that's Tier 1+.
- That the §M3 four-column attribution closes — that's Tier 3, after M4 calibration.
- That a specific mitigation works on production traffic — Tier 1 ablations check the slope, but production confirmation is a separate operator step.
- **That the absolute `block_*_iops` numbers are calibrated.** Tier 0 proves the chain is honest and reproducible; it does NOT prove cgroup and iostat report consistent values against a known workload. For that, run Tier 0.5.

### Tier 0.5 — absolute calibration (~2 min)

**Principle:** Tier 0 proves the instrument is *honest*, but harness
runs show cgroup-vs-iostat ratios as large as 27× during active load.
That divergence may be legitimate (write merging, journal coalescing,
cgroup pre-merge accounting) or it may be a real bug — Tier 0 can't
tell because both sources are measuring the same unknown workload.
Tier 0.5 anchors the legitimate band by running a **known workload**
(a `dd` with O_DIRECT + fdatasync, payload exactly `N` bytes) and
asserting that both instruments report values consistent with the
ground truth.

| Check | Gate band | Why |
|---|---|---|
| `cgroup wbytes / dd_payload` | `[0.95, 1.30]` | cgroup v2 io.stat counts block-layer writes; O_DIRECT bypasses page cache so cgroup should track payload tightly. Anything below 0.95 means cgroup is missing writes; above 1.30 means cgroup is double-counting (fdatasync metadata, journal, etc. — quantify it). |
| `iostat wbytes / dd_payload` | `[0.30, 1.30]` | iostat reads `/proc/diskstats` (post-merge). For O_DIRECT sequential writes the kernel should not merge much, so values close to 1.0 are expected. Below 0.30 means iostat is summing the wrong devices or dropping samples. |
| `cgroup / iostat` | `[0.50, 4.00]` | Defines the legitimate divergence band for this host. Harness numbers outside this band can no longer be hand-waved as "write merging" — they require investigation. |

```bash
bash scripts/tier0.5-calibrate.sh \
  --payload-mib 32 \
  --results-dir results/tier0.5-$(date +%Y%m%d)/
```

The script launches an `iops-calibrator` busybox sidecar on a fresh
docker volume (same physical disk as the nats data volumes), runs
`dd if=/dev/zero of=/data/x bs=4096 count=8192 oflag=direct conv=fdatasync`,
captures cgroup + iostat over the same window, computes the three
ratios above, and prints `VERDICT: PASS` / `FAIL`. Exit code: **0 =
all bands hit**, **1 = at least one ratio out of band**. Always
cleans up the sidecar + volume on exit.

#### What Tier 0.5 told us empirically (2026-05-17 ext4 host)

Three runs across two payload sizes:

| Payload | cg / dd | ios / dd | cg / ios bytes | cg / ios IOs |
|---|---:|---:|---:|---:|
| 32 MiB (run A) | **1.000** | 1.97 | 0.51 | 0.87 |
| 32 MiB (run B) | **1.000** | 1.78 | 0.56 | 0.87 |
| 256 MiB (run C) | **1.000** | 1.11 | 0.91 | 0.98 |

Key results:

1. **cgroup is precise** — under O_DIRECT writes, cgroup wbytes ==
   dd payload exactly. The instrument has zero attribution drift; it
   is the right column for "logical writes by this container".
2. **iostat ≥ dd is normal.** The host overhead is filesystem
   journal commits (jbd2) and metadata flushes — outside the
   sidecar's cgroup but visible at the device. The overhead is
   roughly fixed in absolute bytes, so it dilutes from ~2× at 32 MiB
   to ~1.1× at 256 MiB.
3. **The calibrated direction is `cgroup ≤ iostat`.** This is the
   opposite of what the harness shows on busy windows (`cg = 27×
   ios`). The harness divergence is therefore NOT explained by
   journaling. It means JetStream is generating many small bios
   that the block layer aggressively merges before they hit
   `/proc/diskstats`. The 27× is a **measurement of JetStream's
   coalescing factor**, not an instrument bug.

**Implication for Tier 1+ reporting:** treat the two columns as
distinct quantities, not as cross-checks:

- `block_write_iops` from **cgroup** = JetStream's logical write
  rate, useful for understanding what parti drives NATS to do.
- `block_write_iops` from **iostat** = the disk's physical IOPS,
  useful for capacity planning and answering "can this SSD keep up".

For H1/H2 attribution the *slope* of each column vs N is what
matters; both should respond to the same ablations, with iostat
slope ≈ (cgroup slope / coalescing factor). If they disagree on
*sign* in any ablation, that's a finding worth digging into.

### Tier 1 — fast evidence (~10 h, one overnight)

Goal: answer "is the slope coming from two-phase handoff, and does
raising `SweepInterval` to 5 min kill it?"

| Cell | N values | Reps | Runs |
|---|---|---:|---:|
| M1.0 (control / MDE) | 500, 1000, 3000 | 3 | 9 |
| M1.1 (B-lib, two-phase off) | 500, 1000, 3000 | 3 | 9 |
| M1.2 (B-prod baseline) | 500, 1000, 3000 | 3 | 9 |
| M1.3 (H1.A — sweep=5min) | 500, 1000, 3000 | 3 | 9 |

Total: **36 runs × 17 min ≈ 10.2 h**.

```bash
bash scripts/run-matrix.sh \
  --seed 42 \
  --cells M1.0,M1.1,M1.2,M1.3 \
  --reps 3 \
  --n-values 500,1000,3000 \
  --results-dir results/tier1-$(date +%Y%m%d)/
```

If `analyze.py` shows M1.3's slope drops by ≥10× vs M1.2's, the
investigation is done — recommend `Handoff.SweepInterval = 5 min` and
ship `findings.md`. Most of the user's reported per-partition cost is a
30 s ticker.

### Tier 2 — H2 attribution (~7 h on top of Tier 1)

Goal: if Tier 1 leaves a residual slope (or H1.A is unverified),
attribute the remainder to per-partition pull-consumer state churn.

| Cell | N values | Reps | Runs |
|---|---|---:|---:|
| M1.5 (H2.A — fetch=30s) | 500, 1000, 3000 | 3 | 9 |
| M1.6 (H2.B — queue consumer) | 500, 1000, 3000 | 3 | 9 |
| M1.7 (H2.C — data-storage=memory) | 500, 1000, 3000 | 3 | 9 |

Total: **27 runs ≈ 7.6 h**.

```bash
bash scripts/run-matrix.sh \
  --seed 42 \
  --cells M1.5,M1.6,M1.7 \
  --reps 3 \
  --n-values 500,1000,3000 \
  --results-dir results/tier2-$(date +%Y%m%d)/
```

### Tier 3 — full matrix (~70 h)

Run only when the investigation must produce externally-citable
findings (paper, blog post, vendor escalation) and the n=5 / four-column
budget rigor is required. This is what §M1 + §R5 specify. See §4 below
for the full-campaign invocation.

### Decision rule

1. **Run Tier 0.** All four checks must PASS. If any FAIL, fix the
   instrument before measuring anything (see "Failure interpretation"
   above). Do not bypass.
2. Run Tier 1. Open `analyze.py` output.
3. If `mitigation_table.csv` shows M1.3 verified (`|β̂_M1.3 − β̂_M1.2| > MDE`
   with the right sign) AND the absolute slope reduction is operationally
   meaningful (≥0.10 IOPS/partition): write `findings.md`, ship the
   recommendation. Skip Tiers 2 and 3.
4. If M1.3 is not verified or residual slope ≥ MDE remains: run Tier 2.
5. If Tier 2 still leaves a residual: H5 is firing — escalate to Tier 3
   and consider whether a code-level investigation is warranted.

### What `n=3` buys vs `n=5`

§R5's n=5 is sized so the Tukey outlier rule can actually flag a true
one-off (with n=3 the rejected point dominates its SD). At n=3 you lose
that safety net. Mitigation: visually inspect each cell's 3 reps in
`slope_table.csv`; if between-rep variance is > 30% of the slope, raise
that specific cell to n=5 before believing the result.

---

## 4. Running the matrix

### Full campaign

```bash
bash scripts/run-matrix.sh \
  --seed 42 \
  --results-dir results/$(date +%Y%m%d)/
```

- The seed is pre-registered in every run's `run-meta.yaml` for reproducibility.
- The full schedule (190 runs, ~70 h) is written to
  `results/$(date +%Y%m%d)/schedule.tsv` before any run starts.
- Runs are randomised across cells and N-values; the randomisation is
  deterministic for the same seed.

### Resuming after interruption

The script skips any run directory where `aggregated.csv` already exists.
Re-running with the same `--seed` and `--results-dir` resumes where it left
off:

```bash
bash scripts/run-matrix.sh \
  --seed 42 \
  --results-dir results/$(date +%Y%m%d)/
```

### Dry-run preview

```bash
bash scripts/run-matrix.sh --seed 42 --dry-run
# Prints the full 190-run schedule and exits; no rig activity.
```

### Subset runs

```bash
# Run only M1.3 and M1.4 (useful for targeted re-runs after outlier replacement).
bash scripts/run-matrix.sh \
  --seed 42 \
  --cells M1.3,M1.4 \
  --results-dir results/$(date +%Y%m%d)/
```

### Per-run artifacts

Each run lands in `results/run-NNN-<cell>-N<N>-rep<r>/`:

```
run-meta.yaml       pre-run sidecar (seed, position, cell, N, rep, flags)
manifest.yaml       harness metadata; presence = CSV complete
rpc_counts.csv      per-tick JetStream RPC counters
cgroup_io.raw       cgroup v2 io.stat (primary IOPS source)
iostat.raw          host-level cross-check
jsz.raw             NATS server stats (ndjson)
node_exporter.prom  host sanity metrics
aggregated.csv      produced by the aggregator after each run
```

The campaign summary is written to `results/campaign-manifest.yaml`.

### Failure artifacts

If a run fails, the script writes one or more sentinel files alongside the
run artifacts:

| File | Meaning |
|---|---|
| `failed.txt` | Harness binary exited non-zero. |
| `capture-failed.txt` | A mandatory capture file was missing or empty at run end. |
| `aggregate-failed.txt` | The aggregator step exited non-zero. |

A run is counted as failed in the campaign tally if **any** sentinel file
exists. Check `results/campaign-manifest.yaml` for a tally after the campaign.

---

## 5. Outlier handling (Tukey fences)

Per §R5 of `docs/plans/iops-investigation/00-attribution-plan.md`:

1. For each (cell, N) pair, compute the median and IQR of the **run-level
   means** of `iops_read + iops_write` across the 5 replicates.
2. A replicate whose mean falls outside `median ± 1.5 × IQR` is flagged as
   an outlier.
3. **Re-run once** at a new random position in the schedule (append to the
   existing results dir; the new position number continues the existing
   sequence).
4. If the replacement run is **also** outside the fence, flag the cell/N
   combination as "noisy" in `results/campaign-manifest.yaml` and proceed
   without replacement. Do **not** discard both: keep all 6 replicates and
   let Phase 6 analysis note the flag.

`scripts/analyze.py` identifies Tukey outliers automatically on every run
(no extra flag needed) and writes them to `analysis/tukey_outliers.tsv`. To
check mid-campaign without the M4 calibration file:

```bash
python3 scripts/analyze.py \
  --results-dir results/$(date +%Y%m%d)/ \
  --slopes-only \
  --out results/$(date +%Y%m%d)/analysis/
```

For a full attribution run (once M4 calibration is complete):

```bash
python3 scripts/analyze.py \
  --results-dir results/$(date +%Y%m%d)/ \
  --m4-calibration results/calibration/m4_$(date +%Y%m%d).csv \
  --out results/$(date +%Y%m%d)/analysis/
```

Read flagged runs from `analysis/tukey_outliers.tsv`.

Re-run flagged cells:

```bash
bash scripts/run-matrix.sh \
  --seed 42 \
  --cells M1.3 \
  --reps 1 \
  --results-dir results/$(date +%Y%m%d)/
```

---

## 6. M1.11 HEAD comparison (manual pin-swap)

M1.11 compares the v2.3.0 baseline against a HEAD build of parti.
`run-matrix.sh` **always refuses M1.11 automatically** — the pin-swap must be
a deliberate operator action.

### Steps

1. **Pin HEAD in go.mod:**

   ```bash
   cd test/iops-investigation
   go get github.com/arloliu/parti/v2@HEAD
   # Or use a local replace directive if HEAD is not pushed:
   # go mod edit -replace github.com/arloliu/parti/v2=../..
   go mod tidy
   ```

2. **Rebuild the harness with the new pin:**

   ```bash
   go build -o cmd/harness/harness ./cmd/harness
   ```

3. **Run M1.11 only** (same seed keeps position numbers contiguous):

   ```bash
   bash scripts/run-matrix.sh \
     --seed 42 \
     --cells M1.11 \
     --results-dir results/$(date +%Y%m%d)/
   ```

4. **Restore the pin and rebuild** before running any other cells:

   ```bash
   # Revert go.mod and go.sum to the v2.3.0 state.
   git checkout go.mod go.sum
   go build -o cmd/harness/harness ./cmd/harness
   ```

5. **Record the HEAD commit hash** in the M1.11 run's `notes.md`:

   ```
   parti HEAD: <commit hash>
   go.mod diff: <diff snippet>
   ```

---

## 7. Sharding across hosts

The 190-run campaign can be split across two hosts when the time budget is
tight. Each shard runs a disjoint subset of cells; Phase 6 merges the result
directories.

### Partition rule

Assign whole cells to hosts, not individual runs. Split by cell group:

| Host A | Host B |
|--------|--------|
| M1.0–M1.5 | M1.6–M1.11 |

Both hosts must use **the same `--seed`** and record runs into directories
with disjoint `--results-dir` paths that are merged before Phase 6.

### Pre-shard checklist

- Both hosts must pass the same cgroup v2 / `iostat` / Docker preflight checks
  listed in §1.
- Run Gate 1 (M4 calibration) independently on each host; record both
  calibration CSVs. Phase 6 uses the calibration from the host that ran each
  cell.
- Run Gate 2 (M1.0 control) **on Host A only** (M1.0 is assigned to Host A in
  the table above).
- Both hosts must independently pass the smoke test (`TestE2E_HarnessToAggregatedCSV`).

### Merge before Phase 6

```bash
# On the analysis machine: copy Host B results into Host A's results dir.
rsync -av hostb:~/results/$(date +%Y%m%d)/ results/$(date +%Y%m%d)/
```

Verify the merged schedule is contiguous (no duplicate run numbers) before
running `analyze.py`.

---

## 8. Failure modes

| Symptom | Diagnosis | Remediation |
|---|---|---|
| `manifest.yaml` missing after a run | Harness exited non-zero during warmup or capture. | Check `failed.txt` for the error message. Re-run the cell. |
| `aggregate-failed.txt` present | Aggregator's cgroup/iostat cross-check tripped. | Check `aggregated.csv`-adjacent stderr in `aggregate-failed.txt`. If the host had noisy background I/O, isolate and re-run. |
| `capture-failed.txt` present | `cgroup_io.raw` or `iostat.raw` was empty. | Verify cgroup v2 is active and the containers are using the right cgroup driver. |
| `cgroup_io.raw` rows are all zeros | Container cgroup path not found under the systemd hierarchy. | Check `scripts/capture-cgroup-io.sh --dry-run` for the resolved cgroup paths; adjust `--containers` list. |
| `node_exporter.prom` missing | node_exporter unreachable. | Not fatal: the script writes a one-line stub. Warn in `campaign-manifest.yaml`. |
| Aggregator divergence > 5 % | Host background I/O inflated iostat totals. | Ensure no concurrent disk-intensive workloads during capture. If persistent, set `--max-disagreement-pct 10` only for the affected run; flag it in `notes.md`. |
| `MDE_slope ≥ 0.167` at Gate 3 | Rig too noisy to detect H1 signal. | Identify and eliminate host noise sources. Re-run M1.0. Do not proceed to the knob matrix until the gate passes. |
| Harness `Run` returns "degraded" | A parti worker entered the degraded state during warmup. | Transient; re-run. If persistent, check NATS cluster health (`make reset`) and network stability. |

---

## 9. Handoff to Phase 6

Phase 5 is complete when all 190 `aggregated.csv` files exist and
`results/campaign-manifest.yaml` shows zero unresolved failures (Tukey
replacements counted as resolved if re-run or explicitly flagged "noisy").

### Deliverables for Phase 6

| Artifact | Location |
|---|---|
| Per-run `aggregated.csv` files (190) | `results/$(date +%Y%m%d)/run-*/aggregated.csv` |
| Campaign manifest | `results/$(date +%Y%m%d)/campaign-manifest.yaml` |
| Pre-registered schedule | `results/$(date +%Y%m%d)/schedule.tsv` |
| M4 calibration CSV | `results/calibration/m4_<date>.csv` |
| M1.11 `notes.md` | `results/$(date +%Y%m%d)/run-*-M1.11-*/notes.md` |

### Phase 6 entry command

```bash
python3 scripts/analyze.py \
  --results-dir results/$(date +%Y%m%d)/ \
  --m4-calibration results/calibration/m4_<date>.csv \
  [--out results/$(date +%Y%m%d)/analysis/]
```

The `aggregated.csv` column schema consumed by `analyze.py` is:

```
t_s, node,
iops_read, iops_write, bytes_read, bytes_write,
rpc_read_<bucket>..., rpc_write_<bucket>...,
stream_msgs_<name>..., stream_bytes_<name>...
```

`node="host"` rows carry RPC and stream rates; `node=<container>` rows carry
per-container disk IOPS. Phase 6 uses the host rows for slope estimation and
the container rows for attribution cross-checks.

### Quick completeness check before handoff

```bash
# Confirm all 190 aggregated.csv files are present and non-empty.
find results/$(date +%Y%m%d)/run-* -name aggregated.csv | wc -l
# Expected: 190

# Confirm no unresolved failures.
grep -l failed results/$(date +%Y%m%d)/run-*/*.txt 2>/dev/null | wc -l
# Expected: 0 (or all are Tukey-flagged noisy cells documented in campaign-manifest.yaml)
```

---

## 10. Phase 6: analysis

Once all 190 `aggregated.csv` files are in place and the campaign-manifest
shows zero unresolved failures, run the Phase 6 analyser. It produces the
four CSV tables that `docs/plans/iops-investigation/findings.md` consumes:
`slope_table.csv`, `attribution_table.csv`, `mitigation_table.csv`, and
`mde.csv` — plus a `tukey_outliers.tsv` sidecar cross-checking the outlier
log from §4.

### 9.1 Set up the venv

`scripts/analyze.py` is the only Python in the rig (the stats glue is more
ergonomic in Python than Go; see `01-implementation-strategy.md` §Phase 6).
Pinned deps live in `scripts/requirements.txt`.

```bash
cd test/iops-investigation
python3 -m venv .venv
source .venv/bin/activate
pip install -r scripts/requirements.txt
```

Required: Python ≥ 3.10. Deps: `pandas`, `numpy`, `statsmodels`,
`PyYAML` (statsmodels pulls scipy transitively, which `analyze.py` uses
for the t-critical lookup).

### 9.2 Run the analyser

```bash
python3 scripts/analyze.py \
  --results-dir results/$(date +%Y%m%d)/ \
  --m4-calibration results/calibration/m4_<date>.csv
```

Default output dir: `results/<campaign>/analysis/`. Override with
`--out`. Pass `--strict` to fail loud on any missing cell, malformed
CSV, or unmapped bucket (recommended for the final pass; off by
default so an incomplete campaign still produces a partial report).

Override the predicted-direction map for mitigation verification by
passing a YAML file at `--predictions-yaml`:

```yaml
- baseline: M1.2
  ablation: M1.3
  column: read_rpc_ops
  direction: -1        # -1 = decrease, +1 = increase, 0 = within MDE
```

The default predictions table inside `analyze.py` covers the M1
ablations as specified in §M1 of the attribution plan; override only
when adding ablations or correcting an off-by-one.

### 9.3 Run the unit tests

```bash
cd test/iops-investigation
source .venv/bin/activate
python3 -m unittest scripts.test_analyze
```

The tests synthesise a tiny `results/` tree with known slopes and
assert recovery to within ~5 %. They auto-skip if the venv is not
active (so day-to-day Go test runs are not blocked).

### 9.4 Write the findings

Open `docs/plans/iops-investigation/findings.md`. Replace every
`<TBD>` with the corresponding number from the analyser output:

| `findings.md` section | Source |
|---|---|
| §2 per-N table | mean(measured) per cell+N from per-run `aggregated.csv` (or compute directly from `attribution_table.csv`) |
| §2 slope table | `slope_table.csv` rows where `config = M1.2` |
| §2 attribution rows | `attribution_table.csv` rows where `config = M1.2 AND N = 2000`; group buckets by subsystem using the `DEFAULT_BUCKET_MAP` in `analyze.py` |
| §3 hypothesis evidence | `slope_table.csv` for the relevant cell pairs + `mde.csv` |
| §4 mitigation ranking | `mitigation_table.csv` filtered to `verified = true`, sorted by `|slope_delta|` on `block_write_iops` |
| §6 raw data pointers | `campaign-manifest.yaml`, `schedule.tsv`, the M4 calibration CSV |
| §7 open questions | Inspect `tukey_outliers.tsv` and any `WARNING:` lines on stderr from `analyze.py` |

Interpretation is the Opus 4.7 xhigh part of the work — the runbook
intentionally does not script it.
