package testutil

import (
	"context"
	"encoding/json"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/internal/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// SeedHandoffStableClaim seeds a stable handoff claim for the given partition and owner.
// The claim is written under the "claims/" prefix in the specified bucket.
func SeedHandoffStableClaim(ctx context.Context, js jetstream.JetStream, bucket, partitionID, owner string, ttl time.Duration) error {
	c := parti.HandoffClaim{
		PartitionID: partitionID,
		Owner:       owner,
		State:       parti.HandoffClaimStable,
		Epoch:       1,
		LastUpdated: time.Now().UTC(),
		TTLSeconds:  int64(ttl.Seconds()),
	}

	return SeedHandoffClaim(ctx, js, bucket, c, ttl)
}

// SeedHandoffClaim seeds the provided handoff claim into the bucket under the "claims/" prefix.
// The bucket will be created with the provided TTL if it does not already exist.
func SeedHandoffClaim(ctx context.Context, js jetstream.JetStream, bucket string, claim parti.HandoffClaim, ttl time.Duration) error {
	kv, err := kvutil.EnsureKVBucket(ctx, js, bucket, ttl)
	if err != nil {
		return err
	}
	b, err := json.Marshal(claim)
	if err != nil {
		return err
	}
	_, err = kv.Put(ctx, "claims/"+claim.PartitionID, b)

	return err
}
