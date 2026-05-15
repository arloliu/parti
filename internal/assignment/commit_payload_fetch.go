package assignment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// Typed sentinels for the worker-side commit-path payload verification
// (§3.6 case (c)). Callers should errors.Is-switch on these to pick
// stage-specific metrics.
var (
	// ErrCommitPayloadFetch indicates the payload key referenced by the
	// commit could not be fetched from KV.
	ErrCommitPayloadFetch = errors.New("commit payload fetch failed")

	// ErrCommitPayloadDecompress indicates the payload bytes failed gzip
	// decompression.
	ErrCommitPayloadDecompress = errors.New("commit payload gzip decompression failed")

	// ErrCommitPayloadHashMismatch indicates the sha256 of the
	// decompressed bytes did not match ref.PayloadHash. This is either a
	// hash collision (extraordinary) or KV corruption.
	ErrCommitPayloadHashMismatch = errors.New("commit payload hash mismatch")

	// ErrCommitPayloadDecode indicates the canonical bytes failed JSON
	// decoding into AssignmentPayload.
	ErrCommitPayloadDecode = errors.New("commit payload JSON decode failed")

	// ErrCommitPayloadDigestMismatch indicates the set digest computed
	// over the decoded partitions did not match ref.SetDigest. The
	// PayloadHash already matched, so this is an internal-consistency
	// failure rather than a transport corruption.
	ErrCommitPayloadDigestMismatch = errors.New("commit payload set-digest mismatch")
)

// FetchAndVerifyCommitPayload fetches the payload key referenced by ref
// from kv, gzip-decompresses, sha256-verifies the bytes against
// ref.PayloadHash, JSON-decodes into AssignmentPayload, and verifies the
// computed set digest against ref.SetDigest.
//
// On success, returns the decoded payload. On failure, returns a wrapped
// typed sentinel from this package — callers should errors.Is-switch on
// the sentinels to pick stage-specific metrics. After a failure the
// caller should leave lastSeenLeaderRevision and any pending state
// unchanged (case (c) retry-on-next-tick semantics).
//
// Parameters:
//   - ctx: Context for the KV Get operation.
//   - kv: Assignment KV bucket carrying the payload key.
//   - ref: AssignmentPayloadRef from a commit's Payloads map.
//
// Returns:
//   - types.AssignmentPayload: Decoded and verified payload on success.
//   - error: Wrapped typed sentinel on any verification failure.
func FetchAndVerifyCommitPayload(
	ctx context.Context,
	kv jetstream.KeyValue,
	ref types.AssignmentPayloadRef,
) (types.AssignmentPayload, error) {
	entry, err := kv.Get(ctx, ref.Key)
	if err != nil {
		return types.AssignmentPayload{}, fmt.Errorf("%w: key=%s: %w", ErrCommitPayloadFetch, ref.Key, err)
	}
	stored := entry.Value()

	// Strict gzip decompression per §3.6 case (c) state machine: the
	// publisher writes gzip-compressed payloads, and the worker MUST
	// reject any payload that fails gzip decode (drop transition,
	// RecordPayloadDecompressError). Tolerating uncompressed bytes would
	// silently accept payloads that bypass the publisher's compression
	// invariant and would not surface the decompression failure metric
	// to operators.
	plain, derr := gzipDecompress(stored)
	if derr != nil {
		return types.AssignmentPayload{}, fmt.Errorf("%w: key=%s: %w", ErrCommitPayloadDecompress, ref.Key, derr)
	}

	gotHash := sha256.Sum256(plain)
	gotHex := hex.EncodeToString(gotHash[:])
	if gotHex != ref.PayloadHash {
		return types.AssignmentPayload{}, fmt.Errorf(
			"%w: key=%s expected=%s got=%s",
			ErrCommitPayloadHashMismatch, ref.Key, ref.PayloadHash, gotHex,
		)
	}

	var payload types.AssignmentPayload
	if jerr := json.Unmarshal(plain, &payload); jerr != nil {
		return types.AssignmentPayload{}, fmt.Errorf("%w: key=%s: %w", ErrCommitPayloadDecode, ref.Key, jerr)
	}

	if types.PartitionSetDigest(payload.Partitions) != ref.SetDigest {
		return types.AssignmentPayload{}, fmt.Errorf(
			"%w: key=%s expected=%d got=%d",
			ErrCommitPayloadDigestMismatch, ref.Key, ref.SetDigest, types.PartitionSetDigest(payload.Partitions),
		)
	}

	return payload, nil
}
