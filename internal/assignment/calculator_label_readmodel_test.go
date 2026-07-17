package assignment

import (
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// TestCalculator_LabelSnapshot_RetainedAfterPublish pins the pull-side
// (Manager.LabelState) retention contract across the publish lifecycle:
//
// Phase A: before any publish the snapshot is absent; the first published
// rebalance retains pool sizes and parked counts with exact key parity to
// the gauge pass (labeled pools only, explicit zeros).
// Phase B: a deferred (unpublished) rebalance — the first observation of a
// newly-empty pool — must NOT touch the retained snapshot.
// Phase C: the confirming rebalance publishes with the gold partitions
// parked and the snapshot updates, including a zero-size pool key.
//
// The calculator here is wired with the DEFAULT no-op-collector path
// (newLabelCalc): retention must work precisely when no LabelMetrics
// collector is configured — that deployment is the accessor's whole purpose.
func TestCalculator_LabelSnapshot_RetainedAfterPublish(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-readmodel-asgn-"+t.Name())
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-readmodel-hb-"+t.Name())

	putLabeledHeartbeat(t, ctx, hbKV, "w0", []string{"vip"})
	putLabeledHeartbeat(t, ctx, hbKV, "w1", nil)

	vipPart := types.Partition{Keys: []string{"v"}, Label: "vip"}
	plainPart := types.Partition{Keys: []string{"p"}}
	src := &mutableSource{partitions: []types.Partition{vipPart, plainPart}}

	// Long grace: a confirmed-empty pool PARKS its partitions instead of
	// spilling, which is the state the accessor exists to expose.
	calc := newLabelCalc(t, labelCalcConfig{source: src, heartbeatKV: hbKV, assignmentKV: asgnKV, grace: time.Hour})

	// Phase A: nothing published yet → no snapshot.
	_, _, ok := calc.LabelSnapshot()
	require.False(t, ok, "a calculator that never published must have no label snapshot")

	require.NoError(t, calc.rebalance(ctx, "test"))
	require.NotNil(t, readCalcCommit(t, ctx, asgnKV))

	pools, parked, ok := calc.LabelSnapshot()
	require.True(t, ok)
	require.Equal(t, map[string]int{"vip": 1}, pools,
		"key parity with the gauge pass: labeled pools only — the unlabeled general pool is never a key")
	require.Equal(t, map[string]int{"vip": 0}, parked,
		"a label with nothing parked is present with an explicit 0")

	// Phase B: add gold partitions with NO gold-labeled worker. The first
	// empty-pool observation defers (nothing published) — the retained
	// snapshot must be byte-identical to phase A, with no gold key.
	goldA := types.Partition{Keys: []string{"g1"}, Label: "gold"}
	goldB := types.Partition{Keys: []string{"g2"}, Label: "gold"}
	src.set([]types.Partition{vipPart, plainPart, goldA, goldB})

	require.NoError(t, calc.handleRebalance(ctx, "test"))
	pools, parked, ok = calc.LabelSnapshot()
	require.True(t, ok)
	require.Equal(t, map[string]int{"vip": 1}, pools, "a deferred (unpublished) rebalance must not touch the snapshot")
	require.Equal(t, map[string]int{"vip": 0}, parked, "a deferred (unpublished) rebalance must not touch the snapshot")

	// Phase C: the confirming rebalance publishes with both gold partitions
	// parked (grace window still open) and the snapshot updates.
	require.NoError(t, calc.handleRebalance(ctx, "test"))
	commit := readCalcCommit(t, ctx, asgnKV)
	require.NotNil(t, commit)
	require.Equal(t, 2, commit.ParkedCount, "both gold partitions must be parked, not spilled, inside the grace window")

	pools, parked, ok = calc.LabelSnapshot()
	require.True(t, ok)
	require.Equal(t, map[string]int{"vip": 1, "gold": 0}, pools, "an empty pool is a key with an explicit 0 size")
	require.Equal(t, map[string]int{"vip": 0, "gold": 2}, parked)

	// Copy semantics: mutating the returned maps must not affect what the
	// next call observes.
	pools["vip"] = 99
	delete(parked, "gold")
	pools2, parked2, ok := calc.LabelSnapshot()
	require.True(t, ok)
	require.Equal(t, map[string]int{"vip": 1, "gold": 0}, pools2, "returned maps are copies; caller mutation must not leak back")
	require.Equal(t, map[string]int{"vip": 0, "gold": 2}, parked2, "returned maps are copies; caller mutation must not leak back")
}

// TestCalculator_LabelSnapshot_StopClears pins the leader-scoped lifecycle:
// the snapshot lives only while THIS worker is the calculating leader, so
// Stop must clear it — a deposed leader must not keep serving stale label
// state through Manager.LabelState. Start-driven so Stop exercises the real
// teardown ordering (mirrors TestCalculator_LabelMetrics_StopZeroesGauges).
func TestCalculator_LabelSnapshot_StopClears(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-rmstop-asgn-"+t.Name())
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-rmstop-hb-"+t.Name())

	putLabeledHeartbeat(t, ctx, hbKV, "w0", []string{"vip"})

	src := &mutableSource{partitions: []types.Partition{{Keys: []string{"v"}, Label: "vip"}}}
	calc := newLabelCalc(t, labelCalcConfig{source: src, heartbeatKV: hbKV, assignmentKV: asgnKV})

	require.NoError(t, calc.Start(ctx))
	stopped := false
	defer func() {
		if !stopped {
			_ = calc.Stop(ctx)
		}
	}()

	require.Eventually(t, func() bool {
		_, _, ok := calc.LabelSnapshot()
		return ok
	}, 5*time.Second, 25*time.Millisecond, "the initial rebalance must retain a label snapshot")

	require.NoError(t, calc.Stop(ctx))
	stopped = true

	_, _, ok := calc.LabelSnapshot()
	require.False(t, ok, "Stop must clear the retained snapshot: a deposed leader serves no label state")
}
