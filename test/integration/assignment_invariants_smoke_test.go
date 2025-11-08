package integration_test

import (
	"testing"

	"github.com/arloliu/parti/test/testutil"
	"github.com/arloliu/parti/types"
)

// TestInvariants_Smoke ensures the invariant helper is wired and usable in integration tests.
func TestInvariants_Smoke(t *testing.T) {
	assignments := map[string][]types.Partition{
		"worker-0": {{Keys: []string{"p0"}}, {Keys: []string{"p1"}}},
		"worker-1": {{Keys: []string{"p2"}}, {Keys: []string{"p3"}}},
	}
	testutil.AssertAssignmentsConsistent(t, assignments, 4)
}
