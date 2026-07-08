package assignment_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
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
// Hand-written commit/payload helpers (spec §3.5 wire format)
//
// These reproduce the publisher's content-addressed layout from OUTSIDE the
// leader so a test can stage a commit that a real library version would never
// emit (a pre-label payload with worker_labels_known absent) or that a stale
// incarnation left behind. The payload is JSON-marshalled in canonical form,
// gzip-compressed, and Created at "assignment._payload.<hex(sha256)>"; the
// commit references it by hash + set-digest exactly as
// assignment.FetchAndVerifyCommitPayload verifies on the read side.
// ---------------------------------------------------------------------------

// writeCommitPayload stages one content-addressed AssignmentPayload and returns
// the ref a commit embeds. known=false + labels=nil reproduces a pre-label
// leader's payload (worker_labels_known omitted by omitempty); known=true +
// labels reproduces a label-aware leader's labels-of-record.
func writeCommitPayload(t *testing.T, ctx context.Context, kv jetstream.KeyValue, parts []types.Partition, labels []string, known bool) types.AssignmentPayloadRef {
	t.Helper()

	// Canonicalize partitions by CanonicalID, mirroring the publisher, so the
	// hashed bytes match what a real leader would have produced for this set.
	canonical := slices.Clone(parts)
	slices.SortFunc(canonical, func(a, b types.Partition) int {
		return strings.Compare(a.CanonicalID(), b.CanonicalID())
	})

	payload := types.AssignmentPayload{
		SchemaVersion:     types.AssignmentSchemaVersion,
		Partitions:        canonical,
		WorkerLabels:      labels,
		WorkerLabelsKnown: known,
	}
	canonicalBytes, err := json.Marshal(payload)
	require.NoError(t, err)

	hash := sha256.Sum256(canonicalBytes)
	hashHex := hex.EncodeToString(hash[:])
	key := "assignment._payload." + hashHex

	var gzBuf bytes.Buffer
	gzw, err := gzip.NewWriterLevel(&gzBuf, gzip.BestCompression)
	require.NoError(t, err)
	_, err = gzw.Write(canonicalBytes)
	require.NoError(t, err)
	require.NoError(t, gzw.Close())

	// Create is idempotent by content address: an identical payload staged by a
	// prior write (or the real leader) is a benign ErrKeyExists.
	_, err = kv.Create(ctx, key, gzBuf.Bytes())
	if err != nil && !errors.Is(err, jetstream.ErrKeyExists) {
		require.NoError(t, err)
	}

	return types.AssignmentPayloadRef{
		Key:         key,
		PayloadHash: hashHex,
		SetDigest:   types.PartitionSetDigest(canonical),
	}
}

// putCommit writes a single-worker AssignmentCommit to the singleton commit key
// in canonical JSON, exactly as the publisher's CAS write would land it, so a
// running worker's commit watcher observes and applies it.
func putCommit(t *testing.T, ctx context.Context, kv jetstream.KeyValue, version int64, leaderRev uint64, workerID string, ref types.AssignmentPayloadRef, parts []types.Partition) {
	t.Helper()
	commit := types.AssignmentCommit{
		Version:        version,
		LeaderRevision: leaderRev,
		PublishedAt:    time.Now(),
		Workers:        []string{workerID},
		Payloads:       map[string]types.AssignmentPayloadRef{workerID: ref},
		BatchDigest:    types.PartitionSetDigest(parts),
	}
	_, err := kvutil.PutJSON(ctx, kv, assignmentCommitKey, commit)
	require.NoError(t, err)
}

// waitCommitVersionStable blocks until the commit version stops advancing for a
// quiet window, returning the settled commit. Used before staging a hand-written
// commit so a still-churning cold-start publisher cannot immediately supersede
// the staged version.
func waitCommitVersionStable(t *testing.T, ctx context.Context, kv jetstream.KeyValue, quiet time.Duration) *types.AssignmentCommit {
	t.Helper()
	var last int64 = -1
	stableSince := time.Now()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		commit := readLatestCommit(ctx, kv)
		if commit == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if commit.Version != last {
			last = commit.Version
			stableSince = time.Now()
		} else if time.Since(stableSince) >= quiet {
			return commit
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.FailNow(t, "commit version never stabilized")

	return nil
}

// ---------------------------------------------------------------------------
// Step 1: stable-ID reuse across label classes — guard + convergence
// ---------------------------------------------------------------------------

// TestStableIDReuse_DifferentLabels_NoStaleExposureAndConverge proves the incarnation
// half of spec §9 end to end with full managers: when a stable worker ID is
// reused by a NEW process whose labels differ from the labels-of-record stamped
// into the previous incarnation's still-live commit, the new worker never
// adopts the stale assignment and converges to a fresh, label-correct one.
//
// A (labels=[vip]) claims worker-0, becomes leader, and — after a vip partition
// is promoted in — publishes commit V1 whose worker-0 payload records
// labels-of-record ["vip"]. A stops gracefully (releasing the stable ID), and
// B (UNLABELED) starts and reclaims worker-0 (the lowest free ID). B's own
// leader term republishes a label-correct commit (worker-0 labels-of-record
// []), so B never applies the stale V1 (its assignment version jumps from 0
// straight past V1) and B's heartbeat never acks V1. Convergence lands the vip
// partition — which has no eligible worker under B's unlabeled fleet — parked
// then spilled onto B after the grace window.
//
// The stale-incarnation REJECT path itself (guard fires, terminal drop) is not
// observable through this single-worker full-manager flow, because a lone
// worker wins leadership synchronously during Start and republishes a
// label-correct commit before it ever reads the stale one. That reject is
// pinned at the component level (TestLabelGuard_Matrix,
// TestLabelGuard_RejectIsTerminalNoRetryNoAck); this test pins the reuse +
// convergence half the component tests cannot exercise.
func TestStableIDReuse_DifferentLabels_NoStaleExposureAndConverge(t *testing.T) { //nolint:cyclop // linear reuse→converge scenario kept in one function
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

	srcKV, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "label-incarn-partitions"})
	require.NoError(t, err)
	// Seed all-plain; the vip label is promoted out-of-band after A is stable
	// (the proven pattern — a labeled partition seeded at T=0 races the vip
	// worker's first heartbeat through the cold-start assignment).
	plain := []types.Partition{
		{Keys: []string{"ir-v"}, Weight: 1},
		{Keys: []string{"ir-p"}, Weight: 1},
	}
	src := source.NewNatsKV(srcKV, "partitions", nil)
	require.NoError(t, src.Update(ctx, plain))
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(context.Background()) })

	cfg := testutil.IntegrationTestConfig() // dedicated policy (default)
	cfg.LabelSpillGrace = 5 * time.Second

	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	t.Cleanup(cluster.StopWorkers)

	// --- Incarnation A: labeled "vip", claims worker-0, publishes V1. ---
	a := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("vip"))
	require.NoError(t, a.Start(ctx))
	require.NoError(t, <-a.WaitState(parti.StateStable, 15*time.Second))

	assignKV := openAssignmentKV(t, ctx, js, cfg)
	hbKV := openHeartbeatKV(t, ctx, js, cfg)
	worker0 := a.WorkerID()

	vID := types.Partition{Keys: []string{"ir-v"}}.CanonicalID()
	pID := types.Partition{Keys: []string{"ir-p"}}.CanonicalID()

	// Promote ir-v to "vip" out-of-band so A (the vip worker) homes it — this
	// makes V1's worker-0 payload labels-of-record concretely ["vip"].
	promoted := []types.Partition{
		{Keys: []string{"ir-v"}, Weight: 1, Label: "vip"},
		{Keys: []string{"ir-p"}, Weight: 1},
	}
	encoded, err := partcodec.Encode(promoted)
	require.NoError(t, err)
	_, err = srcKV.Put(ctx, "partitions", encoded)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return partitionsOf(a)[vID]
	}, 30*time.Second, 200*time.Millisecond, "A (vip) must home the promoted vip partition")

	// Capture the stale commit V1: worker-0's labels-of-record are ["vip"].
	v1Commit := readLatestCommit(ctx, assignKV)
	require.NotNil(t, v1Commit)
	v1Ref, ok := v1Commit.Payloads[worker0]
	require.True(t, ok, "V1 must carry a payload for worker-0")
	v1Payload, err := assignment.FetchAndVerifyCommitPayload(ctx, assignKV, v1Ref)
	require.NoError(t, err)
	require.Equal(t, []string{"vip"}, v1Payload.WorkerLabels, "V1 stamps worker-0 labels-of-record ['vip']")
	require.True(t, v1Payload.WorkerLabelsKnown)
	v1 := v1Commit.Version
	require.Positive(t, v1)

	// --- Stop A (graceful: releases the stable ID; the commit V1 persists). ---
	stopWorker(t, a)

	// --- Incarnation B: UNLABELED, reclaims worker-0 (lowest free ID). ---
	b := cluster.AddWorkerWithOptions(ctx) // no labels
	require.NoError(t, b.Start(ctx))
	require.NoError(t, <-b.WaitState(parti.StateStable, 15*time.Second),
		"B must reach StateStable (startup converts the stale-incarnation reject to success-without-apply and CASes to Stable)")
	require.Equal(t, worker0, b.WorkerID(), "B must reuse A's stable ID worker-0")

	// Strong post-reject exposure contract: the startup runner clears the raw
	// waitForAssignment store when the guard rejects the stale commit, BEFORE it
	// CASes to Stable. So from the instant B is Stable, CurrentAssignment() is
	// either still empty (version 0, no partitions) or already B's own
	// label-correct commit (version > V1) — it must NEVER surface the rejected
	// stale-incarnation V1 through the public API.
	curAtStable := b.CurrentAssignment()
	require.NotEqual(t, v1, curAtStable.Version,
		"CurrentAssignment must never expose the rejected stale-incarnation V1")
	if curAtStable.Version == 0 {
		require.Empty(t, curAtStable.Partitions,
			"pre-convergence CurrentAssignment must be empty, not the stale payload")
	} else {
		require.Greater(t, curAtStable.Version, v1,
			"a non-empty CurrentAssignment at Stable must already be B's own label-correct commit")
	}

	// Convergence: B publishes a NEW commit (version > V1) whose worker-0
	// labels-of-record are [] (unlabeled), and owns both partitions once the vip
	// partition — with no eligible vip worker — spills after grace.
	//
	// Throughout the wait, B must never expose V1 via CurrentAssignment (the
	// exposure contract above, held continuously) and must never ACK V1 via its
	// heartbeat (the guard contract: a rejected payload is never applied, so no
	// receipt for it ever reaches the leader).
	ackedV1 := false
	exposedV1 := false
	require.Eventually(t, func() bool {
		if hb, ok := readWorkerHeartbeat(ctx, hbKV, worker0); ok && hb.AppliedVersion == v1 {
			ackedV1 = true
		}
		if b.CurrentAssignment().Version == v1 {
			exposedV1 = true
		}
		commit := readLatestCommit(ctx, assignKV)
		if commit == nil || commit.Version <= v1 {
			return false
		}
		ref, ok := commit.Payloads[worker0]
		if !ok {
			return false
		}
		pl, err := assignment.FetchAndVerifyCommitPayload(ctx, assignKV, ref)
		if err != nil {
			return false
		}
		if !pl.WorkerLabelsKnown || len(pl.WorkerLabels) != 0 {
			return false // worker-0's labels-of-record must be the unlabeled [] of incarnation B
		}
		owned := partitionsOf(b)

		return b.CurrentAssignment().Version > v1 && owned[vID] && owned[pID]
	}, 60*time.Second, 200*time.Millisecond,
		"B must converge to a label-correct commit (> V1, labels-of-record []) and own both partitions after the vip spill")

	require.False(t, ackedV1, "B never acks the stale V1")
	require.False(t, exposedV1, "the stale V1 was never exposed through CurrentAssignment")

	// B's heartbeat advanced straight past V1 to its own label-correct commit.
	hb, ok := readWorkerHeartbeat(ctx, hbKV, worker0)
	require.True(t, ok)
	require.NotEqual(t, v1, hb.AppliedVersion, "B's heartbeat must never ack the stale V1")
	require.Greater(t, hb.AppliedVersion, v1, "B's applied version advanced past V1")
}

// ---------------------------------------------------------------------------
// Step 2: pre-label commit compat (mixed-version — worker-side half)
// ---------------------------------------------------------------------------

// TestPreLabelCommit_CompatApplies proves the spec §9 compat branch end to end:
// a LABELED worker APPLIES a commit whose payload has NO presence bit
// (worker_labels_known=false — exactly what a pre-label leader publishes),
// rather than rejecting it. Under the guard matrix, a labels-KNOWN payload with
// empty labels-of-record would be REJECTED for a labeled worker (empty !=
// ["vip"]); the pre-label payload (labels-UNKNOWN) must instead apply, so a new
// worker can honor commits from an old leader mid rollout.
//
// Two library versions cannot run in one Go test binary, so rather than run an
// old leader we hand-write the pre-label commit directly into the assignment
// bucket in front of a running worker. The worker is stabilized first (a lone
// worker owns everything via the fallback ladder under the dedicated policy),
// its calculator quiesced, then a pre-label commit that assigns it a SUBSET is
// staged; its commit watcher observes and applies it via the compat branch.
func TestPreLabelCommit_CompatApplies(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()

	// All-plain source so the labeled worker's calculator quiesces after cold
	// start (no label pools to re-check), leaving the staged pre-label commit
	// unchallenged by a republish.
	plain := []types.Partition{
		{Keys: []string{"cp-0"}, Weight: 1},
		{Keys: []string{"cp-1"}, Weight: 1},
		{Keys: []string{"cp-2"}, Weight: 1},
	}
	src := newKVSource(t, ctx, js, "label-compat-partitions", plain)

	cfg := testutil.IntegrationTestConfig() // dedicated

	rec := newRecordingLabelMetrics()
	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	worker := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("vip"), parti.WithMetrics(rec))
	t.Cleanup(cluster.StopWorkers)
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(20 * time.Second)

	assignKV := openAssignmentKV(t, ctx, js, cfg)
	hbKV := openHeartbeatKV(t, ctx, js, cfg)
	workerID := worker.WorkerID()

	// The lone labeled worker owns all three partitions via the fallback ladder
	// (empty general pool under dedicated). Wait for that, then let the
	// calculator quiesce so its version stops advancing.
	require.Eventually(t, func() bool {
		return len(partitionsOf(worker)) == len(plain)
	}, 30*time.Second, 200*time.Millisecond, "labeled worker must own every partition via fallback before staging")

	settled := waitCommitVersionStable(t, ctx, assignKV, 2*time.Second)
	baseVersion := settled.Version

	// Stage a PRE-LABEL commit (worker_labels_known=false) that assigns the
	// worker a strict SUBSET, at the next version, reusing the settled leader
	// revision so the worker-side stale-leader fence accepts it.
	subset := []types.Partition{
		{Keys: []string{"cp-0"}, Weight: 1},
		{Keys: []string{"cp-1"}, Weight: 1},
	}
	preRef := writeCommitPayload(t, ctx, assignKV, subset, nil, false)
	putCommit(t, ctx, assignKV, baseVersion+1, settled.LeaderRevision, workerID, preRef, subset)

	cp0 := types.Partition{Keys: []string{"cp-0"}}.CanonicalID()
	cp1 := types.Partition{Keys: []string{"cp-1"}}.CanonicalID()
	cp2 := types.Partition{Keys: []string{"cp-2"}}.CanonicalID()

	// The worker must APPLY the pre-label commit via the compat branch: its
	// assignment advances to the staged version and its ownership becomes the
	// pre-label subset (cp-0, cp-1 — not cp-2).
	require.Eventually(t, func() bool {
		cur := worker.CurrentAssignment()
		owned := partitionsOf(worker)

		return cur.Version == baseVersion+1 && len(owned) == 2 && owned[cp0] && owned[cp1] && !owned[cp2]
	}, 30*time.Second, 200*time.Millisecond,
		"labeled worker must APPLY the pre-label (labels-unknown) commit, adopting the staged subset")

	// It applied — it did NOT route through the incarnation-reject guard.
	require.Zero(t, rec.rejects(),
		"a pre-label (labels-unknown) commit must apply via the compat branch, not the incarnation-reject guard")

	// The heartbeat acks the staged version, proving the apply committed a
	// receipt (not a silent drop).
	require.Eventually(t, func() bool {
		hb, ok := readWorkerHeartbeat(ctx, hbKV, workerID)

		return ok && hb.AppliedVersion == baseVersion+1
	}, 15*time.Second, 100*time.Millisecond, "worker must ack the applied pre-label commit version")
}
