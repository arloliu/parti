package manager_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- NP-3: connected-but-KV-unavailable false return-to-Stable (Finding A) ---
//
// Background: the kv-unavailable fault (see manager_kv_read_unavailable_test.go)
// times out the election / heartbeat / stableid buckets while the NATS
// connection stays CONNECTED. The assignment bucket is intentionally EXCLUDED
// from the fault set. attemptRecoveryFromDegraded fires on connection-uptime
// every ~1s and reads the UNFAULTED assignment bucket; that read succeeds, so
// the manager exits Degraded -> Stable even though heartbeat / election /
// stableid are STILL faulting. That false return-to-Stable then re-degrades =
// a FLAP. This is the explicitly-DEFERRED "Finding A / recover-on-wrong-signal".
//
// Fork B (TestNP3_KVUnavailable_HeldArmed_DoesNotFalselyExitToStable) proves the
// gap: with the fault held armed, the manager must NOT exit Degraded -> Stable.
// On current main it does (then re-degrades), so the hard "zero exits" gate
// FAILS — that failure is the proof.
//
// Fork A (TestNP3_KVUnavailable_Disarm_ReturnsAndHoldsStable) is the positive
// control: once the fault genuinely clears (disarm), recovery returns to Stable
// and HOLDS it. That demonstrates Fork B's exit was a FALSE exit, not a dead
// recovery path.
//
// The kuFaultController / kuFaultJetStream / kuFaultKeyValue types live in
// manager_kv_read_unavailable_test.go (same package); they are REUSED here by
// reference and must NOT be redeclared. Disarm is done via fc.armed.Store(false)
// (the field is package-accessible) — deliberately no disarm method, to avoid a
// collision with NP-4.

// np3TransitionRecorder is a thread-safe recorder of Degraded<->Stable
// transitions observed via OnStateChanged. Hooks fire from background
// goroutines, so all access is mutex-guarded.
type np3TransitionRecorder struct {
	mu                 sync.Mutex
	degradedToStable   int // exits from Degraded into Stable (the false-exit edge under test)
	stableToDegraded   int // (re-)entries from Stable into Degraded
	kvUnavailableCount int // OnDegraded calls with reason == "kv-unavailable"
}

func (r *np3TransitionRecorder) recordTransition(from, to types.State) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case from == types.StateDegraded && to == types.StateStable:
		r.degradedToStable++
	case from == types.StateStable && to == types.StateDegraded:
		r.stableToDegraded++
	}
}

func (r *np3TransitionRecorder) recordKVUnavailable() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kvUnavailableCount++
}

func (r *np3TransitionRecorder) degradedExits() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.degradedToStable
}

func (r *np3TransitionRecorder) reentries() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.stableToDegraded
}

func (r *np3TransitionRecorder) kvUnavailable() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.kvUnavailableCount
}

// np3Harness bundles the shared NP-3 setup: an embedded NATS, an armed-able
// fault JetStream wrapping the real handle, the recorder, and the started
// manager already stabilized in Stable (but NOT yet armed). The caller arms
// the fault.
type np3Harness struct {
	nc       *nats.Conn
	mgr      *parti.Manager
	fc       *kuFaultController
	cfg      parti.Config
	recorder *np3TransitionRecorder
	// degradedReasons receives the reason string from each OnDegraded call.
	degradedReasons chan string
}

// np3SetupArmedHarness performs the shared setup for both forks, modeled on
// TestManager_KVUnavailable_EntersDegraded: it starts NATS, builds the fault
// JetStream over {Election, Heartbeat, StableID}, wires the hooks, starts the
// manager, waits for Stable, asserts Stable IMMEDIATELY before arming (so the
// committedAssignment == snapshot precondition holds and the false-exit branch
// is the one under test), then arms the fault and confirms the first
// OnDegraded reason is "kv-unavailable".
func np3SetupArmedHarness(t *testing.T, ctx context.Context) *np3Harness {
	t.Helper()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	realJS, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	// Degrade quickly and deterministically under the fault.
	cfg.DegradedBehavior.KVErrorThreshold = 3
	cfg.DegradedBehavior.KVErrorWindow = 15 * time.Second
	// Short thresholds so a (false) recovery would happen fast and the
	// hold window is short. ExitThreshold <= EnterThreshold (valid config).
	cfg.DegradedBehavior.ExitThreshold = 2 * time.Second
	cfg.DegradedBehavior.EnterThreshold = 5 * time.Second
	// RecoveryGracePeriod must be >= 5s to pass config validation.
	cfg.DegradedBehavior.RecoveryGracePeriod = 5 * time.Second

	fc := &kuFaultController{}
	faultJS := &kuFaultJetStream{
		JetStream: realJS,
		fc:        fc,
		buckets: map[string]struct{}{
			cfg.KVBuckets.ElectionBucket:  {},
			cfg.KVBuckets.HeartbeatBucket: {},
			cfg.KVBuckets.StableIDBucket:  {},
		},
	}

	src := source.NewStatic(testutil.CreateTestPartitions(4))

	recorder := &np3TransitionRecorder{}
	degradedReasons := make(chan string, 16)
	hooks := &parti.Hooks{
		OnStateChanged: func(_ context.Context, from, to types.State) error {
			recorder.recordTransition(from, to)

			return nil
		},
		OnDegraded: func(_ context.Context, reason string) error {
			if reason == "kv-unavailable" {
				recorder.recordKVUnavailable()
			}
			select {
			case degradedReasons <- reason:
			default:
			}

			return nil
		},
	}

	mgr, err := parti.NewManager(&cfg, faultJS, src, strategy.NewConsistentHash(), parti.WithHooks(hooks))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	require.NoError(t, mgr.Start(ctx))
	require.NoError(t, <-mgr.WaitState(types.StateStable, 30*time.Second),
		"manager must stabilize before the fault is armed")

	// CRITICAL: assert Stable IMMEDIATELY before arming. Arming only after
	// Stable guarantees committedAssignment == snapshot, so the recovery path
	// under test is the false-exit branch (assignment read succeeds and
	// currentAssignmentApplied(cur) is true), not the stay-degraded
	// scheduleApplyRetry branch.
	require.Equal(t, types.StateStable, mgr.State(),
		"manager must be Stable at the instant the fault is armed")

	// Arm the connected-but-KV-unavailable fault: every op against the
	// election / heartbeat / stableid buckets now times out, but the
	// connection stays up and the assignment bucket is unfaulted.
	fc.arm()

	// The first degraded reason must be "kv-unavailable".
	select {
	case reason := <-degradedReasons:
		require.Equal(t, "kv-unavailable", reason,
			"connected-but-KV-unavailable timeouts must degrade with the distinct reason")
	case <-time.After(30 * time.Second):
		t.Fatalf("manager did not enter Degraded within 30s of the KV-unavailable fault; state=%s", mgr.State())
	}

	require.Equal(t, types.StateDegraded, mgr.State())

	return &np3Harness{
		nc:              nc,
		mgr:             mgr,
		fc:              fc,
		cfg:             cfg,
		recorder:        recorder,
		degradedReasons: degradedReasons,
	}
}

// TestNP3_KVUnavailable_HeldArmed_DoesNotFalselyExitToStable proves the
// Finding A gap. With the kv-unavailable fault held ARMED throughout, the
// heartbeat / election / stableid buckets keep timing out, so the manager is
// genuinely still faulting. attemptRecoveryFromDegraded nonetheless reads the
// UNFAULTED assignment bucket every ~1s and (on current main) exits
// Degraded -> Stable on that wrong signal, then re-degrades = a flap.
//
// HARD GATE (last): zero Degraded -> Stable exits while the fault is armed.
// This is the single load-bearing assertion and is predicted to FAIL on main.
func TestNP3_KVUnavailable_HeldArmed_DoesNotFalselyExitToStable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if os.Getenv("PARTI_RUN_NP3_KVUNAVAIL_FLAP_PROOF") == "" {
		t.Skip("opt-in KNOWN-FAILING proof (NP-3 / deferred Finding A recover-on-wrong-signal flap); " +
			"set PARTI_RUN_NP3_KVUNAVAIL_FLAP_PROOF=1 to run")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	h := np3SetupArmedHarness(t, ctx)

	// Keep the fault ARMED. Observe ~3*ExitThreshold so several recovery
	// attempts (which run on ~1s connection-uptime ticks) elapse. ExitThreshold
	// is 2s, so the window is ~9s. Sample injected at start / mid / end to
	// prove the faulting ops keep being exercised across the window (so a zero
	// exit count is NOT vacuous — the fault is genuinely active the whole time).
	injectedStart := h.fc.injected.Load()

	time.Sleep(4500 * time.Millisecond)
	injectedMid := h.fc.injected.Load()

	time.Sleep(4500 * time.Millisecond)
	injectedEnd := h.fc.injected.Load()

	// NON-VACUITY (universally true; safe as require, evaluated BEFORE the gate):
	// the connection stays up, and faulting ops are still exercised across the
	// whole window. These guarantee the hard gate below is meaningful.
	require.True(t, h.nc.IsConnected(),
		"the NATS connection must remain CONNECTED throughout (this is the whole point)")
	require.Greater(t, injectedMid, injectedStart,
		"faulting ops must keep being exercised in the first half of the window")
	require.Greater(t, injectedEnd, injectedMid,
		"faulting ops must keep being exercised in the second half of the window")

	// CORROBORATION (assert, non-halting): a 2nd+ "kv-unavailable" OnDegraded
	// REQUIRES a prior false exit-to-Stable, so on main this corroborates the
	// flap. It is correlated with the hard gate (same flap), so it is an assert
	// — on an unlucky-but-still-buggy run (one false exit, no re-entry yet
	// within the window) the gate below still fails with a legible message.
	assert.GreaterOrEqual(t, h.recorder.kvUnavailable(), 2,
		"a re-entry into kv-unavailable Degraded proves a prior false exit (the flap)")

	// HARD GATE (sole load-bearing assertion, LAST): while the fault is armed
	// the manager must NOT exit Degraded -> Stable. On current main it does
	// (false exit on the unfaulted assignment read), so this FAILS = the gap
	// is proven. Use require.Zero over the observation window, NOT
	// require.Never (which would halt before the corroboration is read).
	require.Zero(t, h.recorder.degradedExits(),
		"manager must NOT exit Degraded->Stable while heartbeat/election/stableid are still faulting "+
			"(false return-to-Stable on the unfaulted assignment read = Finding A flap); "+
			"observed re-entries=%d injected=%d", h.recorder.reentries(), injectedEnd)
}

// TestNP3_KVUnavailable_Disarm_ReturnsAndHoldsStable is the positive control:
// once the fault genuinely clears, recovery returns to Stable and HOLDS it.
// This proves Fork B's exit was a FALSE exit (recovering on the wrong signal),
// not a dead recovery path. Predicted to PASS on current main.
func TestNP3_KVUnavailable_Disarm_ReturnsAndHoldsStable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	h := np3SetupArmedHarness(t, ctx)

	// Disarm: the fault genuinely clears (heartbeat / election / stableid ops
	// now succeed again). Done via the package-accessible armed field, NOT a
	// disarm method (avoids a collision with NP-4).
	h.fc.armed.Store(false)

	// Recovery must return to Stable. Bound: ExitThreshold + OperationTimeout +
	// margin. OperationTimeout is the 10s default here.
	recoverBound := h.cfg.DegradedBehavior.ExitThreshold + h.cfg.OperationTimeout + 10*time.Second
	require.NoError(t, <-h.mgr.WaitState(types.StateStable, recoverBound),
		"after the fault clears, the manager must recover to Stable")

	// And it must HOLD Stable (it must not re-degrade once genuinely healthy).
	require.Never(t, func() bool {
		return h.mgr.State() == types.StateDegraded
	}, 5*time.Second, 100*time.Millisecond,
		"once the fault has cleared, the manager must HOLD Stable and not re-degrade")
}
