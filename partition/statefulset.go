package partition

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ParseStatefulSetOrdinal extracts the ordinal index from a StatefulSet pod hostname.
//
// StatefulSet pods are named: <statefulset-name>-<ordinal>
// Example: "worker-3" → 3
//
// Parameters:
//   - hostname: Pod hostname to parse
//
// Returns:
//   - int: Ordinal index
//   - error: Parse error when hostname format is invalid
func ParseStatefulSetOrdinal(hostname string) (int, error) {
	idx := strings.LastIndex(hostname, "-")
	if idx < 0 {
		return 0, fmt.Errorf("invalid statefulset hostname format: %s", hostname)
	}

	return strconv.Atoi(hostname[idx+1:])
}

// GetPartitionFromEnv reads the partition index from environment.
// Checks PARTITION_INDEX first, then attempts to parse HOSTNAME as StatefulSet ordinal.
//
// Returns:
//   - int: Partition index
//   - error: If neither source provides a valid index
func GetPartitionFromEnv() (int, error) {
	if idx := os.Getenv("PARTITION_INDEX"); idx != "" {
		return strconv.Atoi(idx)
	}

	hostname := os.Getenv("HOSTNAME")
	if hostname == "" {
		var err error
		hostname, err = os.Hostname()
		if err != nil {
			return 0, fmt.Errorf("cannot determine partition: %w", err)
		}
	}

	return ParseStatefulSetOrdinal(hostname)
}
