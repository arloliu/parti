# Plan 02 — NATS Server-Side IOPS Tuning Investigation

> **Status: scaffold / next-phase plan.**
> Prerequisite: Tier 1 + M1.7 + M1.9 are complete; see `findings.md`.
> This plan tests **NATS-server-side** knobs that may reduce
> per-partition IOPS *without* losing durability — the gap left by
> the M1.7 result (memory data stream is 90 % win but loses
> durability across coordinated cluster restart).
> Estimated effort: ~5–6 h elapsed, ~4 h rig time.

---

## 1. Why this plan exists

`findings.md` proved:
- Per-partition IOPS cost lives on the JetStream data stream, not in parti.
- Memory data-stream `Storage = memory` collapses IOPS by 90 % at N=1000 / 72 % at N=3000.
- But memory storage trades durability across coordinated cluster restart.

The deployable-durable mitigation is therefore *NATS server-side
tuning*, which this plan investigates. The original §M1 matrix
(M1.5 `FetchTimeout`, M1.6 `consumer.Queue`) tests only parti-side
ablations; that subset is **lower-priority** than the NATS-side
tuning explored here.

---

## 2. Where to start (for the next agent)

Read in this order:
1. `docs/plans/iops-investigation/findings.md` — current state of the investigation, operator-facing.
2. `docs/plans/iops-investigation/tier1-findings.md`, `m17-findings.md`, `m19-findings.md` — prior result details.
3. `test/iops-investigation/RUNBOOK.md` §3 — execution-tier framework + cadence rationale.
4. `test/iops-investigation/docker/nats-server.conf` — where most server-side knobs go.
5. This file (Plan 02).

Worktree state at handoff:
- Branch: `worktree-iops-investigation`, rebased onto `origin/main`, 42 commits ahead.
- HEAD: `7dcb0ba build: exclude test/iops-investigation/ from TEST_DIRS`.
- `make lint` ✓, `make test` ✓, working tree clean.
- Rig: docker compose R=3 profile is currently up.
- Python venv: `test/iops-investigation/.venv/` with pandas/numpy/statsmodels installed.

Reference data for comparison (capture-window mean iops_write, cluster-summed):

| Cell | N=1000 | N=3000 |
|---|---:|---:|
| M1.2 (default — file storage, default config) | 216 | 450 |
| M1.7 (data stream Storage=memory) | **22** | **128** |
| M1.9 (parti KVs Storage=memory) | 211 | 446 |

Any new cell should compare its N=1000 / N=3000 means against M1.2.

---

## 3. Phase R1 — Research (~30 min, no experiments)

**Goal:** identify 2–4 NATS server-side knobs that can reduce IOPS
**without** dropping the data stream to memory.

Sources to consult:
- nats.io official docs — JetStream administration, filestore behavior, server configuration reference.
- github.com/nats-io/nats-server — search issues/PRs for "fsync", "sync_interval", "iops", "filestore tuning", "high partition count".
- Synadia blog — production tuning posts.
- Community threads — natsio Slack archives if accessible.

Specific candidates to investigate (with prior likelihood):

| Knob | Where set | Mechanism | Prior |
|---|---|---|---|
| `jetstream.sync_interval` | server.conf | Batches fsync across N commits → 1 fsync per interval | **HIGH** |
| Consumer `MaxAckPending` (per consumer) | parti pre-creates the data stream's consumers, or harness sets it | Caps in-flight ack state | MEDIUM |
| Consumer `AckWait` (per consumer) | same as above | Fewer re-arm cycles | MEDIUM |
| `jetstream.max_pending` | server.conf | Backpressure on outstanding writes | LOW (probably affects bursts not IOPS) |
| `cluster.write_deadline` | server.conf | Raft commit timing | LOW |
| Stream `Discard = old` + `MaxAge` | per-stream | Bounds disk usage but probably doesn't change per-Put IOPS | LOW |

Output of R1: a short note (`tmp/r1-nats-tuning-research.md`) listing each candidate knob with:
- Exact config keyword + default value
- Documented effect on durability / data-loss window
- Whether it's a server-config or per-consumer / per-stream setting
- Whether it requires rebuilding the docker image or just editing server.conf

**Stop criterion for R1:** 2–4 candidates identified with documented behavior. If only 1 candidate is solidly documented, that's still actionable; if 0, escalate.

---

## 4. Phase R2 — Cell design (~10 min)

Name new cells `M2.x`. Each cell is *exactly one server.conf or per-consumer change* relative to M1.2 baseline, so the slope/mean delta directly attributes to that knob.

Cell name suggestions (final list depends on R1):

| Cell | What it tries | Server.conf / harness change |
|---|---|---|
| **M2.1** | `sync_interval = 5s` | `jetstream { sync_interval: "5s" }` in nats-server.conf |
| **M2.2** | `sync_interval = 30s` | `jetstream { sync_interval: "30s" }` in nats-server.conf |
| **M2.3** | `MaxAckPending` tuned (e.g. 1000) | harness `--max-ack-pending=1000` (need to add flag) or pre-create consumers |
| **M2.4** | `consumer.Queue` instead of Dynamic | already supported: `--consumer-mode=queue` |

Each cell: N∈{1000, 3000} × 3 reps = 6 runs.

**Predict before running.** For each candidate, write a 1-line prediction in `findings.md` §9 "Open questions". If the prediction is wrong, that's a finding worth reporting.

---

## 5. Phase R3 — Implementation (~30 min)

### 5a. Server-config knobs (M2.1, M2.2)

Edit `test/iops-investigation/docker/nats-server.conf`. Current content includes server name, jetstream block, cluster block. Add the new tuning keyword inside the `jetstream` block. Example:

```hcl
jetstream {
  store_dir: "/data/jetstream"
  sync_interval: "5s"
}
```

This is a *per-cell* edit. Options:
- (a) Maintain a separate server.conf per cell (e.g., `nats-server-M2.1.conf`) and select via a `--server-config` flag passed to docker compose; OR
- (b) Use a sed-based pre-step in run-matrix.sh to inject the right value before `docker compose up`.

Option (a) is cleaner; (b) is less invasive. Start with (a).

### 5b. Consumer-side knobs (M2.3, M2.4)

Check `test/iops-investigation/cmd/harness/main.go` for existing flags. `--consumer-mode` already supports `dynamic` / `queue` / `static`. `--max-ack-pending` may need to be added. Pattern follows existing `--kv-storage` / `--data-storage` flags.

If a flag is missing, add it following existing conventions, write a unit test, run `make lint` + sub-module `go test`, commit.

### 5c. Define cells in run-matrix.sh

Add cell definitions following existing patterns:

```bash
_def_cell M2.1 3 "1000 3000" "--two-phase=true --consumer-mode=dynamic --nats-config=M2.1"
_def_cell M2.2 3 "1000 3000" "--two-phase=true --consumer-mode=dynamic --nats-config=M2.2"
# ... etc
```

`ALL_CELLS=(... M2.1 M2.2 M2.3 M2.4)` so dry-run discovers them.

---

## 6. Phase R4 — Experiments (~3–4 h rig time)

Same shape as M1.7 / M1.9 focused tests:

```bash
cd test/iops-investigation
RESULTS_DIR=$(pwd)/results/m2-$(date +%Y%m%d-%H%M%S)
mkdir -p "$RESULTS_DIR"
nohup bash scripts/run-matrix.sh \
  --seed 42 \
  --cells M2.1,M2.2,M2.3,M2.4 \
  --reps 3 \
  --n-values 1000,3000 \
  --warmup-secs 120 \
  --capture-secs 120 \
  --results-dir "$RESULTS_DIR" \
  > /tmp/claude/m2.log 2>&1 &
```

Expected: 4 cells × 6 runs = 24 runs × ~7.5 min/run = 3 h.

Monitor every 30 min via:

```bash
NUM_OK=$(grep -c "run OK\." /tmp/claude/m2.log)
echo "$NUM_OK / 24"
```

### 6a. Analysis pattern (per cell)

Each cell needs the same comparison as M1.7 in `m17-findings.md`:

1. Time-series check at N=3000 — does the late-window spike persist?
2. Capture-window mean (t ∈ [120, 240] s) per (N, rep) — sum across non-host rows.
3. Two-point slope: (mean@N=3000 − mean@N=1000) / 2000.
4. Δ vs M1.2 baseline (216 / 450 IOPS).

Use the inline-awk pattern (see `m17-findings.md` "Artifacts" note explaining why `analyze.py` mean is diluted).

---

## 7. Phase R5 — Final report (~1 h)

Update `docs/plans/iops-investigation/findings.md`:

### 7a. §2 (matrix legend) — add M2.x rows

Add a sub-table or row for each M2.x cell with its plain-English knob.

### 7b. §5 (Verified mitigations) — add new rows

Each M2.x cell becomes a row. Rank by Δβ₁ on `block_write_iops`. Include the durability tradeoff in the "Side effects" column.

### 7c. New §10 — Operator decision tree

Add a final section that gives a *user-facing* decision tree, e.g.:

```
Q1. Can you tolerate "data stream lost on coordinated cluster restart"?
  Yes → set data stream Storage=memory. 90% IOPS win. STOP.
  No  → continue.
Q2. Can you tolerate a 5-second data-loss window on power failure?
  Yes → set jetstream.sync_interval = 5s. Expected ~70% IOPS win [TBD].
  No  → continue.
Q3. Are your workloads queue-compatible (i.e., any worker can ack any message)?
  Yes → use consumer.Queue instead of N Dynamic consumers. Expected ~? win [TBD].
  No  → fundamental cost; see "Open questions" §11 for architectural redesign.
```

Fill in the [TBD]s with the actual measured deltas.

### 7d. Update §3 budget decomposition

Add the M2.x results to the "what's where" decomposition at N=1000.

### 7e. Commit + push + PR

```bash
git add docs/plans/iops-investigation/findings.md
git commit -m "docs(iops-investigation): integrate NATS server-side tuning results (M2.x)"
git push -u origin worktree-iops-investigation
gh pr create --title "iops-investigation: tier 1 + focused ablations + NATS server tuning" \
  --body "$(cat docs/plans/iops-investigation/findings.md | head -50)"
```

---

## 8. Risks / out of scope

- **OS / FS tuning** (`ext4 data=writeback`, `nobarrier`, `vm.dirty_*`) — dangerous on host, hard to revert. **Skip.**
- **NATS-internal knobs not exposed via config** — would require building a custom NATS image. **Skip** unless R1 finds a high-prior knob worth the effort.
- **Cross-version NATS comparison (2.10 / 2.11 / 2.12)** — interesting but expands scope. **Skip** unless a specific bug fix in a newer version is identified.
- **Spike root-cause identification** — `findings.md` §9 already flags this as open; if R1 reveals the spike is `sync_interval`-triggered, that's a bonus finding. Don't go on a hunt for it.

---

## 9. Success criteria

- At least one M2.x cell shows a measurable, durability-preserving IOPS reduction (≥ 20 % vs M1.2).
- The final findings.md §10 decision tree has at least one branch that doesn't end in "lose durability" or "redesign parti".
- Operator can read the report and pick a config knob *today*.

If R1 finds zero promising knobs, the investigation has produced a different but still useful conclusion: "the IOPS cost is fundamental to JetStream's durable-consumer model; the only paths forward are (a) memory storage, (b) parti single-consumer-with-filter redesign, (c) wait for upstream NATS to expose more tuning." That's also a publishable result.

---

## 10. Resume from here

If this conversation is cleared and a future agent starts here:

```bash
cd /home/arlo/projects/parti/.claude/worktrees/iops-investigation
cat docs/plans/iops-investigation/findings.md          # what we know
cat docs/plans/iops-investigation/02-nats-tuning-plan.md  # this plan
git log --oneline -5                                   # latest commits
docker compose -f test/iops-investigation/docker/docker-compose.yaml --profile r3 ps  # rig state
```

Start at §3 (R1 research) of this plan.
