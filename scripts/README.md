# Flake Detector

Long-running test harness to detect flaky tests across unit, integration, and stress suites. It runs selected suites repeatedly, stops on first flake or hard failure, and saves concise diagnostics and logs for triage.

## Why use it

- Catch intermittent test failures early
- Reproduce racey timing issues locally
- Produce logs and a JSON report for debugging and CI artifacts

## Script

Path: `scripts/flake_detector.sh`

Requires: bash, Go toolchain available on PATH

## What it does

- Iterates test runs for the selected suites (unit, integration, stress)
- Prints run-by-run timing and status
- Early-stops when a failure is detected, distinguishing:
  - Flaky failure: previously passed, then failed in a later iteration
  - Non-flaky failure: fails on the first attempt
- Optionally writes per-run logs and a JSON summary

## Environment variables (options)

- `MAX_RUNS` (default: `1000`)
  - Max iterations before stopping if no flake is found
- `TEST_SET` (default: `unit,integration,stress`)
  - Comma-separated list of suites to run; choose any of: `unit`, `integration`, `stress`
- `PARALLELISM` (default: `4`)
  - Passed to `go test -p` (package-level parallelism)
- `RACE` (default: `1`)
  - Enable Go race detector for unit/integration runs (`0` to disable)
- `STRESS` (default: `1`)
  - Enable stress tests by exporting `PARTI_STRESS=1` when running the `stress` suite
- `TEST_TIMEOUT` (default: `6m`)
  - `go test -timeout` for unit/integration runs
- `STRESS_TIMEOUT` (default: `20m`)
  - `go test -timeout` for stress runs (stress may legitimately exceed 6 minutes)
- `SLOW_LOG_THRESHOLD_SECONDS` (default: `15`)
  - Print a note when a single suite exceeds this duration
- `REPORT_JSON` (optional)
  - Path to a JSON summary file written at the end or on early exit
- `SAVE_LOGS_DIR` (optional)
  - Directory to store per-run logs; directory is created if needed
- `GOFLAGS` (optional)
  - Additional flags forwarded to `go test` (e.g., `-run`, `-bench`, `-count=1` retained by script)

## Suites

- `unit`: all non-integration/non-stress packages
- `integration`: `./test/integration`
- `stress`: `./test/stress` (requires `PARTI_STRESS=1`, set by the script when `STRESS=1`)

## Typical usage

Run a quick flake hunt for unit + integration only (recommended fast loop):

```bash
MAX_RUNS=10 \
TEST_SET=unit,integration \
PARALLELISM=4 \
RACE=1 \
TEST_TIMEOUT=6m \
SAVE_LOGS_DIR=.flake_logs \
REPORT_JSON=.flake_logs/fast_report.json \
./scripts/flake_detector.sh
```

Run stress-only with extended timeout to avoid false timeouts:

```bash
MAX_RUNS=1 \
TEST_SET=stress \
PARALLELISM=2 \
STRESS_TIMEOUT=20m \
SAVE_LOGS_DIR=.flake_logs \
./scripts/flake_detector.sh
```

Full run for all suites (long):

```bash
MAX_RUNS=3 \
TEST_SET=unit,integration,stress \
PARALLELISM=2 \
SAVE_LOGS_DIR=.flake_logs \
REPORT_JSON=.flake_logs/report.json \
./scripts/flake_detector.sh
```

## Output and exit codes

Console output shows:

- Start timestamp and options
- Per-run start/finish lines per suite
- Slow-run warnings beyond `SLOW_LOG_THRESHOLD_SECONDS`
- Early-exit diagnostics with the last ~50 lines of failing logs

Exit codes:

- `0`: Completed all iterations with no failures
- Non-zero: Stopped early due to failure; the script prints whether it was detected as flaky (passed first, then failed) or non-flaky (failed on the first attempt).

Artifacts (when configured):

- `SAVE_LOGS_DIR`: Saves per-run, per-suite logs named by timestamp
- `REPORT_JSON`: Summary report (high-level stats, last failure context, and timing); suitable for CI artifact collection

## CI suggestions

Add two jobs to your CI:

1) Fast daily flake loop (unit + integration)

```yaml
name: Flake hunt (fast)
on:
  schedule:
    - cron: '0 3 * * *'
jobs:
  flake-fast:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.25.x'
      - name: Run fast loop
        run: |
          mkdir -p .flake_logs
          MAX_RUNS=20 TEST_SET=unit,integration PARALLELISM=4 RACE=1 TEST_TIMEOUT=6m \
          SAVE_LOGS_DIR=.flake_logs REPORT_JSON=.flake_logs/fast_report.json \
          ./scripts/flake_detector.sh
      - name: Upload logs
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: flake-fast-logs
          path: .flake_logs
```

2) Weekly stress-only, extended timeout

```yaml
name: Flake hunt (stress)
on:
  schedule:
    - cron: '0 4 * * 0'
jobs:
  flake-stress:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.25.x'
      - name: Run stress-only
        run: |
          mkdir -p .flake_logs
          MAX_RUNS=1 TEST_SET=stress PARALLELISM=2 STRESS_TIMEOUT=30m \
          SAVE_LOGS_DIR=.flake_logs REPORT_JSON=.flake_logs/stress_report.json \
          ./scripts/flake_detector.sh
      - name: Upload logs
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: flake-stress-logs
          path: .flake_logs
```

## Tips & troubleshooting

- Stress timing: If the `stress` suite times out around 6 minutes, increase `STRESS_TIMEOUT` (e.g., `20m`–`30m`).
- Speed vs coverage: Keep `RACE=1` for fidelity; if you need speed for long loops, temporarily set `RACE=0`.
- Parallelism: Tune `PARALLELISM` based on cores and memory. Very high values can increase flakiness.
- Repro a single test: Use `GOFLAGS` to add `-run` filters to a particular package or test while keeping flake loop semantics.
- Disk usage: With `SAVE_LOGS_DIR` set, logs accumulate. Clean the directory periodically.

## Examples for quick copy/paste

Fast loop (local):

```bash
MAX_RUNS=5 TEST_SET=unit,integration PARALLELISM=4 RACE=1 TEST_TIMEOUT=6m \
SAVE_LOGS_DIR=.flake_logs REPORT_JSON=.flake_logs/fast_report.json \
./scripts/flake_detector.sh
```

Stress-only (local):

```bash
MAX_RUNS=1 TEST_SET=stress PARALLELISM=2 STRESS_TIMEOUT=20m \
SAVE_LOGS_DIR=.flake_logs \
./scripts/flake_detector.sh
```

---

Questions or improvements? Open an issue or PR with proposed changes to `scripts/flake_detector.sh` and this README.
