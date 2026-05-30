package failure_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// wfMutableSource is a minimal types.WatchablePartitionSource. source.Static is
// NOT watchable, which drives the calculator down a cold-start path that
// re-publishes a fresh assignment version (V1→V2) — a cold-cluster-bootstrap
// artifact that is NOT part of the F-D3 "restart into an existing fleet"
// scenario. A watchable source whose partition set never changes after
// construction keeps the assignment pinned at V1, matching the traced
// long-outage repro. Ported from tmp/parti-repro/source.go.
type wfMutableSource struct {
	mu    sync.Mutex
	parts []types.Partition
	ch    chan struct{}
}

func newWFMutableSource(parts []types.Partition) *wfMutableSource {
	return &wfMutableSource{
		parts: append([]types.Partition(nil), parts...),
		ch:    make(chan struct{}, 1),
	}
}

func (s *wfMutableSource) Start(_ context.Context) error { return nil }
func (s *wfMutableSource) Stop(_ context.Context) error  { return nil }

func (s *wfMutableSource) List(_ context.Context) ([]types.Partition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]types.Partition(nil), s.parts...), nil
}

func (s *wfMutableSource) Watch(_ context.Context) <-chan struct{} { return s.ch }

// --- WRITE-axis fault seam ---------------------------------------------------
//
// The write-axis companion to resolver_readfault_test.go's READ seam. It wraps
// the handoff bucket's KeyValue and faults ONLY the per-claim WRITE path
// (Create / Update / Put of a claims-prefix key) when armed; Get / Keys / Watch
// all pass through. This reproduces a startup KV-write outage: the two-phase
// coordinator's preparePhase write (kv_store.go's Create/Update via PutIfEpoch)
// fails, so the initial startup apply fails and schedules a retry.
//
// This file reuses the worker-stack / stream / read-back helpers and constants
// (rfBuildStream, rfBuildWorkerStack, rfReadClaimRevision, rfPublish,
// rfHandoffBucket, rfClaimsPrefix) defined in resolver_readfault_test.go — both
// files are in package failure_test.

// wfFaultController is the write-axis toggle+counter.
type wfFaultController struct {
	// writeArmed is the WRITE-axis toggle. When true, a Create/Update/Put
	// against a claim key faults with context.DeadlineExceeded.
	writeArmed atomic.Bool

	// writeFaultsInjected counts how many claim-key writes actually returned
	// the injected error. Lets the test assert the fault genuinely fired
	// (non-vacuous) before disarming.
	writeFaultsInjected atomic.Int64
}

// ArmWrites enables the claim-write fault and resets the counter.
func (fc *wfFaultController) ArmWrites() {
	fc.writeFaultsInjected.Store(0)
	fc.writeArmed.Store(true)
}

// DisarmWrites disables the write fault (simulates KV write recovery).
func (fc *wfFaultController) DisarmWrites() { fc.writeArmed.Store(false) }

// shouldFaultWrite reports whether a claim-key write should fault and counts it.
func (fc *wfFaultController) shouldFaultWrite() bool {
	if !fc.writeArmed.Load() {
		return false
	}
	fc.writeFaultsInjected.Add(1)

	return true
}

// wfFaultJetStream wraps a real jetstream.JetStream. Only the handoff-bucket KV
// handle is wrapped; every other bucket and method passes through.
type wfFaultJetStream struct {
	jetstream.JetStream // embedded: all non-overridden methods pass through

	handoffBucket string
	fc            *wfFaultController
}

func newWFFaultJetStream(inner jetstream.JetStream, handoffBucket string, fc *wfFaultController) *wfFaultJetStream {
	return &wfFaultJetStream{JetStream: inner, handoffBucket: handoffBucket, fc: fc}
}

func (f *wfFaultJetStream) wrap(kv jetstream.KeyValue, bucket string) jetstream.KeyValue {
	if bucket == f.handoffBucket {
		return &wfFaultKeyValue{KeyValue: kv, fc: f.fc, claimsPrefix: rfClaimsPrefix}
	}

	return kv
}

func (f *wfFaultJetStream) KeyValue(ctx context.Context, bucket string) (jetstream.KeyValue, error) {
	kv, err := f.JetStream.KeyValue(ctx, bucket)
	if err != nil {
		return kv, err
	}

	return f.wrap(kv, bucket), nil
}

func (f *wfFaultJetStream) CreateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	kv, err := f.JetStream.CreateKeyValue(ctx, cfg)
	if err != nil {
		return kv, err
	}

	return f.wrap(kv, cfg.Bucket), nil
}

func (f *wfFaultJetStream) CreateOrUpdateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	kv, err := f.JetStream.CreateOrUpdateKeyValue(ctx, cfg)
	if err != nil {
		return kv, err
	}

	return f.wrap(kv, cfg.Bucket), nil
}

// wfFaultKeyValue wraps the real handoff KeyValue and faults ONLY claim-key
// writes (Create / Update / Put of a claimsPrefix key) when the WRITE axis is
// armed. Reads pass through so the resolver can still list/get.
type wfFaultKeyValue struct {
	jetstream.KeyValue // embedded: Get/Keys/Watch/Status/... pass through
	fc                 *wfFaultController
	claimsPrefix       string
}

func (k *wfFaultKeyValue) isClaimKey(key string) bool {
	return k.claimsPrefix != "" && len(key) >= len(k.claimsPrefix) && key[:len(k.claimsPrefix)] == k.claimsPrefix
}

func (k *wfFaultKeyValue) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if k.isClaimKey(key) && k.fc.shouldFaultWrite() {
		return 0, context.DeadlineExceeded
	}

	return k.KeyValue.Create(ctx, key, value, opts...)
}

func (k *wfFaultKeyValue) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if k.isClaimKey(key) && k.fc.shouldFaultWrite() {
		return 0, context.DeadlineExceeded
	}

	return k.KeyValue.Update(ctx, key, value, revision)
}

func (k *wfFaultKeyValue) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	if k.isClaimKey(key) && k.fc.shouldFaultWrite() {
		return 0, context.DeadlineExceeded
	}

	return k.KeyValue.Put(ctx, key, value)
}

// TestStartupWriteFault_SelfHealsWithoutRestart pins the startup empty-diff
// retry fix (RC3 / F-D3) end-to-end through the public NewManager + NewDynamic
// stack.
//
// The bug (on the parent base): a KV-write fault during the one-shot initial
// startup apply fails the claim write. waitForAssignment has already
// pre-advanced the in-memory snapshot to the full partition set, so
// scheduleApplyRetry re-applies with prev = CurrentAssignment() = that full set
// → an EMPTY prepare diff → the retry "succeeds" writing ZERO claims and
// self-exits. The claim never lands in KV and the consumer is suppressed until a
// process restart — even after the KV write fault clears.
//
// This test FLIPS the original scratch scenario's "restart-only" assertion to
// the FIXED behaviour: once the write fault clears, the retry (which now uses an
// explicit empty prev until the first claim commit) re-writes the FULL claim set
// and the consumer resumes — with NO restart.
//
// On the parent base this test FAILS at the healedA step: the claim never
// reappears because the retry self-exited on the first empty-diff success.
func TestStartupWriteFault_SelfHealsWithoutRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	realJS, err := jetstream.New(nc)
	require.NoError(t, err)
	fc := &wfFaultController{}
	faultJS := newWFFaultJetStream(realJS, rfHandoffBucket, fc)

	rfBuildStream(t, ctx, realJS)

	// Single (leader) worker over a WATCHABLE source whose partition set never
	// changes — the assignment pins at V1 so the only claim-writing path during
	// the fault window is the initial apply and its scheduleApplyRetry. This is
	// the traced long-outage repro (tmp/parti-repro scenario_b), NOT a fresh
	// cold cluster that would re-publish V2.
	pids := []string{"p0", "p1"}
	src := newWFMutableSource([]types.Partition{{Keys: []string{pids[0]}}, {Keys: []string{pids[1]}}})

	allStable := func() bool {
		for _, pid := range pids {
			if _, _, stable := rfReadClaimRevision(t, ctx, realJS, pid); !stable {
				return false
			}
		}

		return true
	}

	// Arm the write fault BEFORE the worker starts so the INITIAL startup apply's
	// claim write faults. waitForAssignment then pre-advances the snapshot to the
	// full set with no claims written — the precondition for the empty-diff retry.
	fc.ArmWrites()

	stack := rfBuildWorkerStack(t, ctx, faultJS, src, 0)

	// The initial startup apply must attempt a faulted claim write.
	require.Eventually(t, func() bool { return fc.writeFaultsInjected.Load() >= 1 },
		30*time.Second, 25*time.Millisecond,
		"the initial startup apply never attempted a faulted claim write")

	// Non-vacuous precondition: no Stable claim while claim writes are faulted.
	require.False(t, allStable(), "no claim must be Stable in KV while claim writes are faulted")

	// Hold the fault briefly so the parent's retry demonstrably self-exits on its
	// empty-diff trivial success during the window (claims stay absent), pushing
	// the scenario into the regime that is restart-only on the parent.
	time.Sleep(2 * time.Second)
	require.False(t, allStable(), "claims must still be absent/non-Stable after the retry window under fault")

	t.Logf("[%s] DURING write-fault: writeFaults=%d asg(V=%d parts=%d) state=%s",
		t.Name(), fc.writeFaultsInjected.Load(),
		stack.mgr.CurrentAssignment().Version, len(stack.mgr.CurrentAssignment().Partitions), stack.mgr.State())

	// --- Disarm: simulate KV write recovery. NO restart anywhere. ---
	fc.DisarmWrites()

	// (a) KV layer: every claim must now appear Stable WITHOUT a restart. On the
	// parent the retry already self-exited, so this never happens (RED).
	require.Eventually(t, allStable, 40*time.Second, 100*time.Millisecond,
		"with the fix the retry re-writes the FULL claim set after write recovery — "+
			"every claim must appear Stable in KV WITHOUT any restart")

	require.NoError(t, <-stack.mgr.WaitState(parti.StateStable, 20*time.Second),
		"manager did not reach Stable after write recovery")

	// (b) Consumer layer: a post-recovery publish is consumed end-to-end.
	base := stack.consumed.Load()
	for _, pid := range pids {
		rfPublish(t, ctx, realJS, pid, "post-"+pid)
	}
	require.Eventually(t, func() bool { return stack.consumed.Load() > base },
		40*time.Second, 100*time.Millisecond,
		"after write recovery the consumer must resume pulling without any restart")

	t.Logf("[%s] AFTER write-recovery (no restart): all claims Stable, consumer resumed total=%d",
		t.Name(), stack.consumed.Load())
}

// Note on the version-advance scenario: the path that specifically REQUIRES the
// override to live in the shared apply pipeline (rather than only in
// scheduleApplyRetry) — a higher-version apply over the pre-advanced snapshot —
// is pinned deterministically by the unit test
// TestApplyCore_BootstrapVersionAdvanceOverridesPrev (proven RED under
// retry-only placement by mutation). It is not duplicated as an integration
// case here: with the fix in place the override makes that V2 apply FAIL under
// the write fault (a real write attempt), so the worker snapshot never falsely
// advances to V2 during the fault — there is no longer a worker-observable
// version advance to assert against, which is precisely the corrected behavior.
