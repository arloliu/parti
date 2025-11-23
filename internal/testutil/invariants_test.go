package testutil

import (
	"testing"

	"github.com/arloliu/parti/types"
)

func TestAssertAssignmentsConsistent_Passes(t *testing.T) {
	assignments := map[string][]types.Partition{
		"w1": {
			{Keys: []string{"a"}},
			{Keys: []string{"b"}},
		},
		"w2": {
			{Keys: []string{"c"}},
			{Keys: []string{"d"}},
		},
	}
	AssertAssignmentsConsistent(t, assignments, 4)
}
