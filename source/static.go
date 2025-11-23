package source

import (
	"context"
	"sync"

	"github.com/arloliu/parti/types"
)

// Static implements a partition source with a fixed list of partitions.
type Static struct {
	mu         sync.RWMutex
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
//	js, _ := jetstream.New(conn)
//	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash())
//	if err != nil { /* handle */ }
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
func (s *Static) ListPartitions(_ context.Context) ([]types.Partition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]types.Partition, len(s.partitions))
	copy(result, s.partitions)

	return result, nil
}

// Update updates the partition list.
//
// This allows the static source to simulate dynamic partition changes,
// which is useful for testing partition refresh scenarios.
//
// Parameters:
//   - partitions: New list of partitions
//
// Example:
//
//	src := source.NewStatic(initialPartitions)
//	// Later: add more partitions
//	src.Update(expandedPartitions)
func (s *Static) Update(partitions []types.Partition) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.partitions = make([]types.Partition, len(partitions))
	copy(s.partitions, partitions)
}
