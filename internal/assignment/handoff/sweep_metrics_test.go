package handoff

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// sweepPassEvent captures one IncClaimSweepPass call.
type sweepPassEvent struct {
	origin  string
	outcome string
	reason  string
}

// recordingSweepMetrics implements the full HandoffMetricsRecorder surface
// via the embedded no-op plus the optional HandoffSweepMetricsRecorder
// capability, recording every sweep-pass event in order.
type recordingSweepMetrics struct {
	types.NopHandoffMetricsRecorder

	mu     sync.Mutex
	events []sweepPassEvent
}

func (r *recordingSweepMetrics) IncClaimSweepPass(origin, outcome, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, sweepPassEvent{origin: origin, outcome: outcome, reason: reason})
}

func (r *recordingSweepMetrics) snapshot() []sweepPassEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.events)
}

// bareHandoffMetrics implements HandoffMetricsRecorder WITHOUT the optional
// sweep capability — deliberately not embedding the no-op, which now carries
// IncClaimSweepPass and would satisfy the capability by accident.
type bareHandoffMetrics struct{}

func (bareHandoffMetrics) IncHandoffTotal(string)                     {}
func (bareHandoffMetrics) ObserveHandoffDuration(time.Duration)       {}
func (bareHandoffMetrics) ObservePhaseDuration(string, time.Duration) {}
func (bareHandoffMetrics) IncCASConflicts()                           {}
func (bareHandoffMetrics) SetClaimStoreSize(int)                      {}
func (bareHandoffMetrics) IncClaimStoreStale()                        {}
func (bareHandoffMetrics) IncClaimStaleHandoffReset()                 {}

// TestSweepPassCounter_OriginsAndReasons drives the sweep pipeline through
// every counter emission point and pins the exact (origin, outcome, reason)
// sequence: apply-origin ungated full pass, ticker unlatched-then-latching
// full pass, a confirmed cached pass, a position-mismatch full pass, the
// max-skips forced full pass, and a probe-error fail-open full pass. The
// sequence is EXACT (require.Equal on the whole slice), so any stray
// emission fails the test. The remaining reasons and the emits-nothing
// exclusions are pinned by TestSweepPassCounter_RemainingReasons and
// TestSweepPassCounter_NonAdmittedEmitsNothing.
func TestSweepPassCounter_OriginsAndReasons(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	h := newSweepGateHarness(t, time.Hour)
	rec := &recordingSweepMetrics{}
	h.coord.sweepMetrics = rec
	h.seedStable(t, "p1")

	// 1. Apply-origin passes bypass the gate entirely: full, "ungated".
	h.advance(time.Second)
	h.coord.maybeSweepClaims(ctx, sweepOriginApply)

	// 2. First ticker pass has no cache to skip against: full, "unlatched"
	// (and it latches the clean position).
	h.tick(ctx)

	// 3. Quiet bucket, double-probe confirms: cached pass.
	h.tick(ctx)

	// 4. An external write advances the position: full, "mismatch".
	h.store.advancePos()
	h.tick(ctx)

	// 5. One cached pass, then the forced backstop at sweepMaxSkips=1.
	h.coord.sweepMaxSkips = 1
	h.tick(ctx) // cached (skips -> 1)
	h.tick(ctx) // forced full

	// 6. Probe failure fails open: full, "probe_error".
	h.store.probeErr = errors.New("probe boom")
	h.tick(ctx)

	require.Equal(t, []sweepPassEvent{
		{origin: "apply", outcome: "full", reason: "ungated"},
		{origin: "ticker", outcome: "full", reason: "unlatched"},
		{origin: "ticker", outcome: "cached", reason: ""},
		{origin: "ticker", outcome: "full", reason: "mismatch"},
		{origin: "ticker", outcome: "cached", reason: ""},
		{origin: "ticker", outcome: "full", reason: "forced"},
		{origin: "ticker", outcome: "full", reason: "probe_error"},
	}, rec.snapshot())
}

// TestSweepPassCounter_WiredThroughNew pins the construction-time seam: a
// Config.Metrics recorder that implements the optional capability is
// type-asserted and receives events without any other wiring.
func TestSweepPassCounter_WiredThroughNew(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	rec := &recordingSweepMetrics{}
	coord, ok := New(Config{
		Store:         newProbeMemStore(),
		TTL:           time.Minute,
		SweepInterval: -1,
		Metrics:       rec,
	}, true).(*twoPhaseCoordinator)
	require.True(t, ok)

	coord.maybeSweepClaims(ctx, sweepOriginApply)
	require.Equal(t, []sweepPassEvent{
		{origin: "apply", outcome: "full", reason: "ungated"},
	}, rec.snapshot())
}

// TestSweepPassCounter_RecorderWithoutCapability pins the optionality
// contract: a recorder implementing only the base HandoffMetricsRecorder
// leaves the coordinator's capability handle nil, and the sweep pipeline —
// gated and ungated arms alike — runs unchanged without panicking.
func TestSweepPassCounter_RecorderWithoutCapability(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	coord, ok := New(Config{
		Store:         newProbeMemStore(),
		TTL:           time.Minute,
		SweepInterval: -1,
		Metrics:       bareHandoffMetrics{},
	}, true).(*twoPhaseCoordinator)
	require.True(t, ok)
	require.Nil(t, coord.sweepMetrics, "a base-only recorder must not satisfy the sweep capability")

	coord.sweepConfirmGap = time.Millisecond
	coord.maybeSweepClaims(ctx, sweepOriginApply)  // ungated arm
	coord.maybeSweepClaims(ctx, sweepOriginTicker) // full-pass arm (unlatched)
	coord.maybeSweepClaims(ctx, sweepOriginTicker) // cached arm
}

// TestSweepPassCounter_RemainingReasons closes the documented full-pass
// reason set: "unsafe_config" (a probed position failing the scan-gate
// config guard) and "no_probe_handle" (a store advertising the probe
// capability without a live handle — which also permanently ungates the
// coordinator, so the NEXT pass is "ungated").
func TestSweepPassCounter_RemainingReasons(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	t.Run("unsafe_config", func(t *testing.T) {
		t.Parallel()
		h := newSweepGateHarness(t, time.Hour)
		rec := &recordingSweepMetrics{}
		h.coord.sweepMetrics = rec

		// MaxAge on the claim bucket breaks the gate's cache-coherence
		// assumption (entries can vanish without a position change).
		h.store.setPos(func(p *natsutil.KVStreamPos) { p.MaxAge = time.Hour })
		h.tick(ctx)
		require.Equal(t, []sweepPassEvent{
			{origin: "ticker", outcome: "full", reason: "unsafe_config"},
		}, rec.snapshot())
	})

	t.Run("no_probe_handle", func(t *testing.T) {
		t.Parallel()
		rec := &recordingSweepMetrics{}
		coord, ok := New(Config{
			Store:         &noHandleStore{memStore: newMemStore()},
			TTL:           time.Minute,
			SweepInterval: -1,
			Metrics:       rec,
		}, true).(*twoPhaseCoordinator)
		require.True(t, ok)

		coord.maybeSweepClaims(ctx, sweepOriginTicker) // drops the prober for good
		coord.maybeSweepClaims(ctx, sweepOriginTicker) // now permanently ungated
		require.Equal(t, []sweepPassEvent{
			{origin: "ticker", outcome: "full", reason: "no_probe_handle"},
			{origin: "ticker", outcome: "full", reason: "ungated"},
		}, rec.snapshot())
	})
}

// TestSweepPassCounter_NonAdmittedEmitsNothing pins the emits-nothing
// exclusions: the counter counts passes that RUN a body, so the store-nil
// return, a single-flight TryLock miss, the interval throttle, and the
// confirm-wait abort (admitted, but no body ran) must all record zero
// events.
func TestSweepPassCounter_NonAdmittedEmitsNothing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	t.Run("store nil", func(t *testing.T) {
		t.Parallel()
		rec := &recordingSweepMetrics{}
		coord, ok := New(Config{SweepInterval: -1, Metrics: rec}, true).(*twoPhaseCoordinator)
		require.True(t, ok)

		require.False(t, coord.maybeSweepClaims(ctx, sweepOriginApply))
		require.Empty(t, rec.snapshot())
	})

	t.Run("trylock miss", func(t *testing.T) {
		t.Parallel()
		h := newSweepGateHarness(t, time.Hour)
		rec := &recordingSweepMetrics{}
		h.coord.sweepMetrics = rec

		// Hold the single-flight lock directly: an opportunistic pass
		// arriving while another sweep is mid-body skips, uncounted.
		h.coord.sweepMu.Lock()
		require.False(t, h.coord.maybeSweepClaims(ctx, sweepOriginApply))
		h.coord.sweepMu.Unlock()
		require.Empty(t, rec.snapshot())
	})

	t.Run("interval throttle", func(t *testing.T) {
		t.Parallel()
		rec := &recordingSweepMetrics{}
		fixed := time.Now().UTC()
		coord, ok := New(Config{
			Store:         newProbeMemStore(),
			TTL:           time.Minute,
			SweepInterval: time.Hour,
			Now:           func() time.Time { return fixed },
			Metrics:       rec,
		}, true).(*twoPhaseCoordinator)
		require.True(t, ok)

		require.True(t, coord.maybeSweepClaims(ctx, sweepOriginApply))
		require.False(t, coord.maybeSweepClaims(ctx, sweepOriginApply), "second pass within the interval is throttled")
		require.Equal(t, []sweepPassEvent{
			{origin: "apply", outcome: "full", reason: "ungated"},
		}, rec.snapshot(), "the throttled attempt must not emit")
	})

	t.Run("confirm-wait abort", func(t *testing.T) {
		t.Parallel()
		h := newSweepGateHarness(t, time.Hour)
		rec := &recordingSweepMetrics{}
		h.coord.sweepMetrics = rec

		// Latch a clean cache first (full "unlatched" pass), then drive a
		// ticker pass with a cancelled context: probe 1 confirms the cached
		// position, and the confirm wait aborts on ctx.Done — admitted,
		// but neither pass body runs, so nothing is emitted for it.
		h.tick(ctx)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		h.advance(time.Second)
		require.True(t, h.coord.maybeSweepClaims(cancelled, sweepOriginTicker))
		require.Equal(t, []sweepPassEvent{
			{origin: "ticker", outcome: "full", reason: "unlatched"},
		}, rec.snapshot(), "the aborted confirm-wait pass must not emit")
	})
}
