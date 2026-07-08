package assignment

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newLabelStateForTest(now func() time.Time, grace time.Duration) *labelState {
	return newLabelState(grace, now)
}

func TestLabelState_DeferOnceThenPark(t *testing.T) {
	t.Parallel()

	now := time.Now()
	clock := func() time.Time { return now }
	st := newLabelStateForTest(clock, time.Minute)

	// First observation of an empty pool for "vip": defer (spec §8.5).
	act, deferred := st.observeEmptyPools([]string{"vip"})
	require.True(t, deferred)
	require.Empty(t, act)

	// Second consecutive observation: act — inside grace ⇒ park.
	act, deferred = st.observeEmptyPools([]string{"vip"})
	require.False(t, deferred)
	require.Equal(t, emptyPoolPark, act["vip"])

	// emptySince started at the FIRST observation: advancing the clock
	// past grace-from-first flips to spill (confirmation does not extend
	// the grace window).
	now = now.Add(61 * time.Second)
	act, deferred = st.observeEmptyPools([]string{"vip"})
	require.False(t, deferred)
	require.Equal(t, emptyPoolSpill, act["vip"])
}

func TestLabelState_NonEmptyResets(t *testing.T) {
	t.Parallel()

	now := time.Now()
	st := newLabelStateForTest(func() time.Time { return now }, time.Minute)

	_, _ = st.observeEmptyPools([]string{"vip"})
	st.observeNonEmpty([]string{"vip"}) // pool recovered
	_, deferred := st.observeEmptyPools([]string{"vip"})
	require.True(t, deferred, "recovery resets the confirmation streak AND emptySince")
}

func TestLabelState_PruneRemovedLabels(t *testing.T) {
	t.Parallel()

	now := time.Now()
	st := newLabelStateForTest(func() time.Time { return now }, time.Minute)
	_, _ = st.observeEmptyPools([]string{"vip"})
	st.prune(map[string]bool{}) // "vip" no longer in the snapshot
	require.Empty(t, st.emptySince, "stale grace clocks must not leak")
}

func TestLabelState_ZeroGraceSpillsImmediatelyAfterConfirmation(t *testing.T) {
	t.Parallel()

	now := time.Now()
	st := newLabelStateForTest(func() time.Time { return now }, 0)
	_, deferred := st.observeEmptyPools([]string{"vip"})
	require.True(t, deferred, "confirmation still applies at grace=0")
	act, _ := st.observeEmptyPools([]string{"vip"})
	require.Equal(t, emptyPoolSpill, act["vip"], "grace 0 = spill as soon as confirmed")
}

func TestLabelState_UnknownWorkerDeferThenAct(t *testing.T) {
	t.Parallel()

	st := newLabelStateForTest(time.Now, time.Minute)
	require.True(t, st.observeUnknownWorkers([]string{"w2"}), "first: defer")
	require.False(t, st.observeUnknownWorkers([]string{"w2"}), "second consecutive: act")
	st.observeUnknownWorkers(nil) // successful read resets
	require.True(t, st.observeUnknownWorkers([]string{"w2"}), "reset after recovery")
}
