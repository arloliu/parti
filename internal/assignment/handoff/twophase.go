package handoff

import (
	"context"
	"errors"
	"fmt"
	rand "math/rand/v2"
	"time"

	"github.com/arloliu/parti/types"
	"golang.org/x/sync/errgroup"
)

// twoPhaseCoordinator implements a prepare/commit protocol using KV-backed claims.
// It supports optional observability delays (for tests/demos) and opportunistic
// sweeping of expired non-stable claims.
//
// All latency / retries are internal; Apply should return quickly.
type twoPhaseCoordinator struct {
	cfg       Config
	lastSweep time.Time
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

// retryCAS performs bounded retries with backoff and jitter for CAS operations.
func (t *twoPhaseCoordinator) retryCAS(ctx context.Context, op func() error) error {
	var err error
	for attempt := 0; attempt <= t.cfg.MaxRetries; attempt++ {
		err = op()
		if err == nil {
			return nil
		}
		if attempt == t.cfg.MaxRetries {
			break
		}

		// Policy: allow one immediate retry without delay, then backoff with jitter from the second failure onward.
		if attempt == 0 {
			// immediate retry
			continue
		}

		// compute backoff with jitter; use (attempt-1) to keep schedule roughly equivalent to previous behavior
		effAttempt := attempt - 1
		backoff := min(t.cfg.BaseBackoff<<effAttempt, t.cfg.MaxBackoff)
		d := backoff
		if t.cfg.Jitter > 0 {
			// random factor in [1-jitter, 1+jitter] using math/rand/v2
			f := rand.Float64() //nolint:gosec // Jitter does not require cryptographic randomness
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

	return err
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
	g.SetLimit(20) // Limit concurrent KV operations

	for _, p := range toPrepare {
		g.Go(func() error {
			// Create or transition claim for new partition
			pid := p.ID()
			cur, rev, err := t.cfg.Store.Get(gCtx, pid)
			if err != nil {
				return fmt.Errorf("claim get: %w", err)
			}
			if rev == 0 { // create initial claim
				init := NewInitialClaim(pid, workerID, now, t.cfg.TTL)
				if err := t.retryCAS(gCtx, func() error { _, e := t.cfg.Store.PutIfEpoch(gCtx, pid, 0, init); return e }); err != nil {
					if t.cfg.Metrics != nil {
						t.cfg.Metrics.IncCASConflicts()
					}
					return fmt.Errorf("claim create: %w", err)
				}

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

				return nil
			}
			// Existing claim; if stable and owned by someone else, enter prepare
			if cur.Owner != workerID {
				prepared := cur.NextPrepare(workerID, now)
				if err := t.retryCAS(gCtx, func() error { _, e := t.cfg.Store.PutIfEpoch(gCtx, pid, cur.Epoch, prepared); return e }); err != nil {
					if t.cfg.Metrics != nil {
						t.cfg.Metrics.IncCASConflicts()
					}
					return fmt.Errorf("claim prepare: %w", err)
				}

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
	g.SetLimit(20)

	for _, p := range next.Partitions {
		g.Go(func() error {
			pid := p.ID()
			cur, rev, err := t.cfg.Store.Get(gCtx, pid)
			if err != nil {
				return fmt.Errorf("claim get (commit): %w", err)
			}
			if rev == 0 {
				return nil
			}
			if cur.Owner == workerID && cur.State == ClaimStateStable {
				return nil // already finalized
			}

			// If we're pending owner or current owner differs, move to commit.
			if cur.PendingOwner == workerID || cur.Owner != workerID {
				committed := cur
				if cur.State != ClaimStateCommit {
					committed = cur.NextCommit(now)
				}
				if err := t.retryCAS(gCtx, func() error { _, e := t.cfg.Store.PutIfEpoch(gCtx, pid, cur.Epoch, committed); return e }); err != nil {
					if t.cfg.Metrics != nil {
						t.cfg.Metrics.IncCASConflicts()
					}
					return fmt.Errorf("claim commit: %w", err)
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
			}

			return nil
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
	g.SetLimit(20)

	for _, p := range next.Partitions {
		g.Go(func() error {
			pid := p.ID()
			cur, rev, err := t.cfg.Store.Get(gCtx, pid)
			if err != nil {
				return fmt.Errorf("claim get (stabilize): %w", err)
			}
			if rev == 0 {
				return nil
			}
			if cur.Owner != workerID {
				return nil // not ours yet
			}
			if cur.State != ClaimStateCommit {
				return nil // only finalize from commit
			}

			stabilized := cur.NextStable(now)
			if err := t.retryCAS(gCtx, func() error { _, e := t.cfg.Store.PutIfEpoch(gCtx, pid, cur.Epoch, stabilized); return e }); err != nil {
				if t.cfg.Metrics != nil {
					t.cfg.Metrics.IncCASConflicts()
				}
				if t.cfg.Logger != nil {
					t.cfg.Logger.Warn("handoff_stable_conflict",
						"partition_id", pid,
						"worker_id", workerID,
						"owner", stabilized.Owner,
						"epoch", stabilized.Epoch,
					)
				}

				return fmt.Errorf("claim stabilize: %w", err)
			}

			if t.cfg.Logger != nil {
				t.cfg.Logger.Info("handoff_stable",
					"partition_id", pid,
					"worker_id", workerID,
					"owner", stabilized.Owner,
					"epoch", stabilized.Epoch,
				)
			}

			return nil
		})
	}

	return g.Wait()
}

// maybeSweepClaims opportunistically resets expired non-stable claims back to stable.
// Runs at most once per configured sweep interval. If SweepInterval <= 0, runs every time.
func (t *twoPhaseCoordinator) maybeSweepClaims(ctx context.Context) {
	if t.cfg.Store == nil {
		return
	}
	now := t.cfg.Now()
	if t.cfg.SweepInterval > 0 && !t.lastSweep.IsZero() && now.Sub(t.lastSweep) < t.cfg.SweepInterval {
		return
	}

	// Guard the sweep execution window and proceed best-effort.
	t.lastSweep = now

	keys, err := t.cfg.Store.ListKeys(ctx)
	if err != nil || len(keys) == 0 {
		return
	}
	if t.cfg.Metrics != nil {
		t.cfg.Metrics.SetClaimStoreSize(len(keys))
	}
	for _, pid := range keys {
		cur, rev, err := t.cfg.Store.Get(ctx, pid)
		if err != nil || rev == 0 {
			continue
		}
		if !cur.IsExpired(now) {
			continue
		}
		if cur.State == ClaimStateStable {
			// Leave stable claims as-is; no deletion API available.
			continue
		}
		// Reset to stable and clear any pending owner.
		next := cur.Copy()
		next.State = ClaimStateStable
		next.PendingOwner = ""
		next.LastUpdated = now.UTC()
		if err := t.retryCAS(ctx, func() error {
			_, e := t.cfg.Store.PutIfEpoch(ctx, pid, cur.Epoch, next)
			return e
		}); err == nil {
			if t.cfg.Metrics != nil {
				t.cfg.Metrics.IncClaimStoreStale()
			}
		}
	}
}
