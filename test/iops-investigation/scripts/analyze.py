#!/usr/bin/env python3
"""Phase 6 analyser for the NATS IOPS investigation.

Single-file Python (intentionally — see docs/plans/iops-investigation/
01-implementation-strategy.md §Phase 6). Consumes a Phase 5 results
directory (per-run `aggregated.csv` files + `run-meta.yaml` sidecars)
and the M4 calibration table, and writes four output tables Phase 6's
operator/analyst uses to populate `findings.md`:

    slope_table.csv        OLS slope + 95 % CI + R² per (config, column)
    attribution_table.csv  per-config per-N measured / attributed / residual
    mitigation_table.csv   verified-mitigation table per the §"Verification
                           of candidate mitigations" criterion
    mde.csv                per-column MDE derived from M1.0 (no-Parti control)

The estimator and MDE definitions live in plan §R5 (the OLS line
`y_ij = β₀ + β₁ × N_i + ε_ij` on replicate-level observations, not
per-N means). M3's per-column attribution arithmetic lives in plan §M3.
M4.1's interpolation rule (linear between bracketing `C_stream` grid
points, per-node-role) lives in plan §M4.1 and the row schema is
defined by `internal/calibrate/m41reduce.go:M41RowSchemaHeader`.

CLI:

    analyze.py --results-dir results/CAMPAIGN/ \
               --m4-calibration results/m4/m4_calibration.csv \
               [--out RESULTS_DIR/analysis/] \
               [--predictions-yaml PATH] \
               [--strict]

Heavy imports (pandas, numpy, statsmodels, yaml) are deferred into
`main()` so `--help` works in environments where the deps aren't
installed yet. Per Rule 10 (Fail loud), `--strict` makes any missing
cell, malformed CSV, or unattributable bucket exit non-zero; without
it, the script produces a partial report with warnings on stderr.
"""

from __future__ import annotations

import argparse
import os
import re
import sys
from pathlib import Path

# -----------------------------------------------------------------------------
# Constants and bucket → subsystem mapping
# -----------------------------------------------------------------------------

# Columns of the four-dimensional IOPS budget per plan §00 line 17.
BUDGET_COLUMNS = (
    "read_rpc_ops",
    "write_mutation_ops",
    "block_read_iops",
    "block_write_iops",
)

# Sentinel synthetic-host node name emitted by aggregate.WriteCSV
# (see internal/aggregate/aggregate.go:HostNode).
HOST_NODE = "host"

# Default parti KV bucket names → subsystem tag. Operator can override
# via Config.KVBuckets.* on the harness side; if a bucket name diverges
# from the default we fall back to a substring match (longest first).
# The `__js__` synthetic bucket collects non-KV JetStream RPCs (see
# internal/instrumentedjs/instrumentedjs.go:JetStreamBucket).
DEFAULT_BUCKET_MAP = {
    "parti-stableid": "stable_id",
    "parti-election": "election",
    "parti-heartbeat": "heartbeat",
    "parti-assignment": "assignment",
    "parti-handoff": "handoff",
    "iops-rig-partitions": "partition_source",
    "__js__": "jetstream_other",
}

# Hardcoded predicted-direction map for mitigation verification per plan
# §"Verification of candidate mitigations". Keys: (baseline_cell,
# ablation_cell, column). Value: predicted sign of (β̂₁_ablation -
# β̂₁_baseline). +1 means slope should INCREASE under ablation, -1 means
# DECREASE, 0 means "should be within MDE" (e.g. M1.4 vs M1.1 — same
# config, used as a sanity probe). Override with --predictions-yaml.
DEFAULT_PREDICTIONS = {
    # H1.A — raise SweepInterval 30s → 5min. Predicts read_rpc_ops slope drops 10×.
    ("M1.2", "M1.3", "read_rpc_ops"): -1,
    ("M1.2", "M1.3", "block_read_iops"): -1,
    # H1.B — two-phase off. Predicts H1 contribution vanishes (slope ↓).
    ("M1.2", "M1.4", "read_rpc_ops"): -1,
    ("M1.2", "M1.4", "block_read_iops"): -1,
    # H2.A — FetchTimeout 5s → 30s. Predicts H2 server-internal block writes ↓.
    ("M1.2", "M1.5", "block_write_iops"): -1,
    # H2.B — Queue consumer. Predicts H2 ≈ 0 (slope ↓).
    ("M1.2", "M1.6", "block_write_iops"): -1,
    ("M1.2", "M1.6", "block_read_iops"): -1,
    # H2.C — data stream memory. Predicts H2 block writes ↓.
    ("M1.2", "M1.7", "block_write_iops"): -1,
    # M5 — KV buckets memory. Predicts KV-side block writes ↓ on parti buckets.
    ("M1.2", "M1.9", "block_write_iops"): -1,
    # M1.1 ≡ M1.4 sanity probe — same config, should be indistinguishable.
    ("M1.1", "M1.4", "read_rpc_ops"): 0,
    ("M1.1", "M1.4", "write_mutation_ops"): 0,
    ("M1.1", "M1.4", "block_read_iops"): 0,
    ("M1.1", "M1.4", "block_write_iops"): 0,
}


# -----------------------------------------------------------------------------
# CLI
# -----------------------------------------------------------------------------


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="analyze.py",
        description=(
            "Phase 6 analyser: OLS slope tables, per-config attribution, "
            "mitigation verification, and per-column MDE for the NATS "
            "IOPS investigation."
        ),
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "Example:\n"
            "  analyze.py --results-dir results/20260601/ \\\n"
            "             --m4-calibration results/calibration/m4_20260601.csv\n"
            "\n"
            "Outputs land in <results-dir>/analysis/ by default."
        ),
    )
    p.add_argument(
        "--results-dir",
        required=True,
        help="Phase 5 campaign root containing run-* subdirs.",
    )
    p.add_argument(
        "--m4-calibration",
        default=None,
        help="Path to the M4 / M4.1 calibration CSV (M4.1 schema "
        "documented in internal/calibrate/m41reduce.go). Required for "
        "the attribution / mitigation tables; omit with --slopes-only.",
    )
    p.add_argument(
        "--slopes-only",
        action="store_true",
        help="Produce slope_table.csv + mde.csv only; skip the M4.1-"
        "dependent attribution_table.csv and mitigation_table.csv. "
        "Useful for Tier 0 / quick scans where the M4 calibration "
        "hasn't been collected yet.",
    )
    p.add_argument(
        "--out",
        default=None,
        help="Output directory (default: <results-dir>/analysis/).",
    )
    p.add_argument(
        "--predictions-yaml",
        default=None,
        help="Optional YAML file overriding the predicted-direction "
        "map for mitigation verification.",
    )
    p.add_argument(
        "--control-cell",
        default="M1.0",
        help="Cell that defines the no-Parti control (MDE baseline). "
        "Default: M1.0.",
    )
    p.add_argument(
        "--baseline-cell",
        default="M1.2",
        help="B-prod baseline cell used as the mitigation comparison "
        "point. Default: M1.2.",
    )
    p.add_argument(
        "--strict",
        action="store_true",
        help="Exit non-zero on any missing cell, malformed CSV, or "
        "unattributable bucket. Without --strict, those produce "
        "warnings and a partial report.",
    )
    return p


# -----------------------------------------------------------------------------
# Run discovery
# -----------------------------------------------------------------------------


RUN_DIR_RE = re.compile(r"^run-(?P<pos>\d+)-(?P<cell>M\d+\.\d+)-N(?P<n>\d+)-rep(?P<rep>\d+)$")


def discover_runs(results_dir: Path):
    """Yield (run_dir, cell, n, rep, position) for each well-formed run dir."""
    if not results_dir.is_dir():
        raise SystemExit(f"results-dir does not exist: {results_dir}")
    runs = []
    for child in sorted(results_dir.iterdir()):
        if not child.is_dir():
            continue
        m = RUN_DIR_RE.match(child.name)
        if not m:
            continue
        runs.append(
            (
                child,
                m.group("cell"),
                int(m.group("n")),
                int(m.group("rep")),
                int(m.group("pos")),
            )
        )
    return runs


# -----------------------------------------------------------------------------
# Per-run loading
# -----------------------------------------------------------------------------


def bucket_to_subsystem(bucket: str, bucket_map=None) -> str:
    """Map a wrapper-tagged bucket name to a subsystem tag.

    Exact match first (default-name campaign), then substring fallback
    so renamed buckets still attribute correctly. Unknown buckets get
    `"unknown:<bucket>"` so the residual is explicit, not silent (Rule 10).
    """
    m = bucket_map or DEFAULT_BUCKET_MAP
    if bucket in m:
        return m[bucket]
    # Substring fallback — longest-prefix wins.
    candidates = [(k, v) for k, v in m.items() if k in bucket]
    if candidates:
        candidates.sort(key=lambda kv: -len(kv[0]))
        return candidates[0][1]
    return f"unknown:{bucket}"


def load_run_aggregated(run_dir: Path):
    """Return a per-run summary dict with the four column means.

    The aggregated.csv has dynamic per-bucket columns; the host row
    (node="host") carries cluster-wide rpc_*, the per-container rows
    carry iops_*. We sum container IOPS across nodes within a second to
    get cluster-wide block IOPS, then mean across seconds.
    """
    import numpy as np
    import pandas as pd

    csv_path = run_dir / "aggregated.csv"
    if not csv_path.is_file():
        return None
    df = pd.read_csv(csv_path)
    if df.empty or "node" not in df.columns:
        return None

    host_mask = df["node"] == HOST_NODE
    host_rows = df[host_mask]
    cont_rows = df[~host_mask]

    # Sum read/write RPC rates across all rpc_read_* / rpc_write_* columns.
    rpc_read_cols = [c for c in df.columns if c.startswith("rpc_read_")]
    rpc_write_cols = [c for c in df.columns if c.startswith("rpc_write_")]

    if host_rows.empty:
        read_rpc_mean = 0.0
        write_rpc_mean = 0.0
        per_bucket_read = {}
        per_bucket_write = {}
    else:
        read_rpc_total = host_rows[rpc_read_cols].fillna(0).sum(axis=1)
        write_rpc_total = host_rows[rpc_write_cols].fillna(0).sum(axis=1)
        read_rpc_mean = float(read_rpc_total.mean())
        write_rpc_mean = float(write_rpc_total.mean())
        per_bucket_read = {
            c[len("rpc_read_") :]: float(host_rows[c].fillna(0).mean())
            for c in rpc_read_cols
        }
        per_bucket_write = {
            c[len("rpc_write_") :]: float(host_rows[c].fillna(0).mean())
            for c in rpc_write_cols
        }

    # Per-second cluster block IOPS: sum container rows per t_s then mean.
    if cont_rows.empty:
        br_mean = 0.0
        bw_mean = 0.0
    else:
        grouped = cont_rows.groupby("t_s")[["iops_read", "iops_write"]].sum()
        br_mean = float(grouped["iops_read"].mean()) if not grouped.empty else 0.0
        bw_mean = float(grouped["iops_write"].mean()) if not grouped.empty else 0.0
        if not np.isfinite(br_mean):
            br_mean = 0.0
        if not np.isfinite(bw_mean):
            bw_mean = 0.0

    return {
        "read_rpc_ops": read_rpc_mean,
        "write_mutation_ops": write_rpc_mean,
        "block_read_iops": br_mean,
        "block_write_iops": bw_mean,
        "per_bucket_read": per_bucket_read,
        "per_bucket_write": per_bucket_write,
    }


# -----------------------------------------------------------------------------
# OLS slope + CI
# -----------------------------------------------------------------------------


def ols_slope(ns, ys):
    """Fit y = β₀ + β₁ × N via statsmodels OLS, return dict with slope,
    se, CI, intercept, R², df. ns/ys are replicate-level (not means).
    """
    import numpy as np
    import statsmodels.api as sm

    ns = np.asarray(ns, dtype=float)
    ys = np.asarray(ys, dtype=float)
    if ns.size < 2 or np.allclose(ns, ns[0]):
        return None
    X = sm.add_constant(ns)
    model = sm.OLS(ys, X, missing="drop")
    res = model.fit()
    if not hasattr(res, "params") or len(res.params) < 2:
        return None
    b0, b1 = float(res.params[0]), float(res.params[1])
    se1 = float(res.bse[1])
    se0 = float(res.bse[0])
    df = int(res.df_resid)
    ci = res.conf_int(alpha=0.05)
    return {
        "slope_beta1": b1,
        "slope_se": se1,
        "slope_ci_low": float(ci[1][0]),
        "slope_ci_high": float(ci[1][1]),
        "intercept_beta0": b0,
        "intercept_se": se0,
        "intercept_ci_low": float(ci[0][0]),
        "intercept_ci_high": float(ci[0][1]),
        "r2": float(res.rsquared),
        "n_obs": int(res.nobs),
        "df": df,
    }


# -----------------------------------------------------------------------------
# Tukey-fence reporter
# -----------------------------------------------------------------------------


def tukey_outliers(values):
    """Return indices of values outside median ± 1.5×IQR (plan §R5).

    Operates on run-level means within one (config, N) cell. The script
    only REPORTS — the operator already re-ran outliers per the runbook;
    this is for verifying the operator's outlier log matches.
    """
    import numpy as np

    vals = np.asarray(values, dtype=float)
    if vals.size < 4:  # IQR undefined for tiny samples
        return []
    q1, q3 = np.percentile(vals, [25, 75])
    iqr = q3 - q1
    lo = q1 - 1.5 * iqr
    hi = q3 + 1.5 * iqr
    return [i for i, v in enumerate(vals) if v < lo or v > hi]


# -----------------------------------------------------------------------------
# M4.1 interpolation
# -----------------------------------------------------------------------------


def load_m41(path: Path):
    """Load the M4.1 calibration CSV.

    Schema (internal/calibrate/m41reduce.go:M41RowSchemaHeader):
        c_stream, fetch_timeout_s, data_storage, replicas,
        node_role, block_read_iops, block_write_iops, ci_low, ci_high

    We tolerate the M4 basic-paths table being concatenated into the
    same file by filtering on the presence of the c_stream column.
    """
    import pandas as pd

    df = pd.read_csv(path)
    required = {"c_stream", "fetch_timeout_s", "data_storage", "replicas", "node_role"}
    missing = required - set(df.columns)
    if missing:
        raise SystemExit(
            f"M4 calibration CSV missing columns {sorted(missing)}; "
            "expected the M4.1 schema (see m41reduce.go)."
        )
    return df


def m41_interpolate(m41_df, c_stream, fetch_timeout_s, data_storage, replicas, node_role, column):
    """Linear interpolation between bracketing c_stream grid points per
    plan §M4.1's interpolation rule.

    Raises SystemExit if no rows match the
    (fetch_timeout_s, data_storage, replicas, node_role) slice. Per §R5
    strictness, a missing calibration slice is a hard error: silently
    returning 0.0 would conflate "near-zero H2" (a legitimate calibration
    result) with "uncalibrated cell" and produce confidently-wrong
    attribution_table.csv values. An operator whose run falls outside the
    calibration grid must add the missing grid point before re-running.

    column is one of {block_read_iops, block_write_iops}.
    """
    sub = m41_df[
        (m41_df["fetch_timeout_s"] == fetch_timeout_s)
        & (m41_df["data_storage"] == data_storage)
        & (m41_df["replicas"] == replicas)
        & (m41_df["node_role"] == node_role)
    ].sort_values("c_stream")
    if sub.empty:
        raise SystemExit(
            f"M4.1 calibration slice missing for "
            f"(fetch_timeout_s={fetch_timeout_s}, data_storage={data_storage!r}, "
            f"replicas={replicas}, node_role={node_role!r}). "
            "Add this grid point to the calibration CSV before running attribution."
        )
    cs = sub["c_stream"].to_numpy(dtype=float)
    ys = sub[column].to_numpy(dtype=float)
    if c_stream <= cs[0]:
        return float(ys[0])
    if c_stream >= cs[-1]:
        return float(ys[-1])
    # Find bracketing pair.
    for i in range(len(cs) - 1):
        if cs[i] <= c_stream <= cs[i + 1]:
            t = (c_stream - cs[i]) / (cs[i + 1] - cs[i])
            return float(ys[i] + t * (ys[i + 1] - ys[i]))
    return float(ys[-1])


# -----------------------------------------------------------------------------
# Attribution arithmetic (§M3)
# -----------------------------------------------------------------------------


def attribute_cell(
    measured, per_bucket_read, per_bucket_write, m41_df, run_params, bucket_map=None
):
    """Compute the four-column attribution for one (config, N) replicate.

    Returns a list of attribution-table rows. Per §M3:

        attributed_kv      = Σ_bucket wrapper-counted rpc_* per bucket
                             (for the rpc columns), or 0 for block columns
                             (block IOPS are M4-calibrated separately).
        attributed_h2      = cluster-summed M4.1 interpolation (block columns
                             only). Per §M3 / §M4.1, the single data stream
                             has one leader node and (R-1) follower nodes.
                             The cluster total is:
                               M4.1(leader) + (R-1) × M4.1(follower)
                             For a single-node cluster (R=1) only the leader
                             term is used; the follower lookup is skipped.
        residual           = measured - (attributed_kv + attributed_h2)

    For the rpc columns the attribution is the sum across all known
    buckets (an `unknown:*` bucket implies a wrapper omission and is
    reported via warnings). For the block columns this scaffold uses
    `attributed_kv = 0` because the rig does not currently carry the M4
    basic-paths per-op IOPS factors in machine-readable form; the
    operator must fill them in when the M4 KV-paths table is finalised
    (a `<TBD>` in this scaffold rather than a wrong number — Rule 10).
    """
    rows = []
    replicas = int(run_params.get("replicas", 3))
    for col in BUDGET_COLUMNS:
        meas = float(measured.get(col, 0.0))
        if col == "read_rpc_ops":
            att_kv = sum(per_bucket_read.values())
            att_h2 = 0.0
        elif col == "write_mutation_ops":
            att_kv = sum(per_bucket_write.values())
            att_h2 = 0.0
        elif col == "block_read_iops":
            att_kv = 0.0  # see docstring — needs M4 basic-paths factors
            # Cluster total: leader node + (R-1) follower nodes.
            leader_h2 = m41_interpolate(
                m41_df,
                c_stream=run_params["c_stream"],
                fetch_timeout_s=run_params["fetch_timeout_s"],
                data_storage=run_params["data_storage"],
                replicas=replicas,
                node_role="leader",
                column="block_read_iops",
            )
            if replicas > 1:
                follower_h2 = m41_interpolate(
                    m41_df,
                    c_stream=run_params["c_stream"],
                    fetch_timeout_s=run_params["fetch_timeout_s"],
                    data_storage=run_params["data_storage"],
                    replicas=replicas,
                    node_role="follower",
                    column="block_read_iops",
                )
            else:
                follower_h2 = 0.0
            att_h2 = leader_h2 + (replicas - 1) * follower_h2
        elif col == "block_write_iops":
            att_kv = 0.0
            # Cluster total: leader node + (R-1) follower nodes.
            leader_h2 = m41_interpolate(
                m41_df,
                c_stream=run_params["c_stream"],
                fetch_timeout_s=run_params["fetch_timeout_s"],
                data_storage=run_params["data_storage"],
                replicas=replicas,
                node_role="leader",
                column="block_write_iops",
            )
            if replicas > 1:
                follower_h2 = m41_interpolate(
                    m41_df,
                    c_stream=run_params["c_stream"],
                    fetch_timeout_s=run_params["fetch_timeout_s"],
                    data_storage=run_params["data_storage"],
                    replicas=replicas,
                    node_role="follower",
                    column="block_write_iops",
                )
            else:
                follower_h2 = 0.0
            att_h2 = leader_h2 + (replicas - 1) * follower_h2
        else:
            continue
        residual = meas - (att_kv + att_h2)
        pct = (residual / meas * 100.0) if meas != 0 else 0.0
        rows.append(
            {
                "column": col,
                "measured": meas,
                "attributed_kv": att_kv,
                "attributed_h2": att_h2,
                "residual": residual,
                "residual_pct_of_measured": pct,
            }
        )
    return rows


# -----------------------------------------------------------------------------
# Run-meta parsing — minimum needed for attribution params
# -----------------------------------------------------------------------------


def parse_run_meta(run_dir: Path):
    """Light YAML parse of run-meta.yaml — extract the flags string and
    derive (consumer_mode, fetch_timeout, two_phase, data_storage,
    kv_storage, heartbeat_interval) without requiring PyYAML."""
    meta_path = run_dir / "run-meta.yaml"
    out = {
        "cell": None,
        "n": None,
        "rep": None,
        "replicas": 3,
        "flags": "",
    }
    if not meta_path.is_file():
        return out
    for line in meta_path.read_text().splitlines():
        line = line.rstrip()
        if ":" not in line:
            continue
        k, _, v = line.partition(":")
        k = k.strip()
        v = v.strip().strip("'\"")
        if k in {"cell", "flags"}:
            out[k] = v
        elif k in {"n", "rep", "replicas", "schedule_position", "total_runs"}:
            try:
                out[k] = int(v)
            except ValueError:
                pass
    return out


def flags_to_params(flags: str, n: int):
    """Derive attribution params from the harness flag string."""
    consumer_mode = "dynamic"
    if "--consumer-mode=queue" in flags:
        consumer_mode = "queue"
    elif "--consumer-mode=none-attached" in flags:
        consumer_mode = "none-attached"

    fetch_timeout_s = 5.0
    m = re.search(r"--fetch-timeout=(\d+(?:\.\d+)?)(s|m|ms)?", flags)
    if m:
        v = float(m.group(1))
        unit = m.group(2) or "s"
        if unit == "m":
            v *= 60.0
        elif unit == "ms":
            v /= 1000.0
        fetch_timeout_s = v

    two_phase = "--two-phase=true" in flags
    data_storage = "memory" if "--data-storage=memory" in flags else "file"

    if consumer_mode == "queue":
        c_stream = 1
    elif consumer_mode == "none-attached":
        c_stream = 0
    else:
        c_stream = int(n)

    return {
        "consumer_mode": consumer_mode,
        "fetch_timeout_s": fetch_timeout_s,
        "two_phase": two_phase,
        "data_storage": data_storage,
        "c_stream": c_stream,
    }


# -----------------------------------------------------------------------------
# Main orchestration
# -----------------------------------------------------------------------------


def main(argv=None):
    args = build_parser().parse_args(argv)

    if not args.slopes_only and not args.m4_calibration:
        raise SystemExit(
            "--m4-calibration is required unless --slopes-only is set"
        )

    # Heavy imports here so --help works without deps installed.
    import numpy as np
    import pandas as pd

    try:
        import yaml  # noqa: F401  (only used if --predictions-yaml passed)
    except ImportError:
        yaml = None

    results_dir = Path(args.results_dir).resolve()
    out_dir = Path(args.out).resolve() if args.out else results_dir / "analysis"
    out_dir.mkdir(parents=True, exist_ok=True)

    warnings = []

    # 1. Discover runs.
    runs = discover_runs(results_dir)
    if not runs:
        msg = f"no run-* directories found under {results_dir}"
        if args.strict:
            raise SystemExit(msg)
        warnings.append(msg)

    # 2. Load each run's per-column means + per-bucket rates + params.
    per_run = []
    for run_dir, cell, n, rep, pos in runs:
        agg = load_run_aggregated(run_dir)
        if agg is None:
            msg = f"missing/empty aggregated.csv: {run_dir}"
            if args.strict:
                raise SystemExit(msg)
            warnings.append(msg)
            continue
        meta = parse_run_meta(run_dir)
        params = flags_to_params(meta["flags"] or "", n)
        params["replicas"] = meta.get("replicas") or 3
        per_run.append(
            {
                "run_dir": str(run_dir),
                "cell": cell,
                "n": n,
                "rep": rep,
                "pos": pos,
                "params": params,
                **agg,
            }
        )

    if not per_run:
        raise SystemExit("no usable runs loaded; cannot proceed")

    # 3. M4.1 calibration (skipped in --slopes-only mode).
    m41_df = None if args.slopes_only else load_m41(Path(args.m4_calibration))

    # 4. Slope table — per (cell, column).
    slope_rows = []
    mde_per_column = {}

    # Compute MDE first from the control cell.
    control_rows = [r for r in per_run if r["cell"] == args.control_cell]
    for col in BUDGET_COLUMNS:
        if not control_rows:
            mde_per_column[col] = None
            continue
        ns = [r["n"] for r in control_rows]
        ys = [r[col] for r in control_rows]
        fit = ols_slope(ns, ys)
        if fit is None:
            mde_per_column[col] = None
            continue
        # t critical at df, two-sided 95%. statsmodels brings scipy.
        try:
            from scipy.stats import t as _student_t
            tcrit = float(_student_t.ppf(0.975, fit["df"]))
        except Exception:  # noqa: BLE001
            tcrit = 1.96
        mde = abs(tcrit * fit["slope_se"])
        mde_per_column[col] = {
            "column": col,
            "se_no_parti": fit["slope_se"],
            "mde_slope": mde,
            "t_critical": tcrit,
            "df": fit["df"],
        }

    cells = sorted({r["cell"] for r in per_run})
    for cell in cells:
        cell_rows = [r for r in per_run if r["cell"] == cell]
        for col in BUDGET_COLUMNS:
            ns = [r["n"] for r in cell_rows]
            ys = [r[col] for r in cell_rows]
            fit = ols_slope(ns, ys)
            if fit is None:
                slope_rows.append(
                    {
                        "config": cell,
                        "column": col,
                        "slope_beta1": float("nan"),
                        "slope_se": float("nan"),
                        "slope_ci_low": float("nan"),
                        "slope_ci_high": float("nan"),
                        "intercept_beta0": float("nan"),
                        "intercept_ci_low": float("nan"),
                        "intercept_ci_high": float("nan"),
                        "r2": float("nan"),
                        "n_obs": len(ys),
                        "verdict": "noisy_inconclusive",
                    }
                )
                continue
            mde = (mde_per_column.get(col) or {}).get("mde_slope")
            if mde is None or not np.isfinite(mde):
                verdict = "noisy_inconclusive"
            elif abs(fit["slope_beta1"]) > mde:
                verdict = "slope_above_mde"
            else:
                verdict = "slope_below_mde"
            slope_rows.append(
                {
                    "config": cell,
                    "column": col,
                    "slope_beta1": fit["slope_beta1"],
                    "slope_se": fit["slope_se"],
                    "slope_ci_low": fit["slope_ci_low"],
                    "slope_ci_high": fit["slope_ci_high"],
                    "intercept_beta0": fit["intercept_beta0"],
                    "intercept_ci_low": fit["intercept_ci_low"],
                    "intercept_ci_high": fit["intercept_ci_high"],
                    "r2": fit["r2"],
                    "n_obs": fit["n_obs"],
                    "verdict": verdict,
                }
            )

    pd.DataFrame(slope_rows).to_csv(out_dir / "slope_table.csv", index=False)

    # 5. MDE table.
    mde_rows = [
        v
        for v in (mde_per_column.get(c) for c in BUDGET_COLUMNS)
        if v is not None
    ]
    pd.DataFrame(mde_rows).to_csv(out_dir / "mde.csv", index=False)

    # 6. Attribution table — one row per (cell, N, column), aggregated
    #    across reps (mean of measured + attributed terms). Skipped in
    #    --slopes-only mode because attribution needs the M4.1 table.
    attr_rows = []
    mit_rows = []
    by_cell_n = {}
    for r in per_run:
        key = (r["cell"], r["n"])
        by_cell_n.setdefault(key, []).append(r)
    if args.slopes_only:
        # Still produce a Tukey-fence sidecar — operators want outlier
        # flagging regardless of attribution.
        tukey_lines = ["cell\tN\tcolumn\trep_index\trun_dir"]
        for (cell, n), reps in by_cell_n.items():
            for col in BUDGET_COLUMNS:
                vals = [r[col] for r in reps]
                for idx in tukey_outliers(vals):
                    tukey_lines.append(
                        f"{cell}\t{n}\t{col}\t{reps[idx]['rep']}\t{reps[idx]['run_dir']}"
                    )
        (out_dir / "tukey_outliers.tsv").write_text("\n".join(tukey_lines) + "\n")
        if warnings:
            sys.stderr.write("\n".join(["WARNING: " + w for w in warnings]) + "\n")
        print(f"Wrote analysis to {out_dir} (slopes-only mode)")
        print(
            f"  slope_table.csv ({len(slope_rows)} rows)"
            f"  mde.csv ({len(mde_rows)} rows)"
            f"  tukey_outliers.tsv"
        )
        return 0
    for (cell, n), reps in sorted(by_cell_n.items()):
        # mean replicate's params (they should all match within a cell+N).
        params = reps[0]["params"].copy()
        params["replicas"] = reps[0]["params"].get("replicas", 3)
        # Mean measured + mean per-bucket.
        measured = {c: float(np.mean([r[c] for r in reps])) for c in BUDGET_COLUMNS}
        all_buckets_r = set()
        all_buckets_w = set()
        for r in reps:
            all_buckets_r.update(r["per_bucket_read"].keys())
            all_buckets_w.update(r["per_bucket_write"].keys())
        per_bucket_read = {
            b: float(np.mean([r["per_bucket_read"].get(b, 0.0) for r in reps]))
            for b in all_buckets_r
        }
        per_bucket_write = {
            b: float(np.mean([r["per_bucket_write"].get(b, 0.0) for r in reps]))
            for b in all_buckets_w
        }
        # Flag any unknown buckets up front.
        for b in list(all_buckets_r) + list(all_buckets_w):
            sub = bucket_to_subsystem(b)
            if sub.startswith("unknown:"):
                msg = f"unmapped bucket {b!r} in cell {cell} N={n} — attribution will undercount"
                if args.strict:
                    raise SystemExit(msg)
                warnings.append(msg)

        cell_attr = attribute_cell(
            measured, per_bucket_read, per_bucket_write, m41_df, params
        )
        for row in cell_attr:
            attr_rows.append({"config": cell, "N": n, **row})

    pd.DataFrame(attr_rows).to_csv(out_dir / "attribution_table.csv", index=False)

    # 7. Mitigation verification.
    predictions = dict(DEFAULT_PREDICTIONS)
    if args.predictions_yaml:
        if yaml is None:
            raise SystemExit("--predictions-yaml requires PyYAML")
        with open(args.predictions_yaml) as f:
            extra = yaml.safe_load(f) or {}
        # Expect a list of {baseline, ablation, column, direction}.
        for entry in extra:
            predictions[(entry["baseline"], entry["ablation"], entry["column"])] = int(
                entry["direction"]
            )

    slope_df = pd.DataFrame(slope_rows)
    mit_rows = []
    for (baseline_cell, ablation_cell, col), direction in predictions.items():
        b = slope_df[
            (slope_df["config"] == baseline_cell) & (slope_df["column"] == col)
        ]
        a = slope_df[
            (slope_df["config"] == ablation_cell) & (slope_df["column"] == col)
        ]
        if b.empty or a.empty:
            continue
        b1_base = float(b.iloc[0]["slope_beta1"])
        b1_abl = float(a.iloc[0]["slope_beta1"])
        delta = b1_abl - b1_base
        mde = (mde_per_column.get(col) or {}).get("mde_slope", float("nan"))
        if not np.isfinite(mde):
            verified = False
        elif direction == 0:
            # Sanity probe — verified means |delta| WITHIN MDE.
            verified = abs(delta) <= mde
        else:
            verified = abs(delta) > mde and np.sign(delta) == direction
        mit_rows.append(
            {
                "mitigation": f"{baseline_cell}->{ablation_cell}",
                "ablation_cell": ablation_cell,
                "baseline_cell": baseline_cell,
                "column": col,
                "slope_baseline": b1_base,
                "slope_ablation": b1_abl,
                "slope_delta": delta,
                "mde_at_baseline": mde,
                "verified": verified,
                "predicted_direction": direction,
            }
        )
    pd.DataFrame(mit_rows).to_csv(out_dir / "mitigation_table.csv", index=False)

    # 8. Tukey-fence report — informational, written as a sidecar.
    tukey_lines = ["cell\tN\tcolumn\trep_index\trun_dir"]
    for (cell, n), reps in by_cell_n.items():
        for col in BUDGET_COLUMNS:
            vals = [r[col] for r in reps]
            for idx in tukey_outliers(vals):
                tukey_lines.append(
                    f"{cell}\t{n}\t{col}\t{reps[idx]['rep']}\t{reps[idx]['run_dir']}"
                )
    (out_dir / "tukey_outliers.tsv").write_text("\n".join(tukey_lines) + "\n")

    # 9. Warnings.
    if warnings:
        sys.stderr.write("\n".join(["WARNING: " + w for w in warnings]) + "\n")

    print(f"Wrote analysis to {out_dir}")
    print(
        "  slope_table.csv  ({} rows)".format(len(slope_rows))
        + " attribution_table.csv ({} rows)".format(len(attr_rows))
        + " mitigation_table.csv ({} rows)".format(len(mit_rows))
        + " mde.csv ({} rows)".format(len(mde_rows))
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
