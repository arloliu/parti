package partutil

import "github.com/zeebo/xxh3"

// ComputePartition returns the partition index for the given key.
//
// Returns -1 if key is empty or numPartitions <= 0.
//
// Parameters:
//   - key: Partition key string
//   - numPartitions: Total partition count (must be > 0)
//   - seed: Optional hash seed (0 for default behavior)
//
// Returns:
//   - int: Partition index in [0, numPartitions-1], or -1 on invalid input
func ComputePartition(key string, numPartitions int, seed uint64) int {
	if key == "" || numPartitions <= 0 {
		return -1
	}
	var h uint64
	if seed != 0 {
		h = xxh3.HashStringSeed(key, seed)
	} else {
		h = xxh3.HashString(key)
	}

	//nolint:gosec // Mod ensures result is within int range of numPartitions.
	return int(h % uint64(numPartitions))
}
