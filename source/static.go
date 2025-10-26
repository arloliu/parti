package source

import (
	"context"

	"github.com/arloliu/parti/types"
)

// Static implements a partition source with a fixed list of partitions.
type Static struct {
	partitions []types.Partition
}

var _ types.PartitionSource = (*Static)(nil)

// NewStatic creates a new static partition source.
//
// The source returns a fixed list of partitions that never changes.
// Useful for testing and scenarios where partitions are known at startup.
//
// Parameters:
//   - partitions: Fixed list of partitions
//
// Returns:
//   - *Static: Initialized static source
//
// Example:
//
//	partitions := []types.Partition{
//	    {Keys: []string{"tool001", "chamber1"}, Weight: 100},
//	    {Keys: []string{"tool001", "chamber2"}, Weight: 150},
//	}
//	src := source.NewStatic(partitions)
//	mgr := parti.NewManager(&cfg, conn, src)
func NewStatic(partitions []types.Partition) *Static {
	return &Static{
		partitions: partitions,
	}
}

// ListPartitions returns the static list of partitions.
//
// Returns:
//   - []types.Partition: The fixed list of partitions
//   - error: Always nil (never fails)
func (s *Static) ListPartitions(_ /* ctx */ context.Context) ([]types.Partition, error) {
	result := make([]types.Partition, len(s.partitions))
	copy(result, s.partitions)

	return result, nil
}
