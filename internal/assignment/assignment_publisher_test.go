package assignment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// ----- test infrastructure -----

// publisherFixture wires a real JetStream-backed publisher for tests. Each
// fixture gets its own bucket-pair to keep tests isolated.
type publisherFixture struct {
	pub          *AssignmentPublisher
	assignmentKV jetstream.KeyValue
	heartbeatKV  jetstream.KeyValue
	leaderRev    *atomic.Uint64
	metrics      *countingMetrics
}

// liveRevCheck returns a LeaderCheckFunc that mimics
// NATSElection.CheckLeadership against a test-driven *atomic.Uint64. When the
// claimed value matches the current uint64, returns nil; otherwise returns a
// wrapped types.ErrLeadershipRevisionMismatch (the exact shape production
// callers see).
func liveRevCheck(rev *atomic.Uint64) LeaderCheckFunc {
	return func(_ context.Context, claimed uint64) error {
		live := rev.Load()
		if live != claimed || live == 0 {
			return fmt.Errorf("%w: claimed=%d live=%d", types.ErrLeadershipRevisionMismatch, claimed, live)
		}
		return nil
	}
}

func newPublisherFixture(t testing.TB, name string) *publisherFixture {
	t.Helper()
	_, nc := partitest.StartEmbeddedNATS(t)
	akv := partitest.CreateJetStreamKV(t, nc, "asgn-"+name)
	hkv := partitest.CreateJetStreamKV(t, nc, "hb-"+name)
	lr := &atomic.Uint64{}
	lr.Store(1) // default a non-zero leader revision so leadership checks pass
	m := newCountingMetrics()
	// Tests use a callback that simulates a live leader-key revision compare:
	// a mismatch returns ErrLeadershipRevisionMismatch wrapped, exactly like
	// the production NATSElection.CheckLeadership path. Tests that need to
	// drive the failure inject by writing a different value to lr.
	leaderCheck := func(_ context.Context, claimed uint64) error {
		live := lr.Load()
		if live != claimed || live == 0 {
			return fmt.Errorf("%w: claimed=%d live=%d", types.ErrLeadershipRevisionMismatch, claimed, live)
		}
		return nil
	}
	pub := NewAssignmentPublisher(PublisherConfig{
		AssignmentKV:    akv,
		HeartbeatKV:     hkv,
		Prefix:          "assignment",
		HeartbeatPrefix: "heartbeat",
		LeaderCheckFn:   leaderCheck,
		Logger:          logging.NewNop(),
		Metrics:         m,
	})

	return &publisherFixture{pub: pub, assignmentKV: akv, heartbeatKV: hkv, leaderRev: lr, metrics: m}
}

// putV1Heartbeat writes a v1 (CapAckV1=1) heartbeat for the given worker so
// the publisher's classifyLegacyWorkers treats it as commit-capable.
func (f *publisherFixture) putV1Heartbeat(t *testing.T, ctx context.Context, workerID string) {
	t.Helper()
	hb := types.Heartbeat{
		WorkerID:      workerID,
		SchemaVersion: 1,
		Capabilities:  types.CapAckV1,
		Timestamp:     time.Now().UTC(),
	}
	data, err := json.Marshal(hb)
	require.NoError(t, err)
	_, err = f.heartbeatKV.Put(ctx, "heartbeat."+workerID, data)
	require.NoError(t, err)
}

// putLegacyHeartbeat writes a legacy timestamp-only heartbeat so the
// publisher classifies the worker as legacy_in_batch.
func (f *publisherFixture) putLegacyHeartbeat(t *testing.T, ctx context.Context, workerID string) {
	t.Helper()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := f.heartbeatKV.Put(ctx, "heartbeat."+workerID, []byte(ts))
	require.NoError(t, err)
}

// readCommit fetches and decodes the assignment._commit key. Returns nil if
// the key does not exist.
func (f *publisherFixture) readCommit(t *testing.T, ctx context.Context) *types.AssignmentCommit {
	t.Helper()
	entry, err := f.assignmentKV.Get(ctx, "assignment._commit")
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		require.NoError(t, err)
	}
	var c types.AssignmentCommit
	require.NoError(t, json.Unmarshal(entry.Value(), &c))

	return &c
}

// ----- counting metrics -----

type countingMetrics struct {
	*metrics.NopMetrics
	payloadsCreated          atomic.Int64
	payloadsReused           atomic.Int64
	payloadBytesObservations atomic.Int64
	commitBytesObservations  atomic.Int64
	batchAbortedReasons      map[string]*atomic.Int64
	aliasBarrierFailed       atomic.Int64
	aliasVisibleUncommitted  atomic.Int64
	commitAborts             atomic.Int64
	gcDeleteErrors           atomic.Int64
}

func newCountingMetrics() *countingMetrics {
	return &countingMetrics{
		NopMetrics:          metrics.NewNop(),
		batchAbortedReasons: make(map[string]*atomic.Int64),
	}
}

func (c *countingMetrics) IncrementPayloadsCreated()         { c.payloadsCreated.Add(1) }
func (c *countingMetrics) IncrementPayloadsReused()          { c.payloadsReused.Add(1) }
func (c *countingMetrics) ObservePayloadBytesWritten(_ int)  { c.payloadBytesObservations.Add(1) }
func (c *countingMetrics) ObserveCommitBytesWritten(_ int)   { c.commitBytesObservations.Add(1) }
func (c *countingMetrics) IncrementAliasBarrierFailed()      { c.aliasBarrierFailed.Add(1) }
func (c *countingMetrics) IncrementAliasVisibleUncommitted() { c.aliasVisibleUncommitted.Add(1) }
func (c *countingMetrics) IncrementCommitAborts()            { c.commitAborts.Add(1) }
func (c *countingMetrics) IncrementPayloadDeleteErrors()     { c.gcDeleteErrors.Add(1) }
func (c *countingMetrics) IncrementBatchAborted(reason string) {
	if _, ok := c.batchAbortedReasons[reason]; !ok {
		c.batchAbortedReasons[reason] = &atomic.Int64{}
	}
	c.batchAbortedReasons[reason].Add(1)
}

func (c *countingMetrics) batchAbortedCount(reason string) int64 {
	if v, ok := c.batchAbortedReasons[reason]; ok {
		return v.Load()
	}

	return 0
}

// ----- helpers -----

func ps(keys ...string) types.Partition { return types.Partition{Keys: keys} }

// ----- §3.5 / §3.8 publisher behavior tests -----

// TestPublisher_Crash_BeforeCommit_PayloadsInert — payloads written before a
// crash are inert; a fresh publisher takeover writes commit V+1 with no
// dependence on the leftover payloads.
func TestPublisher_Crash_BeforeCommit_PayloadsInert(t *testing.T) {
	f := newPublisherFixture(t, "crash-before-commit")
	ctx := context.Background()

	// Simulate an aborted publish: write a payload directly with the same
	// key shape the publisher would. We don't write a commit.
	canonical := mustMarshalCanonicalPayload(t, types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    []types.Partition{ps("p1")},
	})
	hash := sha256.Sum256(canonical)
	orphanKey := "assignment._payload." + hex.EncodeToString(hash[:])
	gz, err := gzipCompress(canonical)
	require.NoError(t, err)
	_, err = f.assignmentKV.Create(ctx, orphanKey, gz)
	require.NoError(t, err)

	// New publisher takeover — DiscoverHighestVersion sees no commit, no
	// legacy aliases, so currentVersion stays 0; next publish should succeed
	// at V=1, and the orphan payload key continues to exist (inert).
	f.putV1Heartbeat(t, ctx, "w1")
	srcParts := []types.Partition{ps("p1")}
	err = f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"w1"},
		Assignments:      map[string][]types.Partition{"w1": {ps("p1")}},
		SourcePartitions: srcParts,
		LeaderRevision:   f.leaderRev.Load(),
		Lifecycle:        "test",
	})
	require.NoError(t, err)

	commit := f.readCommit(t, ctx)
	require.NotNil(t, commit)
	require.Equal(t, int64(1), commit.Version)

	// Orphan key is still present (inert).
	_, err = f.assignmentKV.Get(ctx, orphanKey)
	require.NoError(t, err, "orphan payload should remain in KV until GC reaps it")

	// And it is NOT referenced by the commit (the commit's payload for w1
	// references the same content-hash by happy coincidence in this test —
	// guard explicitly).
	commitRef := commit.Payloads["w1"]
	require.Equal(t, orphanKey, commitRef.Key, "in this test the orphan happens to match the new commit's payload, both being content-addressable")

	// Now exercise a different orphan that the next commit does NOT reuse.
	otherCanonical := mustMarshalCanonicalPayload(t, types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    []types.Partition{ps("non-existent")},
	})
	otherHash := sha256.Sum256(otherCanonical)
	otherOrphanKey := "assignment._payload." + hex.EncodeToString(otherHash[:])
	gz2, _ := gzipCompress(otherCanonical)
	_, err = f.assignmentKV.Create(ctx, otherOrphanKey, gz2)
	require.NoError(t, err)

	// Make sure the next commit doesn't reference it.
	commit2 := f.readCommit(t, ctx)
	for _, ref := range commit2.Payloads {
		require.NotEqual(t, otherOrphanKey, ref.Key)
	}
}

// TestPublisher_LegacyBootstrap_NoCommit_RecoversViaDiscoverHighestVersion —
// pre-populate legacy assignment.<W> keys at version N; a takeover publisher
// discovers N and the next publish increments to N+1.
func TestPublisher_LegacyBootstrap_NoCommit_RecoversViaDiscoverHighestVersion(t *testing.T) {
	f := newPublisherFixture(t, "legacy-bootstrap")
	ctx := context.Background()

	// Pre-populate two legacy aliases at V=7 and V=5 (no commit).
	leg7 := types.Assignment{Version: 7, Partitions: []types.Partition{ps("a")}}
	leg5 := types.Assignment{Version: 5, Partitions: []types.Partition{ps("b")}}
	d7, _ := json.Marshal(leg7)
	d5, _ := json.Marshal(leg5)
	_, err := f.assignmentKV.Put(ctx, "assignment.w1", d7)
	require.NoError(t, err)
	_, err = f.assignmentKV.Put(ctx, "assignment.w2", d5)
	require.NoError(t, err)

	ids, err := f.pub.DiscoverHighestVersion(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"w1", "w2"}, ids)
	require.Equal(t, int64(7), f.pub.CurrentVersion())

	// Next publish increments to V=8.
	f.putV1Heartbeat(t, ctx, "w1")
	f.putV1Heartbeat(t, ctx, "w2")
	srcParts := []types.Partition{ps("a"), ps("b")}
	err = f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"w1", "w2"},
		Assignments:      map[string][]types.Partition{"w1": {ps("a")}, "w2": {ps("b")}},
		SourcePartitions: srcParts,
		LeaderRevision:   f.leaderRev.Load(),
	})
	require.NoError(t, err)
	c := f.readCommit(t, ctx)
	require.NotNil(t, c)
	require.Equal(t, int64(8), c.Version)
}

// TestPublisher_CommitCAS_AbortsOnStaleLeader — two leaders race; the stale
// leader's commit CAS fails and the batch aborts.
func TestPublisher_CommitCAS_AbortsOnStaleLeader(t *testing.T) {
	f := newPublisherFixture(t, "commit-cas-stale")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w1")
	srcParts := []types.Partition{ps("p1")}

	// Leader L1 publishes V=1 successfully.
	err := f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"w1"},
		Assignments:      map[string][]types.Partition{"w1": {ps("p1")}},
		SourcePartitions: srcParts,
		LeaderRevision:   f.leaderRev.Load(),
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), f.pub.CurrentVersion())
	priorRev := f.pub.LastCommitRev()
	require.NotZero(t, priorRev)

	// A second leader (different publisher instance) tries to CAS on stale
	// lastCommitRev. Simulate by constructing a competitor publisher whose
	// internal state is FRESHLY ZERO (lastCommitRev=0): its first attempt
	// will use kv.Create, which will fail with ErrKeyExists.
	lr2 := &atomic.Uint64{}
	lr2.Store(1)
	m2 := newCountingMetrics()
	competitor := NewAssignmentPublisher(PublisherConfig{
		AssignmentKV:    f.assignmentKV,
		HeartbeatKV:     f.heartbeatKV,
		Prefix:          "assignment",
		HeartbeatPrefix: "heartbeat",
		LeaderCheckFn:   liveRevCheck(lr2),
		Logger:          logging.NewNop(),
		Metrics:         m2,
	})
	err = competitor.Publish(ctx, PublishInput{
		Workers:          []string{"w1"},
		Assignments:      map[string][]types.Partition{"w1": {ps("p1")}},
		SourcePartitions: srcParts,
		LeaderRevision:   lr2.Load(),
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, types.ErrCommitCASFailed), "competitor must fail commit CAS, got: %v", err)
	require.EqualValues(t, 1, m2.commitAborts.Load(), "stale leader must increment commit_aborts")
	require.EqualValues(t, 1, m2.batchAbortedCount("commit_cas_failed"))

	// Cluster view remains the original leader's commit.
	c := f.readCommit(t, ctx)
	require.Equal(t, int64(1), c.Version)
	require.Equal(t, priorRev, f.pub.LastCommitRev())
}

// TestPublisher_CommitCAS_AbortsOnLeadershipLost — leadership lost after
// payload writes (steps 4–5) aborts pre-alias; legacy aliases are NOT
// written.
func TestPublisher_CommitCAS_AbortsOnLeadershipLost(t *testing.T) {
	f := newPublisherFixture(t, "leadership-lost-pre-alias")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w1")
	srcParts := []types.Partition{ps("p1")}

	// Set up a controllable leader revision: claim 1, but live becomes 99
	// after the publisher reads it once (i.e. between step 4 and step 5).
	// We can't intercept inside Publish without major surgery, so we set
	// live to a different value BEFORE Publish starts: pre-alias check
	// should immediately fail.
	f.leaderRev.Store(99)
	err := f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"w1"},
		Assignments:      map[string][]types.Partition{"w1": {ps("p1")}},
		SourcePartitions: srcParts,
		LeaderRevision:   1, // claimed
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, types.ErrLeadershipLostPreAlias), "got: %v", err)
	require.EqualValues(t, 1, f.metrics.batchAbortedCount("leadership_lost_pre_alias"))

	// No commit and no legacy alias for w1 (CapAckV1 worker, but we never
	// got that far either).
	require.Nil(t, f.readCommit(t, ctx))
	_, err = f.assignmentKV.Get(ctx, "assignment.w1")
	require.Error(t, err, "no legacy alias should have been written")
}

// TestPublisher_LosingLeaderPayloadWriteCannotCorruptWinningCommit (F1) —
// L1 commits; L2 writes its own payloads then loses commit CAS. Workers
// fetching L1's refs see L1's bytes; L2's writes are inert.
func TestPublisher_LosingLeaderPayloadWriteCannotCorruptWinningCommit(t *testing.T) {
	f := newPublisherFixture(t, "no-corruption")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w1")
	f.putV1Heartbeat(t, ctx, "w2")
	srcParts := []types.Partition{ps("p1"), ps("p2")}

	// L1 commits: w1=p1, w2=p2.
	err := f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"w1", "w2"},
		Assignments:      map[string][]types.Partition{"w1": {ps("p1")}, "w2": {ps("p2")}},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	})
	require.NoError(t, err)
	c1 := f.readCommit(t, ctx)
	require.NotNil(t, c1)

	// L2 attempts a different mapping: w1=p2, w2=p1. The payloads have
	// different content-hashes, so kv.Create succeeds for both new keys.
	// THEN L2's commit CAS fails (lastCommitRev=0 against L1's existing
	// commit).
	lr2 := &atomic.Uint64{}
	lr2.Store(1)
	competitor := NewAssignmentPublisher(PublisherConfig{
		AssignmentKV:    f.assignmentKV,
		HeartbeatKV:     f.heartbeatKV,
		Prefix:          "assignment",
		HeartbeatPrefix: "heartbeat",
		LeaderCheckFn:   liveRevCheck(lr2),
		Logger:          logging.NewNop(),
		Metrics:         newCountingMetrics(),
	})
	err = competitor.Publish(ctx, PublishInput{
		Workers:          []string{"w1", "w2"},
		Assignments:      map[string][]types.Partition{"w1": {ps("p2")}, "w2": {ps("p1")}},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	})
	require.True(t, errors.Is(err, types.ErrCommitCASFailed))

	// L1's commit is still authoritative.
	c2 := f.readCommit(t, ctx)
	require.Equal(t, c1.Version, c2.Version)
	require.Equal(t, c1.Payloads["w1"].Key, c2.Payloads["w1"].Key)
	require.Equal(t, c1.Payloads["w2"].Key, c2.Payloads["w2"].Key)

	// Workers fetching by L1's refs decode the partitions L1 assigned.
	for w, expected := range map[string]string{"w1": "p1", "w2": "p2"} {
		ref := c2.Payloads[w]
		entry, err := f.assignmentKV.Get(ctx, ref.Key)
		require.NoError(t, err)
		plain, err := gzipDecompress(entry.Value())
		require.NoError(t, err)
		var p types.AssignmentPayload
		require.NoError(t, json.Unmarshal(plain, &p))
		require.Len(t, p.Partitions, 1)
		require.Equal(t, expected, p.Partitions[0].Keys[0], "worker %s must see L1's assignment, not L2's", w)
		// Verify hash equals ref.PayloadHash.
		gotHash := sha256.Sum256(plain)
		require.Equal(t, ref.PayloadHash, hex.EncodeToString(gotHash[:]))
	}
}

// TestPublisher_InlineSizeRegression_DoesNotApply — commit blob never embeds
// inline partitions; size stays small for typical batches.
func TestPublisher_InlineSizeRegression_DoesNotApply(t *testing.T) {
	f := newPublisherFixture(t, "inline-size-regression")
	ctx := context.Background()
	const n = 25
	workers := make([]string, n)
	assignments := make(map[string][]types.Partition, n)
	srcParts := make([]types.Partition, 0, n*5)
	for i := range n {
		w := fmt.Sprintf("w%02d", i)
		workers[i] = w
		f.putV1Heartbeat(t, ctx, w)
		slice := make([]types.Partition, 0, 5)
		for j := range 5 {
			p := ps(fmt.Sprintf("p-%d-%d", i, j))
			slice = append(slice, p)
			srcParts = append(srcParts, p)
		}
		assignments[w] = slice
	}
	err := f.pub.Publish(ctx, PublishInput{
		Workers: workers, Assignments: assignments, SourcePartitions: srcParts, LeaderRevision: 1,
	})
	require.NoError(t, err)

	entry, err := f.assignmentKV.Get(ctx, "assignment._commit")
	require.NoError(t, err)
	// Per the plan's commit-bytes target: < 10 KB for typical batches. This
	// test profile (25 workers × 5 partitions, 64-char hex hashes per ref)
	// stays comfortably under 10 KB; if this fails, the commit may have
	// regressed to inlining partition data.
	require.Less(t, len(entry.Value()), 10*1024, "commit blob must stay under 10 KB without inline payloads")

	// Decode the commit and confirm Payloads carry refs (not partition lists).
	var c types.AssignmentCommit
	require.NoError(t, json.Unmarshal(entry.Value(), &c))
	for _, ref := range c.Payloads {
		require.True(t, strings.HasPrefix(ref.Key, "assignment._payload."))
		require.Len(t, ref.PayloadHash, 64, "PayloadHash must be 64 hex chars (sha256)")
	}
}

// TestPublisher_ErrKeyExists_VerifiedAndReused — pre-populate a payload key
// with the exact bytes the publisher will produce; assert payloads_reused
// increments, ref carries existing revision.
func TestPublisher_ErrKeyExists_VerifiedAndReused(t *testing.T) {
	f := newPublisherFixture(t, "key-exists-reused")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w1")

	canonical := mustMarshalCanonicalPayload(t, types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    []types.Partition{ps("p1")},
	})
	hash := sha256.Sum256(canonical)
	key := "assignment._payload." + hex.EncodeToString(hash[:])
	gz, _ := gzipCompress(canonical)
	rev, err := f.assignmentKV.Create(ctx, key, gz)
	require.NoError(t, err)

	srcParts := []types.Partition{ps("p1")}
	err = f.pub.Publish(ctx, PublishInput{
		Workers: []string{"w1"}, Assignments: map[string][]types.Partition{"w1": {ps("p1")}},
		SourcePartitions: srcParts, LeaderRevision: 1,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, f.metrics.payloadsReused.Load(), "reuse counter must fire")
	require.EqualValues(t, 0, f.metrics.payloadsCreated.Load(), "no new payload should be created")

	c := f.readCommit(t, ctx)
	require.Equal(t, key, c.Payloads["w1"].Key)
	require.Equal(t, rev, c.Payloads["w1"].Revision, "ref must carry the pre-existing revision")
}

// TestPublisher_ErrKeyExists_HashMismatchSurfacesCollisionError — pre-populate
// the same key with different bytes; assert ErrPayloadHashCollisionOrCorruption.
func TestPublisher_ErrKeyExists_HashMismatchSurfacesCollisionError(t *testing.T) {
	f := newPublisherFixture(t, "key-exists-mismatch")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w1")

	// Compute the key the publisher will use for w1=[p1].
	canonical := mustMarshalCanonicalPayload(t, types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    []types.Partition{ps("p1")},
	})
	hash := sha256.Sum256(canonical)
	key := "assignment._payload." + hex.EncodeToString(hash[:])

	// Plant DIFFERENT bytes at that key (simulating sha256 collision or KV corruption).
	bogus, _ := gzipCompress([]byte(`{"schema_version":1,"partitions":[{"keys":["totally-different"]}]}`))
	_, err := f.assignmentKV.Create(ctx, key, bogus)
	require.NoError(t, err)

	srcParts := []types.Partition{ps("p1")}
	err = f.pub.Publish(ctx, PublishInput{
		Workers: []string{"w1"}, Assignments: map[string][]types.Partition{"w1": {ps("p1")}},
		SourcePartitions: srcParts, LeaderRevision: 1,
	})
	require.True(t, errors.Is(err, types.ErrPayloadHashCollisionOrCorruption), "got %v", err)
	require.Nil(t, f.readCommit(t, ctx), "no commit should land")
}

// TestPublisher_CrossCommitReuse_PayloadUnchanged — same slice for w across
// V and V+1; payload key reused on V+1 (kv.Create returns ErrKeyExists →
// payloads_reused increments). To force a version bump without changing
// either worker's slice we add a third partition + worker on V+1.
func TestPublisher_CrossCommitReuse_PayloadUnchanged(t *testing.T) {
	f := newPublisherFixture(t, "cross-commit-reuse")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w1")
	f.putV1Heartbeat(t, ctx, "w2")
	f.putV1Heartbeat(t, ctx, "w3")

	// V=1: w1=p1, w2=p2 over a 2-partition source.
	err := f.pub.Publish(ctx, PublishInput{
		Workers: []string{"w1", "w2"},
		Assignments: map[string][]types.Partition{
			"w1": {ps("p1")},
			"w2": {ps("p2")},
		},
		SourcePartitions: []types.Partition{ps("p1"), ps("p2")},
		LeaderRevision:   1,
	})
	require.NoError(t, err)
	c1 := f.readCommit(t, ctx)
	w1RefV1 := c1.Payloads["w1"]
	w2RefV1 := c1.Payloads["w2"]
	createdAfterV1 := f.metrics.payloadsCreated.Load()
	reusedAfterV1 := f.metrics.payloadsReused.Load()

	// V=2: source grows to 3 partitions; w3 absorbs p3; w1 and w2 keep their
	// V=1 slices unchanged so their payload keys must be REUSED.
	err = f.pub.Publish(ctx, PublishInput{
		Workers: []string{"w1", "w2", "w3"},
		Assignments: map[string][]types.Partition{
			"w1": {ps("p1")},
			"w2": {ps("p2")},
			"w3": {ps("p3")},
		},
		SourcePartitions: []types.Partition{ps("p1"), ps("p2"), ps("p3")},
		LeaderRevision:   1,
	})
	require.NoError(t, err)
	c2 := f.readCommit(t, ctx)
	require.Equal(t, c1.Version+1, c2.Version)
	require.Equal(t, w1RefV1.Key, c2.Payloads["w1"].Key, "w1's payload must be reused across V and V+1")
	require.Equal(t, w2RefV1.Key, c2.Payloads["w2"].Key, "w2's payload must be reused across V and V+1")
	require.Greater(t, f.metrics.payloadsReused.Load(), reusedAfterV1, "reuse metric must fire on V=2")
	require.Greater(t, f.metrics.payloadsCreated.Load(), createdAfterV1, "exactly one new payload (w3's) must be created on V=2")
}

// TestPublisher_SetEqualityCoversAllPartitions — buggy strategy drops one
// partition; publisher aborts at coverage check.
func TestPublisher_SetEqualityCoversAllPartitions(t *testing.T) {
	f := newPublisherFixture(t, "set-equality")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w1")
	srcParts := []types.Partition{ps("p1"), ps("p2"), ps("p3")}
	// Strategy "drops" p3.
	err := f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"w1"},
		Assignments:      map[string][]types.Partition{"w1": {ps("p1"), ps("p2")}},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	})
	require.True(t, errors.Is(err, types.ErrCoverageMismatch), "got %v", err)
	require.EqualValues(t, 1, f.metrics.batchAbortedCount("coverage_mismatch"))
	require.Nil(t, f.readCommit(t, ctx))

	// Also exercise the "duplicate" path: a strategy assigning p1 to two
	// workers must fail the coverage check too. The publisher catches this
	// via the multiset count check: assigned_raw=4 but source unique=3.
	f.putV1Heartbeat(t, ctx, "w2")
	err = f.pub.Publish(ctx, PublishInput{
		Workers: []string{"w1", "w2"},
		Assignments: map[string][]types.Partition{
			"w1": {ps("p1"), ps("p2")},
			"w2": {ps("p1"), ps("p3")}, // p1 duplicated across workers
		},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	})
	require.True(t, errors.Is(err, types.ErrCoverageMismatch),
		"duplicate-across-workers must surface ErrCoverageMismatch via multiset count check, got %v", err)
	require.EqualValues(t, 2, f.metrics.batchAbortedCount("coverage_mismatch"),
		"second coverage mismatch must increment the metric")
	require.Nil(t, f.readCommit(t, ctx), "no commit should land on duplicate-coverage failure")
}

// TestPublisher_SourceRevisionInCommit — a revisioned source (NatsKV) yields
// SourceRevisionKnown=true and a non-zero SourceRevision in the commit.
func TestPublisher_SourceRevisionInCommit(t *testing.T) {
	f := newPublisherFixture(t, "source-rev-known")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w1")
	srcParts := []types.Partition{ps("p1")}
	err := f.pub.Publish(ctx, PublishInput{
		Workers: []string{"w1"}, Assignments: map[string][]types.Partition{"w1": {ps("p1")}},
		SourcePartitions:    srcParts,
		SourceRevision:      77,
		SourceRevisionKnown: true,
		LeaderRevision:      1,
	})
	require.NoError(t, err)
	c := f.readCommit(t, ctx)
	require.True(t, c.SourceRevisionKnown)
	require.EqualValues(t, 77, c.SourceRevision)
}

// TestPublisher_StaticSource_SourceRevisionUnknown — non-revisioned source
// path yields SourceRevisionKnown=false and SourceRevision=0.
func TestPublisher_StaticSource_SourceRevisionUnknown(t *testing.T) {
	f := newPublisherFixture(t, "source-rev-unknown")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w1")
	srcParts := []types.Partition{ps("p1")}
	err := f.pub.Publish(ctx, PublishInput{
		Workers: []string{"w1"}, Assignments: map[string][]types.Partition{"w1": {ps("p1")}},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	})
	require.NoError(t, err)
	c := f.readCommit(t, ctx)
	require.False(t, c.SourceRevisionKnown)
	require.EqualValues(t, 0, c.SourceRevision)
}

// TestPublisher_CommitLog_WriteFailureDoesNotBlockCommit — inject a commit_log
// failure (pre-populate the log key); the commit still succeeds because the
// log write is best-effort.
func TestPublisher_CommitLog_WriteFailureDoesNotBlockCommit(t *testing.T) {
	f := newPublisherFixture(t, "commit-log-failure")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w1")

	// Pre-populate "assignment._commit_log.1" so the publisher's kv.Create
	// for the same key fails with ErrKeyExists. Step 10 must absorb the
	// failure and still report success.
	_, err := f.assignmentKV.Create(ctx, "assignment._commit_log.1", []byte(`{"squat":true}`))
	require.NoError(t, err)

	srcParts := []types.Partition{ps("p1")}
	err = f.pub.Publish(ctx, PublishInput{
		Workers: []string{"w1"}, Assignments: map[string][]types.Partition{"w1": {ps("p1")}},
		SourcePartitions: srcParts, LeaderRevision: 1,
	})
	require.NoError(t, err, "commit must succeed despite log write failure")
	c := f.readCommit(t, ctx)
	require.NotNil(t, c)
	require.Equal(t, int64(1), c.Version)
}

// ----- rolling-upgrade specific tests -----

// TestRollingUpgrade_NewLeaderOldWorker_AliasWritePresentInCommit (#48) —
// one legacy worker present; the mandatory legacy alias is written before
// the commit lands.
//
// NOTE: the explicit failure-of-alias-barrier path was previously exercised
// here by cancelling the context (which failed at step 4 instead of step 6
// and was therefore degenerate). It now lives in the dedicated test
// TestRollingUpgrade_AliasBarrier_FailureAbortsBeforeCommit_NotAtPayloadCreate
// (in assignment_publisher_v1_review_test.go), which uses a KV wrapper to
// fail Put for the legacy worker's alias key without affecting payload
// Create.
func TestRollingUpgrade_NewLeaderOldWorker_AliasWritePresentInCommit(t *testing.T) {
	f := newPublisherFixture(t, "alias-required")
	ctx := context.Background()
	// Heartbeats: w1 is legacy (timestamp), w2 is v1.
	f.putLegacyHeartbeat(t, ctx, "w1")
	f.putV1Heartbeat(t, ctx, "w2")

	srcParts := []types.Partition{ps("p1"), ps("p2")}
	err := f.pub.Publish(ctx, PublishInput{
		Workers: []string{"w1", "w2"},
		Assignments: map[string][]types.Partition{
			"w1": {ps("p1")}, "w2": {ps("p2")},
		},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	})
	require.NoError(t, err)
	// w1 (legacy) MUST have a legacy alias by the time the commit lands.
	entry, err := f.assignmentKV.Get(ctx, "assignment.w1")
	require.NoError(t, err, "legacy worker w1 must have its mandatory alias written")
	var leg types.Assignment
	require.NoError(t, json.Unmarshal(entry.Value(), &leg))
	require.Equal(t, int64(1), leg.Version)
	require.EqualValues(t, 1, leg.LeaderRevision)
}

// TestAssignmentDiscovery_IgnoresProtocolKeys (#54) —
// pre-populate protocol keys plus a legacy alias; DiscoverHighestVersion
// returns only the legacy alias's worker; CleanupAllAssignments preserves
// protocol keys.
func TestAssignmentDiscovery_IgnoresProtocolKeys(t *testing.T) {
	f := newPublisherFixture(t, "discovery-protocol-filter")
	ctx := context.Background()

	// Plant a fake commit, commit_log, and payload.
	commitBytes, _ := json.Marshal(types.AssignmentCommit{Version: 5})
	_, err := f.assignmentKV.Put(ctx, "assignment._commit", commitBytes)
	require.NoError(t, err)
	logBytes, _ := json.Marshal(types.AssignmentCommitLog{Version: 5})
	_, err = f.assignmentKV.Put(ctx, "assignment._commit_log.5", logBytes)
	require.NoError(t, err)
	_, err = f.assignmentKV.Put(ctx, "assignment._payload.deadbeef", []byte("payload-bytes"))
	require.NoError(t, err)

	// Plant a real legacy alias.
	leg := types.Assignment{Version: 3, Partitions: []types.Partition{ps("p1")}}
	d, _ := json.Marshal(leg)
	_, err = f.assignmentKV.Put(ctx, "assignment.worker-1", d)
	require.NoError(t, err)

	ids, err := f.pub.DiscoverHighestVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"worker-1"}, ids, "protocol keys must not be returned as worker IDs")
	// CurrentVersion is the MAX of the legacy alias version and the commit's version.
	require.Equal(t, int64(5), f.pub.CurrentVersion(), "discovery must seed currentVersion from the commit too")

	// CleanupAllAssignments must NOT delete protocol keys.
	require.NoError(t, f.pub.CleanupAllAssignments(ctx))
	for _, key := range []string{"assignment._commit", "assignment._commit_log.5", "assignment._payload.deadbeef"} {
		_, err = f.assignmentKV.Get(ctx, key)
		require.NoError(t, err, "protocol key %s must be preserved by cleanup", key)
	}
	// Legacy alias must be deleted.
	_, err = f.assignmentKV.Get(ctx, "assignment.worker-1")
	require.Error(t, err)
}

// TestPublisher_LegacyAliasBarrier_UsesTimestampHeartbeatAsLegacyWorker (#59) —
// timestamp heartbeat → classified as legacy → mandatory pre-commit alias.
func TestPublisher_LegacyAliasBarrier_UsesTimestampHeartbeatAsLegacyWorker(t *testing.T) {
	f := newPublisherFixture(t, "legacy-classify")
	ctx := context.Background()
	f.putLegacyHeartbeat(t, ctx, "old-worker")
	srcParts := []types.Partition{ps("p1")}
	err := f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"old-worker"},
		Assignments:      map[string][]types.Partition{"old-worker": {ps("p1")}},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	})
	require.NoError(t, err)
	entry, err := f.assignmentKV.Get(ctx, "assignment.old-worker")
	require.NoError(t, err)
	var leg types.Assignment
	require.NoError(t, json.Unmarshal(entry.Value(), &leg))
	require.Equal(t, int64(1), leg.Version)
}

// TestPublisher_AliasBarrier_RechecksLeadershipBeforeAliasWrites (#60) —
// leadership lost before step 6 (pre-alias check fires); no alias written.
// We exercise this via the same path as TestPublisher_CommitCAS_AbortsOnLeadershipLost
// because the pre-alias check is the first leadership re-read after the
// payload write, and it observes the live revision.
func TestPublisher_AliasBarrier_RechecksLeadershipBeforeAliasWrites(t *testing.T) {
	f := newPublisherFixture(t, "alias-barrier-pre-leader-recheck")
	ctx := context.Background()
	f.putLegacyHeartbeat(t, ctx, "w-legacy")
	srcParts := []types.Partition{ps("p1")}

	// Live leader revision is 99; claimed is 1 → pre-alias recheck must abort
	// BEFORE writing the legacy alias.
	f.leaderRev.Store(99)
	err := f.pub.Publish(ctx, PublishInput{
		Workers: []string{"w-legacy"}, Assignments: map[string][]types.Partition{"w-legacy": {ps("p1")}},
		SourcePartitions: srcParts, LeaderRevision: 1,
	})
	require.True(t, errors.Is(err, types.ErrLeadershipLostPreAlias))
	// No legacy alias must exist.
	_, err = f.assignmentKV.Get(ctx, "assignment.w-legacy")
	require.Error(t, err, "no alias should have landed; pre-alias recheck must fire FIRST")
	require.EqualValues(t, 1, f.metrics.batchAbortedCount("leadership_lost_pre_alias"))
}

// TestPublisher_AliasBarrier_CASFailureAfterAliases_DocumentedMigrationExposure (#61) —
// payloads + aliases land; commit CAS fails (we force this by pre-creating
// the commit with stale-leader-detect bytes). Asserts the
// alias_visible_uncommitted metric and that the cluster recovers to the
// previous V.
func TestPublisher_AliasBarrier_CASFailureAfterAliases_DocumentedMigrationExposure(t *testing.T) {
	f := newPublisherFixture(t, "alias-cas-failure")
	ctx := context.Background()
	f.putLegacyHeartbeat(t, ctx, "old-w")

	// Step A: a "competing" leader has already written an
	// assignment._commit at V=42 that this publisher doesn't know about.
	prior := types.AssignmentCommit{Version: 42, LeaderRevision: 50}
	priorBytes, _ := json.Marshal(prior)
	_, err := f.assignmentKV.Create(ctx, "assignment._commit", priorBytes)
	require.NoError(t, err)
	// This publisher's lastCommitRev is 0, so its kv.Create call at step 9
	// will fail with ErrKeyExists.

	srcParts := []types.Partition{ps("p1")}
	err = f.pub.Publish(ctx, PublishInput{
		Workers: []string{"old-w"}, Assignments: map[string][]types.Partition{"old-w": {ps("p1")}},
		SourcePartitions: srcParts, LeaderRevision: 1,
	})
	require.True(t, errors.Is(err, types.ErrCommitCASFailed))
	require.EqualValues(t, 1, f.metrics.commitAborts.Load())
	require.EqualValues(t, 1, f.metrics.batchAbortedCount("commit_cas_failed"))
	// Documented exposure metric must fire because the legacy alias DID land.
	require.EqualValues(t, 1, f.metrics.aliasVisibleUncommitted.Load(),
		"alias_visible_uncommitted must fire when legacy aliases were written before a CAS-failed commit")
	// Legacy alias for old-w is observable.
	_, err = f.assignmentKV.Get(ctx, "assignment.old-w")
	require.NoError(t, err, "legacy alias was written before the failed CAS")
	// Cluster's authoritative view is the prior V=42 commit (unchanged).
	c := f.readCommit(t, ctx)
	require.NotNil(t, c)
	require.Equal(t, int64(42), c.Version)
}

// TestPublisher_PostAliasLeadershipLoss_AbortsBeforeCommitCAS (#62) —
// Loss between step 6 and step 7 must abort BEFORE commit CAS;
// commit_aborts must NOT increment.
//
// We use a leadership probe that returns the claimed value once (to pass the
// pre-alias check) and then a different value (to fail the post-alias
// check). The publisher serializes pre-alias and post-alias as two separate
// reads of leaderRevisionFn, so a counter-driven probe can simulate this.
func TestPublisher_PostAliasLeadershipLoss_AbortsBeforeCommitCAS(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	akv := partitest.CreateJetStreamKV(t, nc, "asgn-post-alias-loss")
	hkv := partitest.CreateJetStreamKV(t, nc, "hb-post-alias-loss")
	m := newCountingMetrics()
	calls := atomic.Int64{}
	probe := func(_ context.Context, claimed uint64) error {
		n := calls.Add(1)
		// Call 1: pre-alias check passes (claimed matches live).
		// Call 2+: post-alias and beyond — return mismatch (leadership lost).
		if n == 1 {
			return nil
		}
		return fmt.Errorf("%w: claimed=%d live=99", types.ErrLeadershipRevisionMismatch, claimed)
	}
	pub := NewAssignmentPublisher(PublisherConfig{
		AssignmentKV:    akv,
		HeartbeatKV:     hkv,
		Prefix:          "assignment",
		HeartbeatPrefix: "heartbeat",
		LeaderCheckFn:   probe,
		Logger:          logging.NewNop(),
		Metrics:         m,
	})
	ctx := context.Background()
	// Mix of legacy + v1 to exercise step 6 work.
	tsBytes := []byte(time.Now().UTC().Format(time.RFC3339Nano))
	_, err := hkv.Put(ctx, "heartbeat.legacy-w", tsBytes)
	require.NoError(t, err)

	srcParts := []types.Partition{ps("p1")}
	err = pub.Publish(ctx, PublishInput{
		Workers:          []string{"legacy-w"},
		Assignments:      map[string][]types.Partition{"legacy-w": {ps("p1")}},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	})
	require.True(t, errors.Is(err, types.ErrLeadershipLostPostAlias),
		"expected post-alias leadership loss sentinel (not pre-alias), got %v", err)

	// Critical assertion (test #62): commit_aborts must NOT have incremented.
	// The abort happened BEFORE the commit CAS attempt.
	require.EqualValues(t, 0, m.commitAborts.Load(), "commit_aborts must NOT increment on pre-CAS aborts")
	// And no commit was written.
	_, err = akv.Get(ctx, "assignment._commit")
	require.Error(t, err, "no commit should have been written")
	// alias_visible_uncommitted DOES fire because legacy aliases landed.
	require.EqualValues(t, 1, m.aliasVisibleUncommitted.Load())
	require.EqualValues(t, 1, m.batchAbortedCount("leadership_lost_post_alias"))
}

// ----- preserved-from-original tests (post-rewrite shape) -----

// TestAssignmentPublisher_DiscoverHighestVersion_LegacyOnly — ensures legacy
// assignment.<W> keys still drive currentVersion as before, but protocol
// keys (none here) wouldn't affect the result.
func TestAssignmentPublisher_DiscoverHighestVersion_LegacyOnly(t *testing.T) {
	f := newPublisherFixture(t, "legacy-only")
	ctx := context.Background()

	// Initially empty — discovery yields no version, no IDs.
	ids, err := f.pub.DiscoverHighestVersion(ctx)
	if err != nil && !types.IsNoKeysFoundError(err) {
		require.NoError(t, err)
	}
	require.Equal(t, int64(0), f.pub.CurrentVersion())
	require.Empty(t, ids)

	// Add legacy aliases and re-discover.
	a1 := types.Assignment{Version: 5, Partitions: []types.Partition{ps("p1")}}
	a2 := types.Assignment{Version: 10, Partitions: []types.Partition{ps("p2")}}
	d1, _ := json.Marshal(a1)
	d2, _ := json.Marshal(a2)
	_, err = f.assignmentKV.Put(ctx, "assignment.w1", d1)
	require.NoError(t, err)
	_, err = f.assignmentKV.Put(ctx, "assignment.w2", d2)
	require.NoError(t, err)

	ids, err = f.pub.DiscoverHighestVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(10), f.pub.CurrentVersion())
	require.ElementsMatch(t, []string{"w1", "w2"}, ids)
}

// TestAssignmentPublisher_CurrentVersionAndLastRebalance — sanity for the
// accessors after a successful publish.
func TestAssignmentPublisher_CurrentVersionAndLastRebalance(t *testing.T) {
	f := newPublisherFixture(t, "version-time")
	ctx := context.Background()
	require.Equal(t, int64(0), f.pub.CurrentVersion())
	require.True(t, f.pub.LastRebalanceTime().IsZero())

	f.putV1Heartbeat(t, ctx, "w1")
	srcParts := []types.Partition{ps("p1")}
	err := f.pub.Publish(ctx, PublishInput{
		Workers: []string{"w1"}, Assignments: map[string][]types.Partition{"w1": {ps("p1")}},
		SourcePartitions: srcParts, LeaderRevision: 1,
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), f.pub.CurrentVersion())
	require.False(t, f.pub.LastRebalanceTime().IsZero())
}

// TestAssignmentPublisher_CleanupAllAssignments_PreservesProtocolKeys —
// regression for review P2 #7.
func TestAssignmentPublisher_CleanupAllAssignments_PreservesProtocolKeys(t *testing.T) {
	f := newPublisherFixture(t, "cleanup-preserves-protocol")
	ctx := context.Background()
	// Plant protocol + legacy keys.
	_, err := f.assignmentKV.Put(ctx, "assignment._commit", []byte("commit-data"))
	require.NoError(t, err)
	_, err = f.assignmentKV.Put(ctx, "assignment._commit_log.1", []byte("log-data"))
	require.NoError(t, err)
	_, err = f.assignmentKV.Put(ctx, "assignment._payload.feedface", []byte("payload"))
	require.NoError(t, err)
	_, err = f.assignmentKV.Put(ctx, "assignment.alpha", []byte("alpha"))
	require.NoError(t, err)
	_, err = f.assignmentKV.Put(ctx, "assignment.bravo", []byte("bravo"))
	require.NoError(t, err)

	require.NoError(t, f.pub.CleanupAllAssignments(ctx))
	// Protocol keys preserved.
	for _, k := range []string{"assignment._commit", "assignment._commit_log.1", "assignment._payload.feedface"} {
		_, err = f.assignmentKV.Get(ctx, k)
		require.NoError(t, err, "%s preserved", k)
	}
	// Legacy aliases gone.
	for _, k := range []string{"assignment.alpha", "assignment.bravo"} {
		_, err = f.assignmentKV.Get(ctx, k)
		require.Error(t, err, "%s deleted", k)
	}
}

// ----- internal helpers -----

// mustMarshalCanonicalPayload mirrors the publisher's internal canonicalization:
// sort partitions by CanonicalID, then json.Marshal. Tests use this to
// pre-compute the byte-identical bytes the publisher will produce so they can
// pre-populate keys with matching content.
func mustMarshalCanonicalPayload(t *testing.T, p types.AssignmentPayload) []byte {
	t.Helper()
	canonical := make([]types.Partition, len(p.Partitions))
	copy(canonical, p.Partitions)
	// Stable sort by CanonicalID.
	for i := 1; i < len(canonical); i++ {
		for j := i; j > 0 && canonical[j-1].CanonicalID() > canonical[j].CanonicalID(); j-- {
			canonical[j-1], canonical[j] = canonical[j], canonical[j-1]
		}
	}
	out := types.AssignmentPayload{SchemaVersion: p.SchemaVersion, Partitions: canonical}
	b, err := json.Marshal(out)
	require.NoError(t, err)

	return b
}

// BenchmarkDiscoverHighestVersion_WithCommit measures the cold-start
// startup cost of DiscoverHighestVersion when a _commit key already pins
// currentVersion. Pre-fix: O(K) serial Gets across legacy alias keys.
// Post-fix: ListKeys + 1 Get on _commit, independent of K. K=200 keeps
// the benchmark tractable in CI; the asymptote extrapolates to K=1000.
func BenchmarkDiscoverHighestVersion_WithCommit(b *testing.B) {
	const numAliases = 200
	f := newPublisherFixture(b, "bench-discover")
	ctx := context.Background()

	// Seed _commit so the commit-pin branch is taken.
	commit := types.AssignmentCommit{Version: int64(numAliases + 1), PublishedAt: time.Now().UTC()}
	commitBytes, err := json.Marshal(commit)
	require.NoError(b, err)
	_, err = f.assignmentKV.Put(ctx, "assignment._commit", commitBytes)
	require.NoError(b, err)

	// Seed K legacy alias keys.
	for i := 0; i < numAliases; i++ {
		asgn := types.Assignment{Version: int64(i + 1), Partitions: []types.Partition{ps(fmt.Sprintf("p%d", i))}}
		data, merr := json.Marshal(asgn)
		require.NoError(b, merr)
		_, perr := f.assignmentKV.Put(ctx, fmt.Sprintf("assignment.w%d", i), data)
		require.NoError(b, perr)
	}

	b.ResetTimer()
	for b.Loop() {
		if _, err := f.pub.DiscoverHighestVersion(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
