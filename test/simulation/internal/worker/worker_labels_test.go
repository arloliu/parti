package worker

import (
	"testing"

	"github.com/arloliu/parti/v2/types"
)

// TestNewWorker_StampsPartitionLabels verifies that PartitionLabels (parallel
// to the existing PartitionWeights array) gets copied onto each constructed
// types.Partition.Label, defaulting to "" for indices beyond the slice —
// mirroring PartitionWeights' out-of-range default-to-1 behavior exactly.
func TestNewWorker_StampsPartitionLabels(t *testing.T) {
	t.Parallel()
	// NewWorker requires a live NATS connection to go further than the
	// partition-construction loop; this test only needs to prove the loop
	// logic. Extract it as a standalone helper so it's testable without a
	// NATS server. See Step 3 below — buildPartitions is a new unexported
	// helper factored out of NewWorker's existing inline loop.
	got := buildPartitions(3, nil, []string{"vip-a", "", "vip-b"})
	want := []types.Partition{
		{Keys: []string{"0"}, Weight: 1, Label: "vip-a"},
		{Keys: []string{"1"}, Weight: 1, Label: ""},
		{Keys: []string{"2"}, Weight: 1, Label: "vip-b"},
	}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Label != want[i].Label || got[i].Weight != want[i].Weight || got[i].Keys[0] != want[i].Keys[0] {
			t.Errorf("index %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

// TestNewWorker_PartitionLabelsShorterThanCount verifies default-to-""
// out-of-range behavior, mirroring PartitionWeights' default-to-1.
func TestNewWorker_PartitionLabelsShorterThanCount(t *testing.T) {
	t.Parallel()
	got := buildPartitions(3, nil, []string{"vip-a"})
	if got[1].Label != "" || got[2].Label != "" {
		t.Errorf("expected out-of-range partitions to default to unlabeled, got %+v", got)
	}
}
