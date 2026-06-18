package assignment

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// --- merged from commit_gc_test.go ---

// TestPublisher_PayloadGC_DoesNotDeleteCurrentCommitPayloads (#26 — F1 architect) —
// Run several publishes; trigger a GC pass; assert the current commit's
// payloads and the last K commit_log payloads are retained; assert older
// orphan payloads outside the retention window are deleted.
func TestPublisher_PayloadGC_DoesNotDeleteCurrentCommitPayloads(t *testing.T) {
	f := newPublisherFixture(t, "gc-keeps-live")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w1")
	f.putV1Heartbeat(t, ctx, "w2")

	// Publish three commits with progressively different slices.
	publish := func(w1, w2 string) {
		err := f.pub.Publish(ctx, PublishInput{
			Workers: []string{"w1", "w2"},
			Assignments: map[string][]types.Partition{
				"w1": {ps(w1)},
				"w2": {ps(w2)},
			},
			SourcePartitions: []types.Partition{ps(w1), ps(w2)},
			LeaderRevision:   1,
		})
		require.NoError(t, err)
	}
	publish("a", "b")
	publish("c", "d")
	publish("e", "f")

	// Plant an orphan payload key whose age is old enough to be eligible for
	// deletion. Because the test is in-process, we can't easily backdate
	// jetstream Created times — instead we inject a "now" much later than
	// real time via the GC's clock injection.
	orphanCanonical := mustMarshalCanonicalPayload(t, types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    []types.Partition{ps("orphan")},
	})
	orphanHash := sha256.Sum256(orphanCanonical)
	orphanKey := "assignment._payload." + hex.EncodeToString(orphanHash[:])
	gz, err := gzipCompress(orphanCanonical)
	require.NoError(t, err)
	_, err = f.assignmentKV.Create(ctx, orphanKey, gz)
	require.NoError(t, err)

	// Configure GC with KeepCommits=10 (so all 3 commit_logs are retained)
	// and a Now() that's well past the retention window so the orphan is
	// eligible for deletion.
	gc := NewCommitGC(CommitGCConfig{
		Publisher:   f.pub,
		Interval:    time.Hour,
		Retention:   1 * time.Second,
		KeepCommits: 10,
		Now:         func() time.Time { return time.Now().Add(48 * time.Hour) },
		Metrics:     f.metrics,
	})
	require.NotNil(t, gc)
	require.NoError(t, gc.RunOnce(ctx))

	// The orphan must be gone.
	_, err = f.assignmentKV.Get(ctx, orphanKey)
	require.Error(t, err, "orphan payload outside retention window must be deleted")

	// All current-commit payloads must be retained.
	commit := f.readCommit(t, ctx)
	require.NotNil(t, commit)
	for w, ref := range commit.Payloads {
		_, err = f.assignmentKV.Get(ctx, ref.Key)
		require.NoError(t, err, "current-commit payload for %s must be retained", w)
	}
}

// TestCommitGC_RetentionWindowProtectsRecentOrphans — orphan younger than
// Retention is NOT deleted even though it's not live.
func TestCommitGC_RetentionWindowProtectsRecentOrphans(t *testing.T) {
	f := newPublisherFixture(t, "gc-retention-window")
	ctx := context.Background()
	f.putV1Heartbeat(t, ctx, "w1")
	require.NoError(t, f.pub.Publish(ctx, PublishInput{
		Workers: []string{"w1"}, Assignments: map[string][]types.Partition{"w1": {ps("p")}},
		SourcePartitions: []types.Partition{ps("p")}, LeaderRevision: 1,
	}))

	// Plant an orphan; GC.Now() is "now" so the orphan is well within the
	// 24h default retention.
	orphanCanonical := mustMarshalCanonicalPayload(t, types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    []types.Partition{ps("orphan-recent")},
	})
	orphanHash := sha256.Sum256(orphanCanonical)
	orphanKey := "assignment._payload." + hex.EncodeToString(orphanHash[:])
	gz, _ := gzipCompress(orphanCanonical)
	_, err := f.assignmentKV.Create(ctx, orphanKey, gz)
	require.NoError(t, err)

	gc := NewCommitGC(CommitGCConfig{
		Publisher: f.pub,
		Now:       time.Now, // no skew
	})
	require.NoError(t, gc.RunOnce(ctx))

	// Orphan still present.
	_, err = f.assignmentKV.Get(ctx, orphanKey)
	require.NoError(t, err, "recent orphan must be protected by Retention")
}

// TestCommitGC_NoCommit_NoOpSweep — running GC against an empty bucket is a
// no-op (no errors, no deletions).
func TestCommitGC_NoCommit_NoOpSweep(t *testing.T) {
	f := newPublisherFixture(t, "gc-empty-bucket")
	ctx := context.Background()
	gc := NewCommitGC(CommitGCConfig{Publisher: f.pub})
	require.NoError(t, gc.RunOnce(ctx))
	// metric should not have fired.
	require.EqualValues(t, 0, f.metrics.gcDeleteErrors.Load())
}

// TestCommitGC_DefaultsApplied — NewCommitGC fills in zero-valued config
// fields with the documented defaults.
func TestCommitGC_DefaultsApplied(t *testing.T) {
	f := newPublisherFixture(t, "gc-defaults")
	gc := NewCommitGC(CommitGCConfig{Publisher: f.pub})
	require.NotNil(t, gc)
	require.Equal(t, DefaultPayloadGCInterval, gc.cfg.Interval)
	require.Equal(t, DefaultPayloadGCRetention, gc.cfg.Retention)
	require.Equal(t, DefaultPayloadGCKeepCommits, gc.cfg.KeepCommits)
}

// --- merged from commit_state_machine_test.go ---

// TestFetchAndVerifyCommitPayload_FullChainSucceeds verifies the happy path:
// well-formed gzip-compressed canonical bytes whose sha256 matches the
// ref.PayloadHash and whose decoded partitions yield the expected SetDigest.
func TestFetchAndVerifyCommitPayload_FullChainSucceeds(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "fetch-verify-ok")
	ctx := t.Context()

	parts := []types.Partition{
		{Keys: []string{"p1"}},
		{Keys: []string{"p2"}},
	}
	payload := types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    parts,
	}
	canonical, jerr := json.Marshal(payload)
	require.NoError(t, jerr)
	hash := sha256.Sum256(canonical)
	hashHex := hex.EncodeToString(hash[:])
	key := "assignment._payload." + hashHex

	gz := mustGzip(t, canonical)
	_, err := kv.Create(ctx, key, gz)
	require.NoError(t, err)

	ref := types.AssignmentPayloadRef{
		Key:         key,
		PayloadHash: hashHex,
		SetDigest:   types.PartitionSetDigest(parts),
	}

	got, err := FetchAndVerifyCommitPayload(ctx, kv, ref)
	require.NoError(t, err)
	require.Equal(t, parts, got.Partitions)
}

func TestFetchAndVerifyCommitPayload_FetchError(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "fetch-verify-missing")
	ctx := t.Context()

	ref := types.AssignmentPayloadRef{
		Key:         "assignment._payload.nonexistent",
		PayloadHash: "x",
		SetDigest:   0,
	}
	_, err := FetchAndVerifyCommitPayload(ctx, kv, ref)
	require.ErrorIs(t, err, ErrCommitPayloadFetch)
}

func TestFetchAndVerifyCommitPayload_HashMismatch(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "fetch-verify-hash")
	ctx := t.Context()

	parts := []types.Partition{{Keys: []string{"p1"}}}
	payload := types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    parts,
	}
	canonical, _ := json.Marshal(payload)
	hash := sha256.Sum256(canonical)
	hashHex := hex.EncodeToString(hash[:])
	key := "assignment._payload." + hashHex

	// Plant the payload under the correct key but lie about the hash in the ref.
	_, err := kv.Create(ctx, key, mustGzip(t, canonical))
	require.NoError(t, err)

	ref := types.AssignmentPayloadRef{
		Key:         key,
		PayloadHash: "deadbeef-not-the-real-hash",
		SetDigest:   types.PartitionSetDigest(parts),
	}
	_, err = FetchAndVerifyCommitPayload(ctx, kv, ref)
	require.ErrorIs(t, err, ErrCommitPayloadHashMismatch)
}

func TestFetchAndVerifyCommitPayload_DigestMismatch(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "fetch-verify-digest")
	ctx := t.Context()

	parts := []types.Partition{{Keys: []string{"p1"}}}
	payload := types.AssignmentPayload{
		SchemaVersion: types.AssignmentSchemaVersion,
		Partitions:    parts,
	}
	canonical, _ := json.Marshal(payload)
	hash := sha256.Sum256(canonical)
	hashHex := hex.EncodeToString(hash[:])
	key := "assignment._payload." + hashHex
	_, err := kv.Create(ctx, key, mustGzip(t, canonical))
	require.NoError(t, err)

	ref := types.AssignmentPayloadRef{
		Key:         key,
		PayloadHash: hashHex,
		SetDigest:   0xFFFFFFFF, // wrong digest
	}
	_, err = FetchAndVerifyCommitPayload(ctx, kv, ref)
	require.ErrorIs(t, err, ErrCommitPayloadDigestMismatch)
}

// mustGzip is a tiny helper for tests. The publisher writes gzip-compressed
// canonical bytes; mirror that shape here.
func mustGzip(t *testing.T, in []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	require.NoError(t, err)
	_, err = w.Write(in)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	return buf.Bytes()
}
