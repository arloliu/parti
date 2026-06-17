package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEffectiveRate(t *testing.T) {
	// clusterRate/n = 1000/5 = 200; perWorkerMax=200 is not less than 200 → 200
	require.InEpsilon(t, 200.0, EffectiveRate(200, 1000, 5), 1e-9)
	// clusterRate/n = 1000/20 = 50; perWorkerMax=200 is not less than 50 → 50
	require.InEpsilon(t, 50.0, EffectiveRate(200, 1000, 20), 1e-9)
	// clusterRate/n = 50/1 = 50; perWorkerMax=200 is not less than 50 → 50
	require.InEpsilon(t, 50.0, EffectiveRate(200, 50, 1), 1e-9)
	// no ceiling (perWorkerMax<=0): 1000/10 = 100 → 100
	require.InEpsilon(t, 100.0, EffectiveRate(0, 1000, 10), 1e-9)
	// n=0 clamped to 1: 1000/1 = 1000; perWorkerMax=200 < 1000 → 200
	require.InEpsilon(t, 200.0, EffectiveRate(200, 1000, 0), 1e-9)
	// n=0 clamped to 1, no ceiling: 1000/1 = 1000 → 1000
	require.InEpsilon(t, 1000.0, EffectiveRate(0, 1000, 0), 1e-9)
}

func TestTokenBucketLimiter_SetRate_ChangesSteadyState(t *testing.T) {
	l := New(100, 1, nil) // 100/s, burst 1
	require.InEpsilon(t, 100.0, l.Limit(), 1e-9)

	l.SetRate(10) // tighten to 10/s
	require.InEpsilon(t, 10.0, l.Limit(), 1e-9)

	l.SetRate(1000) // loosen
	require.InEpsilon(t, 1000.0, l.Limit(), 1e-9)
}

func TestTokenBucketLimiter_SetRate_PreservesBurst(t *testing.T) {
	l := New(100, 7, nil)
	l.SetRate(5)
	require.Equal(t, 7, l.rl.Burst(), "SetRate must not change burst")
}

func TestTokenBucketLimiter_RateSetter_Assertion(t *testing.T) {
	var lim Limiter = New(100, 1, nil)
	rs, ok := lim.(RateSetter)
	require.True(t, ok, "*TokenBucketLimiter must satisfy RateSetter")
	rs.SetRate(50)
	tbl, ok := lim.(*TokenBucketLimiter)
	require.True(t, ok)
	require.InEpsilon(t, 50.0, tbl.Limit(), 1e-9)
}

func TestTokenBucketLimiter_SetRate_ConcurrentWithWait(t *testing.T) {
	l := New(1000, 100, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for ctx.Err() == nil {
				_ = l.Wait(ctx)
			}
		})
	}
	wg.Go(func() {
		for ctx.Err() == nil {
			l.SetRate(500)
			l.SetRate(2000)
		}
	})
	wg.Wait() // -race must stay clean
}
