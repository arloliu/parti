package stress_test

import (
	"os"
	"testing"
)

// requireStressEnabled skips the test unless long stress tests are explicitly enabled.
//
// Enable by setting environment variable PARTI_STRESS=1 when invoking `go test`.
// Example:
//
//	PARTI_STRESS=1 go test -v -timeout 20m ./test/stress
func requireStressEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("PARTI_STRESS") != "1" {
		t.Skip("Skipping long stress/perf test (set PARTI_STRESS=1 to run)")
	}
}
