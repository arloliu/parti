package parti

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Degraded-record atomicity (Family B). The active degrade {since, reason} pair is
// published as ONE atomic.Pointer[degradedRecord] swap:
//   - enterDegraded builds the full record and CompareAndSwap(nil, rec)s it; a
//     loser's CAS fails on the non-nil record, so it never clobbers the winner.
//   - exitDegraded Store(nil)s after a successful state transition.
//   - a reader therefore observes either nil or a fully-populated record — never a
//     partial pair (since set but reason empty). The empty-reason recovery gate the
//     two-atomic design needed to paper over a post-CAS-pre-store window is gone.
//
// These tests pin that invariant. The first is a deterministic clobber-resistance
// proof; the second is a concurrent multi-reason storm whose oracles are the race
// detector AND the nil-or-fully-populated reader check; the third is a
// non-vacuity guard showing that check would flag a partial record.

// TestReasonOwnership_LosersDoNotClobberWinningReason proves that when many
// goroutines call enterDegraded concurrently with DIFFERENT reasons, exactly one
// wins the CAS and publishes its record; every loser's CAS fails on the non-nil
// record and discards its own. A non-winner reason surviving here would mean the
// reason-scoped recovery gate could key off the wrong reason and falsely exit.
// Deterministic (no sleeps); also runs under -race.
func TestReasonOwnership_LosersDoNotClobberWinningReason(t *testing.T) {
	t.Parallel()

	m, _, _, _ := newTestManager(t)
	m.cfg.DegradedAlert.AlertInterval = time.Minute // monitorDegradedAlerts NewTicker
	m.state.Store(int32(StateStable))               // Stable -> Degraded is valid

	const winner = DegradeReasonKVUnavailable
	m.enterDegraded(winner)
	require.Equal(t, StateDegraded, m.State(), "winner must transition to Degraded")
	rec := m.degraded.Load()
	require.NotNil(t, rec, "winner must publish a degraded record")
	require.NotZero(t, rec.since, "winner's record carries the degrade-entry time")
	require.Equal(t, winner, rec.reason, "the CAS winner is the sole record publisher")

	// N concurrent losers, each with a DISTINCT reason. All lose the CAS (the record
	// is already non-nil), so none may replace the winner's record.
	const losers = 64
	var wg sync.WaitGroup
	for i := range losers {
		reason := fmt.Sprintf("loser-reason-%d", i)
		wg.Go(func() { m.enterDegraded(reason) })
	}
	wg.Wait()

	got := m.degraded.Load()
	require.NotNil(t, got)
	require.Equal(t, winner, got.reason,
		"a losing concurrent enterDegraded must NOT replace the winner's record "+
			"(CAS(nil,&rec) fails on the live record)")
	require.Equal(t, StateDegraded, m.State(), "losers must not change state")
}

// TestReasonOwnership_ConcurrentMultiReasonStorm_NoRace hammers the full protocol
// — concurrent enterDegraded (5 distinct reasons), exitDegraded, recordKVHealthyOp
// (the lastHeartbeatSuccessAt stamp), plus readers of the gate state. Oracles: the
// race detector AND the record-atomicity invariant — whenever the loaded record is
// non-nil it is FULLY populated (since != 0 AND reason is one of the reasons we
// entered with), never a partial pair and never "". A split set-since-then-set-
// reason publish (the rejected two-atomic design) would let a reader observe an
// empty reason here; the single swap makes that unrepresentable.
func TestReasonOwnership_ConcurrentMultiReasonStorm_NoRace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrency stress in short mode")
	}
	t.Parallel()

	m, _, _, _ := newTestManager(t)
	m.cfg.DegradedAlert.AlertInterval = time.Minute
	m.state.Store(int32(StateStable))

	reasons := []string{
		DegradeReasonKVUnavailable,
		"startup-timeout",
		"NATS connection down",
		"bucket-recreated:parti-heartbeat",
		"assignment-watcher-exhausted",
	}
	valid := make(map[string]bool, len(reasons))
	for _, r := range reasons {
		valid[r] = true
	}

	const iters = 300
	var wg sync.WaitGroup
	var bad sync.Map // observed bad record description -> struct{}

	// Enterers: one goroutine per reason, each repeatedly attempting entry.
	for _, r := range reasons {
		wg.Go(func() {
			for range iters {
				m.enterDegraded(r)
			}
		})
	}
	// Exiters: drive Degraded -> Stable so the enterers can re-arm, exercising the
	// transition-then-clear ordering under concurrency.
	for range 3 {
		wg.Go(func() {
			for range iters {
				m.exitDegraded()
			}
		})
	}
	// Heartbeat-success stampers (the lastHeartbeatSuccessAt atomic on the hot path).
	for range 2 {
		wg.Go(func() {
			for range iters {
				m.recordKVHealthyOp()
			}
		})
	}
	// Readers: load the record once and assert it is nil or fully populated.
	for range 3 {
		wg.Go(func() {
			for range iters {
				if rec := m.degraded.Load(); rec != nil {
					if rec.since == 0 || rec.reason == "" || !valid[rec.reason] {
						bad.Store(fmt.Sprintf("since=%d reason=%q", rec.since, rec.reason), struct{}{})
					}
				}
				_ = m.lastHeartbeatSuccessAt.Load()
			}
		})
	}
	wg.Wait()

	bad.Range(func(k, _ any) bool {
		t.Errorf("reader observed a non-atomic / invalid degraded record: %s", k)
		return true
	})
	require.False(t, t.Failed(), "race detector / record-atomicity invariant tripped during the multi-reason storm")
}

// TestDegradedRecord_PartialIsCaught is the non-vacuity guard for the storm's
// record-atomicity oracle: it constructs the partial record a split two-step
// publish would transiently expose (since set, reason empty) and confirms the same
// nil-or-fully-populated check flags it. Production never publishes such a record
// (enterDegraded builds it whole before the CAS); this only proves the oracle is
// discriminating rather than vacuously green.
func TestDegradedRecord_PartialIsCaught(t *testing.T) {
	t.Parallel()

	m, _, _, _ := newTestManager(t)
	m.degraded.Store(&degradedRecord{since: time.Now().UnixNano(), reason: ""})

	rec := m.degraded.Load()
	require.NotNil(t, rec)
	partial := rec.since == 0 || rec.reason == ""
	require.True(t, partial,
		"a since-without-reason record IS representable as a value, so the storm "+
			"reader's check would flag it; the single-swap publish is what prevents "+
			"production from ever creating one")
}
