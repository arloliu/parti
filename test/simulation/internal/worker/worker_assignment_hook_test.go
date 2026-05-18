package worker

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/test/simulation/internal/coordinator"
	"github.com/arloliu/parti/v2/types"
)

// TestNewWorker_RejectsZeroAckWait guards the C2 safety net: any worker.Config
// call site that omits AckWait must fail loudly at NewWorker time, not
// silently fall back to the durable helper's 30s default (which would
// reintroduce the false-positive gap-escalation window that C2 exists
// to eliminate).
func TestNewWorker_RejectsZeroAckWait(t *testing.T) {
	_, err := NewWorker(Config{
		ID:                 "test-worker",
		PartitionCount:     1,
		AssignmentStrategy: "ConsistentHash",
		HandlerConcurrency: 1,
		ConsumerBatchSize:  1,
		// AckWait deliberately omitted.
	})
	if err == nil {
		t.Fatal("expected NewWorker to reject Config with AckWait=0; got nil")
	}
	if !strings.Contains(err.Error(), "AckWait") {
		t.Errorf("error must reference AckWait; got: %v", err)
	}
}

// TestHandleAssignmentChanged_BlockingSendDelivers proves that the
// first-assignment latency sample is delivered (not dropped) when the
// coordinator's drain goroutine is slow. The pre-fix behavior used a
// non-blocking send with a `default:` branch that silently dropped
// samples — blinding the SlowStartExceedances invariant the channel
// exists to enforce.
//
// Test pattern (verify-first on the "hook does not exit while buffer
// is full" property, NOT on delivery order, which was inherently racy):
//  1. Pre-fill a size-1 channel so any send blocks under the fix.
//  2. Start the hook in a goroutine; record completion via hookDone.
//  3. Sleep generously (200ms) so the goroutine has certainly reached
//     the select.
//  4. Pre-fix: `default:` already fired; hookDone is closed. FAIL.
//     Post-fix: goroutine is parked; hookDone is open.
//  5. Drain sentinel; assert the hook's sample is then delivered.
func TestHandleAssignmentChanged_BlockingSendDelivers(t *testing.T) {
	startLatencyCh := make(chan coordinator.StartLatencyReport, 1)
	startLatencyCh <- coordinator.StartLatencyReport{WorkerID: "sentinel"}

	w := &Worker{
		id:                  "test-worker",
		ctx:                 context.Background(),
		startLatencyCh:      startLatencyCh,
		startCalledAt:       time.Now().Add(-100 * time.Millisecond),
		assignmentReportCh:  nil, // not under test
		coordinatorReportCh: nil,
		firstAssignmentOK:   atomic.Bool{},
	}

	hookDone := make(chan struct{})
	go func() {
		_ = w.handleAssignmentChanged(context.Background(), nil, []types.Partition{
			{Keys: []string{"0"}, Weight: 1},
		})
		close(hookDone)
	}()

	// Give the goroutine enough time to reach the select. After this
	// point, pre-fix code has already exited via `default:`.
	time.Sleep(200 * time.Millisecond)

	select {
	case <-hookDone:
		t.Fatal("hook returned without delivering sample — buggy default: branch fired (pre-fix code path)")
	default:
		// Expected: goroutine parked on the blocking send.
	}

	// Drain the sentinel; this releases the parked send.
	select {
	case rep := <-startLatencyCh:
		if rep.WorkerID != "sentinel" {
			t.Fatalf("expected sentinel first, got %q (test setup wrong)", rep.WorkerID)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("could not drain sentinel; channel state unexpected")
	}

	select {
	case rep := <-startLatencyCh:
		if rep.WorkerID != "test-worker" {
			t.Errorf("WorkerID = %q, want %q", rep.WorkerID, "test-worker")
		}
		if rep.Latency <= 0 {
			t.Errorf("Latency = %v, want positive", rep.Latency)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("did not receive StartLatencyReport within 1s after draining sentinel")
	}

	select {
	case <-hookDone:
	case <-time.After(1 * time.Second):
		t.Fatal("handleAssignmentChanged did not return after receive")
	}
}

// TestHandleAssignmentChanged_StampsInitialCohortFlag proves that the
// per-worker IsInitialCohort flag is carried into the StartLatencyReport.
// The coordinator's classifier depends on this stamp to bucket samples
// without racing against baselineLocked flips — a slow chaos-delayed
// initial-cohort worker must still land in the initial bucket regardless
// of channel-receive timing.
func TestHandleAssignmentChanged_StampsInitialCohortFlag(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cohort bool
	}{
		{"initial", true},
		{"takeover", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan coordinator.StartLatencyReport, 1)
			w := &Worker{
				id:                "test-worker",
				ctx:               context.Background(),
				startLatencyCh:    ch,
				startCalledAt:     time.Now().Add(-50 * time.Millisecond),
				firstAssignmentOK: atomic.Bool{},
				isInitialCohort:   tc.cohort,
			}
			_ = w.handleAssignmentChanged(context.Background(), nil, []types.Partition{
				{Keys: []string{"0"}, Weight: 1},
			})
			select {
			case rep := <-ch:
				if rep.IsInitialCohort != tc.cohort {
					t.Errorf("IsInitialCohort = %v, want %v", rep.IsInitialCohort, tc.cohort)
				}
			case <-time.After(1 * time.Second):
				t.Fatal("no StartLatencyReport received")
			}
		})
	}
}

// TestHandleAssignmentChanged_CancellationDoesNotWedge proves that the
// blocking send respects ctx.Done() and does not hang on shutdown when
// the coordinator has stopped draining.
//
// Setup: zero-buffer channel, no receiver, cancel the worker's ctx
// before invoking the hook. Assert the hook returns promptly.
func TestHandleAssignmentChanged_CancellationDoesNotWedge(t *testing.T) {
	startLatencyCh := make(chan coordinator.StartLatencyReport) // unbuffered, no receiver

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call

	w := &Worker{
		id:                  "test-worker",
		ctx:                 ctx,
		startLatencyCh:      startLatencyCh,
		startCalledAt:       time.Now().Add(-100 * time.Millisecond),
		assignmentReportCh:  nil,
		coordinatorReportCh: nil,
		firstAssignmentOK:   atomic.Bool{},
	}

	done := make(chan struct{})
	go func() {
		_ = w.handleAssignmentChanged(ctx, nil, []types.Partition{
			{Keys: []string{"0"}, Weight: 1},
		})
		close(done)
	}()

	select {
	case <-done:
		// Pass — handler returned without wedging.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handleAssignmentChanged wedged after ctx cancellation")
	}
}
