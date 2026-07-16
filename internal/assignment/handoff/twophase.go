package handoff

import (
	"context"
	"errors"
	"fmt"
	rand "math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/internal/ratelimit"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/sync/errgroup"
)

// followerBackstopTarget caps the GATING-INDUCED slowdown for a
// non-authority worker: its ticker cadence is never gated below
// max(SweepInterval, ~followerBackstopTarget). It is NOT an absolute
// healing-latency bound — SweepInterval has no upper bound, and a large
// configured interval slows leaders and followers alike exactly as it
// does today; the gate adds at most this factor on top.
const followerBackstopTarget = 5 * time.Minute

// reapDeleteTimeout bounds the reap arm's conditioned KV delete. The
// ticker sweep otherwise runs on the manager-lifetime context, so
// without this bound a degraded store could hold authMu (and therefore
// block authority sampling) for an unbounded request timeout. With it,
// the worst case is: a tick fires during a stalled delete, blocks in
// sampleAuthority for at most this long, then proceeds — samples are
// delayed, never lost.
const reapDeleteTimeout = 5 * time.Second

// twoPhaseCoordinator implements a prepare/commit protocol using KV-backed claims.
// It supports optional observability delays (for tests/demos) and opportunistic
// sweeping of expired non-stable claims.
//
// All latency / retries are internal; Apply should return quickly.
type twoPhaseCoordinator struct {
	cfg Config
	// sweepMu makes the sweep single-flight: maybeSweepClaims holds it
	// across the WHOLE sweep body (TryLock; a pass that loses simply
	// skips — the sweep is opportunistic and the winner is already doing
	// the work). Serializing bodies is what makes orphanAbsentSince
	// single-writer, and it closes the interleaving where an unvouched
	// pass's clock-clear lands between a concurrent vouched pass's reap
	// decision and its delete — reaping on a clock the clear should have
	// invalidated. lastSweep is guarded by it too.
	sweepMu   sync.Mutex
	lastSweep time.Time
	started   atomic.Bool

	// authMu serializes three critical sections that together implement
	// the sweep-authority generation fence: (1) sampleAuthority's
	// sample+record of SweepAuthority, (2) resolveLiveSet's vouch
	// snapshot {vouchNow, passGen}, taken (or the generation bumped)
	// every time LivePartitions is resolved, and (3) maybeReapOrphan's
	// final generation check plus its conditioned Delete. Lock ordering
	// is sweepMu -> authMu for every sweep-path acquisition;
	// sampleAuthority (the ticker's per-tick sample) takes authMu alone.
	// Never the reverse — and SweepAuthority itself must never call back
	// into the coordinator (that would be exactly the reverse-order
	// cycle).
	authMu sync.Mutex
	// unvouchedGen is a GENERATION, not a timestamp, guarded by authMu.
	// It advances once per: (a) a recorded false SweepAuthority sample;
	// (b) an unvouched vouch attempt (resolveLiveSet's liveOK=false);
	// (c) an ambiguous reap-delete outcome (self-poison) — this third
	// cause advances the generation even under nil SweepAuthority, so
	// nil authority does not make the reap arm's discard path
	// permanently unreachable: a retained clock whose delete outcome is
	// unknown must re-earn a fresh vouched grace regardless of whether
	// SweepAuthority is configured.
	unvouchedGen uint64
	// backstopDue latches a due backstop tick whose pass was NOT
	// admitted (TryLock lost to a concurrent sweep, or the in-pass
	// interval throttle absorbed it because an Apply-origin sweep ran
	// recently): every subsequent tick retries until one ticker pass is
	// admitted. Touched only by the ticker goroutine (Start's single
	// background loop) — no lock needed.
	backstopDue bool
	// testHookAfterVouchSnapshot, when non-nil, is invoked immediately
	// after resolveLiveSet's vouch snapshot and before the first claim
	// decision — in BOTH the full-pass and cached-pass bodies. TEST-ONLY:
	// always nil in production. Lets a test park a pass between the
	// vouch snapshot and its first reap decision to deterministically
	// race a false sample against it.
	testHookAfterVouchSnapshot func()

	// orphanAbsentSince records, per claim key, the vouch snapshot in
	// effect when the sweep first observed the partition absent from a
	// vouched LivePartitions set: since = the pass's vouchNow (not
	// pass-start now — strictly conservative, never seeds earlier than
	// today's behavior) and gen = the pass's unvouchedGen snapshot,
	// fencing the eventual reap decision against any false-authority
	// sample or ambiguous delete outcome recorded after the clock was
	// seeded. Entries clear when the partition reappears, when the claim
	// is reaped, when the claim key stops being listed, wholesale on an
	// unvouched pass, or on a fence mismatch at reap time (discard —
	// never re-seeded from a stale timestamp within the same pass; only
	// a later freshly vouched pass seeds a replacement). Guarded by
	// sweepMu (single-flight sweep = single writer) for map access; the
	// `gen` comparison additionally reads authMu-guarded unvouchedGen.
	// In-memory only: a restart or leadership change restarts the grace
	// clock, which is the conservative direction.
	orphanAbsentSince map[string]orphanClock

	// Sweep scan-gate state, all guarded by sweepMu like every other
	// sweep field. sweepCache holds the (claim, revision) view of the
	// bucket from the last clean ticker-origin full pass; it is valid
	// only while sweepCacheValid and the probed position still equals
	// sweepCachePos. The cache is only ever replaced whole by a clean
	// full pass — never patched.
	sweepCacheValid     bool
	sweepCachePos       natsutil.KVStreamPos
	sweepCache          map[string]cachedClaim
	sweepSkipsSinceFull int
	// sweepMaxSkips bounds consecutive gated (cached) passes before a
	// full pass is forced regardless of the probes. Initialized to
	// natsutil.ScanGateMaxSkippedPasses by New; only in-package tests
	// shorten it to reach the forced-pass backstop on a test timescale.
	// Production behavior is byte-identical to reading the constant.
	sweepMaxSkips    int
	sweepConfigGuard natsutil.ScanGateConfigGuard
	// sweepConfirmGap is the double-probe confirmation wait; tests
	// shorten it. Set by New to natsutil.ScanGateDefaultConfirmGap.
	sweepConfirmGap time.Duration
	// prober is cfg.Store's optional position-probe capability,
	// type-asserted once by New; nil disables the gate.
	prober bucketPosProber
}

// sweepOrigin tags who initiated a sweep pass. The scan gate is
// ticker-only: Apply-origin passes run the full sweep body verbatim and
// leave all gate state untouched, so the manager's apply pipeline never
// pays a probe or a confirm-gap wait.
type sweepOrigin int

const (
	sweepOriginApply sweepOrigin = iota
	sweepOriginTicker
)

// bucketPosProber is an optional capability of a ClaimStore: reporting
// the backing stream's position without creating a consumer. The sweep
// gate activates only when the store implements it; stores that do not
// (test fakes, alternative backends) get today's full-sweep behavior
// unchanged.
type bucketPosProber interface {
	BucketPos(ctx context.Context) (natsutil.KVStreamPos, error)
}

// errNoProbeHandle signals a store that implements bucketPosProber but
// was constructed without a dedicated probe handle. The coordinator
// treats it as "capability permanently absent" and stops probing.
var errNoProbeHandle = errors.New("handoff: no dedicated probe handle configured")

// cachedClaim is one entry of the sweep's claim cache: the body and KV
// revision a clean full pass read.
type cachedClaim struct {
	claim Claim
	rev   uint64
}

// orphanClock is one absence-clock entry: the vouch snapshot the clock
// was seeded from (see orphanAbsentSince).
type orphanClock struct {
	since time.Time
	gen   uint64
}

// vouchSnapshot is the {vouchNow, passGen} pair resolveLiveSet takes,
// under authMu, when LivePartitions vouches for the live set (liveOK ==
// true). Every absence clock seeded during the pass carries this
// snapshot, so a reap decision can later fence itself against any
// unvouched observation recorded after the vouch.
type vouchSnapshot struct {
	now time.Time
	gen uint64
}

// backstopEveryFor derives the follower backstop's tick period from the
// (already-normalized, guaranteed positive) sweep interval: FLOOR
// division, floored at 1 (a no-op — the follower keeps the configured
// full cadence — once the interval alone already exceeds
// followerBackstopTarget). Extracted as a pure function so the boundary
// table (§4.8) is directly unit-testable.
func backstopEveryFor(interval time.Duration) int {
	return max(1, int(followerBackstopTarget/interval))
}

// sampleAuthority is the ONLY path by which a ticker fire observes
// SweepAuthority. Sample and record are one authMu critical section, so
// a false sample can never exist "observed but not yet recorded" while a
// concurrent reap fences its decision against unvouchedGen. A nil
// SweepAuthority short-circuits to authorized == true without touching
// the generation — nil-authority ticker cadence, scan-gate behavior, and
// the reconcile arms stay byte-for-byte today's.
func (t *twoPhaseCoordinator) sampleAuthority() bool {
	t.authMu.Lock()
	defer t.authMu.Unlock()
	authorized := t.cfg.SweepAuthority == nil || t.cfg.SweepAuthority()
	if !authorized {
		t.unvouchedGen++
	}

	return authorized
}

// Start launches a background goroutine that sweeps stale claims at SweepInterval.
//
// This ensures stale/expired claims are cleaned up even in idle systems where
// Apply is rarely called. The goroutine exits when ctx is cancelled (or, in
// tests, when an injected SweepTicks channel is closed).
//
// Every tick samples sweep authority (sampleAuthority). An authorized
// (leader) worker sweeps at the full configured cadence, byte-for-byte
// today's behavior. A non-authorized worker only attempts a pass on
// backstop ticks — every backstopEvery-th tick, phase-staggered by
// SweepPhaseSeed across the fleet — unless a previous due tick's pass was
// not admitted (backstopDue latch), in which case every subsequent tick
// retries until one pass is admitted. This bounds ticker-driven healing
// on non-authority workers to roughly (backstopEvery+1) x SweepInterval
// even when no worker holds authority, while a healthy leader keeps
// today's full cadence.
//
// Start is idempotent: calling it more than once has no effect.
func (t *twoPhaseCoordinator) Start(ctx context.Context) {
	if t.cfg.SweepInterval <= 0 {
		return
	}
	if !t.started.CompareAndSwap(false, true) {
		return // already started
	}

	// backstopEveryFor always returns >= 1 (max(1, ...)), so this
	// narrowing to uint64 never wraps; computed once here so the
	// modulo check inside the goroutine needs no repeated conversion.
	backstopEvery := uint64(backstopEveryFor(t.cfg.SweepInterval)) //nolint:gosec // G115: backstopEveryFor >= 1 always
	phase := t.cfg.SweepPhaseSeed % backstopEvery

	go func() {
		tickSrc := t.cfg.SweepTicks
		if tickSrc == nil {
			ticker := time.NewTicker(t.cfg.SweepInterval)
			defer ticker.Stop()
			tickSrc = ticker.C
		}

		var tick uint64 // coordinator-lifetime monotonic; never reset on authority flaps
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-tickSrc:
				if !ok {
					return // closed injected channel: clean test teardown
				}
				tick++
				authorized := t.sampleAuthority()
				if !authorized {
					if (tick+phase)%backstopEvery != 0 && !t.backstopDue {
						continue // follower off-cycle: no probe, no KV I/O
					}
					t.backstopDue = true
				}
				admitted := t.maybeSweepClaims(ctx, sweepOriginTicker)
				if admitted {
					t.backstopDue = false
				}
				if t.cfg.SweepObserver != nil {
					t.cfg.SweepObserver(sweepOriginTicker, authorized, admitted)
				}
			}
		}
	}()
}

// transitionVersionAdjacent reports whether next is the immediate version
// successor of previous (previous.Version+1 == next.Version). Adjacency
// detects VERSION GAPS ONLY — it is necessary for trusting the prepare
// diff, but it is NOT an authoritative or claim-level continuity proof:
// equal-Version authority divergence and stale cross-worker claim mutation
// (the deferred fence project's shapes) are invisible to it. No future
// walk restriction may treat adjacency==true as authorization on its own.
//
// Any gap (a coalesced or missed intermediate version — the routine case,
// since debounce coalescing, warm restart, and the apply/fetch stashes are
// designed gap sources, not rare ones), equality (same-Version redelivery
// with possibly different content — the alias-vs-commit shape), or
// regression is treated as UNPROVEN and fails open.
//
// The equality/regression arms are defense in depth, not a claimed
// production-reachable path: production dispatch drops same-or-stale
// Version deliveries before they ever reach Apply (manager_assignment.go
// alias and commit handlers, ~lines 1070-1085), so in practice only the gap
// shape is exercised. See Apply's doc comment for what this check does and
// does not repair.
func transitionVersionAdjacent(previous, next types.Assignment) bool {
	return previous.Version+1 == next.Version
}

// Apply performs the multi-phase handoff application for the worker's new assignment.
// Steps: prepare -> (optional delay) -> apply consumer -> commit -> (optional delay) -> stabilize.
//
// Fail-open on unproven continuity (PARTIAL repair). previous is not
// necessarily next's direct predecessor — production callers diff against
// the worker's last COMMITTED assignment, which a coalesced retry/stash can
// leave more than one version behind next. When
// transitionVersionAdjacent(previous, next) is false, Apply treats previous
// as empty for the prepare phase only (commit and stabilize already walk
// all of next unconditionally and are untouched): prepare then re-evaluates
// every partition in next, so a foreign stable claim is re-recorded as
// PendingOwner and a missing claim is recreated via NewInitialClaim, both
// carried to stable by the unchanged commit/stabilize phases.
//
// Scope: this heals missed-version (gap) discontinuities only — including
// both of the round-1 counterexamples (A regains a partition coalesced
// through an intermediate owner; a reaped claim is recreated after a missed
// removal/re-add). It is NOT an end-to-end continuity proof. Two shapes
// stay open, tracked as findings, not covered here: (a) equal-Version
// authority changes (an alias and a commit can expose different content at
// the same Version; a publisher's CAS-loss recovery can also emit two
// different assignments at the same Version) never reach a full Apply,
// because production dispatch drops <=-Version deliveries before Apply is
// even called; (b) claims carry no assignment Version/commit identity, so a
// stale cross-worker retry can still mutate a claim behind an adjacent,
// locally-continuous transition. Both require the deferred claim-level
// commit-identity fence project.
func (t *twoPhaseCoordinator) Apply(ctx context.Context, workerID string, previous, next types.Assignment) error {
	// Opportunistic sweep of expired/non-stable claims; skip metrics as requested.
	// Runs before any early returns to maximize cleanup. Apply-origin sweeps
	// are never gated by sweep authority (untouched, every worker): the
	// authorized value reported to the observer is a constant true marker.
	admitted := t.maybeSweepClaims(ctx, sweepOriginApply)
	if t.cfg.SweepObserver != nil {
		t.cfg.SweepObserver(sweepOriginApply, true, admitted)
	}

	inst := newInstrumenter(t.cfg.Metrics)

	// Prepare diffs against previous only when continuity is proven; on any
	// gap/equality/regression, previous is treated as empty so prepare
	// walks all of next (see the fail-open doc above). Commit and stabilize
	// are unconditionally full-next walks already and are untouched.
	preparePrevious := previous
	if !transitionVersionAdjacent(previous, next) {
		preparePrevious = types.Assignment{}
		if t.cfg.Logger != nil {
			t.cfg.Logger.Debug("handoff_discontinuous_apply",
				"worker_id", workerID,
				"previous_version", previous.Version,
				"next_version", next.Version,
				"partitions", len(next.Partitions),
			)
		}
	}

	// Phase: prepare - write/update claims for partitions being added.
	if err := inst.phase("prepare", func() error { return t.preparePhase(ctx, workerID, preparePrevious, next) }); err != nil {
		inst.finish(err)
		return err
	}

	// Optional testing/demo delay to expose prepare state externally.
	if t.cfg.DelayAfterPrepare > 0 {
		select {
		case <-ctx.Done():
			inst.finish(ctx.Err())
			return ctx.Err()
		case <-time.After(t.cfg.DelayAfterPrepare):
		}
	}

	// Phase: removal guard — block consumer removal until any in-flight transfer commits.
	if t.cfg.RemovalGuard != nil {
		if err := t.cfg.RemovalGuard(ctx, workerID, previous, next); err != nil {
			inst.finish(err)
			return err
		}
	}

	// Phase: apply (delegate to updater for now)
	if t.cfg.ConsumerUpdater != nil {
		err := inst.phase("apply", func() error {
			return t.cfg.ConsumerUpdater.UpdateWorkerConsumer(ctx, workerID, next.Partitions)
		})
		if err != nil {
			inst.finish(err)
			return err
		}
	}

	// Phase: commit - transition prepared claims to commit (owner switch pending).
	if err := inst.phase("commit", func() error { return t.commitPhase(ctx, workerID, next) }); err != nil {
		inst.finish(err)
		return err
	}

	// Optional testing/demo delay to expose commit state externally before stabilizing.
	if t.cfg.DelayBeforeStable > 0 {
		select {
		case <-ctx.Done():
			inst.finish(ctx.Err())
			return ctx.Err()
		case <-time.After(t.cfg.DelayBeforeStable):
		}
	}

	// Phase: stabilize - finalize commit to stable.
	if err := inst.phase("stabilize", func() error { return t.stabilizePhase(ctx, workerID, next) }); err != nil {
		inst.finish(err)
		return err
	}

	inst.finish(nil)

	// Placeholder error injection pattern example (disabled):
	if false {
		return errors.New("two-phase coordinator error")
	}

	return nil
}

// updateClaim performs a read-modify-write cycle with retries on CAS failure.
// It fetches the latest claim, applies the transform function, and attempts to save.
// If the transform returns nil, the update is skipped.
// updateClaim is the central contention-aware read/modify/write helper.
//
// Callers provide a pure transformation from the current claim (or nil if
// no claim exists yet) to the desired next claim. updateClaim then:
//  1. Reads the latest claim from the store on each attempt.
//  2. Invokes the transform with that snapshot.
//  3. Attempts a CAS via PutIfEpoch.
//  4. On CAS conflict, backs off with jitter and retries from step 1.
//
// This design guarantees we never write based on stale epochs while still
// allowing all higher-level phase logic (prepare/commit/stabilize/sweep) to
// remain simple and expressed in terms of claim state transitions.
func (t *twoPhaseCoordinator) updateClaim(ctx context.Context, pid string, transform func(*Claim) (*Claim, error)) error {
	var err error
	for attempt := 0; attempt <= t.cfg.MaxRetries; attempt++ {
		// 1. Get latest state
		cur, rev, getErr := t.cfg.Store.Get(ctx, pid)
		if getErr != nil {
			return fmt.Errorf("claim get: %w", getErr)
		}

		// 2. Apply transformation
		var input *Claim
		if rev > 0 {
			input = &cur
		}
		next, transErr := transform(input)
		if transErr != nil {
			return transErr // Permanent error from logic
		}
		if next == nil {
			return nil // No update needed
		}

		// 3. Attempt CAS. Pace the physical write first if a claim-write limiter
		// is configured: this gates every PutIfEpoch attempt, including CAS
		// retries (the loop re-enters here), so a contention storm is paced at
		// the same rate as a first-try write. A nil limiter is unlimited (no
		// wait). ctx cancellation (shutdown/timeout during a paced wait) aborts
		// the loop so the apply fails pre-commit and the manager retries.
		if err := ratelimit.Wait(ctx, t.cfg.ClaimWriteLimiter); err != nil {
			return err
		}
		var casErr error
		if rev == 0 {
			_, casErr = t.cfg.Store.PutIfEpoch(ctx, pid, 0, *next)
		} else {
			_, casErr = t.cfg.Store.PutIfEpoch(ctx, pid, cur.Epoch, *next)
		}

		if casErr == nil {
			return nil // Success
		}

		// 4. Handle CAS failure (contention)
		if attempt == t.cfg.MaxRetries {
			err = casErr
			break
		}

		if t.cfg.Metrics != nil {
			t.cfg.Metrics.IncCASConflicts()
		}

		// Backoff logic
		if attempt == 0 {
			continue // Immediate retry
		}

		effAttempt := attempt - 1
		backoff := min(t.cfg.BaseBackoff<<effAttempt, t.cfg.MaxBackoff)
		d := backoff
		if t.cfg.Jitter > 0 {
			//nolint:gosec // jitter does not require crypto secure random
			f := rand.Float64()
			low := 1 - t.cfg.Jitter
			high := 1 + t.cfg.Jitter
			d = time.Duration(float64(backoff) * (low + f*(high-low)))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}

	return fmt.Errorf("update claim failed after retries: %w", err)
}

// preparePhase writes/updates claims for partitions newly acquired in 'next' relative to 'previous'.
func (t *twoPhaseCoordinator) preparePhase(ctx context.Context, workerID string, previous, next types.Assignment) error {
	if t.cfg.Store == nil {
		return nil
	}

	now := t.cfg.Now()

	// Build index of old partitions for diff
	oldIndex := make(map[uint64]types.Partition, len(previous.Partitions))
	for _, p := range previous.Partitions {
		oldIndex[p.HashID()] = p
	}

	// Identify partitions that need preparation
	var toPrepare []types.Partition
	for _, p := range next.Partitions {
		if _, exists := oldIndex[p.HashID()]; !exists {
			toPrepare = append(toPrepare, p)
		}
	}

	if len(toPrepare) == 0 {
		return nil
	}

	// Process in parallel with bounded concurrency
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(t.cfg.PhaseConcurrency)

	for _, p := range toPrepare {
		g.Go(func() error {
			// Create or transition claim for new partition. The claim is keyed
			// by SubjectKey() (dot-joined) — the same partition identity the
			// consumer's pull gating and processing gate derive from the
			// partition subject. Keying by ID() (dash-joined) would not match
			// for partitions with more than one key, leaving GetOwner unable to
			// resolve the claim.
			pid := p.SubjectKey()
			// didReset is captured by the transform closure so we can emit
			// the IncClaimStaleHandoffReset metric exactly once, AFTER
			// updateClaim's CAS succeeds. The transform may be invoked
			// multiple times when CAS conflicts force a retry; resetting
			// the flag at the top of each invocation keeps it accurate
			// for whichever invocation produced the durable write.
			var didReset bool
			err := t.updateClaim(gCtx, pid, func(cur *Claim) (*Claim, error) {
				didReset = false
				if cur == nil { // create initial claim
					init := NewInitialClaim(pid, workerID, now, t.cfg.TTL)
					if t.cfg.Logger != nil {
						t.cfg.Logger.Debug("handoff_prepare",
							"partition_id", pid,
							"worker_id", workerID,
							"prev_owner", "",
							"next_owner", workerID,
							"state", string(ClaimStateStable),
							"epoch", init.Epoch,
						)
					}

					return &init, nil
				}

				// Existing claim; if stable and owned by someone else, enter prepare
				if cur.Owner != workerID {
					prepared := cur.NextPrepare(workerID, now)
					if t.cfg.Logger != nil {
						t.cfg.Logger.Debug("handoff_prepare",
							"partition_id", pid,
							"worker_id", workerID,
							"prev_owner", cur.Owner,
							"next_owner", workerID,
							"state", string(ClaimStatePrepare),
							"epoch", prepared.Epoch,
						)
					}

					return &prepared, nil
				}

				// cur.Owner == workerID. The partition is being re-acquired by
				// its existing owner. If a stale in-flight handoff to another
				// worker is still recorded on the claim (state != stable or
				// pendingOwner != ""), reset it back to clean stable. This
				// handles the A->B->A revert race where B's commitPhase never
				// completed: without this reset, the claim stays at
				// state=prepare forever and the processing gate suppresses
				// pulls with state_not_allowed(prepare).
				if cur.State != ClaimStateStable || cur.PendingOwner != "" {
					cleaned := *cur
					cleaned.PendingOwner = ""
					cleaned.State = ClaimStateStable
					cleaned.Epoch++
					cleaned.LastUpdated = now.UTC()
					if t.cfg.Logger != nil {
						t.cfg.Logger.Info("handoff_prepare_reset_stale",
							"partition_id", pid,
							"worker_id", workerID,
							"prev_state", string(cur.State),
							"prev_pending", cur.PendingOwner,
							"epoch", cleaned.Epoch,
						)
					}
					didReset = true

					return &cleaned, nil
				}

				//nolint:nilnil // (nil, nil) is the documented no-update signal for updateClaim's transform.
				return nil, nil
			})
			if err != nil {
				return err
			}
			if didReset && t.cfg.Metrics != nil {
				t.cfg.Metrics.IncClaimStaleHandoffReset()
			}

			return nil
		})
	}

	return g.Wait()
}

// commitPhase transitions prepared claims to commit for partitions now owned by the worker.
func (t *twoPhaseCoordinator) commitPhase(ctx context.Context, workerID string, next types.Assignment) error {
	if t.cfg.Store == nil {
		return nil
	}

	now := t.cfg.Now()

	// Process in parallel with bounded concurrency
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(t.cfg.PhaseConcurrency)

	for _, p := range next.Partitions {
		g.Go(func() error {
			// Keyed by SubjectKey() to match the claim written in preparePhase
			// and the partition identity the consumer resolves ownership by.
			pid := p.SubjectKey()
			return t.updateClaim(gCtx, pid, func(cur *Claim) (*Claim, error) {
				if cur == nil {
					return nil, nil
				}
				if cur.Owner == workerID && cur.State == ClaimStateStable {
					return nil, nil // already finalized
				}

				// If we're pending owner or current owner differs, move to commit.
				if cur.PendingOwner == workerID || cur.Owner != workerID {
					committed := *cur
					if cur.State != ClaimStateCommit {
						committed = cur.NextCommit(now)
					}

					if t.cfg.Logger != nil {
						t.cfg.Logger.Info("handoff_commit",
							"partition_id", pid,
							"worker_id", workerID,
							"prev_owner", cur.Owner,
							"next_owner", committed.Owner,
							"state", string(ClaimStateCommit),
							"epoch", committed.Epoch,
						)
					}

					return &committed, nil
				}

				return nil, nil
			})
		})
	}

	return g.Wait()
}

// stabilizePhase finalizes commit claims to stable for partitions in next.
func (t *twoPhaseCoordinator) stabilizePhase(ctx context.Context, workerID string, next types.Assignment) error {
	if t.cfg.Store == nil {
		return nil
	}

	now := t.cfg.Now()

	// Process in parallel with bounded concurrency
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(t.cfg.PhaseConcurrency)

	for _, p := range next.Partitions {
		g.Go(func() error {
			// Keyed by SubjectKey() to match the claim written in preparePhase
			// and the partition identity the consumer resolves ownership by.
			pid := p.SubjectKey()
			return t.updateClaim(gCtx, pid, func(cur *Claim) (*Claim, error) {
				if cur == nil {
					return nil, nil
				}
				if cur.Owner != workerID {
					return nil, nil // not ours yet
				}
				if cur.State != ClaimStateCommit {
					return nil, nil // only finalize from commit
				}

				stabilized := cur.NextStable(now)
				if t.cfg.Logger != nil {
					t.cfg.Logger.Info("handoff_stable",
						"partition_id", pid,
						"worker_id", workerID,
						"owner", stabilized.Owner,
						"epoch", stabilized.Epoch,
					)
				}

				return &stabilized, nil
			})
		})
	}

	return g.Wait()
}

// maybeSweepClaims reconciles non-terminal claims toward stable. It handles two cases:
//
//  1. A committed-but-unstabilized claim (state == commit, no PendingOwner) is
//     finalized to stable PROMPTLY, independent of TTL. This recovers a
//     stabilizePhase that was missed because its CAS exhausted MaxRetries under
//     two-worker contention; without it, convergence would have to wait for the
//     advisory HandoffTTL to elapse (the expired-reset path below). NextCommit
//     always clears PendingOwner and sets the committed Owner, so finalizing via
//     NextStable preserves ownership and only completes the bookkeeping — exactly
//     what stabilizePhase would have done.
//  2. Any expired non-stable claim (e.g. a stuck prepare) is reset back to stable,
//     clearing any pending owner (the original stale-claim safety net).
//
// Runs at most once per configured sweep interval. If SweepInterval <= 0, runs
// every time — but never concurrently: sweep bodies are single-flight (see the
// sweepMu field doc), and a pass arriving while another sweep is mid-body is
// skipped rather than blocked, so Apply never waits on another sweep's KV I/O.
//
// Ticker-origin passes are additionally scan-gated: when the store can
// report its backing stream position (bucketPosProber) and two probes
// sweepConfirmGap apart both equal the position latched by the last
// clean full pass, the pass body runs against the cached claim view
// instead of a fresh ListKeys + per-key Get storm. The cached pass runs
// every decision arm — commit finalization, TTL-expiry reset, orphan
// reaping against a FRESH LivePartitions resolution — so time-triggered
// work keeps firing at today's cadence on an idle bucket. Apply-origin
// passes never use the gate (no probe, no confirm wait, no gate-state
// mutation): the manager's apply pipeline is byte-for-byte unchanged.
//
// Returns admitted: false on the Store == nil, TryLock-miss, and
// interval-throttle returns; true from the moment the interval throttle
// is consumed (lastSweep advanced) — even if the body then aborts on
// probe failure (fail-open full pass runs, per today's contract) or on
// context cancellation mid-probe (only reachable at shutdown). This
// "admitted = throttle consumed" definition matches the throttle
// semantics that already gate the next pass.
func (t *twoPhaseCoordinator) maybeSweepClaims(ctx context.Context, origin sweepOrigin) (admitted bool) {
	if t.cfg.Store == nil {
		return false
	}
	if !t.sweepMu.TryLock() {
		return false // another sweep is mid-body; this opportunistic pass skips
	}
	defer t.sweepMu.Unlock()

	now := t.cfg.Now()
	if t.cfg.SweepInterval > 0 && !t.lastSweep.IsZero() && now.Sub(t.lastSweep) < t.cfg.SweepInterval {
		return false
	}
	t.lastSweep = now

	// The gate is ticker-only; stores without the probe capability are
	// never gated either. Both cases run today's body verbatim and leave
	// all gate state untouched (coherence needs no bookkeeping: any
	// write such a pass performs advances the stream position, so a
	// stale cache can never satisfy the next ticker probe).
	if origin != sweepOriginTicker || t.prober == nil {
		t.sweepPass(ctx, now, nil)

		return true
	}

	pos, ok, reason := t.sweepProbe(ctx)
	if ok && t.sweepCacheValid && pos.Same(t.sweepCachePos) {
		if t.sweepSkipsSinceFull >= t.sweepMaxSkips {
			reason = "forced"
		} else {
			// Double-probe confirmation. The wait holds sweepMu on the
			// ticker goroutine (which has no other duties); a concurrent
			// Apply's opportunistic sweep TryLock-skips rather than
			// blocks. ctx cancellation aborts the pass with gate state
			// untouched.
			if !t.sweepConfirmWait(ctx) {
				return true
			}
			pos2, ok2, reason2 := t.sweepProbe(ctx)
			if ok2 && pos2.Same(t.sweepCachePos) {
				t.sweepCachedPass(ctx, now)

				return true
			}
			pos, ok = pos2, ok2
			reason = reason2
			if reason == "" {
				reason = "mismatch"
			}
		}
	} else if ok && reason == "" {
		if t.sweepCacheValid {
			reason = "mismatch"
		} else {
			reason = "unlatched"
		}
	}

	var latch *natsutil.KVStreamPos
	if ok {
		latch = &pos
	}
	t.sweepPass(ctx, now, latch)
	if t.cfg.Logger != nil {
		t.cfg.Logger.Debug("handoff sweep full pass",
			"cached_passes_since_last_full", t.sweepSkipsSinceFull,
			"reason", reason,
		)
	}
	t.sweepSkipsSinceFull = 0

	return true
}

// sweepProbe takes one stream-position probe under the fail-open and
// config-validation rules: any error or unsafe config invalidates the
// cache so no cached pass can run until a clean full pass re-latches.
// Runs under sweepMu.
func (t *twoPhaseCoordinator) sweepProbe(ctx context.Context) (natsutil.KVStreamPos, bool, string) {
	pos, err := t.prober.BucketPos(ctx)
	if err != nil {
		t.sweepCacheValid = false
		if errors.Is(err, errNoProbeHandle) {
			// The store implements the capability interface but has no
			// probe handle: permanently ungate this coordinator so we
			// stop paying a doomed probe (and a log line) every tick.
			t.prober = nil
			if t.cfg.Logger != nil {
				t.cfg.Logger.Debug("handoff sweep gate disabled: store has no probe handle")
			}

			return natsutil.KVStreamPos{}, false, "no_probe_handle"
		}
		if t.cfg.Logger != nil {
			t.cfg.Logger.Debug("handoff sweep: stream position probe failed", "error", err)
		}

		return natsutil.KVStreamPos{}, false, "probe_error"
	}
	if !t.sweepConfigGuard.Check(pos, t.cfg.Logger) {
		t.sweepCacheValid = false

		return pos, false, "unsafe_config"
	}

	return pos, true, ""
}

// sweepConfirmWait blocks for sweepConfirmGap or until ctx cancellation.
// Returns false when the pass should be aborted.
func (t *twoPhaseCoordinator) sweepConfirmWait(ctx context.Context) bool {
	timer := time.NewTimer(t.sweepConfirmGap)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// resolveLiveSet resolves the live partition set once per pass for
// orphan reaping. liveOK=false (supplier not vouching: not the leader,
// source down) skips all reap decisions for the pass AND resets every
// absence clock: time spent unvouched is time this worker could not
// verify continuous absence, so it must not count toward OrphanGrace —
// otherwise a clock started before a long follower stint would reap
// instantly on the first vouched pass after it.
//
// liveOK=true clears the absence clock for every pid PRESENT in this
// pass's live set, pass-level and independent of any later per-claim
// read outcome. Without this, a claim that reappears in a vouched live
// set on a pass whose per-key Get then fails would skip sweepClaim
// entirely (see sweepPass), leaving maybeReapOrphan's own in-set clear
// unreached; a later pass could then reap using the ORIGINAL absence
// timestamp instead of requiring a fresh grace measured from the
// SECOND disappearance. maybeReapOrphan's in-set clear stays as
// defense in depth for the common case.
//
// liveOK=true additionally takes — under authMu, as one atomic snapshot
// alongside the clock clear above — {vouchNow: cfg.Now(), passGen:
// unvouchedGen} and returns it as vouch. Every clock seeded during this
// pass carries this snapshot: since = vouchNow (strictly conservative vs
// pass-start now — never seeds earlier than today's behavior), and gen
// fences the eventual reap decision against any unvouched observation
// recorded after this instant. liveOK=false additionally increments
// unvouchedGen under authMu (self-poison: an unvouched pass invalidates
// every clock, matching its wholesale clear above).
//
// Runs under sweepMu, called by both the full-pass and cached-pass
// bodies.
func (t *twoPhaseCoordinator) resolveLiveSet(ctx context.Context) (live map[string]struct{}, liveOK bool, vouch vouchSnapshot) {
	if t.cfg.OrphanGrace <= 0 || t.cfg.LivePartitions == nil {
		return nil, false, vouchSnapshot{}
	}
	live, liveOK = t.cfg.LivePartitions(ctx)
	if !liveOK {
		clear(t.orphanAbsentSince)
		t.authMu.Lock()
		t.unvouchedGen++
		t.authMu.Unlock()

		return live, false, vouchSnapshot{}
	}

	for pid := range t.orphanAbsentSince {
		if _, ok := live[pid]; ok {
			delete(t.orphanAbsentSince, pid)
		}
	}

	t.authMu.Lock()
	vouch = vouchSnapshot{now: t.cfg.Now(), gen: t.unvouchedGen}
	t.authMu.Unlock()

	return live, true, vouch
}

// sweepClaim runs the per-claim sweep decision arms shared by full and
// cached passes: the sweepReconcile pre-filter (with the authoritative
// re-decide on a fresh read inside updateClaim's CAS loop) and, for
// claims needing no reconcile, the orphan-reap check.
func (t *twoPhaseCoordinator) sweepClaim(
	ctx context.Context,
	pid string,
	cur Claim,
	rev uint64,
	live map[string]struct{},
	liveOK bool,
	now time.Time,
	vouch vouchSnapshot,
) {
	// Cheap pre-filter: skip a CAS write attempt when this claim needs
	// no reconcile (the common case for already-stable claims).
	if sweepReconcile(&cur, now) != nil {
		if err := t.updateClaim(ctx, pid, func(cur *Claim) (*Claim, error) {
			// Re-decide on the fresh read inside the CAS loop.
			return sweepReconcile(cur, now), nil
		}); err == nil {
			if t.cfg.Metrics != nil {
				t.cfg.Metrics.IncClaimStoreStale()
			}
		}
		// A just-reconciled claim is in flux; it is not reap-eligible
		// this pass. The next pass re-evaluates it from a fresh read.
		return
	}

	if liveOK {
		t.maybeReapOrphan(ctx, pid, cur, rev, live, now, vouch)
	}
}

// sweepPass is the full sweep body: ListKeys + per-key Get + decision
// arms. When latch is non-nil (a gated ticker-origin pass with a usable
// probe), the pass additionally rebuilds the claim cache and latches
// the probed position iff the pass was clean: list succeeded (an empty
// list is clean — latch an empty cache), and zero per-key Gets errored
// (reads returning rev == 0 — deleted between list and get — do not
// block latching; that deletion consumed a sequence after the probe, so
// the very next probe unlatches anyway). When latch is nil (Apply
// origin, no prober, or an unusable probe) the body is today's sweep,
// byte-for-byte, and gate state is untouched. Runs under sweepMu.
func (t *twoPhaseCoordinator) sweepPass(ctx context.Context, now time.Time, latch *natsutil.KVStreamPos) {
	keys, err := t.cfg.Store.ListKeys(ctx)
	if err != nil {
		if latch != nil {
			t.sweepCacheValid = false
		}

		return
	}
	if len(keys) == 0 {
		if latch != nil {
			// A clean empty pass: without latching here, a bucket whose
			// last claim was deleted would either full-scan forever or
			// retain a stale non-empty cache. The early return itself is
			// today's behavior (no metric call, no live-set resolution,
			// no prune).
			t.latchCache(make(map[string]cachedClaim), *latch)
		}

		return
	}
	if t.cfg.Metrics != nil {
		t.cfg.Metrics.SetClaimStoreSize(len(keys))
	}

	live, liveOK, vouch := t.resolveLiveSet(ctx)
	if t.testHookAfterVouchSnapshot != nil {
		t.testHookAfterVouchSnapshot()
	}

	var cache map[string]cachedClaim
	if latch != nil {
		cache = make(map[string]cachedClaim, len(keys))
	}
	getFailed := false

	for _, pid := range keys {
		cur, rev, err := t.cfg.Store.Get(ctx, pid)
		if err != nil {
			getFailed = true

			continue
		}
		if rev == 0 {
			continue
		}
		if cache != nil {
			cache[pid] = cachedClaim{claim: cur, rev: rev}
		}
		t.sweepClaim(ctx, pid, cur, rev, live, liveOK, now, vouch)
	}

	if liveOK {
		t.pruneOrphanCandidates(keys)
	}

	if latch != nil {
		if getFailed {
			// A transient read failure: do not trust this pass's view.
			// The cache is invalidated so the next ticker pass re-walks —
			// today's retry-on-next-pass semantics, unweakened.
			t.sweepCacheValid = false
		} else {
			t.latchCache(cache, *latch)
		}
	}
}

// latchCache replaces the sweep's cached bucket view whole and records the
// probed position it is valid at. Callers latch only after a clean full
// pass; the cache is never patched incrementally.
func (t *twoPhaseCoordinator) latchCache(cache map[string]cachedClaim, pos natsutil.KVStreamPos) {
	t.sweepCache = cache
	t.sweepCachePos = pos
	t.sweepCacheValid = true
}

// sweepCachedPass runs the sweep decision arms against the cached claim
// view. Under the gate invariant the cached bodies and revisions ARE the
// current KV state, so this pass is behavior-identical to a full pass —
// minus the ListKeys and per-key Get storm. Time and the live set are
// the only inputs that legitimately change without bucket writes, and
// both are re-read fresh. Runs under sweepMu.
func (t *twoPhaseCoordinator) sweepCachedPass(ctx context.Context, now time.Time) {
	// Accounting first: empty cached passes must count toward the
	// forced full pass too.
	t.sweepSkipsSinceFull++
	if t.sweepSkipsSinceFull == 1 && t.cfg.Logger != nil {
		t.cfg.Logger.Debug("handoff sweep gate engaged")
	}
	if len(t.sweepCache) == 0 {
		return // mirrors today's empty-list early return
	}
	if t.cfg.Metrics != nil {
		// Identical to a fresh list's count: the key set provably has
		// not changed.
		t.cfg.Metrics.SetClaimStoreSize(len(t.sweepCache))
	}

	live, liveOK, vouch := t.resolveLiveSet(ctx)
	if t.testHookAfterVouchSnapshot != nil {
		t.testHookAfterVouchSnapshot()
	}

	for pid, cc := range t.sweepCache {
		t.sweepClaim(ctx, pid, cc.claim, cc.rev, live, liveOK, now, vouch)
	}

	// The cached key set IS the current bucket key set (the gate proved
	// the bucket unchanged), so prune absence clocks directly against the
	// cache instead of materializing a throwaway key slice for
	// pruneOrphanCandidates.
	if liveOK && len(t.orphanAbsentSince) > 0 {
		for pid := range t.orphanAbsentSince {
			if _, ok := t.sweepCache[pid]; !ok {
				delete(t.orphanAbsentSince, pid)
			}
		}
	}
}

// maybeReapOrphan applies the orphan-reap decision for one claim against a
// vouched live partition set:
//
//   - in the set → clear any absence clock, keep;
//   - not stable, or a pending owner recorded → keep (in-flight handoffs are
//     the existing sweep arms' job, never the reaper's);
//   - first vouched absence → start the clock (seeded from vouch, not
//     pass-start now), keep;
//   - absent for >= OrphanGrace → the fenced decide-and-delete below.
//
// Fenced decide-and-delete: once grace has elapsed, the clock's gen is
// compared against the current unvouchedGen under authMu. A mismatch
// means an unvouched observation (false sample, unvouched vouch attempt,
// or an earlier ambiguous delete) was recorded after this clock's vouch:
// the window is invalid, so the clock is DISCARDED (deleted, not
// re-seeded from any stale timestamp) and the pass returns without
// reaping — only a later freshly vouched pass may seed a replacement
// from its own snapshot. On a generation match, the conditioned delete
// runs with a bounded reapDeleteTimeout (the ticker sweep otherwise runs
// on the manager-lifetime context) while still holding authMu, and its
// outcome is classified:
//
//   - nil: DEFINITIVE application — clear the clock;
//   - jetstream.ErrKeyExists / ErrKeyNotFound: DEFINITIVE non-application
//     — the server answered (revision moved / key already gone); keep
//     the claim and clock, exactly as today's expected-loser skip;
//   - anything else: AMBIGUOUS — the delete request was already sent and
//     a context expiry only stops the client-side wait; the server may
//     still apply it later. Keep the claim and clock AND self-poison the
//     generation (unvouchedGen++) so every clock seeded before this
//     instant discards at its next fence — conservative whether or not
//     the delete eventually applies.
func (t *twoPhaseCoordinator) maybeReapOrphan(
	ctx context.Context,
	pid string,
	cur Claim,
	rev uint64,
	live map[string]struct{},
	now time.Time,
	vouch vouchSnapshot,
) {
	// Runs under sweepMu (single-flight sweep) — the sole writer of
	// orphanAbsentSince, so no further locking is needed here except for
	// the authMu-guarded generation fence below.
	if _, ok := live[pid]; ok {
		delete(t.orphanAbsentSince, pid)

		return
	}
	// Only terminal, settled claims are reap candidates.
	if cur.State != ClaimStateStable || cur.PendingOwner != "" {
		return
	}
	clock, seen := t.orphanAbsentSince[pid]
	if !seen {
		t.orphanAbsentSince[pid] = orphanClock{since: vouch.now, gen: vouch.gen}

		return
	}
	if now.Sub(clock.since) < t.cfg.OrphanGrace {
		return
	}

	t.authMu.Lock()
	if clock.gen != t.unvouchedGen {
		delete(t.orphanAbsentSince, pid)
		t.authMu.Unlock()
		if t.cfg.Logger != nil {
			t.cfg.Logger.Debug("orphan claim reap discarded: stale generation",
				"partition_id", pid)
		}

		return
	}

	delCtx, cancel := context.WithTimeout(ctx, reapDeleteTimeout)
	delErr := t.cfg.Store.Delete(delCtx, pid, rev)
	cancel()

	switch {
	case delErr == nil:
		delete(t.orphanAbsentSince, pid)
	case errors.Is(delErr, jetstream.ErrKeyExists), errors.Is(delErr, jetstream.ErrKeyNotFound):
		// Definitive non-application: revision conflict or the key is
		// already gone. Keep the claim and the clock; a genuine orphan is
		// re-attempted next pass, a re-added partition clears via the
		// in-set branch above. No self-poison — the outcome is certain.
	default:
		// Ambiguous outcome: self-poison so no local bookkeeping can
		// extend a grace window across it.
		t.unvouchedGen++
	}
	t.authMu.Unlock()

	if delErr != nil {
		if t.cfg.Logger != nil {
			t.cfg.Logger.Debug("orphan claim reap skipped",
				"partition_id", pid, "error", delErr)
		}

		return
	}

	if t.cfg.Logger != nil {
		t.cfg.Logger.Info("reaped orphan claim",
			"partition_id", pid,
			"owner", cur.Owner,
			"absent_for", now.Sub(clock.since).String(),
		)
	}
}

// pruneOrphanCandidates drops absence-clock entries for claim keys that are
// no longer listed in the bucket (reaped by another instance, or deleted by
// other means), so the bookkeeping map cannot grow past the claim count.
// Runs under sweepMu (single-flight sweep).
func (t *twoPhaseCoordinator) pruneOrphanCandidates(listed []string) {
	if len(t.orphanAbsentSince) == 0 {
		return
	}
	listedSet := make(map[string]struct{}, len(listed))
	for _, k := range listed {
		listedSet[k] = struct{}{}
	}
	for pid := range t.orphanAbsentSince {
		if _, ok := listedSet[pid]; !ok {
			delete(t.orphanAbsentSince, pid)
		}
	}
}

// sweepReconcile computes the stable-finalizing transition the sweep should apply
// to a non-terminal claim, or nil if none is needed. It mirrors the two reconcile
// triggers documented on maybeSweepClaims:
//
//  1. a committed-but-unstabilized claim (state == commit, no PendingOwner) is
//     finalized to stable via NextStable, preserving the committed Owner — this
//     recovers a stabilizePhase missed under CAS contention, independent of TTL;
//  2. any expired non-stable claim is reset to stable with its pending owner
//     cleared (the original stale-claim safety net).
//
// Terminal states (stable, abort) and legitimately in-flight, non-expired prepares
// are left untouched.
//
// SAFETY INVARIANT: the commit->stable finalize is owner-agnostic and
// TTL-independent because the implemented lifecycle is strictly
// stable->prepare->commit->stable. A `commit` claim already has its final Owner
// baked in (NextCommit) and the prior owner was guarded out before commit
// (RemovalGuard, which treats commit and stable identically as removal-safe), so
// any coordinator may complete the bookkeeping with NextStable. This relies on
// there being NO commit->abort transition: ClaimStateAbort currently has no
// writer. If an abort-from-commit revert is introduced, revisit this — an eager
// finalize could CAS-win against the revert and lock in the new owner.
func sweepReconcile(cur *Claim, now time.Time) *Claim {
	if cur == nil || cur.State == ClaimStateStable || cur.State == ClaimStateAbort {
		return nil
	}
	if cur.State == ClaimStateCommit && cur.PendingOwner == "" {
		next := cur.NextStable(now)

		return &next
	}
	if cur.IsExpired(now) {
		next := cur.Copy()
		next.State = ClaimStateStable
		next.PendingOwner = ""
		next.LastUpdated = now.UTC()

		return &next
	}

	return nil
}
