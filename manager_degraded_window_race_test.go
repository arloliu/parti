package parti

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDegradedRecord_EnterRecoverRace_NoTornRecord is the widened-window guard.
// Collapsing degradedSince + lastDegradedReason into one atomic.Pointer moves the
// record-visible point back to the entry CAS, widening the PRE-EXISTING window
// where the record is published but transitionState(StateDegraded) has not yet run.
// It must not introduce a new race class. This drives the real recovery path
// (attemptRecoveryFromDegraded, which can reach exitDegraded here because the
// commitment guard is satisfied and the reason is non-reason-scoped) concurrently
// with re-entry (enterDegraded) and a heartbeat-success stamp that advances the
// recovery signal inside the window, under -race, and asserts:
//   - a loaded record is always nil or fully populated (never a torn since/reason),
//   - the storm completes without panic or deadlock, and
//   - the storm never strands in Degraded with a nil record.
//
// That last assertion is the end-to-end guard for the exit-on-confirmed-Degraded
// fix: a recovery tick that hits the window (record published, transition not yet
// run) used to vacuously transition Stable->Stable and clear the in-flight record,
// stranding the worker in Degraded+nil-record. exitDegraded now clears only on a
// genuine Degraded->Stable CAS, so that strand can no longer occur regardless of
// interleaving (the storm may legitimately end Degraded+record or Stable+nil, but
// never Degraded+nil). On the pre-fix parent this assertion is reachable-RED.
func TestDegradedRecord_EnterRecoverRace_NoTornRecord(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-NATS concurrency stress in short mode")
	}
	t.Parallel()

	snap := Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p0"}}}}
	committed := snap
	m, _ := armDegraded(t, &committed, snap) // committed == snapshot: commitment guard passes
	plantAssignment(t, m, snap)
	m.cfg.DegradedAlert.AlertInterval = time.Minute // keep the spawned alert monitors quiet
	m.isLeader.Store(false)                         // non-leader exit skips the recovery-grace goroutine + enumeration gate

	const iters = 150
	var wg sync.WaitGroup
	var bad sync.Map

	// Recoverers: the real exit path (refresh + commitment guard + backstops -> exit).
	for range 2 {
		wg.Go(func() {
			for range iters {
				m.attemptRecoveryFromDegraded()
			}
		})
	}
	// Re-enterers: re-arm with a non-reason-scoped reason after each exit.
	for range 2 {
		wg.Go(func() {
			for range iters {
				m.enterDegraded("startup-timeout")
			}
		})
	}
	// Heartbeat-success stamps: advance the recovery signal inside the window.
	wg.Go(func() {
		for range iters {
			m.recordKVHealthyOp()
		}
	})
	// Readers: record-atomicity oracle.
	for range 2 {
		wg.Go(func() {
			for range iters {
				if rec := m.degraded.Load(); rec != nil {
					if rec.since == 0 || rec.reason == "" {
						bad.Store(rec.reason, struct{}{})
					}
				}
			}
		})
	}
	wg.Wait()

	bad.Range(func(k, _ any) bool {
		t.Errorf("reader observed a torn degraded record while enter/recover raced: reason=%q", k)
		return true
	})
	require.False(t, t.Failed(),
		"the widened enter/transition window must not produce a torn record or a hang")

	// End-to-end exit-on-confirmed-Degraded guard: after the storm drains, the
	// worker must never sit in Degraded with a nil record — the strand the
	// from-Degraded exit CAS closes. Degraded+record (last op was an enter) and
	// Stable+nil (last op was a genuine exit) are both fine.
	require.False(t, m.State() == StateDegraded && m.degraded.Load() == nil,
		"enter/recover storm stranded the worker in Degraded with a nil record")
}
