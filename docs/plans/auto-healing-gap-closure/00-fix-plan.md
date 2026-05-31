# Auto-Healing Gap Closure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the confirmed auto-healing fault-model gaps from `tmp/auto_healing_gap_analysis.md` with source-grounded policy, focused tests, and operator-facing documentation.

**Architecture:** Add policy first, then executable proof. The matrix is the source of truth for expected degraded/self-heal/operator behavior; synthetic seams close cheap deterministic gaps before gated real-cluster and environment probes cover failures that cannot be represented safely in normal CI.

**Tech Stack:** Go, NATS JetStream, existing `partitest` embedded NATS helpers, `test/integration/failure`, `test/integration/manager`, `source`, `consumer`, and `docs/`.

---

## Execution Status

- Task 1 fault matrix and reason taxonomy: implemented.
- Task 2 source timeout signaling: implemented and verified.
- Task 3 G4 handoff-only rebalance proof: implemented as an opt-in known
  failing proof; the proof fails against current production behavior. See
  `02-g4-handoff-rebalance-fix-plan.md`.
- Task 4 full NATS outage proof: implemented and verified.
- Task 5 N-node helper and gated Tier 2 selective-peer probe: implemented and opt-in verified.
- Task 6 wedged/read-only storage probe: implemented and opt-in verified; chmod StoreDir produced API error 10049 for new file-backed stream/bucket creation, while existing runtime surfaces stayed OK, so stronger runtime storage-fault injection remains an investigation finding.
- Task 7 dynamic generic permanent-failure policy: resolved as local durable-layer WARN/metric policy.
- Findings index: `03-findings-index.md` records evidence, status, and the implementation-fix boundary.

## File Map

- Create: `docs/plans/auto-healing-gap-closure/01-fault-matrix.md`
- Create: `docs/plans/auto-healing-gap-closure/03-findings-index.md`
- Modify: `docs/OPERATIONS.md`
- Modify: `docs/API_REFERENCE.md`
- Modify: `types/state.go`
- Modify: `partitest/nats.go`
- Modify: `partitest/doc.go`
- Modify: `partitest/nats_test.go`
- Create: `test/integration/failure/handoff_rebalance_writefault_test.go`
- Create: `test/integration/failure/full_nats_outage_test.go`
- Create: `test/integration/failure/quorum_loss_tier2_test.go`
- Create: `test/integration/failure/wedged_storage_probe_test.go`
- Modify: `source/nats_kv.go`
- Modify: `source/nats_kv_unavailable_hook_test.go`
- Modify: `consumer/dynamic.go`
- Modify: `consumer/dynamic_on_permanent_failure_test.go`

## Invariant

For every fault row, Parti must make one explicit choice: keep serving from committed cached state, self-heal in process, enter Degraded for restart/rotation, call an operator-owned hook, or intentionally stay local with a durable-layer log/metric. No fault row may rely on an implicit fall-through that is not named in source, tests, or docs.

## Task 1: Write The Fault Matrix And Reason Taxonomy

**Files:**
- Create: `docs/plans/auto-healing-gap-closure/01-fault-matrix.md`
- Modify: `docs/OPERATIONS.md`
- Modify: `docs/API_REFERENCE.md`
- Modify: `types/state.go`

- [ ] **Step 1: Create the matrix document**

Create `docs/plans/auto-healing-gap-closure/01-fault-matrix.md` with these rows at minimum:

```markdown
# Auto-Healing Fault Matrix

| ID | Connection | JetStream state | Operation surface | Subsystem | Timing | Expected policy | Reason / signal | Proof |
|---|---|---|---|---|---|---|---|---|
| M1 | connected | RF3 bucket quorum lost | `Get` after `Keys` | handoff claims | stable | in-process self-heal / retry | none | `TestResolverReadFault_ConsumerSurvivesQuorumLossWindow` |
| M2 | connected | manager KV quorum lost | heartbeat/election/stableid KV op | manager | stable | Degraded / rotate if sustained | `kv-unavailable` | `TestManager_KVUnavailable_EntersDegraded` |
| M3 | connected | Parti-owned bucket deleted | KV op | manager | stable | Degraded + restart/rotation | `KV error threshold exceeded` | `TestManager_LiveNATSBucketLoss` |
| M4 | connected | Parti-owned bucket recreated | bucket epoch monitor | manager | stable | Degraded + restart/rotation | `bucket-recreated:<bucket>` | `TestManager_BucketRecreated...` |
| M5 | reconnecting | NATS nodes stopped, reconnect budget unlimited | connection monitor | manager + dynamic consumer | stable | Degraded, keep cached assignment, recover to Stable | `NATS connection down` | planned `TestFullNATSOutage_UnlimitedReconnects_RecoversFleet` |
| M6 | closed | NATS nodes stopped, finite reconnect budget exhausted | connection monitor | manager | stable | Degraded/readiness rotation | `NATS connection down` or explicit closed-connection reason | planned `TestFullNATSOutage_FiniteReconnects_DegradesClosedConnection` |
| M7 | connected | handoff bucket write timeout only | `Create`/`Update` claim | handoff apply | rebalance | keep old data-plane assignment, retry, no false Stable for new assignment | retry log/metric only | opt-in known failing `TestHandoffOnlyWriteFault_RebalancePreservesOldOwners` |
| M8 | connected | source bucket missing/deleted | `Get`/`Watch` | `source.NatsKV` | reconcile | operator-owned recovery | source unavailable hook + gauge | `TestNatsKV_F6A_BucketMissing_FiresHookAndSetsMetric` |
| M9 | connected | source bucket quorum lost | `Get` deadline/no responders | `source.NatsKV` | reconcile | sustained source-unavailable signal, no bucket recreation | source unavailable hook + gauge | planned `TestNatsKV_SourceUnavailable_DeadlineExceededThreshold` |
| M10 | connected | stream missing | consumer info / pull | dynamic consumer | stable | manager Degraded if no user callback overrides | `stream-missing-recovery-exhausted` | `TestStreamMissingNoHook_RoutesPermanentFailureToManager` |
| M11 | connected | generic durable permanent failure | durable retry envelope | dynamic consumer | stable | TBD policy: local log/metric or manager Degraded | TBD typed reason if promoted | planned policy test |
| M12 | connected | read-only/wedged disk | write/read/watch/stream-info/consumer-info | all | any | fail loud according to observed surface | matrix-specific | planned gated probe |
```

- [ ] **Step 2: Update public reason taxonomy**

In `docs/OPERATIONS.md` and `docs/API_REFERENCE.md`, add a compact table with these reason meanings:

```markdown
| Reason | Class | Operator action |
|---|---|---|
| `NATS connection down` | ride-through if reconnecting | keep readiness degraded until NATS is stable; rotate only if closed/expired by policy |
| `kv-unavailable` | connected but KV quorum unavailable | readiness degraded; rotation is acceptable if outage exceeds SLO |
| `KV error threshold exceeded` | Parti-owned coordination data missing/lost | restart/rotate workers after confirming bucket loss |
| `bucket-recreated:<bucket>` | ambiguous Parti-owned data loss | restart/rotate workers; inspect JetStream storage |
| `startup-timeout` | startup apply/wait did not reach Stable in budget | readiness rotation unless recovery completes first |
| `assignment-watcher-exhausted` | assignment watcher retry envelope exhausted | restart/rotate worker; inspect assignment bucket and NATS logs |
| `stream-missing-recovery-exhausted` | dynamic consumer stream missing and no app hook recovered it | operator-owned stream recovery or worker rotation |
| `source-unavailable:<bucket>` | caller-owned source bucket unavailable | caller/operator recovers the source bucket; Parti does not recreate it |
```

- [ ] **Step 3: Clarify `StateDegraded` Godoc**

Change `types/state.go` from a connectivity-only description to a policy description:

```go
// StateDegraded indicates Parti has detected a fault that makes fresh
// coordination, source, or consumer state unreliable. Workers continue with
// last known safe local state where possible; the OnDegraded reason determines
// whether the intended response is ride-through, in-process recovery,
// readiness rotation, or operator-owned recovery.
StateDegraded
```

- [ ] **Step 4: Verify docs**

Run:

```bash
go test ./types -run TestNonExistent -count=0
git diff --check
```

Expected: both commands exit 0.

## Task 2: Close G5 Source-Bucket Sustained Timeout Signaling

**Files:**
- Modify: `source/nats_kv.go`
- Modify: `source/nats_kv_unavailable_hook_test.go`

- [ ] **Step 1: Write failing source timeout tests**

Add tests that inject `context.DeadlineExceeded` and assert no missing-bucket semantics are used until a threshold is crossed:

```go
func TestNatsKV_SourceUnavailable_DeadlineExceededThreshold(t *testing.T) {
	t.Parallel()

	src := NewNatsKV(&deadlineKV{}, "config", nil,
		WithReconcileInterval(10*time.Millisecond),
		WithUnavailableHook(rec.fn()),
		WithMetrics(gauge),
	)
	src.unavailableCooldown = 10 * time.Millisecond

	require.NoError(t, src.Start(t.Context()))
	t.Cleanup(func() { _ = src.Stop(t.Context()) })

	require.Eventually(t, func() bool {
		return rec.calls.Load() >= 1
	}, time.Second, 10*time.Millisecond)
	require.True(t, gauge.missing.Load())
	require.ErrorIs(t, rec.last(), context.DeadlineExceeded)
}
```

Run:

```bash
go test ./source -run TestNatsKV_SourceUnavailable_DeadlineExceededThreshold -count=1
```

Expected before implementation: FAIL because `context.DeadlineExceeded` falls through to the generic reconcile error and does not fire the hook.

- [ ] **Step 2: Implement the minimal classifier**

Add a separate helper so source timeout policy stays distinct from bucket deletion:

```go
func isSourceUnavailableErr(err error) bool {
	return isBucketUnavailableErr(err) ||
		errors.Is(err, context.DeadlineExceeded)
}
```

Then update `noteBucketUnavailable` call sites so source timeout drives the hook/gauge but log text does not say "bucket missing" unless `isBucketUnavailableErr(err)` is true.

- [ ] **Step 3: Verify source package**

Run:

```bash
go test ./source -run 'TestNatsKV_(F6A|SourceUnavailable)' -count=1
```

Expected: PASS.

## Task 3: Close G4 Handoff-Only Rebalance Proof

**Files:**
- Create: `test/integration/failure/handoff_rebalance_writefault_test.go`

- [ ] **Step 1: Write the failing proof test**

Build from the existing startup write-fault wrapper, but arm only the handoff claim bucket after a two-worker dynamic consumer is Stable. Force a rebalance by adding a worker.

Test oracle:

```go
func TestHandoffOnlyWriteFault_RebalancePreservesOldOwners(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	// Arrange two Stable workers and record owner per partition.
	// Arm handoff claim write faults only.
	// Add a third worker to force rebalance.
	// Assert the pre-fault owners continue consuming their old partitions.
	// Assert the new worker does not expose ownership until claims commit.
	// Disarm faults.
	// Assert retry converges, claims are Stable, and all partitions consume.
}
```

Run:

```bash
go test ./test/integration/failure -run TestHandoffOnlyWriteFault_RebalancePreservesOldOwners -count=1
```

Expected before implementation: the test must at least prove the current behavior. If it fails because new ownership is exposed before claim commit, stop and create a code fix plan before changing production code.

After the finding is recorded, keep the proof opt-in until the production fix
lands so the default PR gate does not fail on an intentionally open gap:

```bash
PARTI_RUN_HANDOFF_REBALANCE_PROOF=1 go test ./test/integration/failure -run TestHandoffOnlyWriteFault_RebalancePreservesOldOwners -count=1
```

- [ ] **Step 2: Implement the production fix per the G4 fix plan**

The proof exposed the bug, so the fix is scoped and reviewed separately in
`02-g4-handoff-rebalance-fix-plan.md` (see its "Implementation Outcome" header).
The delivered fix is the **worker-side removal guard**: a `RemovalGuard` hook in
the handoff coordinator plus a fail-closed `guardHandoffRemoval` in the manager
(positive allow-predicate, version/`_commit`-revision-keyed commit-set cache,
`parti_handoff_removal_pending` metric). Do not route handoff apply errors
through `recordKVOpError` by default; that would contradict the policy in the
`applyErr` branch of `applyAssignmentWithPrevCore`
(`manager_assignment.go:1327-1346`). A leader-side calculator capability gate
was tried and removed (bootstrap deadlock — see `02`); mixed-version safety is a
rollout contract, not an in-process gate.

- [ ] **Step 3: Verify**

Run:

```bash
PARTI_RUN_HANDOFF_REBALANCE_PROOF=1 go test ./test/integration/failure -run TestHandoffOnlyWriteFault_RebalancePreservesOldOwners -count=1
go test . -run 'TestApply|TestAttemptRecovery' -count=1
```

Expected: PASS.

## Task 4: Close G3 Full NATS Outage Proof

**Files:**
- Create: `test/integration/failure/full_nats_outage_test.go`

- [ ] **Step 1: Add unlimited reconnect fleet test**

Use embedded NATS with file storage intact. Start manager + dynamic consumer, stop all NATS servers, wait for Degraded, restart the same server(s), and assert Stable plus consumption resumes.

```go
func TestFullNATSOutage_UnlimitedReconnects_RecoversFleet(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	// Connect with nats.MaxReconnects(-1).
	// Start a manager and Dynamic consumer.
	// Stop NATS long enough for enterDegraded("NATS connection down").
	// Restart NATS with the same StoreDir.
	// Assert manager reaches Stable and post-restart messages are consumed.
}
```

- [ ] **Step 2: Add finite reconnect closed-connection test**

```go
func TestFullNATSOutage_FiniteReconnects_DegradesClosedConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	// Connect with nats.MaxReconnects(0) or a small positive value.
	// Stop NATS until the connection status is CLOSED.
	// Assert OnDegraded fires and readiness state does not claim self-healed Stable.
}
```

- [ ] **Step 3: Verify**

Run:

```bash
go test ./test/integration/failure -run 'TestFullNATSOutage_' -count=1
```

Expected: PASS.

## Task 5: Close G2 Real 5-Node RF3 Selective Peer Fault

**Files:**
- Modify: `partitest/nats.go`
- Modify: `partitest/doc.go`
- Modify: `partitest/nats_test.go`
- Create: `test/integration/failure/quorum_loss_tier2_test.go`

- [ ] **Step 1: Add an N-node cluster helper**

Add:

```go
func StartEmbeddedNATSClusterN(t *testing.T, clusterSize int) ([]*server.Server, *nats.Conn) {
	t.Helper()
	if clusterSize < 1 {
		t.Fatalf("clusterSize must be >= 1, got %d", clusterSize)
	}
	return startEmbeddedNATSClusterSized(t, clusterSize)
}
```

Refactor existing `StartEmbeddedNATSCluster` to call the shared helper with `3`, preserving its public behavior.

- [ ] **Step 2: Test helper sizes**

Add:

```go
func TestStartEmbeddedNATSClusterN_FiveNodes(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}

	servers, nc := StartEmbeddedNATSClusterN(t, 5)
	require.Len(t, servers, 5)
	require.True(t, nc.IsConnected())
	for _, s := range servers {
		require.GreaterOrEqual(t, s.NumRoutes(), 4)
	}
}
```

Run:

```bash
go test ./partitest -run 'TestStartEmbeddedNATSCluster' -count=1
```

Expected: PASS.

- [ ] **Step 3: Add gated Tier 2 test**

Create `TestRF3SelectivePeerFault_HandoffQuorumLoss` guarded by:

```go
if os.Getenv("PARTI_RUN_QUORUM_LOSS_TIER2") != "1" {
	t.Skip("set PARTI_RUN_QUORUM_LOSS_TIER2=1 to run")
}
```

Test oracle:

```go
// Start 5-node cluster.
// Create RF3 Parti KV buckets.
// Read handoff stream cluster info and identify the leader plus replicas.
// Stop two nodes that host handoff replicas while keeping JetStream meta quorum alive.
// Assert manager state and dynamic consumer data-plane behavior match the matrix.
// Restart nodes and assert recovery/convergence without process restart when policy says self-heal.
```

- [ ] **Step 4: Verify gated default and opt-in paths**

Run:

```bash
go test ./test/integration/failure -run TestRF3SelectivePeerFault_HandoffQuorumLoss -count=1
PARTI_RUN_QUORUM_LOSS_TIER2=1 go test ./test/integration/failure -run TestRF3SelectivePeerFault_HandoffQuorumLoss -count=1
```

Expected: first command skips cleanly; second command passes on machines that can run a 5-node embedded cluster.

## Task 6: Close G1 Wedged / Read-Only Storage Probe

**Files:**
- Create: `test/integration/failure/wedged_storage_probe_test.go`

- [ ] **Step 1: Add gated read-only file-store probe**

Guard the test:

```go
if os.Getenv("PARTI_RUN_WEDGED_STORAGE_PROBE") != "1" {
	t.Skip("set PARTI_RUN_WEDGED_STORAGE_PROBE=1 to run")
}
```

Probe:

```go
func TestWedgedStorage_ReadOnlyFileStore_RecordsOperationSurfaces(t *testing.T) {
	// Start file-backed embedded NATS with known StoreDir.
	// Create RF1 and RF3 streams/buckets as available.
	// chmod the relevant store subtree read-only.
	// Attempt Keys, Get, Watch, Create, Update, Put, StreamInfo, ConsumerInfo.
	// Record exact error classes in t.Logf and assert each maps to a matrix row.
}
```

- [ ] **Step 2: Keep the probe observational**

The first version must not change production classifiers. If a new error surface appears, update the matrix and create a follow-up production fix plan with the exact error chain and operation surface.

- [ ] **Step 3: Verify gated behavior**

Run:

```bash
go test ./test/integration/failure -run TestWedgedStorage_ReadOnlyFileStore_RecordsOperationSurfaces -count=1
PARTI_RUN_WEDGED_STORAGE_PROBE=1 go test ./test/integration/failure -run TestWedgedStorage_ReadOnlyFileStore_RecordsOperationSurfaces -count=1
```

Expected: first command skips cleanly; second command passes or produces a failing observation that names the unmapped operation surface.

## Task 7: Resolve G6 Dynamic Generic Permanent Failure Policy

**Files:**
- Modify: `consumer/dynamic.go`
- Modify: `consumer/dynamic_on_permanent_failure_test.go`
- Modify: `docs/plans/auto-healing-gap-closure/01-fault-matrix.md`

- [ ] **Step 1: Decide the policy in the matrix before code**

Choose one:

```markdown
| M11 | connected | generic durable permanent failure | durable retry envelope | dynamic consumer | stable | local WARN/metric only | durable permanent metric | `TestDynamic_onPermanentFailure_ManagerObserverOnlyOnStreamMissing` |
```

or:

```markdown
| M11 | connected | generic durable permanent failure | durable retry envelope | dynamic consumer | stable | manager Degraded | `dynamic-permanent-failure-exhausted` | planned test |
```

- [ ] **Step 2A: If local-only stays policy, document it**

Keep `consumer/dynamic.go` behavior unchanged and strengthen docs so this is explicit operator policy, not an accidental gap.

Run:

```bash
go test ./consumer -run TestDynamic_onPermanentFailure_ManagerObserverOnlyOnStreamMissing -count=1
```

Expected: PASS.

- [ ] **Step 2B: If manager Degraded becomes policy, write failing test first**

Add:

```go
func TestDynamic_onPermanentFailure_ManagerObserverOnConfiguredGenericExhaustion(t *testing.T) {
	// Install manager observer.
	// Trigger generic permanent failure.
	// Assert observer receives typed reason dynamic-permanent-failure-exhausted.
}
```

Run:

```bash
go test ./consumer -run TestDynamic_onPermanentFailure_ManagerObserverOnConfiguredGenericExhaustion -count=1
```

Expected before implementation: FAIL.

- [ ] **Step 3: Verify chosen policy**

Run:

```bash
go test ./consumer -run TestDynamic_onPermanentFailure -count=1
go test ./test/integration/failure -run TestStreamMissingNoHook_RoutesPermanentFailureToManager -count=1
```

Expected: PASS.

## Final Verification

- [ ] **Step 1: Format and go fix affected Go packages**

Run only after Go edits:

```bash
go fix ./partitest ./source ./consumer ./test/integration/failure
make fmt
```

- [ ] **Step 2: Run required focused checks**

```bash
go test . -run 'Test(MarkKVUnavailable|RecordKVError_ReadUnavailable_Degrades|Manager_MaxReconnects|Manager_warnOnFiniteMaxReconnects)' -count=1
go test ./partitest -run 'TestStartEmbeddedNATSCluster' -count=1
go test ./source -run 'TestNatsKV_(F6A|SourceUnavailable)' -count=1
go test ./consumer -run TestDynamic_onPermanentFailure -count=1
go test ./internal/durable -run TestQuorumLoss -count=1
go test ./test/integration/failure -run 'Test(ResolverReadFault_ConsumerSurvivesQuorumLossWindow|StartupWriteFault_SelfHealsWithoutRestart|StartupWriteFault_DegradedRecoveryDoesNotReportStableUncommitted|ClaimResolver_RecoversAfterNATSRestart|StreamMissingNoHook_RoutesPermanentFailureToManager|FullNATSOutage)' -count=1
PARTI_RUN_HANDOFF_REBALANCE_PROOF=1 go test ./test/integration/failure -run TestHandoffOnlyWriteFault_RebalancePreservesOldOwners -count=1
go test ./test/integration/manager -run 'TestManager_(KVUnavailable_EntersDegraded|LiveNATSBucketLoss|LiveNATSBucketLoss_OnDegradedHook|Restart_AfterNATSBucketLoss)' -count=1
```

- [ ] **Step 3: Run lint before any commit**

```bash
make lint
git diff --check
```

- [ ] **Step 4: Run pre-PR gate before opening a PR**

This plan touches `manager`-adjacent behavior, `source`, `consumer`, and `internal` test coverage. Before PR:

```bash
make pre-pr
```
