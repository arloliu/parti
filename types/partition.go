package types

// Partition represents a logical work partition.
//
// A partition is the unit of work assignment. Each partition can contain
// multiple keys and has an associated weight for load balancing.
type Partition struct {
	// Keys uniquely identify this partition.
	// For Cassandra: ["keyspace", "table", "token_range"]
	// For Kafka: ["topic", "partition_id"]
	Keys []string `json:"keys"`

	// Weight represents the relative processing cost (default: 100).
	// Used by weighted assignment strategies for load balancing.
	Weight int64 `json:"weight"`
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
