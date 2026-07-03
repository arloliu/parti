package handoff

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// ClaimStore provides CAS-based claim updates over NATS KV.
type ClaimStore interface {
	// Get retrieves the current claim and its revision for a partition.
	//
	// Parameters:
	//   - ctx: Context for cancellation.
	//   - partitionID: The ID of the partition to get the claim for.
	//
	// Returns:
	//   - Claim: The current claim for the partition. If no claim exists, returns an empty Claim.
	//   - uint64: The revision number of the claim in the KV store. Zero if no claim exists.
	//   - error: Any error encountered during the operation.
	//
	// If the partition has no existing claim, the returned Claim will be empty
	// and the revision will be zero.
	Get(ctx context.Context, partitionID string) (Claim, uint64, error)

	// PutIfEpoch updates the claim for a partition if the current epoch matches expectedEpoch.
	//
	// Parameters:
	//   - ctx: Context for cancellation.
	//   - partitionID: The ID of the partition to update the claim for.
	//   - expectedEpoch: The expected current epoch of the claim. The update will only proceed if the current epoch matches this value.
	//   - next: The new Claim to set for the partition.
	//
	// Returns:
	//   - uint64: The new revision number of the claim in the KV store after the update.
	//   - error: Any error encountered during the operation, including epoch mismatches.
	//
	// If the current epoch of the claim does not match expectedEpoch, an error is returned
	// and the claim is not updated.
	PutIfEpoch(ctx context.Context, partitionID string, expectedEpoch int64, next Claim) (uint64, error)

	// ListKeys lists all partition IDs with claims.
	//
	// Parameters:
	//   - ctx: Context for cancellation.
	//
	// Returns:
	//   - []string: A slice of partition IDs that have claims.
	//   - error: Any error encountered during the operation.
	//
	// The returned partition IDs correspond to the keys in the KV store
	// under the claims prefix.
	ListKeys(ctx context.Context) ([]string, error)

	// Delete removes a partition's claim, but only if the claim is still at
	// the given revision (compare-and-delete). A revision mismatch — the
	// claim was transitioned or re-created since the caller read it — must
	// fail the delete and return an error, so a concurrent legitimate
	// writer always wins over a deleter working from a stale read.
	//
	// Parameters:
	//   - ctx: Context for cancellation.
	//   - partitionID: The ID of the partition whose claim to delete.
	//   - revision: The KV revision the caller read the claim at.
	//
	// Returns:
	//   - error: Any error encountered, including revision mismatches.
	Delete(ctx context.Context, partitionID string, revision uint64) error
}

// natsClaimStore implements ClaimStore using a jetstream.KeyValue bucket.
type natsClaimStore struct {
	kv         jetstream.KeyValue
	claimsPref string // e.g., "claims/"
	probeKV    jetstream.KeyValue
}

// buildNATSClaimStore builds the concrete store, normalizing the claims
// prefix to a trailing slash. Shared by both exported constructors.
func buildNATSClaimStore(kv jetstream.KeyValue, prefix string) *natsClaimStore {
	p := prefix
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return &natsClaimStore{kv: kv, claimsPref: p}
}

// NewNATSClaimStore constructs a ClaimStore.
func NewNATSClaimStore(kv jetstream.KeyValue, prefix string) ClaimStore {
	return buildNATSClaimStore(kv, prefix)
}

// NewNATSClaimStoreWithProbe constructs a ClaimStore whose sweep gate
// can probe the backing stream position. probeKV MUST be a dedicated
// handle for the same bucket, used only by the sweep goroutine's
// probes — never the handle production reads/writes go through
// (kv.Status mutates shared *stream state under concurrent Get/Put;
// the epoch-monitor race class). A nil probeKV yields the same
// (ungated) behavior as NewNATSClaimStore.
func NewNATSClaimStoreWithProbe(kv, probeKV jetstream.KeyValue, prefix string) ClaimStore {
	store := buildNATSClaimStore(kv, prefix)
	store.probeKV = probeKV

	return store
}

func (s *natsClaimStore) key(partitionID string) string { return s.claimsPref + partitionID }

func (s *natsClaimStore) Get(ctx context.Context, partitionID string) (Claim, uint64, error) {
	cl, rev, err := kvutil.GetJSON[Claim](ctx, s.kv, s.key(partitionID))
	if err != nil {
		return Claim{}, 0, fmt.Errorf("kv get: %w", err)
	}
	if cl == nil {
		return Claim{}, 0, nil
	}

	return *cl, rev, nil
}

func (s *natsClaimStore) PutIfEpoch(ctx context.Context, partitionID string, expectedEpoch int64, next Claim) (uint64, error) {
	b, err := next.Marshal()
	if err != nil {
		return 0, err
	}
	// Fetch latest to compare epoch and revision for CAS
	cl, rev, err := kvutil.GetJSON[Claim](ctx, s.kv, s.key(partitionID))
	if err != nil {
		return 0, fmt.Errorf("kv get for cas: %w", err)
	}

	if cl == nil {
		// Create only if expectedEpoch == 0
		if expectedEpoch != 0 {
			return 0, fmt.Errorf("epoch mismatch: expected %d on create", expectedEpoch)
		}
		rev, perr := s.kv.Create(ctx, s.key(partitionID), b)
		if perr != nil {
			return 0, fmt.Errorf("kv create: %w", perr)
		}

		return rev, nil
	}

	if cl.Epoch != expectedEpoch {
		return 0, fmt.Errorf("epoch conflict: have %d, expected %d", cl.Epoch, expectedEpoch)
	}
	rev, err = s.kv.Update(ctx, s.key(partitionID), b, rev)
	if err != nil {
		return 0, fmt.Errorf("kv update: %w", err)
	}

	return rev, nil
}

func (s *natsClaimStore) ListKeys(ctx context.Context) ([]string, error) {
	return kvutil.ListKeys(ctx, s.kv, s.claimsPref, true)
}

func (s *natsClaimStore) Delete(ctx context.Context, partitionID string, revision uint64) error {
	// LastRevision makes this a compare-and-delete: NATS rejects the delete
	// (ErrKeyExists wrap) when the key has moved past the given revision.
	if err := s.kv.Delete(ctx, s.key(partitionID), jetstream.LastRevision(revision)); err != nil {
		return fmt.Errorf("kv delete: %w", err)
	}

	return nil
}

// BucketPos reports the position of the stream backing the claims
// bucket via one read-only STREAM.INFO request on the dedicated probe
// handle — the optional bucketPosProber capability the sweep gate
// activates on. Prefix scoping does not matter here: any mutation
// anywhere in the bucket advances the position, which is exactly the
// conservative signal the gate needs. Without a probe handle it
// returns errNoProbeHandle, which permanently disables the gate for
// this coordinator (fail open into full sweeps).
func (s *natsClaimStore) BucketPos(ctx context.Context) (natsutil.KVStreamPos, error) {
	if s.probeKV == nil {
		return natsutil.KVStreamPos{}, errNoProbeHandle
	}

	return natsutil.ProbeKVStreamPos(ctx, s.probeKV)
}

// Now returns time.Now() and allows override in tests.
type Now func() time.Time
