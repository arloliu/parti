package failure_test

import (
	"os"
	"testing"
)

// requireClusterCrashTests gates the heavy multi-node NATS crash/recovery tests
// in this package. Each spins a 5-node embedded JetStream cluster and several
// deliberately crash nodes without restarting them, so the worker's graceful Stop
// times out at cleanup (NATS is degraded) and resources are slow to release.
// Run back-to-back under `-race` in a single `go test` invocation, that pressure
// accumulates and destabilizes later cluster startups.
//
// They pass reliably in isolation; gate them behind an env var (matching the
// repo's PARTI_RUN_HERD_DIAGNOSTIC precedent for heavy cluster diagnostics) so the
// default `make test-integration` run stays stable. The feature logic itself is
// covered by always-on unit tests in internal/recovery and consumer.
//
// Run them with:
//
//	PARTI_RUN_CLUSTER_CRASH=1 go test ./test/integration/failure/ -race -run TestCluster_ -p 1
func requireClusterCrashTests(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping multi-node cluster crash test in short mode")
	}
	if os.Getenv("PARTI_RUN_CLUSTER_CRASH") == "" {
		t.Skip("set PARTI_RUN_CLUSTER_CRASH=1 to run heavy multi-node NATS crash/recovery tests")
	}
}
