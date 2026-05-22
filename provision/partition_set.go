package provision

import (
	"fmt"

	"github.com/arloliu/parti/v2/types"
)

// ValidatePartitionSet performs static validation of a declared partition
// table (PartitionSourceConfig.Partitions): the set must contain at least one
// record, every record must pass types.Partition.Validate, and no two records
// may share a CanonicalID. Every failure wraps ErrInvalidConfig.
//
// This is the partition-record analogue of validatePartitionSource, which
// validates the bucket config. PlanPartitions and ApplyPartitions — the
// `partictl partitions` subcommand — call it; the bucket-provisioning commands
// (plan / apply / adopt) do not, so an env config that omits partitions still
// loads for those commands.
//
// A zero-length set is rejected: Phase 3 exposes no surface for pruning the
// partition table to empty.
func ValidatePartitionSet(partitions []types.Partition) error {
	if len(partitions) == 0 {
		return fmt.Errorf("%w: no partitions declared in partitionSource.partitions", ErrInvalidConfig)
	}

	seen := make(map[string]struct{}, len(partitions))
	for i, p := range partitions {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("%w: partitionSource.partitions[%d]: %w", ErrInvalidConfig, i, err)
		}
		cid := p.CanonicalID()
		if _, dup := seen[cid]; dup {
			return fmt.Errorf("%w: partitionSource.partitions[%d] duplicates an earlier record (canonical_id=%q)",
				ErrInvalidConfig, i, cid)
		}
		seen[cid] = struct{}{}
	}

	return nil
}
