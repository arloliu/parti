package types

import "time"

// AssignmentSchemaVersion is the wire-format version for AssignmentPayload.
//
// Bump only on breaking layout changes; additive fields should use
// json:",omitempty" instead so old workers can still decode payloads written
// by newer leaders.
const AssignmentSchemaVersion uint8 = 1

// AssignmentPayload contains the per-worker partition slice that is hashed and
// stored content-addressably in the assignment KV bucket.
//
// AssignmentPayload deliberately holds ONLY stable per-worker content. Version,
// leader, source, and lifecycle metadata live on AssignmentCommit so identical
// partition slices across versions hash to the same key (cross-commit reuse).
//
// The payload is serialized in canonical form before hashing:
//   - Partitions are sorted by Partition.CanonicalID().
//   - Encoded with stdlib encoding/json (no maps, so output is deterministic).
//
// The sha256 hash of the canonical bytes is the payload's content address; it
// becomes the key suffix at "assignment._payload.<hex(sha256)>".
type AssignmentPayload struct {
	// SchemaVersion identifies the AssignmentPayload wire format. Always 1 for
	// the current generation; readers may use this to gate decoding.
	SchemaVersion uint8 `json:"schema_version"`

	// Partitions is the worker's assigned partition slice, sorted by
	// CanonicalID. Sort is the canonicalization step that lets
	// content-addressable keys reuse across commits.
	Partitions []Partition `json:"partitions"`
}

// AssignmentPayloadRef points to a content-addressable AssignmentPayload key in
// the assignment KV bucket.
//
// The PayloadHash is authoritative for content identity and matches the Key's
// hex(sha256) suffix. SetDigest is an audit/metrics aid only — never use it for
// content identity.
//
// Example:
//
//	ref := AssignmentPayloadRef{
//	    Key:         "assignment._payload.0123abcd...",
//	    PayloadHash: "0123abcd...",
//	    SetDigest:   0xdeadbeefcafef00d,
//	    Revision:    42,
//	}
type AssignmentPayloadRef struct {
	// Key is the full KV key of the payload, e.g. "assignment._payload.<hex(sha256)>".
	Key string `json:"key"`

	// PayloadHash is the hex(sha256) of the canonical AssignmentPayload bytes.
	// Authoritative — readers MUST verify this against the fetched bytes.
	PayloadHash string `json:"payload_hash"`

	// SetDigest is xxh3 over the sorted partition CanonicalIDs in the payload.
	// Diagnostic / audit only. Never used for content identity.
	SetDigest uint64 `json:"set_digest"`

	// Revision is the KV revision of the payload key at the time of Create or
	// Get. Diagnostic only.
	Revision uint64 `json:"revision"`
}

// AssignmentCommit is the single atomic decision point for an assignment
// version. It is written CAS-fenced to "assignment._commit" by the publisher
// and watched by workers.
//
// The commit references payloads by content-addressable AssignmentPayloadRef
// instead of inlining partition slices: this keeps the commit blob small and
// guarantees the payload bytes referenced by a committed commit cannot be
// mutated by a losing leader (immutability via kv.Create + sha256 verify on
// ErrKeyExists).
//
// Workers fetch their own AssignmentPayloadRef from Payloads[workerID], read
// the referenced payload key, verify the bytes against PayloadHash, then apply
// the partitions through the receipt path.
type AssignmentCommit struct {
	// Version is the monotonically-increasing assignment version.
	Version int64 `json:"version"`

	// LeaderRevision is the term-epoch revision of the leader that wrote this
	// commit. Workers fence stale-leader commits using this value.
	LeaderRevision uint64 `json:"leader_revision"`

	// SourceRevision is the partition source's KV revision at snapshot time.
	// Zero (and SourceRevisionKnown=false) when the source is non-revisioned.
	SourceRevision uint64 `json:"source_revision,omitempty"`

	// SourceRevisionKnown is true when SourceRevision was observed from a
	// RevisionedPartitionSource. Audit logic uses this to decide whether
	// strict source-revision checks apply for receipts under this commit.
	SourceRevisionKnown bool `json:"source_revision_known,omitempty"`

	// PublishedAt is the wall-clock time the commit was constructed (not when
	// the CAS write completed). Diagnostic only.
	PublishedAt time.Time `json:"published_at"`

	// Workers is the sorted list of worker IDs the commit assigns. A worker
	// not present here is implicitly revoked at this version.
	Workers []string `json:"workers"`

	// Payloads maps worker ID to the AssignmentPayloadRef the worker must
	// fetch and apply. Workers that appear in Workers must have a Payloads
	// entry; absence is a malformed-commit signal.
	Payloads map[string]AssignmentPayloadRef `json:"payloads"`

	// BatchDigest is xxh3 over all sorted partition CanonicalIDs in the batch.
	// Used as a coverage-proof handle for audit; matches the source partition
	// set's digest at publish time.
	BatchDigest uint64 `json:"batch_digest"`

	// PrevCommitRev is the KV revision the publisher CAS-updated FROM. Zero on
	// the first ever commit (Create instead of Update). Diagnostic only —
	// does not participate in correctness checks once written.
	PrevCommitRev uint64 `json:"prev_commit_rev,omitempty"`

	// Lifecycle is the rebalance reason (cold_start, planned_scale, emergency,
	// restart). Diagnostic only.
	Lifecycle string `json:"lifecycle,omitempty"`
}

// AssignmentCommitLog is an immutable per-version log entry written to
// "assignment._commit_log.<V>". It records the payload keys the commit
// referenced so the GC pass can compute the live payload set without rereading
// every commit-historical blob.
//
// AssignmentCommitLog is consumed by GC and operators only. Workers do not
// read it.
type AssignmentCommitLog struct {
	// Version mirrors AssignmentCommit.Version.
	Version int64 `json:"version"`

	// LeaderRevision mirrors AssignmentCommit.LeaderRevision.
	LeaderRevision uint64 `json:"leader_revision"`

	// PublishedAt mirrors AssignmentCommit.PublishedAt.
	PublishedAt time.Time `json:"published_at"`

	// PayloadKeys is the sorted list of payload key names the commit
	// referenced. GC must retain every key in this set.
	PayloadKeys []string `json:"payload_keys"`
}
