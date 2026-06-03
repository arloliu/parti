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
//   - a loaded record is always nil or fully populated (never a torn since/reason), and
//   - the storm completes without panic or deadlock.
//
// Out of scope (documented; pre-existing on BOTH the two-atomic parent and this
// pointer design): a recovery tick can vacuously transition Stable->Stable in the
// window and clear a record an enterer is mid-publishing, ending Degraded with a
// nil record. Closing that needs an exit-on-confirmed-Degraded guard — a separate
// behaviour change. So this asserts record atomicity + liveness, NOT absolute
// state-vs-record consistency.
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
}
