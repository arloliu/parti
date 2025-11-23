package handoff

import (
	"encoding/json"
	"fmt"
	"time"
)

// ClaimState enumerates the logical states of a handoff claim.
// stable -> prepare -> commit -> stable
// Additional states (abort, rollback) may be introduced later.
type ClaimState string

const (
	ClaimStateStable  ClaimState = "stable"
	ClaimStatePrepare ClaimState = "prepare"
	ClaimStateCommit  ClaimState = "commit"
	ClaimStateAbort   ClaimState = "abort"   // terminal: reverted to prior owner
	ClaimStateUnknown ClaimState = "unknown" // parsing fallback
)

// Claim models ownership and in-flight handoff metadata for a single partition.
// Serialized to KV as JSON: compact & human-inspectable.
type Claim struct {
	PartitionID   string     `json:"partition_id"`
	Owner         string     `json:"owner"`
	PendingOwner  string     `json:"pending_owner,omitempty"`
	State         ClaimState `json:"state"`
	Epoch         int64      `json:"epoch"`
	LastUpdated   time.Time  `json:"last_updated"`
	TTLSeconds    int64      `json:"ttl_seconds"` // advisory TTL (not enforced automatically)
	ConflictCount int64      `json:"conflict_count,omitempty"`
}

// NewInitialClaim creates a stable claim for a partition with initial owner.
func NewInitialClaim(partitionID, owner string, now time.Time, ttl time.Duration) Claim {
	return Claim{
		PartitionID: partitionID,
		Owner:       owner,
		State:       ClaimStateStable,
		Epoch:       1,
		LastUpdated: now.UTC(),
		TTLSeconds:  int64(ttl.Seconds()),
	}
}

// Marshal serializes the claim.
func (c Claim) Marshal() ([]byte, error) { return json.Marshal(c) }

// UnmarshalClaim deserializes claim JSON.
func UnmarshalClaim(b []byte) (Claim, error) {
	var c Claim
	if err := json.Unmarshal(b, &c); err != nil {
		return Claim{}, fmt.Errorf("decode claim: %w", err)
	}
	if c.State == "" {
		c.State = ClaimStateUnknown
	}

	return c, nil
}

// Copy returns a shallow copy.
func (c Claim) Copy() Claim { return c }

// NextPrepare returns a mutated copy for entering prepare.
func (c Claim) NextPrepare(pendingOwner string, now time.Time) Claim {
	next := c.Copy()
	next.PendingOwner = pendingOwner
	next.State = ClaimStatePrepare
	next.Epoch++
	next.LastUpdated = now.UTC()
	return next
}

// NextCommit returns a mutated copy for entering commit (owner switch pending final stabilization).
func (c Claim) NextCommit(now time.Time) Claim {
	next := c.Copy()
	if next.PendingOwner != "" {
		next.Owner = next.PendingOwner
		next.PendingOwner = ""
	}
	next.State = ClaimStateCommit
	next.Epoch++
	next.LastUpdated = now.UTC()

	return next
}

// NextStable finalizes the handoff.
func (c Claim) NextStable(now time.Time) Claim {
	next := c.Copy()
	next.State = ClaimStateStable
	// Increment epoch on stabilization to preserve monotonic transitions across all state changes
	next.Epoch++
	next.LastUpdated = now.UTC()
	return next
}

// IsExpired returns whether TTL advisory has elapsed relative to provided time.
func (c Claim) IsExpired(now time.Time) bool {
	if c.TTLSeconds <= 0 {
		return false
	}
	return now.UTC().After(c.LastUpdated.Add(time.Duration(c.TTLSeconds) * time.Second))
}
