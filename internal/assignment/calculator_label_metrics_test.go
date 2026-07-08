package assignment

import (
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// labelMetricEvent captures a single LabelMetrics call. label is "" for
// events that don't carry one (IncrementUnlabeledFallback).
type labelMetricEvent struct {
	method string // "pool_size" | "parked" | "spill" | "unlabeled_fallback"
	label  string
	value  int // workers/count for gauge events; unused for counter events
}

// fakeLabelMetrics records every LabelMetrics call in order so a test can
// assert both the values recorded and the exact sequence (e.g. proving a
// zeroing pass happened exactly once and nothing "leaked" afterward). Embeds
// NopMetrics so the full CalculatorAndAssignmentMetrics surface is satisfied
// without listing every method, mirroring auditRecordingMetrics.
type fakeLabelMetrics struct {
	*metrics.NopMetrics

	mu     sync.Mutex
	events []labelMetricEvent
}

func newFakeLabelMetrics() *fakeLabelMetrics {
	return &fakeLabelMetrics{NopMetrics: metrics.NewNop()}
}

func (m *fakeLabelMetrics) RecordLabelPoolSize(label string, workers int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, labelMetricEvent{method: "pool_size", label: label, value: workers})
}

func (m *fakeLabelMetrics) RecordParkedPartitions(label string, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, labelMetricEvent{method: "parked", label: label, value: count})
}

func (m *fakeLabelMetrics) IncrementLabelSpill(label string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, labelMetricEvent{method: "spill", label: label})
}

func (m *fakeLabelMetrics) IncrementUnlabeledFallback() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, labelMetricEvent{method: "unlabeled_fallback"})
}

func (m *fakeLabelMetrics) snapshot() []labelMetricEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]labelMetricEvent, len(m.events))
	copy(out, m.events)

	return out
}

// vipEvents filters recorded events down to the "vip" label (the label every
// sequence-asserting test in this file keys on), preserving order.
func vipEvents(events []labelMetricEvent) []labelMetricEvent {
	var out []labelMetricEvent
	for _, e := range events {
		if e.label == "vip" {
			out = append(out, e)
		}
	}

	return out
}

// countEvents counts recorded events matching method+label — for asserting
// EXACT counter cardinality (the counters are per-PARTITION, not
// per-rebalance).
func countEvents(events []labelMetricEvent, method, label string) int {
	n := 0
	for _, e := range events {
		if e.method == method && e.label == label {
			n++
		}
	}

	return n
}

// newLabelCalcWithMetrics builds a Calculator wired to the given KVs/source,
// mirroring newLabelCalc, but with a caller-supplied metrics collector —
// newLabelCalc always defaults to a no-op collector, so tests asserting on
// LabelMetrics calls need this variant instead of widening the shared
// labelCalcConfig used by every other label test in the package.
func newLabelCalcWithMetrics(t *testing.T, cfg labelCalcConfig, m CalculatorAndAssignmentMetrics) *Calculator {
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
		Metrics:                  m,
	})
	require.NoError(t, err)

	return calc
}

// TestCalculator_LabelMetrics_GaugeLifecycle pins the §13 gauge lifecycle
// contract end to end: per-label gauges are recomputed on every completed
// (published) rebalance, and a label that leaves the snapshot is explicitly
// zeroed in the SAME pass rather than left at its last non-zero reading.
//
// Pass 1: source has a vip partition + a vip-labeled worker → pool_size=1,
// parked=0 for "vip".
// Pass 2: source rewritten WITHOUT any vip partition (the vip worker's
// heartbeat is untouched) → the vip label leaves topo.SortedLabels entirely,
// so the zeroing branch (not the normal per-label branch) must emit
// pool_size=0 and parked=0 for "vip", and nothing else references "vip"
// afterward.
// Pass 3: vip still absent → NO further vip records at all — the zeroing
// happens exactly once (on departure), not once per subsequent rebalance.
func TestCalculator_LabelMetrics_GaugeLifecycle(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-metrics-asgn-"+t.Name())
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-metrics-hb-"+t.Name())

	putLabeledHeartbeat(t, ctx, hbKV, "w0", []string{"vip"})
	putLabeledHeartbeat(t, ctx, hbKV, "w1", nil)

	vipPart := types.Partition{Keys: []string{"v"}, Label: "vip"}
	plainPart := types.Partition{Keys: []string{"p"}}
	src := &mutableSource{partitions: []types.Partition{vipPart, plainPart}}

	fm := newFakeLabelMetrics()
	calc, err := NewCalculator(&Config{
		AssignmentKV:         asgnKV,
		HeartbeatKV:          hbKV,
		AssignmentPrefix:     "assignment",
		Source:               src,
		Strategy:             &mockStrategy{},
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         30 * time.Second,
		EmergencyGracePeriod: 1 * time.Second,
		ColdStartWindow:      10 * time.Millisecond,
		PlannedScaleWindow:   10 * time.Millisecond,
		Cooldown:             0,
		Metrics:              fm,
	})
	require.NoError(t, err)

	// Pass 1: vip present.
	require.NoError(t, calc.rebalance(ctx, "test"))
	require.NotNil(t, readCalcCommit(t, ctx, asgnKV))

	vipAfterPass1 := vipEvents(fm.snapshot())
	require.Equal(t, []labelMetricEvent{
		{method: "pool_size", label: "vip", value: 1},
		{method: "parked", label: "vip", value: 0},
	}, vipAfterPass1, "pass 1 must record the vip pool size and zero parked count")

	// Pass 2: source rewritten WITHOUT any vip partition.
	src.set([]types.Partition{plainPart})
	require.NoError(t, calc.rebalance(ctx, "test"))
	require.NotNil(t, readCalcCommit(t, ctx, asgnKV))

	vipAfterPass2 := vipEvents(fm.snapshot())
	require.Equal(t, []labelMetricEvent{
		{method: "pool_size", label: "vip", value: 1},
		{method: "parked", label: "vip", value: 0},
		{method: "pool_size", label: "vip", value: 0},
		{method: "parked", label: "vip", value: 0},
	}, vipAfterPass2, "pass 2 must zero the vip gauges exactly once, in the same pass, and record nothing further for vip")

	// Pass 3: vip still absent. The zeroing must NOT repeat — a departed
	// label records nothing on subsequent rebalances.
	require.NoError(t, calc.rebalance(ctx, "test"))
	vipAfterPass3 := vipEvents(fm.snapshot())
	require.Equal(t, vipAfterPass2, vipAfterPass3,
		"pass 3 must record NO further vip events: a departed label is zeroed once, then silent")
}

// TestCalculator_LabelMetrics_SpillIncrementsOnAppliedSpill pins that
// IncrementLabelSpill fires ONCE PER PARTITION of a label whose empty-pool
// decision was emptyPoolSpill in the PUBLISHED rebalance — derived from the
// same actions the publish applied, not recomputed. Two ghost partitions
// spill together, so the counter must read exactly 2, not 1-per-rebalance.
func TestCalculator_LabelMetrics_SpillIncrementsOnAppliedSpill(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-spill-asgn-"+t.Name())
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-spill-hb-"+t.Name())

	putLabeledHeartbeat(t, ctx, hbKV, "w0", nil) // unlabeled worker only

	ghostA := types.Partition{Keys: []string{"g1"}, Label: "ghost"}
	ghostB := types.Partition{Keys: []string{"g2"}, Label: "ghost"}
	src := &mutableSource{partitions: []types.Partition{ghostA, ghostB}}

	fm := newFakeLabelMetrics()
	// Zero grace: the second confirmed-empty observation spills immediately
	// rather than parking.
	calc := newLabelCalcWithMetrics(t, labelCalcConfig{source: src, heartbeatKV: hbKV, assignmentKV: asgnKV}, fm)

	// Attempt 1: first empty observation defers; nothing published, nothing
	// recorded.
	require.NoError(t, calc.handleRebalance(ctx, "test"))
	require.Nil(t, readCalcCommit(t, ctx, asgnKV))
	require.Empty(t, fm.snapshot(), "a deferred (unpublished) rebalance must record nothing")

	// Attempt 2: confirmed empty within a zero grace window → spill BOTH.
	require.NoError(t, calc.handleRebalance(ctx, "test"))
	commit := readCalcCommit(t, ctx, asgnKV)
	require.NotNil(t, commit)
	require.Equal(t, 0, commit.ParkedCount, "the ghost partitions spill onto the fallback worker, not parked")

	events := fm.snapshot()
	require.Equal(t, 2, countEvents(events, "spill", "ghost"),
		"IncrementLabelSpill is per-PARTITION: 2 spilled ghost partitions = exactly 2 increments")
	require.NotContains(t, events, labelMetricEvent{method: "parked", label: "ghost", value: 2},
		"a spilled label must not also read back as parked")
}

// TestCalculator_LabelMetrics_UnlabeledFallbackIncrements pins that
// IncrementUnlabeledFallback fires ONCE PER PARTITION of the unlabeled group
// when its general/fallback pool is empty (every active worker is labeled)
// and the group is non-empty, forcing the ladder down to AllWorkers. Two
// unlabeled partitions fall back together, so the counter must read exactly
// 2, not 1-per-rebalance.
func TestCalculator_LabelMetrics_UnlabeledFallbackIncrements(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-fallback-asgn-"+t.Name())
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-fallback-hb-"+t.Name())

	putLabeledHeartbeat(t, ctx, hbKV, "w0", []string{"vip"}) // the ONLY worker, and it's labeled

	vipPart := types.Partition{Keys: []string{"v"}, Label: "vip"}
	plainA := types.Partition{Keys: []string{"p1"}} // unlabeled group has TWO partitions
	plainB := types.Partition{Keys: []string{"p2"}}
	src := &mutableSource{partitions: []types.Partition{vipPart, plainA, plainB}}

	fm := newFakeLabelMetrics()
	calc := newLabelCalcWithMetrics(t, labelCalcConfig{source: src, heartbeatKV: hbKV, assignmentKV: asgnKV}, fm)

	require.NoError(t, calc.rebalance(ctx, "test"))
	require.NotNil(t, readCalcCommit(t, ctx, asgnKV))

	events := fm.snapshot()
	require.Equal(t, 2, countEvents(events, "unlabeled_fallback", ""),
		"IncrementUnlabeledFallback is per-PARTITION: 2 fallback-routed unlabeled partitions = exactly 2 increments")
}

// TestCalculator_LabelMetrics_StopZeroesGauges pins the leader-scoped gauge
// lifecycle across leader terms: the per-label gauges live only while THIS
// worker is the calculating leader, so the calculator's stop path must zero
// every label it last recorded (and clear its tracking state). Without this,
// a deposed leader's metrics export freezes at the last recorded values and
// a label that departs while another leader calculates is never zeroed here.
//
// Start-driven (not direct-rebalance-driven) so Stop exercises the real
// lifecycle: Start → initial rebalance records vip gauges → Stop zeroes them.
func TestCalculator_LabelMetrics_StopZeroesGauges(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-stopzero-asgn-"+t.Name())
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-stopzero-hb-"+t.Name())

	putLabeledHeartbeat(t, ctx, hbKV, "w0", []string{"vip"})

	src := &mutableSource{partitions: []types.Partition{{Keys: []string{"v"}, Label: "vip"}}}
	fm := newFakeLabelMetrics()
	calc := newLabelCalcWithMetrics(t, labelCalcConfig{source: src, heartbeatKV: hbKV, assignmentKV: asgnKV}, fm)

	require.NoError(t, calc.Start(ctx))
	stopped := false
	defer func() {
		if !stopped {
			_ = calc.Stop(ctx)
		}
	}()

	// Wait for the initial rebalance to record the vip gauges.
	require.Eventually(t, func() bool {
		return countEvents(fm.snapshot(), "pool_size", "vip") > 0
	}, 5*time.Second, 25*time.Millisecond, "the initial rebalance must record the vip pool gauge")

	require.NoError(t, calc.Stop(ctx))
	stopped = true

	events := vipEvents(fm.snapshot())
	require.GreaterOrEqual(t, len(events), 4, "expected at least one recording pass plus the stop-zeroing pass")
	require.Equal(t, []labelMetricEvent{
		{method: "pool_size", label: "vip", value: 0},
		{method: "parked", label: "vip", value: 0},
	}, events[len(events)-2:], "Stop must zero both vip gauges as its final records")
	require.Contains(t, events[:len(events)-2], labelMetricEvent{method: "pool_size", label: "vip", value: 1},
		"a non-zero pool gauge must have been recorded before Stop zeroed it")
}
