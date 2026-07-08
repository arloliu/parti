package assignment_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/assignment"
	"github.com/arloliu/parti/v2/internal/partcodec"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Orphan / stale-assignment helpers (spec I2/I8)
//
// These tests read the ASSIGNMENT KV bucket directly, not just the in-memory
// CurrentAssignment(), so they can prove the coverage contract at the wire
// level: assigned ∪ parked == source, with no source partition silently
// dropped and no stale worker payload left dangling.
// ---------------------------------------------------------------------------

// assignmentCommitKey is the singleton commit key the publisher CAS-writes:
// "<prefix>._commit" with the fixed leader-side prefix "assignment"
// (manager_assignment.go: AssignmentPrefix). The commit maps each worker to a
// content-addressable payload ref and carries the parked-set metadata.
const assignmentCommitKey = "assignment._commit"

// openAssignmentKV opens the leader-written assignment bucket. Only valid after
// StartWorkers — the manager creates the bucket on Start.
func openAssignmentKV(t *testing.T, ctx context.Context, js jetstream.JetStream, cfg parti.Config) jetstream.KeyValue {
	t.Helper()
	kv, err := js.KeyValue(ctx, cfg.KVBuckets.AssignmentBucket)
	require.NoError(t, err, "open assignment bucket %q", cfg.KVBuckets.AssignmentBucket)

	return kv
}

// openHeartbeatKV opens the heartbeat bucket. Only valid after StartWorkers.
func openHeartbeatKV(t *testing.T, ctx context.Context, js jetstream.JetStream, cfg parti.Config) jetstream.KeyValue {
	t.Helper()
	kv, err := js.KeyValue(ctx, cfg.KVBuckets.HeartbeatBucket)
	require.NoError(t, err, "open heartbeat bucket %q", cfg.KVBuckets.HeartbeatBucket)

	return kv
}

// readLatestCommit fetches the current AssignmentCommit, returning nil when the
// commit has not been written yet (cold start) or on any transient read error.
// Callers treat nil as "not converged" inside require.Eventually predicates.
func readLatestCommit(ctx context.Context, kv jetstream.KeyValue) *types.AssignmentCommit {
	commit, _, err := kvutil.GetJSON[types.AssignmentCommit](ctx, kv, assignmentCommitKey)
	if err != nil {
		return nil
	}

	return commit
}

// fetchAssignedIDs returns the union of every worker payload's partition
// CanonicalID set referenced by commit. ok=false signals a transient fetch
// failure (payload not yet visible); callers treat that as "not converged".
func fetchAssignedIDs(ctx context.Context, kv jetstream.KeyValue, commit *types.AssignmentCommit) (map[string]bool, bool) {
	out := make(map[string]bool)
	for _, wid := range commit.Workers {
		ref, ok := commit.Payloads[wid]
		if !ok {
			return nil, false
		}
		payload, err := assignment.FetchAndVerifyCommitPayload(ctx, kv, ref)
		if err != nil {
			return nil, false
		}
		for _, p := range payload.Partitions {
			out[p.CanonicalID()] = true
		}
	}

	return out, true
}

// canonicalIDSet returns the CanonicalID set of a partition slice.
func canonicalIDSet(parts []types.Partition) map[string]bool {
	out := make(map[string]bool, len(parts))
	for _, p := range parts {
		out[p.CanonicalID()] = true
	}

	return out
}

// coverageResult carries the outcome of a coverage check plus the assigned set
// so callers can make finer per-phase assertions.
type coverageResult struct {
	ok       bool // assigned ∪ parked == source AND parked metadata matches source\assigned
	assigned map[string]bool
	reason   string
}

// checkCoverage validates the spec I2/I8 coverage contract against a live
// commit for a KNOWN-CONSTANT source set: every source partition is either
// assigned (present in some payload) or parked, with the commit's
// ParkedCount / ParkedDigest exactly describing the source∖assigned remainder.
//
// This catches a silently-orphaned partition: if a source partition is neither
// assigned nor recorded as parked, the source∖assigned remainder grows past
// ParkedCount / ParkedDigest and the check fails loudly.
func checkCoverage(ctx context.Context, kv jetstream.KeyValue, commit *types.AssignmentCommit, source []types.Partition) coverageResult {
	if commit == nil {
		return coverageResult{reason: "no commit yet"}
	}
	assigned, ok := fetchAssignedIDs(ctx, kv, commit)
	if !ok {
		return coverageResult{reason: "payload fetch pending"}
	}

	srcIDs := canonicalIDSet(source)
	// Every assigned partition must belong to the source set.
	for id := range assigned {
		if !srcIDs[id] {
			return coverageResult{assigned: assigned, reason: "assigned partition not in source: " + id}
		}
	}

	// parked = source ∖ assigned (by construction disjoint from assigned).
	var parkedParts []types.Partition
	for _, p := range source {
		if !assigned[p.CanonicalID()] {
			parkedParts = append(parkedParts, p)
		}
	}

	if commit.ParkedCount != len(parkedParts) {
		return coverageResult{assigned: assigned, reason: "ParkedCount mismatch vs source∖assigned"}
	}
	if commit.ParkedDigest != types.PartitionSetDigest(parkedParts) {
		return coverageResult{assigned: assigned, reason: "ParkedDigest mismatch vs source∖assigned"}
	}

	return coverageResult{ok: true, assigned: assigned}
}

// readWorkerHeartbeat decodes a worker's current heartbeat. ok=false when the
// key is absent or unparseable (treated as "not yet published").
func readWorkerHeartbeat(ctx context.Context, hbKV jetstream.KeyValue, workerID string) (types.Heartbeat, bool) {
	entry, err := hbKV.Get(ctx, "heartbeat."+workerID)
	if err != nil {
		return types.Heartbeat{}, false
	}
	hb, err := types.DecodeHeartbeat(entry.Value())
	if err != nil {
		return types.Heartbeat{}, false
	}

	return hb, true
}

// recordingUpdater is a parti.WorkerConsumerUpdater that records the
// CanonicalID set applied on every UpdateWorkerConsumer call. It performs no
// real JetStream work — the tests only care about which partition sets the
// manager pushes through the ownership (consumer) layer, to prove a label-only
// edit that does not move ownership triggers no detach/attach churn.
type recordingUpdater struct {
	mu    sync.Mutex
	calls [][]string // each entry: sorted CanonicalID set for one UpdateWorkerConsumer call
}

// UpdateWorkerConsumer implements parti.WorkerConsumerUpdater.
func (r *recordingUpdater) UpdateWorkerConsumer(_ context.Context, _ string, partitions []types.Partition) error {
	ids := make([]string, 0, len(partitions))
	for _, p := range partitions {
		ids = append(ids, p.CanonicalID())
	}
	slices.Sort(ids)

	r.mu.Lock()
	r.calls = append(r.calls, ids)
	r.mu.Unlock()

	return nil
}

// snapshot returns a copy of all recorded calls.
func (r *recordingUpdater) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([][]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = slices.Clone(c)
	}

	return out
}

// setsEqual reports whether two sorted CanonicalID slices describe the same set.
func setsEqual(a, b []string) bool {
	return slices.Equal(a, b)
}

// ---------------------------------------------------------------------------
// Step 1: stale-assignment (I8) regression
// ---------------------------------------------------------------------------

// TestLabelDemotion_NoStaleAssignmentInKV proves spec I8: when a labeled
// worker's last labeled partition is demoted away, the assignment KV does NOT
// keep a stale non-empty payload for it. The merge contract publishes an
// explicit EMPTY payload (WorkerLabelsKnown=true, zero partitions) rather than
// leaving the old vip payload dangling, and the worker ACKS that empty version
// (AppliedVersion advances) — proving the empty assignment was APPLIED, not
// ignored.
//
// Same 2-worker dedicated setup as Task 13 (vip + plain). The rewrites go
// through a raw out-of-band kv.Put so the leader can only learn via the source
// watcher, exactly as an external writer would drive it.
func TestLabelDemotion_NoStaleAssignmentInKV(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 280*time.Second)
	defer cancel()

	srcKV, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "label-i8-partitions"})
	require.NoError(t, err)
	initial := []types.Partition{
		{Keys: []string{"d-p0"}, Weight: 1},
		{Keys: []string{"d-p1"}, Weight: 1},
		{Keys: []string{"d-p2"}, Weight: 1},
		{Keys: []string{"d-p3"}, Weight: 1},
	}
	src := source.NewNatsKV(srcKV, "partitions", nil)
	require.NoError(t, src.Update(ctx, initial))
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(context.Background()) })

	cfg := testutil.IntegrationTestConfig()
	cfg.LabelSpillGrace = 5 * time.Second

	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	vipWorker := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("vip"))
	plainWorker := cluster.AddWorkerWithOptions(ctx)
	t.Cleanup(cluster.StopWorkers)
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(20 * time.Second)

	assignKV := openAssignmentKV(t, ctx, js, cfg)
	hbKV := openHeartbeatKV(t, ctx, js, cfg)
	vipID := vipWorker.WorkerID()

	p0 := types.Partition{Keys: []string{"d-p0"}}.CanonicalID()

	// PROMOTE d-p0 to vip out-of-band, then wait until the vip worker owns it.
	promoted := []types.Partition{
		{Keys: []string{"d-p0"}, Weight: 1, Label: "vip"},
		{Keys: []string{"d-p1"}, Weight: 1},
		{Keys: []string{"d-p2"}, Weight: 1},
		{Keys: []string{"d-p3"}, Weight: 1},
	}
	encoded, err := partcodec.Encode(promoted)
	require.NoError(t, err)
	_, err = srcKV.Put(ctx, "partitions", encoded)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return partitionsOf(vipWorker)[p0] && !partitionsOf(plainWorker)[p0]
	}, 30*time.Second, 200*time.Millisecond, "promotion must move d-p0 to the vip worker")

	// DEMOTE d-p0 back out-of-band. Under dedicated policy the vip worker must
	// drain to empty.
	encoded, err = partcodec.Encode(initial)
	require.NoError(t, err)
	_, err = srcKV.Put(ctx, "partitions", encoded)
	require.NoError(t, err)

	// Assert against the KV bucket directly: the commit still lists the vip
	// worker, its payload exists and is EXPLICITLY empty (labels known), and the
	// worker's heartbeat has acked that commit's version — the empty assignment
	// was applied, not ignored. A stale non-empty vip payload would fail here.
	require.Eventually(t, func() bool {
		commit := readLatestCommit(ctx, assignKV)
		if commit == nil {
			return false
		}
		ref, ok := commit.Payloads[vipID]
		if !ok {
			return false // vip worker must remain in the commit with a payload
		}
		payload, err := assignment.FetchAndVerifyCommitPayload(ctx, assignKV, ref)
		if err != nil {
			return false
		}
		if len(payload.Partitions) != 0 || !payload.WorkerLabelsKnown {
			return false
		}
		// The plain worker must now own d-p0 again (no orphan).
		if !partitionsOf(plainWorker)[p0] {
			return false
		}
		hb, ok := readWorkerHeartbeat(ctx, hbKV, vipID)

		return ok && hb.AppliedVersion == commit.Version
	}, 30*time.Second, 200*time.Millisecond,
		"demotion must publish an explicit empty vip payload that the worker acks (no stale KV assignment)")

	// Hard final assertions for a precise failure message.
	commit := readLatestCommit(ctx, assignKV)
	require.NotNil(t, commit, "commit must exist after demotion")
	require.Contains(t, commit.Workers, vipID, "vip worker must stay in the commit")
	ref := commit.Payloads[vipID]
	payload, err := assignment.FetchAndVerifyCommitPayload(ctx, assignKV, ref)
	require.NoError(t, err)
	require.Empty(t, payload.Partitions, "vip worker payload must be explicitly empty, not stale")
	require.True(t, payload.WorkerLabelsKnown, "empty payload must be labels-of-record, not a pre-label default")
	require.Zero(t, commit.ParkedCount, "demotion to a live pool parks nothing")
}

// ---------------------------------------------------------------------------
// Step 2: parked-coverage (ghost label) — park → spill → re-home
// ---------------------------------------------------------------------------

// TestGhostLabel_ParksThenSpills_NeverOrphans proves the full parked-coverage
// lifecycle for a label no live worker can ever carry: the partition is PARKED
// (not silently dropped, not prematurely spilled) during grace, SPILLS to the
// fallback ladder after grace with NO external event (the label re-check timer
// drives it end to end), and RE-HOMES when a matching worker finally joins.
//
// The coverage invariant assigned ∪ parked == source holds at every converged
// observation because the source set is constant throughout.
func TestGhostLabel_ParksThenSpills_NeverOrphans(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 280*time.Second)
	defer cancel()

	srcKV, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "label-ghost-partitions"})
	require.NoError(t, err)
	// Cold start with 3 plain partitions only. The "ghost" label is introduced
	// out-of-band AFTER the cluster is stable (Task 13's proven pattern): the
	// leader's synchronous cold-start assignment does not tolerate a first-
	// observation label deferral, so a label with an empty pool is always driven
	// in through the steady-state source-watch → rebalance path.
	plain := []types.Partition{
		{Keys: []string{"g-p0"}, Weight: 1},
		{Keys: []string{"g-p1"}, Weight: 1},
		{Keys: []string{"g-p2"}, Weight: 1},
	}
	// Full source once the ghost partition is added: 3 plain + 1 ghost. This is
	// the constant source set for the coverage invariant across every phase.
	withGhost := []types.Partition{
		{Keys: []string{"g-p0"}, Weight: 1},
		{Keys: []string{"g-p1"}, Weight: 1},
		{Keys: []string{"g-p2"}, Weight: 1},
		{Keys: []string{"g-ghost"}, Weight: 1, Label: "ghost"},
	}
	src := source.NewNatsKV(srcKV, "partitions", nil)
	require.NoError(t, src.Update(ctx, plain))
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(context.Background()) })

	cfg := testutil.IntegrationTestConfig()
	cfg.LabelSpillGrace = 6 * time.Second

	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	plainWorker := cluster.AddWorkerWithOptions(ctx) // single unlabeled worker
	t.Cleanup(cluster.StopWorkers)
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(20 * time.Second)

	assignKV := openAssignmentKV(t, ctx, js, cfg)

	ghostID := types.Partition{Keys: []string{"g-ghost"}}.CanonicalID()
	plainIDs := canonicalIDSet(plain)

	// Steady state: the single unlabeled worker owns all 3 plain partitions.
	require.Eventually(t, func() bool {
		return len(partitionsOf(plainWorker)) == 3
	}, 20*time.Second, 200*time.Millisecond, "unlabeled worker must own the 3 plain partitions")

	// Introduce the ghost-labeled partition out-of-band (raw kv.Put): the leader
	// learns via its source watcher exactly as an external writer would drive it.
	encoded, err := partcodec.Encode(withGhost)
	require.NoError(t, err)
	_, err = srcKV.Put(ctx, "partitions", encoded)
	require.NoError(t, err)

	// PHASE PARK: within detection + defer-once confirmation the leader parks
	// the ghost partition. ParkedCount==1, ParkedDigest == digest({ghost}), the
	// 3 plain partitions are assigned, and the ghost is in NO payload.
	require.Eventually(t, func() bool {
		commit := readLatestCommit(ctx, assignKV)
		if commit == nil || commit.ParkedCount != 1 {
			return false
		}
		cov := checkCoverage(ctx, assignKV, commit, withGhost)
		if !cov.ok {
			return false
		}
		// assigned must be exactly the 3 plain partitions (ghost parked).
		if len(cov.assigned) != len(plainIDs) {
			return false
		}
		for id := range plainIDs {
			if !cov.assigned[id] {
				return false
			}
		}

		return !cov.assigned[ghostID]
	}, 30*time.Second, 100*time.Millisecond, "ghost label must be parked during grace, plain partitions assigned")

	parkCommit := readLatestCommit(ctx, assignKV)
	require.NotNil(t, parkCommit)
	require.Equal(t, 1, parkCommit.ParkedCount, "exactly one partition parked")
	require.Equal(t,
		types.PartitionSetDigest([]types.Partition{{Keys: []string{"g-ghost"}, Label: "ghost"}}),
		parkCommit.ParkedDigest, "parked digest must be the ghost partition")
	require.True(t, checkCoverage(ctx, assignKV, parkCommit, withGhost).ok, "coverage during park")

	// PHASE SPILL: after grace, with NO source/worker event, the re-check timer
	// spills the ghost onto the fallback ladder (the unlabeled worker).
	// ParkedCount==0 and the ghost is now in a payload.
	require.Eventually(t, func() bool {
		commit := readLatestCommit(ctx, assignKV)
		if commit == nil || commit.ParkedCount != 0 {
			return false
		}
		cov := checkCoverage(ctx, assignKV, commit, withGhost)

		return cov.ok && cov.assigned[ghostID]
	}, 30*time.Second, 200*time.Millisecond, "ghost must spill after grace via the label re-check timer (no external event)")

	spillCommit := readLatestCommit(ctx, assignKV)
	require.NotNil(t, spillCommit)
	require.Zero(t, spillCommit.ParkedCount, "nothing parked after spill")
	require.True(t, checkCoverage(ctx, assignKV, spillCommit, withGhost).ok, "coverage after spill")
	require.True(t, partitionsOf(plainWorker)[ghostID], "spilled ghost lands on the unlabeled fallback worker")

	// PHASE RECOVER: add a ghost-labeled worker. The ghost partition re-homes to
	// it and the coverage invariant still holds across both workers.
	ghostWorker := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("ghost"))
	require.NoError(t, ghostWorker.Start(ctx))
	require.NoError(t, <-ghostWorker.WaitState(parti.StateStable, 15*time.Second))

	require.Eventually(t, func() bool {
		commit := readLatestCommit(ctx, assignKV)
		if commit == nil || commit.ParkedCount != 0 {
			return false
		}
		cov := checkCoverage(ctx, assignKV, commit, withGhost)

		return cov.ok && partitionsOf(ghostWorker)[ghostID] && !partitionsOf(plainWorker)[ghostID]
	}, 30*time.Second, 200*time.Millisecond, "ghost partition must re-home to the newly-joined ghost worker")

	// Full coverage set assertion after recovery: assigned across both workers ==
	// all four source partitions, nothing parked.
	recoverCommit := readLatestCommit(ctx, assignKV)
	require.NotNil(t, recoverCommit)
	assigned, ok := fetchAssignedIDs(ctx, assignKV, recoverCommit)
	require.True(t, ok)
	require.Equal(t, canonicalIDSet(withGhost), assigned, "every source partition assigned exactly once after recovery")
	require.Zero(t, recoverCommit.ParkedCount)
}

// ---------------------------------------------------------------------------
// Step 3: pool-outage lifecycle — park → spill → re-home under worker loss
// ---------------------------------------------------------------------------

// TestLabelPoolOutage_ParkSpillRehome drives the labeled-pool outage lifecycle
// with real worker loss: partitions concentrate while the pool is non-empty
// (no parking), park when the pool empties, spill after grace, and re-home when
// a labeled worker returns. The coverage invariant assigned ∪ parked == source
// (source constant) is asserted at every converged phase so any orphan window
// fails loudly.
//
// Phase 3 doubles as the regression pin for the takeover-deferral fix: when
// the LAST labeled worker stopped is also the current leader, draining its
// pool forces a leadership takeover, and the new leader's synchronous
// "takeover_immediate" assignment observes the empty pool FIRST — a benign
// errLabelObservationDeferred. Before the fix, that sentinel escaped
// startCalculator as fatal, releasing leadership and livelocking the fleet in
// a takeover flap: each takeover rebuilt labelState, so the defer-once streak
// never confirmed the empty pool and the park never committed (spec I2
// violation — the vip partitions stayed stuck on the dead worker). The fix
// treats the deferral as success-without-publish, so the new leader keeps
// leadership and the label re-check timer drives the park to commit.
func TestLabelPoolOutage_ParkSpillRehome(t *testing.T) { //nolint:cyclop // five-phase pool-outage lifecycle kept as one function for readability
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 280*time.Second)
	defer cancel()

	srcKV, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "label-outage-partitions"})
	require.NoError(t, err)
	// Seed all-plain; the vip labels are promoted out-of-band after the cluster
	// is stable (same rationale as the ghost test — the cold-start assignment
	// does not tolerate a first-observation label deferral, and seeding labels
	// at T=0 races the vip workers' first heartbeat).
	plain := []types.Partition{
		{Keys: []string{"o-v0"}, Weight: 1},
		{Keys: []string{"o-v1"}, Weight: 1},
		{Keys: []string{"o-p0"}, Weight: 1},
		{Keys: []string{"o-p1"}, Weight: 1},
	}
	// Constant source set for the coverage invariant once the vip labels exist.
	partitions := []types.Partition{
		{Keys: []string{"o-v0"}, Weight: 1, Label: "vip"},
		{Keys: []string{"o-v1"}, Weight: 1, Label: "vip"},
		{Keys: []string{"o-p0"}, Weight: 1},
		{Keys: []string{"o-p1"}, Weight: 1},
	}
	src := source.NewNatsKV(srcKV, "partitions", nil)
	require.NoError(t, src.Update(ctx, plain))
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(context.Background()) })

	cfg := testutil.IntegrationTestConfig()
	cfg.LabelSpillGrace = 8 * time.Second

	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	vip1 := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("vip"))
	vip2 := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("vip"))
	// TWO plain workers so the fleet never shrinks to a single total worker when
	// both vip workers are stopped: a lone worker under the aggressive
	// integration TTLs oscillates leadership and cannot run its calculator to
	// completion (an election-layer artifact, orthogonal to the parking
	// machinery under test). Two plain workers keep a stable multi-worker fleet
	// with an empty vip pool, which is exactly what phase 3 needs.
	plain1 := cluster.AddWorkerWithOptions(ctx)
	plain2 := cluster.AddWorkerWithOptions(ctx)
	t.Cleanup(cluster.StopWorkers)
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(20 * time.Second)

	assignKV := openAssignmentKV(t, ctx, js, cfg)

	v0 := types.Partition{Keys: []string{"o-v0"}}.CanonicalID()
	v1 := types.Partition{Keys: []string{"o-v1"}}.CanonicalID()

	// plainHasVip reports whether either plain worker owns a vip partition.
	plainHasVip := func() bool {
		for _, w := range []*parti.Manager{plain1, plain2} {
			p := partitionsOf(w)
			if p[v0] || p[v1] {
				return true
			}
		}

		return false
	}

	// Promote o-v0/o-v1 to "vip" out-of-band so the vip pool (both vip workers,
	// now stable and heartbeating their labels) is non-empty when the labels
	// first appear — no cold-start deferral.
	encoded, err := partcodec.Encode(partitions)
	require.NoError(t, err)
	_, err = srcKV.Put(ctx, "partitions", encoded)
	require.NoError(t, err)

	// PHASE 1 — stable: vip partitions live only on the vip workers, never the
	// plain worker, and nothing is parked.
	require.Eventually(t, func() bool {
		commit := readLatestCommit(ctx, assignKV)
		if commit == nil || commit.ParkedCount != 0 {
			return false
		}
		cov := checkCoverage(ctx, assignKV, commit, partitions)
		vipOnVipPool := (partitionsOf(vip1)[v0] || partitionsOf(vip2)[v0]) &&
			(partitionsOf(vip1)[v1] || partitionsOf(vip2)[v1])

		return cov.ok && vipOnVipPool && !plainHasVip()
	}, 30*time.Second, 200*time.Millisecond, "stable: vip partitions on vip workers, none parked")

	// PHASE 2 — stop ONE vip worker: its partitions concentrate onto the
	// surviving vip worker. The pool is still non-empty, so NOTHING parks.
	stopWorker(t, vip2)
	require.Eventually(t, func() bool {
		commit := readLatestCommit(ctx, assignKV)
		if commit == nil || commit.ParkedCount != 0 {
			return false
		}
		cov := checkCoverage(ctx, assignKV, commit, partitions)

		return cov.ok && partitionsOf(vip1)[v0] && partitionsOf(vip1)[v1] && !plainHasVip()
	}, 30*time.Second, 200*time.Millisecond,
		"one vip down: partitions concentrate on the survivor, no parking")

	// PHASE 3 — stop the SECOND vip worker: pool now empty. Within detection +
	// defer-once confirmation, BOTH vip partitions park (ParkedCount==2) and the
	// plain workers do NOT receive them during grace.
	stopWorker(t, vip1)
	require.Eventually(t, func() bool {
		commit := readLatestCommit(ctx, assignKV)
		if commit == nil || commit.ParkedCount != 2 {
			return false
		}
		cov := checkCoverage(ctx, assignKV, commit, partitions)

		return cov.ok && !plainHasVip()
	}, 30*time.Second, 100*time.Millisecond,
		"empty vip pool must park both vip partitions during grace; plain workers get none")

	parkCommit := readLatestCommit(ctx, assignKV)
	require.NotNil(t, parkCommit)
	require.Equal(t, 2, parkCommit.ParkedCount, "both vip partitions parked")
	require.Equal(t,
		types.PartitionSetDigest([]types.Partition{
			{Keys: []string{"o-v0"}, Label: "vip"}, {Keys: []string{"o-v1"}, Label: "vip"},
		}),
		parkCommit.ParkedDigest, "parked digest must be the two vip partitions")

	// PHASE 4 — after grace: spill. The vip partitions appear in the plain
	// pool's payloads (assigned to some plain worker) and ParkedCount returns
	// to 0.
	require.Eventually(t, func() bool {
		commit := readLatestCommit(ctx, assignKV)
		if commit == nil || commit.ParkedCount != 0 {
			return false
		}
		cov := checkCoverage(ctx, assignKV, commit, partitions)

		return cov.ok && cov.assigned[v0] && cov.assigned[v1] && plainHasVip()
	}, 30*time.Second, 200*time.Millisecond, "after grace the vip partitions spill to the plain pool")

	spillCommit := readLatestCommit(ctx, assignKV)
	require.NotNil(t, spillCommit)
	assignedSpill, ok := fetchAssignedIDs(ctx, assignKV, spillCommit)
	require.True(t, ok)
	require.Equal(t, canonicalIDSet(partitions), assignedSpill, "full coverage after spill: all four partitions assigned")

	// PHASE 5 — restart a vip worker: the vip partitions re-home to it and the
	// plain worker sheds them; nothing parks.
	vip3 := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("vip"))
	require.NoError(t, vip3.Start(ctx))
	require.NoError(t, <-vip3.WaitState(parti.StateStable, 15*time.Second))

	require.Eventually(t, func() bool {
		commit := readLatestCommit(ctx, assignKV)
		if commit == nil || commit.ParkedCount != 0 {
			return false
		}
		cov := checkCoverage(ctx, assignKV, commit, partitions)

		return cov.ok && partitionsOf(vip3)[v0] && partitionsOf(vip3)[v1] && !plainHasVip()
	}, 30*time.Second, 200*time.Millisecond, "vip partitions re-home to the restarted vip worker; plain worker sheds them")

	// Full coverage set assertion after re-home.
	rehomeCommit := readLatestCommit(ctx, assignKV)
	require.NotNil(t, rehomeCommit)
	assignedRehome, ok := fetchAssignedIDs(ctx, assignKV, rehomeCommit)
	require.True(t, ok)
	require.Equal(t, canonicalIDSet(partitions), assignedRehome, "full coverage after re-home")
}

// stopWorker stops a single manager with a bounded timeout. The cluster's
// StopWorkers cleanup skips already-shutdown workers, so stopping here is safe.
func stopWorker(t *testing.T, mgr *parti.Manager) {
	t.Helper()
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, mgr.Stop(stopCtx), "stop worker %s", mgr.WorkerID())
}

// ---------------------------------------------------------------------------
// Step 3b: no ownership movement on a label-only edit (spec §4.1 pin)
// ---------------------------------------------------------------------------

// TestLabelOnlyEdit_NoConsumerChurnWhenPlacementUnchanged proves that a
// label-only source edit that CANNOT change placement (the only vip-capable
// worker already owns the partition) still advances the assignment version
// (the payload hash changes because labels-of-record are part of it) but
// triggers NO detach/attach at the ownership layer — every UpdateWorkerConsumer
// call carries the same partition CanonicalID set.
//
// Single worker labeled "vip" under the SHARED policy, so it also serves
// unlabeled work and therefore owns every partition regardless of labels.
func TestLabelOnlyEdit_NoConsumerChurnWhenPlacementUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 280*time.Second)
	defer cancel()

	srcKV, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "label-nochurn-partitions"})
	require.NoError(t, err)
	initial := []types.Partition{
		{Keys: []string{"c-p0"}, Weight: 1},
		{Keys: []string{"c-p1"}, Weight: 1},
		{Keys: []string{"c-p2"}, Weight: 1},
		{Keys: []string{"c-p3"}, Weight: 1},
	}
	src := source.NewNatsKV(srcKV, "partitions", nil)
	require.NoError(t, src.Update(ctx, initial))
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(context.Background()) })

	cfg := testutil.IntegrationTestConfig()
	cfg.UnlabeledPartitionPolicy = "shared"
	cfg.LabelSpillGrace = 5 * time.Second

	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	rec := &recordingUpdater{}
	vipWorker := cluster.AddWorkerWithOptions(ctx,
		parti.WithWorkerLabels("vip"), parti.WithWorkerConsumerUpdater(rec))
	t.Cleanup(cluster.StopWorkers)
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(20 * time.Second)

	fullSet := canonicalIDSet(initial)
	p0 := types.Partition{Keys: []string{"c-p0"}}.CanonicalID()

	// Steady state: the single shared worker owns ALL partitions, including the
	// (unlabeled) c-p0.
	require.Eventually(t, func() bool {
		owned := partitionsOf(vipWorker)
		if len(owned) != len(fullSet) {
			return false
		}
		for id := range fullSet {
			if !owned[id] {
				return false
			}
		}

		return owned[p0]
	}, 30*time.Second, 200*time.Millisecond, "shared single worker must own every partition")

	// Baseline: the ownership set the consumer layer has settled on, and the
	// current applied version. Wait for at least one settled consumer update
	// carrying the full set so the baseline is meaningful.
	sortedFull := make([]string, 0, len(fullSet))
	for id := range fullSet {
		sortedFull = append(sortedFull, id)
	}
	slices.Sort(sortedFull)

	require.Eventually(t, func() bool {
		calls := rec.snapshot()
		if len(calls) == 0 {
			return false
		}

		return setsEqual(calls[len(calls)-1], sortedFull)
	}, 30*time.Second, 200*time.Millisecond, "consumer layer must settle on the full ownership set")

	baselineCalls := len(rec.snapshot())
	v0 := vipWorker.CurrentAssignment().Version
	require.Positive(t, v0, "worker must have a committed version at steady state")

	// PROMOTE c-p0 to vip out-of-band (label-only edit). Placement cannot change:
	// the sole vip-capable worker already owns it.
	promoted := []types.Partition{
		{Keys: []string{"c-p0"}, Weight: 1, Label: "vip"},
		{Keys: []string{"c-p1"}, Weight: 1},
		{Keys: []string{"c-p2"}, Weight: 1},
		{Keys: []string{"c-p3"}, Weight: 1},
	}
	encoded, err := partcodec.Encode(promoted)
	require.NoError(t, err)
	_, err = srcKV.Put(ctx, "partitions", encoded)
	require.NoError(t, err)

	// A NEW assignment version must be applied (payload hash changed because
	// labels-of-record are part of the payload), AND a new consumer update must
	// fire (the apply path runs UpdateWorkerConsumer on every version).
	require.Eventually(t, func() bool {
		return vipWorker.CurrentAssignment().Version > v0 && len(rec.snapshot()) > baselineCalls
	}, 30*time.Second, 200*time.Millisecond,
		"label-only promotion must advance the version and re-run the consumer update")

	// NO CHURN: every consumer update from the settled baseline onward — across
	// the promotion — carries the SAME CanonicalID set. No partition was
	// detached or attached at the ownership layer.
	calls := rec.snapshot()
	require.Greater(t, len(calls), baselineCalls, "a post-promotion consumer update was recorded")
	for i := baselineCalls - 1; i < len(calls); i++ {
		require.Truef(t, setsEqual(calls[i], sortedFull),
			"consumer update #%d changed the ownership set: got %v, want %v", i, calls[i], sortedFull)
	}

	// The partition is now vip-labeled in the payload but still owned by the same
	// worker — confirm placement is genuinely unchanged.
	require.True(t, partitionsOf(vipWorker)[p0], "c-p0 stays on the same worker after promotion")
}
