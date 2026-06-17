package consumer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWithConsumerCreateClusterRate_RequiresPerWorker(t *testing.T) {
	_, err := buildDynamic(t, WithConsumerCreateClusterRate(1000)) // no WithConsumerCreateRate
	require.Error(t, err)
	require.Contains(t, err.Error(), "WithConsumerCreateClusterRate")
}

func TestWithConsumerCreateClusterRate_RejectedWithInjectedLimiter(t *testing.T) {
	inj, err := NewConsumerCreateLimiter(50, 5)
	require.NoError(t, err)
	_, err = buildDynamic(t,
		WithConsumerCreateLimiter(inj),
		WithConsumerCreateClusterRate(1000),
	)
	require.Error(t, err)
}

func TestWithConsumerCreateClusterRate_NegativeRejected(t *testing.T) {
	t.Run("with per-worker rate", func(t *testing.T) {
		_, err := buildDynamic(t, WithConsumerCreateRate(200, 50), WithConsumerCreateClusterRate(-1))
		require.Error(t, err)
	})
	t.Run("alone (no per-worker rate)", func(t *testing.T) {
		_, err := buildDynamic(t, WithConsumerCreateClusterRate(-1))
		require.Error(t, err)
	})
	t.Run("with injected limiter", func(t *testing.T) {
		inj, err := NewConsumerCreateLimiter(50, 5)
		require.NoError(t, err)
		_, err = buildDynamic(t, WithConsumerCreateLimiter(inj), WithConsumerCreateClusterRate(-1))
		require.Error(t, err)
	})
}

func TestWithConsumerCreateClusterRate_ValidWithPerWorker(t *testing.T) {
	d, err := buildDynamic(t,
		WithConsumerCreateRate(200, 50),
		WithConsumerCreateClusterRate(1000),
	)
	require.NoError(t, err)
	require.NotNil(t, d.inner.ConsumerCreateLimiter())
}

func TestDynamic_ObserveWorkerCount_RetunesBuiltInLimiter(t *testing.T) {
	d, err := buildDynamic(t,
		WithConsumerCreateRate(200, 50),
		WithConsumerCreateClusterRate(1000),
	)
	require.NoError(t, err)

	d.ObserveWorkerCount(5) // min(200, 1000/5=200) = 200
	lim := d.inner.ConsumerCreateLimiter()
	tb, ok := lim.(interface{ Limit() float64 })
	require.True(t, ok)
	require.InEpsilon(t, 200.0, tb.Limit(), 1e-9)

	d.ObserveWorkerCount(20) // min(200, 1000/20=50) = 50
	require.InEpsilon(t, 50.0, tb.Limit(), 1e-9)
}

func TestDynamic_ObserveWorkerCount_NoClusterRate_NoOp(t *testing.T) {
	d, err := buildDynamic(t, WithConsumerCreateRate(200, 50)) // no cluster rate
	require.NoError(t, err)
	d.ObserveWorkerCount(20) // must not change the fixed 200/s
	lim := d.inner.ConsumerCreateLimiter()
	tb, ok := lim.(interface{ Limit() float64 })
	require.True(t, ok)
	require.InEpsilon(t, 200.0, tb.Limit(), 1e-9)
}

func TestDynamic_ObserveWorkerCount_InjectedLimiter_NoOp(t *testing.T) {
	inj, err := NewConsumerCreateLimiter(50, 5)
	require.NoError(t, err)
	d, err := buildDynamic(t, WithConsumerCreateLimiter(inj))
	require.NoError(t, err)
	require.NotPanics(t, func() { d.ObserveWorkerCount(10) }) // no RateSetter captured -> no-op
}
