package failure_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

const (
	rfStreamName    = "EVENTS"
	rfSubjectTmpl   = "events.{{.PartitionID}}"
	rfHandoffBucket = "parti-handoff"
	rfClaimsPrefix  = "claims/"
)

// --- READ-axis fault seam ----------------------------------------------------
//
// This is the trimmed READ-only port of the S2 scratch-harness seam
// (tmp/parti-repro/seam.go). The entire WRITE axis is dropped: S2 only needs
// the asymmetric Keys-ok / Get-fail window. The seam wraps the handoff bucket's
// KeyValue and overrides ONLY Get to fault claim-prefix keys when armed; Keys /
// Watch / WatchAll / Status all pass through via embedding. Every other bucket
// and every non-KV JetStream method passes straight through to the real js.

// rfFaultController is the single shared toggle+counter every wrapper references.
type rfFaultController struct {
	// readArmed is the READ-axis toggle. When true, a per-key Get against a
	// claim key faults with context.DeadlineExceeded.
	readArmed atomic.Bool

	// readFaultsInjected counts how many claim-key Get calls actually returned
	// the injected read error. Lets the test assert the asymmetric read fault
	// genuinely fired (non-vacuous) and that a reconcile pass ran under it.
	readFaultsInjected atomic.Int64
}

func newRFFaultController() *rfFaultController { return &rfFaultController{} }

// ArmReads enables the asymmetric read fault: per-key Get on a claim key faults
// while Keys()/WatchAll() stay live. It resets the read-fault counter so the
// test can assert the fault fired from this point.
func (fc *rfFaultController) ArmReads() {
	fc.readFaultsInjected.Store(0)
	fc.readArmed.Store(true)
}

// DisarmReads disables the read fault (simulates KV read recovery — the claim
// becomes Get-able again at its unchanged revision R, since writes were never
// faulted on this axis).
func (fc *rfFaultController) DisarmReads() { fc.readArmed.Store(false) }

// shouldFaultRead reports whether a claim-key Get should fault and counts the
// injected fault. Deterministic atomic toggle — no timing race.
func (fc *rfFaultController) shouldFaultRead() bool {
	if !fc.readArmed.Load() {
		return false
	}
	fc.readFaultsInjected.Add(1)

	return true
}

// rfFaultJetStream wraps a real jetstream.JetStream. Only handoff-bucket KV
// handles are wrapped; everything else passes through.
type rfFaultJetStream struct {
	jetstream.JetStream // embedded: all non-overridden methods pass through

	handoffBucket string
	fc            *rfFaultController
}

func newRFFaultJetStream(inner jetstream.JetStream, handoffBucket string, fc *rfFaultController) *rfFaultJetStream {
	return &rfFaultJetStream{JetStream: inner, handoffBucket: handoffBucket, fc: fc}
}

func (f *rfFaultJetStream) wrap(kv jetstream.KeyValue, bucket string) jetstream.KeyValue {
	if bucket == f.handoffBucket {
		return &rfFaultKeyValue{KeyValue: kv, fc: f.fc, claimsPrefix: rfClaimsPrefix}
	}

	return kv
}

// KeyValue is the get-first lookup used by both manager setup and consumer
// setup (durable.ensureGateResolver → kvutil.EnsureKVBucket → js.KeyValue).
func (f *rfFaultJetStream) KeyValue(ctx context.Context, bucket string) (jetstream.KeyValue, error) {
	kv, err := f.JetStream.KeyValue(ctx, bucket)
	if err != nil {
		return kv, err
	}

	return f.wrap(kv, bucket), nil
}

// CreateKeyValue is the create path used when the bucket does not yet exist.
func (f *rfFaultJetStream) CreateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	kv, err := f.JetStream.CreateKeyValue(ctx, cfg)
	if err != nil {
		return kv, err
	}

	return f.wrap(kv, cfg.Bucket), nil
}

// CreateOrUpdateKeyValue is wrapped for completeness so a future setup change
// cannot leak an unwrapped handoff handle.
func (f *rfFaultJetStream) CreateOrUpdateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	kv, err := f.JetStream.CreateOrUpdateKeyValue(ctx, cfg)
	if err != nil {
		return kv, err
	}

	return f.wrap(kv, cfg.Bucket), nil
}

// rfFaultKeyValue wraps the real handoff KeyValue and faults ONLY the per-key
// claim READ path (Get of a claimsPrefix key) when the READ axis is armed.
// Keys() and WatchAll() are NEVER overridden — they always pass through. That
// is the whole point: it reproduces the asymmetric Keys-ok/Get-fail window, and
// the live WatchAll lets the resolver's watcher + reconcile-triggered replay run.
type rfFaultKeyValue struct {
	jetstream.KeyValue // embedded: Keys/Watch/WatchAll/Status/... pass through
	fc                 *rfFaultController
	claimsPrefix       string
}

// Get is the per-key read primitive the resolver uses both in warm() (Start)
// and in every reconcileOnce pass. When the READ axis is armed it faults a
// CLAIM key (claimsPrefix) with context.DeadlineExceeded; non-claim keys and
// all reads while disarmed pass straight through.
func (k *rfFaultKeyValue) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if k.claimsPrefix != "" && len(key) >= len(k.claimsPrefix) && key[:len(k.claimsPrefix)] == k.claimsPrefix {
		if k.fc.shouldFaultRead() {
			return nil, context.DeadlineExceeded
		}
	}

	return k.KeyValue.Get(ctx, key)
}

// --- Minimal worker stack ----------------------------------------------------

// rfWorkerStack bundles one worker's manager, its consumer.Dynamic, and the
// consumption counter. The scenario uses a single partition, so one atomic
// counter of consumed events.* messages is sufficient.
type rfWorkerStack struct {
	mgr      *parti.Manager
	dyn      *consumer.Dynamic
	consumed atomic.Int64
}

// rfBuildStream creates the events stream (file storage — single-node embedded
// server so storage type does not affect symptom injection).
func rfBuildStream(t *testing.T, ctx context.Context, realJS jetstream.JetStream) {
	t.Helper()
	_, err := realJS.CreateStream(ctx, jetstream.StreamConfig{
		Name:      rfStreamName,
		Subjects:  []string{"events.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.FileStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)
}

// rfBuildWorkerStack constructs one worker: a consumer.Dynamic (pull-gating +
// processing gate + auto-created claim resolver reading the WRAPPED handoff KV)
// wired into a Manager via WithWorkerConsumerUpdater. The manager runs two-phase
// handoff. faultJS is handed to BOTH NewDynamic and NewManager.
func rfBuildWorkerStack(
	t *testing.T,
	ctx context.Context,
	faultJS jetstream.JetStream,
	src parti.PartitionSource,
	index int,
) *rfWorkerStack {
	t.Helper()

	s := &rfWorkerStack{}

	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		const prefix = "events."
		if subj := msg.Subject(); len(subj) > len(prefix) {
			s.consumed.Add(1)
		}

		return msg.Ack()
	})

	dyn, err := consumer.NewDynamic(
		faultJS,
		rfStreamName,
		fmt.Sprintf("w%d", index),
		rfSubjectTmpl,
		handler,
		consumer.WithPullGating(true),
		consumer.WithProcessingGate(&consumer.ProcessingGateConfig{
			Enabled:  true,
			NakDelay: 50 * time.Millisecond,
			AllowedStates: []types.HandoffState{
				types.HandoffStateStable,
				types.HandoffStateCommit,
			},
		}),
		consumer.WithResolver(consumer.ResolverConfig{
			HandoffBucketName:   rfHandoffBucket,
			HandoffClaimsPrefix: rfClaimsPrefix,
			// Keep the auto-created resolver's reconcile snappy so the
			// Keys-ok/Get-fail window is hit within the test bound.
			ReconcileInterval: 1 * time.Second,
		}),
	)
	require.NoError(t, err)
	s.dyn = dyn
	t.Cleanup(func() { _ = dyn.Stop(context.Background()) })

	cfg := parti.TestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = rfHandoffBucket
	cfg.WorkerIDMax = 8
	cfg.StartupTimeout = 60 * time.Second

	mgr, err := parti.NewManager(&cfg, faultJS, src, strategy.NewConsistentHash(),
		parti.WithWorkerConsumerUpdater(dyn),
	)
	require.NoError(t, err)
	s.mgr = mgr
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	require.NoError(t, mgr.Start(ctx))

	return s
}

// rfReadClaimRevision opens the REAL (unwrapped) handoff KV and returns the
// claim's KV revision for pid, whether it exists, and whether it is Stable.
// It MUST be called with realJS (never the wrapped faultJS) so that an armed
// read fault cannot make the read-back observe a phantom absence.
func rfReadClaimRevision(t *testing.T, ctx context.Context, realJS jetstream.JetStream, pid string) (revision uint64, exists bool, stable bool) {
	t.Helper()
	kv, err := realJS.KeyValue(ctx, rfHandoffBucket)
	require.NoError(t, err)
	entry, err := kv.Get(ctx, rfClaimsPrefix+pid)
	if err != nil {
		return 0, false, false
	}
	cl, err := handoff.UnmarshalClaim(entry.Value())
	require.NoError(t, err)

	return entry.Revision(), true, cl.State == handoff.ClaimStateStable
}

// rfPublish sends one message to events.<pid> via the REAL js.
func rfPublish(t *testing.T, ctx context.Context, realJS jetstream.JetStream, pid, payload string) {
	t.Helper()
	_, err := realJS.Publish(ctx, "events."+pid, []byte(payload))
	require.NoError(t, err)
}

// rfOwnsPID reports whether the worker's current assignment owns pid.
func rfOwnsPID(s *rfWorkerStack, pid string) bool {
	for _, p := range s.mgr.CurrentAssignment().Partitions {
		if p.ID() == pid {
			return true
		}
	}

	return false
}

// TestResolverReadFault_ConsumerSurvivesQuorumLossWindow pins the resolver
// tombstone fix end-to-end through the public NewManager + NewDynamic stack.
//
// The bug (Defect 2, on the parent base): a quorum-loss "Keys-ok/Get-fail"
// window made reconcileOnce stage a synthetic delete tombstone at R+1 for a
// claim that still sat live in KV at revision R, permanently suppressing the
// consumer until a restart. The original scratch scenario asserted that
// suppression as the killer signature.
//
// This test FLIPS those assertions to the FIXED behaviour: a listed-but-
// unreadable claim no longer tombstones, so the consumer keeps consuming
// THROUGHOUT the read fault and after it recovers — with NO restart.
//
// The oracle is purely the CONSUMER data plane. We deliberately do NOT assert
// on the manager's Degraded state (the manager-side classifier is a separate
// later change); no OnDegraded hook is wired.
func TestResolverReadFault_ConsumerSurvivesQuorumLossWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	realJS, err := jetstream.New(nc)
	require.NoError(t, err)
	fc := newRFFaultController()
	faultJS := newRFFaultJetStream(realJS, rfHandoffBucket, fc)

	rfBuildStream(t, ctx, realJS)

	// Single partition, single worker — the steady-state trap. NO rebalance:
	// the bug poisons an ALREADY-committed-version cache, so no apply must fire
	// after Stable. Static is non-watchable, so the calculator never re-applies
	// after Stable — that is what keeps the claim pinned at revision R.
	const pid = "p0"
	src := source.NewStatic([]types.Partition{{Keys: []string{pid}}})

	stack := rfBuildWorkerStack(t, ctx, faultJS, src, 0)
	require.NoError(t, <-stack.mgr.WaitState(parti.StateStable, 30*time.Second),
		"manager did not reach Stable")

	// (1) Reach a Stable claim for p0 in KV at revision R, with the worker
	// owning p0. Read-back uses the UNWRAPPED realJS.
	var revR uint64
	require.Eventually(t, func() bool {
		rev, exists, stable := rfReadClaimRevision(t, ctx, realJS, pid)
		if exists && stable && rfOwnsPID(stack, pid) {
			revR = rev

			return true
		}

		return false
	}, 20*time.Second, 100*time.Millisecond, "p0 did not reach a Stable claim in KV")
	require.Positive(t, revR, "claim revision R must be > 0")

	// (2) Baseline: a steady-state publish is consumed (gate open).
	rfPublish(t, ctx, realJS, pid, "pre-p0")
	require.Eventually(t, func() bool { return stack.consumed.Load() >= 1 },
		15*time.Second, 100*time.Millisecond, "p0 baseline pull failed — gate not open at steady state")

	t.Logf("[%s] BASELINE: claim p0 Stable in KV at revision R=%d, baseline pull OK, state=%s",
		t.Name(), revR, stack.mgr.State())

	// (3) Arm the ASYMMETRIC read fault: Keys() stays live, Get(claims/p0)
	// returns context.DeadlineExceeded. Writes are untouched, so the claim
	// stays committed at R.
	fc.ArmReads()

	// (4) Force at least one reconcileOnce pass under the fault: wait until a
	// claim-key Get has actually faulted at least twice (proves a reconcile
	// cycle observed the Keys-ok/Get-fail window — the exact condition that
	// tombstoned on the parent base). This ordering is load-bearing: the
	// "during-p0" publish below MUST happen AFTER the fault has fired, or the
	// FLIP assertion would prove nothing.
	require.Eventually(t, func() bool { return fc.readFaultsInjected.Load() >= 2 },
		15*time.Second, 25*time.Millisecond,
		"the resolver never performed a faulted claim-key Get under the read fault — "+
			"no reconcile pass observed the Keys-ok/Get-fail window")

	// (5) FLIP — the key assertion. Publish AFTER faults have fired, then assert
	// the message IS consumed within a bound comfortably above the 1s
	// ReconcileInterval. The original asserted NOT-consumed (the bug suppressed
	// the pull); the fix means the claim is NEVER tombstoned, so the consumer
	// keeps working under the read fault.
	baseDuring := stack.consumed.Load()
	rfPublish(t, ctx, realJS, pid, "during-p0")
	require.Eventually(t, func() bool { return stack.consumed.Load() > baseDuring },
		10*time.Second, 100*time.Millisecond,
		"with the fix, a listed-but-unreadable claim must NOT suppress the consumer — "+
			"p0 was published after the Keys-ok/Get-fail window fired and must still be consumed")

	// (6) The claim must STILL be present in KV at the UNCHANGED revision R while
	// the fault is armed — proving the read fault is read-only and no tombstone
	// or rewrite occurred (writes were never faulted). Read-back uses realJS.
	revDuring, existsDuring, stableDuring := rfReadClaimRevision(t, ctx, realJS, pid)
	require.True(t, existsDuring && stableDuring,
		"claim must remain present+Stable in KV while only reads are faulted")
	require.Equal(t, revR, revDuring,
		"claim revision must be unchanged at R while only reads are faulted")

	t.Logf("[%s] DURING read-fault: readFaults=%d during-p0 consumed claimRevInKV=%d(==R %v) state=%s",
		t.Name(), fc.readFaultsInjected.Load(), revDuring, revDuring == revR, stack.mgr.State())

	// (7) Disarm reads (simulate KV read recovery). No restart anywhere.
	fc.DisarmReads()

	// (8) Publish AFTER recovery; the consumer continues without any restart.
	baseAfter := stack.consumed.Load()
	rfPublish(t, ctx, realJS, pid, "post-p0")
	require.Eventually(t, func() bool { return stack.consumed.Load() > baseAfter },
		15*time.Second, 100*time.Millisecond,
		"after read recovery the consumer must continue without any restart")

	t.Logf("[%s] AFTER read-recovery (no restart): consumer continued; total p0 consumed=%d",
		t.Name(), stack.consumed.Load())
}
