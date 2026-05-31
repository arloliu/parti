package failure_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestFD1_IntermittentKVTimeouts_NoFlap pins the F-D1 success-reset fix
// (Finding B). It drives an INTERMITTENT connected-but-KV-unavailable pattern on
// the heartbeat bucket — each fault blip is deliberately sized BELOW the
// KV-error threshold, but the blips fall within a single KVErrorWindow. A worker
// that never resets its error counter on a healthy KV op (the parent behavior)
// SUMS the blips across the window and trips Degraded even though no single blip
// is sustained enough to warrant it. The fix clears the transient (F-D1) error
// entries on each successful heartbeat publish while not degraded, giving the
// circuit consecutive-error semantics: only a sustained run with no intervening
// success trips.
//
// Sizing (TestConfig HeartbeatInterval=500ms, KVErrorThreshold=10,
// KVErrorWindow=30s):
//   - blipOn=3s  => ~6 faulted heartbeats per blip, < threshold 10.
//   - blipOff=3s => the heartbeat recovers and emits a success (which, with the
//     fix, clears the prior blip's transient entries within one interval).
//   - cycles=3   => parent accrues ~18 errors inside the 30s window and trips;
//     the fix keeps each blip independent (~6 < 10) and never trips.
//
// On the parent base this FAILS: the cross-blip accumulation drives the worker
// Degraded (flaps >= 1). With the fix it is GREEN (flaps == 0).
func TestFD1_IntermittentKVTimeouts_NoFlap(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	const (
		blipOn   = 3 * time.Second // ~6 faulted heartbeats: below threshold 10
		blipOff  = 3 * time.Second // heal long enough for a success-reset
		cycles   = 3               // ~18 errors total inside the 30s window
		healTail = 8 * time.Second // final heal so the worker ends Stable
	)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	realJS, err := jetstream.New(nc)
	require.NoError(t, err)
	fc := &wfFaultController{}
	// Fault only the heartbeat bucket (all writes, DeadlineExceeded) — the pure
	// F-D1 connected-but-KV-unavailable trigger. The NATS connection stays up.
	faultJS := newWFFaultJetStreamDual(realJS, fc)

	rfBuildStream(t, ctx, realJS)
	src := newWFMutableSource([]types.Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}})

	var flaps atomic.Int64
	stack := rfBuildWorkerStackCfg(t, ctx, faultJS, src, 0,
		func(cfg *parti.Config) {
			// One blip (~6 errors) must stay below threshold; the cross-blip
			// sum (~18) must exceed it within the window.
			cfg.DegradedBehavior.KVErrorThreshold = 10
			cfg.DegradedBehavior.KVErrorWindow = 30 * time.Second
		},
		func(h *parti.Hooks) {
			h.OnDegraded = func(_ context.Context, _ string) error {
				flaps.Add(1)
				return nil
			}
		},
	)

	require.NoError(t, <-stack.mgr.WaitState(parti.StateStable, 30*time.Second),
		"worker must reach Stable before the intermittent fault pattern")

	for range cycles {
		fc.ArmHeartbeat()
		time.Sleep(blipOn)
		fc.DisarmWrites()
		time.Sleep(blipOff)
	}

	// Non-vacuity: the heartbeat fault must have actually fired across the blips,
	// otherwise flaps==0 would pass trivially.
	require.GreaterOrEqual(t, fc.heartbeatFaultsInjected.Load(), int64(cycles*4),
		"heartbeat fault never fired enough times — the probe is vacuous")

	// The fix: intermittent sub-threshold blips with a healthy gap between them
	// must NOT degrade. On the parent base the cross-blip accumulation trips.
	require.Equal(t, int64(0), flaps.Load(),
		"intermittent sub-threshold KV timeouts with recovery between blips must not enter Degraded")

	// And the worker must still be Stable after a final heal.
	require.NoError(t, <-stack.mgr.WaitState(parti.StateStable, healTail),
		"worker must remain Stable after the intermittent fault pattern clears")
}
