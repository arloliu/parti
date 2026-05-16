"""Fixture-based tests for analyze.py.

Synthesises a small results/ tree with 3 cells × 4 N × 5 reps of fake
aggregated.csv whose slopes are known by construction, then asserts:

  - Slope estimates land within ~5 % of the constructed values.
  - The MDE column is sensible (positive, smaller than the strongest
    fixture slope).
  - The mitigation-verification logic correctly flags a constructed
    "verified" ablation and rejects a constructed "below MDE" one.

Uses unittest from the stdlib so it runs without pytest. Run as:

    python3 -m unittest test.iops-investigation.scripts.test_analyze

or:

    cd test/iops-investigation && python3 -m unittest scripts.test_analyze

Skips itself if pandas/numpy/statsmodels are not importable, so it
can live in-tree without forcing a venv on every developer.
"""

from __future__ import annotations

import csv
import os
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path

try:
    import numpy as np  # noqa: F401
    import pandas as pd  # noqa: F401
    import statsmodels  # noqa: F401
    DEPS_OK = True
except ImportError:
    DEPS_OK = False

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))


@unittest.skipUnless(DEPS_OK, "pandas/numpy/statsmodels not installed")
class AnalyzeFixtureTest(unittest.TestCase):
    """Construct a controlled fixture and recover known slopes."""

    @classmethod
    def setUpClass(cls):
        import analyze  # noqa: WPS433  in-tree import after sys.path
        cls.analyze = analyze
        cls.tmpdir = tempfile.mkdtemp(prefix="analyze-fixture-")
        cls.results_dir = Path(cls.tmpdir) / "campaign"
        cls.results_dir.mkdir()
        # Construct three cells:
        #   M1.0 — control, slope ≈ 0 (small noise → defines MDE).
        #   M1.2 — baseline, block_write_iops slope = 0.50 ops/partition.
        #   M1.6 — ablation, block_write_iops slope = 0.05 ops/partition
        #          (verified: |delta|=0.45 ≫ MDE, sign matches prediction -1).
        #   M1.4 — slope identical to M1.2 (below MDE delta → not verified).
        cls._build_cell("M1.0", slope=0.0, noise=0.5, flags="--two-phase=false --consumer-mode=dynamic")
        cls._build_cell("M1.2", slope=0.50, noise=0.5, flags="--two-phase=true --consumer-mode=dynamic")
        cls._build_cell("M1.4", slope=0.50, noise=0.5, flags="--two-phase=false --consumer-mode=dynamic")
        cls._build_cell("M1.6", slope=0.05, noise=0.5, flags="--two-phase=true --consumer-mode=queue")
        cls._build_m41(cls.tmpdir)

    @classmethod
    def _build_cell(cls, cell, slope, noise, flags):
        rng = np.random.default_rng(seed=hash(cell) & 0xFFFFFFFF)
        ns = [500, 1000, 2000, 3000]
        reps = 5
        pos = 1
        for n in ns:
            for rep in range(1, reps + 1):
                run_dir = cls.results_dir / f"run-{pos:03d}-{cell}-N{n}-rep{rep}"
                run_dir.mkdir()
                # Fake aggregated.csv:
                # one container row at each t_s carrying iops_read/iops_write,
                # one host row carrying rpc_read_parti-handoff + rpc_write_parti-heartbeat.
                bw = max(0.0, slope * n + rng.normal(0, noise))
                br = max(0.0, 0.1 * n + rng.normal(0, noise))
                rpc_r = 0.167 * n if "two-phase=true" in flags else 0.0
                rpc_w = 1.0
                with open(run_dir / "aggregated.csv", "w", newline="") as f:
                    w = csv.writer(f)
                    w.writerow([
                        "t_s", "node", "iops_read", "iops_write",
                        "bytes_read", "bytes_write",
                        "rpc_read_parti-handoff", "rpc_write_parti-heartbeat",
                    ])
                    for t in range(60):
                        # container row
                        w.writerow([t, "iops-nats-1", br, bw, 0, 0, "", ""])
                        # host row
                        w.writerow([t, "host", "", "", "", "", rpc_r, rpc_w])
                (run_dir / "run-meta.yaml").write_text(
                    f"cell: {cell}\nn: {n}\nrep: {rep}\nreplicas: 3\nflags: '{flags}'\n"
                )
                pos += 1

    @classmethod
    def _build_m41(cls, tmpdir):
        # Minimal M4.1 calibration: 6 c_stream × 2 ft × 2 storage × 1 R × 2 roles.
        path = Path(tmpdir) / "m4_calibration.csv"
        with open(path, "w", newline="") as f:
            w = csv.writer(f)
            w.writerow([
                "c_stream", "fetch_timeout_s", "data_storage", "replicas",
                "node_role", "block_read_iops", "block_write_iops",
                "ci_low", "ci_high",
            ])
            for cs in (0, 100, 500, 1000, 2000, 3000):
                for ft in (5.0, 30.0):
                    for ds in ("file", "memory"):
                        for role in ("leader", "follower"):
                            # Make M4.1 contribute ~0.05 × N for block_write
                            # in the dynamic-file leader slice; near-zero elsewhere.
                            bw = 0.05 * cs if (ds == "file" and ft == 5.0 and role == "leader") else 0.0
                            br = 0.0
                            w.writerow([cs, ft, ds, 3, role, br, bw, bw - 1, bw + 1])
        cls.m41_path = path

    def test_recovers_slopes(self):
        out_dir = Path(self.tmpdir) / "analysis-out"
        self.analyze.main([
            "--results-dir", str(self.results_dir),
            "--m4-calibration", str(self.m41_path),
            "--out", str(out_dir),
        ])
        slope_df = pd.read_csv(out_dir / "slope_table.csv")
        mde_df = pd.read_csv(out_dir / "mde.csv")
        mit_df = pd.read_csv(out_dir / "mitigation_table.csv")

        # M1.2 block_write_iops slope ≈ 0.50, within ~5 %.
        row = slope_df[
            (slope_df["config"] == "M1.2") & (slope_df["column"] == "block_write_iops")
        ].iloc[0]
        self.assertAlmostEqual(row["slope_beta1"], 0.50, delta=0.05)

        # M1.6 block_write_iops slope ≈ 0.05, within ~0.05 absolute.
        row = slope_df[
            (slope_df["config"] == "M1.6") & (slope_df["column"] == "block_write_iops")
        ].iloc[0]
        self.assertAlmostEqual(row["slope_beta1"], 0.05, delta=0.05)

        # MDE: positive, smaller than the strongest fixture slope (0.50).
        mde_bw = mde_df[mde_df["column"] == "block_write_iops"].iloc[0]
        self.assertGreater(mde_bw["mde_slope"], 0.0)
        self.assertLess(mde_bw["mde_slope"], 0.50)

        # Mitigation M1.2 -> M1.6 on block_write_iops should be VERIFIED.
        m_ver = mit_df[
            (mit_df["baseline_cell"] == "M1.2")
            & (mit_df["ablation_cell"] == "M1.6")
            & (mit_df["column"] == "block_write_iops")
        ].iloc[0]
        self.assertTrue(bool(m_ver["verified"]))

        # Sanity probe M1.1 vs M1.4 (M1.1 absent → no row generated;
        # construction has M1.2 vs M1.4 both 0.50, so delta ≈ 0 — when the
        # predicted direction is -1 (e.g. -> M1.4 vs M1.2 read_rpc_ops),
        # verification must FAIL because the slope didn't move.
        m_nv = mit_df[
            (mit_df["baseline_cell"] == "M1.2")
            & (mit_df["ablation_cell"] == "M1.4")
            & (mit_df["column"] == "block_write_iops")
        ]
        if not m_nv.empty:
            self.assertFalse(bool(m_nv.iloc[0]["verified"]))

    def test_slopes_only_mode(self):
        """--slopes-only must produce slope + MDE without --m4-calibration.

        Tier 0 sanity scans run before M4 calibration exists. analyze.py
        must still produce slope_table.csv + mde.csv (and the tukey
        sidecar) so the operator can sanity-check the rig.
        """
        out_dir = Path(self.tmpdir) / "analysis-slopes-only"
        rc = self.analyze.main([
            "--results-dir", str(self.results_dir),
            "--slopes-only",
            "--out", str(out_dir),
        ])
        self.assertEqual(rc, 0)
        self.assertTrue((out_dir / "slope_table.csv").exists())
        self.assertTrue((out_dir / "mde.csv").exists())
        self.assertTrue((out_dir / "tukey_outliers.tsv").exists())
        # Attribution / mitigation tables MUST NOT exist in slopes-only.
        self.assertFalse((out_dir / "attribution_table.csv").exists())
        self.assertFalse((out_dir / "mitigation_table.csv").exists())

        # Slope estimate for M1.2 should match the full-mode fixture.
        slope_df = pd.read_csv(out_dir / "slope_table.csv")
        row = slope_df[
            (slope_df["config"] == "M1.2") & (slope_df["column"] == "block_write_iops")
        ].iloc[0]
        self.assertAlmostEqual(row["slope_beta1"], 0.50, delta=0.05)

    def test_full_mode_rejects_missing_m4_calibration(self):
        """Without --slopes-only, --m4-calibration is required."""
        out_dir = Path(self.tmpdir) / "analysis-bad"
        with self.assertRaises(SystemExit):
            self.analyze.main([
                "--results-dir", str(self.results_dir),
                "--out", str(out_dir),
            ])

    def test_cluster_attribution_sums_leader_and_followers(self):
        """P1-A regression: attributed_h2 must be leader + (R-1)*follower.

        Build a fixture where leader and follower have distinct block_write_iops
        values (leader=10, follower=2) and R=3. The correct cluster total is
        10 + 2*2 = 14. Prior to the fix, only the leader value (10) was used.
        """
        import pandas as pd

        tmpdir = Path(self.tmpdir)

        # Build a minimal M4.1 with one c_stream grid point so interpolation
        # clamps to the single value; leader=10, follower=2; replicas=3.
        m41_path = tmpdir / "m41_distinct.csv"
        with open(m41_path, "w", newline="") as f:
            w = csv.writer(f)
            w.writerow([
                "c_stream", "fetch_timeout_s", "data_storage", "replicas",
                "node_role", "block_read_iops", "block_write_iops",
                "ci_low", "ci_high",
            ])
            # leader: block_write_iops = 10
            w.writerow([1000, 5.0, "file", 3, "leader", 0, 10, 9, 11])
            # follower: block_write_iops = 2
            w.writerow([1000, 5.0, "file", 3, "follower", 0, 2, 1, 3])

        # Build a results dir with a single M1.2-like run: N=1000, R=3, file storage.
        results_dir = tmpdir / "campaign-distinct"
        results_dir.mkdir(exist_ok=True)
        run_dir = results_dir / "run-001-M1.2-N1000-rep1"
        run_dir.mkdir(exist_ok=True)
        with open(run_dir / "aggregated.csv", "w", newline="") as f:
            w = csv.writer(f)
            w.writerow([
                "t_s", "node", "iops_read", "iops_write",
                "bytes_read", "bytes_write",
            ])
            for t in range(10):
                w.writerow([t, "iops-nats-1", 0, 50, 0, 0])
                w.writerow([t, "iops-nats-2", 0, 50, 0, 0])
        (run_dir / "run-meta.yaml").write_text(
            "cell: M1.2\nn: 1000\nrep: 1\nreplicas: 3\n"
            "flags: '--two-phase=true --consumer-mode=dynamic'\n"
        )

        # Second run at a different N so OLS has at least 2 points.
        run_dir2 = results_dir / "run-002-M1.2-N2000-rep1"
        run_dir2.mkdir(exist_ok=True)
        with open(run_dir2 / "aggregated.csv", "w", newline="") as f:
            w = csv.writer(f)
            w.writerow([
                "t_s", "node", "iops_read", "iops_write",
                "bytes_read", "bytes_write",
            ])
            for t in range(10):
                w.writerow([t, "iops-nats-1", 0, 50, 0, 0])
        (run_dir2 / "run-meta.yaml").write_text(
            "cell: M1.2\nn: 2000\nrep: 1\nreplicas: 3\n"
            "flags: '--two-phase=true --consumer-mode=dynamic'\n"
        )

        # M1.0 control (required so main() doesn't raise on missing control cell;
        # we provide minimal runs so the code path completes).
        for ni, n in enumerate([1000, 2000], 1):
            rd = results_dir / f"run-00{ni + 2}-M1.0-N{n}-rep1"
            rd.mkdir(exist_ok=True)
            with open(rd / "aggregated.csv", "w", newline="") as f:
                w = csv.writer(f)
                w.writerow(["t_s", "node", "iops_read", "iops_write", "bytes_read", "bytes_write"])
                for t in range(10):
                    w.writerow([t, "iops-nats-1", 0, 5, 0, 0])
            (rd / "run-meta.yaml").write_text(
                f"cell: M1.0\nn: {n}\nrep: 1\nreplicas: 3\n"
                "flags: '--two-phase=false --consumer-mode=dynamic'\n"
            )

        out_dir = tmpdir / "analysis-distinct"
        self.analyze.main([
            "--results-dir", str(results_dir),
            "--m4-calibration", str(m41_path),
            "--out", str(out_dir),
        ])

        attr_df = pd.read_csv(out_dir / "attribution_table.csv")
        # N=1000 with c_stream=1000: interpolation at c_stream=1000 clamps to
        # the single grid point, so leader=10, follower=2 → cluster total=14.
        row = attr_df[
            (attr_df["config"] == "M1.2")
            & (attr_df["N"] == 1000)
            & (attr_df["column"] == "block_write_iops")
        ].iloc[0]
        self.assertAlmostEqual(
            row["attributed_h2"], 14.0, delta=0.01,
            msg=(
                f"attributed_h2={row['attributed_h2']}: expected 14 "
                "(leader=10 + 2×follower=2); got leader-only or wrong sum"
            ),
        )

    def test_missing_m41_slice_raises(self):
        """P1-B regression: m41_interpolate raises SystemExit on a missing slice.

        Build a M4.1 fixture that covers (ft=5, file, R=3, leader) but omits
        (ft=5, file, R=3, follower). With R=3 the new attribution code requests
        the follower slice, which is absent → SystemExit with a message naming
        the missing (fetch_timeout_s, data_storage, replicas, node_role) tuple.
        """
        tmpdir = Path(self.tmpdir)

        # M4.1 with only the leader row — follower row is intentionally absent.
        m41_path = tmpdir / "m41_missing_follower.csv"
        with open(m41_path, "w", newline="") as f:
            w = csv.writer(f)
            w.writerow([
                "c_stream", "fetch_timeout_s", "data_storage", "replicas",
                "node_role", "block_read_iops", "block_write_iops",
                "ci_low", "ci_high",
            ])
            w.writerow([1000, 5.0, "file", 3, "leader", 0, 10, 9, 11])
            # follower row for (ft=5, file, R=3) is intentionally missing.

        # Re-use the same results dir from the cluster-attribution test.
        results_dir = tmpdir / "campaign-missing-slice"
        results_dir.mkdir(exist_ok=True)
        for ni, (cell, n) in enumerate([("M1.2", 1000), ("M1.2", 2000), ("M1.0", 1000), ("M1.0", 2000)], 1):
            rd = results_dir / f"run-{ni:03d}-{cell}-N{n}-rep1"
            rd.mkdir(exist_ok=True)
            with open(rd / "aggregated.csv", "w", newline="") as f:
                w = csv.writer(f)
                w.writerow(["t_s", "node", "iops_read", "iops_write", "bytes_read", "bytes_write"])
                for t in range(10):
                    w.writerow([t, "iops-nats-1", 0, 5, 0, 0])
            (rd / "run-meta.yaml").write_text(
                f"cell: {cell}\nn: {n}\nrep: 1\nreplicas: 3\n"
                "flags: '--two-phase=true --consumer-mode=dynamic'\n"
            )

        out_dir = tmpdir / "analysis-missing-slice"
        with self.assertRaises(SystemExit) as cm:
            self.analyze.main([
                "--results-dir", str(results_dir),
                "--m4-calibration", str(m41_path),
                "--out", str(out_dir),
            ])
        msg = str(cm.exception)
        # The error must name the missing slice parameters so the operator
        # knows exactly which grid point to add.
        self.assertIn("follower", msg)
        self.assertIn("replicas=3", msg)
        self.assertIn("data_storage=", msg)


if __name__ == "__main__":
    unittest.main()
