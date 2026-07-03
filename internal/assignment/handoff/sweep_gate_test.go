package handoff

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/stretchr/testify/require"
)

// probeMemStore is memStore plus the bucketPosProber capability and
// call counters. Writes advance the fake stream position (LastSeq),
// mirroring real KV semantics where every mutation consumes a sequence.
type probeMemStore struct {
	*memStore
	mu         sync.Mutex
	pos        natsutil.KVStreamPos
	probeErr   error
	probeCalls int
	script     []sweepProbeResult
	listCalls  atomic.Int32
	getCalls   atomic.Int32
}

type sweepProbeResult struct {
	pos natsutil.KVStreamPos
	err error
}

func newProbeMemStore() *probeMemStore {
	return &probeMemStore{
		memStore: newMemStore(),
		pos:      natsutil.KVStreamPos{Created: time.Unix(1000, 0), LastSeq: 1, Msgs: 0},
	}
}

func (s *probeMemStore) BucketPos(ctx context.Context) (natsutil.KVStreamPos, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probeCalls++
	if len(s.script) > 0 {
		r := s.script[0]
		s.script = s.script[1:]

		return r.pos, r.err
	}

	return s.pos, s.probeErr
}

func (s *probeMemStore) probeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.probeCalls
}

func (s *probeMemStore) setPos(mutate func(*natsutil.KVStreamPos)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mutate(&s.pos)
}

func (s *probeMemStore) advancePos() {
	s.setPos(func(p *natsutil.KVStreamPos) { p.LastSeq++; p.Msgs++ })
}

func (s *probeMemStore) ListKeys(ctx context.Context) ([]string, error) {
	s.listCalls.Add(1)

	return s.memStore.ListKeys(ctx)
}

func (s *probeMemStore) Get(ctx context.Context, pid string) (Claim, uint64, error) {
	s.getCalls.Add(1)

	return s.memStore.Get(ctx, pid)
}

func (s *probeMemStore) PutIfEpoch(ctx context.Context, pid string, epoch int64, next Claim) (uint64, error) {
	rev, err := s.memStore.PutIfEpoch(ctx, pid, epoch, next)
	if err == nil {
		s.advancePos()
	}

	return rev, err
}

func (s *probeMemStore) Delete(ctx context.Context, pid string, rev uint64) error {
	err := s.memStore.Delete(ctx, pid, rev)
	if err == nil {
		// A KV delete writes a tombstone marker: LastSeq advances.
		s.setPos(func(p *natsutil.KVStreamPos) { p.LastSeq++ })
	}

	return err
}

// sweepGateHarness bundles a two-phase coordinator over a probeMemStore
// with a controllable clock and live-set supplier.
type sweepGateHarness struct {
	store     *probeMemStore
	coord     *twoPhaseCoordinator
	mu        sync.Mutex
	now       time.Time
	live      map[string]struct{}
	vouch     bool
	liveCalls atomic.Int32 // LivePartitions resolutions (fresh per pass)
	log       *sweepCaptureLogger
}

// sweepCaptureLogger is a minimal types.Logger capturing messages for
// substring assertions (mirrors internal/durable's captureLogger).
type sweepCaptureLogger struct {
	mu   sync.Mutex
	msgs []string
}

func (l *sweepCaptureLogger) record(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, msg)
}

func (l *sweepCaptureLogger) Debug(msg string, _ ...any) { l.record(msg) }
func (l *sweepCaptureLogger) Info(msg string, _ ...any)  { l.record(msg) }
func (l *sweepCaptureLogger) Warn(msg string, _ ...any)  { l.record(msg) }
func (l *sweepCaptureLogger) Error(msg string, _ ...any) { l.record(msg) }
func (l *sweepCaptureLogger) Fatal(msg string, _ ...any) { l.record(msg) }

func (l *sweepCaptureLogger) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.msgs {
		if strings.Contains(m, substr) {
			n++
		}
	}

	return n
}

func newSweepGateHarness(t *testing.T, grace time.Duration) *sweepGateHarness {
	t.Helper()
	h := &sweepGateHarness{
		store: newProbeMemStore(),
		now:   time.Now().UTC(),
		live:  map[string]struct{}{},
		vouch: true,
		log:   &sweepCaptureLogger{},
	}
	cfg := Config{
		Store:         h.store,
		Logger:        h.log,
		TTL:           time.Minute,
		SweepInterval: -1, // no throttle; tests drive passes directly
		OrphanGrace:   grace,
		Now: func() time.Time {
			h.mu.Lock()
			defer h.mu.Unlock()

			return h.now
		},
		LivePartitions: func(ctx context.Context) (map[string]struct{}, bool) {
			h.liveCalls.Add(1)
			h.mu.Lock()
			defer h.mu.Unlock()
			out := make(map[string]struct{}, len(h.live))
			for k := range h.live {
				out[k] = struct{}{}
			}

			return out, h.vouch
		},
	}
	coord, ok := New(cfg, true).(*twoPhaseCoordinator)
	require.True(t, ok)
	coord.sweepConfirmGap = time.Millisecond
	h.coord = coord

	return h
}

func (h *sweepGateHarness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

func (h *sweepGateHarness) setLive(pids ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.live = make(map[string]struct{}, len(pids))
	for _, p := range pids {
		h.live[p] = struct{}{}
	}
}

func (h *sweepGateHarness) setVouch(v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.vouch = v
}

func (h *sweepGateHarness) tick(ctx context.Context) {
	h.advance(time.Second)
	h.coord.maybeSweepClaims(ctx, sweepOriginTicker)
}

// seedStable writes a stable claim directly into the store and advances
// the fake position, as any external writer would.
func (h *sweepGateHarness) seedStable(t *testing.T, pid string) {
	t.Helper()
	h.mu.Lock()
	now := h.now
	h.mu.Unlock()
	c := NewInitialClaim(pid, "worker-1", now, time.Minute)
	_, err := h.store.PutIfEpoch(context.Background(), pid, 0, c)
	require.NoError(t, err)
}

func TestSweepCachedPassSkipsListAndGets(t *testing.T) {
	t.Parallel()

	h := newSweepGateHarness(t, time.Hour)
	ctx := context.Background()
	h.seedStable(t, "p1")
	h.seedStable(t, "p2")
	h.setLive("p1", "p2")

	h.tick(ctx) // full pass: lists, reads, latches
	require.EqualValues(t, 1, h.store.listCalls.Load())
	baseGets := h.store.getCalls.Load()
	baseLive := h.liveCalls.Load()

	for i := 0; i < 5; i++ {
		h.tick(ctx) // cached passes
	}
	require.EqualValues(t, 1, h.store.listCalls.Load(), "cached passes must not list")
	require.Equal(t, baseGets, h.store.getCalls.Load(), "cached passes must not read per key")
	require.Equal(t, baseLive+5, h.liveCalls.Load(),
		"every cached pass must resolve the live set fresh")
}

func TestSweepExpiryResetFiresFromCachedPass(t *testing.T) {
	t.Parallel()

	h := newSweepGateHarness(t, 0) // reaping off; isolate the expiry arm
	ctx := context.Background()

	// Three stable claims plus one stuck prepare with a short TTL.
	h.seedStable(t, "p1")
	h.seedStable(t, "p2")
	h.seedStable(t, "p3")
	h.mu.Lock()
	now := h.now
	h.mu.Unlock()
	stuck := Claim{
		PartitionID:  "p4",
		Owner:        "worker-1",
		PendingOwner: "worker-2",
		State:        ClaimStatePrepare,
		Epoch:        3,
		LastUpdated:  now,
		TTLSeconds:   2,
	}
	_, err := h.store.PutIfEpoch(ctx, "p4", 0, stuck)
	require.NoError(t, err)

	h.tick(ctx) // full pass latches; prepare not yet expired
	listsAfterLatch := h.store.listCalls.Load()
	getsAfterLatch := h.store.getCalls.Load()

	// Cross the TTL with zero bucket writes: the position is unchanged,
	// so the pass is cached — and the expiry arm must still fire.
	h.advance(3 * time.Second)
	h.coord.maybeSweepClaims(ctx, sweepOriginTicker)

	got, _, err := h.store.Get(ctx, "p4")
	require.NoError(t, err)
	require.Equal(t, ClaimStateStable, got.State, "expired prepare must reset from a cached pass")
	require.Empty(t, got.PendingOwner)
	require.Equal(t, listsAfterLatch, h.store.listCalls.Load(), "the reset must not need a list scan")
	// The only extra reads are updateClaim's fresh CAS-loop read plus
	// this assertion's own Get — far below a full 4-key walk.
	require.LessOrEqual(t, h.store.getCalls.Load()-getsAfterLatch, int32(3))
}

func TestSweepOrphanReapFiresFromCachedPass(t *testing.T) {
	t.Parallel()

	h := newSweepGateHarness(t, 2*time.Second)
	ctx := context.Background()
	h.seedStable(t, "p1")
	h.seedStable(t, "p2")
	h.setLive("p1", "p2")

	h.tick(ctx) // full pass latches

	// The live set stops vouching for p2 — a manager-local change, no
	// bucket write. Cached passes must run the absence clock and reap.
	h.setLive("p1")
	h.tick(ctx) // cached: clock starts
	h.tick(ctx) // cached: grace not yet elapsed (1s)
	h.tick(ctx) // cached: grace elapsed (2s) ⇒ compare-and-delete at cached rev
	_, rev, err := h.store.Get(ctx, "p2")
	require.NoError(t, err)
	require.Zero(t, rev, "orphan must be reaped from a cached pass")
	require.EqualValues(t, 1, h.store.listCalls.Load(), "reap must not need a list scan")
}

func TestSweepFullPassAfterAnyWrite(t *testing.T) {
	t.Parallel()

	h := newSweepGateHarness(t, time.Hour)
	ctx := context.Background()
	h.seedStable(t, "p1")
	h.setLive("p1")
	h.mu.Lock()
	now := h.now
	h.mu.Unlock()
	stuck := Claim{
		PartitionID: "p2", Owner: "worker-1", PendingOwner: "worker-2",
		State: ClaimStatePrepare, Epoch: 3, LastUpdated: now, TTLSeconds: 2,
	}
	_, err := h.store.PutIfEpoch(ctx, "p2", 0, stuck)
	require.NoError(t, err)

	h.tick(ctx) // full pass latches
	h.advance(3 * time.Second)
	h.coord.maybeSweepClaims(ctx, sweepOriginTicker) // cached pass WRITES (expiry reset)
	require.EqualValues(t, 1, h.store.listCalls.Load())

	h.tick(ctx) // the write advanced the position ⇒ full pass rebuilds
	require.EqualValues(t, 2, h.store.listCalls.Load(), "a write during a cached pass must force the next pass full")

	h.tick(ctx) // and the rebuilt latch skips again
	require.EqualValues(t, 2, h.store.listCalls.Load())
}

func TestSweepFullPassEmptyListClearsCacheAndLatches(t *testing.T) {
	t.Parallel()

	h := newSweepGateHarness(t, time.Hour)
	ctx := context.Background()
	h.seedStable(t, "p1")
	h.seedStable(t, "p2")
	h.setLive("p1", "p2")

	h.tick(ctx) // full pass latches a 2-claim cache

	// All claims deleted externally (position advances).
	require.NoError(t, h.store.Delete(ctx, "p1", 1))
	require.NoError(t, h.store.Delete(ctx, "p2", 1))

	h.tick(ctx) // mismatch ⇒ full pass sees the empty list, latches empty
	require.EqualValues(t, 2, h.store.listCalls.Load())

	getsAfter := h.store.getCalls.Load()
	for i := 0; i < 5; i++ {
		h.tick(ctx) // empty cached passes: short-circuit
	}
	require.EqualValues(t, 2, h.store.listCalls.Load(), "an emptied bucket must not full-scan forever")
	require.Equal(t, getsAfter, h.store.getCalls.Load())
}

func TestSweepMsgsDropForcesFullPass(t *testing.T) {
	t.Parallel()

	h := newSweepGateHarness(t, time.Hour)
	ctx := context.Background()
	h.seedStable(t, "p1")
	h.setLive("p1")

	h.tick(ctx) // latch
	h.tick(ctx) // cached
	require.EqualValues(t, 1, h.store.listCalls.Load())

	// Invisible removal: Msgs drops, LastSeq/Created unchanged.
	h.store.setPos(func(p *natsutil.KVStreamPos) { p.Msgs-- })
	h.tick(ctx)
	require.EqualValues(t, 2, h.store.listCalls.Load(), "Msgs drop must force a full pass")
}

func TestSweepUnsafeConfigDisablesGate(t *testing.T) {
	t.Parallel()

	h := newSweepGateHarness(t, time.Hour)
	ctx := context.Background()
	h.seedStable(t, "p1")
	h.setLive("p1")
	h.store.setPos(func(p *natsutil.KVStreamPos) { p.MaxAge = time.Hour })

	for i := 0; i < 3; i++ {
		h.tick(ctx)
	}
	require.EqualValues(t, 3, h.store.listCalls.Load(), "unsafe config must full-pass every tick")
	// Edge-triggered Warn semantics: "gate disabled" appears only in
	// the one Warn line, not in the per-tick Debug lines.
	require.Equal(t, 1, h.log.count("gate disabled"),
		"exactly one Warn on the safe→unsafe transition")

	h.store.setPos(func(p *natsutil.KVStreamPos) { p.MaxAge = 0 })
	h.tick(ctx) // clean full pass latches
	require.Equal(t, 1, h.log.count("config restored"),
		"one Info on the unsafe→safe transition")
	h.tick(ctx) // cached
	require.EqualValues(t, 4, h.store.listCalls.Load())
}

func TestSweepNoProbeHandlePermanentlyUngates(t *testing.T) {
	t.Parallel()

	// A store that advertises the capability but has no probe handle
	// (natsClaimStore built via NewNATSClaimStore): the first ticker
	// pass drops the prober for good — full sweeps, one probe total.
	store := &noHandleStore{memStore: newMemStore()}
	coord, ok := New(Config{
		Store:         store,
		TTL:           time.Minute,
		SweepInterval: -1,
		Now:           time.Now,
	}, true).(*twoPhaseCoordinator)
	require.True(t, ok)
	require.NotNil(t, coord.prober)

	ctx := context.Background()
	c := NewInitialClaim("p1", "worker-1", time.Now().UTC(), time.Minute)
	_, err := store.PutIfEpoch(ctx, "p1", 0, c)
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		coord.maybeSweepClaims(ctx, sweepOriginTicker)
	}
	require.EqualValues(t, 3, store.lists.Load(), "every pass full-sweeps")
	require.EqualValues(t, 1, store.probes.Load(), "the prober is dropped after the first probe")
	require.Nil(t, coord.prober)
}

// noHandleStore advertises bucketPosProber but always reports the
// missing-handle condition, mirroring NewNATSClaimStore without a probe.
type noHandleStore struct {
	*memStore
	lists  atomic.Int32
	probes atomic.Int32
}

func (s *noHandleStore) ListKeys(ctx context.Context) ([]string, error) {
	s.lists.Add(1)

	return s.memStore.ListKeys(ctx)
}

func (s *noHandleStore) BucketPos(context.Context) (natsutil.KVStreamPos, error) {
	s.probes.Add(1)

	return natsutil.KVStreamPos{}, errNoProbeHandle
}

func TestSweepDoesNotLatchOnGetError(t *testing.T) {
	t.Parallel()

	h := newSweepGateHarness(t, time.Hour)
	ctx := context.Background()
	h.seedStable(t, "p1")
	h.setLive("p1")

	// One Get fails during the full pass: no latch, next pass is full.
	failing := &getFailOnceStore{probeMemStore: h.store}
	h.coord.cfg.Store = failing

	h.tick(ctx) // full pass with a Get error ⇒ must not latch
	h.tick(ctx) // still full (nothing latched); the read succeeds now ⇒ latches
	require.EqualValues(t, 2, h.store.listCalls.Load())

	h.tick(ctx) // cached
	require.EqualValues(t, 2, h.store.listCalls.Load())
}

// getFailOnceStore fails exactly the first per-key Get it sees.
type getFailOnceStore struct {
	*probeMemStore
	failed atomic.Bool
}

func (s *getFailOnceStore) Get(ctx context.Context, pid string) (Claim, uint64, error) {
	if s.failed.CompareAndSwap(false, true) {
		return Claim{}, 0, context.DeadlineExceeded
	}

	return s.probeMemStore.Get(ctx, pid)
}

func TestSweepFailsOpenOnProbeError(t *testing.T) {
	t.Parallel()

	h := newSweepGateHarness(t, time.Hour)
	ctx := context.Background()
	h.seedStable(t, "p1")
	h.setLive("p1")

	h.tick(ctx) // latch
	h.store.mu.Lock()
	h.store.probeErr = context.DeadlineExceeded
	h.store.mu.Unlock()

	h.tick(ctx)
	h.tick(ctx)
	require.EqualValues(t, 3, h.store.listCalls.Load(), "probe errors must full-pass every tick")

	h.store.mu.Lock()
	h.store.probeErr = nil
	h.store.mu.Unlock()
	h.tick(ctx) // clean full pass re-latches
	h.tick(ctx) // cached
	require.EqualValues(t, 4, h.store.listCalls.Load())
}

func TestSweepForcedFullPassAfterMax(t *testing.T) {
	t.Parallel()

	h := newSweepGateHarness(t, time.Hour)
	ctx := context.Background()
	h.seedStable(t, "p1")
	h.setLive("p1")

	// Tick 1 latches (full). Ticks 2..20 cached (19). Tick 21 forced full.
	for i := 0; i < 21; i++ {
		h.tick(ctx)
	}
	require.EqualValues(t, 2, h.store.listCalls.Load(), "exactly one forced full pass per 20 gated ticks")
}

func TestSweepDoubleProbeConfirms(t *testing.T) {
	t.Parallel()

	h := newSweepGateHarness(t, time.Hour)
	ctx := context.Background()
	h.seedStable(t, "p1")
	h.setLive("p1")

	h.tick(ctx) // full pass: 1 probe
	require.Equal(t, 1, h.store.probeCount())

	h.tick(ctx) // cached pass: first probe + confirm probe
	require.Equal(t, 3, h.store.probeCount(), "a cached pass requires two probes")
	require.EqualValues(t, 1, h.store.listCalls.Load())
}

func TestSweepSecondProbeMismatchRunsFull(t *testing.T) {
	t.Parallel()

	h := newSweepGateHarness(t, time.Hour)
	ctx := context.Background()
	h.seedStable(t, "p1")
	h.setLive("p1")

	h.tick(ctx) // latch (steady pos: LastSeq 2 after the seed write)

	// Deposed-leader shape: first probe stale-matches the latch, the
	// confirm probe reports the truth.
	h.store.mu.Lock()
	stale := h.store.pos
	truth := stale
	truth.LastSeq++
	truth.Msgs++
	h.store.script = []sweepProbeResult{{pos: stale}, {pos: truth}}
	h.store.pos = truth
	h.store.mu.Unlock()

	h.tick(ctx) // confirm mismatch ⇒ full pass
	require.EqualValues(t, 2, h.store.listCalls.Load())
}

func TestSweepWithoutProberUnchanged(t *testing.T) {
	t.Parallel()

	// A store without BucketPos: every ticker pass lists + reads.
	store := newMemStore()
	var lists atomic.Int32
	counting := &countingListStore{ClaimStore: store, lists: &lists}
	coord, ok := New(Config{
		Store:         counting,
		TTL:           time.Minute,
		SweepInterval: -1,
		Now:           time.Now,
	}, true).(*twoPhaseCoordinator)
	require.True(t, ok)

	ctx := context.Background()
	c := NewInitialClaim("p1", "worker-1", time.Now().UTC(), time.Minute)
	_, err := store.PutIfEpoch(ctx, "p1", 0, c)
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		coord.maybeSweepClaims(ctx, sweepOriginTicker)
	}
	require.EqualValues(t, 3, lists.Load(), "no probe capability ⇒ today's full sweep every pass")
}

// countingListStore counts ListKeys on an arbitrary ClaimStore without
// adding the probe capability.
type countingListStore struct {
	ClaimStore
	lists *atomic.Int32
}

func (s *countingListStore) ListKeys(ctx context.Context) ([]string, error) {
	s.lists.Add(1)

	return s.ClaimStore.ListKeys(ctx)
}

func TestSweepUnvouchedCachedPassClearsClocks(t *testing.T) {
	t.Parallel()

	h := newSweepGateHarness(t, time.Hour)
	ctx := context.Background()
	h.seedStable(t, "p1")
	h.seedStable(t, "p2")
	h.setLive("p1", "p2")

	h.tick(ctx) // latch
	h.setLive("p1")
	h.tick(ctx) // cached, vouched: p2's absence clock starts
	require.Len(t, h.coord.orphanAbsentSince, 1)

	h.setVouch(false)
	h.tick(ctx) // cached, UNVOUCHED: clocks must clear
	require.Empty(t, h.coord.orphanAbsentSince)
}

func TestSweepApplyOriginBypassesGate(t *testing.T) {
	t.Parallel()

	h := newSweepGateHarness(t, time.Hour)
	h.coord.sweepConfirmGap = 30 * time.Second // Apply must never wait this
	ctx := context.Background()
	h.seedStable(t, "p1")
	h.setLive("p1")

	h.tick(ctx) // ticker full pass latches
	probes := h.store.probeCount()
	lists := h.store.listCalls.Load()

	start := time.Now()
	h.advance(time.Second)
	h.coord.maybeSweepClaims(ctx, sweepOriginApply)
	elapsed := time.Since(start)

	require.Equal(t, probes, h.store.probeCount(), "Apply-origin sweeps must not probe")
	require.EqualValues(t, lists+1, h.store.listCalls.Load(), "Apply-origin sweeps run the full body")
	require.Less(t, elapsed, 5*time.Second, "Apply-origin sweeps must not wait the confirm gap")

	// Gate state untouched: the latch is still valid, so (no writes
	// having happened) the next ticker pass is cached.
	h.tick(ctx)
	require.EqualValues(t, lists+1, h.store.listCalls.Load())
}

func TestSweepConcurrentApplySkipsDuringConfirmGap(t *testing.T) {
	t.Parallel()

	h := newSweepGateHarness(t, time.Hour)
	h.coord.sweepConfirmGap = 500 * time.Millisecond
	ctx := context.Background()
	h.seedStable(t, "p1")
	h.setLive("p1")

	h.tick(ctx) // latch
	probesBefore := h.store.probeCount()

	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		h.advance(time.Second)
		h.coord.maybeSweepClaims(ctx, sweepOriginTicker) // enters the confirm gap holding sweepMu
	}()
	require.Eventually(t, func() bool { return h.store.probeCount() == probesBefore+1 },
		2*time.Second, time.Millisecond, "ticker pass must take its first probe")

	// A concurrent Apply-origin sweep must TryLock-skip, not block.
	start := time.Now()
	h.coord.maybeSweepClaims(ctx, sweepOriginApply)
	require.Less(t, time.Since(start), 200*time.Millisecond,
		"Apply must not block on the ticker's confirm gap")
	require.Equal(t, probesBefore+1, h.store.probeCount(),
		"the concurrent Apply happened during the gap (confirm probe not yet taken)")

	<-tickerDone
}

func TestSweepForcedFullPassFromEmptyCache(t *testing.T) {
	t.Parallel()

	h := newSweepGateHarness(t, time.Hour)
	ctx := context.Background()

	// Empty bucket from the start: pass 1 latches an EMPTY cache.
	h.tick(ctx)
	require.EqualValues(t, 1, h.store.listCalls.Load())

	// Ticks 2..20 are empty cached passes (19). They must still count
	// toward the forced-pass budget: tick 21 lists again.
	for i := 0; i < 19; i++ {
		h.tick(ctx)
	}
	require.EqualValues(t, 1, h.store.listCalls.Load())
	h.tick(ctx)
	require.EqualValues(t, 2, h.store.listCalls.Load(),
		"the forced full pass must fire from an empty-cache latch")
}
