package partition

import "github.com/zeebo/xxh3"

func computePartition(key string, numPartitions int, seed uint64) int {
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
