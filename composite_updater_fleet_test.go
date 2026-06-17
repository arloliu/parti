package parti

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type fleetChild struct {
	last int
}

func (f *fleetChild) UpdateWorkerConsumer(_ context.Context, _ string, _ []Partition) error {
	return nil
}
func (f *fleetChild) ObserveWorkerCount(n int) { f.last = n }

func TestComposite_ForwardsObserveWorkerCount(t *testing.T) {
	a, b := &fleetChild{}, &fleetChild{}
	c := NewCompositeConsumerUpdater(a, b)
	c.ObserveWorkerCount(7)
	require.Equal(t, 7, a.last)
	require.Equal(t, 7, b.last)
}

func TestComposite_ReplaysLastNToLateAddedChild(t *testing.T) {
	a := &fleetChild{}
	c := NewCompositeConsumerUpdater(a)
	c.ObserveWorkerCount(9)
	require.Equal(t, 9, a.last)

	late := &fleetChild{}
	c.Add(late)
	require.Equal(t, 9, late.last, "late-added child must receive the cached N")
}

func TestComposite_LateAddBeforeAnyObserve_NoReplay(t *testing.T) {
	c := NewCompositeConsumerUpdater()
	late := &fleetChild{last: -1}
	c.Add(late)
	require.Equal(t, -1, late.last, "no N observed yet -> no replay")
}
