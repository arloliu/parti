package assignment

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// getFailKV wraps a real KeyValue but forces every Get to fail with a fixed
// error, while Keys/Put/etc. delegate to the embedded bucket. Used to stage a
// connectivity-classed heartbeat-read failure without tearing down the cluster.
type getFailKV struct {
	jetstream.KeyValue
	getErr error
}

func (k *getFailKV) Get(_ context.Context, _ string) (jetstream.KeyValueEntry, error) {
	return nil, k.getErr
}

// putLabeledHeartbeat writes a v1 JSON heartbeat carrying the given labels
// under the "worker-hb" prefix newLabelCalc configures.
func putLabeledHeartbeat(t *testing.T, ctx context.Context, kv jetstream.KeyValue, workerID string, labels []string) {
	t.Helper()
	hb := types.Heartbeat{
		WorkerID:      workerID,
		SchemaVersion: 1,
		Capabilities:  types.CapAckV1,
		Labels:        labels,
		Timestamp:     time.Now().UTC(),
	}
	data, err := json.Marshal(hb)
	require.NoError(t, err)
	_, err = kv.Put(ctx, "worker-hb."+workerID, data)
	require.NoError(t, err)
}

// keysFailKV wraps a real KeyValue but fails every Keys scan with a fixed
// error while the toggle is on. Used to stage a connectivity-degraded worker
// enumeration (cache fallback) without tearing down the cluster.
type keysFailKV struct {
	jetstream.KeyValue
	fail   atomic.Bool
	keyErr error
}

func (k *keysFailKV) Keys(ctx context.Context, opts ...jetstream.WatchOpt) ([]string, error) {
	if k.fail.Load() {
		return nil, k.keyErr
	}

	return k.KeyValue.Keys(ctx, opts...)
}

// debugCapturingLogger records Debug-level entries (message + key/values) so a
// test can assert the requestLabelRecheck stub fired with a specific reason.
// Other levels are no-ops. Safe for concurrent use.
type debugCapturingLogger struct {
	mu      sync.Mutex
	entries []capturedDebugLog
}

type capturedDebugLog struct {
	msg string
	kv  []any
}

func (l *debugCapturingLogger) Debug(msg string, kv ...any) {
	l.mu.Lock()
	l.entries = append(l.entries, capturedDebugLog{msg: msg, kv: kv})
	l.mu.Unlock()
}
func (l *debugCapturingLogger) Info(string, ...any)  {}
func (l *debugCapturingLogger) Warn(string, ...any)  {}
func (l *debugCapturingLogger) Error(string, ...any) {}
func (l *debugCapturingLogger) Fatal(string, ...any) {}

// recheckRequested reports whether requestLabelRecheck fired with the given
// reason (the Task 8 stub logs "label recheck requested" at Debug).
func (l *debugCapturingLogger) recheckRequested(reason string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if e.msg != "label recheck requested" {
			continue
		}
		for i := 0; i+1 < len(e.kv); i += 2 {
			if e.kv[i] == "reason" && e.kv[i+1] == reason {
				return true
			}
		}
	}

	return false
}

type labelCalcConfig struct {
	source         types.PartitionSource
	heartbeatKV    jetstream.KeyValue
	assignmentKV   jetstream.KeyValue
	grace          time.Duration
	policy         string
	onBroadFailure func(err error)
	logger         types.Logger
}

// newLabelCalc builds a Calculator wired to the given KVs and source, NOT
// Start()ed — the label tests drive rebalance directly, mirroring the F10-A /
// F6-B fixtures, so no monitor goroutine races the explicit calls.
func newLabelCalc(t *testing.T, cfg labelCalcConfig) *Calculator {
	t.Helper()
	calc, err := NewCalculator(&Config{
		AssignmentKV:             cfg.assignmentKV,
		HeartbeatKV:              cfg.heartbeatKV,
		AssignmentPrefix:         "assignment",
		Source:                   cfg.source,
		Strategy:                 &mockStrategy{},
		HeartbeatPrefix:          "worker-hb",
		HeartbeatTTL:             30 * time.Second,
		EmergencyGracePeriod:     1 * time.Second,
		ColdStartWindow:          10 * time.Millisecond,
		PlannedScaleWindow:       10 * time.Millisecond,
		Cooldown:                 0,
		LabelSpillGrace:          cfg.grace,
		UnlabeledPartitionPolicy: cfg.policy,
		OnLabelReadBroadFailure:  cfg.onBroadFailure,
		Logger:                   cfg.logger,
	})
	require.NoError(t, err)

	return calc
}

// readCalcCommit fetches and decodes the assignment._commit key from a
// calculator-owned assignment bucket. Returns nil when absent.
func readCalcCommit(t *testing.T, ctx context.Context, kv jetstream.KeyValue) *types.AssignmentCommit {
	t.Helper()
	entry, err := kv.Get(ctx, "assignment._commit")
	if err != nil {
		require.ErrorIs(t, err, jetstream.ErrKeyNotFound)
		return nil
	}
	var c types.AssignmentCommit
	require.NoError(t, json.Unmarshal(entry.Value(), &c))

	return &c
}

// readCalcPayload fetches, decompresses, and decodes a payload by its key.
func readCalcPayload(t *testing.T, ctx context.Context, kv jetstream.KeyValue, key string) types.AssignmentPayload {
	t.Helper()
	entry, err := kv.Get(ctx, key)
	require.NoError(t, err)
	plain, err := gzipDecompress(entry.Value())
	require.NoError(t, err)
	var p types.AssignmentPayload
	require.NoError(t, json.Unmarshal(plain, &p))

	return p
}

// TestRebalance_LabelRouting_EndToEnd: 2 workers (w0 labels=[vip], w1
// unlabeled) with live heartbeats, source = 1 vip + 1 plain partition. After a
// rebalance the vip partition lands on w0 (with WorkerLabels=[vip], Known=true)
// and the plain partition on w1; nothing is parked.
func TestRebalance_LabelRouting_EndToEnd(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-route-asgn-"+t.Name())
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-route-hb-"+t.Name())

	putLabeledHeartbeat(t, ctx, hbKV, "w0", []string{"vip"})
	putLabeledHeartbeat(t, ctx, hbKV, "w1", nil)

	vipPart := types.Partition{Keys: []string{"v"}, Label: "vip"}
	plainPart := types.Partition{Keys: []string{"p"}}
	src := &mutableSource{partitions: []types.Partition{vipPart, plainPart}}

	calc := newLabelCalc(t, labelCalcConfig{source: src, heartbeatKV: hbKV, assignmentKV: asgnKV})

	require.NoError(t, calc.rebalance(ctx, "test"))

	commit := readCalcCommit(t, ctx, asgnKV)
	require.NotNil(t, commit)
	require.Equal(t, 0, commit.ParkedCount, "nothing parked when every label pool is populated")

	p0 := readCalcPayload(t, ctx, asgnKV, commit.Payloads["w0"].Key)
	require.Len(t, p0.Partitions, 1)
	require.Equal(t, "vip", p0.Partitions[0].Label, "w0 gets exactly the vip partition")
	require.Equal(t, []string{"vip"}, p0.WorkerLabels)
	require.True(t, p0.WorkerLabelsKnown)

	p1 := readCalcPayload(t, ctx, asgnKV, commit.Payloads["w1"].Key)
	require.Len(t, p1.Partitions, 1)
	require.Equal(t, "", p1.Partitions[0].Label, "w1 gets exactly the plain partition")
	require.Nil(t, p1.WorkerLabels, "unlabeled worker gets nil labels-of-record")
	require.True(t, p1.WorkerLabelsKnown)
}

// TestRebalance_EmptyPool_DeferThenPark: a "ghost"-labeled partition with no
// matching worker. Via handleRebalance the first attempt is a benign no-op (no
// commit, deferred pending confirmation); the second parks the ghost partition.
func TestRebalance_EmptyPool_DeferThenPark(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-park-asgn-"+t.Name())
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-park-hb-"+t.Name())

	putLabeledHeartbeat(t, ctx, hbKV, "w0", nil) // unlabeled worker only

	ghost := types.Partition{Keys: []string{"g"}, Label: "ghost"}
	src := &mutableSource{partitions: []types.Partition{ghost}}

	// Large grace so the confirmed-empty pool PARKS (not spills) on attempt 2.
	calc := newLabelCalc(t, labelCalcConfig{source: src, heartbeatKV: hbKV, assignmentKV: asgnKV, grace: time.Hour})

	// Attempt 1: first empty observation → defer. handleRebalance maps the
	// benign sentinel to nil, and nothing is published.
	require.NoError(t, calc.handleRebalance(ctx, "test"))
	require.Nil(t, readCalcCommit(t, ctx, asgnKV), "first (deferred) attempt must not publish")

	// Attempt 2: confirmed empty within grace → park the ghost partition.
	require.NoError(t, calc.handleRebalance(ctx, "test"))
	commit := readCalcCommit(t, ctx, asgnKV)
	require.NotNil(t, commit)
	require.Equal(t, 1, commit.ParkedCount, "the ghost partition is parked")

	p0 := readCalcPayload(t, ctx, asgnKV, commit.Payloads["w0"].Key)
	require.Empty(t, p0.Partitions, "the parked ghost partition appears in no worker payload")
}

// TestRebalance_EmptyPool_DeferReturnsSentinel pins that the first empty-pool
// observation surfaces errLabelObservationDeferred from rebalance itself.
func TestRebalance_EmptyPool_DeferReturnsSentinel(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-sentinel-asgn-"+t.Name())
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-sentinel-hb-"+t.Name())

	putLabeledHeartbeat(t, ctx, hbKV, "w0", nil)

	ghost := types.Partition{Keys: []string{"g"}, Label: "ghost"}
	src := &mutableSource{partitions: []types.Partition{ghost}}
	calc := newLabelCalc(t, labelCalcConfig{source: src, heartbeatKV: hbKV, assignmentKV: asgnKV, grace: time.Hour})

	require.ErrorIs(t, calc.rebalance(ctx, "test"), errLabelObservationDeferred)
}

// TestReadWorkerLabels_SmallFleetTaxonomy pins the §14 small-fleet
// classification: isolated per-worker misses stay unknown; broad failures
// (over the max(1,10%) cap, or ANY connectivity/degrading-class error) abort.
func TestReadWorkerLabels_SmallFleetTaxonomy(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)

	t.Run("1-of-3 isolated unknown", func(t *testing.T) {
		t.Parallel()
		asgnKV := partitest.CreateJetStreamKV(t, nc, "tax-a-asgn")
		hbKV := partitest.CreateJetStreamKV(t, nc, "tax-a-hb")
		putLabeledHeartbeat(t, ctx, hbKV, "w0", []string{"vip"})
		putLabeledHeartbeat(t, ctx, hbKV, "w1", nil)
		calc := newLabelCalc(t, labelCalcConfig{source: &mutableSource{}, heartbeatKV: hbKV, assignmentKV: asgnKV})

		labels, unknown, err := calc.readWorkerLabels(ctx, []string{"w0", "w1", "w2"})
		require.NoError(t, err)
		require.Equal(t, []string{"vip"}, labels["w0"])
		require.Contains(t, labels, "w1")
		require.Equal(t, map[string]bool{"w2": true}, unknown)
	})

	t.Run("1-of-2 isolated unknown (cap=max(1,10%)=1)", func(t *testing.T) {
		t.Parallel()
		asgnKV := partitest.CreateJetStreamKV(t, nc, "tax-b-asgn")
		hbKV := partitest.CreateJetStreamKV(t, nc, "tax-b-hb")
		putLabeledHeartbeat(t, ctx, hbKV, "w0", nil)
		calc := newLabelCalc(t, labelCalcConfig{source: &mutableSource{}, heartbeatKV: hbKV, assignmentKV: asgnKV})

		_, unknown, err := calc.readWorkerLabels(ctx, []string{"w0", "w1"})
		require.NoError(t, err)
		require.Equal(t, map[string]bool{"w1": true}, unknown)
	})

	t.Run("2-of-3 broad (over the count cap)", func(t *testing.T) {
		t.Parallel()
		asgnKV := partitest.CreateJetStreamKV(t, nc, "tax-c-asgn")
		hbKV := partitest.CreateJetStreamKV(t, nc, "tax-c-hb")
		putLabeledHeartbeat(t, ctx, hbKV, "w0", nil)
		calc := newLabelCalc(t, labelCalcConfig{source: &mutableSource{}, heartbeatKV: hbKV, assignmentKV: asgnKV})

		_, _, err := calc.readWorkerLabels(ctx, []string{"w0", "w1", "w2"})
		require.ErrorIs(t, err, errLabelReadBroadFailure)
	})

	t.Run("1-worker connectivity error (class beats count)", func(t *testing.T) {
		t.Parallel()
		asgnKV := partitest.CreateJetStreamKV(t, nc, "tax-d-asgn")
		hbKV := partitest.CreateJetStreamKV(t, nc, "tax-d-hb")
		failKV := &getFailKV{KeyValue: hbKV, getErr: nats.ErrTimeout}
		calc := newLabelCalc(t, labelCalcConfig{source: &mutableSource{}, heartbeatKV: failKV, assignmentKV: asgnKV})

		_, _, err := calc.readWorkerLabels(ctx, []string{"w0"})
		require.ErrorIs(t, err, errLabelReadBroadFailure,
			"a connectivity-classed error on the only worker must NOT be treated as an isolated unknown")
	})

	t.Run("1-of-3 malformed-present is unknown, not unlabeled", func(t *testing.T) {
		t.Parallel()
		asgnKV := partitest.CreateJetStreamKV(t, nc, "tax-e-asgn")
		hbKV := partitest.CreateJetStreamKV(t, nc, "tax-e-hb")
		putLabeledHeartbeat(t, ctx, hbKV, "w0", []string{"vip"})
		putLabeledHeartbeat(t, ctx, hbKV, "w1", nil)
		// w2's heartbeat KEY exists but the payload is unparseable: neither
		// JSON nor a legacy timestamp. Spec §6: unreadable labels are
		// unknown, never guessed — "unlabeled" would hand general work to
		// what may be a dedicated worker under the dedicated policy.
		_, err := hbKV.Put(ctx, "worker-hb.w2", []byte("not-a-heartbeat"))
		require.NoError(t, err)
		calc := newLabelCalc(t, labelCalcConfig{source: &mutableSource{}, heartbeatKV: hbKV, assignmentKV: asgnKV})

		labels, unknown, err := calc.readWorkerLabels(ctx, []string{"w0", "w1", "w2"})
		require.NoError(t, err, "one malformed worker of three is an isolated unknown, not broad")
		require.Equal(t, map[string]bool{"w2": true}, unknown,
			"a present-but-undecodeable heartbeat is UNKNOWN")
		require.NotContains(t, labels, "w2",
			"an unknown worker must not appear in the labels map as unlabeled")
	})

	t.Run("malformed plus missing crossing the cap is broad", func(t *testing.T) {
		t.Parallel()
		asgnKV := partitest.CreateJetStreamKV(t, nc, "tax-f-asgn")
		hbKV := partitest.CreateJetStreamKV(t, nc, "tax-f-hb")
		putLabeledHeartbeat(t, ctx, hbKV, "w0", nil)
		// w1 present-but-malformed, w2 absent: together 2 of 3 unreadable,
		// over the max(1, 3/10)=1 cap — malformed payloads count toward the
		// broad-failure cap exactly like missing keys.
		_, err := hbKV.Put(ctx, "worker-hb.w1", []byte("not-a-heartbeat"))
		require.NoError(t, err)
		calc := newLabelCalc(t, labelCalcConfig{source: &mutableSource{}, heartbeatKV: hbKV, assignmentKV: asgnKV})

		_, _, err = calc.readWorkerLabels(ctx, []string{"w0", "w1", "w2"})
		require.ErrorIs(t, err, errLabelReadBroadFailure,
			"malformed-present and absent workers must both count toward the broad cap")
	})
}

// TestRebalance_BroadLabelReadFailure_AbortsBeforeDecision: a broad heartbeat
// label-read failure aborts the rebalance before any label decision — no
// commit, labelState streaks untouched, and OnLabelReadBroadFailure fires once.
func TestRebalance_BroadLabelReadFailure_AbortsBeforeDecision(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-broad-asgn-"+t.Name())
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-broad-hb-"+t.Name())

	// A real key so the Keys()-based active-worker scan returns w0, but a Get
	// that always fails with a connectivity-classed error.
	putLabeledHeartbeat(t, ctx, hbKV, "w0", nil)
	failKV := &getFailKV{KeyValue: hbKV, getErr: nats.ErrTimeout}

	var broadFires atomic.Int64
	src := &mutableSource{partitions: []types.Partition{{Keys: []string{"v"}, Label: "vip"}}}
	calc := newLabelCalc(t, labelCalcConfig{
		source:         src,
		heartbeatKV:    failKV,
		assignmentKV:   asgnKV,
		onBroadFailure: func(error) { broadFires.Add(1) },
	})

	err := calc.rebalance(ctx, "test")
	require.ErrorIs(t, err, errLabelReadBroadFailure)
	require.Nil(t, readCalcCommit(t, ctx, asgnKV), "broad failure must abort before publishing")
	require.Equal(t, int64(1), broadFires.Load(), "OnLabelReadBroadFailure fires exactly once")
	require.Empty(t, calc.labelState.emptyStreak, "empty-pool streak must not advance on a broad-failure abort")
	require.Empty(t, calc.labelState.unknownStreak, "unknown-worker streak must not advance on a broad-failure abort")
}

// seedReplayFixture builds a 4-worker calculator (decodable heartbeats), runs
// one fresh seed rebalance so a commit + retained snapshot exist, then deletes
// 3 of the 4 heartbeat keys so the next enumeration is a suspicious shrink
// (1*100 < 4*50, counter 1 < ConfirmCount default 2) that degrades to the
// CACHED 4-worker list with fresh=false — the replay-or-defer path.
func seedReplayFixture(t *testing.T, ctx context.Context, src *mutableSource, logger types.Logger) (*Calculator, jetstream.KeyValue) {
	t.Helper()
	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-replay-asgn-"+t.Name())
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-replay-hb-"+t.Name())

	for _, w := range []string{"w0", "w1", "w2", "w3"} {
		putLabeledHeartbeat(t, ctx, hbKV, w, nil)
	}

	calc := newLabelCalc(t, labelCalcConfig{source: src, heartbeatKV: hbKV, assignmentKV: asgnKV, logger: logger})
	require.NoError(t, calc.rebalance(ctx, "seed"))
	require.NotNil(t, readCalcCommit(t, ctx, asgnKV), "seed rebalance must publish")

	for _, w := range []string{"w1", "w2", "w3"} {
		require.NoError(t, hbKV.Delete(ctx, "worker-hb."+w))
	}

	return calc, asgnKV
}

// TestRebalance_CachedObservation_ReplaySkipsPublish: cached observation +
// prior commit + content-unchanged source ⇒ replay is provable and the publish
// is skipped entirely — NoError, no new commit bytes, labelState untouched,
// and the label re-check was requested (belt-and-braces convergence).
func TestRebalance_CachedObservation_ReplaySkipsPublish(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	logger := &debugCapturingLogger{}
	src := &mutableSource{partitions: []types.Partition{{Keys: []string{"a"}}, {Keys: []string{"b"}}}}
	calc, asgnKV := seedReplayFixture(t, ctx, src, logger)

	before := readCalcCommit(t, ctx, asgnKV)
	require.NotNil(t, before)

	require.NoError(t, calc.rebalance(ctx, "test"),
		"a replay-provable cached observation is a successful no-op")

	after := readCalcCommit(t, ctx, asgnKV)
	require.NotNil(t, after)
	require.Equal(t, before.Version, after.Version, "replay must not publish a new commit")
	require.Equal(t, before.Payloads, after.Payloads, "replay must not write new payload bytes")
	require.Empty(t, calc.labelState.emptyStreak, "replay must not advance empty-pool streaks")
	require.Empty(t, calc.labelState.unknownStreak, "replay must not advance unknown-worker streaks")
	require.True(t, logger.recheckRequested("non_fresh_observation"),
		"every replay-skip must arm the label re-check so the system converges once conditions heal")
}

// TestRebalance_CachedObservation_SourceContentChanged_Defers: cached
// observation + a LABEL-ONLY source change since the last publish ⇒ replay is
// not provable (content equality is label-aware; the digests are deliberately
// label-blind and would miss this) ⇒ benign defer: no publish,
// requestLabelRecheck fired, and handleRebalance treats it as a no-op.
func TestRebalance_CachedObservation_SourceContentChanged_Defers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	logger := &debugCapturingLogger{}
	src := &mutableSource{partitions: []types.Partition{{Keys: []string{"a"}}, {Keys: []string{"b"}}}}
	calc, asgnKV := seedReplayFixture(t, ctx, src, logger)

	// Label-only edit: same count (partition guard silent), same label-blind
	// digest — only label-aware content equality can catch it.
	src.set([]types.Partition{{Keys: []string{"a"}}, {Keys: []string{"b"}, Label: "vip"}})

	before := readCalcCommit(t, ctx, asgnKV)
	require.NotNil(t, before)

	require.ErrorIs(t, calc.rebalance(ctx, "test"), errLabelObservationDeferred,
		"a cached observation with changed source content must defer, not compute new routing")
	require.NoError(t, calc.handleRebalance(ctx, "test"),
		"handleRebalance must treat the deferred outcome as benign")

	after := readCalcCommit(t, ctx, asgnKV)
	require.NotNil(t, after)
	require.Equal(t, before.Version, after.Version, "a deferred cached observation must not publish")
	require.True(t, logger.recheckRequested("non_fresh_observation"),
		"the defer must arm the label re-check")
}

// TestRebalance_CachedObservation_NoPriorCommit_Defers: a connectivity-degraded
// (cached) observation with NO prior commit has nothing to replay ⇒ benign
// defer, nothing published.
func TestRebalance_CachedObservation_NoPriorCommit_Defers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-nocommit-asgn-"+t.Name())
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-nocommit-hb-"+t.Name())

	putLabeledHeartbeat(t, ctx, hbKV, "w0", nil)
	putLabeledHeartbeat(t, ctx, hbKV, "w1", nil)
	failKV := &keysFailKV{KeyValue: hbKV, keyErr: nats.ErrTimeout}

	src := &mutableSource{partitions: []types.Partition{{Keys: []string{"a"}}}}
	calc := newLabelCalc(t, labelCalcConfig{source: src, heartbeatKV: failKV, assignmentKV: asgnKV})

	// Populate the worker cache with a healthy enumeration, then degrade the
	// scan so the rebalance observes the cached list with fresh=false.
	workers, fresh, err := calc.getActiveWorkers(ctx)
	require.NoError(t, err)
	require.True(t, fresh)
	require.Len(t, workers, 2)
	failKV.fail.Store(true)

	require.ErrorIs(t, calc.rebalance(ctx, "test"), errLabelObservationDeferred,
		"no prior commit means nothing to replay: the cached observation must defer")
	require.Nil(t, readCalcCommit(t, ctx, asgnKV), "the deferred rebalance must not publish")
}

// TestRebalance_EmergencyCarveOut_LabeledSurvivor pins the emergency carve-out
// at the label level: an emergency rebalance whose enumeration degraded to the
// cached list but whose removals are emergency-CONFIRMED runs the FULL label
// pipeline on the survivor set — the committed payload carries the survivor's
// real labels-of-record, proving the pipeline (not a fabricated stamp) ran.
func TestRebalance_EmergencyCarveOut_LabeledSurvivor(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-emerg-asgn-"+t.Name())
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-emerg-hb-"+t.Name())

	putLabeledHeartbeat(t, ctx, hbKV, "w0", []string{"vip"})
	for _, w := range []string{"w1", "w2", "w3"} {
		putLabeledHeartbeat(t, ctx, hbKV, w, nil)
	}

	vipPart := types.Partition{Keys: []string{"v"}, Label: "vip"}
	plainPart := types.Partition{Keys: []string{"p"}}
	src := &mutableSource{partitions: []types.Partition{vipPart, plainPart}}
	calc := newLabelCalc(t, labelCalcConfig{source: src, heartbeatKV: hbKV, assignmentKV: asgnKV})

	require.NoError(t, calc.rebalance(ctx, "seed"))
	seedCommit := readCalcCommit(t, ctx, asgnKV)
	require.NotNil(t, seedCommit)

	// w1..w3 die; EmergencyDetector confirms (simulated by populating the
	// buffer, mirroring the F10-A floor-release fixture). The next enumeration
	// is a suspicious 4→1 shrink that degrades to the cached 4-worker list;
	// the emergency filter strips the confirmed-dead → survivor set [w0],
	// fresh=false, deathsConfirmed=true.
	for _, w := range []string{"w1", "w2", "w3"} {
		require.NoError(t, hbKV.Delete(ctx, "worker-hb."+w))
	}
	calc.mu.Lock()
	calc.disappearedWorkers = []string{"w1", "w2", "w3"}
	calc.mu.Unlock()

	require.NoError(t, calc.rebalance(ctx, "emergency"),
		"emergency-confirmed deaths make the survivor set trusted: the rebalance must proceed and commit")

	commit := readCalcCommit(t, ctx, asgnKV)
	require.NotNil(t, commit)
	require.Greater(t, commit.Version, seedCommit.Version, "the emergency rebalance must publish a NEW commit")
	require.Contains(t, commit.Payloads, "w0")
	require.Len(t, commit.Payloads, 1, "only the survivor gets a payload")

	p0 := readCalcPayload(t, ctx, asgnKV, commit.Payloads["w0"].Key)
	require.Equal(t, []string{"vip"}, p0.WorkerLabels,
		"the carve-out must run the real label pipeline: labels-of-record come from a fresh heartbeat read, not a fabricated stamp")
	require.True(t, p0.WorkerLabelsKnown)
	require.Len(t, p0.Partitions, 2, "the survivor absorbs both partitions (vip pool + all-workers fallback)")
}

// vipInWorkerPayload reports whether the given worker's committed payload
// carries a vip-labeled partition.
func vipInWorkerPayload(t *testing.T, ctx context.Context, kv jetstream.KeyValue, commit *types.AssignmentCommit, workerID string) bool {
	t.Helper()
	ref, ok := commit.Payloads[workerID]
	if !ok {
		return false
	}
	p := readCalcPayload(t, ctx, kv, ref.Key)
	for _, part := range p.Partitions {
		if part.Label == "vip" {
			return true
		}
	}

	return false
}

// TestCalculator_TightTakeover_LabelChangeTriggersRebalance proves the
// label-change trigger drives a rebalance through requestLabelRecheck end to
// end. worker-0 heartbeats with ["vip"] and holds the one vip partition. A
// tight takeover keeps the SAME heartbeat key alive but swaps its payload to
// labels [] (a different process incarnation; the key never lapses, so the
// worker SET never changes).
//
// Two independent signals confirm the wiring:
//
//  1. requestLabelRecheck fires with reason "label_change" — the load-bearing,
//     wiring-specific proof. Only SetOnLabelChange → requestLabelRecheck
//     ("label_change") produces this reason; the generic recheck reasons
//     (observation_deferred / grace_expiry) do not. Without the calculator
//     wiring this reason never appears, so this assertion is what fails RED.
//  2. A NEW commit reassigns the vip partition off w0 (parked). A long
//     LabelSpillGrace keeps the park stable within the window (no spill-back
//     to the now-unlabeled w0), so the end-to-end outcome is deterministic.
func TestCalculator_TightTakeover_LabelChangeTriggersRebalance(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-takeover-asgn-"+t.Name())
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-takeover-hb-"+t.Name())

	putLabeledHeartbeat(t, ctx, hbKV, "w0", []string{"vip"})

	src := &mockWatchableSource{partitions: []types.Partition{{Keys: []string{"v"}, Label: "vip"}}}
	lg := &debugCapturingLogger{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:                asgnKV,
		HeartbeatKV:                 hbKV,
		AssignmentPrefix:            "assignment",
		HeartbeatPrefix:             "worker-hb",
		HeartbeatTTL:                30 * time.Second,
		Source:                      src,
		Strategy:                    &mockStrategy{},
		EmergencyGracePeriod:        5 * time.Second,
		Cooldown:                    0,
		ColdStartWindow:             10 * time.Millisecond,
		PlannedScaleWindow:          10 * time.Millisecond,
		LabelSpillGrace:             30 * time.Second, // long: the park stays put for the window
		RebalanceGraceDrainInterval: 75 * time.Millisecond,
		Logger:                      lg,
	})
	require.NoError(t, err)

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()

	// Initial rebalance: the vip partition lands on w0. Waiting for this commit
	// also guarantees the monitor's watcher has replayed w0's ["vip"] heartbeat
	// and seeded its label fingerprint before the takeover flips the labels.
	var initialVersion int64
	require.Eventually(t, func() bool {
		commit := readCalcCommit(t, ctx, asgnKV)
		if commit == nil || !vipInWorkerPayload(t, ctx, asgnKV, commit, "w0") {
			return false
		}
		initialVersion = commit.Version

		return true
	}, 3*time.Second, 25*time.Millisecond, "the vip partition must initially land on w0")

	// Tight takeover: the SAME heartbeat key stays alive but its labels flip to
	// [] — the worker SET never changes.
	putLabeledHeartbeat(t, ctx, hbKV, "w0", nil)

	// The label change must (1) route through requestLabelRecheck("label_change")
	// and (2) drive a NEW commit that removes the vip partition from w0.
	require.Eventually(t, func() bool {
		if !lg.recheckRequested("label_change") {
			return false
		}
		commit := readCalcCommit(t, ctx, asgnKV)

		return commit != nil && commit.Version > initialVersion &&
			!vipInWorkerPayload(t, ctx, asgnKV, commit, "w0")
	}, 5*time.Second, 25*time.Millisecond,
		"the label change must fire requestLabelRecheck(\"label_change\") and reassign the vip partition off w0")
}

// TestBroadLabelReadFailure_SuppressedDuringShutdown: a broad label-read
// failure induced by shutdown (stop channel closed, so the stop-aware
// rebalance context aborts the heartbeat reads) is NOT a KV failure and must
// not fire OnLabelReadBroadFailure — the manager routes that callback into
// the degraded circuit under m.mu, and Stop-time noise there both pollutes
// the degraded window and (before the stopCalculator lock restructure)
// closed a lock cycle that deadlocked Manager.Stop. The rebalance still
// aborts; only the callback is suppressed.
func TestBroadLabelReadFailure_SuppressedDuringShutdown(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-stop-suppress-asgn")
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-stop-suppress-hb")

	putLabeledHeartbeat(t, ctx, hbKV, "w0", nil)
	failKV := &getFailKV{KeyValue: hbKV, getErr: nats.ErrTimeout}

	var fires atomic.Int64
	src := &mutableSource{partitions: []types.Partition{{Keys: []string{"p"}}}}
	calc := newLabelCalc(t, labelCalcConfig{
		source:         src,
		heartbeatKV:    failKV,
		assignmentKV:   asgnKV,
		onBroadFailure: func(error) { fires.Add(1) },
	})

	// Simulate Stop having signalled: the stop channel is closed while a
	// rebalance is still deriving assignments.
	close(calc.stopCh)

	_, err := calc.deriveRebalanceAssignments(ctx, "test", []string{"w0"}, src.partitions, time.Now())
	require.Error(t, err, "the broad failure still aborts the rebalance")
	require.Zero(t, fires.Load(),
		"a shutdown-induced broad label-read failure must not enter the manager's degraded circuit")
}
