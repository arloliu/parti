package integration_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

func TestPartitionSource_StaticSource(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Test 1: Create static source with partitions
	t.Log("Test 1: Verify static source returns partitions")

	initialPartitions := []types.Partition{
		{Keys: []string{"partition-001"}, Weight: 100},
		{Keys: []string{"partition-002"}, Weight: 150},
		{Keys: []string{"partition-003"}, Weight: 200},
	}

	src := source.NewStatic(initialPartitions)
	require.NotNil(t, src, "NewStatic should not return nil")

	// List partitions
	partitions, err := src.ListPartitions(ctx)
	require.NoError(t, err, "ListPartitions should not return error")
	require.Len(t, partitions, 3, "Should return 3 partitions")

	// Verify partition contents
	for i, p := range partitions {
		require.Equal(t, initialPartitions[i].Keys, p.Keys, "Partition keys should match")
		require.Equal(t, initialPartitions[i].Weight, p.Weight, "Partition weight should match")
	}

	// Test 2: Update partitions and verify changes
	t.Log("Test 2: Verify Update() changes partition list")

	updatedPartitions := []types.Partition{
		{Keys: []string{"partition-001"}, Weight: 100},
		{Keys: []string{"partition-002"}, Weight: 150},
		{Keys: []string{"partition-003"}, Weight: 200},
		{Keys: []string{"partition-004"}, Weight: 100},
		{Keys: []string{"partition-005"}, Weight: 100},
	}

	src.Update(updatedPartitions)

	partitions, err = src.ListPartitions(ctx)
	require.NoError(t, err, "ListPartitions should not return error")
	require.Len(t, partitions, 5, "Should return 5 partitions after update")

	// Verify updated contents
	for i, p := range partitions {
		require.Equal(t, updatedPartitions[i].Keys, p.Keys, "Updated partition keys should match")
		require.Equal(t, updatedPartitions[i].Weight, p.Weight, "Updated partition weight should match")
	}

	// Test 3: Verify ListPartitions returns a copy (not the internal slice)
	t.Log("Test 3: Verify ListPartitions returns a copy")

	partitions1, _ := src.ListPartitions(ctx)
	partitions2, _ := src.ListPartitions(ctx)

	// Modify first result
	partitions1[0].Weight = 999

	// Second result should be unchanged
	require.NotEqual(t, partitions1[0].Weight, partitions2[0].Weight, "ListPartitions should return a copy")
	require.Equal(t, int64(100), partitions2[0].Weight, "Second call should return original weight")

	t.Log("Test passed - static source works correctly")
}

func TestPartitionSource_EmptyPartitions(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Test 1: Create source with empty list
	t.Log("Test 1: Verify empty partition list is handled")

	src := source.NewStatic([]types.Partition{})

	partitions, err := src.ListPartitions(ctx)
	require.NoError(t, err, "ListPartitions should not return error for empty list")
	require.Len(t, partitions, 0, "Should return empty slice")
	require.NotNil(t, partitions, "Should return empty slice, not nil")

	// Test 2: Update from non-empty to empty
	t.Log("Test 2: Verify updating to empty list works")

	src.Update([]types.Partition{
		{Keys: []string{"partition-001"}, Weight: 100},
	})

	partitions, err = src.ListPartitions(ctx)
	require.NoError(t, err, "Should return partition")
	require.Len(t, partitions, 1, "Should have 1 partition")

	// Update to empty
	src.Update([]types.Partition{})

	partitions, err = src.ListPartitions(ctx)
	require.NoError(t, err, "ListPartitions should not return error after empty update")
	require.Len(t, partitions, 0, "Should return empty slice after update")

	// Test 3: Create source with nil
	t.Log("Test 3: Verify nil partition list is handled")

	srcNil := source.NewStatic(nil)

	partitions, err = srcNil.ListPartitions(ctx)
	require.NoError(t, err, "ListPartitions should not return error for nil input")
	require.Len(t, partitions, 0, "Should return empty slice for nil input")

	t.Log("Test passed - empty partition handling works correctly")
}

func TestPartitionSource_ConcurrentAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create source with initial partitions
	initialPartitions := make([]types.Partition, 100)
	for i := 0; i < 100; i++ {
		initialPartitions[i] = types.Partition{
			Keys:   []string{fmt.Sprintf("partition-%03d", i)},
			Weight: 100,
		}
	}

	src := source.NewStatic(initialPartitions)

	// Test concurrent reads and writes
	t.Log("Testing concurrent ListPartitions and Update operations...")

	const (
		numReaders = 10
		numWriters = 5
		iterations = 50
	)

	var wg sync.WaitGroup
	errChan := make(chan error, numReaders+numWriters)

	// Start readers
	for i := 0; i < numReaders; i++ {
		readerID := i
		wg.Go(func() {
			for j := 0; j < iterations; j++ {
				partitions, err := src.ListPartitions(ctx)
				if err != nil {
					errChan <- fmt.Errorf("reader %d: iteration %d: %w", readerID, j, err)
					return
				}

				// Verify partition list is valid
				if len(partitions) == 0 {
					errChan <- fmt.Errorf("reader %d: iteration %d: empty partition list", readerID, j)
					return
				}

				// Verify all partitions have valid keys
				for _, p := range partitions {
					if len(p.Keys) == 0 {
						errChan <- fmt.Errorf("reader %d: iteration %d: partition with empty keys", readerID, j)
						return
					}
				}
			}
		})
	}

	// Start writers
	for i := 0; i < numWriters; i++ {
		wg.Go(func() {
			for j := 0; j < iterations; j++ {
				// Alternate between different partition counts
				partCount := 50 + (j % 50) // 50-99 partitions
				newPartitions := make([]types.Partition, partCount)
				for k := 0; k < partCount; k++ {
					newPartitions[k] = types.Partition{
						Keys:   []string{fmt.Sprintf("partition-%03d", k)},
						Weight: 100 + int64(k),
					}
				}

				src.Update(newPartitions)

				// Brief sleep to allow readers to see the update
				time.Sleep(time.Millisecond)
			}
		})
	}

	// Wait for all goroutines to finish
	wg.Wait()
	close(errChan)

	// Check for errors
	errors := make([]error, 0, numReaders+numWriters)
	for err := range errChan {
		errors = append(errors, err)
	}

	require.Empty(t, errors, "Should have no errors during concurrent access: %v", errors)

	// Final verification
	partitions, err := src.ListPartitions(ctx)
	require.NoError(t, err, "Final ListPartitions should succeed")
	require.NotEmpty(t, partitions, "Should have partitions after concurrent operations")

	t.Logf("Test passed - %d readers and %d writers completed %d iterations each without errors",
		numReaders, numWriters, iterations)
}

func TestPartitionSource_CustomImplementation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Test: Verify custom PartitionSource implementations work
	t.Log("Test: Verify custom PartitionSource interface implementation")

	// Create a custom implementation that generates partitions dynamically
	customSrc := &customPartitionSource{
		partitionCount: 10,
		prefix:         "custom",
	}

	partitions, err := customSrc.ListPartitions(ctx)
	require.NoError(t, err, "Custom source should not return error")
	require.Len(t, partitions, 10, "Should return 10 partitions")

	// Verify partition format
	for i, p := range partitions {
		expectedKey := fmt.Sprintf("custom-partition-%03d", i)
		require.Equal(t, []string{expectedKey}, p.Keys, "Partition key should match format")
		require.Equal(t, int64(100+i*10), p.Weight, "Partition weight should be calculated correctly")
	}

	// Test context cancellation
	t.Log("Test: Verify context cancellation is respected")

	cancelCtx, cancelFunc := context.WithCancel(context.Background())
	cancelFunc() // Cancel immediately

	_, err = customSrc.ListPartitions(cancelCtx)
	require.Error(t, err, "Should return error when context is cancelled")
	require.Equal(t, context.Canceled, err, "Error should be context.Canceled")

	// Test error handling
	t.Log("Test: Verify error handling in custom source")

	errorSrc := &errorPartitionSource{
		shouldFail: true,
	}

	_, err = errorSrc.ListPartitions(ctx)
	require.Error(t, err, "Error source should return error")
	require.Contains(t, err.Error(), "simulated partition discovery failure", "Error message should match")

	// Test successful case after fixing error
	errorSrc.shouldFail = false
	partitions, err = errorSrc.ListPartitions(ctx)
	require.NoError(t, err, "Should succeed when error flag is false")
	require.Len(t, partitions, 3, "Should return partitions when successful")

	t.Log("Test passed - custom PartitionSource implementations work correctly")
}

// customPartitionSource is a test implementation that generates partitions dynamically
type customPartitionSource struct {
	partitionCount int
	prefix         string
}

func (c *customPartitionSource) ListPartitions(ctx context.Context) ([]types.Partition, error) {
	// Check context cancellation
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	partitions := make([]types.Partition, c.partitionCount)
	for i := 0; i < c.partitionCount; i++ {
		partitions[i] = types.Partition{
			Keys:   []string{fmt.Sprintf("%s-partition-%03d", c.prefix, i)},
			Weight: int64(100 + i*10),
		}
	}

	return partitions, nil
}

// errorPartitionSource is a test implementation that can simulate errors
type errorPartitionSource struct {
	shouldFail bool
}

func (e *errorPartitionSource) ListPartitions(ctx context.Context) ([]types.Partition, error) {
	if e.shouldFail {
		return nil, errors.New("simulated partition discovery failure")
	}

	return []types.Partition{
		{Keys: []string{"partition-001"}, Weight: 100},
		{Keys: []string{"partition-002"}, Weight: 100},
		{Keys: []string{"partition-003"}, Weight: 100},
	}, nil
}
