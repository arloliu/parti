package manager_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// e2eTestConfig returns a base parti.Config tuned for end-to-end invariant
// tests that need the full safety chain (CapAckV1 | CapTwoPhaseHandoff |
// CapProcessingGate) enabled. Uses IntegrationTestConfig defaults
// (HeartbeatTTL=5s) so the audit grace window in test #70
// (ApplyGracePeriod + ExtendedApplyGracePeriod = 5 × HeartbeatTTL = 25s)
// stays predictable and within a tractable wall-clock budget.
func e2eTestConfig(suffix string) parti.Config {
	cfg := testutil.IntegrationTestConfig()
	cfg.WorkerIDPrefix = "e2e-worker-" + suffix
	cfg.EnableTwoPhaseHandoff = true
	// Per-test KV bucket names so parallel tests do not collide on the
	// shared embedded NATS server.
	cfg.KVBuckets.StableIDBucket = "parti-stableid-" + suffix
	cfg.KVBuckets.ElectionBucket = "parti-election-" + suffix
	cfg.KVBuckets.HeartbeatBucket = "parti-heartbeat-" + suffix
	cfg.KVBuckets.AssignmentBucket = "parti-assignment-" + suffix
	cfg.KVBuckets.HandoffBucket = "parti-handoff-" + suffix
	cfg.KVBuckets.HandoffTTL = 30 * time.Second

	return cfg
}

// capReportingUpdater is a minimal WorkerConsumerUpdater that also implements
// CapabilityReporter so the manager OR-s CapProcessingGate into its capability
// bitmask after the first Apply attempt. This satisfies the audit's
// requiredAuditCaps gate (see internal/assignment/calculator_audit.go:15)
// without spinning up a real consumer + JetStream stream.
//
// When failFn is non-nil it is consulted on every UpdateWorkerConsumer call.
// Returning a non-nil error causes the handoff coordinator's apply phase to
// fail, which in turn keeps the worker's heartbeat AppliedDigest stale and
// drives the leader-side audit into escalation.
type capReportingUpdater struct {
	workerID string
	// failFn returns nil to succeed, non-nil to fail this apply attempt.
	// May be nil (always succeed). Safe to read concurrently with Capabilities.
	failFn atomic.Pointer[func(callIdx int) error]
	calls  atomic.Int64
}

func newCapReportingUpdater(workerID string) *capReportingUpdater {
	return &capReportingUpdater{workerID: workerID}
}

// UpdateWorkerConsumer satisfies parti.WorkerConsumerUpdater. It records call
// counts, consults failFn for an error decision, and (on success) is treated
// by Capabilities as having "wired" the processing gate.
func (u *capReportingUpdater) UpdateWorkerConsumer(_ context.Context, _ string, _ []parti.Partition) error {
	idx := int(u.calls.Add(1))
	if fnPtr := u.failFn.Load(); fnPtr != nil && *fnPtr != nil {
		if err := (*fnPtr)(idx); err != nil {
			return err
		}
	}

	return nil
}

// Capabilities satisfies parti.CapabilityReporter. It always advertises
// CapProcessingGate because this fake updater stands in for a real consumer
// whose handlers would be wrapped by the processing gate at construction
// time. Monotonic by construction (constant return).
func (u *capReportingUpdater) Capabilities() uint32 {
	return types.CapProcessingGate
}

// setFailFn installs (or clears) the failure function atomically. Tests use
// this to start with persistent failures and then "heal" the updater after
// the audit has had time to escalate.
func (u *capReportingUpdater) setFailFn(fn func(callIdx int) error) {
	if fn == nil {
		u.failFn.Store(nil)
		return
	}
	u.failFn.Store(&fn)
}

// makeE2EPartitions returns n distinct partitions with deterministic keys.
func makeE2EPartitions(n int) []types.Partition {
	out := make([]types.Partition, n)
	for i := range out {
		out[i] = types.Partition{
			Keys:   []string{fmt.Sprintf("e2e-part-%02d", i)},
			Weight: 100,
		}
	}

	return out
}

// invariantHolds checks the three invariants required by the end-to-end tests:
//
//	(a) Every partition CanonicalID in the expected set appears in exactly one
//	    active worker's CurrentAssignment.
//	(b) The active worker fleet matches the leader's most-recent
//	    AssignmentCommit.Workers set — no active worker is missing from the
//	    commit (would mean the leader hasn't yet seen it), no commit worker
//	    is missing from the active fleet (would mean a stale pre-failover
//	    commit). This is what proves #69's post-failover convergence: the
//	    new leader's commit must describe the surviving workers.
//	(c) For every active worker, the published heartbeat carries
//	    AppliedDigest == commit.Payloads[W].SetDigest and AppliedVersion ==
//	    commit.Version. Checking against the ACTIVE fleet (not just
//	    commit.Workers) catches a stopped-but-stale leader whose commit
//	    doesn't reflect the live cluster.
//
// Returns nil on success, otherwise a descriptive error suitable for surfacing
// via t.Fatalf.
func invariantHolds(
	ctx context.Context,
	cluster *testutil.WorkerCluster,
	assignKV jetstream.KeyValue,
	hbKV jetstream.KeyValue,
	expected []types.Partition,
) error {
	// --- (a) Ownership coverage across active workers.
	active := cluster.GetActiveWorkers()
	if len(active) == 0 {
		return errors.New("no active workers")
	}

	activeIDs := make(map[string]struct{}, len(active))
	owners := make(map[string]string, len(expected)) // canonicalID -> workerID
	for _, mgr := range active {
		wid := mgr.WorkerID()
		activeIDs[wid] = struct{}{}
		for _, p := range mgr.CurrentAssignment().Partitions {
			cid := p.CanonicalID()
			if other, dup := owners[cid]; dup {
				return fmt.Errorf("partition %q owned by both %q and %q", cid, other, wid)
			}
			owners[cid] = wid
		}
	}
	for _, p := range expected {
		cid := p.CanonicalID()
		if _, ok := owners[cid]; !ok {
			return fmt.Errorf("partition %q has no owner", cid)
		}
	}
	if len(owners) != len(expected) {
		return fmt.Errorf("owner count %d differs from expected %d (extra ownership entries)", len(owners), len(expected))
	}

	commit, err := fetchAssignmentCommit(ctx, assignKV)
	if err != nil {
		return fmt.Errorf("fetch commit: %w", err)
	}

	hbs, err := fetchAllHeartbeats(ctx, hbKV)
	if err != nil {
		return fmt.Errorf("fetch heartbeats: %w", err)
	}

	// --- (b) Active fleet ↔ commit.Workers match.
	commitWorkers := make(map[string]struct{}, len(commit.Workers))
	for _, w := range commit.Workers {
		commitWorkers[w] = struct{}{}
	}
	for wid := range activeIDs {
		if _, ok := commitWorkers[wid]; !ok {
			return fmt.Errorf("active worker %q not in commit.Workers (commit Version=%d, Workers=%v)",
				wid, commit.Version, commit.Workers)
		}
	}
	for _, w := range commit.Workers {
		if _, ok := activeIDs[w]; !ok {
			return fmt.Errorf("commit worker %q not in active fleet (commit Version=%d, active=%v)",
				w, commit.Version, sortedKeys(activeIDs))
		}
	}

	// --- (c) Digest + version convergence for every ACTIVE worker.
	for wid := range activeIDs {
		ref, ok := commit.Payloads[wid]
		if !ok {
			return fmt.Errorf("commit malformed: active worker %q missing Payloads entry", wid)
		}
		hb, ok := hbs[wid]
		if !ok {
			return fmt.Errorf("active worker %q heartbeat missing", wid)
		}
		if hb.AppliedVersion != commit.Version {
			return fmt.Errorf("active worker %q AppliedVersion=%d, commit.Version=%d",
				wid, hb.AppliedVersion, commit.Version)
		}
		if hb.AppliedDigest != ref.SetDigest {
			return fmt.Errorf("active worker %q AppliedDigest=%#x, commit.Payloads[%q].SetDigest=%#x",
				wid, hb.AppliedDigest, wid, ref.SetDigest)
		}
	}

	return nil
}

// sortedKeys returns m's keys sorted lexicographically. Used only for
// deterministic error messages in invariant assertions.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)

	return out
}

// errAssignmentCommitNotYetWritten is returned by fetchAssignmentCommit when
// the assignment._commit key has not been written yet (typically pre-bootstrap).
// Callers treat this as a transient retry signal rather than a hard failure.
var errAssignmentCommitNotYetWritten = errors.New("assignment commit not yet written")

// fetchAssignmentCommit reads "assignment._commit" from the pre-opened
// assignment KV handle and unmarshals it. Returns
// errAssignmentCommitNotYetWritten when the commit key does not exist yet
// so callers can branch on the sentinel.
//
// The raw key "assignment._commit" is the wire-format key the production
// publisher writes (internal/assignment/assignment_publisher.go); this E2E
// test legitimately reads it directly to validate convergence.
func fetchAssignmentCommit(ctx context.Context, kv jetstream.KeyValue) (*types.AssignmentCommit, error) {
	entry, err := kv.Get(ctx, "assignment._commit")
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, errAssignmentCommitNotYetWritten
		}

		return nil, fmt.Errorf("get assignment._commit: %w", err)
	}
	var commit types.AssignmentCommit
	if err := json.Unmarshal(entry.Value(), &commit); err != nil {
		return nil, fmt.Errorf("decode assignment._commit: %w", err)
	}

	return &commit, nil
}

// fetchAllHeartbeats walks the pre-opened heartbeat KV handle and returns
// the decoded heartbeat for every worker key currently present.
func fetchAllHeartbeats(ctx context.Context, kv jetstream.KeyValue) (map[string]types.Heartbeat, error) {
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return map[string]types.Heartbeat{}, nil
		}

		return nil, fmt.Errorf("list heartbeat keys: %w", err)
	}

	result := make(map[string]types.Heartbeat, len(keys))
	const prefix = "heartbeat."
	for _, k := range keys {
		entry, gerr := kv.Get(ctx, k)
		if gerr != nil {
			// Tolerate transient races: a key can be expired by TTL between
			// Keys() and Get(). Skip rather than fail.
			if errors.Is(gerr, jetstream.ErrKeyNotFound) {
				continue
			}

			return nil, fmt.Errorf("get heartbeat key %q: %w", k, gerr)
		}
		hb, derr := types.DecodeHeartbeat(entry.Value())
		if derr != nil {
			return nil, fmt.Errorf("decode heartbeat key %q: %w", k, derr)
		}
		result[strings.TrimPrefix(k, prefix)] = hb
	}

	return result, nil
}

// requireInvariantEventually polls the invariant on a 200ms cadence until it
// holds or timeout elapses. On failure, t.Fatalf surfaces the MOST RECENT
// invariant violation rather than a generic "condition never satisfied"
// message — useful for diagnosing which assertion broke.
//
// Pre-opens the assignment + heartbeat KV handles once so each poll cycle
// reuses them (saves ~2 NATS round-trips per tick).
func requireInvariantEventually(
	t *testing.T,
	ctx context.Context,
	cluster *testutil.WorkerCluster,
	js jetstream.JetStream,
	cfg parti.Config,
	expected []types.Partition,
	timeout time.Duration,
) {
	t.Helper()

	assignKV, err := js.KeyValue(ctx, cfg.KVBuckets.AssignmentBucket)
	require.NoError(t, err, "open assignment bucket %q", cfg.KVBuckets.AssignmentBucket)
	hbKV, err := js.KeyValue(ctx, cfg.KVBuckets.HeartbeatBucket)
	require.NoError(t, err, "open heartbeat bucket %q", cfg.KVBuckets.HeartbeatBucket)

	var (
		lastErr error
		mu      sync.Mutex // require.Eventually polls the condition from a separate goroutine
	)
	ok := assert.Eventually(t, func() bool {
		err := invariantHolds(ctx, cluster, assignKV, hbKV, expected)
		mu.Lock()
		lastErr = err
		mu.Unlock()

		return err == nil
	}, timeout, 200*time.Millisecond)
	if !ok {
		mu.Lock()
		err := lastErr
		mu.Unlock()
		if err != nil {
			t.Fatalf("invariant not satisfied within %s: %v", timeout, err)
		}
		t.Fatalf("invariant not satisfied within %s (no error captured)", timeout)
	}
}

// startWorkersWithCapUpdater stands up `count` workers backed by capReportingUpdater
// instances and returns them in worker-index order alongside the cluster. The
// returned updaters are aligned with cluster.Workers by index.
func startWorkersWithCapUpdater(
	t *testing.T,
	ctx context.Context,
	cluster *testutil.WorkerCluster,
	count int,
) []*capReportingUpdater {
	t.Helper()

	updaters := make([]*capReportingUpdater, count)
	for i := range count {
		upd := newCapReportingUpdater(fmt.Sprintf("idx-%d", i))
		updaters[i] = upd
		cluster.AddWorkerWithOptions(ctx, parti.WithWorkerConsumerUpdater(upd))
	}
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(15 * time.Second)
	cluster.WaitForLeader(10 * time.Second)

	return updaters
}

// TestE2E_AddPartitionsConvergesToInvariant verifies that adding partitions
// to a NatsKV-backed source while 3 workers are running causes the cluster to
// converge on a state where (a) every partition has exactly one owner and (b)
// every worker's AppliedDigest matches its commit.Payloads[W].SetDigest.
func TestE2E_AddPartitionsConvergesToInvariant(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := e2eTestConfig("add")
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "parti-source-add"})
	require.NoError(t, err)

	// Seed the source with a single partition so workers have something to
	// own at start-up (avoids a startup edge where every worker has empty
	// assignment and the commit map is degenerate).
	seed := makeE2EPartitions(1)
	src := source.NewNatsKV(kv, "config", nil)
	require.NoError(t, src.Update(ctx, seed))
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(context.Background()) })

	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	t.Cleanup(cluster.StopWorkers)

	_ = startWorkersWithCapUpdater(t, ctx, cluster, 3)

	// Initial invariant should hold for the seed partition.
	requireInvariantEventually(t, ctx, cluster, js, cfg, seed, 20*time.Second)

	// Add 10 more partitions via Modify, leaving the seed in place.
	addedSet := makeE2EPartitions(11) // includes seed at index 0 by construction
	require.NoError(t, src.Modify(ctx, func(_ []types.Partition) []types.Partition {
		return append([]types.Partition{}, addedSet...)
	}))

	// Convergence: ownership invariant + digest invariant for the full set.
	requireInvariantEventually(t, ctx, cluster, js, cfg, addedSet, 45*time.Second)
}

// TestE2E_PartitionAdditionDuringLeaderChange verifies that the invariant
// holds after a leader is stopped immediately after a partition Modify call.
// Whether the old leader managed to publish the new commit before stopping
// is irrelevant — the new leader must converge the cluster on the invariant.
func TestE2E_PartitionAdditionDuringLeaderChange(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := e2eTestConfig("leader")
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "parti-source-leader"})
	require.NoError(t, err)

	seed := makeE2EPartitions(1)
	src := source.NewNatsKV(kv, "config", nil)
	require.NoError(t, src.Update(ctx, seed))
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(context.Background()) })

	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	t.Cleanup(cluster.StopWorkers)

	_ = startWorkersWithCapUpdater(t, ctx, cluster, 3)

	leader := cluster.WaitForLeader(10 * time.Second)
	require.NotNil(t, leader)
	oldLeaderID := leader.WorkerID()
	t.Logf("initial leader=%s", oldLeaderID)

	// Wait until the initial commit is observable so we know the cluster
	// is past bootstrap (otherwise the leader kill below races bootstrap).
	requireInvariantEventually(t, ctx, cluster, js, cfg, seed, 20*time.Second)

	addedSet := makeE2EPartitions(11)
	require.NoError(t, src.Modify(ctx, func(_ []types.Partition) []types.Partition {
		return append([]types.Partition{}, addedSet...)
	}))

	// Stop the leader immediately after the Modify. We do not try to time
	// the kill into a specific publish phase — the invariant must hold
	// regardless of whether the old leader's commit landed first.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	stopErr := leader.Stop(stopCtx)
	stopCancel()
	t.Logf("old leader stop err: %v", stopErr)

	// New leader must take over before we can converge.
	newLeader := cluster.WaitForNewLeader(oldLeaderID, 30*time.Second)
	require.NotNil(t, newLeader)
	t.Logf("new leader=%s", newLeader.WorkerID())

	// The active fleet is now 2 workers; assert eventual convergence.
	requireInvariantEventually(t, ctx, cluster, js, cfg, addedSet, 60*time.Second)
}

// TestE2E_PartialApplyFailureRecovery verifies that the leader-side audit
// (see internal/assignment/calculator_audit.go::maybeEscalateAudit) escalates
// when one worker repeatedly fails its handoff apply, reassigning that
// worker's partitions to a healthy peer. After the audit fires the failure
// injector is lifted, and the cluster must converge on the invariant.
//
// All workers register a capReportingUpdater so the audit's
// requiredAuditCaps gate (CapAckV1 | CapTwoPhaseHandoff | CapProcessingGate)
// is satisfied on both the behind worker (so it is escalatable) and at
// least one target (so the audit has somewhere to move the load).
func TestE2E_PartialApplyFailureRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := e2eTestConfig("partial")
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "parti-source-partial"})
	require.NoError(t, err)

	seed := makeE2EPartitions(1)
	src := source.NewNatsKV(kv, "config", nil)
	require.NoError(t, src.Update(ctx, seed))
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(context.Background()) })

	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	t.Cleanup(cluster.StopWorkers)

	updaters := startWorkersWithCapUpdater(t, ctx, cluster, 3)

	// Wait for the initial commit to be acknowledged by every worker so the
	// fleet is past bootstrap. After this point, every worker reports the
	// full cap chain (CapAckV1 + CapTwoPhaseHandoff + CapProcessingGate)
	// because the first successful apply has run reportConsumerCapabilities.
	requireInvariantEventually(t, ctx, cluster, js, cfg, seed, 20*time.Second)

	// Pick a victim worker that is NOT the current leader, so the leader
	// (which drives the audit loop) cannot be the one whose apply fails.
	leader := cluster.WaitForLeader(10 * time.Second)
	require.NotNil(t, leader)
	victimIdx := -1
	for i, mgr := range cluster.GetActiveWorkers() {
		if mgr.WorkerID() != leader.WorkerID() {
			victimIdx = i
			break
		}
	}
	require.GreaterOrEqual(t, victimIdx, 0, "expected at least one non-leader worker")
	t.Logf("victim worker idx=%d id=%s leader id=%s",
		victimIdx, cluster.Workers[victimIdx].WorkerID(), leader.WorkerID())

	// Capture the commit version at the moment we arm the failure, so we
	// can detect the audit-driven escalation as a version bump.
	assignKV, err := js.KeyValue(ctx, cfg.KVBuckets.AssignmentBucket)
	require.NoError(t, err)
	preCommit, err := fetchAssignmentCommit(ctx, assignKV)
	require.NoError(t, err)
	require.NotNil(t, preCommit)
	preVersion := preCommit.Version

	// Arm persistent failure on the victim BEFORE issuing the partition
	// change. Every apply attempt against the victim's updater will fail
	// until we clear failFn.
	var failureCalls atomic.Int64
	updaters[victimIdx].setFailFn(func(_ int) error {
		failureCalls.Add(1)

		return errors.New("e2e: injected handoff failure")
	})

	addedSet := makeE2EPartitions(11)
	require.NoError(t, src.Modify(ctx, func(_ []types.Partition) []types.Partition {
		return append([]types.Partition{}, addedSet...)
	}))

	// Wait for the audit to escalate. The grace window before escalation
	// is ApplyGracePeriod + ExtendedApplyGracePeriod ≈ 5×HeartbeatTTL =
	// 5 × 5s = 25s with IntegrationTestConfig. The audit-driven rebalance
	// then bumps the commit version. Allow generous slack.
	require.Eventually(t, func() bool {
		c, gerr := fetchAssignmentCommit(ctx, assignKV)
		if gerr != nil {
			return false
		}

		return c.Version > preVersion+1 // tolerate the partition-add commit + 1 audit-repair commit
	}, 90*time.Second, 500*time.Millisecond,
		"expected commit version to advance past audit_repair escalation (pre=%d)", preVersion)

	require.Positive(t, failureCalls.Load(),
		"audit escalation observed but failure injector recorded 0 calls — wiring is wrong")

	// Heal the updater so the victim can apply its (possibly empty) new
	// assignment and the invariant converges.
	updaters[victimIdx].setFailFn(nil)

	requireInvariantEventually(t, ctx, cluster, js, cfg, addedSet, 60*time.Second)
}
