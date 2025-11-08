package types

import "strings"

// Partition represents a logical work partition.
//
// A partition is the unit of work assignment. Each partition can contain
// multiple keys and has an associated weight for load balancing.
type Partition struct {
	// Keys uniquely identify this partition.
	// For Kafka: ["topic", "partition_id"]
	Keys []string `json:"keys"`

	// Weight represents the relative processing cost (default: 100).
	// Used by weighted assignment strategies for load balancing.
	Weight int64 `json:"weight"`
}

// SubjectKey returns the canonical subject identifier for the partition formed by
// joining the Keys with a dot ("."). This is used for subject templating and
// JetStream FilterSubjects construction.
//
// Returns:
//   - string: Dot-joined key sequence ("" if no keys)
func (p Partition) SubjectKey() string {
	if len(p.Keys) == 0 {
		return ""
	}

	return strings.Join(p.Keys, ".")
}

// ID returns the canonical durable name fragment for the partition by joining
// the Keys with a dash ("-"). This is suitable for durable consumer names and
// hashing contexts requiring a stable, human-readable identifier.
//
// Returns:
//   - string: Dash-joined key sequence ("" if no keys)
func (p Partition) ID() string {
	if len(p.Keys) == 0 {
		return ""
	}

	return strings.Join(p.Keys, "-")
}

// Compare performs a lexicographic comparison of partition key sequences.
//
// Ordering rules:
//   - Compare Keys element-wise using string order
//   - If all shared elements are equal, the shorter Keys slice sorts first
//   - Returns 0 when both key sequences are identical (weight is not considered)
//
// Returns:
//   - int: -1 if p < q, 0 if equal, +1 if p > q
func (p Partition) Compare(q Partition) int {
	al, bl := len(p.Keys), len(q.Keys)
	n := min(bl, al)

	for i := range n {
		if p.Keys[i] == q.Keys[i] {
			continue
		}
		if p.Keys[i] < q.Keys[i] {
			return -1
		}

		return 1
	}
	if al == bl {
		return 0
	}
	if al < bl {
		return -1
	}

	return 1
}

// Assignment contains the current partition assignment for a worker.
//
// Assignments are versioned and include lifecycle metadata for coordination.
type Assignment struct {
	// Version is a monotonically increasing assignment version.
	// Used to detect stale assignments and coordinate updates.
	Version int64 `json:"version"`

	// Lifecycle indicates the assignment phase (e.g., "stable", "scaling", "rebalancing").
	Lifecycle string `json:"lifecycle"`

	// Partitions is the list of partitions assigned to this worker.
	Partitions []Partition `json:"partitions"`
}
