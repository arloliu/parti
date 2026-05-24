package manager_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestStartupAsync_CalculatorStateNotClobbered exercises the race window
// where the runner is mid-apply while the calculator transitions away
// from WaitingAssignment.
//
// Scope of this test: liveness + smoke coverage only. The integration
// layer cannot deterministically distinguish a broken CAS clobber from
// normal calculator-driven state oscillation via OnStateChanged alone
// (both produce overlapping transition sequences; the transition table
// at manager_state.go:165-167 permits Stable ↔ Scaling/Rebalancing/
// Emergency as valid steady-state transitions). The precise CAS-guard
// regression pin lives in the three unit tests in
// manager_startup_async_cas_test.go
// (TestCasToStableFromWaitingAssignment_*), which exercise the helper
// directly.
//
// What this test contributes: 3-worker join under live KV traffic
// converges to a healthy steady state; OnStateChanged is correctly
// wired; partition coverage is complete; and existing workers do not
// see a spurious full revoke caused by a joining worker.
func TestStartupAsync_CalculatorStateNotClobbered(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	partitions := testutil.CreateTestPartitions(6)
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Per-worker state-history recorder. Hook fires on every transition.
	// The recorder is used as a liveness/smoke signal — the assertion below
	// only verifies that OnStateChanged fired at all (non-zero hook
	// delivery). Deterministically distinguishing a broken CAS clobber
	// from normal calculator-driven oscillation requires a production
	// test hook; see CHANGELOG follow-ups.
	type transition struct {
		from, to types.State
	}
	var (
		mu       sync.Mutex
		recorded = map[int][]transition{} // worker index → transitions
	)
	makeHooks := func(idx int) *parti.Hooks {
		return &parti.Hooks{
			OnStateChanged: func(_ context.Context, from, to parti.State) error {
				mu.Lock()
				recorded[idx] = append(recorded[idx], transition{from, to})
				mu.Unlock()
				return nil
			},
		}
	}

	// Sampler: poll each worker's partition count every 50ms during the
	// join sequence so we can assert no existing worker ever dropped to
	// zero partitions during the join (a spurious-revoke regression
	// caused by the new worker's startup misinterpreting the assignment).
	type sample struct {
		idx   int
		count int
	}
	var (
		samplesMu sync.Mutex
		samples   = map[int][]int{}
	)
	samplerCtx, stopSampler := context.WithCancel(ctx)
	defer stopSampler()

	// Three workers in sequence so each addition forces a calculator-
	// state projection (Scaling) on existing leader and on the joiners.
	workers := make([]*parti.Manager, 0, 3)
	startSampler := func(idx int, mgr *parti.Manager) {
		go func() {
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-samplerCtx.Done():
					return
				case <-ticker.C:
					count := len(mgr.CurrentAssignment().Partitions)
					samplesMu.Lock()
					samples[idx] = append(samples[idx], count)
					samplesMu.Unlock()
					_ = sample{idx: idx, count: count} // keep struct used
				}
			}
		}()
	}

	for i := 0; i < 3; i++ {
		mgr, err := parti.NewManager(&cfg, js, src, assignStrat, parti.WithHooks(makeHooks(i)))
		require.NoError(t, err)
		require.NoError(t, mgr.Start(ctx))
		workers = append(workers, mgr)
		startSampler(i, mgr)
		// Stagger so each worker joins while the cluster is still
		// settling — maximises the window where the runner is mid-
		// apply while the calculator projects an active state.
		time.Sleep(200 * time.Millisecond)
	}
	t.Cleanup(func() {
		for _, mgr := range workers {
			_ = mgr.Stop(context.Background())
		}
	})

	// Wait for convergence: all workers reach StateStable.
	for i, mgr := range workers {
		require.NoErrorf(t, <-mgr.WaitState(types.StateStable, 15*time.Second),
			"worker %d did not reach StateStable", i)
	}

	// Stop the sampler now that convergence is reached.
	stopSampler()

	// Smoke: OnStateChanged actually fires under 3-worker join.
	mu.Lock()
	transitionCount := 0
	for _, trs := range recorded {
		transitionCount += len(trs)
	}
	mu.Unlock()
	require.Greater(t, transitionCount, 0,
		"OnStateChanged should have fired during 3-worker join — if 0, the hook is not wired correctly")

	// Liveness sanity check: the union of all partitions across workers
	// must equal the source set.
	seen := make(map[string]struct{}, len(partitions))
	for _, mgr := range workers {
		for _, p := range mgr.CurrentAssignment().Partitions {
			seen[p.ID()] = struct{}{}
		}
	}
	require.Len(t, seen, len(partitions))

	// Existing-cluster-not-disrupted: for each worker that ever held a
	// partition during the join window, assert it never dropped to zero.
	// A drop to zero would indicate a spurious "lose everything then
	// re-acquire" oscillation triggered by a joiner — the kind of
	// regression the runner refactor might introduce if monitor startup
	// is mishandled.
	samplesMu.Lock()
	defer samplesMu.Unlock()
	for idx, series := range samples {
		evenHeld := false
		minNonZero := -1
		for _, c := range series {
			if c > 0 {
				evenHeld = true
				if minNonZero < 0 || c < minNonZero {
					minNonZero = c
				}
			}
		}
		if !evenHeld {
			// This worker never held a partition during sampling — fine
			// (e.g., 1-partition cluster with multiple workers, or
			// sampler missed the held window).
			continue
		}
		// Find the first zero after the first non-zero — that would
		// be a spurious revoke.
		seenNonZero := false
		for _, c := range series {
			if c > 0 {
				seenNonZero = true
				continue
			}
			if seenNonZero && c == 0 {
				t.Errorf("worker %d dropped to zero partitions after holding non-zero — possible spurious-revoke regression. samples=%v", idx, series)
				break
			}
		}
	}
}
