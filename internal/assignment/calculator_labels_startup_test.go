package assignment

import (
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// These tests pin the fix for the takeover/startup deferred-label defect found
// by the Task 14 pool-outage integration test
// (test/integration/assignment/label_orphan_stale_test.go,
// TestLabelPoolOutage_ParkSpillRehome): the synchronous immediate-assignment
// path in Calculator.Start treated the BENIGN errLabelObservationDeferred
// sentinel as fatal. On leadership takeover that failed startCalculator, which
// released leadership; the next leader built a fresh per-term labelState
// (spec §8.4), deferred again, and released — an infinite leadership flap in
// which the defer-once streak never reached 2 and parked partitions were never
// committed (spec I2 violation). Deferral is benign EVERYWHERE (spec §8.5):
// the previous commit stays valid, and the re-check armed inside the rebalance
// drives the confirming second observation.

// TestStart_TakeoverImmediate_DeferredLabelObservation_Benign: a new leader
// takes over (prior commit exists) exactly when a labeled pool is empty. The
// takeover_immediate rebalance defers (first adverse observation of a fresh
// labelState) — Start must SUCCEED without publishing, and the armed re-check
// must drive the confirming second observation that publishes the parked
// commit.
func TestStart_TakeoverImmediate_DeferredLabelObservation_Benign(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-takeover-defer-asgn")
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-takeover-defer-hb")

	putLabeledHeartbeat(t, ctx, hbKV, "w0", nil)

	plain := types.Partition{Keys: []string{"p"}}
	ghost := types.Partition{Keys: []string{"g"}, Label: "ghost"}
	src := &mutableSource{partitions: []types.Partition{plain}}

	// Previous leader publishes a commit, then dies (never Started, so its
	// assignment keys survive for the takeover's version discovery).
	prev := newLabelCalc(t, labelCalcConfig{source: src, heartbeatKV: hbKV, assignmentKV: asgnKV, grace: time.Hour})
	require.NoError(t, prev.rebalance(ctx, "seed"))
	seed := readCalcCommit(t, ctx, asgnKV)
	require.NotNil(t, seed)

	// The ghost pool drains coincident with the takeover: the new leader's
	// FRESH labelState sees the empty pool for the first time.
	src.set([]types.Partition{plain, ghost})

	calc := newLabelCalc(t, labelCalcConfig{source: src, heartbeatKV: hbKV, assignmentKV: asgnKV, grace: time.Hour})
	require.NoError(t, calc.Start(ctx),
		"a deferred label observation during takeover_immediate is benign and must not fail Start "+
			"(failing releases leadership and livelocks the fleet: every new leader defers its first observation)")
	defer func() { _ = calc.Stop(ctx) }()

	// The confirming SECOND observation (label re-check / stabilization
	// rebalance) publishes the parked commit; inside grace ⇒ park.
	require.Eventually(t, func() bool {
		c := readCalcCommit(t, ctx, asgnKV)
		return c != nil && c.Version > seed.Version && c.ParkedCount == 1
	}, 5*time.Second, 20*time.Millisecond,
		"the armed re-check must confirm the empty pool and publish the parked commit")
}

// TestStart_ColdStartImmediate_DeferredLabelObservation_Benign: same defect
// class on the cold-start path — no prior commit, a ghost-labeled partition is
// present at T=0. The cold_start_immediate rebalance defers; Manager.Start
// must not fail, and the confirming observation publishes the parked commit.
func TestStart_ColdStartImmediate_DeferredLabelObservation_Benign(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-coldstart-defer-asgn")
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-coldstart-defer-hb")

	putLabeledHeartbeat(t, ctx, hbKV, "w0", nil)

	src := &mutableSource{partitions: []types.Partition{
		{Keys: []string{"p"}},
		{Keys: []string{"g"}, Label: "ghost"},
	}}

	calc := newLabelCalc(t, labelCalcConfig{source: src, heartbeatKV: hbKV, assignmentKV: asgnKV, grace: time.Hour})
	require.NoError(t, calc.Start(ctx),
		"a deferred label observation during cold_start_immediate is benign and must not fail Start")
	defer func() { _ = calc.Stop(ctx) }()

	require.Eventually(t, func() bool {
		c := readCalcCommit(t, ctx, asgnKV)
		return c != nil && c.ParkedCount == 1
	}, 5*time.Second, 20*time.Millisecond,
		"the confirming observation must publish the first commit with the ghost partition parked")
}

// TestTriggerRebalance_DeferredLabelObservation_Benign: the manual-refresh
// path (Manager.TriggerRebalance → Calculator.TriggerRebalance) is the same
// sentinel-consumer class. A deferral there is success-without-publish, not an
// error surfaced to the API caller.
func TestTriggerRebalance_DeferredLabelObservation_Benign(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, "lbl-trigger-defer-asgn")
	hbKV := partitest.CreateJetStreamKV(t, nc, "lbl-trigger-defer-hb")

	putLabeledHeartbeat(t, ctx, hbKV, "w0", nil)

	plain := types.Partition{Keys: []string{"p"}}
	src := &mutableSource{partitions: []types.Partition{plain}}

	calc := newLabelCalc(t, labelCalcConfig{source: src, heartbeatKV: hbKV, assignmentKV: asgnKV, grace: time.Hour})
	require.NoError(t, calc.Start(ctx), "healthy cold start must succeed")
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for the startup flow (immediate + stabilization rebalance) to
	// settle so the manual trigger below is deterministically the FIRST
	// observation of the ghost pool.
	require.Eventually(t, func() bool {
		return readCalcCommit(t, ctx, asgnKV) != nil
	}, 5*time.Second, 20*time.Millisecond, "startup must publish the healthy commit")

	src.set([]types.Partition{plain, {Keys: []string{"g"}, Label: "ghost"}})

	require.NoError(t, calc.TriggerRebalance(ctx),
		"a deferred label observation on manual refresh is benign: success-without-publish, "+
			"the armed re-check owns the confirming retry")
}
