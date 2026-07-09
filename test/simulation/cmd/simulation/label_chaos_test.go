package main

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/test/simulation/internal/coordinator"
	"github.com/arloliu/parti/v2/test/simulation/internal/natsutil"
	"github.com/arloliu/parti/v2/test/simulation/internal/worker"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestHandleLabelHeartbeatTakeover_FiresTightTakeoverBranch proves the new
// chaos primitive lands on a still-live heartbeat key and triggers the
// worker-monitor's tight-takeover fingerprint-mismatch callback — the
// specific branch no other test in this repo reaches (see
// test/integration/assignment/label_stress_test.go's own doc comment,
// which documents its g3 goroutine does NOT reach this branch because a
// graceful stop deletes the key first).
//
// This test spins up ONE real worker directly against embedded NATS
// (bypassing the full sim orchestrator) so it's fast and focused — the
// full scenario-level proof lives in label_tight_takeover_churn.yaml
// (Task 10), which exercises this primitive under concurrent chaos.
//
// NOTE: this test does NOT call t.Parallel(). handleLabelHeartbeatTakeover
// resolves its probe JetStream via freshJS() → the package-global aioNS,
// which this test must set. Mutating a package global under t.Parallel()
// would be a data race under -race against any future parallel test in this
// package that also touches aioNS, so the test runs serially and resets the
// global on cleanup.
func TestHandleLabelHeartbeatTakeover_FiresTightTakeoverBranch(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)

	// freshJS() (used by the handler under test) opens its probe JetStream
	// against aioNS. In the real all-in-one run this is the shared
	// connection every worker goroutine uses; here we point it at the same
	// embedded-NATS conn the worker below uses so the handler reads and
	// rewrites the very heartbeat key the worker publishes.
	aioNS = nc
	t.Cleanup(func() { aioNS = nil })

	// The worker's manager reaches StateStable only after it applies its
	// assignment, which creates JetStream consumers on the "SIMULATION"
	// work stream. In the full sim, main.go creates this stream before any
	// worker starts (natsutil.CreateStream at main.go:338); this isolated
	// test must do the same or the worker never leaves WaitingAssignment.
	require.NoError(t, natsutil.CreateStream(nc, 4))

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	coordCh := make(chan coordinator.AssignmentReport, 8)
	w, err := worker.NewWorker(worker.Config{
		ID:                 "worker-0",
		NC:                 nc,
		JS:                 js,
		PartitionCount:     4,
		AssignmentStrategy: "ConsistentHash",
		AckWait:            2 * time.Second,
		AssignmentReportCh: coordCh,
		WorkerLabels:       []string{"vip-a"},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, w.Start(ctx))
	defer w.Stop()

	// Wait for the worker to reach Stable so its heartbeat key exists and
	// is being actively watched by its own calculator (single-worker
	// cluster: it's its own leader, so its own monitor observes its own
	// PUT).
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if parti.State(w.WorkerStateInt()) == parti.StateStable {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if parti.State(w.WorkerStateInt()) != parti.StateStable {
		t.Fatalf("worker did not reach Stable in time (state=%d)", w.WorkerStateInt())
	}

	registry := coordinator.NewGoroutineRegistry()
	registry.Register("worker-0", coordinator.WorkerGoroutine, cancel, nil, w)

	before := w.ChaosProof().LabelChangeTriggers()
	handleLabelHeartbeatTakeover(ctx, registry, "worker-0", []string{"vip-b"})

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if w.ChaosProof().LabelChangeTriggers() > before {
			return // success: the tight-takeover branch fired
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected LabelChangeTriggers to increase after handleLabelHeartbeatTakeover; still %d", w.ChaosProof().LabelChangeTriggers())
}
