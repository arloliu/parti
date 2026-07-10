package parti_test

import (
	"errors"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestLabelSpillGrace_ConfigZeroIsUnreachable is the verify-first reproducer for
// Gap 1: on a non-pointer Config field, an explicit LabelSpillGrace = 0 cannot
// be distinguished from the zero value, so NewManager's SetDefaults call
// re-defaults it to 60s. This documents the v2.9.0 behavior that
// WithLabelSpillGrace exists to work around.
func TestLabelSpillGrace_ConfigZeroIsUnreachable(t *testing.T) {
	t.Parallel()

	cfg := parti.Config{LabelSpillGrace: 0}
	require.NoError(t, parti.SetDefaults(&cfg))
	require.Equal(t, 60*time.Second, cfg.LabelSpillGrace,
		"explicit config 0 is coerced back to the 60s default (the bug WithLabelSpillGrace fixes)")
}

// TestWithLabelSpillGrace_Override asserts the option resolves onto the
// Manager's config copy — the value startCalculator hands the calculator —
// including the immediate-spill 0 that the config field cannot express.
func TestWithLabelSpillGrace_Override(t *testing.T) {
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	src := source.NewStatic(testutil.CreateTestPartitions(3))

	newMgr := func(t *testing.T, opts ...parti.Option) *parti.Manager {
		t.Helper()
		cfg := testutil.IntegrationTestConfig()
		cfg.LabelSpillGrace = 0 // config 0 would otherwise re-default to 60s
		mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(), opts...)
		require.NoError(t, err)
		return mgr
	}

	t.Run("no option keeps the 60s default", func(t *testing.T) {
		t.Parallel()
		mgr := newMgr(t)
		require.Equal(t, 60*time.Second, parti.LabelSpillGraceForTest(mgr),
			"without the option, config 0 stays coerced to 60s")
	})

	t.Run("zero reaches immediate spill", func(t *testing.T) {
		t.Parallel()
		mgr := newMgr(t, parti.WithLabelSpillGrace(0))
		require.Equal(t, time.Duration(0), parti.LabelSpillGraceForTest(mgr),
			"WithLabelSpillGrace(0) makes an immediate spill reachable")
	})

	t.Run("positive value wins over config", func(t *testing.T) {
		t.Parallel()
		mgr := newMgr(t, parti.WithLabelSpillGrace(30*time.Second))
		require.Equal(t, 30*time.Second, parti.LabelSpillGraceForTest(mgr))
	})
}

// TestWithLabelSpillGrace_NegativeRejected asserts the negative guard mirrors
// the config field's gte=0 validation, returning a wrapped ErrInvalidConfig
// naming the option.
func TestWithLabelSpillGrace_NegativeRejected(t *testing.T) {
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	src := source.NewStatic(testutil.CreateTestPartitions(3))

	_, err = parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(),
		parti.WithLabelSpillGrace(-time.Second))
	require.Error(t, err)
	require.True(t, errors.Is(err, types.ErrInvalidConfig), "must wrap ErrInvalidConfig")
	require.Contains(t, err.Error(), "WithLabelSpillGrace", "message must name the option")
}
