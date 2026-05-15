package assignment

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

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
