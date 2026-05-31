# G4 Handoff Rebalance Write-Fault Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

## Implementation Outcome (read first — supersedes Task 1 below)

The **worker-side removal guard is the fix** and is implemented:
- **Task 2** (handoff coordinator `RemovalGuard` hook) — done.
- **Task 3** (manager `guardHandoffRemoval`, fail-closed positive allow-predicate,
  version/`_commit`-revision-keyed commit-set cache, `parti_handoff_removal_pending`
  metric, retry routing that never enters the degraded circuit) — done. The G4
  proof `TestHandoffOnlyWriteFault_RebalancePreservesOldOwners` **passes** with
  these two alone.

**Task 1 (the leader-side calculator capability gate) was implemented and then
removed.** A post-implementation review confirmed it had a fatal bootstrap
deadlock: the target-side check refused to assign a transfer-gained partition to
a worker until it reported `CapProcessingGate`, but that capability is only
reported *after* a worker's first apply — so a freshly-joined worker (whose first
partitions are always transfers in steady state) could never be assigned work.
Capabilities are runtime wire-up state, not config intent, so a pre-assignment
gate on a post-apply capability is inherently circular. The whole gate (source +
target), the `CapHandoffRemovalGuard` bit, and the `RecordRebalanceDeferred`
metric were dropped.

**Mixed-version safety is now a rollout contract, not an in-process gate:** M7
safety holds for a fully-upgraded fleet running two-phase handoff with the
processing gate wired; complete the rolling upgrade before relying on M7. During
the upgrade window an un-upgraded worker carries the same M7 exposure it has on
current `main` — no regression, just no new in-process protection. A future
mixed-version defense (e.g. a startup-reported config-intent capability) is a
separate plan-review item and must not reuse the post-apply `CapProcessingGate`
as a pre-transfer predicate.

Task 1 below is retained for historical context only; **do not implement it.**

**Tasks 4 and 5 are done.**
- **Task 4 (gaining-worker-death liveness)** — `TestGuardHandoffRemoval_GainingWorkerDeath_Liveness`
  in `manager_handoff_liveness_test.go` wires the REAL sweep to the REAL guard:
  a real `twoPhaseCoordinator` sweep resets an expired
  `{owner: OLD, pending: NEW, prepare}` claim to `{owner: OLD, stable}`, and the
  manager guard then blocks OLD's removal. **Convergence dependency (rollout
  note):** when the gaining worker dies, the data plane stays *safe* (OLD keeps
  serving) but does NOT self-converge — `scheduleApplyRetry` replays the same
  failed commit and the guard keeps blocking. Convergence requires a *new*
  signal: a later assignment that returns the partition to OLD, or one that
  reassigns it to a live worker whose claim commits. The test asserts both the
  safety block (including no spurious self-convergence on retry) and both
  convergence paths.
- **Task 5 (proof + matrix + contract/concurrency sweep)** — the G4 proof
  `TestHandoffOnlyWriteFault_RebalancePreservesOldOwners` passes with the
  processing gate wired (via the shared `rfBuildWorkerStack`); it stays opt-in
  behind `PARTI_RUN_HANDOFF_REBALANCE_PROOF=1` (a 30–45s rebalance+write-fault
  timing test kept out of the default suite to avoid load-flakes — run it in a
  dedicated job, not unconditionally in CI). The matrix M7 row already names the
  `parti_handoff_removal_pending` signal and the `CapProcessingGate`-only
  prerequisite (the dropped `CapHandoffRemovalGuard` is intentionally absent).
  `TestHandoffRemovalGuard_NoRaceUnderConcurrentApplies`
  (`test/integration/failure/handoff_removal_guard_concurrency_test.go`) is the
  AGENTS.md-required `-race` stress test: it churns the partition set on a live
  3-worker two-phase cluster so the guard's commit/payload reads on
  `m.assignmentKV` race the assignment/commit watcher goroutines.

**Goal:** Keep pre-fault data-plane owners active during a handoff-bucket-only
write outage in a rebalance, then converge after claim writes recover — and make
that property hold under a rolling/mixed-version fleet, not only a homogeneous
fully-upgraded one.

**Architecture:** The failing proof shows the new (gaining) worker is gated
correctly, but old owners drop some partitions before the receiving worker can
commit ownership. The fix has three layers, in dependency order:

1. **Capability gate (leader-side, two-sided).** **(SUPERSEDED — see the
   Implementation Outcome header: this leader-side gate was implemented then
   removed due to a bootstrap deadlock; `CapHandoffRemovalGuard` was dropped and
   mixed-version source-side safety is a rollout contract, not an in-process
   gate. Layer 1 is retained only to explain the original three-layer design.)**
   The removal guard and the
   processing gate are *local* defenses on the losing and gaining workers
   respectively. A worker on an older binary reports `CapTwoPhaseHandoff` but may
   lack either, so the leader must gate BOTH endpoints of a transfer: do not move
   a partition away from a source that does not report `CapHandoffRemovalGuard`,
   and do not assign a transfer-gained partition to a target that does not report
   `CapProcessingGate` (Task 1 Step 3). This mirrors the existing safety-chain
   gate the audit path already uses on both ends
   (`internal/assignment/calculator_audit.go:15` — `requiredAuditCaps`; `:171`
   filters source/behind workers, `:185-190` filters target workers; with
   `cap_missing_behind` / `cap_missing_targets` metrics).
2. **Removal guard (worker-side).** Add a two-phase removal guard that runs
   before the consumer updater removes partitions: if a removed partition is
   still present in the current assignment batch and its handoff claim is not
   yet committed to a different owner, return a retryable apply error so Manager
   keeps the old local assignment and `scheduleApplyRetry` drives convergence.
   Partition-source deletion must still remove the local subject, so the guard
   only blocks **transfer** removals (partition still in the current commit
   batch), not globally removed partitions.
3. **Processing-gate prerequisite (explicit).** "The new owner does not expose
   uncommitted ownership" is only true when the consumer processing/pull gate is
   wired. That gate is optional and **default-disabled**
   (`internal/durable/processing_gate.go:15-17`;
   `internal/durable/worker_consumer.go:386-387` wraps handlers only when
   `ProcessingGate.Enabled` + a resolver are present). M7 safety therefore
   requires `CapProcessingGate`; the plan and matrix must say so, and the
   integration proof must run with the gate enabled.

**Tech Stack:** Go, Parti manager assignment commit path, internal handoff
coordinator, NATS JetStream KV handoff claims, leader calculator capability
filtering, existing integration failure harness.

---

## Safety Contract (read before implementing)

The G4 invariant — "during a handoff-bucket-only write outage in a rebalance,
pre-fault owners keep consuming, the gaining worker does not expose uncommitted
ownership, and the fleet converges after writes recover" — holds **only** under
this contract:

- **C1 (gate):** **(SUPERSEDED — the leader-side capability gate was removed; see
  the Implementation Outcome header. `CapHandoffRemovalGuard` does not exist and
  mixed-version source-side safety is a rollout contract, not an in-process gate.
  The C2 fencing requirement below still holds.)** The leader gates both
  endpoints of a two-phase transfer: a
  transfer *source* must report `CapHandoffRemovalGuard` before the leader moves
  a partition away from it, and a transfer *target* must report
  `CapProcessingGate` (see C2) before the leader assigns it a transfer-gained
  partition. During a rolling upgrade the leader itself must be upgraded for the
  gate to take effect (an old-binary leader runs old behavior); this is the same
  rollout limitation that already applies to `CapTwoPhaseHandoff` /
  `requiredAuditCaps`. Document this in the rollout notes.
- **C2 (fencing):** Workers report `CapProcessingGate`. Without it the consumer
  updater can expose the gaining worker while its claim is still `prepare`
  (`internal/assignment/handoff/twophase.go:80-91` runs the updater before
  `commitPhase`). The manager already WARNs on this misconfiguration
  (`manager.go` `maybeWarnMissingProcessingGate`, ~`:956`/`:985`).
- **C3 (fail-closed):** A transfer removal whose claim cannot be proven
  committed-to-another-owner is **retryable**, never silently allowed. This
  covers `rev == 0` (missing claim) for a partition still in the current commit
  batch.

## Current Evidence

Command:

```bash
PARTI_RUN_HANDOFF_REBALANCE_PROOF=1 go test ./test/integration/failure -run TestHandoffOnlyWriteFault_RebalancePreservesOldOwners -count=1 -v
```

Observed failure:

```text
during handoff write fault: new worker state=WaitingAssignment assignment_parts=3 writeFaults=6
old owners must keep consuming their pre-fault partitions while new ownership is uncommitted;
want old deltas=(8,4) got=(5,3) new_delta=0
```

Source evidence:

- `internal/assignment/handoff/twophase.go:64-67` returns on prepare write
  failure before the consumer updater, so the receiving worker is protected.
- `internal/assignment/handoff/twophase.go:80-91` applies the consumer updater
  after prepare succeeds and before `commitPhase`. A losing worker with no
  newly acquired partitions can reach this updater and remove subjects even
  while the receiver is failing claim writes.
- The `applyErr` branch of `applyAssignmentWithPrevCore`
  (`manager_assignment.go:1327-1346`) already defines the intended policy:
  handoff apply errors stay local, route through `scheduleApplyRetry`, and must
  not flow through `recordKVOpError` / manager Degraded.
- Claims are keyed by `Partition.SubjectKey()` (dot-joined), not `ID()`
  (dash-joined) (`internal/assignment/handoff/twophase.go` prepare/commit;
  `types/partition.go:60`). The guard and the diff helper must key the same way.
- During a transfer the claim reads `{owner: OLD, pendingOwner: NEW,
  state: prepare}` because `NextPrepare` keeps `Owner` unchanged
  (`internal/assignment/handoff/claims.go:66-70`) and `NextCommit` only then
  switches owner (`:76-80`). The guard's positive allow predicate (Task 3 Step 2)
  permits removal only for a non-empty owner *different from* this worker in
  `commit`/`stable` state, so while the claim still names OLD (any state) it
  keeps the OLD owner blocking until the transfer commits to NEW.

## File Map

- Modify: `types/heartbeat.go` (add `CapHandoffRemovalGuard`)
- Modify: `manager.go` (report the capability; add `handoffStore` field)
- Modify: `manager_setup.go` (wire guard + capability)
- Modify: `manager_assignment.go` (guard helper, diff helper, cached commit-set lookup)
- Modify: `internal/assignment/calculator.go` (gate transfer-removals by capability)
- Modify: `internal/assignment/calculator_audit.go` (extend or reuse the safety-chain constant)
- Modify: `internal/assignment/handoff/coordinator.go` (RemovalGuard API)
- Modify: `internal/assignment/handoff/twophase.go` (invoke guard before consumer update)
- Modify: `internal/assignment/handoff/twophase_test.go`
- Modify: `internal/assignment/calculator_test.go` (or a focused new test file)
- Modify: `types/metrics_collector.go` + `internal/metrics/prometheus.go` (removal-pending metric)
- Modify: `test/integration/failure/handoff_rebalance_writefault_test.go`
- Modify: `docs/plans/auto-healing-gap-closure/01-fault-matrix.md`

## Task 1: Add The Capability Gate (Mixed-Version Safety, P0)

**Files:**
- Modify: `types/heartbeat.go`
- Modify: `manager.go`, `manager_setup.go`
- Modify: `internal/assignment/calculator.go`, `internal/assignment/calculator_audit.go`
- Modify: `internal/assignment/calculator_test.go`

- [ ] **Step 1: Add the capability bit**

In `types/heartbeat.go`, alongside the existing bits
(`CapAckV1 = 1 << 0`, `CapTwoPhaseHandoff = 1 << 1`,
`CapProcessingGate = 1 << 2` at `:42-46`), add:

```go
// CapHandoffRemovalGuard indicates the manager runs the handoff removal guard
// that blocks transfer removals until the gaining worker commits its claim.
CapHandoffRemovalGuard uint32 = 1 << 3
```

Do **not** add it to `reportableCapBits` (`manager.go:927`). That set is only
sampled from the consumer updater's `CapabilityReporter` after apply
(`manager.go:944-955`); `CapHandoffRemovalGuard` is manager/coordinator-owned,
not consumer-updater state. It is reported solely via `SetCapability` in Step 2.
The heartbeat publisher reads the live capability function when building each
heartbeat (`internal/heartbeat/publisher.go:377-401`), so a `SetCapability` call
is sufficient to surface the bit.

- [ ] **Step 2: Report the capability after wiring the guard**

In `manager.go`, where `SetCapability(types.CapTwoPhaseHandoff, true)` fires
after `setupHandoff` (~`:563`), report `CapHandoffRemovalGuard` once the guard
is wired into the live coordinator (Task 2/3). Only report it when the guard
is actually installed, never on the constructor-time placeholder coordinator.

- [ ] **Step 3: Gate transfer-removals in the calculator (source AND target)**

A two-phase transfer is M7-safe only when BOTH endpoints carry their respective
capability: the losing worker cannot run a guard it does not have, and the
gaining worker cannot fence delivery before commit without a processing gate
(C1 + C2). So the leader must not generate a transfer unless both hold. Reuse
the existing capability-filter pattern — the audit path already filters BOTH the
reassignment source (behind) and the target by capability
(`internal/assignment/calculator_audit.go:15` defines `requiredAuditCaps`;
`:171` filters behind/source workers, `:185-190` filters target workers).

Add two sibling constants (do **not** silently widen `requiredAuditCaps`, which
is the audit-escalation chain):

```go
// requiredRemovalSourceCaps is the chain a worker must report before the leader
// will move a partition AWAY from it during a two-phase rebalance.
const requiredRemovalSourceCaps = types.CapTwoPhaseHandoff | types.CapHandoffRemovalGuard

// requiredTransferTargetCaps is the chain a worker must report before the
// leader will assign it a partition GAINED via two-phase transfer (fencing).
const requiredTransferTargetCaps = types.CapTwoPhaseHandoff | types.CapProcessingGate
```

Gate semantics, when `EnableTwoPhaseHandoff` is on (direct mode is unaffected,
matching the audit gate's scope):
- **Source check:** do not remove a partition from a *live, active* owner `W`
  unless `hbs[W].Capabilities & requiredRemovalSourceCaps == requiredRemovalSourceCaps`.
  Keep the partition on `W` (defer the transfer).
- **Target check:** do not assign a transfer-gained partition to worker `T`
  unless `hbs[T].Capabilities & requiredTransferTargetCaps == requiredTransferTargetCaps`.
  Leave the partition on its current owner (defer) rather than handing it to an
  unfenceable target.
- Record a skip metric mirroring `cap_missing_behind` / `cap_missing_targets`,
  e.g. `RecordRebalanceDeferred("cap_missing_removal_source", W)` and
  `RecordRebalanceDeferred("cap_missing_transfer_target", T)`.

**Heartbeat snapshot + absent-worker semantics (P1):** current rebalance has no
heartbeat map at assignment time — `collectRebalanceWorkers` returns only
`([]string, bool, error)` (`internal/assignment/calculator.go:1472`) and
`rebalance` then calls `Strategy.Assign(workers, partitions)` (`:1541-1564`,
`:1620-1625`). Add a heartbeat fetch via `WorkerMonitor.GetHeartbeats`
(`internal/assignment/worker_monitor.go:197`) and thread the resulting
`map[string]types.Heartbeat` into the gate. Failure / absence policy:
  - **KV read failure** (GetHeartbeats returns error): treat as
    inconclusive — defer the transfer (fail safe), do not move partitions.
  - **Active worker with missing/undecodable heartbeat** (GetHeartbeats silently
    omits decode failures per its Godoc): treat as *missing caps* → defer
    transfers off and onto it.
  - **Worker absent from the active set** (scale-down / removal): do NOT apply
    the removal-source gate — an absent worker cannot run a local removal path,
    and pinning its partitions would strand them. Reassignment of an absent
    worker's partitions is governed by the existing worker-removal semantics
    (workers not in `PublishInput.Workers` are implicitly revoked —
    `internal/assignment/assignment_publisher.go:401-413`,
    `types/assignment_commit.go:102-109`). The gate composes with — does not
    replace — the F10-A worker-shrink floor that already gates shrunk-worker-set
    rebalances on emergency-confirmed deaths (`internal/assignment/calculator.go:1579-1608`):
    the floor decides *whether* a shrink is real; this gate only constrains
    transfers among the live active set.

- [ ] **Step 4: Rollout note**

In the rollout notes for this plan, state C1 from the Safety Contract: the gate
only takes effect once the **leader** is upgraded; until then an old-binary
leader rebalances with pre-fix behavior, which is no worse than current `main`
(the existing M7 gap), i.e. no regression — but full mixed-version safety is
reached only after the whole fleet, including any worker that can win election,
is upgraded.

- [ ] **Step 5: Run the calculator unit checks**

```bash
go test ./internal/assignment -run 'TestCalculator.*Rebalance|TestCalculator.*Cap' -count=1
```

Expected: PASS. Add tests covering:
- **Source-missing/target-full:** active owner lacks `CapHandoffRemovalGuard` →
  transfer off it is deferred (partition stays).
- **Source-full/target-missing:** target lacks `CapProcessingGate` → transfer to
  it is deferred (partition stays on current owner).
- **Both-full:** fully-capable source and target → transfer proceeds.
- **Heartbeat read failure / undecodable active-worker heartbeat:** transfer
  deferred (fail safe).
- **Absent (confirmed-dead) source:** its partitions are reassignable under the
  existing worker-removal path, not pinned by the source gate.
- **Direct mode:** gate bypassed when `EnableTwoPhaseHandoff` is disabled.

## Task 2: Pin The Coordinator Guard Contract

**Files:**
- Modify: `internal/assignment/handoff/coordinator.go`
- Modify: `internal/assignment/handoff/twophase.go`
- Modify: `internal/assignment/handoff/twophase_test.go`

- [ ] **Step 1: Add the failing unit test**

Add this test to `internal/assignment/handoff/twophase_test.go`:

```go
func TestTwoPhase_RemovalGuardBlocksConsumerRemoval(t *testing.T) {
	t.Parallel()

	up := &mockUpdater{}
	guardCalls := atomic.Int64{}
	coord := New(Config{
		Store:           newMemStore(),
		ConsumerUpdater: up,
		RemovalGuard: func(ctx context.Context, workerID string, previous, next types.Assignment) error {
			guardCalls.Add(1)
			require.Equal(t, "w1", workerID)
			require.Len(t, previous.Partitions, 1)
			require.Empty(t, next.Partitions)
			return ErrRemovalPending
		},
		TTL: time.Minute,
	}, true)

	prev := types.Assignment{Version: 1, Partitions: []types.Partition{{Keys: []string{"p0"}}}}
	next := types.Assignment{Version: 2, Partitions: nil}

	err := coord.Apply(context.Background(), "w1", prev, next)
	require.ErrorIs(t, err, ErrRemovalPending)
	require.Equal(t, int64(1), guardCalls.Load())
	require.Equal(t, int64(0), up.calls.Load(), "consumer updater must not remove subjects while guard blocks")
}
```

- [ ] **Step 2: Run the unit RED**

```bash
go test ./internal/assignment/handoff -run TestTwoPhase_RemovalGuardBlocksConsumerRemoval -count=1
```

Expected before implementation: compile failure for missing `RemovalGuard` and `ErrRemovalPending`.

- [ ] **Step 3: Add the internal guard API**

In `internal/assignment/handoff/coordinator.go`, add:

```go
var ErrRemovalPending = errors.New("handoff removal pending")

type RemovalGuard func(ctx context.Context, workerID string, previous, next types.Assignment) error
```

Add this field to `Config`:

```go
// RemovalGuard optionally blocks consumer removal of partitions whose transfer
// has not committed yet. It must return ErrRemovalPending for retryable
// handoff-transfer waits.
RemovalGuard RemovalGuard
```

Add the `errors` import.

- [ ] **Step 4: Invoke the guard before consumer update**

In `internal/assignment/handoff/twophase.go`, immediately before the
`ConsumerUpdater.UpdateWorkerConsumer` phase (the `// Phase: apply` block at
`:80-91`), insert:

```go
if t.cfg.RemovalGuard != nil {
	if err := t.cfg.RemovalGuard(ctx, workerID, previous, next); err != nil {
		inst.finish(err)
		return err
	}
}
```

- [ ] **Step 5: Run the unit GREEN**

```bash
go test ./internal/assignment/handoff -run 'TestTwoPhase_(RemovalGuardBlocksConsumerRemoval|DelaysExposeIntermediateStates|MultiKeyPartition)' -count=1
```

Expected: PASS.

## Task 3: Implement Manager Transfer-Removal Detection (Fail-Closed)

**Files:**
- Modify: `manager_setup.go`
- Modify: `manager_assignment.go`
- Modify: `manager.go`
- Modify: `types/metrics_collector.go`, `internal/metrics/prometheus.go`

- [ ] **Step 1: Wire the guard into the real two-phase coordinator**

In `manager_setup.go`, add `RemovalGuard: m.guardHandoffRemoval,` to the
`handoff.Config` used in `setupHandoff`. Do not add the guard to the
constructor-time placeholder coordinator in `manager.go`; that coordinator has
no claim store yet and is replaced during `Start`.

- [ ] **Step 2: Add the manager guard helper (fail-closed on uncommitted transfer)**

Add this helper near the apply helpers in `manager_assignment.go`:

```go
func (m *Manager) guardHandoffRemoval(ctx context.Context, workerID string, previous, next Assignment) error {
	removed := removedPartitions(previous.Partitions, next.Partitions)
	if len(removed) == 0 {
		return nil
	}

	batch, err := m.currentCommitPartitionSet(ctx, next.Version)
	if err != nil {
		// Read failure is retryable, not fail-open: a guard read error
		// returns through scheduleApplyRetry (apply errors do not enter the
		// degraded circuit — manager_assignment.go:1327-1346).
		return fmt.Errorf("handoff removal guard commit read: %w", err)
	}
	if len(batch) == 0 {
		return nil
	}

	store, ok := m.handoffClaimStore()
	if !ok {
		return nil
	}
	for _, p := range removed {
		pid := p.SubjectKey()
		if _, transfer := batch[pid]; !transfer {
			// Globally removed (partition-source deletion) — release locally.
			continue
		}
		claim, rev, err := store.Get(ctx, pid)
		if err != nil {
			return fmt.Errorf("handoff removal guard claim read %s: %w", pid, err)
		}
		// FAIL CLOSED: a transfer removal is safe ONLY when the claim proves a
		// DIFFERENT owner now holds the partition. Use a POSITIVE allow
		// predicate; a negative block list lets unsafe shapes through —
		// self-owned commit, different-owner prepare/abort/unknown, empty
		// owner. NextCommit only sets Owner=PendingOwner
		// (internal/assignment/handoff/claims.go:76-84), and resume treats
		// owner==self && state==commit as still-owned-by-self
		// (manager_handoff.go:124-130), so a self-commit is NOT proof of
		// transfer. Allow removal only for a non-empty owner different from
		// this worker in a post-switch ownership state (commit or stable).
		committedElsewhere := rev != 0 &&
			claim.Owner != "" &&
			claim.Owner != workerID &&
			(claim.State == handoff.ClaimStateCommit || claim.State == handoff.ClaimStateStable)
		if !committedElsewhere {
			m.metrics.RecordHandoffRemovalPending(workerID)
			m.logger.Debug("handoff removal deferred: transfer not committed",
				"worker_id", workerID, "partition_id", pid,
				"claim_owner", claim.Owner, "claim_state", string(claim.State), "rev", rev)
			return handoff.ErrRemovalPending
		}
	}

	return nil
}
```

> Rationale for fail-closed (P0): the claim store returns `rev == 0` for a
> missing key (`internal/assignment/handoff/kv_store.go:77-82`). A swept,
> aged-out (handoff bucket MaxAge — see `manager_setup.go` handoff TTL note), or
> never-populated claim during a write-fault transfer would otherwise let the
> OLD owner drop a still-assigned partition — the exact M7 dark-partition
> outcome. The guard only fails open for partitions **absent from the current
> commit batch**, which is the legitimate source-deletion release path.

The implementation requires a manager-owned claim-store reference. Add this
private field to `Manager` in `manager.go`:

```go
handoffStore handoff.ClaimStore
```

Set it in `setupHandoff` after `store := handoff.NewNATSClaimStore(...)`:

```go
m.handoffStore = store
```

Add:

```go
func (m *Manager) handoffClaimStore() (handoff.ClaimStore, bool) {
	if m.handoffStore == nil {
		return nil, false
	}
	return m.handoffStore, true
}
```

- [ ] **Step 3: Add the partition diff helper**

```go
func removedPartitions(previous, next []types.Partition) []types.Partition {
	nextSet := make(map[string]struct{}, len(next))
	for _, p := range next {
		nextSet[p.SubjectKey()] = struct{}{}
	}
	removed := make([]types.Partition, 0)
	for _, p := range previous {
		if _, ok := nextSet[p.SubjectKey()]; !ok {
			removed = append(removed, p)
		}
	}
	return removed
}
```

- [ ] **Step 4: Add current commit batch lookup with a version-keyed cache (P1 cost)**

Add `currentCommitPartitionSet(ctx, version)` in `manager_assignment.go`. It must
read `assignment._commit`, verify the version matches `next.Version`, fetch each
payload referenced by `commit.Payloads`, and return a `map[string]struct{}`
keyed by `Partition.SubjectKey()`. Use the same payload reader as
`buildAssignmentFromCommit` (`manager_assignment.go:1063`):

```go
payload, err := assignment.FetchAndVerifyCommitPayload(ctx, m.assignmentKV, ref)
```

Return an empty map when the current commit version does not match `version`
(avoids blocking stale alias paths).

**Cache requirement:** `FetchAndVerifyCommitPayload` does a KV `Get` + gzip
decode + sha256 + JSON decode + digest check per payload
(`internal/assignment/commit_payload_fetch.go:43-99`), and `scheduleApplyRetry`
repeats the failed assignment on a 1s→30s backoff
(`manager_assignment.go`), so a naive lookup re-fans-out N payload reads on every
retry tick while holding `applyStoreMu`. Cache the partition set keyed by `(commit version, commit identity)`, where
**commit identity is the `assignment._commit` KV entry revision** (the `uint64`
returned by the KV `Get` of `_commit`) — not `AssignmentCommit.PrevCommitRev`,
which is diagnostic-only and does not identify the entry itself
(`types/assignment_commit.go:116-119`). Invalidate when a newer version or a
different `_commit` revision for the same version is observed (guards against a
commit replaced at the same version). The `len(removed) == 0` short-circuit in
`guardHandoffRemoval` already avoids the fan-out when nothing is removed; the
cache covers the repeated-retry case.

- [ ] **Step 5: Add the removal-pending metric (P2 — closes matrix M7 signal)**

Add a bounded metric to `types/metrics_collector.go` and
`internal/metrics/prometheus.go` (mirror the existing apply/handoff metric
surface, e.g. `types/metrics_collector.go:58`, `internal/metrics/prometheus.go:555`):

```go
// RecordHandoffRemovalPending counts guard blocks of a transfer removal while
// the gaining worker has not committed its claim. Worker-scoped label only, to
// keep cardinality bounded — matching RecordApplyAttempt which intentionally
// discards version (types/metrics_collector.go:58, internal/metrics/prometheus.go:555).
RecordHandoffRemovalPending(workerID string)
```

The metric carries no `partitionID` label (cardinality). Per-partition detail is
emitted on the debug log line in `guardHandoffRemoval` (Step 2), not as a label.

- [ ] **Step 6: Run the manager unit checks**

```bash
go test . -run 'TestApply|TestAttemptRecovery|TestCasToStableFromWaitingAssignment|TestGuardHandoffRemoval' -count=1
```

Expected: PASS. Add `guardHandoffRemoval` unit tests covering the full
positive-predicate state matrix:
- **Allows** (committed-elsewhere): different-owner `commit`; different-owner `stable`.
- **Blocks** (`ErrRemovalPending`): self-owner `commit`; self-owner `stable`;
  self-owner `prepare`; different-owner `prepare`; different-owner `abort`;
  different-owner `unknown`; empty owner; **missing claim (`rev==0`) in current batch**.
- **Allows** (not a transfer): source-deletion partition absent from the current
  commit batch.
- **Retryable, not Degraded:** claim-read error and commit-read error return a
  wrapped error that routes through `scheduleApplyRetry`.
- **No block:** commit version mismatch returns an empty batch.

## Task 4: Document And Test Gaining-Worker-Death Liveness (P1)

**Files:**
- Modify: `docs/plans/auto-healing-gap-closure/02-g4-handoff-rebalance-fix-plan.md` (this section)
- Modify: `internal/assignment/handoff/twophase_test.go` or `test/integration/failure/handoff_rebalance_writefault_test.go`

The guard makes the data plane *safe* when the gaining worker dies mid-handoff,
but it does **not** by itself make the fleet *converge against the same commit*:

- `maybeSweepClaims` resets an expired non-stable claim back to `stable` and
  clears `PendingOwner` (`internal/assignment/handoff/twophase.go:467-487`); it
  does not delete a stable claim for a still-owned partition. So after sweep the
  claim is `{owner: OLD, state: stable}`.
- With the current commit still assigning that partition away from OLD, the
  guard keeps blocking: a `{owner: OLD, state: stable}` claim is owned by *this*
  worker, so it never satisfies the positive "committed to a different owner"
  allow predicate (Task 3 Step 2). OLD keeps serving — safe — but does not
  converge to the assignment that moved the partition away.
- **Convergence dependency:** either the gaining worker returns and commits, or
  the leader publishes a *later* assignment that assigns the partition to a live
  worker. `scheduleApplyRetry` alone retries the same failed commit and will not
  converge against a dead gaining worker. State this dependency in the rollout
  notes.

- [x] **Step 1: Add the liveness test** — done:
`TestGuardHandoffRemoval_GainingWorkerDeath_Liveness` (`manager_handoff_liveness_test.go`).

Simulate `{owner: OLD, pendingOwner: NEW, state: prepare}`, stop NEW, let the
sweep run, assert OLD keeps serving and the guard still blocks the stale
removal, then publish a later assignment returning the partition to OLD (or to a
third live worker) and assert convergence.

## Task 5: Verify The G4 Integration Proof (Gate Enabled)

**Files:**
- Modify: `test/integration/failure/handoff_rebalance_writefault_test.go`
- Modify: `docs/plans/auto-healing-gap-closure/01-fault-matrix.md`

- [x] **Step 1: Ensure the proof runs the full safety stack (C2)**

The proof must enable the processing/pull gate (as the existing M7-adjacent
proof does — `test/integration/failure/resolver_readfault_test.go:226-230`
enables pull gating and allows only Stable/Commit states). M7 safety is only
claimed for `CapProcessingGate` deployments; assert the gate is wired.

- [x] **Step 2: Run the failing proof again**

```bash
PARTI_RUN_HANDOFF_REBALANCE_PROOF=1 go test ./test/integration/failure -run TestHandoffOnlyWriteFault_RebalancePreservesOldOwners -count=1 -v
```

Expected after implementation: PASS. The log should still show `writeFaults>0`;
old-owner deltas must meet the pre-fault owner counts and `new_delta=0` during
the fault window.

- [x] **Step 3: Update the matrix proof + prerequisite cells**

In `docs/plans/auto-healing-gap-closure/01-fault-matrix.md`, the M7 proof now
names `TestHandoffOnlyWriteFault_RebalancePreservesOldOwners`, the expected-policy
cell carries the `CapProcessingGate` prerequisite (the `CapHandoffRemovalGuard`
gate was dropped — see the Implementation Outcome header; do NOT add it), and the
signal cell names the concrete metric (`parti_handoff_removal_pending` / the
apply-retry log). Already reflected in the matrix; no further edit needed.

- [x] **Step 4: Run focused regression + contract checks**

```bash
go test ./internal/assignment/handoff -run 'TestTwoPhase' -count=1
go test ./internal/assignment -run 'TestCalculator' -count=1
go test ./test/integration/failure -run TestStartupWriteFault -count=1
PARTI_RUN_HANDOFF_REBALANCE_PROOF=1 go test ./test/integration/failure -run TestHandoffOnlyWriteFault -count=1
```

Cross-feature contracts (AGENTS.md) — a classification/apply-path change must
not regress these:

```bash
go test ./test/integration/manager -run 'TestManager_(LiveNATSBucketLoss|LiveNATSBucketLoss_OnDegradedHook)' -count=1 -race
go test ./test/integration/stableid -run TestStableID_StaleKeyTakeover_Reclaim -count=1 -race
go test . -run 'TestStart_ReturnsBeforeStable|TestCasToStableFromWaitingAssignment|TestStartupAsync_CalculatorStateNotClobbered' -count=1
```

Expected: PASS.

## Final Verification

- [ ] **Step 1: Format and go fix affected packages**

```bash
go fix ./internal/assignment/handoff ./internal/assignment ./test/integration/failure
make fmt
```

- [ ] **Step 2: Run required gates**

```bash
make lint
make test
git diff --check
```

Expected: all commands exit 0.

- [ ] **Step 3: Run pre-PR gate before PR**

This touches `manager`, `internal/assignment`, and `internal/assignment/handoff`,
so run:

```bash
make pre-pr
```

The guard adds claim-store + assignment-commit KV reads inside the apply
critical section (`applyStoreMu`), and this repo has a history of nats.go cached
`*stream` concurrency races under live load. `make pre-pr` chains the
race-enabled integration suite (`Makefile`), but additionally add a focused
race check covering guarded applies while assignment/commit watchers are active
(template: `test/integration/manager/epoch_monitor_concurrency_test.go`).

Expected: PASS before opening a PR.
