package retry

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// instantSleep is a Sleep replacement that records its arguments and
// returns immediately, so tests can assert on backoff math without
// waiting real time. Returns ctx.Err() if cancelled (mirrors the
// production sleep contract).
type instantSleep struct {
	delays []time.Duration
}

func (s *instantSleep) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.delays = append(s.delays, d)
	return nil
}

func TestEnvelope_Run_SuccessFirstAttempt(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	sl := &instantSleep{}
	env := New(Config{
		Work:        func(context.Context) error { calls.Add(1); return nil },
		BaseBackoff: time.Millisecond,
		MaxBackoff:  time.Millisecond,
		MaxAttempts: 5,
		Sleep:       sl.sleep,
	})
	require.NoError(t, env.Run(context.Background()))
	require.Equal(t, int64(1), calls.Load())
	require.Empty(t, sl.delays, "success on first attempt must not sleep")
}

func TestEnvelope_Run_TransientThenSuccess(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	sl := &instantSleep{}
	env := New(Config{
		Work: func(context.Context) error {
			if calls.Add(1) < 4 {
				return errors.New("transient")
			}
			return nil
		},
		BaseBackoff: 10 * time.Millisecond,
		MaxBackoff:  80 * time.Millisecond,
		MaxAttempts: 5,
		Sleep:       sl.sleep,
	})
	require.NoError(t, env.Run(context.Background()))
	require.Equal(t, int64(4), calls.Load())
	require.Len(t, sl.delays, 3, "three failures must produce three backoffs")
}

func TestEnvelope_Run_ExhaustsAndFiresPermanent(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	var permErr error
	var permFires atomic.Int64
	last := errors.New("attempt-N")
	sl := &instantSleep{}
	env := New(Config{
		Work: func(context.Context) error {
			calls.Add(1)
			return last
		},
		OnPermanent: func(err error) {
			permFires.Add(1)
			permErr = err
		},
		BaseBackoff: time.Millisecond,
		MaxBackoff:  time.Millisecond,
		MaxAttempts: 4,
		Sleep:       sl.sleep,
	})
	err := env.Run(context.Background())
	require.ErrorIs(t, err, ErrExhausted)
	require.Equal(t, int64(4), calls.Load())
	require.Equal(t, int64(1), permFires.Load(),
		"OnPermanent must fire EXACTLY once on exhaustion")
	require.Same(t, last, permErr, "OnPermanent receives the last error")
	require.Len(t, sl.delays, 3, "N attempts → N-1 sleeps")
}

func TestEnvelope_Run_GiveUpFiresPermanentImmediately(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	var permFires atomic.Int64
	err := errors.New("bucket-gone")
	env := New(Config{
		Work: func(context.Context) error {
			calls.Add(1)
			return err
		},
		Classify:    func(error) Class { return GiveUp },
		OnPermanent: func(error) { permFires.Add(1) },
		BaseBackoff: time.Millisecond,
		MaxBackoff:  time.Millisecond,
		MaxAttempts: 10,
		Sleep:       (&instantSleep{}).sleep,
	})
	require.ErrorIs(t, env.Run(context.Background()), ErrExhausted)
	require.Equal(t, int64(1), calls.Load(), "GiveUp must short-circuit on the first attempt")
	require.Equal(t, int64(1), permFires.Load())
}

func TestEnvelope_Run_FatalReturnsErrorNoPermanent(t *testing.T) {
	t.Parallel()
	var permFires atomic.Int64
	original := errors.New("config-mistake")
	env := New(Config{
		Work:        func(context.Context) error { return original },
		Classify:    func(error) Class { return Fatal },
		OnPermanent: func(error) { permFires.Add(1) },
		BaseBackoff: time.Millisecond,
		MaxBackoff:  time.Millisecond,
		MaxAttempts: 10,
		Sleep:       (&instantSleep{}).sleep,
	})
	err := env.Run(context.Background())
	require.Same(t, original, err, "Fatal returns the original error directly")
	require.Equal(t, int64(0), permFires.Load(),
		"Fatal must NOT fire OnPermanent (caller handles it)")
}

func TestEnvelope_Run_ContextCancelMidBackoff(t *testing.T) {
	t.Parallel()
	var permFires atomic.Int64
	env := New(Config{
		Work:        func(context.Context) error { return errors.New("transient") },
		OnPermanent: func(error) { permFires.Add(1) },
		BaseBackoff: 100 * time.Millisecond,
		MaxBackoff:  100 * time.Millisecond,
		MaxAttempts: 100,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := env.Run(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, int64(0), permFires.Load(),
		"context cancel must NOT fire OnPermanent — it's not an exhaustion event")
}

func TestEnvelope_Run_BackoffCappedAtMaxBackoff(t *testing.T) {
	t.Parallel()
	sl := &instantSleep{}
	env := New(Config{
		Work:        func(context.Context) error { return errors.New("transient") },
		BaseBackoff: 10 * time.Millisecond,
		MaxBackoff:  40 * time.Millisecond,
		MaxAttempts: 6,
		Sleep:       sl.sleep,
		// Jitter omitted (0) so the math is exact.
	})
	_ = env.Run(context.Background())
	// Backoffs after attempts 1..5: 10, 20, 40, 40, 40
	require.Equal(t, []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		40 * time.Millisecond,
		40 * time.Millisecond,
	}, sl.delays)
}

func TestEnvelope_Run_JitterStaysWithinBounds(t *testing.T) {
	t.Parallel()
	const trials = 200
	const base = 100 * time.Millisecond
	const jitter = 0.3 // ± 30%
	low := time.Duration(float64(base) * (1 - jitter))
	high := time.Duration(float64(base) * (1 + jitter))
	for range trials {
		sl := &instantSleep{}
		env := New(Config{
			Work: func(context.Context) error {
				return errors.New("transient")
			},
			BaseBackoff: base,
			MaxBackoff:  base,
			MaxAttempts: 2, // one failure → one backoff
			Jitter:      jitter,
			Sleep:       sl.sleep,
		})
		_ = env.Run(context.Background())
		require.Len(t, sl.delays, 1)
		d := sl.delays[0]
		require.GreaterOrEqual(t, d, low,
			"jittered backoff must not drop below %v; got %v", low, d)
		require.LessOrEqual(t, d, high,
			"jittered backoff must not exceed %v; got %v", high, d)
	}
}

func TestEnvelope_Run_ProgressFiredOnEachAttempt(t *testing.T) {
	t.Parallel()
	var progressN atomic.Int64
	env := New(Config{
		Work: func(context.Context) error {
			return errors.New("transient")
		},
		OnProgress: func(_ int, _ error) { progressN.Add(1) },
		// silence OnPermanent — irrelevant for this test
		BaseBackoff: time.Millisecond,
		MaxBackoff:  time.Millisecond,
		MaxAttempts: 5,
		Sleep:       (&instantSleep{}).sleep,
	})
	_ = env.Run(context.Background())
	require.Equal(t, int64(5), progressN.Load(),
		"OnProgress must fire once per failed attempt (including the final one that exhausts)")
}

func TestEnvelope_New_PanicsOnBadConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  Config
	}{
		{"nil Work", Config{BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond, MaxAttempts: 1}},
		{"zero MaxAttempts", Config{Work: func(context.Context) error { return nil }, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond}},
		{"zero BaseBackoff", Config{Work: func(context.Context) error { return nil }, MaxBackoff: time.Millisecond, MaxAttempts: 1}},
		{"MaxBackoff < BaseBackoff", Config{Work: func(context.Context) error { return nil }, BaseBackoff: 100 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, MaxAttempts: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Panics(t, func() {
				New(tc.cfg)
			})
		})
	}
}

// TestEnvelope_DefaultClassifyIsTransient confirms that omitting
// Classify defaults to Transient, so callers that only care about
// the attempt-budget contract do not need to provide a classifier.
func TestEnvelope_DefaultClassifyIsTransient(t *testing.T) {
	t.Parallel()
	var permFires atomic.Int64
	env := New(Config{
		Work:        func(context.Context) error { return errors.New("anything") },
		OnPermanent: func(error) { permFires.Add(1) },
		BaseBackoff: time.Millisecond,
		MaxBackoff:  time.Millisecond,
		MaxAttempts: 3,
		Sleep:       (&instantSleep{}).sleep,
	})
	require.ErrorIs(t, env.Run(context.Background()), ErrExhausted)
	require.Equal(t, int64(1), permFires.Load())
}

// TestEnvelope_Run_BackoffDoublingMath sanity-checks the exponential
// expansion against a hand-computed table. Independent of jitter
// (jitter=0) so the table is exact.
func TestEnvelope_Run_BackoffDoublingMath(t *testing.T) {
	t.Parallel()
	sl := &instantSleep{}
	env := New(Config{
		Work:        func(context.Context) error { return errors.New("transient") },
		BaseBackoff: 100 * time.Millisecond,
		MaxBackoff:  1 * time.Second,
		MaxAttempts: 5,
		Sleep:       sl.sleep,
	})
	_ = env.Run(context.Background())
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
	}
	require.Equal(t, want, sl.delays)
}

// TestEnvelope_Run_NoOverflowOnHugeAttempt guards against integer
// overflow in the backoff doubling loop if MaxAttempts is large.
func TestEnvelope_Run_NoOverflowOnHugeAttempt(t *testing.T) {
	t.Parallel()
	sl := &instantSleep{}
	env := New(Config{
		Work:        func(context.Context) error { return errors.New("transient") },
		BaseBackoff: 10 * time.Millisecond,
		MaxBackoff:  100 * time.Millisecond,
		MaxAttempts: 64,
		Sleep:       sl.sleep,
	})
	_ = env.Run(context.Background())
	for _, d := range sl.delays {
		require.LessOrEqual(t, d, 100*time.Millisecond,
			"capped backoff must never exceed MaxBackoff under any attempt count")
		require.GreaterOrEqual(t, d, time.Duration(0),
			"backoff must never be negative under any attempt count")
	}
}
