package parti

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Reason-ownership protocol (Family B). The active degrade reason is owned
// atomically with the winning degradedSince CAS:
//   - enterDegraded stores lastDegradedReason ONLY after the winning CAS (losers
//     return at the CAS and never write it).
//   - exitDegraded clears the reason BEFORE clearing degradedSince.
//   - the recovery gate treats an empty reason as "not yet observable" and stays
//     degraded that tick.
//
// These tests pin that protocol. The first is a deterministic clobber-resistance
// proof (it FAILS on the rejected store-before-CAS design); the second is a
// concurrent multi-reason storm whose load-bearing oracle is the race detector
// (none of NP-3b / NP-5 / NP-9 drives multiple distinct degrade reasons racing
// into the CAS).

// TestReasonOwnership_LosersDoNotClobberWinningReason proves the P0 correction:
// when many goroutines call enterDegraded concurrently with DIFFERENT reasons,
// exactly one wins the degradedSince CAS and stores its reason; every loser must
// return at the failed CAS WITHOUT writing lastDegradedReason. Storing the reason
// before the CAS (the rejected design) would let a loser overwrite the winner's
// reason — which would make the reason-scoped recovery gate key off the wrong
// reason and falsely exit. Deterministic (no sleeps); also runs under -race.
func TestReasonOwnership_LosersDoNotClobberWinningReason(t *testing.T) {
	t.Parallel()

	m, _, _, _ := newTestManager(t)
	m.cfg.DegradedAlert.AlertInterval = time.Minute // monitorDegradedAlerts NewTicker
	m.state.Store(int32(StateStable))               // Stable -> Degraded is valid

	const winner = degradedReasonKVUnavailable
	m.enterDegraded(winner)
	require.Equal(t, StateDegraded, m.State(), "winner must transition to Degraded")
	require.NotZero(t, m.degradedSince.Load(), "winner must set degradedSince")
	require.Equal(t, winner, m.lastDegradedReason.Load(),
		"the CAS winner is the sole reason writer")

	// N concurrent losers, each with a DISTINCT reason. All lose the CAS
	// (degradedSince is already set), so none may write lastDegradedReason.
	const losers = 64
	var wg sync.WaitGroup
	for i := range losers {
		reason := fmt.Sprintf("loser-reason-%d", i)
		wg.Go(func() { m.enterDegraded(reason) })
	}
	wg.Wait()

	require.Equal(t, winner, m.lastDegradedReason.Load(),
		"a losing concurrent enterDegraded must NOT clobber the winner's active reason "+
			"(store-after-CAS); a non-winner reason here means store-before-CAS regressed")
	require.Equal(t, StateDegraded, m.State(), "losers must not change state")
}

// TestReasonOwnership_ConcurrentMultiReasonStorm_NoRace hammers the full protocol
// — concurrent enterDegraded (5 distinct reasons), exitDegraded, recordKVHealthyOp
// (the lastHeartbeatSuccessAt stamp), plus readers of the gate state — and is
// designed to surface any data race on lastDegradedReason / degradedSince /
// lastHeartbeatSuccessAt or any torn protocol interleaving. Load-bearing oracle:
// the race detector. Light functional invariant: whenever degraded, the observed
// reason is always either "" (the brief CAS-won-but-not-yet-stored / just-exited
// window) or one of the reasons we actually entered with — never stale garbage.
func TestReasonOwnership_ConcurrentMultiReasonStorm_NoRace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrency stress in short mode")
	}
	t.Parallel()

	m, _, _, _ := newTestManager(t)
	m.cfg.DegradedAlert.AlertInterval = time.Minute
	m.state.Store(int32(StateStable))

	reasons := []string{
		degradedReasonKVUnavailable,
		"startup-timeout",
		"NATS connection down",
		"bucket-recreated:parti-heartbeat",
		"assignment-watcher-exhausted",
	}
	valid := map[string]bool{"": true}
	for _, r := range reasons {
		valid[r] = true
	}

	const iters = 300
	var wg sync.WaitGroup
	var invalidReason sync.Map // observed bad reason -> struct{}

	// Enterers: one goroutine per reason, each repeatedly attempting entry.
	for _, r := range reasons {
		wg.Go(func() {
			for range iters {
				m.enterDegraded(r)
			}
		})
	}
	// Exiters: drive Degraded -> Stable so the enterers can re-arm, exercising
	// the clear-reason-before-degradedSince ordering under concurrency.
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
	// Readers: read the gate state the recovery tick reads, validating the reason.
	for range 3 {
		wg.Go(func() {
			for range iters {
				if m.degradedSince.Load() != 0 {
					reason, _ := m.lastDegradedReason.Load().(string)
					if !valid[reason] {
						invalidReason.Store(reason, struct{}{})
					}
				}
				_ = m.lastHeartbeatSuccessAt.Load()
			}
		})
	}
	wg.Wait()

	invalidReason.Range(func(k, _ any) bool {
		t.Errorf("recovery gate observed an invalid degrade reason while degraded: %q", k)
		return true
	})
	require.False(t, t.Failed(), "race detector / invalid-reason invariant tripped during the multi-reason storm")
}
