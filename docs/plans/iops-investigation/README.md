# NATS IOPS Investigation — Plan & Experiment Records

This directory is the home for the IOPS-cost investigation for Parti
deployments. It is **not** a one-shot plan: it hosts the running series of
measurements, hypotheses, and verified mitigations.

## Why this exists

A user running Parti **v2.3.0** in a 5-worker cluster against a 3-replica
JetStream cluster reported a steady-state PVC IOPS curve that scales
~linearly with the number of partitions, *even with no application
messages flowing through the consumer*:

| Partitions | PVC IOPS |
|-----------:|---------:|
|       1000 |      275 |
|       2000 |      412 |
|       3000 |      565 |

Slope: ~0.14 IOPS / partition above a ~135 IOPS floor.

The user also reports that switching parti's KV buckets to **memory storage
left the IOPS curve essentially unchanged**, which is the most interesting
diagnostic signal in the original report — it constrains the search.

## The hypothesis the investigation starts from

The only loop in `v2.3.0` library code that does O(N partitions) work per
tick in steady state, by code inspection, is:

> `v2.3.0:internal/assignment/handoff/twophase.go:41-49` →
> `twoPhaseCoordinator.maybeSweepClaims` (`:380-418`)

- Runs in every worker **only when `parti.Config.EnableTwoPhaseHandoff =
  true`**. v2.3.0's library default is `false`
  (`v2.3.0:config.go:382-394`); when false, `handoff.New` returns a
  no-op `direct` coordinator
  (`v2.3.0:internal/assignment/handoff/coordinator.go:103-139`).
  **Implication:** the user's observed slope is itself evidence that
  their deployment has two-phase handoff enabled, or the hypothesis
  is wrong. The plan records the flag explicitly in every run's
  manifest and runs both a B-prod (`true`) baseline and a B-lib
  (`false`) baseline.
- Default `Handoff.SweepInterval = 30s` (`v2.3.0:config.go:48-55`,
  `v2.3.0:internal/assignment/handoff/coordinator.go:127-130`).
- Per sweep, per worker: `Store.ListKeys()` + `Store.Get(pid)` per claim.
  Number of claims equals the number of partitions in steady state.
- The sweep is **read-only** in idle: `PutIfEpoch` runs only for
  *expired non-stable* claims, of which there are normally zero
  (`v2.3.0:internal/assignment/handoff/twophase.go:405-418`). So H1's
  predicted slope is in *read-RPC* operations, not in *disk-write*
  IOPS. The user's PVC-IOPS observation is most naturally read-write,
  so the plan maintains a four-column budget (`read_rpc_ops`,
  `write_mutation_ops`, `block_read_iops`, `block_write_iops`) and
  closes attribution per-column.

Cluster cost, back-of-envelope:

```
5 workers × (N + 1) ops / 30s ≈ N / 6 IOPS
```

| Partitions | Predicted | Observed delta (over base) |
|-----------:|----------:|---------------------------:|
|       1000 |       167 |                        140 |
|       2000 |       333 |                        277 |
|       3000 |       500 |                        430 |

Shape and magnitude match within ~15%. This is the **lead suspect**.

Note: `internal/durable/claim_resolver.go`'s `reconcileLoop` (commit
`5bc46cc`) was **added after v2.3.0** and is not part of the user's
deployment. The reconcile-loop hypothesis from the earlier conversation
does not apply to v2.3.0.

## What this directory is for

1. Pre-register the hypothesis above and its falsifiable predictions.
2. Build a reproducible, instrumented test rig (docker-compose +
   testcontainer-driven N-replica JetStream cluster) that mirrors the
   reported topology cheaply enough to iterate on knobs.
3. Attribute total disk IOPS to specific NATS streams / KV buckets / JS
   consumers via correlated host-level + server-level monitoring.
4. Falsify or confirm the hypothesis. Quantify residual IOPS from other
   subsystems (per-partition JS pull consumers, KV watchers, election,
   heartbeats).
5. Verify candidate mitigations (raise sweep interval, disable two-phase
   handoff, change consumer fetch params) actually move the curve.

The plan does **not** commit to a code change; it commits to data.

## Layout

The investigation is split between two trees: planning artefacts here
under `docs/plans/`, and the runnable rig under `test/iops-investigation/`
(mirroring the existing `test/simulation/` convention).

**Planning artefacts — this directory:**

| Path | Content |
|---|---|
| [`00-attribution-plan.md`](00-attribution-plan.md) | The measurement plan: rig, knobs, hypotheses, success criteria. |
| [`01-implementation-strategy.md`](01-implementation-strategy.md) | Phased build plan with suggested model + effort per phase and review checkpoints. |
| `findings.md` | Written conclusions after enough runs to draw them. Added at the end. |
| `reviews/` | External review reports (e.g. `/post-impl-review` output) if produced. |

**Runnable rig — `test/iops-investigation/` (added during Phase 1):**

| Path | Content |
|---|---|
| `Makefile` | `make up`, `make down`, `make reset`, `make image-digest`. |
| `docker/docker-compose.yaml` | 3 / 5-replica NATS cluster; image overridable via `IOPS_RIG_NATS_IMAGE` (default `nats:2.12.6`). |
| `docker/nats-server.conf` | Minimal JS configuration. |
| `cmd/harness/main.go` | Workload binary with every knob in §M1 of the plan. |
| `cmd/calibrate/main.go` | M4 / M4.1 calibration driver. |
| `cmd/aggregate/main.go` | Reconciles cgroup + iostat + jsz + harness counters into a per-run CSV. |
| `internal/instrumentedjs/` | `jetstream.JetStream` + `jetstream.KeyValue` wrapper with per-`(bucket, op)` counters. |
| `internal/storageverify/` | Stream storage-class assertion. |
| `scripts/capture-cgroup-io.sh` | 1 Hz io.stat poller (primary IOPS source). |
| `scripts/capture-iostat.sh` | Secondary host-level cross-check. |
| `scripts/run-matrix.sh` | Drives M1.0–M1.11 in a pre-registered random schedule. |
| `results/` | **Gitignored.** One subfolder per measurement run with raw captures + aggregated CSV. |
| `README.md` | How to run the rig end-to-end. |

## Status

| Phase | State |
|---|---|
| Hypothesis written down | Done (this README + `00-attribution-plan.md`) |
| Plan reviewed | Done — 8 rounds of Copilot `gpt-5.5 xhigh` review + 1 Gemini 3.1 Pro pass. Consensus: ready for execution. See `tmp/00-attribution-plan_iops-investigation_review*.md`. |
| Rig built | TODO |
| Baseline measurement on v2.3.0 | TODO |
| Knob-ablation matrix | TODO |
| Mitigation verification | TODO |
| Findings written up | TODO |

## Decision log (append-only)

- _2026-05-16_ — Investigation opened. Initial hypothesis: two-phase
  handoff sweep is the dominant per-partition source on v2.3.0. To be
  tested.
- _2026-05-16_ — Plan reviewed across 8 Copilot rounds + 1 Gemini pass;
  consensus reached. Key plan refinements during review: (1) v2.3.0
  defaults `EnableTwoPhaseHandoff=false`, so two distinct baselines
  (B-prod with two-phase on, B-lib library default) must be run; the
  user's observed slope is itself evidence of two-phase being enabled;
  (2) attribution is a four-column budget (read RPCs, write mutations,
  block-read IOPS, block-write IOPS), not single-column; (3) H2
  server-internal pull-consumer state I/O is calibrated separately via
  M4.1 (`C_stream × FetchTimeout × data_storage × R × node_role` grid)
  and folded into the block-I/O columns; (4) statistical design uses
  n=5 with replicate-level OLS, MDE from a no-Parti control, Tukey-fence
  outlier rule, and randomised run order; (5) wall-clock budget is
  ~70 hours for the matrix.
