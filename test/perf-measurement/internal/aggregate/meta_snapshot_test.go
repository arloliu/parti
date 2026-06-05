package aggregate

import (
	"strings"
	"testing"
)

func TestSummarizeMetaSnapshot_UnderGated(t *testing.T) {
	// The fixture has 2 jsz lines both sharing the same last_time,
	// so MetaSnapshotCount == 1, which is below MetaSnapshotMinCount (5).
	// SummarizeMetaSnapshot must return Gated=false and log the under-gate decision.
	samples, err := ParseMetaSnapshot("../../testdata/jsz_meta_sample.ndjson")
	if err != nil {
		t.Fatal(err)
	}

	sum, logLine := SummarizeMetaSnapshot(samples)

	if sum.Gated {
		t.Fatalf("expected Gated=false for count=1 < %d, got Gated=true", MetaSnapshotMinCount)
	}
	if sum.SnapshotCount != 1 {
		t.Fatalf("SnapshotCount = %d, want 1", sum.SnapshotCount)
	}
	// Numeric fields must be zero when under-gated.
	if sum.LastDurationMs != 0 {
		t.Fatalf("LastDurationMs = %v, want 0 (under-gated)", sum.LastDurationMs)
	}
	if sum.PendingSize != 0 {
		t.Fatalf("PendingSize = %d, want 0 (under-gated)", sum.PendingSize)
	}
	// Log line must mention the under-gate decision.
	if !strings.Contains(logLine, "under-gated") {
		t.Fatalf("expected log line to mention under-gated, got: %s", logLine)
	}
}

func TestSummarizeMetaSnapshot_Gated(t *testing.T) {
	// Build synthetic samples with 5 distinct last_time values to exercise
	// the gated path.
	samples := []MetaSnapshotSample{
		{LastDurationNs: 1000000, PendingSize: 100, PendingEntries: 5, LastTime: "2026-01-01T00:00:01Z"},
		{LastDurationNs: 1100000, PendingSize: 110, PendingEntries: 6, LastTime: "2026-01-01T00:00:02Z"},
		{LastDurationNs: 1200000, PendingSize: 120, PendingEntries: 7, LastTime: "2026-01-01T00:00:03Z"},
		{LastDurationNs: 1300000, PendingSize: 130, PendingEntries: 8, LastTime: "2026-01-01T00:00:04Z"},
		{LastDurationNs: 2011874, PendingSize: 68319, PendingEntries: 72, LastTime: "2026-01-01T00:00:05Z"},
	}

	sum, logLine := SummarizeMetaSnapshot(samples)

	if !sum.Gated {
		t.Fatalf("expected Gated=true for count=5 >= %d", MetaSnapshotMinCount)
	}
	if sum.SnapshotCount != 5 {
		t.Fatalf("SnapshotCount = %d, want 5", sum.SnapshotCount)
	}
	// Last sample values.
	const wantDurMs = float64(2011874) / 1e6
	if sum.LastDurationMs != wantDurMs {
		t.Fatalf("LastDurationMs = %v, want %v", sum.LastDurationMs, wantDurMs)
	}
	if sum.PendingSize != 68319 {
		t.Fatalf("PendingSize = %d, want 68319", sum.PendingSize)
	}
	if sum.PendingEntries != 72 {
		t.Fatalf("PendingEntries = %d, want 72", sum.PendingEntries)
	}
	if !strings.Contains(logLine, "count=5") {
		t.Fatalf("expected log line to contain count=5, got: %s", logLine)
	}
}
