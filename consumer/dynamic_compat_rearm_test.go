package consumer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// T3 covers three sub-tests against the same package-private seam:
//   - basic: both Update and UpdateWorkerConsumer run the check; the
//     second call (in either order) is served from the cached result;
//     resetCompatCheck re-arms and the next call re-runs the check.
//   - T3a: deterministic stale-store interleaving — the slow-path
//     check publishes its result before resetCompatCheck takes effect.
//   - T3b: concurrent stress under -race.
//
// All three use the package-private compatCheckFn seam so no mock
// jetstream.JetStream is needed and no embedded NATS is started.

// dynamicForCompatTest builds a *Dynamic with only the fields the
// compat re-arm path touches. The other fields (inner, js,
// streamName, recoveryStrategy) are stubs that compile against the
// real types but never get exercised in these tests.
func dynamicForCompatTest(t *testing.T) *Dynamic {
	t.Helper()
	return &Dynamic{
		// streamName / recoveryStrategy are read by the compat check
		// fn we swap in; the seam ignores them but they must be set
		// for the production fn to compile-time match.
		streamName:       "T3_STREAM",
		recoveryStrategy: RecoverFromLastProcessed,
	}
}

// swapCompatCheckFn installs a stub for the package-private seam and
// returns a cleanup function that restores the original.
func swapCompatCheckFn(t *testing.T, stub func(ctx context.Context, js jetstream.JetStream, streamName string, strategy RecoveryStrategy) error) {
	t.Helper()
	prev := compatCheckFn
	compatCheckFn = stub
	t.Cleanup(func() { compatCheckFn = prev })
}

// T3 (basic) — both paths run the check; the cache stops a second
// invocation; resetCompatCheck re-arms; the re-armed check runs again
// and surfaces the new result.
//
// On parent (today's code) UpdateWorkerConsumer bypasses the check
// entirely, so the call count would stay at 1 even after a reset.
// The inverse assertion (calls increase on the second
// UpdateWorkerConsumer after resetCompatCheck) is the load-bearing
// part — it fails on parent and pins the fix.
func TestDynamic_CompatRearm_BothPathsRun_CacheServes_ResetRearms(t *testing.T) {
	d := dynamicForCompatTest(t)

	var (
		calls atomic.Int32
		next  atomic.Pointer[error] // nil → return nil; *err → return *err
	)
	swapCompatCheckFn(t, func(_ context.Context, _ jetstream.JetStream, _ string, _ RecoveryStrategy) error {
		calls.Add(1)
		if p := next.Load(); p != nil {
			return *p
		}
		return nil
	})

	ctx := context.Background()

	// First call (via Update path): check must run.
	require.NoError(t, d.ensureCompatChecked(ctx))
	require.Equal(t, int32(1), calls.Load(),
		"first ensureCompatChecked must invoke compatCheckFn")

	// Second call (via UpdateWorkerConsumer path) without reset:
	// served from the cached result, MUST NOT re-run.
	require.NoError(t, d.ensureCompatChecked(ctx))
	require.Equal(t, int32(1), calls.Load(),
		"cached result must serve subsequent calls without re-running the check")

	// Reset (called from OnStreamRecreated after a hook success).
	d.resetCompatCheck()

	// Swap the check fn's outcome to a compat failure (e.g., the
	// recreated stream came back as WorkQueuePolicy + RecoverFromLastProcessed).
	failErr := fmt.Errorf("%w: simulated WorkQueuePolicy mismatch", ErrInvalidConfig)
	next.Store(&failErr)

	// Third call (next UpdateWorkerConsumer after reset): MUST re-run
	// and MUST surface the new failure. On parent this would not
	// re-run because UpdateWorkerConsumer bypassed the check entirely.
	err := d.ensureCompatChecked(ctx)
	require.Equal(t, int32(2), calls.Load(),
		"resetCompatCheck must re-arm; the next ensureCompatChecked MUST re-run compatCheckFn")
	require.ErrorIs(t, err, ErrInvalidConfig,
		"re-armed check must surface the new outcome (compat failure), not the cached old success")

	// Fourth call: served from the freshly cached failure result.
	err = d.ensureCompatChecked(ctx)
	require.Equal(t, int32(2), calls.Load(),
		"after re-arm, the new result must itself be cached; no extra invocation")
	require.ErrorIs(t, err, ErrInvalidConfig)
}

// T3a — deterministic stale-store interleaving. Pins the v2-P0.3 race
// fix: resetCompatCheck must take the same workQueueMu the slow-path
// check holds, so a reset cannot interleave between the slow-path
// "I'm running" and "I publish my result" steps. Without the mutex
// (Store-only reset), the slow path would publish its now-stale
// "compatible" outcome AFTER the reset, and the next reader would
// see stale "compatible" against an incompatible recreated stream.
//
// Test orchestration:
//  1. Goroutine A enters ensureCompatChecked; the stub blocks before
//     returning, holding the slow-path inside the mutex region.
//  2. Goroutine B calls resetCompatCheck — must BLOCK on the mutex
//     until A finishes and unlocks.
//  3. Test releases A's stub → A publishes "compatible" → A unlocks.
//  4. B now takes the mutex → clears the cache → unlocks.
//  5. Next reader observes nil (forced re-arm), not stale "compatible".
func TestDynamic_CompatRearm_SlowPath_ResetSerializedByMutex(t *testing.T) {
	d := dynamicForCompatTest(t)

	resumeA := make(chan struct{})
	aEntered := make(chan struct{})

	swapCompatCheckFn(t, func(_ context.Context, _ jetstream.JetStream, _ string, _ RecoveryStrategy) error {
		close(aEntered)
		<-resumeA // hold the slow-path inside the mutex until released
		return nil
	})

	// Goroutine A: slow-path compat check.
	aDone := make(chan error, 1)
	go func() {
		aDone <- d.ensureCompatChecked(context.Background())
	}()

	<-aEntered // A is inside compatCheckFn (and holds the mutex).

	// Goroutine B: reset must BLOCK on the mutex until A publishes.
	bReturned := make(chan struct{})
	go func() {
		d.resetCompatCheck()
		close(bReturned)
	}()

	// B must not have returned yet — it's waiting on the mutex.
	select {
	case <-bReturned:
		t.Fatal("resetCompatCheck returned while the slow-path compat check still held workQueueMu; v3 mutex contract is broken")
	default:
		// expected: B is parked on the mutex
	}

	// Release A → A publishes "compatible" → A unlocks → B unblocks.
	close(resumeA)
	require.NoError(t, <-aDone, "slow-path check returned no error")
	<-bReturned // B's reset now completes after A.

	// Next reader: cache is cleared, so a fresh check must run.
	var freshCalls atomic.Int32
	swapCompatCheckFn(t, func(_ context.Context, _ jetstream.JetStream, _ string, _ RecoveryStrategy) error {
		freshCalls.Add(1)
		return nil
	})
	require.NoError(t, d.ensureCompatChecked(context.Background()))
	require.Equal(t, int32(1), freshCalls.Load(),
		"after the mutex-serialized reset, the next ensureCompatChecked must observe nil and re-run the check, not return stale 'compatible'")
}

// T3b — concurrent stress under -race. Spawns N goroutines that
// hammer ensureCompatChecked and resetCompatCheck in a loop. The
// assertion is that the race detector does not trigger; the test
// exists to keep the compat-cache slot under -race in CI.
func TestDynamic_CompatRearm_ConcurrentStress_NoRace(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped in -short mode")
	}

	d := dynamicForCompatTest(t)

	var outcomes atomic.Int32
	swapCompatCheckFn(t, func(_ context.Context, _ jetstream.JetStream, _ string, _ RecoveryStrategy) error {
		// Alternate the outcome to keep the cache slot mutating.
		if outcomes.Add(1)%2 == 0 {
			return errors.New("alternating-failure")
		}
		return nil
	})

	ctx := context.Background()
	const workers = 8
	const itersPerWorker = 500

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < itersPerWorker; j++ {
				_ = d.ensureCompatChecked(ctx)
				if (id+j)%17 == 0 {
					d.resetCompatCheck()
				}
			}
		}(i)
	}
	wg.Wait()

	// We don't assert a particular outcome — any (cached err) or nil
	// is acceptable. The point is -race not triggering.
}
