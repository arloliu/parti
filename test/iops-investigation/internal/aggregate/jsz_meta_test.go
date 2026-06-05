package aggregate

import (
	"testing"
)

func TestParseMetaSnapshot(t *testing.T) {
	samples, err := ParseMetaSnapshot("../../testdata/jsz_meta_sample.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	// Fixture has 2 jsz lines (the varz line is ignored) ⇒ 2 samples.
	if len(samples) != 2 {
		t.Fatalf("expected 2 meta samples (2 jsz lines, varz ignored), got %d", len(samples))
	}
	// CONCRETE assertions pinned to the captured fixture (no loose >=0).
	s := samples[0]
	if s.LastDurationNs != 2011874 {
		t.Fatalf("LastDurationNs = %d, want 2011874", s.LastDurationNs)
	}
	if s.PendingSize != 68319 {
		t.Fatalf("PendingSize = %d, want 68319", s.PendingSize)
	}
	if s.PendingEntries != 72 {
		t.Fatalf("PendingEntries = %d, want 72", s.PendingEntries)
	}
	if s.LastTime != "2026-06-04T02:03:29.446274011Z" {
		t.Fatalf("LastTime = %q", s.LastTime)
	}
}

func TestMetaSnapshotCount(t *testing.T) {
	// The fixture has 2 jsz lines with identical last_time values,
	// so MetaSnapshotCount should return 1 (distinct non-zero LastTime).
	samples, err := ParseMetaSnapshot("../../testdata/jsz_meta_sample.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	count := MetaSnapshotCount(samples)
	if count != 1 {
		t.Fatalf("MetaSnapshotCount = %d, want 1 (2 identical last_time values => 1 distinct)", count)
	}
}
