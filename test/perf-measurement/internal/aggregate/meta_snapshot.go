package aggregate

import (
	"encoding/json"
	"fmt"
	"io"
)

// MetaSnapshotMinCount is the minimum number of distinct metacontroller
// snapshots required for the §8.1 gate to consider the distribution stable
// enough to emit summary stats. Below this threshold the data is
// under-gated: reporting a single last_duration value would be misleading.
const MetaSnapshotMinCount = 5

// MetaSnapshotSummary holds the run-level metacontroller snapshot metrics
// extracted from a /jsz capture. It is emitted only when SnapshotCount >= 5.
type MetaSnapshotSummary struct {
	// Gated reports whether SnapshotCount met the MetaSnapshotMinCount
	// threshold. When false, the other fields are zero/empty.
	Gated bool `json:"gated"`
	// SnapshotCount is the number of distinct non-zero last_time values
	// (= number of snapshots observed during the capture window).
	SnapshotCount int `json:"snapshot_count"`
	// LastDurationMs is last_duration expressed in milliseconds (last_duration
	// is in nanoseconds on the wire). Taken from the final sample, which
	// reflects the most recent snapshot the cluster completed.
	LastDurationMs float64 `json:"meta_snapshot_last_duration_ms,omitempty"`
	// PendingSize is the pending WAL entry byte count from the final sample
	// (same field meta_compact_size gates on). Named per design §8.1.
	PendingSize int64 `json:"meta_snapshot_pending_size,omitempty"`
	// PendingEntries is the pending entry count from the final sample.
	PendingEntries int64 `json:"meta_snapshot_pending_entries,omitempty"`
}

// SummarizeMetaSnapshot applies the §8.1 gate and returns a MetaSnapshotSummary.
// When count < MetaSnapshotMinCount, Gated is false and the numeric fields are
// zero so callers can distinguish "no data" from "zero value". The logLine
// return is a human-readable one-liner suitable for logging either way.
func SummarizeMetaSnapshot(samples []MetaSnapshotSample) (MetaSnapshotSummary, string) {
	count := MetaSnapshotCount(samples)
	if count < MetaSnapshotMinCount {
		return MetaSnapshotSummary{
				Gated:         false,
				SnapshotCount: count,
			}, fmt.Sprintf("meta_snapshot: under-gated (count=%d < %d); snapshot stats not emitted",
				count, MetaSnapshotMinCount)
	}
	// Use the last sample for per-run stats (most recent snapshot completed).
	last := samples[len(samples)-1]
	s := MetaSnapshotSummary{
		Gated:          true,
		SnapshotCount:  count,
		LastDurationMs: float64(last.LastDurationNs) / 1e6,
		PendingSize:    last.PendingSize,
		PendingEntries: last.PendingEntries,
	}

	return s, fmt.Sprintf("meta_snapshot: count=%d last_duration_ms=%.2f pending_size=%d pending_entries=%d",
		s.SnapshotCount, s.LastDurationMs, s.PendingSize, s.PendingEntries)
}

// WriteMetaSnapshot writes a MetaSnapshotSummary as a single-line JSON file.
// The caller should only invoke this when sum.Gated is true; it is a no-op
// (returns nil) when Gated is false.
func WriteMetaSnapshot(w io.Writer, sum MetaSnapshotSummary) error {
	if !sum.Gated {
		return nil
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(sum)
}
