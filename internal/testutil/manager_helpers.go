package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/nats-io/nats.go/jetstream"
)

// StartManagerWithHandoffRecorder starts a Manager with the provided config, JetStream,
// source and strategy, injecting the given handoff metrics recorder. It returns the
// started manager and a cleanup function that stops it.
//
// Usage:
//
//	mr := NewHandoffMetricsRecorder()
//	mgr, cleanup := StartManagerWithHandoffRecorder(t, &cfg, js, src, strategy, mr)
//	defer cleanup()
func StartManagerWithHandoffRecorder(t *testing.T, cfg *parti.Config, js jetstream.JetStream, src parti.PartitionSource, strategy parti.AssignmentStrategy, mr *HandoffMetricsRecorder) (*parti.Manager, func()) {
	t.Helper()
	mgr, err := parti.NewManager(cfg, js, src, strategy, parti.WithHandoffMetricsRecorder(mr))
	if err != nil {
		t.Fatalf("NewManager error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager start error: %v", err)
	}
	// Manager.Start now returns once the synchronous sanity-check phase
	// is complete (StateWaitingAssignment). Block until the background
	// runner has applied the initial assignment so callers can read
	// CurrentAssignment() immediately on return.
	if err := <-mgr.WaitState(parti.StateStable, 10*time.Second); err != nil {
		_ = mgr.Stop(context.Background())
		t.Fatalf("Manager did not reach StateStable: %v", err)
	}
	cleanup := func() { _ = mgr.Stop(context.Background()) }

	return mgr, cleanup
}
