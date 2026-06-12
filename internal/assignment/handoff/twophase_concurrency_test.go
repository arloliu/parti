package handoff

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// observingClaimStore wraps the package-local memStore and instruments
// PutIfEpoch so tests can observe in-flight concurrency.
type observingClaimStore struct {
	inner    *memStore
	inFlight atomic.Int32
	peak     atomic.Int32
	holdFor  time.Duration
}

func newObservingClaimStore(hold time.Duration) *observingClaimStore {
	return &observingClaimStore{inner: newMemStore(), holdFor: hold}
}

func (o *observingClaimStore) Get(ctx context.Context, partitionID string) (Claim, uint64, error) {
	return o.inner.Get(ctx, partitionID)
}

func (o *observingClaimStore) PutIfEpoch(
	ctx context.Context, partitionID string, expectedEpoch int64, next Claim,
) (uint64, error) {
	cur := o.inFlight.Add(1)
	defer o.inFlight.Add(-1)
	for {
		old := o.peak.Load()
		if cur <= old || o.peak.CompareAndSwap(old, cur) {
			break
		}
	}
	if o.holdFor > 0 {
		time.Sleep(o.holdFor)
	}

	return o.inner.PutIfEpoch(ctx, partitionID, expectedEpoch, next)
}

func (o *observingClaimStore) ListKeys(ctx context.Context) ([]string, error) {
	return o.inner.ListKeys(ctx)
}

func (o *observingClaimStore) Delete(ctx context.Context, partitionID string, revision uint64) error {
	return o.inner.Delete(ctx, partitionID, revision)
}

// compile-time assertion
var _ ClaimStore = (*observingClaimStore)(nil)

// TestTwoPhase_PhaseConcurrency_HonorsLimit verifies that setting
// PhaseConcurrency=N causes preparePhase to run at most N in-flight
// updateClaim calls at any instant.
func TestTwoPhase_PhaseConcurrency_HonorsLimit(t *testing.T) {
	const partitions = 50
	const limit = 5

	store := newObservingClaimStore(10 * time.Millisecond)

	coord := New(Config{
		Store:            store,
		TTL:              1 * time.Minute,
		PhaseConcurrency: limit,
	}, true)

	parts := make([]types.Partition, partitions)
	for i := range parts {
		parts[i] = types.Partition{Keys: []string{fmt.Sprintf("p%d", i)}}
	}

	err := coord.Apply(
		context.Background(),
		"worker-1",
		types.Assignment{},
		types.Assignment{Partitions: parts, Version: 1},
	)
	require.NoError(t, err)
	require.LessOrEqual(t, store.peak.Load(), int32(limit), "peak in-flight exceeded limit")
}

// TestTwoPhase_PhaseConcurrency_DefaultsTo20 proves that zero
// PhaseConcurrency is normalized to 20 by handoff.New. If normalization
// is bypassed, errgroup.SetLimit(0) prevents new goroutines from being
// added and the Apply call would hang.
func TestTwoPhase_PhaseConcurrency_DefaultsTo20(t *testing.T) {
	const partitions = 50

	store := newObservingClaimStore(10 * time.Millisecond)

	// PhaseConcurrency omitted — sentinel 0; New must normalize to 20.
	coord := New(Config{
		Store: store,
		TTL:   1 * time.Minute,
	}, true)

	parts := make([]types.Partition, partitions)
	for i := range parts {
		parts[i] = types.Partition{Keys: []string{fmt.Sprintf("p%d", i)}}
	}

	err := coord.Apply(
		context.Background(),
		"worker-1",
		types.Assignment{},
		types.Assignment{Partitions: parts, Version: 1},
	)
	require.NoError(t, err)
	require.LessOrEqual(t, store.peak.Load(), int32(20), "peak in-flight exceeded default 20")
	require.Greater(t, store.peak.Load(), int32(1), "default must be parallel, not serial")
}

// TestTwoPhase_PhaseConcurrency_OneIsSerial proves the operator contract:
// PhaseConcurrency=1 means one in-flight per phase, ever.
func TestTwoPhase_PhaseConcurrency_OneIsSerial(t *testing.T) {
	const partitions = 20

	store := newObservingClaimStore(5 * time.Millisecond)

	coord := New(Config{
		Store:            store,
		TTL:              1 * time.Minute,
		PhaseConcurrency: 1,
	}, true)

	parts := make([]types.Partition, partitions)
	for i := range parts {
		parts[i] = types.Partition{Keys: []string{fmt.Sprintf("p%d", i)}}
	}

	err := coord.Apply(
		context.Background(),
		"worker-1",
		types.Assignment{},
		types.Assignment{Partitions: parts, Version: 1},
	)
	require.NoError(t, err)
	require.Equal(t, int32(1), store.peak.Load(), "PhaseConcurrency=1 must be strictly serial")
}
