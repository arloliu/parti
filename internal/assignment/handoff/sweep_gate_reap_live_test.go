package handoff

// In-package live-NATS proof of the handoff sweep gate's two longest-horizon
// backstops, neither of which any CI run otherwise reaches (~10 minutes at
// production defaults):
//
//  1. the forced full pass every sweepMaxSkips+1 gated (cached) ticks, even
//     while the bucket stays byte-idle; and
//  2. orphan-claim reaping after OrphanGrace elapses WHILE the scan gate is
//     actively latched — the cached/gated ticker pass is a legitimate
//     finalizer of the clock-driven reap arm.
//
// sweepMaxSkips is a per-instance seam defaulting to
// natsutil.ScanGateMaxSkippedPasses (19); only in-package tests shorten it.
// OrphanGrace is already a settable Config field (production wires the 10m
// const), so no seam is needed there — the test just configures it short.

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// sweepArgLogger records each message together with its structured args so
// a test can count full passes carrying a specific reason (the plain
// sweepCaptureLogger keeps only the message). Safe for the sweep goroutine
// to write while the test goroutine reads.
type sweepArgLogger struct {
	mu      sync.Mutex
	entries []sweepArgEntry
}

type sweepArgEntry struct {
	msg string
	kv  []any
}

var _ types.Logger = (*sweepArgLogger)(nil)

func (l *sweepArgLogger) log(msg string, kv ...any) {
	l.mu.Lock()
	l.entries = append(l.entries, sweepArgEntry{msg: msg, kv: kv})
	l.mu.Unlock()
}

func (l *sweepArgLogger) Debug(msg string, kv ...any) { l.log(msg, kv...) }
func (l *sweepArgLogger) Info(msg string, kv ...any)  { l.log(msg, kv...) }
func (l *sweepArgLogger) Warn(msg string, kv ...any)  { l.log(msg, kv...) }
func (l *sweepArgLogger) Error(msg string, kv ...any) { l.log(msg, kv...) }
func (l *sweepArgLogger) Fatal(msg string, kv ...any) { l.log(msg, kv...) }

func (l *sweepArgLogger) countMsg(msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.entries {
		if e.msg == msg {
			n++
		}
	}

	return n
}

func (l *sweepArgLogger) countReason(msg, reason string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.entries {
		if e.msg != msg {
			continue
		}
		for i := 0; i+1 < len(e.kv); i += 2 {
			k, kok := e.kv[i].(string)
			v, vok := e.kv[i+1].(string)
			if kok && vok && k == "reason" && v == reason {
				n++
			}
		}
	}

	return n
}

// TestSweepGateLive_ForcedPassAndOrphanReap proves both sweep-gate
// backstops against a real embedded NATS server. A reap-eligible orphan
// (stable claim whose partition is absent from the vouched live set) and a
// live stable claim are seeded; the bucket then stays byte-idle so the gate
// latches. The test asserts (i) forced full passes fire on schedule while
// idle and (ii) the orphan is reaped once OrphanGrace elapses despite the
// gate being actively latched, while the live claim survives untouched.
func TestSweepGateLive_ForcedPassAndOrphanReap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	// partitest (not internal/testutil) is used to start embedded NATS: an
	// in-package handoff test cannot import internal/testutil, which pulls
	// in the root parti package that itself imports this package (cycle).
	// partitest depends only on the leaf types package.
	srv, nc := partitest.StartEmbeddedNATS(t)
	defer func() {
		nc.Close()
		srv.Shutdown()
		srv.WaitForShutdown()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := "sweep-gate-live-reap"
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	// Dedicated probe handle for the sweep gate — never the handle the
	// coordinator's store reads/writes through (kv.Status mutates shared
	// *stream state under concurrent ops; the epoch-monitor race class).
	probeKV, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)

	store := NewNATSClaimStoreWithProbe(kv, probeKV, "claims/")

	// Dedicated sanity handle for the test's own reads (ListKeys spins up a
	// throwaway ordered consumer that mutates its handle's stream state), so
	// it is never a handle a live component owns.
	sanityKV, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)
	sanityStore := NewNATSClaimStore(sanityKV, "claims/")

	const (
		livePID   = "pLive"
		orphanPID = "pOrphan"
	)

	now := time.Now()
	_, err = store.PutIfEpoch(ctx, livePID, 0, NewInitialClaim(livePID, "worker-live", now, time.Minute))
	require.NoError(t, err)
	_, err = store.PutIfEpoch(ctx, orphanPID, 0, NewInitialClaim(orphanPID, "worker-gone", now, time.Minute))
	require.NoError(t, err)

	// Confirm both claims exist before starting — so a later "orphan gone"
	// is a genuine reap, not a claim that was never written.
	keys, err := sanityStore.ListKeys(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{livePID, orphanPID}, keys)

	log := &sweepArgLogger{}
	// vouchedLive answers ok=true with only livePID present, so orphanPID is
	// continuously absent from the vouched set and thus reap-eligible.
	vouchedLive := func(ctx context.Context) (map[string]struct{}, bool) {
		return map[string]struct{}{livePID: {}}, true
	}

	coord, ok := New(Config{
		Store:          store,
		Logger:         log,
		TTL:            time.Minute,
		SweepInterval:  100 * time.Millisecond,
		OrphanGrace:    1 * time.Second,
		LivePartitions: vouchedLive,
		// Now defaults to time.Now — the grace clock advances on the real
		// clock, so the reap can only fire after ~1s of real sweeps.
	}, true).(*twoPhaseCoordinator)
	require.True(t, ok)

	// In-package seams: shorten the confirm gap so the gate latches on a
	// test timescale, and shrink the skip budget so the forced-pass backstop
	// fires within a couple of seconds instead of ~10 minutes.
	coord.sweepConfirmGap = 20 * time.Millisecond
	coord.sweepMaxSkips = 4

	coord.Start(ctx)

	const (
		fullPassMsg = "handoff sweep full pass"
		engagedMsg  = "handoff sweep gate engaged"
		reapMsg     = "reaped orphan claim"
	)

	// (ii) The orphan must be reaped once OrphanGrace elapses, even though
	// the gate is actively latched over the idle bucket. Poll a dedicated
	// handle for the orphan key disappearing.
	require.Eventually(t, func() bool {
		ks, lerr := sanityStore.ListKeys(ctx)
		if lerr != nil {
			return false
		}

		return !slices.Contains(ks, orphanPID)
	}, 8*time.Second, 100*time.Millisecond,
		"the orphan claim must be reaped after OrphanGrace under an active gate")

	// The live claim must survive: reaping only ever touches the absent
	// partition's claim.
	survivors, err := sanityStore.ListKeys(ctx)
	require.NoError(t, err)
	require.Contains(t, survivors, livePID, "the live claim must survive the sweep")
	require.NotContains(t, survivors, orphanPID, "the orphan claim must be gone")

	// The reap must have been logged (the Info line), confirming it went
	// through the reaper rather than some other deletion.
	require.GreaterOrEqual(t, log.countMsg(reapMsg), 1, "the reaper must log the orphan reap")

	// (i) Forced full passes must fire on schedule while the bucket is idle.
	// With sweepMaxSkips=4 a forced pass fires every 5th cached tick; after
	// the reap the bucket is idle again and the gate re-latches, so forced
	// passes keep accruing. Two is well within a few seconds.
	require.Eventually(t, func() bool {
		return log.countReason(fullPassMsg, "forced") >= 2
	}, 8*time.Second, 100*time.Millisecond,
		"the forced-pass backstop must fire at least twice over the idle window")

	require.GreaterOrEqual(t, log.countReason(fullPassMsg, "forced"), 2,
		"expected >=2 forced full passes across the idle window")

	// The gate must genuinely latch (engage) rather than fail open every
	// tick — this is what makes the reap-under-active-gate claim non-vacuous.
	require.GreaterOrEqual(t, log.countMsg(engagedMsg), 2,
		"the sweep gate must engage (latch) repeatedly, not fail open every tick")
}
