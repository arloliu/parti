package handoff

import (
	"context"
	"errors"
	"fmt"
	rand "math/rand/v2"
	"sync"
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
	cfg       Config
	sweepMu   sync.Mutex
	lastSweep time.Time
}

// Start launches a background goroutine that sweeps stale claims at SweepInterval.
//
// This ensures stale/expired claims are cleaned up even in idle systems where
// Apply is rarely called. The goroutine exits when ctx is cancelled.
func (t *twoPhaseCoordinator) Start(ctx context.Context) {
	if t.cfg.SweepInterval <= 0 {
		return
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
	g.SetLimit(20) // Limit concurrent KV operations

	for _, p := range toPrepare {
		g.Go(func() error {
			// Create or transition claim for new partition
			pid := p.ID()
			return t.updateClaim(gCtx, pid, func(cur *Claim) (*Claim, error) {
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

				return nil, nil
			})
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
	g.SetLimit(20)

	for _, p := range next.Partitions {
		g.Go(func() error {
			pid := p.ID()
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

// maybeSweepClaims opportunistically resets expired non-stable claims back to stable.
// Runs at most once per configured sweep interval. If SweepInterval <= 0, runs every time.
func (t *twoPhaseCoordinator) maybeSweepClaims(ctx context.Context) {
	if t.cfg.Store == nil {
		return
	}
	now := t.cfg.Now()

	// Check sweep interval with lock
	t.sweepMu.Lock()
	if t.cfg.SweepInterval > 0 && !t.lastSweep.IsZero() && now.Sub(t.lastSweep) < t.cfg.SweepInterval {
		t.sweepMu.Unlock()
		return
	}
	// Update last sweep time and release lock immediately to avoid blocking Apply
	t.lastSweep = now
	t.sweepMu.Unlock()

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
		if err := t.updateClaim(ctx, pid, func(cur *Claim) (*Claim, error) {
			if cur == nil {
				//nolint:nilnil // nil, nil indicates no update needed
				return nil, nil
			}
			if !cur.IsExpired(now) {
				//nolint:nilnil // nil, nil indicates no update needed
				return nil, nil
			}
			if cur.State == ClaimStateStable {
				//nolint:nilnil // nil, nil indicates no update needed
				return nil, nil
			}
			next := cur.Copy()
			next.State = ClaimStateStable
			next.PendingOwner = ""
			next.LastUpdated = now.UTC()

			return &next, nil
		}); err == nil {
			if t.cfg.Metrics != nil {
				t.cfg.Metrics.IncClaimStoreStale()
			}
		}
	}
}
