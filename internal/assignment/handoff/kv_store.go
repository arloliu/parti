package handoff

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
}

// natsClaimStore implements ClaimStore using a jetstream.KeyValue bucket.
type natsClaimStore struct {
	kv         jetstream.KeyValue
	claimsPref string // e.g., "claims/"
}

// NewNATSClaimStore constructs a ClaimStore.
func NewNATSClaimStore(kv jetstream.KeyValue, prefix string) ClaimStore {
	p := prefix
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return &natsClaimStore{kv: kv, claimsPref: p}
}

func (s *natsClaimStore) key(partitionID string) string { return s.claimsPref + partitionID }

func (s *natsClaimStore) Get(ctx context.Context, partitionID string) (Claim, uint64, error) {
	entry, err := s.kv.Get(ctx, s.key(partitionID))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return Claim{}, 0, nil
		}
		return Claim{}, 0, fmt.Errorf("kv get: %w", err)
	}

	cl, err := UnmarshalClaim(entry.Value())
	if err != nil {
		return Claim{}, 0, err
	}

	return cl, entry.Revision(), nil
}

func (s *natsClaimStore) PutIfEpoch(ctx context.Context, partitionID string, expectedEpoch int64, next Claim) (uint64, error) {
	b, err := next.Marshal()
	if err != nil {
		return 0, err
	}
	// Fetch latest to compare epoch and revision for CAS
	entry, err := s.kv.Get(ctx, s.key(partitionID))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
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

		return 0, fmt.Errorf("kv get for cas: %w", err)
	}
	cur, err := UnmarshalClaim(entry.Value())
	if err != nil {
		return 0, err
	}
	if cur.Epoch != expectedEpoch {
		return 0, fmt.Errorf("epoch conflict: have %d, expected %d", cur.Epoch, expectedEpoch)
	}
	rev, err := s.kv.Update(ctx, s.key(partitionID), b, entry.Revision())
	if err != nil {
		return 0, fmt.Errorf("kv update: %w", err)
	}

	return rev, nil
}

func (s *natsClaimStore) ListKeys(ctx context.Context) ([]string, error) {
	keys, err := s.kv.Keys(ctx)
	if err != nil {
		return nil, fmt.Errorf("kv keys: %w", err)
	}
	if len(s.claimsPref) == 0 {
		return keys, nil
	}
	// Return partition IDs without the claims prefix to match Get/PutIfEpoch API expectations.
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if after, ok := strings.CutPrefix(k, s.claimsPref); ok {
			// Trim the prefix to return the logical partition ID
			out = append(out, after)
		}
	}

	return out, nil
}

// Now returns time.Now() and allows override in tests.
type Now func() time.Time
