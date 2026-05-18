package main

import (
	"encoding/json"
	"testing"
)

// TestFailureReportSchemaMatchesCoordinator verifies that the visualizer's
// MessageGapError JSON tags decode coordinator-produced failure_report.json
// payloads correctly. Coordinator writes snake_case; a prior mismatch
// (UpperCamelCase here) silently decoded every field to zero, making the
// visualizer's partition filter always target partition 0 and producing
// misleading timelines.
//
// The fixture below is a minimal, coordinator-shape failure report. If the
// coordinator's MessageGapError JSON schema changes, this test must update
// in lockstep.
func TestFailureReportSchemaMatchesCoordinator(t *testing.T) {
	raw := []byte(`{
		"timestamp": "2026-05-18T00:00:00Z",
		"reason": "test",
		"detailed_gaps": [
			{
				"partition_id": 42,
				"expected_seq": 7,
				"received_seq": 9,
				"last_sent": 12
			}
		]
	}`)

	var report FailureReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(report.DetailedGaps) != 1 {
		t.Fatalf("DetailedGaps len = %d, want 1", len(report.DetailedGaps))
	}

	g := report.DetailedGaps[0]
	if g.PartitionID != 42 {
		t.Errorf("PartitionID = %d, want 42 (snake_case tag mismatch?)", g.PartitionID)
	}
	if g.ExpectedSeq != 7 {
		t.Errorf("ExpectedSeq = %d, want 7", g.ExpectedSeq)
	}
	if g.ReceivedSeq != 9 {
		t.Errorf("ReceivedSeq = %d, want 9", g.ReceivedSeq)
	}
	if g.LastSent != 12 {
		t.Errorf("LastSent = %d, want 12", g.LastSent)
	}
}
