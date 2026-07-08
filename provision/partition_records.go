package provision

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/arloliu/parti/v2/internal/partcodec"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// ErrPartitionBucketMissing is returned by PlanPartitions and ApplyPartitions
// when the partition-source bucket named by config does not exist live. It
// wraps ErrLiveValidation, so errors.Is(err, ErrLiveValidation) holds and the
// CLI maps it to exit code 3. The partitions subcommand never creates the
// bucket; the operator runs `partictl apply` to provision it first.
var ErrPartitionBucketMissing = fmt.Errorf("%w: partition-source bucket does not exist", ErrLiveValidation)

// PlanPartitions computes the record-level diff between the declared partition
// set (cfg.PartitionSource.Partitions) and the live contents of the
// partition-source KV key. It is read-only — it never mutates NATS.
//
// Unlike Plan, which reconciles bucket *config*, PlanPartitions reconciles the
// *contents* of the partition-source key. The reconcile policy ladder
// (warn / adopt / safe-update) does not apply: record-level behavior is
// identical under every valid policy. The policy value is still subject to the
// inherited static validation below.
//
// Algorithm:
//
//   - Run the inherited static config validation (Validate), then require a
//     non-empty cfg.PartitionSource.Partitions and run ValidatePartitionSet.
//     A malformed config fails here, before any NATS I/O, wrapping
//     ErrInvalidConfig.
//   - Open the partition-source bucket by exact name. A missing bucket returns
//     ErrPartitionBucketMissing.
//   - Read the partition-source key. A missing key (never written, deleted, or
//     purged — nats.go maps all three to ErrKeyNotFound) is treated as an
//     empty live table: the declared config is the source of truth.
//   - Diff declared vs live by CanonicalID and emit at most one
//     write-partitions action plus a partition-records drift finding.
//
// Cancellation: a cancelled ctx returns a zero-value PlanResult plus ctx.Err().
// Static config validation runs before the cancellation check, so a malformed
// config still yields ErrInvalidConfig even under a cancelled context.
func PlanPartitions(ctx context.Context, js jetstream.JetStream, cfg Config) (PlanResult, error) {
	// Inherited static config-validation boundary: rejects a bad apiVersion,
	// an empty partitionSource.bucket / .key, and an unsupported policy. It
	// runs first — before the cancellation check and any NATS I/O — so a
	// malformed config always yields ErrInvalidConfig (CLI exit 3), the same
	// boundary the bucket commands enforce.
	if err := Validate(cfg); err != nil {
		return PlanResult{}, err
	}
	if cfg.PartitionSource == nil || len(cfg.PartitionSource.Partitions) == 0 {
		return PlanResult{}, fmt.Errorf("%w: no partitions declared in partitionSource.partitions", ErrInvalidConfig)
	}
	desired := cfg.PartitionSource.Partitions
	if err := ValidatePartitionSet(desired); err != nil {
		return PlanResult{}, err
	}

	if err := ctx.Err(); err != nil {
		return PlanResult{}, err
	}

	live, err := readPartitionKey(ctx, js, cfg.PartitionSource.Bucket, cfg.PartitionSource.Key)
	if err != nil {
		if ctx.Err() != nil {
			return PlanResult{}, ctx.Err()
		}

		return PlanResult{}, err
	}

	added, removed, changed := diffPartitions(live, desired)

	out := PlanResult{
		APIVersion: APIVersionProvisionV1,
		Kind:       KindPlan,
		Actions:    []PlannedAction{},
		Drift:      []DriftFinding{},
	}

	if len(added) == 0 && len(removed) == 0 && len(changed) == 0 {
		out.Drift = append(out.Drift, DriftFinding{
			Severity: SeverityInformational,
			Kind:     KindPartitionRecords,
			Name:     cfg.PartitionSource.Bucket,
			Detail:   map[string]any{"partitions": len(desired)},
		})

		return out, nil
	}

	out.Actions = append(out.Actions, PlannedAction{
		Kind: ActionWritePartitions,
		Name: cfg.PartitionSource.Bucket,
		Resource: &WritePartitionsResource{
			Bucket:  cfg.PartitionSource.Bucket,
			Key:     cfg.PartitionSource.Key,
			Added:   added,
			Removed: removed,
			Changed: changed,
			Before:  clonePartitions(live),
			After:   clonePartitions(desired),
		},
	})
	out.Drift = append(out.Drift, DriftFinding{
		Severity: SeverityDriftMutable,
		Kind:     KindPartitionRecords,
		Name:     cfg.PartitionSource.Bucket,
		Detail: map[string]any{
			"added":   len(added),
			"removed": len(removed),
			"changed": len(changed),
		},
	})

	return out, nil
}

// readPartitionKey opens the partition-source bucket and reads + decodes the
// partition-source key. A missing bucket returns ErrPartitionBucketMissing. A
// missing key (ErrKeyNotFound — never-written, deleted, or purged are
// indistinguishable through nats.go's Get) returns an empty table and no
// error: the declared config is authoritative.
func readPartitionKey(ctx context.Context, js jetstream.JetStream, bucket, key string) ([]types.Partition, error) {
	kv, err := js.KeyValue(ctx, bucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, fmt.Errorf("%w: bucket %q not found; run `partictl apply` to provision it first",
			ErrPartitionBucketMissing, bucket)
	}
	if err != nil {
		return nil, fmt.Errorf("provision: open partition-source bucket %q: %w", bucket, err)
	}

	entry, err := kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("provision: read partition-source key %q/%q: %w", bucket, key, err)
	}

	live, err := partcodec.Decode(entry.Value())
	if err != nil {
		return nil, fmt.Errorf("provision: decode partition-source key %q/%q: %w", bucket, key, err)
	}

	return live, nil
}

// diffPartitions computes the record-level difference between the live and
// declared partition tables, keyed by CanonicalID:
//
//   - added   — declared records absent from live.
//   - removed — live records absent from the declared set.
//   - changed — records present in both whose Weight or Label differs.
//
// A record whose key set differs is a distinct partition: it appears as one
// add and one remove, never a change. Every returned slice is non-nil and
// sorted by CanonicalID so plan output is deterministic. All returned records
// are deep clones of the inputs.
func diffPartitions(live, desired []types.Partition) (added, removed []types.Partition, changed []PartitionWeightChange) {
	added = []types.Partition{}
	removed = []types.Partition{}
	changed = []PartitionWeightChange{}

	liveByID := make(map[string]types.Partition, len(live))
	for _, p := range live {
		liveByID[p.CanonicalID()] = p
	}
	desiredByID := make(map[string]types.Partition, len(desired))
	for _, p := range desired {
		desiredByID[p.CanonicalID()] = p
	}

	for id, d := range desiredByID {
		live, ok := liveByID[id]
		if !ok {
			added = append(added, clonePartition(d))
			continue
		}
		if live.Weight != d.Weight || live.Label != d.Label {
			changed = append(changed, PartitionWeightChange{
				Keys:      slices.Clone(d.Keys),
				OldWeight: live.Weight,
				NewWeight: d.Weight,
				OldLabel:  live.Label,
				NewLabel:  d.Label,
			})
		}
	}
	for id, l := range liveByID {
		if _, ok := desiredByID[id]; !ok {
			removed = append(removed, clonePartition(l))
		}
	}

	slices.SortFunc(added, cmpPartitionCanonical)
	slices.SortFunc(removed, cmpPartitionCanonical)
	slices.SortFunc(changed, func(a, b PartitionWeightChange) int {
		return strings.Compare(canonicalIDOfKeys(a.Keys), canonicalIDOfKeys(b.Keys))
	})

	return added, removed, changed
}

// cmpPartitionCanonical orders partitions by CanonicalID.
func cmpPartitionCanonical(a, b types.Partition) int {
	return strings.Compare(a.CanonicalID(), b.CanonicalID())
}

// canonicalIDOfKeys returns the CanonicalID for a bare key set.
func canonicalIDOfKeys(keys []string) string {
	return types.Partition{Keys: keys}.CanonicalID()
}

// clonePartition deep-copies a partition, reallocating its Keys slice.
func clonePartition(p types.Partition) types.Partition {
	cp := p // struct-value copy: Weight, Label, and future fields
	cp.Keys = slices.Clone(p.Keys)

	return cp
}

// clonePartitions deep-copies a partition slice. The result is always non-nil
// so it renders as a JSON array, never null.
func clonePartitions(src []types.Partition) []types.Partition {
	out := make([]types.Partition, len(src))
	for i, p := range src {
		out[i] = clonePartition(p)
	}

	return out
}
