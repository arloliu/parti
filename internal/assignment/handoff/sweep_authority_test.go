package handoff

// Leader-gated ticker claim sweep (SweepAuthority + follower backstop).
// Pins tmp/leader-gated-claim-sweep-design.md (v7) §4.1-4.9. Every test
// here is deterministic: SweepTicks + a fake Config.Now replace the real
// ticker, SweepObserver replaces polling for the ticker's own decisions,
// and testHookAfterVouchSnapshot pins the vouch-to-seed race window.
// No real tickers, no time.Sleep.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// --- authHarness: drives the ticker loop deterministically ---

// sweepAuthEvent is one SweepObserver invocation captured by authHarness.
// tick is a TEST-SIDE tag (never part of the production SweepObserver
// signature — see recvTickEvent's doc for why it exists and how it is
// assigned race-free).
type sweepAuthEvent struct {
	origin     sweepOrigin
	authorized bool
	admitted   bool
	tick       uint64
}

// authHarness drives the coordinator's real ticker goroutine (via
// coord.Start) deterministically: an injected SweepTicks channel replaces
// time.NewTicker, and a fake clock is advanced by exactly one configured
// interval per pushed tick (mirroring the real relationship between a
// ticker firing and elapsed wall-clock time — needed because
// maybeSweepClaims' OWN interval throttle would otherwise absorb
// back-to-back ticks whose Now() never advances).
//
// The unbuffered ticks channel is a lockstep barrier FOR THE SENDER: a
// pushTick call blocks until the ticker goroutine is ready to receive,
// which only happens once it has looped back to its select statement —
// i.e. after the PREVIOUS tick's entire case body, including any
// SweepObserver call, has finished. This lets a test push a known
// sequence of ticks without racing the goroutine's own progress.
//
// It does NOT, however, make it safe to non-blockingly check "no event
// yet" immediately after pushing a tick that is ITSELF expected to admit
// — pushTick only proves that tick's sampleAuthority() call has
// happened (via the sampled handshake below), not that the rest of its
// case body — maybeSweepClaims and any SweepObserver call — has
// finished. A stale event from an EARLIER, incorrectly-admitting
// off-cycle tick could therefore still be sitting in `events` when a
// naive caller checks. recvTickEvent closes this gap by tagging every
// event with the tick sequence number that produced it (see its doc);
// skip ticks need no interleaved checks (the `continue` path never
// reaches the observer, so there is nothing to race against for THEM
// specifically), but the run's FINAL (admit) tick must always be
// consumed via recvTickEvent, never a bare FIFO recv.
type authHarness struct {
	store *memStore
	coord *twoPhaseCoordinator

	mu         sync.Mutex
	now        time.Time
	live       map[string]struct{}
	vouch      bool
	authorized bool

	interval     time.Duration
	ticks        chan time.Time
	events       chan sweepAuthEvent
	nilAuthority bool
	// sampled is signaled by sampleAuth every time the coordinator's
	// sampleAuthority() calls it — i.e. once per tick, unconditionally,
	// as the very first thing that tick's processing does — carrying
	// the tick's own sequence number (see tickSeq). pushTick waits on it
	// (when SweepAuthority is wired at all — see nilAuthority) so a
	// push doesn't return until the JUST-PUSHED tick's authority read
	// has actually happened. Without this, "the tick was received" (the
	// ticks-channel rendezvous) is NOT sufficient proof that its
	// sampleAuthority() call already happened — a test that mutates
	// `authorized` immediately after a push (to set up the NEXT tick's
	// expected value) can otherwise race the CURRENT (skip) tick's own
	// read of it.
	sampled chan uint64
	// tickSeq is a coordinator-lifetime monotonic counter, incremented
	// exactly once per real tick — INSIDE sampleAuth, which runs on the
	// SUT's own ticker goroutine — so every read anywhere else in that
	// SAME tick's processing (in particular, the SweepObserver closure,
	// which always runs later in the same tick's body) is race-free
	// same-goroutine sequential access, guarded by mu only because the
	// TEST driver goroutine also reads it (via lastTick/pushTick).
	// lastTick records the sequence number pushTick most recently
	// learned about, i.e. the tag any immediately-following
	// recvTickEvent call should expect.
	tickSeq  uint64
	lastTick uint64
}

type authHarnessOpts struct {
	grace        time.Duration
	interval     time.Duration
	phaseSeed    uint64
	nilAuthority bool
	// store, when non-nil, is wired as cfg.Store instead of h.store
	// directly — e.g. a probeMemStore or counting wrapper around
	// baseStore (for scan-gate / call-count tests). It MUST wrap the
	// same instance passed as baseStore.
	store ClaimStore
	// baseStore, when non-nil, becomes h.store (seed()/claimExists()'s
	// target) — the same underlying map `store` wraps. Ignored when
	// store is nil.
	baseStore *memStore
}

func newAuthHarness(t *testing.T, opts authHarnessOpts) *authHarness {
	t.Helper()
	if opts.interval == 0 {
		opts.interval = 30 * time.Second
	}
	base := opts.baseStore
	if base == nil {
		base = newMemStore()
	}
	h := &authHarness{
		store:        base,
		now:          time.Now().UTC(),
		live:         map[string]struct{}{},
		vouch:        true,
		authorized:   true,
		interval:     opts.interval,
		ticks:        make(chan time.Time),
		events:       make(chan sweepAuthEvent, 4096),
		nilAuthority: opts.nilAuthority,
		sampled:      make(chan uint64),
	}
	cfgStore := ClaimStore(h.store)
	if opts.store != nil {
		cfgStore = opts.store
	}

	cfg := Config{
		Store: cfgStore,
		Now: func() time.Time {
			h.mu.Lock()
			defer h.mu.Unlock()

			return h.now
		},
		LivePartitions: func(context.Context) (map[string]struct{}, bool) {
			h.mu.Lock()
			defer h.mu.Unlock()
			out := make(map[string]struct{}, len(h.live))
			for k := range h.live {
				out[k] = struct{}{}
			}

			return out, h.vouch
		},
		OrphanGrace:    opts.grace,
		SweepInterval:  opts.interval,
		SweepPhaseSeed: opts.phaseSeed,
		SweepTicks:     h.ticks,
		SweepObserver: func(origin sweepOrigin, authorized, admitted bool) {
			// Runs on the SUT's ticker goroutine (for ticker-origin) or the
			// caller's own goroutine (for Apply-origin, synchronous — see
			// recvTickEvent's doc). Reading tickSeq here is race-free for
			// the ticker-origin case specifically: sampleAuth (same
			// goroutine, earlier in this same tick's body) is the only
			// writer, and Go's memory model needs no extra synchronization
			// for same-goroutine sequential reads/writes; mu is taken only
			// because the harness's OWN driver goroutine also reads it.
			h.mu.Lock()
			seq := h.tickSeq
			h.mu.Unlock()
			h.events <- sweepAuthEvent{origin: origin, authorized: authorized, admitted: admitted, tick: seq}
		},
		MaxRetries:  1,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  2 * time.Millisecond,
	}
	if !opts.nilAuthority {
		cfg.SweepAuthority = h.sampleAuth
	}

	coord, ok := New(cfg, true).(*twoPhaseCoordinator)
	require.True(t, ok)
	h.coord = coord
	h.coord.Start(context.Background())

	return h
}

func (h *authHarness) sampleAuth() bool {
	h.mu.Lock()
	authorized := h.authorized
	h.tickSeq++
	seq := h.tickSeq
	h.mu.Unlock()
	// Signal AFTER reading state (and bumping tickSeq), so a pushTick
	// caller that then mutates `authorized` cannot possibly race THIS
	// read (see the `sampled` field doc), and so the SweepObserver call
	// later in THIS SAME tick's body (if reached) tags its event with
	// the tickSeq value THIS call just assigned, never a value a
	// subsequently-pushed tick bumped to first.
	h.sampled <- seq

	return authorized
}

func (h *authHarness) setAuthorized(v bool) {
	h.mu.Lock()
	h.authorized = v
	h.mu.Unlock()
}

func (h *authHarness) setLive(pids ...string) {
	h.mu.Lock()
	h.live = make(map[string]struct{}, len(pids))
	for _, p := range pids {
		h.live[p] = struct{}{}
	}
	h.mu.Unlock()
}

func (h *authHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func (h *authHarness) nowVal() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.now
}

// gen reads the coordinator's current unvouchedGen under authMu (mirrors
// fenceHarness.gen).
func (h *authHarness) gen() uint64 {
	h.coord.authMu.Lock()
	defer h.coord.authMu.Unlock()

	return h.coord.unvouchedGen
}

func (h *authHarness) seed(c Claim) {
	h.store.mu.Lock()
	h.store.data[c.PartitionID] = c
	h.store.rev[c.PartitionID] = 1
	h.store.mu.Unlock()
}

// pushTick advances the fake clock by one configured interval, then
// sends on the ticks channel (see the type doc for the barrier
// property this establishes).
func (h *authHarness) pushTick() {
	h.pushTickAdvance(h.interval)
}

// pushTickAdvance is pushTick with an explicit advance, used to model a
// tick landing less than a full interval after some other event (e.g.
// an Apply-origin sweep) consumed the shared throttle.
//
// It does not return until the pushed tick's sampleAuthority() call has
// happened (see the `sampled` field doc) — so it is safe for the caller
// to mutate authorized-state (setAuthorized, setLive, setVouch, advance)
// immediately afterward without racing the tick it just pushed. It also
// records that tick's sequence number in h.lastTick, which
// recvTickEvent uses to reject a stale event instead of silently
// accepting it. Skipped under nilAuthority, where SweepAuthority is
// never wired at all (a nil-authority coordinator's sampleAuthority
// short-circuits without calling it) — nilAuthority tests never call
// recvTickEvent (see its doc), so an unset lastTick is harmless there.
func (h *authHarness) pushTickAdvance(d time.Duration) {
	h.advance(d)
	select {
	case h.ticks <- h.nowVal():
	case <-time.After(5 * time.Second):
		panic("authHarness.pushTick: ticker goroutine did not consume the tick")
	}
	if h.nilAuthority {
		return
	}
	select {
	case seq := <-h.sampled:
		h.mu.Lock()
		h.lastTick = seq
		h.mu.Unlock()
	case <-time.After(5 * time.Second):
		panic("authHarness.pushTick: sampleAuthority was not called for the pushed tick")
	}
}

// recvEvent blocks for the very next SweepObserver event, FIFO, with no
// tick-index verification. Safe ONLY for Apply-origin events: Apply
// calls maybeSweepClaims and the observer SYNCHRONOUSLY, in the calling
// goroutine, so by the time Apply returns its event is already sitting
// in `events` — there is no async window for a stale event to hide in.
// Ticker-origin events MUST use recvTickEvent instead.
func (h *authHarness) recvEvent(t *testing.T) sweepAuthEvent {
	t.Helper()
	select {
	case ev := <-h.events:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a SweepObserver event")

		return sweepAuthEvent{}
	}
}

// recvTickEvent blocks for the ticker-origin SweepObserver event tagged
// with h.lastTick — the sequence number of the tick most recently
// confirmed via pushTick/pushTickAdvance. This is the fix for review
// finding P1-1 (codex-r1-postimpl.md): pushTick only proves that tick's
// sampleAuthority() call has happened, not that its maybeSweepClaims +
// SweepObserver call has finished, so a bare FIFO recv can silently
// dequeue a STALE event queued by an earlier, incorrectly-admitting
// off-cycle tick — masking exactly the bug these cadence tests exist to
// catch (see the review's 5-step false-pass sequence). Any event whose
// tag does not match the expected tick fails the test immediately
// instead of being accepted as "the" event.
//
// Once the tick-tagged event for the LAST tick pushed so far has been
// observed, that tick's entire case body (through the SweepObserver
// call) is PROVEN complete — the observer call is the final action
// before the ticker goroutine loops back to select — and, since no
// further tick has been pushed, the goroutine is now guaranteed idle.
// This is what makes an expectNoPendingEvent call immediately
// afterward non-racy: nothing else could still be in flight.
func (h *authHarness) recvTickEvent(t *testing.T) sweepAuthEvent {
	t.Helper()
	h.mu.Lock()
	want := h.lastTick
	h.mu.Unlock()
	select {
	case ev := <-h.events:
		require.Equal(t, sweepOriginTicker, ev.origin, "expected a ticker-origin event")
		require.Equal(t, want, ev.tick,
			"observer event is tagged for tick %d, not the expected tick %d — an earlier off-cycle "+
				"tick must have incorrectly admitted and left a STALE event queued (the exact false-pass "+
				"mode this tagging exists to catch)", ev.tick, want)

		return ev
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for tick %d's SweepObserver event", want)

		return sweepAuthEvent{}
	}
}

// pushTickThenRecv pushes one tick (a full h.interval advance) and
// returns its tick-verified event — a convenience for tests that push
// and immediately consume one tick at a time with no batching (so no
// staleness window exists), used by the leader-cadence fault-matrix
// rows where every tick is individually significant.
func (h *authHarness) pushTickThenRecv(t *testing.T) sweepAuthEvent {
	t.Helper()
	h.pushTick()

	return h.recvTickEvent(t)
}

// pushTickThenRecvAdvance is pushTickThenRecv with an explicit advance,
// mirroring pushTickAdvance.
func (h *authHarness) pushTickThenRecvAdvance(t *testing.T, d time.Duration) sweepAuthEvent {
	t.Helper()
	h.pushTickAdvance(d)

	return h.recvTickEvent(t)
}

// maybeRecvTickEvent is recvTickEvent's tolerant counterpart, for tests
// that cannot know in advance (round by round) whether the tick they
// just pushed is a skip or an admit — unlike recvTickEvent, which always
// requires an event, ok is false for a genuine off-cycle skip (no event
// is ever sent for that tick — ticks channel provably silent, per
// runSkipThenAdmit's doc). Any event that DOES arrive within the bounded
// window is still tag-checked against h.lastTick, so a stale event from
// an earlier tick fails the test immediately rather than being
// misattributed to the current one.
func (h *authHarness) maybeRecvTickEvent(t *testing.T) (sweepAuthEvent, bool) {
	t.Helper()
	h.mu.Lock()
	want := h.lastTick
	h.mu.Unlock()
	select {
	case ev := <-h.events:
		require.Equal(t, sweepOriginTicker, ev.origin, "expected a ticker-origin event")
		require.Equal(t, want, ev.tick,
			"observer event is tagged for tick %d, not the expected tick %d", ev.tick, want)

		return ev, true
	case <-time.After(150 * time.Millisecond):
		return sweepAuthEvent{}, false
	}
}

func (h *authHarness) expectNoPendingEvent(t *testing.T) {
	t.Helper()
	select {
	case ev := <-h.events:
		t.Fatalf("unexpected SweepObserver event: %+v", ev)
	default:
	}
}

// runSkipThenAdmit pushes skipCount ticks expected to produce NO
// SweepObserver event, followed by one tick expected to admit; returns
// that tick's event. Skip ticks are pushed with NO interleaved checks
// (they are provably silent — the off-cycle path `continue`s before ever
// reaching the observer, so there is nothing to race against); only the
// final (admit) tick is consumed, via recvTickEvent (which rejects a
// STALE event tagged for an earlier tick instead of silently accepting
// it — see its doc), followed by one drain check confirming no other
// event arrived across the whole run — so an unexpected admission
// anywhere in the skip run either fails immediately (recvTickEvent's tag
// mismatch, if it races ahead of the real final event) or fails via the
// drain check (if it trails behind), even though this helper cannot name
// which skip tick misbehaved.
func (h *authHarness) runSkipThenAdmit(t *testing.T, skipCount int) sweepAuthEvent {
	t.Helper()
	for range skipCount {
		h.pushTick()
	}
	h.pushTick() // the admit tick
	ev := h.recvTickEvent(t)
	h.expectNoPendingEvent(t)

	return ev
}

// --- fenceHarness: drives sweep internals directly, synchronously ---

// fenceHarness exercises the generation-fence protocol (§2.2a) and the
// per-pass reap decision via direct, synchronous calls into the
// coordinator's unexported methods — appropriate here because these
// tests pin exact interleavings between a "sample" (sampleAuthority) and
// a "pass" (a full sweep reaching maybeReapOrphan) that driving the real
// ticker goroutine would make nondeterministic to orchestrate. In-package
// tests calling unexported methods directly is the established pattern
// (see reapHarness in orphan_reap_test.go, sweepGateHarness in
// sweep_gate_test.go).
type fenceHarness struct {
	store *memStore
	coord *twoPhaseCoordinator

	mu         sync.Mutex
	now        time.Time
	live       map[string]struct{}
	vouch      bool
	authorized bool
	authFn     func() bool
}

func newFenceHarness(t *testing.T, grace time.Duration) *fenceHarness {
	t.Helper()

	return newFenceHarnessCfg(t, grace, false)
}

// newFenceHarnessNilAuthority builds a harness with Config.SweepAuthority
// left nil — the exact I1 contract under test, distinct from a harness
// whose SweepAuthority happens to always return true.
func newFenceHarnessNilAuthority(t *testing.T, grace time.Duration) *fenceHarness {
	t.Helper()

	return newFenceHarnessCfg(t, grace, true)
}

func newFenceHarnessCfg(t *testing.T, grace time.Duration, nilAuthority bool) *fenceHarness {
	t.Helper()
	h := &fenceHarness{
		store:      newMemStore(),
		now:        time.Now().UTC(),
		live:       map[string]struct{}{},
		vouch:      true,
		authorized: true,
	}
	cfg := Config{
		Store: h.store,
		Now: func() time.Time {
			h.mu.Lock()
			defer h.mu.Unlock()

			return h.now
		},
		LivePartitions: func(context.Context) (map[string]struct{}, bool) {
			h.mu.Lock()
			defer h.mu.Unlock()
			out := make(map[string]struct{}, len(h.live))
			for k := range h.live {
				out[k] = struct{}{}
			}

			return out, h.vouch
		},
		OrphanGrace:   grace,
		SweepInterval: -1, // sweep on every Apply call; the tests drive passes directly
		MaxRetries:    1,
		BaseBackoff:   time.Millisecond,
		MaxBackoff:    2 * time.Millisecond,
	}
	if !nilAuthority {
		cfg.SweepAuthority = h.sampleAuth
	}
	coord, ok := New(cfg, true).(*twoPhaseCoordinator)
	require.True(t, ok)
	h.coord = coord

	return h
}

func (h *fenceHarness) sampleAuth() bool {
	h.mu.Lock()
	fn := h.authFn
	authorized := h.authorized
	h.mu.Unlock()
	if fn != nil {
		return fn()
	}

	return authorized
}

func (h *fenceHarness) setAuthorized(v bool) {
	h.mu.Lock()
	h.authorized = v
	h.mu.Unlock()
}

func (h *fenceHarness) setAuthFn(f func() bool) {
	h.mu.Lock()
	h.authFn = f
	h.mu.Unlock()
}

// test's claim is absent from the live set), but the variadic mirrors
// reapHarness.setLive's general API for whichever future test needs a
// live pid.
//
//nolint:unparam // pids is nil at every current call site (every fence
func (h *fenceHarness) setLive(pids ...string) {
	h.mu.Lock()
	h.live = make(map[string]struct{}, len(pids))
	for _, p := range pids {
		h.live[p] = struct{}{}
	}
	h.mu.Unlock()
}

func (h *fenceHarness) setVouch(v bool) {
	h.mu.Lock()
	h.vouch = v
	h.mu.Unlock()
}

func (h *fenceHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func (h *fenceHarness) seed(c Claim) {
	h.store.mu.Lock()
	h.store.data[c.PartitionID] = c
	h.store.rev[c.PartitionID] = 1
	h.store.mu.Unlock()
}

// parameterized to match reapHarness.claimExists's API.
func (h *fenceHarness) claimExists(t *testing.T, pid string) bool {
	t.Helper()
	_, rev, err := h.store.Get(context.Background(), pid)
	require.NoError(t, err)

	return rev != 0
}

// pass drives one full sweep pass synchronously via Apply (sweepOriginApply
// -> ungated, always the full body; never samples SweepAuthority).
func (h *fenceHarness) pass(t *testing.T) {
	t.Helper()
	require.NoError(t, h.coord.Apply(context.Background(), "worker-1", types.Assignment{}, types.Assignment{}))
}

// swapStore replaces the coordinator's store mid-test.
func (h *fenceHarness) swapStore(s ClaimStore) {
	h.coord.cfg.Store = s
}

// gen reads the coordinator's current unvouchedGen under authMu.
func (h *fenceHarness) gen() uint64 {
	h.coord.authMu.Lock()
	defer h.coord.authMu.Unlock()

	return h.coord.unvouchedGen
}

// clockOf reads the absence-clock entry for pid, if any.
//
// parameterized for symmetry with claimExists and seed.
//
//nolint:unparam // pid is "p-gone" at every current call site; kept
func (h *fenceHarness) clockOf(pid string) (orphanClock, bool) {
	c, ok := h.coord.orphanAbsentSince[pid]

	return c, ok
}

// ambiguousDeleteStore always returns an ambiguous (non-definitive) error
// from Delete — modeling a client-observed timeout whose already-sent
// request the server may or may not still apply — without ever mutating
// the underlying store.
type ambiguousDeleteStore struct {
	ClaimStore
}

func (s *ambiguousDeleteStore) Delete(context.Context, string, uint64) error {
	return context.DeadlineExceeded
}

// --- §4.1: nil-authority regression + the universal reap-arm deltas ---

func TestSweepAuthority_NilAuthorityCadenceRegression(t *testing.T) {
	t.Parallel()

	h := newAuthHarness(t, authHarnessOpts{nilAuthority: true})
	for range 5 {
		h.pushTick()
		ev := h.recvEvent(t)
		require.Equal(t, sweepOriginTicker, ev.origin)
		require.True(t, ev.authorized, "nil SweepAuthority must report authorized=true (fail-open default)")
		require.True(t, ev.admitted, "nil SweepAuthority must admit every tick — today's full cadence")
	}
}

func TestSweepAuthority_ConstantTrueMatchesNilCadence(t *testing.T) {
	t.Parallel()

	h := newAuthHarness(t, authHarnessOpts{})
	h.setAuthorized(true)
	for range 5 {
		h.pushTick()
		ev := h.recvTickEvent(t)
		require.Equal(t, sweepOriginTicker, ev.origin)
		require.True(t, ev.authorized)
		require.True(t, ev.admitted)
	}
}

// TestSweepAuthority_I1_ClockSeedsFromVouchSnapshotNotPassStart pins §4.1(a):
// under nil authority, a new absence clock's timestamp equals the pass's
// vouch snapshot time (resolveLiveSet, read AFTER ListKeys + the per-key
// Get storm), not maybeSweepClaims' pass-start now (read before any of
// that work). A Now() that returns a strictly later reading on each
// successive call distinguishes the two call sites.
func TestSweepAuthority_I1_ClockSeedsFromVouchSnapshotNotPassStart(t *testing.T) {
	t.Parallel()

	h := newFenceHarnessNilAuthority(t, orphanTestGrace)
	h.seed(stableClaim("p-gone", h.now))
	h.setLive() // absent from the vouched set

	base := h.now
	var calls int
	h.coord.cfg.Now = func() time.Time {
		calls++

		return base.Add(time.Duration(calls) * time.Second)
	}

	h.pass(t)

	clock, ok := h.clockOf("p-gone")
	require.True(t, ok)
	require.Equal(t, base.Add(2*time.Second), clock.since,
		"the absence clock must seed from the vouch snapshot (the SECOND Now() reading: pass-start is the first)")
	require.NotEqual(t, base.Add(1*time.Second), clock.since,
		"the absence clock must not seed from pass-start now (the FIRST Now() reading)")
}

// TestSweepAuthority_I1_AmbiguousDeleteSelfPoisonsUnderNilAuthority pins
// §4.1(b): under nil authority, a reap delete against a stalled store
// returns an ambiguous outcome, keeps the claim and clock, AND
// self-poisons the generation. The discard branch — reached only via
// this flow, since liveOK never goes false in this scenario — fires on
// the next grace-eligible vouched pass; a later vouched pass reseeds
// fresh; a reap fires only after a full fresh grace from that seed.
func TestSweepAuthority_I1_AmbiguousDeleteSelfPoisonsUnderNilAuthority(t *testing.T) {
	t.Parallel()

	h := newFenceHarnessNilAuthority(t, orphanTestGrace)
	h.seed(stableClaim("p-gone", h.now))
	h.setLive() // absent

	h.pass(t) // seeds the clock at the current generation
	before := h.gen()

	h.swapStore(&ambiguousDeleteStore{ClaimStore: h.store})
	h.advance(orphanTestGrace + time.Second)
	h.pass(t) // grace elapsed: reaches the fence, attempts delete, ambiguous outcome

	require.True(t, h.claimExists(t, "p-gone"), "an ambiguous delete outcome must keep the claim")
	clock, ok := h.clockOf("p-gone")
	require.True(t, ok, "an ambiguous delete outcome must keep the clock")
	require.Equal(t, before, clock.gen, "the retained clock still carries its ORIGINAL generation")
	require.Greater(t, h.gen(), before,
		"an ambiguous delete outcome must self-poison the generation, even under nil SweepAuthority")

	// The NEXT grace-eligible vouched pass sees the generation mismatch
	// and discards — no reap, no reseed within that same pass.
	h.swapStore(h.store)
	h.pass(t)
	require.True(t, h.claimExists(t, "p-gone"), "a discarded (stale-generation) clock must not reap")
	_, ok = h.clockOf("p-gone")
	require.False(t, ok, "a fence mismatch must remove the clock")

	// A later freshly vouched pass seeds a new clock at the current generation.
	h.pass(t)
	clock2, ok := h.clockOf("p-gone")
	require.True(t, ok)
	require.Equal(t, h.gen(), clock2.gen)

	// A reap fires only after a FULL fresh grace from that fresh seed.
	h.advance(orphanTestGrace - time.Second)
	h.pass(t)
	require.True(t, h.claimExists(t, "p-gone"), "must not reap before a full fresh grace from the fresh seed")

	h.advance(2 * time.Second)
	h.pass(t)
	require.False(t, h.claimExists(t, "p-gone"), "must reap once a full fresh grace has elapsed from the fresh seed")
}

// TestSweepAuthority_I1_UnvouchedPassClearsClocksAndAdvancesGeneration
// pins §4.1(c): an in-pass liveOK=false still clears every clock
// (today's behavior, unweakened) and now also advances the generation.
// In a nil-authority flow with NO ambiguous delete, the discard path is
// never reached (there is nothing left to discard — the wholesale clear
// already removed it), so a normal grace-based reap still succeeds.
func TestSweepAuthority_I1_UnvouchedPassClearsClocksAndAdvancesGeneration(t *testing.T) {
	t.Parallel()

	h := newFenceHarnessNilAuthority(t, orphanTestGrace)
	h.seed(stableClaim("p-gone", h.now))
	h.setLive()

	h.pass(t) // vouched: seeds a clock
	_, ok := h.clockOf("p-gone")
	require.True(t, ok)
	genBefore := h.gen()

	h.setVouch(false)
	h.pass(t) // in-pass liveOK=false

	_, ok = h.clockOf("p-gone")
	require.False(t, ok, "an unvouched pass must clear every absence clock")
	require.Greater(t, h.gen(), genBefore, "an unvouched vouch attempt must advance the generation")

	h.setVouch(true)
	h.pass(t) // reseeds at the current generation
	clock, ok := h.clockOf("p-gone")
	require.True(t, ok)
	require.Equal(t, h.gen(), clock.gen)

	h.advance(orphanTestGrace + time.Second)
	h.pass(t)
	require.False(t, h.claimExists(t, "p-gone"),
		"a normal grace-based reap must succeed when the only generation-advancing cause is "+
			"liveOK=false, never an ambiguous delete")
}

// --- §4.2: cadence table + flap storm ---

// TestSweepAuthority_BackstopCadence_PhaseTable pins §4.2: under
// authority=false, admitted passes occur ONLY on backstop ticks — the
// smallest tick >= 1 satisfying (tick+phase) % backstopEvery == 0 — for
// every phase value at the default interval's backstopEvery=10.
func TestSweepAuthority_BackstopCadence_PhaseTable(t *testing.T) {
	t.Parallel()

	for phase := uint64(0); phase < 10; phase++ {
		t.Run(fmt.Sprintf("phase=%d", phase), func(t *testing.T) {
			t.Parallel()

			h := newAuthHarness(t, authHarnessOpts{phaseSeed: phase})
			h.setAuthorized(false)

			firstAdmitTick := (10 - phase) % 10
			if firstAdmitTick == 0 {
				firstAdmitTick = 10
			}
			ev := h.runSkipThenAdmit(t, int(firstAdmitTick)-1)
			require.Equal(t, sweepOriginTicker, ev.origin)
			require.False(t, ev.authorized)
			require.True(t, ev.admitted)
		})
	}
}

// TestSweepAuthority_FlapStorm_PreservesAbsoluteTickIndices pins §4.2:
// cadence follows the probed authority value tick-by-tick, AND the
// absolute backstop tick indices survive an authority flap storm
// unchanged (the tick counter is coordinator-lifetime monotonic, never
// reset on a transition).
func TestSweepAuthority_FlapStorm_PreservesAbsoluteTickIndices(t *testing.T) {
	t.Parallel()

	h := newAuthHarness(t, authHarnessOpts{phaseSeed: 0}) // backstopEvery=10, phase=0
	h.setAuthorized(true)

	// Ticks 1..9: authorized, every tick admits.
	for range 9 {
		h.pushTick()
		ev := h.recvTickEvent(t)
		require.True(t, ev.authorized)
		require.True(t, ev.admitted)
	}

	// Flap to unauthorized right at the phase-0 backstop boundary.
	h.setAuthorized(false)
	h.pushTick() // tick 10: due backstop tick — admitted despite the flap
	ev := h.recvTickEvent(t)
	require.False(t, ev.authorized)
	require.True(t, ev.admitted)

	// Ticks 11..19 (9 off-cycle skips): the counter must not reset across
	// the flap, so these stay off-cycle exactly as without any flap —
	// pushed with no interleaved checks (skip ticks are provably silent;
	// see runSkipThenAdmit's doc for why checking in between would race).
	for range 9 {
		h.pushTick()
	}

	// Flap back to authorized right after: tick 20 must be the very next
	// admitted tick. The drain check below fails if any of ticks 11..19
	// had wrongly leaked an event too (two events queued instead of one).
	h.setAuthorized(true)
	h.pushTick() // tick 20
	ev = h.recvTickEvent(t)
	require.True(t, ev.authorized, "no stray admission before tick 20")
	require.True(t, ev.admitted)
	h.expectNoPendingEvent(t)
}

// --- §4.3: backstop passes engage the scan gate ---

// TestSweepAuthority_BackstopPassesEngageScanGate pins §4.3: a backstop
// pass runs the identical maybeSweepClaims body a leader's tick does —
// scan-gated when the store can probe its position. The first backstop
// pass (nothing latched yet) is full; a second admitted backstop pass
// against an unchanged position is cached (no additional ListKeys).
func TestSweepAuthority_BackstopPassesEngageScanGate(t *testing.T) {
	t.Parallel()

	base := newMemStore()
	store := &probeMemStore{memStore: base, pos: natsutil.KVStreamPos{Created: time.Unix(1000, 0), LastSeq: 1}}

	h := newAuthHarness(t, authHarnessOpts{store: store, baseStore: base})
	h.coord.sweepConfirmGap = time.Nanosecond // must not block this deterministic test
	c := NewInitialClaim("p1", "worker-1", h.nowVal(), time.Minute)
	_, err := store.PutIfEpoch(context.Background(), "p1", 0, c)
	require.NoError(t, err)
	h.setAuthorized(false)

	ev := h.runSkipThenAdmit(t, 9) // tick 10: first backstop, nothing latched -> full
	require.True(t, ev.admitted)
	require.EqualValues(t, 1, store.listCalls.Load())

	ev = h.runSkipThenAdmit(t, 9) // tick 20: position unchanged -> cached
	require.True(t, ev.admitted)
	require.EqualValues(t, 1, store.listCalls.Load(),
		"an unchanged position must engage the gate on a backstop pass too")
}

// TestSweepAuthority_BackstopPassFailsOpenOnProbeError pins §4.3: a
// probe error inside an admitted backstop pass fails open to a full
// pass, exactly as a leader's gated tick does.
func TestSweepAuthority_BackstopPassFailsOpenOnProbeError(t *testing.T) {
	t.Parallel()

	base := newMemStore()
	store := &probeMemStore{
		memStore: base,
		pos:      natsutil.KVStreamPos{Created: time.Unix(1000, 0), LastSeq: 1},
		probeErr: context.DeadlineExceeded,
	}

	h := newAuthHarness(t, authHarnessOpts{store: store, baseStore: base})
	h.coord.sweepConfirmGap = time.Nanosecond
	c := NewInitialClaim("p1", "worker-1", h.nowVal(), time.Minute)
	_, err := store.PutIfEpoch(context.Background(), "p1", 0, c)
	require.NoError(t, err)
	h.setAuthorized(false)

	ev := h.runSkipThenAdmit(t, 9) // tick 10
	require.True(t, ev.admitted)
	require.EqualValues(t, 1, store.listCalls.Load())

	ev = h.runSkipThenAdmit(t, 9) // tick 20: probe still failing -> full again
	require.True(t, ev.admitted)
	require.EqualValues(t, 2, store.listCalls.Load(),
		"a probe error on a backstop pass must fail open to a full pass")
}

// TestSweepAuthority_NoProberStoreRunsFullPassesAtBackstopCadenceOnly
// pins §4.3: a store without the prober capability, under
// authority=false, runs full passes at backstop cadence only — never
// every tick (that would be the pre-gate behavior), never zero (the
// backstop cadence must keep firing).
func TestSweepAuthority_NoProberStoreRunsFullPassesAtBackstopCadenceOnly(t *testing.T) {
	t.Parallel()

	base := newMemStore()
	c := NewInitialClaim("p1", "worker-1", time.Now().UTC(), time.Minute)
	_, err := base.PutIfEpoch(context.Background(), "p1", 0, c)
	require.NoError(t, err)
	var lists atomic.Int32
	counting := &countingListStore{ClaimStore: base, lists: &lists}

	h := newAuthHarness(t, authHarnessOpts{store: counting, baseStore: base})
	h.setAuthorized(false)

	ev := h.runSkipThenAdmit(t, 9) // tick 10: first backstop
	require.True(t, ev.admitted)
	require.EqualValues(t, 1, lists.Load(), "never every tick — only at the backstop tick")

	ev = h.runSkipThenAdmit(t, 9) // tick 20: second backstop
	require.True(t, ev.admitted)
	require.EqualValues(t, 2, lists.Load(), "never zero — the backstop cadence keeps firing")
}

// --- §4.4: generation fence (all interleavings) ---

// TestSweepAuthority_Fence_BasicSequencing pins §4.4(a): seed as leader;
// one false sample; regain; the next vouched pass whose grace has
// elapsed sees the generation mismatch and DISCARDS — no reap AND no
// reseed within that same pass; the clock is then absent; a SUBSEQUENT
// freshly vouched pass seeds a new clock from its own snapshot; a reap
// may fire only after a full grace from that fresh seed. Three distinct
// observations, asserted separately.
func TestSweepAuthority_Fence_BasicSequencing(t *testing.T) {
	t.Parallel()

	h := newFenceHarness(t, orphanTestGrace)
	h.seed(stableClaim("p-gone", h.now))
	h.setLive() // absent
	h.setAuthorized(true)

	h.pass(t) // leader, vouched: seeds the clock at the current generation
	genAtSeed := h.gen()
	require.Zero(t, genAtSeed)

	// Grace elapses while still (nominally) leader.
	h.advance(orphanTestGrace + time.Second)

	// One false sample (only the ticker samples in production; a direct
	// call here is the deterministic equivalent).
	h.setAuthorized(false)
	require.False(t, h.coord.sampleAuthority())
	require.Greater(t, h.gen(), genAtSeed)

	// Regain.
	h.setAuthorized(true)

	// Observation 1: the next vouched pass DISCARDS — no reap, no reseed.
	h.pass(t)
	require.True(t, h.claimExists(t, "p-gone"), "a discarded clock must not reap")

	// Observation 2: the clock is absent afterward.
	_, ok := h.clockOf("p-gone")
	require.False(t, ok, "a fence mismatch must remove the clock, not re-seed it")

	// Observation 3: a SUBSEQUENT freshly vouched pass seeds a new clock,
	// and a reap fires only after a full grace measured from it.
	h.pass(t)
	clock, ok := h.clockOf("p-gone")
	require.True(t, ok)
	require.Equal(t, h.gen(), clock.gen)

	h.advance(orphanTestGrace - time.Second)
	h.pass(t)
	require.True(t, h.claimExists(t, "p-gone"), "must not reap before a full fresh grace")

	h.advance(2 * time.Second)
	h.pass(t)
	require.False(t, h.claimExists(t, "p-gone"), "must reap once the fresh grace has fully elapsed")
}

// TestSweepAuthority_Fence_SamplePauseRace pins §4.4(b): a SweepAuthority
// callback that parks INSIDE the sample (holding authMu) while a
// grace-expired reaper reaches its fence must block the reaper until the
// sample returns false and records; the fence must then see the
// mismatch and discard.
//
// Per review finding P1-2 (codex-r1-postimpl.md), the original version
// launched the reaper and immediately released the parked sample without
// ever proving the reaper had reached AND BLOCKED — a broken
// implementation that schedules the reaper only after generation
// recording (i.e. one that races the fence in the WRONG order) could
// still have passed. A bounded non-completion check — the same
// goroutine-blocked idiom TestSweep_SingleFlight uses
// (orphan_reap_test.go) — now proves that. Note this proves blocking on
// authMu generally, not specifically inside maybeReapOrphan's own
// Lock(): resolveLiveSet's vouch snapshot (earlier in the SAME pass)
// ALSO takes authMu, so with the sample parked for the whole callback,
// the pass is provably stuck on authMu well before it could ever reach
// the reap fence — an equally valid proof of "reached and blocked",
// since both acquisitions are the identical sweepMu->authMu ordering
// the review's lock-ordering claim is about. (A hook placed
// specifically at the reap fence's own Lock() call was tried and
// rejected: it is UNREACHABLE in this exact interleaving, since
// resolveLiveSet's own authMu use blocks first — attempting to wait for
// it deadlocks.)
func TestSweepAuthority_Fence_SamplePauseRace(t *testing.T) {
	t.Parallel()

	h := newFenceHarness(t, orphanTestGrace)
	h.seed(stableClaim("p-gone", h.now))
	h.setLive()
	h.setAuthorized(true)

	h.pass(t) // seeds the clock at the current generation
	genAtSeed := h.gen()
	h.advance(orphanTestGrace + time.Second) // grace elapsed

	parked := make(chan struct{})
	release := make(chan struct{})
	h.setAuthFn(func() bool {
		close(parked)
		<-release

		return false
	})

	sampleDone := make(chan struct{})
	go func() {
		h.coord.sampleAuthority()
		close(sampleDone)
	}()
	<-parked // the sample now holds authMu, parked inside the callback

	reapDone := make(chan struct{})
	go func() {
		h.pass(t) // must block on authMu until the sample releases
		close(reapDone)
	}()

	// Prove the reap pass actually BLOCKS on authMu instead of racing
	// through: the sample is still holding it, so the pass must not
	// complete within a bounded window.
	select {
	case <-reapDone:
		t.Fatal("the reap pass completed before the parked sample released authMu — it was not reached and blocked")
	case <-time.After(100 * time.Millisecond):
	}

	close(release) // the sample returns false, records, releases authMu
	<-sampleDone
	<-reapDone

	require.Greater(t, h.gen(), genAtSeed, "the parked sample must have recorded")
	require.True(t, h.claimExists(t, "p-gone"), "the fenced reap must discard, not reap, on the mismatch")
	_, ok := h.clockOf("p-gone")
	require.False(t, ok)
}

// parkingDeleteStore blocks its Delete call until released, then
// delegates to the wrapped store's Delete (a definitive outcome).
type parkingDeleteStore struct {
	ClaimStore
	entered chan struct{}
	release chan struct{}
}

func (s *parkingDeleteStore) Delete(ctx context.Context, pid string, rev uint64) error {
	close(s.entered)
	<-s.release

	return s.ClaimStore.Delete(ctx, pid, rev)
}

// TestSweepAuthority_Fence_InverseOrdering pins §4.4(c): the fence's
// generation comparison passes and the delete is in flight when a false
// sample arrives — the sample blocks on authMu until the delete returns;
// the claim is gone; the sample records AFTERWARD (allowed by I3's
// linearization point).
//
// Per review finding P1-2, the original version released the delete
// immediately after starting the sample, without ever asserting the
// sample actually remained blocked while the delete was in flight — an
// unfenced sample could have completed during the delete and still
// satisfied the final assertions. A bounded non-completion check (the
// established goroutine-blocked idiom in this package — see
// TestSweep_SingleFlight, orphan_reap_test.go) now proves that.
func TestSweepAuthority_Fence_InverseOrdering(t *testing.T) {
	t.Parallel()

	h := newFenceHarness(t, orphanTestGrace)
	h.seed(stableClaim("p-gone", h.now))
	h.setLive()
	h.setAuthorized(true)

	h.pass(t) // seeds the clock at the current generation
	genAtSeed := h.gen()
	h.advance(orphanTestGrace + time.Second)

	parking := &parkingDeleteStore{ClaimStore: h.store, entered: make(chan struct{}), release: make(chan struct{})}
	h.swapStore(parking)

	reapDone := make(chan struct{})
	go func() {
		h.pass(t) // reaches the fence (gen matches), Delete parks
		close(reapDone)
	}()
	<-parking.entered // Delete is in flight, authMu held

	h.setAuthorized(false)
	sampleDone := make(chan struct{})
	go func() {
		h.coord.sampleAuthority() // must block on authMu until Delete returns
		close(sampleDone)
	}()

	// The sample must remain blocked WHILE the delete is still in
	// flight, not race ahead of it.
	select {
	case <-sampleDone:
		t.Fatal("sampleAuthority returned before the in-flight delete released authMu")
	case <-time.After(100 * time.Millisecond):
	}

	close(parking.release) // Delete returns nil: definitive apply
	<-reapDone
	<-sampleDone

	require.False(t, h.claimExists(t, "p-gone"), "the definitive delete must apply")
	require.Greater(t, h.gen(), genAtSeed, "the false sample records AFTER the delete's linearization point")
}

// TestSweepAuthority_Fence_VouchToSeedBarrier_FullPass pins §4.4(d) for
// the full-pass body: a false sample recorded in the gap between the
// vouch snapshot and the first claim decision produces a clock whose gen
// is already stale — the fence must discard it.
func TestSweepAuthority_Fence_VouchToSeedBarrier_FullPass(t *testing.T) {
	t.Parallel()

	h := newFenceHarness(t, orphanTestGrace)
	h.seed(stableClaim("p-gone", h.now))
	h.setLive() // absent: this pass will seed a fresh clock
	h.setAuthorized(true)

	hookEntered := make(chan struct{})
	hookRelease := make(chan struct{})
	h.coord.testHookAfterVouchSnapshot = func() {
		close(hookEntered)
		<-hookRelease
	}

	passDone := make(chan struct{})
	go func() {
		h.pass(t) // full pass (nothing cached yet)
		close(passDone)
	}()
	<-hookEntered // vouch snapshot taken; parked before the first claim decision

	genAtSnapshot := h.gen()
	h.setAuthorized(false)
	require.False(t, h.coord.sampleAuthority()) // records a false sample IN THE GAP
	require.Greater(t, h.gen(), genAtSnapshot)

	close(hookRelease)
	<-passDone
	h.coord.testHookAfterVouchSnapshot = nil

	clock, ok := h.clockOf("p-gone")
	require.True(t, ok)
	require.Equal(t, genAtSnapshot, clock.gen, "the clock carries the STALE pre-race generation")
	require.NotEqual(t, h.gen(), clock.gen)

	// The fence must discard it at reap time.
	h.setAuthorized(true)
	h.advance(orphanTestGrace + time.Second)
	h.pass(t)
	require.True(t, h.claimExists(t, "p-gone"), "a stale-generation clock must discard, not reap")
	_, ok = h.clockOf("p-gone")
	require.False(t, ok)
}

// TestSweepAuthority_Fence_VouchToSeedBarrier_CachedPass pins §4.4(d) for
// the cached-pass body — the identical race, but the pass being raced is
// a gated (cached) pass, proven by zero additional ListKeys calls across
// it.
func TestSweepAuthority_Fence_VouchToSeedBarrier_CachedPass(t *testing.T) {
	t.Parallel()

	base := newMemStore()
	store := &probeMemStore{memStore: base, pos: natsutil.KVStreamPos{Created: time.Unix(1000, 0), LastSeq: 1}}

	var (
		mu         sync.Mutex
		now        = time.Now().UTC()
		live       = map[string]struct{}{}
		vouch      = true
		authorized atomic.Bool
	)
	authorized.Store(true)

	coord, ok := New(Config{
		Store: store,
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()

			return now
		},
		LivePartitions: func(context.Context) (map[string]struct{}, bool) {
			mu.Lock()
			defer mu.Unlock()
			out := make(map[string]struct{}, len(live))
			for k := range live {
				out[k] = struct{}{}
			}

			return out, vouch
		},
		SweepAuthority: authorized.Load,
		OrphanGrace:    orphanTestGrace,
		SweepInterval:  -1,
		MaxRetries:     1,
		BaseBackoff:    time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	}, true).(*twoPhaseCoordinator)
	require.True(t, ok)
	coord.sweepConfirmGap = time.Nanosecond

	c1 := NewInitialClaim("p-live", "worker-1", now, time.Minute)
	_, err := store.PutIfEpoch(context.Background(), "p-live", 0, c1)
	require.NoError(t, err)
	c2 := NewInitialClaim("p-gone", "worker-1", now, time.Minute)
	_, err = store.PutIfEpoch(context.Background(), "p-gone", 0, c2)
	require.NoError(t, err)

	mu.Lock()
	live = map[string]struct{}{"p-live": {}, "p-gone": {}}
	mu.Unlock()

	require.True(t, coord.maybeSweepClaims(context.Background(), sweepOriginTicker)) // full pass: latches both
	require.EqualValues(t, 1, store.listCalls.Load())

	// p-gone drops out of the live set: no bucket write, so the position
	// is unchanged and the gate stays cached for the next pass.
	mu.Lock()
	live = map[string]struct{}{"p-live": {}}
	mu.Unlock()

	hookEntered := make(chan struct{})
	hookRelease := make(chan struct{})
	coord.testHookAfterVouchSnapshot = func() {
		close(hookEntered)
		<-hookRelease
	}

	passDone := make(chan struct{})
	go func() {
		admitted := coord.maybeSweepClaims(context.Background(), sweepOriginTicker) // cached pass
		require.True(t, admitted)
		close(passDone)
	}()
	<-hookEntered

	coord.authMu.Lock()
	genAtSnapshot := coord.unvouchedGen
	coord.authMu.Unlock()

	authorized.Store(false)
	require.False(t, coord.sampleAuthority()) // records a false sample IN THE GAP

	close(hookRelease)
	<-passDone
	coord.testHookAfterVouchSnapshot = nil
	require.EqualValues(t, 1, store.listCalls.Load(), "the raced pass must have been CACHED, not full")

	clock, ok := coord.orphanAbsentSince["p-gone"]
	require.True(t, ok)
	require.Equal(t, genAtSnapshot, clock.gen)

	coord.authMu.Lock()
	currentGen := coord.unvouchedGen
	coord.authMu.Unlock()
	require.NotEqual(t, currentGen, clock.gen)

	// The fence must discard it at reap time.
	authorized.Store(true)
	mu.Lock()
	now = now.Add(orphanTestGrace + time.Second)
	mu.Unlock()
	require.True(t, coord.maybeSweepClaims(context.Background(), sweepOriginTicker))

	_, rev, err := store.Get(context.Background(), "p-gone")
	require.NoError(t, err)
	require.NotZero(t, rev, "a stale-generation clock must discard, not reap")
	_, ok = coord.orphanAbsentSince["p-gone"]
	require.False(t, ok)
}

// TestSweepAuthority_Fence_MismatchDiscard pins §4.4(e) in isolation: on
// a fence mismatch the clock is REMOVED (not left in place, not
// re-seeded) — asserted as three separate facts: absent immediately
// after, absent until a later freshly vouched pass, and that later pass
// seeds from ITS OWN snapshot (never a re-seed from pass-start now).
func TestSweepAuthority_Fence_MismatchDiscard(t *testing.T) {
	t.Parallel()

	h := newFenceHarness(t, orphanTestGrace)
	h.seed(stableClaim("p-gone", h.now))
	h.setLive()

	h.pass(t) // seed at the current generation
	h.advance(orphanTestGrace + time.Second)

	h.setAuthorized(false)
	require.False(t, h.coord.sampleAuthority()) // bump the generation past the seed

	h.setAuthorized(true)
	h.pass(t) // fence mismatch: discard
	_, ok := h.clockOf("p-gone")
	require.False(t, ok, "removed immediately")

	h.pass(t) // still absent, no time advanced: seeds fresh at the CURRENT generation
	clock, ok := h.clockOf("p-gone")
	require.True(t, ok)
	require.Equal(t, h.gen(), clock.gen)
	require.True(t, h.claimExists(t, "p-gone"), "the fresh seed must not itself reap")
}

// TestSweepAuthority_Fence_InPassUnvouchedClearsClocks pins §4.4(f): an
// unvouched ADMITTED pass clears every absence clock — resolveLiveSet's
// existing contract, unweakened by the fence (cross-referenced from
// TestOrphanReap_UnvouchedGapResetsExistingClock and
// TestSweepAuthority_I1_UnvouchedPassClearsClocksAndAdvancesGeneration,
// which additionally pins the generation bump).
func TestSweepAuthority_Fence_InPassUnvouchedClearsClocks(t *testing.T) {
	t.Parallel()

	h := newFenceHarness(t, orphanTestGrace)
	h.seed(stableClaim("p-gone", h.now))
	h.setLive()

	h.pass(t)
	_, ok := h.clockOf("p-gone")
	require.True(t, ok)

	h.setVouch(false)
	h.pass(t)
	_, ok = h.clockOf("p-gone")
	require.False(t, ok, "an unvouched admitted pass must clear every absence clock")
}

// TestSweepAuthority_Fence_LockOrdering pins §4.4(g): the reap path
// acquires sweepMu -> authMu only; sampleAuthority never touches
// sweepMu. Heavy concurrent contention between full sweep passes (which
// enter authMu from inside sweepMu) and bare sampleAuthority calls
// (authMu alone) must never deadlock or race — run under -race.
//
// Per review finding P1-2, the original version never advanced the fake
// clock after seeding, so the 1ms grace never elapsed: every pass hit
// the initial-seed branch (return before the fence) or the
// already-seeded, still-under-grace branch, and the ONLY authMu
// acquisition actually exercised under contention was
// resolveLiveSet's own separate snapshot — never the REAP fence
// (sweepMu -> authMu inside maybeReapOrphan's delete branch) the test
// claims to pin. A dedicated goroutine now continuously reseeds fresh
// claims and advances the clock past grace, so pass goroutines
// repeatedly reach and cross the reap fence itself throughout the
// contention window; a final assertion confirms at least one claim was
// actually reaped, proving the fence path — not just the vouch-snapshot
// path — was genuinely exercised.
func TestSweepAuthority_Fence_LockOrdering(t *testing.T) {
	t.Parallel()

	h := newFenceHarness(t, time.Millisecond) // tiny grace: passes actually reach the fence
	for i := range 20 {
		h.seed(stableClaim(fmt.Sprintf("p-%d", i), h.now))
	}
	h.setLive() // all absent

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Continuously reseed fresh claims and advance the clock past grace
	// so pass goroutines keep reaching the REAP fence's authMu.Lock()
	// throughout the run, not just once at the very start.
	wg.Go(func() {
		n := 20
		for {
			select {
			case <-done:
				return
			default:
				h.seed(stableClaim(fmt.Sprintf("p-%d", n), h.now))
				n++
				h.advance(2 * time.Millisecond)
			}
		}
	})

	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
					h.pass(t)
				}
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
					h.coord.sampleAuthority()
				}
			}
		})
	}

	time.Sleep(50 * time.Millisecond) // bounded contention window (not a state-wait)
	close(done)
	wg.Wait()

	reaped := false
	for i := range 20 {
		if !h.claimExists(t, fmt.Sprintf("p-%d", i)) {
			reaped = true

			break
		}
	}
	require.True(t, reaped,
		"the reap fence must have actually been reached and applied at least once during the contention window")
}

// scheduledAmbiguousDeleteStore makes exactly one armed Delete call
// return an ambiguous (non-definitive) DeadlineExceeded WITHOUT EVER
// touching the underlying store — modeling a client-side timeout on an
// already-sent request whose eventual server-side application (if any)
// is entirely test-orchestrated, out-of-band, strictly AFTER this
// Delete call has already returned to its caller (see
// TestSweepAuthority_Fence_AmbiguousDeleteOutcome's "late deletion
// eventually applies" subtest). Per review finding P1-2, an earlier
// version applied a "late" deletion SYNCHRONOUSLY inside this call,
// before returning the ambiguous error — but maybeReapOrphan holds
// authMu for the whole Delete call, so nothing (in particular, no false
// sample) could ever interleave in that window; there was no gap for
// the interleaving §4.4(h) actually describes to occur in.
type scheduledAmbiguousDeleteStore struct {
	ClaimStore
	mu    sync.Mutex
	armed bool
}

func (s *scheduledAmbiguousDeleteStore) arm() {
	s.mu.Lock()
	s.armed = true
	s.mu.Unlock()
}

func (s *scheduledAmbiguousDeleteStore) Delete(ctx context.Context, pid string, rev uint64) error {
	s.mu.Lock()
	armed := s.armed
	s.armed = false
	s.mu.Unlock()
	if !armed {
		return s.ClaimStore.Delete(ctx, pid, rev)
	}

	return context.DeadlineExceeded
}

// TestSweepAuthority_Fence_AmbiguousDeleteOutcome pins §4.4(h): the
// store's conditioned delete returns an ambiguous error while the
// deletion request may still apply after the fence releases. Assert (i)
// the generation self-poisons, (ii) a late-applying deletion is
// tolerated (no double-decision), and (iii) a never-applying deletion
// still requires a full fresh grace before any reap.
func TestSweepAuthority_Fence_AmbiguousDeleteOutcome(t *testing.T) {
	t.Parallel()

	t.Run("late deletion eventually applies", func(t *testing.T) {
		t.Parallel()

		h := newFenceHarness(t, orphanTestGrace)
		h.seed(stableClaim("p-gone", h.now))
		h.setLive()
		h.pass(t) // seed at the current generation
		genAtSeed := h.gen()

		store := &scheduledAmbiguousDeleteStore{ClaimStore: h.store}
		h.swapStore(store)
		store.arm()

		h.advance(orphanTestGrace + time.Second)
		h.pass(t) // ambiguous outcome: generation self-poisons; Delete has NOT applied

		require.Greater(t, h.gen(), genAtSeed, "(i) the generation must self-poison on the ambiguous outcome")
		require.True(t, h.claimExists(t, "p-gone"), "the sweep must return with the claim still present — nothing has applied yet")
		genAfterAmbiguous := h.gen()

		// The test records a false sample IN THE GAP: after the sweep has
		// returned (authMu is free again) and BEFORE the "late" deletion
		// is applied — the exact interleaving §4.4(h) describes.
		h.setAuthorized(false)
		require.False(t, h.coord.sampleAuthority())
		require.Greater(t, h.gen(), genAfterAmbiguous, "the false sample must record its own generation bump")

		// THEN the test applies the deletion out-of-band, exactly as a
		// server that eventually honors the earlier ambiguous request
		// would — bypassing the coordinator entirely.
		_, rev, err := h.store.Get(context.Background(), "p-gone")
		require.NoError(t, err)
		require.NoError(t, h.store.Delete(context.Background(), "p-gone", rev))

		require.False(t, h.claimExists(t, "p-gone"),
			"(ii) the late deletion applying is tolerated: the key is simply gone")

		// The next pass handles the vanished key through the existing
		// paths — no double-decision, no error.
		h.setAuthorized(true)
		h.pass(t)
	})

	t.Run("scheduled deletion never applies", func(t *testing.T) {
		t.Parallel()

		h := newFenceHarness(t, orphanTestGrace)
		h.seed(stableClaim("p-gone", h.now))
		h.setLive()
		h.pass(t)
		genAtSeed := h.gen()

		store := &scheduledAmbiguousDeleteStore{ClaimStore: h.store}
		h.swapStore(store)
		store.arm() // ambiguous outcome; this subtest never applies the deletion

		h.advance(orphanTestGrace + time.Second)
		h.pass(t) // ambiguous outcome: generation self-poisons, claim retained

		require.Greater(t, h.gen(), genAtSeed)
		require.True(t, h.claimExists(t, "p-gone"))

		// (iii) the claim requires a FULL fresh grace from the next
		// vouched seed before any reap: the stale clock discards first...
		h.swapStore(h.store)
		h.pass(t)
		require.True(t, h.claimExists(t, "p-gone"), "the stale clock must discard, not reap")
		_, ok := h.clockOf("p-gone")
		require.False(t, ok)

		// ...then a fresh seed, then a full grace.
		h.pass(t)
		h.advance(orphanTestGrace - time.Second)
		h.pass(t)
		require.True(t, h.claimExists(t, "p-gone"), "must not reap before the fresh grace fully elapses")

		h.advance(2 * time.Second)
		h.pass(t)
		require.False(t, h.claimExists(t, "p-gone"))
	})
}

// --- §4.5: backstopDue latch ---

// TestSweepAuthority_BackstopDueLatch pins §4.5: an Apply-origin sweep
// landing just before the due tick absorbs it via the shared interval
// throttle (admitted=false observed via SweepObserver); the latch keeps
// the tick "due" and the NEXT tick admits — bounding either-origin
// admitted cadence to (backstopEvery+1) x SweepInterval.
func TestSweepAuthority_BackstopDueLatch(t *testing.T) {
	t.Parallel()

	h := newAuthHarness(t, authHarnessOpts{phaseSeed: 0}) // backstopEvery=10
	h.setAuthorized(false)

	for range 9 {
		h.pushTick() // ticks 1..9: off-cycle skips
	}

	// An Apply-origin sweep lands right before the due tick and consumes
	// the shared interval throttle.
	require.NoError(t, h.coord.Apply(context.Background(), "worker-1", types.Assignment{}, types.Assignment{}))
	applyEv := h.recvEvent(t)
	require.Equal(t, sweepOriginApply, applyEv.origin)
	require.True(t, applyEv.authorized, "Apply-origin authorized is the constant true marker")
	require.True(t, applyEv.admitted)

	// The due tick (10) fires close behind (< one interval later): its
	// OWN throttle check absorbs it — admitted=false — but the backstop
	// latch keeps it due.
	h.pushTickAdvance(h.interval - time.Second)
	tickEv := h.recvTickEvent(t)
	require.Equal(t, sweepOriginTicker, tickEv.origin)
	require.False(t, tickEv.authorized)
	require.False(t, tickEv.admitted, "the due tick's own pass must be absorbed by the shared interval throttle")

	// The latch forces the VERY NEXT tick (11, off-cycle by schedule) to
	// retry, and this time the throttle has cleared.
	h.pushTick()
	retryEv := h.recvTickEvent(t)
	require.Equal(t, sweepOriginTicker, retryEv.origin)
	require.False(t, retryEv.authorized)
	require.True(t, retryEv.admitted, "the backstopDue latch must force a retry on the very next tick")
}

// TestSweepAuthority_ContinuousApplyBoundsEitherOriginCadence pins the
// §4.5 continuous-Apply case the review (P1-3) flagged as missing:
// TestSweepAuthority_BackstopDueLatch above pins a SINGLE Apply/ticker
// collision; this test drives sustained Apply-origin sweeps, round after
// round, across several backstop cycles — repeatedly swallowing the
// ticker's own due-tick attempts — and asserts that the either-origin
// admitted cadence (whichever origin actually lands: Apply's own
// frequent admissions, or the ticker's eventual latched retry) never
// exceeds the (backstopEvery+1) x SweepInterval bound between any two
// successive admitted passes, using the fake clock and the full
// SweepObserver history (never a real wait).
func TestSweepAuthority_ContinuousApplyBoundsEitherOriginCadence(t *testing.T) {
	t.Parallel()

	h := newAuthHarness(t, authHarnessOpts{phaseSeed: 0}) // backstopEvery=10
	h.setAuthorized(false)
	bound := time.Duration(backstopEveryFor(h.interval)+1) * h.interval

	lastAdmitAt := h.nowVal()
	checkGap := func(at time.Time) {
		gap := at.Sub(lastAdmitAt)
		require.LessOrEqual(t, gap, bound,
			"either-origin admitted cadence exceeded the (backstopEvery+1) x interval bound: gap=%s bound=%s", gap, bound)
		lastAdmitAt = at
	}

	const rounds = 30 // three full backstop cycles' worth of ticks
	for range rounds {
		// A sustained Apply-origin sweep lands every round, well inside
		// one interval of the previous admission — modeling continuous
		// Apply traffic that keeps consuming the shared throttle ahead
		// of the ticker.
		h.advance(h.interval / 2)
		require.NoError(t, h.coord.Apply(context.Background(), "worker-1", types.Assignment{}, types.Assignment{}))
		applyEv := h.recvEvent(t)
		require.Equal(t, sweepOriginApply, applyEv.origin)
		if applyEv.admitted {
			checkGap(h.nowVal())
		}

		// The ticker tick for this round, landing shortly after Apply.
		h.pushTickAdvance(h.interval / 2)
		if ev, ok := h.maybeRecvTickEvent(t); ok && ev.admitted {
			checkGap(h.nowVal())
		}
	}
}

// --- §4.6: admitted-pass fault matrix ---
//
// Per review finding P1-3, every row below asserts all four spec
// obligations: (a) the faulted pass is counted as admitted via
// SweepObserver; (b) backstopDue actually clears; (c) the next attempt
// occurs at the documented next backstop tick; (d) recovery converges
// once the fault lifts.
//
// Rows 1-4 (probe/ListKeys/per-key-Get/reconcile-CAS faults) touch
// NOTHING the generation fence guards — sampleAuthority and the fence
// are entirely orthogonal to them — so they run under a SUSTAINED
// authority=false backstop cadence via the shared runFaultMatrix*
// helpers below, where (b)/(c) are directly observable exactly as
// phrased.
//
// Rows 5-6 (reap-Delete definitive non-application: revision-CAS loss,
// ErrKeyNotFound) and the new fast ambiguous-outcome row run under a
// CONTINUOUSLY AUTHORIZED (leader) ticker instead: sampleAuthority
// bumps unvouchedGen on EVERY unauthorized sample, unconditionally, for
// EVERY real tick — including off-cycle skips, which call it before the
// skip check. Under a sustained backstop (authority=false) worker, this
// means the fault-tick's OWN sample already bumps the generation past
// whatever an earlier admitted pass seeded, so the fence would discard
// the clock before ever reaching Delete — that is the fence protocol
// working exactly as intended (a worker that has been unauthorized at
// any point since seeding a clock must never trust it; that is the
// whole point of §4.4's fence, exercised directly by the dedicated
// Fence_* tests). It is not a gap in these rows: it makes the delete
// arms UNREACHABLE under backstop cadence by construction, so a leader
// ticker pass is the only way to exercise them at all. There is no
// "backstopDue"/"next backstop tick" concept under full leader cadence
// (every tick admits), so (b)/(c) there become "the fault does not
// disrupt full ticker cadence" and "the immediately following tick
// already reflects the fault's resolution" — which is what these rows
// assert instead.

// runFaultMatrixFirstAdmit sets authority=false and drives h to the
// FIRST backstop tick (tick backstopEveryFor(h.interval), phase 0 —
// every row below uses the default phaseSeed=0), returning that
// admitted pass's event. The caller arms its fault BEFORE calling this
// so the fault is active for the pass it returns.
func runFaultMatrixFirstAdmit(t *testing.T, h *authHarness) sweepAuthEvent {
	t.Helper()
	h.setAuthorized(false)
	backstopEvery := backstopEveryFor(h.interval)

	return h.runSkipThenAdmit(t, backstopEvery-1)
}

// assertBackstopDueClears pins obligation (b): the tick immediately
// after an admitted (even faulted) pass must NOT itself admit —
// proving backstopDue actually cleared rather than staying latched.
func assertBackstopDueClears(t *testing.T, h *authHarness) {
	t.Helper()
	h.pushTick()
	select {
	case unexpected := <-h.events:
		t.Fatalf("backstopDue must clear after an admitted pass — got an unexpected event: %+v", unexpected)
	case <-time.After(200 * time.Millisecond):
	}
}

// runFaultMatrixNextBackstop pins obligation (c): drives h to the NEXT
// documented backstop tick — one full backstopEvery cycle after the
// tick assertBackstopDueClears already consumed — returning that pass's
// event.
func runFaultMatrixNextBackstop(t *testing.T, h *authHarness) sweepAuthEvent {
	t.Helper()
	backstopEvery := backstopEveryFor(h.interval)

	return h.runSkipThenAdmit(t, backstopEvery-2)
}

func TestSweepAuthority_FaultMatrix_ProbeFailureFailsOpen(t *testing.T) {
	t.Parallel()

	base := newMemStore()
	store := &probeMemStore{memStore: base, pos: natsutil.KVStreamPos{Created: time.Unix(1000, 0), LastSeq: 1}}
	h := newAuthHarness(t, authHarnessOpts{store: store, baseStore: base})
	c := NewInitialClaim("p1", "worker-1", h.nowVal(), time.Minute)
	_, err := store.PutIfEpoch(context.Background(), "p1", 0, c)
	require.NoError(t, err)

	store.mu.Lock()
	store.probeErr = context.DeadlineExceeded
	store.mu.Unlock()

	ev := runFaultMatrixFirstAdmit(t, h)
	require.True(t, ev.admitted, "(a) a probe failure must still count as admitted (throttle consumed)")
	require.EqualValues(t, 1, store.listCalls.Load(), "a probe failure must fail open to a full pass")

	assertBackstopDueClears(t, h)

	store.mu.Lock()
	store.probeErr = nil
	store.mu.Unlock()

	ev = runFaultMatrixNextBackstop(t, h)
	require.True(t, ev.admitted, "(c) the next attempt must land at the documented next backstop tick")
	require.EqualValues(t, 2, store.listCalls.Load(), "(d) recovery converges: the gate resumes normal operation once the fault lifts")
}

type listFailStore struct {
	ClaimStore
	mu   sync.Mutex
	fail bool
}

func (s *listFailStore) ListKeys(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()
	if fail {
		return nil, errors.New("listkeys: injected failure")
	}

	return s.ClaimStore.ListKeys(ctx)
}

func TestSweepAuthority_FaultMatrix_ListKeysFailure(t *testing.T) {
	t.Parallel()

	h := newAuthHarness(t, authHarnessOpts{})
	// A stuck, expired prepare: the reconcile arm heals it once a pass
	// can actually see it (ListKeys succeeds) — this is what "(d)
	// recovery converges" observes for this row; a ListKeys failure
	// itself leaves no other trace to check.
	stuck := Claim{
		PartitionID: "p1", Owner: "w2", PendingOwner: "w1",
		State: ClaimStatePrepare, Epoch: 3, LastUpdated: h.nowVal(), TTLSeconds: 1,
	}
	h.seed(stuck)
	failing := &listFailStore{ClaimStore: h.store, fail: true}
	h.coord.cfg.Store = failing

	ev := runFaultMatrixFirstAdmit(t, h)
	require.True(t, ev.admitted, "(a) a ListKeys failure must still count as admitted (the pass returns without healing)")
	got, rev, err := h.store.Get(context.Background(), "p1")
	require.NoError(t, err)
	require.NotZero(t, rev)
	require.Equal(t, ClaimStatePrepare, got.State, "a ListKeys failure must leave the claim untouched — the pass never saw it")

	assertBackstopDueClears(t, h)

	failing.mu.Lock()
	failing.fail = false
	failing.mu.Unlock()

	ev = runFaultMatrixNextBackstop(t, h)
	require.True(t, ev.admitted, "(c) the next attempt must land at the documented next backstop tick")
	got, rev, err = h.store.Get(context.Background(), "p1")
	require.NoError(t, err)
	require.NotZero(t, rev)
	require.Equal(t, ClaimStateStable, got.State, "(d) recovery converges: the reconcile arm heals once ListKeys works again")
}

func TestSweepAuthority_FaultMatrix_PerKeyGetFailureStillClearsInSetClocks(t *testing.T) {
	t.Parallel()

	h := newAuthHarness(t, authHarnessOpts{grace: orphanTestGrace})
	h.seed(stableClaim("p-flap", h.nowVal()))
	failing := &getFailArmedStore{ClaimStore: h.store, target: "p-flap"}
	h.coord.cfg.Store = failing

	// Seed p-flap's absence clock via an Apply-origin sweep BEFORE any
	// ticks: Apply-origin passes never sample SweepAuthority, so they
	// cannot pre-poison the generation the fault-tick fence will later
	// check (irrelevant here — this row never reaps — but keeping the
	// seed off the ticker avoids the row depending on which tick number
	// first admits).
	h.setLive() // absent
	require.NoError(t, h.coord.Apply(context.Background(), "worker-1", types.Assignment{}, types.Assignment{}))
	seedEv := h.recvEvent(t)
	require.True(t, seedEv.admitted)

	h.setLive("p-flap") // reappears in the vouched set
	failing.arm()       // this pass's Get for p-flap fails (self-disarming)

	ev := runFaultMatrixFirstAdmit(t, h)
	require.True(t, ev.admitted, "(a) a per-key Get failure must still count as admitted")

	// B3: the pass-level clear (independent of the per-key read outcome)
	// must still have cleared p-flap's absence clock.
	_, ok := h.coord.orphanAbsentSince["p-flap"]
	require.False(t, ok, "an in-set pid's clock must clear even when its own Get fails (B3)")

	assertBackstopDueClears(t, h)

	// getFailArmedStore self-disarms after firing once — no explicit
	// disarm needed; (d) recovery converges trivially: steady state is
	// maintained (p-flap stays live and clock-free) at the next
	// documented backstop tick.
	ev = runFaultMatrixNextBackstop(t, h)
	require.True(t, ev.admitted, "(c) the next attempt must land at the documented next backstop tick")
	_, ok = h.coord.orphanAbsentSince["p-flap"]
	require.False(t, ok, "(d) recovery converges: no absence clock lingers for a live pid")
	_, rev, err := h.store.Get(context.Background(), "p-flap")
	require.NoError(t, err)
	require.NotZero(t, rev, "p-flap's claim must have survived untouched")
}

type casFailStore struct {
	ClaimStore
}

func (s *casFailStore) PutIfEpoch(context.Context, string, int64, Claim) (uint64, error) {
	return 0, errors.New("cas: injected conflict")
}

func TestSweepAuthority_FaultMatrix_ReconcileCASExhaustion(t *testing.T) {
	t.Parallel()

	h := newAuthHarness(t, authHarnessOpts{})
	// A stuck, expired prepare: sweepReconcile targets it for a reset, but
	// every CAS attempt fails (contention), exhausting MaxRetries.
	stuck := Claim{
		PartitionID: "p1", Owner: "w2", PendingOwner: "w1",
		State: ClaimStatePrepare, Epoch: 3, LastUpdated: h.nowVal(), TTLSeconds: 1,
	}
	h.seed(stuck)
	h.coord.cfg.MaxRetries = 1
	failing := &casFailStore{ClaimStore: h.store}
	h.coord.cfg.Store = failing

	h.advance(2 * time.Second) // past the 1s TTL, before any tick is pushed
	ev := runFaultMatrixFirstAdmit(t, h)
	require.True(t, ev.admitted, "(a) CAS exhaustion inside the reconcile arm must still count as admitted")

	got, rev, err := h.store.Get(context.Background(), "p1")
	require.NoError(t, err)
	require.NotZero(t, rev)
	require.Equal(t, ClaimStatePrepare, got.State, "the claim must be unchanged after CAS exhaustion")

	assertBackstopDueClears(t, h)

	h.coord.cfg.Store = h.store // (d) lift the fault

	ev = runFaultMatrixNextBackstop(t, h)
	require.True(t, ev.admitted, "(c) the next attempt must land at the documented next backstop tick")
	got, rev, err = h.store.Get(context.Background(), "p1")
	require.NoError(t, err)
	require.NotZero(t, rev)
	require.Equal(t, ClaimStateStable, got.State, "(d) recovery converges: the reconcile CAS succeeds once the fault lifts")
}

// definitiveConflictStore always returns jetstream.ErrKeyExists from
// Delete — the definitive "the server answered: revision moved" outcome.
type definitiveConflictStore struct {
	ClaimStore
}

func (s *definitiveConflictStore) Delete(context.Context, string, uint64) error {
	return jetstream.ErrKeyExists
}

// TestSweepAuthority_FaultMatrix_ReapRevisionCASLoss drives the
// reap-Delete revision-CAS-loss row via a continuously-authorized
// (leader) ticker pass — see the §4.6 section doc for why the
// generation fence makes this row unreachable under backstop cadence.
func TestSweepAuthority_FaultMatrix_ReapRevisionCASLoss(t *testing.T) {
	t.Parallel()

	h := newAuthHarness(t, authHarnessOpts{grace: orphanTestGrace})
	h.seed(stableClaim("p-gone", h.nowVal()))
	h.setLive() // absent
	h.setAuthorized(true)

	h.pushTick() // seeds the clock at the current generation
	seedEv := h.recvTickEvent(t)
	require.True(t, seedEv.admitted)
	genAtSeed := h.gen()

	h.coord.cfg.Store = &definitiveConflictStore{ClaimStore: h.store}

	h.pushTickAdvance(orphanTestGrace + time.Second)
	ev := h.recvTickEvent(t)
	require.True(t, ev.admitted, "(a) a revision-CAS loss must still count as admitted")

	_, rev, err := h.store.Get(context.Background(), "p-gone")
	require.NoError(t, err)
	require.NotZero(t, rev, "a revision-CAS loss must keep the claim")
	clock, ok := h.coord.orphanAbsentSince["p-gone"]
	require.True(t, ok, "a revision-CAS loss must keep the clock")
	require.Equal(t, genAtSeed, clock.gen)
	require.Equal(t, genAtSeed, h.gen(), "a DEFINITIVE non-application must NOT self-poison the generation")

	// (b') full leader cadence is unaffected: the very next tick also
	// admits (there is no backstopDue latch under a continuously
	// authorized leader).
	h.coord.cfg.Store = h.store // (d) lift the fault

	ev = h.pushTickThenRecv(t)
	require.True(t, ev.admitted, "(b')/(c') the immediately following tick admits and converges")
	_, rev, err = h.store.Get(context.Background(), "p-gone")
	require.NoError(t, err)
	require.Zero(t, rev, "(d) recovery converges: the claim is reaped once the fault lifts")
}

// definitiveKeyGoneStore always returns jetstream.ErrKeyNotFound from
// Delete — the OTHER definitive "the server answered" outcome pinned by
// §2.2a alongside ErrKeyExists: the key is already gone. This row was
// entirely missing before (review finding P1-3): keep claim + clock, NO
// self-poison — same DEFINITIVE non-application treatment as a
// revision-CAS loss.
type definitiveKeyGoneStore struct {
	ClaimStore
}

func (s *definitiveKeyGoneStore) Delete(context.Context, string, uint64) error {
	return jetstream.ErrKeyNotFound
}

func TestSweepAuthority_FaultMatrix_ReapKeyNotFound(t *testing.T) {
	t.Parallel()

	h := newAuthHarness(t, authHarnessOpts{grace: orphanTestGrace})
	h.seed(stableClaim("p-gone", h.nowVal()))
	h.setLive()
	h.setAuthorized(true)

	h.pushTick()
	seedEv := h.recvTickEvent(t)
	require.True(t, seedEv.admitted)
	genAtSeed := h.gen()

	h.coord.cfg.Store = &definitiveKeyGoneStore{ClaimStore: h.store}

	h.pushTickAdvance(orphanTestGrace + time.Second)
	ev := h.recvTickEvent(t)
	require.True(t, ev.admitted, "(a) an ErrKeyNotFound outcome must still count as admitted")

	_, rev, err := h.store.Get(context.Background(), "p-gone")
	require.NoError(t, err)
	require.NotZero(t, rev, "an ErrKeyNotFound outcome must keep the claim")
	clock, ok := h.coord.orphanAbsentSince["p-gone"]
	require.True(t, ok, "an ErrKeyNotFound outcome must keep the clock")
	require.Equal(t, genAtSeed, clock.gen)
	require.Equal(t, genAtSeed, h.gen(), "a DEFINITIVE non-application must NOT self-poison the generation")

	h.coord.cfg.Store = h.store // (d) lift the fault

	ev = h.pushTickThenRecv(t)
	require.True(t, ev.admitted, "(b')/(c') the immediately following tick admits and converges")
	_, rev, err = h.store.Get(context.Background(), "p-gone")
	require.NoError(t, err)
	require.Zero(t, rev, "(d) recovery converges: the claim is reaped once the fault lifts")
}

// TestSweepAuthority_FaultMatrix_ReapAmbiguousOutcome drives the
// reap-Delete ambiguous-outcome row via a continuously-authorized
// (leader) ticker pass with a FAST (non-blocking) ambiguous store,
// proving SweepObserver reports it correctly (admitted=true,
// origin=ticker) — the slower TestSweepAuthority_FaultMatrix_
// ReapAmbiguousTimeoutBoundsAuthMu below additionally proves the real
// reapDeleteTimeout bound and the concurrent-sampleAuthority-blocking
// behavior, which this row does not repeat.
func TestSweepAuthority_FaultMatrix_ReapAmbiguousOutcome(t *testing.T) {
	t.Parallel()

	h := newAuthHarness(t, authHarnessOpts{grace: orphanTestGrace})
	h.seed(stableClaim("p-gone", h.nowVal()))
	h.setLive()
	h.setAuthorized(true)

	h.pushTick()
	seedEv := h.recvTickEvent(t)
	require.True(t, seedEv.admitted)
	genAtSeed := h.gen()

	store := &scheduledAmbiguousDeleteStore{ClaimStore: h.store}
	h.coord.cfg.Store = store
	store.arm()

	h.pushTickAdvance(orphanTestGrace + time.Second)
	ev := h.recvTickEvent(t)
	require.True(t, ev.admitted, "(a) an ambiguous delete outcome must still count as admitted")

	_, rev, err := h.store.Get(context.Background(), "p-gone")
	require.NoError(t, err)
	require.NotZero(t, rev, "an ambiguous outcome must keep the claim")
	require.Greater(t, h.gen(), genAtSeed, "an ambiguous outcome must self-poison the generation")

	h.coord.cfg.Store = h.store // (d) lift the fault

	// (b')/(c') full leader cadence is unaffected, but (d) full
	// recovery convergence for an AMBIGUOUS (self-poisoned) outcome is a
	// longer chain than the definitive rows above: the stale clock must
	// first discard at its next fence check, then a later pass reseeds
	// fresh, then a full fresh grace completes the reap.
	ev = h.pushTickThenRecv(t)
	require.True(t, ev.admitted)
	_, rev, err = h.store.Get(context.Background(), "p-gone")
	require.NoError(t, err)
	require.NotZero(t, rev, "the stale (self-poisoned) clock must discard, not reap")

	ev = h.pushTickThenRecv(t) // reseed
	require.True(t, ev.admitted)

	ev = h.pushTickThenRecvAdvance(t, orphanTestGrace+time.Second)
	require.True(t, ev.admitted)
	_, rev, err = h.store.Get(context.Background(), "p-gone")
	require.NoError(t, err)
	require.Zero(t, rev, "(d) recovery converges: the claim reaps once a full fresh grace elapses from the fresh seed")
}

// blockingUntilCtxDoneStore blocks Delete until ctx is done, then returns
// ctx.Err() — models a genuinely stalled store honoring only the
// caller's context, exercising the REAL reapDeleteTimeout bound (not a
// hand-parked double).
type blockingUntilCtxDoneStore struct {
	ClaimStore
	entered chan struct{}
}

func (s *blockingUntilCtxDoneStore) Delete(ctx context.Context, pid string, rev uint64) error {
	if s.entered != nil {
		close(s.entered)
	}
	<-ctx.Done()

	return ctx.Err()
}

// TestSweepAuthority_FaultMatrix_ReapAmbiguousTimeoutBoundsAuthMu pins
// the reap-Delete-ambiguous-outcome fault-matrix row against the REAL
// reapDeleteTimeout (not a hand-parked double): a stalled store's Delete
// call is bounded by the internal context.WithTimeout, so authMu is
// eventually released; a sampleAuthority call that blocks on authMu for
// the duration proceeds once the timeout expires, and its record is not
// lost (both the delete's self-poison AND the sample's own false-record
// land). Slow (~reapDeleteTimeout) by construction — it waits out a real
// bounded timeout to prove the bound, not a mock of it.
func TestSweepAuthority_FaultMatrix_ReapAmbiguousTimeoutBoundsAuthMu(t *testing.T) {
	h := newFenceHarness(t, orphanTestGrace)
	h.seed(stableClaim("p-gone", h.now))
	h.setLive()
	h.pass(t)
	genAtSeed := h.gen()
	h.advance(orphanTestGrace + time.Second)

	entered := make(chan struct{})
	h.swapStore(&blockingUntilCtxDoneStore{ClaimStore: h.store, entered: entered})

	reapDone := make(chan struct{})
	go func() {
		h.pass(t)
		close(reapDone)
	}()
	<-entered // Delete is in flight, authMu held, bounded by reapDeleteTimeout

	h.setAuthorized(false)
	sampleDone := make(chan struct{})
	start := time.Now()
	go func() {
		h.coord.sampleAuthority()
		close(sampleDone)
	}()

	<-reapDone
	<-sampleDone
	elapsed := time.Since(start)

	require.Less(t, elapsed, 10*time.Second,
		"sampleAuthority must proceed once the bounded delete times out, not block forever")
	require.True(t, h.claimExists(t, "p-gone"), "an ambiguous (timed-out) delete must keep the claim")
	require.Equal(t, genAtSeed+2, h.gen(),
		"both the ambiguous delete's self-poison AND the blocked sample's own false-record must land — neither lost")
}

// --- §4.7: split-brain R1-neutrality ---

// TestSweepAuthority_SplitBrain_R1Neutrality pins §4.7: two coordinators
// share one store, both authority=true, with DIVERGENT vouched live
// sets. The pin is R1-neutrality — the gating mechanism changes nothing
// about which reaps fire relative to a full-cadence (nil-authority)
// baseline under the identical interleaving. The divergent-view deletion
// itself is the PRE-EXISTING leader-vouched reaper contract, out of R1's
// scope; this test only pins that R1 adds no new decision.
func TestSweepAuthority_SplitBrain_R1Neutrality(t *testing.T) {
	t.Parallel()

	run := func(nilAuthority bool) bool {
		store := newMemStore()
		now := time.Now().UTC()
		store.data["p-shared"] = stableClaim("p-shared", now)
		store.rev["p-shared"] = 1

		newCoord := func() *twoPhaseCoordinator {
			cfg := Config{
				Store:         store,
				Now:           func() time.Time { return now },
				OrphanGrace:   time.Second,
				SweepInterval: -1,
				MaxRetries:    1,
				BaseBackoff:   time.Millisecond,
				MaxBackoff:    2 * time.Millisecond,
			}
			if !nilAuthority {
				cfg.SweepAuthority = func() bool { return true }
			}
			c, ok := New(cfg, true).(*twoPhaseCoordinator)
			require.True(t, ok)

			return c
		}

		coordA := newCoord() // A's view: the partition is live
		coordA.cfg.LivePartitions = func(context.Context) (map[string]struct{}, bool) {
			return map[string]struct{}{"p-shared": {}}, true
		}
		coordB := newCoord() // B's divergent view: the partition is absent
		coordB.cfg.LivePartitions = func(context.Context) (map[string]struct{}, bool) {
			return map[string]struct{}{}, true
		}

		// Identical interleaving regardless of nilAuthority: A's pass is a
		// no-op (its view has the partition live); B seeds its own
		// (process-local) absence clock; once B's grace elapses, B reaps.
		require.True(t, coordA.maybeSweepClaims(context.Background(), sweepOriginTicker))
		require.True(t, coordB.maybeSweepClaims(context.Background(), sweepOriginTicker))
		now = now.Add(2 * time.Second)
		require.True(t, coordB.maybeSweepClaims(context.Background(), sweepOriginTicker))

		_, rev, err := store.Get(context.Background(), "p-shared")
		require.NoError(t, err)

		return rev == 0 // reaped?
	}

	gatedReaped := run(false)
	baselineReaped := run(true)

	require.Equal(t, baselineReaped, gatedReaped,
		"SweepAuthority=true must not change which reaps fire vs nil authority")
}

// --- §4.8: negative space + boundary table ---

// TestSweepAuthority_BackstopEveryBoundaryTable pins §4.8's floor-division
// boundary table, plus the §2.3 default example.
func TestSweepAuthority_BackstopEveryBoundaryTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		interval time.Duration
		want     int
	}{
		{100 * time.Second, 3},
		{149 * time.Second, 2},
		{150 * time.Second, 2},
		{151 * time.Second, 1},
		{300 * time.Second, 1},
		{3600 * time.Second, 1},
		{30 * time.Second, 10}, // the §2.3 default example
	}
	for _, tc := range cases {
		t.Run(tc.interval.String(), func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, backstopEveryFor(tc.interval))
		})
	}
}

// TestSweepAuthority_ApplyOriginUnaffectedByAuthority pins §4.8: Apply
// origin cadence and admission are unaffected by authority=false.
func TestSweepAuthority_ApplyOriginUnaffectedByAuthority(t *testing.T) {
	t.Parallel()

	h := newAuthHarness(t, authHarnessOpts{})
	h.setAuthorized(false) // even a non-authorized worker's Apply sweeps run

	require.NoError(t, h.coord.Apply(context.Background(), "worker-1", types.Assignment{}, types.Assignment{}))
	ev := h.recvEvent(t)
	require.Equal(t, sweepOriginApply, ev.origin)
	require.True(t, ev.authorized, "Apply-origin authorized is a constant true marker, independent of real authority")
	require.True(t, ev.admitted)
}

// TestSweepAuthority_TickerDisableOnlyOnNegativeInterval pins §4.8: only
// a NEGATIVE internal SweepInterval disables the ticker; zero normalizes
// to the 30s default and starts it.
func TestSweepAuthority_TickerDisableOnlyOnNegativeInterval(t *testing.T) {
	t.Parallel()

	t.Run("negative disables the ticker", func(t *testing.T) {
		t.Parallel()
		coord, ok := New(Config{Store: newMemStore(), SweepInterval: -1}, true).(*twoPhaseCoordinator)
		require.True(t, ok)
		coord.Start(context.Background())
		require.False(t, coord.started.Load(), "a negative interval must never start the ticker goroutine")
	})

	t.Run("zero normalizes to the 30s default, ticker starts", func(t *testing.T) {
		t.Parallel()
		coord, ok := New(Config{Store: newMemStore(), SweepInterval: 0}, true).(*twoPhaseCoordinator)
		require.True(t, ok)
		require.Equal(t, 30*time.Second, coord.cfg.SweepInterval)
		coord.Start(context.Background())
		require.True(t, coord.started.Load())
	})
}

// --- §4.9: closed SweepTicks channel ---

// TestSweepAuthority_ClosedTicksChannelExitsWithoutSpinning pins §4.9:
// the two-value receive on SweepTicks means a CLOSED injected channel
// terminates the ticker goroutine cleanly instead of hot-looping on zero
// values. A single-value-receive regression would call SweepAuthority
// continuously; a bounded window with a counting SweepAuthority
// distinguishes the two — the fixed behavior stays at exactly 0 calls; a
// spin would rack up an enormous count within the same window.
func TestSweepAuthority_ClosedTicksChannelExitsWithoutSpinning(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	ticks := make(chan time.Time)
	close(ticks)

	coord, ok := New(Config{
		Store:         newMemStore(),
		SweepInterval: time.Second,
		SweepTicks:    ticks,
		SweepAuthority: func() bool {
			calls.Add(1)

			return true
		},
	}, true).(*twoPhaseCoordinator)
	require.True(t, ok)

	coord.Start(context.Background())
	<-time.After(20 * time.Millisecond)
	require.Zero(t, calls.Load(),
		"a closed SweepTicks channel must exit the goroutine without ever sampling authority")
}
