package failure_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestDegradedRecovery_Rearm_NoRace stresses the degraded-recovery re-arm side
// effect (scheduleApplyRetry issued from the connection-monitor goroutine) added
// by the F-D3 follow-up, concurrently with assignment-version churn. The worker
// is held in the real uncommitted-Degraded state by a DUAL write fault: claims/*
// on the handoff bucket (keeps the assignment uncommitted) plus all writes on
// the heartbeat bucket (drives Degraded via the KV-error threshold circuit, which
// fires from any state — the startup-timeout watchdog does not, because a
// single-worker leader's calculator leaves WaitingAssignment within ~1s). While
// degraded, the source churns its partition set so the assignment version
// advances repeatedly; each recovery tick re-arms scheduleApplyRetry against the
// then-current version, racing the monitor goroutine against the watcher/apply
// paths that share nats.go cached *stream state. Only a live worker under -race
// can find such races. Pass condition: the run completes with no DATA RACE.
func TestDegradedRecovery_Rearm_NoRace(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	realJS, err := jetstream.New(nc)
	require.NoError(t, err)
	fc := &wfFaultController{}
	faultJS := newWFFaultJetStreamDual(realJS, fc)

	rfBuildStream(t, ctx, realJS)

	src := newWFMutableSource([]types.Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}})

	// Claim-write fault armed before Start so the worker can never latch; the
	// heartbeat fault is armed after Start so the initial heartbeat succeeds and
	// the background publisher's periodic puts then drive the KV-error circuit.
	fc.ArmWrites()
	stack := rfBuildWorkerStackCfg(t, ctx, faultJS, src, 0,
		func(cfg *parti.Config) {
			cfg.DegradedBehavior.KVErrorThreshold = 3
			cfg.DegradedBehavior.ExitThreshold = 200 * time.Millisecond
		},
		nil,
	)
	fc.ArmHeartbeat()

	require.NoError(t, <-stack.mgr.WaitState(parti.StateDegraded, 30*time.Second),
		"KV-error circuit must drive the worker to Degraded so recovery ticks run")

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Driver: churn the source's partition set so the assignment version advances
	// repeatedly during the held fault. Each advance is what the recovery re-arm
	// reads as cur after the refresh — racing scheduleApplyRetry (monitor
	// goroutine) against the watcher/apply paths.
	wg.Go(func() {
		toggle := true
		for {
			select {
			case <-stop:
				return
			default:
				if toggle {
					src.set([]types.Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}, {Keys: []string{"p2"}}})
				} else {
					src.set([]types.Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}})
				}
				toggle = !toggle
				time.Sleep(20 * time.Millisecond)
			}
		}
	})

	time.Sleep(5 * time.Second)
	close(stop)
	wg.Wait()
	fc.DisarmWrites()

	// Pass condition: the run completes with no DATA RACE. The race detector marks
	// the test failed at detection time; t.Failed() is the in-body signal. No
	// functional assertion is needed — the per-claim CAS arbitrates concurrent
	// writers; this test only guards the new monitor-goroutine side effect.
	require.False(t, t.Failed(), "race detector tripped during the recovery re-arm stress run")
}
