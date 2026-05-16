package parti

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// forwardingHandoff is a minimal handoff.Coordinator stub that mirrors
// production wiring for capability sampling tests: Apply forwards to the
// registered consumer updater and returns its error. The ordering of the
// updater call and the subsequent reportConsumerCapabilities() invocation
// is what Test #7 verifies.
type forwardingHandoff struct {
	updater WorkerConsumerUpdater
}

func (h *forwardingHandoff) Start(_ context.Context) {}

func (h *forwardingHandoff) Apply(ctx context.Context, workerID string, _, next types.Assignment) error {
	if h.updater == nil {
		return nil
	}
	return h.updater.UpdateWorkerConsumer(ctx, workerID, next.Partitions)
}

// gateReportingUpdater is the stub specified in plan §Test #7: a
// WorkerConsumerUpdater that also implements CapabilityReporter. Its
// reported bit starts at 0; UpdateWorkerConsumer atomically stores
// CapProcessingGate into the reported value before returning a non-nil
// error. This proves the post-Apply sampling ordering: a manager that
// samples BEFORE the updater runs would observe reported=0 and the bit
// would not be set after applyAssignmentWithPrev returns.
type gateReportingUpdater struct {
	reported atomic.Uint32
	updates  atomic.Int64
	err      error
}

func (g *gateReportingUpdater) UpdateWorkerConsumer(_ context.Context, _ string, _ []Partition) error {
	g.reported.Store(types.CapProcessingGate)
	g.updates.Add(1)
	return g.err
}

func (g *gateReportingUpdater) Capabilities() uint32 { return g.reported.Load() }

// TestManager_CapProcessingGate_SampledAfterUpdaterOnApplyError verifies
// the post-updater sampling-order invariant: reportConsumerCapabilities()
// MUST run AFTER the consumer updater has had a chance to wire the
// capability, regardless of whether the apply path succeeds or errors.
//
// A buggy implementation that samples BEFORE the updater would observe
// the stub's reported value as 0 (the initial atomic load), so the
// CapProcessingGate bit would not be set after applyAssignmentWithPrev
// returns. The assertion below specifically exercises this ordering by
// returning a non-nil error from UpdateWorkerConsumer.
func TestManager_CapProcessingGate_SampledAfterUpdaterOnApplyError(t *testing.T) {
	t.Parallel()

	stub := &gateReportingUpdater{err: errors.New("synthetic apply-phase failure")}

	m, _, _, _ := newTestManager(t)
	m.consumerUpdater = stub
	m.capReporter = asCapabilityReporter(stub)
	m.handoffCoordinator = &forwardingHandoff{updater: stub}

	err := m.applyAssignmentWithPrev(Assignment{}, Assignment{
		Version:        1,
		LeaderRevision: 1,
		Partitions:     []Partition{{Keys: []string{"p1"}}},
	})
	require.Error(t, err, "applyAssignmentWithPrev must propagate the updater's error")
	require.Equal(t, int64(1), stub.updates.Load(),
		"sanity: updater MUST have been invoked exactly once")
	require.NotZero(t, m.Capabilities()&types.CapProcessingGate,
		"CapProcessingGate MUST be sampled AFTER the updater runs, even on apply error")
}

// gateIntegrationManagerCfg builds a minimal Config suitable for a single-
// worker integration test that exercises the real Start path. Bucket names
// must be unique per test to avoid cross-test KV interference within the
// shared embedded NATS server.
func gateIntegrationManagerCfg(suffix string) *Config {
	return &Config{
		WorkerIDPrefix:        "worker",
		WorkerIDMin:           0,
		WorkerIDMax:           9,
		WorkerIDTTL:           10 * time.Second,
		HeartbeatInterval:     200 * time.Millisecond,
		HeartbeatTTL:          2 * time.Second,
		ElectionTimeout:       2 * time.Second,
		StartupTimeout:        15 * time.Second,
		ShutdownTimeout:       5 * time.Second,
		ColdStartWindow:       500 * time.Millisecond,
		PlannedScaleWindow:    500 * time.Millisecond,
		RestartDetectionRatio: 0.5,
		RebalanceCooldown:     100 * time.Millisecond,
		EmergencyGracePeriod:  750 * time.Millisecond,
		KVBuckets: KVBucketConfig{
			StableIDBucket:   "parti-stableid-" + suffix,
			ElectionBucket:   "parti-election-" + suffix,
			HeartbeatBucket:  "parti-heartbeat-" + suffix,
			AssignmentBucket: "parti-assignment-" + suffix,
			HandoffBucket:    "parti-handoff-" + suffix,
			HandoffTTL:       30 * time.Second,
		},
	}
}

// preCreateHeartbeatBucketWithHistory pre-creates the heartbeat KV bucket
// with a larger History value BEFORE Manager.Start runs. Manager's own
// bucket-create call uses kvutil.EnsureKVBucketWithRetry, which opens
// the existing bucket if one is already present — so the larger History
// limit survives. This lets waitForPostApplyHeartbeat replay the retained
// heartbeat sequence and pick the FIRST one with AppliedVersion > 0,
// which is provably the immediate post-apply PublishNow heartbeat (the
// publisher's AppliedVersion is monotone, see
// internal/heartbeat/publisher.go:168). Without this, History=1
// (manager_setup.go:148) means a later ticker heartbeat could overwrite
// the post-apply one before our watcher gets to it — the proof gap
// v2 P1 identified.
func preCreateHeartbeatBucketWithHistory(t *testing.T, ctx context.Context, js jetstream.JetStream, bucketName string) {
	t.Helper()
	_, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  bucketName,
		History: 64, // retains the full publish sequence for the test window
	})
	require.NoError(t, err)
}

// waitForPostApplyHeartbeat watches the heartbeat KV bucket and returns
// the FIRST heartbeat with a non-zero AppliedVersion for the given
// worker. With the bucket pre-created at sufficient History (see
// preCreateHeartbeatBucketWithHistory), IncludeHistory() replays the
// retained heartbeat sequence — so the FIRST AppliedVersion>0 entry
// is provably the immediate post-apply PublishNow, regardless of when
// this function is called relative to Manager.Start. Relies on the
// publisher's AppliedVersion being monotone non-decreasing
// (internal/heartbeat/publisher.go:168).
func waitForPostApplyHeartbeat(
	t *testing.T, ctx context.Context, js jetstream.JetStream, bucketName, workerID string, timeout time.Duration,
) types.Heartbeat {
	t.Helper()

	kv, err := js.KeyValue(ctx, bucketName)
	require.NoError(t, err)

	w, err := kv.Watch(ctx, "heartbeat."+workerID, jetstream.IncludeHistory())
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Stop() })

	deadline := time.After(timeout)
	for {
		select {
		case e, ok := <-w.Updates():
			if !ok {
				t.Fatalf("heartbeat watcher closed before post-apply publish for %s", workerID)
			}
			if e == nil {
				continue
			}
			if e.Operation() == jetstream.KeyValuePut {
				hb, decodeErr := types.DecodeHeartbeat(e.Value())
				require.NoError(t, decodeErr)
				if hb.AppliedVersion > 0 {
					return hb
				}
			}
		case <-deadline:
			t.Fatalf("no post-apply heartbeat (AppliedVersion>0) for %s within %s", workerID, timeout)
		case <-ctx.Done():
			t.Fatalf("context cancelled while waiting for post-apply heartbeat: %v", ctx.Err())
		}
	}
}

// TestManager_CapProcessingGate_ReportsAfterFirstApply runs a real
// single-worker Manager wired with consumer.Dynamic (gate enabled) as
// the WorkerConsumerUpdater and asserts that after the initial apply
// the CapProcessingGate bit appears in both Manager.Capabilities() AND
// the published heartbeat. Driving "the published heartbeat" proves the
// PublishNow inside applyAssignmentWithPrev saw the new bit (because
// reportConsumerCapabilities runs BEFORE SetAppliedAssignment +
// PublishNow).
func TestManager_CapProcessingGate_ReportsAfterFirstApply(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Stream for the Dynamic consumer.
	streamName := "EVENTS_CAP_GATE"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"capgate.>"},
	})
	require.NoError(t, err)

	partitions := []types.Partition{{Keys: []string{"p0"}, Weight: 100}}
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()
	cfg := gateIntegrationManagerCfg("capgate-on")

	gateCfg := &consumer.ProcessingGateConfig{Enabled: true}
	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })
	dyn, err := consumer.NewDynamic(js, streamName, "capgate", "capgate.{{.PartitionID}}", handler,
		consumer.WithProcessingGate(gateCfg),
		consumer.WithResolver(consumer.ResolverConfig{OwnershipResolver: partitest.NopOwnershipResolver{}}),
	)
	require.NoError(t, err)
	defer func() { _ = dyn.Stop(ctx) }()

	// Pre-create the heartbeat bucket with enough History so the post-Start
	// watcher can replay the full publish sequence — provably observing
	// the immediate post-apply PublishNow, not a later ticker heartbeat.
	preCreateHeartbeatBucketWithHistory(t, ctx, js, cfg.KVBuckets.HeartbeatBucket)

	mgr, err := NewManager(cfg, js, src, assignStrat, WithWorkerConsumerUpdater(dyn))
	require.NoError(t, err)

	require.NoError(t, mgr.Start(ctx))
	defer func() {
		stopCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		_ = mgr.Stop(stopCtx)
	}()

	// Observe the FIRST post-apply heartbeat (AppliedVersion > 0) via a
	// KV watcher with history replay. This proves the immediate
	// PublishNow inside applyAssignmentWithPrev saw the new capability
	// bit — a later ticker heartbeat cannot satisfy this assertion.
	hb := waitForPostApplyHeartbeat(t, ctx, js, cfg.KVBuckets.HeartbeatBucket, mgr.WorkerID(), 10*time.Second)
	require.NotZero(t, hb.Capabilities&types.CapProcessingGate,
		"the immediate post-apply PublishNow heartbeat MUST carry CapProcessingGate "+
			"(proves reportConsumerCapabilities ran before PublishNow)")

	// And Manager.Capabilities() must reflect the same bit at this point
	// — the publisher reads it from the manager's live bitmask callback.
	require.NotZero(t, mgr.Capabilities()&types.CapProcessingGate,
		"Manager.Capabilities() must include CapProcessingGate after the first apply")
}

// TestManager_CapProcessingGate_StaysClearWithoutGate verifies the same
// path with the processing gate disabled: the bit must remain clear in
// both Manager.Capabilities() and the published heartbeat.
func TestManager_CapProcessingGate_StaysClearWithoutGate(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "EVENTS_CAP_NOGATE"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"capnogate.>"},
	})
	require.NoError(t, err)

	partitions := []types.Partition{{Keys: []string{"p0"}, Weight: 100}}
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()
	cfg := gateIntegrationManagerCfg("capgate-off")

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })
	dyn, err := consumer.NewDynamic(js, streamName, "capnogate", "capnogate.{{.PartitionID}}", handler)
	require.NoError(t, err)
	defer func() { _ = dyn.Stop(ctx) }()

	preCreateHeartbeatBucketWithHistory(t, ctx, js, cfg.KVBuckets.HeartbeatBucket)

	mgr, err := NewManager(cfg, js, src, assignStrat, WithWorkerConsumerUpdater(dyn))
	require.NoError(t, err)

	require.NoError(t, mgr.Start(ctx))
	defer func() {
		stopCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		_ = mgr.Stop(stopCtx)
	}()

	// Use the same event-driven approach: capture the FIRST post-apply
	// heartbeat and assert CapProcessingGate is clear. Cannot use a
	// "stays clear over time" assertion because the gate-disabled path
	// has no event we could otherwise hook; the first post-apply
	// heartbeat is the load-bearing one (publisher reads caps then).
	hb := waitForPostApplyHeartbeat(t, ctx, js, cfg.KVBuckets.HeartbeatBucket, mgr.WorkerID(), 10*time.Second)
	require.Zero(t, hb.Capabilities&types.CapProcessingGate,
		"published heartbeat MUST NOT carry CapProcessingGate when the gate is disabled")
	require.Zero(t, mgr.Capabilities()&types.CapProcessingGate,
		"Manager.Capabilities() MUST NOT include CapProcessingGate when the gate is disabled")
}

// TestManager_CapProcessingGate_EmptyAssignmentStaysClear verifies the
// empty-assignment edge case: with the processing gate enabled but no
// partitions in the assignment, the WorkerConsumer never creates a
// per-subject consumer and never wraps a handler, so gateWired stays
// false and the bit stays 0. A subsequent non-empty apply must flip it on.
func TestManager_CapProcessingGate_EmptyAssignmentStaysClear(t *testing.T) {
	t.Parallel()

	_, nc := partitest.StartEmbeddedNATS(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	streamName := "EVENTS_CAP_EMPTY"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"capempty.>"},
	})
	require.NoError(t, err)

	gateCfg := &consumer.ProcessingGateConfig{Enabled: true}
	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })
	dyn, err := consumer.NewDynamic(js, streamName, "capempty", "capempty.{{.PartitionID}}", handler,
		consumer.WithProcessingGate(gateCfg),
		consumer.WithResolver(consumer.ResolverConfig{OwnershipResolver: partitest.NopOwnershipResolver{}}),
	)
	require.NoError(t, err)
	defer func() { _ = dyn.Stop(ctx) }()

	// Use the lower-level test Manager with a forwarding handoff so we can
	// drive empty / non-empty applies deterministically without spinning
	// up the full Manager.Start election + assignment pipeline.
	m, _, _, _ := newTestManager(t)
	m.consumerUpdater = dyn
	m.capReporter = asCapabilityReporter(dyn)
	m.handoffCoordinator = &forwardingHandoff{updater: dyn}

	// 1) Empty assignment: no subjects to wrap; bit must stay clear.
	require.NoError(t, m.applyAssignmentWithPrev(Assignment{},
		Assignment{Version: 1, LeaderRevision: 1, Partitions: nil}))
	require.Zero(t, m.Capabilities()&types.CapProcessingGate,
		"CapProcessingGate MUST remain clear for an empty assignment")

	// 2) Non-empty assignment: gate wraps the new subject; bit MUST flip on.
	require.NoError(t, m.applyAssignmentWithPrev(
		Assignment{Version: 1, LeaderRevision: 1, Partitions: nil},
		Assignment{Version: 2, LeaderRevision: 1, Partitions: []Partition{{Keys: []string{"p1"}}}}))
	require.NotZero(t, m.Capabilities()&types.CapProcessingGate,
		"CapProcessingGate MUST be reported after the first non-empty apply")
}
