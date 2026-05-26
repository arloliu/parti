package coordinator

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// SourceConvergenceOracle asserts that a mid-run mutation of the partition
// source (single-key edit on the parti-sim-source bucket) propagates to
// every active worker's AssignmentReport within T_converge.
//
// Phase 2 invariant INV1: after a watcher_stall outlives the watcher's
// natural rebind window, the periodic reconciler is the sole recovery path
// for converging on a fresh partition list. The oracle samples the union
// of all workers' partition sets and checks two predicates:
//
//   - "expected" partitions: every expected partition ID must appear in at
//     least one worker's assignment within the deadline.
//   - "forbidden" partitions: no removed partition ID may remain assigned
//     to any worker after the deadline.
//
// The oracle does NOT depend on a specific reassignment shape (e.g., all
// partitions reassigned to one worker). It only checks that the global
// union eventually matches the expected set.
type SourceConvergenceOracle struct {
	mu     sync.Mutex
	active []*sourceConvergenceExpectation

	observed atomic.Int64
	missing  atomic.Int64

	// spuriousSourceUnavailable counts source-unavailable degraded reports
	// that fired during an active expectation window (INV2: a clean watcher
	// stall shorter than bucketUnavailableCooldown should NOT raise the
	// source-unavailable hook).
	spuriousSourceUnavailable atomic.Int64
}

type sourceConvergenceExpectation struct {
	label            string
	expectedSet      map[int]struct{}
	forbiddenSet     map[int]struct{}
	deadline         time.Time
	completed        bool
	allowUnavailable bool
}

// NewSourceConvergenceOracle constructs an oracle ready to accept
// expectations.
func NewSourceConvergenceOracle() *SourceConvergenceOracle {
	return &SourceConvergenceOracle{}
}

// ExpectAfter registers an expectation. expectedPartitions is the post-
// mutation full partition-ID set; forbiddenPartitions are IDs that were
// removed by the mutation (and should not appear in any worker's
// assignment after the deadline). within is the convergence budget.
//
// allowSourceUnavailable controls whether source-unavailable degraded
// reports during the window count as spurious (INV2). Composed scenarios
// that DO expect source-unavailable (e.g. bucket_delete of the source
// bucket) pass true.
func (o *SourceConvergenceOracle) ExpectAfter(label string, expected, forbidden []int, within time.Duration, allowSourceUnavailable bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	exp := &sourceConvergenceExpectation{
		label:            label,
		expectedSet:      idSet(expected),
		forbiddenSet:     idSet(forbidden),
		deadline:         time.Now().Add(within),
		allowUnavailable: allowSourceUnavailable,
	}
	o.active = append(o.active, exp)
	log.Printf("[SourceConvergenceOracle] EXPECT label=%s expected=%d forbidden=%d deadline=%v",
		label, len(exp.expectedSet), len(exp.forbiddenSet), exp.deadline.Format(time.RFC3339))
}

// CheckSnapshot is called periodically (e.g. from coordinator.processAssignments)
// with the current global union of worker partitions. An expectation is
// satisfied as soon as one snapshot matches expected ⊆ union and
// forbidden ∩ union = ∅.
func (o *SourceConvergenceOracle) CheckSnapshot(union map[int]struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, exp := range o.active {
		if exp.completed {
			continue
		}
		if time.Now().After(exp.deadline) {
			continue
		}
		if matchesExpectation(union, exp) {
			exp.completed = true
			o.observed.Add(1)
			log.Printf("[SourceConvergenceOracle] OBSERVED label=%s expected=%d", exp.label, len(exp.expectedSet))
		}
	}
}

// ObserveSourceUnavailable is called when a worker emits a degraded report
// whose reason carries the source-unavailable family ("source-unavailable:").
// During an active expectation that disallows it, increments the spurious
// counter. Called from the coordinator's degraded-report drain loop.
func (o *SourceConvergenceOracle) ObserveSourceUnavailable(workerID, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	for _, exp := range o.active {
		if now.After(exp.deadline) {
			continue
		}
		if exp.allowUnavailable {
			continue
		}
		log.Printf("[SourceConvergenceOracle] SPURIOUS_SOURCE_UNAVAILABLE label=%s worker=%s reason=%s", exp.label, workerID, reason)
		o.spuriousSourceUnavailable.Add(1)

		return
	}
}

// Sweep is called periodically; expectations whose deadline has elapsed
// without completing are recorded as missing.
func (o *SourceConvergenceOracle) Sweep(now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	kept := o.active[:0]
	for _, exp := range o.active {
		if now.Before(exp.deadline) {
			kept = append(kept, exp)
			continue
		}
		if !exp.completed {
			log.Printf("[SourceConvergenceOracle] MISSING label=%s expected=%d forbidden=%d deadline=%v",
				exp.label, len(exp.expectedSet), len(exp.forbiddenSet), exp.deadline.Format(time.RFC3339))
			o.missing.Add(1)
		}
	}
	o.active = kept
}

// Run starts a background sweep goroutine.
func (o *SourceConvergenceOracle) Run(ctx interface{ Done() <-chan struct{} }, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				o.Sweep(time.Now())
				return
			case t := <-ticker.C:
				o.Sweep(t)
			}
		}
	}()
}

// Observed returns the count of expectations that converged within deadline.
func (o *SourceConvergenceOracle) Observed() int64 { return o.observed.Load() }

// Missing returns the count of expectations whose deadlines elapsed without
// convergence.
func (o *SourceConvergenceOracle) Missing() int64 { return o.missing.Load() }

// SpuriousSourceUnavailable returns the count of source-unavailable degraded
// reports that fired during an active expectation that disallowed them
// (INV2 violation count).
func (o *SourceConvergenceOracle) SpuriousSourceUnavailable() int64 {
	return o.spuriousSourceUnavailable.Load()
}

func idSet(ids []int) map[int]struct{} {
	m := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}

func matchesExpectation(union map[int]struct{}, exp *sourceConvergenceExpectation) bool {
	for id := range exp.expectedSet {
		if _, ok := union[id]; !ok {
			return false
		}
	}
	for id := range exp.forbiddenSet {
		if _, ok := union[id]; ok {
			return false
		}
	}

	return true
}
