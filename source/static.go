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

var (
	_ types.PartitionSource  = (*Static)(nil)
	_ types.PartitionUpdater = (*Static)(nil)
)

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

// Start implements PartitionSource.Start.
// For Static source, this is a no-op.
func (s *Static) Start(_ context.Context) error {
	return nil
}

// Stop implements PartitionSource.Stop.
// For Static source, this is a no-op.
func (s *Static) Stop() error {
	return nil
}

// List returns the static list of partitions.
//
// Returns:
//   - []types.Partition: The fixed list of partitions
//   - error: Always nil (never fails)
func (s *Static) List(_ context.Context) ([]types.Partition, error) {
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
//   - ctx: Context for the operation (unused)
//   - partitions: New list of partitions
//
// Returns:
//   - error: Always nil
//
// Example:
//
//	src := source.NewStatic(initialPartitions)
//	// Later: add more partitions
//	src.Update(context.Background(), expandedPartitions)
func (s *Static) Update(_ context.Context, partitions []types.Partition) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.partitions = make([]types.Partition, len(partitions))
	copy(s.partitions, partitions)
	return nil
}
