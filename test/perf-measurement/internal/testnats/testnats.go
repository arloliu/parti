// Package testnats provides a tiny embedded JetStream NATS server for tests
// in this module. It mirrors the bring-up pattern used across the rig's
// existing tests (internal/instrumentedjs, cmd/harness, cmd/calibrate,
// internal/storageverify): server.NewServer + go Start + ReadyForConnections.
package testnats

import (
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

// Start launches an in-process NATS server with JetStream enabled on a random
// port, backed by a temp StoreDir. It returns the client URL and a shutdown
// closure. The caller owns its own NATS connection; shutdown only stops the
// server.
func Start(t *testing.T) (string, func()) {
	t.Helper()
	opts := &server.Options{
		JetStream: true,
		Port:      -1,
		StoreDir:  t.TempDir(),
	}
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("testnats: NewServer: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		ns.Shutdown()
		t.Fatal("testnats: NATS server not ready")
	}

	return ns.ClientURL(), ns.Shutdown
}
