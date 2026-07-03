package durable

// In-package live-NATS proof of the reconcile scan gate's longest-horizon
// backstop: a full pass is FORCED at least every gateMaxSkips+1 gated ticks
// even while the bucket stays byte-idle, so a mis-latched gate can never
// suppress the reconciler indefinitely. The gate's unit suite proves this
// with a fake probe and a scripted counter; this test proves it against a
// real embedded NATS server and the real streamPos probe.
//
// gateMaxSkips is a per-instance seam defaulting to
// natsutil.ScanGateMaxSkippedPasses (19). Only in-package tests shorten it
// (here to 4) so the forced-pass backstop is reachable on a test timescale
// rather than the ~10 minutes a production budget would take.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// argLogger is a types.Logger that records each message together with its
// structured key/value args, so a test can count full passes carrying a
// specific reason (the plain captureLogger keeps only the message and
// cannot distinguish reason="forced" from reason="mismatch"). Safe for the
// reconciler goroutine to write while the test goroutine reads.
type argLogger struct {
	mu      sync.Mutex
	entries []argEntry
}

type argEntry struct {
	msg string
	kv  []any
}

var _ types.Logger = (*argLogger)(nil)

func (l *argLogger) log(msg string, kv ...any) {
	l.mu.Lock()
	l.entries = append(l.entries, argEntry{msg: msg, kv: kv})
	l.mu.Unlock()
}

func (l *argLogger) Debug(msg string, kv ...any) { l.log(msg, kv...) }
func (l *argLogger) Info(msg string, kv ...any)  { l.log(msg, kv...) }
func (l *argLogger) Warn(msg string, kv ...any)  { l.log(msg, kv...) }
func (l *argLogger) Error(msg string, kv ...any) { l.log(msg, kv...) }
func (l *argLogger) Fatal(msg string, kv ...any) { l.log(msg, kv...) }

// countMsg counts recorded entries whose message equals msg.
func (l *argLogger) countMsg(msg string) int {
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

// countReason counts recorded entries whose message equals msg and whose
// args carry the pair "reason" == reason.
func (l *argLogger) countReason(msg, reason string) int {
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

// TestGateLive_ForcedFullPassBackstop proves the reconcile gate's
// forced-full-pass backstop end-to-end. With a shortened skip budget the
// resolver, faced with a perfectly byte-idle bucket (no writes after the
// seed), must still run a full pass on schedule: at least once every
// gateMaxSkips+1 gated ticks, with the gate re-engaging between forced
// passes. This is the longest-horizon guarantee of the scan gate that no
// CI run otherwise reaches (~10 minutes at production defaults).
func TestGateLive_ForcedFullPassBackstop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := "gate-live-forcedpass"
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	// Seed one claim, then never touch the bucket again — this is what
	// keeps the backing stream position byte-identical so the gate latches
	// and stays latched until the forced-pass budget trips it.
	seed := handoff.NewInitialClaim("pA", "w1", time.Now(), time.Minute)
	seedBytes, err := seed.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pA", seedBytes)
	require.NoError(t, err)

	// Dedicated probe handle — never shared with the resolver's live kv
	// handle, per the WithStreamPosProbe handle-ownership contract.
	probeKV, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)

	cl := &argLogger{}
	r := NewClaimBasedResolver(kv, "claims/", cl,
		WithReconcileInterval(100*time.Millisecond),
		WithStreamPosProbe(probeKV),
	)
	// In-package seams: shorten the confirm gap so the gate latches on a
	// test timescale, and shrink the skip budget so the forced-pass
	// backstop fires within a couple of seconds instead of ~10 minutes.
	r.gateConfirmGap = 20 * time.Millisecond
	r.gateMaxSkips = 4
	t.Cleanup(r.Stop)

	require.NoError(t, r.Start(ctx))

	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("pA")

		return ok && owner == "w1"
	}, 3*time.Second, 20*time.Millisecond, "resolver did not resolve pA at startup")

	const fullPassMsg = "claim resolver reconcile full pass"
	const engagedMsg = "claim resolver reconcile gate engaged"

	// With gateMaxSkips=4 a forced full pass fires every 5th gated tick
	// (~500ms). Wait for at least two forced passes; the bucket is idle,
	// so the ONLY thing that can produce these full passes is the
	// forced-pass budget tripping — a mismatch is impossible when nothing
	// writes to the bucket.
	require.Eventually(t, func() bool {
		return cl.countReason(fullPassMsg, "forced") >= 2
	}, 10*time.Second, 50*time.Millisecond,
		"the forced-pass backstop must fire at least twice over an idle window")

	forced := cl.countReason(fullPassMsg, "forced")
	require.GreaterOrEqual(t, forced, 2,
		"expected >=2 forced full passes across the idle window")

	// The gate must re-engage between forced passes: each forced pass
	// resets the skip counter, and the next gated skip logs "gate engaged"
	// again. At least two engagements proves the gate genuinely latched
	// repeatedly rather than failing open into a full pass every tick.
	require.GreaterOrEqual(t, cl.countMsg(engagedMsg), 2,
		"the gate must re-engage between forced passes (not permanently failed open)")

	// The bucket never changed, so every full pass after the first latch is
	// a FORCED pass — there must be no mismatch-driven pass to explain them
	// away.
	require.Zero(t, cl.countReason(fullPassMsg, "mismatch"),
		"an idle bucket must never produce a mismatch-driven full pass")

	// The resolver keeps serving the seeded owner throughout — the forced
	// passes reconcile the same idle state without dropping ownership.
	owner, _, _, ok := r.GetOwner("pA")
	require.True(t, ok)
	require.Equal(t, "w1", owner)
}
