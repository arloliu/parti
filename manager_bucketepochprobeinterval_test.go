package parti

import (
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestResolveBucketEpochProbeInterval pins the small helper monitorBucketEpochs
// delegates its ticker interval to: a positive configured value passes
// through unchanged, and a non-positive one (unset Config in tests, or a
// value that somehow slipped past cfg.Validate's gt=0 rule) falls back to
// 10s.
func TestResolveBucketEpochProbeInterval(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{"positive value passes through", 30 * time.Second, 30 * time.Second},
		{"zero falls back to 10s", 0, 10 * time.Second},
		{"negative falls back to 10s", -5 * time.Second, 10 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, resolveBucketEpochProbeInterval(tc.configured))
		})
	}
}

// newUnconnectedManagerDeps returns constructor dependencies sufficient for
// exercising NewManager's option/validation path without a live NATS
// connection — an unconnected jetstream.JetStream is enough for the
// constructor to run (the same pattern manager_test.go's option tests use
// inline).
func newUnconnectedManagerDeps() (jetstream.JetStream, *mockSource, *mockStrategy) {
	conn := &nats.Conn{}
	js, _ := jetstream.New(conn)

	return js, &mockSource{}, &mockStrategy{}
}

// TestManager_BucketEpochProbeInterval_Option verifies WithBucketEpochProbeInterval
// overrides Config.BucketEpochProbeInterval on the Manager's resolved config,
// and that omitting the option leaves the config-default value (applied by
// SetDefaults inside NewManager) in place. No live NATS connection is
// needed — mirrors TestManager_WorkerLabels_WithOptionOverride's helper
// pattern (an unconnected jetstream.JetStream is sufficient for the
// constructor path).
func TestManager_BucketEpochProbeInterval_Option(t *testing.T) {
	t.Parallel()

	js, src, assignStrat := newUnconnectedManagerDeps()

	t.Run("no option keeps the config default", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		mgr, err := NewManager(&cfg, js, src, assignStrat)
		require.NoError(t, err)
		require.Equal(t, 10*time.Second, mgr.cfg.BucketEpochProbeInterval)
	})

	t.Run("option overrides the config value", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		mgr, err := NewManager(&cfg, js, src, assignStrat, WithBucketEpochProbeInterval(45*time.Second))
		require.NoError(t, err)
		require.Equal(t, 45*time.Second, mgr.cfg.BucketEpochProbeInterval)
	})
}

// TestManager_BucketEpochProbeInterval_InvalidRejected verifies the option's
// guard mirrors Config.BucketEpochProbeInterval's gt=0 validation (matching
// OperationTimeout's own rule), rejecting zero and negative durations with a
// wrapped ErrInvalidConfig naming the option.
func TestManager_BucketEpochProbeInterval_InvalidRejected(t *testing.T) {
	t.Parallel()

	js, src, assignStrat := newUnconnectedManagerDeps()

	for _, d := range []time.Duration{0, -time.Second} {
		cfg := DefaultConfig()
		_, err := NewManager(&cfg, js, src, assignStrat, WithBucketEpochProbeInterval(d))
		require.Error(t, err)
		require.ErrorIs(t, err, types.ErrInvalidConfig, "must wrap ErrInvalidConfig")
		require.Contains(t, err.Error(), "WithBucketEpochProbeInterval", "message must name the option")
	}
}
