package durable

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// fakeProbe is a scriptable stream-position probe. If script is
// non-empty, calls pop results from it in order; once exhausted (or if
// empty), calls return the steady pos/err.
type fakeProbe struct {
	mu     sync.Mutex
	pos    natsutil.KVStreamPos
	err    error
	calls  int
	script []probeResult
}

type probeResult struct {
	pos natsutil.KVStreamPos
	err error
}

func (f *fakeProbe) probe(context.Context) (natsutil.KVStreamPos, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.script) > 0 {
		r := f.script[0]
		f.script = f.script[1:]

		return r.pos, r.err
	}

	return f.pos, f.err
}

func (f *fakeProbe) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

func (f *fakeProbe) set(pos natsutil.KVStreamPos) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pos = pos
}

func (f *fakeProbe) push(results ...probeResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script, results...)
}

func gatePos(seq, msgs uint64) natsutil.KVStreamPos {
	return natsutil.KVStreamPos{Created: time.Unix(1000, 0), LastSeq: seq, Msgs: msgs}
}

// newGateResolver wires a resolver over kv with an injected probe, a
// short confirm gap, and manual reconcile driving (interval 0 disables
// the loop; tests call reconcileOnce directly).
func newGateResolver(kv jetstream.KeyValue, p *fakeProbe, logger *captureLogger) *ClaimBasedResolver {
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	if logger != nil {
		r.logger = logger
	}
	r.streamPos = p.probe
	r.gateConfirmGap = time.Millisecond

	return r
}

func TestGateSkipsWhenPosUnchanged(t *testing.T) {
	t.Parallel()

	kv := newHealthyKV(t)
	p := &fakeProbe{pos: gatePos(5, 1)}
	r := newGateResolver(kv, p, nil)
	ctx := context.Background()

	r.reconcileOnce(ctx) // full pass: lists, latches
	require.EqualValues(t, 1, kv.keysCalls.Load())

	for i := 0; i < 5; i++ {
		r.reconcileOnce(ctx)
	}
	require.EqualValues(t, 1, kv.keysCalls.Load(), "gated ticks must not list")
	owner, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok)
	require.Equal(t, quorumTestOwner, owner)
}

func TestGateFullScanOnSeqAdvance(t *testing.T) {
	t.Parallel()

	kv := newHealthyKV(t)
	p := &fakeProbe{pos: gatePos(5, 1)}
	r := newGateResolver(kv, p, nil)
	ctx := context.Background()

	r.reconcileOnce(ctx) // latch
	r.reconcileOnce(ctx) // skip
	require.EqualValues(t, 1, kv.keysCalls.Load())

	// A mutation: new claim body at revision 6, LastSeq advances.
	kv.store[quorumTestFullKey] = marshalClaim(t, handoff.Claim{
		PartitionID: quorumTestPID,
		Owner:       "worker-B",
		State:       handoff.ClaimStateStable,
		Epoch:       2,
	})
	kv.revision = 6
	p.set(gatePos(6, 1))

	r.reconcileOnce(ctx) // mismatch: full pass, re-latch
	require.EqualValues(t, 2, kv.keysCalls.Load())
	owner, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok)
	require.Equal(t, "worker-B", owner)

	r.reconcileOnce(ctx) // skip again at the new latch
	require.EqualValues(t, 2, kv.keysCalls.Load())
}

func TestGateFullScanOnMsgsDrop(t *testing.T) {
	t.Parallel()

	kv := newHealthyKV(t)
	p := &fakeProbe{pos: gatePos(5, 1)}
	r := newGateResolver(kv, p, nil)
	ctx := context.Background()

	r.reconcileOnce(ctx) // latch with the claim present

	// Invisible removal: same Created and LastSeq, Msgs drops, key gone.
	delete(kv.store, quorumTestFullKey)
	p.set(gatePos(5, 0))

	r.reconcileOnce(ctx)
	require.EqualValues(t, 2, kv.keysCalls.Load(), "Msgs drop must force a full pass")
	_, _, _, ok := r.GetOwner(quorumTestPID)
	require.False(t, ok, "vanished key must be tombstoned")
}

func TestGateFullScanOnCreatedChange(t *testing.T) {
	t.Parallel()

	kv := newHealthyKV(t)
	p := &fakeProbe{pos: gatePos(5, 1)}
	r := newGateResolver(kv, p, nil)
	ctx := context.Background()

	r.reconcileOnce(ctx) // latch

	recreated := gatePos(5, 1)
	recreated.Created = time.Unix(2000, 0)
	p.set(recreated)

	r.reconcileOnce(ctx)
	require.EqualValues(t, 2, kv.keysCalls.Load(), "bucket recreate must force a full pass")
}

func TestGateDisabledOnUnsafeConfig(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*natsutil.KVStreamPos){
		"max_age":       func(p *natsutil.KVStreamPos) { p.MaxAge = time.Hour },
		"allow_msg_ttl": func(p *natsutil.KVStreamPos) { p.AllowMsgTTL = true },
		"marker_ttl":    func(p *natsutil.KVStreamPos) { p.SubjectDeleteMarkerTTL = time.Minute },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			kv := newHealthyKV(t)
			unsafePos := gatePos(5, 1)
			mutate(&unsafePos)
			p := &fakeProbe{pos: unsafePos}
			cl := &captureLogger{}
			r := newGateResolver(kv, p, cl)
			ctx := context.Background()

			for i := 0; i < 3; i++ {
				r.reconcileOnce(ctx)
			}
			require.EqualValues(t, 3, kv.keysCalls.Load(), "unsafe config must full-pass every tick")
			// "gate disabled" appears only in the Warn line, not in the
			// per-tick Debug line, so this pins edge-triggering.
			require.Equal(t, 1, cl.count("gate disabled"),
				"exactly one Warn on the safe→unsafe transition")

			// Config restored: one Info, then latching resumes.
			p.set(gatePos(5, 1))
			r.reconcileOnce(ctx) // clean full pass, latches
			require.True(t, cl.has("config restored"))
			r.reconcileOnce(ctx) // skip
			require.EqualValues(t, 4, kv.keysCalls.Load())
		})
	}
}

func TestGateFailsOpenOnProbeError(t *testing.T) {
	t.Parallel()

	kv := newHealthyKV(t)
	p := &fakeProbe{pos: gatePos(5, 1)}
	r := newGateResolver(kv, p, nil)
	ctx := context.Background()

	r.reconcileOnce(ctx) // latch
	p.mu.Lock()
	p.err = context.DeadlineExceeded
	p.mu.Unlock()

	r.reconcileOnce(ctx)
	r.reconcileOnce(ctx)
	require.EqualValues(t, 3, kv.keysCalls.Load(), "probe errors must full-pass every tick")

	// Probe recovers at the latched position: the error invalidated the
	// latch, so the next tick must still be a full pass (one clean pass
	// before skipping resumes).
	p.mu.Lock()
	p.err = nil
	p.mu.Unlock()
	r.reconcileOnce(ctx) // full pass, re-latches
	require.EqualValues(t, 4, kv.keysCalls.Load())
	r.reconcileOnce(ctx) // skip
	require.EqualValues(t, 4, kv.keysCalls.Load())
}

func TestGateDoesNotLatchOnUnreadableKeys(t *testing.T) {
	t.Parallel()

	kv := newHealthyKV(t)
	kv.getErrByKey = map[string]error{quorumTestFullKey: context.DeadlineExceeded}
	p := &fakeProbe{pos: gatePos(5, 1)}
	r := newGateResolver(kv, p, nil)
	ctx := context.Background()

	r.reconcileOnce(ctx) // unclean: unreadable key, must not latch
	r.reconcileOnce(ctx) // pos unchanged, but unlatched ⇒ full pass
	require.EqualValues(t, 2, kv.keysCalls.Load())

	kv.getErrByKey = nil
	r.reconcileOnce(ctx) // clean full pass, latches
	r.reconcileOnce(ctx) // skip
	require.EqualValues(t, 3, kv.keysCalls.Load())
}

func TestGateForcedFullPassAfterMaxSkips(t *testing.T) {
	t.Parallel()

	kv := newHealthyKV(t)
	p := &fakeProbe{pos: gatePos(5, 1)}
	r := newGateResolver(kv, p, nil)
	ctx := context.Background()

	// Tick 1 latches (full). Ticks 2..20 skip (19 skips). Tick 21 is
	// forced full. Then the budget renews.
	for i := 0; i < 21; i++ {
		r.reconcileOnce(ctx)
	}
	require.EqualValues(t, 2, kv.keysCalls.Load(), "exactly one forced full pass per 20 gated ticks")
	for i := 0; i < 19; i++ {
		r.reconcileOnce(ctx)
	}
	require.EqualValues(t, 2, kv.keysCalls.Load())
	r.reconcileOnce(ctx)
	require.EqualValues(t, 3, kv.keysCalls.Load())
}

func TestGateSkipDoesNotEmitRescueOrDriftRestart(t *testing.T) {
	t.Parallel()

	kv := newHealthyKV(t)
	p := &fakeProbe{pos: gatePos(5, 1)}
	r := newGateResolver(kv, p, nil)
	ms := newMetricsSpy()
	r.SetMetrics(ms)
	r.driftRestartCooldown = time.Nanosecond // would fire if a rescue happened
	seedResolverCache(r)                     // cache already in sync with KV
	ctx := context.Background()

	r.reconcileOnce(ctx) // full pass over in-sync state: no rescue
	for i := 0; i < 5; i++ {
		r.reconcileOnce(ctx) // skips
	}
	require.Zero(t, ms.reconcileRescueCount())
	require.Zero(t, r.lastDriftRestartNano.Load())
}

func TestGateRecoveryEquivalence(t *testing.T) {
	t.Parallel()

	// Silent watcher stall is the default in these tests: no watcher
	// runs at all. A mutation + probe advance must converge on the next
	// tick, exactly like an ungated reconciler.
	kv := newHealthyKV(t)
	p := &fakeProbe{pos: gatePos(5, 1)}
	r := newGateResolver(kv, p, nil)
	ctx := context.Background()

	r.reconcileOnce(ctx) // latch
	r.reconcileOnce(ctx) // skip

	kv.store[quorumTestFullKey] = marshalClaim(t, handoff.Claim{
		PartitionID: quorumTestPID,
		Owner:       "worker-C",
		State:       handoff.ClaimStateStable,
		Epoch:       3,
	})
	kv.revision = 6
	p.set(gatePos(6, 1))

	r.reconcileOnce(ctx) // the very next tick converges
	owner, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok)
	require.Equal(t, "worker-C", owner)
}

func TestGateDoubleProbeConfirms(t *testing.T) {
	t.Parallel()

	kv := newHealthyKV(t)
	p := &fakeProbe{pos: gatePos(5, 1)}
	r := newGateResolver(kv, p, nil)
	ctx := context.Background()

	r.reconcileOnce(ctx) // full pass: exactly 1 probe
	require.Equal(t, 1, p.callCount())

	r.reconcileOnce(ctx) // gated tick: first probe + confirm probe
	require.Equal(t, 3, p.callCount(), "a skip requires two probes")
	require.EqualValues(t, 1, kv.keysCalls.Load())
}

func TestGateSecondProbeMismatchRunsFull(t *testing.T) {
	t.Parallel()

	kv := newHealthyKV(t)
	p := &fakeProbe{pos: gatePos(6, 1)} // steady = truth after the mutation
	r := newGateResolver(kv, p, nil)
	ctx := context.Background()

	// Pass 1 latches the OLD state at seq 5.
	p.push(probeResult{pos: gatePos(5, 1)})
	r.reconcileOnce(ctx)
	owner, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok)
	require.Equal(t, quorumTestOwner, owner, "cache must hold the pre-mutation owner")

	// The hidden mutation lands AFTER the latch (watcher stalled: the
	// cache does not see it).
	kv.store[quorumTestFullKey] = marshalClaim(t, handoff.Claim{
		PartitionID: quorumTestPID,
		Owner:       "worker-B",
		State:       handoff.ClaimStateStable,
		Epoch:       2,
	})
	kv.revision = 6

	// Deposed-leader race: first probe answers the stale latched pos,
	// the confirm probe (post step-down) answers the truth.
	p.push(probeResult{pos: gatePos(5, 1)}, probeResult{pos: gatePos(6, 1)})
	r.reconcileOnce(ctx) // confirm mismatch ⇒ full pass converges NOW
	require.EqualValues(t, 2, kv.keysCalls.Load())
	owner, _, _, ok = r.GetOwner(quorumTestPID)
	require.True(t, ok)
	require.Equal(t, "worker-B", owner, "the mismatching confirm probe must rescue the stale cache")

	r.reconcileOnce(ctx) // steady pos (6): skip resumes at the new latch
	require.EqualValues(t, 2, kv.keysCalls.Load())
}

func TestGateStaleProbeBoundedByOneInterval(t *testing.T) {
	t.Parallel()

	kv := newHealthyKV(t)
	p := &fakeProbe{pos: gatePos(6, 1)} // steady = truth after the mutation
	r := newGateResolver(kv, p, nil)
	ctx := context.Background()

	// Pass 1 latches the OLD state at seq 5.
	p.push(probeResult{pos: gatePos(5, 1)})
	r.reconcileOnce(ctx)
	owner, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok)
	require.Equal(t, quorumTestOwner, owner)

	// Hidden mutation AFTER the latch (watcher stalled).
	kv.store[quorumTestFullKey] = marshalClaim(t, handoff.Claim{
		PartitionID: quorumTestPID,
		Owner:       "worker-B",
		State:       handoff.ClaimStateStable,
		Epoch:       2,
	})
	kv.revision = 6

	// Worst-case triple-fault corner: BOTH probes of the next tick
	// answer the stale latched pos (double race loss). That tick
	// wrongly skips — the documented residual — and the cache REALLY IS
	// stale across it.
	p.push(probeResult{pos: gatePos(5, 1)}, probeResult{pos: gatePos(5, 1)})
	r.reconcileOnce(ctx)
	require.EqualValues(t, 1, kv.keysCalls.Load(), "the corner tick skips")
	owner, _, _, ok = r.GetOwner(quorumTestPID)
	require.True(t, ok)
	require.Equal(t, quorumTestOwner, owner, "suppression is real: the cache is stale for this one interval")

	// The NEXT tick's probe answers the truth ⇒ full pass, converge.
	// Suppression bounded to exactly one interval.
	r.reconcileOnce(ctx)
	require.EqualValues(t, 2, kv.keysCalls.Load())
	owner, _, _, ok = r.GetOwner(quorumTestPID)
	require.True(t, ok)
	require.Equal(t, "worker-B", owner)
}

func TestGateConfirmWaitAbortsOnStop(t *testing.T) {
	t.Parallel()

	kv := newHealthyKV(t)
	p := &fakeProbe{pos: gatePos(5, 1)}
	r := newGateResolver(kv, p, nil)
	r.gateConfirmGap = 30 * time.Second // Stop must not wait this out
	ctx := context.Background()

	r.reconcileOnce(ctx) // latch

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.reconcileOnce(ctx) // enters the confirm gap
	}()
	require.Eventually(t, func() bool { return p.callCount() >= 2 },
		2*time.Second, time.Millisecond, "tick must take its first probe")
	r.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile pass did not abort promptly on Stop")
	}
	require.EqualValues(t, 1, kv.keysCalls.Load(), "aborted pass must not scan")
}
