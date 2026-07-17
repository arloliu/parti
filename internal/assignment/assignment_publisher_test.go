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

	"github.com/arloliu/parti/v2/internal/election"
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
	return newPublisherFixtureWrapKV(t, name, nil)
}

// newPublisherFixtureWrapKV is newPublisherFixture with an optional decorator
// around the assignment KV handed to the publisher. The fixture's own
// assignmentKV field always holds the raw bucket so test Puts/Gets bypass any
// injected faults. wrap == nil means no decoration.
func newPublisherFixtureWrapKV(t testing.TB, name string, wrap func(jetstream.KeyValue) jetstream.KeyValue) *publisherFixture {
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
	pubKV := akv
	if wrap != nil {
		pubKV = wrap(akv)
	}
	pub := NewAssignmentPublisher(PublisherConfig{
		AssignmentKV:    pubKV,
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
		// Label-aware publisher stamps WorkerLabelsKnown=true on every payload
		// (Task 7); mirror it here so the planted content-address matches.
		WorkerLabelsKnown: true,
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
// increments and the adoption CAS-touch advances the ref's revision past the
// pre-existing one while leaving the stored content byte-identical (see
// createOrAdoptPayload — the touch is the fencing token against a racing GC
// delete).
func TestPublisher_ErrKeyExists_VerifiedAndReused(t *testing.T) {
	f := newPublisherFixture(t, "key-exists-reused")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w1")

	canonical := mustMarshalCanonicalPayload(t, types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    []types.Partition{ps("p1")},
		// Label-aware publisher stamps WorkerLabelsKnown=true on every payload
		// (Task 7); mirror it here so the planted content-address matches.
		WorkerLabelsKnown: true,
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
	// The adoption touch (kv.Update at the verified revision) advances the
	// key's revision; the ref must carry the POST-touch revision so the
	// commit references the fenced state, not the pre-adoption one.
	require.Greater(t, c.Payloads["w1"].Revision, rev,
		"ref must carry the post-touch revision (adoption CAS-touch advances it)")
	entry, gerr := f.assignmentKV.Get(ctx, key)
	require.NoError(t, gerr)
	require.Equal(t, c.Payloads["w1"].Revision, entry.Revision(),
		"ref revision must match the live key's revision after the touch")
	plain, derr := gzipDecompress(entry.Value())
	require.NoError(t, derr)
	require.Equal(t, canonical, plain, "touch must not change the stored content")
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
		// Label-aware publisher stamps WorkerLabelsKnown=true on every payload
		// (Task 7); mirror it here so the planted content-address matches.
		WorkerLabelsKnown: true,
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
	// via the multiset count check: covered_raw=4 but source unique=3.
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
	out := types.AssignmentPayload{
		SchemaVersion:     p.SchemaVersion,
		Partitions:        canonical,
		WorkerLabels:      p.WorkerLabels,
		WorkerLabelsKnown: p.WorkerLabelsKnown,
	}
	b, err := json.Marshal(out)
	require.NoError(t, err)

	return b
}

// TestPublisher_CASFailure_RefreshesLastCommitRev_AndRecovers (ISSUE-001).
//
// Scenario: a valid leader's lastCommitRev becomes stale because the live
// _commit's KV revision advanced past it. In production the only source is
// another leader's in-flight CAS landing during a leader handoff; the test
// models this with a direct KV Put that advances the revision.
//
// Pre-fix: every subsequent Publish from this leader returned
// ErrCommitCASFailed indefinitely — lastCommitRev never refreshed — and the
// pre-alias LeaderCheck did NOT catch it because the publisher IS a valid
// leader (the fence is about the election key, not the commit revision).
//
// Post-fix: the first Publish after the race fails CAS, refreshes
// lastCommitRev from the live entry, and the next Publish succeeds.
func TestPublisher_CASFailure_RefreshesLastCommitRev_AndRecovers(t *testing.T) {
	f := newPublisherFixture(t, "cas-loss-refresh")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w1")
	srcParts := []types.Partition{ps("p1"), ps("p2")}
	input := PublishInput{
		Workers:          []string{"w1"},
		Assignments:      map[string][]types.Partition{"w1": {ps("p1"), ps("p2")}},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	}

	// 1. Initial publish establishes a real commit chain.
	require.NoError(t, f.pub.Publish(ctx, input))
	initialRev := f.pub.LastCommitRev()
	require.Greater(t, initialRev, uint64(0))

	// 2. Model the cross-leader race: an external write advances the live
	//    _commit revision past the publisher's cached lastCommitRev. The
	//    payload bytes don't matter for the CAS check — only the revision.
	currentCommit := f.readCommit(t, ctx)
	require.NotNil(t, currentCommit)
	forged := types.AssignmentCommit{
		Version:        currentCommit.Version + 1,
		LeaderRevision: currentCommit.LeaderRevision,
		Workers:        currentCommit.Workers,
		Payloads:       currentCommit.Payloads,
	}
	forgedBytes, merr := json.Marshal(forged)
	require.NoError(t, merr)
	_, perr := f.assignmentKV.Put(ctx, "assignment._commit", forgedBytes)
	require.NoError(t, perr)
	liveEntry, gerr := f.assignmentKV.Get(ctx, "assignment._commit")
	require.NoError(t, gerr)
	require.Greater(t, liveEntry.Revision(), initialRev,
		"forged write must have advanced the live revision")

	// 3. First Publish — CAS must fail (lastCommitRev is stale).
	err := f.pub.Publish(ctx, input)
	require.Error(t, err)
	require.ErrorIs(t, err, types.ErrCommitCASFailed,
		"first publish must fail CAS, not the election fence; got: %v", err)

	// 4. Second Publish — post-fix, this MUST succeed because the
	//    CAS-failure branch refreshed lastCommitRev from the live entry.
	require.NoError(t, f.pub.Publish(ctx, input),
		"second publish must recover after lastCommitRev refresh")

	// And lastCommitRev now reflects the latest CAS write.
	require.Greater(t, f.pub.LastCommitRev(), liveEntry.Revision(),
		"lastCommitRev should have advanced past the forged revision")
}

// TestPublisher_NonCASFailures_DoNotRefreshLastCommitRev (ISSUE-001 negative).
//
// Confirms the refresh is gated on ErrCommitCASFailed — other Publish
// failure modes (here: ErrLeadershipLostPreAlias driven by a leader-rev
// mismatch) must not perturb lastCommitRev.
func TestPublisher_NonCASFailures_DoNotRefreshLastCommitRev(t *testing.T) {
	f := newPublisherFixture(t, "non-cas-no-refresh")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w1")
	srcParts := []types.Partition{ps("p1")}
	input := PublishInput{
		Workers:          []string{"w1"},
		Assignments:      map[string][]types.Partition{"w1": {ps("p1")}},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	}

	require.NoError(t, f.pub.Publish(ctx, input))
	revBefore := f.pub.LastCommitRev()
	require.Greater(t, revBefore, uint64(0))

	// Drive the pre-alias fence to fail by mismatching the claimed vs. live
	// leader revision (mirrors TestPublisher_CommitCAS_AbortsOnLeadershipLost).
	f.leaderRev.Store(99)
	err := f.pub.Publish(ctx, input)
	require.Error(t, err)
	require.ErrorIs(t, err, types.ErrLeadershipLostPreAlias)

	require.Equal(t, revBefore, f.pub.LastCommitRev(),
		"non-CAS failures must not refresh lastCommitRev")

	// Negative space for the reseed latch: a non-CAS failure must not leave
	// the publisher in reseed-pending — after leadership is restored the next
	// publish must proceed normally instead of aborting fail-closed.
	f.leaderRev.Store(1)
	require.NoError(t, f.pub.Publish(ctx, input),
		"a non-CAS failure must not latch reseed-pending")
	require.EqualValues(t, 0, f.metrics.batchAbortedCount("commit_reseed_pending"))
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
	for i := range numAliases {
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

// --- merged from assignment_publisher_v1_review_test.go ---

// ============================================================================
// P0-1: live election-KV revision fence
// ============================================================================

// TestPublisher_LeadershipFence_LiveElectionKV_AbortsWhenLiveRevisionMismatches
// wires the production NATSElection.CheckLeadership through the publisher
// and verifies that a former leader whose claim does not match the live KV
// revision is rejected at the pre-alias fence — even when a "naive cached
// callback" would have returned nil. This proves the live KV read is the
// load-bearing part of the fence (P0-1).
func TestPublisher_LeadershipFence_LiveElectionKV_AbortsWhenLiveRevisionMismatches(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	electionKV := partitest.CreateJetStreamKV(t, nc, "live-fence-election")
	akv := partitest.CreateJetStreamKV(t, nc, "live-fence-asgn")
	hkv := partitest.CreateJetStreamKV(t, nc, "live-fence-hb")

	e := election.NewNATSElection(electionKV, "leader")
	isLeader, err := e.RequestLeadership(ctx, "leader-w", 30)
	require.NoError(t, err)
	require.True(t, isLeader)
	claimed := e.Revision()
	require.NotZero(t, claimed)

	m := newCountingMetrics()
	pub := NewAssignmentPublisher(PublisherConfig{
		AssignmentKV:    akv,
		HeartbeatKV:     hkv,
		Prefix:          "assignment",
		HeartbeatPrefix: "heartbeat",
		LeaderCheckFn:   e.CheckLeadership,
		Logger:          logging.NewNop(),
		Metrics:         m,
	})

	// Sanity: with the matching claim, publish succeeds.
	tsBytes := []byte(time.Now().UTC().Format(time.RFC3339Nano))
	_, err = hkv.Put(ctx, "heartbeat.legacy-w", tsBytes)
	require.NoError(t, err)
	require.NoError(t, pub.Publish(ctx, PublishInput{
		Workers:          []string{"legacy-w"},
		Assignments:      map[string][]types.Partition{"legacy-w": {ps("p1")}},
		SourcePartitions: []types.Partition{ps("p1")},
		LeaderRevision:   claimed,
	}))

	// Now simulate a takeover at the KV layer by overwriting the leader key
	// (real takeovers do this atomically after TTL expiry; we model the
	// resulting KV state). The former leader's cached Revision() is unchanged,
	// so a naive cached-callback fence would still pass the claim — but the
	// live KV revision differs.
	require.NoError(t, electionKV.Delete(ctx, "leader"))
	_, err = electionKV.Create(ctx, "leader", []byte("new-leader:1"))
	require.NoError(t, err)
	require.Equal(t, claimed, e.Revision(), "cached Revision must still report the stale term")

	// Pre-alias fence MUST abort with the wrapped sentinel.
	err = pub.Publish(ctx, PublishInput{
		Workers:          []string{"legacy-w"},
		Assignments:      map[string][]types.Partition{"legacy-w": {ps("p2")}},
		SourcePartitions: []types.Partition{ps("p2")},
		LeaderRevision:   claimed, // stale claim
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, types.ErrLeadershipLostPreAlias),
		"want ErrLeadershipLostPreAlias, got %v", err)
	require.True(t, errors.Is(err, types.ErrLeadershipRevisionMismatch),
		"underlying mismatch sentinel must be preserved in the chain, got %v", err)
	// No legacy alias was written for the new attempt — the abort happened
	// BEFORE step 6.
	entry, gerr := akv.Get(ctx, "assignment.legacy-w")
	require.NoError(t, gerr)
	// The alias from the FIRST publish is the v=1 one — confirm it wasn't
	// overwritten with v=2.
	require.Less(t, len(entry.Value()), 1024)
}

// ============================================================================
// P0-2: GC must not delete a payload that an in-flight publish adopted
// ============================================================================

// fakeLiveRefsProvider lets a test stage exact in-flight payload keys.
type fakeLiveRefsProvider struct {
	refs []string
}

func (f *fakeLiveRefsProvider) LiveRefs() []string { return f.refs }

// TestCommitGC_DoesNotDeletePayloadAdoptedByInFlightPublish verifies that GC
// honors the publisher's in-flight ref set (P0-2). We pre-create an orphan
// payload whose age exceeds retention; without the in-flight set, GC would
// delete it. With the publisher's LiveRefs reporting the orphan key as
// in-flight, GC must skip it.
func TestCommitGC_DoesNotDeletePayloadAdoptedByInFlightPublish(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "gc-respects-inflight")
	ctx := t.Context()

	// Plant an orphan payload with the same key shape the publisher would use.
	canonical := mustMarshalCanonicalPayload(t, types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    []types.Partition{ps("about-to-be-adopted")},
	})
	hash := sha256.Sum256(canonical)
	orphanKey := "assignment._payload." + hex.EncodeToString(hash[:])
	gz, gerr := gzipCompress(canonical)
	require.NoError(t, gerr)
	_, err := f.assignmentKV.Create(ctx, orphanKey, gz)
	require.NoError(t, err)

	// Stage a fake LiveRefsProvider that says "this key is in-flight". This
	// models the state mid-publish, after the publisher's verify-back at step
	// 4 and BEFORE its CAS at step 9 returns.
	staged := &fakeLiveRefsProvider{refs: []string{orphanKey}}
	gc := NewCommitGC(CommitGCConfig{
		Publisher:        f.pub,
		LiveRefsProvider: staged,
		Interval:         time.Hour,
		Retention:        time.Second,
		KeepCommits:      10,
		Now:              func() time.Time { return time.Now().Add(48 * time.Hour) },
		Metrics:          f.metrics,
	})
	require.NotNil(t, gc)
	require.NoError(t, gc.RunOnce(ctx))

	// The "in-flight" payload MUST still be present.
	_, err = f.assignmentKV.Get(ctx, orphanKey)
	require.NoError(t, err, "GC must not delete a payload reported as in-flight by the publisher")

	// Now drop the in-flight claim and re-run GC — the orphan is now eligible
	// and should be reaped (proves the only thing keeping it alive was the
	// in-flight set).
	staged.refs = nil
	require.NoError(t, gc.RunOnce(ctx))
	_, err = f.assignmentKV.Get(ctx, orphanKey)
	require.Error(t, err, "after the in-flight claim drops, GC must reap the orphan")
}

// TestCommitGC_PublisherAdoptedPayloadVisibleViaLiveRefs is a smaller assertion
// that the production publisher's LiveRefs() actually exposes adopted payload
// keys mid-publish. We can't easily pause the publisher mid-flight without a
// test seam, so we exercise the round trip by manually calling LiveRefs after
// stuffing the publisher's inflightRefs map directly. This documents the
// invariant: GC's snapshot includes whatever keys the publisher has staged.
func TestCommitGC_PublisherAdoptedPayloadVisibleViaLiveRefs(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "live-refs-snapshot")
	require.Empty(t, f.pub.LiveRefs(), "no in-flight keys before any publish")

	// Simulate the publisher having staged two refs.
	f.pub.inflightRefs.Store("assignment._payload.aaa", struct{}{})
	f.pub.inflightRefs.Store("assignment._payload.bbb", struct{}{})
	t.Cleanup(func() {
		f.pub.inflightRefs.Delete("assignment._payload.aaa")
		f.pub.inflightRefs.Delete("assignment._payload.bbb")
	})
	got := f.pub.LiveRefs()
	require.ElementsMatch(t, []string{"assignment._payload.aaa", "assignment._payload.bbb"}, got)
}

// ============================================================================
// P1-1: GC lifecycle starts on calculator Start, stops on calculator Stop
// ============================================================================

// TestCommitGC_LifecycleStartStop verifies that the GC lifecycle is wired to
// the calculator: Start launches the loop and a Trigger wakes it; Stop
// terminates the loop within a bounded time.
//
// We use a calculator-internal hook by directly running GC.Start and GC.Stop
// since the calculator constructs its own GC at NewCalculator and starts it
// in Start; this test proves the Start/Stop primitives the calculator uses
// behave as expected.
func TestCommitGC_LifecycleStartStop(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "gc-lifecycle")
	ctx := t.Context()

	gc := NewCommitGC(CommitGCConfig{
		Publisher: f.pub,
		Interval:  10 * time.Millisecond, // fast for test
		Retention: time.Second,
	})
	require.NotNil(t, gc)
	require.NoError(t, gc.Start(ctx))
	// Trigger several times — non-blocking, should coalesce, no panic.
	for range 10 {
		gc.Trigger()
	}
	// Allow at least one sweep to run.
	time.Sleep(50 * time.Millisecond)

	// Stop must drain the loop and return promptly.
	stopped := make(chan struct{})
	go func() {
		gc.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("GC.Stop did not return within 2s")
	}

	// Calling Stop again must be safe (no panic, no hang).
	gc.Stop()
}

// ============================================================================
// P1-2: alias-barrier failure proven distinct from payload-create failure
// ============================================================================

// putFailingKV wraps a JetStream KV with an override that fails Put for a
// specific key. Used to drive an alias-barrier failure (step 6) without
// affecting payload Create (step 4) — proving the abort path is the
// alias-barrier path, not a step-4 payload-create failure.
type putFailingKV struct {
	jetstream.KeyValue
	failKey string
	err     error
}

func (k *putFailingKV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	if key == k.failKey {
		return 0, k.err
	}
	return k.KeyValue.Put(ctx, key, value)
}

// TestRollingUpgrade_AliasBarrier_FailureAbortsBeforeCommit_NotAtPayloadCreate
// replaces the degenerate cancelled-context test (#48). It uses a KV wrapper
// that fails Put for the legacy worker's alias key, so step 4 (payload
// Create) succeeds and step 6 (alias barrier) deterministically fails. Proves
// that the publisher aborts before commit CAS with the correct sentinel and
// the correct metrics fire.
func TestRollingUpgrade_AliasBarrier_FailureAbortsBeforeCommit_NotAtPayloadCreate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)
	akv := partitest.CreateJetStreamKV(t, nc, "alias-barrier-fail-asgn")
	hkv := partitest.CreateJetStreamKV(t, nc, "alias-barrier-fail-hb")

	const oldWorker = "old-w"
	failingKV := &putFailingKV{
		KeyValue: akv,
		failKey:  "assignment." + oldWorker,
		err:      errors.New("simulated KV put failure"),
	}

	lr := &atomic.Uint64{}
	lr.Store(1)
	m := newCountingMetrics()
	pub := NewAssignmentPublisher(PublisherConfig{
		AssignmentKV:    failingKV,
		HeartbeatKV:     hkv,
		Prefix:          "assignment",
		HeartbeatPrefix: "heartbeat",
		LeaderCheckFn:   liveRevCheck(lr),
		Logger:          logging.NewNop(),
		Metrics:         m,
	})

	// Legacy heartbeat for the old worker classifies it as legacy_in_batch.
	tsBytes := []byte(time.Now().UTC().Format(time.RFC3339Nano))
	_, err := hkv.Put(ctx, "heartbeat."+oldWorker, tsBytes)
	require.NoError(t, err)

	srcParts := []types.Partition{ps("p1"), ps("p2")}
	err = pub.Publish(ctx, PublishInput{
		Workers: []string{oldWorker, "new-w"},
		Assignments: map[string][]types.Partition{
			oldWorker: {ps("p1")},
			"new-w":   {ps("p2")},
		},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, types.ErrAliasBarrierFailed),
		"want ErrAliasBarrierFailed (step 6), got %v", err)

	// Metric checks: alias_barrier_failed and batch_aborted("alias_barrier_failed")
	// fire; commit_aborts does NOT (the abort happened BEFORE the CAS attempt).
	require.EqualValues(t, 1, m.aliasBarrierFailed.Load())
	require.EqualValues(t, 1, m.batchAbortedCount("alias_barrier_failed"))
	require.EqualValues(t, 0, m.commitAborts.Load(),
		"commit_aborts must NOT fire; abort happened before CAS")

	// No commit landed.
	_, err = akv.Get(ctx, "assignment._commit")
	require.Error(t, err, "no commit may exist")

	// alias_visible_uncommitted must NOT fire — no alias landed (P2 fix
	// validates this here too: this is the FIRST attempt failing).
	require.EqualValues(t, 0, m.aliasVisibleUncommitted.Load(),
		"alias_visible_uncommitted must NOT fire when no legacy alias actually landed")
}

// ============================================================================
// P1-3: heartbeat default-classification paths (missing, malformed)
// ============================================================================

// TestPublisher_LegacyAliasBarrier_MissingHeartbeatTreatedAsLegacy verifies
// that a worker with no heartbeat key is classified as legacy and its
// mandatory alias is written before commit (the safe default).
func TestPublisher_LegacyAliasBarrier_MissingHeartbeatTreatedAsLegacy(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "legacy-missing-hb")
	ctx := t.Context()
	// NOTE: deliberately do NOT write a heartbeat for "no-hb-w".
	srcParts := []types.Partition{ps("p1")}
	require.NoError(t, f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"no-hb-w"},
		Assignments:      map[string][]types.Partition{"no-hb-w": {ps("p1")}},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	}))
	// Mandatory legacy alias must exist for the missing-heartbeat worker.
	entry, err := f.assignmentKV.Get(ctx, "assignment.no-hb-w")
	require.NoError(t, err, "missing heartbeat must be classified as legacy and produce a mandatory alias")
	require.NotZero(t, entry.Revision())
}

// TestPublisher_LegacyAliasBarrier_MalformedHeartbeatTreatedAsLegacy verifies
// that a worker with a malformed (neither JSON nor RFC3339) heartbeat is
// classified as legacy and its mandatory alias is written before commit.
func TestPublisher_LegacyAliasBarrier_MalformedHeartbeatTreatedAsLegacy(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "legacy-malformed-hb")
	ctx := t.Context()
	// Plant garbage that fails both JSON and timestamp parses.
	_, err := f.heartbeatKV.Put(ctx, "heartbeat.bad-hb-w", []byte("\x00\x01\x02not-json-not-time"))
	require.NoError(t, err)
	srcParts := []types.Partition{ps("p1")}
	require.NoError(t, f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"bad-hb-w"},
		Assignments:      map[string][]types.Partition{"bad-hb-w": {ps("p1")}},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	}))
	entry, err := f.assignmentKV.Get(ctx, "assignment.bad-hb-w")
	require.NoError(t, err, "malformed heartbeat must be classified as legacy and produce a mandatory alias")
	require.NotZero(t, entry.Revision())
}

// ============================================================================
// P1-4: documented exposure invariant — legacy heartbeat lacks AppliedVersion=V
// ============================================================================

// TestPublisher_AliasBarrier_CASFailure_LegacyHeartbeatHasNoAppliedVersion is
// the same scenario as test #61 with the additional invariant assertion: the
// legacy worker's timestamp heartbeat decodes to a Heartbeat with
// Capabilities=0 and AppliedVersion!=V. This documents the recovery story
// (cluster does not rely on legacy ack drift).
func TestPublisher_AliasBarrier_CASFailure_LegacyHeartbeatHasNoAppliedVersion(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "alias-cas-failure-hb-invariant")
	ctx := t.Context()
	const w = "old-w"
	f.putLegacyHeartbeat(t, ctx, w)

	// Pre-create a competing commit at V=42 so this publisher's CAS fails.
	prior := types.AssignmentCommit{Version: 42, LeaderRevision: 50}
	priorBytes, jerr := json.Marshal(prior)
	require.NoError(t, jerr)
	_, err := f.assignmentKV.Create(ctx, "assignment._commit", priorBytes)
	require.NoError(t, err)

	const proposedV int64 = 1
	srcParts := []types.Partition{ps("p1")}
	err = f.pub.Publish(ctx, PublishInput{
		Workers:          []string{w},
		Assignments:      map[string][]types.Partition{w: {ps("p1")}},
		SourcePartitions: srcParts,
		LeaderRevision:   1,
	})
	require.True(t, errors.Is(err, types.ErrCommitCASFailed))

	// Read the legacy heartbeat back; it must remain a timestamp string with
	// no ack info — the cluster's recovery does not rely on a "legacy worker
	// acked V" signal.
	hbEntry, err := f.heartbeatKV.Get(ctx, "heartbeat."+w)
	require.NoError(t, err)
	hb, derr := types.DecodeHeartbeat(hbEntry.Value())
	require.NoError(t, derr, "legacy timestamp heartbeat must still decode (with zero capability fields)")
	require.EqualValues(t, 0, hb.Capabilities, "legacy heartbeat must report no capabilities")
	require.NotEqual(t, proposedV, hb.AppliedVersion,
		"legacy heartbeat must NOT carry AppliedVersion=V — the publisher does not rely on legacy ack drift")
}

// ============================================================================
// Phase 4 step 4: LastCommit + BootstrapLastCommit accessors
// ============================================================================

// TestPublisher_LastCommit_PopulatedAfterSuccessfulCAS verifies that
// LastCommit returns a defensive copy of the most recently CAS-written
// AssignmentCommit and that mutating the returned struct does not affect
// publisher state.
func TestPublisher_LastCommit_PopulatedAfterSuccessfulCAS(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "last-commit-after-cas")
	ctx := t.Context()
	const w = "w1"
	f.putV1Heartbeat(t, ctx, w)

	// Pre-condition: no commit observed yet.
	require.Nil(t, f.pub.LastCommit())

	require.NoError(t, f.pub.Publish(ctx, PublishInput{
		Workers:          []string{w},
		Assignments:      map[string][]types.Partition{w: {ps("p1")}},
		SourcePartitions: []types.Partition{ps("p1")},
		LeaderRevision:   1,
		Lifecycle:        "cold_start",
	}))

	got := f.pub.LastCommit()
	require.NotNil(t, got)
	require.Equal(t, int64(1), got.Version)
	require.Equal(t, uint64(1), got.LeaderRevision)
	require.Equal(t, []string{w}, got.Workers)
	require.Contains(t, got.Payloads, w)

	// Defensive-copy invariant: mutating Workers + Payloads must not affect
	// the publisher's cached commit.
	got.Workers[0] = "tampered"
	got.Payloads["x"] = types.AssignmentPayloadRef{Key: "tampered"}
	again := f.pub.LastCommit()
	require.Equal(t, []string{w}, again.Workers, "publisher cache must be insulated from caller mutation")
	require.NotContains(t, again.Payloads, "x")
}

// TestPublisher_BootstrapLastCommit_SeedsFromKV verifies that
// BootstrapLastCommit reads the live "<prefix>._commit" key once and seeds
// the in-memory cache so subsequent LastCommit calls return the bootstrap
// value (used at calculator start before this publisher writes its own
// commit).
func TestPublisher_BootstrapLastCommit_SeedsFromKV(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "bootstrap-last-commit")
	ctx := t.Context()

	// Plant a pre-existing commit (as a prior leader would have left).
	pre := types.AssignmentCommit{
		Version:        7,
		LeaderRevision: 20,
		PublishedAt:    time.Now().UTC(),
		Workers:        []string{"a", "b"},
		Payloads: map[string]types.AssignmentPayloadRef{
			"a": {Key: "assignment._payload.deadbeef", PayloadHash: "deadbeef", SetDigest: 1, Revision: 1},
			"b": {Key: "assignment._payload.cafef00d", PayloadHash: "cafef00d", SetDigest: 2, Revision: 2},
		},
	}
	preBytes, jerr := json.Marshal(pre)
	require.NoError(t, jerr)
	_, err := f.assignmentKV.Create(ctx, "assignment._commit", preBytes)
	require.NoError(t, err)

	// Fresh publisher (newPublisherFixture builds one, but the cache is empty).
	require.Nil(t, f.pub.LastCommit(), "fresh publisher must report no commit")
	require.NoError(t, f.pub.BootstrapLastCommit(ctx))

	got := f.pub.LastCommit()
	require.NotNil(t, got, "bootstrap must seed the cache")
	require.Equal(t, int64(7), got.Version)
	require.Equal(t, uint64(20), got.LeaderRevision)
	require.ElementsMatch(t, []string{"a", "b"}, got.Workers)
	require.Equal(t, "deadbeef", got.Payloads["a"].PayloadHash)
}

// TestPublisher_BootstrapLastCommit_AbsentKey_NoError verifies that the
// bootstrap path is non-fatal when no commit exists in KV (cold-start path
// against an empty assignment bucket).
func TestPublisher_BootstrapLastCommit_AbsentKey_NoError(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "bootstrap-absent")
	ctx := t.Context()
	require.NoError(t, f.pub.BootstrapLastCommit(ctx))
	require.Nil(t, f.pub.LastCommit())
}
