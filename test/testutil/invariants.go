package testutil

import (
	"testing"

	"github.com/arloliu/parti/types"
)

// AssertAssignmentsConsistent verifies that the sum of assigned partitions across
// all workers equals the expected total and that no partition is assigned to more than one worker.
//
// Parameters:
//   - t: testing handle
//   - assignments: map of workerID -> slice of assigned partitions
//   - expectedTotal: expected total number of unique partitions across all workers
func AssertAssignmentsConsistent(t *testing.T, assignments map[string][]types.Partition, expectedTotal int) {
	t.Helper()

	seen := make(map[string]struct{}, expectedTotal)
	sum := 0
	for _, parts := range assignments {
		sum += len(parts)
		for _, p := range parts {
			key := p.SubjectKey()
			if _, ok := seen[key]; ok {
				t.Fatalf("duplicate partition detected: %s", key)
			}
			seen[key] = struct{}{}
		}
	}

	if sum != expectedTotal {
		t.Fatalf("sum of assignments (%d) does not equal expected total (%d)", sum, expectedTotal)
	}
	if len(seen) != expectedTotal {
		t.Fatalf("unique partition count (%d) does not equal expected total (%d)", len(seen), expectedTotal)
	}
}
