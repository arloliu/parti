package manager_test

import (
	"context"
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

// TestNP4_SyncStartFailure_NoSelfHeal_StableIDFault is the NP-4 contract lock
// for a SYNCHRONOUS-PHASE Start() failure.
//
// It reuses the connected-but-KV-unavailable fault seam from
// manager_kv_read_unavailable_test.go (kuFaultController / kuFaultJetStream /
// kuFaultKeyValue), but inverts the timing: the fault is ARMED BEFORE Start
// and is scoped to the StableID bucket ONLY. With OperationTimeout shrunk so
// the faulted claim returns context.DeadlineExceeded fast, the failure lands
// at claimWorkerID — the EARLIEST synchronous op in Start, before any
// background goroutine (heartbeat renew, election renew, watchers, recovery
// monitors) is spawned.
//
// The asserted invariant, all hinging on real state (not a timeout):
//
//  1. Start returns an error pinned to the synchronous claim op
//     ("failed to claim worker ID").
//  2. The fault genuinely fired (fc.injected > 0) — non-vacuity.
//  3. The deferred auto-Stop ran: the manager is left in StateShutdown.
//  4. NO self-heal: clearing the fault and waiting past a connection-monitor
//     tick leaves the SAME instance in StateShutdown — a synchronous-phase
//     failure spawns no recovery goroutine, so nothing watches for the fault
//     to clear.
//  5. Must-Start-fresh: a second Start on the same instance returns
//     ErrAlreadyStarted (m.ctx is non-nil), it cannot be restarted.
func TestNP4_SyncStartFailure_NoSelfHeal_StableIDFault(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	realJS, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	// Shrink the per-op timeout so the faulted claim Create/Get returns
	// context.DeadlineExceeded quickly. The fault returns the error
	// synchronously, but this also bounds any real op the claim path makes.
	cfg.OperationTimeout = 500 * time.Millisecond

	// Fault ONLY the StableID bucket, armed BEFORE Start. Bucket
	// ensure/reconcile use Status() (NOT faulted by the seam), so
	// ensureStableIDKV succeeds and the fault first bites at claimWorkerID's
	// kv.Create — guaranteeing zero background goroutines have started when
	// Start returns its error.
	fc := &kuFaultController{}
	faultJS := &kuFaultJetStream{
		JetStream: realJS,
		fc:        fc,
		buckets: map[string]struct{}{
			cfg.KVBuckets.StableIDBucket: {},
		},
	}

	src := source.NewStatic(testutil.CreateTestPartitions(4))

	mgr, err := parti.NewManager(&cfg, faultJS, src, strategy.NewConsistentHash())
	require.NoError(t, err)
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = mgr.Stop(stopCtx)
	})

	// Arm the StableID-bucket fault BEFORE Start (inversion of the sibling
	// test, which arms only after the manager reaches Stable).
	fc.arm()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err = mgr.Start(ctx)

	// (1) Start fails, pinned to the synchronous claim op.
	require.Error(t, err, "Start must fail when the StableID bucket is faulted before claim")
	require.ErrorContains(t, err, "failed to claim worker ID",
		"the failure must be the synchronous claimWorkerID op, not a later phase")

	// (2) Non-vacuity: the fault actually injected (the claim genuinely hit
	// the faulted StableID bucket; Start did not fail for some other reason).
	require.Positive(t, fc.injected.Load(),
		"the StableID-bucket fault must have actually injected during claim")

	// (3) The deferred auto-Stop ran: the manager is in StateShutdown
	// immediately after the failed Start.
	require.Equal(t, types.StateShutdown, mgr.State(),
		"a failed synchronous Start must auto-Stop the manager to StateShutdown")

	// (4) No self-heal. Clear the fault and wait well past one connection-
	// monitor tick. A synchronous-phase failure spawns NO recovery goroutine
	// (the failure preceded every background-goroutine launch), so nothing
	// can observe the fault clearing and resurrect the manager. The instance
	// MUST remain terminal.
	fc.armed.Store(false)
	require.Never(t, func() bool {
		return mgr.State() != types.StateShutdown
	}, 3*time.Second, 200*time.Millisecond,
		"a synchronous-phase Start failure must NOT self-heal after the fault clears")
	require.Equal(t, types.StateShutdown, mgr.State(),
		"the same instance must stay terminal (StateShutdown) — no recovery goroutine was ever spawned")

	// (5) Must-Start-fresh: a second Start on the same instance is rejected.
	// prepareStart sees m.ctx != nil and returns ErrAlreadyStarted; the
	// instance cannot be restarted.
	err2 := mgr.Start(ctx)
	require.ErrorIs(t, err2, types.ErrAlreadyStarted,
		"a stopped/failed instance must not be restartable; Start must return ErrAlreadyStarted")
}
