package handoff

import (
	"context"
	"errors"
	"fmt"
	rand "math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/v2/types"
	"golang.org/x/sync/errgroup"
)

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

	// orphanAbsentSince records, per claim key, when the sweep first observed
	// the partition absent from a vouched LivePartitions set. Entries clear
	// when the partition reappears, when the claim is reaped, when the claim
	// key stops being listed, or wholesale on an unvouched pass. Guarded by
	// sweepMu (single-flight sweep = single writer). In-memory only: a
	// restart or leadership change restarts the grace clock, which is the
	// conservative direction.
	orphanAbsentSince map[string]time.Time
}

// Start launches a background goroutine that sweeps stale claims at SweepInterval.
//
// This ensures stale/expired claims are cleaned up even in idle systems where
// Apply is rarely called. The goroutine exits when ctx is cancelled.
//
// Start is idempotent: calling it more than once has no effect.
func (t *twoPhaseCoordinator) Start(ctx context.Context) {
	if t.cfg.SweepInterval <= 0 {
		return
	}
	if !t.started.CompareAndSwap(false, true) {
		return // already started
	}
	go func() {
		ticker := time.NewTicker(t.cfg.SweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.maybeSweepClaims(ctx)
			}
		}
	}()
}

// Apply performs the multi-phase handoff application for the worker's new assignment.
// Steps: prepare -> (optional delay) -> apply consumer -> commit -> (optional delay) -> stabilize.
func (t *twoPhaseCoordinator) Apply(ctx context.Context, workerID string, previous, next types.Assignment) error {
	// Opportunistic sweep of expired/non-stable claims; skip metrics as requested.
	// Runs before any early returns to maximize cleanup.
	t.maybeSweepClaims(ctx)

	inst := newInstrumenter(t.cfg.Metrics)

	// Phase: prepare - write/update claims for partitions being added.
	if err := inst.phase("prepare", func() error { return t.preparePhase(ctx, workerID, previous, next) }); err != nil {
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

		// 3. Attempt CAS
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
func (t *twoPhaseCoordinator) maybeSweepClaims(ctx context.Context) {
	if t.cfg.Store == nil {
		return
	}
	if !t.sweepMu.TryLock() {
		return // another sweep is mid-body; this opportunistic pass skips
	}
	defer t.sweepMu.Unlock()

	now := t.cfg.Now()
	if t.cfg.SweepInterval > 0 && !t.lastSweep.IsZero() && now.Sub(t.lastSweep) < t.cfg.SweepInterval {
		return
	}
	t.lastSweep = now

	keys, err := t.cfg.Store.ListKeys(ctx)
	if err != nil || len(keys) == 0 {
		return
	}
	if t.cfg.Metrics != nil {
		t.cfg.Metrics.SetClaimStoreSize(len(keys))
	}

	// Resolve the live partition set once per pass for orphan reaping.
	// liveOK=false (supplier not vouching: not the leader, source down)
	// skips all reap decisions for the pass AND resets every absence clock:
	// time spent unvouched is time this worker could not verify continuous
	// absence, so it must not count toward OrphanGrace — otherwise a clock
	// started before a long follower stint would reap instantly on the
	// first vouched pass after it.
	var live map[string]struct{}
	liveOK := false
	if t.cfg.OrphanGrace > 0 && t.cfg.LivePartitions != nil {
		live, liveOK = t.cfg.LivePartitions(ctx)
		if !liveOK {
			clear(t.orphanAbsentSince)
		}
	}

	for _, pid := range keys {
		cur, rev, err := t.cfg.Store.Get(ctx, pid)
		if err != nil || rev == 0 {
			continue
		}
		// Cheap pre-filter: skip a CAS write attempt when this claim needs no
		// reconcile (the common case for already-stable claims).
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
			continue
		}

		if liveOK {
			t.maybeReapOrphan(ctx, pid, cur, rev, live, now)
		}
	}

	if liveOK {
		t.pruneOrphanCandidates(keys)
	}
}

// maybeReapOrphan applies the orphan-reap decision for one claim against a
// vouched live partition set:
//
//   - in the set → clear any absence clock, keep;
//   - not stable, or a pending owner recorded → keep (in-flight handoffs are
//     the existing sweep arms' job, never the reaper's);
//   - first vouched absence → start the clock, keep;
//   - absent for >= OrphanGrace → compare-and-delete at the revision this
//     pass read. A lost CAS means the claim transitioned concurrently (e.g.
//     the partition was re-added and prepared) — the reaper yields and the
//     next pass re-evaluates from a fresh read.
func (t *twoPhaseCoordinator) maybeReapOrphan(
	ctx context.Context,
	pid string,
	cur Claim,
	rev uint64,
	live map[string]struct{},
	now time.Time,
) {
	// Runs under sweepMu (single-flight sweep) — the sole writer of
	// orphanAbsentSince, so no further locking is needed here.
	if _, ok := live[pid]; ok {
		delete(t.orphanAbsentSince, pid)

		return
	}
	// Only terminal, settled claims are reap candidates.
	if cur.State != ClaimStateStable || cur.PendingOwner != "" {
		return
	}
	since, seen := t.orphanAbsentSince[pid]
	if !seen {
		t.orphanAbsentSince[pid] = now

		return
	}
	if now.Sub(since) < t.cfg.OrphanGrace {
		return
	}

	if err := t.cfg.Store.Delete(ctx, pid, rev); err != nil {
		// Revision conflict or transient KV failure: keep the claim and the
		// clock; a genuine orphan is re-attempted next pass, a re-added
		// partition clears via the in-set branch above.
		if t.cfg.Logger != nil {
			t.cfg.Logger.Debug("orphan claim reap skipped",
				"partition_id", pid, "error", err)
		}

		return
	}
	delete(t.orphanAbsentSince, pid)
	if t.cfg.Logger != nil {
		t.cfg.Logger.Info("reaped orphan claim",
			"partition_id", pid,
			"owner", cur.Owner,
			"absent_for", now.Sub(since).String(),
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
