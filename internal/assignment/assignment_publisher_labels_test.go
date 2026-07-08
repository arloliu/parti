package assignment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// TestCheckCoverage_ParkedUnionAndDisjointness pins the widened coverage
// contract: assigned ∪ parked == source AND assigned ∩ parked == ∅. A nil
// parked set must degenerate to the pre-label set-equality check exactly.
func TestCheckCoverage_ParkedUnionAndDisjointness(t *testing.T) {
	t.Parallel()
	// checkCoverage is a pure method (no KV access); a minimal publisher with a
	// logger and metrics is sufficient.
	p := NewAssignmentPublisher(PublisherConfig{
		Logger:  logging.NewNop(),
		Metrics: newCountingMetrics(),
	})

	src := []types.Partition{
		{Keys: []string{"a"}}, {Keys: []string{"b"}, Label: "vip"},
	}
	assignedOnly := map[string][]types.Partition{"w0": {src[0]}}

	// (1) parked partition completes coverage:
	require.NoError(t, p.checkCoverage(src, assignedOnly, []types.Partition{src[1]}))

	// (2) partition missing from BOTH assignment and parked → error:
	err := p.checkCoverage(src, assignedOnly, nil)
	require.ErrorIs(t, err, types.ErrCoverageMismatch)

	// (3) partition in BOTH assignment and parked → error:
	both := map[string][]types.Partition{"w0": {src[0], src[1]}}
	err = p.checkCoverage(src, both, []types.Partition{src[1]})
	require.ErrorIs(t, err, types.ErrCoverageMismatch)

	// (4) legacy shape (no parked) unchanged:
	full := map[string][]types.Partition{"w0": {src[0]}, "w1": {src[1]}}
	require.NoError(t, p.checkCoverage(src, full, nil))
}

// TestPublish_ParkedMetadataOnCommit asserts the commit carries ParkedCount and
// ParkedDigest over the parked set only, while BatchDigest still covers the full
// source set.
func TestPublish_ParkedMetadataOnCommit(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "parked-commit-metadata")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w0")

	src := []types.Partition{ps("a"), {Keys: []string{"b"}, Label: "vip"}}
	parked := []types.Partition{src[1]}

	// Publish with one parked partition: w0 gets "a"; "b" is parked.
	err := f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"w0"},
		Assignments:      map[string][]types.Partition{"w0": {src[0]}},
		SourcePartitions: src,
		ParkedPartitions: parked,
		LeaderRevision:   1,
	})
	require.NoError(t, err)

	commit := f.readCommit(t, ctx)
	require.NotNil(t, commit)
	require.Equal(t, 1, commit.ParkedCount)
	require.Equal(t, types.PartitionSetDigest(parked), commit.ParkedDigest)
	// BatchDigest covers the FULL source set, not just the assigned subset.
	require.Equal(t, types.PartitionSetDigest(src), commit.BatchDigest)

	// A second publish that parks nothing (full coverage) must produce the same
	// BatchDigest and zero parked metadata.
	f.putV1Heartbeat(t, ctx, "w1")
	err = f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"w0", "w1"},
		Assignments:      map[string][]types.Partition{"w0": {src[0]}, "w1": {src[1]}},
		SourcePartitions: src,
		LeaderRevision:   1,
	})
	require.NoError(t, err)

	commit2 := f.readCommit(t, ctx)
	require.NotNil(t, commit2)
	require.Equal(t, 0, commit2.ParkedCount)
	require.Equal(t, uint64(0), commit2.ParkedDigest)
	require.Equal(t, commit.BatchDigest, commit2.BatchDigest,
		"BatchDigest must cover the full source set regardless of parking")
}

// TestPublish_PayloadCarriesLabelsOfRecord asserts the per-worker payload
// carries labels-of-record with an unconditional presence bit, and that the
// labels-of-record are part of the payload's content identity (hash).
func TestPublish_PayloadCarriesLabelsOfRecord(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "payload-labels-of-record")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w0")
	f.putV1Heartbeat(t, ctx, "w1")

	src := []types.Partition{ps("a"), ps("b")}
	err := f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"w0", "w1"},
		Assignments:      map[string][]types.Partition{"w0": {src[0]}, "w1": {src[1]}},
		SourcePartitions: src,
		// w0 has labels-of-record; w1 is absent from the map → nil labels but
		// still Known=true.
		WorkerLabels:   map[string][]string{"w0": {"vip"}},
		LeaderRevision: 1,
	})
	require.NoError(t, err)

	commit := f.readCommit(t, ctx)
	require.NotNil(t, commit)

	p0 := readPayload(t, ctx, f, commit.Payloads["w0"].Key)
	require.Equal(t, []string{"vip"}, p0.WorkerLabels)
	require.True(t, p0.WorkerLabelsKnown, "label-aware leader always records presence")

	p1 := readPayload(t, ctx, f, commit.Payloads["w1"].Key)
	require.Nil(t, p1.WorkerLabels, "unlabeled worker gets nil labels")
	require.True(t, p1.WorkerLabelsKnown, "presence bit is unconditional, even for unlabeled workers")

	// Labels-of-record are part of content identity: the hash of w0's payload
	// WITH labels differs from the hash the same worker/partitions would produce
	// WITHOUT labels (but still with the presence bit set).
	withoutLabels := mustMarshalCanonicalPayload(t, types.AssignmentPayload{
		SchemaVersion:     types.AssignmentSchemaVersion,
		Partitions:        []types.Partition{src[0]},
		WorkerLabelsKnown: true,
	})
	withoutHash := sha256.Sum256(withoutLabels)
	require.NotEqual(t, hex.EncodeToString(withoutHash[:]), commit.Payloads["w0"].PayloadHash,
		"labels-of-record must be part of the payload content address")
}

// TestPublish_LabelsOfRecordSortedForContentAddress asserts the publisher
// stamps labels-of-record in sorted order regardless of caller order, so two
// leaders observing the same label set produce the identical content-addressed
// payload (mirrors the partition CanonicalID sort in the same function). The
// caller's slice must not be mutated.
func TestPublish_LabelsOfRecordSortedForContentAddress(t *testing.T) {
	t.Parallel()
	f := newPublisherFixture(t, "labels-sorted-content-address")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w0")

	src := []types.Partition{ps("a")}
	unsorted := []string{"vip", "batch"} // deliberately unsorted
	err := f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"w0"},
		Assignments:      map[string][]types.Partition{"w0": {src[0]}},
		SourcePartitions: src,
		WorkerLabels:     map[string][]string{"w0": unsorted},
		LeaderRevision:   1,
	})
	require.NoError(t, err)

	c1 := f.readCommit(t, ctx)
	require.NotNil(t, c1)
	p0 := readPayload(t, ctx, f, c1.Payloads["w0"].Key)
	require.Equal(t, []string{"batch", "vip"}, p0.WorkerLabels,
		"labels-of-record must be stamped in sorted order")
	require.Equal(t, []string{"vip", "batch"}, unsorted,
		"the caller's label slice must not be mutated")

	// Cross-leader determinism: a second publish with PRE-SORTED labels must
	// produce the identical content address (payload reuse fires).
	reusedBefore := f.metrics.payloadsReused.Load()
	err = f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"w0"},
		Assignments:      map[string][]types.Partition{"w0": {src[0]}},
		SourcePartitions: src,
		WorkerLabels:     map[string][]string{"w0": {"batch", "vip"}},
		LeaderRevision:   1,
	})
	require.NoError(t, err)

	c2 := f.readCommit(t, ctx)
	require.NotNil(t, c2)
	require.Equal(t, c1.Payloads["w0"].PayloadHash, c2.Payloads["w0"].PayloadHash,
		"unsorted and pre-sorted label input must content-address identically")
	require.Greater(t, f.metrics.payloadsReused.Load(), reusedBefore,
		"the second publish must reuse the first publish's payload key")

	// Duplicate labels in the input dedupe to the same stamp (symmetric with
	// normalizeWorkerLabels: sort + compact), so a repeated label
	// content-addresses identically to the deduped set and reuses the payload.
	dupInput := []string{"vip", "batch", "vip"}
	err = f.pub.Publish(ctx, PublishInput{
		Workers:          []string{"w0"},
		Assignments:      map[string][]types.Partition{"w0": {src[0]}},
		SourcePartitions: src,
		WorkerLabels:     map[string][]string{"w0": dupInput},
		LeaderRevision:   1,
	})
	require.NoError(t, err)

	c3 := f.readCommit(t, ctx)
	require.NotNil(t, c3)
	p3 := readPayload(t, ctx, f, c3.Payloads["w0"].Key)
	require.Equal(t, []string{"batch", "vip"}, p3.WorkerLabels,
		"duplicate labels must dedupe in the stamped labels-of-record")
	require.Equal(t, []string{"vip", "batch", "vip"}, dupInput,
		"the caller's label slice must not be mutated")
	require.Equal(t, c1.Payloads["w0"].PayloadHash, c3.Payloads["w0"].PayloadHash,
		"duplicate and deduped label input must content-address identically")
}

// TestBuildLegacyAlias_CopiesLabelsOfRecord asserts the legacy alias envelope
// mirrors the payload's labels-of-record so the worker-side stale-incarnation
// guard can read them from the alias path too.
func TestBuildLegacyAlias_CopiesLabelsOfRecord(t *testing.T) {
	t.Parallel()
	payload := types.AssignmentPayload{
		SchemaVersion:     1,
		Partitions:        []types.Partition{{Keys: []string{"a"}}},
		WorkerLabels:      []string{"vip"},
		WorkerLabelsKnown: true,
	}
	alias := buildLegacyAlias(payload, 7, 3, 0, "steady", 2)
	require.Equal(t, []string{"vip"}, alias.WorkerLabels)
	require.True(t, alias.WorkerLabelsKnown)
}

// readPayload fetches, decompresses, and decodes an AssignmentPayload from its
// content-addressable KV key.
func readPayload(t *testing.T, ctx context.Context, f *publisherFixture, key string) types.AssignmentPayload {
	t.Helper()
	entry, err := f.assignmentKV.Get(ctx, key)
	require.NoError(t, err)
	plain, err := gzipDecompress(entry.Value())
	require.NoError(t, err)
	var p types.AssignmentPayload
	require.NoError(t, json.Unmarshal(plain, &p))

	return p
}
