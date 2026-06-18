package parti

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestManager_recordKVError(t *testing.T) {
	logger := logging.NewNop()
	nopMetrics := metrics.NewNop()
	cfg := Config{
		DegradedBehavior: DegradedBehaviorConfig{
			KVErrorThreshold: 3,
			KVErrorWindow:    10 * time.Second,
		},
		DegradedAlert: DegradedAlertConfig{
			AlertInterval: 1 * time.Minute,
		},
	}

	t.Run("ignores nil error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.recordKVError(nil)
		require.Equal(t, int32(0), m.kvErrorCount.Load())
	})

	t.Run("ignores non-connectivity error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.recordKVError(nats.ErrInvalidArg) // Not a connectivity error
		require.Equal(t, int32(0), m.kvErrorCount.Load())
	})

	t.Run("records connectivity error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.recordKVError(nats.ErrTimeout)
		require.Equal(t, int32(1), m.kvErrorCount.Load())
	})

	t.Run("records degrading jetstream error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.recordKVError(jetstream.ErrBucketNotFound)
		require.Equal(t, int32(1), m.kvErrorCount.Load())
	})

	t.Run("triggers degraded mode", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.state.Store(int32(StateStable)) // must be a state that allows Degraded entry

		// Trigger threshold
		for range 3 {
			m.recordKVError(nats.ErrTimeout)
		}
		// Cancel to stop the monitorDegradedAlerts goroutine started by enterDegraded
		cancel()
		m.wg.Wait()

		require.Equal(t, int32(3), m.kvErrorCount.Load())
		require.NotZero(t, m.degradedSinceNano())
		require.Equal(t, StateDegraded, m.State())
	})

	t.Run("ignores stream-missing errors", func(t *testing.T) {
		// Cross-feature contract pin: stream-missing exhaustion is
		// routed through the dynamic-consumer observer to
		// enterDegraded("stream-missing-recovery-exhausted"), NOT
		// through the generic KV-error threshold. A stream-missing
		// error that incidentally wraps jetstream.ErrStreamNotFound
		// (which natsutil treats as a degrading-JetStream error) must
		// be short-circuited here so it does not double-count.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		// Wrap mirroring partition_consumer.go's wrapping pattern:
		// types.ErrStreamMissing PLUS the degrading-JetStream cause.
		wrapped := fmt.Errorf("%w: stream %q: %w", types.ErrStreamMissing, "TEST", jetstream.ErrStreamNotFound)
		m.recordKVError(wrapped)
		require.Equal(t, int32(0), m.kvErrorCount.Load(),
			"stream-missing errors must not count against the KV threshold")
		require.Empty(t, m.kvErrorWindow,
			"stream-missing errors must not append to the KV error window")
	})

	t.Run("resets on success", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.recordKVError(nats.ErrTimeout)
		require.Equal(t, int32(1), m.kvErrorCount.Load())

		m.recordKVSuccess()
		require.Equal(t, int32(0), m.kvErrorCount.Load())
		require.Empty(t, m.kvErrorWindow)
	})
}

// TestManager_recordKVHealthyOp pins the F-D1 healthy-op success-reset: a
// successful periodic KV op while not degraded clears ONLY the transient
// (connected-but-KV-unavailable) error entries, leaving whole-bucket-loss
// entries to accumulate. This is what gives the F-D1 circuit consecutive-error
// semantics without masking a whole-bucket loss when an unaffected bucket keeps
// succeeding.
func TestManager_recordKVHealthyOp(t *testing.T) {
	logger := logging.NewNop()
	nopMetrics := metrics.NewNop()
	cfg := Config{
		DegradedBehavior: DegradedBehaviorConfig{
			// High threshold so recording errors never trips Degraded here —
			// this test isolates the window bookkeeping, not the trip path.
			KVErrorThreshold: 100,
			KVErrorWindow:    10 * time.Second,
		},
		DegradedAlert: DegradedAlertConfig{AlertInterval: 1 * time.Minute},
	}

	t.Run("clears only transient entries", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		m.recordKVError(nats.ErrTimeout)                  // whole-bucket (connectivity)
		m.recordKVOpError(context.DeadlineExceeded)       // transient (F-D1)
		m.recordKVOpError(context.DeadlineExceeded)       // transient (F-D1)
		require.Equal(t, int32(3), m.kvErrorCount.Load()) // sanity

		m.recordKVHealthyOp()

		require.Equal(t, int32(1), m.kvErrorCount.Load(),
			"only the whole-bucket entry must survive a healthy op")
		require.Len(t, m.kvErrorWindow, 1)
		require.False(t, m.kvErrorWindow[0].transient,
			"the surviving entry must be the whole-bucket-loss one")
	})

	t.Run("no-op while degraded", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		// Record BEFORE marking degraded (recordKVError short-circuits while degraded).
		m.recordKVError(nats.ErrTimeout)            // whole-bucket
		m.recordKVOpError(context.DeadlineExceeded) // transient
		require.Equal(t, int32(2), m.kvErrorCount.Load())

		m.markDegraded(time.Now().UnixNano(), DegradeReasonNATSConnectionDown)
		m.recordKVHealthyOp()

		require.Equal(t, int32(2), m.kvErrorCount.Load(),
			"a healthy op while degraded must not touch the window")
		require.Len(t, m.kvErrorWindow, 2)
	})

	t.Run("empty window is a no-op", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		m.recordKVHealthyOp()
		require.Equal(t, int32(0), m.kvErrorCount.Load())
		require.Empty(t, m.kvErrorWindow)
	})
}

func TestManager_enterDegraded_rejectsShutdown(t *testing.T) {
	logger := logging.NewNop()
	nopMetrics := metrics.NewNop()
	cfg := Config{
		DegradedAlert: DegradedAlertConfig{
			AlertInterval: 1 * time.Minute,
		},
	}

	t.Run("blocks degraded entry from Shutdown state", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.state.Store(int32(StateShutdown))

		m.enterDegraded("test: should be rejected")

		require.Equal(t, StateShutdown, m.State(),
			"enterDegraded must not override StateShutdown")
		require.Zero(t, m.degradedSinceNano(),
			"the degraded record must remain unset when enterDegraded is rejected")
	})

	t.Run("allows degraded entry from Stable state", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
		m.state.Store(int32(StateStable))

		m.enterDegraded("test: should be allowed")

		// Cancel context to stop monitorDegradedAlerts goroutine
		cancel()
		m.wg.Wait()

		require.Equal(t, StateDegraded, m.State())
		require.NotZero(t, m.degradedSinceNano())
	})
}

func TestManager_exitDegraded_safeWithCAS(t *testing.T) {
	logger := logging.NewNop()
	nopMetrics := metrics.NewNop()
	cfg := Config{
		DegradedAlert: DegradedAlertConfig{
			AlertInterval: 1 * time.Minute,
		},
	}

	t.Run("does not overwrite Shutdown with Stable", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		// Simulate: entered degraded mode, then Stop set StateShutdown
		m.markDegraded(time.Now().UnixNano(), DegradeReasonNATSConnectionDown)
		m.state.Store(int32(StateShutdown))

		m.exitDegraded()

		require.Equal(t, StateShutdown, m.State(),
			"exitDegraded must not overwrite StateShutdown with Stable")
	})

	t.Run("exits normally from Degraded to Stable", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}

		m.markDegraded(time.Now().UnixNano(), DegradeReasonNATSConnectionDown)
		m.state.Store(int32(StateDegraded))

		m.exitDegraded()
		m.wg.Wait()

		require.Equal(t, StateStable, m.State())
		require.Zero(t, m.degradedSinceNano())
	})
}

// TestEnterDegraded_ReentryAfterExit verifies enter -> exit -> re-enter works:
// exitDegraded clears the degraded record (Store(nil)), so a later enterDegraded
// wins CompareAndSwap(nil, rec) and re-arms. This locks the re-entry contract that
// an earlier typed-nil atomic.Value storage bug once broke (Store((*time.Time)(nil))
// produced a non-nil interface that permanently blocked re-entry); the pointer
// record makes "cleared" an honest nil, so re-entry cannot be silently blocked.
func TestEnterDegraded_ReentryAfterExit(t *testing.T) {
	logger := logging.NewNop()
	nopMetrics := metrics.NewNop()
	cfg := Config{
		DegradedAlert: DegradedAlertConfig{
			AlertInterval: 1 * time.Minute,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Manager{logger: logger, cfg: cfg, hooks: &Hooks{}, metrics: nopMetrics, ctx: ctx, cancel: cancel}
	m.state.Store(int32(StateStable))

	// First degraded entry
	m.enterDegraded("first failure")
	cancel()    // stop monitorDegradedAlerts
	m.wg.Wait() // wait for goroutines to exit

	require.Equal(t, StateDegraded, m.State(), "must enter Degraded")
	require.NotZero(t, m.degradedSinceNano())

	// Recovery: exit degraded
	m.exitDegraded()
	require.Equal(t, StateStable, m.State(), "must return to Stable")
	require.Zero(t, m.degradedSinceNano(), "the degraded record must be cleared after exit")

	// Re-entry: must succeed after reset (the core bug fix)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	m.ctx = ctx2
	m.cancel = cancel2

	m.enterDegraded("second failure")
	cancel2()
	m.wg.Wait()

	require.Equal(t, StateDegraded, m.State(), "re-entry into Degraded must work after recovery")
	require.NotZero(t, m.degradedSinceNano(), "the degraded record must be set on re-entry")
}

// Test helpers for the degraded-record-pointer state. Production code holds the
// degraded {since, reason} pair as one atomic.Pointer[degradedRecord]; these keep
// the white-box tests that previously poked the two separate atomics concise.

// markDegraded forces the degraded record directly, mirroring the production
// single-swap publish of {since, reason}. Tests that drive enterDegraded through
// the real path do not need this; it is for tests that set up a degraded record
// without a state transition.
func (m *Manager) markDegraded(sinceNano int64, reason string) {
	m.degraded.Store(&degradedRecord{since: sinceNano, reason: reason})
}

// degradedSinceNano returns the degrade-entry UnixNano (0 if not degraded),
// preserving the require.Zero / require.NotZero assertion style that previously
// read the degradedSince atomic directly.
func (m *Manager) degradedSinceNano() int64 {
	if rec := m.degraded.Load(); rec != nil {
		return rec.since
	}

	return 0
}

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

// TestCalculateAlertLevel_BoundaryTable characterizes calculateAlertLevel across
// every alert level and both sides of each threshold band. The function had zero
// coverage; this pins the duration->level mapping AND the highest-first cascade
// order (Critical -> Error -> Warn -> Info) so a consolidation refactor cannot
// reorder the checks or remap a band undetected.
//
// Note on what is (not) locked: time.Since always returns threshold+epsilon for a
// a degrade-start time computed as now-threshold, so the exact >=/> boundary at a single
// nanosecond is below the observable resolution and is intentionally NOT asserted
// (no operator degrades for exactly 30.000000000s). The load-bearing behavior is
// the band mapping and the cascade ORDER, both of which this table locks.
func TestCalculateAlertLevel_BoundaryTable(t *testing.T) {
	t.Parallel()
	m, _, _, _ := newTestManager(t)
	m.cfg.DegradedAlert = DegradedAlertConfig{
		InfoThreshold:     30 * time.Second,
		WarnThreshold:     2 * time.Minute,
		ErrorThreshold:    5 * time.Minute,
		CriticalThreshold: 10 * time.Minute,
		AlertInterval:     time.Minute,
	}

	belowInfo := AlertLevelInfo - 1
	cases := []struct {
		name     string
		dur      time.Duration
		expected AlertLevel
	}{
		{"zero", 0, belowInfo},
		{"below-info", 29 * time.Second, belowInfo},
		{"at-info", 30 * time.Second, AlertLevelInfo},
		{"info-band", 90 * time.Second, AlertLevelInfo},
		{"just-below-warn", 119 * time.Second, AlertLevelInfo},
		{"at-warn", 2 * time.Minute, AlertLevelWarn},
		{"warn-band", 4 * time.Minute, AlertLevelWarn},
		{"just-below-error", 299 * time.Second, AlertLevelWarn},
		{"at-error", 5 * time.Minute, AlertLevelError},
		{"error-band", 8 * time.Minute, AlertLevelError},
		{"just-below-critical", 599 * time.Second, AlertLevelError},
		{"at-critical", 10 * time.Minute, AlertLevelCritical},
		{"above-critical", time.Hour, AlertLevelCritical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := m.calculateAlertLevel(time.Now().Add(-tc.dur))
			require.Equal(t, tc.expected, got, "duration %s", tc.dur)
		})
	}
}

// alertRecordingMetrics captures the alert-emission metric calls so the
// level->name mapping in emitDegradedAlert is observable without timing.
type alertRecordingMetrics struct {
	*metrics.NopMetrics
	lastLevel int
	lastName  string
	emitted   int
}

func (r *alertRecordingMetrics) SetAlertLevel(level int)           { r.lastLevel = level }
func (r *alertRecordingMetrics) IncrementAlertEmitted(name string) { r.lastName = name; r.emitted++ }

// TestEmitDegradedAlert_LevelNameMapping pins emitDegradedAlert's level->name
// mapping and that it records both the numeric level metric and the named
// counter exactly once. Observable via the metrics collector seam.
func TestEmitDegradedAlert_LevelNameMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		level AlertLevel
		name  string
	}{
		{AlertLevelInfo, "info"},
		{AlertLevelWarn, "warn"},
		{AlertLevelError, "error"},
		{AlertLevelCritical, "critical"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, _, _, _ := newTestManager(t)
			rec := &alertRecordingMetrics{NopMetrics: metrics.NewNop()}
			m.metrics = rec

			m.emitDegradedAlert(tc.level, time.Now().Add(-time.Minute))

			require.Equal(t, int(tc.level), rec.lastLevel, "numeric alert level metric")
			require.Equal(t, tc.name, rec.lastName, "named alert counter")
			require.Equal(t, 1, rec.emitted, "exactly one emission")
		})
	}
}
